package bootstrap

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The rules of the v1 table as the refusal text spells them, written apart
// from the table itself, so a change of wording must reach this file too.
const (
	removedKeyRule  = "no longer read; remove it"
	databaseKeyRule = "no longer read; the metadata store is metadata.backend"
	familyKeyRule   = "no longer read; a record or a setting, set it through the management API"
	drainKeyRule    = "no longer read; set adapters.<protocol>.drain_timeout " +
		"through the management API"
)

// rfc13Path is RFC 13 in this repository.
const rfc13Path = "../../docs/internals/rfcs/rfc-13-configuration.md"

// The class of a row of the RFC's table as renamedParse reads it.
const (
	classDropped = "dropped"
	classRenamed = "renamed"
	classFamily  = "family"
	classTyped   = "typed"
	classEnv     = "environment"
)

// nestedFile is the file spelling of one dotted key with a value: one
// mapping per level, the leaf last.
func nestedFile(key, value string) string {
	parts := strings.Split(key, ".")

	var b strings.Builder

	indent := ""

	for _, level := range parts[:len(parts)-1] {
		b.WriteString(indent + level + ":\n")
		indent += "  "
	}

	b.WriteString(indent + parts[len(parts)-1] + ": " + value + "\n")

	return b.String()
}

// Every entry of the v1 table refuses in the file under its own rule, which
// this test spells from the approved wording, never from the table: a row
// left out, a row naming another row's replacement, and a value printed are
// each a failure. A family name set as a scalar names the family's rule too,
// since the file's walk takes it as the family's own key.
func TestRetiredKeysInFile(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		key  string
		rule string
	}{
		{"database.x", databaseKeyRule},
		{"metadata.badger.block_cache_mb", removedKeyRule},
		{"metadata.badger.index_cache_mb", removedKeyRule},
		{"controlplane.tls.client_ca", removedKeyRule},
		{"controlplane.tls.min_version", removedKeyRule},
		{"controlplane.require_initial_password_change", removedKeyRule},
		{"controlplane.drain_stall_timeout", removedKeyRule},
		{"controlplane.read_timeout", removedKeyRule},
		{"controlplane.write_timeout", removedKeyRule},
		{"controlplane.idle_timeout", removedKeyRule},
		{"controlplane.host", "no longer read; write api.listen"},
		{"controlplane.port", "no longer read; write api.listen"},
		{"controlplane.tls.cert_file", "no longer read; write api.tls.cert_file"},
		{"controlplane.tls.key_file", "no longer read; write api.tls.key_file"},
		{"controlplane.pprof", removedKeyRule},
		{"controlplane.pprof_mutex_rate", removedKeyRule},
		{"controlplane.pprof_block_rate_ns", removedKeyRule},
		{"controlplane.jwt.x", removedKeyRule},
		{"metrics.auth", removedKeyRule},
		{"metrics.token_file", removedKeyRule},
		{"metrics.path", removedKeyRule},
		{"metrics.enabled", "no longer read; write metrics.listen"},
		{"metrics.host", "no longer read; write metrics.listen"},
		{"metrics.port", "no longer read; write metrics.listen"},
		{"metrics.tls.x", removedKeyRule},
		{"blockstore.journal.path", "no longer read; write journals"},
		{"blockstore.journal.chunk_size", removedKeyRule},
		{"blockstore.journal.chunk_max", removedKeyRule},
		{"blockstore.journal.dirty_expire", removedKeyRule},
		{"blockstore.journal.max_log_bytes", removedKeyRule},
		{"blockstore.journal.backpressure_max_wait", removedKeyRule},
		{"shutdown_timeout", drainKeyRule},
		{"admin.x", familyKeyRule},
		{"kerberos.x", familyKeyRule},
		{"identity.x", familyKeyRule},
		{"ldap.x", familyKeyRule},
		{"gc.x", familyKeyRule},
		{"snapshot.x", familyKeyRule},
		{"integrity.x", familyKeyRule},
	} {
		t.Run(tc.key, func(t *testing.T) {
			t.Parallel()

			path, got := loadRefused(t, nestedFile(tc.key, "s3cr3t"), nil)
			want := wantReport(path, "P: "+tc.key+": "+tc.rule)

			if got != want {
				t.Errorf("report:\ngot:\n%s\nwant:\n%s", got, want)
			}

			if strings.Contains(got, "s3cr3t") {
				t.Errorf("report prints the value:\n%s", got)
			}
		})
	}

	path, got := loadRefused(t, "identity: s3cr3t\n", nil)
	want := wantReport(path, "P: identity: "+familyKeyRule)

	if got != want {
		t.Errorf("report of a family as a scalar:\ngot:\n%s\nwant:\n%s", got, want)
	}

	runRefusals(t, []refusalRow{
		{
			name:    "a name that only shares the letters of a family",
			content: "databases:\n  x: 1\n",
			want:    []string{"P: databases.x: unknown key; did you mean journals?"},
		},
		{
			name:    "a key that only starts like an exact entry",
			content: "metrics:\n  portx: 1\n",
			want:    []string{"P: metrics.portx: unknown key; did you mean metrics.listen?"},
		},
		{
			name:    "a family name with letters after it",
			content: "adminx:\n  y: 1\n",
			want:    []string{"P: adminx.y: unknown key; did you mean journals?"},
		},
	})
}

