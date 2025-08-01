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

package executor

import (
	"context"
	"strings"
	"time"

	"github.com/pingcap/tidb/pkg/executor/internal/exec"
	"github.com/pingcap/tidb/pkg/expression"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/sessionctx"
	"github.com/pingcap/tidb/pkg/util/chunk"
)

// ReloadExprPushdownBlacklistExec indicates ReloadExprPushdownBlacklist executor.
type ReloadExprPushdownBlacklistExec struct {
	exec.BaseExecutor
}

// Next implements the Executor Next interface.
func (e *ReloadExprPushdownBlacklistExec) Next(context.Context, *chunk.Chunk) error {
	return LoadExprPushdownBlacklist(e.Ctx())
}

// LoadExprPushdownBlacklist loads the latest data from table mysql.expr_pushdown_blacklist.
func LoadExprPushdownBlacklist(sctx sessionctx.Context) (err error) {
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnSysVar)
	exec := sctx.GetRestrictedSQLExecutor()
	rows, _, err := exec.ExecRestrictedSQL(ctx, nil, "select HIGH_PRIORITY name, store_type from mysql.expr_pushdown_blacklist")
	if err != nil {
		return err
	}
	newBlocklist := make(map[string]uint32, len(rows))
	for _, row := range rows {
		name := strings.ToLower(row.GetString(0))
		storeTypeString := strings.ToLower(row.GetString(1))
		if alias, ok := funcName2Alias[name]; ok {
			name = alias
		}
		var value uint32
		if val, ok := newBlocklist[name]; ok {
			value = val
		}
		storeTypes := strings.Split(storeTypeString, ",")
		for _, typeString := range storeTypes {
			if typeString == kv.TiDB.Name() {
				value |= 1 << kv.TiDB
			} else if typeString == kv.TiFlash.Name() {
				value |= 1 << kv.TiFlash
			} else if typeString == kv.TiKV.Name() {
				value |= 1 << kv.TiKV
			}
		}
		newBlocklist[name] = value
	}
	if isSameExprPushDownBlackList(newBlocklist, *expression.DefaultExprPushDownBlacklist.Load()) {
		return nil
	}
	expression.ExprPushDownBlackListReloadTimeStamp.Store(time.Now().UnixNano())
	expression.DefaultExprPushDownBlacklist.Store(&newBlocklist)
	return nil
}

// isSameExprPushDownBlackList checks whether two exprPushDownBlacklist are the same.
func isSameExprPushDownBlackList(l1, l2 map[string]uint32) bool {
	if len(l1) != len(l2) {
		return false
	}
	for k, v1 := range l1 {
		v2, ok := l2[k]
		if !ok || v1 != v2 {
			return false
		}
	}
	return true
}

