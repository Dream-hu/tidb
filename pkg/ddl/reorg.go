// Copyright 2015 PingCAP, Inc.
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

package ddl

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/tidb/pkg/ddl/ingest"
	"github.com/pingcap/tidb/pkg/ddl/logutil"
	sess "github.com/pingcap/tidb/pkg/ddl/session"
	"github.com/pingcap/tidb/pkg/distsql"
	distsqlctx "github.com/pingcap/tidb/pkg/distsql/context"
	"github.com/pingcap/tidb/pkg/errctx"
	"github.com/pingcap/tidb/pkg/expression/exprctx"
	"github.com/pingcap/tidb/pkg/expression/exprstatic"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/meta"
	"github.com/pingcap/tidb/pkg/meta/autoid"
	"github.com/pingcap/tidb/pkg/meta/model"
	"github.com/pingcap/tidb/pkg/metrics"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/parser/terror"
	"github.com/pingcap/tidb/pkg/sessionctx"
	"github.com/pingcap/tidb/pkg/sessionctx/stmtctx"
	"github.com/pingcap/tidb/pkg/sessionctx/vardef"
	"github.com/pingcap/tidb/pkg/sessionctx/variable"
	"github.com/pingcap/tidb/pkg/statistics"
	"github.com/pingcap/tidb/pkg/table"
	"github.com/pingcap/tidb/pkg/table/tables"
	"github.com/pingcap/tidb/pkg/table/tblctx"
	"github.com/pingcap/tidb/pkg/tablecodec"
	"github.com/pingcap/tidb/pkg/types"
	"github.com/pingcap/tidb/pkg/util/chunk"
	"github.com/pingcap/tidb/pkg/util/codec"
	contextutil "github.com/pingcap/tidb/pkg/util/context"
	"github.com/pingcap/tidb/pkg/util/dbterror"
	"github.com/pingcap/tidb/pkg/util/intest"
	"github.com/pingcap/tidb/pkg/util/ranger"
	"github.com/pingcap/tidb/pkg/util/rowcodec"
	"github.com/pingcap/tidb/pkg/util/timeutil"
	"github.com/pingcap/tipb/go-tipb"
	atomicutil "go.uber.org/atomic"
	"go.uber.org/zap"
)

// reorgCtx is for reorganization.
type reorgCtx struct {
	// doneCh is used to notify.
	// If the reorganization job is done, we will use this channel to notify outer.
	// TODO: Now we use goroutine to simulate reorganization jobs, later we may
	// use a persistent job list.
	doneCh chan reorgFnResult
	// rowCount is used to simulate a job's row count.
	rowCount int64

	mu struct {
		sync.Mutex
		// warnings are used to store the warnings when doing the reorg job under certain SQL modes.
		warnings      map[errors.ErrorID]*terror.Error
		warningsCount map[errors.ErrorID]int64
	}

	references atomicutil.Int32
}

// reorgFnResult records the DDL owner TS before executing reorg function, in order to help
// receiver determine if the result is from reorg function of previous DDL owner in this instance.
type reorgFnResult struct {
	ownerTS int64
	err     error
}

func newReorgExprCtx() *exprstatic.ExprContext {
	evalCtx := exprstatic.NewEvalContext(
		exprstatic.WithSQLMode(mysql.ModeNone),
		exprstatic.WithTypeFlags(types.DefaultStmtFlags),
		exprstatic.WithErrLevelMap(stmtctx.DefaultStmtErrLevels),
	)

	planCacheTracker := contextutil.NewPlanCacheTracker(contextutil.IgnoreWarn)

	return exprstatic.NewExprContext(
		exprstatic.WithEvalCtx(evalCtx),
		exprstatic.WithPlanCacheTracker(&planCacheTracker),
	)
}

func newReorgExprCtxWithReorgMeta(reorgMeta *model.DDLReorgMeta, warnHandler contextutil.WarnHandler) (*exprstatic.ExprContext, error) {
	intest.AssertNotNil(reorgMeta)
	intest.AssertNotNil(warnHandler)
	loc, err := reorgTimeZoneWithTzLoc(reorgMeta.Location)
	if err != nil {
		return nil, errors.Trace(err)
	}

	ctx := newReorgExprCtx()
	evalCtx := ctx.GetStaticEvalCtx().Apply(
		exprstatic.WithSQLMode(reorgMeta.SQLMode),
		exprstatic.WithLocation(loc),
		exprstatic.WithTypeFlags(reorgTypeFlagsWithSQLMode(reorgMeta.SQLMode)),
		exprstatic.WithErrLevelMap(reorgErrLevelsWithSQLMode(reorgMeta.SQLMode)),
		exprstatic.WithWarnHandler(warnHandler),
	)
	return ctx.Apply(exprstatic.WithEvalCtx(evalCtx)), nil
}

// reorgTableMutateContext implements table.MutateContext for reorganization.
type reorgTableMutateContext struct {
	exprCtx            exprctx.ExprContext
	encodingConfig     tblctx.RowEncodingConfig
	mutateBuffers      *tblctx.MutateBuffers
	shardID            *variable.RowIDShardGenerator
	reservedRowIDAlloc stmtctx.ReservedRowIDAlloc
}

// AlternativeAllocators implements table.MutateContext.AlternativeAllocators.
func (*reorgTableMutateContext) AlternativeAllocators(*model.TableInfo) (autoid.Allocators, bool) {
	// No alternative allocators for all tables because temporary tables
	// are not supported (temporary tables do not have any data in TiKV) in reorganization.
	return autoid.Allocators{}, false
}

// GetExprCtx implements table.MutateContext.GetExprCtx.
func (ctx *reorgTableMutateContext) GetExprCtx() exprctx.ExprContext {
	return ctx.exprCtx
}

// ConnectionID implements table.MutateContext.ConnectionID.
func (*reorgTableMutateContext) ConnectionID() uint64 {
	return 0
}

// InRestrictedSQL implements table.MutateContext.InRestrictedSQL.
func (*reorgTableMutateContext) InRestrictedSQL() bool {
	return false
}

