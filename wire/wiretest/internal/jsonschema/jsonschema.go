// Package jsonschema validates JSON values against a JSON Schema (draft
// 2020-12) of the keywords the protocol's schema, schema/apron.schema.json,
// uses, and no others: so that package conformance checks every frame
// against it, as the protocol's conformance suite does, without a module
// of its own.
//
// The assertions and applicators it knows are type, const, enum, minimum,
// exclusiveMinimum, maxLength, pattern, required, properties,
// additionalProperties, propertyNames, dependentRequired,
// dependentSchemas, items, minItems, contains (with minContains and
// maxContains), allOf, anyOf, oneOf, not, if, then, else, and $ref to a
// definition of the same document (#/$defs/Name). The annotations
// $schema, $id, $comment, title, description, default, and examples are
// ignored. Compile refuses a schema with any other keyword, so that a
// schema that comes to use one is not checked short of what it says.
//
// A pattern is an ECMA-262 regular expression, as JSON Schema has it, which
// Compile translates to Go's syntax: \s is ECMA-262's white space and line
// terminators, \d is [0-9], \uXXXX is a code point, and . matches anything
// but a line terminator. One that uses what Go's syntax lacks, such as a
// lookahead or a backreference, is refused. Python's re, which the
// protocol's Python suite validates with, differs from ECMA-262 in two
// ways that only an invalid value can show: its $ also matches before a
// final newline, and its \s does not match U+FEFF; this package follows
// ECMA-262.
package jsonschema

import (
	"encoding/json/v2"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Schema is a compiled schema document. It is safe for concurrent use.
type Schema struct {
	root *node
	defs map[string]*node
}

// Error is one way a value does not match a schema: where, as a JSON
// pointer into the value (such as /params/messages/0), and what.
type Error struct {
	Path    string
	Message string
}

func (e Error) Error() string { return e.Path + ": " + e.Message }

// node is a compiled schema: a boolean schema, or an object's keywords.
type node struct {
	// always is set for a boolean schema, true or false.
	always *bool

	ref     string
	refNode *node

	types []string

	hasConst bool
	constant any
	enum     []any

	minimum, exclusiveMinimum *float64
	maxLength                 *int
	pattern                   *regexp.Regexp
	patternSource             string

	required             []string
	properties           map[string]*node
	additionalProperties *node
	propertyNames        *node
	dependentRequired    map[string][]string
	dependentSchemas     map[string]*node

	items                    *node
	minItems                 *int
	contains                 *node
	minContains, maxContains *int

	allOf, anyOf, oneOf []*node
	not                 *node
	ifNode, then, els   *node
}

// annotations are the keywords that assert nothing, which are ignored.
var annotations = map[string]bool{
	"$schema": true, "$id": true, "$comment": true, "$defs": true,
	"title": true, "description": true, "default": true, "examples": true,
}

// Compile compiles a schema document, such as apron.schema.json: its
// root, and each of its $defs.
func Compile(document []byte) (*Schema, error) {
	var doc any
	if err := json.Unmarshal(document, &doc); err != nil {
		return nil, err
	}
	s := &Schema{defs: map[string]*node{}}
	c := &compiler{s: s}
	if obj, ok := doc.(map[string]any); ok {
		if defs, ok := obj["$defs"]; ok {
			defs, ok := defs.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("jsonschema: $defs is not an object")
			}
			for name, def := range defs {
				n, err := c.compile(def, "/$defs/"+name)
				if err != nil {
					return nil, err
				}
				s.defs[name] = n
			}
		}
	}
	root, err := c.compile(doc, "")
	if err != nil {
		return nil, err
	}
	s.root = root
	for _, n := range c.refs {
		name, ok := strings.CutPrefix(n.ref, "#/$defs/")
		if n.ref == "#" {
			n.refNode = s.root
			continue
		}
		if n.refNode = s.defs[name]; !ok || n.refNode == nil {
			return nil, fmt.Errorf("jsonschema: $ref %q is not a definition of the document", n.ref)
		}
	}
	return s, nil
}

// Has reports whether the document defines name in its $defs.
func (s *Schema) Has(name string) bool {
	_, ok := s.defs[name]
	return ok
}

// Validate returns the ways value does not match the definition name, or
// the document's root when name is "": none when it matches. value is as
// encoding/json decodes JSON into an any: a map[string]any, []any,
// string, float64, bool, or nil. A name the document does not define
// matches everything, as Has tells.
func (s *Schema) Validate(value any, name string) []Error {
	n := s.root
	if name != "" {
		if n = s.defs[name]; n == nil {
			return nil
		}
	}
	var errs []Error
	n.validate(value, "", &errs)
	return errs
}

