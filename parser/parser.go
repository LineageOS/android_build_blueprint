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
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/scanner"
)

var errTooManyErrors = errors.New("too many errors")

const maxErrors = 1

const default_select_branch_name = "__soong_conditions_default__"
const any_select_branch_name = "__soong_conditions_any__"

type ParseError struct {
	Err error
	Pos scanner.Position
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("%s: %s", e.Pos, e.Err)
}

type File struct {
	Name     string
	Defs     []Definition
	Comments []*CommentGroup
}

func parse(p *parser) (file *File, errs []error) {
	defer func() {
		if r := recover(); r != nil {
			if r == errTooManyErrors {
				errs = p.errors
				return
			}
			panic(r)
		}
	}()

	p.next()
	defs := p.parseDefinitions()
	p.accept(scanner.EOF)
	errs = p.errors
	comments := p.comments

	return &File{
		Name:     p.scanner.Filename,
		Defs:     defs,
		Comments: comments,
	}, errs

}

func ParseAndEval(filename string, r io.Reader, scope *Scope) (file *File, errs []error) {
	file, errs = Parse(filename, r)
	if len(errs) > 0 {
		return nil, errs
	}

	// evaluate all module properties
	var newDefs []Definition
	for _, def := range file.Defs {
		switch d := def.(type) {
		case *Module:
			for _, prop := range d.Map.Properties {
				newval, err := prop.Value.Eval(scope)
				if err != nil {
					return nil, []error{err}
				}
				switch newval.(type) {
				case *String, *Bool, *Int64, *Select, *Map, *List:
					// ok
				default:
					panic(fmt.Sprintf("Evaled but got %#v\n", newval))
				}
				prop.Value = newval
			}
			newDefs = append(newDefs, d)
		case *Assignment:
			if err := scope.HandleAssignment(d); err != nil {
				return nil, []error{err}
			}
		}
	}

	// This is not strictly necessary, but removing the assignments from
	// the result makes it clearer that this is an evaluated file.
	// We could also consider adding a "EvaluatedFile" type to return.
	file.Defs = newDefs

	return file, nil
}

func Parse(filename string, r io.Reader) (file *File, errs []error) {
	p := newParser(r)
	p.scanner.Filename = filename

	return parse(p)
}

func ParseExpression(r io.Reader) (value Expression, errs []error) {
	p := newParser(r)
	p.next()
	value = p.parseExpression()
	p.accept(scanner.EOF)
	errs = p.errors
	return
}

type parser struct {
	scanner  scanner.Scanner
	tok      rune
	errors   []error
	comments []*CommentGroup
}

func newParser(r io.Reader) *parser {
	p := &parser{}
	p.scanner.Init(r)
	p.scanner.Error = func(sc *scanner.Scanner, msg string) {
		p.errorf(msg)
	}
	p.scanner.Mode = scanner.ScanIdents | scanner.ScanInts | scanner.ScanStrings |
		scanner.ScanRawStrings | scanner.ScanComments
	return p
}

func (p *parser) error(err error) {
	pos := p.scanner.Position
	if !pos.IsValid() {
		pos = p.scanner.Pos()
	}
	err = &ParseError{
		Err: err,
		Pos: pos,
	}
	p.errors = append(p.errors, err)
	if len(p.errors) >= maxErrors {
		panic(errTooManyErrors)
	}
}

func (p *parser) errorf(format string, args ...interface{}) {
	p.error(fmt.Errorf(format, args...))
}

func (p *parser) accept(toks ...rune) bool {
	for _, tok := range toks {
		if p.tok != tok {
			p.errorf("expected %s, found %s", scanner.TokenString(tok),
				scanner.TokenString(p.tok))
			return false
		}
		p.next()
	}
	return true
}

func (p *parser) next() {
	if p.tok != scanner.EOF {
		p.tok = p.scanner.Scan()
		if p.tok == scanner.Comment {
			var comments []*Comment
			for p.tok == scanner.Comment {
				lines := strings.Split(p.scanner.TokenText(), "\n")
				if len(comments) > 0 && p.scanner.Position.Line > comments[len(comments)-1].End().Line+1 {
					p.comments = append(p.comments, &CommentGroup{Comments: comments})
					comments = nil
				}
				comments = append(comments, &Comment{lines, p.scanner.Position})
				p.tok = p.scanner.Scan()
			}
			p.comments = append(p.comments, &CommentGroup{Comments: comments})
		}
	}
}

