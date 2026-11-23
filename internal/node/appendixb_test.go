package node

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/node/api/apisettings"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/settings"
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

// clusterAuditRetention is text with the audit.retention row's installation
// scope cell edited to cluster, a scope that is not one.
func clusterAuditRetention(t *testing.T, text string) string {
	t.Helper()
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
	return edited
}

// RFC 13 §9's "§3 scope forms": every Appendix B scope cell is one of the five
// scopes in one of §3's three forms, or none with a class of fixed, derived or
// bootstrap; the same check over the appendix with one scope cell edited to
// cluster, a scope that is not one, fails.
func TestAppendixBScopeForms(t *testing.T) {
	t.Parallel()
	text := readRFC13(t)
	if bad := scopeFormFailures(t, text); len(bad) > 0 {
		t.Errorf("Appendix B scope cells outside RFC 13 §3's forms:\n%s", strings.Join(bad, "\n"))
	}

	if bad := scopeFormFailures(t, clusterAuditRetention(t, text)); len(bad) != 1 || !strings.Contains(bad[0], "audit.retention") {
		t.Errorf("scope cell edited to cluster: failures %q, want one naming audit.retention", bad)
	}
}

// Each of RFC 13 §3's rules on a scope cell refuses the cell that breaks it.
func TestScopeFormErr(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// tableStore is a fresh in-memory control plane the check reads a table
// through: Read over it, with no row, gives every section's defaults, and
// Check inside its Write judges a probe write.
func tableStore(t *testing.T) *kvstore.Store {
	t.Helper()
	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatalf("open the KV: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the KV: %v", err)
		}
	})
	if err := kvstore.Init(t.Context(), db, uuid.New()); err != nil {
		t.Fatalf("init the store: %v", err)
	}
	st, err := kvstore.Open(t.Context(), db)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	return st
}

// probeNode is the node the check reads a table for.
var probeNode = entity.NodeID{0xa9, 15: 1}

// sectionSeeds is the smallest set of rows under which Read accepts every
// section of the node's table whose defaults alone it refuses: one with a
// required key, which has no default.
var sectionSeeds = []entity.Setting{
	{Scope: entity.SettingScope{Kind: entity.ScopeInstallation},
		Key: "identity.directory.address", Value: []byte(`"ldaps://dc.example.test"`)},
}

// putBeneathCheck writes row straight to the store at the current schema,
// as no write through Check could, to stand up a section or plant a value.
func putBeneathCheck(t *testing.T, st *kvstore.Store, row entity.Setting) {
	t.Helper()
	row.Schema = settings.Versions()[entity.FormatSettings].Max
	err := st.ControlPlane().Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		cur, _, err := tx.Setting(row.Scope, row.Key)
		if err != nil {
			return err
		}
		_, err = tx.PutSetting(row, cur.Generation)
		return err
	})
	if err != nil {
		t.Fatalf("write %s beneath the check: %v", row.Key, err)
	}
}

// tableKey is one key the table declares, as the check reads it.
type tableKey struct {
	settings.Change
	scopes    []entity.ScopeKind // the scope kinds Check accepts a write of it at
	adminOnly bool               // Check refuses an operator's write of it
}

