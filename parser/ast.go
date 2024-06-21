// Copyright 2016 Google Inc. All rights reserved.
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
	"fmt"
	"os"
	"strings"
	"text/scanner"
)

type Node interface {
	// Pos returns the position of the first token in the Node
	Pos() scanner.Position
	// End returns the position of the character after the last token in the Node
	End() scanner.Position
}

// Definition is an Assignment or a Module at the top level of a Blueprints file
type Definition interface {
	Node
	String() string
	definitionTag()
}

// An Assignment is a variable assignment at the top level of a Blueprints file, scoped to the
// file and subdirs.
type Assignment struct {
	Name       string
	NamePos    scanner.Position
	Value      Expression
	EqualsPos  scanner.Position
	Assigner   string
	Referenced bool
}

func (a *Assignment) String() string {
	return fmt.Sprintf("%s@%s %s %s %t", a.Name, a.EqualsPos, a.Assigner, a.Value, a.Referenced)
}

func (a *Assignment) Pos() scanner.Position { return a.NamePos }
func (a *Assignment) End() scanner.Position { return a.Value.End() }

func (a *Assignment) definitionTag() {}

// A Module is a module definition at the top level of a Blueprints file
type Module struct {
	Type    string
	TypePos scanner.Position
	Map
	//TODO(delmerico) make this a private field once ag/21588220 lands
	Name__internal_only *string
}

func (m *Module) Copy() *Module {
	ret := *m
	ret.Properties = make([]*Property, len(m.Properties))
	for i := range m.Properties {
		ret.Properties[i] = m.Properties[i].Copy()
	}
	return &ret
}

func (m *Module) String() string {
	propertyStrings := make([]string, len(m.Properties))
	for i, property := range m.Properties {
		propertyStrings[i] = property.String()
	}
	return fmt.Sprintf("%s@%s-%s{%s}", m.Type,
		m.LBracePos, m.RBracePos,
		strings.Join(propertyStrings, ", "))
}

func (m *Module) definitionTag() {}

func (m *Module) Pos() scanner.Position { return m.TypePos }
func (m *Module) End() scanner.Position { return m.Map.End() }

func (m *Module) Name() string {
	if m.Name__internal_only != nil {
		return *m.Name__internal_only
	}
	for _, prop := range m.Properties {
		if prop.Name == "name" {
			if stringProp, ok := prop.Value.(*String); ok {
				name := stringProp.Value
				m.Name__internal_only = &name
			} else {
				name := prop.Value.String()
				m.Name__internal_only = &name
			}
		}
	}
	if m.Name__internal_only == nil {
		name := ""
		m.Name__internal_only = &name
	}
	return *m.Name__internal_only
}

// A Property is a name: value pair within a Map, which may be a top level Module.
type Property struct {
	Name     string
	NamePos  scanner.Position
	ColonPos scanner.Position
	Value    Expression
}

func (p *Property) Copy() *Property {
	ret := *p
	ret.Value = p.Value.Copy()
	return &ret
}

func (p *Property) String() string {
	return fmt.Sprintf("%s@%s: %s", p.Name, p.ColonPos, p.Value)
}

func (p *Property) Pos() scanner.Position { return p.NamePos }
func (p *Property) End() scanner.Position { return p.Value.End() }

func (p *Property) MarkReferencedVariables(scope *Scope) {
	p.Value.MarkReferencedVariables(scope)
}

// An Expression is a Value in a Property or Assignment.  It can be a literal (String or Bool), a
// Map, a List, an Operator that combines two expressions of the same type, or a Variable that
// references and Assignment.
type Expression interface {
	Node
	// Copy returns a copy of the Expression that will not affect the original if mutated
	Copy() Expression
	String() string
	// Type returns the underlying Type enum of the Expression if it were to be evaluated, if it's known.
	// It's possible that the type isn't known, such as when a select statement with a late-bound variable
	// is used. For that reason, Type() is mostly for use in error messages, not to make logic decisions
	// off of.
	Type() Type
	// Eval returns an expression that is fully evaluated to a simple type (List, Map, String,
	// Bool, or Select).  It will return the origional expression if possible, or allocate a
	// new one if modifications were necessary.
	Eval(scope *Scope) (Expression, error)
	// PrintfInto will substitute any %s's in string literals in the AST with the provided
	// value. It will modify the AST in-place. This is used to implement soong config value
	// variables, but should be removed when those have switched to selects.
	PrintfInto(value string) error
	// MarkReferencedVariables marks the variables in the given scope referenced if there
	// is a matching variable reference in this expression. This happens naturally during
	// Eval as well, but for selects, we need to mark variables as referenced without
	// actually evaluating the expression yet.
	MarkReferencedVariables(scope *Scope)
}

