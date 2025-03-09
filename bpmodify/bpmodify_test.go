// Copyright 2020 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package bpmodify

import (
	"strings"
	"testing"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func must2[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func simplifyModuleDefinition(def string) string {
	var result string
	for _, line := range strings.Split(def, "\n") {
		result += strings.TrimSpace(line)
	}
	return result
}
func TestBpModify(t *testing.T) {
	var testCases = []struct {
		name     string
		input    string
		output   string
		err      string
		modified bool
		f        func(bp *Blueprint)
	}{
		{
			name: "add",
			input: `
			cc_foo {
				name: "foo",
			}
			`,
			output: `
			cc_foo {
				name: "foo",
				deps: ["bar"],
			}
			`,
			modified: true,
			f: func(bp *Blueprint) {
				props := must2(bp.ModulesByName("foo").GetOrCreateProperty(List, "deps"))
				must(props.AddStringToList("bar"))
			},
		},
		{
			name: "remove",
			input: `
			cc_foo {
				name: "foo",
				deps: ["bar"],
			}
			`,
			output: `
			cc_foo {
				name: "foo",
				deps: [],
			}
			`,
			modified: true,
			f: func(bp *Blueprint) {
				props := must2(bp.ModulesByName("foo").GetProperty("deps"))
				must(props.RemoveStringFromList("bar"))
			},
		},
		{
			name: "nested add",
			input: `
			cc_foo {
				name: "foo",
			}
			`,
			output: `
			cc_foo {
				name: "foo",
				arch: {
					arm: {
						deps: [
							"dep2",
							"nested_dep",],
					},
				},
			}
			`,
			modified: true,
			f: func(bp *Blueprint) {
				props := must2(bp.ModulesByName("foo").GetOrCreateProperty(List, "arch.arm.deps"))
				must(props.AddStringToList("nested_dep", "dep2"))
			},
		},
		{
			name: "nested remove",
			input: `
			cc_foo {
				name: "foo",
				arch: {
					arm: {
						deps: [
							"dep2",
							"nested_dep",
						],
					},
				},
			}
			`,
			output: `
			cc_foo {
				name: "foo",
				arch: {
					arm: {
						deps: [
						],
					},
				},
			}
			`,
			modified: true,
			f: func(bp *Blueprint) {
				props := must2(bp.ModulesByName("foo").GetProperty("arch.arm.deps"))
				must(props.RemoveStringFromList("nested_dep", "dep2"))
			},
		},
		{
			name: "add existing",
			input: `
			cc_foo {
				name: "foo",
				arch: {
					arm: {
						deps: [
							"nested_dep",
							"dep2",
						],
					},
				},
			}
			`,
			output: `
			cc_foo {
				name: "foo",
				arch: {
					arm: {
						deps: [
							"nested_dep",
							"dep2",
						],
					},
				},
			}
			`,
			modified: false,
			f: func(bp *Blueprint) {
				props := must2(bp.ModulesByName("foo").GetOrCreateProperty(List, "arch.arm.deps"))
				must(props.AddStringToList("dep2", "dep2"))
			},
		},
		{
			name: "remove missing",
			input: `
			cc_foo {
				name: "foo",
				arch: {
					arm: {
						deps: [
							"nested_dep",
							"dep2",
						],
					},
				},
			}
			`,
			output: `
			cc_foo {
				name: "foo",
				arch: {
					arm: {
						deps: [
							"nested_dep",
							"dep2",
						],
					},
				},
			}
			`,
			modified: false,
			f: func(bp *Blueprint) {
				props := must2(bp.ModulesByName("foo").GetProperty("arch.arm.deps"))
				must(props.RemoveStringFromList("dep3", "dep4"))
			},
		},
		{
			name: "remove non existent",
			input: `
			cc_foo {
				name: "foo",
			}
			`,
			output: `
			cc_foo {
				name: "foo",
			}
			`,
			modified: false,
			f: func(bp *Blueprint) {
				props := must2(bp.ModulesByName("foo").GetProperty("deps"))
				must(props.RemoveStringFromList("bar"))
			},
		},
		{
			name: "remove non existent nested",
			input: `
			cc_foo {
				name: "foo",
				arch: {},
			}
			`,
			output: `
			cc_foo {
				name: "foo",
				arch: {},
			}
			`,
			modified: false,
			f: func(bp *Blueprint) {
				props := must2(bp.ModulesByName("foo").GetProperty("arch.arm.deps"))
				must(props.RemoveStringFromList("bar"))
			},
		},
		{
			name: "add numeric sorted",
			input: `
			cc_foo {
				name: "foo",
				versions: ["1", "2", "20"],
			}
			`,
			output: `
			cc_foo {
				name: "foo",
				versions: [
					"1",
					"2",
					"10",
					"20",
				],
			}
			`,
			modified: true,
			f: func(bp *Blueprint) {
				props := must2(bp.ModulesByName("foo").GetProperty("versions"))
				must(props.AddStringToList("10"))
			},
		},
		{
			name: "add mixed sorted",
			input: `
			cc_foo {
				name: "foo",
				deps: ["bar-v1-bar", "bar-v2-bar"],
			}
			`,
			output: `
			cc_foo {
				name: "foo",
				deps: [
					"bar-v1-bar",
					"bar-v2-bar",
					"bar-v10-bar",
				],
			}
			`,
			modified: true,
			f: func(bp *Blueprint) {
				props := must2(bp.ModulesByName("foo").GetProperty("deps"))
				must(props.AddStringToList("bar-v10-bar"))
			},
		},
		{
			name:  "add a struct with literal",
			input: `cc_foo {name: "foo"}`,
			output: `cc_foo {
				name: "foo",
				structs: [
					{
						version: "1",

						imports: [
							"bar1",
							"bar2",
						],
					},
				],
			}
			`,
			modified: true,
			f: func(bp *Blueprint) {
				props := must2(bp.ModulesByName("foo").GetOrCreateProperty(List, "structs"))
				must(props.AddLiteral(`{version: "1", imports: ["bar1", "bar2"]}`))
			},
		},
		{
			name: "set string",
			input: `
			cc_foo {
				name: "foo",
			}
			`,
			output: `
			cc_foo {
				name: "foo",
				foo: "bar",
			}
			`,
			modified: true,
			f: func(bp *Blueprint) {
				props := must2(bp.ModulesByName("foo").GetOrCreateProperty(String, "foo"))
				must(props.SetString("bar"))
			},
		},
		{
			name: "set existing string",
			input: `
			cc_foo {
				name: "foo",
				foo: "baz",
			}
			`,
			output: `
			cc_foo {
				name: "foo",
				foo: "bar",
			}
			`,
			modified: true,
			f: func(bp *Blueprint) {
				props := must2(bp.ModulesByName("foo").GetOrCreateProperty(String, "foo"))
				must(props.SetString("bar"))
			},
		},
		{
			name: "set bool",
			input: `
			cc_foo {
				name: "foo",
			}
			`,
			output: `
			cc_foo {
				name: "foo",
				foo: true,
			}
			`,
			modified: true,
			f: func(bp *Blueprint) {
				props := must2(bp.ModulesByName("foo").GetOrCreateProperty(Bool, "foo"))
				must(props.SetBool(true))
			},
		},
		{
			name: "set existing bool",
			input: `
			cc_foo {
				name: "foo",
				foo: true,
			}
			`,
			output: `
			cc_foo {
				name: "foo",
				foo: false,
			}
			`,
			modified: true,
			f: func(bp *Blueprint) {
				props := must2(bp.ModulesByName("foo").GetOrCreateProperty(Bool, "foo"))
				must(props.SetBool(false))
			},
		},
		{
			name: "remove existing property",
			input: `
			cc_foo {
				name: "foo",
				foo: "baz",
			}
			`,
			output: `
			cc_foo {
				name: "foo",
			}
			`,
			modified: true,
			f: func(bp *Blueprint) {
				must(bp.ModulesByName("foo").RemoveProperty("foo"))
			},
		}, {
			name: "remove nested property",
			input: `
			cc_foo {
				name: "foo",
				foo: {
					bar: "baz",
				},
			}
			`,
			output: `
			cc_foo {
				name: "foo",
				foo: {},
			}
			`,
			modified: true,
			f: func(bp *Blueprint) {
				must(bp.ModulesByName("foo").RemoveProperty("foo.bar"))
			},
		}, {
			name: "remove non-existing property",
			input: `
			cc_foo {
				name: "foo",
				foo: "baz",
			}
			`,
			output: `
			cc_foo {
				name: "foo",
				foo: "baz",
			}
			`,
			modified: false,
			f: func(bp *Blueprint) {
				must(bp.ModulesByName("foo").RemoveProperty("bar"))
			},
		}, {
			name: "replace property",
			input: `
			cc_foo {
				name: "foo",
				deps: ["baz", "unchanged"],
			}
			`,
			output: `
			cc_foo {
				name: "foo",
				deps: [
                "baz_lib",
                "unchanged",
				],
			}
			`,
			modified: true,
			f: func(bp *Blueprint) {
				props := must2(bp.ModulesByName("foo").GetProperty("deps"))
				must(props.ReplaceStrings(map[string]string{"baz": "baz_lib", "foobar": "foobar_lib"}))
			},
		}, {
			name: "replace property multiple modules",
			input: `
			cc_foo {
				name: "foo",
				deps: ["baz", "unchanged"],
				unchanged: ["baz"],
				required: ["foobar"],
			}
			`,
			output: `
			cc_foo {
				name: "foo",
				deps: [
								"baz_lib",
								"unchanged",
				],
				unchanged: ["baz"],
				required: ["foobar_lib"],
			}
			`,
			modified: true,
			f: func(bp *Blueprint) {
				props := must2(bp.ModulesByName("foo").GetProperty("deps", "required"))
				must(props.ReplaceStrings(map[string]string{"baz": "baz_lib", "foobar": "foobar_lib"}))
			},
		}, {
			name: "replace property string value",
			input: `
			cc_foo {
				name: "foo",
				deps: ["baz"],
				unchanged: ["baz"],
				required: ["foobar"],
			}
			`,
			output: `
			cc_foo {
				name: "foo_lib",
				deps: ["baz"],
				unchanged: ["baz"],
				required: ["foobar"],
			}
			`,
			modified: true,
			f: func(bp *Blueprint) {
				props := must2(bp.ModulesByName("foo").GetProperty("name"))
				must(props.ReplaceStrings(map[string]string{"foo": "foo_lib"}))
			},
		}, {
			name: "replace property string and list values",
			input: `
			cc_foo {
				name: "foo",
				deps: ["baz"],
				unchanged: ["baz"],
				required: ["foobar"],
			}
			`,
			output: `
			cc_foo {
				name: "foo_lib",
				deps: ["baz_lib"],
				unchanged: ["baz"],
				required: ["foobar"],
			}
			`,
			modified: true,
			f: func(bp *Blueprint) {
				props := must2(bp.ModulesByName("foo").GetProperty("name", "deps"))
				must(props.ReplaceStrings(map[string]string{"foo": "foo_lib", "baz": "baz_lib"}))
			},
		}, {
			name: "move contents of property into non-existing property",
			input: `
			cc_foo {
				name: "foo",
				bar: ["barContents"],
				}
`,
			output: `
			cc_foo {
				name: "foo",
				baz: ["barContents"],
			}
			`,
			modified: true,
			f: func(bp *Blueprint) {
				must(bp.ModulesByName("foo").MoveProperty("baz", "bar"))
			},
		}, {
			name: "move contents of property into existing property",
			input: `
			cc_foo {
				name: "foo",
				baz: ["bazContents"],
				bar: ["barContents"],
			}
			`,
			output: `
			cc_foo {
				name: "foo",
				baz: [
					"bazContents",
					"barContents",
				],

			}
			`,
			modified: true,
			f: func(bp *Blueprint) {
				must(bp.ModulesByName("foo").MoveProperty("baz", "bar"))
			},
		}, {
			name: "replace nested",
			input: `
			cc_foo {
				name: "foo",
				foo: {
					bar: "baz",
				},
			}
			`,
			output: `
			cc_foo {
				name: "foo",
				foo: {
					bar: "baz2",
				},
			}
			`,
			modified: true,
			f: func(bp *Blueprint) {
				props := must2(bp.ModulesByName("foo").GetProperty("foo.bar"))
				must(props.ReplaceStrings(map[string]string{"baz": "baz2"}))
			},
		},
	}

	for i, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			bp, err := NewBlueprint("", []byte(testCase.input))
			if err != nil {
				t.Fatalf("error creating Blueprint: %s", err)
			}
			err = nil
			func() {
				defer func() {
					if r := recover(); r != nil {
						if recoveredErr, ok := r.(error); ok {
							err = recoveredErr
						} else {
							t.Fatalf("unexpected panic: %q", r)
						}
					}
				}()
				testCase.f(bp)
			}()
			if err != nil {
				if testCase.err != "" {
					if g, w := err.Error(), testCase.err; !strings.Contains(w, g) {
						t.Errorf("unexpected error, want %q, got %q", g, w)
					}
				} else {
					t.Errorf("unexpected error %q", err.Error())
				}
			} else {
				if testCase.err != "" {
					t.Errorf("missing error, expected %q", testCase.err)
				}
			}

			if g, w := bp.Modified(), testCase.modified; g != w {
				t.Errorf("incorrect bp.Modified() value, want %v, got %v", w, g)
			}

			inModuleString := bp.String()
			if simplifyModuleDefinition(inModuleString) != simplifyModuleDefinition(testCase.output) {
				t.Errorf("test case %d:", i)
				t.Errorf("expected module definition:")
				t.Errorf("  %s", testCase.output)
				t.Errorf("actual module definition:")
				t.Errorf("  %s", inModuleString)
			}
		})
	}
}