// The v1 keys refuse among the unknown ones, each under its own rule, and
// none of the values reaches the report.
func TestRetiredKeyAmongUnknown(t *testing.T) {
	t.Parallel()

	content := "database:\n  type: sqlite\n" +
		"controlplane:\n  jwt:\n    secret: s3cr3t\nloging:\n  level: debug\n"

	path, got := loadRefused(t, content, nil)

	want := wantReport(path,
		"P: controlplane.jwt.secret: "+removedKeyRule,
		"P: database.type: "+databaseKeyRule,
		"P: loging.level: unknown key; did you mean logging.level?",
	)

	if got != want {
		t.Errorf("report:\ngot:\n%s\nwant:\n%s", got, want)
	}

	for _, value := range []string{"sqlite", "s3cr3t"} {
		if strings.Contains(got, value) {
			t.Errorf("report prints %q:\n%s", value, got)
		}
	}
}

// The keys Appendix C keeps with a new default still load: a table that
// takes every key of a family as a refusal, consulted before the declared
// keys, would refuse them.
func TestKeptRotationKeysLoad(t *testing.T) {
	t.Parallel()

	b := loads(t, "logging:\n  rotation:\n    max_backups: 5\n    max_age: \"720h\"\n", nil)

	if got := b.Logging.Rotation.MaxBackups; got != 5 {
		t.Errorf("max_backups: got %d, want 5", got)
	}

	if got := b.Logging.Rotation.MaxAge; got != 30 {
		t.Errorf("max_age: got %d days, want 30", got)
	}

	for _, key := range []string{"logging.rotation.max_backups", "logging.rotation.max_age"} {
		if got := b.Source[key]; got != sourceFile {
			t.Errorf("source of %s: got %q, want %q", key, got, sourceFile)
		}
	}
}

// typedRefusals are the four keys Appendix C refuses by their type or keeps
// with a new default: their own checks say what to write, so no row of the
// v1 table names them.
var typedRefusals = []string{
	"logging.level",
	"logging.rotation.max_size",
	"logging.rotation.max_backups",
	"logging.rotation.max_age",
}

// The four keys Appendix C refuses by their type keep their own refusal:
// none of them sits in the v1 table, and the RFC's own table rows for them
// never reach the entry check either.
func TestNoTableRowForTypedRefusals(t *testing.T) {
	t.Parallel()

	for _, key := range typedRefusals {
		if _, retired := retiredRule(key); retired {
			t.Errorf("the table holds %s, a key the typed refusal owns", key)
		}
	}

	data, err := os.ReadFile(rfc13Path)
	if err != nil {
		t.Fatal(err)
	}

	var parsed []string

	for _, row := range renamedParse(string(data)) {
		if row.class == classTyped {
			parsed = append(parsed, row.keys...)
		}
	}

	slices.Sort(parsed)

	want := slices.Clone(typedRefusals)
	slices.Sort(want)

	if !slices.Equal(parsed, want) {
		t.Errorf("the typed rows of the RFC's table:\ngot  %q\nwant %q", parsed, want)
	}
}

