// Copyright 2023 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package taskexecutor

import (
	"context"
	"sync"
	"time"

	"github.com/docker/go-units"
	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	litstorage "github.com/pingcap/tidb/br/pkg/storage"
	"github.com/pingcap/tidb/pkg/config"
	"github.com/pingcap/tidb/pkg/disttask/framework/handle"
	"github.com/pingcap/tidb/pkg/disttask/framework/proto"
	"github.com/pingcap/tidb/pkg/disttask/framework/scheduler"
	"github.com/pingcap/tidb/pkg/disttask/framework/storage"
	"github.com/pingcap/tidb/pkg/metrics"
	"github.com/pingcap/tidb/pkg/sessionctx/vardef"
	tidbutil "github.com/pingcap/tidb/pkg/util"
	"github.com/pingcap/tidb/pkg/util/backoff"
	"github.com/pingcap/tidb/pkg/util/cgroup"
	"github.com/pingcap/tidb/pkg/util/cpu"
	"github.com/pingcap/tidb/pkg/util/intest"
	"github.com/pingcap/tidb/pkg/util/logutil"
	"github.com/pingcap/tidb/pkg/util/memory"
	"go.uber.org/zap"
)

var (
	// TaskCheckInterval is the interval to check whether there are tasks to run.
	// TODO maybe change this interval larger for performance.
	TaskCheckInterval = 300 * time.Millisecond
	// SubtaskCheckInterval is the interval to check whether there are subtasks to run.
	// exported for testing.
	SubtaskCheckInterval = 300 * time.Millisecond
	// MaxSubtaskCheckInterval is the max interval to check whether there are subtasks to run.
	// exported for testing.
	MaxSubtaskCheckInterval = 2 * time.Second
	maxChecksWhenNoSubtask  = 7
	recoverMetaInterval     = 90 * time.Second
	unfinishedSubtaskStates = []proto.SubtaskState{
		proto.SubtaskStatePending,
		proto.SubtaskStateRunning,
	}
)

// Manager monitors the task table and manages the taskExecutors.
type Manager struct {
	taskTable TaskTable
	mu        struct {
		sync.RWMutex
		// taskID -> TaskExecutor.
		taskExecutors map[int64]TaskExecutor
	}
	// id, it's the same as server id now, i.e. host:port.
	id          string
	wg          tidbutil.WaitGroupWrapper
	executorWG  tidbutil.WaitGroupWrapper
	ctx         context.Context
	cancel      context.CancelFunc
	logger      *zap.Logger
	slotManager *slotManager

	totalCPU int
	totalMem int64
}

// NewManager creates a new task executor Manager.
func NewManager(ctx context.Context, id string, taskTable TaskTable) (*Manager, error) {
	logger := logutil.ErrVerboseLogger()
	if intest.InTest {
		logger = logger.With(zap.String("server-id", id))
	}
	totalMem, err := memory.MemTotal()
	if err != nil {
		// should not happen normally, as in main function of tidb-server, we assert
		// that memory.MemTotal() will not fail.
		return nil, err
	}
	totalCPU := cpu.GetCPUCount()
	if totalCPU <= 0 || totalMem <= 0 {
		return nil, errors.Errorf("invalid cpu or memory, cpu: %d, memory: %d", totalCPU, totalMem)
	}
	cgroupLimit, version, err := cgroup.GetCgroupMemLimit()
	// ignore the error of cgroup.GetCgroupMemLimit, as it's not a must-success step.
	if err == nil && version == cgroup.V2 {
		// see cgroup.detectMemLimitInV2 for more details.
		// below are some real memory limits tested on GCP:
		// node-spec  real-limit  percent
		// 16c32g        27.83Gi    87%
		// 32c64g        57.36Gi    89.6%
		// we use 'limit', not totalMem for adjust, as totalMem = min(physical-mem, 'limit')
		// content of 'memory.max' might be 'max', so we use the min of them.
		adjustedMem := min(totalMem, uint64(float64(cgroupLimit)*0.88))
		logger.Info("adjust memory limit for cgroup v2",
			zap.String("before", units.BytesSize(float64(totalMem))),
			zap.String("after", units.BytesSize(float64(adjustedMem))))
		totalMem = adjustedMem
	}
	logger.Info("build task executor manager", zap.Int("total-cpu", totalCPU),
		zap.String("total-mem", units.BytesSize(float64(totalMem))))
	m := &Manager{
		id:          id,
		taskTable:   taskTable,
		logger:      logger,
		slotManager: newSlotManager(totalCPU),
		totalCPU:    totalCPU,
		totalMem:    int64(totalMem),
	}
	m.ctx, m.cancel = context.WithCancel(ctx)
	m.mu.taskExecutors = make(map[int64]TaskExecutor)

	return m, nil
}