func (p *parser) parseDefinitions() (defs []Definition) {
	for {
		switch p.tok {
		case scanner.Ident:
			ident := p.scanner.TokenText()
			pos := p.scanner.Position

			p.accept(scanner.Ident)

			switch p.tok {
			case '+':
				p.accept('+')
				defs = append(defs, p.parseAssignment(ident, pos, "+="))
			case '=':
				defs = append(defs, p.parseAssignment(ident, pos, "="))
			case '{', '(':
				defs = append(defs, p.parseModule(ident, pos))
			default:
				p.errorf("expected \"=\" or \"+=\" or \"{\" or \"(\", found %s",
					scanner.TokenString(p.tok))
			}
		case scanner.EOF:
			return
		default:
			p.errorf("expected assignment or module definition, found %s",
				scanner.TokenString(p.tok))
			return
		}
	}
}

func (p *parser) parseAssignment(name string, namePos scanner.Position,
	assigner string) (assignment *Assignment) {

	// These are used as keywords in select statements, prevent making variables
	// with the same name to avoid any confusion.
	switch name {
	case "default", "unset":
		p.errorf("'default' and 'unset' are reserved keywords, and cannot be used as variable names")
		return nil
	}

	assignment = new(Assignment)

	pos := p.scanner.Position
	if !p.accept('=') {
		return
	}
	value := p.parseExpression()

	assignment.Name = name
	assignment.NamePos = namePos
	assignment.Value = value
	assignment.EqualsPos = pos
	assignment.Assigner = assigner

	return
}

func (p *parser) parseModule(typ string, typPos scanner.Position) *Module {

	compat := false
	lbracePos := p.scanner.Position
	if p.tok == '{' {
		compat = true
	}

	if !p.accept(p.tok) {
		return nil
	}
	properties := p.parsePropertyList(true, compat)
	rbracePos := p.scanner.Position
	if !compat {
		p.accept(')')
	} else {
		p.accept('}')
	}

	return &Module{
		Type:    typ,
		TypePos: typPos,
		Map: Map{
			Properties: properties,
			LBracePos:  lbracePos,
			RBracePos:  rbracePos,
		},
	}
}

func (p *parser) parsePropertyList(isModule, compat bool) (properties []*Property) {
	for p.tok == scanner.Ident {
		properties = append(properties, p.parseProperty(isModule, compat))

		if p.tok != ',' {
			// There was no comma, so the list is done.
			break
		}

		p.accept(',')
	}

	return
}

func (p *parser) parseProperty(isModule, compat bool) (property *Property) {
	property = new(Property)

	name := p.scanner.TokenText()
	namePos := p.scanner.Position
	p.accept(scanner.Ident)
	pos := p.scanner.Position

	if isModule {
		if compat {
			if !p.accept(':') {
				return
			}
		} else {
			if !p.accept('=') {
				return
			}
		}
	} else {
		if !p.accept(':') {
			return
		}
	}

	value := p.parseExpression()

	property.Name = name
	property.NamePos = namePos
	property.Value = value
	property.ColonPos = pos

	return
}

func (p *parser) parseExpression() (value Expression) {
	value = p.parseValue()
	switch p.tok {
	case '+':
		return p.parseOperator(value)
	case '-':
		p.errorf("subtraction not supported: %s", p.scanner.String())
		return value
	default:
		return value
	}
}

func (p *parser) parseOperator(value1 Expression) Expression {
	operator := p.tok
	pos := p.scanner.Position
	p.accept(operator)

	value2 := p.parseExpression()

	return &Operator{
		Args:        [2]Expression{value1, value2},
		Operator:    operator,
		OperatorPos: pos,
	}
}

func (p *parser) parseValue() (value Expression) {
	switch p.tok {
	case scanner.Ident:
		switch text := p.scanner.TokenText(); text {
		case "true", "false":
			return p.parseBoolean()
		case "select":
			return p.parseSelect()
		default:
			return p.parseVariable()
		}
	case '-', scanner.Int: // Integer might have '-' sign ahead ('+' is only treated as operator now)
		return p.parseIntValue()
	case scanner.String, scanner.RawString:
		return p.parseStringValue()
	case '[':
		return p.parseListValue()
	case '{':
		return p.parseMapValue()
	default:
		p.errorf("expected bool, list, or string value; found %s",
			scanner.TokenString(p.tok))
		return
	}
}

