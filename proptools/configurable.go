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
	"strconv"
	"strings"

	"github.com/google/blueprint/optional"
	"github.com/google/blueprint/parser"
)

// ConfigurableOptional is the same as ShallowOptional, but we use this separate
// name to reserve the ability to switch to an alternative implementation later.
type ConfigurableOptional[T any] struct {
	shallowOptional optional.ShallowOptional[T]
}

// IsPresent returns true if the optional contains a value
func (o *ConfigurableOptional[T]) IsPresent() bool {
	return o.shallowOptional.IsPresent()
}

// IsEmpty returns true if the optional does not have a value
func (o *ConfigurableOptional[T]) IsEmpty() bool {
	return o.shallowOptional.IsEmpty()
}

// Get() returns the value inside the optional. It panics if IsEmpty() returns true
func (o *ConfigurableOptional[T]) Get() T {
	return o.shallowOptional.Get()
}

// GetOrDefault() returns the value inside the optional if IsPresent() returns true,
// or the provided value otherwise.
func (o *ConfigurableOptional[T]) GetOrDefault(other T) T {
	return o.shallowOptional.GetOrDefault(other)
}

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

// ConfigurableCondition represents a condition that is being selected on, like
// arch(), os(), soong_config_variable("namespace", "variable"), or other variables.
// It's represented generically as a function name + arguments in blueprint, soong
// interprets the function name and args into specific variable values.
//
// ConfigurableCondition is treated as an immutable object so that it may be shared
// between different configurable properties.
type ConfigurableCondition struct {
	functionName string
	args         []string
}

func NewConfigurableCondition(functionName string, args []string) ConfigurableCondition {
	return ConfigurableCondition{
		functionName: functionName,
		args:         slices.Clone(args),
	}
}

func (c ConfigurableCondition) FunctionName() string {
	return c.functionName
}

func (c ConfigurableCondition) NumArgs() int {
	return len(c.args)
}

func (c ConfigurableCondition) Arg(i int) string {
	return c.args[i]
}

