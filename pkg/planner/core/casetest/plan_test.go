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

package casetest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pingcap/tidb/pkg/domain"
	"github.com/pingcap/tidb/pkg/errno"
	"github.com/pingcap/tidb/pkg/meta/model"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/planner/core"
	"github.com/pingcap/tidb/pkg/planner/core/base"
	"github.com/pingcap/tidb/pkg/testkit"
	"github.com/pingcap/tidb/pkg/testkit/testdata"
	"github.com/pingcap/tidb/pkg/util/plancodec"
	"github.com/stretchr/testify/require"
)

func getPlanRows(planStr string) []string {
	planStr = strings.Replace(planStr, "\t", " ", -1)
	return strings.Split(planStr, "\n")
}

func compareStringSlice(t *testing.T, ss1, ss2 []string) {
	require.Equal(t, len(ss1), len(ss2))
	for i, s := range ss1 {
		require.Equal(t, len(s), len(ss2[i]))
	}
}

func TestPreferRangeScan(t *testing.T) {
	testkit.RunTestUnderCascades(t, func(t *testing.T, testKit *testkit.TestKit, cascades, caller string) {
		testKit.MustExec("use test")
		testKit.MustExec(`set @@tidb_enable_non_prepared_plan_cache=0`) // affect this ut: tidb_opt_prefer_range_scan
		testKit.MustExec("drop table if exists test;")
		testKit.MustExec("create table test(`id` int(10) NOT NULL AUTO_INCREMENT,`name` varchar(50) NOT NULL DEFAULT 'tidb',`age` int(11) NOT NULL,`addr` varchar(50) DEFAULT 'The ocean of stars',PRIMARY KEY (`id`),KEY `idx_age` (`age`))")
		testKit.MustExec("insert into test(age) values(5);")
		testKit.MustExec("insert into test(name,age,addr) select name,age,addr from test;")
		testKit.MustExec("insert into test(name,age,addr) select name,age,addr from test;")
		testKit.MustExec("insert into test(name,age,addr) select name,age,addr from test;")
		testKit.MustExec("insert into test(name,age,addr) select name,age,addr from test;")
		testKit.MustExec("insert into test(name,age,addr) select name,age,addr from test;")
		testKit.MustExec("insert into test(name,age,addr) select name,age,addr from test;")
		testKit.MustExec("insert into test(name,age,addr) select name,age,addr from test;")
		testKit.MustExec("insert into test(name,age,addr) select name,age,addr from test;")
		testKit.MustExec("insert into test(name,age,addr) select name,age,addr from test;")
		testKit.MustExec("insert into test(name,age,addr) select name,age,addr from test;")
		testKit.MustExec("insert into test(name,age,addr) select name,age,addr from test;")
		testKit.MustExec("analyze table test;")

		// Default RPC encoding may cause statistics explain result differ and then the test unstable.
		testKit.MustExec("set @@tidb_enable_chunk_rpc = on")

		var input []string
		var output []struct {
			SQL  string
			Plan []string
		}
		planNormalizedSuiteData := GetPlanNormalizedSuiteData()
		planNormalizedSuiteData.LoadTestCases(t, &input, &output, cascades, caller)
		for i, tt := range input {
			switch i {
			case 0:
				testKit.MustExec("set session tidb_opt_prefer_range_scan=0")
			case 1:
				testKit.MustExec("set session tidb_opt_prefer_range_scan=1")
			}
			testKit.Session().GetSessionVars().PlanID.Store(0)
			testKit.MustExec(tt)
			info := testKit.Session().ShowProcess()
			require.NotNil(t, info)
			p, ok := info.Plan.(base.Plan)
			require.True(t, ok)
			normalized, digest := core.NormalizePlan(p)

			// test the new normalization code
			flat := core.FlattenPhysicalPlan(p, false)
			newNormalized, newDigest := core.NormalizeFlatPlan(flat)
			require.Equal(t, normalized, newNormalized)
			require.Equal(t, digest, newDigest)

			normalizedPlan, err := plancodec.DecodeNormalizedPlan(normalized)
			normalizedPlanRows := getPlanRows(normalizedPlan)
			require.NoError(t, err)
			testdata.OnRecord(func() {
				output[i].SQL = tt
				output[i].Plan = normalizedPlanRows
			})
			compareStringSlice(t, normalizedPlanRows, output[i].Plan)
		}
	})
}

