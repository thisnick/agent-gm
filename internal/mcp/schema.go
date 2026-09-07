package mcp

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

// JSON Schema, as much of it as spec section 8.2 asks for and no more.
//
// The rules that matter are stated there and enforced here:
//
//   - every input schema is **closed** (`additionalProperties: false`), and it
//     is enforced by the decoder rejecting unknown fields (decodeArgs) rather
//     than only declared, because a declaration nothing checks is a comment;
//   - every argument carries a description;
//   - a closed vocabulary is a real `enum` **plus** a `oneOf` of `const`s,
//     because `oneOf` is the only place JSON Schema lets a per-value
//     description live;
//   - an optional filter over a closed vocabulary admits `null` in both, so
//     that omitting the filter stays valid -- an `enum` that omitted `null`
//     would make the common case, not passing the filter at all, a schema
//     violation. The description still names the accepted values, so a model
//     reading only prose is not left guessing.

// Schema is a JSON Schema fragment.
type Schema struct {
	// Type is a string or an array of strings; a nullable value is
	// `["string", "null"]`.
	Type any `json:"type,omitempty"`
	// Description is mandatory on every argument (section 8.2) and on every
	// value of a closed vocabulary.
	Description string `json:"description,omitempty"`

	Properties map[string]*Schema `json:"properties,omitempty"`
	Required   []string           `json:"required,omitempty"`
	// AdditionalProperties is a pointer so that `false` is serialised and
	// an unset value is omitted.
	AdditionalProperties *bool `json:"additionalProperties,omitempty"`

	Items *Schema `json:"items,omitempty"`

	Enum  []any     `json:"enum,omitempty"`
	OneOf []*Schema `json:"oneOf,omitempty"`
	Const any       `json:"const,omitempty"`

	Format   string `json:"format,omitempty"`
	MinItems *int   `json:"minItems,omitempty"`
	MaxItems *int   `json:"maxItems,omitempty"`
}

func boolPtr(b bool) *bool { return &b }
func intPtr(i int) *int    { return &i }

// str is a plain required-shaped string.
func str(description string) *Schema {
	return &Schema{Type: "string", Description: description}
}

// nullableStr is an optional string: `["string", "null"]`, so a caller that
// wants no filter can pass null as readily as omit the key.
func nullableStr(description string) *Schema {
	return &Schema{Type: []string{"string", "null"}, Description: description}
}

func nullableBool(description string) *Schema {
	return &Schema{Type: []string{"boolean", "null"}, Description: description}
}

func nullableInt(description string) *Schema {
	return &Schema{Type: []string{"integer", "null"}, Description: description}
}

// Vocabulary is one closed vocabulary: the accepted values, each with the
// description that goes on its `const`.
type Vocabulary struct {
	// Name is what the vocabulary is called in prose, for the sentence that
	// names the values in the argument's own description.
	Name   string
	Values []VocabularyValue
}

// VocabularyValue is one accepted value and what it means.
type VocabularyValue struct {
	Value       string
	Description string
}

func (v Vocabulary) values() []string {
	out := make([]string, 0, len(v.Values))
	for _, val := range v.Values {
		out = append(out, val.Value)
	}
	return out
}

// sentence renders "one of `a`, `b` or `c`" for an argument description.
func (v Vocabulary) sentence() string {
	names := v.values()
	for i, n := range names {
		names[i] = "`" + n + "`"
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
	}
}

// enumSchema builds a closed vocabulary's schema: a real `enum`, plus a
// `oneOf` of `const`s so each value carries its own description.
//
// nullable adds `null` to the type, the enum and the oneOf. It is set for
// every vocabulary that is an optional FILTER, because an enum that omitted
// null would make omitting the filter invalid -- and omitting a filter is the
// ordinary case, not an error.
func enumSchema(description string, v Vocabulary, nullable bool) *Schema {
	s := &Schema{Description: description}
	oneOf := make([]*Schema, 0, len(v.Values)+1)
	enum := make([]any, 0, len(v.Values)+1)
	for _, val := range v.Values {
		enum = append(enum, val.Value)
		oneOf = append(oneOf, &Schema{Const: val.Value, Description: val.Description})
	}
	if nullable {
		s.Type = []string{"string", "null"}
		enum = append(enum, nil)
		oneOf = append(oneOf, &Schema{
			Const:       nil,
			Description: "null, or the argument omitted altogether: do not filter on " + v.Name + " at all.",
		})
	} else {
		s.Type = "string"
	}
	s.Enum = enum
	s.OneOf = oneOf
	return s
}