// tableKeys is every key tb declares, read through pkg/settings' exported
// surface only.
//
// decision: the table exports no list of its keys, so they are read as
// Diff(nil, Read over a store holding only sectionSeeds) lists them, each
// with its section, class and base scope, and each key's scope form and
// write role are probed with Check: a delete at each of the five scope
// kinds, refused with ErrWrongScope or not, and an operator's delete at its
// base scope, refused with ErrForbidden or not. Diff lists no key of a
// section whose base names no instance (one per share, namespace or remote
// store, or per thing) or whose value Read refuses, so the check fails when
// Read refuses a section, naming it for a seed, and when it reaches fewer
// sections than the table holds, rather than skip one; an At and a PerThing
// at one scope accept the same kind, so the probe cannot tell them apart.
// Add a Table.Keys() export, or seed a row per instance, when a section
// the probe cannot reach is declared.
func tableKeys(t *testing.T, st *kvstore.Store, tb settings.Table) []tableKey {
	t.Helper()
	for _, row := range sectionSeeds {
		putBeneathCheck(t, st, row)
	}
	snap, refused, err := settings.Read(t.Context(), st.Nodes(), tb, probeNode)
	if err != nil {
		t.Fatalf("read the table's defaults: %v", err)
	}
	for sec, err := range refused {
		t.Fatalf("section %s: Read refuses it, so the check cannot list its keys; "+
			"give it a row in sectionSeeds: %v", sec, err)
	}
	changes := settings.Diff(nil, snap)
	var sections []string
	for _, ch := range changes {
		if !slices.Contains(sections, ch.Section) {
			sections = append(sections, ch.Section)
		}
	}
	if len(sections) != len(tb) {
		t.Fatalf("Diff lists keys of %d sections %v of the table's %d; "+
			"a section with no base instance is beyond the check", len(sections), sections, len(tb))
	}

	check := func(caller entity.APIRole, w settings.Write) error {
		return st.ControlPlane().Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
			return tb.Check(tx, caller, w, nil)
		})
	}
	out := make([]tableKey, 0, len(changes))
	for _, ch := range changes {
		k := tableKey{Change: ch}
		for kind := entity.ScopeInstallation; kind <= entity.ScopeShare; kind++ {
			scope := entity.SettingScope{Kind: kind}
			if kind != entity.ScopeInstallation {
				scope.ID = probeNode
			}
			if !errors.Is(check(entity.RoleAdmin, settings.Delete(scope, ch.Key)), settings.ErrWrongScope) {
				k.scopes = append(k.scopes, kind)
			}
		}
		k.adminOnly = errors.Is(check(entity.RoleOperator, settings.Delete(ch.Scope, ch.Key)), settings.ErrForbidden)
		out = append(out, k)
	}
	return out
}

// scopeKinds is each scope Appendix B names, by the name it writes.
var scopeKinds = map[string]entity.ScopeKind{
	"installation": entity.ScopeInstallation,
	"node":         entity.ScopeNode,
	"namespace":    entity.ScopeNamespace,
	"remote store": entity.ScopeRemote,
	"share":        entity.ScopeShare,
}

