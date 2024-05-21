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

package optional

// ShallowOptional is an optional type that can be constructed from a pointer.
// It will not copy the pointer, and its size is the same size as a single pointer.
// It can be used to prevent a downstream consumer from modifying the value through
// the pointer, but is not suitable for an Optional type with stronger value semantics
// like you would expect from C++ or Rust Optionals.
type ShallowOptional[T any] struct {
	inner *T
}

// NewShallowOptional creates a new ShallowOptional from a pointer.
// The pointer will not be copied, the object could be changed by the calling
// code after the optional was created.
func NewShallowOptional[T any](inner *T) ShallowOptional[T] {
	return ShallowOptional[T]{inner: inner}
}

// IsPresent returns true if the optional contains a value
func (o *ShallowOptional[T]) IsPresent() bool {
	return o.inner != nil
}

// IsEmpty returns true if the optional does not have a value
func (o *ShallowOptional[T]) IsEmpty() bool {
	return o.inner == nil
}

// Get() returns the value inside the optional. It panics if IsEmpty() returns true
func (o *ShallowOptional[T]) Get() T {
	if o.inner == nil {
		panic("tried to get an empty optional")
	}
	return *o.inner
}

// GetOrDefault() returns the value inside the optional if IsPresent() returns true,
// or the provided value otherwise.
func (o *ShallowOptional[T]) GetOrDefault(other T) T {
	if o.inner == nil {
		return other
	}
	return *o.inner
}