// InitMeta initializes the meta of the Manager.
// not a must-success step before start manager,
// manager will try to recover meta periodically.
func (m *Manager) InitMeta() error {
	return m.runWithRetry(func() error {
		return m.taskTable.InitMeta(m.ctx, m.id, config.GetGlobalConfig().Instance.TiDBServiceScope)
	}, "init meta failed")
}

func (m *Manager) recoverMeta() error {
	return m.runWithRetry(func() error {
		return m.taskTable.RecoverMeta(m.ctx, m.id, config.GetGlobalConfig().Instance.TiDBServiceScope)
	}, "recover meta failed")
}

// Start starts the Manager.
func (m *Manager) Start() error {
	m.logger.Info("task executor manager start")
	m.wg.Run(m.handleTasksLoop)
	m.wg.Run(m.recoverMetaLoop)
	return nil
}

// Cancel cancels the executor manager.
// used in test to simulate tidb node shutdown.
func (m *Manager) Cancel() {
	m.cancel()
}

// Stop stops the Manager.
func (m *Manager) Stop() {
	m.cancel()
	m.executorWG.Wait()
	m.wg.Wait()
}

// handleTasksLoop handle tasks of interested states, including:
//   - pending/running: start the task executor.
//   - reverting: cancel the task executor, and mark running subtasks as Canceled.
//   - pausing: cancel the task executor, mark all pending/running subtasks of current
//     node as paused.
//
// Pausing is handled on every executor to make sure all subtasks are
// NOT running by executor before mark the task as paused.
func (m *Manager) handleTasksLoop() {
	defer tidbutil.Recover(metrics.LabelDomain, "handleTasksLoop", m.handleTasksLoop, false)
	ticker := time.NewTicker(TaskCheckInterval)
	for {
		select {
		case <-m.ctx.Done():
			m.logger.Info("handle tasks loop done")
			return
		case <-ticker.C:
		}

		m.handleTasks()
		// service scope might change, so we call WithLabelValues every time.
		metrics.DistTaskUsedSlotsGauge.WithLabelValues(vardef.ServiceScope.Load()).
			Set(float64(m.slotManager.usedSlots()))
		metrics.GlobalSortUploadWorkerCount.Set(float64(litstorage.GetActiveUploadWorkerCount()))
	}
}

// we handle tasks by their rank which is defined by Task.Compare.
// Manager will make sure tasks with high ranking are run before tasks with low ranking,
// when there are not enough slots, we might preempt the tasks with low ranking,
// i.e. to cancel their task executor directly, so it's possible some subtask of
// those tasks are half done, they have to rerun when the task is scheduled again.
// when there is no enough slots to run a task even after considers preemption,
// tasks with low ranking can run.
func (m *Manager) handleTasks() {
	// we don't query task in 'modifying' state, if it's prev-state is 'pending'
	// or 'paused', then they are not executable, if it's 'running', it should be
	// queried out soon as 'modifying' is a fast process.
	// it's possible that after we create task executor for a 'running' task, it
	// enters 'modifying', as slots are allocated already, that's ok.
	tasks, err := m.taskTable.GetTaskExecInfoByExecID(m.ctx, m.id)
	if err != nil {
		m.logErr(err)
		return
	}

	executableTasks := make([]*storage.TaskExecInfo, 0, len(tasks))
	for _, task := range tasks {
		switch task.State {
		case proto.TaskStateRunning:
			if !m.isExecutorStarted(task.ID) {
				executableTasks = append(executableTasks, task)
			}
		case proto.TaskStatePausing:
			if err := m.handlePausingTask(task.ID); err != nil {
				m.logErr(err)
			}
		case proto.TaskStateReverting:
			if err := m.handleRevertingTask(task.ID); err != nil {
				m.logErr(err)
			}
		}
	}

	if len(executableTasks) > 0 {
		m.handleExecutableTasks(executableTasks)
	}
}

