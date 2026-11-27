package bootstrap

import (
	"os"
	"strings"
	"testing"
)

// The configuration guide documents every key of the table, each in a row
// of its own whose first cell is the whole key in backquotes, a role's keys
// written keys.<role>. as RFC 13 Appendix C writes them. A key named only in
// prose, or as a suffix of another key's row, does not count.
func TestGuideDocumentsEveryKey(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("../../docs/guide/configuration.md")
	if err != nil {
		t.Fatal(err)
	}

	rows := map[string]bool{}

	for line := range strings.SplitSeq(string(data), "\n") {
		cell, _, ok := strings.Cut(strings.TrimPrefix(line, "| "), " |")
		if ok && strings.HasPrefix(line, "| `") {
			rows[cell] = true
		}
	}

	seen := map[string]bool{}

	for _, s := range keySpecs {
		key := strings.Replace(s.key, "keys.protocol.", "keys.<role>.", 1)
		key = strings.Replace(key, "keys.storage.", "keys.<role>.", 1)

		if !rows["`"+key+"`"] && !seen[key] {
			t.Errorf("docs/guide/configuration.md: no table row for `%s`", key)
		}

		seen[key] = true
	}
}
