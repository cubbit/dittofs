package api

import (
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"

	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// fieldError is a refused request value: ErrInvalid, naming the field.
type fieldError struct{ field, detail string }

func (e *fieldError) Error() string { return "invalid " + e.field + ": " + e.detail }
func (e *fieldError) Unwrap() error { return vfserr.ErrInvalid }

// decode reads one JSON value from r into v, refusing a field v does not
// declare, a value of the wrong type, and anything after the value, each as a
// *fieldError naming the field where JSON names one. An empty body is the
// empty request: v keeps its zero value.
func decode(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if err == io.EOF {
			return nil
		}
		return decodeError(err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return &fieldError{detail: "data after the request's value"}
	}
	return nil
}

// decodeError names the field encoding/json refused: an unknown one it gives
// only in its message, a mistyped one in its error's Field.
func decodeError(err error) error {
	if name, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		if n, uerr := strconv.Unquote(name); uerr == nil {
			name = n
		}
		return &fieldError{field: name, detail: "unknown field"}
	}
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) {
		return &fieldError{field: te.Field, detail: "want " + te.Type.String() + ", got " + te.Value}
	}
	return &fieldError{detail: err.Error()}
}
