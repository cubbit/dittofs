package node

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// rfc13 is the copy of RFC 13 whose Appendix B the check reads.
//
// decision: the check reads RFC 13 as the branch it runs on holds it, never
// another branch's copy, so an Appendix B change reaches the check only when
// the RFC text is brought to this branch, and a change adding rows to the
// node's table brings that text first; the check failing on a stale copy is
// the signal. Withdraw it if the RFCs ever live in one copy.
const rfc13 = "../../docs/internals/rfcs/rfc-13-configuration.md"

// appendixRow is one row of RFC 13 Appendix B, as the check reads it.
type appendixRow struct {
	line  int      // the row's line in the file, for a failure to name
	keys  []string // the backticked keys the row's name gives, none for a row named in prose
	scope string   // the scope cell as written
	class string   // the class cell's first words, one of classWords, or "" for none of them
	admin bool     // the class cell says the row is written by admin only
}

// classWords are the class cell's leading words Appendix B uses: RFC 13 §5's
// four classes and §4.1's and §2.2's three kinds that have no scope.
var classWords = []string{"live", "restart", "next write", "bound", "fixed", "derived", "bootstrap"}

// keyForm is a key as RFC 13 Appendix B writes one: lower case, its parts
// joined by dots and its words by underscores, a part in angle brackets
// standing for one per thing, as adapters.<protocol>.port.
var keyForm = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.([a-z][a-z0-9_]*|<[a-z]+>))*$`)

// backticked finds each span a row writes in backticks.
var backticked = regexp.MustCompile("`([^`]*)`")

// parseAppendixB reads every row of the tables under RFC 13's Appendix B
// heading in text, up to the next heading of its level. A row is five cells:
// setting, defined in, scope, class, default. Its keys are the backticked
// spans of its setting cell, before the first ": ", that read as keys.
func parseAppendixB(text string) ([]appendixRow, error) {
	var rows []appendixRow
	in := false
	for i, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "## ") {
			if in {
				break
			}
			in = strings.HasPrefix(line, "## Appendix B")
			continue
		}
		if !in || !strings.HasPrefix(line, "|") ||
			strings.HasPrefix(line, "| Setting |") || strings.HasPrefix(line, "| --- |") {
			continue
		}
		if !strings.HasPrefix(line, "| ") || !strings.HasSuffix(line, " |") {
			return nil, fmt.Errorf("line %d: a row not padded as \"| cell | … |\"", i+1)
		}
		cells := strings.Split(strings.TrimSuffix(strings.TrimPrefix(line, "| "), " |"), " | ")
		if len(cells) != 5 {
			return nil, fmt.Errorf("line %d: %d cells, want 5", i+1, len(cells))
		}
		row := appendixRow{
			line:  i + 1,
			scope: cells[2],
			admin: strings.Contains(cells[3], "written by `admin` only"),
		}
		name, _, _ := strings.Cut(cells[0], ": ")
		for _, m := range backticked.FindAllStringSubmatch(name, -1) {
			if keyForm.MatchString(m[1]) {
				row.keys = append(row.keys, m[1])
			}
		}
		for _, w := range classWords {
			if strings.HasPrefix(cells[3], w) {
				row.class = w
				break
			}
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil, errors.New("no Appendix B row found")
	}
	return rows, nil
}

// wider is each of RFC 13 §3's five scopes and the scope directly wider than
// it, "" for installation's.
var wider = map[string]string{
	"installation": "",
	"node":         "installation",
	"remote store": "installation",
	"namespace":    "remote store",
	"share":        "namespace",
}

// within reports whether scope narrow lies inside scope wide, a shard counting
// as a part of a share, so inside the share scope itself as well.
func within(narrow, wide string) bool {
	if narrow == "shard" {
		if wide == "share" {
			return true
		}
		narrow = "share"
	}
	for s := wider[narrow]; s != ""; s = wider[s] {
		if s == wide {
			return true
		}
	}
	return false
}

// The three scope forms of RFC 13 §3 that are not a bare scope.
var (
	perThingForm = regexp.MustCompile(`^([a-z ]+), per ([a-z ]+)$`)
	// A qualifier after override, as "(in its shard record)", is the row's own prose.
	overrideForm = regexp.MustCompile(`^([a-z ]+?) default, ([a-z ]+?) override(?: .+)?$`)
)

// scopeFormErr is nil when a row's scope cell is one of RFC 13 §3's five
// scopes in one of its three forms, or no scope with a class of fixed,
// derived or bootstrap, and else says why not.
func scopeFormErr(scope, class string) error {
	unscoped := slices.Contains([]string{"fixed", "derived", "bootstrap"}, class)
	if scope == "—" {
		if !unscoped {
			return fmt.Errorf("no scope with class %q, want fixed, derived or bootstrap", class)
		}
		return nil
	}
	if unscoped {
		return fmt.Errorf("class %s with scope %q, want \"—\"", class, scope)
	}
	isScope := func(s string) bool { _, ok := wider[s]; return ok }
	if m := overrideForm.FindStringSubmatch(scope); m != nil {
		if !isScope(m[1]) || !within(m[2], m[1]) {
			return fmt.Errorf("override %q: %q is not a scope inside the scope %q", scope, m[2], m[1])
		}
		return nil
	}
	if m := perThingForm.FindStringSubmatch(scope); m != nil {
		if !isScope(m[1]) || isScope(m[2]) {
			return fmt.Errorf("per-thing %q: want a scope per a thing that is no scope", scope)
		}
		return nil
	}
	if !isScope(scope) {
		return fmt.Errorf("%q is none of the five scopes in any of RFC 13 §3's forms", scope)
	}
	return nil
}

// readRFC13 is the RFC copy the check reads.
func readRFC13(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(rfc13)
	if err != nil {
		t.Fatalf("read %s: %v", rfc13, err)
	}
	return string(b)
}

// scopeFormFailures is every row of text's Appendix B whose scope cell
// scopeFormErr refuses, each naming its line.
func scopeFormFailures(t *testing.T, text string) []string {
	t.Helper()
	rows, err := parseAppendixB(text)
	if err != nil {
		t.Fatalf("parse Appendix B: %v", err)
	}
	var bad []string
	for _, r := range rows {
		if err := scopeFormErr(r.scope, r.class); err != nil {
			bad = append(bad, fmt.Sprintf("line %d %v: %v", r.line, r.keys, err))
		}
	}
	return bad
}

// RFC 13 §9's "§3 scope forms": every Appendix B scope cell is one of the five
// scopes in one of §3's three forms, or none with a class of fixed, derived or
// bootstrap; the same check over the appendix with one scope cell edited to
// cluster, a scope that is not one, fails.
func TestAppendixBScopeForms(t *testing.T) {
	text := readRFC13(t)
	if bad := scopeFormFailures(t, text); len(bad) > 0 {
		t.Errorf("Appendix B scope cells outside RFC 13 §3's forms:\n%s", strings.Join(bad, "\n"))
	}

	const row = "| `audit.retention`: "
	at := strings.Index(text, row)
	if at < 0 {
		t.Fatalf("no row starting %q in %s", row, rfc13)
	}
	end := at + strings.IndexByte(text[at:], '\n')
	edited := text[:at] + strings.Replace(text[at:end], " | installation | ", " | cluster | ", 1) + text[end:]
	if edited == text {
		t.Fatalf("the audit.retention row has no installation scope cell to edit")
	}
	if bad := scopeFormFailures(t, edited); len(bad) != 1 || !strings.Contains(bad[0], "audit.retention") {
		t.Errorf("scope cell edited to cluster: failures %q, want one naming audit.retention", bad)
	}
}

// Each of RFC 13 §3's rules on a scope cell refuses the cell that breaks it.
func TestScopeFormErr(t *testing.T) {
	for _, tc := range []struct {
		scope, class string
		ok           bool
	}{
		{"installation", "live", true},
		{"remote store", "next write", true},
		{"node, per journal", "restart", true},
		{"installation default, shard override (in its shard record)", "live", true},
		{"installation default, node override", "live", true},
		{"installation, node override", "live", false},
		{"share default, shard override", "live", true},
		{"—", "fixed", true},
		{"—", "derived", true},
		{"none", "derived", false},
		{"—", "bootstrap", true},
		{"cluster", "live", false},
		{"—", "live", false},
		{"installation", "fixed", false},
		{"installation, per node", "live", false},
		{"share default, installation override", "live", false},
		{"node default, namespace override", "live", false},
		{"cluster default, node override", "live", false},
	} {
		if err := scopeFormErr(tc.scope, tc.class); (err == nil) != tc.ok {
			t.Errorf("scopeFormErr(%q, %q) = %v, want accepted %v", tc.scope, tc.class, err, tc.ok)
		}
	}
}

// The parse refuses an Appendix B row it would otherwise skip or misread: an
// unpadded one and one with a wrong cell count.
func TestParseAppendixBRefusesMalformedRow(t *testing.T) {
	const head = "## Appendix B\n\n| Setting | Defined in | Scope | Class | Default |\n| --- | --- | --- | --- | --- |\n"
	for name, row := range map[string]string{
		"unpadded":   "|`x`|RFC 13|installation|live|1|",
		"no end pad": "| `x` | RFC 13 | installation | live | 1|",
		"four cells": "| `x` | RFC 13 | installation | live |",
	} {
		if _, err := parseAppendixB(head + row + "\n"); err == nil || !strings.Contains(err.Error(), "line 5") {
			t.Errorf("%s row %q: error %v, want one naming line 5", name, row, err)
		}
	}
}

// The parse finds RFC 23's rows by key, each at installation scope, live and
// written by admin only.
func TestAppendixBRFC23Rows(t *testing.T) {
	rows, err := parseAppendixB(readRFC13(t))
	if err != nil {
		t.Fatalf("parse Appendix B: %v", err)
	}
	for _, key := range []string{"api.session_lifetime", "api.token_max_lifetime", "audit.retention"} {
		i := slices.IndexFunc(rows, func(r appendixRow) bool { return slices.Contains(r.keys, key) })
		if i < 0 {
			t.Errorf("%s: no Appendix B row found", key)
			continue
		}
		if r := rows[i]; r.scope != "installation" || r.class != "live" || !r.admin {
			t.Errorf("%s (line %d): scope %q, class %q, admin only %v; want installation, live, true",
				key, r.line, r.scope, r.class, r.admin)
		}
	}
}