// handleExecutableTasks handles executable tasks.
func (m *Manager) handleExecutableTasks(taskInfos []*storage.TaskExecInfo) {
	for _, task := range taskInfos {
		canAlloc, tasksNeedFree := m.slotManager.canAlloc(task.TaskBase)
		if len(tasksNeedFree) > 0 {
			m.cancelTaskExecutors(tasksNeedFree)
			m.logger.Info("need to preempt tasks of low ranking", zap.Stringer("task", task.TaskBase),
				zap.Stringers("preemptedTasks", tasksNeedFree))
			// do not handle the tasks with low ranking if current task is waiting
			// other tasks to free slots to make sure the order of running.
			break
		}

		if !canAlloc {
			// try to run tasks of low ranking
			m.logger.Debug("no enough slots to run task", zap.Int64("task-id", task.ID))
			continue
		}
		failpoint.InjectCall("beforeCallStartTaskExecutor", task.TaskBase)
		if !m.startTaskExecutor(task.TaskBase) {
			// we break to make sure the order of running.
			// it's possible some other low ranking tasks alloc more slots at
			// runtime, in this case we should try preempt them in next iteration.
			break
		}
	}
}

// cancelRunningSubtaskOf cancels the running subtask of the task, the subtask
// will switch to `canceled` state.
func (m *Manager) cancelRunningSubtaskOf(taskID int64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if executor, ok := m.mu.taskExecutors[taskID]; ok {
		m.logger.Info("onCanceledTasks", zap.Int64("task-id", taskID))
		executor.CancelRunningSubtask()
	}
}

// onPausingTasks pauses/cancels the pending/running subtasks.
func (m *Manager) handlePausingTask(taskID int64) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	m.logger.Info("handle pausing task", zap.Int64("task-id", taskID))
	if executor, ok := m.mu.taskExecutors[taskID]; ok {
		executor.Cancel()
	}
	// we pause subtasks belongs to this exec node even when there's no executor running.
	// as balancer might move subtasks to this node when the executor hasn't started.
	return m.taskTable.PauseSubtasks(m.ctx, m.id, taskID)
}

func (m *Manager) handleRevertingTask(taskID int64) error {
	m.cancelRunningSubtaskOf(taskID)
	return m.taskTable.CancelSubtask(m.ctx, m.id, taskID)
}

// recoverMetaLoop recovers dist_framework_meta for the tidb node running the taskExecutor manager.
// This is necessary when the TiDB node experiences a prolonged network partition
// and the scheduler deletes `dist_framework_meta`.
// When the TiDB node recovers from the network partition,
// we need to re-insert the metadata.
func (m *Manager) recoverMetaLoop() {
	defer tidbutil.Recover(metrics.LabelDomain, "recoverMetaLoop", m.recoverMetaLoop, false)
	ticker := time.NewTicker(recoverMetaInterval)
	for {
		select {
		case <-m.ctx.Done():
			m.logger.Info("recoverMetaLoop done")
			return
		case <-ticker.C:
			if err := m.recoverMeta(); err != nil {
				m.logErr(err)
				continue
			}
		}
	}
}

// cancelTaskExecutors cancels the task executors.
// unlike cancelRunningSubtaskOf, this function doesn't change subtask state.
func (m *Manager) cancelTaskExecutors(tasks []*proto.TaskBase) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, task := range tasks {
		m.logger.Info("cancel task executor", zap.Int64("task-id", task.ID))
		if executor, ok := m.mu.taskExecutors[task.ID]; ok {
			executor.Cancel()
		}
	}
}