// compiler compiles the nodes of one document, and keeps those with a
// $ref, which are resolved once every definition is compiled.
type compiler struct {
	s    *Schema
	refs []*node
}

func (c *compiler) compile(v any, at string) (*node, error) {
	if b, ok := v.(bool); ok {
		return &node{always: &b}, nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("jsonschema: %s: a schema is an object or a boolean", orRoot(at))
	}
	n := &node{}
	sub := func(key string) (*node, error) { return c.compile(obj[key], at+"/"+key) }
	subs := func(key string) ([]*node, error) {
		list, ok := obj[key].([]any)
		if !ok || len(list) == 0 {
			return nil, fmt.Errorf("jsonschema: %s/%s is not a non-empty array", orRoot(at), key)
		}
		var nodes []*node
		for i, item := range list {
			sn, err := c.compile(item, at+"/"+key+"/"+strconv.Itoa(i))
			if err != nil {
				return nil, err
			}
			nodes = append(nodes, sn)
		}
		return nodes, nil
	}
	schemaMap := func(key string) (map[string]*node, error) {
		m, ok := obj[key].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("jsonschema: %s/%s is not an object", orRoot(at), key)
		}
		nodes := map[string]*node{}
		for name, item := range m {
			sn, err := c.compile(item, at+"/"+key+"/"+name)
			if err != nil {
				return nil, err
			}
			nodes[name] = sn
		}
		return nodes, nil
	}
	number := func(key string) (*float64, error) {
		f, ok := obj[key].(float64)
		if !ok {
			return nil, fmt.Errorf("jsonschema: %s/%s is not a number", orRoot(at), key)
		}
		return &f, nil
	}
	count := func(key string) (*int, error) {
		f, ok := obj[key].(float64)
		if !ok || f < 0 || f != math.Trunc(f) {
			return nil, fmt.Errorf("jsonschema: %s/%s is not a non-negative integer", orRoot(at), key)
		}
		i := int(f)
		return &i, nil
	}
	strs := func(key string, v any) ([]string, error) {
		list, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("jsonschema: %s/%s is not an array of strings", orRoot(at), key)
		}
		var out []string
		for _, item := range list {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("jsonschema: %s/%s is not an array of strings", orRoot(at), key)
			}
			out = append(out, s)
		}
		return out, nil
	}
	var err error
	for _, key := range slices.Sorted(maps.Keys(obj)) {
		value := obj[key]
		switch key {
		case "$ref":
			ref, ok := value.(string)
			if !ok || ref != "#" && !strings.HasPrefix(ref, "#/$defs/") {
				return nil, fmt.Errorf("jsonschema: %s/$ref %v is not a definition of the document", orRoot(at), value)
			}
			n.ref = ref
			c.refs = append(c.refs, n)
		case "type":
			switch t := value.(type) {
			case string:
				n.types = []string{t}
			default:
				n.types, err = strs(key, value)
			}
			for _, t := range n.types {
				if !slices.Contains([]string{"null", "boolean", "object", "array", "number", "integer", "string"}, t) {
					return nil, fmt.Errorf("jsonschema: %s/type %q is not a type", orRoot(at), t)
				}
			}
		case "const":
			n.hasConst, n.constant = true, value
		case "enum":
			var ok bool
			if n.enum, ok = value.([]any); !ok {
				return nil, fmt.Errorf("jsonschema: %s/enum is not an array", orRoot(at))
			}
		case "minimum":
			n.minimum, err = number(key)
		case "exclusiveMinimum":
			n.exclusiveMinimum, err = number(key)
		case "maxLength":
			n.maxLength, err = count(key)
		case "pattern":
			source, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("jsonschema: %s/pattern is not a string", orRoot(at))
			}
			n.patternSource = source
			if n.pattern, err = translate(source); err != nil {
				return nil, fmt.Errorf("jsonschema: %s/pattern: %w", orRoot(at), err)
			}
		case "required":
			n.required, err = strs(key, value)
		case "properties":
			n.properties, err = schemaMap(key)
		case "additionalProperties":
			n.additionalProperties, err = sub(key)
		case "propertyNames":
			n.propertyNames, err = sub(key)
		case "dependentRequired":
			m, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("jsonschema: %s/dependentRequired is not an object", orRoot(at))
			}
			n.dependentRequired = map[string][]string{}
			for name, list := range m {
				if n.dependentRequired[name], err = strs(key+"/"+name, list); err != nil {
					return nil, err
				}
			}
		case "dependentSchemas":
			n.dependentSchemas, err = schemaMap(key)
		case "items":
			n.items, err = sub(key)
		case "minItems":
			n.minItems, err = count(key)
		case "contains":
			n.contains, err = sub(key)
		case "minContains":
			n.minContains, err = count(key)
		case "maxContains":
			n.maxContains, err = count(key)
		case "allOf":
			n.allOf, err = subs(key)
		case "anyOf":
			n.anyOf, err = subs(key)
		case "oneOf":
			n.oneOf, err = subs(key)
		case "not":
			n.not, err = sub(key)
		case "if":
			n.ifNode, err = sub(key)
		case "then":
			n.then, err = sub(key)
		case "else":
			n.els, err = sub(key)
		default:
			if !annotations[key] {
				return nil, fmt.Errorf("jsonschema: %s: keyword %q is not supported", orRoot(at), key)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	return n, nil
}

