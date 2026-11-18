package bootstrap

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// settingKey is a dotted key in backquotes, as Appendix B's first column
// writes one.
var settingKey = regexp.MustCompile("`([a-z_]+(?:\\.[a-z_<>]+)+)`")

// The setting keys are RFC 13 Appendix B's: every dotted key named in the
// first column of a row whose class is a setting's (live, restart, next
// write, bound), and no other.
func TestSettingKeysMatchAppendixB(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("../../docs/internals/rfcs/rfc-13-configuration.md")
	if err != nil {
		t.Fatal(err)
	}

	text := string(data)

	start := strings.Index(text, "\n## Appendix B")
	end := strings.Index(text, "\n## Appendix C")

	if start < 0 || end < start {
		t.Fatalf("RFC 13: Appendix B not found before Appendix C")
	}

	var want []string

	for line := range strings.SplitSeq(text[start:end], "\n") {
		cells := strings.Split(line, "|")
		if !strings.HasPrefix(line, "|") || len(cells) < 6 {
			continue
		}

		class := strings.TrimSpace(cells[4])
		if !strings.HasPrefix(class, "live") && !strings.HasPrefix(class, "restart") &&
			!strings.HasPrefix(class, "next write") && !strings.HasPrefix(class, "bound") {
			continue
		}

		for _, m := range settingKey.FindAllStringSubmatch(cells[1], -1) {
			want = append(want, m[1])
		}
	}

	slices.Sort(want)
	want = slices.Compact(want)

	got := slices.Sorted(slices.Values(settingKeys))

	if !slices.Equal(got, want) {
		t.Errorf("setting keys:\ngot  %q\nwant %q (RFC 13 Appendix B)", got, want)
	}
}
