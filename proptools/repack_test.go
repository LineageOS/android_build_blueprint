// Copyright 2024 Google Inc. All rights reserved.
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
	"regexp"
	"strings"
	"testing"

	"github.com/google/blueprint/parser"
)

type testSymlinkStruct struct {
	Target string
	Name   string
}

type testPropStructNested struct {
	My_string_ptr *string
}

type testPropStruct struct {
	My_string                   string
	My_configurable_string      Configurable[string]
	My_configurable_string_list Configurable[[]string]
	My_string_ptr               *string
	My_string_list              []string
	My_struct_list              []testSymlinkStruct
	My_bool                     bool
	My_int                      int
	My_int64                    int64
	Nested                      testPropStructNested
}

type testPropStructOnlyConfigurableStringList struct {
	My_configurable_string_list Configurable[[]string]
}

func TestRepack(t *testing.T) {
	testCases := []struct {
		name        string
		propStructs []interface{}
		expectedBp  string
		expectedErr string
	}{
		{
			name: "Simple prop struct",
			propStructs: []interface{}{&testPropStruct{
				My_string:                   "foo",
				My_configurable_string:      NewSimpleConfigurable("qux"),
				My_configurable_string_list: NewSimpleConfigurable([]string{"a", "b", "c"}),
				My_string_ptr:               StringPtr("bar"),
				My_string_list:              []string{"foo", "bar"},
				My_struct_list: []testSymlinkStruct{
					{Name: "foo", Target: "foo_target"},
					{Name: "bar", Target: "bar_target"},
				},
				My_bool:  true,
				My_int:   5,
				My_int64: 64,
				Nested: testPropStructNested{
					My_string_ptr: StringPtr("baz"),
				},
			}},
			expectedBp: `
module {
    my_string: "foo",
    my_configurable_string: "qux",
    my_configurable_string_list: [
        "a",
        "b",
        "c",
    ],
    my_string_ptr: "bar",
    my_string_list: [
        "foo",
        "bar",
    ],
    my_struct_list: [
        {
            Target: "foo_target",
            Name: "foo",
        },
        {
            Target: "bar_target",
            Name: "bar",
        },
    ],
    my_bool: true,
    my_int: 5,
    my_int64: 64,
    nested: {
        my_string_ptr: "baz",
    },
}`,
		},
		{
			name: "Complicated select",
			propStructs: []interface{}{&testPropStructOnlyConfigurableStringList{
				My_configurable_string_list: createComplicatedSelect(),
			}},
			expectedBp: `
module {
    my_configurable_string_list: ["a"] + select((os(), arch()), {
        ("android", "x86"): [
            "android",
            "x86",
        ],
        ("android", "arm64"): [
            "android",
            "arm64",
        ],
        (default, "x86"): [
            "default",
            "x86",
        ],
        (default, default): [
            "default",
            "default",
        ],
    }) + ["b"],
}`,
		},
		{
			name: "Multiple property structs",
			propStructs: []interface{}{
				&testPropStruct{
					My_string:      "foo",
					My_string_ptr:  nil,
					My_string_list: []string{"foo", "bar"},
					My_bool:        true,
					My_int:         5,
				},
				&testPropStructNested{
					My_string_ptr: StringPtr("bar"),
				},
			},
			expectedBp: `
module {
    my_string: "foo",
    my_string_ptr: "bar",
    my_string_list: [
        "foo",
        "bar",
    ],
    my_struct_list: [],
    my_bool: true,
    my_int: 5,
    my_int64: 0,
}`,
		},
		{
			name: "Multiple conflicting property structs",
			propStructs: []interface{}{
				&testPropStruct{
					My_string:      "foo",
					My_string_ptr:  StringPtr("foo"),
					My_string_list: []string{"foo", "bar"},
					My_bool:        true,
					My_int:         5,
				},
				&testPropStructNested{
					My_string_ptr: StringPtr("bar"),
				},
			},
			expectedErr: `Conflicting fields in property structs had values "foo" and "bar"`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := RepackProperties(tc.propStructs)
			if err != nil {
				if tc.expectedErr != "" {
					match, err2 := regexp.MatchString(tc.expectedErr, err.Error())
					if err2 != nil {
						t.Fatal(err2)
					}
					if !match {
						t.Fatalf("Expected error matching %q, found %q", tc.expectedErr, err.Error())
					}
					return
				} else {
					t.Fatal(err)
				}
			} else if tc.expectedErr != "" {
				t.Fatalf("Expected error matching %q, but got success", tc.expectedErr)
			}
			file := &parser.File{
				Defs: []parser.Definition{
					&parser.Module{
						Type: "module",
						Map:  *result,
					},
				},
			}
			bytes, err := parser.Print(file)
			if err != nil {
				t.Fatal(err)
			}
			expected := strings.TrimSpace(tc.expectedBp)
			actual := strings.TrimSpace(string(bytes))
			if expected != actual {
				t.Fatalf("Expected:\n%s\nBut found:\n%s\n", expected, actual)
			}
		})
	}
}

func createComplicatedSelect() Configurable[[]string] {
	result := NewSimpleConfigurable([]string{"a"})
	result.Append(NewConfigurable([]ConfigurableCondition{
		NewConfigurableCondition("os", nil),
		NewConfigurableCondition("arch", nil),
	}, []ConfigurableCase[[]string]{
		NewConfigurableCase([]ConfigurablePattern{
			NewStringConfigurablePattern("android"),
			NewStringConfigurablePattern("x86"),
		}, &[]string{"android", "x86"}),
		NewConfigurableCase([]ConfigurablePattern{
			NewStringConfigurablePattern("android"),
			NewStringConfigurablePattern("arm64"),
		}, &[]string{"android", "arm64"}),
		NewConfigurableCase([]ConfigurablePattern{
			NewDefaultConfigurablePattern(),
			NewStringConfigurablePattern("x86"),
		}, &[]string{"default", "x86"}),
		NewConfigurableCase([]ConfigurablePattern{
			NewDefaultConfigurablePattern(),
			NewDefaultConfigurablePattern(),
		}, &[]string{"default", "default"}),
	}))
	result.Append(NewSimpleConfigurable([]string{"b"}))
	return result
}
