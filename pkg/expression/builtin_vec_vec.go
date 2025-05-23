// Copyright 2024 PingCAP, Inc.
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

// Code generated by go generate in expression/generator; DO NOT EDIT.

package expression

import (
	"math"

	"github.com/pingcap/tidb/pkg/types"
	"github.com/pingcap/tidb/pkg/util/chunk"
)

func (b *builtinVecDimsSig) vectorized() bool {
	return true
}

func (b *builtinVecDimsSig) vecEvalInt(ctx EvalContext, input *chunk.Chunk, result *chunk.Column) error {
	n := input.NumRows()
	buf, err := b.bufAllocator.get()
	if err != nil {
		return err
	}
	defer b.bufAllocator.put(buf)
	if err := b.args[0].VecEvalVectorFloat32(ctx, input, buf); err != nil {
		return err
	}
	result.ResizeInt64(n, false)
	result.MergeNulls(buf)
	res := result.Int64s()
	for i := range n {
		if result.IsNull(i) {
			continue
		}
		length := buf.GetVectorFloat32(i).Len()
		res[i] = int64(length)
	}
	return nil
}

func (b *builtinVecL1DistanceSig) vectorized() bool {
	return true
}

func (b *builtinVecL1DistanceSig) vecEvalReal(ctx EvalContext, input *chunk.Chunk, result *chunk.Column) error {
	n := input.NumRows()
	col1, err := b.bufAllocator.get()
	if err != nil {
		return err
	}
	defer b.bufAllocator.put(col1)
	if err := b.args[0].VecEvalVectorFloat32(ctx, input, col1); err != nil {
		return err
	}

	col2, err := b.bufAllocator.get()
	if err != nil {
		return err
	}
	defer b.bufAllocator.put(col2)
	if err := b.args[1].VecEvalVectorFloat32(ctx, input, col2); err != nil {
		return err
	}
	result.ResizeFloat64(n, false)
	result.MergeNulls(col1, col2)
	res := result.Float64s()
	for i := range n {
		if result.IsNull(i) {
			continue
		}
		x := col1.GetVectorFloat32(i)
		y := col2.GetVectorFloat32(i)
		d, err := x.L1Distance(y)
		if err != nil {
			return err
		}
		if math.IsNaN(d) {
			result.SetNull(i, true)
			continue
		}
		res[i] = d
	}
	return nil
}

func (b *builtinVecL2DistanceSig) vectorized() bool {
	return true
}

func (b *builtinVecL2DistanceSig) vecEvalReal(ctx EvalContext, input *chunk.Chunk, result *chunk.Column) error {
	n := input.NumRows()
	col1, err := b.bufAllocator.get()
	if err != nil {
		return err
	}
	defer b.bufAllocator.put(col1)
	if err := b.args[0].VecEvalVectorFloat32(ctx, input, col1); err != nil {
		return err
	}

	col2, err := b.bufAllocator.get()
	if err != nil {
		return err
	}
	defer b.bufAllocator.put(col2)
	if err := b.args[1].VecEvalVectorFloat32(ctx, input, col2); err != nil {
		return err
	}
	result.ResizeFloat64(n, false)
	result.MergeNulls(col1, col2)
	res := result.Float64s()
	for i := range n {
		if result.IsNull(i) {
			continue
		}
		x := col1.GetVectorFloat32(i)
		y := col2.GetVectorFloat32(i)
		d, err := x.L2Distance(y)
		if err != nil {
			return err
		}
		if math.IsNaN(d) {
			result.SetNull(i, true)
			continue
		}
		res[i] = d
	}
	return nil
}

func (b *builtinVecNegativeInnerProductSig) vectorized() bool {
	return true
}

func (b *builtinVecNegativeInnerProductSig) vecEvalReal(ctx EvalContext, input *chunk.Chunk, result *chunk.Column) error {
	n := input.NumRows()
	col1, err := b.bufAllocator.get()
	if err != nil {
		return err
	}
	defer b.bufAllocator.put(col1)
	if err := b.args[0].VecEvalVectorFloat32(ctx, input, col1); err != nil {
		return err
	}

	col2, err := b.bufAllocator.get()
	if err != nil {
		return err
	}
	defer b.bufAllocator.put(col2)
	if err := b.args[1].VecEvalVectorFloat32(ctx, input, col2); err != nil {
		return err
	}
	result.ResizeFloat64(n, false)
	result.MergeNulls(col1, col2)
	res := result.Float64s()
	for i := range n {
		if result.IsNull(i) {
			continue
		}
		x := col1.GetVectorFloat32(i)
		y := col2.GetVectorFloat32(i)
		d, err := x.NegativeInnerProduct(y)
		if err != nil {
			return err
		}
		if math.IsNaN(d) {
			result.SetNull(i, true)
			continue
		}
		res[i] = d
	}
	return nil
}

