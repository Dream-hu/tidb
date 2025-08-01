// Copyright 2019 PingCAP, Inc.
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

package lock

import (
	stdctx "context"
	"errors"

	"github.com/pingcap/tidb/pkg/infoschema"
	infoschemacontext "github.com/pingcap/tidb/pkg/infoschema/context"
	"github.com/pingcap/tidb/pkg/lock/context"
	"github.com/pingcap/tidb/pkg/meta/metadef"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/table"
)

// Checker uses to check tables lock.
type Checker struct {
	ctx context.TableLockReadContext
	is  infoschema.InfoSchema
}

// ErrLockedTableDropped returns error when try to drop the table with write lock
var ErrLockedTableDropped = errors.New("other table can be accessed after locked table dropped")

// NewChecker return new lock Checker.
func NewChecker(ctx context.TableLockReadContext, is infoschema.InfoSchema) *Checker {
	return &Checker{ctx: ctx, is: is}
}

// CheckTableLock uses to check table lock.
func (c *Checker) CheckTableLock(db, table string, privilege mysql.PrivilegeType, alterWriteable bool) error {
	if db == "" && table == "" || privilege == mysql.LockTablesPriv {
		return nil
	}
	// System DB and memory DB are not support table lock.
	if metadef.IsMemOrSysDB(db) {
		return nil
	}
	// check operation on database.
	if !alterWriteable && table == "" {
		return c.CheckLockInDB(db, privilege)
	}

	switch privilege {
	case mysql.ShowDBPriv, mysql.AllPrivMask:
		// AllPrivMask only used in show create table statement now.
		return nil
	case mysql.CreatePriv, mysql.CreateViewPriv:
		if c.ctx.HasLockedTables() {
			// TODO: For `create table t_exists ...` statement, mysql will check out `t_exists` first, but in TiDB now,
			//  will return below error first.
			return infoschema.ErrTableNotLocked.GenWithStackByArgs(table)
		}
		return nil
	}
	// TODO: try to remove this get for speed up.
	tb, err := c.is.TableByName(stdctx.Background(), ast.NewCIStr(db), ast.NewCIStr(table))
	// Ignore this error for "drop table if not exists t1" when t1 doesn't exists.
	if infoschema.ErrTableNotExists.Equal(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if tb.Meta().Lock == nil {
		return nil
	}
	if privilege == mysql.DropPriv && tb.Meta().Name.O == table && c.ctx.HasLockedTables() {
		lockTables := c.ctx.GetAllTableLocks()
		for _, lockT := range lockTables {
			if lockT.TableID == tb.Meta().ID {
				switch tb.Meta().Lock.Tp {
				case ast.TableLockWrite:
					return ErrLockedTableDropped
				case ast.TableLockRead, ast.TableLockWriteLocal, ast.TableLockReadOnly:
					return infoschema.ErrTableNotLockedForWrite.GenWithStackByArgs(tb.Meta().Name)
				}
			}
		}
	}

	if !alterWriteable && c.ctx.HasLockedTables() {
		if locked, tp := c.ctx.CheckTableLocked(tb.Meta().ID); locked {
			if checkLockTpMeetPrivilege(tp, privilege) {
				return nil
			}
			return infoschema.ErrTableNotLockedForWrite.GenWithStackByArgs(tb.Meta().Name)
		}
		return infoschema.ErrTableNotLocked.GenWithStackByArgs(tb.Meta().Name)
	}

	if privilege == mysql.SelectPriv {
		switch tb.Meta().Lock.Tp {
		case ast.TableLockRead, ast.TableLockWriteLocal, ast.TableLockReadOnly:
			return nil
		}
	}
	if alterWriteable && tb.Meta().Lock.Tp == ast.TableLockReadOnly {
		return nil
	}

	return infoschema.ErrTableLocked.GenWithStackByArgs(tb.Meta().Name.L, tb.Meta().Lock.Tp, tb.Meta().Lock.Sessions[0])
}

func checkLockTpMeetPrivilege(tp ast.TableLockType, privilege mysql.PrivilegeType) bool {
	// TableLockReadOnly doesn't need to check in this, because it is session unrelated.
	switch tp {
	case ast.TableLockWrite, ast.TableLockWriteLocal:
		return true
	case ast.TableLockRead:
		// ShowDBPriv, AllPrivMask, CreatePriv, CreateViewPriv already checked before.
		// The other privilege in read lock was not allowed.
		if privilege == mysql.SelectPriv {
			return true
		}
	}
	return false
}

// CheckLockInDB uses to check operation on database.
func (c *Checker) CheckLockInDB(db string, privilege mysql.PrivilegeType) error {
	if c.ctx.HasLockedTables() {
		switch privilege {
		case mysql.CreatePriv, mysql.DropPriv, mysql.AlterPriv:
			return table.ErrLockOrActiveTransaction.GenWithStackByArgs()
		}
	}
	if privilege == mysql.CreatePriv {
		return nil
	}
	rs := c.is.ListTablesWithSpecialAttribute(infoschemacontext.TableLockAttribute)
	for _, schema := range rs {
		for _, tbl := range schema.TableInfos {
			err := c.CheckTableLock(db, tbl.Name.L, privilege, false)
			if err != nil {
				return err
			}
		}
	}
	return nil
}