func TestNormalizedPlan(t *testing.T) {
	testkit.RunTestUnderCascades(t, func(t *testing.T, testKit *testkit.TestKit, cascades, caller string) {
		testKit.MustExec("use test")
		testKit.MustExec("set @@tidb_partition_prune_mode='static';")
		testKit.MustExec("drop table if exists t1,t2,t3,t4")
		testKit.MustExec("create table t1 (a int key,b int,c int, index (b));")
		testKit.MustExec("create table t2 (a int key,b int,c int, index (b));")
		testKit.MustExec("create table t3 (a int key,b int) partition by hash(a) partitions 2;")
		testKit.MustExec("create table t4 (a int, b int, index(a)) partition by range(a) (partition p0 values less than (10),partition p1 values less than MAXVALUE);")
		testKit.MustExec("set @@global.tidb_enable_foreign_key=1")
		testKit.MustExec("set @@foreign_key_checks=1")
		testKit.MustExec("create table t5 (id int key, id2 int, id3 int, unique index idx2(id2), index idx3(id3));")
		testKit.MustExec("create table t6 (id int,     id2 int, id3 int, index idx_id(id), index idx_id2(id2), " +
			"foreign key fk_1 (id) references t5(id) ON UPDATE CASCADE ON DELETE CASCADE, " +
			"foreign key fk_2 (id2) references t5(id2) ON UPDATE CASCADE, " +
			"foreign key fk_3 (id3) references t5(id3) ON DELETE CASCADE);")
		testKit.MustExec("insert into t5 values (1,1,1), (2,2,2)")
		var input []string
		var output []struct {
			SQL  string
			Plan []string
		}
		planNormalizedSuiteData := GetPlanNormalizedSuiteData()
		planNormalizedSuiteData.LoadTestCases(t, &input, &output, cascades, caller)
		for i, tt := range input {
			testKit.Session().GetSessionVars().PlanID.Store(0)
			testKit.MustExec(tt)
			info := testKit.Session().ShowProcess()
			require.NotNil(t, info)
			p, ok := info.Plan.(base.Plan)
			require.True(t, ok)
			normalized, digest := core.NormalizePlan(p)

			// test the new normalization code
			flat := core.FlattenPhysicalPlan(p, false)
			newNormalized, newDigest := core.NormalizeFlatPlan(flat)
			require.Equal(t, normalized, newNormalized)
			require.Equal(t, digest, newDigest)
			// Test for GenHintsFromFlatPlan won't panic.
			core.GenHintsFromFlatPlan(flat)

			normalizedPlan, err := plancodec.DecodeNormalizedPlan(normalized)
			normalizedPlanRows := getPlanRows(normalizedPlan)
			require.NoError(t, err)
			testdata.OnRecord(func() {
				output[i].SQL = tt
				output[i].Plan = normalizedPlanRows
			})
			compareStringSlice(t, normalizedPlanRows, output[i].Plan)
		}
	})
}