func (c *ConfigurableCondition) String() string {
	var sb strings.Builder
	sb.WriteString(c.functionName)
	sb.WriteRune('(')
	for i, arg := range c.args {
		sb.WriteString(strconv.Quote(arg))
		if i < len(c.args)-1 {
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

func (c *ConfigurableValue) toExpression() parser.Expression {
	switch c.typ {
	case configurableValueTypeBool:
		return &parser.Bool{Value: c.boolValue}
	case configurableValueTypeString:
		return &parser.String{Value: c.stringValue}
	default:
		panic(fmt.Sprintf("Unhandled configurableValueType: %s", c.typ.String()))
	}
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
	configurablePatternTypeAny
)

func (v *configurablePatternType) String() string {
	switch *v {
	case configurablePatternTypeString:
		return "string"
	case configurablePatternTypeBool:
		return "bool"
	case configurablePatternTypeDefault:
		return "default"
	case configurablePatternTypeAny:
		return "any"
	default:
		panic("unimplemented")
	}
}

// ConfigurablePattern represents a concrete value for a ConfigurableCase.
// Currently this just means the value of whatever variable is being looked
// up with the ConfigurableCase, but in the future it may be expanded to
// match multiple values (e.g. ranges of integers like 3..7).
//
// ConfigurablePattern can represent different types of values, like
// strings vs bools.
//
// ConfigurablePattern must be immutable so it can be shared between
// different configurable properties.
type ConfigurablePattern struct {
	typ         configurablePatternType
	stringValue string
	boolValue   bool
	binding     string
}

func NewStringConfigurablePattern(s string) ConfigurablePattern {
	return ConfigurablePattern{
		typ:         configurablePatternTypeString,
		stringValue: s,
	}
}

func NewBoolConfigurablePattern(b bool) ConfigurablePattern {
	return ConfigurablePattern{
		typ:       configurablePatternTypeBool,
		boolValue: b,
	}
}

func NewDefaultConfigurablePattern() ConfigurablePattern {
	return ConfigurablePattern{
		typ: configurablePatternTypeDefault,
	}
}

func (p *ConfigurablePattern) matchesValue(v ConfigurableValue) bool {
	if p.typ == configurablePatternTypeDefault {
		return true
	}
	if v.typ == configurableValueTypeUndefined {
		return false
	}
	if p.typ == configurablePatternTypeAny {
		return true
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

func (p *ConfigurablePattern) matchesValueType(v ConfigurableValue) bool {
	if p.typ == configurablePatternTypeDefault {
		return true
	}
	if v.typ == configurableValueTypeUndefined {
		return true
	}
	if p.typ == configurablePatternTypeAny {
		return true
	}
	return p.typ == v.typ.patternType()
}

// ConfigurableCase represents a set of ConfigurablePatterns
// (exactly 1 pattern per ConfigurableCase), and a value to use
// if all of the patterns are matched.
//
// ConfigurableCase must be immutable so it can be shared between
// different configurable properties.
type ConfigurableCase[T ConfigurableElements] struct {
	patterns []ConfigurablePattern
	value    parser.Expression
}

type configurableCaseReflection interface {
	initialize(patterns []ConfigurablePattern, value parser.Expression)
}

var _ configurableCaseReflection = &ConfigurableCase[string]{}

func NewConfigurableCase[T ConfigurableElements](patterns []ConfigurablePattern, value *T) ConfigurableCase[T] {
	var valueExpr parser.Expression
	if value == nil {
		valueExpr = &parser.UnsetProperty{}
	} else {
		switch v := any(value).(type) {
		case *string:
			valueExpr = &parser.String{Value: *v}
		case *bool:
			valueExpr = &parser.Bool{Value: *v}
		case *[]string:
			innerValues := make([]parser.Expression, 0, len(*v))
			for _, x := range *v {
				innerValues = append(innerValues, &parser.String{Value: x})
			}
			valueExpr = &parser.List{Values: innerValues}
		default:
			panic(fmt.Sprintf("should be unreachable due to the ConfigurableElements restriction: %#v", value))
		}
	}
	// Clone the values so they can't be modified from soong
	patterns = slices.Clone(patterns)
	return ConfigurableCase[T]{
		patterns: patterns,
		value:    valueExpr,
	}
}

func (c *ConfigurableCase[T]) initialize(patterns []ConfigurablePattern, value parser.Expression) {
	c.patterns = patterns
	c.value = value
}

// for the given T, return the reflect.type of configurableCase[T]
func configurableCaseType(configuredType reflect.Type) reflect.Type {
	// I don't think it's possible to do this generically with go's
	// current reflection apis unfortunately
	switch configuredType.Kind() {
	case reflect.String:
		return reflect.TypeOf(ConfigurableCase[string]{})
	case reflect.Bool:
		return reflect.TypeOf(ConfigurableCase[bool]{})
	case reflect.Slice:
		switch configuredType.Elem().Kind() {
		case reflect.String:
			return reflect.TypeOf(ConfigurableCase[[]string]{})
		}
	}
	panic("unimplemented")
}

// for the given T, return the reflect.type of Configurable[T]
func configurableType(configuredType reflect.Type) (reflect.Type, error) {
	// I don't think it's possible to do this generically with go's
	// current reflection apis unfortunately
	switch configuredType.Kind() {
	case reflect.String:
		return reflect.TypeOf(Configurable[string]{}), nil
	case reflect.Bool:
		return reflect.TypeOf(Configurable[bool]{}), nil
	case reflect.Slice:
		switch configuredType.Elem().Kind() {
		case reflect.String:
			return reflect.TypeOf(Configurable[[]string]{}), nil
		}
	}
	return nil, fmt.Errorf("configurable structs can only contain strings, bools, or string slices, found %s", configuredType.String())
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
	marker       configurableMarker
	propertyName string
	inner        *configurableInner[T]
	// See Configurable.evaluate for a description of the postProcessor algorithm and
	// why this is a 2d list
	postProcessors *[][]postProcessor[T]
}

type postProcessor[T ConfigurableElements] struct {
	f func(T) T
	// start and end represent the range of configurableInners
	// that this postprocessor is applied to. When appending two configurables
	// together, the start and end values will stay the same for the left
	// configurable's postprocessors, but the rights will be rebased by the
	// number of configurableInners in the left configurable. This way
	// the postProcessors still only apply to the configurableInners they
	// origionally applied to before the appending.
	start int
	end   int
}

type configurableInner[T ConfigurableElements] struct {
	single  singleConfigurable[T]
	replace bool
	next    *configurableInner[T]
}

// singleConfigurable must be immutable so it can be reused
// between multiple configurables
type singleConfigurable[T ConfigurableElements] struct {
	conditions []ConfigurableCondition
	cases      []ConfigurableCase[T]
	scope      *parser.Scope
}

// Ignore the warning about the unused marker variable, it's used via reflection
var _ configurableMarker = Configurable[string]{}.marker

func NewConfigurable[T ConfigurableElements](conditions []ConfigurableCondition, cases []ConfigurableCase[T]) Configurable[T] {
	for _, c := range cases {
		if len(c.patterns) != len(conditions) {
			panic(fmt.Sprintf("All configurables cases must have as many patterns as the configurable has conditions. Expected: %d, found: %d", len(conditions), len(c.patterns)))
		}
	}
	// Clone the slices so they can't be modified from soong
	conditions = slices.Clone(conditions)
	cases = slices.Clone(cases)
	var zeroPostProcessors [][]postProcessor[T]
	return Configurable[T]{
		inner: &configurableInner[T]{
			single: singleConfigurable[T]{
				conditions: conditions,
				cases:      cases,
			},
		},
		postProcessors: &zeroPostProcessors,
	}
}

func newConfigurableWithPropertyName[T ConfigurableElements](propertyName string, conditions []ConfigurableCondition, cases []ConfigurableCase[T], addScope bool) Configurable[T] {
	result := NewConfigurable(conditions, cases)
	result.propertyName = propertyName
	if addScope {
		for curr := result.inner; curr != nil; curr = curr.next {
			curr.single.scope = parser.NewScope(nil)
		}
	}
	return result
}

func (c *Configurable[T]) AppendSimpleValue(value T) {
	value = copyConfiguredValue(value)
	// This may be a property that was never initialized from a bp file
	if c.inner == nil {
		c.initialize(nil, "", nil, []ConfigurableCase[T]{{
			value: configuredValueToExpression(value),
		}})
		return
	}
	c.inner.appendSimpleValue(value)
}

// AddPostProcessor adds a function that will modify the result of
// Get() when Get() is called. It operates on all the current contents
// of the Configurable property, but if other values are appended to
// the Configurable property afterwards, the postProcessor will not run
// on them. This can be useful to essentially modify a configurable
// property without evaluating it.
func (c *Configurable[T]) AddPostProcessor(p func(T) T) {
	// Add the new postProcessor on top of the tallest stack of postProcessors.
	// See Configurable.evaluate for more details on the postProcessors algorithm
	// and data structure.
	num_links := c.inner.numLinks()
	if c.postProcessors == nil || len(*c.postProcessors) == 0 {
		c.postProcessors = &[][]postProcessor[T]{{{
			f:     p,
			start: 0,
			end:   num_links,
		}}}
	} else {
		deepestI := 0
		deepestDepth := 0
		for i := 0; i < len(*c.postProcessors); i++ {
			if len((*c.postProcessors)[i]) > deepestDepth {
				deepestDepth = len((*c.postProcessors)[i])
				deepestI = i
			}
		}
		(*c.postProcessors)[deepestI] = append((*c.postProcessors)[deepestI], postProcessor[T]{
			f:     p,
			start: 0,
			end:   num_links,
		})
	}
}

// Get returns the final value for the configurable property.
// A configurable property may be unset, in which case Get will return nil.
func (c *Configurable[T]) Get(evaluator ConfigurableEvaluator) ConfigurableOptional[T] {
	result := c.evaluate(c.propertyName, evaluator)
	return configuredValuePtrToOptional(result)
}

// GetOrDefault is the same as Get, but will return the provided default value if the property was unset.
func (c *Configurable[T]) GetOrDefault(evaluator ConfigurableEvaluator, defaultValue T) T {
	result := c.evaluate(c.propertyName, evaluator)
	if result != nil {
		// Copy the result so that it can't be changed from soong
		return copyConfiguredValue(*result)
	}
	return defaultValue
}

type valueAndIndices[T ConfigurableElements] struct {
	value   *T
	replace bool
	// Similar to start/end in postProcessor, these represent the origional
	// range or configurableInners that this merged group represents. It's needed
	// in order to apply recursive postProcessors to only the relevant
	// configurableInners, even after those configurableInners have been merged
	// in order to apply an earlier postProcessor.
	start int
	end   int
}

func (c *Configurable[T]) evaluate(propertyName string, evaluator ConfigurableEvaluator) *T {
	if c.inner == nil {
		return nil
	}

	if len(*c.postProcessors) == 0 {
		// Use a simpler algorithm if there are no postprocessors
		return c.inner.evaluate(propertyName, evaluator)
	}

	// The basic idea around evaluating with postprocessors is that each individual
	// node in the chain (each configurableInner) is first evaluated, and then when
	// a postprocessor operates on a certain range, that range is merged before passing
	// it to the postprocessor. We want postProcessors to only accept a final merged
	// value instead of a linked list, but at the same time, only operate over a portion
	// of the list. If more configurables are appended onto this one, their values won't
	// be operated on by the existing postProcessors, but they may have their own
	// postprocessors.
	//
	// _____________________
	// |         __________|
	// ______    |    _____|        ___
	// |    |         |    |        | |
	// a -> b -> c -> d -> e -> f -> g
	//
	// In this diagram, the letters along the bottom is the chain of configurableInners.
	// The brackets on top represent postprocessors, where higher brackets are processed
	// after lower ones.
	//
	// To evaluate this example, first we evaluate the raw values for all nodes a->g.
	// Then we merge nodes a/b and d/e and apply the postprocessors to their merged values,
	// and also to g. Those merged and postprocessed nodes are then reinserted into the
	// list, and we move on to doing the higher level postprocessors (starting with the c->e one)
	// in the same way. When all postprocessors are done, a final merge is done on anything
	// leftover.
	//
	// The Configurable.postProcessors field is a 2d array to represent this hierarchy.
	// The outer index moves right on this graph, the inner index goes up.
	// When adding a new postProcessor, it will always be the last postProcessor to run
	// until another is added or another configurable is appended. So in AddPostProcessor(),
	// we add it to the tallest existing stack.

	var currentValues []valueAndIndices[T]
	for curr, i := c.inner, 0; curr != nil; curr, i = curr.next, i+1 {
		value := curr.single.evaluateNonTransitive(propertyName, evaluator)
		currentValues = append(currentValues, valueAndIndices[T]{
			value:   value,
			replace: curr.replace,
			start:   i,
			end:     i + 1,
		})
	}

	if c.postProcessors == nil || len(*c.postProcessors) == 0 {
		return mergeValues(currentValues).value
	}

	foundPostProcessor := true
	for depth := 0; foundPostProcessor; depth++ {
		foundPostProcessor = false
		var newValues []valueAndIndices[T]
		i := 0
		for _, postProcessorGroup := range *c.postProcessors {
			if len(postProcessorGroup) > depth {
				foundPostProcessor = true
				postProcessor := postProcessorGroup[depth]
				startI := 0
				endI := 0
				for currentValues[startI].start < postProcessor.start {
					startI++
				}
				for currentValues[endI].end < postProcessor.end {
					endI++
				}
				endI++
				newValues = append(newValues, currentValues[i:startI]...)
				merged := mergeValues(currentValues[startI:endI])
				if merged.value != nil {
					processed := postProcessor.f(*merged.value)
					merged.value = &processed
				}
				newValues = append(newValues, merged)
				i = endI
			}
		}
		newValues = append(newValues, currentValues[i:]...)
		currentValues = newValues
	}

	return mergeValues(currentValues).value
}

func mergeValues[T ConfigurableElements](values []valueAndIndices[T]) valueAndIndices[T] {
	if len(values) < 0 {
		panic("Expected at least 1 value in mergeValues")
	}
	result := values[0]
	for i := 1; i < len(values); i++ {
		if result.replace {
			result.value = replaceConfiguredValues(result.value, values[i].value)
		} else {
			result.value = appendConfiguredValues(result.value, values[i].value)
		}
		result.end = values[i].end
		result.replace = values[i].replace
	}
	return result
}

func (c *configurableInner[T]) evaluate(propertyName string, evaluator ConfigurableEvaluator) *T {
	if c == nil {
		return nil
	}
	if c.next == nil {
		return c.single.evaluateNonTransitive(propertyName, evaluator)
	}
	if c.replace {
		return replaceConfiguredValues(
			c.single.evaluateNonTransitive(propertyName, evaluator),
			c.next.evaluate(propertyName, evaluator),
		)
	} else {
		return appendConfiguredValues(
			c.single.evaluateNonTransitive(propertyName, evaluator),
			c.next.evaluate(propertyName, evaluator),
		)
	}
}

func (c *singleConfigurable[T]) evaluateNonTransitive(propertyName string, evaluator ConfigurableEvaluator) *T {
	for i, case_ := range c.cases {
		if len(c.conditions) != len(case_.patterns) {
			evaluator.PropertyErrorf(propertyName, "Expected each case to have as many patterns as conditions. conditions: %d, len(cases[%d].patterns): %d", len(c.conditions), i, len(case_.patterns))
			return nil
		}
	}
	if len(c.conditions) == 0 {
		if len(c.cases) == 0 {
			return nil
		} else if len(c.cases) == 1 {
			if result, err := expressionToConfiguredValue[T](c.cases[0].value, c.scope); err != nil {
				evaluator.PropertyErrorf(propertyName, "%s", err.Error())
				return nil
			} else {
				return result
			}
		} else {
			evaluator.PropertyErrorf(propertyName, "Expected 0 or 1 branches in an unconfigured select, found %d", len(c.cases))
			return nil
		}
	}
	values := make([]ConfigurableValue, len(c.conditions))
	for i, condition := range c.conditions {
		values[i] = evaluator.EvaluateConfiguration(condition, propertyName)
	}
	foundMatch := false
	nonMatchingIndex := 0
	var result *T
	for _, case_ := range c.cases {
		allMatch := true
		for i, pat := range case_.patterns {
			if !pat.matchesValueType(values[i]) {
				evaluator.PropertyErrorf(propertyName, "Expected all branches of a select on condition %s to have type %s, found %s", c.conditions[i].String(), values[i].typ.String(), pat.typ.String())
				return nil
			}
			if !pat.matchesValue(values[i]) {
				allMatch = false
				nonMatchingIndex = i
				break
			}
		}
		if allMatch && !foundMatch {
			newScope := createScopeWithBindings(c.scope, case_.patterns, values)
			if r, err := expressionToConfiguredValue[T](case_.value, newScope); err != nil {
				evaluator.PropertyErrorf(propertyName, "%s", err.Error())
				return nil
			} else {
				result = r
			}
			foundMatch = true
		}
	}
	if foundMatch {
		return result
	}

	evaluator.PropertyErrorf(propertyName, "%s had value %s, which was not handled by the select statement", c.conditions[nonMatchingIndex].String(), values[nonMatchingIndex].String())
	return nil
}

func createScopeWithBindings(parent *parser.Scope, patterns []ConfigurablePattern, values []ConfigurableValue) *parser.Scope {
	result := parent
	for i, pattern := range patterns {
		if pattern.binding != "" {
			if result == parent {
				result = parser.NewScope(parent)
			}
			err := result.HandleAssignment(&parser.Assignment{
				Name:     pattern.binding,
				Value:    values[i].toExpression(),
				Assigner: "=",
			})
			if err != nil {
				// This shouldn't happen due to earlier validity checks
				panic(err.Error())
			}
		}
	}
	return result
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
	setAppend(append any, replace bool, prepend bool)
	configuredType() reflect.Type
	clone() any
	isEmpty() bool
	printfInto(value string) error
}

// Same as configurableReflection, but since initialize needs to take a pointer
// to a Configurable, it was broken out into a separate interface.
type configurablePtrReflection interface {
	initialize(scope *parser.Scope, propertyName string, conditions []ConfigurableCondition, cases any)
}

var _ configurableReflection = Configurable[string]{}
var _ configurablePtrReflection = &Configurable[string]{}

func (c *Configurable[T]) initialize(scope *parser.Scope, propertyName string, conditions []ConfigurableCondition, cases any) {
	c.propertyName = propertyName
	c.inner = &configurableInner[T]{
		single: singleConfigurable[T]{
			conditions: conditions,
			cases:      cases.([]ConfigurableCase[T]),
			scope:      scope,
		},
	}
	var postProcessors [][]postProcessor[T]
	c.postProcessors = &postProcessors
}

func (c *Configurable[T]) Append(other Configurable[T]) {
	c.setAppend(other, false, false)
}

func (c Configurable[T]) setAppend(append any, replace bool, prepend bool) {
	a := append.(Configurable[T])
	if a.inner.isEmpty() {
		return
	}

	if prepend {
		newBase := a.inner.numLinks()
		*c.postProcessors = appendPostprocessors(*a.postProcessors, *c.postProcessors, newBase)
	} else {
		newBase := c.inner.numLinks()
		*c.postProcessors = appendPostprocessors(*c.postProcessors, *a.postProcessors, newBase)
	}

	c.inner.setAppend(a.inner, replace, prepend)
	if c.inner == c.inner.next {
		panic("pointer loop")
	}
}

func appendPostprocessors[T ConfigurableElements](a, b [][]postProcessor[T], newBase int) [][]postProcessor[T] {
	var result [][]postProcessor[T]
	for i := 0; i < len(a); i++ {
		result = append(result, slices.Clone(a[i]))
	}
	for i := 0; i < len(b); i++ {
		n := slices.Clone(b[i])
		for j := 0; j < len(n); j++ {
			n[j].start += newBase
			n[j].end += newBase
		}
		result = append(result, n)
	}
	return result
}

func (c *configurableInner[T]) setAppend(append *configurableInner[T], replace bool, prepend bool) {
	if c.isEmpty() {
		*c = *append.clone()
	} else if prepend {
		if replace && c.alwaysHasValue() {
			// The current value would always override the prepended value, so don't do anything
			return
		}
		// We're going to replace the head node with the one from append, so allocate
		// a new one here.
		old := &configurableInner[T]{
			single:  c.single,
			replace: c.replace,
			next:    c.next,
		}
		*c = *append.clone()
		curr := c
		for curr.next != nil {
			curr = curr.next
		}
		curr.next = old
		curr.replace = replace
	} else {
		// If we're replacing with something that always has a value set,
		// we can optimize the code by replacing our entire append chain here.
		if replace && append.alwaysHasValue() {
			*c = *append.clone()
		} else {
			curr := c
			for curr.next != nil {
				curr = curr.next
			}
			curr.next = append.clone()
			curr.replace = replace
		}
	}
}

func (c *configurableInner[T]) numLinks() int {
	result := 0
	for curr := c; curr != nil; curr = curr.next {
		result++
	}
	return result
}

func (c *configurableInner[T]) appendSimpleValue(value T) {
	if c.next == nil {
		c.replace = false
		c.next = &configurableInner[T]{
			single: singleConfigurable[T]{
				cases: []ConfigurableCase[T]{{
					value: configuredValueToExpression(value),
				}},
			},
		}
	} else {
		c.next.appendSimpleValue(value)
	}
}

func (c Configurable[T]) printfInto(value string) error {
	return c.inner.printfInto(value)
}

func (c *configurableInner[T]) printfInto(value string) error {
	for c != nil {
		if err := c.single.printfInto(value); err != nil {
			return err
		}
		c = c.next
	}
	return nil
}

func (c *singleConfigurable[T]) printfInto(value string) error {
	for _, c := range c.cases {
		if c.value == nil {
			continue
		}
		if err := c.value.PrintfInto(value); err != nil {
			return err
		}
	}
	return nil
}

func (c Configurable[T]) clone() any {
	var newPostProcessors *[][]postProcessor[T]
	if c.postProcessors != nil {
		x := appendPostprocessors(*c.postProcessors, nil, 0)
		newPostProcessors = &x
	}
	return Configurable[T]{
		propertyName:   c.propertyName,
		inner:          c.inner.clone(),
		postProcessors: newPostProcessors,
	}
}

func (c Configurable[T]) Clone() Configurable[T] {
	return c.clone().(Configurable[T])
}

func (c *configurableInner[T]) clone() *configurableInner[T] {
	if c == nil {
		return nil
	}
	return &configurableInner[T]{
		// We don't need to clone the singleConfigurable because
		// it's supposed to be immutable
		single:  c.single,
		replace: c.replace,
		next:    c.next.clone(),
	}
}

func (c *configurableInner[T]) isEmpty() bool {
	if c == nil {
		return true
	}
	if !c.single.isEmpty() {
		return false
	}
	return c.next.isEmpty()
}

func (c Configurable[T]) isEmpty() bool {
	return c.inner.isEmpty()
}

func (c *singleConfigurable[T]) isEmpty() bool {
	if c == nil {
		return true
	}
	if len(c.cases) > 1 {
		return false
	}
	if len(c.cases) == 1 && c.cases[0].value != nil {
		if _, ok := c.cases[0].value.(*parser.UnsetProperty); ok {
			return true
		}
		return false
	}
	return true
}

func (c *configurableInner[T]) alwaysHasValue() bool {
	for curr := c; curr != nil; curr = curr.next {
		if curr.single.alwaysHasValue() {
			return true
		}
	}
	return false
}

func (c *singleConfigurable[T]) alwaysHasValue() bool {
	if len(c.cases) == 0 {
		return false
	}
	for _, c := range c.cases {
		if _, isUnset := c.value.(*parser.UnsetProperty); isUnset || c.value == nil {
			return false
		}
	}
	return true
}

func (c Configurable[T]) configuredType() reflect.Type {
	return reflect.TypeOf((*T)(nil)).Elem()
}

func expressionToConfiguredValue[T ConfigurableElements](expr parser.Expression, scope *parser.Scope) (*T, error) {
	expr, err := expr.Eval(scope)
	if err != nil {
		return nil, err
	}
	switch e := expr.(type) {
	case *parser.UnsetProperty:
		return nil, nil
	case *parser.String:
		if result, ok := any(&e.Value).(*T); ok {
			return result, nil
		} else {
			return nil, fmt.Errorf("can't assign string value to %s property", configuredTypeToString[T]())
		}
	case *parser.Bool:
		if result, ok := any(&e.Value).(*T); ok {
			return result, nil
		} else {
			return nil, fmt.Errorf("can't assign bool value to %s property", configuredTypeToString[T]())
		}
	case *parser.List:
		result := make([]string, 0, len(e.Values))
		for _, x := range e.Values {
			if y, ok := x.(*parser.String); ok {
				result = append(result, y.Value)
			} else {
				return nil, fmt.Errorf("expected list of strings but found list of %s", x.Type())
			}
		}
		if result, ok := any(&result).(*T); ok {
			return result, nil
		} else {
			return nil, fmt.Errorf("can't assign list of strings to list of %s property", configuredTypeToString[T]())
		}
	default:
		// If the expression was not evaluated beforehand we could hit this error even when the types match,
		// but that's an internal logic error.
		return nil, fmt.Errorf("expected %s but found %s (%#v)", configuredTypeToString[T](), expr.Type().String(), expr)
	}
}

func configuredValueToExpression[T ConfigurableElements](value T) parser.Expression {
	switch v := any(value).(type) {
	case string:
		return &parser.String{Value: v}
	case bool:
		return &parser.Bool{Value: v}
	case []string:
		values := make([]parser.Expression, 0, len(v))
		for _, x := range v {
			values = append(values, &parser.String{Value: x})
		}
		return &parser.List{Values: values}
	default:
		panic("unhandled type in configuredValueToExpression")
	}
}

func configuredTypeToString[T ConfigurableElements]() string {
	var zero T
	switch any(zero).(type) {
	case string:
		return "string"
	case bool:
		return "bool"
	case []string:
		return "list of strings"
	default:
		panic("should be unreachable")
	}
}

func copyConfiguredValue[T ConfigurableElements](t T) T {
	switch t2 := any(t).(type) {
	case []string:
		return any(slices.Clone(t2)).(T)
	default:
		return t
	}
}

func configuredValuePtrToOptional[T ConfigurableElements](t *T) ConfigurableOptional[T] {
	if t == nil {
		return ConfigurableOptional[T]{optional.NewShallowOptional(t)}
	}
	switch t2 := any(*t).(type) {
	case []string:
		result := any(slices.Clone(t2)).(T)
		return ConfigurableOptional[T]{optional.NewShallowOptional(&result)}
	default:
		return ConfigurableOptional[T]{optional.NewShallowOptional(t)}
	}
}

// PrintfIntoConfigurable replaces %s occurrences in strings in Configurable properties
// with the provided string value. It's intention is to support soong config value variables
// on Configurable properties.
func PrintfIntoConfigurable(c any, value string) error {
	return c.(configurableReflection).printfInto(value)
}

func promoteValueToConfigurable(origional reflect.Value) reflect.Value {
	var expr parser.Expression
	var kind reflect.Kind
	if origional.Kind() == reflect.Pointer && origional.IsNil() {
		expr = &parser.UnsetProperty{}
		kind = origional.Type().Elem().Kind()
	} else {
		if origional.Kind() == reflect.Pointer {
			origional = origional.Elem()
		}
		kind = origional.Kind()
		switch kind {
		case reflect.String:
			expr = &parser.String{Value: origional.String()}
		case reflect.Bool:
			expr = &parser.Bool{Value: origional.Bool()}
		case reflect.Slice:
			strList := origional.Interface().([]string)
			exprList := make([]parser.Expression, 0, len(strList))
			for _, x := range strList {
				exprList = append(exprList, &parser.String{Value: x})
			}
			expr = &parser.List{Values: exprList}
		default:
			panic("can only convert string/bool/[]string to configurable")
		}
	}
	switch kind {
	case reflect.String:
		return reflect.ValueOf(Configurable[string]{
			inner: &configurableInner[string]{
				single: singleConfigurable[string]{
					cases: []ConfigurableCase[string]{{
						value: expr,
					}},
				},
			},
			postProcessors: &[][]postProcessor[string]{},
		})
	case reflect.Bool:
		return reflect.ValueOf(Configurable[bool]{
			inner: &configurableInner[bool]{
				single: singleConfigurable[bool]{
					cases: []ConfigurableCase[bool]{{
						value: expr,
					}},
				},
			},
			postProcessors: &[][]postProcessor[bool]{},
		})
	case reflect.Slice:
		return reflect.ValueOf(Configurable[[]string]{
			inner: &configurableInner[[]string]{
				single: singleConfigurable[[]string]{
					cases: []ConfigurableCase[[]string]{{
						value: expr,
					}},
				},
			},
			postProcessors: &[][]postProcessor[[]string]{},
		})
	default:
		panic(fmt.Sprintf("Can't convert %s property to a configurable", origional.Kind().String()))
	}
}