// ExpressionsAreSame tells whether the two values are the same Expression.
// This includes the symbolic representation of each Expression but not their positions in the original source tree.
// This does not apply any simplification to the expressions before comparing them
// (for example, "!!a" wouldn't be deemed equal to "a")
func ExpressionsAreSame(a Expression, b Expression) (equal bool, err error) {
	return hackyExpressionsAreSame(a, b)
}

// TODO(jeffrygaston) once positions are removed from Expression structs,
// remove this function and have callers use reflect.DeepEqual(a, b)
func hackyExpressionsAreSame(a Expression, b Expression) (equal bool, err error) {
	left, err := hackyFingerprint(a)
	if err != nil {
		return false, nil
	}
	right, err := hackyFingerprint(b)
	if err != nil {
		return false, nil
	}
	areEqual := string(left) == string(right)
	return areEqual, nil
}

func hackyFingerprint(expression Expression) (fingerprint []byte, err error) {
	assignment := &Assignment{"a", noPos, expression, noPos, "=", false}
	module := &File{}
	module.Defs = append(module.Defs, assignment)
	p := newPrinter(module)
	return p.Print()
}

type Type int

const (
	UnknownType Type = iota
	BoolType
	StringType
	Int64Type
	ListType
	MapType
	UnsetType
)

func (t Type) String() string {
	switch t {
	case UnknownType:
		return "unknown"
	case BoolType:
		return "bool"
	case StringType:
		return "string"
	case Int64Type:
		return "int64"
	case ListType:
		return "list"
	case MapType:
		return "map"
	case UnsetType:
		return "unset"
	default:
		panic(fmt.Sprintf("Unknown type %d", t))
	}
}

type Operator struct {
	Args        [2]Expression
	Operator    rune
	OperatorPos scanner.Position
}

func (x *Operator) Copy() Expression {
	ret := *x
	ret.Args[0] = x.Args[0].Copy()
	ret.Args[1] = x.Args[1].Copy()
	return &ret
}

func (x *Operator) Type() Type {
	t1 := x.Args[0].Type()
	t2 := x.Args[1].Type()
	if t1 == UnknownType {
		return t2
	}
	if t2 == UnknownType {
		return t1
	}
	if t1 != t2 {
		return UnknownType
	}
	return t1
}

func (x *Operator) Eval(scope *Scope) (Expression, error) {
	return evaluateOperator(scope, x.Operator, x.Args[0], x.Args[1])
}

func evaluateOperator(scope *Scope, operator rune, left, right Expression) (Expression, error) {
	if operator != '+' {
		return nil, fmt.Errorf("unknown operator %c", operator)
	}
	l, err := left.Eval(scope)
	if err != nil {
		return nil, err
	}
	r, err := right.Eval(scope)
	if err != nil {
		return nil, err
	}

	if _, ok := l.(*Select); !ok {
		if _, ok := r.(*Select); ok {
			// Promote l to a select so we can add r to it
			l = &Select{
				Cases: []*SelectCase{{
					Value: l,
				}},
			}
		}
	}

	l = l.Copy()

	switch v := l.(type) {
	case *String:
		if _, ok := r.(*String); !ok {
			fmt.Fprintf(os.Stderr, "not ok")
		}
		v.Value += r.(*String).Value
	case *Int64:
		v.Value += r.(*Int64).Value
		v.Token = ""
	case *List:
		v.Values = append(v.Values, r.(*List).Values...)
	case *Map:
		var err error
		v.Properties, err = addMaps(scope, v.Properties, r.(*Map).Properties)
		if err != nil {
			return nil, err
		}
	case *Select:
		v.Append = r
	default:
		return nil, fmt.Errorf("operator %c not supported on %v", operator, v)
	}

	return l, nil
}