func orRoot(at string) string {
	if at == "" {
		return "the root"
	}
	return at
}

// valid reports whether value matches n.
func (n *node) valid(value any) bool {
	var errs []Error
	n.validate(value, "", &errs)
	return len(errs) == 0
}

// validate appends to errs the ways value, at path, does not match n.
func (n *node) validate(value any, path string, errs *[]Error) {
	fail := func(format string, args ...any) {
		*errs = append(*errs, Error{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	if n.always != nil {
		if !*n.always {
			fail("%s is not allowed", short(value))
		}
		return
	}
	if n.refNode != nil {
		n.refNode.validate(value, path, errs)
	}
	if len(n.types) > 0 && !slices.ContainsFunc(n.types, func(t string) bool { return isType(value, t) }) {
		fail("%s is not of type %s", short(value), strings.Join(n.types, ", "))
	}
	if n.hasConst && !equal(value, n.constant) {
		fail("%s was expected", short(n.constant))
	}
	if n.enum != nil && !slices.ContainsFunc(n.enum, func(e any) bool { return equal(value, e) }) {
		fail("%s is not one of %s", short(value), short(n.enum))
	}

	switch value := value.(type) {
	case float64:
		if n.minimum != nil && value < *n.minimum {
			fail("%v is less than the minimum of %v", value, *n.minimum)
		}
		if n.exclusiveMinimum != nil && value <= *n.exclusiveMinimum {
			fail("%v is less than or equal to the minimum of %v", value, *n.exclusiveMinimum)
		}
	case string:
		if n.maxLength != nil && utf8.RuneCountInString(value) > *n.maxLength {
			fail("%s is longer than %d characters", short(value), *n.maxLength)
		}
		if n.pattern != nil && !n.pattern.MatchString(value) {
			fail("%s does not match %q", short(value), n.patternSource)
		}
	case map[string]any:
		for _, key := range n.required {
			if _, ok := value[key]; !ok {
				fail("%q is a required property", key)
			}
		}
		for _, key := range slices.Sorted(maps.Keys(value)) {
			member := value[key]
			at := path + "/" + key
			if sn, ok := n.properties[key]; ok {
				sn.validate(member, at, errs)
			} else if n.additionalProperties != nil {
				n.additionalProperties.validate(member, at, errs)
			}
			if n.propertyNames != nil && !n.propertyNames.valid(key) {
				fail("property name %q does not match its schema", key)
			}
			for _, dependent := range n.dependentRequired[key] {
				if _, ok := value[dependent]; !ok {
					fail("%q is a dependency of %q", dependent, key)
				}
			}
			if sn, ok := n.dependentSchemas[key]; ok {
				sn.validate(value, path, errs)
			}
		}
	case []any:
		if n.minItems != nil && len(value) < *n.minItems {
			fail("%s has fewer than %d items", short(value), *n.minItems)
		}
		for i, item := range value {
			if n.items != nil {
				n.items.validate(item, path+"/"+strconv.Itoa(i), errs)
			}
		}
		if n.contains != nil {
			matches := 0
			for _, item := range value {
				if n.contains.valid(item) {
					matches++
				}
			}
			least := 1
			if n.minContains != nil {
				least = *n.minContains
			}
			switch {
			case matches < least:
				fail("%s has %d items that match contains, fewer than %d", short(value), matches, least)
			case n.maxContains != nil && matches > *n.maxContains:
				fail("%s has %d items that match contains, more than %d", short(value), matches, *n.maxContains)
			}
		}
	}

	for _, sn := range n.allOf {
		sn.validate(value, path, errs)
	}
	if n.anyOf != nil && !slices.ContainsFunc(n.anyOf, func(sn *node) bool { return sn.valid(value) }) {
		fail("%s is not valid under any of the given schemas", short(value))
	}
	if n.oneOf != nil {
		matched := 0
		for _, sn := range n.oneOf {
			if sn.valid(value) {
				matched++
			}
		}
		switch {
		case matched == 0:
			fail("%s is not valid under any of the given schemas", short(value))
		case matched > 1:
			fail("%s is valid under %d of the given schemas, not one", short(value), matched)
		}
	}
	if n.not != nil && n.not.valid(value) {
		fail("%s should not be valid under its not schema", short(value))
	}
	if n.ifNode != nil {
		if n.ifNode.valid(value) {
			if n.then != nil {
				n.then.validate(value, path, errs)
			}
		} else if n.els != nil {
			n.els.validate(value, path, errs)
		}
	}
}

// isType reports whether value is of a JSON Schema type: an integer is a
// number with no fraction, such as 1.0.
func isType(value any, t string) bool {
	switch value := value.(type) {
	case nil:
		return t == "null"
	case bool:
		return t == "boolean"
	case string:
		return t == "string"
	case []any:
		return t == "array"
	case map[string]any:
		return t == "object"
	case float64:
		return t == "number" || t == "integer" && value == math.Trunc(value) && !math.IsInf(value, 0)
	}
	return false
}

// equal reports whether two JSON values are equal: numbers by value, and a
// boolean only to a boolean.
func equal(a, b any) bool {
	switch a := a.(type) {
	case nil:
		return b == nil
	case bool:
		b, ok := b.(bool)
		return ok && a == b
	case float64:
		b, ok := b.(float64)
		return ok && a == b
	case string:
		b, ok := b.(string)
		return ok && a == b
	case []any:
		b, ok := b.([]any)
		return ok && slices.EqualFunc(a, b, equal)
	case map[string]any:
		b, ok := b.(map[string]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for k, v := range a {
			if w, ok := b[k]; !ok || !equal(v, w) {
				return false
			}
		}
		return true
	}
	return false
}

// short renders a value for a message, cut to 80 bytes.
func short(value any) string {
	raw, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		return fmt.Sprint(value)
	}
	if len(raw) > 80 {
		return string(raw[:77]) + "..."
	}
	return string(raw)
}

// The classes of ECMA-262's escapes that Go's syntax spells otherwise, as
// the inside of a bracket expression: \s is white space and line
// terminators (ECMA-262 §12.2, §12.3), and \d the ASCII digits.
const (
	spaceClass = `\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`
	digitClass = `0-9`
	// notLineTerminator is ECMA-262's ., which matches anything but a line
	// terminator.
	notLineTerminator = `[^\n\r\x{2028}\x{2029}]`
)

// translate compiles an ECMA-262 pattern as Go's regexp, rewriting what
// the two spell differently; what Go's syntax lacks fails to compile.
func translate(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	inClass := false
	for i := 0; i < len(pattern); i++ {
		ch := pattern[i]
		switch {
		case ch == '\\' && i+1 < len(pattern):
			i++
			switch e := pattern[i]; e {
			case 's', 'd':
				class := spaceClass
				if e == 'd' {
					class = digitClass
				}
				if inClass {
					b.WriteString(class)
				} else {
					b.WriteString("[" + class + "]")
				}
			case 'S', 'D':
				if inClass {
					return nil, fmt.Errorf("\\%c within a class is not supported", e)
				}
				class := spaceClass
				if e == 'D' {
					class = digitClass
				}
				b.WriteString("[^" + class + "]")
			case 'u':
				if i+4 >= len(pattern) {
					return nil, fmt.Errorf("a \\u escape needs four hex digits")
				}
				hex := pattern[i+1 : i+5]
				if _, err := strconv.ParseUint(hex, 16, 16); err != nil {
					return nil, fmt.Errorf("\\u%s is not a code point", hex)
				}
				b.WriteString(`\x{` + hex + `}`)
				i += 4
			default:
				b.WriteByte('\\')
				b.WriteByte(e)
			}
		case ch == '[' && !inClass:
			inClass = true
			b.WriteByte(ch)
			// A ] or ^] right after [ is literal in neither syntax the
			// same way; patterns that need it are not supported.
			if strings.HasPrefix(pattern[i+1:], "]") || strings.HasPrefix(pattern[i+1:], "^]") {
				return nil, fmt.Errorf("a class that starts with ] is not supported")
			}
		case ch == ']' && inClass:
			inClass = false
			b.WriteByte(ch)
		case ch == '.' && !inClass:
			b.WriteString(notLineTerminator)
		default:
			b.WriteByte(ch)
		}
	}
	return regexp.Compile(b.String())
}
