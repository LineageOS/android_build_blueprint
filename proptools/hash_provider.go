// Copyright 2023 Google Inc. All rights reserved.
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

package proptools

import (
	"cmp"
	"encoding/binary"
	"fmt"
	"hash"
	"hash/fnv"
	"math"
	"reflect"
	"slices"
	"unsafe"
)

// byte to insert between elements of lists, fields of structs/maps, etc in order
// to try and make sure the hash is different when values are moved around between
// elements. 36 is arbitrary, but it's the ascii code for a record separator
var recordSeparator []byte = []byte{36}

func CalculateHash(value interface{}) (uint64, error) {
	hasher := hasher{
		Hash64:   fnv.New64(),
		int64Buf: make([]byte, 8),
	}
	v := reflect.ValueOf(value)
	var err error
	if v.IsValid() {
		err = hasher.calculateHash(v)
	}
	return hasher.Sum64(), err
}

type hasher struct {
	hash.Hash64
	int64Buf      []byte
	ptrs          map[uintptr]bool
	mapStateCache *mapState
}

type mapState struct {
	indexes []int
	keys    []reflect.Value
	values  []reflect.Value
}

func (hasher *hasher) writeUint64(i uint64) {
	binary.LittleEndian.PutUint64(hasher.int64Buf, i)
	hasher.Write(hasher.int64Buf)
}

func (hasher *hasher) writeByte(i byte) {
	hasher.int64Buf[0] = i
	hasher.Write(hasher.int64Buf[:1])
}

func (hasher *hasher) getMapState(size int) *mapState {
	s := hasher.mapStateCache
	// Clear hasher.mapStateCache so that any recursive uses don't collide with this frame.
	hasher.mapStateCache = nil

	if s == nil {
		s = &mapState{}
	}

	// Reset the slices to length `size` and capacity at least `size`
	s.indexes = slices.Grow(s.indexes[:0], size)[0:size]
	s.keys = slices.Grow(s.keys[:0], size)[0:size]
	s.values = slices.Grow(s.values[:0], size)[0:size]

	return s
}

func (hasher *hasher) putMapState(s *mapState) {
	if hasher.mapStateCache == nil || cap(hasher.mapStateCache.indexes) < cap(s.indexes) {
		hasher.mapStateCache = s
	}
}

func (hasher *hasher) calculateHash(v reflect.Value) error {
	hasher.writeUint64(uint64(v.Kind()))
	v.IsValid()
	switch v.Kind() {
	case reflect.Struct:
		hasher.writeUint64(uint64(v.NumField()))
		for i := 0; i < v.NumField(); i++ {
			hasher.Write(recordSeparator)
			err := hasher.calculateHash(v.Field(i))
			if err != nil {
				return fmt.Errorf("in field %s: %s", v.Type().Field(i).Name, err.Error())
			}
		}
	case reflect.Map:
		hasher.writeUint64(uint64(v.Len()))
		iter := v.MapRange()
		s := hasher.getMapState(v.Len())
		for i := 0; iter.Next(); i++ {
			s.indexes[i] = i
			s.keys[i] = iter.Key()
			s.values[i] = iter.Value()
		}
		slices.SortFunc(s.indexes, func(i, j int) int {
			return compare_values(s.keys[i], s.keys[j])
		})
		for i := 0; i < v.Len(); i++ {
			hasher.Write(recordSeparator)
			err := hasher.calculateHash(s.keys[s.indexes[i]])
			if err != nil {
				return fmt.Errorf("in map: %s", err.Error())
			}
			hasher.Write(recordSeparator)
			err = hasher.calculateHash(s.keys[s.indexes[i]])
			if err != nil {
				return fmt.Errorf("in map: %s", err.Error())
			}
		}
		hasher.putMapState(s)
	case reflect.Slice, reflect.Array:
		hasher.writeUint64(uint64(v.Len()))
		for i := 0; i < v.Len(); i++ {
			hasher.Write(recordSeparator)
			err := hasher.calculateHash(v.Index(i))
			if err != nil {
				return fmt.Errorf("in %s at index %d: %s", v.Kind().String(), i, err.Error())
			}
		}
	case reflect.Pointer:
		if v.IsNil() {
			hasher.writeByte(0)
			return nil
		}
		// Hardcoded value to indicate it is a pointer
		hasher.writeUint64(uint64(0x55))
		addr := v.Pointer()
		if hasher.ptrs == nil {
			hasher.ptrs = make(map[uintptr]bool)
		}
		if _, ok := hasher.ptrs[addr]; ok {
			// We could make this an error if we want to disallow pointer cycles in the future
			return nil
		}
		hasher.ptrs[addr] = true
		err := hasher.calculateHash(v.Elem())
		if err != nil {
			return fmt.Errorf("in pointer: %s", err.Error())
		}
	case reflect.Interface:
		if v.IsNil() {
			hasher.writeByte(0)
		} else {
			// The only way get the pointer out of an interface to hash it or check for cycles
			// would be InterfaceData(), but that's deprecated and seems like it has undefined behavior.
			err := hasher.calculateHash(v.Elem())
			if err != nil {
				return fmt.Errorf("in interface: %s", err.Error())
			}
		}
	case reflect.String:
		strLen := len(v.String())
		if strLen == 0 {
			// unsafe.StringData is unspecified in this case
			hasher.writeByte(0)
			return nil
		}
		hasher.Write(unsafe.Slice(unsafe.StringData(v.String()), strLen))
	case reflect.Bool:
		if v.Bool() {
			hasher.writeByte(1)
		} else {
			hasher.writeByte(0)
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		hasher.writeUint64(v.Uint())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		hasher.writeUint64(uint64(v.Int()))
	case reflect.Float32, reflect.Float64:
		hasher.writeUint64(math.Float64bits(v.Float()))
	default:
		return fmt.Errorf("data may only contain primitives, strings, arrays, slices, structs, maps, and pointers, found: %s", v.Kind().String())
	}
	return nil
}

func compare_values(x, y reflect.Value) int {
	if x.Type() != y.Type() {
		panic("Expected equal types")
	}

	switch x.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return cmp.Compare(x.Uint(), y.Uint())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return cmp.Compare(x.Int(), y.Int())
	case reflect.Float32, reflect.Float64:
		return cmp.Compare(x.Float(), y.Float())
	case reflect.String:
		return cmp.Compare(x.String(), y.String())
	case reflect.Bool:
		if x.Bool() == y.Bool() {
			return 0
		} else if x.Bool() {
			return 1
		} else {
			return -1
		}
	case reflect.Pointer:
		return cmp.Compare(x.Pointer(), y.Pointer())
	case reflect.Array:
		for i := 0; i < x.Len(); i++ {
			if result := compare_values(x.Index(i), y.Index(i)); result != 0 {
				return result
			}
		}
		return 0
	case reflect.Struct:
		for i := 0; i < x.NumField(); i++ {
			if result := compare_values(x.Field(i), y.Field(i)); result != 0 {
				return result
			}
		}
		return 0
	case reflect.Interface:
		if x.IsNil() && y.IsNil() {
			return 0
		} else if x.IsNil() {
			return 1
		} else if y.IsNil() {
			return -1
		}
		return compare_values(x.Elem(), y.Elem())
	default:
		panic(fmt.Sprintf("Could not compare types %s and %s", x.Type().String(), y.Type().String()))
	}
}
