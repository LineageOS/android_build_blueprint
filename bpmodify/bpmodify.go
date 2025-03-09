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

package bpmodify

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/blueprint/parser"
)

// NewBlueprint returns a Blueprint for the given file contents that allows making modifications.
func NewBlueprint(filename string, data []byte) (*Blueprint, error) {
	r := bytes.NewReader(data)
	file, errs := parser.Parse(filename, r)
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	return &Blueprint{
		data:   data,
		bpFile: file,
	}, nil
}

type Blueprint struct {
	data     []byte
	bpFile   *parser.File
	modified bool
}

// Bytes returns a copy of the current, possibly modified contents of the Blueprint as a byte slice.
func (bp *Blueprint) Bytes() ([]byte, error) {
	if bp.modified {
		data, err := parser.Print(bp.bpFile)
		if err != nil {
			return nil, err
		}
		return data, nil
	}
	return slices.Clone(bp.data), nil
}

// String returns the current, possibly modified contents of the Blueprint as a string.
func (bp *Blueprint) String() string {
	data, err := bp.Bytes()
	if err != nil {
		return err.Error()
	}
	return string(data)
}

// Modified returns true if any of the calls on the Blueprint caused the contents to be modified.
func (bp *Blueprint) Modified() bool {
	return bp.modified
}

// ModulesByName returns a ModuleSet that contains all modules with the given list of names.
// Requesting a module that does not exist is not an error.
func (bp *Blueprint) ModulesByName(names ...string) *ModuleSet {
	moduleSet := &ModuleSet{
		bp: bp,
	}
	for _, def := range bp.bpFile.Defs {
		module, ok := def.(*parser.Module)
		if !ok {
			continue
		}

		for _, prop := range module.Properties {
			if prop.Name == "name" {
				if stringValue, ok := prop.Value.(*parser.String); ok && slices.Contains(names, stringValue.Value) {
					moduleSet.modules = append(moduleSet.modules, module)
				}
			}
		}
	}

	return moduleSet
}

// AllModules returns a ModuleSet that contains all modules in the Blueprint.
func (bp *Blueprint) AllModules() *ModuleSet {
	moduleSet := &ModuleSet{
		bp: bp,
	}
	for _, def := range bp.bpFile.Defs {
		module, ok := def.(*parser.Module)
		if !ok {
			continue
		}

		moduleSet.modules = append(moduleSet.modules, module)
	}

	return moduleSet
}

// A ModuleSet represents a set of modules in a Blueprint, and can be used to make modifications
// the modules.
type ModuleSet struct {
	bp      *Blueprint
	modules []*parser.Module
}

// GetProperty returns a PropertySet that contains all properties with the given list of names
// in all modules in the ModuleSet.  Requesting properties that do not exist is not an error.
// It returns an error for a malformed property name, or if the requested property is nested
// in a property that is not a map.
func (ms *ModuleSet) GetProperty(properties ...string) (*PropertySet, error) {
	propertySet := &PropertySet{
		bp: ms.bp,
	}

	targetProperties, err := parseQualifiedProperties(properties)
	if err != nil {
		return nil, err
	}

	for _, targetProperty := range targetProperties {
		for _, module := range ms.modules {
			prop, _, err := getRecursiveProperty(module, targetProperty)
			if err != nil {
				return nil, err
			} else if prop == nil {
				continue
			}
			propertySet.properties = append(propertySet.properties, &property{
				property: prop,
				module:   module,
				name:     targetProperty,
			})
		}
	}

	return propertySet, nil
}

// GetOrCreateProperty returns a PropertySet that contains all properties with the given list of names
// in all modules in the ModuleSet, creating empty placeholder properties if they don't exist.
// It returns an error for a malformed property name, or if the requested property is nested
// in a property that is not a map.
func (ms *ModuleSet) GetOrCreateProperty(typ Type, properties ...string) (*PropertySet, error) {
	propertySet := &PropertySet{
		bp: ms.bp,
	}

	targetProperties, err := parseQualifiedProperties(properties)
	if err != nil {
		return nil, err
	}

	for _, targetProperty := range targetProperties {
		for _, module := range ms.modules {
			prop, _, err := getRecursiveProperty(module, targetProperty)
			if err != nil {
				return nil, err
			} else if prop == nil {
				prop, err = createRecursiveProperty(module, targetProperty, parser.ZeroExpression(parser.Type(typ)))
				if err != nil {
					return nil, err
				}
				ms.bp.modified = true
			} else {
				if prop.Value.Type() != parser.Type(typ) {
					return nil, fmt.Errorf("unexpected type found in property %q, wanted %s, found %s",
						targetProperty.String(), typ, prop.Value.Type())
				}
			}
			propertySet.properties = append(propertySet.properties, &property{
				property: prop,
				module:   module,
				name:     targetProperty,
			})
		}
	}

	return propertySet, nil
}

