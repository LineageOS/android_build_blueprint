// Copyright 2019 Google Inc. All rights reserved.
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

package blueprint

import (
	"reflect"
	"strings"
	"testing"
)

type moduleCtxTestModule struct {
	SimpleName
}

func newModuleCtxTestModule() (Module, []interface{}) {
	m := &moduleCtxTestModule{}
	return m, []interface{}{&m.SimpleName.Properties}
}

func (f *moduleCtxTestModule) GenerateBuildActions(ModuleContext) {
}

func addVariantDepsResultMutator(variants []Variation, tag DependencyTag, from, to string, results map[string][]Module) func(ctx BottomUpMutatorContext) {
	return func(ctx BottomUpMutatorContext) {
		if ctx.ModuleName() == from {
			ret := ctx.AddVariationDependencies(variants, tag, to)
			results[ctx.ModuleName()] = ret
		}
	}
}

func expectedErrors(t *testing.T, errs []error, expectedMessages ...string) {
	t.Helper()
	if len(errs) != len(expectedMessages) {
		t.Errorf("expected %d error, found: %q", len(expectedMessages), errs)
	} else {
		for i, expected := range expectedMessages {
			err := errs[i]
			if err.Error() != expected {
				t.Errorf("expected error %q found %q", expected, err)
			}
		}
	}
}

func TestAddVariationDependencies(t *testing.T) {
	runWithFailures := func(ctx *Context, expectedErr string) {
		t.Helper()
		bp := `
			test {
				name: "foo",
			}

			test {
				name: "bar",
			}
		`

		mockFS := map[string][]byte{
			"Android.bp": []byte(bp),
		}

		ctx.MockFileSystem(mockFS)

		_, errs := ctx.ParseFileList(".", []string{"Android.bp"}, nil)
		if len(errs) > 0 {
			t.Errorf("unexpected parse errors:")
			for _, err := range errs {
				t.Errorf("  %s", err)
			}
		}

		_, errs = ctx.ResolveDependencies(nil)
		if len(errs) > 0 {
			if expectedErr == "" {
				t.Errorf("unexpected dep errors:")
				for _, err := range errs {
					t.Errorf("  %s", err)
				}
			} else {
				for _, err := range errs {
					if strings.Contains(err.Error(), expectedErr) {
						continue
					} else {
						t.Errorf("unexpected dep error: %s", err)
					}
				}
			}
		} else if expectedErr != "" {
			t.Errorf("missing dep error: %s", expectedErr)
		}
	}

	run := func(ctx *Context) {
		t.Helper()
		runWithFailures(ctx, "")
	}

	t.Run("parallel", func(t *testing.T) {
		ctx := NewContext()
		ctx.RegisterModuleType("test", newModuleCtxTestModule)
		results := make(map[string][]Module)
		depsMutator := addVariantDepsResultMutator(nil, nil, "foo", "bar", results)
		ctx.RegisterBottomUpMutator("deps", depsMutator)

		run(ctx)

		foo := ctx.moduleGroupFromName("foo", nil).moduleByVariantName("")
		bar := ctx.moduleGroupFromName("bar", nil).moduleByVariantName("")

		if g, w := foo.forwardDeps, []*moduleInfo{bar}; !reflect.DeepEqual(g, w) {
			t.Fatalf("expected foo deps to be %q, got %q", w, g)
		}

		if g, w := results["foo"], []Module{bar.logicModule}; !reflect.DeepEqual(g, w) {
			t.Fatalf("expected AddVariationDependencies return value to be %q, got %q", w, g)
		}
	})

	t.Run("missing", func(t *testing.T) {
		ctx := NewContext()
		ctx.RegisterModuleType("test", newModuleCtxTestModule)
		results := make(map[string][]Module)
		depsMutator := addVariantDepsResultMutator(nil, nil, "foo", "baz", results)
		ctx.RegisterBottomUpMutator("deps", depsMutator)
		runWithFailures(ctx, `"foo" depends on undefined module "baz"`)

		foo := ctx.moduleGroupFromName("foo", nil).moduleByVariantName("")

		if g, w := foo.forwardDeps, []*moduleInfo(nil); !reflect.DeepEqual(g, w) {
			t.Fatalf("expected foo deps to be %q, got %q", w, g)
		}

		if g, w := results["foo"], []Module{nil}; !reflect.DeepEqual(g, w) {
			t.Fatalf("expected AddVariationDependencies return value to be %q, got %q", w, g)
		}
	})

	t.Run("allow missing", func(t *testing.T) {
		ctx := NewContext()
		ctx.SetAllowMissingDependencies(true)
		ctx.RegisterModuleType("test", newModuleCtxTestModule)
		results := make(map[string][]Module)
		depsMutator := addVariantDepsResultMutator(nil, nil, "foo", "baz", results)
		ctx.RegisterBottomUpMutator("deps", depsMutator)
		run(ctx)

		foo := ctx.moduleGroupFromName("foo", nil).moduleByVariantName("")

		if g, w := foo.forwardDeps, []*moduleInfo(nil); !reflect.DeepEqual(g, w) {
			t.Fatalf("expected foo deps to be %q, got %q", w, g)
		}

		if g, w := results["foo"], []Module{nil}; !reflect.DeepEqual(g, w) {
			t.Fatalf("expected AddVariationDependencies return value to be %q, got %q", w, g)
		}
	})

}

