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

package logclient

// each 64 items constitute a bitmap unit
type bitMap map[int]uint64

func newBitMap() bitMap {
	return make(map[int]uint64)
}

func (m bitMap) pos(off int) (blockIndex int, bitOffset uint64) {
	return off >> 6, uint64(1) << (off & 63)
}

func (m bitMap) Set(off int) {
	blockIndex, bitOffset := m.pos(off)
	m[blockIndex] |= bitOffset
}

func (m bitMap) Hit(off int) bool {
	blockIndex, bitOffset := m.pos(off)
	return (m[blockIndex] & bitOffset) > 0
}

type bitMapExt struct {
	bitMap
	skip bool
}

func newBitMapExt(skip bool) bitMapExt {
	return bitMapExt{
		bitMap: newBitMap(),
		skip:   skip,
	}
}

type fileMap struct {
	// group index -> bitmap of kv files
	pos map[int]bitMap
}

func newFileMap() fileMap {
	return fileMap{
		pos: make(map[int]bitMap),
	}
}

type fileMapExt struct {
	// group index -> bitmap of kv files
	pos  map[int]bitMapExt
	skip bool
}

func newFileMapExt(skip bool) fileMapExt {
	return fileMapExt{
		pos:  make(map[int]bitMapExt),
		skip: skip,
	}
}

type LogFilesSkipMap struct {
	// metadata group key -> group map
	skipMap map[string]fileMap
}

func NewLogFilesSkipMap() *LogFilesSkipMap {
	return &LogFilesSkipMap{
		skipMap: make(map[string]fileMap),
	}
}

func (m *LogFilesSkipMap) Insert(metaKey string, groupOff, fileOff int) {
	mp, exists := m.skipMap[metaKey]
	if !exists {
		mp = newFileMap()
		m.skipMap[metaKey] = mp
	}
	gp, exists := mp.pos[groupOff]
	if !exists {
		gp = newBitMap()
		mp.pos[groupOff] = gp
	}
	gp.Set(fileOff)
}

func (m *LogFilesSkipMap) NeedSkip(metaKey string, groupOff, fileOff int) bool {
	mp, exists := m.skipMap[metaKey]
	if !exists {
		return false
	}
	gp, exists := mp.pos[groupOff]
	if !exists {
		return false
	}
	return gp.Hit(fileOff)
}

type LogFilesSkipMapExt struct {
	// metadata group key -> group map
	skipMap map[string]fileMapExt
}

func NewLogFilesSkipMapExt() *LogFilesSkipMapExt {
	return &LogFilesSkipMapExt{
		skipMap: make(map[string]fileMapExt),
	}
}

func (m *LogFilesSkipMapExt) Insert(metaKey string, groupOff, fileOff int) {
	mp, exists := m.skipMap[metaKey]
	if !exists {
		mp = newFileMapExt(false)
		m.skipMap[metaKey] = mp
	}
	if mp.skip {
		return
	}
	gp, exists := mp.pos[groupOff]
	if !exists {
		gp = newBitMapExt(false)
		mp.pos[groupOff] = gp
	}
	if gp.skip {
		return
	}
	gp.Set(fileOff)
}

func (m *LogFilesSkipMapExt) SkipMeta(metaKey string) {
	m.skipMap[metaKey] = newFileMapExt(true)
}

func (m *LogFilesSkipMapExt) SkipGroup(metaKey string, groupOff int) {
	mp, exists := m.skipMap[metaKey]
	if !exists {
		mp = newFileMapExt(false)
		m.skipMap[metaKey] = mp
	}
	if mp.skip {
		return
	}
	mp.pos[groupOff] = newBitMapExt(true)
}

func (m *LogFilesSkipMapExt) NeedSkip(metaKey string, groupOff, fileOff int) bool {
	mp, exists := m.skipMap[metaKey]
	if !exists {
		return false
	}
	if mp.skip {
		return true
	}
	gp, exists := mp.pos[groupOff]
	if !exists {
		return false
	}
	if gp.skip {
		return true
	}
	return gp.Hit(fileOff)
}
