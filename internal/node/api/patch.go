package api

import (
	"bytes"
	"encoding/json"
	"io"
)

// mergePatch applies patch to doc as a JSON merge patch (RFC 7396): a member
// present in the patch is set, recursively for an object, and a null member
// removes the target's, which decoding the result into a fresh value then
// reads as the field's default. A patch that is not an object replaces doc
// whole. Numbers are carried as written, so a large integer keeps its digits.
func mergePatch(doc, patch []byte) ([]byte, error) {
	p, err := decodeAny(patch)
	if err != nil {
		return nil, &fieldError{detail: "patch: " + err.Error()}
	}
	var d any
	if len(bytes.TrimSpace(doc)) > 0 {
		if d, err = decodeAny(doc); err != nil {
			return nil, err
		}
	}
	return json.Marshal(merge(d, p))
}

func merge(target, patch any) any {
	pm, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	tm, ok := target.(map[string]any)
	if !ok {
		tm = map[string]any{}
	}
	for k, v := range pm {
		if v == nil {
			delete(tm, k)
		} else {
			tm[k] = merge(tm[k], v)
		}
	}
	return tm
}

// decodeAny reads one JSON value, its numbers as json.Number.
func decodeAny(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, &fieldError{detail: "data after the value"}
	}
	return v, nil
}
