// Copyright 2023 Google Inc. All rights reserved.
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
package proptools

import (
	"fmt"
	"reflect"
	"slices"

	"github.com/google/blueprint/parser"
)

const default_select_branch_name = "__soong_conditions_default__"

type ConfigurableElements interface {
	string | bool | []string
}

type ConfigurableEvaluator interface {
	EvaluateConfiguration(typ parser.SelectType, property, condition string) (string, bool)
	PropertyErrorf(property, fmt string, args ...interface{})
}

// configurableMarker is just so that reflection can check type of the first field of
// the struct to determine if it is a configurable struct.
type configurableMarker bool

var configurableMarkerType reflect.Type = reflect.TypeOf((*configurableMarker)(nil)).Elem()

// Configurable can wrap the type of a blueprint property,
// in order to allow select statements to be used in bp files
// for that property. For example, for the property struct:
//
//	my_props {
//	  Property_a: string,
//	  Property_b: Configurable[string],
//	}
//
// property_b can then use select statements:
//
//	my_module {
//	  property_a: "foo"
//	  property_b: select soong_config_variable: "my_namespace" "my_variable" {
//	    "value_1": "bar",
//	    "value_2": "baz",
//	    default: "qux",
//	  }
//	}
//
// The configurable property holds all the branches of the select
// statement in the bp file. To extract the final value, you must
// call Evaluate() on the configurable property.
//
// All configurable properties support being unset, so there is
// no need to use a pointer type like Configurable[*string].
type Configurable[T ConfigurableElements] struct {
	marker        configurableMarker
	propertyName  string
	typ           parser.SelectType
	condition     string
	cases         map[string]*T
	appendWrapper *appendWrapper[T]
}

// Ignore the warning about the unused marker variable, it's used via reflection
var _ configurableMarker = Configurable[string]{}.marker

// appendWrapper exists so that we can set the value of append
// from a non-pointer method receiver. (setAppend)
type appendWrapper[T ConfigurableElements] struct {
	append  Configurable[T]
	replace bool
}

// Get returns the final value for the configurable property.
// A configurable property may be unset, in which case Get will return nil.
func (c *Configurable[T]) Get(evaluator ConfigurableEvaluator) *T {
	if c == nil || c.appendWrapper == nil {
		return nil
	}
	if c.appendWrapper.replace {
		return replaceConfiguredValues(
			c.evaluateNonTransitive(evaluator),
			c.appendWrapper.append.Get(evaluator),
		)
	} else {
		return appendConfiguredValues(
			c.evaluateNonTransitive(evaluator),
			c.appendWrapper.append.Get(evaluator),
		)
	}
}

// GetOrDefault is the same as Get, but will return the provided default value if the property was unset.
func (c *Configurable[T]) GetOrDefault(evaluator ConfigurableEvaluator, defaultValue T) T {
	result := c.Get(evaluator)
	if result != nil {
		return *result
	}
	return defaultValue
}

func (c *Configurable[T]) evaluateNonTransitive(evaluator ConfigurableEvaluator) *T {
	if c.typ == parser.SelectTypeUnconfigured {
		if len(c.cases) == 0 {
			return nil
		} else if len(c.cases) != 1 {
			panic(fmt.Sprintf("Expected 0 or 1 branches in an unconfigured select, found %d", len(c.cases)))
		}
		result, ok := c.cases[default_select_branch_name]
		if !ok {
			actual := ""
			for k := range c.cases {
				actual = k
			}
			panic(fmt.Sprintf("Expected the single branch of an unconfigured select to be %q, got %q", default_select_branch_name, actual))
		}
		return result
	}
	val, defined := evaluator.EvaluateConfiguration(c.typ, c.propertyName, c.condition)
	if !defined {
		if result, ok := c.cases[default_select_branch_name]; ok {
			return result
		}
		evaluator.PropertyErrorf(c.propertyName, "%s %q was not defined", c.typ.String(), c.condition)
		return nil
	}
	if val == default_select_branch_name {
		panic("Evaluator cannot return the default branch")
	}
	if result, ok := c.cases[val]; ok {
		return result
	}
	if result, ok := c.cases[default_select_branch_name]; ok {
		return result
	}
	evaluator.PropertyErrorf(c.propertyName, "%s %q had value %q, which was not handled by the select statement", c.typ.String(), c.condition, val)
	return nil
}

