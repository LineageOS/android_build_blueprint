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
	"fmt"
	"reflect"
	"slices"

	"github.com/google/blueprint/parser"
)

func RepackProperties(props []interface{}) (*parser.Map, error) {

	var dereferencedProps []reflect.Value
	for _, rawProp := range props {
		propStruct := reflect.ValueOf(rawProp)
		if !isStructPtr(propStruct.Type()) {
			return nil, fmt.Errorf("properties must be *struct, got %s",
				propStruct.Type())
		}
		propStruct = propStruct.Elem()
		dereferencedProps = append(dereferencedProps, propStruct)
	}

	return repackStructs(dereferencedProps)
}

func repackStructs(props []reflect.Value) (*parser.Map, error) {
	var allFieldNames []string
	for _, prop := range props {
		propType := prop.Type()
		for i := 0; i < propType.NumField(); i++ {
			field := propType.Field(i)
			if !slices.Contains(allFieldNames, field.Name) {
				allFieldNames = append(allFieldNames, field.Name)
			}
		}
	}

	result := &parser.Map{}

	for _, fieldName := range allFieldNames {
		var fields []reflect.Value
		for _, prop := range props {
			field := prop.FieldByName(fieldName)
			if field.IsValid() {
				fields = append(fields, field)
			}
		}
		if err := assertFieldsEquivalent(fields); err != nil {
			return nil, err
		}

		var expr parser.Expression
		var field reflect.Value
		for _, f := range fields {
			if !isPropEmpty(f) {
				field = f
				break
			}
		}
		if !field.IsValid() {
			continue
		}
		if isStruct(field.Type()) && !isConfigurable(field.Type()) {
			x, err := repackStructs(fields)
			if err != nil {
				return nil, err
			}
			if x != nil {
				expr = x
			}
		} else {
			x, err := fieldToExpr(field)
			if err != nil {
				return nil, err
			}
			if x != nil {
				expr = *x
			}
		}

		if expr != nil {
			result.Properties = append(result.Properties, &parser.Property{
				Name:  PropertyNameForField(fieldName),
				Value: expr,
			})
		}
	}

	return result, nil
}

func fieldToExpr(field reflect.Value) (*parser.Expression, error) {
	if IsConfigurable(field.Type()) {
		return field.Interface().(configurableReflection).toExpression()
	}
	if field.Kind() == reflect.Pointer {
		if field.IsNil() {
			return nil, nil
		}
		field = field.Elem()
	}
	switch field.Kind() {
	case reflect.String:
		var result parser.Expression = &parser.String{Value: field.String()}
		return &result, nil
	case reflect.Bool:
		var result parser.Expression = &parser.Bool{Value: field.Bool()}
		return &result, nil
	case reflect.Int, reflect.Int64:
		var result parser.Expression = &parser.Int64{Value: field.Int()}
		return &result, nil
	case reflect.Slice:
		var contents []parser.Expression
		for i := 0; i < field.Len(); i++ {
			inner, err := fieldToExpr(field.Index(i))
			if err != nil {
				return nil, err
			}
			contents = append(contents, *inner)
		}
		var result parser.Expression = &parser.List{Values: contents}
		return &result, nil
	case reflect.Struct:
		var properties []*parser.Property
		typ := field.Type()
		for i := 0; i < typ.NumField(); i++ {
			inner, err := fieldToExpr(field.Field(i))
			if err != nil {
				return nil, err
			}
			properties = append(properties, &parser.Property{
				Name:  typ.Field(i).Name,
				Value: *inner,
			})
		}
		var result parser.Expression = &parser.Map{Properties: properties}
		return &result, nil
	default:
		return nil, fmt.Errorf("Unhandled type: %s", field.Kind().String())
	}
}

func isPropEmpty(value reflect.Value) bool {
	switch value.Kind() {
	case reflect.Pointer:
		if value.IsNil() {
			return true
		}
		return isPropEmpty(value.Elem())
	case reflect.Struct:
		if isConfigurable(value.Type()) {
			return value.Interface().(configurableReflection).isEmpty()
		}
		for i := 0; i < value.NumField(); i++ {
			if !isPropEmpty(value.Field(i)) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func assertFieldsEquivalent(fields []reflect.Value) error {
	var firstNonEmpty reflect.Value
	var firstIndex int
	for i, f := range fields {
		if !isPropEmpty(f) {
			firstNonEmpty = f
			firstIndex = i
			break
		}
	}
	if !firstNonEmpty.IsValid() {
		return nil
	}
	for i, f := range fields {
		if i != firstIndex && !isPropEmpty(f) {
			if err := assertTwoNonEmptyFieldsEquivalent(firstNonEmpty, f); err != nil {
				return err
			}
		}
	}
	return nil
}

func assertTwoNonEmptyFieldsEquivalent(a, b reflect.Value) error {
	aType := a.Type()
	bType := b.Type()

	if aType != bType {
		return fmt.Errorf("fields must have the same type")
	}

	switch aType.Kind() {
	case reflect.Pointer:
		return assertTwoNonEmptyFieldsEquivalent(a.Elem(), b.Elem())
	case reflect.String:
		if a.String() != b.String() {
			return fmt.Errorf("Conflicting fields in property structs had values %q and %q", a.String(), b.String())
		}
	case reflect.Bool:
		if a.Bool() != b.Bool() {
			return fmt.Errorf("Conflicting fields in property structs had values %t and %t", a.Bool(), b.Bool())
		}
	case reflect.Slice:
		if a.Len() != b.Len() {
			return fmt.Errorf("Conflicting fields in property structs had lengths %d and %d", a.Len(), b.Len())
		}
		for i := 0; i < a.Len(); i++ {
			if err := assertTwoNonEmptyFieldsEquivalent(a.Index(i), b.Index(i)); err != nil {
				return err
			}
		}
	case reflect.Int:
		if a.Int() != b.Int() {
			return fmt.Errorf("Conflicting fields in property structs had values %d and %d", a.Int(), b.Int())
		}
	case reflect.Struct:
		if isConfigurable(a.Type()) {
			// We could properly check that two configurables are equivalent, but that's a lot more
			// work for a case that I don't think should show up in practice.
			return fmt.Errorf("Cannot handle two property structs with nonempty configurable properties")
		}
		// We don't care about checking if structs are equivalent, we'll check their individual
		// fields when we recurse down.
	default:
		return fmt.Errorf("Unhandled kind: %s", aType.Kind().String())
	}

	return nil
}
