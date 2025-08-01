// Copyright 2017 PingCAP, Inc.
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

package core

import (
	"fmt"
	"math"
	"slices"

	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/tidb/pkg/executor/join/joinversion"
	"github.com/pingcap/tidb/pkg/expression"
	"github.com/pingcap/tidb/pkg/expression/aggregation"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/planner/cardinality"
	"github.com/pingcap/tidb/pkg/planner/cascades/memo"
	"github.com/pingcap/tidb/pkg/planner/core/base"
	"github.com/pingcap/tidb/pkg/planner/core/cost"
	"github.com/pingcap/tidb/pkg/planner/core/operator/logicalop"
	"github.com/pingcap/tidb/pkg/planner/core/operator/physicalop"
	"github.com/pingcap/tidb/pkg/planner/property"
	"github.com/pingcap/tidb/pkg/planner/util"
	"github.com/pingcap/tidb/pkg/planner/util/fixcontrol"
	"github.com/pingcap/tidb/pkg/sessionctx/vardef"
	"github.com/pingcap/tidb/pkg/statistics"
	"github.com/pingcap/tidb/pkg/types"
	"github.com/pingcap/tidb/pkg/util/dbterror/plannererrors"
	h "github.com/pingcap/tidb/pkg/util/hint"
	"github.com/pingcap/tidb/pkg/util/logutil"
	"github.com/pingcap/tidb/pkg/util/plancodec"
	"github.com/pingcap/tidb/pkg/util/ranger"
	"github.com/pingcap/tipb/go-tipb"
	"go.uber.org/zap"
)

func exhaustPhysicalPlans4LogicalUnionScan(lp base.LogicalPlan, prop *property.PhysicalProperty) ([]base.PhysicalPlan, bool, error) {
	p := lp.(*logicalop.LogicalUnionScan)
	if prop.IsFlashProp() {
		p.SCtx().GetSessionVars().RaiseWarningWhenMPPEnforced(
			"MPP mode may be blocked because operator `UnionScan` is not supported now.")
		return nil, true, nil
	}
	childProp := prop.CloneEssentialFields()
	childProp = admitIndexJoinProp(childProp, prop)
	if childProp == nil {
		// even hint can not work with this. index join prop is not satisfied in mpp task type.
		return nil, false, nil
	}
	// here we just pass down the keep order property to the child.
	// cuz, in union scan exec, it will feel the underlying tableReader or indexReader to get the keepOrder.
	us := physicalop.PhysicalUnionScan{
		Conditions: p.Conditions,
		HandleCols: p.HandleCols,
	}.Init(p.SCtx(), p.StatsInfo(), p.QueryBlockOffset(), childProp)
	return []base.PhysicalPlan{us}, true, nil
}

// IsGAForHashJoinV2 judges if this hash join is GA
func IsGAForHashJoinV2(joinType logicalop.JoinType, leftJoinKeys []*expression.Column, isNullEQ []bool, leftNAJoinKeys []*expression.Column) bool {
	// nullaware join
	if len(leftNAJoinKeys) > 0 {
		return false
	}
	// cross join
	if len(leftJoinKeys) == 0 {
		return false
	}
	// join with null equal condition
	for _, value := range isNullEQ {
		if value {
			return false
		}
	}
	switch joinType {
	case logicalop.LeftOuterJoin, logicalop.RightOuterJoin, logicalop.InnerJoin, logicalop.AntiSemiJoin, logicalop.SemiJoin:
		return true
	default:
		return false
	}
}