func TestPlanDigest4InList(t *testing.T) {
	testkit.RunTestUnderCascades(t, func(t *testing.T, testKit *testkit.TestKit, cascades, caller string) {
		testKit.MustExec("use test")
		testKit.MustExec("drop table if exists t")
		testKit.MustExec("create table t (a int);")
		testKit.MustExec("set global tidb_ignore_inlist_plan_digest=true;")
		testKit.Session().GetSessionVars().PlanID.Store(0)
		queriesGroup1 := []string{
			"select * from t where a in (1, 2);",
			"select a in (1, 2) from t;",
		}
		queriesGroup2 := []string{
			"select * from t where a in (1, 2, 3);",
			"select a in (1, 2, 3) from t;",
		}
		for i := range queriesGroup1 {
			query1 := queriesGroup1[i]
			query2 := queriesGroup2[i]
			t.Run(query1+" vs "+query2, func(t *testing.T) {
				testKit.MustExec(query1)
				info1 := testKit.Session().ShowProcess()
				require.NotNil(t, info1)
				p1, ok := info1.Plan.(base.Plan)
				require.True(t, ok)
				_, digest1 := core.NormalizePlan(p1)
				testKit.MustExec(query2)
				info2 := testKit.Session().ShowProcess()
				require.NotNil(t, info2)
				p2, ok := info2.Plan.(base.Plan)
				require.True(t, ok)
				_, digest2 := core.NormalizePlan(p2)
				require.Equal(t, digest1, digest2)
			})
		}
	})
}

func TestIssue47634(t *testing.T) {
	testkit.RunTestUnderCascades(t, func(t *testing.T, testKit *testkit.TestKit, cascades, caller string) {
		testKit.MustExec("use test")
		testKit.MustExec("drop table if exists t3,t4")
		testKit.MustExec("create table t3(a int, b int, c int);")
		testKit.MustExec("create table t4(a int, b int, c int, primary key (a, b) clustered);")
		testKit.MustExec("create table t5(a int, b int, c int, key idx_a_b (a, b));")
		testKit.Session().GetSessionVars().PlanID.Store(0)
		queriesGroup1 := []string{
			"explain select /*+ inl_join(t4) */ * from t3 join t4 on t3.b = t4.b where t4.a = 1;",
			"explain select /*+ inl_join(t5) */ * from t3 join t5 on t3.b = t5.b where t5.a = 1;",
		}
		queriesGroup2 := []string{
			"explain select /*+ inl_join(t4) */ * from t3 join t4 on t3.b = t4.b where t4.a = 2;",
			"explain select /*+ inl_join(t5) */ * from t3 join t5 on t3.b = t5.b where t5.a = 2;",
		}
		for i := range queriesGroup1 {
			query1 := queriesGroup1[i]
			query2 := queriesGroup2[i]
			t.Run(query1+" vs "+query2, func(t *testing.T) {
				testKit.MustExec(query1)
				info1 := testKit.Session().ShowProcess()
				require.NotNil(t, info1)
				p1, ok := info1.Plan.(base.Plan)
				require.True(t, ok)
				_, digest1 := core.NormalizePlan(p1)
				testKit.MustExec(query2)
				info2 := testKit.Session().ShowProcess()
				require.NotNil(t, info2)
				p2, ok := info2.Plan.(base.Plan)
				require.True(t, ok)
				_, digest2 := core.NormalizePlan(p2)
				require.Equal(t, digest1, digest2)
			})
		}
	})
}

func TestNormalizedPlanForDiffStore(t *testing.T) {
	testkit.RunTestUnderCascadesWithDomain(t, func(t *testing.T, testKit *testkit.TestKit, dom *domain.Domain, cascades, caller string) {
		testKit.MustExec("use test")
		testKit.MustExec("drop table if exists t1")
		testKit.MustExec("create table t1 (a int, b int, c int, primary key(a))")
		testKit.MustExec("insert into t1 values(1,1,1), (2,2,2), (3,3,3)")
		tbl, err := dom.InfoSchema().TableByName(context.Background(), ast.CIStr{O: "test", L: "test"}, ast.CIStr{O: "t1", L: "t1"})
		require.NoError(t, err)
		// Set the hacked TiFlash replica for explain tests.
		tbl.Meta().TiFlashReplica = &model.TiFlashReplicaInfo{Count: 1, Available: true}

		var input []string
		var output []struct {
			Digest string
			Plan   []string
		}
		planNormalizedSuiteData := GetPlanNormalizedSuiteData()
		planNormalizedSuiteData.LoadTestCases(t, &input, &output, cascades, caller)
		lastDigest := ""
		for i, tt := range input {
			testKit.Session().GetSessionVars().PlanID.Store(0)
			testKit.MustExec(tt)
			info := testKit.Session().ShowProcess()
			require.NotNil(t, info)
			ep, ok := info.Plan.(*core.Explain)
			require.True(t, ok)
			normalized, digest := core.NormalizePlan(ep.TargetPlan)

			// test the new normalization code
			flat := core.FlattenPhysicalPlan(ep.TargetPlan, false)
			newNormalized, newPlanDigest := core.NormalizeFlatPlan(flat)
			require.Equal(t, digest, newPlanDigest)
			require.Equal(t, normalized, newNormalized)

			normalizedPlan, err := plancodec.DecodeNormalizedPlan(normalized)
			normalizedPlanRows := getPlanRows(normalizedPlan)
			require.NoError(t, err)
			testdata.OnRecord(func() {
				output[i].Digest = digest.String()
				output[i].Plan = normalizedPlanRows
			})
			compareStringSlice(t, normalizedPlanRows, output[i].Plan)
			require.NotEqual(t, digest.String(), lastDigest)
			lastDigest = digest.String()
		}
	})
}

