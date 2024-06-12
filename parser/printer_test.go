// Copyright 2014 Google Inc. All rights reserved.
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

package parser

import (
	"bytes"
	"testing"
)

var validPrinterTestCases = []struct {
	name   string
	input  string
	output string
}{
	{
		input: `
foo {}
`,
		output: `
foo {}
`,
	},
	{
		input: `
foo(name= "abc",num= 4,)
`,
		output: `
foo {
    name: "abc",
    num: 4,
}
`,
	},
	{
		input: `
			foo {
				stuff: ["asdf", "jkl;", "qwert",
					"uiop", "bnm,"]
			}
			`,
		output: `
foo {
    stuff: [
        "asdf",
        "bnm,",
        "jkl;",
        "qwert",
        "uiop",
    ],
}
`,
	},
	{
		input: `
		        var = "asdf"
			foo {
				stuff: ["asdf"] + var,
			}`,
		output: `
var = "asdf"
foo {
    stuff: ["asdf"] + var,
}
`,
	},
	{
		input: `
		        var = "asdf"
			foo {
				stuff: [
				    "asdf"
				] + var,
			}`,
		output: `
var = "asdf"
foo {
    stuff: [
        "asdf",
    ] + var,
}
`,
	},
	{
		input: `
		        var = "asdf"
			foo {
				stuff: ["asdf"] + var + ["qwert"],
			}`,
		output: `
var = "asdf"
foo {
    stuff: ["asdf"] + var + ["qwert"],
}
`,
	},
	{
		input: `
		foo {
			stuff: {
				isGood: true,
				name: "bar",
				num: 4,
			}
		}
		`,
		output: `
foo {
    stuff: {
        isGood: true,
        name: "bar",
        num: 4,
    },
}
`,
	},
	{
		input: `
// comment1
foo {
	// comment2
	isGood: true,  // comment3
}
`,
		output: `
// comment1
foo {
    // comment2
    isGood: true, // comment3
}
`,
	},
	{
		input: `
foo {
	name: "abc",
	num: 4,
}

bar  {
	name: "def",
	num: 5,
}
		`,
		output: `
foo {
    name: "abc",
    num: 4,
}

bar {
    name: "def",
    num: 5,
}
`,
	},
	{
		input: `
foo {
    bar: "b" +
        "a" +
	"z",
}
`,
		output: `
foo {
    bar: "b" +
        "a" +
        "z",
}
`,
	},
	{
		input: `
foo = "stuff"
bar = foo
baz = foo + bar
baz += foo
`,
		output: `
foo = "stuff"
bar = foo
baz = foo + bar
baz += foo
`,
	},
	{
		input: `
foo = 100
bar = foo
baz = foo + bar
baz += foo
`,
		output: `
foo = 100
bar = foo
baz = foo + bar
baz += foo
`,
	},
	{
		input: `
foo = "bar " +
    "" +
    "baz"
`,
		output: `
foo = "bar " +
    "" +
    "baz"
`,
	},
	{
		input: `
//test
test /* test */ {
    srcs: [
        /*"bootstrap/bootstrap.go",
    "bootstrap/cleanup.go",*/
        "bootstrap/command.go",
        "bootstrap/doc.go", //doc.go
        "bootstrap/config.go", //config.go
    ],
    deps: ["libabc"],
    incs: []
} //test
//test
test2 {
}


//test3
`,
		output: `
//test
test /* test */ {
    srcs: [
        /*"bootstrap/bootstrap.go",
        "bootstrap/cleanup.go",*/
        "bootstrap/command.go",
        "bootstrap/config.go", //config.go
        "bootstrap/doc.go", //doc.go
    ],
    deps: ["libabc"],
    incs: [],
} //test
//test

test2 {
}

//test3
`,
	},
	{
		input: `
// test
module // test

 {
    srcs
   : [
        "src1.c",
        "src2.c",
    ],
//test
}
//test2
`,
		output: `
// test
module { // test
    srcs: [
        "src1.c",
        "src2.c",
    ],
    //test
}

//test2
`,
	},
	{
		input: `
/*test {
    test: true,
}*/

test {
/*test: true,*/
}

// This
/* Is *//* A */ // A
// A

// Multiline
// Comment

test {}

// This
/* Is */
// A
// Trailing

// Multiline
// Comment
`,
		output: `
/*test {
    test: true,
}*/

test {
    /*test: true,*/
}

// This
/* Is */ /* A */ // A
// A

// Multiline
// Comment

test {}

// This
/* Is */
// A
// Trailing

// Multiline
// Comment
`,
	},
	{
		input: `
test // test

// test
{
}
`,
		output: `
test { // test
// test
}
`,
	},
	{
		input: `
// test
stuff {
    namespace: "google",
    string_vars: [
      {
          var: "one",
          values: [ "one_a", "one_b",],
      },
      {
          var: "two",
          values: [ "two_a", "two_b", ],
      },
    ],
}`,
		output: `
// test
stuff {
    namespace: "google",
    string_vars: [
        {
            var: "one",
            values: [
                "one_a",
                "one_b",
            ],
        },
        {
            var: "two",
            values: [
                "two_a",
                "two_b",
            ],
        },
    ],
}
`,
	},
	{
		input: `
// test
stuff {
    namespace: "google",
    list_of_lists: [
        [ "a", "b" ],
        [ "c", "d" ],
    ],
}
`,
		output: `
// test
stuff {
    namespace: "google",
    list_of_lists: [
        [
            "a",
            "b",
        ],
        [
            "c",
            "d",
        ],
    ],
}
`,
	},
	{
		input: `
// test
stuff {
    namespace: "google",
    list_of_structs: [{ key1: "a", key2: "b" }],
}
`,
		output: `
// test
stuff {
    namespace: "google",
    list_of_structs: [
        {
            key1: "a",
            key2: "b",
        },
    ],
}
`,
	},
	{
		input: `
// test
foo {
    stuff: [
        "a", // great comment
        "b",
    ],
}
`,
		output: `
// test
foo {
    stuff: [
        "a", // great comment
        "b",
    ],
}
`,
	},
	{
		input: `
// test
foo {
    stuff: [
        "a",
        // b comment
        "b",
    ],
}
`,
		output: `
// test
foo {
    stuff: [
        "a",
        // b comment
        "b",
    ],
}
`,
	},
	{
		input: `
// test
foo {
    stuff: [
        "a", // a comment
        // b comment
        "b",
    ],
}
`,
		output: `
// test
foo {
    stuff: [
        "a", // a comment
        // b comment
        "b",
    ],
}
`,
	},
	{
		input: `
// test
foo {
    stuff: [
        "a",
        // b comment
        // on multiline
        "b",
    ],
}
`,
		output: `
// test
foo {
    stuff: [
        "a",
        // b comment
        // on multiline
        "b",
    ],
}
`,
	},
	{ // Line comment are treat as groups separator
		input: `
// test
foo {
    stuff: [
        "b",
        // a comment
        "a",
    ],
}
`,
		output: `
// test
foo {
    stuff: [
        "b",
        // a comment
        "a",
    ],
}
`,
	},
	{
		name: "Basic selects",
		input: `
// test
foo {
    stuff: select(soong_config_variable("my_namespace", "my_variable"), {
        "a": "a2",
        // test2
        "b": "b2",
        // test3
        default: "c2",
    }),
}
`,
		output: `
// test
foo {
    stuff: select(soong_config_variable("my_namespace", "my_variable"), {
        "a": "a2",
        // test2
        "b": "b2",
        // test3
        default: "c2",
    }),
}
`,
	},
	{
		name: "Remove select with only default",
		input: `
// test
foo {
    stuff: select(soong_config_variable("my_namespace", "my_variable"), {
        // test2
        default: "c2",
    }),
}
`,
		output: `
// test
foo {
    stuff: "c2", // test2
}
`,
	},
	{
		name: "Appended selects",
		input: `
// test
foo {
    stuff: select(soong_config_variable("my_namespace", "my_variable"), {
        "a": "a2",
        // test2
        "b": "b2",
        // test3
        default: "c2",
    }) + select(release_variable("RELEASE_TEST"), {
        "d": "d2",
        "e": "e2",
        default: "f2",
    }),
}
`,
		output: `
// test
foo {
    stuff: select(soong_config_variable("my_namespace", "my_variable"), {
        "a": "a2",
        // test2
        "b": "b2",
        // test3
        default: "c2",
    }) + select(release_variable("RELEASE_TEST"), {
        "d": "d2",
        "e": "e2",
        default: "f2",
    }),
}
`,
	},
	{
		name: "Select with unset property",
		input: `
foo {
    stuff: select(soong_config_variable("my_namespace", "my_variable"), {
        "foo": unset,
        default: "c2",
    }),
}
`,
		output: `
foo {
    stuff: select(soong_config_variable("my_namespace", "my_variable"), {
        "foo": unset,
        default: "c2",
    }),
}
`,
	},
	{
		name: "Multi-condition select",
		input: `
foo {
    stuff: select((arch(), os()), {
        ("x86", "linux"): "a",
        (default, default): "b",
    }),
}
`,
		output: `
foo {
    stuff: select((arch(), os()), {
        ("x86", "linux"): "a",
        (default, default): "b",
    }),
}
`,
	},
	{
		name: "Multi-condition select with conditions on new lines",
		input: `
foo {
    stuff: select((arch(), 
    os()), {
        ("x86", "linux"): "a",
        (default, default): "b",
    }),
}
`,
		output: `
foo {
    stuff: select((
        arch(),
        os(),
    ), {
        ("x86", "linux"): "a",
        (default, default): "b",
    }),
}
`,
	},
	{
		name: "Select with multiline inner expression",
		input: `
foo {
    cflags: [
        "-DPRODUCT_COMPATIBLE_PROPERTY",
        "-DRIL_SHLIB",
        "-Wall",
        "-Wextra",
        "-Werror",
    ] + select(soong_config_variable("sim", "sim_count"), {
        "2": [
            "-DANDROID_MULTI_SIM",
            "-DANDROID_SIM_COUNT_2",
        ],
        default: [],
    }),
}
`,
		output: `
foo {
    cflags: [
        "-DPRODUCT_COMPATIBLE_PROPERTY",
        "-DRIL_SHLIB",
        "-Wall",
        "-Werror",
        "-Wextra",
    ] + select(soong_config_variable("sim", "sim_count"), {
        "2": [
            "-DANDROID_MULTI_SIM",
            "-DANDROID_SIM_COUNT_2",
        ],
        default: [],
    }),
}
`,
	},
}

func TestPrinter(t *testing.T) {
	for _, testCase := range validPrinterTestCases {
		t.Run(testCase.name, func(t *testing.T) {
			in := testCase.input[1:]
			expected := testCase.output[1:]

			r := bytes.NewBufferString(in)
			file, errs := Parse("", r)
			if len(errs) != 0 {
				t.Errorf("test case: %s", in)
				t.Errorf("unexpected errors:")
				for _, err := range errs {
					t.Errorf("  %s", err)
				}
				t.FailNow()
			}

			SortLists(file)

			got, err := Print(file)
			if err != nil {
				t.Errorf("test case: %s", in)
				t.Errorf("unexpected error: %s", err)
				t.FailNow()
			}

			if string(got) != expected {
				t.Errorf("test case: %s", in)
				t.Errorf("  expected: %s", expected)
				t.Errorf("       got: %s", string(got))
			}
		})
	}
}