// TxnAssertionLevel implements table.MutateContext.TxnAssertionLevel.
func (*reorgTableMutateContext) TxnAssertionLevel() variable.AssertionLevel {
	// Because only `index.Create` and `index.Delete` are invoked in reorganization which does not use this method,
	// we can just return `AssertionLevelOff`.
	return variable.AssertionLevelOff
}

// EnableMutationChecker implements table.MutateContext.EnableMutationChecker.
func (*reorgTableMutateContext) EnableMutationChecker() bool {
	// Because only `index.Create` and `index.Delete` are invoked in reorganization which does not use this method,
	// we can just return false.
	return false
}

// GetRowEncodingConfig implements table.MutateContext.GetRowEncodingConfig.
func (ctx *reorgTableMutateContext) GetRowEncodingConfig() tblctx.RowEncodingConfig {
	return ctx.encodingConfig
}

// GetMutateBuffers implements table.MutateContext.GetMutateBuffers.
func (ctx *reorgTableMutateContext) GetMutateBuffers() *tblctx.MutateBuffers {
	return ctx.mutateBuffers
}

// GetRowIDShardGenerator implements table.MutateContext.GetRowIDShardGenerator.
func (ctx *reorgTableMutateContext) GetRowIDShardGenerator() *variable.RowIDShardGenerator {
	return ctx.shardID
}

// GetReservedRowIDAlloc implements table.MutateContext.GetReservedRowIDAlloc.
func (ctx *reorgTableMutateContext) GetReservedRowIDAlloc() (*stmtctx.ReservedRowIDAlloc, bool) {
	return &ctx.reservedRowIDAlloc, true
}

// GetStatisticsSupport implements table.MutateContext.GetStatisticsSupport.
func (*reorgTableMutateContext) GetStatisticsSupport() (tblctx.StatisticsSupport, bool) {
	// We can just return `(nil, false)` because:
	// - Only `index.Create` and `index.Delete` are invoked in reorganization which does not use this method.
	// - DDL reorg do need to collect statistics in this way.
	return nil, false
}

// GetCachedTableSupport implements table.MutateContext.GetCachedTableSupport.
func (*reorgTableMutateContext) GetCachedTableSupport() (tblctx.CachedTableSupport, bool) {
	// We can just return `(nil, false)` because:
	// - Only `index.Create` and `index.Delete` are invoked in reorganization which does not use this method.
	// - It is not allowed to execute DDL on a cached table.
	return nil, false
}

// GetTemporaryTableSupport implements table.MutateContext.GetTemporaryTableSupport.
func (*reorgTableMutateContext) GetTemporaryTableSupport() (tblctx.TemporaryTableSupport, bool) {
	// We can just return `(nil, false)` because:
	// - Only `index.Create` and `index.Delete` are invoked in reorganization which does not use this method.
	// - Temporary tables do not have any data in TiKV.
	return nil, false
}

// GetExchangePartitionDMLSupport implements table.MutateContext.GetExchangePartitionDMLSupport.
func (*reorgTableMutateContext) GetExchangePartitionDMLSupport() (tblctx.ExchangePartitionDMLSupport, bool) {
	// We can just return `(nil, false)` because:
	// - Only `index.Create` and `index.Delete` are invoked in reorganization which does not use this method.
	return nil, false
}

// newReorgTableMutateContext creates a new table.MutateContext for reorganization.
func newReorgTableMutateContext(exprCtx exprctx.ExprContext) table.MutateContext {
	rowEncoder := &rowcodec.Encoder{
		Enable: vardef.GetDDLReorgRowFormat() != vardef.DefTiDBRowFormatV1,
	}

	encodingConfig := tblctx.RowEncodingConfig{
		IsRowLevelChecksumEnabled: rowEncoder.Enable,
		RowEncoder:                rowEncoder,
	}

	return &reorgTableMutateContext{
		exprCtx:        exprCtx,
		encodingConfig: encodingConfig,
		mutateBuffers:  tblctx.NewMutateBuffers(&variable.WriteStmtBufs{}),
		// Though currently, `RowIDShardGenerator` is not required in DDL reorg,
		// we still provide a valid one to keep the context complete and to avoid panic if it is used in the future.
		shardID: variable.NewRowIDShardGenerator(
			rand.New(rand.NewSource(time.Now().UnixNano())), // #nosec G404
			vardef.DefTiDBShardAllocateStep,
		),
	}
}

func reorgTypeFlagsWithSQLMode(mode mysql.SQLMode) types.Flags {
	return types.StrictFlags.
		WithTruncateAsWarning(!mode.HasStrictMode()).
		WithIgnoreInvalidDateErr(mode.HasAllowInvalidDatesMode()).
		WithIgnoreZeroInDate(!mode.HasStrictMode() || mode.HasAllowInvalidDatesMode()).
		WithCastTimeToYearThroughConcat(true)
}

func reorgErrLevelsWithSQLMode(mode mysql.SQLMode) errctx.LevelMap {
	return errctx.LevelMap{
		errctx.ErrGroupTruncate:  errctx.ResolveErrLevel(false, !mode.HasStrictMode()),
		errctx.ErrGroupBadNull:   errctx.ResolveErrLevel(false, !mode.HasStrictMode()),
		errctx.ErrGroupNoDefault: errctx.ResolveErrLevel(false, !mode.HasStrictMode()),
		errctx.ErrGroupDividedByZero: errctx.ResolveErrLevel(
			!mode.HasErrorForDivisionByZeroMode(),
			!mode.HasStrictMode(),
		),
	}
}

func reorgTimeZoneWithTzLoc(tzLoc *model.TimeZoneLocation) (*time.Location, error) {
	if tzLoc == nil {
		// It is set to SystemLocation to be compatible with nil LocationInfo.
		return timeutil.SystemLocation(), nil
	}
	return tzLoc.GetLocation()
}

func (rc *reorgCtx) setRowCount(count int64) {
	atomic.StoreInt64(&rc.rowCount, count)
}

