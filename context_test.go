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

package blueprint

import (
	"bytes"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"text/scanner"
	"time"

	"github.com/google/blueprint/parser"
	"github.com/google/blueprint/proptools"
)

type Walker interface {
	Walk() bool
}

func walkDependencyGraph(ctx *Context, topModule *moduleInfo, allowDuplicates bool) (string, string) {
	var outputDown string
	var outputUp string
	ctx.walkDeps(topModule, allowDuplicates,
		func(dep depInfo, parent *moduleInfo) bool {
			outputDown += ctx.ModuleName(dep.module.logicModule)
			if tag, ok := dep.tag.(walkerDepsTag); ok {
				if !tag.follow {
					return false
				}
			}
			if dep.module.logicModule.(Walker).Walk() {
				return true
			}

			return false
		},
		func(dep depInfo, parent *moduleInfo) {
			outputUp += ctx.ModuleName(dep.module.logicModule)
		})
	return outputDown, outputUp
}

type depsProvider interface {
	Deps() []string
	IgnoreDeps() []string
}

type IncrementalTestProvider struct {
	Value string
}

var IncrementalTestProviderKey = NewProvider[IncrementalTestProvider]()

type baseTestModule struct {
	SimpleName
	properties struct {
		Deps         []string
		Ignored_deps []string
	}
	GenerateBuildActionsCalled bool
}

func (b *baseTestModule) Deps() []string {
	return b.properties.Deps
}

func (b *baseTestModule) IgnoreDeps() []string {
	return b.properties.Ignored_deps
}

var pctx PackageContext

func init() {
	pctx = NewPackageContext("android/blueprint")
}
func (b *baseTestModule) GenerateBuildActions(ctx ModuleContext) {
	b.GenerateBuildActionsCalled = true
	outputFile := ctx.ModuleName() + "_phony_output"
	ctx.Build(pctx, BuildParams{
		Rule:    Phony,
		Outputs: []string{outputFile},
	})
	SetProvider(ctx, IncrementalTestProviderKey, IncrementalTestProvider{
		Value: ctx.ModuleName(),
	})
}

type fooModule struct {
	baseTestModule
}

func newFooModule() (Module, []interface{}) {
	m := &fooModule{}
	return m, []interface{}{&m.baseTestModule.properties, &m.SimpleName.Properties}
}

func (f *fooModule) Walk() bool {
	return true
}

type barModule struct {
	SimpleName
	baseTestModule
}

func newBarModule() (Module, []interface{}) {
	m := &barModule{}
	return m, []interface{}{&m.baseTestModule.properties, &m.SimpleName.Properties}
}

func (b *barModule) Walk() bool {
	return false
}

type incrementalModule struct {
	SimpleName
	baseTestModule
	IncrementalModule
}

var _ Incremental = &incrementalModule{}

func newIncrementalModule() (Module, []interface{}) {
	m := &incrementalModule{}
	return m, []interface{}{&m.baseTestModule.properties, &m.SimpleName.Properties}
}

type walkerDepsTag struct {
	BaseDependencyTag
	// True if the dependency should be followed, false otherwise.
	follow bool
}

func depsMutator(mctx BottomUpMutatorContext) {
	if m, ok := mctx.Module().(depsProvider); ok {
		mctx.AddDependency(mctx.Module(), walkerDepsTag{follow: false}, m.IgnoreDeps()...)
		mctx.AddDependency(mctx.Module(), walkerDepsTag{follow: true}, m.Deps()...)
	}
}