func TestJSONPlanInExplain(t *testing.T) {
	testkit.RunTestUnderCascades(t, func(t *testing.T, testKit *testkit.TestKit, cascades, caller string) {
		testKit.MustExec("use test")
		testKit.MustExec("drop table if exists t1, t2")
		testKit.MustExec("create table t1(id int, key(id))")
		testKit.MustExec("create table t2(id int, key(id))")

		var input []string
		var output []struct {
			SQL      string
			JSONPlan []*core.ExplainInfoForEncode
		}
		planSuiteData := GetJSONPlanSuiteData()
		planSuiteData.LoadTestCases(t, &input, &output, cascades, caller)

		for i, test := range input {
			resJSON := testKit.MustQuery(test).Rows()
			var res []*core.ExplainInfoForEncode
			require.NoError(t, json.Unmarshal([]byte(resJSON[0][0].(string)), &res))
			for j, expect := range output[i].JSONPlan {
				require.Equal(t, expect.ID, res[j].ID)
				require.Equal(t, expect.EstRows, res[j].EstRows)
				require.Equal(t, expect.ActRows, res[j].ActRows)
				require.Equal(t, expect.TaskType, res[j].TaskType)
				require.Equal(t, expect.AccessObject, res[j].AccessObject)
				require.Equal(t, expect.OperatorInfo, res[j].OperatorInfo)
			}
		}
	})
}

func TestHandleEQAll(t *testing.T) {
	testkit.RunTestUnderCascades(t, func(t *testing.T, testKit *testkit.TestKit, cascades, caller string) {
		testKit.MustExec("use test")
		testKit.MustExec("CREATE TABLE t1 (c1 int, c2 int, UNIQUE i1 (c1, c2));")
		testKit.MustExec("INSERT INTO t1 VALUES (7, null),(5,1);")
		testKit.MustQuery("SELECT c1 FROM t1 WHERE ('m' = ALL (SELECT /*+ IGNORE_INDEX(t1, i1) */ c2 FROM t1)) IS NOT UNKNOWN; ").Check(testkit.Rows("5", "7"))
		testKit.MustQuery("SELECT c1 FROM t1 WHERE ('m' = ALL (SELECT /*+ use_INDEX(t1, i1) */ c2 FROM t1)) IS NOT UNKNOWN; ").Check(testkit.Rows("5", "7"))
		testKit.MustQuery("select (null = ALL (SELECT /*+ NO_INDEX() */ c2 FROM t1)) IS NOT UNKNOWN").Check(testkit.Rows("0"))
		testKit.MustExec("CREATE TABLE t2 (c1 int, c2 int, UNIQUE i1 (c1, c2));")
		testKit.MustExec("INSERT INTO t2 VALUES (7, null),(5,null);")
		testKit.MustQuery("select (null = ALL (SELECT /*+ NO_INDEX() */ c2 FROM t2)) IS NOT UNKNOWN").Check(testkit.Rows("0"))
		testKit.MustQuery("SELECT c1 FROM t2 WHERE ('m' = ALL (SELECT /*+ IGNORE_INDEX(t2, i1) */ c2 FROM t2)) IS NOT UNKNOWN; ").Check(testkit.Rows())
		testKit.MustQuery("SELECT c1 FROM t2 WHERE ('m' = ALL (SELECT /*+ use_INDEX(t2, i1) */ c2 FROM t2)) IS NOT UNKNOWN; ").Check(testkit.Rows())
		testKit.MustExec("truncate table t2")
		testKit.MustExec("INSERT INTO t2 VALUES (7, null),(7,null);")
		testKit.MustQuery("select c1 from t2 where (c1 = all (select /*+ IGNORE_INDEX(t2, i1) */ c1 from t2))").Check(testkit.Rows("7", "7"))
		testKit.MustQuery("select c1 from t2 where (c1 = all (select /*+ use_INDEX(t2, i1) */ c1 from t2))").Check(testkit.Rows("7", "7"))
		testKit.MustQuery("select c2 from t2 where (c2 = all (select /*+ IGNORE_INDEX(t2, i1) */ c2 from t2))").Check(testkit.Rows())
		testKit.MustQuery("select c2 from t2 where (c2 = all (select /*+ use_INDEX(t2, i1) */ c2 from t2))").Check(testkit.Rows())
	})
}

