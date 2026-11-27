package api

import (
	"bytes"
	"encoding/base64"
)

// cursorVersion is the first byte of every cursor this node writes.
const cursorVersion = 1

// encodeCursor is the cursor that resumes a listing after the store key
// after: a version byte and the key, base64url without padding.
//
// decision: a cursor is the store's resume key, encoded and not signed, so a
// client can forge one. decodeCursor refuses a key outside the listing's
// range, so a forged cursor resumes the same listing at another key and
// reads nothing the listing would not. Overturned by a listing whose range
// crosses records the caller may not read: its cursor must then be sealed.
func encodeCursor(after []byte) string {
	return base64.RawURLEncoding.EncodeToString(append([]byte{cursorVersion}, after...))
}

// decodeCursor returns the key s resumes after, refusing a cursor that does
// not decode, of another version, or whose key lies outside [lo, hi), a nil
// hi bounding nothing, as a *fieldError naming after.
func decodeCursor(s string, lo, hi []byte) ([]byte, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	switch {
	case err != nil:
		return nil, &fieldError{field: "after", detail: "not a cursor"}
	case len(b) == 0 || b[0] != cursorVersion:
		return nil, &fieldError{field: "after", detail: "not a cursor of this version"}
	}
	key := b[1:]
	if bytes.Compare(key, lo) < 0 || hi != nil && bytes.Compare(key, hi) >= 0 {
		return nil, &fieldError{field: "after", detail: "not a cursor of this listing"}
	}
	return key, nil
}