// The Appendix B setting line wins over a family entry of the v1 table that
// holds the same first word: a setting refused as v1 stops telling the
// operator the one way to change it, through the management API.
func TestSettingBeforeFamilyEntry(t *testing.T) {
	t.Parallel()

	content := "gc:\n  interval: 1h\n  legacy: 1\n" +
		"identity:\n  id_range: x\nadapters:\n  smb:\n    port: 445\n"

	path, got := loadRefused(t, content, nil)

	want := wantReport(path,
		"P: adapters.smb.port: "+setting,
		"P: gc.interval: "+setting,
		"P: gc.legacy: "+familyKeyRule,
		"P: identity.id_range: "+setting,
	)

	if got != want {
		t.Errorf("report:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// The v1 family the loader read beside its renamed twin follows the twin's
// end: its entries name what the twin names, never the intermediate key. A
// key in another case is an unknown key, not a retired one.
func TestBlockstoreLocalChain(t *testing.T) {
	t.Parallel()

	runRefusals(t, []refusalRow{
		{
			name:    "the twin of the retired key",
			content: "blockstore:\n  local:\n    path: /srv/blocks\n",
			want:    []string{"P: blockstore.local.path: no longer read; write journals"},
		},
		{
			name:    "a sibling under the same plan",
			content: "blockstore:\n  local:\n    chunk_size: \"1MiB\"\n",
			want:    []string{"P: blockstore.local.chunk_size: " + removedKeyRule},
		},
		{
			name:    "a key in another case",
			content: "Blockstore:\n  Local:\n    Path: s3cr3t\n",
			want: []string{
				"P: Blockstore.Local.Path: unknown key; did you mean keys.storage.local.file?",
			},
		},
	})
}

// proseOnly are the table keys the RFC names inside a row's prose and not as
// a row of its own: the timeouts, the jwt family and the pprof trio of the
// controlplane.host row's sentence, and the TLS pair its tls sentence names.
var proseOnly = map[string]bool{
	"controlplane.read_timeout":        true,
	"controlplane.write_timeout":       true,
	"controlplane.idle_timeout":        true,
	"controlplane.jwt":                 true,
	"controlplane.pprof":               true,
	"controlplane.pprof_mutex_rate":    true,
	"controlplane.pprof_block_rate_ns": true,
	"controlplane.tls.cert_file":       true,
	"controlplane.tls.key_file":        true,
}

// renamedRow is one row of RFC 13 Appendix C's "From the files of today"
// table as renamedParse reads it.
type renamedRow struct {
	keys  []string // the row's keys resolved, a family's as its own name
	class string   // one of the class constants above
	write string   // the replacement a renamed row's second column names
}

// backquote holds one backquoted word of a row's cells.
var backquote = regexp.MustCompile("`([^`]*)`")

// asides holds a row's parenthetical, an example and not a second key.
var asides = regexp.MustCompile(`\([^)]*\)`)

// renamedParse reads the rows of RFC 13 Appendix C's "From the files of
// today" table out of its text: the first column's backquoted keys, a
// ".x" key resolved against the row's first, a "x.*" family kept as the
// family's own name; the rows the file's refusals own by their type, and
// the rows of the names read outside the file, held apart by their class;
// the replacement named by the first backquoted key of the second column.
func renamedParse(text string) []renamedRow {
	start := strings.Index(text, "**From the files of today.**")
	if start < 0 {
		return nil
	}

	end := strings.Index(text[start:], "\nA start refuses all of these")
	if end < 0 {
		return nil
	}

	var rows []renamedRow

	for line := range strings.SplitSeq(text[start:start+end], "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}

		cells := strings.Split(line, "|")
		if len(cells) < 3 {
			continue
		}

		clean := asides.ReplaceAllString(cells[1], "")
		first, row := "", renamedRow{}

		for _, m := range backquote.FindAllStringSubmatch(clean, -1) {
			tok := m[1]

			if i := strings.Index(tok, ": "); i >= 0 {
				tok = tok[:i]
			}

			key := tok
			switch {
			case strings.HasPrefix(tok, "."):
				fam := strings.Split(first, ".")
				key = strings.Join(fam[:len(fam)-1], ".") + "." + tok[1:]
			case first == "":
				first = tok
			}

			if family, ok := strings.CutSuffix(key, ".*"); ok {
				key = family
			}

			if first == "" {
				first = key
			}

			if strings.HasPrefix(key, "DITTOFS") || strings.HasPrefix(key, "_") {
				row.class = classEnv
			}

			row.keys = append(row.keys, key)
		}

		if row.class == "" {
			switch {
			case strings.HasPrefix(first, "logging."):
				row.class = classTyped
			case strings.Contains(cells[2], "no longer host facts"):
				row.class = classFamily
			case strings.HasPrefix(strings.TrimSpace(cells[2]), "dropped:"):
				row.class = classDropped
			default:
				row.class = classRenamed

				if m := backquote.FindStringSubmatch(cells[2]); m != nil {
					row.write = m[1]
				}
			}
		}

		rows = append(rows, row)
	}

	return rows
}

// divergence lists every key the v1 table does not agree on with the RFC's
// table in text: a row the table holds no entry for, an entry no row of the
// RFC or its rows' prose names, and a renamed entry whose rule does not name
// the replacement its row gives.
func divergence(text string) []string {
	var diff []string

	parsed := map[string]bool{}

	for _, row := range renamedParse(text) {
		if row.class == classTyped || row.class == classEnv {
			continue
		}

		for _, key := range row.keys {
			parsed[key] = true

			rule, ok := retiredRule(key)
			if !ok {
				diff = append(diff, key+": the RFC names it, the table does not")

				continue
			}

			if row.write != "" && !strings.Contains(rule, row.write) {
				diff = append(diff, key+": the rule does not name "+row.write)
			}
		}
	}

	for _, e := range retiredKeys {
		if _, ok := parsed[e.key]; !ok && !proseOnly[e.key] {
			diff = append(diff, e.key+": no row of the RFC names it")
		}
	}

	slices.Sort(diff)

	return slices.Compact(diff)
}

// namesOf reports whether diff holds one line naming key.
func namesOf(diff []string, key string) bool {
	return slices.ContainsFunc(diff, func(d string) bool {
		return strings.HasPrefix(d, key+":")
	})
}

// plantedRFC drops the row that starts with from from text, or, when needle is
// not empty, rewrites needle in every line it is in to to.
func plantedRFC(text string, from, needle, to string) string {
	var b strings.Builder

	for line := range strings.SplitSeq(text, "\n") {
		switch {
		case from != "" && strings.HasPrefix(line, from):
			continue
		case needle != "":
			b.WriteString(strings.Replace(line, needle, to, 1) + "\n")

			continue
		}

		b.WriteString(line + "\n")
	}

	return b.String()
}

// The v1 table is Appendix C's "From the files of today": every key the
// RFC's table names is an entry, every entry is a key its table or its rows'
// prose names, and a renamed entry's rule names the replacement its row
// gives. A planted copy alters one row and fails naming the row's key.
func TestRetiredKeysMatchAppendixC(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(rfc13Path)
	if err != nil {
		t.Fatal(err)
	}

	text := string(data)

	if diff := divergence(text); len(diff) > 0 {
		t.Errorf("the table and the RFC:\n%s", strings.Join(diff, "\n"))
	}

	removal := plantedRFC(text, "| `metrics.auth`", "", "")
	if removal == text {
		t.Fatal("the planted missing row did not change the RFC")
	}

	if diff := divergence(removal); !namesOf(diff, "metrics.auth") {
		t.Errorf("the missing row is not named:\ngot  %q", diff)
	}

	if !strings.Contains(text, "`controlplane.host`, `controlplane.port`") {
		t.Fatal("the planted key is not in the RFC")
	}

	renamed := plantedRFC(
		text,
		"",
		"`controlplane.host`, `controlplane.port`",
		"`controlplane.hostname`, `controlplane.port`",
	)

	diff := divergence(renamed)
	if !namesOf(diff, "controlplane.hostname") || !namesOf(diff, "controlplane.host") {
		t.Errorf("the renamed row is not named:\ngot  %q", diff)
	}

	if !strings.Contains(text, "`metrics.listen`;") {
		t.Fatal("the planted replacement is not in the RFC")
	}

	other := plantedRFC(text, "", "`metrics.listen`;", "`metrics.interval`;")
	if diff := divergence(other); !namesOf(diff, "metrics.enabled") {
		t.Errorf("the row of the other replacement is not named:\ngot  %q", diff)
	}
}