// CanUseHashJoinV2 returns true if current join is supported by hash join v2
func canUseHashJoinV2(joinType logicalop.JoinType, leftJoinKeys []*expression.Column, isNullEQ []bool, leftNAJoinKeys []*expression.Column) bool {
	if !IsGAForHashJoinV2(joinType, leftJoinKeys, isNullEQ, leftNAJoinKeys) && !joinversion.UseHashJoinV2ForNonGAJoin {
		return false
	}
	switch joinType {
	case logicalop.LeftOuterJoin, logicalop.RightOuterJoin, logicalop.InnerJoin, logicalop.LeftOuterSemiJoin,
		logicalop.SemiJoin, logicalop.AntiSemiJoin, logicalop.AntiLeftOuterSemiJoin:
		// null aware join is not supported yet
		if len(leftNAJoinKeys) > 0 {
			return false
		}
		// cross join is not supported
		if len(leftJoinKeys) == 0 {
			return false
		}
		// NullEQ is not supported yet
		for _, value := range isNullEQ {
			if value {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func getHashJoins(super base.LogicalPlan, prop *property.PhysicalProperty) (joins []base.PhysicalPlan, forced bool) {
	ge, p := getGEAndLogicalJoin(super)
	if !prop.IsSortItemEmpty() { // hash join doesn't promise any orders
		return
	}

	forceLeftToBuild := ((p.PreferJoinType & h.PreferLeftAsHJBuild) > 0) || ((p.PreferJoinType & h.PreferRightAsHJProbe) > 0)
	forceRightToBuild := ((p.PreferJoinType & h.PreferRightAsHJBuild) > 0) || ((p.PreferJoinType & h.PreferLeftAsHJProbe) > 0)
	if forceLeftToBuild && forceRightToBuild {
		p.SCtx().GetSessionVars().StmtCtx.SetHintWarning("Conflicting HASH_JOIN_BUILD and HASH_JOIN_PROBE hints detected. " +
			"Both sides cannot be specified to use the same table. Please review the hints")
		forceLeftToBuild = false
		forceRightToBuild = false
	}
	joins = make([]base.PhysicalPlan, 0, 2)
	switch p.JoinType {
	case logicalop.SemiJoin, logicalop.AntiSemiJoin:
		leftJoinKeys, _, isNullEQ, _ := p.GetJoinKeys()
		leftNAJoinKeys, _ := p.GetNAJoinKeys()
		if p.SCtx().GetSessionVars().UseHashJoinV2 && joinversion.IsHashJoinV2Supported() && canUseHashJoinV2(p.JoinType, leftJoinKeys, isNullEQ, leftNAJoinKeys) {
			if !forceLeftToBuild {
				joins = append(joins, getHashJoin(ge, p, prop, 1, false))
			}
			if !forceRightToBuild {
				joins = append(joins, getHashJoin(ge, p, prop, 1, true))
			}
		} else {
			joins = append(joins, getHashJoin(ge, p, prop, 1, false))
			if forceLeftToBuild || forceRightToBuild {
				p.SCtx().GetSessionVars().StmtCtx.SetHintWarning(fmt.Sprintf(
					"The HASH_JOIN_BUILD and HASH_JOIN_PROBE hints are not supported for %s with hash join version 1. "+
						"Please remove these hints",
					p.JoinType))
				forceLeftToBuild = false
				forceRightToBuild = false
			}
		}
	case logicalop.LeftOuterSemiJoin, logicalop.AntiLeftOuterSemiJoin:
		joins = append(joins, getHashJoin(ge, p, prop, 1, false))
		if forceLeftToBuild || forceRightToBuild {
			p.SCtx().GetSessionVars().StmtCtx.SetHintWarning(fmt.Sprintf(
				"HASH_JOIN_BUILD and HASH_JOIN_PROBE hints are not supported for %s because the build side is fixed. "+
					"Please remove these hints",
				p.JoinType))
			forceLeftToBuild = false
			forceRightToBuild = false
		}
	case logicalop.LeftOuterJoin:
		if !forceLeftToBuild {
			joins = append(joins, getHashJoin(ge, p, prop, 1, false))
		}
		if !forceRightToBuild {
			joins = append(joins, getHashJoin(ge, p, prop, 1, true))
		}
	case logicalop.RightOuterJoin:
		if !forceLeftToBuild {
			joins = append(joins, getHashJoin(ge, p, prop, 0, true))
		}
		if !forceRightToBuild {
			joins = append(joins, getHashJoin(ge, p, prop, 0, false))
		}
	case logicalop.InnerJoin:
		if forceLeftToBuild {
			joins = append(joins, getHashJoin(ge, p, prop, 0, false))
		} else if forceRightToBuild {
			joins = append(joins, getHashJoin(ge, p, prop, 1, false))
		} else {
			joins = append(joins, getHashJoin(ge, p, prop, 1, false))
			joins = append(joins, getHashJoin(ge, p, prop, 0, false))
		}
	}

	forced = (p.PreferJoinType&h.PreferHashJoin > 0) || forceLeftToBuild || forceRightToBuild
	shouldSkipHashJoin := physicalop.ShouldSkipHashJoin(p)
	if !forced && shouldSkipHashJoin {
		return nil, false
	} else if forced && shouldSkipHashJoin {
		p.SCtx().GetSessionVars().StmtCtx.SetHintWarning(
			"A conflict between the HASH_JOIN hint and the NO_HASH_JOIN hint, " +
				"or the tidb_opt_enable_hash_join system variable, the HASH_JOIN hint will take precedence.")
	}
	return
}

func getHashJoin(ge *memo.GroupExpression, p *logicalop.LogicalJoin, prop *property.PhysicalProperty, innerIdx int, useOuterToBuild bool) *PhysicalHashJoin {
	stats0, stats1, _, _ := getJoinChildStatsAndSchema(ge, p)
	chReqProps := make([]*property.PhysicalProperty, 2)
	chReqProps[innerIdx] = &property.PhysicalProperty{ExpectedCnt: math.MaxFloat64, CTEProducerStatus: prop.CTEProducerStatus, NoCopPushDown: prop.NoCopPushDown}
	chReqProps[1-innerIdx] = &property.PhysicalProperty{ExpectedCnt: math.MaxFloat64, CTEProducerStatus: prop.CTEProducerStatus, NoCopPushDown: prop.NoCopPushDown}
	var outerStats *property.StatsInfo
	if 1-innerIdx == 0 {
		outerStats = stats0
	} else {
		outerStats = stats1
	}
	if prop.ExpectedCnt < p.StatsInfo().RowCount {
		expCntScale := prop.ExpectedCnt / p.StatsInfo().RowCount
		chReqProps[1-innerIdx].ExpectedCnt = outerStats.RowCount * expCntScale
	}
	hashJoin := NewPhysicalHashJoin(p, innerIdx, useOuterToBuild, p.StatsInfo().ScaleByExpectCnt(prop.ExpectedCnt), chReqProps...)
	hashJoin.SetSchema(p.Schema())
	return hashJoin
}

// constructIndexHashJoinStatic is used to enumerate current a physical index hash join with undecided inner plan. Via index join prop
// pushed down to the inner side, the inner plans will check the admission of valid indexJoinProp and enumerate admitted inner
// operator. This function is quite similar with constructIndexJoinStatic.
func constructIndexHashJoinStatic(
	p *logicalop.LogicalJoin,
	prop *property.PhysicalProperty,
	outerIdx int,
	indexJoinProp *property.IndexJoinRuntimeProp,
	outerStats *property.StatsInfo,
) []base.PhysicalPlan {
	// new one index join with the same index join prop pushed down.
	indexJoins := constructIndexJoinStatic(p, prop, outerIdx, indexJoinProp, outerStats)
	indexHashJoins := make([]base.PhysicalPlan, 0, len(indexJoins))
	for _, plan := range indexJoins {
		join := plan.(*physicalop.PhysicalIndexJoin)
		indexHashJoin := PhysicalIndexHashJoin{
			PhysicalIndexJoin: *join,
			// Prop is empty means that the parent operator does not need the
			// join operator to provide any promise of the output order.
			KeepOuterOrder: !prop.IsSortItemEmpty(),
		}.Init(p.SCtx())
		indexHashJoins = append(indexHashJoins, indexHashJoin)
	}
	return indexHashJoins
}

// constructIndexJoinStatic is used to enumerate current a physical index join with undecided inner plan. Via index join prop
// pushed down to the inner side, the inner plans will check the admission of valid indexJoinProp and enumerate admitted inner
// operator. This function is quite similar with constructIndexJoin. While differing in following part:
//
// Since constructIndexJoin will fill the physicalIndexJoin some runtime detail even for adjusting the keys, hash-keys, move
// eq condition into other conditions because the underlying ds couldn't use it or something. This is because previously the
// index join enumeration can see the complete index chosen result after inner task is built. But for the refactored one, the
// enumerated physical index here can only see the info it owns. That's why we call the function constructIndexJoinStatic.
//
// The indexJoinProp is passed down to the inner side, which contains the runtime constant inner key, which is used to build the
// underlying index/pk range. When the inner side is built bottom up, it will return the indexJoinInfo, which contains the runtime
// information that this physical index join wants. That's introduce second function called completePhysicalIndexJoin, which will
// fill physicalIndexJoin about all the runtime information it lacks in static enumeration phase.
func constructIndexJoinStatic(
	p *logicalop.LogicalJoin,
	prop *property.PhysicalProperty,
	outerIdx int,
	indexJoinProp *property.IndexJoinRuntimeProp,
	outerStats *property.StatsInfo,
) []base.PhysicalPlan {
	joinType := p.JoinType
	var (
		innerJoinKeys []*expression.Column
		outerJoinKeys []*expression.Column
		isNullEQ      []bool
		hasNullEQ     bool
	)
	if outerIdx == 0 {
		outerJoinKeys, innerJoinKeys, isNullEQ, hasNullEQ = p.GetJoinKeys()
	} else {
		innerJoinKeys, outerJoinKeys, isNullEQ, hasNullEQ = p.GetJoinKeys()
	}
	// TODO: support null equal join keys for index join
	if hasNullEQ {
		return nil
	}
	chReqProps := make([]*property.PhysicalProperty, 2)
	// outer side expected cnt will be amplified by the prop.ExpectedCnt / p.StatsInfo().RowCount with same ratio.
	chReqProps[outerIdx] = &property.PhysicalProperty{TaskTp: property.RootTaskType, ExpectedCnt: math.MaxFloat64,
		SortItems: prop.SortItems, CTEProducerStatus: prop.CTEProducerStatus, NoCopPushDown: prop.NoCopPushDown}
	orderRatio := p.SCtx().GetSessionVars().OptOrderingIdxSelRatio
	// Record the variable usage for explain explore.
	p.SCtx().GetSessionVars().RecordRelevantOptVar(vardef.TiDBOptOrderingIdxSelRatio)
	outerRowCount := outerStats.RowCount
	estimatedRowCount := p.StatsInfo().RowCount
	if (prop.ExpectedCnt < estimatedRowCount) ||
		(orderRatio > 0 && outerRowCount > estimatedRowCount && prop.ExpectedCnt < outerRowCount && estimatedRowCount > 0) {
		// Apply the orderRatio to recognize that a large outer table scan may
		// read additional rows before the inner table reaches the limit values
		rowsToMeetFirst := max(0.0, (outerRowCount-estimatedRowCount)*orderRatio)
		expCntScale := prop.ExpectedCnt / estimatedRowCount
		expectedCnt := (outerRowCount * expCntScale) + rowsToMeetFirst
		chReqProps[outerIdx].ExpectedCnt = expectedCnt
	}

	// inner side should pass down the indexJoinProp, which contains the runtime constant inner key, which is used to build the underlying index/pk range.
	chReqProps[1-outerIdx] = &property.PhysicalProperty{TaskTp: property.RootTaskType, ExpectedCnt: math.MaxFloat64,
		CTEProducerStatus: prop.CTEProducerStatus, IndexJoinProp: indexJoinProp, NoCopPushDown: prop.NoCopPushDown}

	// for old logic from constructIndexJoin like
	// 1. feeling the keyOff2IdxOffs' -1 and refill the eq condition back to other conditions and adjust inner or outer keys, we
	// move it to completeIndexJoin because it requires the indexJoinInfo which is generated by underlying ds and passed bottom-up
	// within the Task to be filled.
	// 2. extract the eq condition from new other conditions to build the hash join keys, this kind of eq can be used by hash key
	// mapping, we move it to completePhysicalIndexJoin because it requires the indexJoinInfo which is generated by underlying ds
	// and passed bottom-up within the Task to be filled as well.

	baseJoin := physicalop.BasePhysicalJoin{
		InnerChildIdx:   1 - outerIdx,
		LeftConditions:  p.LeftConditions,
		RightConditions: p.RightConditions,
		// for static enumeration here, we just pass down the original other conditions
		OtherConditions: p.OtherConditions,
		JoinType:        joinType,
		// for static enumeration here, we just pass down the original outerJoinKeys, innerJoinKeys, isNullEQ
		OuterJoinKeys: outerJoinKeys,
		InnerJoinKeys: innerJoinKeys,
		IsNullEQ:      isNullEQ,
		DefaultValues: p.DefaultValues,
	}

	join := physicalop.PhysicalIndexJoin{
		BasePhysicalJoin: baseJoin,
		// for static enumeration here, we don't need to fill inner plan anymore.
		// for static enumeration here, the KeyOff2IdxOff, Ranges, CompareFilters, OuterHashKeys, InnerHashKeys are
		// waiting for attach2Task's complement after see the inner plan's indexJoinInfo returned by underlying ds.
		//
		// for static enumeration here, we just pass down the original equal condition for condition adjustment rather
		// depend on the original logical join node.
		EqualConditions: p.EqualConditions,
	}.Init(p.SCtx(), p.StatsInfo().ScaleByExpectCnt(prop.ExpectedCnt), p.QueryBlockOffset(), chReqProps...)
	join.SetSchema(p.Schema())
	return []base.PhysicalPlan{join}
}

// completePhysicalIndexJoin
// as you see, completePhysicalIndexJoin is called in attach2Task, when the inner plan of a physical index join or a
// physical index hash join is built bottom-up. Then indexJoin info is passed bottom-up within the Task to be filled.
// completePhysicalIndexJoin will fill physicalIndexJoin about all the runtime information it lacks in static enumeration
// phase.
// There are several things to be filled:
// 1. ranges:
// as the old comment said: when inner plan is TableReader, the parameter `ranges` will be nil. Because pk
// only have one column. So all of its range is generated during execution time. So set a new ranges{} when it is nil.
//
// 2. KeyOff2IdxOffs: fill the keyOff2IdxOffs' -1 and refill the eq condition back to other conditions and adjust inner
// or outer keys and info.KeyOff2IdxOff has been used above to re-derive newInnerKeys, newOuterKeys, newIsNullEQ,
// newOtherConds, newKeyOff.
//
//	physic.IsNullEQ = newIsNullEQ
//	physic.InnerJoinKeys = newInnerKeys
//	physic.OuterJoinKeys = newOuterKeys
//	physic.OtherConditions = newOtherConds
//	physic.KeyOff2IdxOff = newKeyOff
//
// 3. OuterHashKeys, InnerHashKeys:
// for indexHashJoin, those not used EQ condition which has been moved into the new other-conditions can be extracted out
// to build the hash table to avoid lazy evaluation as other conditions.
//
//  4. Info's Ranges, IdxColLens, CompareFilters:
//     the underlying ds chosen path's ranges, idxColLens, and compareFilters.
//     physic.Ranges = info.Ranges
//     physic.IdxColLens = info.IdxColLens
//     physic.CompareFilters = info.CompareFilters
func completePhysicalIndexJoin(physic *physicalop.PhysicalIndexJoin, rt *RootTask, innerS, outerS *expression.Schema, extractOtherEQ bool) base.PhysicalPlan {
	info := rt.IndexJoinInfo
	// runtime fill back ranges
	if info.Ranges == nil {
		info.Ranges = ranger.Ranges{} // empty range
	}
	// set the new key off according to the index join info's keyOff2IdxOff
	newKeyOff := make([]int, 0, len(info.KeyOff2IdxOff))
	// IsNullEQ & InnerJoinKeys & OuterJoinKeys in physic may change.
	newIsNullEQ := make([]bool, 0, len(physic.IsNullEQ))
	newInnerKeys := make([]*expression.Column, 0, len(physic.InnerJoinKeys))
	newOuterKeys := make([]*expression.Column, 0, len(physic.OuterJoinKeys))
	// OtherCondition may change because EQ can be leveraged in hash table retrieve.
	newOtherConds := make([]expression.Expression, len(physic.OtherConditions), len(physic.OtherConditions)+len(physic.EqualConditions))
	copy(newOtherConds, physic.OtherConditions)
	for keyOff, idxOff := range info.KeyOff2IdxOff {
		// if the keyOff is not used in the index join, we need to move the equal condition back to other conditions to eval them.
		if info.KeyOff2IdxOff[keyOff] < 0 {
			newOtherConds = append(newOtherConds, physic.EqualConditions[keyOff])
			continue
		}
		// collecting the really used inner keys, outer keys, isNullEQ, and keyOff2IdxOff.
		newInnerKeys = append(newInnerKeys, physic.InnerJoinKeys[keyOff])
		newOuterKeys = append(newOuterKeys, physic.OuterJoinKeys[keyOff])
		newIsNullEQ = append(newIsNullEQ, physic.IsNullEQ[keyOff])
		newKeyOff = append(newKeyOff, idxOff)
	}

	// we can use the `col <eq> col` in new `OtherCondition` to build the hashtable to avoid the unnecessary calculating.
	// for indexHashJoin, those not used EQ condition which has been moved into new other-conditions can be extracted out
	// to build the hash table.
	var outerHashKeys, innerHashKeys []*expression.Column
	outerHashKeys, innerHashKeys = make([]*expression.Column, len(newOuterKeys)), make([]*expression.Column, len(newInnerKeys))
	// used innerKeys and outerKeys can surely be the hashKeys, besides that, the EQ in otherConds can also be used as hashKeys.
	copy(outerHashKeys, newOuterKeys)
	copy(innerHashKeys, newInnerKeys)
	for i := len(newOtherConds) - 1; extractOtherEQ && i >= 0; i = i - 1 {
		switch c := newOtherConds[i].(type) {
		case *expression.ScalarFunction:
			if c.FuncName.L == ast.EQ {
				lhs, ok1 := c.GetArgs()[0].(*expression.Column)
				rhs, ok2 := c.GetArgs()[1].(*expression.Column)
				if ok1 && ok2 {
					if lhs.InOperand || rhs.InOperand {
						// if this other-cond is from a `[not] in` sub-query, do not convert it into eq-cond since
						// IndexJoin cannot deal with NULL correctly in this case; please see #25799 for more details.
						continue
					}
					// when it arrives here, we can sure that we got a EQ conditions and each side of them is a bare
					// column, while we don't know whether each of them comes from the inner or outer, so check it.
					outerSchema, innerSchema := outerS, innerS
					if outerSchema.Contains(lhs) && innerSchema.Contains(rhs) {
						outerHashKeys = append(outerHashKeys, lhs) // nozero
						innerHashKeys = append(innerHashKeys, rhs) // nozero
					} else if innerSchema.Contains(lhs) && outerSchema.Contains(rhs) {
						outerHashKeys = append(outerHashKeys, rhs) // nozero
						innerHashKeys = append(innerHashKeys, lhs) // nozero
					}
					// if not, this EQ function is useless, keep it in new other conditions.
					newOtherConds = slices.Delete(newOtherConds, i, i+1)
				}
			}
		default:
			continue
		}
	}
	// then, fill all newXXX runtime info back to the physic indexJoin.
	// info.KeyOff2IdxOff has been used above to derive newInnerKeys, newOuterKeys, newIsNullEQ, newOtherConds, newKeyOff.
	physic.IsNullEQ = newIsNullEQ
	physic.InnerJoinKeys = newInnerKeys
	physic.OuterJoinKeys = newOuterKeys
	physic.OtherConditions = newOtherConds
	physic.KeyOff2IdxOff = newKeyOff
	// the underlying ds chosen path's ranges, idxColLens, and compareFilters.
	physic.Ranges = info.Ranges
	physic.IdxColLens = info.IdxColLens
	physic.CompareFilters = info.CompareFilters
	// fill executing hashKeys, which containing inner/outer keys, and extracted EQ keys from otherConds if any.
	physic.OuterHashKeys = outerHashKeys
	physic.InnerHashKeys = innerHashKeys
	// the logical EqualConditions is not used anymore in later phase.
	physic.EqualConditions = nil
	// clear rootTask's indexJoinInfo in case of pushing upward, because physical index join is indexJoinInfo's consumer.
	rt.IndexJoinInfo = nil
	return physic
}

// When inner plan is TableReader, the parameter `ranges` will be nil. Because pk only have one column. So all of its range
// is generated during execution time.
func constructIndexJoin(
	p *logicalop.LogicalJoin,
	prop *property.PhysicalProperty,
	outerIdx int,
	innerTask base.Task,
	ranges ranger.MutableRanges,
	keyOff2IdxOff []int,
	path *util.AccessPath,
	compareFilters *physicalop.ColWithCmpFuncManager,
	extractOtherEQ bool,
) []base.PhysicalPlan {
	if innerTask.Invalid() {
		return nil
	}
	if ranges == nil {
		ranges = ranger.Ranges{} // empty range
	}

	joinType := p.JoinType
	var (
		innerJoinKeys []*expression.Column
		outerJoinKeys []*expression.Column
		isNullEQ      []bool
		hasNullEQ     bool
	)
	if outerIdx == 0 {
		outerJoinKeys, innerJoinKeys, isNullEQ, hasNullEQ = p.GetJoinKeys()
	} else {
		innerJoinKeys, outerJoinKeys, isNullEQ, hasNullEQ = p.GetJoinKeys()
	}
	// TODO: support null equal join keys for index join
	if hasNullEQ {
		return nil
	}
	chReqProps := make([]*property.PhysicalProperty, 2)
	chReqProps[outerIdx] = &property.PhysicalProperty{TaskTp: property.RootTaskType, ExpectedCnt: math.MaxFloat64,
		SortItems: prop.SortItems, CTEProducerStatus: prop.CTEProducerStatus, NoCopPushDown: prop.NoCopPushDown}
	if prop.ExpectedCnt < p.StatsInfo().RowCount {
		expCntScale := prop.ExpectedCnt / p.StatsInfo().RowCount
		chReqProps[outerIdx].ExpectedCnt = p.Children()[outerIdx].StatsInfo().RowCount * expCntScale
	}
	newInnerKeys := make([]*expression.Column, 0, len(innerJoinKeys))
	newOuterKeys := make([]*expression.Column, 0, len(outerJoinKeys))
	newIsNullEQ := make([]bool, 0, len(isNullEQ))
	newKeyOff := make([]int, 0, len(keyOff2IdxOff))
	newOtherConds := make([]expression.Expression, len(p.OtherConditions), len(p.OtherConditions)+len(p.EqualConditions))
	copy(newOtherConds, p.OtherConditions)
	for keyOff, idxOff := range keyOff2IdxOff {
		if keyOff2IdxOff[keyOff] < 0 {
			newOtherConds = append(newOtherConds, p.EqualConditions[keyOff])
			continue
		}
		newInnerKeys = append(newInnerKeys, innerJoinKeys[keyOff])
		newOuterKeys = append(newOuterKeys, outerJoinKeys[keyOff])
		newIsNullEQ = append(newIsNullEQ, isNullEQ[keyOff])
		newKeyOff = append(newKeyOff, idxOff)
	}

	var outerHashKeys, innerHashKeys []*expression.Column
	outerHashKeys, innerHashKeys = make([]*expression.Column, len(newOuterKeys)), make([]*expression.Column, len(newInnerKeys))
	copy(outerHashKeys, newOuterKeys)
	copy(innerHashKeys, newInnerKeys)
	// we can use the `col <eq> col` in `OtherCondition` to build the hashtable to avoid the unnecessary calculating.
	for i := len(newOtherConds) - 1; extractOtherEQ && i >= 0; i = i - 1 {
		switch c := newOtherConds[i].(type) {
		case *expression.ScalarFunction:
			if c.FuncName.L == ast.EQ {
				lhs, ok1 := c.GetArgs()[0].(*expression.Column)
				rhs, ok2 := c.GetArgs()[1].(*expression.Column)
				if ok1 && ok2 {
					if lhs.InOperand || rhs.InOperand {
						// if this other-cond is from a `[not] in` sub-query, do not convert it into eq-cond since
						// IndexJoin cannot deal with NULL correctly in this case; please see #25799 for more details.
						continue
					}
					outerSchema, innerSchema := p.Children()[outerIdx].Schema(), p.Children()[1-outerIdx].Schema()
					if outerSchema.Contains(lhs) && innerSchema.Contains(rhs) {
						outerHashKeys = append(outerHashKeys, lhs) // nozero
						innerHashKeys = append(innerHashKeys, rhs) // nozero
					} else if innerSchema.Contains(lhs) && outerSchema.Contains(rhs) {
						outerHashKeys = append(outerHashKeys, rhs) // nozero
						innerHashKeys = append(innerHashKeys, lhs) // nozero
					}
					newOtherConds = slices.Delete(newOtherConds, i, i+1)
				}
			}
		default:
			continue
		}
	}

	baseJoin := physicalop.BasePhysicalJoin{
		InnerChildIdx:   1 - outerIdx,
		LeftConditions:  p.LeftConditions,
		RightConditions: p.RightConditions,
		OtherConditions: newOtherConds,
		JoinType:        joinType,
		OuterJoinKeys:   newOuterKeys,
		InnerJoinKeys:   newInnerKeys,
		IsNullEQ:        newIsNullEQ,
		DefaultValues:   p.DefaultValues,
	}

	join := physicalop.PhysicalIndexJoin{
		BasePhysicalJoin: baseJoin,
		InnerPlan:        innerTask.Plan(),
		KeyOff2IdxOff:    newKeyOff,
		Ranges:           ranges,
		CompareFilters:   compareFilters,
		OuterHashKeys:    outerHashKeys,
		InnerHashKeys:    innerHashKeys,
	}.Init(p.SCtx(), p.StatsInfo().ScaleByExpectCnt(prop.ExpectedCnt), p.QueryBlockOffset(), chReqProps...)
	if path != nil {
		join.IdxColLens = path.IdxColLens
	}
	join.SetSchema(p.Schema())
	return []base.PhysicalPlan{join}
}

func constructIndexMergeJoin(
	p *logicalop.LogicalJoin,
	prop *property.PhysicalProperty,
	outerIdx int,
	innerTask base.Task,
	ranges ranger.MutableRanges,
	keyOff2IdxOff []int,
	path *util.AccessPath,
	compareFilters *physicalop.ColWithCmpFuncManager,
) []base.PhysicalPlan {
	hintExists := false
	if (outerIdx == 1 && (p.PreferJoinType&h.PreferLeftAsINLMJInner) > 0) || (outerIdx == 0 && (p.PreferJoinType&h.PreferRightAsINLMJInner) > 0) {
		hintExists = true
	}
	indexJoins := constructIndexJoin(p, prop, outerIdx, innerTask, ranges, keyOff2IdxOff, path, compareFilters, !hintExists)
	indexMergeJoins := make([]base.PhysicalPlan, 0, len(indexJoins))
	for _, plan := range indexJoins {
		join := plan.(*physicalop.PhysicalIndexJoin)
		// Index merge join can't handle hash keys. So we ban it heuristically.
		if len(join.InnerHashKeys) > len(join.InnerJoinKeys) {
			return nil
		}

		// EnumType/SetType Unsupported: merge join conflicts with index order.
		// ref: https://github.com/pingcap/tidb/issues/24473, https://github.com/pingcap/tidb/issues/25669
		for _, innerKey := range join.InnerJoinKeys {
			if innerKey.RetType.GetType() == mysql.TypeEnum || innerKey.RetType.GetType() == mysql.TypeSet {
				return nil
			}
		}
		for _, outerKey := range join.OuterJoinKeys {
			if outerKey.RetType.GetType() == mysql.TypeEnum || outerKey.RetType.GetType() == mysql.TypeSet {
				return nil
			}
		}

		hasPrefixCol := false
		for _, l := range join.IdxColLens {
			if l != types.UnspecifiedLength {
				hasPrefixCol = true
				break
			}
		}
		// If index column has prefix length, the merge join can not guarantee the relevance
		// between index and join keys. So we should skip this case.
		// For more details, please check the following code and comments.
		if hasPrefixCol {
			continue
		}

		// keyOff2KeyOffOrderByIdx is map the join keys offsets to [0, len(joinKeys)) ordered by the
		// join key position in inner index.
		keyOff2KeyOffOrderByIdx := make([]int, len(join.OuterJoinKeys))
		keyOffMapList := make([]int, len(join.KeyOff2IdxOff))
		copy(keyOffMapList, join.KeyOff2IdxOff)
		keyOffMap := make(map[int]int, len(keyOffMapList))
		for i, idxOff := range keyOffMapList {
			keyOffMap[idxOff] = i
		}
		slices.Sort(keyOffMapList)
		keyIsIndexPrefix := true
		for keyOff, idxOff := range keyOffMapList {
			if keyOff != idxOff {
				keyIsIndexPrefix = false
				break
			}
			keyOff2KeyOffOrderByIdx[keyOffMap[idxOff]] = keyOff
		}
		if !keyIsIndexPrefix {
			continue
		}
		// isOuterKeysPrefix means whether the outer join keys are the prefix of the prop items.
		isOuterKeysPrefix := len(join.OuterJoinKeys) <= len(prop.SortItems)
		compareFuncs := make([]expression.CompareFunc, 0, len(join.OuterJoinKeys))
		outerCompareFuncs := make([]expression.CompareFunc, 0, len(join.OuterJoinKeys))

		for i := range join.KeyOff2IdxOff {
			if isOuterKeysPrefix && !prop.SortItems[i].Col.EqualColumn(join.OuterJoinKeys[keyOff2KeyOffOrderByIdx[i]]) {
				isOuterKeysPrefix = false
			}
			compareFuncs = append(compareFuncs, expression.GetCmpFunction(p.SCtx().GetExprCtx(), join.OuterJoinKeys[i], join.InnerJoinKeys[i]))
			outerCompareFuncs = append(outerCompareFuncs, expression.GetCmpFunction(p.SCtx().GetExprCtx(), join.OuterJoinKeys[i], join.OuterJoinKeys[i]))
		}
		// canKeepOuterOrder means whether the prop items are the prefix of the outer join keys.
		canKeepOuterOrder := len(prop.SortItems) <= len(join.OuterJoinKeys)
		for i := 0; canKeepOuterOrder && i < len(prop.SortItems); i++ {
			if !prop.SortItems[i].Col.EqualColumn(join.OuterJoinKeys[keyOff2KeyOffOrderByIdx[i]]) {
				canKeepOuterOrder = false
			}
		}
		// Since index merge join requires prop items the prefix of outer join keys
		// or outer join keys the prefix of the prop items. So we need `canKeepOuterOrder` or
		// `isOuterKeysPrefix` to be true.
		if canKeepOuterOrder || isOuterKeysPrefix {
			indexMergeJoin := PhysicalIndexMergeJoin{
				PhysicalIndexJoin:       *join,
				KeyOff2KeyOffOrderByIdx: keyOff2KeyOffOrderByIdx,
				NeedOuterSort:           !isOuterKeysPrefix,
				CompareFuncs:            compareFuncs,
				OuterCompareFuncs:       outerCompareFuncs,
				Desc:                    !prop.IsSortItemEmpty() && prop.SortItems[0].Desc,
			}.Init(p.SCtx())
			indexMergeJoins = append(indexMergeJoins, indexMergeJoin)
		}
	}
	return indexMergeJoins
}

func constructIndexHashJoin(
	p *logicalop.LogicalJoin,
	prop *property.PhysicalProperty,
	outerIdx int,
	innerTask base.Task,
	ranges ranger.MutableRanges,
	keyOff2IdxOff []int,
	path *util.AccessPath,
	compareFilters *physicalop.ColWithCmpFuncManager,
) []base.PhysicalPlan {
	indexJoins := constructIndexJoin(p, prop, outerIdx, innerTask, ranges, keyOff2IdxOff, path, compareFilters, true)
	indexHashJoins := make([]base.PhysicalPlan, 0, len(indexJoins))
	for _, plan := range indexJoins {
		join := plan.(*physicalop.PhysicalIndexJoin)
		indexHashJoin := PhysicalIndexHashJoin{
			PhysicalIndexJoin: *join,
			// Prop is empty means that the parent operator does not need the
			// join operator to provide any promise of the output order.
			KeepOuterOrder: !prop.IsSortItemEmpty(),
		}.Init(p.SCtx())
		indexHashJoins = append(indexHashJoins, indexHashJoin)
	}
	return indexHashJoins
}

// enumerateIndexJoinByOuterIdx will enumerate temporary index joins by index join prop required for its inner child.
func enumerateIndexJoinByOuterIdx(super base.LogicalPlan, prop *property.PhysicalProperty, outerIdx int) (joins []base.PhysicalPlan) {
	ge, p := getGEAndLogicalJoin(super)
	stats0, stats1, schema0, schema1 := getJoinChildStatsAndSchema(ge, p)
	var outerSchema *expression.Schema
	var outerStats *property.StatsInfo
	if outerIdx == 0 {
		outerSchema = schema0
		outerStats = stats0
	} else {
		outerSchema = schema1
		outerStats = stats1
	}
	// need same order
	all, _ := prop.AllSameOrder()
	// If the order by columns are not all from outer child, index join cannot promise the order.
	if !prop.AllColsFromSchema(outerSchema) || !all {
		return nil
	}
	var (
		innerJoinKeys []*expression.Column
		outerJoinKeys []*expression.Column
	)
	if outerIdx == 0 {
		outerJoinKeys, innerJoinKeys, _, _ = p.GetJoinKeys()
	} else {
		innerJoinKeys, outerJoinKeys, _, _ = p.GetJoinKeys()
	}
	// computed the avgInnerRowCnt
	var avgInnerRowCnt float64
	if count := outerStats.RowCount; count > 0 {
		avgInnerRowCnt = p.EqualCondOutCnt / count
	}
	// for pk path
	indexJoinPropTS := &property.IndexJoinRuntimeProp{
		OtherConditions: p.OtherConditions,
		InnerJoinKeys:   innerJoinKeys,
		OuterJoinKeys:   outerJoinKeys,
		AvgInnerRowCnt:  avgInnerRowCnt,
		TableRangeScan:  true,
	}
	// for normal index path
	indexJoinPropIS := &property.IndexJoinRuntimeProp{
		OtherConditions: p.OtherConditions,
		InnerJoinKeys:   innerJoinKeys,
		OuterJoinKeys:   outerJoinKeys,
		AvgInnerRowCnt:  avgInnerRowCnt,
		TableRangeScan:  false,
	}
	indexJoins := constructIndexJoinStatic(p, prop, outerIdx, indexJoinPropTS, outerStats)
	indexJoins = append(indexJoins, constructIndexJoinStatic(p, prop, outerIdx, indexJoinPropIS, outerStats)...)
	indexJoins = append(indexJoins, constructIndexHashJoinStatic(p, prop, outerIdx, indexJoinPropTS, outerStats)...)
	indexJoins = append(indexJoins, constructIndexHashJoinStatic(p, prop, outerIdx, indexJoinPropIS, outerStats)...)
	return indexJoins
}

// getIndexJoinByOuterIdx will generate index join by outerIndex. OuterIdx points out the outer child.
// First of all, we'll check whether the inner child is DataSource.
// Then, we will extract the join keys of p's equal conditions. Then check whether all of them are just the primary key
// or match some part of on index. If so we will choose the best one and construct a index join.
func getIndexJoinByOuterIdx(p *logicalop.LogicalJoin, prop *property.PhysicalProperty, outerIdx int) (joins []base.PhysicalPlan) {
	outerChild, innerChild := p.Children()[outerIdx], p.Children()[1-outerIdx]
	all, _ := prop.AllSameOrder()
	// If the order by columns are not all from outer child, index join cannot promise the order.
	if !prop.AllColsFromSchema(outerChild.Schema()) || !all {
		return nil
	}
	var (
		innerJoinKeys []*expression.Column
		outerJoinKeys []*expression.Column
	)
	if outerIdx == 0 {
		outerJoinKeys, innerJoinKeys, _, _ = p.GetJoinKeys()
	} else {
		innerJoinKeys, outerJoinKeys, _, _ = p.GetJoinKeys()
	}
	innerChildWrapper := extractIndexJoinInnerChildPattern(p, innerChild)
	if innerChildWrapper == nil {
		return nil
	}

	var avgInnerRowCnt float64
	if outerChild.StatsInfo().RowCount > 0 {
		avgInnerRowCnt = p.EqualCondOutCnt / outerChild.StatsInfo().RowCount
	}
	joins = buildIndexJoinInner2TableScan(p, prop, innerChildWrapper, innerJoinKeys, outerJoinKeys, outerIdx, avgInnerRowCnt)
	if joins != nil {
		return
	}
	return buildIndexJoinInner2IndexScan(p, prop, innerChildWrapper, innerJoinKeys, outerJoinKeys, outerIdx, avgInnerRowCnt)
}

// indexJoinInnerChildWrapper is a wrapper for the inner child of an index join.
// It contains the lowest DataSource operator and other inner child operator
// which is flattened into a list structure from tree structure .
// For example, the inner child of an index join is a tree structure like:
//
//	Projection
//	       Aggregation
//				Selection
//					DataSource
//
// The inner child wrapper will be:
// DataSource: the lowest DataSource operator.
// hasDitryWrite: whether the inner child contains dirty data.
// zippedChildren: [Projection, Aggregation, Selection]
type indexJoinInnerChildWrapper struct {
	ds             *logicalop.DataSource
	hasDitryWrite  bool
	zippedChildren []base.LogicalPlan
}

func checkOpSelfSatisfyPropTaskTypeRequirement(p base.LogicalPlan, prop *property.PhysicalProperty) bool {
	switch prop.TaskTp {
	case property.MppTaskType:
		// when parent operator ask current op to be mppTaskType, check operator itself here.
		return logicalop.CanSelfBeingPushedToCopImpl(p, kv.TiFlash)
	case property.CopSingleReadTaskType, property.CopMultiReadTaskType:
		return logicalop.CanSelfBeingPushedToCopImpl(p, kv.TiKV)
	default:
		return true
	}
}

// admitIndexJoinInnerChildPattern is used to check whether current physical choosing is under an index join's
// probe side. If it is, and we ganna check the original inner pattern check here to keep compatible with the old.
// the @first bool indicate whether current logical plan is valid of index join inner side.
func admitIndexJoinInnerChildPattern(p base.LogicalPlan) bool {
	switch x := p.GetBaseLogicalPlan().(*logicalop.BaseLogicalPlan).Self().(type) {
	case *logicalop.DataSource:
		// DS that prefer tiFlash reading couldn't walk into index join.
		if x.PreferStoreType&h.PreferTiFlash != 0 {
			return false
		}
	case *logicalop.LogicalProjection, *logicalop.LogicalSelection, *logicalop.LogicalAggregation:
		if !p.SCtx().GetSessionVars().EnableINLJoinInnerMultiPattern {
			return false
		}
	case *logicalop.LogicalUnionScan:
	default: // index join inner side couldn't allow join, sort, limit, etc. todo: open it.
		return false
	}
	return true
}

func extractIndexJoinInnerChildPattern(p *logicalop.LogicalJoin, innerChild base.LogicalPlan) *indexJoinInnerChildWrapper {
	wrapper := &indexJoinInnerChildWrapper{}
	nextChild := func(pp base.LogicalPlan) base.LogicalPlan {
		if len(pp.Children()) != 1 {
			return nil
		}
		return pp.Children()[0]
	}
childLoop:
	for curChild := innerChild; curChild != nil; curChild = nextChild(curChild) {
		switch child := curChild.(type) {
		case *logicalop.DataSource:
			wrapper.ds = child
			break childLoop
		case *logicalop.LogicalProjection, *logicalop.LogicalSelection, *logicalop.LogicalAggregation:
			if !p.SCtx().GetSessionVars().EnableINLJoinInnerMultiPattern {
				return nil
			}
			wrapper.zippedChildren = append(wrapper.zippedChildren, child)
		case *logicalop.LogicalUnionScan:
			wrapper.hasDitryWrite = true
			wrapper.zippedChildren = append(wrapper.zippedChildren, child)
		default:
			return nil
		}
	}
	if wrapper.ds == nil || wrapper.ds.PreferStoreType&h.PreferTiFlash != 0 {
		return nil
	}
	return wrapper
}

// buildDataSource2IndexScanByIndexJoinProp builds an IndexScan as the inner child for an
// IndexJoin based on IndexJoinProp included in prop if possible.
//
// buildDataSource2IndexScanByIndexJoinProp differs with buildIndexJoinInner2IndexScan in that
// the first one is try to build a single table scan as the inner child of an index join then return
// this inner task(raw table scan) bottom-up, which will be attached with other inner parents of an
// index join in attach2Task when bottom-up of enumerating the physical plans;
//
// while the second is try to build a table scan as the inner child of an index join, then build
// entire inner subtree of a index join out as innerTask instantly according those validated and
// zipped inner patterns with calling constructInnerIndexScanTask. That's not done yet, it also
// tries to enumerate kinds of index join operators based on the finished innerTask and un-decided
// outer child which will be physical-ed in the future.
func buildDataSource2IndexScanByIndexJoinProp(
	ds *logicalop.DataSource,
	prop *property.PhysicalProperty) base.Task {
	indexValid := func(path *util.AccessPath) bool {
		if path.IsTablePath() {
			return false
		}
		// if path is index path. index path currently include two kind of, one is normal, and the other is mv index.
		// for mv index like mvi(a, json, b), if driving condition is a=1, and we build a prefix scan with range [1,1]
		// on mvi, it will return many index rows which breaks handle-unique attribute here.
		//
		// the basic rule is that: mv index can be and can only be accessed by indexMerge operator. (embedded handle duplication)
		if !isMVIndexPath(path) {
			return true // not a MVIndex path, it can successfully be index join probe side.
		}
		return false
	}
	indexJoinResult, keyOff2IdxOff := getBestIndexJoinPathResultByProp(ds, prop.IndexJoinProp, indexValid)
	if indexJoinResult == nil {
		return base.InvalidTask
	}
	rangeInfo := indexJoinPathRangeInfo(ds.SCtx(), prop.IndexJoinProp.OuterJoinKeys, indexJoinResult)
	maxOneRow := false
	if indexJoinResult.chosenPath.Index.Unique && indexJoinResult.usedColsLen == len(indexJoinResult.chosenPath.FullIdxCols) {
		l := len(indexJoinResult.chosenAccess)
		if l == 0 {
			maxOneRow = true
		} else {
			sf, ok := indexJoinResult.chosenAccess[l-1].(*expression.ScalarFunction)
			maxOneRow = ok && (sf.FuncName.L == ast.EQ)
		}
	}
	var innerTask base.Task
	if !prop.IsSortItemEmpty() && isMatchProp(ds, indexJoinResult.chosenPath, prop) {
		innerTask = constructDS2IndexScanTask(ds, indexJoinResult.chosenPath, indexJoinResult.chosenRanges.Range(), indexJoinResult.chosenRemained, indexJoinResult.idxOff2KeyOff, rangeInfo, true, prop.SortItems[0].Desc, prop.IndexJoinProp.AvgInnerRowCnt, maxOneRow)
	} else {
		innerTask = constructDS2IndexScanTask(ds, indexJoinResult.chosenPath, indexJoinResult.chosenRanges.Range(), indexJoinResult.chosenRemained, indexJoinResult.idxOff2KeyOff, rangeInfo, false, false, prop.IndexJoinProp.AvgInnerRowCnt, maxOneRow)
	}
	// since there is a possibility that inner task can't be built and the returned value is nil, we just return base.InvalidTask.
	if innerTask == nil {
		return base.InvalidTask
	}
	// prepare the index path chosen information and wrap them as IndexJoinInfo and fill back to CopTask.
	// here we don't need to construct physical index join here anymore, because we will encapsulate it bottom-up.
	// chosenPath and lastColManager of indexJoinResult should be returned to the caller (seen by index join to keep
	// index join aware of indexColLens and compareFilters).
	completeIndexJoinFeedBackInfo(innerTask.(*CopTask), indexJoinResult, indexJoinResult.chosenRanges, keyOff2IdxOff)
	return innerTask
}

// buildDataSource2TableScanByIndexJoinProp builds a TableScan as the inner child for an
// IndexJoin if possible.
// If the inner side of an index join is a TableScan, only one tuple will be
// fetched from the inner side for every tuple from the outer side. This will be
// promised to be no worse than building IndexScan as the inner child.
func buildDataSource2TableScanByIndexJoinProp(
	ds *logicalop.DataSource,
	prop *property.PhysicalProperty) base.Task {
	var tblPath *util.AccessPath
	for _, path := range ds.PossibleAccessPaths {
		if path.IsTablePath() && path.StoreType == kv.TiKV { // old logic
			tblPath = path
			break
		}
	}
	if tblPath == nil {
		return base.InvalidTask
	}
	var keyOff2IdxOff []int
	var ranges ranger.MutableRanges = ranger.Ranges{}
	var innerTask base.Task
	var indexJoinResult *indexJoinPathResult
	if ds.TableInfo.IsCommonHandle {
		// for the leaf datasource, we use old logic to get the indexJoinResult, which contain the chosen path and ranges.
		indexJoinResult, keyOff2IdxOff = getBestIndexJoinPathResultByProp(ds, prop.IndexJoinProp, func(path *util.AccessPath) bool { return path.IsCommonHandlePath })
		// if there is no chosen info, it means the leaf datasource couldn't even leverage this indexJoinProp, return InvalidTask.
		if indexJoinResult == nil {
			return base.InvalidTask
		}
		// prepare the range info with outer join keys, it shows like: [xxx] decided by:
		rangeInfo := indexJoinPathRangeInfo(ds.SCtx(), prop.IndexJoinProp.OuterJoinKeys, indexJoinResult)
		// construct the inner task with chosen path and ranges, note: it only for this leaf datasource.
		// like the normal way, we need to check whether the chosen path is matched with the prop, if so, we will set the `keepOrder` to true.
		if isMatchProp(ds, indexJoinResult.chosenPath, prop) {
			innerTask = constructDS2TableScanTask(ds, indexJoinResult.chosenRanges.Range(), rangeInfo, true, !prop.IsSortItemEmpty() && prop.SortItems[0].Desc, prop.IndexJoinProp.AvgInnerRowCnt)
		} else {
			innerTask = constructDS2TableScanTask(ds, indexJoinResult.chosenRanges.Range(), rangeInfo, false, false, prop.IndexJoinProp.AvgInnerRowCnt)
		}
		ranges = indexJoinResult.chosenRanges
	} else {
		var (
			ok               bool
			chosenPath       *util.AccessPath
			newOuterJoinKeys []*expression.Column
			// note: pk col doesn't have mutableRanges, the global var(ranges) which will be handled as empty range in constructIndexJoin.
			localRanges ranger.Ranges
		)
		keyOff2IdxOff, newOuterJoinKeys, localRanges, chosenPath, ok = getIndexJoinIntPKPathInfo(ds, prop.IndexJoinProp.InnerJoinKeys, prop.IndexJoinProp.OuterJoinKeys, func(path *util.AccessPath) bool { return path.IsIntHandlePath })
		if !ok {
			return base.InvalidTask
		}
		rangeInfo := indexJoinIntPKRangeInfo(ds.SCtx().GetExprCtx().GetEvalCtx(), newOuterJoinKeys)
		if !prop.IsSortItemEmpty() && isMatchProp(ds, chosenPath, prop) {
			innerTask = constructDS2TableScanTask(ds, localRanges, rangeInfo, true, prop.SortItems[0].Desc, prop.IndexJoinProp.AvgInnerRowCnt)
		} else {
			innerTask = constructDS2TableScanTask(ds, localRanges, rangeInfo, false, false, prop.IndexJoinProp.AvgInnerRowCnt)
		}
	}
	// since there is a possibility that inner task can't be built and the returned value is nil, we just return base.InvalidTask.
	if innerTask == nil {
		return base.InvalidTask
	}
	// prepare the index path chosen information and wrap them as IndexJoinInfo and fill back to CopTask.
	// here we don't need to construct physical index join here anymore, because we will encapsulate it bottom-up.
	// chosenPath and lastColManager of indexJoinResult should be returned to the caller (seen by index join to keep
	// index join aware of indexColLens and compareFilters).
	completeIndexJoinFeedBackInfo(innerTask.(*CopTask), indexJoinResult, ranges, keyOff2IdxOff)
	return innerTask
}

// completeIndexJoinFeedBackInfo completes the IndexJoinInfo for the innerTask.
// indexJoin
//
//	+--- outer child
//	+--- inner child (say: projection ------------> unionScan -------------> ds)
//	        <-------RootTask(IndexJoinInfo) <--RootTask(IndexJoinInfo) <--copTask(IndexJoinInfo)
//
// when we build the underlying datasource as table-scan, we will return wrap it and
// return as a CopTask, inside which the index join contains some index path chosen
// information which will be used in indexJoin execution runtime: ref IndexJoinInfo
// declaration for more information.
// the indexJoinInfo will be filled back to the innerTask, passed upward to RootTask
// once this copTask is converted to RootTask type, and finally end up usage in the
// indexJoin's attach2Task with calling completePhysicalIndexJoin.
func completeIndexJoinFeedBackInfo(innerTask *CopTask, indexJoinResult *indexJoinPathResult, ranges ranger.MutableRanges, keyOff2IdxOff []int) {
	info := innerTask.IndexJoinInfo
	if info == nil {
		info = &IndexJoinInfo{}
	}
	if indexJoinResult != nil {
		if indexJoinResult.chosenPath != nil {
			info.IdxColLens = indexJoinResult.chosenPath.IdxColLens
		}
		info.CompareFilters = indexJoinResult.lastColManager
	}
	info.Ranges = ranges
	info.KeyOff2IdxOff = keyOff2IdxOff
	// fill it back to the bottom-up Task.
	innerTask.IndexJoinInfo = info
}

// buildIndexJoinInner2TableScan builds a TableScan as the inner child for an
// IndexJoin if possible.
// If the inner side of an index join is a TableScan, only one tuple will be
// fetched from the inner side for every tuple from the outer side. This will be
// promised to be no worse than building IndexScan as the inner child.
func buildIndexJoinInner2TableScan(
	p *logicalop.LogicalJoin,
	prop *property.PhysicalProperty, wrapper *indexJoinInnerChildWrapper,
	innerJoinKeys, outerJoinKeys []*expression.Column,
	outerIdx int, avgInnerRowCnt float64) (joins []base.PhysicalPlan) {
	ds := wrapper.ds
	var tblPath *util.AccessPath
	for _, path := range ds.PossibleAccessPaths {
		if path.IsTablePath() && path.StoreType == kv.TiKV {
			tblPath = path
			break
		}
	}
	if tblPath == nil {
		return nil
	}
	var keyOff2IdxOff []int
	var ranges ranger.MutableRanges = ranger.Ranges{}
	var innerTask, innerTask2 base.Task
	var indexJoinResult *indexJoinPathResult
	if ds.TableInfo.IsCommonHandle {
		indexJoinResult, keyOff2IdxOff = getBestIndexJoinPathResult(p, ds, innerJoinKeys, outerJoinKeys, func(path *util.AccessPath) bool { return path.IsCommonHandlePath })
		if indexJoinResult == nil {
			return nil
		}
		rangeInfo := indexJoinPathRangeInfo(p.SCtx(), outerJoinKeys, indexJoinResult)
		innerTask = constructInnerTableScanTask(p, prop, wrapper, indexJoinResult.chosenRanges.Range(), rangeInfo, false, false, avgInnerRowCnt)
		// The index merge join's inner plan is different from index join, so we
		// should construct another inner plan for it.
		// Because we can't keep order for union scan, if there is a union scan in inner task,
		// we can't construct index merge join.
		if !wrapper.hasDitryWrite {
			innerTask2 = constructInnerTableScanTask(p, prop, wrapper, indexJoinResult.chosenRanges.Range(), rangeInfo, true, !prop.IsSortItemEmpty() && prop.SortItems[0].Desc, avgInnerRowCnt)
		}
		ranges = indexJoinResult.chosenRanges
	} else {
		var (
			ok bool
			// note: pk col doesn't have mutableRanges, the global var(ranges) which will be handled as empty range in constructIndexJoin.
			localRanges ranger.Ranges
		)
		keyOff2IdxOff, outerJoinKeys, localRanges, _, ok = getIndexJoinIntPKPathInfo(ds, innerJoinKeys, outerJoinKeys, func(path *util.AccessPath) bool { return path.IsIntHandlePath })
		if !ok {
			return nil
		}
		rangeInfo := indexJoinIntPKRangeInfo(p.SCtx().GetExprCtx().GetEvalCtx(), outerJoinKeys)
		innerTask = constructInnerTableScanTask(p, prop, wrapper, localRanges, rangeInfo, false, false, avgInnerRowCnt)
		// The index merge join's inner plan is different from index join, so we
		// should construct another inner plan for it.
		// Because we can't keep order for union scan, if there is a union scan in inner task,
		// we can't construct index merge join.
		if !wrapper.hasDitryWrite {
			innerTask2 = constructInnerTableScanTask(p, prop, wrapper, localRanges, rangeInfo, true, !prop.IsSortItemEmpty() && prop.SortItems[0].Desc, avgInnerRowCnt)
		}
	}
	var (
		path       *util.AccessPath
		lastColMng *physicalop.ColWithCmpFuncManager
	)
	if indexJoinResult != nil {
		path = indexJoinResult.chosenPath
		lastColMng = indexJoinResult.lastColManager
	}
	joins = make([]base.PhysicalPlan, 0, 3)
	failpoint.Inject("MockOnlyEnableIndexHashJoin", func(val failpoint.Value) {
		if val.(bool) && !p.SCtx().GetSessionVars().InRestrictedSQL {
			failpoint.Return(constructIndexHashJoin(p, prop, outerIdx, innerTask, nil, keyOff2IdxOff, path, lastColMng))
		}
	})
	joins = append(joins, constructIndexJoin(p, prop, outerIdx, innerTask, ranges, keyOff2IdxOff, path, lastColMng, true)...)
	// We can reuse the `innerTask` here since index nested loop hash join
	// do not need the inner child to promise the order.
	joins = append(joins, constructIndexHashJoin(p, prop, outerIdx, innerTask, ranges, keyOff2IdxOff, path, lastColMng)...)
	if innerTask2 != nil {
		joins = append(joins, constructIndexMergeJoin(p, prop, outerIdx, innerTask2, ranges, keyOff2IdxOff, path, lastColMng)...)
	}
	return joins
}

func buildIndexJoinInner2IndexScan(
	p *logicalop.LogicalJoin,
	prop *property.PhysicalProperty, wrapper *indexJoinInnerChildWrapper, innerJoinKeys, outerJoinKeys []*expression.Column,
	outerIdx int, avgInnerRowCnt float64) (joins []base.PhysicalPlan) {
	ds := wrapper.ds
	indexValid := func(path *util.AccessPath) bool {
		if path.IsTablePath() {
			return false
		}
		// if path is index path. index path currently include two kind of, one is normal, and the other is mv index.
		// for mv index like mvi(a, json, b), if driving condition is a=1, and we build a prefix scan with range [1,1]
		// on mvi, it will return many index rows which breaks handle-unique attribute here.
		//
		// the basic rule is that: mv index can be and can only be accessed by indexMerge operator. (embedded handle duplication)
		if !isMVIndexPath(path) {
			return true // not a MVIndex path, it can successfully be index join probe side.
		}
		return false
	}
	indexJoinResult, keyOff2IdxOff := getBestIndexJoinPathResult(p, ds, innerJoinKeys, outerJoinKeys, indexValid)
	if indexJoinResult == nil {
		return nil
	}
	joins = make([]base.PhysicalPlan, 0, 3)
	rangeInfo := indexJoinPathRangeInfo(p.SCtx(), outerJoinKeys, indexJoinResult)
	maxOneRow := false
	if indexJoinResult.chosenPath.Index.Unique && indexJoinResult.usedColsLen == len(indexJoinResult.chosenPath.FullIdxCols) {
		l := len(indexJoinResult.chosenAccess)
		if l == 0 {
			maxOneRow = true
		} else {
			sf, ok := indexJoinResult.chosenAccess[l-1].(*expression.ScalarFunction)
			maxOneRow = ok && (sf.FuncName.L == ast.EQ)
		}
	}
	innerTask := constructInnerIndexScanTask(p, prop, wrapper, indexJoinResult.chosenPath, indexJoinResult.chosenRanges.Range(), indexJoinResult.chosenRemained, indexJoinResult.idxOff2KeyOff, rangeInfo, false, false, avgInnerRowCnt, maxOneRow)
	failpoint.Inject("MockOnlyEnableIndexHashJoin", func(val failpoint.Value) {
		if val.(bool) && !p.SCtx().GetSessionVars().InRestrictedSQL && innerTask != nil {
			failpoint.Return(constructIndexHashJoin(p, prop, outerIdx, innerTask, indexJoinResult.chosenRanges, keyOff2IdxOff, indexJoinResult.chosenPath, indexJoinResult.lastColManager))
		}
	})
	if innerTask != nil {
		joins = append(joins, constructIndexJoin(p, prop, outerIdx, innerTask, indexJoinResult.chosenRanges, keyOff2IdxOff, indexJoinResult.chosenPath, indexJoinResult.lastColManager, true)...)
		// We can reuse the `innerTask` here since index nested loop hash join
		// do not need the inner child to promise the order.
		joins = append(joins, constructIndexHashJoin(p, prop, outerIdx, innerTask, indexJoinResult.chosenRanges, keyOff2IdxOff, indexJoinResult.chosenPath, indexJoinResult.lastColManager)...)
	}
	// The index merge join's inner plan is different from index join, so we
	// should construct another inner plan for it.
	// Because we can't keep order for union scan, if there is a union scan in inner task,
	// we can't construct index merge join.
	if !wrapper.hasDitryWrite {
		innerTask2 := constructInnerIndexScanTask(p, prop, wrapper, indexJoinResult.chosenPath, indexJoinResult.chosenRanges.Range(), indexJoinResult.chosenRemained, indexJoinResult.idxOff2KeyOff, rangeInfo, true, !prop.IsSortItemEmpty() && prop.SortItems[0].Desc, avgInnerRowCnt, maxOneRow)
		if innerTask2 != nil {
			joins = append(joins, constructIndexMergeJoin(p, prop, outerIdx, innerTask2, indexJoinResult.chosenRanges, keyOff2IdxOff, indexJoinResult.chosenPath, indexJoinResult.lastColManager)...)
		}
	}
	return joins
}

// constructInnerTableScanTask is specially used to construct the inner plan for PhysicalIndexJoin.
func constructInnerTableScanTask(
	p *logicalop.LogicalJoin,
	prop *property.PhysicalProperty,
	wrapper *indexJoinInnerChildWrapper,
	ranges ranger.Ranges,
	rangeInfo string,
	keepOrder bool,
	desc bool,
	rowCount float64,
) base.Task {
	copTask := constructDS2TableScanTask(wrapper.ds, ranges, rangeInfo, keepOrder, desc, rowCount)
	if copTask == nil {
		return nil
	}
	return constructIndexJoinInnerSideTaskWithAggCheck(p, prop, copTask.(*CopTask), wrapper.ds, nil, wrapper)
}

// constructInnerTableScanTask is specially used to construct the inner plan for PhysicalIndexJoin.
func constructDS2TableScanTask(
	ds *logicalop.DataSource,
	ranges ranger.Ranges,
	rangeInfo string,
	keepOrder bool,
	desc bool,
	rowCount float64,
) base.Task {
	// If `ds.TableInfo.GetPartitionInfo() != nil`,
	// it means the data source is a partition table reader.
	// If the inner task need to keep order, the partition table reader can't satisfy it.
	if keepOrder && ds.TableInfo.GetPartitionInfo() != nil {
		return nil
	}
	ts := PhysicalTableScan{
		Table:           ds.TableInfo,
		Columns:         ds.Columns,
		TableAsName:     ds.TableAsName,
		DBName:          ds.DBName,
		filterCondition: ds.PushedDownConds,
		Ranges:          ranges,
		rangeInfo:       rangeInfo,
		KeepOrder:       keepOrder,
		Desc:            desc,
		physicalTableID: ds.PhysicalTableID,
		isPartition:     ds.PartitionDefIdx != nil,
		tblCols:         ds.TblCols,
		tblColHists:     ds.TblColHists,
	}.Init(ds.SCtx(), ds.QueryBlockOffset())
	ts.SetSchema(ds.Schema().Clone())
	if rowCount <= 0 {
		rowCount = float64(1)
	}
	selectivity := float64(1)
	countAfterAccess := rowCount
	if len(ts.filterCondition) > 0 {
		var err error
		selectivity, _, err = cardinality.Selectivity(ds.SCtx(), ds.TableStats.HistColl, ts.filterCondition, ds.PossibleAccessPaths)
		if err != nil || selectivity <= 0 {
			logutil.BgLogger().Debug("unexpected selectivity, use selection factor", zap.Float64("selectivity", selectivity), zap.String("table", ts.TableAsName.L))
			selectivity = cost.SelectionFactor
		}
		// rowCount is computed from result row count of join, which has already accounted the filters on DataSource,
		// i.e, rowCount equals to `countAfterAccess * selectivity`.
		countAfterAccess = rowCount / selectivity
	}
	ts.SetStats(&property.StatsInfo{
		// TableScan as inner child of IndexJoin can return at most 1 tuple for each outer row.
		RowCount:     math.Min(1.0, countAfterAccess),
		StatsVersion: ds.StatsInfo().StatsVersion,
		// NDV would not be used in cost computation of IndexJoin, set leave it as default nil.
	})
	usedStats := ds.SCtx().GetSessionVars().StmtCtx.GetUsedStatsInfo(false)
	if usedStats != nil && usedStats.GetUsedInfo(ts.physicalTableID) != nil {
		ts.usedStatsInfo = usedStats.GetUsedInfo(ts.physicalTableID)
	}
	copTask := &CopTask{
		tablePlan:         ts,
		indexPlanFinished: true,
		tblColHists:       ds.TblColHists,
		keepOrder:         ts.KeepOrder,
	}
	copTask.physPlanPartInfo = &PhysPlanPartInfo{
		PruningConds:   ds.AllConds,
		PartitionNames: ds.PartitionNames,
		Columns:        ds.TblCols,
		ColumnNames:    ds.OutputNames(),
	}
	ts.PlanPartInfo = copTask.physPlanPartInfo
	selStats := ts.StatsInfo().Scale(selectivity)
	ts.addPushedDownSelection(copTask, selStats, ds.AstIndexHints)
	return copTask
}

func constructIndexJoinInnerSideTask(curTask base.Task, prop *property.PhysicalProperty, zippedChildren []base.LogicalPlan, skipAgg bool) base.Task {
	for i := len(zippedChildren) - 1; i >= 0; i-- {
		switch x := zippedChildren[i].(type) {
		case *logicalop.LogicalUnionScan:
			curTask = constructInnerUnionScan(prop, x, curTask.Plan()).Attach2Task(curTask)
		case *logicalop.LogicalProjection:
			curTask = constructInnerProj(prop, x, curTask.Plan()).Attach2Task(curTask)
		case *logicalop.LogicalSelection:
			curTask = constructInnerSel(prop, x, curTask.Plan()).Attach2Task(curTask)
		case *logicalop.LogicalAggregation:
			if skipAgg {
				continue
			}
			curTask = constructInnerAgg(prop, x, curTask.Plan()).Attach2Task(curTask)
		}
		if curTask.Invalid() {
			return nil
		}
	}
	return curTask
}

func constructInnerAgg(prop *property.PhysicalProperty, logicalAgg *logicalop.LogicalAggregation, child base.PhysicalPlan) base.PhysicalPlan {
	if logicalAgg == nil {
		return child
	}
	physicalHashAgg := NewPhysicalHashAgg(logicalAgg, logicalAgg.StatsInfo(), prop)
	physicalHashAgg.SetSchema(logicalAgg.Schema().Clone())
	return physicalHashAgg
}

func constructInnerSel(prop *property.PhysicalProperty, sel *logicalop.LogicalSelection, child base.PhysicalPlan) base.PhysicalPlan {
	if sel == nil {
		return child
	}
	physicalSel := physicalop.PhysicalSelection{
		Conditions: sel.Conditions,
	}.Init(sel.SCtx(), sel.StatsInfo(), sel.QueryBlockOffset(), prop)
	return physicalSel
}

func constructInnerProj(prop *property.PhysicalProperty, proj *logicalop.LogicalProjection, child base.PhysicalPlan) base.PhysicalPlan {
	if proj == nil {
		return child
	}
	physicalProj := physicalop.PhysicalProjection{
		Exprs:            proj.Exprs,
		CalculateNoDelay: proj.CalculateNoDelay,
	}.Init(proj.SCtx(), proj.StatsInfo(), proj.QueryBlockOffset(), prop)
	physicalProj.SetSchema(proj.Schema())
	return physicalProj
}

func constructInnerUnionScan(prop *property.PhysicalProperty, us *logicalop.LogicalUnionScan, childPlan base.PhysicalPlan) base.PhysicalPlan {
	if us == nil {
		return childPlan
	}
	// Use `reader.StatsInfo()` instead of `us.StatsInfo()` because it should be more accurate. No need to specify
	// childrenReqProps now since we have got reader already.
	physicalUnionScan := physicalop.PhysicalUnionScan{
		Conditions: us.Conditions,
		HandleCols: us.HandleCols,
	}.Init(us.SCtx(), childPlan.StatsInfo(), us.QueryBlockOffset(), prop)
	return physicalUnionScan
}

// getColsNDVLowerBoundFromHistColl tries to get a lower bound of the NDV of columns (whose uniqueIDs are colUIDs).
func getColsNDVLowerBoundFromHistColl(colUIDs []int64, histColl *statistics.HistColl) int64 {
	if len(colUIDs) == 0 || histColl == nil {
		return -1
	}

	// 1. Try to get NDV from column stats if it's a single column.
	if len(colUIDs) == 1 && histColl.ColNum() > 0 {
		uid := colUIDs[0]
		if colStats := histColl.GetCol(uid); colStats != nil && colStats.IsStatsInitialized() {
			return colStats.NDV
		}
	}

	slices.Sort(colUIDs)

	// 2. Try to get NDV from index stats.
	// Note that we don't need to specially handle prefix index here, because the NDV of a prefix index is
	// equal or less than the corresponding normal index, and that's safe here since we want a lower bound.
	for idxID, idxCols := range histColl.Idx2ColUniqueIDs {
		if len(idxCols) != len(colUIDs) {
			continue
		}
		orderedIdxCols := make([]int64, len(idxCols))
		copy(orderedIdxCols, idxCols)
		slices.Sort(orderedIdxCols)
		if !slices.Equal(orderedIdxCols, colUIDs) {
			continue
		}
		if idxStats := histColl.GetIdx(idxID); idxStats != nil && idxStats.IsStatsInitialized() {
			return idxStats.NDV
		}
	}

	// TODO: if there's an index that contains the expected columns, we can also make use of its NDV.
	// For example, NDV(a,b,c) / NDV(c) is a safe lower bound of NDV(a,b).

	// 3. If we still haven't got an NDV, we use the maximum NDV in the column stats as a lower bound.
	maxNDV := int64(-1)
	for _, uid := range colUIDs {
		colStats := histColl.GetCol(uid)
		if colStats == nil || !colStats.IsStatsInitialized() {
			continue
		}
		maxNDV = max(maxNDV, colStats.NDV)
	}
	return maxNDV
}

// constructInnerIndexScanTask is specially used to construct the inner plan for PhysicalIndexJoin.
func constructInnerIndexScanTask(
	p *logicalop.LogicalJoin,
	prop *property.PhysicalProperty,
	wrapper *indexJoinInnerChildWrapper,
	path *util.AccessPath,
	ranges ranger.Ranges,
	filterConds []expression.Expression,
	idxOffset2joinKeyOffset []int,
	rangeInfo string,
	keepOrder bool,
	desc bool,
	rowCount float64,
	maxOneRow bool,
) base.Task {
	copTask := constructDS2IndexScanTask(wrapper.ds, path, ranges, filterConds, idxOffset2joinKeyOffset, rangeInfo, keepOrder, desc, rowCount, maxOneRow)
	if copTask == nil {
		return nil
	}
	return constructIndexJoinInnerSideTaskWithAggCheck(p, prop, copTask.(*CopTask), wrapper.ds, path, wrapper)
}

// constructDS2IndexScanTask is specially used to construct the inner plan for PhysicalIndexJoin.
func constructDS2IndexScanTask(
	ds *logicalop.DataSource,
	path *util.AccessPath,
	ranges ranger.Ranges,
	filterConds []expression.Expression,
	idxOffset2joinKeyOffset []int,
	rangeInfo string,
	keepOrder bool,
	desc bool,
	rowCount float64,
	maxOneRow bool,
) base.Task {
	// If `ds.TableInfo.GetPartitionInfo() != nil`,
	// it means the data source is a partition table reader.
	// If the inner task need to keep order, the partition table reader can't satisfy it.
	if keepOrder && ds.TableInfo.GetPartitionInfo() != nil {
		return nil
	}
	is := PhysicalIndexScan{
		Table:            ds.TableInfo,
		TableAsName:      ds.TableAsName,
		DBName:           ds.DBName,
		Columns:          ds.Columns,
		Index:            path.Index,
		IdxCols:          path.IdxCols,
		IdxColLens:       path.IdxColLens,
		dataSourceSchema: ds.Schema(),
		KeepOrder:        keepOrder,
		Ranges:           ranges,
		rangeInfo:        rangeInfo,
		Desc:             desc,
		isPartition:      ds.PartitionDefIdx != nil,
		physicalTableID:  ds.PhysicalTableID,
		tblColHists:      ds.TblColHists,
		pkIsHandleCol:    ds.GetPKIsHandleCol(),
	}.Init(ds.SCtx(), ds.QueryBlockOffset())
	cop := &CopTask{
		indexPlan:   is,
		tblColHists: ds.TblColHists,
		tblCols:     ds.TblCols,
		keepOrder:   is.KeepOrder,
	}
	cop.physPlanPartInfo = &PhysPlanPartInfo{
		PruningConds:   ds.AllConds,
		PartitionNames: ds.PartitionNames,
		Columns:        ds.TblCols,
		ColumnNames:    ds.OutputNames(),
	}
	if !path.IsSingleScan {
		// On this way, it's double read case.
		ts := PhysicalTableScan{
			Columns:         ds.Columns,
			Table:           is.Table,
			TableAsName:     ds.TableAsName,
			DBName:          ds.DBName,
			isPartition:     ds.PartitionDefIdx != nil,
			physicalTableID: ds.PhysicalTableID,
			tblCols:         ds.TblCols,
			tblColHists:     ds.TblColHists,
		}.Init(ds.SCtx(), ds.QueryBlockOffset())
		ts.SetSchema(is.dataSourceSchema.Clone())
		if ds.TableInfo.IsCommonHandle {
			commonHandle := ds.HandleCols.(*util.CommonHandleCols)
			for _, col := range commonHandle.GetColumns() {
				if ts.Schema().ColumnIndex(col) == -1 {
					ts.Schema().Append(col)
					ts.Columns = append(ts.Columns, col.ToInfo())
					cop.needExtraProj = true
				}
			}
		}
		// We set `StatsVersion` here and fill other fields in `(*copTask).finishIndexPlan`. Since `copTask.indexPlan` may
		// change before calling `(*copTask).finishIndexPlan`, we don't know the stats information of `ts` currently and on
		// the other hand, it may be hard to identify `StatsVersion` of `ts` in `(*copTask).finishIndexPlan`.
		ts.SetStats(&property.StatsInfo{StatsVersion: ds.TableStats.StatsVersion})
		usedStats := ds.SCtx().GetSessionVars().StmtCtx.GetUsedStatsInfo(false)
		if usedStats != nil && usedStats.GetUsedInfo(ts.physicalTableID) != nil {
			ts.usedStatsInfo = usedStats.GetUsedInfo(ts.physicalTableID)
		}
		// If inner cop task need keep order, the extraHandleCol should be set.
		if cop.keepOrder && !ds.TableInfo.IsCommonHandle {
			var needExtraProj bool
			cop.extraHandleCol, needExtraProj = ts.appendExtraHandleCol(ds)
			cop.needExtraProj = cop.needExtraProj || needExtraProj
		}
		if cop.needExtraProj {
			cop.originSchema = ds.Schema()
		}
		cop.tablePlan = ts
	}
	if cop.tablePlan != nil && ds.TableInfo.IsCommonHandle {
		cop.commonHandleCols = ds.CommonHandleCols
	}
	is.initSchema(append(path.FullIdxCols, ds.CommonHandleCols...), cop.tablePlan != nil)
	indexConds, tblConds := splitIndexFilterConditions(ds, filterConds, path.FullIdxCols, path.FullIdxColLens)

	// Note: due to a regression in JOB workload, we use the optimizer fix control to enable this for now.
	//
	// Because we are estimating an average row count of the inner side corresponding to each row from the outer side,
	// the estimated row count of the IndexScan should be no larger than (total row count / NDV of join key columns).
	// We can calculate the lower bound of the NDV therefore we can get an upper bound of the row count here.
	rowCountUpperBound := -1.0
	fixControlOK := fixcontrol.GetBoolWithDefault(ds.SCtx().GetSessionVars().GetOptimizerFixControlMap(), fixcontrol.Fix44855, false)
	ds.SCtx().GetSessionVars().RecordRelevantOptFix(fixcontrol.Fix44855)
	if fixControlOK && ds.TableStats != nil {
		usedColIDs := make([]int64, 0)
		// We only consider columns in this index that (1) are used to probe as join key,
		// and (2) are not prefix column in the index (for which we can't easily get a lower bound)
		for idxOffset, joinKeyOffset := range idxOffset2joinKeyOffset {
			if joinKeyOffset < 0 ||
				path.FullIdxColLens[idxOffset] != types.UnspecifiedLength ||
				path.FullIdxCols[idxOffset] == nil {
				continue
			}
			usedColIDs = append(usedColIDs, path.FullIdxCols[idxOffset].UniqueID)
		}
		joinKeyNDV := getColsNDVLowerBoundFromHistColl(usedColIDs, ds.TableStats.HistColl)
		if joinKeyNDV > 0 {
			rowCountUpperBound = ds.TableStats.RowCount / float64(joinKeyNDV)
		}
	}

	if rowCountUpperBound > 0 {
		rowCount = math.Min(rowCount, rowCountUpperBound)
	}
	if maxOneRow {
		// Theoretically, this line is unnecessary because row count estimation of join should guarantee rowCount is not larger
		// than 1.0; however, there may be rowCount larger than 1.0 in reality, e.g, pseudo statistics cases, which does not reflect
		// unique constraint in NDV.
		rowCount = math.Min(rowCount, 1.0)
	}
	tmpPath := &util.AccessPath{
		IndexFilters:         indexConds,
		TableFilters:         tblConds,
		CountAfterIndex:      rowCount,
		CountAfterAccess:     rowCount,
		CorrCountAfterAccess: 0,
	}
	// Assume equal conditions used by index join and other conditions are independent.
	if len(tblConds) > 0 {
		selectivity, _, err := cardinality.Selectivity(ds.SCtx(), ds.TableStats.HistColl, tblConds, ds.PossibleAccessPaths)
		if err != nil || selectivity <= 0 {
			logutil.BgLogger().Debug("unexpected selectivity, use selection factor", zap.Float64("selectivity", selectivity), zap.String("table", ds.TableAsName.L))
			selectivity = cost.SelectionFactor
		}
		// rowCount is computed from result row count of join, which has already accounted the filters on DataSource,
		// i.e, rowCount equals to `countAfterIndex * selectivity`.
		cnt := rowCount / selectivity
		if rowCountUpperBound > 0 {
			cnt = math.Min(cnt, rowCountUpperBound)
		}
		if maxOneRow {
			cnt = math.Min(cnt, 1.0)
		}
		tmpPath.CountAfterIndex = cnt
		tmpPath.CountAfterAccess = cnt
	}
	if len(indexConds) > 0 {
		selectivity, _, err := cardinality.Selectivity(ds.SCtx(), ds.TableStats.HistColl, indexConds, ds.PossibleAccessPaths)
		if err != nil || selectivity <= 0 {
			logutil.BgLogger().Debug("unexpected selectivity, use selection factor", zap.Float64("selectivity", selectivity), zap.String("table", ds.TableAsName.L))
			selectivity = cost.SelectionFactor
		}
		cnt := tmpPath.CountAfterIndex / selectivity
		if rowCountUpperBound > 0 {
			cnt = math.Min(cnt, rowCountUpperBound)
		}
		if maxOneRow {
			cnt = math.Min(cnt, 1.0)
		}
		tmpPath.CountAfterAccess = cnt
	}
	is.SetStats(ds.TableStats.ScaleByExpectCnt(tmpPath.CountAfterAccess))
	usedStats := ds.SCtx().GetSessionVars().StmtCtx.GetUsedStatsInfo(false)
	if usedStats != nil && usedStats.GetUsedInfo(is.physicalTableID) != nil {
		is.usedStatsInfo = usedStats.GetUsedInfo(is.physicalTableID)
	}
	finalStats := ds.TableStats.ScaleByExpectCnt(rowCount)
	if err := is.addPushedDownSelection(cop, ds, tmpPath, finalStats); err != nil {
		logutil.BgLogger().Warn("unexpected error happened during addPushedDownSelection function", zap.Error(err))
		return nil
	}
	return cop
}

// construct the inner join task by inner child plan tree
// The Logical include two parts: logicalplan->physicalplan, physicalplan->task
// Step1: whether agg can be pushed down to coprocessor
//
//	Step1.1: If the agg can be pushded down to coprocessor, we will build a copTask and attach the agg to the copTask
//	There are two kinds of agg: stream agg and hash agg. Stream agg depends on some conditions, such as the group by cols
//
// Step2: build other inner plan node to task
func constructIndexJoinInnerSideTaskWithAggCheck(p *logicalop.LogicalJoin, prop *property.PhysicalProperty, dsCopTask *CopTask, ds *logicalop.DataSource, path *util.AccessPath, wrapper *indexJoinInnerChildWrapper) base.Task {
	var la *logicalop.LogicalAggregation
	var canPushAggToCop bool
	if len(wrapper.zippedChildren) > 0 {
		la, canPushAggToCop = wrapper.zippedChildren[len(wrapper.zippedChildren)-1].(*logicalop.LogicalAggregation)
		if la != nil && la.HasDistinct() {
			// TODO: remove AllowDistinctAggPushDown after the cost estimation of distinct pushdown is implemented.
			// If AllowDistinctAggPushDown is set to true, we should not consider RootTask.
			if !la.SCtx().GetSessionVars().AllowDistinctAggPushDown {
				canPushAggToCop = false
			}
		}
	}

	// If the bottom plan is not aggregation or the aggregation can't be pushed to coprocessor, we will construct a root task directly.
	if !canPushAggToCop {
		result := dsCopTask.ConvertToRootTask(ds.SCtx()).(*RootTask)
		return constructIndexJoinInnerSideTask(result, prop, wrapper.zippedChildren, false)
	}

	numAgg := 0
	for _, child := range wrapper.zippedChildren {
		if _, ok := child.(*logicalop.LogicalAggregation); ok {
			numAgg++
		}
	}
	if numAgg > 1 {
		// can't support this case now, see #61669.
		return base.InvalidTask
	}

	// Try stream aggregation first.
	// We will choose the stream aggregation if the following conditions are met:
	// 1. Force hint stream agg by /*+ stream_agg() */
	// 2. Other conditions copy from getStreamAggs() in exhaust_physical_plans.go
	_, preferStream := la.ResetHintIfConflicted()
	for _, aggFunc := range la.AggFuncs {
		if aggFunc.Mode == aggregation.FinalMode {
			preferStream = false
			break
		}
	}
	// group by a + b is not interested in any order.
	groupByCols := la.GetGroupByCols()
	if len(groupByCols) != len(la.GroupByItems) {
		preferStream = false
	}
	if la.HasDistinct() && !la.DistinctArgsMeetsProperty() {
		preferStream = false
	}
	// sort items must be the super set of group by items
	if path != nil && path.Index != nil && !path.Index.MVIndex &&
		ds.TableInfo.GetPartitionInfo() == nil {
		if len(path.IdxCols) < len(groupByCols) {
			preferStream = false
		} else {
			sctx := p.SCtx()
			for i, groupbyCol := range groupByCols {
				if path.IdxColLens[i] != types.UnspecifiedLength ||
					!groupbyCol.EqualByExprAndID(sctx.GetExprCtx().GetEvalCtx(), path.IdxCols[i]) {
					preferStream = false
				}
			}
		}
	} else {
		preferStream = false
	}

	// build physical agg and attach to task
	var aggTask base.Task
	// build stream agg and change ds keep order to true
	stats := la.StatsInfo()
	if dsCopTask.indexPlan != nil {
		stats = stats.ScaleByExpectCnt(dsCopTask.indexPlan.StatsInfo().RowCount)
	} else if dsCopTask.tablePlan != nil {
		stats = stats.ScaleByExpectCnt(dsCopTask.tablePlan.StatsInfo().RowCount)
	}
	if preferStream {
		newGbyItems := make([]expression.Expression, len(la.GroupByItems))
		copy(newGbyItems, la.GroupByItems)
		newAggFuncs := make([]*aggregation.AggFuncDesc, len(la.AggFuncs))
		copy(newAggFuncs, la.AggFuncs)
		baseAgg := &physicalop.BasePhysicalAgg{
			GroupByItems: newGbyItems,
			AggFuncs:     newAggFuncs,
		}
		streamAgg := baseAgg.InitForStream(la.SCtx(), la.StatsInfo(), la.QueryBlockOffset(), la.Schema().Clone(), prop)
		// change to keep order for index scan and dsCopTask
		if dsCopTask.indexPlan != nil {
			// get the index scan from dsCopTask.indexPlan
			physicalIndexScan, _ := dsCopTask.indexPlan.(*PhysicalIndexScan)
			if physicalIndexScan == nil && len(dsCopTask.indexPlan.Children()) == 1 {
				physicalIndexScan, _ = dsCopTask.indexPlan.Children()[0].(*PhysicalIndexScan)
			}
			// The double read case should change the table plan together if we want to build stream agg,
			// so it need to find out the table scan
			// Try to get the physical table scan from dsCopTask.tablePlan
			// now, we only support the pattern tablescan and tablescan+selection
			var physicalTableScan *PhysicalTableScan
			if dsCopTask.tablePlan != nil {
				physicalTableScan, _ = dsCopTask.tablePlan.(*PhysicalTableScan)
				if physicalTableScan == nil && len(dsCopTask.tablePlan.Children()) == 1 {
					physicalTableScan, _ = dsCopTask.tablePlan.Children()[0].(*PhysicalTableScan)
				}
				// We may not be able to build stream agg, break here and directly build hash agg
				if physicalTableScan == nil {
					goto buildHashAgg
				}
			}
			if physicalIndexScan != nil {
				physicalIndexScan.KeepOrder = true
				dsCopTask.keepOrder = true
				// Fix issue #60297, if index lookup(double read) as build side and table key is not common handle(row_id),
				// we need to reset extraHandleCol and needExtraProj.
				// The reason why the reset cop task needs to be specially modified here is that:
				// The cop task has been constructed in the previous logic,
				// but it was not possible to determine whether the stream agg was needed (that is, whether keep order was true).
				// Therefore, when updating the keep order, the relevant properties in the cop task need to be modified at the same time.
				// The following code is copied from the logic when keep order is true in function constructDS2IndexScanTask.
				if dsCopTask.tablePlan != nil && physicalTableScan != nil && !ds.TableInfo.IsCommonHandle {
					var needExtraProj bool
					dsCopTask.extraHandleCol, needExtraProj = physicalTableScan.appendExtraHandleCol(ds)
					dsCopTask.needExtraProj = dsCopTask.needExtraProj || needExtraProj
				}
				if dsCopTask.needExtraProj {
					dsCopTask.originSchema = ds.Schema()
				}
				streamAgg.SetStats(stats)
				aggTask = streamAgg.Attach2Task(dsCopTask)
			}
		}
	}

buildHashAgg:
	// build hash agg, when the stream agg is illegal such as the order by prop is not matched
	if aggTask == nil {
		physicalHashAgg := NewPhysicalHashAgg(la, stats, prop)
		physicalHashAgg.SetSchema(la.Schema().Clone())
		aggTask = physicalHashAgg.Attach2Task(dsCopTask)
	}

	// build other inner plan node to task
	result, ok := aggTask.(*RootTask)
	if !ok {
		return nil
	}
	return constructIndexJoinInnerSideTask(result, prop, wrapper.zippedChildren, true)
}

func filterIndexJoinBySessionVars(sc base.PlanContext, indexJoins []base.PhysicalPlan) []base.PhysicalPlan {
	if sc.GetSessionVars().EnableIndexMergeJoin {
		return indexJoins
	}
	return slices.DeleteFunc(indexJoins, func(indexJoin base.PhysicalPlan) bool {
		_, ok := indexJoin.(*PhysicalIndexMergeJoin)
		return ok
	})
}

const (
	joinLeft             = 0
	joinRight            = 1
	indexJoinMethod      = 0
	indexHashJoinMethod  = 1
	indexMergeJoinMethod = 2
)

func getIndexJoinSideAndMethod(join base.PhysicalPlan) (innerSide, joinMethod int, ok bool) {
	var innerIdx int
	switch ij := join.(type) {
	case *physicalop.PhysicalIndexJoin:
		innerIdx = ij.GetInnerChildIdx()
		joinMethod = indexJoinMethod
	case *PhysicalIndexHashJoin:
		innerIdx = ij.GetInnerChildIdx()
		joinMethod = indexHashJoinMethod
	case *PhysicalIndexMergeJoin:
		innerIdx = ij.GetInnerChildIdx()
		joinMethod = indexMergeJoinMethod
	default:
		return 0, 0, false
	}
	ok = true
	innerSide = joinLeft
	if innerIdx == 1 {
		innerSide = joinRight
	}
	return
}

// tryToEnumerateIndexJoin returns all available index join plans, which will require inner indexJoinProp downside
// compared with original tryToGetIndexJoin.
func tryToEnumerateIndexJoin(super base.LogicalPlan, prop *property.PhysicalProperty) []base.PhysicalPlan {
	_, p := getGEAndLogicalJoin(super)
	// supportLeftOuter and supportRightOuter indicates whether this type of join
	// supports the left side or right side to be the outer side.
	var supportLeftOuter, supportRightOuter bool
	switch p.JoinType {
	case logicalop.SemiJoin, logicalop.AntiSemiJoin, logicalop.LeftOuterSemiJoin, logicalop.AntiLeftOuterSemiJoin, logicalop.LeftOuterJoin:
		supportLeftOuter = true
	case logicalop.RightOuterJoin:
		supportRightOuter = true
	case logicalop.InnerJoin:
		supportLeftOuter, supportRightOuter = true, true
	}
	// according join type to enumerate index join with inner children's indexJoinProp.
	candidates := make([]base.PhysicalPlan, 0, 2)
	if supportLeftOuter {
		candidates = append(candidates, enumerateIndexJoinByOuterIdx(super, prop, 0)...)
	}
	if supportRightOuter {
		candidates = append(candidates, enumerateIndexJoinByOuterIdx(super, prop, 1)...)
	}
	// Pre-Handle hints and variables about index join, which try to detect the contradictory hint and variables
	// The priority is: force hints like TIDB_INLJ > filter hints like NO_INDEX_JOIN > variables and rec warns.
	stmtCtx := p.SCtx().GetSessionVars().StmtCtx
	if p.PreferAny(h.PreferLeftAsINLJInner, h.PreferRightAsINLJInner) && p.PreferAny(h.PreferNoIndexJoin) {
		stmtCtx.SetHintWarning("Some INL_JOIN and NO_INDEX_JOIN hints conflict, NO_INDEX_JOIN may be ignored")
	}
	if p.PreferAny(h.PreferLeftAsINLHJInner, h.PreferRightAsINLHJInner) && p.PreferAny(h.PreferNoIndexHashJoin) {
		stmtCtx.SetHintWarning("Some INL_HASH_JOIN and NO_INDEX_HASH_JOIN hints conflict, NO_INDEX_HASH_JOIN may be ignored")
	}
	if p.PreferAny(h.PreferLeftAsINLMJInner, h.PreferRightAsINLMJInner) && p.PreferAny(h.PreferNoIndexMergeJoin) {
		stmtCtx.SetHintWarning("Some INL_MERGE_JOIN and NO_INDEX_MERGE_JOIN hints conflict, NO_INDEX_MERGE_JOIN may be ignored")
	}
	// previously we will think about force index join hints here, but we have to wait the inner plans to be a valid
	// physical one/ones. Because indexJoinProp may not be admitted by its inner patterns, so we innovatively move all
	// hint related handling to the findBestTask function when we see the entire inner physical-ized plan tree. See xxx
	// for details.
	//
	// handleFilterIndexJoinHints is trying to avoid generating index join or index hash join when no-index-join related
	// hint is specified in the query. So we can do it in physic enumeration phase here.
	return handleFilterIndexJoinHints(p, candidates)
}

// tryToGetIndexJoin returns all available index join plans, and the second returned value indicates whether this plan is enforced by hints.
func tryToGetIndexJoin(p *logicalop.LogicalJoin, prop *property.PhysicalProperty) (indexJoins []base.PhysicalPlan, canForced bool) {
	// supportLeftOuter and supportRightOuter indicates whether this type of join
	// supports the left side or right side to be the outer side.
	var supportLeftOuter, supportRightOuter bool
	switch p.JoinType {
	case logicalop.SemiJoin, logicalop.AntiSemiJoin, logicalop.LeftOuterSemiJoin, logicalop.AntiLeftOuterSemiJoin, logicalop.LeftOuterJoin:
		supportLeftOuter = true
	case logicalop.RightOuterJoin:
		supportRightOuter = true
	case logicalop.InnerJoin:
		supportLeftOuter, supportRightOuter = true, true
	}
	candidates := make([]base.PhysicalPlan, 0, 2)
	if supportLeftOuter {
		candidates = append(candidates, getIndexJoinByOuterIdx(p, prop, 0)...)
	}
	if supportRightOuter {
		candidates = append(candidates, getIndexJoinByOuterIdx(p, prop, 1)...)
	}

	// Handle hints and variables about index join.
	// The priority is: force hints like TIDB_INLJ > filter hints like NO_INDEX_JOIN > variables.
	// Handle hints conflict first.
	stmtCtx := p.SCtx().GetSessionVars().StmtCtx
	if p.PreferAny(h.PreferLeftAsINLJInner, h.PreferRightAsINLJInner) && p.PreferAny(h.PreferNoIndexJoin) {
		stmtCtx.SetHintWarning("Some INL_JOIN and NO_INDEX_JOIN hints conflict, NO_INDEX_JOIN may be ignored")
	}
	if p.PreferAny(h.PreferLeftAsINLHJInner, h.PreferRightAsINLHJInner) && p.PreferAny(h.PreferNoIndexHashJoin) {
		stmtCtx.SetHintWarning("Some INL_HASH_JOIN and NO_INDEX_HASH_JOIN hints conflict, NO_INDEX_HASH_JOIN may be ignored")
	}
	if p.PreferAny(h.PreferLeftAsINLMJInner, h.PreferRightAsINLMJInner) && p.PreferAny(h.PreferNoIndexMergeJoin) {
		stmtCtx.SetHintWarning("Some INL_MERGE_JOIN and NO_INDEX_MERGE_JOIN hints conflict, NO_INDEX_MERGE_JOIN may be ignored")
	}

	candidates, canForced = handleForceIndexJoinHints(p, prop, candidates)
	if canForced {
		return candidates, canForced
	}
	candidates = handleFilterIndexJoinHints(p, candidates)
	// todo: if any variables banned it, why bother to generate it first?
	return filterIndexJoinBySessionVars(p.SCtx(), candidates), false
}

func enumerationContainIndexJoin(candidates []base.PhysicalPlan) bool {
	return slices.ContainsFunc(candidates, func(candidate base.PhysicalPlan) bool {
		_, _, ok := getIndexJoinSideAndMethod(candidate)
		return ok
	})
}

// handleFilterIndexJoinHints is trying to avoid generating index join or index hash join when no-index-join related
// hint is specified in the query. So we can do it in physic enumeration phase.
func handleFilterIndexJoinHints(p *logicalop.LogicalJoin, candidates []base.PhysicalPlan) []base.PhysicalPlan {
	if !p.PreferAny(h.PreferNoIndexJoin, h.PreferNoIndexHashJoin, h.PreferNoIndexMergeJoin) {
		return candidates // no filter index join hints
	}
	filtered := make([]base.PhysicalPlan, 0, len(candidates))
	for _, candidate := range candidates {
		_, joinMethod, ok := getIndexJoinSideAndMethod(candidate)
		if !ok {
			continue
		}
		if (p.PreferAny(h.PreferNoIndexJoin) && joinMethod == indexJoinMethod) ||
			(p.PreferAny(h.PreferNoIndexHashJoin) && joinMethod == indexHashJoinMethod) ||
			(p.PreferAny(h.PreferNoIndexMergeJoin) && joinMethod == indexMergeJoinMethod) {
			continue
		}
		filtered = append(filtered, candidate)
	}
	return filtered
}

func recordWarnings(lp base.LogicalPlan, prop *property.PhysicalProperty, inEnforce bool) error {
	switch x := lp.(type) {
	case *logicalop.LogicalAggregation:
		return recordAggregationHintWarnings(x)
	case *logicalop.LogicalTopN, *logicalop.LogicalLimit:
		return recordLimitToCopWarnings(lp)
	case *logicalop.LogicalJoin:
		return recordIndexJoinHintWarnings(x, prop, inEnforce)
	default:
		// no warnings to record
		return nil
	}
}

func recordAggregationHintWarnings(la *logicalop.LogicalAggregation) error {
	if la.PreferAggToCop {
		return plannererrors.ErrInternal.FastGen("Optimizer Hint AGG_TO_COP is inapplicable")
	}
	return nil
}

func recordLimitToCopWarnings(lp base.LogicalPlan) error {
	var preferPushDown *bool
	switch lp := lp.(type) {
	case *logicalop.LogicalTopN:
		preferPushDown = &lp.PreferLimitToCop
	case *logicalop.LogicalLimit:
		preferPushDown = &lp.PreferLimitToCop
	default:
		return nil
	}
	if *preferPushDown {
		return plannererrors.ErrInternal.FastGen("Optimizer Hint LIMIT_TO_COP is inapplicable")
	}
	return nil
}

// recordIndexJoinHintWarnings records the warnings msg if no valid preferred physic are picked.
// todo: extend recordIndexJoinHintWarnings to support all kind of operator's warnings handling.
func recordIndexJoinHintWarnings(p *logicalop.LogicalJoin, prop *property.PhysicalProperty, inEnforce bool) error {
	// handle mpp join hints first.
	if (p.PreferJoinType&h.PreferShuffleJoin) > 0 || (p.PreferJoinType&h.PreferBCJoin) > 0 {
		var errMsg string
		if (p.PreferJoinType & h.PreferShuffleJoin) > 0 {
			errMsg = "The join can not push down to the MPP side, the shuffle_join() hint is invalid"
		} else {
			errMsg = "The join can not push down to the MPP side, the broadcast_join() hint is invalid"
		}
		return plannererrors.ErrInternal.FastGen(errMsg)
	}
	// handle index join hints.
	if !p.PreferAny(h.PreferRightAsINLJInner, h.PreferRightAsINLHJInner, h.PreferRightAsINLMJInner,
		h.PreferLeftAsINLJInner, h.PreferLeftAsINLHJInner, h.PreferLeftAsINLMJInner) {
		return nil // no force index join hints
	}
	// Cannot find any valid index join plan with these force hints.
	// Print warning message if any hints cannot work.
	// If the required property is not empty, we will enforce it and try the hint again.
	// So we only need to generate warning message when the property is empty.
	//
	// but for warnings handle inside findBestTask here, even the not-empty prop
	// will be reset to get the planNeedEnforce plans, but the prop passed down here will
	// still be the same, so here we change the admission to both.
	if prop.IsSortItemEmpty() || inEnforce {
		var indexJoinTables, indexHashJoinTables, indexMergeJoinTables []h.HintedTable
		if p.HintInfo != nil {
			t := p.HintInfo.IndexJoin
			indexJoinTables, indexHashJoinTables, indexMergeJoinTables = t.INLJTables, t.INLHJTables, t.INLMJTables
		}
		var errMsg string
		switch {
		case p.PreferAny(h.PreferLeftAsINLJInner, h.PreferRightAsINLJInner): // prefer index join
			errMsg = fmt.Sprintf("Optimizer Hint %s or %s is inapplicable", h.Restore2JoinHint(h.HintINLJ, indexJoinTables), h.Restore2JoinHint(h.TiDBIndexNestedLoopJoin, indexJoinTables))
		case p.PreferAny(h.PreferLeftAsINLHJInner, h.PreferRightAsINLHJInner): // prefer index hash join
			errMsg = fmt.Sprintf("Optimizer Hint %s is inapplicable", h.Restore2JoinHint(h.HintINLHJ, indexHashJoinTables))
		case p.PreferAny(h.PreferLeftAsINLMJInner, h.PreferRightAsINLMJInner): // prefer index merge join
			errMsg = fmt.Sprintf("Optimizer Hint %s is inapplicable", h.Restore2JoinHint(h.HintINLMJ, indexMergeJoinTables))
		default:
			// only record warnings for index join hint not working now.
			return nil
		}
		// Append inapplicable reason.
		if len(p.EqualConditions) == 0 {
			errMsg += " without column equal ON condition"
		}
		// Generate warning message to client.
		return plannererrors.ErrInternal.FastGen(errMsg)
	}
	return nil
}

func applyLogicalHintVarEigen(lp base.LogicalPlan, state *enumerateState, pp base.PhysicalPlan, childTasks []base.Task) (preferred bool) {
	return applyLogicalJoinHint(lp, pp) ||
		applyLogicalTopNAndLimitHint(lp, state, pp, childTasks) ||
		applyLogicalAggregationHint(lp, pp, childTasks)
}

// Get the most preferred and efficient one by hint and low-cost priority.
// since hint applicable plan may greater than 1, like inl_join can suit for:
// index_join, index_hash_join, index_merge_join, we should chase the most efficient
// one among them.
// applyLogicalJoinHint is used to handle logic hint/prefer/variable, which is not a strong guide for optimization phase.
// It is changed from handleForceIndexJoinHints to handle the preferred join hint among several valid physical plan choices.
// It will return true if the hint can be applied when saw a real physic plan successfully built and returned up from child.
// we cache the most preferred one among this valid and preferred physic plans. If there is no preferred physic applicable
// for the logic hint, we will return false and the optimizer will continue to return the normal low-cost one.
func applyLogicalJoinHint(lp base.LogicalPlan, physicPlan base.PhysicalPlan) (preferred bool) {
	return preferMergeJoin(lp, physicPlan) || preferIndexJoinFamily(lp, physicPlan) ||
		preferHashJoin(lp, physicPlan)
}

func applyLogicalAggregationHint(lp base.LogicalPlan, physicPlan base.PhysicalPlan, childTasks []base.Task) (preferred bool) {
	la, ok := lp.(*logicalop.LogicalAggregation)
	if !ok {
		return false
	}
	if physicPlan == nil {
		return false
	}
	if la.HasDistinct() {
		// TODO: remove after the cost estimation of distinct pushdown is implemented.
		if la.SCtx().GetSessionVars().AllowDistinctAggPushDown {
			// when AllowDistinctAggPushDown is true, we will not consider root task type as before.
			if _, ok := childTasks[0].(*CopTask); ok {
				return true
			}
		} else {
			switch childTasks[0].(type) {
			case *RootTask:
				// If the distinct agg can't be allowed to push down, we will consider root task type.
				// which is try to get the same behavior as before like types := {RootTask} only.
				return true
			case *MppTask:
				// If the distinct agg can't be allowed to push down, we will consider mpp task type too --- RootTask vs MPPTask
				// which is try to get the same behavior as before like types := {RootTask} and appended {MPPTask}.
				return true
			default:
				return false
			}
		}
	} else if la.PreferAggToCop {
		// If the aggregation is preferred to be pushed down to coprocessor, we will prefer it.
		if _, ok := childTasks[0].(*CopTask); ok {
			return true
		}
	}
	return false
}

func applyLogicalTopNAndLimitHint(lp base.LogicalPlan, state *enumerateState, pp base.PhysicalPlan, childTasks []base.Task) (preferred bool) {
	hintPrefer, meetThreshold := pushLimitOrTopNForcibly(lp)
	if hintPrefer {
		// if there is a user hint control, try to get the copTask as the prior.
		// here we don't assert task itself, because when topN attach 2 cop task, it will become root type automatically.
		if _, ok := childTasks[0].(*CopTask); ok {
			return true
		}
		return false
	}
	if meetThreshold {
		// previously, we set meetThreshold for pruning root task type but mpp task type. so:
		// 1: when one copTask exists, we will ignore root task type.
		// 2: when one copTask exists, another copTask should be cost compared with.
		// 3: mppTask is always in the cbo comparing.
		// 4: when none copTask exists, we will consider rootTask vs mppTask.
		// the following check priority logic is compatible with former pushLimitOrTopNForcibly prop pruning logic.
		_, isTopN := pp.(*physicalop.PhysicalTopN)
		if isTopN {
			if state.topNCopExist {
				if _, ok := childTasks[0].(*RootTask); ok {
					return false
				}
				// peer cop task should compare the cost with each other.
				if _, ok := childTasks[0].(*CopTask); ok {
					return true
				}
			} else {
				if _, ok := childTasks[0].(*CopTask); ok {
					state.topNCopExist = true
					return true
				}
				// when we encounter rootTask type while still see no topNCopExist.
				// that means there is no copTask valid before, we will consider rootTask here.
				if _, ok := childTasks[0].(*RootTask); ok {
					return true
				}
			}
			if _, ok := childTasks[0].(*MppTask); ok {
				return true
			}
			// shouldn't be here
			return false
		}
		// limit case:
		if state.limitCopExist {
			if _, ok := childTasks[0].(*RootTask); ok {
				return false
			}
			// peer cop task should compare the cost with each other.
			if _, ok := childTasks[0].(*CopTask); ok {
				return true
			}
		} else {
			if _, ok := childTasks[0].(*CopTask); ok {
				state.limitCopExist = true
				return true
			}
			// when we encounter rootTask type while still see no limitCopExist.
			// that means there is no copTask valid before, we will consider rootTask here.
			if _, ok := childTasks[0].(*RootTask); ok {
				return true
			}
		}
		if _, ok := childTasks[0].(*MppTask); ok {
			return true
		}
	}
	return false
}

// hash join has two types:
// one is hash join type: normal hash join, shuffle join, broadcast join
// another is the build side hint type: prefer left as build side, prefer right as build side
// the first one is used to control the join type, the second one is used to control the build side of hash join.
// the priority is:
// once the join type is set, we should respect them first, not this type are all ignored.
// after we see all the joins under this type, then we only consider the build side hints satisfied or not.
//
// for the priority among the hash join types, we will respect the join fine-grained hints first, then the normal hash join type,
// that is, the priority is: shuffle join / broadcast join > normal hash join.
func preferHashJoin(lp base.LogicalPlan, physicPlan base.PhysicalPlan) (preferred bool) {
	p, ok := lp.(*logicalop.LogicalJoin)
	if !ok {
		return false
	}
	if physicPlan == nil {
		return false
	}
	forceLeftToBuild := ((p.PreferJoinType & h.PreferLeftAsHJBuild) > 0) || ((p.PreferJoinType & h.PreferRightAsHJProbe) > 0)
	forceRightToBuild := ((p.PreferJoinType & h.PreferRightAsHJBuild) > 0) || ((p.PreferJoinType & h.PreferLeftAsHJProbe) > 0)
	if forceLeftToBuild && forceRightToBuild {
		// for build hint conflict, restore all of them
		forceLeftToBuild = false
		forceRightToBuild = false
	}
	physicalHashJoin, ok := physicPlan.(*PhysicalHashJoin)
	if !ok {
		return false
	}
	// If the hint is set, we should prefer MPP shuffle join.
	preferShuffle := (p.PreferJoinType & h.PreferShuffleJoin) > 0
	preferBCJ := (p.PreferJoinType & h.PreferBCJoin) > 0
	if preferShuffle {
		if physicalHashJoin.storeTp == kv.TiFlash && physicalHashJoin.mppShuffleJoin {
			// first: respect the shuffle join hint.
			// BCJ build side hint are handled in the enumeration phase.
			return true
		}
		return false
	}
	if preferBCJ {
		if physicalHashJoin.storeTp == kv.TiFlash && !physicalHashJoin.mppShuffleJoin {
			// first: respect the broadcast join hint.
			// BCJ build side hint are handled in the enumeration phase.
			return true
		}
		return false
	}
	// Respect the join type and join side hints.
	if p.PreferJoinType&h.PreferHashJoin > 0 {
		// first: normal hash join hint are set.
		if forceLeftToBuild || forceRightToBuild {
			// second: respect the join side if join side hints are set.
			return (forceRightToBuild && physicalHashJoin.InnerChildIdx == 1) ||
				(forceLeftToBuild && physicalHashJoin.InnerChildIdx == 0)
		}
		// second: no join side hints are set, respect the join type is enough.
		return true
	}
	// no hash join type hint is set, we only need to respect the hash join side hints.
	return (forceRightToBuild && physicalHashJoin.InnerChildIdx == 1) ||
		(forceLeftToBuild && physicalHashJoin.InnerChildIdx == 0)
}

func preferMergeJoin(lp base.LogicalPlan, physicPlan base.PhysicalPlan) (preferred bool) {
	p, ok := lp.(*logicalop.LogicalJoin)
	if !ok {
		return false
	}
	if physicPlan == nil {
		return false
	}
	_, ok = physicPlan.(*physicalop.PhysicalMergeJoin)
	return ok && p.PreferJoinType&h.PreferMergeJoin > 0
}

func preferIndexJoinFamily(lp base.LogicalPlan, physicPlan base.PhysicalPlan) (preferred bool) {
	p, ok := lp.(*logicalop.LogicalJoin)
	if !ok {
		return false
	}
	if physicPlan == nil {
		return false
	}
	if !p.PreferAny(h.PreferRightAsINLJInner, h.PreferRightAsINLHJInner, h.PreferRightAsINLMJInner,
		h.PreferLeftAsINLJInner, h.PreferLeftAsINLHJInner, h.PreferLeftAsINLMJInner) {
		return false // no force index join hints
	}
	innerSide, joinMethod, ok := getIndexJoinSideAndMethod(physicPlan)
	if !ok {
		return false
	}
	if (p.PreferAny(h.PreferLeftAsINLJInner) && innerSide == joinLeft && joinMethod == indexJoinMethod) ||
		(p.PreferAny(h.PreferRightAsINLJInner) && innerSide == joinRight && joinMethod == indexJoinMethod) ||
		(p.PreferAny(h.PreferLeftAsINLHJInner) && innerSide == joinLeft && joinMethod == indexHashJoinMethod) ||
		(p.PreferAny(h.PreferRightAsINLHJInner) && innerSide == joinRight && joinMethod == indexHashJoinMethod) ||
		(p.PreferAny(h.PreferLeftAsINLMJInner) && innerSide == joinLeft && joinMethod == indexMergeJoinMethod) ||
		(p.PreferAny(h.PreferRightAsINLMJInner) && innerSide == joinRight && joinMethod == indexMergeJoinMethod) {
		// valid physic for the hint
		return true
	}
	return false
}

// handleForceIndexJoinHints handles the force index join hints and returns all plans that can satisfy the hints.
func handleForceIndexJoinHints(p *logicalop.LogicalJoin, prop *property.PhysicalProperty, candidates []base.PhysicalPlan) (indexJoins []base.PhysicalPlan, canForced bool) {
	if !p.PreferAny(h.PreferRightAsINLJInner, h.PreferRightAsINLHJInner, h.PreferRightAsINLMJInner,
		h.PreferLeftAsINLJInner, h.PreferLeftAsINLHJInner, h.PreferLeftAsINLMJInner) {
		return candidates, false // no force index join hints
	}
	forced := make([]base.PhysicalPlan, 0, len(candidates))
	for _, candidate := range candidates {
		innerSide, joinMethod, ok := getIndexJoinSideAndMethod(candidate)
		if !ok {
			continue
		}
		if (p.PreferAny(h.PreferLeftAsINLJInner) && innerSide == joinLeft && joinMethod == indexJoinMethod) ||
			(p.PreferAny(h.PreferRightAsINLJInner) && innerSide == joinRight && joinMethod == indexJoinMethod) ||
			(p.PreferAny(h.PreferLeftAsINLHJInner) && innerSide == joinLeft && joinMethod == indexHashJoinMethod) ||
			(p.PreferAny(h.PreferRightAsINLHJInner) && innerSide == joinRight && joinMethod == indexHashJoinMethod) ||
			(p.PreferAny(h.PreferLeftAsINLMJInner) && innerSide == joinLeft && joinMethod == indexMergeJoinMethod) ||
			(p.PreferAny(h.PreferRightAsINLMJInner) && innerSide == joinRight && joinMethod == indexMergeJoinMethod) {
			forced = append(forced, candidate)
		}
	}

	if len(forced) > 0 {
		return forced, true
	}
	// Cannot find any valid index join plan with these force hints.
	// Print warning message if any hints cannot work.
	// If the required property is not empty, we will enforce it and try the hint again.
	// So we only need to generate warning message when the property is empty.
	if prop.IsSortItemEmpty() {
		var indexJoinTables, indexHashJoinTables, indexMergeJoinTables []h.HintedTable
		if p.HintInfo != nil {
			t := p.HintInfo.IndexJoin
			indexJoinTables, indexHashJoinTables, indexMergeJoinTables = t.INLJTables, t.INLHJTables, t.INLMJTables
		}
		var errMsg string
		switch {
		case p.PreferAny(h.PreferLeftAsINLJInner, h.PreferRightAsINLJInner): // prefer index join
			errMsg = fmt.Sprintf("Optimizer Hint %s or %s is inapplicable", h.Restore2JoinHint(h.HintINLJ, indexJoinTables), h.Restore2JoinHint(h.TiDBIndexNestedLoopJoin, indexJoinTables))
		case p.PreferAny(h.PreferLeftAsINLHJInner, h.PreferRightAsINLHJInner): // prefer index hash join
			errMsg = fmt.Sprintf("Optimizer Hint %s is inapplicable", h.Restore2JoinHint(h.HintINLHJ, indexHashJoinTables))
		case p.PreferAny(h.PreferLeftAsINLMJInner, h.PreferRightAsINLMJInner): // prefer index merge join
			errMsg = fmt.Sprintf("Optimizer Hint %s is inapplicable", h.Restore2JoinHint(h.HintINLMJ, indexMergeJoinTables))
		}
		// Append inapplicable reason.
		if len(p.EqualConditions) == 0 {
			errMsg += " without column equal ON condition"
		}
		// Generate warning message to client.
		p.SCtx().GetSessionVars().StmtCtx.SetHintWarning(errMsg)
	}
	return candidates, false
}

func checkChildFitBC(pctx base.PlanContext, stats *property.StatsInfo, schema *expression.Schema) bool {
	if stats.HistColl == nil {
		return pctx.GetSessionVars().BroadcastJoinThresholdCount == -1 || stats.Count() < pctx.GetSessionVars().BroadcastJoinThresholdCount
	}
	avg := cardinality.GetAvgRowSize(pctx, stats.HistColl, schema.Columns, false, false)
	sz := avg * float64(stats.Count())
	return pctx.GetSessionVars().BroadcastJoinThresholdSize == -1 || sz < float64(pctx.GetSessionVars().BroadcastJoinThresholdSize)
}

func calcBroadcastExchangeSize(p base.Plan, mppStoreCnt int) (row float64, size float64, hasSize bool) {
	s := p.StatsInfo()
	row = float64(s.Count()) * float64(mppStoreCnt-1)
	if s.HistColl == nil {
		return row, 0, false
	}
	avg := cardinality.GetAvgRowSize(p.SCtx(), s.HistColl, p.Schema().Columns, false, false)
	size = avg * row
	return row, size, true
}

func calcBroadcastExchangeSizeByChild(p1 base.Plan, p2 base.Plan, mppStoreCnt int) (row float64, size float64, hasSize bool) {
	row1, size1, hasSize1 := calcBroadcastExchangeSize(p1, mppStoreCnt)
	row2, size2, hasSize2 := calcBroadcastExchangeSize(p2, mppStoreCnt)

	// broadcast exchange size:
	//   Build: (mppStoreCnt - 1) * sizeof(BuildTable)
	//   Probe: 0
	// choose the child plan with the maximum approximate value as Probe

	if hasSize1 && hasSize2 {
		return math.Min(row1, row2), math.Min(size1, size2), true
	}

	return math.Min(row1, row2), 0, false
}

func calcHashExchangeSize(p base.Plan, mppStoreCnt int) (row float64, sz float64, hasSize bool) {
	s := p.StatsInfo()
	row = float64(s.Count()) * float64(mppStoreCnt-1) / float64(mppStoreCnt)
	if s.HistColl == nil {
		return row, 0, false
	}
	avg := cardinality.GetAvgRowSize(p.SCtx(), s.HistColl, p.Schema().Columns, false, false)
	sz = avg * row
	return row, sz, true
}

func calcHashExchangeSizeByChild(p1 base.Plan, p2 base.Plan, mppStoreCnt int) (row float64, size float64, hasSize bool) {
	row1, size1, hasSize1 := calcHashExchangeSize(p1, mppStoreCnt)
	row2, size2, hasSize2 := calcHashExchangeSize(p2, mppStoreCnt)

	// hash exchange size:
	//   Build: sizeof(BuildTable) * (mppStoreCnt - 1) / mppStoreCnt
	//   Probe: sizeof(ProbeTable) * (mppStoreCnt - 1) / mppStoreCnt

	if hasSize1 && hasSize2 {
		return row1 + row2, size1 + size2, true
	}
	return row1 + row2, 0, false
}

// The size of `Build` hash table when using broadcast join is about `X`.
// The size of `Build` hash table when using shuffle join is about `X / (mppStoreCnt)`.
// It will cost more time to construct `Build` hash table and search `Probe` while using broadcast join.
// Set a scale factor (`mppStoreCnt^*`) when estimating broadcast join in `isJoinFitMPPBCJ` and `isJoinChildFitMPPBCJ` (based on TPCH benchmark, it has been verified in Q9).

func isJoinFitMPPBCJ(p *logicalop.LogicalJoin, mppStoreCnt int) bool {
	rowBC, szBC, hasSizeBC := calcBroadcastExchangeSizeByChild(p.Children()[0], p.Children()[1], mppStoreCnt)
	rowHash, szHash, hasSizeHash := calcHashExchangeSizeByChild(p.Children()[0], p.Children()[1], mppStoreCnt)
	if hasSizeBC && hasSizeHash {
		return szBC*float64(mppStoreCnt) <= szHash
	}
	return rowBC*float64(mppStoreCnt) <= rowHash
}

func isJoinChildFitMPPBCJ(p *logicalop.LogicalJoin, childIndexToBC int, mppStoreCnt int) bool {
	rowBC, szBC, hasSizeBC := calcBroadcastExchangeSize(p.Children()[childIndexToBC], mppStoreCnt)
	rowHash, szHash, hasSizeHash := calcHashExchangeSizeByChild(p.Children()[0], p.Children()[1], mppStoreCnt)

	if hasSizeBC && hasSizeHash {
		return szBC*float64(mppStoreCnt) <= szHash
	}
	return rowBC*float64(mppStoreCnt) <= rowHash
}

func getChildStatsAndSchema(ge *memo.GroupExpression, p base.LogicalPlan) (stats0 *property.StatsInfo, schema0 *expression.Schema) {
	if ge != nil {
		stats0, schema0 = ge.Inputs[0].GetLogicalProperty().Stats, ge.Inputs[0].GetLogicalProperty().Schema
	} else {
		stats0, schema0 = p.Children()[0].StatsInfo(), p.Children()[0].Schema()
	}
	return
}

func getJoinChildStatsAndSchema(ge *memo.GroupExpression, p base.LogicalPlan) (stats0, stats1 *property.StatsInfo, schema0, schema1 *expression.Schema) {
	if ge != nil {
		stats0, schema0 = ge.Inputs[0].GetLogicalProperty().Stats, ge.Inputs[0].GetLogicalProperty().Schema
		stats1, schema1 = ge.Inputs[1].GetLogicalProperty().Stats, ge.Inputs[1].GetLogicalProperty().Schema
	} else {
		stats1, schema1 = p.Children()[1].StatsInfo(), p.Children()[1].Schema()
		stats0, schema0 = p.Children()[0].StatsInfo(), p.Children()[0].Schema()
	}
	return
}

// If we can use mpp broadcast join, that's our first choice.
func preferMppBCJ(super base.LogicalPlan) bool {
	ge, p := getGEAndLogicalJoin(super)
	if len(p.EqualConditions) == 0 && p.SCtx().GetSessionVars().AllowCartesianBCJ == 2 {
		return true
	}

	onlyCheckChild1 := p.JoinType == logicalop.LeftOuterJoin || p.JoinType == logicalop.SemiJoin || p.JoinType == logicalop.AntiSemiJoin
	onlyCheckChild0 := p.JoinType == logicalop.RightOuterJoin

	if p.SCtx().GetSessionVars().PreferBCJByExchangeDataSize {
		mppStoreCnt, err := p.SCtx().GetMPPClient().GetMPPStoreCount()

		// No need to exchange data if there is only ONE mpp store. But the behavior of optimizer is unexpected if use broadcast way forcibly, such as tpch q4.
		// TODO: always use broadcast way to exchange data if there is only ONE mpp store.

		if err == nil && mppStoreCnt > 0 {
			if !(onlyCheckChild1 || onlyCheckChild0) {
				return isJoinFitMPPBCJ(p, mppStoreCnt)
			}
			if mppStoreCnt > 1 {
				if onlyCheckChild1 {
					return isJoinChildFitMPPBCJ(p, 1, mppStoreCnt)
				} else if onlyCheckChild0 {
					return isJoinChildFitMPPBCJ(p, 0, mppStoreCnt)
				}
			}
			// If mppStoreCnt is ONE and only need to check one child plan, rollback to original way.
			// Otherwise, the plan of tpch q4 may be unexpected.
		}
	}
	stats0, stats1, schema0, schema1 := getJoinChildStatsAndSchema(ge, p)
	pctx := p.SCtx()
	if onlyCheckChild1 {
		return checkChildFitBC(pctx, stats1, schema1)
	} else if onlyCheckChild0 {
		return checkChildFitBC(pctx, stats0, schema0)
	}
	return checkChildFitBC(pctx, stats0, schema0) || checkChildFitBC(pctx, stats1, schema1)
}

func canExprsInJoinPushdown(p *logicalop.LogicalJoin, storeType kv.StoreType) bool {
	equalExprs := make([]expression.Expression, 0, len(p.EqualConditions))
	for _, eqCondition := range p.EqualConditions {
		if eqCondition.FuncName.L == ast.NullEQ {
			return false
		}
		equalExprs = append(equalExprs, eqCondition)
	}
	pushDownCtx := util.GetPushDownCtx(p.SCtx())
	if !expression.CanExprsPushDown(pushDownCtx, equalExprs, storeType) {
		return false
	}
	if !expression.CanExprsPushDown(pushDownCtx, p.LeftConditions, storeType) {
		return false
	}
	if !expression.CanExprsPushDown(pushDownCtx, p.RightConditions, storeType) {
		return false
	}
	if !expression.CanExprsPushDown(pushDownCtx, p.OtherConditions, storeType) {
		return false
	}
	return true
}

func tryToGetMppHashJoin(super base.LogicalPlan, prop *property.PhysicalProperty, useBCJ bool) []base.PhysicalPlan {
	ge, p := getGEAndLogicalJoin(super)
	if !prop.IsSortItemEmpty() {
		return nil
	}
	if prop.TaskTp != property.RootTaskType && prop.TaskTp != property.MppTaskType {
		return nil
	}

	if !expression.IsPushDownEnabled(p.JoinType.String(), kv.TiFlash) {
		p.SCtx().GetSessionVars().RaiseWarningWhenMPPEnforced("MPP mode may be blocked because join type `" + p.JoinType.String() + "` is blocked by blacklist, check `table mysql.expr_pushdown_blacklist;` for more information.")
		return nil
	}

	if p.JoinType != logicalop.InnerJoin && p.JoinType != logicalop.LeftOuterJoin && p.JoinType != logicalop.RightOuterJoin && p.JoinType != logicalop.SemiJoin && p.JoinType != logicalop.AntiSemiJoin && p.JoinType != logicalop.LeftOuterSemiJoin && p.JoinType != logicalop.AntiLeftOuterSemiJoin {
		p.SCtx().GetSessionVars().RaiseWarningWhenMPPEnforced("MPP mode may be blocked because join type `" + p.JoinType.String() + "` is not supported now.")
		return nil
	}

	if len(p.EqualConditions) == 0 {
		if !useBCJ {
			p.SCtx().GetSessionVars().RaiseWarningWhenMPPEnforced("MPP mode may be blocked because `Cartesian Product` is only supported by broadcast join, check value and documents of variables `tidb_broadcast_join_threshold_size` and `tidb_broadcast_join_threshold_count`.")
			return nil
		}
		if p.SCtx().GetSessionVars().AllowCartesianBCJ == 0 {
			p.SCtx().GetSessionVars().RaiseWarningWhenMPPEnforced("MPP mode may be blocked because `Cartesian Product` is only supported by broadcast join, check value and documents of variable `tidb_opt_broadcast_cartesian_join`.")
			return nil
		}
	}
	if len(p.LeftConditions) != 0 && p.JoinType != logicalop.LeftOuterJoin {
		p.SCtx().GetSessionVars().RaiseWarningWhenMPPEnforced("MPP mode may be blocked because there is a join that is not `left join` but has left conditions, which is not supported by mpp now, see github.com/pingcap/tidb/issues/26090 for more information.")
		return nil
	}
	if len(p.RightConditions) != 0 && p.JoinType != logicalop.RightOuterJoin {
		p.SCtx().GetSessionVars().RaiseWarningWhenMPPEnforced("MPP mode may be blocked because there is a join that is not `right join` but has right conditions, which is not supported by mpp now.")
		return nil
	}

	if prop.MPPPartitionTp == property.BroadcastType {
		return nil
	}
	if !canExprsInJoinPushdown(p, kv.TiFlash) {
		return nil
	}
	lkeys, rkeys, _, _ := p.GetJoinKeys()
	lNAkeys, rNAKeys := p.GetNAJoinKeys()
	// check match property
	baseJoin := physicalop.BasePhysicalJoin{
		JoinType:        p.JoinType,
		LeftConditions:  p.LeftConditions,
		RightConditions: p.RightConditions,
		OtherConditions: p.OtherConditions,
		DefaultValues:   p.DefaultValues,
		LeftJoinKeys:    lkeys,
		RightJoinKeys:   rkeys,
		LeftNAJoinKeys:  lNAkeys,
		RightNAJoinKeys: rNAKeys,
	}
	// It indicates which side is the build side.
	forceLeftToBuild := ((p.PreferJoinType & h.PreferLeftAsHJBuild) > 0) || ((p.PreferJoinType & h.PreferRightAsHJProbe) > 0)
	forceRightToBuild := ((p.PreferJoinType & h.PreferRightAsHJBuild) > 0) || ((p.PreferJoinType & h.PreferLeftAsHJProbe) > 0)
	if forceLeftToBuild && forceRightToBuild {
		p.SCtx().GetSessionVars().StmtCtx.SetHintWarning(
			"Some HASH_JOIN_BUILD and HASH_JOIN_PROBE hints are conflicts, please check the hints")
		forceLeftToBuild = false
		forceRightToBuild = false
	}
	preferredBuildIndex := 0
	fixedBuildSide := false // Used to indicate whether the build side for the MPP join is fixed or not.
	stats0, stats1, _, _ := getJoinChildStatsAndSchema(ge, p)
	if p.JoinType == logicalop.InnerJoin {
		if stats0.Count() > stats1.Count() {
			preferredBuildIndex = 1
		}
	} else if p.JoinType.IsSemiJoin() {
		if !useBCJ && !p.IsNAAJ() && len(p.EqualConditions) > 0 && (p.JoinType == logicalop.SemiJoin || p.JoinType == logicalop.AntiSemiJoin) {
			// TiFlash only supports Non-null_aware non-cross semi/anti_semi join to use both sides as build side
			preferredBuildIndex = 1
			// MPPOuterJoinFixedBuildSide default value is false
			// use MPPOuterJoinFixedBuildSide here as a way to disable using left table as build side
			if !p.SCtx().GetSessionVars().MPPOuterJoinFixedBuildSide && stats1.Count() > stats0.Count() {
				preferredBuildIndex = 0
			}
		} else {
			preferredBuildIndex = 1
			fixedBuildSide = true
		}
	}
	if p.JoinType == logicalop.LeftOuterJoin || p.JoinType == logicalop.RightOuterJoin {
		// TiFlash does not require that the build side must be the inner table for outer join.
		// so we can choose the build side based on the row count, except that:
		// 1. it is a broadcast join(for broadcast join, it makes sense to use the broadcast side as the build side)
		// 2. or session variable MPPOuterJoinFixedBuildSide is set to true
		// 3. or nullAware/cross joins
		if useBCJ || p.IsNAAJ() || len(p.EqualConditions) == 0 || p.SCtx().GetSessionVars().MPPOuterJoinFixedBuildSide {
			if !p.SCtx().GetSessionVars().MPPOuterJoinFixedBuildSide {
				// The hint has higher priority than variable.
				fixedBuildSide = true
			}
			if p.JoinType == logicalop.LeftOuterJoin {
				preferredBuildIndex = 1
			}
		} else if stats0.Count() > stats1.Count() {
			preferredBuildIndex = 1
		}
	}

	if forceLeftToBuild || forceRightToBuild {
		match := (forceLeftToBuild && preferredBuildIndex == 0) || (forceRightToBuild && preferredBuildIndex == 1)
		if !match {
			if fixedBuildSide {
				// A warning will be generated if the build side is fixed, but we attempt to change it using the hint.
				p.SCtx().GetSessionVars().StmtCtx.SetHintWarning(
					"Some HASH_JOIN_BUILD and HASH_JOIN_PROBE hints cannot be utilized for MPP joins, please check the hints")
			} else {
				// The HASH_JOIN_BUILD OR HASH_JOIN_PROBE hints can take effective.
				preferredBuildIndex = 1 - preferredBuildIndex
			}
		}
	}

	// set preferredBuildIndex for test
	failpoint.Inject("mockPreferredBuildIndex", func(val failpoint.Value) {
		if !p.SCtx().GetSessionVars().InRestrictedSQL {
			preferredBuildIndex = val.(int)
		}
	})

	baseJoin.InnerChildIdx = preferredBuildIndex
	childrenProps := make([]*property.PhysicalProperty, 2)
	if useBCJ {
		childrenProps[preferredBuildIndex] = &property.PhysicalProperty{TaskTp: property.MppTaskType, ExpectedCnt: math.MaxFloat64, MPPPartitionTp: property.BroadcastType, CanAddEnforcer: true, CTEProducerStatus: prop.CTEProducerStatus}
		expCnt := math.MaxFloat64
		if prop.ExpectedCnt < p.StatsInfo().RowCount {
			expCntScale := prop.ExpectedCnt / p.StatsInfo().RowCount
			var targetStats *property.StatsInfo
			if 1-preferredBuildIndex == 0 {
				targetStats = stats0
			} else {
				targetStats = stats1
			}
			expCnt = targetStats.RowCount * expCntScale
		}
		if prop.MPPPartitionTp == property.HashType {
			lPartitionKeys, rPartitionKeys := p.GetPotentialPartitionKeys()
			hashKeys := rPartitionKeys
			if preferredBuildIndex == 1 {
				hashKeys = lPartitionKeys
			}
			matches := prop.IsSubsetOf(hashKeys)
			if len(matches) == 0 {
				return nil
			}
			childrenProps[1-preferredBuildIndex] = &property.PhysicalProperty{TaskTp: property.MppTaskType, ExpectedCnt: expCnt, MPPPartitionTp: property.HashType, MPPPartitionCols: prop.MPPPartitionCols, CTEProducerStatus: prop.CTEProducerStatus}
		} else {
			childrenProps[1-preferredBuildIndex] = &property.PhysicalProperty{TaskTp: property.MppTaskType, ExpectedCnt: expCnt, MPPPartitionTp: property.AnyType, CTEProducerStatus: prop.CTEProducerStatus}
		}
	} else {
		lPartitionKeys, rPartitionKeys := p.GetPotentialPartitionKeys()
		if prop.MPPPartitionTp == property.HashType {
			var matches []int
			switch p.JoinType {
			case logicalop.InnerJoin:
				if matches = prop.IsSubsetOf(lPartitionKeys); len(matches) == 0 {
					matches = prop.IsSubsetOf(rPartitionKeys)
				}
			case logicalop.RightOuterJoin:
				// for right out join, only the right partition keys can possibly matches the prop, because
				// the left partition keys will generate NULL values randomly
				// todo maybe we can add a null-sensitive flag in the MPPPartitionColumn to indicate whether the partition column is
				//  null-sensitive(used in aggregation) or null-insensitive(used in join)
				matches = prop.IsSubsetOf(rPartitionKeys)
			default:
				// for left out join, only the left partition keys can possibly matches the prop, because
				// the right partition keys will generate NULL values randomly
				// for semi/anti semi/left out semi/anti left out semi join, only left partition keys are returned,
				// so just check the left partition keys
				matches = prop.IsSubsetOf(lPartitionKeys)
			}
			if len(matches) == 0 {
				return nil
			}
			lPartitionKeys = choosePartitionKeys(lPartitionKeys, matches)
			rPartitionKeys = choosePartitionKeys(rPartitionKeys, matches)
		}
		childrenProps[0] = &property.PhysicalProperty{TaskTp: property.MppTaskType, ExpectedCnt: math.MaxFloat64, MPPPartitionTp: property.HashType, MPPPartitionCols: lPartitionKeys, CanAddEnforcer: true, CTEProducerStatus: prop.CTEProducerStatus}
		childrenProps[1] = &property.PhysicalProperty{TaskTp: property.MppTaskType, ExpectedCnt: math.MaxFloat64, MPPPartitionTp: property.HashType, MPPPartitionCols: rPartitionKeys, CanAddEnforcer: true, CTEProducerStatus: prop.CTEProducerStatus}
	}
	join := PhysicalHashJoin{
		BasePhysicalJoin:  baseJoin,
		Concurrency:       uint(p.SCtx().GetSessionVars().CopTiFlashConcurrencyFactor),
		EqualConditions:   p.EqualConditions,
		NAEqualConditions: p.NAEQConditions,
		storeTp:           kv.TiFlash,
		mppShuffleJoin:    !useBCJ,
		// Mpp Join has quite heavy cost. Even limit might not suspend it in time, so we don't scale the count.
	}.Init(p.SCtx(), p.StatsInfo(), p.QueryBlockOffset(), childrenProps...)
	join.SetSchema(p.Schema())
	return []base.PhysicalPlan{join}
}

func choosePartitionKeys(keys []*property.MPPPartitionColumn, matches []int) []*property.MPPPartitionColumn {
	newKeys := make([]*property.MPPPartitionColumn, 0, len(matches))
	for _, id := range matches {
		newKeys = append(newKeys, keys[id])
	}
	return newKeys
}

// get the possible group expression and logical operator from common super pointer.
func getGEAndLogicalJoin(super base.LogicalPlan) (ge *memo.GroupExpression, join *logicalop.LogicalJoin) {
	switch x := super.(type) {
	case *logicalop.LogicalJoin:
		// previously, wrapped BaseLogicalPlan serve as the common part, so we need to use self()
		// to downcast as the every specific logical operator.
		join = x
	case *memo.GroupExpression:
		// currently, since GroupExpression wrap a LogicalPlan as its first field, we GE itself is
		// naturally can be referred as a LogicalPlan, and we need ot use GetWrappedLogicalPlan to
		// get the specific logical operator inside.
		ge = x
		join = ge.GetWrappedLogicalPlan().(*logicalop.LogicalJoin)
	}
	return ge, join
}

// it can generates hash join, index join and sort merge join.
// Firstly we check the hint, if hint is figured by user, we force to choose the corresponding physical plan.
// If the hint is not matched, it will get other candidates.
// If the hint is not figured, we will pick all candidates.
func exhaustPhysicalPlans4LogicalJoin(super base.LogicalPlan, prop *property.PhysicalProperty) ([]base.PhysicalPlan, bool, error) {
	ge, p := getGEAndLogicalJoin(super)
	failpoint.Inject("MockOnlyEnableIndexHashJoin", func(val failpoint.Value) {
		if val.(bool) && !p.SCtx().GetSessionVars().InRestrictedSQL {
			indexJoins, _ := tryToGetIndexJoin(p, prop)
			failpoint.Return(indexJoins, true, nil)
		}
	})

	if !isJoinHintSupportedInMPPMode(p.PreferJoinType) {
		if hasMPPJoinHints(p.PreferJoinType) {
			// If there are MPP hints but has some conflicts join method hints, all the join hints are invalid.
			p.SCtx().GetSessionVars().StmtCtx.SetHintWarning("The MPP join hints are in conflict, and you can only specify join method hints that are currently supported by MPP mode now")
			p.PreferJoinType = 0
		} else {
			// If there are no MPP hints but has some conflicts join method hints, the MPP mode will be blocked.
			p.SCtx().GetSessionVars().RaiseWarningWhenMPPEnforced("MPP mode may be blocked because you have used hint to specify a join algorithm which is not supported by mpp now.")
			if prop.IsFlashProp() {
				return nil, false, nil
			}
		}
	}
	if prop.MPPPartitionTp == property.BroadcastType {
		return nil, false, nil
	}
	joins := make([]base.PhysicalPlan, 0, 8)
	// we lift the p.canPushToTiFlash check here, because we want to generate all the plans to be decided by the attachment layer.
	if p.SCtx().GetSessionVars().IsMPPAllowed() {
		// prefer hint should be handled in the attachment layer. because the enumerated mpp join may couldn't be built bottom-up.
		if hasMPPJoinHints(p.PreferJoinType) {
			// generate them all for later attachment prefer picking. cause underlying ds may not have tiFlash path.
			// even all mpp join is invalid, they can still resort to root joins as an alternative.
			joins = append(joins, tryToGetMppHashJoin(super, prop, true)...)
			joins = append(joins, tryToGetMppHashJoin(super, prop, false)...)
		} else {
			// join don't have a mpp join hints, only generate preferMppBCJ mpp joins.
			if preferMppBCJ(super) {
				joins = append(joins, tryToGetMppHashJoin(super, prop, true)...)
			} else {
				joins = append(joins, tryToGetMppHashJoin(super, prop, false)...)
			}
		}
	} else {
		hasMppHints := false
		var errMsg string
		if (p.PreferJoinType & h.PreferShuffleJoin) > 0 {
			errMsg = "The join can not push down to the MPP side, the shuffle_join() hint is invalid"
			hasMppHints = true
		}
		if (p.PreferJoinType & h.PreferBCJoin) > 0 {
			errMsg = "The join can not push down to the MPP side, the broadcast_join() hint is invalid"
			hasMppHints = true
		}
		if hasMppHints {
			p.SCtx().GetSessionVars().StmtCtx.SetHintWarning(errMsg)
		}
	}
	if prop.IsFlashProp() {
		return joins, true, nil
	}

	if !p.IsNAAJ() {
		// naaj refuse merge join and index join.
		stats0, stats1, _, _ := getJoinChildStatsAndSchema(ge, p)
		mergeJoins := physicalop.GetMergeJoin(p, prop, p.Schema(), p.StatsInfo(), stats0, stats1)
		if (p.PreferJoinType&h.PreferMergeJoin) > 0 && len(mergeJoins) > 0 {
			return mergeJoins, true, nil
		}
		joins = append(joins, mergeJoins...)

		if p.SCtx().GetSessionVars().EnhanceIndexJoinBuildV2 {
			indexJoins := tryToEnumerateIndexJoin(super, prop)
			joins = append(joins, indexJoins...)

			failpoint.Inject("MockOnlyEnableIndexHashJoinV2", func(val failpoint.Value) {
				if val.(bool) && !p.SCtx().GetSessionVars().InRestrictedSQL {
					indexHashJoin := make([]base.PhysicalPlan, 0, len(indexJoins))
					for _, one := range indexJoins {
						if _, ok := one.(*PhysicalIndexHashJoin); ok {
							indexHashJoin = append(indexHashJoin, one)
						}
					}
					failpoint.Return(indexHashJoin, true, nil)
				}
			})
		} else {
			indexJoins, forced := tryToGetIndexJoin(p, prop)
			if forced {
				return indexJoins, true, nil
			}
			joins = append(joins, indexJoins...)
		}
	}

	hashJoins, forced := getHashJoins(super, prop)
	if forced && len(hashJoins) > 0 {
		return hashJoins, true, nil
	}
	joins = append(joins, hashJoins...)

	if p.PreferJoinType > 0 {
		// If we reach here, it means we have a hint that doesn't work.
		// It might be affected by the required property, so we enforce
		// this property and try the hint again.
		return joins, false, nil
	}
	return joins, true, nil
}

func exhaustPhysicalPlans4LogicalExpand(lp base.LogicalPlan, prop *property.PhysicalProperty) ([]base.PhysicalPlan, bool, error) {
	p := lp.(*logicalop.LogicalExpand)
	// under the mpp task type, if the sort item is not empty, refuse it, cause expanded data doesn't support any sort items.
	if !prop.IsSortItemEmpty() {
		// false, meaning we can add a sort enforcer.
		return nil, false, nil
	}
	// when TiDB Expand execution is introduced: we can deal with two kind of physical plans.
	// RootTaskType means expand should be run at TiDB node.
	//	(RootTaskType is the default option, we can also generate a mpp candidate for it)
	// MPPTaskType means expand should be run at TiFlash node.
	if prop.TaskTp != property.RootTaskType && prop.TaskTp != property.MppTaskType {
		return nil, true, nil
	}
	// now Expand mode can only be executed on TiFlash node.
	// Upper layer shouldn't expect any mpp partition from an Expand operator.
	// todo: data output from Expand operator should keep the origin data mpp partition.
	if prop.TaskTp == property.MppTaskType && prop.MPPPartitionTp != property.AnyType {
		return nil, true, nil
	}
	var physicalExpands []base.PhysicalPlan
	// for property.RootTaskType and property.MppTaskType with no partition option, we can give an MPP Expand.
	// we just remove whether subtree can be pushed to tiFlash check, and left child handle itself.
	if p.SCtx().GetSessionVars().IsMPPAllowed() {
		mppProp := prop.CloneEssentialFields()
		mppProp.TaskTp = property.MppTaskType
		expand := PhysicalExpand{
			GroupingSets:          p.RollupGroupingSets,
			LevelExprs:            p.LevelExprs,
			ExtraGroupingColNames: p.ExtraGroupingColNames,
		}.Init(p.SCtx(), p.StatsInfo().ScaleByExpectCnt(prop.ExpectedCnt), p.QueryBlockOffset(), mppProp)
		expand.SetSchema(p.Schema())
		physicalExpands = append(physicalExpands, expand)
		// when the MppTaskType is required, we can return the physical plan directly.
		if prop.TaskTp == property.MppTaskType {
			return physicalExpands, true, nil
		}
	}
	// for property.RootTaskType, we can give a TiDB Expand.
	{
		taskTypes := []property.TaskType{property.CopSingleReadTaskType, property.CopMultiReadTaskType, property.MppTaskType, property.RootTaskType}
		for _, taskType := range taskTypes {
			// require cop task type for children.F
			tidbProp := prop.CloneEssentialFields()
			tidbProp.TaskTp = taskType
			expand := PhysicalExpand{
				GroupingSets:          p.RollupGroupingSets,
				LevelExprs:            p.LevelExprs,
				ExtraGroupingColNames: p.ExtraGroupingColNames,
			}.Init(p.SCtx(), p.StatsInfo().ScaleByExpectCnt(prop.ExpectedCnt), p.QueryBlockOffset(), tidbProp)
			expand.SetSchema(p.Schema())
			physicalExpands = append(physicalExpands, expand)
		}
	}
	return physicalExpands, true, nil
}

// get the possible group expression and logical operator from common super pointer.
func getGEAndLogicalProjection(super base.LogicalPlan) (ge *memo.GroupExpression, proj *logicalop.LogicalProjection) {
	switch x := super.(type) {
	case *logicalop.LogicalProjection:
		// previously, wrapped BaseLogicalPlan serve as the common part, so we need to use self()
		// to downcast as the every specific logical operator.
		proj = x
	case *memo.GroupExpression:
		// currently, since GroupExpression wrap a LogicalPlan as its first field, we GE itself is
		// naturally can be referred as a LogicalPlan, and we need ot use GetWrappedLogicalPlan to
		// get the specific logical operator inside.
		ge = x
		proj = ge.GetWrappedLogicalPlan().(*logicalop.LogicalProjection)
	}
	return ge, proj
}

func exhaustPhysicalPlans4LogicalProjection(super base.LogicalPlan, prop *property.PhysicalProperty) ([]base.PhysicalPlan, bool, error) {
	ge, p := getGEAndLogicalProjection(super)
	newProp, ok := p.TryToGetChildProp(prop)
	if !ok {
		return nil, true, nil
	}
	newProps := []*property.PhysicalProperty{newProp}
	// generate a mpp task candidate if mpp mode is allowed
	ctx := p.SCtx()
	pushDownCtx := util.GetPushDownCtx(ctx)
	_, childSchema := getChildStatsAndSchema(ge, p)
	// lift the recursive check of canPushToCop(tiFlash)
	if newProp.TaskTp != property.MppTaskType && ctx.GetSessionVars().IsMPPAllowed() &&
		expression.CanExprsPushDown(pushDownCtx, p.Exprs, kv.TiFlash) {
		mppProp := newProp.CloneEssentialFields()
		mppProp.TaskTp = property.MppTaskType
		newProps = append(newProps, mppProp)
	}
	// lift the recursive check of canPushToCop(tikv)
	if newProp.TaskTp != property.CopSingleReadTaskType && ctx.GetSessionVars().AllowProjectionPushDown &&
		expression.CanExprsPushDown(pushDownCtx, p.Exprs, kv.TiKV) && !expression.ContainVirtualColumn(p.Exprs) &&
		expression.ProjectionBenefitsFromPushedDown(p.Exprs, childSchema.Len()) {
		copProp := newProp.CloneEssentialFields()
		copProp.TaskTp = property.CopSingleReadTaskType
		newProps = append(newProps, copProp)
	}

	ret := make([]base.PhysicalPlan, 0, len(newProps))
	newProps = admitIndexJoinProps(newProps, prop)
	for _, newProp := range newProps {
		proj := physicalop.PhysicalProjection{
			Exprs:            p.Exprs,
			CalculateNoDelay: p.CalculateNoDelay,
		}.Init(ctx, p.StatsInfo().ScaleByExpectCnt(prop.ExpectedCnt), p.QueryBlockOffset(), newProp)
		proj.SetSchema(p.Schema())
		ret = append(ret, proj)
	}
	return ret, true, nil
}

func pushLimitOrTopNForcibly(p base.LogicalPlan) (meetThreshold bool, preferPushDown bool) {
	switch lp := p.(type) {
	case *logicalop.LogicalTopN:
		preferPushDown = lp.PreferLimitToCop
		meetThreshold = lp.Count+lp.Offset <= uint64(lp.SCtx().GetSessionVars().LimitPushDownThreshold)
	case *logicalop.LogicalLimit:
		preferPushDown = lp.PreferLimitToCop
		meetThreshold = true // always push Limit down in this case since it has no side effect
	default:
		return preferPushDown, meetThreshold
	}

	// we remove the child subTree check, each logical operator only focus on themselves.
	// for current level, they prefer a push-down copTask.
	return preferPushDown, meetThreshold
}

func getPhysTopN(lt *logicalop.LogicalTopN, prop *property.PhysicalProperty) []base.PhysicalPlan {
	// topN should always generate rootTaskType for:
	// case1: after v7.5, since tiFlash Cop has been banned, mppTaskType may return invalid task when there are some root conditions.
	// case2: for index merge case which can only be run in root type, topN and limit can't be pushed to the inside index merge when it's an intersection.
	// note: don't change the task enumeration order here.
	allTaskTypes := []property.TaskType{property.CopSingleReadTaskType, property.CopMultiReadTaskType, property.RootTaskType}
	// we move the pushLimitOrTopNForcibly check to attach2Task to do the prefer choice.
	mppAllowed := lt.SCtx().GetSessionVars().IsMPPAllowed()
	if mppAllowed {
		allTaskTypes = append(allTaskTypes, property.MppTaskType)
	}
	ret := make([]base.PhysicalPlan, 0, len(allTaskTypes))
	for _, tp := range allTaskTypes {
		resultProp := &property.PhysicalProperty{TaskTp: tp, ExpectedCnt: math.MaxFloat64,
			CTEProducerStatus: prop.CTEProducerStatus, NoCopPushDown: prop.NoCopPushDown}
		topN := physicalop.PhysicalTopN{
			ByItems:     lt.ByItems,
			PartitionBy: lt.PartitionBy,
			Count:       lt.Count,
			Offset:      lt.Offset,
		}.Init(lt.SCtx(), lt.StatsInfo(), lt.QueryBlockOffset(), resultProp)
		topN.SetSchema(lt.Schema())
		ret = append(ret, topN)
	}
	// If we can generate MPP task and there's vector distance function in the order by column.
	// We will try to generate a property for possible vector indexes.
	if mppAllowed {
		if len(lt.ByItems) != 1 {
			return ret
		}
		vs := expression.InterpretVectorSearchExpr(lt.ByItems[0].Expr)
		if vs == nil {
			return ret
		}
		// Currently vector index only accept ascending order.
		if lt.ByItems[0].Desc {
			return ret
		}
		// Currently, we only deal with the case the TopN is directly above a DataSource.
		ds, ok := lt.Children()[0].(*logicalop.DataSource)
		if !ok {
			return ret
		}
		// Reject any filters.
		if len(ds.PushedDownConds) > 0 {
			return ret
		}
		resultProp := &property.PhysicalProperty{
			TaskTp:            property.MppTaskType,
			ExpectedCnt:       math.MaxFloat64,
			CTEProducerStatus: prop.CTEProducerStatus,
		}
		resultProp.VectorProp.VSInfo = vs
		resultProp.VectorProp.TopK = uint32(lt.Count + lt.Offset)
		topN := physicalop.PhysicalTopN{
			ByItems:     lt.ByItems,
			PartitionBy: lt.PartitionBy,
			Count:       lt.Count,
			Offset:      lt.Offset,
		}.Init(lt.SCtx(), lt.StatsInfo(), lt.QueryBlockOffset(), resultProp)
		topN.SetSchema(lt.Schema())
		ret = append(ret, topN)
	}
	return ret
}

func getPhysLimits(lt *logicalop.LogicalTopN, prop *property.PhysicalProperty) []base.PhysicalPlan {
	p, canPass := GetPropByOrderByItems(lt.ByItems)
	if !canPass {
		return nil
	}
	// note: don't change the task enumeration order here.
	allTaskTypes := []property.TaskType{property.CopSingleReadTaskType, property.CopMultiReadTaskType, property.RootTaskType}
	ret := make([]base.PhysicalPlan, 0, len(allTaskTypes))
	for _, tp := range allTaskTypes {
		resultProp := &property.PhysicalProperty{TaskTp: tp, ExpectedCnt: float64(lt.Count + lt.Offset), SortItems: p.SortItems,
			CTEProducerStatus: prop.CTEProducerStatus, NoCopPushDown: prop.NoCopPushDown}
		limit := physicalop.PhysicalLimit{
			Count:       lt.Count,
			Offset:      lt.Offset,
			PartitionBy: lt.GetPartitionBy(),
		}.Init(lt.SCtx(), lt.StatsInfo(), lt.QueryBlockOffset(), resultProp)
		limit.SetSchema(lt.Schema())
		ret = append(ret, limit)
	}
	return ret
}

// MatchItems checks if this prop's columns can match by items totally.
func MatchItems(p *property.PhysicalProperty, items []*util.ByItems) bool {
	if len(items) < len(p.SortItems) {
		return false
	}
	for i, col := range p.SortItems {
		sortItem := items[i]
		if sortItem.Desc != col.Desc || !col.Col.EqualColumn(sortItem.Expr) {
			return false
		}
	}
	return true
}

// GetHashJoin is public for cascades planner.
func GetHashJoin(ge *memo.GroupExpression, la *logicalop.LogicalApply, prop *property.PhysicalProperty) *PhysicalHashJoin {
	return getHashJoin(ge, &la.LogicalJoin, prop, 1, false)
}

// get the possible group expression and logical operator from common super pointer.
func getGEAndLogicalApply(super base.LogicalPlan) (ge *memo.GroupExpression, apply *logicalop.LogicalApply) {
	switch x := super.(type) {
	case *logicalop.LogicalApply:
		// previously, wrapped BaseLogicalPlan serve as the common part, so we need to use self()
		// to downcast as the every specific logical operator.
		apply = x
	case *memo.GroupExpression:
		// currently, since GroupExpression wrap a LogicalPlan as its first field, we GE itself is
		// naturally can be referred as a LogicalPlan, and we need ot use GetWrappedLogicalPlan to
		// get the specific logical operator inside.
		ge = x
		apply = ge.GetWrappedLogicalPlan().(*logicalop.LogicalApply)
	}
	return ge, apply
}

// exhaustPhysicalPlans4LogicalApply generates the physical plan for a logical apply.
func exhaustPhysicalPlans4LogicalApply(super base.LogicalPlan, prop *property.PhysicalProperty) ([]base.PhysicalPlan, bool, error) {
	ge, la := getGEAndLogicalApply(super)
	_, _, schema0, _ := getJoinChildStatsAndSchema(ge, la)
	if !prop.AllColsFromSchema(schema0) || prop.IsFlashProp() { // for convenient, we don't pass through any prop
		la.SCtx().GetSessionVars().RaiseWarningWhenMPPEnforced(
			"MPP mode may be blocked because operator `Apply` is not supported now.")
		return nil, true, nil
	}
	if !prop.IsSortItemEmpty() && la.SCtx().GetSessionVars().EnableParallelApply {
		la.SCtx().GetSessionVars().StmtCtx.AppendWarning(errors.NewNoStackError("Parallel Apply rejects the possible order properties of its outer child currently"))
		return nil, true, nil
	}
	join := GetHashJoin(ge, la, prop)
	var columns = make([]*expression.Column, 0, len(la.CorCols))
	for _, colColumn := range la.CorCols {
		// fix the liner warning.
		tmp := colColumn
		columns = append(columns, &tmp.Column)
	}
	cacheHitRatio := 0.0
	if la.StatsInfo().RowCount != 0 {
		ndv, _ := cardinality.EstimateColsNDVWithMatchedLen(columns, la.Schema(), la.StatsInfo())
		// for example, if there are 100 rows and the number of distinct values of these correlated columns
		// are 70, then we can assume 30 rows can hit the cache so the cache hit ratio is 1 - (70/100) = 0.3
		cacheHitRatio = 1 - (ndv / la.StatsInfo().RowCount)
	}

	var canUseCache bool
	if cacheHitRatio > 0.1 && la.SCtx().GetSessionVars().MemQuotaApplyCache > 0 {
		canUseCache = true
	} else {
		canUseCache = false
	}

	apply := PhysicalApply{
		PhysicalHashJoin: *join,
		OuterSchema:      la.CorCols,
		CanUseCache:      canUseCache,
	}.Init(la.SCtx(),
		la.StatsInfo().ScaleByExpectCnt(prop.ExpectedCnt),
		la.QueryBlockOffset(),
		&property.PhysicalProperty{ExpectedCnt: math.MaxFloat64, SortItems: prop.SortItems, CTEProducerStatus: prop.CTEProducerStatus, NoCopPushDown: true},
		&property.PhysicalProperty{ExpectedCnt: math.MaxFloat64, CTEProducerStatus: prop.CTEProducerStatus, NoCopPushDown: prop.NoCopPushDown})
	apply.SetSchema(la.Schema())
	return []base.PhysicalPlan{apply}, true, nil
}

func tryToGetMppWindows(lw *logicalop.LogicalWindow, prop *property.PhysicalProperty) []base.PhysicalPlan {
	if !prop.IsSortItemAllForPartition() {
		return nil
	}
	if prop.TaskTp != property.RootTaskType && prop.TaskTp != property.MppTaskType {
		return nil
	}
	if prop.MPPPartitionTp == property.BroadcastType {
		return nil
	}

	{
		allSupported := true
		sctx := lw.SCtx()
		for _, windowFunc := range lw.WindowFuncDescs {
			if !windowFunc.CanPushDownToTiFlash(util.GetPushDownCtx(sctx)) {
				lw.SCtx().GetSessionVars().RaiseWarningWhenMPPEnforced(
					"MPP mode may be blocked because window function `" + windowFunc.Name + "` or its arguments are not supported now.")
				allSupported = false
			} else if !expression.IsPushDownEnabled(windowFunc.Name, kv.TiFlash) {
				lw.SCtx().GetSessionVars().RaiseWarningWhenMPPEnforced("MPP mode may be blocked because window function `" + windowFunc.Name + "` is blocked by blacklist, check `table mysql.expr_pushdown_blacklist;` for more information.")
				return nil
			}
		}
		if !allSupported {
			return nil
		}

		if lw.Frame != nil && lw.Frame.Type == ast.Ranges {
			ctx := lw.SCtx().GetExprCtx()
			if _, err := expression.ExpressionsToPBList(ctx.GetEvalCtx(), lw.Frame.Start.CalcFuncs, lw.SCtx().GetClient()); err != nil {
				lw.SCtx().GetSessionVars().RaiseWarningWhenMPPEnforced(
					"MPP mode may be blocked because window function frame can't be pushed down, because " + err.Error())
				return nil
			}
			if !expression.CanExprsPushDown(util.GetPushDownCtx(sctx), lw.Frame.Start.CalcFuncs, kv.TiFlash) {
				lw.SCtx().GetSessionVars().RaiseWarningWhenMPPEnforced(
					"MPP mode may be blocked because window function frame can't be pushed down")
				return nil
			}
			if _, err := expression.ExpressionsToPBList(ctx.GetEvalCtx(), lw.Frame.End.CalcFuncs, lw.SCtx().GetClient()); err != nil {
				lw.SCtx().GetSessionVars().RaiseWarningWhenMPPEnforced(
					"MPP mode may be blocked because window function frame can't be pushed down, because " + err.Error())
				return nil
			}
			if !expression.CanExprsPushDown(util.GetPushDownCtx(sctx), lw.Frame.End.CalcFuncs, kv.TiFlash) {
				lw.SCtx().GetSessionVars().RaiseWarningWhenMPPEnforced(
					"MPP mode may be blocked because window function frame can't be pushed down")
				return nil
			}

			if !lw.CheckComparisonForTiFlash(lw.Frame.Start) || !lw.CheckComparisonForTiFlash(lw.Frame.End) {
				lw.SCtx().GetSessionVars().RaiseWarningWhenMPPEnforced(
					"MPP mode may be blocked because window function frame can't be pushed down, because Duration vs Datetime is invalid comparison as TiFlash can't handle it so far.")
				return nil
			}
		}
	}

	var byItems []property.SortItem
	byItems = append(byItems, lw.PartitionBy...)
	byItems = append(byItems, lw.OrderBy...)
	childProperty := &property.PhysicalProperty{
		ExpectedCnt:           math.MaxFloat64,
		CanAddEnforcer:        true,
		SortItems:             byItems,
		TaskTp:                property.MppTaskType,
		SortItemsForPartition: byItems,
		CTEProducerStatus:     prop.CTEProducerStatus,
	}
	if !prop.IsPrefix(childProperty) {
		return nil
	}

	if len(lw.PartitionBy) > 0 {
		partitionCols := lw.GetPartitionKeys()
		// trying to match the required partitions.
		if prop.MPPPartitionTp == property.HashType {
			matches := prop.IsSubsetOf(partitionCols)
			if len(matches) == 0 {
				// do not satisfy the property of its parent, so return empty
				return nil
			}
			partitionCols = choosePartitionKeys(partitionCols, matches)
		}
		childProperty.MPPPartitionTp = property.HashType
		childProperty.MPPPartitionCols = partitionCols
	} else {
		childProperty.MPPPartitionTp = property.SinglePartitionType
	}

	if prop.MPPPartitionTp == property.SinglePartitionType && childProperty.MPPPartitionTp != property.SinglePartitionType {
		return nil
	}

	window := physicalop.PhysicalWindow{
		WindowFuncDescs: lw.WindowFuncDescs,
		PartitionBy:     lw.PartitionBy,
		OrderBy:         lw.OrderBy,
		Frame:           lw.Frame,
		StoreTp:         kv.TiFlash,
	}.Init(lw.SCtx(), lw.StatsInfo().ScaleByExpectCnt(prop.ExpectedCnt), lw.QueryBlockOffset(), childProperty)
	window.SetSchema(lw.Schema())

	return []base.PhysicalPlan{window}
}

func exhaustPhysicalPlans4LogicalWindow(lp base.LogicalPlan, prop *property.PhysicalProperty) ([]base.PhysicalPlan, bool, error) {
	lw := lp.(*logicalop.LogicalWindow)
	windows := make([]base.PhysicalPlan, 0, 2)

	// we lift the p.CanPushToCop(tiFlash) check here.
	if lw.SCtx().GetSessionVars().IsMPPAllowed() {
		mppWindows := tryToGetMppWindows(lw, prop)
		windows = append(windows, mppWindows...)
	}

	// if there needs a mpp task, we don't generate tidb window function.
	if prop.TaskTp == property.MppTaskType {
		return windows, true, nil
	}
	var byItems []property.SortItem
	byItems = append(byItems, lw.PartitionBy...)
	byItems = append(byItems, lw.OrderBy...)
	childProperty := &property.PhysicalProperty{ExpectedCnt: math.MaxFloat64, SortItems: byItems,
		CanAddEnforcer: true, CTEProducerStatus: prop.CTEProducerStatus, NoCopPushDown: prop.NoCopPushDown}
	if !prop.IsPrefix(childProperty) {
		return nil, true, nil
	}
	window := physicalop.PhysicalWindow{
		WindowFuncDescs: lw.WindowFuncDescs,
		PartitionBy:     lw.PartitionBy,
		OrderBy:         lw.OrderBy,
		Frame:           lw.Frame,
	}.Init(lw.SCtx(), lw.StatsInfo().ScaleByExpectCnt(prop.ExpectedCnt), lw.QueryBlockOffset(), childProperty)
	window.SetSchema(lw.Schema())

	windows = append(windows, window)
	return windows, true, nil
}

func getEnforcedStreamAggs(la *logicalop.LogicalAggregation, prop *property.PhysicalProperty) []base.PhysicalPlan {
	if prop.IsFlashProp() {
		return nil
	}
	if prop.IndexJoinProp != nil {
		// since this stream agg is in the inner side of an index join, the
		// enforced sort operator couldn't be built by executor layer now.
		return nil
	}
	_, desc := prop.AllSameOrder()
	allTaskTypes := prop.GetAllPossibleChildTaskTypes()
	enforcedAggs := make([]base.PhysicalPlan, 0, len(allTaskTypes))
	childProp := &property.PhysicalProperty{
		ExpectedCnt:    math.Max(prop.ExpectedCnt*la.InputCount/la.StatsInfo().RowCount, prop.ExpectedCnt),
		CanAddEnforcer: true,
		SortItems:      property.SortItemsFromCols(la.GetGroupByCols(), desc),
		NoCopPushDown:  prop.NoCopPushDown,
	}
	if !prop.IsPrefix(childProp) {
		// empty
		return enforcedAggs
	}
	childProp = admitIndexJoinProp(childProp, prop)
	if childProp == nil {
		// empty
		return enforcedAggs
	}
	taskTypes := []property.TaskType{property.CopSingleReadTaskType, property.CopMultiReadTaskType, property.RootTaskType}
	// only admit special types for index join prop
	taskTypes = admitIndexJoinTypes(taskTypes, prop)
	for _, taskTp := range taskTypes {
		copiedChildProperty := new(property.PhysicalProperty)
		*copiedChildProperty = *childProp // It's ok to not deep copy the "cols" field.
		copiedChildProperty.TaskTp = taskTp

		newGbyItems := make([]expression.Expression, len(la.GroupByItems))
		copy(newGbyItems, la.GroupByItems)
		newAggFuncs := make([]*aggregation.AggFuncDesc, len(la.AggFuncs))
		copy(newAggFuncs, la.AggFuncs)

		agg := physicalop.BasePhysicalAgg{
			GroupByItems: newGbyItems,
			AggFuncs:     newAggFuncs,
		}
		streamAgg := agg.InitForStream(la.SCtx(), la.StatsInfo().ScaleByExpectCnt(prop.ExpectedCnt), la.QueryBlockOffset(), la.Schema().Clone(), copiedChildProperty)
		enforcedAggs = append(enforcedAggs, streamAgg)
	}
	return enforcedAggs
}

func getStreamAggs(lp base.LogicalPlan, prop *property.PhysicalProperty) []base.PhysicalPlan {
	la := lp.(*logicalop.LogicalAggregation)
	// TODO: support CopTiFlash task type in stream agg
	if prop.IsFlashProp() {
		return nil
	}
	all, desc := prop.AllSameOrder()
	if !all {
		return nil
	}

	for _, aggFunc := range la.AggFuncs {
		if aggFunc.Mode == aggregation.FinalMode {
			return nil
		}
	}
	// group by a + b is not interested in any order.
	groupByCols := la.GetGroupByCols()
	if len(groupByCols) != len(la.GroupByItems) {
		return nil
	}

	allTaskTypes := prop.GetAllPossibleChildTaskTypes()
	streamAggs := make([]base.PhysicalPlan, 0, len(la.PossibleProperties)*(len(allTaskTypes)-1)+len(allTaskTypes))
	childProp := &property.PhysicalProperty{
		ExpectedCnt:   math.Max(prop.ExpectedCnt*la.InputCount/la.StatsInfo().RowCount, prop.ExpectedCnt),
		NoCopPushDown: prop.NoCopPushDown,
	}
	childProp = admitIndexJoinProp(childProp, prop)
	if childProp == nil {
		return nil
	}
	for _, possibleChildProperty := range la.PossibleProperties {
		childProp.SortItems = property.SortItemsFromCols(possibleChildProperty[:len(groupByCols)], desc)
		if !prop.IsPrefix(childProp) {
			continue
		}
		// The table read of "CopDoubleReadTaskType" can't promises the sort
		// property that the stream aggregation required, no need to consider.
		taskTypes := []property.TaskType{property.CopSingleReadTaskType, property.RootTaskType}
		// aggregation has a special case that it can be pushed down to TiKV which is indicated by the prop.NoCopPushDown
		if prop.NoCopPushDown {
			taskTypes = []property.TaskType{property.RootTaskType}
		}
		if la.HasDistinct() && la.SCtx().GetSessionVars().AllowDistinctAggPushDown && !la.DistinctArgsMeetsProperty() {
			// if distinct agg push down is allowed, while the distinct args doesn't meet the required property, continue
			// to next possible property check.
			continue
		}
		taskTypes = admitIndexJoinTypes(taskTypes, prop)
		for _, taskTp := range taskTypes {
			copiedChildProperty := new(property.PhysicalProperty)
			*copiedChildProperty = *childProp // It's ok to not deep copy the "cols" field.
			copiedChildProperty.TaskTp = taskTp

			newGbyItems := make([]expression.Expression, len(la.GroupByItems))
			copy(newGbyItems, la.GroupByItems)
			newAggFuncs := make([]*aggregation.AggFuncDesc, len(la.AggFuncs))
			copy(newAggFuncs, la.AggFuncs)

			baseAgg := &physicalop.BasePhysicalAgg{
				GroupByItems: newGbyItems,
				AggFuncs:     newAggFuncs,
			}
			streamAgg := baseAgg.InitForStream(la.SCtx(), la.StatsInfo().ScaleByExpectCnt(prop.ExpectedCnt), la.QueryBlockOffset(), la.Schema().Clone(), copiedChildProperty)
			streamAggs = append(streamAggs, streamAgg)
		}
	}
	// If STREAM_AGG hint is existed, it should consider enforce stream aggregation,
	// because we can't trust possibleChildProperty completely.
	if (la.PreferAggType & h.PreferStreamAgg) > 0 {
		streamAggs = append(streamAggs, getEnforcedStreamAggs(la, prop)...)
	}
	return streamAggs
}

// TODO: support more operators and distinct later
func checkCanPushDownToMPP(la *logicalop.LogicalAggregation) bool {
	hasUnsupportedDistinct := false
	for _, agg := range la.AggFuncs {
		// MPP does not support distinct except count distinct now
		if agg.HasDistinct {
			if agg.Name != ast.AggFuncCount && agg.Name != ast.AggFuncGroupConcat {
				hasUnsupportedDistinct = true
			}
		}
		// MPP does not support AggFuncApproxCountDistinct now
		if agg.Name == ast.AggFuncApproxCountDistinct {
			hasUnsupportedDistinct = true
		}
	}
	if hasUnsupportedDistinct {
		warnErr := errors.NewNoStackError("Aggregation can not be pushed to storage layer in mpp mode because it contains agg function with distinct")
		if la.SCtx().GetSessionVars().StmtCtx.InExplainStmt {
			la.SCtx().GetSessionVars().StmtCtx.AppendWarning(warnErr)
		} else {
			la.SCtx().GetSessionVars().StmtCtx.AppendExtraWarning(warnErr)
		}
		return false
	}
	return physicalop.CheckAggCanPushCop(la.SCtx(), la.AggFuncs, la.GroupByItems, kv.TiFlash)
}

func tryToGetMppHashAggs(la *logicalop.LogicalAggregation, prop *property.PhysicalProperty) (hashAggs []base.PhysicalPlan) {
	if !prop.IsSortItemEmpty() {
		return nil
	}
	if prop.TaskTp != property.RootTaskType && prop.TaskTp != property.MppTaskType {
		return nil
	}
	if prop.MPPPartitionTp == property.BroadcastType {
		return nil
	}

	// Is this aggregate a final stage aggregate?
	// Final agg can't be split into multi-stage aggregate
	hasFinalAgg := len(la.AggFuncs) > 0 && la.AggFuncs[0].Mode == aggregation.FinalMode
	// count final agg should become sum for MPP execution path.
	// In the traditional case, TiDB take up the final agg role and push partial agg to TiKV,
	// while TiDB can tell the partialMode and do the sum computation rather than counting but MPP doesn't
	finalAggAdjust := func(aggFuncs []*aggregation.AggFuncDesc) {
		for i, agg := range aggFuncs {
			if agg.Mode == aggregation.FinalMode && agg.Name == ast.AggFuncCount {
				oldFT := agg.RetTp
				aggFuncs[i], _ = aggregation.NewAggFuncDesc(la.SCtx().GetExprCtx(), ast.AggFuncSum, agg.Args, false)
				aggFuncs[i].TypeInfer4FinalCount(oldFT)
			}
		}
	}
	// ref: https://github.com/pingcap/tiflash/blob/3ebb102fba17dce3d990d824a9df93d93f1ab
	// 766/dbms/src/Flash/Coprocessor/AggregationInterpreterHelper.cpp#L26
	validMppAgg := func(mppAgg *PhysicalHashAgg) bool {
		isFinalAgg := true
		if mppAgg.AggFuncs[0].Mode != aggregation.FinalMode && mppAgg.AggFuncs[0].Mode != aggregation.CompleteMode {
			isFinalAgg = false
		}
		for _, one := range mppAgg.AggFuncs[1:] {
			otherIsFinalAgg := one.Mode == aggregation.FinalMode || one.Mode == aggregation.CompleteMode
			if isFinalAgg != otherIsFinalAgg {
				// different agg mode detected in mpp side.
				return false
			}
		}
		return true
	}

	if len(la.GroupByItems) > 0 {
		partitionCols := la.GetPotentialPartitionKeys()
		// trying to match the required partitions.
		if prop.MPPPartitionTp == property.HashType {
			// partition key required by upper layer is subset of current layout.
			matches := prop.IsSubsetOf(partitionCols)
			if len(matches) == 0 {
				// do not satisfy the property of its parent, so return empty
				return nil
			}
			partitionCols = choosePartitionKeys(partitionCols, matches)
		} else if prop.MPPPartitionTp != property.AnyType {
			return nil
		}
		// TODO: permute various partition columns from group-by columns
		// 1-phase agg
		// If there are no available partition cols, but still have group by items, that means group by items are all expressions or constants.
		// To avoid mess, we don't do any one-phase aggregation in this case.
		// If this is a skew distinct group agg, skip generating 1-phase agg, because skew data will cause performance issue
		//
		// Rollup can't be 1-phase agg: cause it will append grouping_id to the schema, and expand each row as multi rows with different grouping_id.
		// In a general, group items should also append grouping_id as its group layout, let's say 1-phase agg has grouping items as <a,b,c>, and
		// lower OP can supply <a,b> as original partition layout, when we insert Expand logic between them:
		// <a,b>             -->    after fill null in Expand    --> and this shown two rows should be shuffled to the same node (the underlying partition is not satisfied yet)
		// <1,1> in node A           <1,null,gid=1> in node A
		// <1,2> in node B           <1,null,gid=1> in node B
		if len(partitionCols) != 0 && !la.SCtx().GetSessionVars().EnableSkewDistinctAgg {
			childProp := &property.PhysicalProperty{
				TaskTp:            property.MppTaskType,
				ExpectedCnt:       math.MaxFloat64,
				MPPPartitionTp:    property.HashType,
				MPPPartitionCols:  partitionCols,
				CanAddEnforcer:    true,
				CTEProducerStatus: prop.CTEProducerStatus,
				NoCopPushDown:     prop.NoCopPushDown,
			}
			agg := NewPhysicalHashAgg(la, la.StatsInfo().ScaleByExpectCnt(prop.ExpectedCnt), childProp)
			agg.SetSchema(la.Schema().Clone())
			agg.MppRunMode = physicalop.Mpp1Phase
			finalAggAdjust(agg.AggFuncs)
			if validMppAgg(agg) {
				hashAggs = append(hashAggs, agg)
			}
		}

		// Final agg can't be split into multi-stage aggregate, so exit early
		if hasFinalAgg {
			return
		}

		// 2-phase agg
		// no partition property down，record partition cols inside agg itself, enforce shuffler latter.
		childProp := &property.PhysicalProperty{TaskTp: property.MppTaskType,
			ExpectedCnt:       math.MaxFloat64,
			MPPPartitionTp:    property.AnyType,
			CTEProducerStatus: prop.CTEProducerStatus,
			NoCopPushDown:     prop.NoCopPushDown,
		}
		agg := NewPhysicalHashAgg(la, la.StatsInfo().ScaleByExpectCnt(prop.ExpectedCnt), childProp)
		agg.SetSchema(la.Schema().Clone())
		agg.MppRunMode = physicalop.Mpp2Phase
		agg.MppPartitionCols = partitionCols
		if validMppAgg(agg) {
			hashAggs = append(hashAggs, agg)
		}

		// agg runs on TiDB with a partial agg on TiFlash if possible
		if prop.TaskTp == property.RootTaskType {
			childProp := &property.PhysicalProperty{
				TaskTp:            property.MppTaskType,
				ExpectedCnt:       math.MaxFloat64,
				CTEProducerStatus: prop.CTEProducerStatus,
				NoCopPushDown:     prop.NoCopPushDown,
			}
			agg := NewPhysicalHashAgg(la, la.StatsInfo().ScaleByExpectCnt(prop.ExpectedCnt), childProp)
			agg.SetSchema(la.Schema().Clone())
			agg.MppRunMode = physicalop.MppTiDB
			hashAggs = append(hashAggs, agg)
		}
	} else if !hasFinalAgg {
		// TODO: support scalar agg in MPP, merge the final result to one node
		childProp := &property.PhysicalProperty{TaskTp: property.MppTaskType,
			ExpectedCnt:       math.MaxFloat64,
			CTEProducerStatus: prop.CTEProducerStatus,
			NoCopPushDown:     prop.NoCopPushDown,
		}

		agg := NewPhysicalHashAgg(la, la.StatsInfo().ScaleByExpectCnt(prop.ExpectedCnt), childProp)
		agg.SetSchema(la.Schema().Clone())
		if la.HasDistinct() || la.HasOrderBy() {
			// mpp scalar mode means the data will be pass through to only one tiFlash node at last.
			agg.MppRunMode = physicalop.MppScalar
		} else {
			agg.MppRunMode = physicalop.MppTiDB
		}
		hashAggs = append(hashAggs, agg)
	}

	// handle MPP Agg hints
	var preferMode physicalop.AggMppRunMode
	var prefer bool
	if la.PreferAggType&h.PreferMPP1PhaseAgg > 0 {
		preferMode, prefer = physicalop.Mpp1Phase, true
	} else if la.PreferAggType&h.PreferMPP2PhaseAgg > 0 {
		preferMode, prefer = physicalop.Mpp2Phase, true
	}
	if prefer {
		var preferPlans []base.PhysicalPlan
		for _, agg := range hashAggs {
			if hg, ok := agg.(*PhysicalHashAgg); ok && hg.MppRunMode == preferMode {
				preferPlans = append(preferPlans, hg)
			}
		}
		hashAggs = preferPlans
	}
	return
}

// getHashAggs will generate some kinds of taskType here, which finally converted to different task plan.
// when deciding whether to add a kind of taskType, there is a rule here. [Not is Not, Yes is not Sure]
// eg: which means
//
//	1: when you find something here that block hashAgg to be pushed down to XXX, just skip adding the XXXTaskType.
//	2: when you find nothing here to block hashAgg to be pushed down to XXX, just add the XXXTaskType here.
//	for 2, the final result for this physical operator enumeration is chosen or rejected is according to more factors later (hint/variable/partition/virtual-col/cost)
//
// That is to say, the non-complete positive judgement of canPushDownToMPP/canPushDownToTiFlash/canPushDownToTiKV is not that for sure here.
func getHashAggs(lp base.LogicalPlan, prop *property.PhysicalProperty) []base.PhysicalPlan {
	la := lp.(*logicalop.LogicalAggregation)
	if !prop.IsSortItemEmpty() {
		return nil
	}
	if prop.TaskTp == property.MppTaskType && !checkCanPushDownToMPP(la) {
		return nil
	}
	hashAggs := make([]base.PhysicalPlan, 0, len(prop.GetAllPossibleChildTaskTypes()))
	taskTypes := []property.TaskType{property.CopSingleReadTaskType, property.CopMultiReadTaskType, property.RootTaskType}
	// aggregation has a special case that it can be pushed down to TiKV which is indicated by the prop.NoCopPushDown
	if prop.NoCopPushDown {
		taskTypes = []property.TaskType{property.RootTaskType}
	}
	// lift the recursive check of canPushToCop(tiFlash)
	canPushDownToMPP := la.SCtx().GetSessionVars().IsMPPAllowed() && checkCanPushDownToMPP(la)
	if canPushDownToMPP {
		taskTypes = append(taskTypes, property.MppTaskType)
	} else {
		hasMppHints := false
		var errMsg string
		if la.PreferAggType&h.PreferMPP1PhaseAgg > 0 {
			errMsg = "The agg can not push down to the MPP side, the MPP_1PHASE_AGG() hint is invalid"
			hasMppHints = true
		}
		if la.PreferAggType&h.PreferMPP2PhaseAgg > 0 {
			errMsg = "The agg can not push down to the MPP side, the MPP_2PHASE_AGG() hint is invalid"
			hasMppHints = true
		}
		if hasMppHints {
			la.SCtx().GetSessionVars().StmtCtx.SetHintWarning(errMsg)
		}
	}
	if prop.IsFlashProp() {
		taskTypes = []property.TaskType{prop.TaskTp}
	}
	taskTypes = admitIndexJoinTypes(taskTypes, prop)
	for _, taskTp := range taskTypes {
		if taskTp == property.MppTaskType {
			mppAggs := tryToGetMppHashAggs(la, prop)
			if len(mppAggs) > 0 {
				hashAggs = append(hashAggs, mppAggs...)
			}
		} else {
			childProp := &property.PhysicalProperty{ExpectedCnt: math.MaxFloat64, TaskTp: taskTp, CTEProducerStatus: prop.CTEProducerStatus, NoCopPushDown: prop.NoCopPushDown}
			// mainly to fill indexJoinProp to childProp.
			childProp = admitIndexJoinProp(childProp, prop)
			if childProp == nil {
				continue
			}
			agg := NewPhysicalHashAgg(la, la.StatsInfo().ScaleByExpectCnt(prop.ExpectedCnt), childProp)
			agg.SetSchema(la.Schema().Clone())
			hashAggs = append(hashAggs, agg)
		}
	}
	return hashAggs
}

func exhaustPhysicalPlans4LogicalAggregation(lp base.LogicalPlan, prop *property.PhysicalProperty) ([]base.PhysicalPlan, bool, error) {
	la := lp.(*logicalop.LogicalAggregation)
	preferHash, preferStream := la.ResetHintIfConflicted()
	hashAggs := getHashAggs(la, prop)
	if len(hashAggs) > 0 && preferHash {
		return hashAggs, true, nil
	}
	streamAggs := getStreamAggs(la, prop)
	if len(streamAggs) > 0 && preferStream {
		return streamAggs, true, nil
	}
	aggs := append(hashAggs, streamAggs...)

	if streamAggs == nil && preferStream && !prop.IsSortItemEmpty() {
		la.SCtx().GetSessionVars().StmtCtx.SetHintWarning("Optimizer Hint STREAM_AGG is inapplicable")
	}
	return aggs, !(preferStream || preferHash), nil
}

func admitIndexJoinTypes(types []property.TaskType, prop *property.PhysicalProperty) []property.TaskType {
	if prop.TaskTp == property.MppTaskType {
		// if the parent prop is mppTask, we assume it couldn't contain indexJoinProp by default,
		// which is guaranteed by the parent physical plans enumeration.
		return types
	}
	// only admit root & cop task type to push down indexJoinProp.
	if prop.IndexJoinProp != nil {
		newTypes := types[:0]
		for _, tp := range types {
			if tp != property.MppTaskType {
				newTypes = append(newTypes, tp)
			}
		}
		types = newTypes
	}
	return types
}

func admitIndexJoinProps(children []*property.PhysicalProperty, prop *property.PhysicalProperty) []*property.PhysicalProperty {
	if prop.TaskTp == property.MppTaskType {
		// if the parent prop is mppTask, we assume it couldn't contain indexJoinProp by default,
		// which is guaranteed by the parent physical plans enumeration.
		return children
	}
	// only admit root & cop task type to push down indexJoinProp.
	if prop.IndexJoinProp != nil {
		newChildren := children[:0]
		for _, child := range children {
			if child.TaskTp != property.MppTaskType {
				child.IndexJoinProp = prop.IndexJoinProp
				// only admit non-mpp task prop.
				newChildren = append(newChildren, child)
			}
		}
		children = newChildren
	}
	return children
}

func admitIndexJoinProp(child, prop *property.PhysicalProperty) *property.PhysicalProperty {
	if prop.TaskTp == property.MppTaskType {
		// if the parent prop is mppTask, we assume it couldn't contain indexJoinProp by default,
		// which is guaranteed by the parent physical plans enumeration.
		return child
	}
	// only admit root & cop task type to push down indexJoinProp.
	if prop.IndexJoinProp != nil {
		if child.TaskTp != property.MppTaskType {
			child.IndexJoinProp = prop.IndexJoinProp
		} else {
			// only admit non-mpp task prop.
			child = nil
		}
	}
	return child
}

func exhaustPhysicalPlans4LogicalSelection(lp base.LogicalPlan, prop *property.PhysicalProperty) ([]base.PhysicalPlan, bool, error) {
	p := lp.(*logicalop.LogicalSelection)
	newProps := make([]*property.PhysicalProperty, 0, 2)
	childProp := prop.CloneEssentialFields()
	newProps = append(newProps, childProp)
	// we lift the p.CanPushDown(kv.TiFlash) check here, which may depend on the children.
	canPushDownToTiFlash := !expression.ContainVirtualColumn(p.Conditions) &&
		expression.CanExprsPushDown(util.GetPushDownCtx(p.SCtx()), p.Conditions, kv.TiFlash)

	if prop.TaskTp != property.MppTaskType &&
		p.SCtx().GetSessionVars().IsMPPAllowed() &&
		canPushDownToTiFlash {
		childPropMpp := prop.CloneEssentialFields()
		childPropMpp.TaskTp = property.MppTaskType
		newProps = append(newProps, childPropMpp)
	}

	ret := make([]base.PhysicalPlan, 0, len(newProps))
	newProps = admitIndexJoinProps(newProps, prop)
	for _, newProp := range newProps {
		sel := physicalop.PhysicalSelection{
			Conditions: p.Conditions,
		}.Init(p.SCtx(), p.StatsInfo().ScaleByExpectCnt(prop.ExpectedCnt), p.QueryBlockOffset(), newProp)
		ret = append(ret, sel)
	}
	return ret, true, nil
}

func exhaustPhysicalPlans4LogicalLimit(lp base.LogicalPlan, prop *property.PhysicalProperty) ([]base.PhysicalPlan, bool, error) {
	p := lp.(*logicalop.LogicalLimit)
	return getLimitPhysicalPlans(p, prop)
}

func getLimitPhysicalPlans(p *logicalop.LogicalLimit, prop *property.PhysicalProperty) ([]base.PhysicalPlan, bool, error) {
	if !prop.IsSortItemEmpty() {
		return nil, true, nil
	}

	allTaskTypes := []property.TaskType{property.CopSingleReadTaskType, property.CopMultiReadTaskType, property.RootTaskType}
	// lift the recursive check of canPushToCop(tiFlash)
	if p.SCtx().GetSessionVars().IsMPPAllowed() {
		allTaskTypes = append(allTaskTypes, property.MppTaskType)
	}
	ret := make([]base.PhysicalPlan, 0, len(allTaskTypes))
	for _, tp := range allTaskTypes {
		resultProp := &property.PhysicalProperty{TaskTp: tp, ExpectedCnt: float64(p.Count + p.Offset),
			CTEProducerStatus: prop.CTEProducerStatus, NoCopPushDown: prop.NoCopPushDown}
		limit := physicalop.PhysicalLimit{
			Offset:      p.Offset,
			Count:       p.Count,
			PartitionBy: p.GetPartitionBy(),
		}.Init(p.SCtx(), p.StatsInfo(), p.QueryBlockOffset(), resultProp)
		limit.SetSchema(p.Schema())
		ret = append(ret, limit)
	}
	return ret, true, nil
}

func exhaustPhysicalPlans4LogicalLock(lp base.LogicalPlan, prop *property.PhysicalProperty) ([]base.PhysicalPlan, bool, error) {
	p := lp.(*logicalop.LogicalLock)
	if prop.IsFlashProp() {
		p.SCtx().GetSessionVars().RaiseWarningWhenMPPEnforced(
			"MPP mode may be blocked because operator `Lock` is not supported now.")
		return nil, true, nil
	}
	childProp := prop.CloneEssentialFields()
	lock := PhysicalLock{
		Lock:               p.Lock,
		TblID2Handle:       p.TblID2Handle,
		TblID2PhysTblIDCol: p.TblID2PhysTblIDCol,
	}.Init(p.SCtx(), p.StatsInfo().ScaleByExpectCnt(prop.ExpectedCnt), childProp)
	return []base.PhysicalPlan{lock}, true, nil
}

func exhaustPhysicalPlans4LogicalUnionAll(lp base.LogicalPlan, prop *property.PhysicalProperty) ([]base.PhysicalPlan, bool, error) {
	p := lp.(*logicalop.LogicalUnionAll)
	// TODO: UnionAll can not pass any order, but we can change it to sort merge to keep order.
	if !prop.IsSortItemEmpty() || (prop.IsFlashProp() && prop.TaskTp != property.MppTaskType) {
		return nil, true, nil
	}
	// TODO: UnionAll can pass partition info, but for briefness, we prevent it from pushing down.
	if prop.TaskTp == property.MppTaskType && prop.MPPPartitionTp != property.AnyType {
		return nil, true, nil
	}
	// when arrived here, operator itself has already checked checkOpSelfSatisfyPropTaskTypeRequirement, we only need to feel allowMPP here.
	canUseMpp := p.SCtx().GetSessionVars().IsMPPAllowed()
	chReqProps := make([]*property.PhysicalProperty, 0, p.ChildLen())
	for range p.Children() {
		if canUseMpp && prop.TaskTp == property.MppTaskType {
			chReqProps = append(chReqProps, &property.PhysicalProperty{
				ExpectedCnt:       prop.ExpectedCnt,
				TaskTp:            property.MppTaskType,
				CTEProducerStatus: prop.CTEProducerStatus,
				NoCopPushDown:     prop.NoCopPushDown,
			})
		} else {
			chReqProps = append(chReqProps, &property.PhysicalProperty{ExpectedCnt: prop.ExpectedCnt,
				CTEProducerStatus: prop.CTEProducerStatus, NoCopPushDown: prop.NoCopPushDown})
		}
	}
	ua := physicalop.PhysicalUnionAll{
		Mpp: canUseMpp && prop.TaskTp == property.MppTaskType,
	}.Init(p.SCtx(), p.StatsInfo().ScaleByExpectCnt(prop.ExpectedCnt), p.QueryBlockOffset(), chReqProps...)
	ua.SetSchema(p.Schema())
	if canUseMpp && prop.TaskTp == property.RootTaskType {
		chReqProps = make([]*property.PhysicalProperty, 0, p.ChildLen())
		for range p.Children() {
			chReqProps = append(chReqProps, &property.PhysicalProperty{
				ExpectedCnt:       prop.ExpectedCnt,
				TaskTp:            property.MppTaskType,
				CTEProducerStatus: prop.CTEProducerStatus,
				NoCopPushDown:     prop.NoCopPushDown,
			})
		}
		mppUA := physicalop.PhysicalUnionAll{Mpp: true}.Init(p.SCtx(), p.StatsInfo().ScaleByExpectCnt(prop.ExpectedCnt), p.QueryBlockOffset(), chReqProps...)
		mppUA.SetSchema(p.Schema())
		return []base.PhysicalPlan{ua, mppUA}, true, nil
	}
	return []base.PhysicalPlan{ua}, true, nil
}

func exhaustPhysicalPlans4LogicalPartitionUnionAll(lp base.LogicalPlan, prop *property.PhysicalProperty) ([]base.PhysicalPlan, bool, error) {
	p := lp.(*logicalop.LogicalPartitionUnionAll)
	uas, flagHint, err := p.LogicalUnionAll.ExhaustPhysicalPlans(prop)
	if err != nil {
		return nil, false, err
	}
	for _, ua := range uas {
		ua.(*physicalop.PhysicalUnionAll).SetTP(plancodec.TypePartitionUnion)
	}
	return uas, flagHint, nil
}

func exhaustPhysicalPlans4LogicalTopN(lp base.LogicalPlan, prop *property.PhysicalProperty) ([]base.PhysicalPlan, bool, error) {
	lt := lp.(*logicalop.LogicalTopN)
	if MatchItems(prop, lt.ByItems) {
		return append(getPhysTopN(lt, prop), getPhysLimits(lt, prop)...), true, nil
	}
	return nil, true, nil
}

func exhaustPhysicalPlans4LogicalSort(lp base.LogicalPlan, prop *property.PhysicalProperty) ([]base.PhysicalPlan, bool, error) {
	ls := lp.(*logicalop.LogicalSort)
	switch prop.TaskTp {
	case property.RootTaskType:
		if MatchItems(prop, ls.ByItems) {
			ret := make([]base.PhysicalPlan, 0, 2)
			ret = append(ret, getPhysicalSort(ls, prop))
			ns := getNominalSort(ls, prop)
			if ns != nil {
				ret = append(ret, ns)
			}
			return ret, true, nil
		}
	case property.MppTaskType:
		// just enumerate mpp task type requirement for child.
		ps := getNominalSortSimple(ls, prop)
		if ps != nil {
			return []base.PhysicalPlan{ps}, true, nil
		}
	default:
		return nil, true, nil
	}
	return nil, true, nil
}

func getPhysicalSort(ls *logicalop.LogicalSort, prop *property.PhysicalProperty) base.PhysicalPlan {
	ps := physicalop.PhysicalSort{ByItems: ls.ByItems}.Init(ls.SCtx(), ls.StatsInfo().ScaleByExpectCnt(prop.ExpectedCnt), ls.QueryBlockOffset(),
		&property.PhysicalProperty{TaskTp: prop.TaskTp, ExpectedCnt: math.MaxFloat64,
			CTEProducerStatus: prop.CTEProducerStatus, NoCopPushDown: prop.NoCopPushDown})
	return ps
}

func getNominalSort(ls *logicalop.LogicalSort, reqProp *property.PhysicalProperty) *physicalop.NominalSort {
	prop, canPass, onlyColumn := GetPropByOrderByItemsContainScalarFunc(ls.ByItems)
	if !canPass {
		return nil
	}
	prop.ExpectedCnt = reqProp.ExpectedCnt
	prop.NoCopPushDown = reqProp.NoCopPushDown
	ps := physicalop.NominalSort{OnlyColumn: onlyColumn, ByItems: ls.ByItems}.Init(
		ls.SCtx(), ls.StatsInfo().ScaleByExpectCnt(prop.ExpectedCnt), ls.QueryBlockOffset(), prop)
	return ps
}

func getNominalSortSimple(ls *logicalop.LogicalSort, reqProp *property.PhysicalProperty) *physicalop.NominalSort {
	prop, canPass, onlyColumn := GetPropByOrderByItemsContainScalarFunc(ls.ByItems)
	if !canPass || !onlyColumn {
		return nil
	}
	newProp := reqProp.CloneEssentialFields()
	newProp.SortItems = prop.SortItems
	ps := physicalop.NominalSort{OnlyColumn: true, ByItems: ls.ByItems}.Init(
		ls.SCtx(), ls.StatsInfo().ScaleByExpectCnt(reqProp.ExpectedCnt), ls.QueryBlockOffset(), newProp)
	return ps
}

func exhaustPhysicalPlans4LogicalMaxOneRow(lp base.LogicalPlan, prop *property.PhysicalProperty) ([]base.PhysicalPlan, bool, error) {
	p := lp.(*logicalop.LogicalMaxOneRow)
	if !prop.IsSortItemEmpty() || prop.IsFlashProp() {
		p.SCtx().GetSessionVars().RaiseWarningWhenMPPEnforced("MPP mode may be blocked because operator `MaxOneRow` is not supported now.")
		return nil, true, nil
	}
	mor := physicalop.PhysicalMaxOneRow{}.Init(p.SCtx(), p.StatsInfo(), p.QueryBlockOffset(), &property.PhysicalProperty{ExpectedCnt: 2, CTEProducerStatus: prop.CTEProducerStatus, NoCopPushDown: prop.NoCopPushDown})
	return []base.PhysicalPlan{mor}, true, nil
}

func exhaustPhysicalPlans4LogicalCTE(lp base.LogicalPlan, prop *property.PhysicalProperty) ([]base.PhysicalPlan, bool, error) {
	p := lp.(*logicalop.LogicalCTE)
	pcte := PhysicalCTE{CTE: p.Cte}.Init(p.SCtx(), p.StatsInfo())
	if prop.IsFlashProp() {
		pcte.storageSender = PhysicalExchangeSender{
			ExchangeType: tipb.ExchangeType_Broadcast,
		}.Init(p.SCtx(), p.StatsInfo())
	}
	pcte.SetSchema(p.Schema())
	pcte.SetChildrenReqProps([]*property.PhysicalProperty{prop.CloneEssentialFields()})
	return []base.PhysicalPlan{(*PhysicalCTEStorage)(pcte)}, true, nil
}

// getGEAndLogicalSequence extracts the possible group expression and logical sequence operator from a common super pointer.
// This function handles two cases:
// 1. When super is already a LogicalSequence - directly return it
// 2. When super is a GroupExpression - extract the wrapped LogicalSequence from it
func getGEAndLogicalSequence(super base.LogicalPlan) (ge *memo.GroupExpression, seq *logicalop.LogicalSequence) {
	switch x := super.(type) {
	case *logicalop.LogicalSequence:
		// Direct LogicalSequence case - no need for downcasting
		seq = x
	case *memo.GroupExpression:
		// GroupExpression case - extract the wrapped LogicalSequence
		ge = x
		seq = ge.GetWrappedLogicalPlan().(*logicalop.LogicalSequence)
	}
	return ge, seq
}

func exhaustPhysicalPlans4LogicalSequence(super base.LogicalPlan, prop *property.PhysicalProperty) ([]base.PhysicalPlan, bool, error) {
	ge, ls := getGEAndLogicalSequence(super)
	possibleChildrenProps := make([][]*property.PhysicalProperty, 0, 2)
	anyType := &property.PhysicalProperty{TaskTp: property.MppTaskType, ExpectedCnt: math.MaxFloat64, MPPPartitionTp: property.AnyType, CanAddEnforcer: true,
		CTEProducerStatus: prop.CTEProducerStatus, NoCopPushDown: prop.NoCopPushDown}
	if prop.TaskTp == property.MppTaskType {
		if prop.CTEProducerStatus == property.SomeCTEFailedMpp {
			return nil, true, nil
		}
		anyType.CTEProducerStatus = property.AllCTECanMpp
		possibleChildrenProps = append(possibleChildrenProps, []*property.PhysicalProperty{anyType, prop.CloneEssentialFields()})
	} else {
		copied := prop.CloneEssentialFields()
		copied.CTEProducerStatus = property.SomeCTEFailedMpp
		possibleChildrenProps = append(possibleChildrenProps, []*property.PhysicalProperty{{TaskTp: property.RootTaskType, ExpectedCnt: math.MaxFloat64, CTEProducerStatus: property.SomeCTEFailedMpp}, copied})
	}

	if prop.TaskTp != property.MppTaskType && prop.CTEProducerStatus != property.SomeCTEFailedMpp &&
		ls.SCtx().GetSessionVars().IsMPPAllowed() && prop.IsSortItemEmpty() {
		possibleChildrenProps = append(possibleChildrenProps, []*property.PhysicalProperty{anyType, anyType.CloneEssentialFields()})
	}
	var seqSchema *expression.Schema
	if ge != nil {
		seqSchema = ge.Inputs[len(ge.Inputs)-1].GetLogicalProperty().Schema
	} else {
		seqSchema = ls.Children()[ls.ChildLen()-1].Schema()
	}
	seqs := make([]base.PhysicalPlan, 0, len(possibleChildrenProps))
	for _, propChoice := range possibleChildrenProps {
		childReqs := make([]*property.PhysicalProperty, 0, ls.ChildLen())
		for range ls.ChildLen() - 1 {
			childReqs = append(childReqs, propChoice[0].CloneEssentialFields())
		}
		childReqs = append(childReqs, propChoice[1])
		seq := PhysicalSequence{}.Init(ls.SCtx(), ls.StatsInfo(), ls.QueryBlockOffset(), childReqs...)
		seq.SetSchema(seqSchema)
		seqs = append(seqs, seq)
	}
	return seqs, true, nil
}
