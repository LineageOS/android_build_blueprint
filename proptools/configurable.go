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
	"reflect"
	"slices"
	"strconv"
	"strings"
)

type ConfigurableElements interface {
	string | bool | []string
}

type ConfigurableEvaluator interface {
	EvaluateConfiguration(condition ConfigurableCondition, property string) ConfigurableValue
	PropertyErrorf(property, fmt string, args ...interface{})
}

// configurableMarker is just so that reflection can check type of the first field of
// the struct to determine if it is a configurable struct.
type configurableMarker bool

var configurableMarkerType reflect.Type = reflect.TypeOf((*configurableMarker)(nil)).Elem()

type ConfigurableCondition struct {
	FunctionName string
	Args         []string
}

func (c *ConfigurableCondition) String() string {
	var sb strings.Builder
	sb.WriteString(c.FunctionName)
	sb.WriteRune('(')
	for i, arg := range c.Args {
		sb.WriteString(strconv.Quote(arg))
		if i < len(c.Args)-1 {
			sb.WriteString(", ")
		}
	}
	sb.WriteRune(')')
	return sb.String()
}

type configurableValueType int

const (
	configurableValueTypeString configurableValueType = iota
	configurableValueTypeBool
	configurableValueTypeUndefined
)

func (v *configurableValueType) patternType() configurablePatternType {
	switch *v {
	case configurableValueTypeString:
		return configurablePatternTypeString
	case configurableValueTypeBool:
		return configurablePatternTypeBool
	default:
		panic("unimplemented")
	}
}

func (v *configurableValueType) String() string {
	switch *v {
	case configurableValueTypeString:
		return "string"
	case configurableValueTypeBool:
		return "bool"
	case configurableValueTypeUndefined:
		return "undefined"
	default:
		panic("unimplemented")
	}
}

// ConfigurableValue represents the value of a certain condition being selected on.
// This type mostly exists to act as a sum type between string, bool, and undefined.
type ConfigurableValue struct {
	typ         configurableValueType
	stringValue string
	boolValue   bool
}

func (c *ConfigurableValue) String() string {
	switch c.typ {
	case configurableValueTypeString:
		return strconv.Quote(c.stringValue)
	case configurableValueTypeBool:
		if c.boolValue {
			return "true"
		} else {
			return "false"
		}
	case configurableValueTypeUndefined:
		return "undefined"
	default:
		panic("unimplemented")
	}
}

func ConfigurableValueString(s string) ConfigurableValue {
	return ConfigurableValue{
		typ:         configurableValueTypeString,
		stringValue: s,
	}
}

func ConfigurableValueBool(b bool) ConfigurableValue {
	return ConfigurableValue{
		typ:       configurableValueTypeBool,
		boolValue: b,
	}
}

func ConfigurableValueUndefined() ConfigurableValue {
	return ConfigurableValue{
		typ: configurableValueTypeUndefined,
	}
}

type configurablePatternType int

const (
	configurablePatternTypeString configurablePatternType = iota
	configurablePatternTypeBool
	configurablePatternTypeDefault
)

func (v *configurablePatternType) String() string {
	switch *v {
	case configurablePatternTypeString:
		return "string"
	case configurablePatternTypeBool:
		return "bool"
	case configurablePatternTypeDefault:
		return "default"
	default:
		panic("unimplemented")
	}
}

type configurablePattern struct {
	typ         configurablePatternType
	stringValue string
	boolValue   bool
}

func (p *configurablePattern) matchesValue(v ConfigurableValue) bool {
	if p.typ == configurablePatternTypeDefault {
		return true
	}
	if v.typ == configurableValueTypeUndefined {
		return false
	}
	if p.typ != v.typ.patternType() {
		return false
	}
	switch p.typ {
	case configurablePatternTypeString:
		return p.stringValue == v.stringValue
	case configurablePatternTypeBool:
		return p.boolValue == v.boolValue
	default:
		panic("unimplemented")
	}
}

func (p *configurablePattern) matchesValueType(v ConfigurableValue) bool {
	if p.typ == configurablePatternTypeDefault {
		return true
	}
	if v.typ == configurableValueTypeUndefined {
		return true
	}
	return p.typ == v.typ.patternType()
}