func appendConfiguredValues[T ConfigurableElements](a, b *T) *T {
	if a == nil && b == nil {
		return nil
	}
	switch any(a).(type) {
	case *[]string:
		var a2 []string
		var b2 []string
		if a != nil {
			a2 = *any(a).(*[]string)
		}
		if b != nil {
			b2 = *any(b).(*[]string)
		}
		result := make([]string, len(a2)+len(b2))
		idx := 0
		for i := 0; i < len(a2); i++ {
			result[idx] = a2[i]
			idx += 1
		}
		for i := 0; i < len(b2); i++ {
			result[idx] = b2[i]
			idx += 1
		}
		return any(&result).(*T)
	case *string:
		a := String(any(a).(*string))
		b := String(any(b).(*string))
		result := a + b
		return any(&result).(*T)
	case *bool:
		// Addition of bools will OR them together. This is inherited behavior
		// from how proptools.ExtendBasicType works with non-configurable bools.
		result := false
		if a != nil {
			result = result || *any(a).(*bool)
		}
		if b != nil {
			result = result || *any(b).(*bool)
		}
		return any(&result).(*T)
	default:
		panic("Should be unreachable")
	}
}

func replaceConfiguredValues[T ConfigurableElements](a, b *T) *T {
	if b != nil {
		return b
	}
	return a
}

// configurableReflection is an interface that exposes some methods that are
// helpful when working with reflect.Values of Configurable objects, used by
// the property unpacking code. You can't call unexported methods from reflection,
// (at least without unsafe pointer trickery) so this is the next best thing.
type configurableReflection interface {
	setAppend(append any, replace bool)
	configuredType() reflect.Type
	cloneToReflectValuePtr() reflect.Value
	isEmpty() bool
}

// Same as configurableReflection, but since initialize needs to take a pointer
// to a Configurable, it was broken out into a separate interface.
type configurablePtrReflection interface {
	initialize(propertyName string, typ parser.SelectType, condition string, cases any)
}

var _ configurableReflection = Configurable[string]{}
var _ configurablePtrReflection = &Configurable[string]{}

func (c *Configurable[T]) initialize(propertyName string, typ parser.SelectType, condition string, cases any) {
	c.propertyName = propertyName
	c.typ = typ
	c.condition = condition
	c.cases = cases.(map[string]*T)
	c.appendWrapper = &appendWrapper[T]{}
}

func (c Configurable[T]) setAppend(append any, replace bool) {
	if c.appendWrapper.append.isEmpty() {
		c.appendWrapper.append = append.(Configurable[T])
		c.appendWrapper.replace = replace
	} else {
		c.appendWrapper.append.setAppend(append, replace)
	}
}

func (c Configurable[T]) isEmpty() bool {
	if c.appendWrapper != nil && !c.appendWrapper.append.isEmpty() {
		return false
	}
	return c.typ == parser.SelectTypeUnconfigured && len(c.cases) == 0
}

func (c Configurable[T]) configuredType() reflect.Type {
	return reflect.TypeOf((*T)(nil)).Elem()
}

func (c Configurable[T]) cloneToReflectValuePtr() reflect.Value {
	return reflect.ValueOf(c.clone())
}

func (c *Configurable[T]) clone() *Configurable[T] {
	if c == nil {
		return nil
	}
	var inner *appendWrapper[T]
	if c.appendWrapper != nil {
		inner = &appendWrapper[T]{}
		if !c.appendWrapper.append.isEmpty() {
			inner.append = *c.appendWrapper.append.clone()
			inner.replace = c.appendWrapper.replace
		}
	}

	casesCopy := make(map[string]*T, len(c.cases))
	for k, v := range c.cases {
		casesCopy[k] = copyConfiguredValue(v)
	}

	return &Configurable[T]{
		propertyName:  c.propertyName,
		typ:           c.typ,
		condition:     c.condition,
		cases:         casesCopy,
		appendWrapper: inner,
	}
}

func copyConfiguredValue[T ConfigurableElements](t *T) *T {
	if t == nil {
		return nil
	}
	switch t2 := any(*t).(type) {
	case []string:
		result := any(slices.Clone(t2)).(T)
		return &result
	default:
		return t
	}
}
