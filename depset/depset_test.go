// Copyright 2020 Google Inc. All rights reserved.
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

package depset

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func ExampleDepSet_ToList_postordered() {
	a := NewBuilder[string](POSTORDER).Direct("a").Build()
	b := NewBuilder[string](POSTORDER).Direct("b").Transitive(a).Build()
	c := NewBuilder[string](POSTORDER).Direct("c").Transitive(a).Build()
	d := NewBuilder[string](POSTORDER).Direct("d").Transitive(b, c).Build()

	fmt.Println(d.ToList())
	// Output: [a b c d]
}

func ExampleDepSet_ToList_preordered() {
	a := NewBuilder[string](PREORDER).Direct("a").Build()
	b := NewBuilder[string](PREORDER).Direct("b").Transitive(a).Build()
	c := NewBuilder[string](PREORDER).Direct("c").Transitive(a).Build()
	d := NewBuilder[string](PREORDER).Direct("d").Transitive(b, c).Build()

	fmt.Println(d.ToList())
	// Output: [d b a c]
}

func ExampleDepSet_ToList_topological() {
	a := NewBuilder[string](TOPOLOGICAL).Direct("a").Build()
	b := NewBuilder[string](TOPOLOGICAL).Direct("b").Transitive(a).Build()
	c := NewBuilder[string](TOPOLOGICAL).Direct("c").Transitive(a).Build()
	d := NewBuilder[string](TOPOLOGICAL).Direct("d").Transitive(b, c).Build()

	fmt.Println(d.ToList())
	// Output: [d b c a]
}