// cellScopes is the scope kinds a write of a key under a scope cell is
// accepted at: the scope itself, also per thing, and both scopes of an
// override. A cell naming no scope kind, "—" or a shard, gives none.
func cellScopes(cell string) []entity.ScopeKind {
	names := []string{cell}
	if m := overrideForm.FindStringSubmatch(cell); m != nil {
		names = m[1:3]
	} else if m := perThingForm.FindStringSubmatch(cell); m != nil {
		names = m[1:2]
	}
	var out []entity.ScopeKind
	for _, n := range names {
		if k, ok := scopeKinds[n]; ok {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// classNames is each class as Appendix B's class cell starts.
var classNames = map[settings.Class]string{
	settings.Live:      "live",
	settings.Restart:   "restart",
	settings.NextWrite: "next write",
	settings.Bound:     "bound",
}

// compareAppendixB compares the table's keys with Appendix B's rows, each
// key's scope, class and write role, and returns every disagreement, each
// naming its key, as failures; an Appendix B key under a section's prefix
// that the section does not declare comes back in reported, and is not a
// failure. A key with no row of its own is held by a row naming its section.
//
// decision: a key held by a row naming its whole section has its scope, class
// and write role checked, never its name: the row lists the section's fields
// in prose ("the attributes holding a user's and a group's name"), which no
// key matches word for word, so a field the row does not name, as
// identity.directory.bind_name today, passes. Check field names against the
// row once Appendix B lists a whole section's fields as keys.
func compareAppendixB(t *testing.T, rows []appendixRow, tb settings.Table) (failures, reported []string) {
	t.Helper()
	byKey := make(map[string]appendixRow)
	for _, r := range rows {
		for _, k := range r.keys {
			if _, dup := byKey[k]; !dup {
				byKey[k] = r
			}
		}
	}

	keys := tableKeys(t, tableStore(t), tb)
	declared := make(map[string]bool, len(keys))
	var sections []string
	for _, k := range keys {
		declared[k.Key] = true
		if !slices.Contains(sections, k.Section) {
			sections = append(sections, k.Section)
		}

		// A row naming a whole section, as identity.directory names the
		// directory record, holds each of the section's keys.
		r, ok := byKey[k.Key]
		if !ok {
			r, ok = byKey[k.Section]
			declared[k.Section] = ok
		}
		if !ok {
			failures = append(failures, fmt.Sprintf("%s: declared by the table, no Appendix B row", k.Key))
			continue
		}
		if want := cellScopes(r.scope); !slices.Equal(k.scopes, want) {
			failures = append(failures, fmt.Sprintf("%s (line %d): written at %v, Appendix B's scope %q is %v",
				k.Key, r.line, k.scopes, r.scope, want))
		}
		if got := classNames[k.Class]; got != r.class {
			failures = append(failures, fmt.Sprintf("%s (line %d): class %q, Appendix B's %q",
				k.Key, r.line, got, r.class))
		}

		if k.adminOnly != r.admin {
			failures = append(failures, fmt.Sprintf("%s (line %d): written by admin only %v, Appendix B's %v",
				k.Key, r.line, k.adminOnly, r.admin))
		}
	}

	var undeclared []string
	for _, r := range rows {
		for _, key := range r.keys {
			if declared[key] {
				continue
			}
			if slices.ContainsFunc(sections, func(p string) bool { return strings.HasPrefix(key, p+".") }) {
				reported = append(reported, key)
			} else {
				undeclared = append(undeclared, key)
			}
		}
	}
	// Both are reported, never failed: a section is declared with its
	// component, and a key of a declared section can land after the section.
	t.Logf("Appendix B keys under a declared section that it lacks: %v", reported)
	t.Logf("Appendix B keys no section declares: %d %v", len(undeclared), undeclared)
	return failures, reported
}

// The node's table agrees with Appendix B on every key's scope, class and
// write role, and the api section's two later keys are reported, not failed.
func TestAppendixBMatchesTheTable(t *testing.T) {
	t.Parallel()
	rows, err := parseAppendixB(readRFC13(t))
	if err != nil {
		t.Fatalf("parse Appendix B: %v", err)
	}
	failures, reported := compareAppendixB(t, rows, newTable())
	if len(failures) > 0 {
		t.Errorf("the table and Appendix B disagree:\n%s", strings.Join(failures, "\n"))
	}
	for _, key := range []string{"api.oidc_role_map", "api.password_sign_in"} {
		if !slices.Contains(reported, key) {
			t.Errorf("%s: not among the reported keys %v", key, reported)
		}
	}
}

// renamedAPI is the api section with session_lifetime renamed.
type renamedAPI struct {
	SessionLifetime  time.Duration `json:"session_lifetimes" class:"live"`
	TokenMaxLifetime time.Duration `json:"token_max_lifetime" class:"live"`
}

func (renamedAPI) Validate() error { return nil }

// Each drift between the table and Appendix B fails the comparison, naming
// the key that drifted.
func TestAppendixBDriftFails(t *testing.T) {
	t.Parallel()
	text := readRFC13(t)
	cluster := clusterAuditRetention(t, text)

	api := settings.Of("api", settings.At(entity.ScopeInstallation), apisettings.Defaults(),
		settings.WriteRole(entity.RoleAdmin))
	audit := settings.Of("audit", settings.At(entity.ScopeInstallation), apisettings.AuditDefaults(),
		settings.WriteRole(entity.RoleAdmin))
	for _, tc := range []struct {
		name, text string
		table      settings.Table
		key        string
	}{
		{"a key of the table renamed", text, settings.Table{
			settings.Of("api", settings.At(entity.ScopeInstallation), renamedAPI{
				SessionLifetime:  apisettings.Defaults().SessionLifetime,
				TokenMaxLifetime: apisettings.Defaults().TokenMaxLifetime,
			},
				settings.WriteRole(entity.RoleAdmin)),
			audit,
		}, "api.session_lifetimes"},
		{"audit at the default role", text, settings.Table{
			api,
			settings.Of("audit", settings.At(entity.ScopeInstallation), apisettings.AuditDefaults()),
		}, "audit.retention"},
		{"a scope cell edited to cluster", cluster, settings.Table{api, audit}, "audit.retention"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows, err := parseAppendixB(tc.text)
			if err != nil {
				t.Fatalf("parse Appendix B: %v", err)
			}
			failures, _ := compareAppendixB(t, rows, tc.table)
			if len(failures) != 1 || !regexp.MustCompile(`^`+regexp.QuoteMeta(tc.key)+`[: ]`).MatchString(failures[0]) {
				t.Errorf("failures %q, want one naming %s", failures, tc.key)
			}
		})
	}
}

// refMarker is the reference the check writes into every reference field: as
// a 16-byte reference, or as a string for a reference field of string type.
var refMarker = [16]byte([]byte("SECRET-MARKER-01"))

// refMarkerForms is every way a Validate could print refMarker: as text, in
// hex either case, as a UUID, and its first bytes as Go or JSON prints a
// byte array.
var refMarkerForms = []string{
	string(refMarker[:]),
	hex.EncodeToString(refMarker[:]),
	strings.ToUpper(hex.EncodeToString(refMarker[:])),
	uuid.UUID(refMarker).String(),
	"83 69 67 82", // fmt's %v or %d of the bytes "SECR"
	"83,69,67,82", // json's
}

// refLeaks drives each section of tb's Validate with refMarker in every
// reference field, and with each of refusing written beneath Check, and
// returns the reference keys it found by section, and as failures each
// refusal that prints refMarker and each section holding a reference that
// Read does not refuse, since a section Validate accepts prints nothing for
// the search to search.
//
// decision: the table exports no list of its reference fields, so a key is
// one when Check refuses an admin's put of refMarker, as a string or as a
// 16-byte array, with ErrSecretRef, the refusal it gives a reference field
// only once the value decodes into it; the row is then written beneath Check
// and Read runs Validate on it. A reference field of another type would not
// be found: add a Table export of the reference fields if a section declares
// one. Only the refusal refusing gives each section is searched, so a
// Validate printing a reference in another refusal passes.
func refLeaks(t *testing.T, tb settings.Table, refusing []entity.Setting) (refs map[string][]string, failures []string) {
	t.Helper()
	st := tableStore(t)
	cp := st.ControlPlane()
	schema := settings.Versions()[entity.FormatSettings].Max
	arr, err := json.Marshal(refMarker)
	if err != nil {
		t.Fatal(err)
	}
	str, err := json.Marshal(string(refMarker[:]))
	if err != nil {
		t.Fatal(err)
	}

	// A marker row that a probe stored is the reference the next probe's
	// write is judged whole with, so each key is probed over the same clean
	// store the first was: the markers a section's reference fields leave
	// are removed before every probe and put back once the sweep ends, for
	// the refusal search below, which reads each section with them stored.
	marks := make(map[string]entity.Setting)
	removeMarks := func() {
		for key, row := range marks {
			err := cp.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
				cur, _, err := tx.Setting(row.Scope, key)
				if err != nil {
					return err
				}

				return tx.DeleteSetting(row.Scope, key, cur.Generation)
			})
			if err != nil {
				t.Fatalf("remove the probe's %s row: %v", key, err)
			}
		}
	}

	refs = make(map[string][]string)

	for _, k := range tableKeys(t, st, tb) {
		removeMarks()

		for _, v := range [][]byte{arr, str} {
			row := entity.Setting{Scope: k.Scope, Key: k.Key, Value: v, Schema: schema}
			err := cp.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
				return tb.Check(tx, entity.RoleAdmin, settings.Put(row), nil)
			})
			if !errors.Is(err, settings.ErrSecretRef) {
				continue
			}
			putBeneathCheck(t, st, row)
			marks[k.Key] = row
			refs[k.Section] = append(refs[k.Section], k.Key)
			break
		}
	}

	for _, row := range marks {
		putBeneathCheck(t, st, row)
	}

	for _, row := range refusing {
		putBeneathCheck(t, st, row)
	}

	_, refused, err := settings.Read(t.Context(), st.Nodes(), tb, probeNode)
	if err != nil {
		t.Fatalf("read the table with its reference fields set: %v", err)
	}
	for sec, fields := range refs {
		err := refused[sec]
		if err == nil {
			failures = append(failures, fmt.Sprintf("%s: holds the reference %v, but Read accepts it, "+
				"so no refusal is searched; give it a row in refusing", sec, fields))
			continue
		}
		for _, form := range refMarkerForms {
			if strings.Contains(err.Error(), form) {
				failures = append(failures, fmt.Sprintf("%s: refusal prints the reference in %v: %v", sec, fields, err))
				break
			}
		}
	}
	return refs, failures
}

