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

package blueprint

import (
	"slices"
	"testing"
)

func TestTransition(t *testing.T) {
	ctx := newContext()
	ctx.MockFileSystem(map[string][]byte{
		"Android.bp": []byte(`
			transition_module {
			    name: "A",
			    deps: ["B", "C"],
				split: ["a", "b"],
			}

			transition_module {
				name: "B",
				deps: ["C"],
				outgoing: "c",
			}

			transition_module {
				name: "C",
				deps: ["D"],
			}

			transition_module {
				name: "D",
				incoming: "d",
			}
		`),
	})

	ctx.RegisterBottomUpMutator("deps", depsMutator)
	ctx.RegisterTransitionMutator("transition", transitionTestMutator{})

	ctx.RegisterModuleType("transition_module", newTransitionModule)
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

	getModule := func(name, variant string) *transitionModule {
		group := ctx.moduleGroupFromName(name, nil)
		module := group.moduleOrAliasByVariantName(variant).module()
		return module.logicModule.(*transitionModule)
	}

	checkVariants := func(name string, expectedVariants []string) {
		t.Helper()
		group := ctx.moduleGroupFromName(name, nil)
		var gotVariants []string
		for _, variant := range group.modules {
			gotVariants = append(gotVariants, variant.moduleOrAliasVariant().variations["transition"])
		}
		if !slices.Equal(expectedVariants, gotVariants) {
			t.Errorf("expected variants of %q to be %q, got %q", name, expectedVariants, gotVariants)
		}
	}

	// Module A uses Split to create a and b variants
	checkVariants("A", []string{"a", "b"})
	// Module B inherits a and b variants from A
	checkVariants("B", []string{"", "a", "b"})
	// Module C inherits a and b variants from A, but gets an outgoing c variant from B
	checkVariants("C", []string{"", "a", "b", "c"})
	// Module D always has incoming variant d
	checkVariants("D", []string{"", "d"})

	A_a := getModule("A", "a")
	A_b := getModule("A", "b")
	B_a := getModule("B", "a")
	B_b := getModule("B", "b")
	C_a := getModule("C", "a")
	C_b := getModule("C", "b")
	C_c := getModule("C", "c")
	D_d := getModule("D", "d")

	checkDeps := func(m Module, expected ...string) {
		var got []string
		ctx.VisitDirectDeps(m, func(m Module) {
			got = append(got, ctx.ModuleName(m)+"("+ctx.ModuleSubDir(m)+")")
		})
		if !slices.Equal(got, expected) {
			t.Errorf("unexpected %q dependencies, got %q expected %q",
				ctx.ModuleName(m), got, expected)
		}
	}

	checkDeps(A_a, "B(a)", "C(a)")
	checkDeps(A_b, "B(b)", "C(b)")
	checkDeps(B_a, "C(c)")
	checkDeps(B_b, "C(c)")
	checkDeps(C_a, "D(d)")
	checkDeps(C_b, "D(d)")
	checkDeps(C_c, "D(d)")
	checkDeps(D_d)

	checkMutate := func(m *transitionModule, variant string) {
		t.Helper()
		if m.properties.Mutated != variant {
			t.Errorf("unexpected mutated property in %q, expected %q got %q", m.Name(), variant, m.properties.Mutated)
		}
	}

	checkMutate(A_a, "a")
	checkMutate(A_b, "b")
	checkMutate(B_a, "a")
	checkMutate(B_b, "b")
	checkMutate(C_a, "a")
	checkMutate(C_b, "b")
	checkMutate(C_c, "c")
	checkMutate(D_d, "d")
}

type transitionTestMutator struct{}

func (transitionTestMutator) Split(ctx BaseModuleContext) []string {
	if split := ctx.Module().(*transitionModule).properties.Split; len(split) > 0 {
		return split
	}
	return []string{""}
}

func (transitionTestMutator) OutgoingTransition(ctx OutgoingTransitionContext, sourceVariation string) string {
	if outgoing := ctx.Module().(*transitionModule).properties.Outgoing; outgoing != nil {
		return *outgoing
	}
	return sourceVariation
}

func (transitionTestMutator) IncomingTransition(ctx IncomingTransitionContext, incomingVariation string) string {
	if incoming := ctx.Module().(*transitionModule).properties.Incoming; incoming != nil {
		return *incoming
	}
	return incomingVariation
}

func (transitionTestMutator) Mutate(ctx BottomUpMutatorContext, variation string) {
	ctx.Module().(*transitionModule).properties.Mutated = variation
}

type transitionModule struct {
	SimpleName
	properties struct {
		Deps     []string
		Split    []string
		Outgoing *string
		Incoming *string

		Mutated string `blueprint:"mutated"`
	}
}

func newTransitionModule() (Module, []interface{}) {
	m := &transitionModule{}
	return m, []interface{}{&m.properties, &m.SimpleName.Properties}
}

func (f *transitionModule) GenerateBuildActions(ModuleContext) {
}

func (f *transitionModule) Deps() []string {
	return f.properties.Deps
}

func (f *transitionModule) IgnoreDeps() []string {
	return nil
}
