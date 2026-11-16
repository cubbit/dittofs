package api

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"io"
	"reflect"
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
		return &fieldError{field: te.Field, detail: "want " + te.Type.String() + ", got " + te.Value}
	case err != nil:
		return &fieldError{detail: err.Error()}
	}
	return nil
}

// checkKeys refuses b unless it holds one JSON value with no key repeated in
// any object, and, under a t that is not nil, every key of an object read
// into a struct is exactly the JSON name of one of its fields.
func checkKeys(b []byte, t reflect.Type) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := walk(dec, t, ""); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return &fieldError{detail: "data after the value"}
	}
	return nil
}

var unmarshaler = reflect.TypeFor[json.Unmarshaler]()
var textUnmarshaler = reflect.TypeFor[encoding.TextUnmarshaler]()

// walk reads the next value from dec as checkKeys checks it, at the field
// path. A type that decodes itself is checked for repeated keys only.
func walk(dec *json.Decoder, t reflect.Type, path string) error {
	tok, err := dec.Token()
	if err != nil {
		return &fieldError{detail: err.Error()}
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
			fields = jsonFields(t, map[string]reflect.Type{})
		}
		seen := map[string]bool{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return &fieldError{detail: err.Error()}
			}
			k := kt.(string)
			name := k
			if path != "" {
				name = path + "." + k
			}
			if seen[k] {
				return &fieldError{field: name, detail: "repeated key"}
			}
			seen[k] = true
			var et reflect.Type
			switch {
			case fields != nil:
				f, ok := fields[k]
				if !ok {
					return &fieldError{field: name, detail: "unknown field"}
				}
				et = f
			case t != nil && t.Kind() == reflect.Map:
				et = t.Elem()
			}
			if err := walk(dec, et, name); err != nil {
				return err
			}
		}
	case json.Delim('['):
		var et reflect.Type
		if t != nil && (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) {
			et = t.Elem()
		}
		for dec.More() {
			if err := walk(dec, et, path); err != nil {
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

// jsonFields adds to out each field of struct t by the name encoding/json
// reads it under, an untagged embedded struct's fields as t's own.
func jsonFields(t reflect.Type, out map[string]reflect.Type) map[string]reflect.Type {
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		ft := f.Type
		if f.Anonymous && name == "" {
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				jsonFields(ft, out)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
	return out
}