// refusingSeeds is one row per section of the node's table holding a
// reference that its Validate refuses for a reason other than the reference.
var refusingSeeds = []entity.Setting{
	{Scope: entity.SettingScope{Kind: entity.ScopeInstallation},
		Key: "identity.netbios_name", Value: []byte(`"not a netbios name"`)},
	{Scope: entity.SettingScope{Kind: entity.ScopeInstallation},
		Key: "identity.directory.realm", Value: []byte(`"example.test"`)},
}

// No section of the node's table prints a reference field's value in a
// refusal, each section holding one refused, and the reference fields found
// are the two RFC 18 and Appendix B name.
func TestNoRefusalPrintsAReference(t *testing.T) {
	t.Parallel()
	refs, failures := refLeaks(t, newTable(), refusingSeeds)
	if len(failures) > 0 {
		t.Errorf("reference search:\n%s", strings.Join(failures, "\n"))
	}
	want := map[string][]string{
		"identity":           {"identity.keytab"},
		"identity.directory": {"identity.directory.bind_credential"},
	}
	if !maps.EqualFunc(refs, want, slices.Equal) {
		t.Errorf("reference fields found %v, want %v", refs, want)
	}
}

// Sections holding a reference field, each with a Validate that refuses any
// value set in it.
type (
	hexRef struct {
		Credential entity.SecretRef `json:"credential" class:"live"`
	}
	quotedRef struct {
		Token string `json:"token" class:"live"`
	}
	quietRef struct {
		Credential entity.SecretRef `json:"credential" class:"live"`
	}
)

