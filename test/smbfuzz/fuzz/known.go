package fuzz

import (
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Known is the list of accepted divergences (DESIGN.md §12.2): one entry per
// signature, each with its reason. An entry excuses that signature only.
type Known struct {
	Entries []KnownEntry `yaml:"divergences"`
}

// KnownEntry is one accepted divergence.
type KnownEntry struct {
	Signature string `yaml:"signature"`
	Reason    string `yaml:"reason"`
	Issue     string `yaml:"issue,omitempty"`
}

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
	if err := yaml.Unmarshal(b, &k); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, e := range k.Entries {
		if e.Signature == "" || e.Reason == "" {
			return nil, fmt.Errorf("%s: every entry needs a signature and a reason", path)
		}
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
