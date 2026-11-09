//go:build linux

package fuzz

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// Known is the list of accepted divergences (DESIGN.md §12.2): one entry per
// signature, each with its reason and either the issue that tracks it or, for
// a difference DittoFS makes by design, the rule it follows. An entry excuses
// that signature only.
type Known struct {
	Entries []KnownEntry `yaml:"divergences"`
}

// KnownEntry is one accepted divergence.
type KnownEntry struct {
	Signature string `yaml:"signature"`
	Reason    string `yaml:"reason"`
	Issue     string `yaml:"issue,omitempty"` // "#123", "123" or a URL
	Spec      string `yaml:"spec,omitempty"`  // the rule a by-design difference follows, "RFC 22 §12.2"
}

// issueRef is what an issue field holds: a number or a URL.
var issueRef = regexp.MustCompile(`^(#?[0-9]+|https?://\S+)$`)

// LoadKnown reads the known-divergence file; a missing file is an empty list.
func LoadKnown(path string) (*Known, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Known{}, nil
	}
	if err != nil {
		return nil, err
	}
	var k Known
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&k); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	seen := map[string]bool{}
	for i, e := range k.Entries {
		switch {
		case e.Signature == "" || e.Reason == "":
			return nil, fmt.Errorf("%s: entry %d: every entry needs a signature and a reason", path, i+1)
		case e.Issue == "" && e.Spec == "":
			return nil, fmt.Errorf("%s: %s: needs an issue, or a spec citing the rule DittoFS follows by design", path, e.Signature)
		case e.Issue != "" && !issueRef.MatchString(e.Issue):
			return nil, fmt.Errorf("%s: %s: issue %q is neither a number nor a URL", path, e.Signature, e.Issue)
		case seen[e.Signature]:
			return nil, fmt.Errorf("%s: %s: listed twice", path, e.Signature)
		}
		seen[e.Signature] = true
	}
	return &k, nil
}

// Has reports whether a signature is known.
func (k *Known) Has(sig string) bool { return k.Reason(sig) != "" }

// Reason returns the reason a signature is accepted, or "".
func (k *Known) Reason(sig string) string {
	for _, e := range k.Entries {
		if e.Signature == sig {
			return e.Reason
		}
	}
	return ""
}
