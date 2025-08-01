// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package gluetidb

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/log"
	"github.com/pingcap/tidb/br/pkg/glue"
	"github.com/pingcap/tidb/br/pkg/gluetikv"
	"github.com/pingcap/tidb/br/pkg/logutil"
	"github.com/pingcap/tidb/pkg/config"
	"github.com/pingcap/tidb/pkg/ddl"
	"github.com/pingcap/tidb/pkg/domain"
	"github.com/pingcap/tidb/pkg/executor"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/meta/model"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/session"
	"github.com/pingcap/tidb/pkg/session/sessionapi"
	"github.com/pingcap/tidb/pkg/sessionctx"
	pd "github.com/tikv/pd/client"
	"go.uber.org/zap"
)

// Asserting Glue implements glue.ConsoleGlue and glue.Glue at compile time.
var (
	_ glue.ConsoleGlue = Glue{}
	_ glue.Glue        = Glue{}
)

const brComment = `/*from(br)*/`

// New makes a new tidb glue.
func New() Glue {
	log.Debug("enabling no register config")
	config.UpdateGlobal(func(conf *config.Config) {
		conf.SkipRegisterToDashboard = true
		conf.Log.EnableSlowLog.Store(false)
		conf.TiKVClient.CoprReqTimeout = 1800 * time.Second
	})
	return Glue{
		startDomainMu: &sync.Mutex{},
	}
}

// Glue is an implementation of glue.Glue using a new TiDB session.
type Glue struct {
	glue.StdIOGlue

	tikvGlue      gluetikv.Glue
	startDomainMu *sync.Mutex
}

func WrapSession(se sessionapi.Session) glue.Session {
	return &tidbSession{se: se}
}

type tidbSession struct {
	se sessionapi.Session
}

// GetDomain implements glue.Glue.
func (g Glue) GetDomain(store kv.Storage) (*domain.Domain, error) {
	existDom, _ := session.GetDomain(nil)
	initStatsSe, err := g.createTypesSession(store)
	if err != nil {
		return nil, errors.Trace(err)
	}
	dom, err := session.GetDomain(store)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if existDom == nil {
		err = session.InitMDLVariable(store)
		if err != nil {
			return nil, err
		}
		// create stats handler for backup and restore.
		err = dom.UpdateTableStatsLoop(initStatsSe)
		if err != nil {
			return nil, errors.Trace(err)
		}
	}
	return dom, nil
}

// CreateSession implements glue.Glue.
func (g Glue) CreateSession(store kv.Storage) (glue.Session, error) {
	se, err := g.createTypesSession(store)
	if err != nil {
		return nil, errors.Trace(err)
	}
	tiSession := &tidbSession{
		se: se,
	}
	return tiSession, nil
}

func (g Glue) startDomainAsNeeded(store kv.Storage) error {
	g.startDomainMu.Lock()
	defer g.startDomainMu.Unlock()
	existDom, _ := session.GetDomain(nil)
	if existDom != nil {
		return nil
	}
	if err := ddl.StartOwnerManager(context.Background(), store); err != nil {
		return errors.Trace(err)
	}
	dom, err := session.GetDomain(store)
	if err != nil {
		return err
	}
	return dom.Start(ddl.Normal)
}

func (g Glue) createTypesSession(store kv.Storage) (sessionapi.Session, error) {
	if err := g.startDomainAsNeeded(store); err != nil {
		return nil, errors.Trace(err)
	}
	return session.CreateSession(store)
}

// Open implements glue.Glue.
func (g Glue) Open(path string, option pd.SecurityOption) (kv.Storage, error) {
	return g.tikvGlue.Open(path, option)
}

// OwnsStorage implements glue.Glue.
func (Glue) OwnsStorage() bool {
	return true
}

// StartProgress implements glue.Glue.
func (g Glue) StartProgress(ctx context.Context, cmdName string, total int64, redirectLog bool) glue.Progress {
	return g.tikvGlue.StartProgress(ctx, cmdName, total, redirectLog)
}

// Record implements glue.Glue.
func (g Glue) Record(name string, value uint64) {
	g.tikvGlue.Record(name, value)
}

// GetVersion implements glue.Glue.
func (g Glue) GetVersion() string {
	return g.tikvGlue.GetVersion()
}

// UseOneShotSession implements glue.Glue.
func (g Glue) UseOneShotSession(store kv.Storage, closeDomain bool, fn func(glue.Session) error) error {
	se, err := g.createTypesSession(store)
	if err != nil {
		return errors.Trace(err)
	}
	glueSession := &tidbSession{
		se: se,
	}
	defer func() {
		se.Close()
		log.Info("one shot session closed")
	}()
	// dom will be created during create session.
	dom, err := session.GetDomain(store)
	if err != nil {
		return errors.Trace(err)
	}
	if err = session.InitMDLVariable(store); err != nil {
		return errors.Trace(err)
	}

	// because domain was created during the whole program exists.
	// and it will register br info to info syncer.
	// we'd better close it as soon as possible.
	if closeDomain {
		defer func() {
			dom.Close()
			log.Info("one shot domain closed")
		}()
	}
	err = fn(glueSession)
	if err != nil {
		return errors.Trace(err)
	}
	return nil
}