func (rc *reorgCtx) mergeWarnings(warnings map[errors.ErrorID]*terror.Error, warningsCount map[errors.ErrorID]int64) {
	if len(warnings) == 0 || len(warningsCount) == 0 {
		return
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.mu.warnings, rc.mu.warningsCount = mergeWarningsAndWarningsCount(warnings, rc.mu.warnings, warningsCount, rc.mu.warningsCount)
}

func (rc *reorgCtx) resetWarnings() {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.mu.warnings = make(map[errors.ErrorID]*terror.Error)
	rc.mu.warningsCount = make(map[errors.ErrorID]int64)
}

func (rc *reorgCtx) increaseRowCount(count int64) {
	atomic.AddInt64(&rc.rowCount, count)
}

func (rc *reorgCtx) getRowCount() int64 {
	row := atomic.LoadInt64(&rc.rowCount)
	return row
}

// runReorgJob is used as a portal to do the reorganization work.
// eg:
// 1: add index
// 2: alter column type
// 3: clean global index
// 4: reorganize partitions
/*
 ddl goroutine >---------+
   ^                     |
   |                     |
   |                     |
   |                     | <---(doneCh)--- f()
 HandleDDLQueue(...)     | <---(regular timeout)
   |                     | <---(ctx done)
   |                     |
   |                     |
 A more ddl round  <-----+
*/
// How can we cancel reorg job?
//
// The background reorg is continuously running except for several factors, for instances, ddl owner change,
// logic error (kv duplicate when insert index / cast error when alter column), ctx done, and cancel signal.
//
// When `admin cancel ddl jobs xxx` takes effect, we will give this kind of reorg ddl one more round.
// because we should pull the result from doneCh out, otherwise, the reorg worker will hang on `f()` logic,
// which is a kind of goroutine leak.
//
// That's why we couldn't set the job to rollingback state directly in `convertJob2RollbackJob`, which is a
// cancelling portal for admin cancel action.
//
// In other words, the cancelling signal is informed from the bottom up, we set the atomic cancel variable
// in the cancelling portal to notify the lower worker goroutine, and fetch the cancel error from them in
// the additional ddl round.
//
// After that, we can make sure that the worker goroutine is correctly shut down.
func (w *worker) runReorgJob(
	jobCtx *jobContext,
	reorgInfo *reorgInfo,
	tblInfo *model.TableInfo,
	reorgFn func() error,
) error {
	job := reorgInfo.Job
	d := reorgInfo.jobCtx.oldDDLCtx
	// This is for tests compatible, because most of the early tests try to build the reorg job manually
	// without reorg meta info, which will cause nil pointer in here.
	if job.ReorgMeta == nil {
		job.ReorgMeta = &model.DDLReorgMeta{
			SQLMode:       mysql.ModeNone,
			Warnings:      make(map[errors.ErrorID]*terror.Error),
			WarningsCount: make(map[errors.ErrorID]int64),
			Location:      &model.TimeZoneLocation{Name: time.UTC.String(), Offset: 0},
			Version:       model.CurrentReorgMetaVersion,
		}
	}

	rc := w.getReorgCtx(job.ID)
	if rc == nil {
		// This job is cancelling, we should return ErrCancelledDDLJob directly.
		//
		// Q: Is there any possibility that the job is cancelling and has no reorgCtx?
		// A: Yes, consider the case that :
		// - we cancel the job when backfilling the last batch of data, the cancel txn is commit first,
		// - and then the backfill workers send signal to the `doneCh` of the reorgCtx,
		// - and then the DDL worker will remove the reorgCtx
		// - and update the DDL job to `done`
		// - but at the commit time, the DDL txn will raise a "write conflict" error and retry, and it happens.
		if job.IsCancelling() {
			return dbterror.ErrCancelledDDLJob
		}

		beOwnerTS := w.ddlCtx.reorgCtx.getOwnerTS()
		rc = w.newReorgCtx(reorgInfo.Job.ID, reorgInfo.Job.GetRowCount())
		w.wg.Run(func() {
			err := reorgFn()
			rc.doneCh <- reorgFnResult{ownerTS: beOwnerTS, err: err}
		})
	}

	updateProgressInverval := 5 * time.Second
	failpoint.Inject("updateProgressIntervalInMs", func(val failpoint.Value) {
		if v, ok := val.(int); ok {
			updateProgressInverval = time.Duration(v) * time.Millisecond
		}
	})
	updateProcessTicker := time.NewTicker(updateProgressInverval)
	defer updateProcessTicker.Stop()
	for {
		select {
		case res := <-rc.doneCh:
			err := res.err
			curTS := w.ddlCtx.reorgCtx.getOwnerTS()
			if res.ownerTS != curTS {
				d.removeReorgCtx(job.ID)
				logutil.DDLLogger().Warn("owner ts mismatch, return timeout error and retry",
					zap.Int64("prevTS", res.ownerTS),
					zap.Int64("curTS", curTS))
				jobCtx.reorgTimeoutOccurred = true
				return dbterror.ErrWaitReorgTimeout
			}
			// Since job is cancelled，we don't care about its partial counts.
			// TODO(lance6716): should we also do for paused job?
			if terror.ErrorEqual(err, dbterror.ErrCancelledDDLJob) {
				d.removeReorgCtx(job.ID)
				return err
			}
			rowCount := rc.getRowCount()
			job.SetRowCount(rowCount)
			if err != nil {
				logutil.DDLLogger().Warn("run reorg job done",
					zap.Int64("jobID", reorgInfo.ID),
					zap.Int64("handled rows", rowCount), zap.Error(err))
			} else {
				logutil.DDLLogger().Info("run reorg job done",
					zap.Int64("jobID", reorgInfo.ID),
					zap.Int64("handled rows", rowCount))
			}

			// Update a job's warnings.
			w.mergeWarningsIntoJob(job)

			d.removeReorgCtx(job.ID)

			updateBackfillProgress(w, reorgInfo, tblInfo, rowCount)

			// For other errors, even err is not nil here, we still wait the partial counts to be collected.
			// since in the next round, the startKey is brand new which is stored by last time.
			return errors.Trace(err)
		case <-updateProcessTicker.C:
			rowCount := rc.getRowCount()
			job.SetRowCount(rowCount)
			updateBackfillProgress(w, reorgInfo, tblInfo, rowCount)

			// Update a job's warnings.
			w.mergeWarningsIntoJob(job)

			rc.resetWarnings()
			jobCtx.reorgTimeoutOccurred = true
			return dbterror.ErrWaitReorgTimeout
		}
	}
}

func overwriteReorgInfoFromGlobalCheckpoint(w *worker, sess *sess.Session, job *model.Job, reorgInfo *reorgInfo) error {
	if job.ReorgMeta.ReorgTp != model.ReorgTypeLitMerge {
		// Only used for the ingest mode job.
		return nil
	}
	if reorgInfo.mergingTmpIdx {
		// Merging the temporary index uses txn mode, so we don't need to consider the checkpoint.
		return nil
	}
	if job.ReorgMeta.IsDistReorg {
		// The global checkpoint is not used in distributed tasks.
		return nil
	}
	if w.getReorgCtx(job.ID) != nil {
		// We only overwrite from checkpoint when the job runs for the first time on this TiDB instance.
		return nil
	}
	start, pid, err := getImportedKeyFromCheckpoint(sess, job)
	if err != nil {
		return errors.Trace(err)
	}
	if pid != reorgInfo.PhysicalTableID {
		// Current physical ID does not match checkpoint physical ID.
		// Don't overwrite reorgInfo.StartKey.
		return nil
	}
	if len(start) > 0 {
		reorgInfo.StartKey = start
	}
	return nil
}

func extractElemIDs(r *reorgInfo) []int64 {
	elemIDs := make([]int64, 0, len(r.elements))
	for _, elem := range r.elements {
		elemIDs = append(elemIDs, elem.ID)
	}
	return elemIDs
}

func (w *worker) mergeWarningsIntoJob(job *model.Job) {
	rc := w.getReorgCtx(job.ID)
	rc.mu.Lock()
	partWarnings := rc.mu.warnings
	partWarningsCount := rc.mu.warningsCount
	rc.mu.Unlock()
	warnings, warningsCount := job.GetWarnings()
	warnings, warningsCount = mergeWarningsAndWarningsCount(partWarnings, warnings, partWarningsCount, warningsCount)
	job.SetWarnings(warnings, warningsCount)
}

func updateBackfillProgress(w *worker, reorgInfo *reorgInfo, tblInfo *model.TableInfo,
	addedRowCount int64) {
	if tblInfo == nil {
		return
	}
	progress := float64(0)
	if addedRowCount != 0 {
		totalCount := getTableTotalCount(w, tblInfo)
		if totalCount > 0 {
			progress = float64(addedRowCount) / float64(totalCount)
		} else {
			progress = 0
		}
		if progress > 1 {
			progress = 1
		}
		logutil.DDLLogger().Debug("update progress",
			zap.Float64("progress", progress),
			zap.Int64("addedRowCount", addedRowCount),
			zap.Int64("totalCount", totalCount))
	}
	switch reorgInfo.Type {
	case model.ActionAddIndex, model.ActionAddPrimaryKey:
		var label string
		if reorgInfo.mergingTmpIdx {
			label = metrics.LblAddIndexMerge
		} else {
			label = metrics.LblAddIndex
		}
		idxNames := ""
		args, err := model.GetModifyIndexArgs(reorgInfo.Job)
		if err != nil {
			logutil.DDLLogger().Error("Fail to get ModifyIndexArgs", zap.Error(err))
		} else {
			idxNames = getIdxNamesFromArgs(args)
		}
		metrics.GetBackfillProgressByLabel(label, reorgInfo.SchemaName, tblInfo.Name.String(), idxNames).Set(progress * 100)
	case model.ActionModifyColumn:
		colName := ""
		args, err := model.GetModifyColumnArgs(reorgInfo.Job)
		if err != nil {
			logutil.DDLLogger().Error("Fail to get ModifyColumnArgs", zap.Error(err))
		} else {
			colName = args.OldColumnName.O
		}
		metrics.GetBackfillProgressByLabel(metrics.LblModifyColumn, reorgInfo.SchemaName, tblInfo.Name.String(), colName).Set(progress * 100)
	case model.ActionReorganizePartition, model.ActionRemovePartitioning,
		model.ActionAlterTablePartitioning:
		metrics.GetBackfillProgressByLabel(metrics.LblReorgPartition, reorgInfo.SchemaName, tblInfo.Name.String(), "").Set(progress * 100)
	}
}

func getTableTotalCount(w *worker, tblInfo *model.TableInfo) int64 {
	var ctx sessionctx.Context
	ctx, err := w.sessPool.Get()
	if err != nil {
		return statistics.PseudoRowCount
	}
	defer w.sessPool.Put(ctx)
	executor := ctx.GetRestrictedSQLExecutor()
	var rows []chunk.Row
	if tblInfo.Partition != nil && len(tblInfo.Partition.DroppingDefinitions) > 0 {
		// if Reorganize Partition, only select number of rows from the selected partitions!
		defs := tblInfo.Partition.DroppingDefinitions
		partIDs := make([]string, 0, len(defs))
		for _, def := range defs {
			partIDs = append(partIDs, strconv.FormatInt(def.ID, 10))
		}
		sql := "select sum(table_rows) from information_schema.partitions where tidb_partition_id in (%?);"
		rows, _, err = executor.ExecRestrictedSQL(w.workCtx, nil, sql, strings.Join(partIDs, ","))
	} else {
		sql := "select table_rows from information_schema.tables where tidb_table_id=%?;"
		rows, _, err = executor.ExecRestrictedSQL(w.workCtx, nil, sql, tblInfo.ID)
	}
	if err != nil {
		return statistics.PseudoRowCount
	}
	if len(rows) != 1 {
		return statistics.PseudoRowCount
	}
	return rows[0].GetInt64(0)
}

func (dc *ddlCtx) isReorgRunnable(ctx context.Context, isDistReorg bool) error {
	if dc.ctx.Err() != nil {
		// Worker is closed. So it can't do the reorganization.
		return dbterror.ErrInvalidWorker.GenWithStack("worker is closed")
	}

	if ctx.Err() != nil {
		return context.Cause(ctx)
	}

	// If isDistReorg is true, we needn't check if it is owner.
	if isDistReorg {
		return nil
	}
	if !dc.isOwner() {
		// If it's not the owner, we will try later, so here just returns an error.
		logutil.DDLLogger().Info("DDL is not the DDL owner", zap.String("ID", dc.uuid))
		return errors.Trace(dbterror.ErrNotOwner)
	}
	return nil
}

type reorgInfo struct {
	*model.Job

	StartKey      kv.Key
	EndKey        kv.Key
	jobCtx        *jobContext
	first         bool
	mergingTmpIdx bool
	// PhysicalTableID is used for partitioned table.
	// DDL reorganize for a partitioned table will handle partitions one by one,
	// PhysicalTableID is used to trace the current partition we are handling.
	// If the table is not partitioned, PhysicalTableID would be TableID.
	PhysicalTableID int64
	dbInfo          *model.DBInfo
	elements        []*meta.Element
	currElement     *meta.Element
}

func (r *reorgInfo) NewJobContext() *ReorgContext {
	return r.jobCtx.oldDDLCtx.jobContext(r.Job.ID, r.Job.ReorgMeta)
}

func (r *reorgInfo) String() string {
	var isEnabled bool
	if ingest.LitInitialized {
		isEnabled = r.ReorgMeta != nil && r.ReorgMeta.IsFastReorg
	}
	return "CurrElementType:" + string(r.currElement.TypeKey) + "," +
		"CurrElementID:" + strconv.FormatInt(r.currElement.ID, 10) + "," +
		"StartKey:" + hex.EncodeToString(r.StartKey) + "," +
		"EndKey:" + hex.EncodeToString(r.EndKey) + "," +
		"First:" + strconv.FormatBool(r.first) + "," +
		"PhysicalTableID:" + strconv.FormatInt(r.PhysicalTableID, 10) + "," +
		"Ingest mode:" + strconv.FormatBool(isEnabled)
}

func constructOneRowTableScanPB(
	physicalTableID int64,
	tblInfo *model.TableInfo,
	handleCols []*model.ColumnInfo,
	desc bool,
) *tipb.Executor {
	tblScan := tables.BuildTableScanFromInfos(tblInfo, handleCols, false)
	tblScan.TableId = physicalTableID
	tblScan.Desc = desc
	return &tipb.Executor{Tp: tipb.ExecType_TypeTableScan, TblScan: tblScan}
}

func constructLimitPB(count uint64) *tipb.Executor {
	limitExec := &tipb.Limit{
		Limit: count,
	}
	return &tipb.Executor{Tp: tipb.ExecType_TypeLimit, Limit: limitExec}
}

func buildOneRowTableScanDAG(
	distSQLCtx *distsqlctx.DistSQLContext,
	tbl table.PhysicalTable,
	handleCols []*model.ColumnInfo,
	limit uint64,
	desc bool,
) (*tipb.DAGRequest, error) {
	dagReq := &tipb.DAGRequest{}
	_, timeZoneOffset := time.Now().In(time.UTC).Zone()
	dagReq.TimeZoneOffset = int64(timeZoneOffset)
	for i := range handleCols {
		dagReq.OutputOffsets = append(dagReq.OutputOffsets, uint32(i))
	}
	dagReq.Flags |= model.FlagInSelectStmt

	tblScanExec := constructOneRowTableScanPB(tbl.GetPhysicalID(), tbl.Meta(), handleCols, desc)
	dagReq.Executors = append(dagReq.Executors, tblScanExec)
	dagReq.Executors = append(dagReq.Executors, constructLimitPB(limit))
	distsql.SetEncodeType(distSQLCtx, dagReq)
	return dagReq, nil
}

func getColumnsTypes(columns []*model.ColumnInfo) []*types.FieldType {
	colTypes := make([]*types.FieldType, 0, len(columns))
	for _, col := range columns {
		colTypes = append(colTypes, &col.FieldType)
	}
	return colTypes
}

// buildOneRowTableScan builds a table scan that only return one row upon tblInfo.
func buildOneRowTableScan(
	ctx *ReorgContext,
	store kv.Storage,
	startTS uint64,
	tbl table.PhysicalTable,
	handleCols []*model.ColumnInfo,
	limit uint64,
	desc bool,
) (distsql.SelectResult, error) {
	distSQLCtx := newDefaultReorgDistSQLCtx(store.GetClient(), contextutil.NewStaticWarnHandler(0))
	dagPB, err := buildOneRowTableScanDAG(distSQLCtx, tbl, handleCols, limit, desc)
	if err != nil {
		return nil, errors.Trace(err)
	}
	var b distsql.RequestBuilder
	var builder *distsql.RequestBuilder
	var ranges []*ranger.Range
	if tbl.Meta().IsCommonHandle {
		ranges = ranger.FullNotNullRange()
	} else {
		ranges = ranger.FullIntRange(false)
	}
	builder = b.SetHandleRanges(distSQLCtx, tbl.GetPhysicalID(), tbl.Meta().IsCommonHandle, ranges)
	builder.SetDAGRequest(dagPB).
		SetStartTS(startTS).
		SetKeepOrder(true).
		SetConcurrency(1).
		SetDesc(desc).
		SetResourceGroupTagger(ctx.getResourceGroupTaggerForTopSQL()).
		SetResourceGroupName(ctx.resourceGroupName)

	builder.Request.NotFillCache = true
	builder.Request.Priority = kv.PriorityLow
	builder.RequestSource.RequestSourceInternal = true
	builder.RequestSource.RequestSourceType = ctx.ddlJobSourceType()

	kvReq, err := builder.Build()
	if err != nil {
		return nil, errors.Trace(err)
	}

	result, err := distsql.Select(ctx.ddlJobCtx, distSQLCtx, kvReq, getColumnsTypes(handleCols))
	if err != nil {
		return nil, errors.Trace(err)
	}
	return result, nil
}

// GetTableMaxHandle gets the max handle of a PhysicalTable.
func GetTableMaxHandle(ctx *ReorgContext, store kv.Storage, startTS uint64, tbl table.PhysicalTable) (maxHandle kv.Handle, emptyTable bool, err error) {
	tblInfo := tbl.Meta()
	handleCols := buildHandleCols(tbl)

	// build a desc scan of tblInfo, which limit is 1, we can use it to retrieve the last handle of the table.
	result, err := buildOneRowTableScan(ctx, store, startTS, tbl, handleCols, 1, true)
	if err != nil {
		return nil, false, errors.Trace(err)
	}
	defer terror.Call(result.Close)

	chk := chunk.New(getColumnsTypes(handleCols), 1, 1)
	err = result.Next(ctx.ddlJobCtx, chk)
	if err != nil {
		return nil, false, errors.Trace(err)
	}

	if chk.NumRows() == 0 {
		// empty table
		return nil, true, nil
	}
	row := chk.GetRow(0)
	if tblInfo.IsCommonHandle {
		pkIdx := tables.FindPrimaryIndex(tblInfo)
		maxHandle, err = buildCommonHandleFromChunkRow(time.UTC, tblInfo, pkIdx, handleCols, row)
		return maxHandle, false, err
	}
	return kv.IntHandle(row.GetInt64(0)), false, nil
}

// existsTableRow checks if there is at least one row in the specified table.
// In case of an error during the operation, it returns false along with the error.
func existsTableRow(ctx *ReorgContext, store kv.Storage, tbl table.PhysicalTable, startTS uint64) (bool, error) {
	found := false
	err := iterateSnapshotKeys(ctx, store, kv.PriorityLow, tbl.RecordPrefix(), startTS, nil, nil,
		func(_ kv.Handle, _ kv.Key, _ []byte) (bool, error) {
			found = true
			return false, nil
		})
	if err != nil {
		return false, errors.Trace(err)
	}
	return found, nil
}

func buildHandleCols(tbl table.PhysicalTable) []*model.ColumnInfo {
	var handleCols []*model.ColumnInfo
	var pkIdx *model.IndexInfo
	tblInfo := tbl.Meta()
	switch {
	case tblInfo.PKIsHandle:
		for _, col := range tbl.Meta().Columns {
			if mysql.HasPriKeyFlag(col.GetFlag()) {
				handleCols = []*model.ColumnInfo{col}
				break
			}
		}
	case tblInfo.IsCommonHandle:
		pkIdx = tables.FindPrimaryIndex(tblInfo)
		cols := tblInfo.Cols()
		for _, idxCol := range pkIdx.Columns {
			handleCols = append(handleCols, cols[idxCol.Offset])
		}
	default:
		handleCols = []*model.ColumnInfo{model.NewExtraHandleColInfo()}
	}
	return handleCols
}

func buildCommonHandleFromChunkRow(loc *time.Location, tblInfo *model.TableInfo, idxInfo *model.IndexInfo,
	cols []*model.ColumnInfo, row chunk.Row) (kv.Handle, error) {
	fieldTypes := make([]*types.FieldType, 0, len(cols))
	for _, col := range cols {
		fieldTypes = append(fieldTypes, &col.FieldType)
	}
	datumRow := row.GetDatumRow(fieldTypes)
	tablecodec.TruncateIndexValues(tblInfo, idxInfo, datumRow)

	var handleBytes []byte
	handleBytes, err := codec.EncodeKey(loc, nil, datumRow...)
	if err != nil {
		return nil, err
	}
	return kv.NewCommonHandle(handleBytes)
}

// getTableRange gets the start and end handle of a table (or partition).
func getTableRange(ctx *ReorgContext, store kv.Storage, tbl table.PhysicalTable, snapshotVer uint64, priority int) (startHandleKey, endHandleKey kv.Key, err error) {
	// Get the start handle of this partition.
	err = iterateSnapshotKeys(ctx, store, priority, tbl.RecordPrefix(), snapshotVer, nil, nil,
		func(_ kv.Handle, rowKey kv.Key, _ []byte) (bool, error) {
			startHandleKey = rowKey
			return false, nil
		})
	if err != nil {
		return startHandleKey, endHandleKey, errors.Trace(err)
	}
	maxHandle, isEmptyTable, err := GetTableMaxHandle(ctx, store, snapshotVer, tbl)
	if err != nil {
		return startHandleKey, nil, errors.Trace(err)
	}
	if maxHandle != nil {
		endHandleKey = tablecodec.EncodeRecordKey(tbl.RecordPrefix(), maxHandle).Next()
	}
	if isEmptyTable || endHandleKey.Cmp(startHandleKey) <= 0 {
		logutil.DDLLogger().Info("get noop table range",
			zap.String("table", fmt.Sprintf("%v", tbl.Meta())),
			zap.Int64("table/partition ID", tbl.GetPhysicalID()),
			zap.String("start key", hex.EncodeToString(startHandleKey)),
			zap.String("end key", hex.EncodeToString(endHandleKey)),
			zap.Bool("is empty table", isEmptyTable))
		if startHandleKey == nil {
			endHandleKey = nil
		} else {
			endHandleKey = startHandleKey.Next()
		}
	}
	return
}

func getValidCurrentVersion(store kv.Storage) (ver kv.Version, err error) {
	ver, err = store.CurrentVersion(kv.GlobalTxnScope)
	if err != nil {
		return ver, errors.Trace(err)
	} else if ver.Ver <= 0 {
		return ver, dbterror.ErrInvalidStoreVer.GenWithStack("invalid storage current version %d", ver.Ver)
	}
	return ver, nil
}

func getReorgInfo(ctx *ReorgContext, jobCtx *jobContext, rh *reorgHandler, job *model.Job, dbInfo *model.DBInfo,
	tbl table.Table, elements []*meta.Element, mergingTmpIdx bool) (*reorgInfo, error) {
	var (
		element *meta.Element
		start   kv.Key
		end     kv.Key
		pid     int64
		info    reorgInfo
	)

	if job.SnapshotVer == 0 {
		// For the case of the old TiDB version(do not exist the element information) is upgraded to the new TiDB version.
		// Third step, we need to remove the element information to make sure we can save the reorganized information to storage.
		failpoint.Inject("MockGetIndexRecordErr", func(val failpoint.Value) {
			if val.(string) == "addIdxNotOwnerErr" && atomic.CompareAndSwapUint32(&mockNotOwnerErrOnce, 3, 4) {
				if err := rh.RemoveReorgElementFailPoint(job); err != nil {
					failpoint.Return(nil, errors.Trace(err))
				}
				info.first = true
				failpoint.Return(&info, nil)
			}
		})

		info.first = true
		delayForAsyncCommit()
		ver, err := getValidCurrentVersion(jobCtx.store)
		if err != nil {
			return nil, errors.Trace(err)
		}
		tblInfo := tbl.Meta()
		pid = tblInfo.ID
		var tb table.PhysicalTable
		if pi := tblInfo.GetPartitionInfo(); pi != nil {
			pid = pi.Definitions[0].ID
			tb = tbl.(table.PartitionedTable).GetPartition(pid)
		} else {
			tb = tbl.(table.PhysicalTable)
		}
		if mergingTmpIdx {
			for _, element := range elements {
				if !bytes.Equal(element.TypeKey, meta.IndexElementKey) {
					continue
				}
				// If has a global index in elements, need start process at `tblInfo.ID`
				// because there are some temporary global indexes prefixed with table ID.
				idxInfo := model.FindIndexInfoByID(tblInfo.Indices, element.ID)
				if idxInfo.Global {
					pid = tblInfo.ID
				}
			}
			start, end = encodeTempIndexRange(pid, elements[0].ID, elements[len(elements)-1].ID)
		} else {
			start, end, err = getTableRange(ctx, jobCtx.store, tb, ver.Ver, job.Priority)
			if err != nil {
				return nil, errors.Trace(err)
			}
		}
		logutil.DDLLogger().Info("job get table range",
			zap.Int64("jobID", job.ID), zap.Int64("physicalTableID", pid),
			zap.String("startKey", hex.EncodeToString(start)),
			zap.String("endKey", hex.EncodeToString(end)))

		failpoint.Inject("errorUpdateReorgHandle", func() {
			failpoint.Return(&info, errors.New("occur an error when update reorg handle"))
		})
		err = rh.InitDDLReorgHandle(job, start, end, pid, elements[0])
		if err != nil {
			return &info, errors.Trace(err)
		}
		// Update info should after data persistent.
		job.SnapshotVer = ver.Ver
		element = elements[0]
	} else {
		failpoint.Inject("MockGetIndexRecordErr", func(val failpoint.Value) {
			// For the case of the old TiDB version(do not exist the element information) is upgraded to the new TiDB version.
			// Second step, we need to remove the element information to make sure we can get the error of "ErrDDLReorgElementNotExist".
			// However, since "txn.Reset()" will be called later, the reorganized information cannot be saved to storage.
			if val.(string) == "addIdxNotOwnerErr" && atomic.CompareAndSwapUint32(&mockNotOwnerErrOnce, 2, 3) {
				if err := rh.RemoveReorgElementFailPoint(job); err != nil {
					failpoint.Return(nil, errors.Trace(err))
				}
			}
		})

		var err error
		element, start, end, pid, err = rh.GetDDLReorgHandle(job)
		if err != nil {
			// If the reorg element doesn't exist, this reorg info should be saved by the older TiDB versions.
			// It's compatible with the older TiDB versions.
			// We'll try to remove it in the next major TiDB version.
			if meta.ErrDDLReorgElementNotExist.Equal(err) {
				job.SnapshotVer = 0
				logutil.DDLLogger().Warn("get reorg info, the element does not exist", zap.Stringer("job", job))
				if job.IsCancelling() {
					return nil, nil
				}
			}
			return &info, errors.Trace(err)
		}
	}
	info.Job = job
	info.jobCtx = jobCtx
	info.StartKey = start
	info.EndKey = end
	info.PhysicalTableID = pid
	info.currElement = element
	info.elements = elements
	info.mergingTmpIdx = mergingTmpIdx
	info.dbInfo = dbInfo

	return &info, nil
}

func getSplitKeysForTempIndexRanges(pid int64, elements []*meta.Element) []kv.Key {
	splitKeys := make([]kv.Key, 0, len(elements))
	for _, e := range elements {
		if !bytes.Equal(e.TypeKey, meta.IndexElementKey) {
			continue
		}
		tempIdxID := tablecodec.TempIndexPrefix | e.ID
		splitKey := tablecodec.EncodeIndexSeekKey(pid, tempIdxID, nil)
		splitKeys = append(splitKeys, splitKey)
	}
	return splitKeys
}

func encodeTempIndexRange(physicalID, firstIdxID, lastIdxID int64) (start kv.Key, end kv.Key) {
	firstElemTempID := tablecodec.TempIndexPrefix | firstIdxID
	lastElemTempID := tablecodec.TempIndexPrefix | lastIdxID
	start = tablecodec.EncodeIndexSeekKey(physicalID, firstElemTempID, nil)
	end = tablecodec.EncodeIndexSeekKey(physicalID, lastElemTempID, []byte{255})
	return start, end
}

func getReorgInfoFromPartitions(ctx *ReorgContext, jobCtx *jobContext, rh *reorgHandler, job *model.Job, dbInfo *model.DBInfo, tbl table.PartitionedTable, partitionIDs []int64, elements []*meta.Element) (*reorgInfo, error) {
	var (
		element *meta.Element
		start   kv.Key
		end     kv.Key
		pid     int64
		info    reorgInfo
	)
	if job.SnapshotVer == 0 {
		info.first = true
		delayForAsyncCommit()
		ver, err := getValidCurrentVersion(jobCtx.store)
		if err != nil {
			return nil, errors.Trace(err)
		}
		pid = partitionIDs[0]
		physTbl := tbl.GetPartition(pid)

		start, end, err = getTableRange(ctx, jobCtx.store, physTbl, ver.Ver, job.Priority)
		if err != nil {
			return nil, errors.Trace(err)
		}
		logutil.DDLLogger().Info("job get table range",
			zap.Int64("job ID", job.ID), zap.Int64("physical table ID", pid),
			zap.String("start key", hex.EncodeToString(start)),
			zap.String("end key", hex.EncodeToString(end)))

		err = rh.InitDDLReorgHandle(job, start, end, pid, elements[0])
		if err != nil {
			return &info, errors.Trace(err)
		}
		// Update info should after data persistent.
		job.SnapshotVer = ver.Ver
		element = elements[0]
	} else {
		var err error
		element, start, end, pid, err = rh.GetDDLReorgHandle(job)
		if err != nil {
			// If the reorg element doesn't exist, this reorg info should be saved by the older TiDB versions.
			// It's compatible with the older TiDB versions.
			// We'll try to remove it in the next major TiDB version.
			if meta.ErrDDLReorgElementNotExist.Equal(err) {
				job.SnapshotVer = 0
				logutil.DDLLogger().Warn("get reorg info, the element does not exist", zap.Stringer("job", job))
			}
			return &info, errors.Trace(err)
		}
	}
	info.Job = job
	info.jobCtx = jobCtx
	info.StartKey = start
	info.EndKey = end
	info.PhysicalTableID = pid
	info.currElement = element
	info.elements = elements
	info.dbInfo = dbInfo

	return &info, nil
}

// UpdateReorgMeta creates a new transaction and updates tidb_ddl_reorg table,
// so the reorg can restart in case of issues.
func (r *reorgInfo) UpdateReorgMeta(startKey kv.Key, pool *sess.Pool) (err error) {
	if startKey == nil && r.EndKey == nil {
		return nil
	}
	sctx, err := pool.Get()
	if err != nil {
		return
	}
	defer pool.Put(sctx)

	se := sess.NewSession(sctx)
	err = se.Begin(context.Background())
	if err != nil {
		return
	}
	rh := newReorgHandler(se)
	err = updateDDLReorgHandle(rh.s, r.Job.ID, startKey, r.EndKey, r.PhysicalTableID, r.currElement)
	err1 := se.Commit(context.Background())
	if err == nil {
		err = err1
	}
	return errors.Trace(err)
}

// reorgHandler is used to handle the reorg information duration reorganization DDL job.
type reorgHandler struct {
	s *sess.Session
}

// NewReorgHandlerForTest creates a new reorgHandler, only used in test.
func NewReorgHandlerForTest(se sessionctx.Context) *reorgHandler {
	return newReorgHandler(sess.NewSession(se))
}

func newReorgHandler(sess *sess.Session) *reorgHandler {
	return &reorgHandler{s: sess}
}

// InitDDLReorgHandle initializes the job reorganization information.
func (r *reorgHandler) InitDDLReorgHandle(job *model.Job, startKey, endKey kv.Key, physicalTableID int64, element *meta.Element) error {
	return initDDLReorgHandle(r.s, job.ID, startKey, endKey, physicalTableID, element)
}

// RemoveReorgElementFailPoint removes the element of the reorganization information.
func (r *reorgHandler) RemoveReorgElementFailPoint(job *model.Job) error {
	return removeReorgElement(r.s, job)
}

// RemoveDDLReorgHandle removes the job reorganization related handles.
func (r *reorgHandler) RemoveDDLReorgHandle(job *model.Job, elements []*meta.Element) error {
	return removeDDLReorgHandle(r.s, job, elements)
}

// cleanupDDLReorgHandles removes the job reorganization related handles.
func cleanupDDLReorgHandles(job *model.Job, s *sess.Session) {
	if job != nil && !job.IsFinished() && !job.IsSynced() {
		// Job is given, but it is neither finished nor synced; do nothing
		return
	}

	err := cleanDDLReorgHandles(s, job)
	if err != nil {
		// ignore error, cleanup is not that critical
		logutil.DDLLogger().Warn("Failed removing the DDL reorg entry in tidb_ddl_reorg", zap.Stringer("job", job), zap.Error(err))
	}
}

// GetDDLReorgHandle gets the latest processed DDL reorganize position.
func (r *reorgHandler) GetDDLReorgHandle(job *model.Job) (element *meta.Element, startKey, endKey kv.Key, physicalTableID int64, err error) {
	element, startKey, endKey, physicalTableID, err = getDDLReorgHandle(r.s, job)
	if err != nil {
		return element, startKey, endKey, physicalTableID, err
	}
	adjustedEndKey := adjustEndKeyAcrossVersion(job, endKey)
	return element, startKey, adjustedEndKey, physicalTableID, nil
}

// #46306 changes the table range from [start_key, end_key] to [start_key, end_key.next).
// For old version TiDB, the semantic is still [start_key, end_key], we need to adjust it in new version TiDB.
func adjustEndKeyAcrossVersion(job *model.Job, endKey kv.Key) kv.Key {
	if job.ReorgMeta != nil && job.ReorgMeta.Version == model.ReorgMetaVersion0 {
		logutil.DDLLogger().Info("adjust range end key for old version ReorgMetas",
			zap.Int64("jobID", job.ID),
			zap.Int64("reorgMetaVersion", job.ReorgMeta.Version),
			zap.String("endKey", hex.EncodeToString(endKey)))
		return endKey.Next()
	}
	return endKey
}