// startTaskExecutor handles a runnable task.
func (m *Manager) startTaskExecutor(taskBase *proto.TaskBase) (executorStarted bool) {
	// TODO: remove it when we can create task executor with task base.
	task, err := m.taskTable.GetTaskByID(m.ctx, taskBase.ID)
	if err != nil {
		m.logger.Error("get task failed", zap.Int64("task-id", taskBase.ID), zap.Error(err))
		return false
	}
	if !m.slotManager.alloc(&task.TaskBase) {
		m.logger.Info("alloc slots failed, maybe other task executor alloc more slots at runtime",
			zap.Int64("task-id", taskBase.ID), zap.Int("concurrency", taskBase.Concurrency),
			zap.Int("remaining-slots", m.slotManager.availableSlots()))
		return false
	}
	defer func() {
		// free the slot if executor not started.
		if !executorStarted {
			m.slotManager.free(task.ID)
		}
	}()

	factory := GetTaskExecutorFactory(task.Type)
	if factory == nil {
		err := errors.Errorf("task type %s not found", task.Type)
		m.failSubtask(err, task.ID, nil)
		return false
	}
	executor := factory(m.ctx, task, Param{
		taskTable: m.taskTable,
		slotMgr:   m.slotManager,
		nodeRc:    m.getNodeResource(),
		execID:    m.id,
	})
	err = executor.Init(m.ctx)
	if err != nil {
		m.failSubtask(err, task.ID, executor)
		return false
	}
	m.addTaskExecutor(executor)
	m.logger.Info("task executor started", zap.Int64("task-id", task.ID),
		zap.Stringer("type", task.Type), zap.Int("concurrency", task.Concurrency),
		zap.Int("remaining-slots", m.slotManager.availableSlots()))
	m.executorWG.RunWithLog(func() {
		defer func() {
			m.logger.Info("task executor exit", zap.Int64("task-id", task.ID),
				zap.Stringer("type", task.Type))
			m.slotManager.free(task.ID)
			m.delTaskExecutor(executor)
			executor.Close()
		}()
		executor.Run()
	})
	return true
}

func (m *Manager) getNodeResource() *NodeResource {
	return NewNodeResource(m.totalCPU, m.totalMem)
}

func (m *Manager) addTaskExecutor(executor TaskExecutor) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mu.taskExecutors[executor.GetTaskBase().ID] = executor
}

func (m *Manager) delTaskExecutor(executor TaskExecutor) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.mu.taskExecutors, executor.GetTaskBase().ID)
}

func (m *Manager) isExecutorStarted(taskID int64) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.mu.taskExecutors[taskID]
	return ok
}

func (m *Manager) logErr(err error) {
	m.logger.Error("task manager met error", zap.Error(err), zap.Stack("stack"))
}

func (m *Manager) failSubtask(err error, taskID int64, taskExecutor TaskExecutor) {
	m.logErr(err)
	// TODO we want to define err of taskexecutor.Init as fatal, but add-index have
	// some code in Init that need retry, remove it after it's decoupled.
	if taskExecutor != nil && taskExecutor.IsRetryableError(err) {
		m.logger.Error("met retryable err", zap.Error(err), zap.Stack("stack"))
		return
	}
	err1 := m.runWithRetry(func() error {
		return m.taskTable.FailSubtask(m.ctx, m.id, taskID, err)
	}, "update to subtask failed")
	if err1 == nil {
		m.logger.Error("update error to subtask success", zap.Int64("task-id", taskID),
			zap.Error(err1), zap.Stack("stack"))
	}
}

func (m *Manager) runWithRetry(fn func() error, msg string) error {
	backoffer := backoff.NewExponential(scheduler.RetrySQLInterval, 2, scheduler.RetrySQLMaxInterval)
	err1 := handle.RunWithRetry(m.ctx, scheduler.RetrySQLTimes, backoffer, m.logger,
		func(_ context.Context) (bool, error) {
			return true, fn()
		},
	)
	if err1 != nil {
		m.logger.Warn(msg, zap.Error(err1))
	}
	return err1
}

// NodeResource is the resource of the node.
// exported for test.
type NodeResource struct {
	totalCPU int
	totalMem int64
}

// NewNodeResource creates a new NodeResource.
// exported for test.
func NewNodeResource(totalCPU int, totalMem int64) *NodeResource {
	return &NodeResource{
		totalCPU: totalCPU,
		totalMem: totalMem,
	}
}

func (nr *NodeResource) getStepResource(concurrency int) *proto.StepResource {
	return &proto.StepResource{
		CPU: proto.NewAllocatable(int64(concurrency)),
		// same proportion as CPU
		Mem: proto.NewAllocatable(int64(float64(concurrency) / float64(nr.totalCPU) * float64(nr.totalMem))),
	}
}
