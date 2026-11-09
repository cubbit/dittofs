//go:build linux

package fuzz

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// known.yaml's rule is enforced: each entry has a reason and an issue or a
// spec, and names a signature no other entry names.
func TestLoadKnownEnforcesTheRule(t *testing.T) {
	dir := t.TempDir()
	load := func(text string) error {
		p := filepath.Join(dir, "known.yaml")
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadKnown(p)
		return err
	}
	for _, text := range []string{
		"divergences: []\n",
		"divergences:\n- {signature: getacl:owner:alice/bob, reason: r, issue: \"#123\"}\n",
		"divergences:\n- {signature: getacl:owner:alice/bob, reason: r, issue: https://github.com/cubbit/dittofs/issues/1}\n",
		"divergences:\n- {signature: stat:size:dir, reason: r, spec: RFC 22 §12.2}\n",
	} {
		if err := load(text); err != nil {
			t.Errorf("%q: %v", text, err)
		}
	}
	for _, x := range []struct{ text, want string }{
		{"divergences:\n- {signature: stat:size:dir, reason: noise}\n", "needs an issue"},
		{"divergences:\n- {signature: stat:size:dir, reason: r, issue: soon}\n", "neither a number nor a URL"},
		{"divergences:\n- {signature: stat:size:dir, reason: r, issue: \"1\"}\n- {signature: stat:size:dir, reason: s, issue: \"2\"}\n", "listed twice"},
		{"divergences:\n- {signature: stat:size:dir, reason: r, isue: \"1\"}\n", "not found"},
		{"divergences:\n- {signature: stat:size:dir, issue: \"1\"}\n", "a signature and a reason"},
	} {
		if err := load(x.text); err == nil || !strings.Contains(err.Error(), x.want) {
			t.Errorf("%q: %v, want an error saying %q", x.text, err, x.want)
		}
	}
	if err := load(""); err != nil {
		t.Errorf("an empty file: %v", err)
	}
	if _, err := LoadKnown("../known.yaml"); err != nil {
		t.Errorf("the repository's list: %v", err)
	}
}