func (b *builtinVecCosineDistanceSig) vectorized() bool {
	return true
}

func (b *builtinVecCosineDistanceSig) vecEvalReal(ctx EvalContext, input *chunk.Chunk, result *chunk.Column) error {
	n := input.NumRows()
	col1, err := b.bufAllocator.get()
	if err != nil {
		return err
	}
	defer b.bufAllocator.put(col1)
	if err := b.args[0].VecEvalVectorFloat32(ctx, input, col1); err != nil {
		return err
	}

	col2, err := b.bufAllocator.get()
	if err != nil {
		return err
	}
	defer b.bufAllocator.put(col2)
	if err := b.args[1].VecEvalVectorFloat32(ctx, input, col2); err != nil {
		return err
	}
	result.ResizeFloat64(n, false)
	result.MergeNulls(col1, col2)
	res := result.Float64s()
	for i := range n {
		if result.IsNull(i) {
			continue
		}
		x := col1.GetVectorFloat32(i)
		y := col2.GetVectorFloat32(i)
		d, err := x.CosineDistance(y)
		if err != nil {
			return err
		}
		if math.IsNaN(d) {
			result.SetNull(i, true)
			continue
		}
		res[i] = d
	}
	return nil
}

func (b *builtinVecL2NormSig) vectorized() bool {
	return true
}

func (b *builtinVecL2NormSig) vecEvalReal(ctx EvalContext, input *chunk.Chunk, result *chunk.Column) error {
	n := input.NumRows()
	col1, err := b.bufAllocator.get()
	if err != nil {
		return err
	}
	defer b.bufAllocator.put(col1)
	if err := b.args[0].VecEvalVectorFloat32(ctx, input, col1); err != nil {
		return err
	}
	result.ResizeFloat64(n, false)
	result.MergeNulls(col1)
	res := result.Float64s()
	for i := range n {
		if result.IsNull(i) {
			continue
		}
		v := col1.GetVectorFloat32(i)
		d := v.L2Norm()
		if math.IsNaN(d) {
			result.SetNull(i, true)
			continue
		}
		res[i] = d
	}
	return nil
}

func (b *builtinVecFromTextSig) vectorized() bool {
	return true
}

func (b *builtinVecFromTextSig) vecEvalVectorFloat32(ctx EvalContext, input *chunk.Chunk, result *chunk.Column) error {
	n := input.NumRows()
	buf, err := b.bufAllocator.get()
	if err != nil {
		return err
	}
	defer b.bufAllocator.put(buf)
	if err := b.args[0].VecEvalString(ctx, input, buf); err != nil {
		return err
	}
	result.ReserveVectorFloat32(n)
	for i := range n {
		if buf.IsNull(i) {
			result.AppendNull()
			continue
		}
		v := buf.GetString(i)
		vec, err := types.ParseVectorFloat32(v)
		if err != nil {
			return err
		}
		if err = vec.CheckDimsFitColumn(b.tp.GetFlen()); err != nil {
			return err
		}
		result.AppendVectorFloat32(vec)
	}
	return nil
}

func (b *builtinVecAsTextSig) vectorized() bool {
	return true
}

func (b *builtinVecAsTextSig) vecEvalString(ctx EvalContext, input *chunk.Chunk, result *chunk.Column) error {
	n := input.NumRows()
	buf, err := b.bufAllocator.get()
	if err != nil {
		return err
	}
	defer b.bufAllocator.put(buf)
	if err := b.args[0].VecEvalVectorFloat32(ctx, input, buf); err != nil {
		return err
	}
	result.ReserveString(n)
	for i := range n {
		if buf.IsNull(i) {
			result.AppendNull()
			continue
		}
		vec := buf.GetVectorFloat32(i)
		result.AppendString(vec.String())
	}
	return nil
}