func (p *parser) parseBoolean() Expression {
	switch text := p.scanner.TokenText(); text {
	case "true", "false":
		result := &Bool{
			LiteralPos: p.scanner.Position,
			Value:      text == "true",
			Token:      text,
		}
		p.accept(scanner.Ident)
		return result
	default:
		p.errorf("Expected true/false, got %q", text)
		return nil
	}
}

func (p *parser) parseVariable() Expression {
	var value Expression

	text := p.scanner.TokenText()
	value = &Variable{
		Name:    text,
		NamePos: p.scanner.Position,
	}

	p.accept(scanner.Ident)
	return value
}

func (p *parser) parseSelect() Expression {
	result := &Select{
		KeywordPos: p.scanner.Position,
	}
	// Read the "select("
	p.accept(scanner.Ident)
	if !p.accept('(') {
		return nil
	}

	// If we see another '(', there's probably multiple conditions and there must
	// be a ')' after. Set the multipleConditions variable to remind us to check for
	// the ')' after.
	multipleConditions := false
	if p.tok == '(' {
		multipleConditions = true
		p.accept('(')
	}

	// Read all individual conditions
	conditions := []ConfigurableCondition{}
	for first := true; first || multipleConditions; first = false {
		condition := ConfigurableCondition{
			position:     p.scanner.Position,
			FunctionName: p.scanner.TokenText(),
		}
		if !p.accept(scanner.Ident) {
			return nil
		}
		if !p.accept('(') {
			return nil
		}

		for p.tok != ')' {
			if s := p.parseStringValue(); s != nil {
				condition.Args = append(condition.Args, *s)
			} else {
				return nil
			}
			if p.tok == ')' {
				break
			}
			if !p.accept(',') {
				return nil
			}
		}
		p.accept(')')

		for _, c := range conditions {
			if c.Equals(condition) {
				p.errorf("Duplicate select condition found: %s", c.String())
			}
		}

		conditions = append(conditions, condition)

		if multipleConditions {
			if p.tok == ')' {
				p.next()
				break
			}
			if !p.accept(',') {
				return nil
			}
			// Retry the closing parent to allow for a trailing comma
			if p.tok == ')' {
				p.next()
				break
			}
		}
	}

	if multipleConditions && len(conditions) < 2 {
		p.errorf("Expected multiple select conditions due to the extra parenthesis, but only found 1. Please remove the extra parenthesis.")
		return nil
	}

	result.Conditions = conditions

	if !p.accept(',') {
		return nil
	}

	result.LBracePos = p.scanner.Position
	if !p.accept('{') {
		return nil
	}

	maybeParseBinding := func() (Variable, bool) {
		if p.scanner.TokenText() != "@" {
			return Variable{}, false
		}
		p.next()
		value := Variable{
			Name:    p.scanner.TokenText(),
			NamePos: p.scanner.Position,
		}
		p.accept(scanner.Ident)
		return value, true
	}

	parseOnePattern := func() SelectPattern {
		var result SelectPattern
		switch p.tok {
		case scanner.Ident:
			switch p.scanner.TokenText() {
			case "any":
				result.Value = &String{
					LiteralPos: p.scanner.Position,
					Value:      any_select_branch_name,
				}
				p.next()
				if binding, exists := maybeParseBinding(); exists {
					result.Binding = binding
				}
				return result
			case "default":
				result.Value = &String{
					LiteralPos: p.scanner.Position,
					Value:      default_select_branch_name,
				}
				p.next()
				return result
			case "true":
				result.Value = &Bool{
					LiteralPos: p.scanner.Position,
					Value:      true,
				}
				p.next()
				return result
			case "false":
				result.Value = &Bool{
					LiteralPos: p.scanner.Position,
					Value:      false,
				}
				p.next()
				return result
			default:
				p.errorf("Expected a string, true, false, or default, got %s", p.scanner.TokenText())
			}
		case scanner.String:
			if s := p.parseStringValue(); s != nil {
				if strings.HasPrefix(s.Value, "__soong") {
					p.errorf("select branch patterns starting with __soong are reserved for internal use")
					return result
				}
				result.Value = s
				return result
			}
			fallthrough
		default:
			p.errorf("Expected a string, true, false, or default, got %s", p.scanner.TokenText())
		}
		return result
	}

	hasNonUnsetValue := false
	for p.tok != '}' {
		c := &SelectCase{}

		if multipleConditions {
			if !p.accept('(') {
				return nil
			}
			for i := 0; i < len(conditions); i++ {
				c.Patterns = append(c.Patterns, parseOnePattern())
				if i < len(conditions)-1 {
					if !p.accept(',') {
						return nil
					}
				} else if p.tok == ',' {
					// allow optional trailing comma
					p.next()
				}
			}
			if !p.accept(')') {
				return nil
			}
		} else {
			c.Patterns = append(c.Patterns, parseOnePattern())
		}
		c.ColonPos = p.scanner.Position
		if !p.accept(':') {
			return nil
		}
		if p.tok == scanner.Ident && p.scanner.TokenText() == "unset" {
			c.Value = &UnsetProperty{Position: p.scanner.Position}
			p.accept(scanner.Ident)
		} else {
			hasNonUnsetValue = true
			c.Value = p.parseExpression()
		}
		if !p.accept(',') {
			return nil
		}
		result.Cases = append(result.Cases, c)
	}

	// If all branches have the value "unset", then this is equivalent
	// to an empty select.
	if !hasNonUnsetValue {
		p.errorf("This select statement is empty, remove it")
		return nil
	}

	patternsEqual := func(a, b SelectPattern) bool {
		// We can ignore the bindings, they don't affect which pattern is matched
		switch a2 := a.Value.(type) {
		case *String:
			if b2, ok := b.Value.(*String); ok {
				return a2.Value == b2.Value
			} else {
				return false
			}
		case *Bool:
			if b2, ok := b.Value.(*Bool); ok {
				return a2.Value == b2.Value
			} else {
				return false
			}
		default:
			// true so that we produce an error in this unexpected scenario
			return true
		}
	}

	patternListsEqual := func(a, b []SelectPattern) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if !patternsEqual(a[i], b[i]) {
				return false
			}
		}
		return true
	}

	for i, c := range result.Cases {
		// Check for duplicate patterns across different branches
		for _, d := range result.Cases[i+1:] {
			if patternListsEqual(c.Patterns, d.Patterns) {
				p.errorf("Found duplicate select patterns: %v", c.Patterns)
				return nil
			}
		}
		// check for duplicate bindings within this branch
		for i := range c.Patterns {
			if c.Patterns[i].Binding.Name != "" {
				for j := i + 1; j < len(c.Patterns); j++ {
					if c.Patterns[i].Binding.Name == c.Patterns[j].Binding.Name {
						p.errorf("Found duplicate select pattern binding: %s", c.Patterns[i].Binding.Name)
						return nil
					}
				}
			}
		}
		// Check that the only all-default cases is the last one
		if i < len(result.Cases)-1 {
			isAllDefault := true
			for _, x := range c.Patterns {
				if x2, ok := x.Value.(*String); !ok || x2.Value != default_select_branch_name {
					isAllDefault = false
					break
				}
			}
			if isAllDefault {
				p.errorf("Found a default select branch at index %d, expected it to be last (index %d)", i, len(result.Cases)-1)
				return nil
			}
		}
	}

	result.RBracePos = p.scanner.Position
	if !p.accept('}') {
		return nil
	}
	if !p.accept(')') {
		return nil
	}
	return result
}