// RemoveProperty removes the given list of properties from all modules in the ModuleSet.
// It returns an error for a malformed property name, or if the requested property is nested
// in a property that is not a map.  Removing a property that does not exist is not an error.
func (ms *ModuleSet) RemoveProperty(properties ...string) error {
	targetProperties, err := parseQualifiedProperties(properties)
	if err != nil {
		return err
	}

	for _, targetProperty := range targetProperties {
		for _, module := range ms.modules {
			prop, parent, err := getRecursiveProperty(module, targetProperty)
			if err != nil {
				return err
			} else if prop != nil {
				parent.RemoveProperty(prop.Name)
				ms.bp.modified = true
			}
		}
	}
	return nil
}

// MoveProperty moves the given list of properties to a new parent property.
// It returns an error for a malformed property name, or if the requested property is nested
// in a property that is not a map.  Moving a property that does not exist is not an error.
func (ms *ModuleSet) MoveProperty(newParent string, properties ...string) error {
	targetProperties, err := parseQualifiedProperties(properties)
	if err != nil {
		return err
	}

	for _, targetProperty := range targetProperties {
		for _, module := range ms.modules {
			prop, parent, err := getRecursiveProperty(module, targetProperty)
			if err != nil {
				return err
			} else if prop != nil {
				parent.MovePropertyContents(prop.Name, newParent)
				ms.bp.modified = true
			}
		}
	}
	return nil
}

// PropertySet represents a set of properties in a set of modules.
type PropertySet struct {
	bp         *Blueprint
	properties []*property
	sortLists  bool
}

type property struct {
	property *parser.Property
	module   *parser.Module
	name     *qualifiedProperty
}

// SortListsWhenModifying causes any future modifications to lists in the PropertySet to sort
// the lists.  Otherwise, lists are only sorted if they appear to be sorted before modification.
func (ps *PropertySet) SortListsWhenModifying() {
	ps.sortLists = true
}

// SetString sets all properties in the PropertySet to the given string.  It returns an error
// if any of the properties are not strings.
func (ps *PropertySet) SetString(s string) error {
	var errs []error
	for _, prop := range ps.properties {
		value := prop.property.Value
		str, ok := value.(*parser.String)
		if !ok {
			errs = append(errs, fmt.Errorf("expected property %s in module %s to be string, found %s",
				prop.name, prop.module.Name(), value.Type().String()))
			continue
		}
		if str.Value != s {
			str.Value = s
			ps.bp.modified = true
		}
	}

	return errors.Join(errs...)
}

// SetBool sets all properties in the PropertySet to the given boolean.  It returns an error
// if any of the properties are not booleans.
func (ps *PropertySet) SetBool(b bool) error {
	var errs []error
	for _, prop := range ps.properties {
		value := prop.property.Value
		res, ok := value.(*parser.Bool)
		if !ok {
			errs = append(errs, fmt.Errorf("expected property %s in module %s to be bool, found %s",
				prop.name, prop.module.Name(), value.Type().String()))
			continue
		}
		if res.Value != b {
			res.Value = b
			ps.bp.modified = true
		}
	}
	return errors.Join(errs...)
}

// AddStringToList adds the given strings to all properties in the PropertySet.  It returns an error
// if any of the properties are not lists of strings.
func (ps *PropertySet) AddStringToList(strs ...string) error {
	var errs []error
	for _, prop := range ps.properties {
		value := prop.property.Value
		list, ok := value.(*parser.List)
		if !ok {
			errs = append(errs, fmt.Errorf("expected property %s in module %s to be list, found %s",
				prop.name, prop.module.Name(), value.Type()))
			continue
		}
		wasSorted := parser.ListIsSorted(list)
		modified := false
		for _, s := range strs {
			m := parser.AddStringToList(list, s)
			modified = modified || m
		}
		if modified {
			ps.bp.modified = true
			if wasSorted || ps.sortLists {
				parser.SortList(ps.bp.bpFile, list)
			}
		}
	}

	return errors.Join(errs...)
}

// RemoveStringFromList removes the given strings to all properties in the PropertySet if they are present.
// It returns an error  if any of the properties are not lists of strings.
func (ps *PropertySet) RemoveStringFromList(strs ...string) error {
	var errs []error
	for _, prop := range ps.properties {
		value := prop.property.Value
		list, ok := value.(*parser.List)
		if !ok {
			errs = append(errs, fmt.Errorf("expected property %s in module %s to be list, found %s",
				prop.name, prop.module.Name(), value.Type()))
			continue
		}
		wasSorted := parser.ListIsSorted(list)
		modified := false
		for _, s := range strs {
			m := parser.RemoveStringFromList(list, s)
			modified = modified || m
		}
		if modified {
			ps.bp.modified = true
			if wasSorted || ps.sortLists {
				parser.SortList(ps.bp.bpFile, list)
			}
		}
	}

	return errors.Join(errs...)
}