func addMaps(scope *Scope, map1, map2 []*Property) ([]*Property, error) {
	ret := make([]*Property, 0, len(map1))

	inMap1 := make(map[string]*Property)
	inMap2 := make(map[string]*Property)
	inBoth := make(map[string]*Property)

	for _, prop1 := range map1 {
		inMap1[prop1.Name] = prop1
	}

	for _, prop2 := range map2 {
		inMap2[prop2.Name] = prop2
		if _, ok := inMap1[prop2.Name]; ok {
			inBoth[prop2.Name] = prop2
		}
	}

	for _, prop1 := range map1 {
		if prop2, ok := inBoth[prop1.Name]; ok {
			var err error
			newProp := *prop1
			newProp.Value, err = evaluateOperator(scope, '+', prop1.Value, prop2.Value)
			if err != nil {
				return nil, err
			}
			ret = append(ret, &newProp)
		} else {
			ret = append(ret, prop1)
		}
	}

	for _, prop2 := range map2 {
		if _, ok := inBoth[prop2.Name]; !ok {
			ret = append(ret, prop2)
		}
	}

	return ret, nil
}

func (x *Operator) PrintfInto(value string) error {
	if err := x.Args[0].PrintfInto(value); err != nil {
		return err
	}
	return x.Args[1].PrintfInto(value)
}

func (x *Operator) MarkReferencedVariables(scope *Scope) {
	x.Args[0].MarkReferencedVariables(scope)
	x.Args[1].MarkReferencedVariables(scope)
}

func (x *Operator) Pos() scanner.Position { return x.Args[0].Pos() }
func (x *Operator) End() scanner.Position { return x.Args[1].End() }

func (x *Operator) String() string {
	return fmt.Sprintf("(%s %c %s)@%s", x.Args[0].String(), x.Operator, x.Args[1].String(),
		x.OperatorPos)
}

type Variable struct {
	Name    string
	NamePos scanner.Position
	Type_   Type
}

func (x *Variable) Pos() scanner.Position { return x.NamePos }
func (x *Variable) End() scanner.Position { return endPos(x.NamePos, len(x.Name)) }

func (x *Variable) Copy() Expression {
	ret := *x
	return &ret
}

func (x *Variable) Eval(scope *Scope) (Expression, error) {
	if assignment := scope.Get(x.Name); assignment != nil {
		assignment.Referenced = true
		return assignment.Value, nil
	}
	return nil, fmt.Errorf("undefined variable %s", x.Name)
}

func (x *Variable) PrintfInto(value string) error {
	return nil
}

func (x *Variable) MarkReferencedVariables(scope *Scope) {
	if assignment := scope.Get(x.Name); assignment != nil {
		assignment.Referenced = true
	}
}

func (x *Variable) String() string {
	return x.Name
}

func (x *Variable) Type() Type {
	// Variables do not normally have a type associated with them, this is only
	// filled out in the androidmk tool
	return x.Type_
}

type Map struct {
	LBracePos  scanner.Position
	RBracePos  scanner.Position
	Properties []*Property
}

func (x *Map) Pos() scanner.Position { return x.LBracePos }
func (x *Map) End() scanner.Position { return endPos(x.RBracePos, 1) }

func (x *Map) Copy() Expression {
	ret := *x
	ret.Properties = make([]*Property, len(x.Properties))
	for i := range x.Properties {
		ret.Properties[i] = x.Properties[i].Copy()
	}
	return &ret
}

func (x *Map) Eval(scope *Scope) (Expression, error) {
	newProps := make([]*Property, len(x.Properties))
	for i, prop := range x.Properties {
		newVal, err := prop.Value.Eval(scope)
		if err != nil {
			return nil, err
		}
		newProps[i] = &Property{
			Name:     prop.Name,
			NamePos:  prop.NamePos,
			ColonPos: prop.ColonPos,
			Value:    newVal,
		}
	}
	return &Map{
		LBracePos:  x.LBracePos,
		RBracePos:  x.RBracePos,
		Properties: newProps,
	}, nil
}

func (x *Map) PrintfInto(value string) error {
	// We should never reach this because selects cannot hold maps
	panic("printfinto() is unsupported on maps")
}

func (x *Map) MarkReferencedVariables(scope *Scope) {
	for _, prop := range x.Properties {
		prop.MarkReferencedVariables(scope)
	}
}