func (p *parser) parseStringValue() *String {
	str, err := strconv.Unquote(p.scanner.TokenText())
	if err != nil {
		p.errorf("couldn't parse string: %s", err)
		return nil
	}

	value := &String{
		LiteralPos: p.scanner.Position,
		Value:      str,
	}
	p.accept(p.tok)
	return value
}

func (p *parser) parseIntValue() *Int64 {
	var str string
	literalPos := p.scanner.Position
	if p.tok == '-' {
		str += string(p.tok)
		p.accept(p.tok)
		if p.tok != scanner.Int {
			p.errorf("expected int; found %s", scanner.TokenString(p.tok))
			return nil
		}
	}
	str += p.scanner.TokenText()
	i, err := strconv.ParseInt(str, 10, 64)
	if err != nil {
		p.errorf("couldn't parse int: %s", err)
		return nil
	}

	value := &Int64{
		LiteralPos: literalPos,
		Value:      i,
		Token:      str,
	}
	p.accept(scanner.Int)
	return value
}

func (p *parser) parseListValue() *List {
	lBracePos := p.scanner.Position
	if !p.accept('[') {
		return nil
	}

	var elements []Expression
	for p.tok != ']' {
		element := p.parseExpression()
		elements = append(elements, element)

		if p.tok != ',' {
			// There was no comma, so the list is done.
			break
		}

		p.accept(',')
	}

	rBracePos := p.scanner.Position
	p.accept(']')

	return &List{
		LBracePos: lBracePos,
		RBracePos: rBracePos,
		Values:    elements,
	}
}