type configurableCase[T ConfigurableElements] struct {
	patterns []configurablePattern
	value    *T
}

func (c *configurableCase[T]) Clone() configurableCase[T] {
	return configurableCase[T]{
		patterns: slices.Clone(c.patterns),
		value:    copyConfiguredValue(c.value),
	}
}

type configurableCaseReflection interface {
	initialize(patterns []configurablePattern, value interface{})
}

var _ configurableCaseReflection = &configurableCase[string]{}

func (c *configurableCase[T]) initialize(patterns []configurablePattern, value interface{}) {
	c.patterns = patterns
	c.value = value.(*T)
}

// for the given T, return the reflect.type of configurableCase[T]
func configurableCaseType(configuredType reflect.Type) reflect.Type {
	// I don't think it's possible to do this generically with go's
	// current reflection apis unfortunately
	switch configuredType.Kind() {
	case reflect.String:
		return reflect.TypeOf(configurableCase[string]{})
	case reflect.Bool:
		return reflect.TypeOf(configurableCase[bool]{})
	case reflect.Slice:
		switch configuredType.Elem().Kind() {
		case reflect.String:
			return reflect.TypeOf(configurableCase[[]string]{})
		}
	}
	panic("unimplemented")
}

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
//	  property_b: select(soong_config_variable("my_namespace", "my_variable"), {
//	    "value_1": "bar",
//	    "value_2": "baz",
//	    default: "qux",
//	  })
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
	conditions    []ConfigurableCondition
	cases         []configurableCase[T]
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
	for i, case_ := range c.cases {
		if len(c.conditions) != len(case_.patterns) {
			evaluator.PropertyErrorf(c.propertyName, "Expected each case to have as many patterns as conditions. conditions: %d, len(cases[%d].patterns): %d", len(c.conditions), i, len(case_.patterns))
			return nil
		}
	}
	if len(c.conditions) == 0 {
		if len(c.cases) == 0 {
			return nil
		} else if len(c.cases) == 1 {
			return c.cases[0].value
		} else {
			evaluator.PropertyErrorf(c.propertyName, "Expected 0 or 1 branches in an unconfigured select, found %d", len(c.cases))
			return nil
		}
	}
	values := make([]ConfigurableValue, len(c.conditions))
	for i, condition := range c.conditions {
		values[i] = evaluator.EvaluateConfiguration(condition, c.propertyName)
	}
	foundMatch := false
	var result *T
	for _, case_ := range c.cases {
		allMatch := true
		for i, pat := range case_.patterns {
			if !pat.matchesValueType(values[i]) {
				evaluator.PropertyErrorf(c.propertyName, "Expected all branches of a select on condition %s to have type %s, found %s", c.conditions[i].String(), values[i].typ.String(), pat.typ.String())
				return nil
			}
			if !pat.matchesValue(values[i]) {
				allMatch = false
				break
			}
		}
		if allMatch && !foundMatch {
			result = case_.value
			foundMatch = true
		}
	}
	if foundMatch {
		return result
	}
	evaluator.PropertyErrorf(c.propertyName, "%s had value %s, which was not handled by the select statement", c.conditions, values)
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
	initialize(propertyName string, conditions []ConfigurableCondition, cases any)
}

var _ configurableReflection = Configurable[string]{}
var _ configurablePtrReflection = &Configurable[string]{}

func (c *Configurable[T]) initialize(propertyName string, conditions []ConfigurableCondition, cases any) {
	c.propertyName = propertyName
	c.conditions = conditions
	c.cases = cases.([]configurableCase[T])
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
	return len(c.cases) == 0
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

	conditionsCopy := make([]ConfigurableCondition, len(c.conditions))
	copy(conditionsCopy, c.conditions)

	casesCopy := make([]configurableCase[T], len(c.cases))
	for i, case_ := range c.cases {
		casesCopy[i] = case_.Clone()
	}

	return &Configurable[T]{
		propertyName:  c.propertyName,
		conditions:    conditionsCopy,
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