// AddLiteral adds the given literal blueprint snippet to all properties in the PropertySet if they are present.
// It returns an error  if any of the properties are not lists.
func (ps *PropertySet) AddLiteral(s string) error {
	var errs []error
	for _, prop := range ps.properties {
		value := prop.property.Value
		if ps.sortLists {
			return fmt.Errorf("sorting not supported when adding a literal")
		}
		list, ok := value.(*parser.List)
		if !ok {
			errs = append(errs, fmt.Errorf("expected property %s in module %s to be list, found %s",
				prop.name, prop.module.Name(), value.Type().String()))
			continue
		}
		value, parseErrs := parser.ParseExpression(strings.NewReader(s))
		if len(parseErrs) > 0 {
			errs = append(errs, parseErrs...)
			continue
		}
		list.Values = append(list.Values, value)
		ps.bp.modified = true
	}

	return errors.Join(errs...)
}

// ReplaceStrings applies replacements to all properties in the PropertySet.  It replaces all instances
// of the strings in the keys of the given map with their corresponding values.  It returns an error
// if any of the properties are not lists of strings.
func (ps *PropertySet) ReplaceStrings(replacements map[string]string) error {
	var errs []error
	for _, prop := range ps.properties {
		value := prop.property.Value
		if list, ok := value.(*parser.List); ok {
			modified := parser.ReplaceStringsInList(list, replacements)
			if modified {
				ps.bp.modified = true
			}
		} else if str, ok := value.(*parser.String); ok {
			oldVal := str.Value
			replacementValue := replacements[oldVal]
			if replacementValue != "" {
				str.Value = replacementValue
				ps.bp.modified = true
			}
		} else {
			errs = append(errs, fmt.Errorf("expected property %s in module %s to be a list or string, found %s",
				prop.name, prop.module.Name(), value.Type().String()))
		}
	}
	return errors.Join(errs...)
}

func getRecursiveProperty(module *parser.Module, property *qualifiedProperty) (prop *parser.Property,
	parent *parser.Map, err error) {

	parent, err = traverseToQualifiedPropertyParent(module, property, false)
	if err != nil {
		return nil, nil, err
	}
	if parent == nil {
		return nil, nil, nil
	}
	if prop, found := parent.GetProperty(property.name()); found {
		return prop, parent, nil
	}

	return nil, nil, nil
}

func createRecursiveProperty(module *parser.Module, property *qualifiedProperty,
	value parser.Expression) (prop *parser.Property, err error) {
	parent, err := traverseToQualifiedPropertyParent(module, property, true)
	if err != nil {
		return nil, err
	}
	if _, found := parent.GetProperty(property.name()); found {
		return nil, fmt.Errorf("property %q already exists", property.String())
	}

	prop = &parser.Property{Name: property.name(), Value: value}
	parent.Properties = append(parent.Properties, prop)
	return prop, nil
}

func traverseToQualifiedPropertyParent(module *parser.Module, property *qualifiedProperty,
	create bool) (parent *parser.Map, err error) {
	m := &module.Map
	for i, prefix := range property.prefixes() {
		if prop, found := m.GetProperty(prefix); found {
			if mm, ok := prop.Value.(*parser.Map); ok {
				m = mm
			} else {
				// We've found a property in the AST and such property is not of type *parser.Map
				return nil, fmt.Errorf("Expected property %q to be a map, found %s",
					strings.Join(property.prefixes()[:i+1], "."), prop.Value.Type())
			}
		} else if create {
			mm := &parser.Map{}
			m.Properties = append(m.Properties, &parser.Property{Name: prefix, Value: mm})
			m = mm
		} else {
			return nil, nil
		}
	}
	return m, nil
}

type qualifiedProperty struct {
	parts []string
}

func (p *qualifiedProperty) name() string {
	return p.parts[len(p.parts)-1]
}
func (p *qualifiedProperty) prefixes() []string {
	return p.parts[:len(p.parts)-1]
}
func (p *qualifiedProperty) String() string {
	return strings.Join(p.parts, ".")
}

func parseQualifiedProperty(s string) (*qualifiedProperty, error) {
	parts := strings.Split(s, ".")
	if len(parts) == 0 {
		return nil, fmt.Errorf("%q is not a valid property name", s)
	}
	for _, part := range parts {
		if part == "" {
			return nil, fmt.Errorf("%q is not a valid property name", s)
		}
	}
	prop := qualifiedProperty{parts}
	return &prop, nil

}

func parseQualifiedProperties(properties []string) ([]*qualifiedProperty, error) {
	var qualifiedProperties []*qualifiedProperty
	var errs []error
	for _, property := range properties {
		qualifiedProperty, err := parseQualifiedProperty(property)
		if err != nil {
			errs = append(errs, err)
		}
		qualifiedProperties = append(qualifiedProperties, qualifiedProperty)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return qualifiedProperties, nil
}

type Type parser.Type

var (
	List   = Type(parser.ListType)
	String = Type(parser.StringType)
	Bool   = Type(parser.BoolType)
	Int64  = Type(parser.Int64Type)
	Map    = Type(parser.MapType)
)

func (t Type) String() string {
	return parser.Type(t).String()
}
