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
	"sort"
	"unsafe"
)

// byte to insert between elements of lists, fields of structs/maps, etc in order
// to try and make sure the hash is different when values are moved around between
// elements. 36 is arbitrary, but it's the ascii code for a record separator
var recordSeparator []byte = []byte{36}

func CalculateHash(value interface{}) (uint64, error) {
	hasher := fnv.New64()
	ptrs := make(map[uintptr]bool)
	v := reflect.ValueOf(value)
	var err error
	if v.IsValid() {
		err = calculateHashInternal(hasher, v, ptrs)
	}
	return hasher.Sum64(), err
}

func calculateHashInternal(hasher hash.Hash64, v reflect.Value, ptrs map[uintptr]bool) error {
	var int64Array [8]byte
	int64Buf := int64Array[:]
	binary.LittleEndian.PutUint64(int64Buf, uint64(v.Kind()))
	hasher.Write(int64Buf)
	v.IsValid()
	switch v.Kind() {
	case reflect.Struct:
		binary.LittleEndian.PutUint64(int64Buf, uint64(v.NumField()))
		hasher.Write(int64Buf)
		for i := 0; i < v.NumField(); i++ {
			hasher.Write(recordSeparator)
			err := calculateHashInternal(hasher, v.Field(i), ptrs)
			if err != nil {
				return fmt.Errorf("in field %s: %s", v.Type().Field(i).Name, err.Error())
			}
		}
	case reflect.Map:
		binary.LittleEndian.PutUint64(int64Buf, uint64(v.Len()))
		hasher.Write(int64Buf)
		indexes := make([]int, v.Len())
		keys := make([]reflect.Value, v.Len())
		values := make([]reflect.Value, v.Len())
		iter := v.MapRange()
		for i := 0; iter.Next(); i++ {
			indexes[i] = i
			keys[i] = iter.Key()
			values[i] = iter.Value()
		}
		sort.SliceStable(indexes, func(i, j int) bool {
			return compare_values(keys[indexes[i]], keys[indexes[j]]) < 0
		})
		for i := 0; i < v.Len(); i++ {
			hasher.Write(recordSeparator)
			err := calculateHashInternal(hasher, keys[indexes[i]], ptrs)
			if err != nil {
				return fmt.Errorf("in map: %s", err.Error())
			}
			hasher.Write(recordSeparator)
			err = calculateHashInternal(hasher, keys[indexes[i]], ptrs)
			if err != nil {
				return fmt.Errorf("in map: %s", err.Error())
			}
		}
	case reflect.Slice, reflect.Array:
		binary.LittleEndian.PutUint64(int64Buf, uint64(v.Len()))
		hasher.Write(int64Buf)
		for i := 0; i < v.Len(); i++ {
			hasher.Write(recordSeparator)
			err := calculateHashInternal(hasher, v.Index(i), ptrs)
			if err != nil {
				return fmt.Errorf("in %s at index %d: %s", v.Kind().String(), i, err.Error())
			}
		}
	case reflect.Pointer:
		if v.IsNil() {
			int64Buf[0] = 0
			hasher.Write(int64Buf[:1])
			return nil
		}
		// Hardcoded value to indicate it is a pointer
		binary.LittleEndian.PutUint64(int64Buf, uint64(0x55))
		hasher.Write(int64Buf)
		addr := v.Pointer()
		if _, ok := ptrs[addr]; ok {
			// We could make this an error if we want to disallow pointer cycles in the future
			return nil
		}
		ptrs[addr] = true
		err := calculateHashInternal(hasher, v.Elem(), ptrs)
		if err != nil {
			return fmt.Errorf("in pointer: %s", err.Error())
		}
	case reflect.Interface:
		if v.IsNil() {
			int64Buf[0] = 0
			hasher.Write(int64Buf[:1])
		} else {
			// The only way get the pointer out of an interface to hash it or check for cycles
			// would be InterfaceData(), but that's deprecated and seems like it has undefined behavior.
			err := calculateHashInternal(hasher, v.Elem(), ptrs)
			if err != nil {
				return fmt.Errorf("in interface: %s", err.Error())
			}
		}
	case reflect.String:
		strLen := len(v.String())
		if strLen == 0 {
			// unsafe.StringData is unspecified in this case
			int64Buf[0] = 0
			hasher.Write(int64Buf[:1])
			return nil
		}
		hasher.Write(unsafe.Slice(unsafe.StringData(v.String()), strLen))
	case reflect.Bool:
		if v.Bool() {
			int64Buf[0] = 1
		} else {
			int64Buf[0] = 0
		}
		hasher.Write(int64Buf[:1])
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		binary.LittleEndian.PutUint64(int64Buf, v.Uint())
		hasher.Write(int64Buf)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		binary.LittleEndian.PutUint64(int64Buf, uint64(v.Int()))
		hasher.Write(int64Buf)
	case reflect.Float32, reflect.Float64:
		binary.LittleEndian.PutUint64(int64Buf, math.Float64bits(v.Float()))
		hasher.Write(int64Buf)
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