func TestCheckBlueprintSyntax(t *testing.T) {
	factories := map[string]ModuleFactory{
		"test": newModuleCtxTestModule,
	}

	t.Run("valid", func(t *testing.T) {
		errs := CheckBlueprintSyntax(factories, "path/Blueprint", `
test {
	name: "test",
}
`)
		expectedErrors(t, errs)
	})

	t.Run("syntax error", func(t *testing.T) {
		errs := CheckBlueprintSyntax(factories, "path/Blueprint", `
test {
	name: "test",

`)

		expectedErrors(t, errs, `path/Blueprint:5:1: expected "}", found EOF`)
	})

	t.Run("unknown module type", func(t *testing.T) {
		errs := CheckBlueprintSyntax(factories, "path/Blueprint", `
test2 {
	name: "test",
}
`)

		expectedErrors(t, errs, `path/Blueprint:2:1: unrecognized module type "test2"`)
	})

	t.Run("unknown property name", func(t *testing.T) {
		errs := CheckBlueprintSyntax(factories, "path/Blueprint", `
test {
	nam: "test",
}
`)

		expectedErrors(t, errs, `path/Blueprint:3:5: unrecognized property "nam"`)
	})

	t.Run("invalid property type", func(t *testing.T) {
		errs := CheckBlueprintSyntax(factories, "path/Blueprint", `
test {
	name: false,
}
`)

		expectedErrors(t, errs, `path/Blueprint:3:8: can't assign bool value to string property "name"`)
	})

	t.Run("multiple failures", func(t *testing.T) {
		errs := CheckBlueprintSyntax(factories, "path/Blueprint", `
test {
	name: false,
}

test2 {
	name: false,
}
`)

		expectedErrors(t, errs,
			`path/Blueprint:3:8: can't assign bool value to string property "name"`,
			`path/Blueprint:6:1: unrecognized module type "test2"`,
		)
	})
}

type addNinjaDepsTestModule struct {
	SimpleName
}

func addNinjaDepsTestModuleFactory() (Module, []interface{}) {
	module := &addNinjaDepsTestModule{}
	AddLoadHook(module, func(ctx LoadHookContext) {
		ctx.AddNinjaFileDeps("LoadHookContext")
	})
	return module, []interface{}{&module.SimpleName.Properties}
}

func (m *addNinjaDepsTestModule) GenerateBuildActions(ctx ModuleContext) {
	ctx.AddNinjaFileDeps("GenerateBuildActions")
}

func addNinjaDepsTestBottomUpMutator(ctx BottomUpMutatorContext) {
	ctx.AddNinjaFileDeps("BottomUpMutator")
}

func addNinjaDepsTestTopDownMutator(ctx TopDownMutatorContext) {
	ctx.AddNinjaFileDeps("TopDownMutator")
}

type addNinjaDepsTestSingleton struct{}

func addNinjaDepsTestSingletonFactory() Singleton {
	return &addNinjaDepsTestSingleton{}
}

func (s *addNinjaDepsTestSingleton) GenerateBuildActions(ctx SingletonContext) {
	ctx.AddNinjaFileDeps("Singleton")
}

func TestAddNinjaFileDeps(t *testing.T) {
	ctx := NewContext()
	ctx.MockFileSystem(map[string][]byte{
		"Android.bp": []byte(`
			test {
			    name: "test",
			}
		`),
	})

	ctx.RegisterModuleType("test", addNinjaDepsTestModuleFactory)
	ctx.RegisterBottomUpMutator("testBottomUpMutator", addNinjaDepsTestBottomUpMutator)
	ctx.RegisterTopDownMutator("testTopDownMutator", addNinjaDepsTestTopDownMutator)
	ctx.RegisterSingletonType("testSingleton", addNinjaDepsTestSingletonFactory, false)
	parseDeps, errs := ctx.ParseBlueprintsFiles("Android.bp", nil)
	if len(errs) > 0 {
		t.Errorf("unexpected parse errors:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	resolveDeps, errs := ctx.ResolveDependencies(nil)
	if len(errs) > 0 {
		t.Errorf("unexpected dep errors:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	prepareDeps, errs := ctx.PrepareBuildActions(nil)
	if len(errs) > 0 {
		t.Errorf("unexpected prepare errors:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	if g, w := parseDeps, []string{"Android.bp", "LoadHookContext"}; !reflect.DeepEqual(g, w) {
		t.Errorf("ParseBlueprintsFiles: wanted deps %q, got %q", w, g)
	}

	if g, w := resolveDeps, []string{"BottomUpMutator", "TopDownMutator"}; !reflect.DeepEqual(g, w) {
		t.Errorf("ResolveDependencies: wanted deps %q, got %q", w, g)
	}

	if g, w := prepareDeps, []string{"GenerateBuildActions", "Singleton"}; !reflect.DeepEqual(g, w) {
		t.Errorf("PrepareBuildActions: wanted deps %q, got %q", w, g)
	}

}
