package remote

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// sentinels is the closed set (RFC 4 §4.8), by name.
var sentinels = []struct {
	name string
	err  error
}{
	{"ErrNotFound", ErrNotFound},
	{"ErrInvalid", ErrInvalid},
	{"ErrDenied", ErrDenied},
	{"ErrTransient", ErrTransient},
	{"ErrCorrupt", ErrCorrupt},
	{"ErrThrottled", ErrThrottled},
	{"ErrMalformed", ErrMalformed},
}

// Each sentinel matches itself and, for the two that wrap, exactly the one
// they wrap: ErrMalformed is a kind of ErrCorrupt (RFC 4 §3.4) and ErrThrottled
// a kind of ErrTransient, so a caller testing the wider one still classifies
// them, and a caller that must treat them apart tests the narrower one first.
func TestSentinelsMatchOnlyThemselvesAndWhatTheyWrap(t *testing.T) {
	wraps := map[string]string{"ErrThrottled": "ErrTransient", "ErrMalformed": "ErrCorrupt"}

	for _, a := range sentinels {
		for _, b := range sentinels {
			want := a.name == b.name || wraps[a.name] == b.name
			if got := errors.Is(a.err, b.err); got != want {
				t.Errorf("errors.Is(%s, %s) = %v, want %v", a.name, b.name, got, want)
			}
		}
	}
}

// A refusal of each cause matches ErrDenied and nothing else of the set, even
// wrapped as a store wraps it, and errors.As recovers its cause and code; its
// message names the cause's word and the code.
func TestDeniedErrorCarriesItsCause(t *testing.T) {
	for _, tc := range []struct {
		cause Cause
		word  string
	}{
		{CauseAccess, "access"},
		{CauseQuota, "quota"},
		{CauseSize, "size"},
	} {
		err := fmt.Errorf("put: %w", &DeniedError{Cause: tc.cause, Code: "SomeCode"})

		for _, s := range sentinels {
			if got, want := errors.Is(err, s.err), s.err == ErrDenied; got != want {
				t.Errorf("cause %s: errors.Is(err, %s) = %v, want %v", tc.word, s.name, got, want)
			}
		}

		var d *DeniedError
		if !errors.As(err, &d) {
			t.Fatalf("cause %s: errors.As found no *DeniedError in %v", tc.word, err)
		}

		if d.Cause != tc.cause || d.Code != "SomeCode" {
			t.Errorf("cause %s: recovered %+v, want cause %d and code SomeCode", tc.word, *d, tc.cause)
		}

		if got := tc.cause.String(); got != tc.word {
			t.Errorf("Cause(%d).String() = %q, want %q", tc.cause, got, tc.word)
		}

		if msg := err.Error(); !strings.Contains(msg, "("+tc.word+")") || !strings.Contains(msg, "SomeCode") {
			t.Errorf("cause %s: message %q does not name both the cause's word and the code", tc.word, msg)
		}
	}
}

// A cause outside the set prints as its number, never as one of the words.
func TestUnknownCausePrintsItsNumber(t *testing.T) {
	if got := Cause(9).String(); got != "Cause(9)" {
		t.Errorf("Cause(9).String() = %q, want %q", got, "Cause(9)")
	}
}

// The zero StoredSum is a get whose service reported no checksum (RFC 4 §4.4).
func TestZeroStoredSumReportsNone(t *testing.T) {
	if got := (StoredSum{}).Kind; got != ChecksumNone {
		t.Errorf("zero StoredSum's Kind = %d, want ChecksumNone (%d)", got, ChecksumNone)
	}
}

// A typed-nil *DeniedError returned as an error prints instead of panicking.
func TestNilDeniedErrorPrints(t *testing.T) {
	var e *DeniedError
	if got := e.Error(); got != ErrDenied.Error() {
		t.Errorf("(*DeniedError)(nil).Error() = %q, want %q", got, ErrDenied.Error())
	}
}

// ErrMalformed names a block short at rest, never a fault in transit.
func TestMalformedMessageNamesNoTransit(t *testing.T) {
	if msg := ErrMalformed.Error(); strings.Contains(msg, "transit") {
		t.Errorf("ErrMalformed.Error() = %q, want no mention of transit", msg)
	}
}

// Each role prints RFC 4 §4.13's name, the key a store files it under.
func TestRoleNames(t *testing.T) {
	for r, want := range map[Role]string{RoleClaim: "claim", RoleHealth: "health", RoleKeys: "keys", 0: "Role(0)"} {
		if got := r.String(); got != want {
			t.Errorf("Role(%d).String() = %q, want %q", r, got, want)
		}
	}
}
