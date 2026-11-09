//go:build linux

package fuzz

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/test/smbfuzz/sd"
)

func testResolver() *sd.Resolver {
	r := &sd.Resolver{Domain: sd.MustSID("S-1-5-21-1-2-3"), Accounts: map[string]sd.SID{}}
	for i, n := range []string{"alice", "bob", "carol", "Administrator", "devs", "engineering", "fuzzers"} {
		r.Accounts[n] = sd.MustSID("S-1-5-21-1-2-3-" + string(rune('0'+i)) + "100")
	}
	return r
}

// Every generated plan formats, parses back to the same text, and every
// value in it resolves: sizes evaluate, descriptors parse, flags are known.
func TestGeneratedPlansRoundTrip(t *testing.T) {
	res := testResolver()
	for round := uint64(1); round <= 2000; round++ {
		p, _ := Generate(1, round)
		if len(p.Steps) != StepsPerPlan {
			t.Fatalf("round %d: %d steps", round, len(p.Steps))
		}
		users := map[string]bool{}
		for _, s := range p.Steps {
			users[s.User] = true
		}
		if len(users) < 2 {
			t.Fatalf("round %d names one user", round)
		}
		text := p.Format()
		q, err := ParsePlan(strings.NewReader(text))
		if err != nil {
			t.Fatalf("round %d: parse: %v\n%s", round, err, text)
		}
		if q.Format() != text {
			t.Fatalf("round %d: round trip differs:\n%s\n---\n%s", round, text, q.Format())
		}
		for _, s := range p.Steps {
			for _, k := range []string{"off", "off2", "len"} {
				if v, ok := s.Args[k]; ok {
					if _, err := EvalSize(v); err != nil {
						t.Fatalf("round %d: %v", round, err)
					}
				}
			}
			if v, ok := s.Args["sd"]; ok {
				if _, err := res.Parse(v); err != nil {
					t.Fatalf("round %d: sd %q: %v", round, v, err)
				}
			}
			if s.Op == "open" {
				if _, err := parseFlags(s.Args["flags"], openFlagNames); err != nil {
					t.Fatalf("round %d: %v", round, err)
				}
			}
			if s.Op == "setsparse" && s.User != "admin" {
				t.Fatalf("round %d: setsparse as %s", round, s.User)
			}
		}
	}
}

// Test N is always the same plan: the first 2000 rounds of generator 1 are
// pinned, enough that a small change to one draw's weight moves the hash.
// A change that moves this hash changes what every test number means and
// belongs in a new generator version.
func TestGeneratorV1IsFrozen(t *testing.T) {
	h := sha256.New()
	for round := uint64(1); round <= 2000; round++ {
		p, _ := Generate(1, round)
		h.Write([]byte(p.Format()))
	}
	got := hex.EncodeToString(h.Sum(nil))
	const want = "17b275b0a1becba7d425168d423fab989cde0ddb4ac3dfd3c55c251ea9ed0723"
	if got != want {
		t.Fatalf("generator 1 changed: hash %s, pinned %s", got, want)
	}
}

func TestEvalSize(t *testing.T) {
	for expr, want := range map[string]int64{
		"0": 0, "4096+1": 4097, "2^20+2^13": 1<<20 + 8192, "chunk.max+1": 16<<20 + 1,
		"chunk.max-1": 16<<20 - 1, "block-1+4097": 8<<20 - 1 + 4097,
		"4096-1": 4095, "2^20+2^13-1": 1<<20 + 8191,
	} {
		got, err := EvalSize(expr)
		if err != nil || got != want {
			t.Errorf("%s = %d, %v; want %d", expr, got, err, want)
		}
	}
}

func TestParseQuotedPath(t *testing.T) {
	p, err := ParsePlan(strings.NewReader("01 alice open \"a b\" O_RDONLY 0644 -> alice.h0\n02 bob pwrite bob.h1 off=4096+1 len=1 data=seq\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Steps[0].Args["path"] != "a b" || p.Steps[1].Args["off"] != "4096+1" || p.Steps[0].Out != "alice.h0" {
		t.Fatalf("%+v", p.Steps)
	}
}