func (x *Map) String() string {
	propertyStrings := make([]string, len(x.Properties))
	for i, property := range x.Properties {
		propertyStrings[i] = property.String()
	}
	return fmt.Sprintf("@%s-%s{%s}", x.LBracePos, x.RBracePos,
		strings.Join(propertyStrings, ", "))
}

func (x *Map) Type() Type { return MapType }

// GetProperty looks for a property with the given name.
// It resembles the bracket operator of a built-in Golang map.
func (x *Map) GetProperty(name string) (Property *Property, found bool) {
	prop, found, _ := x.getPropertyImpl(name)
	return prop, found // we don't currently expose the index to callers
}

func (x *Map) getPropertyImpl(name string) (Property *Property, found bool, index int) {
	for i, prop := range x.Properties {
		if prop.Name == name {
			return prop, true, i
		}
	}
	return nil, false, -1
}

// RemoveProperty removes the property with the given name, if it exists.
func (x *Map) RemoveProperty(propertyName string) (removed bool) {
	_, found, index := x.getPropertyImpl(propertyName)
	if found {
		x.Properties = append(x.Properties[:index], x.Properties[index+1:]...)
	}
	return found
}

// MovePropertyContents moves the contents of propertyName into property newLocation
// If property newLocation doesn't exist, MovePropertyContents renames propertyName as newLocation.
// Otherwise, MovePropertyContents only supports moving contents that are a List of String.
func (x *Map) MovePropertyContents(propertyName string, newLocation string) (removed bool) {
	oldProp, oldFound, _ := x.getPropertyImpl(propertyName)
	newProp, newFound, _ := x.getPropertyImpl(newLocation)

	// newLoc doesn't exist, simply renaming property
	if oldFound && !newFound {
		oldProp.Name = newLocation
		return oldFound
	}

	if oldFound {
		old, oldOk := oldProp.Value.(*List)
		new, newOk := newProp.Value.(*List)
		if oldOk && newOk {
			toBeMoved := make([]string, len(old.Values)) //
			for i, p := range old.Values {
				toBeMoved[i] = p.(*String).Value
			}

			for _, moved := range toBeMoved {
				RemoveStringFromList(old, moved)
				AddStringToList(new, moved)
			}
			// oldProp should now be empty and needs to be deleted
			x.RemoveProperty(oldProp.Name)
		} else {
			print(`MovePropertyContents currently only supports moving PropertyName
					with List of Strings into an existing newLocation with List of Strings\n`)
		}
	}
	return oldFound
}

type List struct {
	LBracePos scanner.Position
	RBracePos scanner.Position
	Values    []Expression
}

func (x *List) Pos() scanner.Position { return x.LBracePos }
func (x *List) End() scanner.Position { return endPos(x.RBracePos, 1) }

func (x *List) Copy() Expression {
	ret := *x
	ret.Values = make([]Expression, len(x.Values))
	for i := range ret.Values {
		ret.Values[i] = x.Values[i].Copy()
	}
	return &ret
}

func (x *List) Eval(scope *Scope) (Expression, error) {
	newValues := make([]Expression, len(x.Values))
	for i, val := range x.Values {
		newVal, err := val.Eval(scope)
		if err != nil {
			return nil, err
		}
		newValues[i] = newVal
	}
	return &List{
		LBracePos: x.LBracePos,
		RBracePos: x.RBracePos,
		Values:    newValues,
	}, nil
}

func (x *List) PrintfInto(value string) error {
	for _, val := range x.Values {
		if err := val.PrintfInto(value); err != nil {
			return err
		}
	}
	return nil
}

func (x *List) MarkReferencedVariables(scope *Scope) {
	for _, val := range x.Values {
		val.MarkReferencedVariables(scope)
	}
}

func (x *List) String() string {
	valueStrings := make([]string, len(x.Values))
	for i, value := range x.Values {
		valueStrings[i] = value.String()
	}
	return fmt.Sprintf("@%s-%s[%s]", x.LBracePos, x.RBracePos,
		strings.Join(valueStrings, ", "))
}

func (x *List) Type() Type { return ListType }

type String struct {
	LiteralPos scanner.Position
	Value      string
}

func (x *String) Pos() scanner.Position { return x.LiteralPos }
func (x *String) End() scanner.Position { return endPos(x.LiteralPos, len(x.Value)+2) }

func (x *String) Copy() Expression {
	ret := *x
	return &ret
}

