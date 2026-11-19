package api

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// fieldError is a refused request value: ErrInvalid, naming the field.
type fieldError struct{ field, detail string }

func (e *fieldError) Error() string { return "invalid " + e.field + ": " + e.detail }
func (e *fieldError) Unwrap() error { return vfserr.ErrInvalid }

// decode reads one JSON value from r into v, refusing a key repeated in one
// object, a key that is not exactly the JSON name of a field v declares, a
// value of the wrong type, and anything after the value, each as a
// *fieldError naming the field where there is one. encoding/json alone would
// match a key to a field ignoring case and keep the last of a repeated key,
// so a client's {"Name":…} beside {"name":…} would change nothing it can
// see. An empty body is the empty request: v keeps its zero value.
func decode(r io.Reader, v any) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return &fieldError{detail: err.Error()}
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	if err := checkKeys(b, reflect.TypeOf(v)); err != nil {
		return err
	}
	var te *json.UnmarshalTypeError
	switch err := json.Unmarshal(b, v); {
	case errors.As(err, &te):
		return &fieldError{field: te.Field, detail: "want " + jsonKind(te.Type) + ", got " + strings.Replace(te.Value, "bool", "boolean", 1)}
	case err != nil:
		return &fieldError{detail: err.Error()}
	}
	return nil
}

// jsonKind is what a value of Go type t is written as in JSON, in the
// client's terms rather than Go's.
func jsonKind(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if reflect.PointerTo(t).Implements(textUnmarshaler) {
		return "string"
	}
	switch t.Kind() {
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return "integer"
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return "non-negative integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.String:
		return "string"
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return "string"
		}
		return "array"
	}
	return "object"
}

// maxDepth bounds how deeply a request body's objects and arrays nest.
// checkKeys reads a body before encoding/json does, so encoding/json's own
// bound never applies to it.
const maxDepth = 64

// checkKeys refuses the first JSON value b holds unless no key repeats in
// any of its objects, its objects and arrays nest at most maxDepth deep and,
// under a t that is not nil, every key of an object read into a struct is
// exactly the JSON name of one of its fields. What follows the value is its
// callers' to refuse.
func checkKeys(b []byte, t reflect.Type) error {
	return walk(json.NewDecoder(bytes.NewReader(b)), t, nil, 0)
}

var unmarshaler = reflect.TypeFor[json.Unmarshaler]()
var textUnmarshaler = reflect.TypeFor[encoding.TextUnmarshaler]()

// walk reads the next value from dec as checkKeys checks it, depth objects
// and arrays down, at the field path, one key per element and joined only to
// name a refused field. A type that decodes itself is checked for repeated
// keys only.
func walk(dec *json.Decoder, t reflect.Type, path []string, depth int) error {
	tok, err := dec.Token()
	if err != nil {
		return &fieldError{detail: err.Error()}
	}
	if (tok == json.Delim('{') || tok == json.Delim('[')) && depth == maxDepth {
		return &fieldError{field: strings.Join(path, "."), detail: "nested more than " + strconv.Itoa(maxDepth) + " deep"}
	}
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t != nil && (reflect.PointerTo(t).Implements(unmarshaler) || reflect.PointerTo(t).Implements(textUnmarshaler)) {
		t = nil
	}
	switch tok {
	case json.Delim('{'):
		var fields map[string]reflect.Type
		if t != nil && t.Kind() == reflect.Struct {
			fields = map[string]reflect.Type{}
			for _, f := range wireFields(t, nil) {
				fields[f.name] = f.typ
			}
		}
		seen := map[string]bool{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return &fieldError{detail: err.Error()}
			}
			k := kt.(string)
			at := append(path, k)
			if seen[k] {
				return &fieldError{field: strings.Join(at, "."), detail: "repeated key"}
			}
			seen[k] = true
			var et reflect.Type
			switch {
			case fields != nil:
				f, ok := fields[k]
				if !ok {
					return &fieldError{field: strings.Join(at, "."), detail: "unknown field"}
				}
				et = f
			case t != nil && t.Kind() == reflect.Map:
				et = t.Elem()
			}
			if err := walk(dec, et, at, depth+1); err != nil {
				return err
			}
		}
	case json.Delim('['):
		var et reflect.Type
		if t != nil && (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) {
			et = t.Elem()
		}
		for dec.More() {
			if err := walk(dec, et, path, depth+1); err != nil {
				return err
			}
		}
	default:
		return nil
	}
	if _, err := dec.Token(); err != nil { // the closing delimiter
		return &fieldError{detail: err.Error()}
	}
	return nil
}

// wireField is a field of a struct as encoding/json reads and writes it: its
// name, its type, and whether omitempty or omitzero leaves it out when
// empty.
type wireField struct {
	name     string
	typ      reflect.Type
	optional bool
}

// wireFields appends to out each field of struct t, in order, by the name
// encoding/json reads it under, an untagged embedded struct's fields as t's
// own.
func wireFields(t reflect.Type, out []wireField) []wireField {
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		ft := f.Type
		if f.Anonymous && name == "" {
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				out = wireFields(ft, out)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		optional := slices.ContainsFunc(strings.Split(opts, ","), func(o string) bool { return o == "omitempty" || o == "omitzero" })
		out = append(out, wireField{name, f.Type, optional})
	}
	return out
}