func TestContextParse(t *testing.T) {
	ctx := NewContext()
	ctx.RegisterModuleType("foo_module", newFooModule)
	ctx.RegisterModuleType("bar_module", newBarModule)

	r := bytes.NewBufferString(`
		foo_module {
	        name: "MyFooModule",
			deps: ["MyBarModule"],
		}

		bar_module {
	        name: "MyBarModule",
		}
	`)

	_, _, errs := ctx.parseOne(".", "Blueprint", r, parser.NewScope(nil), nil)
	if len(errs) > 0 {
		t.Errorf("unexpected parse errors:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	_, errs = ctx.ResolveDependencies(nil)
	if len(errs) > 0 {
		t.Errorf("unexpected dep errors:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}
}

// > |===B---D       - represents a non-walkable edge
// > A               = represents a walkable edge
// > |===C===E---G
// >     |       |   A should not be visited because it's the root node.
// >     |===F===|   B, D and E should not be walked.
func TestWalkDeps(t *testing.T) {
	ctx := NewContext()
	ctx.MockFileSystem(map[string][]byte{
		"Android.bp": []byte(`
			foo_module {
			    name: "A",
			    deps: ["B", "C"],
			}

			bar_module {
			    name: "B",
			    deps: ["D"],
			}

			foo_module {
			    name: "C",
			    deps: ["E", "F"],
			}

			foo_module {
			    name: "D",
			}

			bar_module {
			    name: "E",
			    deps: ["G"],
			}

			foo_module {
			    name: "F",
			    deps: ["G"],
			}

			foo_module {
			    name: "G",
			}
		`),
	})

	ctx.RegisterModuleType("foo_module", newFooModule)
	ctx.RegisterModuleType("bar_module", newBarModule)
	ctx.RegisterBottomUpMutator("deps", depsMutator)
	_, errs := ctx.ParseBlueprintsFiles("Android.bp", nil)
	if len(errs) > 0 {
		t.Errorf("unexpected parse errors:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	_, errs = ctx.ResolveDependencies(nil)
	if len(errs) > 0 {
		t.Errorf("unexpected dep errors:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	topModule := ctx.moduleGroupFromName("A", nil).modules.firstModule()
	outputDown, outputUp := walkDependencyGraph(ctx, topModule, false)
	if outputDown != "BCEFG" {
		t.Errorf("unexpected walkDeps behaviour: %s\ndown should be: BCEFG", outputDown)
	}
	if outputUp != "BEGFC" {
		t.Errorf("unexpected walkDeps behaviour: %s\nup should be: BEGFC", outputUp)
	}
}

// > |===B---D           - represents a non-walkable edge
// > A                   = represents a walkable edge
// > |===C===E===\       A should not be visited because it's the root node.
// >     |       |       B, D should not be walked.
// >     |===F===G===H   G should be visited multiple times
// >         \===/       H should only be visited once
func TestWalkDepsDuplicates(t *testing.T) {
	ctx := NewContext()
	ctx.MockFileSystem(map[string][]byte{
		"Android.bp": []byte(`
			foo_module {
			    name: "A",
			    deps: ["B", "C"],
			}

			bar_module {
			    name: "B",
			    deps: ["D"],
			}

			foo_module {
			    name: "C",
			    deps: ["E", "F"],
			}

			foo_module {
			    name: "D",
			}

			foo_module {
			    name: "E",
			    deps: ["G"],
			}

			foo_module {
			    name: "F",
			    deps: ["G", "G"],
			}

			foo_module {
			    name: "G",
				deps: ["H"],
			}

			foo_module {
			    name: "H",
			}
		`),
	})

	ctx.RegisterModuleType("foo_module", newFooModule)
	ctx.RegisterModuleType("bar_module", newBarModule)
	ctx.RegisterBottomUpMutator("deps", depsMutator)
	_, errs := ctx.ParseBlueprintsFiles("Android.bp", nil)
	if len(errs) > 0 {
		t.Errorf("unexpected parse errors:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	_, errs = ctx.ResolveDependencies(nil)
	if len(errs) > 0 {
		t.Errorf("unexpected dep errors:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	topModule := ctx.moduleGroupFromName("A", nil).modules.firstModule()
	outputDown, outputUp := walkDependencyGraph(ctx, topModule, true)
	if outputDown != "BCEGHFGG" {
		t.Errorf("unexpected walkDeps behaviour: %s\ndown should be: BCEGHFGG", outputDown)
	}
	if outputUp != "BHGEGGFC" {
		t.Errorf("unexpected walkDeps behaviour: %s\nup should be: BHGEGGFC", outputUp)
	}
}

// >                     - represents a non-walkable edge
// > A                   = represents a walkable edge
// > |===B-------\       A should not be visited because it's the root node.
// >     |       |       B -> D should not be walked.
// >     |===C===D===E   B -> C -> D -> E should be walked
func TestWalkDepsDuplicates_IgnoreFirstPath(t *testing.T) {
	ctx := NewContext()
	ctx.MockFileSystem(map[string][]byte{
		"Android.bp": []byte(`
			foo_module {
			    name: "A",
			    deps: ["B"],
			}

			foo_module {
			    name: "B",
			    deps: ["C"],
			    ignored_deps: ["D"],
			}

			foo_module {
			    name: "C",
			    deps: ["D"],
			}

			foo_module {
			    name: "D",
			    deps: ["E"],
			}

			foo_module {
			    name: "E",
			}
		`),
	})

	ctx.RegisterModuleType("foo_module", newFooModule)
	ctx.RegisterModuleType("bar_module", newBarModule)
	ctx.RegisterBottomUpMutator("deps", depsMutator)
	_, errs := ctx.ParseBlueprintsFiles("Android.bp", nil)
	if len(errs) > 0 {
		t.Errorf("unexpected parse errors:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	_, errs = ctx.ResolveDependencies(nil)
	if len(errs) > 0 {
		t.Errorf("unexpected dep errors:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	topModule := ctx.moduleGroupFromName("A", nil).modules.firstModule()
	outputDown, outputUp := walkDependencyGraph(ctx, topModule, true)
	expectedDown := "BDCDE"
	if outputDown != expectedDown {
		t.Errorf("unexpected walkDeps behaviour: %s\ndown should be: %s", outputDown, expectedDown)
	}
	expectedUp := "DEDCB"
	if outputUp != expectedUp {
		t.Errorf("unexpected walkDeps behaviour: %s\nup should be: %s", outputUp, expectedUp)
	}
}

func TestCreateModule(t *testing.T) {
	ctx := newContext()
	ctx.MockFileSystem(map[string][]byte{
		"Android.bp": []byte(`
			foo_module {
			    name: "A",
			    deps: ["B", "C"],
			}
		`),
	})

	ctx.RegisterBottomUpMutator("create", createTestMutator).UsesCreateModule()
	ctx.RegisterBottomUpMutator("deps", depsMutator)

	ctx.RegisterModuleType("foo_module", newFooModule)
	ctx.RegisterModuleType("bar_module", newBarModule)
	_, errs := ctx.ParseBlueprintsFiles("Android.bp", nil)
	if len(errs) > 0 {
		t.Errorf("unexpected parse errors:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	_, errs = ctx.ResolveDependencies(nil)
	if len(errs) > 0 {
		t.Errorf("unexpected dep errors:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	a := ctx.moduleGroupFromName("A", nil).modules.firstModule().logicModule.(*fooModule)
	b := ctx.moduleGroupFromName("B", nil).modules.firstModule().logicModule.(*barModule)
	c := ctx.moduleGroupFromName("C", nil).modules.firstModule().logicModule.(*barModule)
	d := ctx.moduleGroupFromName("D", nil).modules.firstModule().logicModule.(*fooModule)

	checkDeps := func(m Module, expected string) {
		var deps []string
		ctx.VisitDirectDeps(m, func(m Module) {
			deps = append(deps, ctx.ModuleName(m))
		})
		got := strings.Join(deps, ",")
		if got != expected {
			t.Errorf("unexpected %q dependencies, got %q expected %q",
				ctx.ModuleName(m), got, expected)
		}
	}

	checkDeps(a, "B,C")
	checkDeps(b, "D")
	checkDeps(c, "D")
	checkDeps(d, "")
}

func createTestMutator(ctx BottomUpMutatorContext) {
	type props struct {
		Name string
		Deps []string
	}

	ctx.CreateModule(newBarModule, "new_bar", &props{
		Name: "B",
		Deps: []string{"D"},
	})

	ctx.CreateModule(newBarModule, "new_bar", &props{
		Name: "C",
		Deps: []string{"D"},
	})

	ctx.CreateModule(newFooModule, "new_foo", &props{
		Name: "D",
	})
}

func TestWalkFileOrder(t *testing.T) {
	// Run the test once to see how long it normally takes
	start := time.Now()
	doTestWalkFileOrder(t, time.Duration(0))
	duration := time.Since(start)

	// Run the test again, but put enough of a sleep into each visitor to detect ordering
	// problems if they exist
	doTestWalkFileOrder(t, duration)
}

// test that WalkBlueprintsFiles calls asyncVisitor in the right order
func doTestWalkFileOrder(t *testing.T, sleepDuration time.Duration) {
	// setup mock context
	ctx := newContext()
	mockFiles := map[string][]byte{
		"Android.bp": []byte(`
			sample_module {
			    name: "a",
			}
		`),
		"dir1/Android.bp": []byte(`
			sample_module {
			    name: "b",
			}
		`),
		"dir1/dir2/Android.bp": []byte(`
			sample_module {
			    name: "c",
			}
		`),
	}
	ctx.MockFileSystem(mockFiles)

	// prepare to monitor the visit order
	visitOrder := []string{}
	visitLock := sync.Mutex{}
	correctVisitOrder := []string{"Android.bp", "dir1/Android.bp", "dir1/dir2/Android.bp"}

	// sleep longer when processing the earlier files
	chooseSleepDuration := func(fileName string) (duration time.Duration) {
		duration = time.Duration(0)
		for i := len(correctVisitOrder) - 1; i >= 0; i-- {
			if fileName == correctVisitOrder[i] {
				return duration
			}
			duration = duration + sleepDuration
		}
		panic("unrecognized file name " + fileName)
	}

	visitor := func(file *parser.File) {
		time.Sleep(chooseSleepDuration(file.Name))
		visitLock.Lock()
		defer visitLock.Unlock()
		visitOrder = append(visitOrder, file.Name)
	}
	keys := []string{"Android.bp", "dir1/Android.bp", "dir1/dir2/Android.bp"}

	// visit the blueprints files
	ctx.WalkBlueprintsFiles(".", keys, visitor)

	// check the order
	if !reflect.DeepEqual(visitOrder, correctVisitOrder) {
		t.Errorf("Incorrect visit order; expected %v, got %v", correctVisitOrder, visitOrder)
	}
}

// test that WalkBlueprintsFiles reports syntax errors
func TestWalkingWithSyntaxError(t *testing.T) {
	// setup mock context
	ctx := newContext()
	mockFiles := map[string][]byte{
		"Android.bp": []byte(`
			sample_module {
			    name: "a" "b",
			}
		`),
		"dir1/Android.bp": []byte(`
			sample_module {
			    name: "b",
		`),
		"dir1/dir2/Android.bp": []byte(`
			sample_module {
			    name: "c",
			}
		`),
	}
	ctx.MockFileSystem(mockFiles)

	keys := []string{"Android.bp", "dir1/Android.bp", "dir1/dir2/Android.bp"}

	// visit the blueprints files
	_, errs := ctx.WalkBlueprintsFiles(".", keys, func(file *parser.File) {})

	expectedErrs := []error{
		errors.New(`Android.bp:3:18: expected "}", found String`),
		errors.New(`dir1/Android.bp:4:3: expected "}", found EOF`),
	}
	if fmt.Sprintf("%s", expectedErrs) != fmt.Sprintf("%s", errs) {
		t.Errorf("Incorrect errors; expected:\n%s\ngot:\n%s", expectedErrs, errs)
	}

}

func TestParseFailsForModuleWithoutName(t *testing.T) {
	ctx := NewContext()
	ctx.MockFileSystem(map[string][]byte{
		"Android.bp": []byte(`
			foo_module {
			    name: "A",
			}

			bar_module {
			    deps: ["A"],
			}
		`),
	})
	ctx.RegisterModuleType("foo_module", newFooModule)
	ctx.RegisterModuleType("bar_module", newBarModule)

	_, errs := ctx.ParseBlueprintsFiles("Android.bp", nil)

	expectedErrs := []error{
		errors.New(`Android.bp:6:4: property 'name' is missing from a module`),
	}
	if fmt.Sprintf("%s", expectedErrs) != fmt.Sprintf("%s", errs) {
		t.Errorf("Incorrect errors; expected:\n%s\ngot:\n%s", expectedErrs, errs)
	}
}

func Test_findVariant(t *testing.T) {
	module := &moduleInfo{
		variant: variant{
			name: "normal_local",
			variations: variationMap{
				map[string]string{
					"normal": "normal",
				},
			},
		},
	}

	type alias struct {
		variant variant
		target  int
	}

	makeDependencyGroup := func(in ...*moduleInfo) *moduleGroup {
		group := &moduleGroup{
			name: "dep",
		}
		for _, m := range in {
			m.group = group
			group.modules = append(group.modules, m)
		}
		return group
	}

	tests := []struct {
		name         string
		possibleDeps *moduleGroup
		variations   []Variation
		far          bool
		reverse      bool
		want         string
	}{
		{
			name: "AddVariationDependencies(nil)",
			// A dependency that matches the non-local variations of the module
			possibleDeps: makeDependencyGroup(
				&moduleInfo{
					variant: variant{
						name: "normal",
						variations: variationMap{
							map[string]string{
								"normal": "normal",
							},
						},
					},
				},
			),
			variations: nil,
			far:        false,
			reverse:    false,
			want:       "normal",
		},
		{
			name: "AddVariationDependencies(a)",
			// A dependency with local variations
			possibleDeps: makeDependencyGroup(
				&moduleInfo{
					variant: variant{
						name: "normal_a",
						variations: variationMap{
							map[string]string{
								"normal": "normal",
								"a":      "a",
							},
						},
					},
				},
			),
			variations: []Variation{{"a", "a"}},
			far:        false,
			reverse:    false,
			want:       "normal_a",
		},
		{
			name: "AddFarVariationDependencies(far)",
			// A dependency with far variations
			possibleDeps: makeDependencyGroup(
				&moduleInfo{
					variant: variant{
						name:       "",
						variations: variationMap{},
					},
				},
				&moduleInfo{
					variant: variant{
						name: "far",
						variations: variationMap{
							map[string]string{
								"far": "far",
							},
						},
					},
				},
			),
			variations: []Variation{{"far", "far"}},
			far:        true,
			reverse:    false,
			want:       "far",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := NewContext()
			got, _, errs := ctx.findVariant(module, nil, tt.possibleDeps, tt.variations, tt.far, tt.reverse)
			if errs != nil {
				t.Fatal(errs)
			}
			if g, w := got == nil, tt.want == "nil"; g != w {
				t.Fatalf("findVariant() got = %v, want %v", got, tt.want)
			}
			if got != nil {
				if g, w := got.String(), fmt.Sprintf("module %q variant %q", "dep", tt.want); g != w {
					t.Errorf("findVariant() got = %v, want %v", g, w)
				}
			}
		})
	}
}

func Test_parallelVisit(t *testing.T) {
	addDep := func(from, to *moduleInfo) {
		from.directDeps = append(from.directDeps, depInfo{to, nil})
		from.forwardDeps = append(from.forwardDeps, to)
		to.reverseDeps = append(to.reverseDeps, from)
	}

	create := func(name string) *moduleInfo {
		m := &moduleInfo{
			group: &moduleGroup{
				name: name,
			},
		}
		m.group.modules = moduleList{m}
		return m
	}
	moduleA := create("A")
	moduleB := create("B")
	moduleC := create("C")
	moduleD := create("D")
	moduleE := create("E")
	moduleF := create("F")
	moduleG := create("G")

	// A depends on B, B depends on C.  Nothing depends on D through G, and they don't depend on
	// anything.
	addDep(moduleA, moduleB)
	addDep(moduleB, moduleC)

	t.Run("no modules", func(t *testing.T) {
		errs := parallelVisit(slices.Values([]*moduleInfo(nil)), bottomUpVisitorImpl{}, 1,
			func(module *moduleInfo, pause chan<- pauseSpec) bool {
				panic("unexpected call to visitor")
			})
		if errs != nil {
			t.Errorf("expected no errors, got %q", errs)
		}
	})
	t.Run("bottom up", func(t *testing.T) {
		order := ""
		errs := parallelVisit(slices.Values([]*moduleInfo{moduleA, moduleB, moduleC}), bottomUpVisitorImpl{}, 1,
			func(module *moduleInfo, pause chan<- pauseSpec) bool {
				order += module.group.name
				return false
			})
		if errs != nil {
			t.Errorf("expected no errors, got %q", errs)
		}
		if g, w := order, "CBA"; g != w {
			t.Errorf("expected order %q, got %q", w, g)
		}
	})
	t.Run("pause", func(t *testing.T) {
		order := ""
		errs := parallelVisit(slices.Values([]*moduleInfo{moduleA, moduleB, moduleC, moduleD}), bottomUpVisitorImpl{}, 1,
			func(module *moduleInfo, pause chan<- pauseSpec) bool {
				if module == moduleC {
					// Pause module C on module D
					unpause := make(chan struct{})
					pause <- pauseSpec{moduleC, moduleD, unpause}
					<-unpause
				}
				order += module.group.name
				return false
			})
		if errs != nil {
			t.Errorf("expected no errors, got %q", errs)
		}
		if g, w := order, "DCBA"; g != w {
			t.Errorf("expected order %q, got %q", w, g)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		order := ""
		errs := parallelVisit(slices.Values([]*moduleInfo{moduleA, moduleB, moduleC}), bottomUpVisitorImpl{}, 1,
			func(module *moduleInfo, pause chan<- pauseSpec) bool {
				order += module.group.name
				// Cancel in module B
				return module == moduleB
			})
		if errs != nil {
			t.Errorf("expected no errors, got %q", errs)
		}
		if g, w := order, "CB"; g != w {
			t.Errorf("expected order %q, got %q", w, g)
		}
	})
	t.Run("pause and cancel", func(t *testing.T) {
		order := ""
		errs := parallelVisit(slices.Values([]*moduleInfo{moduleA, moduleB, moduleC, moduleD}), bottomUpVisitorImpl{}, 1,
			func(module *moduleInfo, pause chan<- pauseSpec) bool {
				if module == moduleC {
					// Pause module C on module D
					unpause := make(chan struct{})
					pause <- pauseSpec{moduleC, moduleD, unpause}
					<-unpause
				}
				order += module.group.name
				// Cancel in module D
				return module == moduleD
			})
		if errs != nil {
			t.Errorf("expected no errors, got %q", errs)
		}
		if g, w := order, "D"; g != w {
			t.Errorf("expected order %q, got %q", w, g)
		}
	})
	t.Run("parallel", func(t *testing.T) {
		order := ""
		errs := parallelVisit(slices.Values([]*moduleInfo{moduleA, moduleB, moduleC}), bottomUpVisitorImpl{}, 3,
			func(module *moduleInfo, pause chan<- pauseSpec) bool {
				order += module.group.name
				return false
			})
		if errs != nil {
			t.Errorf("expected no errors, got %q", errs)
		}
		if g, w := order, "CBA"; g != w {
			t.Errorf("expected order %q, got %q", w, g)
		}
	})
	t.Run("pause existing", func(t *testing.T) {
		order := ""
		errs := parallelVisit(slices.Values([]*moduleInfo{moduleA, moduleB, moduleC}), bottomUpVisitorImpl{}, 3,
			func(module *moduleInfo, pause chan<- pauseSpec) bool {
				if module == moduleA {
					// Pause module A on module B (an existing dependency)
					unpause := make(chan struct{})
					pause <- pauseSpec{moduleA, moduleB, unpause}
					<-unpause
				}
				order += module.group.name
				return false
			})
		if errs != nil {
			t.Errorf("expected no errors, got %q", errs)
		}
		if g, w := order, "CBA"; g != w {
			t.Errorf("expected order %q, got %q", w, g)
		}
	})
	t.Run("cycle", func(t *testing.T) {
		errs := parallelVisit(slices.Values([]*moduleInfo{moduleA, moduleB, moduleC}), bottomUpVisitorImpl{}, 3,
			func(module *moduleInfo, pause chan<- pauseSpec) bool {
				if module == moduleC {
					// Pause module C on module A (a dependency cycle)
					unpause := make(chan struct{})
					pause <- pauseSpec{moduleC, moduleA, unpause}
					<-unpause
				}
				return false
			})
		want := []string{
			`encountered dependency cycle`,
			`module "C" depends on module "A"`,
			`module "A" depends on module "B"`,
			`module "B" depends on module "C"`,
		}
		for i := range want {
			if len(errs) <= i {
				t.Errorf("missing error %s", want[i])
			} else if !strings.Contains(errs[i].Error(), want[i]) {
				t.Errorf("expected error %s, got %s", want[i], errs[i])
			}
		}
		if len(errs) > len(want) {
			for _, err := range errs[len(want):] {
				t.Errorf("unexpected error %s", err.Error())
			}
		}
	})
	t.Run("pause cycle", func(t *testing.T) {
		errs := parallelVisit(slices.Values([]*moduleInfo{moduleA, moduleB, moduleC, moduleD}), bottomUpVisitorImpl{}, 3,
			func(module *moduleInfo, pause chan<- pauseSpec) bool {
				if module == moduleC {
					// Pause module C on module D
					unpause := make(chan struct{})
					pause <- pauseSpec{moduleC, moduleD, unpause}
					<-unpause
				}
				if module == moduleD {
					// Pause module D on module C (a pause cycle)
					unpause := make(chan struct{})
					pause <- pauseSpec{moduleD, moduleC, unpause}
					<-unpause
				}
				return false
			})
		want := []string{
			`encountered dependency cycle`,
			`module "D" depends on module "C"`,
			`module "C" depends on module "D"`,
		}
		for i := range want {
			if len(errs) <= i {
				t.Errorf("missing error %s", want[i])
			} else if !strings.Contains(errs[i].Error(), want[i]) {
				t.Errorf("expected error %s, got %s", want[i], errs[i])
			}
		}
		if len(errs) > len(want) {
			for _, err := range errs[len(want):] {
				t.Errorf("unexpected error %s", err.Error())
			}
		}
	})
	t.Run("pause cycle with deps", func(t *testing.T) {
		pauseDeps := map[*moduleInfo]*moduleInfo{
			// F and G form a pause cycle
			moduleF: moduleG,
			moduleG: moduleF,
			// D depends on E which depends on the pause cycle, making E the first alphabetical
			// entry in pauseMap, which is not part of the cycle.
			moduleD: moduleE,
			moduleE: moduleF,
		}
		errs := parallelVisit(slices.Values([]*moduleInfo{moduleD, moduleE, moduleF, moduleG}), bottomUpVisitorImpl{}, 4,
			func(module *moduleInfo, pause chan<- pauseSpec) bool {
				if dep, ok := pauseDeps[module]; ok {
					unpause := make(chan struct{})
					pause <- pauseSpec{module, dep, unpause}
					<-unpause
				}
				return false
			})
		want := []string{
			`encountered dependency cycle`,
			`module "G" depends on module "F"`,
			`module "F" depends on module "G"`,
		}
		for i := range want {
			if len(errs) <= i {
				t.Errorf("missing error %s", want[i])
			} else if !strings.Contains(errs[i].Error(), want[i]) {
				t.Errorf("expected error %s, got %s", want[i], errs[i])
			}
		}
		if len(errs) > len(want) {
			for _, err := range errs[len(want):] {
				t.Errorf("unexpected error %s", err.Error())
			}
		}
	})
}

func TestDeduplicateOrderOnlyDeps(t *testing.T) {
	b := func(output string, inputs []string, orderOnlyDeps []string) *buildDef {
		return &buildDef{
			OutputStrings:    []string{output},
			InputStrings:     inputs,
			OrderOnlyStrings: orderOnlyDeps,
		}
	}
	m := func(bs ...*buildDef) *moduleInfo {
		return &moduleInfo{actionDefs: localBuildActions{buildDefs: bs}}
	}
	type testcase struct {
		modules        []*moduleInfo
		expectedPhonys []*buildDef
		conversions    map[string][]string
	}
	fnvHash := func(s string) string {
		hash := fnv.New64a()
		hash.Write([]byte(s))
		return strconv.FormatUint(hash.Sum64(), 16)
	}
	testCases := []testcase{{
		modules: []*moduleInfo{
			m(b("A", nil, []string{"d"})),
			m(b("B", nil, []string{"d"})),
		},
		expectedPhonys: []*buildDef{
			b("dedup-"+fnvHash("d"), []string{"d"}, nil),
		},
		conversions: map[string][]string{
			"A": []string{"dedup-" + fnvHash("d")},
			"B": []string{"dedup-" + fnvHash("d")},
		},
	}, {
		modules: []*moduleInfo{
			m(b("A", nil, []string{"a"})),
			m(b("B", nil, []string{"b"})),
		},
	}, {
		modules: []*moduleInfo{
			m(b("A", nil, []string{"a"})),
			m(b("B", nil, []string{"b"})),
			m(b("C", nil, []string{"a"})),
		},
		expectedPhonys: []*buildDef{b("dedup-"+fnvHash("a"), []string{"a"}, nil)},
		conversions: map[string][]string{
			"A": []string{"dedup-" + fnvHash("a")},
			"B": []string{"b"},
			"C": []string{"dedup-" + fnvHash("a")},
		},
	}, {
		modules: []*moduleInfo{
			m(b("A", nil, []string{"a", "b"}),
				b("B", nil, []string{"a", "b"})),
			m(b("C", nil, []string{"a", "c"}),
				b("D", nil, []string{"a", "c"})),
		},
		expectedPhonys: []*buildDef{
			b("dedup-"+fnvHash("ab"), []string{"a", "b"}, nil),
			b("dedup-"+fnvHash("ac"), []string{"a", "c"}, nil)},
		conversions: map[string][]string{
			"A": []string{"dedup-" + fnvHash("ab")},
			"B": []string{"dedup-" + fnvHash("ab")},
			"C": []string{"dedup-" + fnvHash("ac")},
			"D": []string{"dedup-" + fnvHash("ac")},
		},
	}}
	for index, tc := range testCases {
		t.Run(fmt.Sprintf("TestCase-%d", index), func(t *testing.T) {
			ctx := NewContext()
			actualPhonys := ctx.deduplicateOrderOnlyDeps(tc.modules)
			if len(actualPhonys.variables) != 0 {
				t.Errorf("No variables expected but found %v", actualPhonys.variables)
			}
			if len(actualPhonys.rules) != 0 {
				t.Errorf("No rules expected but found %v", actualPhonys.rules)
			}
			if e, a := len(tc.expectedPhonys), len(actualPhonys.buildDefs); e != a {
				t.Errorf("Expected %d build statements but got %d", e, a)
			}
			for i := 0; i < len(tc.expectedPhonys); i++ {
				a := actualPhonys.buildDefs[i]
				e := tc.expectedPhonys[i]
				if !reflect.DeepEqual(e.Outputs, a.Outputs) {
					t.Errorf("phonys expected %v but actualPhonys %v", e.Outputs, a.Outputs)
				}
				if !reflect.DeepEqual(e.Inputs, a.Inputs) {
					t.Errorf("phonys expected %v but actualPhonys %v", e.Inputs, a.Inputs)
				}
			}
			find := func(k string) *buildDef {
				for _, m := range tc.modules {
					for _, b := range m.actionDefs.buildDefs {
						if reflect.DeepEqual(b.OutputStrings, []string{k}) {
							return b
						}
					}
				}
				return nil
			}
			for k, conversion := range tc.conversions {
				actual := find(k)
				if actual == nil {
					t.Errorf("Couldn't find %s", k)
				}
				if !reflect.DeepEqual(actual.OrderOnlyStrings, conversion) {
					t.Errorf("expected %s.OrderOnly = %v but got %v", k, conversion, actual.OrderOnly)
				}
			}
		})
	}
}

func TestSourceRootDirAllowed(t *testing.T) {
	type pathCase struct {
		path           string
		decidingPrefix string
		allowed        bool
	}
	testcases := []struct {
		desc      string
		rootDirs  []string
		pathCases []pathCase
	}{
		{
			desc: "simple case",
			rootDirs: []string{
				"a",
				"b/c/d",
				"-c",
				"-d/c/a",
				"c/some_single_file",
			},
			pathCases: []pathCase{
				{
					path:           "a",
					decidingPrefix: "a",
					allowed:        true,
				},
				{
					path:           "a/b/c",
					decidingPrefix: "a",
					allowed:        true,
				},
				{
					path:           "b",
					decidingPrefix: "",
					allowed:        true,
				},
				{
					path:           "b/c/d/a",
					decidingPrefix: "b/c/d",
					allowed:        true,
				},
				{
					path:           "c",
					decidingPrefix: "c",
					allowed:        false,
				},
				{
					path:           "c/a/b",
					decidingPrefix: "c",
					allowed:        false,
				},
				{
					path:           "c/some_single_file",
					decidingPrefix: "c/some_single_file",
					allowed:        true,
				},
				{
					path:           "d/c/a/abc",
					decidingPrefix: "d/c/a",
					allowed:        false,
				},
			},
		},
		{
			desc: "root directory order matters",
			rootDirs: []string{
				"-a",
				"a/c/some_allowed_file",
				"a/b/d/some_allowed_file",
				"a/b",
				"a/c",
				"-a/b/d",
			},
			pathCases: []pathCase{
				{
					path:           "a",
					decidingPrefix: "a",
					allowed:        false,
				},
				{
					path:           "a/some_disallowed_file",
					decidingPrefix: "a",
					allowed:        false,
				},
				{
					path:           "a/c/some_allowed_file",
					decidingPrefix: "a/c/some_allowed_file",
					allowed:        true,
				},
				{
					path:           "a/b/d/some_allowed_file",
					decidingPrefix: "a/b/d/some_allowed_file",
					allowed:        true,
				},
				{
					path:           "a/b/c",
					decidingPrefix: "a/b",
					allowed:        true,
				},
				{
					path:           "a/b/c/some_allowed_file",
					decidingPrefix: "a/b",
					allowed:        true,
				},
				{
					path:           "a/b/d",
					decidingPrefix: "a/b/d",
					allowed:        false,
				},
			},
		},
	}
	for _, tc := range testcases {
		dirs := SourceRootDirs{}
		dirs.Add(tc.rootDirs...)
		for _, pc := range tc.pathCases {
			t.Run(fmt.Sprintf("%s: %s", tc.desc, pc.path), func(t *testing.T) {
				allowed, decidingPrefix := dirs.SourceRootDirAllowed(pc.path)
				if allowed != pc.allowed {
					if pc.allowed {
						t.Errorf("expected path %q to be allowed, but was not; root allowlist: %q", pc.path, tc.rootDirs)
					} else {
						t.Errorf("path %q was allowed unexpectedly; root allowlist: %q", pc.path, tc.rootDirs)
					}
				}
				if decidingPrefix != pc.decidingPrefix {
					t.Errorf("expected decidingPrefix to be %q, but got %q", pc.decidingPrefix, decidingPrefix)
				}
			})
		}
	}
}

func TestSourceRootDirs(t *testing.T) {
	root_foo_bp := `
	foo_module {
		name: "foo",
		deps: ["foo_dir1", "foo_dir_ignored_special_case"],
	}
	`
	dir1_foo_bp := `
	foo_module {
		name: "foo_dir1",
		deps: ["foo_dir_ignored"],
	}
	`
	dir_ignored_foo_bp := `
	foo_module {
		name: "foo_dir_ignored",
	}
	`
	dir_ignored_special_case_foo_bp := `
	foo_module {
		name: "foo_dir_ignored_special_case",
	}
	`
	mockFs := map[string][]byte{
		"Android.bp":                          []byte(root_foo_bp),
		"dir1/Android.bp":                     []byte(dir1_foo_bp),
		"dir_ignored/Android.bp":              []byte(dir_ignored_foo_bp),
		"dir_ignored/special_case/Android.bp": []byte(dir_ignored_special_case_foo_bp),
	}
	fileList := []string{}
	for f := range mockFs {
		fileList = append(fileList, f)
	}
	testCases := []struct {
		sourceRootDirs       []string
		expectedModuleDefs   []string
		unexpectedModuleDefs []string
		expectedErrs         []string
	}{
		{
			sourceRootDirs: []string{},
			expectedModuleDefs: []string{
				"foo",
				"foo_dir1",
				"foo_dir_ignored",
				"foo_dir_ignored_special_case",
			},
		},
		{
			sourceRootDirs: []string{"-", ""},
			unexpectedModuleDefs: []string{
				"foo",
				"foo_dir1",
				"foo_dir_ignored",
				"foo_dir_ignored_special_case",
			},
		},
		{
			sourceRootDirs: []string{"-"},
			unexpectedModuleDefs: []string{
				"foo",
				"foo_dir1",
				"foo_dir_ignored",
				"foo_dir_ignored_special_case",
			},
		},
		{
			sourceRootDirs: []string{"dir1"},
			expectedModuleDefs: []string{
				"foo",
				"foo_dir1",
				"foo_dir_ignored",
				"foo_dir_ignored_special_case",
			},
		},
		{
			sourceRootDirs: []string{"-dir1"},
			expectedModuleDefs: []string{
				"foo",
				"foo_dir_ignored",
				"foo_dir_ignored_special_case",
			},
			unexpectedModuleDefs: []string{
				"foo_dir1",
			},
			expectedErrs: []string{
				`Android.bp:2:2: module "foo" depends on skipped module "foo_dir1"; "foo_dir1" was defined in files(s) [dir1/Android.bp], but was skipped for reason(s) ["dir1/Android.bp" is a descendant of "dir1", and that path prefix was not included in PRODUCT_SOURCE_ROOT_DIRS]`,
			},
		},
		{
			sourceRootDirs: []string{"-", "dir1"},
			expectedModuleDefs: []string{
				"foo_dir1",
			},
			unexpectedModuleDefs: []string{
				"foo",
				"foo_dir_ignored",
				"foo_dir_ignored_special_case",
			},
			expectedErrs: []string{
				`dir1/Android.bp:2:2: module "foo_dir1" depends on skipped module "foo_dir_ignored"; "foo_dir_ignored" was defined in files(s) [dir_ignored/Android.bp], but was skipped for reason(s) ["dir_ignored/Android.bp" is a descendant of "", and that path prefix was not included in PRODUCT_SOURCE_ROOT_DIRS]`,
			},
		},
		{
			sourceRootDirs: []string{"-", "dir1", "dir_ignored/special_case/Android.bp"},
			expectedModuleDefs: []string{
				"foo_dir1",
				"foo_dir_ignored_special_case",
			},
			unexpectedModuleDefs: []string{
				"foo",
				"foo_dir_ignored",
			},
			expectedErrs: []string{
				"dir1/Android.bp:2:2: module \"foo_dir1\" depends on skipped module \"foo_dir_ignored\"; \"foo_dir_ignored\" was defined in files(s) [dir_ignored/Android.bp], but was skipped for reason(s) [\"dir_ignored/Android.bp\" is a descendant of \"\", and that path prefix was not included in PRODUCT_SOURCE_ROOT_DIRS]",
			},
		},
	}
	for _, tc := range testCases {
		t.Run(fmt.Sprintf(`source root dirs are %q`, tc.sourceRootDirs), func(t *testing.T) {
			ctx := NewContext()
			ctx.MockFileSystem(mockFs)
			ctx.RegisterModuleType("foo_module", newFooModule)
			ctx.RegisterBottomUpMutator("deps", depsMutator)
			ctx.AddSourceRootDirs(tc.sourceRootDirs...)
			ctx.ParseFileList(".", fileList, nil)
			_, actualErrs := ctx.ResolveDependencies(nil)

			stringErrs := []string(nil)
			for _, err := range actualErrs {
				stringErrs = append(stringErrs, err.Error())
			}
			if !reflect.DeepEqual(tc.expectedErrs, stringErrs) {
				t.Errorf("expected to find errors %v; got %v", tc.expectedErrs, stringErrs)
			}
			for _, modName := range tc.expectedModuleDefs {
				allMods := ctx.moduleGroupFromName(modName, nil)
				if allMods == nil || len(allMods.modules) != 1 {
					mods := moduleList{}
					if allMods != nil {
						mods = allMods.modules
					}
					t.Errorf("expected to find one definition for module %q, but got %v", modName, mods)
				}
			}

			for _, modName := range tc.unexpectedModuleDefs {
				allMods := ctx.moduleGroupFromName(modName, nil)
				if allMods != nil {
					t.Errorf("expected to find no definitions for module %q, but got %v", modName, allMods.modules)
				}
			}
		})
	}
}

func incrementalSetup(t *testing.T) *Context {
	ctx := NewContext()
	fileSystem := map[string][]byte{
		"Android.bp": []byte(`
			incremental_module {
					name: "MyIncrementalModule",
					deps: ["MyBarModule"],
			}

			bar_module {
					name: "MyBarModule",
			}
		`),
	}
	ctx.MockFileSystem(fileSystem)
	ctx.RegisterBottomUpMutator("deps", depsMutator)
	ctx.RegisterModuleType("incremental_module", newIncrementalModule)
	ctx.RegisterModuleType("bar_module", newBarModule)

	_, errs := ctx.ParseBlueprintsFiles("Android.bp", nil)
	if len(errs) > 0 {
		t.Errorf("unexpected parse errors:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	_, errs = ctx.ResolveDependencies(nil)
	if len(errs) > 0 {
		t.Errorf("unexpected dep errors:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	return ctx
}

func incrementalSetupForRestore(t *testing.T, orderOnlyStrings []string) (*Context, any) {
	ctx := incrementalSetup(t)
	incInfo := ctx.moduleGroupFromName("MyIncrementalModule", nil).modules.firstModule()
	barInfo := ctx.moduleGroupFromName("MyBarModule", nil).modules.firstModule()

	providerHashes := make([]uint64, len(providerRegistry))
	// Use fixed value since SetProvider hasn't been called yet, so we can't go
	// through the providers of the module.
	for k, v := range map[providerKey]any{
		IncrementalTestProviderKey.providerKey: IncrementalTestProvider{
			Value: barInfo.Name(),
		},
	} {
		hash, err := proptools.CalculateHash(v)
		if err != nil {
			panic(fmt.Sprintf("Can't hash value of providers"))
		}
		providerHashes[k.id] = hash
	}
	cacheKey := calculateHashKey(incInfo, [][]uint64{providerHashes})
	var providerValue any = IncrementalTestProvider{Value: "MyIncrementalModule"}
	toCache := BuildActionCache{
		cacheKey: &BuildActionCachedData{
			Pos: &scanner.Position{
				Filename: "Android.bp",
				Line:     2,
				Column:   4,
				Offset:   4,
			},
			Providers: []CachedProvider{{
				Id:    &IncrementalTestProviderKey.providerKey,
				Value: &providerValue,
			}},
			OrderOnlyStrings: orderOnlyStrings,
		},
	}
	ctx.SetIncrementalEnabled(true)
	ctx.SetIncrementalAnalysis(true)
	ctx.buildActionsFromCache = toCache

	return ctx, providerValue
}

func calculateHashKey(m *moduleInfo, providerHashes [][]uint64) BuildActionCacheKey {
	hash, err := proptools.CalculateHash(m.properties)
	if err != nil {
		panic(newPanicErrorf(err, "failed to calculate properties hash"))
	}
	cacheInput := new(BuildActionCacheInput)
	cacheInput.PropertiesHash = hash
	cacheInput.ProvidersHash = providerHashes
	hash, err = proptools.CalculateHash(&cacheInput)
	if err != nil {
		panic(newPanicErrorf(err, "failed to calculate cache input hash"))
	}
	return BuildActionCacheKey{
		Id:        m.ModuleCacheKey(),
		InputHash: hash,
	}
}

func TestCacheBuildActions(t *testing.T) {
	ctx := incrementalSetup(t)
	ctx.SetIncrementalEnabled(true)

	_, errs := ctx.PrepareBuildActions(nil)
	if len(errs) > 0 {
		t.Errorf("unexpected errors calling generateModuleBuildActions:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	incInfo := ctx.moduleGroupFromName("MyIncrementalModule", nil).modules.firstModule()
	barInfo := ctx.moduleGroupFromName("MyBarModule", nil).modules.firstModule()
	if len(ctx.buildActionsToCache) != 1 {
		t.Errorf("build actions are not cached for the incremental module")
	}
	cacheKey := calculateHashKey(incInfo, [][]uint64{barInfo.providerInitialValueHashes})
	cache := ctx.buildActionsToCache[cacheKey]
	if cache == nil {
		t.Errorf("failed to find cached build actions for the incremental module")
	}
	var providerValue any = IncrementalTestProvider{Value: "MyIncrementalModule"}
	expectedCache := BuildActionCachedData{
		Pos: &scanner.Position{
			Filename: "Android.bp",
			Line:     2,
			Column:   4,
			Offset:   4,
		},
		Providers: []CachedProvider{{
			Id:    &IncrementalTestProviderKey.providerKey,
			Value: &providerValue,
		}},
	}
	if !reflect.DeepEqual(expectedCache, *cache) {
		t.Errorf("expected: %v actual %v", expectedCache, *cache)
	}
}

func TestRestoreBuildActions(t *testing.T) {
	ctx, providerValue := incrementalSetupForRestore(t, nil)
	incInfo := ctx.moduleGroupFromName("MyIncrementalModule", nil).modules.firstModule()
	barInfo := ctx.moduleGroupFromName("MyBarModule", nil).modules.firstModule()
	_, errs := ctx.PrepareBuildActions(nil)
	if len(errs) > 0 {
		t.Errorf("unexpected errors calling generateModuleBuildActions:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	// Verify that the GenerateBuildActions was skipped for the incremental module
	incRerun := incInfo.logicModule.(*incrementalModule).GenerateBuildActionsCalled
	barRerun := barInfo.logicModule.(*barModule).GenerateBuildActionsCalled
	if incRerun || !barRerun {
		t.Errorf("failed to skip/rerun GenerateBuildActions: %t %t", incRerun, barRerun)
	}
	// Verify that the provider is set correctly for the incremental module
	if !reflect.DeepEqual(incInfo.providers[IncrementalTestProviderKey.id], providerValue) {
		t.Errorf("provider is not set correctly when restoring from cache")
	}
}

func TestSkipNinjaForCacheHit(t *testing.T) {
	ctx, _ := incrementalSetupForRestore(t, nil)
	_, errs := ctx.PrepareBuildActions(nil)
	if len(errs) > 0 {
		t.Errorf("unexpected errors calling generateModuleBuildActions:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	buf := bytes.NewBuffer(nil)
	w := newNinjaWriter(buf)
	ctx.writeAllModuleActions(w, true, "test.ninja")
	// Verify that soong updated the ninja file for the bar module and skipped the
	// ninja file writing of the incremental module
	file, err := ctx.fs.Open("test.0.ninja")
	if err != nil {
		t.Errorf("no ninja file for MyBarModule")
	}
	content := make([]byte, 1024)
	file.Read(content)
	if !strings.Contains(string(content), "build MyBarModule_phony_output: phony") {
		t.Errorf("ninja file doesn't have build statements for MyBarModule: %s", string(content))
	}

	file, err = ctx.fs.Open("test_incremental_ninja/.-MyIncrementalModule-none-incremental_module.ninja")
	if !os.IsNotExist(err) {
		t.Errorf("shouldn't generate ninja file for MyIncrementalModule: %s", err.Error())
	}
}

func TestNotSkipNinjaForCacheMiss(t *testing.T) {
	ctx := incrementalSetup(t)
	ctx.SetIncrementalEnabled(true)
	ctx.SetIncrementalAnalysis(true)
	_, errs := ctx.PrepareBuildActions(nil)
	if len(errs) > 0 {
		t.Errorf("unexpected errors calling generateModuleBuildActions:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	buf := bytes.NewBuffer(nil)
	w := newNinjaWriter(buf)
	ctx.writeAllModuleActions(w, true, "test.ninja")
	// Verify that soong updated the ninja files for both the bar module and the
	// incremental module
	file, err := ctx.fs.Open("test.0.ninja")
	if err != nil {
		t.Errorf("no ninja file for MyBarModule")
	}
	content := make([]byte, 1024)
	file.Read(content)
	if !strings.Contains(string(content), "build MyBarModule_phony_output: phony") {
		t.Errorf("ninja file doesn't have build statements for MyBarModule: %s", string(content))
	}

	file, err = ctx.fs.Open("test_incremental_ninja/.-MyIncrementalModule-none-incremental_module.ninja")
	if err != nil {
		t.Errorf("no ninja file for MyIncrementalModule")
	}
	file.Read(content)
	if !strings.Contains(string(content), "build MyIncrementalModule_phony_output: phony") {
		t.Errorf("ninja file doesn't have build statements for MyIncrementalModule: %s", string(content))
	}
}

func TestOrderOnlyStringsCaching(t *testing.T) {
	ctx := incrementalSetup(t)
	ctx.SetIncrementalEnabled(true)
	_, errs := ctx.PrepareBuildActions(nil)
	if len(errs) > 0 {
		t.Errorf("unexpected errors calling generateModuleBuildActions:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}
	incInfo := ctx.moduleGroupFromName("MyIncrementalModule", nil).modules.firstModule()
	barInfo := ctx.moduleGroupFromName("MyBarModule", nil).modules.firstModule()
	bDef := buildDef{
		Rule:             Phony,
		OrderOnlyStrings: []string{"test.lib"},
	}
	incInfo.actionDefs.buildDefs = append(incInfo.actionDefs.buildDefs, &bDef)
	barInfo.actionDefs.buildDefs = append(barInfo.actionDefs.buildDefs, &bDef)

	buf := bytes.NewBuffer(nil)
	w := newNinjaWriter(buf)
	ctx.writeAllModuleActions(w, true, "test.ninja")

	verifyOrderOnlyStringsCache(t, ctx, incInfo, barInfo)
}

func TestOrderOnlyStringsRestoring(t *testing.T) {
	phony := "dedup-d479e9a8133ff998"
	orderOnlyStrings := []string{phony}
	ctx, _ := incrementalSetupForRestore(t, orderOnlyStrings)
	ctx.orderOnlyStringsFromCache = make(OrderOnlyStringsCache)
	ctx.orderOnlyStringsFromCache[phony] = []string{"test.lib"}
	_, errs := ctx.PrepareBuildActions(nil)
	if len(errs) > 0 {
		t.Errorf("unexpected errors calling generateModuleBuildActions:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	buf := bytes.NewBuffer(nil)
	w := newNinjaWriter(buf)
	ctx.writeAllModuleActions(w, true, "test.ninja")

	incInfo := ctx.moduleGroupFromName("MyIncrementalModule", nil).modules.firstModule()
	barInfo := ctx.moduleGroupFromName("MyBarModule", nil).modules.firstModule()
	verifyOrderOnlyStringsCache(t, ctx, incInfo, barInfo)

	// Verify dedup-d479e9a8133ff998 is still written to the common ninja file even
	// though MyBarModule no longer uses it.
	expected := strings.Join([]string{"build", phony + ":", "phony", "test.lib"}, " ")
	if !strings.Contains(buf.String(), expected) {
		t.Errorf("phony target not found: %s", buf.String())
	}
}

func verifyOrderOnlyStringsCache(t *testing.T, ctx *Context, incInfo, barInfo *moduleInfo) {
	// Verify that soong cache all the order only strings that are used by the
	// incremental modules
	ok, key := mapContainsValue(ctx.orderOnlyStringsToCache, "test.lib")
	if !ok {
		t.Errorf("no order only strings used by incremetnal modules cached: %v", ctx.orderOnlyStringsToCache)
	}

	// Verify that the dedup-* order only strings used by MyIncrementalModule is
	// cached along with its other cached values
	cacheKey := calculateHashKey(incInfo, [][]uint64{barInfo.providerInitialValueHashes})
	cache := ctx.buildActionsToCache[cacheKey]
	if cache == nil {
		t.Errorf("failed to find cached build actions for the incremental module")
	}
	if !listContainsValue(cache.OrderOnlyStrings, key) {
		t.Errorf("no order only strings cached for MyIncrementalModule: %v", cache.OrderOnlyStrings)
	}
}

func listContainsValue[K comparable](l []K, target K) bool {
	for _, value := range l {
		if value == target {
			return true
		}
	}
	return false
}

func mapContainsValue[K comparable, V comparable](m map[K][]V, target V) (bool, K) {
	for k, v := range m {
		if listContainsValue(v, target) {
			return true, k
		}
	}
	var key K
	return false, key
}

func TestDisallowedMutatorMethods(t *testing.T) {
	testCases := []struct {
		name              string
		mutatorHandleFunc func(MutatorHandle)
		mutatorFunc       func(BottomUpMutatorContext)
		expectedPanic     string
	}{
		{
			name:              "rename",
			mutatorHandleFunc: func(handle MutatorHandle) { handle.UsesRename() },
			mutatorFunc:       func(ctx BottomUpMutatorContext) { ctx.Rename("qux") },
			expectedPanic:     "method Rename called from mutator that was not marked UsesRename",
		},
		{
			name:              "replace_dependencies",
			mutatorHandleFunc: func(handle MutatorHandle) { handle.UsesReplaceDependencies() },
			mutatorFunc:       func(ctx BottomUpMutatorContext) { ctx.ReplaceDependencies("bar") },
			expectedPanic:     "method ReplaceDependenciesIf called from mutator that was not marked UsesReplaceDependencies",
		},
		{
			name:              "replace_dependencies_if",
			mutatorHandleFunc: func(handle MutatorHandle) { handle.UsesReplaceDependencies() },
			mutatorFunc: func(ctx BottomUpMutatorContext) {
				ctx.ReplaceDependenciesIf("bar", func(from Module, tag DependencyTag, to Module) bool { return false })
			},
			expectedPanic: "method ReplaceDependenciesIf called from mutator that was not marked UsesReplaceDependencies",
		},
		{
			name:              "reverse_dependencies",
			mutatorHandleFunc: func(handle MutatorHandle) { handle.UsesReverseDependencies() },
			mutatorFunc:       func(ctx BottomUpMutatorContext) { ctx.AddReverseDependency(ctx.Module(), nil, "baz") },
			expectedPanic:     "method AddReverseDependency called from mutator that was not marked UsesReverseDependencies",
		},
		{
			name:              "create_module",
			mutatorHandleFunc: func(handle MutatorHandle) { handle.UsesCreateModule() },
			mutatorFunc: func(ctx BottomUpMutatorContext) {
				ctx.CreateModule(newFooModule, "create_module",
					&struct{ Name string }{Name: "quz"})
			},
			expectedPanic: "method CreateModule called from mutator that was not marked UsesCreateModule",
		},
	}

	runTest := func(mutatorHandleFunc func(MutatorHandle), mutatorFunc func(ctx BottomUpMutatorContext), expectedPanic string) {
		ctx := NewContext()

		ctx.MockFileSystem(map[string][]byte{
			"Android.bp": []byte(`
			foo_module {
				name: "foo",
			}

			foo_module {
				name: "bar",
				deps: ["foo"],
			}

			foo_module {
				name: "baz",
			}
		`)})

		ctx.RegisterModuleType("foo_module", newFooModule)
		ctx.RegisterBottomUpMutator("deps", depsMutator)
		handle := ctx.RegisterBottomUpMutator("mutator", func(ctx BottomUpMutatorContext) {
			if ctx.ModuleName() == "foo" {
				mutatorFunc(ctx)
			}
		})
		mutatorHandleFunc(handle)

		_, errs := ctx.ParseBlueprintsFiles("Android.bp", nil)
		if len(errs) > 0 {
			t.Errorf("unexpected parse errors:")
			for _, err := range errs {
				t.Errorf("  %s", err)
			}
			t.FailNow()
		}

		_, errs = ctx.ResolveDependencies(nil)
		if expectedPanic != "" {
			if len(errs) == 0 {
				t.Errorf("missing expected error %q", expectedPanic)
			} else if !strings.Contains(errs[0].Error(), expectedPanic) {
				t.Errorf("missing expected error %q in %q", expectedPanic, errs[0].Error())
			}
		} else if len(errs) > 0 {
			t.Errorf("unexpected dep errors:")
			for _, err := range errs {
				t.Errorf("  %s", err)
			}
			t.FailNow()
		}
	}

	noopMutatorHandleFunc := func(MutatorHandle) {}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Run("allowed", func(t *testing.T) {
				// Test that the method doesn't panic when the handle function is called.
				runTest(testCase.mutatorHandleFunc, testCase.mutatorFunc, "")
			})
			t.Run("disallowed", func(t *testing.T) {
				// Test that the method does panic with the expected error when the
				// handle function is not called.
				runTest(noopMutatorHandleFunc, testCase.mutatorFunc, testCase.expectedPanic)
			})
		})
	}

}