func (x *String) Eval(scope *Scope) (Expression, error) {
	return x, nil
}

func (x *String) PrintfInto(value string) error {
	count := strings.Count(x.Value, "%")
	if count == 0 {
		return nil
	}

	if count > 1 {
		return fmt.Errorf("list/value variable properties only support a single '%%'")
	}

	if !strings.Contains(x.Value, "%s") {
		return fmt.Errorf("unsupported %% in value variable property")
	}

	x.Value = fmt.Sprintf(x.Value, value)
	return nil
}

func (x *String) MarkReferencedVariables(scope *Scope) {
}

func (x *String) String() string {
	return fmt.Sprintf("%q@%s", x.Value, x.LiteralPos)
}

func (x *String) Type() Type {
	return StringType
}

type Int64 struct {
	LiteralPos scanner.Position
	Value      int64
	Token      string
}

func (x *Int64) Pos() scanner.Position { return x.LiteralPos }
func (x *Int64) End() scanner.Position { return endPos(x.LiteralPos, len(x.Token)) }

func (x *Int64) Copy() Expression {
	ret := *x
	return &ret
}

func (x *Int64) Eval(scope *Scope) (Expression, error) {
	return x, nil
}

func (x *Int64) PrintfInto(value string) error {
	return nil
}

func (x *Int64) MarkReferencedVariables(scope *Scope) {
}

func (x *Int64) String() string {
	return fmt.Sprintf("%q@%s", x.Value, x.LiteralPos)
}

func (x *Int64) Type() Type {
	return Int64Type
}

type Bool struct {
	LiteralPos scanner.Position
	Value      bool
	Token      string
}

func (x *Bool) Pos() scanner.Position { return x.LiteralPos }
func (x *Bool) End() scanner.Position { return endPos(x.LiteralPos, len(x.Token)) }

func (x *Bool) Copy() Expression {
	ret := *x
	return &ret
}

func (x *Bool) Eval(scope *Scope) (Expression, error) {
	return x, nil
}

func (x *Bool) PrintfInto(value string) error {
	return nil
}

func (x *Bool) MarkReferencedVariables(scope *Scope) {
}

func (x *Bool) String() string {
	return fmt.Sprintf("%t@%s", x.Value, x.LiteralPos)
}

func (x *Bool) Type() Type {
	return BoolType
}

type CommentGroup struct {
	Comments []*Comment
}

func (x *CommentGroup) Pos() scanner.Position { return x.Comments[0].Pos() }
func (x *CommentGroup) End() scanner.Position { return x.Comments[len(x.Comments)-1].End() }

type Comment struct {
	Comment []string
	Slash   scanner.Position
}

func (c Comment) Pos() scanner.Position {
	return c.Slash
}

func (c Comment) End() scanner.Position {
	pos := c.Slash
	for _, comment := range c.Comment {
		pos.Offset += len(comment) + 1
		pos.Column = len(comment) + 1
	}
	pos.Line += len(c.Comment) - 1
	return pos
}

func (c Comment) String() string {
	l := 0
	for _, comment := range c.Comment {
		l += len(comment) + 1
	}
	buf := make([]byte, 0, l)
	for _, comment := range c.Comment {
		buf = append(buf, comment...)
		buf = append(buf, '\n')
	}

	return string(buf) + "@" + c.Slash.String()
}

// Return the text of the comment with // or /* and */ stripped
func (c Comment) Text() string {
	l := 0
	for _, comment := range c.Comment {
		l += len(comment) + 1
	}
	buf := make([]byte, 0, l)

	blockComment := false
	if strings.HasPrefix(c.Comment[0], "/*") {
		blockComment = true
	}

	for i, comment := range c.Comment {
		if blockComment {
			if i == 0 {
				comment = strings.TrimPrefix(comment, "/*")
			}
			if i == len(c.Comment)-1 {
				comment = strings.TrimSuffix(comment, "*/")
			}
		} else {
			comment = strings.TrimPrefix(comment, "//")
		}
		buf = append(buf, comment...)
		buf = append(buf, '\n')
	}

	return string(buf)
}

func endPos(pos scanner.Position, n int) scanner.Position {
	pos.Offset += n
	pos.Column += n
	return pos
}

type ConfigurableCondition struct {
	position     scanner.Position
	FunctionName string
	Args         []String
}