// funcName2Alias indicates map of the origin function name to the name used in TiDB.
var funcName2Alias = map[string]string{
	"and":                        ast.LogicAnd,
	"cast":                       ast.Cast,
	"<<":                         ast.LeftShift,
	">>":                         ast.RightShift,
	"or":                         ast.LogicOr,
	">=":                         ast.GE,
	"<=":                         ast.LE,
	"=":                          ast.EQ,
	"!=":                         ast.NE,
	"<>":                         ast.NE,
	"<":                          ast.LT,
	">":                          ast.GT,
	"+":                          ast.Plus,
	"-":                          ast.Minus,
	"&&":                         ast.And,
	"||":                         ast.Or,
	"%":                          ast.Mod,
	"xor_bit":                    ast.Xor,
	"/":                          ast.Div,
	"*":                          ast.Mul,
	"!":                          ast.UnaryNot,
	"~":                          ast.BitNeg,
	"div":                        ast.IntDiv,
	"xor_logic":                  ast.LogicXor, // Avoid name conflict with "xor_bit".,
	"<=>":                        ast.NullEQ,
	"+_unary":                    ast.UnaryPlus, // Avoid name conflict with `plus`.,
	"-_unary":                    ast.UnaryMinus,
	"in":                         ast.In,
	"like":                       ast.Like,
	"case":                       ast.Case,
	"regexp":                     ast.Regexp,
	"is null":                    ast.IsNull,
	"is true":                    ast.IsTruthWithoutNull,
	"is false":                   ast.IsFalsity,
	"values":                     ast.Values,
	"bit_count":                  ast.BitCount,
	"coalesce":                   ast.Coalesce,
	"greatest":                   ast.Greatest,
	"least":                      ast.Least,
	"interval":                   ast.Interval,
	"abs":                        ast.Abs,
	"acos":                       ast.Acos,
	"asin":                       ast.Asin,
	"atan":                       ast.Atan,
	"atan2":                      ast.Atan2,
	"ceil":                       ast.Ceil,
	"ceiling":                    ast.Ceiling,
	"conv":                       ast.Conv,
	"cos":                        ast.Cos,
	"cot":                        ast.Cot,
	"crc32":                      ast.CRC32,
	"degrees":                    ast.Degrees,
	"exp":                        ast.Exp,
	"floor":                      ast.Floor,
	"ln":                         ast.Ln,
	"log":                        ast.Log,
	"log2":                       ast.Log2,
	"log10":                      ast.Log10,
	"pi":                         ast.PI,
	"pow":                        ast.Pow,
	"power":                      ast.Power,
	"radians":                    ast.Radians,
	"rand":                       ast.Rand,
	"round":                      ast.Round,
	"sign":                       ast.Sign,
	"sin":                        ast.Sin,
	"sqrt":                       ast.Sqrt,
	"tan":                        ast.Tan,
	"truncate":                   ast.Truncate,
	"adddate":                    ast.AddDate,
	"addtime":                    ast.AddTime,
	"convert_tz":                 ast.ConvertTz,
	"curdate":                    ast.Curdate,
	"current_date":               ast.CurrentDate,
	"current_time":               ast.CurrentTime,
	"current_timestamp":          ast.CurrentTimestamp,
	"curtime":                    ast.Curtime,
	"date":                       ast.Date,
	"date_add":                   ast.DateAdd,
	"date_format":                ast.DateFormat,
	"date_sub":                   ast.DateSub,
	"datediff":                   ast.DateDiff,
	"day":                        ast.Day,
	"dayname":                    ast.DayName,
	"dayofmonth":                 ast.DayOfMonth,
	"dayofweek":                  ast.DayOfWeek,
	"dayofyear":                  ast.DayOfYear,
	"extract":                    ast.Extract,
	"from_days":                  ast.FromDays,
	"from_unixtime":              ast.FromUnixTime,
	"get_format":                 ast.GetFormat,
	"hour":                       ast.Hour,
	"localtime":                  ast.LocalTime,
	"localtimestamp":             ast.LocalTimestamp,
	"makedate":                   ast.MakeDate,
	"maketime":                   ast.MakeTime,
	"microsecond":                ast.MicroSecond,
	"minute":                     ast.Minute,
	"month":                      ast.Month,
	"monthname":                  ast.MonthName,
	"now":                        ast.Now,
	"period_add":                 ast.PeriodAdd,
	"period_diff":                ast.PeriodDiff,
	"quarter":                    ast.Quarter,
	"sec_to_time":                ast.SecToTime,
	"second":                     ast.Second,
	"str_to_date":                ast.StrToDate,
	"subdate":                    ast.SubDate,
	"subtime":                    ast.SubTime,
	"sysdate":                    ast.Sysdate,
	"time":                       ast.Time,
	"time_format":                ast.TimeFormat,
	"time_to_sec":                ast.TimeToSec,
	"timediff":                   ast.TimeDiff,
	"timestamp":                  ast.Timestamp,
	"timestampadd":               ast.TimestampAdd,
	"timestampdiff":              ast.TimestampDiff,
	"to_days":                    ast.ToDays,
	"to_seconds":                 ast.ToSeconds,
	"unix_timestamp":             ast.UnixTimestamp,
	"utc_date":                   ast.UTCDate,
	"utc_time":                   ast.UTCTime,
	"utc_timestamp":              ast.UTCTimestamp,
	"week":                       ast.Week,
	"weekday":                    ast.Weekday,
	"weekofyear":                 ast.WeekOfYear,
	"year":                       ast.Year,
	"yearweek":                   ast.YearWeek,
	"last_day":                   ast.LastDay,
	"ascii":                      ast.ASCII,
	"bin":                        ast.Bin,
	"concat":                     ast.Concat,
	"concat_ws":                  ast.ConcatWS,
	"convert":                    ast.Convert,
	"elt":                        ast.Elt,
	"export_set":                 ast.ExportSet,
	"field":                      ast.Field,
	"format":                     ast.Format,
	"from_base64":                ast.FromBase64,
	"insert_func":                ast.InsertFunc,
	"instr":                      ast.Instr,
	"lcase":                      ast.Lcase,
	"left":                       ast.Left,
	"length":                     ast.Length,
	"load_file":                  ast.LoadFile,
	"locate":                     ast.Locate,
	"lower":                      ast.Lower,
	"lpad":                       ast.Lpad,
	"ltrim":                      ast.LTrim,
	"make_set":                   ast.MakeSet,
	"mid":                        ast.Mid,
	"oct":                        ast.Oct,
	"octet_length":               ast.OctetLength,
	"ord":                        ast.Ord,
	"position":                   ast.Position,
	"quote":                      ast.Quote,
	"repeat":                     ast.Repeat,
	"replace":                    ast.Replace,
	"reverse":                    ast.Reverse,
	"right":                      ast.Right,
	"rtrim":                      ast.RTrim,
	"space":                      ast.Space,
	"strcmp":                     ast.Strcmp,
	"substring":                  ast.Substring,
	"substr":                     ast.Substr,
	"substring_index":            ast.SubstringIndex,
	"to_base64":                  ast.ToBase64,
	"trim":                       ast.Trim,
	"upper":                      ast.Upper,
	"ucase":                      ast.Ucase,
	"hex":                        ast.Hex,
	"unhex":                      ast.Unhex,
	"rpad":                       ast.Rpad,
	"bit_length":                 ast.BitLength,
	"char_func":                  ast.CharFunc,
	"char_length":                ast.CharLength,
	"character_length":           ast.CharacterLength,
	"find_in_set":                ast.FindInSet,
	"benchmark":                  ast.Benchmark,
	"charset":                    ast.Charset,
	"coercibility":               ast.Coercibility,
	"collation":                  ast.Collation,
	"connection_id":              ast.ConnectionID,
	"current_user":               ast.CurrentUser,
	"current_resource_group":     ast.CurrentResourceGroup,
	"current_role":               ast.CurrentRole,
	"database":                   ast.Database,
	"found_rows":                 ast.FoundRows,
	"last_insert_id":             ast.LastInsertId,
	"row_count":                  ast.RowCount,
	"schema":                     ast.Schema,
	"session_user":               ast.SessionUser,
	"system_user":                ast.SystemUser,
	"user":                       ast.User,
	"if":                         ast.If,
	"ifnull":                     ast.Ifnull,
	"nullif":                     ast.Nullif,
	"any_value":                  ast.AnyValue,
	"default_func":               ast.DefaultFunc,
	"inet_aton":                  ast.InetAton,
	"inet_ntoa":                  ast.InetNtoa,
	"inet6_aton":                 ast.Inet6Aton,
	"inet6_ntoa":                 ast.Inet6Ntoa,
	"is_free_lock":               ast.IsFreeLock,
	"is_ipv4":                    ast.IsIPv4,
	"is_ipv4_compat":             ast.IsIPv4Compat,
	"is_ipv4_mapped":             ast.IsIPv4Mapped,
	"is_ipv6":                    ast.IsIPv6,
	"is_used_lock":               ast.IsUsedLock,
	"name_const":                 ast.NameConst,
	"release_all_locks":          ast.ReleaseAllLocks,
	"sleep":                      ast.Sleep,
	"uuid":                       ast.UUID,
	"uuid_short":                 ast.UUIDShort,
	"get_lock":                   ast.GetLock,
	"release_lock":               ast.ReleaseLock,
	"aes_decrypt":                ast.AesDecrypt,
	"aes_encrypt":                ast.AesEncrypt,
	"compress":                   ast.Compress,
	"decode":                     ast.Decode,
	"encode":                     ast.Encode,
	"md5":                        ast.MD5,
	"password":                   ast.PasswordFunc,
	"random_bytes":               ast.RandomBytes,
	"sha1":                       ast.SHA1,
	"sha":                        ast.SHA,
	"sha2":                       ast.SHA2,
	"sm3":                        ast.SM3,
	"uncompress":                 ast.Uncompress,
	"uncompressed_length":        ast.UncompressedLength,
	"validate_password_strength": ast.ValidatePasswordStrength,
	"json_type":                  ast.JSONType,
	"json_extract":               ast.JSONExtract,
	"json_unquote":               ast.JSONUnquote,
	"json_array":                 ast.JSONArray,
	"json_object":                ast.JSONObject,
	"json_merge":                 ast.JSONMerge,
	"json_set":                   ast.JSONSet,
	"json_insert":                ast.JSONInsert,
	"json_replace":               ast.JSONReplace,
	"json_remove":                ast.JSONRemove,
	"json_contains":              ast.JSONContains,
	"json_contains_path":         ast.JSONContainsPath,
	"json_valid":                 ast.JSONValid,
	"json_array_append":          ast.JSONArrayAppend,
	"json_array_insert":          ast.JSONArrayInsert,
	"json_merge_patch":           ast.JSONMergePatch,
	"json_merge_preserve":        ast.JSONMergePreserve,
	"json_pretty":                ast.JSONPretty,
	"json_quote":                 ast.JSONQuote,
	"json_schema_valid":          ast.JSONSchemaValid,
	"json_search":                ast.JSONSearch,
	"json_storage_size":          ast.JSONStorageSize,
	"json_depth":                 ast.JSONDepth,
	"json_keys":                  ast.JSONKeys,
	"json_length":                ast.JSONLength,
	"vec_dims":                   ast.VecDims,
	"vec_l1_distance":            ast.VecL1Distance,
	"vec_l2_distance":            ast.VecL2Distance,
	"vec_negative_inner_product": ast.VecNegativeInnerProduct,
	"vec_cosine_distance":        ast.VecCosineDistance,
	"vec_l2_norm":                ast.VecL2Norm,
	"vec_from_text":              ast.VecFromText,
	"vec_as_text":                ast.VecAsText,
}