func (p *parser) parseMapValue() *Map {
	lBracePos := p.scanner.Position
	if !p.accept('{') {
		return nil
	}

	properties := p.parsePropertyList(false, false)

	rBracePos := p.scanner.Position
	p.accept('}')

	return &Map{
		LBracePos:  lBracePos,
		RBracePos:  rBracePos,
		Properties: properties,
	}
}

type Scope struct {
	vars              map[string]*Assignment
	preventInheriting map[string]bool
	parentScope       *Scope
}

func NewScope(s *Scope) *Scope {
	return &Scope{
		vars:              make(map[string]*Assignment),
		preventInheriting: make(map[string]bool),
		parentScope:       s,
	}
}

func (s *Scope) HandleAssignment(assignment *Assignment) error {
	switch assignment.Assigner {
	case "+=":
		if !s.preventInheriting[assignment.Name] && s.parentScope.Get(assignment.Name) != nil {
			return fmt.Errorf("modified non-local variable %q with +=", assignment.Name)
		}
		if old, ok := s.vars[assignment.Name]; !ok {
			return fmt.Errorf("modified non-existent variable %q with +=", assignment.Name)
		} else if old.Referenced {
			return fmt.Errorf("modified variable %q with += after referencing", assignment.Name)
		} else {
			newValue, err := evaluateOperator(s, '+', old.Value, assignment.Value)
			if err != nil {
				return err
			}
			old.Value = newValue
		}
	case "=":
		if old, ok := s.vars[assignment.Name]; ok {
			return fmt.Errorf("variable already set, previous assignment: %s", old)
		}

		if old := s.parentScope.Get(assignment.Name); old != nil && !s.preventInheriting[assignment.Name] {
			return fmt.Errorf("variable already set in inherited scope, previous assignment: %s", old)
		}

		if newValue, err := assignment.Value.Eval(s); err != nil {
			return err
		} else {
			assignment.Value = newValue
		}
		s.vars[assignment.Name] = assignment
	default:
		return fmt.Errorf("Unknown assigner '%s'", assignment.Assigner)
	}
	return nil
}

func (s *Scope) Get(name string) *Assignment {
	if s == nil {
		return nil
	}
	if a, ok := s.vars[name]; ok {
		return a
	}
	if s.preventInheriting[name] {
		return nil
	}
	return s.parentScope.Get(name)
}

func (s *Scope) GetLocal(name string) *Assignment {
	if s == nil {
		return nil
	}
	if a, ok := s.vars[name]; ok {
		return a
	}
	return nil
}

// DontInherit prevents this scope from inheriting the given variable from its
// parent scope.
func (s *Scope) DontInherit(name string) {
	s.preventInheriting[name] = true
}

func (s *Scope) String() string {
	var sb strings.Builder
	s.stringInner(&sb)
	return sb.String()
}

func (s *Scope) stringInner(sb *strings.Builder) {
	if s == nil {
		return
	}
	vars := make([]string, 0, len(s.vars))
	for k := range s.vars {
		vars = append(vars, k)
	}

	sort.Strings(vars)

	for _, v := range vars {
		sb.WriteString(s.vars[v].String())
		sb.WriteRune('\n')
	}

	s.parentScope.stringInner(sb)
}