// Tests based on Bazel's ExpanderTestBase.java to ensure compatibility
// https://github.com/bazelbuild/bazel/blob/master/src/test/java/com/google/devtools/build/lib/collect/nestedset/ExpanderTestBase.java
func TestDepSet(t *testing.T) {
	tests := []struct {
		name                             string
		depSet                           func(t *testing.T, order Order) DepSet[string]
		postorder, preorder, topological []string
	}{
		{
			name: "simple",
			depSet: func(t *testing.T, order Order) DepSet[string] {
				return New[string](order, []string{"c", "a", "b"}, nil)
			},
			postorder:   []string{"c", "a", "b"},
			preorder:    []string{"c", "a", "b"},
			topological: []string{"c", "a", "b"},
		},
		{
			name: "simpleNoDuplicates",
			depSet: func(t *testing.T, order Order) DepSet[string] {
				return New[string](order, []string{"c", "a", "a", "a", "b"}, nil)
			},
			postorder:   []string{"c", "a", "b"},
			preorder:    []string{"c", "a", "b"},
			topological: []string{"c", "a", "b"},
		},
		{
			name: "nesting",
			depSet: func(t *testing.T, order Order) DepSet[string] {
				subset := New[string](order, []string{"c", "a", "e"}, nil)
				return New[string](order, []string{"b", "d"}, []DepSet[string]{subset})
			},
			postorder:   []string{"c", "a", "e", "b", "d"},
			preorder:    []string{"b", "d", "c", "a", "e"},
			topological: []string{"b", "d", "c", "a", "e"},
		},
		{
			name: "builderReuse",
			depSet: func(t *testing.T, order Order) DepSet[string] {
				assertEquals := func(t *testing.T, w, g []string) {
					t.Helper()
					if !reflect.DeepEqual(w, g) {
						t.Errorf("want %q, got %q", w, g)
					}
				}
				builder := NewBuilder[string](order)
				assertEquals(t, nil, builder.Build().ToList())

				builder.Direct("b")
				assertEquals(t, []string{"b"}, builder.Build().ToList())

				builder.Direct("d")
				assertEquals(t, []string{"b", "d"}, builder.Build().ToList())

				child := NewBuilder[string](order).Direct("c", "a", "e").Build()
				builder.Transitive(child)
				return builder.Build()
			},
			postorder:   []string{"c", "a", "e", "b", "d"},
			preorder:    []string{"b", "d", "c", "a", "e"},
			topological: []string{"b", "d", "c", "a", "e"},
		},
		{
			name: "builderChaining",
			depSet: func(t *testing.T, order Order) DepSet[string] {
				return NewBuilder[string](order).Direct("b").Direct("d").
					Transitive(NewBuilder[string](order).Direct("c", "a", "e").Build()).Build()
			},
			postorder:   []string{"c", "a", "e", "b", "d"},
			preorder:    []string{"b", "d", "c", "a", "e"},
			topological: []string{"b", "d", "c", "a", "e"},
		},
		{
			name: "transitiveDepsHandledSeparately",
			depSet: func(t *testing.T, order Order) DepSet[string] {
				subset := NewBuilder[string](order).Direct("c", "a", "e").Build()
				builder := NewBuilder[string](order)
				// The fact that we add the transitive subset between the Direct(b) and Direct(d)
				// calls should not change the result.
				builder.Direct("b")
				builder.Transitive(subset)
				builder.Direct("d")
				return builder.Build()
			},
			postorder:   []string{"c", "a", "e", "b", "d"},
			preorder:    []string{"b", "d", "c", "a", "e"},
			topological: []string{"b", "d", "c", "a", "e"},
		},
		{
			name: "nestingNoDuplicates",
			depSet: func(t *testing.T, order Order) DepSet[string] {
				subset := NewBuilder[string](order).Direct("c", "a", "e").Build()
				return NewBuilder[string](order).Direct("b", "d", "e").Transitive(subset).Build()
			},
			postorder:   []string{"c", "a", "e", "b", "d"},
			preorder:    []string{"b", "d", "e", "c", "a"},
			topological: []string{"b", "d", "c", "a", "e"},
		},
		{
			name: "chain",
			depSet: func(t *testing.T, order Order) DepSet[string] {
				c := NewBuilder[string](order).Direct("c").Build()
				b := NewBuilder[string](order).Direct("b").Transitive(c).Build()
				a := NewBuilder[string](order).Direct("a").Transitive(b).Build()

				return a
			},
			postorder:   []string{"c", "b", "a"},
			preorder:    []string{"a", "b", "c"},
			topological: []string{"a", "b", "c"},
		},
		{
			name: "diamond",
			depSet: func(t *testing.T, order Order) DepSet[string] {
				d := NewBuilder[string](order).Direct("d").Build()
				c := NewBuilder[string](order).Direct("c").Transitive(d).Build()
				b := NewBuilder[string](order).Direct("b").Transitive(d).Build()
				a := NewBuilder[string](order).Direct("a").Transitive(b).Transitive(c).Build()

				return a
			},
			postorder:   []string{"d", "b", "c", "a"},
			preorder:    []string{"a", "b", "d", "c"},
			topological: []string{"a", "b", "c", "d"},
		},
		{
			name: "extendedDiamond",
			depSet: func(t *testing.T, order Order) DepSet[string] {
				d := NewBuilder[string](order).Direct("d").Build()
				e := NewBuilder[string](order).Direct("e").Build()
				b := NewBuilder[string](order).Direct("b").Transitive(d).Transitive(e).Build()
				c := NewBuilder[string](order).Direct("c").Transitive(e).Transitive(d).Build()
				a := NewBuilder[string](order).Direct("a").Transitive(b).Transitive(c).Build()
				return a
			},
			postorder:   []string{"d", "e", "b", "c", "a"},
			preorder:    []string{"a", "b", "d", "e", "c"},
			topological: []string{"a", "b", "c", "e", "d"},
		},
		{
			name: "extendedDiamondRightArm",
			depSet: func(t *testing.T, order Order) DepSet[string] {
				d := NewBuilder[string](order).Direct("d").Build()
				e := NewBuilder[string](order).Direct("e").Build()
				b := NewBuilder[string](order).Direct("b").Transitive(d).Transitive(e).Build()
				c2 := NewBuilder[string](order).Direct("c2").Transitive(e).Transitive(d).Build()
				c := NewBuilder[string](order).Direct("c").Transitive(c2).Build()
				a := NewBuilder[string](order).Direct("a").Transitive(b).Transitive(c).Build()
				return a
			},
			postorder:   []string{"d", "e", "b", "c2", "c", "a"},
			preorder:    []string{"a", "b", "d", "e", "c", "c2"},
			topological: []string{"a", "b", "c", "c2", "e", "d"},
		},
		{
			name: "orderConflict",
			depSet: func(t *testing.T, order Order) DepSet[string] {
				child1 := NewBuilder[string](order).Direct("a", "b").Build()
				child2 := NewBuilder[string](order).Direct("b", "a").Build()
				parent := NewBuilder[string](order).Transitive(child1).Transitive(child2).Build()
				return parent
			},
			postorder:   []string{"a", "b"},
			preorder:    []string{"a", "b"},
			topological: []string{"b", "a"},
		},
		{
			name: "orderConflictNested",
			depSet: func(t *testing.T, order Order) DepSet[string] {
				a := NewBuilder[string](order).Direct("a").Build()
				b := NewBuilder[string](order).Direct("b").Build()
				child1 := NewBuilder[string](order).Transitive(a).Transitive(b).Build()
				child2 := NewBuilder[string](order).Transitive(b).Transitive(a).Build()
				parent := NewBuilder[string](order).Transitive(child1).Transitive(child2).Build()
				return parent
			},
			postorder:   []string{"a", "b"},
			preorder:    []string{"a", "b"},
			topological: []string{"b", "a"},
		},
		{
			name: "zeroDepSet",
			depSet: func(t *testing.T, order Order) DepSet[string] {
				a := NewBuilder[string](order).Build()
				var b DepSet[string]
				c := NewBuilder[string](order).Direct("c").Transitive(a, b).Build()
				return c
			},
			postorder:   []string{"c"},
			preorder:    []string{"c"},
			topological: []string{"c"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Run("postorder", func(t *testing.T) {
				depSet := tt.depSet(t, POSTORDER)
				if g, w := depSet.ToList(), tt.postorder; !slices.Equal(g, w) {
					t.Errorf("expected ToList() = %q, got %q", w, g)
				}
			})
			t.Run("preorder", func(t *testing.T) {
				depSet := tt.depSet(t, PREORDER)
				if g, w := depSet.ToList(), tt.preorder; !slices.Equal(g, w) {
					t.Errorf("expected ToList() = %q, got %q", w, g)
				}
			})
			t.Run("topological", func(t *testing.T) {
				depSet := tt.depSet(t, TOPOLOGICAL)
				if g, w := depSet.ToList(), tt.topological; !slices.Equal(g, w) {
					t.Errorf("expected ToList() = %q, got %q", w, g)
				}
			})
		})
	}
}

func TestDepSetInvalidOrder(t *testing.T) {
	orders := []Order{POSTORDER, PREORDER, TOPOLOGICAL}

	run := func(t *testing.T, order1, order2 Order) {
		defer func() {
			if r := recover(); r != nil {
				if err, ok := r.(error); !ok {
					t.Fatalf("expected panic error, got %v", err)
				} else if !strings.Contains(err.Error(), "incompatible order") {
					t.Fatalf("expected incompatible order error, got %v", err)
				}
			}
		}()
		New(order1, nil, []DepSet[string]{New[string](order2, []string{"a"}, nil)})
		t.Fatal("expected panic")
	}

	for _, order1 := range orders {
		t.Run(order1.String(), func(t *testing.T) {
			for _, order2 := range orders {
				t.Run(order2.String(), func(t *testing.T) {
					if order1 != order2 {
						run(t, order1, order2)
					}
				})
			}
		})
	}
}