func (Glue) GetClient() glue.GlueClient {
	return glue.ClientCLP
}

// GetSessionCtx implements glue.Glue
func (gs *tidbSession) GetSessionCtx() sessionctx.Context {
	return gs.se
}

// Execute implements glue.Session.
func (gs *tidbSession) Execute(ctx context.Context, sql string) error {
	return gs.ExecuteInternal(ctx, sql)
}

func (gs *tidbSession) ExecuteInternal(ctx context.Context, sql string, args ...any) error {
	ctx = kv.WithInternalSourceType(ctx, kv.InternalTxnBR)
	rs, err := gs.se.ExecuteInternal(ctx, sql, args...)
	if err != nil {
		return errors.Trace(err)
	}
	defer func() {
		vars := gs.se.GetSessionVars()
		vars.TxnCtxMu.Lock()
		vars.TxnCtx.InfoSchema = nil
		vars.TxnCtxMu.Unlock()
	}()
	// Some of SQLs (like ADMIN RECOVER INDEX) may lazily take effect
	// when we are polling the result set.
	// At least call `next` once for triggering theirs side effect.
	// (Maybe we'd better drain all returned rows?)
	if rs != nil {
		//nolint: errcheck
		defer rs.Close()
		c := rs.NewChunk(nil)
		if err := rs.Next(ctx, c); err != nil {
			log.Warn("Error during draining result of internal sql.",
				logutil.Redact(zap.String("sql", sql)), logutil.ShortError(err))
			return nil
		}
	}
	return nil
}

// CreateDatabase implements glue.Session.
func (gs *tidbSession) CreateDatabase(ctx context.Context, schema *model.DBInfo) error {
	return errors.Trace(executor.BRIECreateDatabase(gs.se, schema, brComment))
}

// CreatePlacementPolicy implements glue.Session.
func (gs *tidbSession) CreatePlacementPolicy(ctx context.Context, policy *model.PolicyInfo) error {
	d := domain.GetDomain(gs.se).DDLExecutor()
	gs.se.SetValue(sessionctx.QueryString, gs.showCreatePlacementPolicy(policy))
	// the default behaviour is ignoring duplicated policy during restore.
	return d.CreatePlacementPolicyWithInfo(gs.se, policy, ddl.OnExistIgnore)
}

// CreateTables implements glue.BatchCreateTableSession.
func (gs *tidbSession) CreateTables(_ context.Context,
	tables map[string][]*model.TableInfo, cs ...ddl.CreateTableOption) error {
	return errors.Trace(executor.BRIECreateTables(gs.se, tables, brComment, cs...))
}

// CreateTable implements glue.Session.
func (gs *tidbSession) CreateTable(_ context.Context, dbName ast.CIStr,
	table *model.TableInfo, cs ...ddl.CreateTableOption) error {
	return errors.Trace(executor.BRIECreateTable(gs.se, dbName, table, brComment, cs...))
}

// Close implements glue.Session.
func (gs *tidbSession) Close() {
	gs.se.Close()
}

// GetGlobalVariable implements glue.Session.
func (gs *tidbSession) GetGlobalVariable(name string) (string, error) {
	return gs.se.GetSessionVars().GlobalVarsAccessor.GetTiDBTableValue(name)
}

// GetGlobalSysVar gets the global system variable value for name.
func (gs *tidbSession) GetGlobalSysVar(name string) (string, error) {
	return gs.se.GetSessionVars().GlobalVarsAccessor.GetGlobalSysVar(name)
}

func (gs *tidbSession) showCreatePlacementPolicy(policy *model.PolicyInfo) string {
	return executor.ConstructResultOfShowCreatePlacementPolicy(policy)
}

func (gs *tidbSession) AlterTableMode(
	_ context.Context,
	schemaID int64,
	tableID int64,
	tableMode model.TableMode) error {
	originQueryString := gs.se.Value(sessionctx.QueryString)
	defer gs.se.SetValue(sessionctx.QueryString, originQueryString)
	d := domain.GetDomain(gs.se).DDLExecutor()
	gs.se.SetValue(sessionctx.QueryString,
		fmt.Sprintf("ALTER TABLE MODE SCHEMA_ID=%d TABLE_ID=%d TO %s", schemaID, tableID, tableMode.String()))
	args := &model.AlterTableModeArgs{
		SchemaID:  schemaID,
		TableID:   tableID,
		TableMode: tableMode,
	}
	return d.AlterTableMode(gs.se, args)
}

// RefreshMeta submits a refresh meta job to update the info schema with the latest metadata.
func (gs *tidbSession) RefreshMeta(
	_ context.Context,
	args *model.RefreshMetaArgs) error {
	originQueryString := gs.se.Value(sessionctx.QueryString)
	defer gs.se.SetValue(sessionctx.QueryString, originQueryString)
	d := domain.GetDomain(gs.se).DDLExecutor()
	gs.se.SetValue(sessionctx.QueryString,
		fmt.Sprintf("REFRESH META SCHEMA_ID=%d TABLE_ID=%d INVOLVED_DB=%s INVOLVED_TABLE=%s",
			args.SchemaID, args.TableID, args.InvolvedDB, args.InvolvedTable))
	return d.RefreshMeta(gs.se, args)
}
