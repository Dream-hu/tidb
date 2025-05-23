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

package privilege

import (
	"context"
	"fmt"

	"github.com/pingcap/tidb/pkg/parser/auth"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/privilege/conn"
	"github.com/pingcap/tidb/pkg/sessionctx"
	"github.com/pingcap/tidb/pkg/sessionctx/variable"
	"github.com/pingcap/tidb/pkg/types"
	"github.com/pingcap/tidb/pkg/util/sqlexec"
)

type keyType int

func (k keyType) String() string {
	return "privilege-key"
}

// VerificationInfo records some information returned by Manager.ConnectionVerification
type VerificationInfo struct {
	// InSandBoxMode indicates that the session will enter sandbox mode, and only execute statement for resetting password.
	InSandBoxMode bool
	// FailedDueToWrongPassword indicates that the verification failed due to wrong password.
	FailedDueToWrongPassword bool
	// ResourceGroupName records the resource group name for the user.
	ResourceGroupName string
}

// Manager is the interface for providing privilege related operations.
type Manager interface {
	// ShowGrants shows granted privileges for user.
	ShowGrants(ctx context.Context, sctx sessionctx.Context, user *auth.UserIdentity, roles []*auth.RoleIdentity) ([]string, error)

	// RequestVerification verifies user privilege for the request.
	// If table is "", only check global/db scope privileges.
	// If table is not "", check global/db/table scope privileges.
	// priv should be a defined constant like CreatePriv, if pass AllPrivMask to priv,
	// this means any privilege would be OK.
	RequestVerification(activeRole []*auth.RoleIdentity, db, table, column string, priv mysql.PrivilegeType) bool

	// RequestVerificationWithUser verifies specific user privilege for the request.
	RequestVerificationWithUser(ctx context.Context, db, table, column string, priv mysql.PrivilegeType, user *auth.UserIdentity) bool

	// HasExplicitlyGrantedDynamicPrivilege verifies is a user has a dynamic privilege granted
	// without using the SUPER privilege as a fallback.
	HasExplicitlyGrantedDynamicPrivilege(activeRoles []*auth.RoleIdentity, privName string, grantable bool) bool

	// RequestDynamicVerification verifies user privilege for a DYNAMIC privilege.
	// Dynamic privileges are only assignable globally, and have their own grantable attribute.
	RequestDynamicVerification(activeRoles []*auth.RoleIdentity, privName string, grantable bool) bool

	// RequestDynamicVerificationWithUser verifies a DYNAMIC privilege for a specific user.
	RequestDynamicVerificationWithUser(ctx context.Context, privName string, grantable bool, user *auth.UserIdentity) bool

	// VerifyAccountAutoLockInMemory automatically unlock when the time comes.
	VerifyAccountAutoLockInMemory(user string, host string) (bool, error)

	// IsAccountAutoLockEnabled verifies whether the account has enabled Failed-Login Tracking and Temporary Account Locking.
	IsAccountAutoLockEnabled(user string, host string) bool

	// ConnectionVerification verifies user privilege for connection.
	// Requires exact match on user name and host name.
	ConnectionVerification(user *auth.UserIdentity, authUser, authHost string, auth, salt []byte, sessionVars *variable.SessionVars, authConn conn.AuthConn) (VerificationInfo, error)

	// AuthSuccess records auth success state
	AuthSuccess(authUser, authHost string)

	// GetAuthWithoutVerification uses to get auth name without verification.
	// Requires exact match on user name and host name.
	GetAuthWithoutVerification(user, host string) bool

	// MatchIdentity matches an identity
	MatchIdentity(ctx context.Context, user, host string, skipNameResolve bool) (string, string, bool)

	// MatchUserResourceGroupName matches a user with specified resource group name
	MatchUserResourceGroupName(exec sqlexec.RestrictedSQLExecutor, resourceGroupName string) (string, bool)

	// DBIsVisible returns true is the database is visible to current user.
	DBIsVisible(activeRole []*auth.RoleIdentity, db string) bool

	// UserPrivilegesTable provide data for INFORMATION_SCHEMA.USER_PRIVILEGES table.
	UserPrivilegesTable(activeRoles []*auth.RoleIdentity, user, host string) [][]types.Datum

	// ActiveRoles active roles for current session.
	// The first illegal role will be returned.
	ActiveRoles(ctx context.Context, sctx sessionctx.Context, roleList []*auth.RoleIdentity) (bool, string)

	// FindEdge find if there is an edge between role and user.
	FindEdge(ctx context.Context, role *auth.RoleIdentity, user *auth.UserIdentity) bool

	// GetDefaultRoles returns all default roles for certain user.
	GetDefaultRoles(ctx context.Context, user, host string) []*auth.RoleIdentity

	// GetAllRoles return all roles of user.
	GetAllRoles(user, host string) []*auth.RoleIdentity

	// IsDynamicPrivilege returns if a privilege is in the list of privileges.
	IsDynamicPrivilege(privNameInUpper string) bool

	// GetAuthPluginForConnection gets the authentication plugin used in connection establishment.
	GetAuthPluginForConnection(ctx context.Context, user, host string) (string, error)

	//GetUserResources gets the max user connections for the account identified by the user and host
	GetUserResources(user, host string) (int64, error)
}

const key keyType = 0

// BindPrivilegeManager binds Manager to context.
func BindPrivilegeManager(ctx sessionctx.Context, pc Manager) {
	ctx.SetValue(key, pc)
}

type privilegeManagerKeyProvider interface {
	// Value returns the value associated with this context for key.
	Value(key fmt.Stringer) any
}

// GetPrivilegeManager gets Checker from context.
func GetPrivilegeManager(ctx privilegeManagerKeyProvider) Manager {
	if v, ok := ctx.Value(key).(Manager); ok {
		return v
	}
	return nil
}
