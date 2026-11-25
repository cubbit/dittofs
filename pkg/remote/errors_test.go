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
// they wrap: a syncer that re-gets on ErrCorrupt and holds the flow on
// ErrThrottled reads them this way, so ErrMalformed declared on its own would
// be re-got, and a sentinel wrapping the wrong one would be retried wrongly.
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