// --- output schemas ---------------------------------------------------------

// schemaForType turns a DTO's Go type into a JSON Schema, by reflection over
// the very struct the REST handler serialises.
//
// Section 8.2 asks every tool to declare "an `outputSchema` describing the
// envelope it really returns, with `data` typed by that tool's own DTO". Doing
// it by reflection rather than by hand is what makes "really returns" true: a
// field added to a DTO appears in the tool's output schema without anybody
// remembering, and a hand-written copy that had drifted would be indetectable.
//
// The mapping is deliberately shallow on two Go types the DTOs use for
// deliberately open values -- `any` and `map[string]any` -- which serialise as
// an unconstrained object. The alternative would be to invent a shape the
// handler does not promise.
func schemaForType(t reflect.Type) *Schema {
	return schemaFor(t, map[reflect.Type]bool{})
}

func schemaFor(t reflect.Type, seen map[reflect.Type]bool) *Schema {
	switch t.Kind() {
	case reflect.Pointer:
		// A pointer field is the DTO layer's "this may be JSON null".
		inner := schemaFor(t.Elem(), seen)
		return nullable(inner)
	case reflect.String:
		return &Schema{Type: "string"}
	case reflect.Bool:
		return &Schema{Type: "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return &Schema{Type: "integer"}
	case reflect.Float32, reflect.Float64:
		return &Schema{Type: "number"}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return &Schema{Type: "string", Format: "byte"}
		}
		return &Schema{Type: "array", Items: schemaFor(t.Elem(), seen)}
	case reflect.Map:
		return &Schema{Type: "object"}
	case reflect.Interface:
		// `any`: the handler promises nothing about the shape, so neither
		// does the schema.
		return &Schema{}
	case reflect.Struct:
		if seen[t] {
			// A self-referential DTO would otherwise recurse for ever. None
			// does today; the guard is here so that one added later fails
			// safe rather than hanging a server at startup.
			return &Schema{Type: "object"}
		}
		seen[t] = true
		defer delete(seen, t)
		props := map[string]*Schema{}
		var required []string
		collectFields(t, seen, props, &required)
		sort.Strings(required)
		return &Schema{Type: "object", Properties: props, Required: required}
	default:
		return &Schema{}
	}
}

// collectFields walks a struct's exported fields, following the `json` tags
// and flattening embedded structs the way encoding/json does -- which matters
// for `accountDetailDTO`, whose list fields are embedded rather than repeated.
func collectFields(t reflect.Type, seen map[reflect.Type]bool, props map[string]*Schema, required *[]string) {
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				collectFields(ft, seen, props, required)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		props[name] = schemaFor(f.Type, seen)
		if !strings.Contains(opts, "omitempty") {
			// A field without omitempty is always present on the wire, which
			// is a promise worth making: `message_id` is null until the echo
			// lands, and a client must be able to tell "not yet" from "this
			// route has no such key".
			*required = append(*required, name)
		}
	}
}

// nullable widens a schema's type to admit null.
func nullable(s *Schema) *Schema {
	switch t := s.Type.(type) {
	case string:
		s.Type = []string{t, "null"}
	case nil:
		// An unconstrained schema already admits null.
	}
	return s
}

// MarshalJSON is here only so that a Schema with no fields set serialises as
// `{}` -- an unconstrained schema -- rather than as `null`.
func (s *Schema) MarshalJSON() ([]byte, error) {
	type plain Schema
	return json.Marshal((*plain)(s))
}