func (c *ConfigurableCondition) Equals(other ConfigurableCondition) bool {
	if c.FunctionName != other.FunctionName {
		return false
	}
	if len(c.Args) != len(other.Args) {
		return false
	}
	for i := range c.Args {
		if c.Args[i] != other.Args[i] {
			return false
		}
	}
	return true
}

func (c *ConfigurableCondition) String() string {
	var sb strings.Builder
	sb.WriteString(c.FunctionName)
	sb.WriteRune('(')
	for i, arg := range c.Args {
		sb.WriteRune('"')
		sb.WriteString(arg.Value)
		sb.WriteRune('"')
		if i < len(c.Args)-1 {
			sb.WriteString(", ")
		}
	}
	sb.WriteRune(')')
	return sb.String()
}

type Select struct {
	Scope      *Scope           // scope used to evaluate the body of the select later on
	KeywordPos scanner.Position // the keyword "select"
	Conditions []ConfigurableCondition
	LBracePos  scanner.Position
	RBracePos  scanner.Position
	Cases      []*SelectCase // the case statements
	Append     Expression
}

func (s *Select) Pos() scanner.Position { return s.KeywordPos }
func (s *Select) End() scanner.Position { return endPos(s.RBracePos, 1) }

func (s *Select) Copy() Expression {
	ret := *s
	ret.Cases = make([]*SelectCase, len(ret.Cases))
	for i, selectCase := range s.Cases {
		ret.Cases[i] = selectCase.Copy()
	}
	if s.Append != nil {
		ret.Append = s.Append.Copy()
	}
	return &ret
}

func (s *Select) Eval(scope *Scope) (Expression, error) {
	s.Scope = scope
	s.MarkReferencedVariables(scope)
	return s, nil
}

func (x *Select) PrintfInto(value string) error {
	// PrintfInto will be handled at the Configurable object level
	panic("Cannot call PrintfInto on a select expression")
}

func (x *Select) MarkReferencedVariables(scope *Scope) {
	for _, c := range x.Cases {
		c.MarkReferencedVariables(scope)
	}
	if x.Append != nil {
		x.Append.MarkReferencedVariables(scope)
	}
}

func (s *Select) String() string {
	return "<select>"
}

func (s *Select) Type() Type {
	if len(s.Cases) == 0 {
		return UnsetType
	}
	return UnknownType
}

type SelectPattern struct {
	Value   Expression
	Binding Variable
}

func (c *SelectPattern) Pos() scanner.Position { return c.Value.Pos() }
func (c *SelectPattern) End() scanner.Position {
	if c.Binding.NamePos.IsValid() {
		return c.Binding.End()
	}
	return c.Value.End()
}

type SelectCase struct {
	Patterns []SelectPattern
	ColonPos scanner.Position
	Value    Expression
}

func (x *SelectCase) MarkReferencedVariables(scope *Scope) {
	x.Value.MarkReferencedVariables(scope)
}

func (c *SelectCase) Copy() *SelectCase {
	ret := *c
	ret.Value = c.Value.Copy()
	return &ret
}

func (c *SelectCase) String() string {
	return "<select case>"
}

func (c *SelectCase) Pos() scanner.Position { return c.Patterns[0].Pos() }
func (c *SelectCase) End() scanner.Position { return c.Value.End() }

// UnsetProperty is the expression type of the "unset" keyword that can be
// used in select statements to make the property unset. For example:
//
//	my_module_type {
//	  name: "foo",
//	  some_prop: select(soong_config_variable("my_namespace", "my_var"), {
//	    "foo": unset,
//	    "default": "bar",
//	  })
//	}
type UnsetProperty struct {
	Position scanner.Position
}

func (n *UnsetProperty) Copy() Expression {
	return &UnsetProperty{Position: n.Position}
}

func (n *UnsetProperty) String() string {
	return "unset"
}

func (n *UnsetProperty) Type() Type {
	return UnsetType
}

func (n *UnsetProperty) Eval(scope *Scope) (Expression, error) {
	return n, nil
}

func (x *UnsetProperty) PrintfInto(value string) error {
	return nil
}

func (x *UnsetProperty) MarkReferencedVariables(scope *Scope) {
}

func (n *UnsetProperty) Pos() scanner.Position { return n.Position }
func (n *UnsetProperty) End() scanner.Position { return n.Position }