func (s hexRef) Validate() error {
	if s.Credential != (entity.SecretRef{}) {
		return fmt.Errorf("credential %x is not sealed", s.Credential)
	}
	return nil
}

func (s quotedRef) Validate() error {
	if s.Token != "" {
		return fmt.Errorf("token %q is not sealed", s.Token)
	}
	return nil
}

func (s quietRef) Validate() error {
	if s.Credential != (entity.SecretRef{}) {
		return errors.New("credential is not sealed")
	}
	return nil
}

// A Validate that prints its reference field fails the search, as a 16-byte
// reference or as a string; one refusing it without printing it passes.
func TestRefLeaksFindsAPrintedReference(t *testing.T) {
	t.Parallel()
	at := settings.At(entity.ScopeInstallation)
	for _, tc := range []struct {
		name    string
		section settings.Section
		leaks   bool
	}{
		{"16-byte reference in hex", settings.Of("test.hex", at, hexRef{},
			settings.SecretRef("Credential", entity.SecretPassword)), true},
		{"string reference quoted", settings.Of("test.quoted", at, quotedRef{},
			settings.SecretRef("Token", entity.SecretPassword)), true},
		{"refused without the value", settings.Of("test.quiet", at, quietRef{},
			settings.SecretRef("Credential", entity.SecretPassword)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, leaks := refLeaks(t, settings.Table{tc.section}, nil)
			if got := len(leaks) == 1 && strings.Contains(leaks[0], "prints the reference"); got != tc.leaks || len(leaks) > 1 {
				t.Errorf("failures %q, want one leak %v", leaks, tc.leaks)
			}
		})
	}
}