func TestOuterJoinElimination(t *testing.T) {
	testkit.RunTestUnderCascades(t, func(t *testing.T, testKit *testkit.TestKit, cascades, caller string) {
		testKit.MustExec("use test")
		testKit.MustExec(`create table t1 (a int, b int, c int)`)
		testKit.MustExec(`create table t2 (a int, b int, c int)`)
		testKit.MustExec(`create table t2_k (a int, b int, c int, key(a))`)
		testKit.MustExec(`create table t2_uk (a int, b int, c int, unique key(a))`)
		testKit.MustExec(`create table t2_nnuk (a int not null, b int, c int, unique key(a))`)
		testKit.MustExec(`create table t2_pk (a int, b int, c int, primary key(a))`)

		// only when t2.a has unique attribute, we can eliminate the outer join.
		// nullable unique index is not allowed to trigger the outer join elinimation.
		testKit.MustHavePlan("select count(*) from t1 left join t2 on t1.a = t2.a", "Join")
		testKit.MustHavePlan("select count(*) from t1 left join t2_k on t1.a = t2_k.a", "Join")
		testKit.MustHavePlan("select count(*) from t1 left join t2_uk on t1.a = t2_uk.a", "Join")
		testKit.MustNotHavePlan("select count(*) from t1 left join t2_nnuk on t1.a = t2_nnuk.a", "Join")
		testKit.MustNotHavePlan("select count(*) from t1 left join t2_pk on t1.a = t2_pk.a", "Join")

		testKit.MustHavePlan("select count(*) from t1 left join t2 on t1.a = t2.a group by t1.a", "Join")
		testKit.MustHavePlan("select count(*) from t1 left join t2_k on t1.a = t2_k.a group by t1.a", "Join")
		testKit.MustHavePlan("select count(*) from t1 left join t2_uk on t1.a = t2_uk.a group by t1.a", "Join")
		testKit.MustNotHavePlan("select count(*) from t1 left join t2_nnuk on t1.a = t2_nnuk.a group by t1.a", "Join")
		testKit.MustNotHavePlan("select count(*) from t1 left join t2_pk on t1.a = t2_pk.a group by t1.a", "Join")

		// test distinct aggregation
		testKit.MustNotHavePlan("select distinct t1.a from t1 left join t2 on t1.a = t2.a", "Join")
		testKit.MustNotHavePlan("select distinct t1.a from t1 left join t2_k t2 on t1.a = t2.a", "Join")
		testKit.MustNotHavePlan("select distinct t1.a from t1 left join t2_uk t2 on t1.a = t2.a", "Join")
		testKit.MustNotHavePlan("select distinct t1.a from t1 left join t2_nnuk t2 on t1.a = t2.a", "Join")
		testKit.MustNotHavePlan("select distinct t1.a from t1 left join t2_pk t2 on t1.a = t2.a", "Join")
		// test constant columns with distinct
		testKit.MustNotHavePlan("select distinct 1 from t1 left join t2 on t1.a = t2.a", "Join")
		testKit.MustNotHavePlan("select distinct 1 from t1 left join t2_k t2 on t1.a = t2.a", "Join")
		testKit.MustNotHavePlan("select distinct 1 from t1 left join t2_uk t2 on t1.a = t2.a", "Join")
		testKit.MustNotHavePlan("select distinct 1 from t1 left join t2_nnuk t2 on t1.a = t2.a", "Join")
		testKit.MustNotHavePlan("select distinct 1 from t1 left join t2_pk t2 on t1.a = t2.a", "Join")
		// test constant columns with distinct
		testKit.MustHavePlan("select 1 from t1 left join t2 on t1.a = t2.a", "Join")
		testKit.MustHavePlan("select 1 from t1 left join t2_k t2 on t1.a = t2.a", "Join")
		testKit.MustHavePlan("select 1 from t1 left join t2_uk t2 on t1.a = t2.a", "Join")
		testKit.MustNotHavePlan("select 1 from t1 left join t2_nnuk t2 on t1.a = t2.a", "Join")
		testKit.MustNotHavePlan("select 1 from t1 left join t2_pk t2 on t1.a = t2.a", "Join")
		// test subqueries
		testKit.MustHavePlan("select 1 from (select distinct a from t1) t1 left join t2 on t1.a = t2.a", "Join")
		testKit.MustNotHavePlan("select distinct 1 from (select distinct a from t1) t1 left join t2 on t1.a = t2.a", "Join")
		testKit.MustHavePlan("select t1.a from (select distinct a from t1) t1 left join t2 on t1.a = t2.a", "Join")
		testKit.MustNotHavePlan("select distinct t1.a from (select distinct a from t1) t1 left join t2 on t1.a = t2.a", "Join")
	})
}

