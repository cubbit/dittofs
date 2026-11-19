package directory_test

import (
	"testing"

	"github.com/cubbit/dittofs/internal/identity/directory"
)

func TestParseRange(t *testing.T) {
	for _, tc := range []struct {
		in     string
		want   directory.Range
		parsed bool
	}{
		{"member;range=0-1499", directory.Range{Attr: "member", Lo: 0, Hi: 1499}, true},
		{"member;range=1500-*", directory.Range{Attr: "member", Lo: 1500, Hi: -1}, true},
		{"MEMBER;Range=0-0", directory.Range{Attr: "MEMBER", Lo: 0, Hi: 0}, true},
		{"member;binary;range=3-4", directory.Range{Attr: "member;binary", Lo: 3, Hi: 4}, true},
		{"member;range=1500-1499", directory.Range{}, false},
		{"member;range=-1-5", directory.Range{}, false},
		{"member;range=+1-5", directory.Range{}, false},
		{"member;range=0-", directory.Range{}, false},
		{"member;range=-*", directory.Range{}, false},
		{"member;range=0*", directory.Range{}, false},
		{"member;range=0-2147483648", directory.Range{}, false},
		{"member;range=1-*-*", directory.Range{}, false},
		{";range=0-1", directory.Range{}, false},
		{"member", directory.Range{}, false},
	} {
		got, ok := directory.ParseRange(tc.in)
		if ok != tc.parsed || got != tc.want {
			t.Errorf("ParseRange(%q) = %+v, %v; want %+v, %v", tc.in, got, ok, tc.want, tc.parsed)
		}
	}
}

// A parsed range has lo <= hi or hi *, and re-formats to text that parses to
// the same range.
func FuzzRangeAttr(f *testing.F) {
	for _, s := range []string{"member;range=0-1499", "member;range=1500-*", "member;range=0-0"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, desc string) {
		r, ok := directory.ParseRange(desc)
		if !ok {
			return
		}
		if r.Lo < 0 || r.Hi < -1 || r.Hi >= 0 && r.Lo > r.Hi {
			t.Fatalf("ParseRange(%q) = %+v: bounds out of order", desc, r)
		}
		again, ok := directory.ParseRange(r.String())
		if !ok || again != r {
			t.Fatalf("ParseRange(%q) = %+v, re-formatted %q parses to %+v, %v", desc, r, r.String(), again, ok)
		}
	})
}