func TestCTEErrNotSupportedYet(t *testing.T) {
	testkit.RunTestUnderCascades(t, func(t *testing.T, testKit *testkit.TestKit, cascades, caller string) {
		testKit.MustExec("use test")
		testKit.MustExec(`
CREATE TABLE pub_branch (
  id int(5) NOT NULL,
  code varchar(12) NOT NULL,
  type_id int(3) DEFAULT NULL,
  name varchar(64) NOT NULL,
  short_name varchar(32) DEFAULT NULL,
  organ_code varchar(15) DEFAULT NULL,
  parent_code varchar(12) DEFAULT NULL,
  organ_layer tinyint(1) NOT NULL,
  inputcode1 varchar(12) DEFAULT NULL,
  inputcode2 varchar(12) DEFAULT NULL,
  state tinyint(1) NOT NULL,
  modify_empid int(9) NOT NULL,
  modify_time datetime NOT NULL,
  organ_level int(9) DEFAULT NULL,
  address varchar(256) DEFAULT NULL,
  db_user varchar(32) DEFAULT NULL,
  db_password varchar(64) DEFAULT NULL,
  org_no int(3) DEFAULT NULL,
  ord int(5) DEFAULT NULL,
  org_code_mpa varchar(10) DEFAULT NULL,
  org_code_gb varchar(30) DEFAULT NULL,
  wdchis_id int(5) DEFAULT NULL,
  medins_code varchar(32) DEFAULT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY pub_barnch_unique (code),
  KEY idx_pub_branch_parent (parent_code)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
`)
		testKit.MustExec(`
CREATE VIEW udc_branch_test (
  branch_id,
  his_branch_id,
  branch_code,
  branch_name,
  pid,
  his_pid,
  short_name,
  inputcode1,
  inputcode2,
  org_no,
  org_code,
  org_level,
  org_layer,
  address,
  state,
  modify_by,
  modify_time,
  remark
)
AS
SELECT a.id AS branch_id, a.id AS his_branch_id, a.code AS branch_code, a.name AS branch_name
  , a.id + 1000000 AS pid, id AS his_pid, a.short_name AS short_name
  , a.inputcode1 AS inputcode1, a.inputcode2 AS inputcode2, a.id AS org_no, a.code AS org_code, a.organ_level AS org_level
  , a.organ_layer AS org_layer, a.address AS address, a.state AS state, a.modify_empid AS modify_by, a.modify_time AS modify_time
  , NULL AS remark
FROM pub_branch a
WHERE organ_layer = 4
UNION ALL
SELECT a.id + 1000000 AS branch_id, a.id AS his_branch_id, a.code AS branch_code
  , CONCAT(a.name, _UTF8MB4 '(中心)') AS branch_name
  , (
    SELECT id AS id
    FROM pub_branch a
    WHERE organ_layer = 2
      AND state = 1
    LIMIT 1
  ) AS pid, id AS his_pid, a.short_name AS short_name, a.inputcode1 AS inputcode1, a.inputcode2 AS inputcode2
  , a.id AS org_no, a.code AS org_code, a.organ_level AS org_level, a.organ_layer AS org_layer, a.address AS address
  , a.state AS state, 1 AS modify_by, a.modify_time AS modify_time, NULL AS remark
FROM pub_branch a
WHERE organ_layer = 4
UNION ALL
SELECT a.id AS branch_id, a.id AS his_branch_id, a.code AS branch_code, a.name AS branch_name, NULL AS pid
  , id AS his_pid, a.short_name AS short_name, a.inputcode1 AS inputcode1, a.inputcode2 AS inputcode2, a.id AS org_no
  , a.code AS org_code, a.organ_level AS org_level, a.organ_layer AS org_layer, a.address AS address, a.state AS state
  , a.modify_empid AS modify_by, a.modify_time AS modify_time, NULL AS remark
FROM pub_branch a
WHERE organ_layer = 2;
`)
		testKit.MustExec(`
CREATE TABLE udc_branch_temp (
  branch_id int(11) NOT NULL AUTO_INCREMENT COMMENT '',
  his_branch_id varchar(20) DEFAULT NULL COMMENT '',
  branch_code varchar(20) DEFAULT NULL COMMENT '',
  branch_name varchar(64) NOT NULL COMMENT '',
  pid int(11) DEFAULT NULL COMMENT '',
  his_pid varchar(20) DEFAULT NULL COMMENT '',
  short_name varchar(64) DEFAULT NULL COMMENT '',
  inputcode1 varchar(12) DEFAULT NULL COMMENT '辅码1',
  inputcode2 varchar(12) DEFAULT NULL COMMENT '辅码2',
  org_no int(11) DEFAULT NULL COMMENT '',
  org_code varchar(20) DEFAULT NULL COMMENT ',',
  org_level tinyint(4) DEFAULT NULL COMMENT '',
  org_layer tinyint(4) DEFAULT NULL COMMENT '',
  address varchar(255) DEFAULT NULL COMMENT '机构地址',
  state tinyint(4) NOT NULL DEFAULT '1' COMMENT '',
  modify_by int(11) NOT NULL COMMENT '',
  modify_time datetime DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '修改时间',
  remark varchar(255) DEFAULT NULL COMMENT '备注',
  PRIMARY KEY (branch_id) /*T![clustered_index] CLUSTERED */
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin AUTO_INCREMENT=1030102 COMMENT='';
`)
		testKit.MustGetErrCode(`
SELECT res.*
FROM (
    (
        WITH RECURSIVE d AS (
            SELECT ub.*
            FROM udc_branch_test ub
            WHERE ub.branch_id = 1000102
            UNION ALL
            SELECT ub1.*
            FROM udc_branch_test ub1
            INNER JOIN d ON d.branch_id = ub1.pid
        )
        SELECT d.*
        FROM d
    )
) AS res
WHERE res.state != 2
ORDER BY res.branch_id;
`, errno.ErrNotSupportedYet)
	})
}
