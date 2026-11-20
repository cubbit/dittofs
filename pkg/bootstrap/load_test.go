package bootstrap

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/google/uuid"
)

// writeFile writes content to config.yaml in a new directory and returns its
// path.
func writeFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return path
}

// wantReport renders want's lines with "P: " replaced by path, one per line, as
// Load's error prints them.
func wantReport(path string, want ...string) string {
	out := make([]string, len(want))
	for i, line := range want {
		if strings.HasPrefix(line, "P: ") {
			line = path + line[1:]
		}

		out[i] = line
	}

	return strings.Join(out, "\n")
}

// loadRefused loads content with environ and returns the error's text, failing
// the test when the load succeeds or returns a bootstrap beside the error.
func loadRefused(t *testing.T, content string, environ []string) (path, got string) {
	t.Helper()

	path = writeFile(t, content)

	loaded, err := LoadWithSources(path, environ)
	if err == nil {
		t.Fatalf("LoadWithSources of %q: got no error, want a refusal", content)
	}

	if loaded != nil {
		t.Errorf("LoadWithSources of %q: got a bootstrap beside the error, want nil", content)
	}

	return path, err.Error()
}

// loads loads content with environ and fails the test on any refusal.
func loads(t *testing.T, content string, environ []string) *Loaded {
	t.Helper()

	loaded, err := LoadWithSources(writeFile(t, content), environ)
	if err != nil {
		t.Fatalf("LoadWithSources of %q: %v", content, err)
	}

	return loaded
}

// setting is the rule a setting written in the host's file breaks.
const setting = "a setting, not a host fact; set it through the management API"

// A refusal row: a file's content and an environment, and the exact report.
type refusalRow struct {
	name    string
	content string
	environ []string
	want    []string
	absent  []string
}

func runRefusals(t *testing.T, rows []refusalRow) {
	t.Helper()

	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path, got := loadRefused(t, tc.content, tc.environ)

			if want := wantReport(path, tc.want...); got != want {
				t.Errorf("report:\ngot:\n%s\nwant:\n%s", got, want)
			}

			for _, secret := range tc.absent {
				if strings.Contains(got, secret) {
					t.Errorf("report prints %q, which it must never print:\n%s", secret, got)
				}
			}
		})
	}
}

// One load reports every refusal of the file, sorted by where and key, each
// in RFC 13 §6's shape: unknown keys with the closest declared key, a setting
// written in the host's file, a relative path.
func TestLoadReport(t *testing.T) {
	t.Parallel()

	runRefusals(t, []refusalRow{
		{
			name: "two unknown keys, a setting and a relative journal",
			content: "loging:\n  level: debug\nmetrics:\n  listen_address: x\n" +
				"adapters:\n  smb:\n    port: 445\njournals:\n  - path: data/j1\n",
			want: []string{
				"P: adapters.smb.port: " + setting,
				"P: journals: data/j1 is not an absolute path",
				"P: loging.level: unknown key; did you mean logging.level?",
				"P: metrics.listen_address: unknown key; did you mean metrics.listen?",
			},
		},
		{
			name:    "sorted by key, not by rule",
			content: "a:\n  x: 1\nadapters:\n  smb:\n    port: 445\n",
			want: []string{
				"P: a.x: unknown key; did you mean node.id?",
				"P: adapters.smb.port: " + setting,
			},
		},
		{
			name:    "a second document",
			content: "logging:\n  level: debug\n---\nlogging:\n  level: s3cr3t\n",
			want:    []string{"P: -: a second YAML document; the file holds one"},
			absent:  []string{"s3cr3t"},
		},
		{
			name:    "a dotted name is no nested key",
			content: "logging.level: debug\n",
			want: []string{
				"P: logging.level: unknown key; write each part of a key at its own level",
			},
		},
		{
			name:    "a dotted name beside the nested key",
			content: "logging.level: debug\nlogging:\n  level: warn\n",
			want: []string{
				"P: logging.level: unknown key; write each part of a key at its own level",
			},
		},
		{
			name:    "a key in another case names the declared key",
			content: "LOGGING:\n  LEVEL: debug\n",
			want:    []string{"P: LOGGING.LEVEL: unknown key; did you mean logging.level?"},
		},
		{
			name:    "a scratch path Appendix B lists as bootstrap is no setting",
			content: "events:\n  path: /x\n",
			want:    []string{"P: events.path: unknown key; did you mean node.id?"},
		},
		{
			name:    "a syntax error is reworded",
			content: "logging: [s3cr3t\n",
			want:    []string{"P: -: not valid YAML at line 1"},
			absent:  []string{"s3cr3t"},
		},
		{
			name:    "a key listed twice",
			content: "logging:\n  level: debug\nlogging:\n  level: warn\n",
			want:    []string{"P: logging: listed twice"},
		},
		{
			name:    "a root that is not a mapping",
			content: "- a\n",
			want:    []string{"P: -: the root is not a mapping"},
		},
		{
			name:    "a scalar where a block is declared",
			content: "logging: debug\n",
			want:    []string{"P: logging: must be a mapping, got a string"},
		},
	})
}

// A bootstrap key under a prefix settings share is no setting, and a key
// under a setting is the setting.
func TestLoadSettingKeys(t *testing.T) {
	t.Parallel()

	content := "adapters:\n  listen: [127.0.0.1]\n  nfs:\n    port: 2049\n" +
		"identity:\n  oidc:\n    issuer: x\n"

	path, got := loadRefused(t, content, nil)

	want := wantReport(path,
		"P: adapters.nfs.port: "+setting,
		"P: identity.oidc.issuer: "+setting,
	)
	if got != want {
		t.Errorf("report:\ngot:\n%s\nwant:\n%s", got, want)
	}

	if b := loads(t, "adapters:\n  listen: [127.0.0.1]\n", nil); !reflect.DeepEqual(
		b.Adapters.Listen,
		[]string{"127.0.0.1"},
	) {
		t.Errorf("adapters.listen: got %q, want [127.0.0.1]", b.Adapters.Listen)
	}
}

// Each value is refused by its type or its bound, naming the key; a type
// refusal names the type, never the value, and nothing is coerced.
func TestLoadRefusesValues(t *testing.T) {
	t.Parallel()

	rotation := func(key, value string) string {
		return "logging:\n  rotation:\n    " + key + ": " + value + "\n"
	}
	sizeType := `P: logging.rotation.max_size: must be a size such as "100MiB", got `
	ageType := `P: logging.rotation.max_age: must be a duration such as "720h", got `
	wholeMiB := "P: logging.rotation.max_size: must be a whole number of MiB, at least 1MiB, got "

	rows := []refusalRow{
		{
			name: "scalar for a list", content: "journals: \"/data/j1\"\n",
			want: []string{"P: journals: must be a list, got a string"},
		},
		{
			name: "quoted t", content: rotation("compress", `"t"`),
			want: []string{"P: logging.rotation.compress: must be true or false, got a string"},
		},
		{
			name: "quoted true", content: rotation("compress", `"true"`),
			want: []string{"P: logging.rotation.compress: must be true or false, got a string"},
		},
		{
			name: "upper case TRUE", content: rotation("compress", "TRUE"),
			want: []string{"P: logging.rotation.compress: must be true or false, got TRUE"},
		},
		{
			name: "hex integer", content: rotation("max_backups", "0x10"),
			want: []string{"P: logging.rotation.max_backups: must be an integer, got a number"},
		},
		{
			name: "integer with an underscore", content: rotation("max_backups", "1_0"),
			want: []string{"P: logging.rotation.max_backups: must be an integer, got a number"},
		},
		{
			name: "number for a string", content: "logging:\n  format: 1\n",
			want: []string{"P: logging.format: must be a string, got a number"},
		},
		{
			name: "number for an age", content: rotation("max_age", "7"),
			want: []string{ageType + "a number"},
		},
		{
			name: "fraction for a size", content: rotation("max_size", "1.5"),
			want: []string{sizeType + "a number"},
		},
		{
			name: "v1 integer size", content: rotation("max_size", "100"),
			want: []string{sizeType + "a number"},
		},
		{
			name: "1536KiB", content: rotation("max_size", `"1536KiB"`),
			want: []string{wholeMiB + "1536KiB"},
		},
		{
			name: "512KiB", content: rotation("max_size", `"512KiB"`),
			want: []string{wholeMiB + "512KiB"},
		},
		{
			name: "1MB", content: rotation("max_size", `"1MB"`),
			want: []string{wholeMiB + "1MB"},
		},
		{
			name: "36h", content: rotation("max_age", `"36h"`),
			want: []string{"P: logging.rotation.max_age: must be a whole number of days, got 36h"},
		},
		{
			name: "level verbose", content: "logging:\n  level: verbose\n",
			want: []string{
				"P: logging.level: must be one of debug, info, warn, error, got verbose",
			},
		},
		{
			name: "level INFO", content: "logging:\n  level: INFO\n",
			want: []string{"P: logging.level: must be one of debug, info, warn, error, got INFO"},
		},
		{
			name: "format xml", content: "logging:\n  format: xml\n",
			want: []string{"P: logging.format: must be text or json, got xml"},
		},
		{
			name: "kind aws", content: "keys:\n  protocol:\n    kind: aws\n",
			want: []string{"P: keys.protocol.kind: must be local or kmip, got aws"},
		},
		{
			name: "role compute", content: "node:\n  roles: [compute]\n",
			want: []string{"P: node.roles: must be protocol or storage, got compute"},
		},
		{
			name: "relative output", content: "logging:\n  output: logs/d.log\n",
			want: []string{"P: logging.output: logs/d.log is not an absolute path"},
		},
		{
			name: "trailing slash", content: "journals:\n  - path: /data/j1/\n",
			want: []string{"P: journals: /data/j1/ is not a clean absolute path; write /data/j1"},
		},
		{
			name: "tilde", content: "metadata:\n  badger:\n    path: ~/m\n",
			want: []string{"P: metadata.badger.path: ~/m is not an absolute path"},
		},
		{
			name:    "PEM in a key file",
			content: "api:\n  tls:\n    key_file: \"-----BEGIN PRIVATE KEY-----\\nMIIBcanary\"\n",
			want:    []string{"P: api.tls.key_file: not an absolute path"},
			absent:  []string{"BEGIN", "MIIBcanary"},
		},
		{
			name: "string for an integer", content: rotation("max_backups", "s3cr3t"),
			want:   []string{"P: logging.rotation.max_backups: must be an integer, got a string"},
			absent: []string{"s3cr3t"},
		},
		{
			name: "misspelled password", content: "pasword: s3cr3t\n",
			want:   []string{"P: pasword: unknown key; did you mean node.id?"},
			absent: []string{"s3cr3t"},
		},
		{
			name: "v1 secret", content: "controlplane:\n  jwt:\n    secret: s3cr3t\n",
			want:   []string{"P: controlplane.jwt.secret: " + removedKeyRule},
			absent: []string{"s3cr3t"},
		},
		{
			name: "empty host", content: "api:\n  listen: [\":8080\"]\n",
			want: []string{
				"P: api.listen: :8080 has an empty host; write 0.0.0.0:8080 or [::]:8080",
			},
		},
		{
			name: "host name", content: "api:\n  listen: [\"host.example:8080\"]\n",
			want: []string{"P: api.listen: host.example:8080: the host is not an IP literal"},
		},
		{
			name: "port out of range", content: "api:\n  listen: [\"127.0.0.1:70000\"]\n",
			want: []string{"P: api.listen: 127.0.0.1:70000: the port must be between 0 and 65535"},
		},
		{
			name:    "adapter address with a port",
			content: "adapters:\n  listen: [\"127.0.0.1:80\"]\n",
			want:    []string{"P: adapters.listen: 127.0.0.1:80 is not an IP literal"},
		},
		{
			name: "TLS cert alone",
			content: "metadata:\n  backend: foundationdb\n  foundationdb:\n    cluster_file: /c\n" +
				"    tls:\n      cert_file: /t/c\n",
			want: []string{
				"P: metadata.foundationdb.tls.cert_file: named without key_file and ca_file; " +
					"name the three together or none",
			},
		},
		{
			name: "TLS without a CA",
			content: "metadata:\n  backend: foundationdb\n  foundationdb:\n    cluster_file: /c\n" +
				"    tls:\n      cert_file: /t/c\n      key_file: /t/k\n",
			want: []string{"P: metadata.foundationdb.tls.cert_file: named without ca_file; " +
				"name the three together or none"},
		},
		{
			name: "empty journal entry", content: "journals:\n  - path: \"\"\n",
			want: []string{"P: journals: an empty entry"},
		},
		{
			name: "node.id not a UUID", content: "node:\n  id: not-a-uuid\n",
			want: []string{"P: node.id: must be a UUID"}, absent: []string{"not-a-uuid"},
		},
		{
			name: "negative backups", content: rotation("max_backups", "-1"),
			want: []string{"P: logging.rotation.max_backups: must be 0 or more, got -1"},
		},
		{
			name: "null level", content: "logging:\n  level:\n",
			want: []string{"P: logging.level: must be one of debug, info, warn, error"},
		},
	}

	rows = append(rows,
		refusalRow{
			// The bound is the largest uint64 bytes: a record may hold a
			// byte count up to it, so the product bound is the same here.
			name:    "a size past the whole range",
			content: "logging:\n  rotation:\n    max_size: \"18446744073709551615TiB\"\n",
			want: []string{
				`P: logging.rotation.max_size: must be a size such as "100MiB", got a string`,
			},
		},
		refusalRow{
			name: "a duration past int64",
			// 5124096h is past int64's nanoseconds, and its product wraps
			// to about 27 minutes, so a parser without the bound reads it.
			content: "logging:\n  rotation:\n    max_age: \"5124096h\"\n",
			want: []string{
				`P: logging.rotation.max_age: must be a duration such as "720h", got a string`,
			},
		},
		refusalRow{
			name:    "an empty duration",
			content: "logging:\n  rotation:\n    max_age: \"\"\n",
			want: []string{
				`P: logging.rotation.max_age: must be a duration such as "720h", got a string`,
			},
		},
		refusalRow{
			name:    "a zero size",
			content: "logging:\n  rotation:\n    max_size: \"0MiB\"\n",
			want: []string{
				"P: logging.rotation.max_size: must be a whole number of MiB, at least 1MiB, " +
					"got 0MiB",
			},
		},
		refusalRow{
			name:    "a negative port",
			content: "api:\n  listen: [\"127.0.0.1:-1\"]\n",
			want:    []string{"P: api.listen: 127.0.0.1:-1: the port must be between 0 and 65535"},
		},
		refusalRow{
			name:    "a key on two lines is quoted",
			content: "\"a\\nenvironment: x\": 1\n",
			want: []string{
				`P: "a\nenvironment: x": unknown key; did you mean api.listen?`,
			},
		},
		refusalRow{
			name:    "a journal entry with another key",
			content: "journals:\n  - path: /j1\n    size: 3\n",
			want:    []string{"P: journals.size: unknown key; did you mean journals?"},
		},
		refusalRow{
			name:    "a journal entry that is no mapping",
			content: "journals:\n  - /j1\n",
			want: []string{
				"P: journals: each entry must be a mapping with a path, got a string",
			},
		},
		refusalRow{
			name:    "a journal path that is no string",
			content: "journals:\n  - path: 1\n",
			want:    []string{"P: journals: each entry's path must be a string, got a number"},
		},
		refusalRow{
			name:    "a list holding a number",
			content: "api:\n  listen: [1]\n",
			want: []string{
				"P: api.listen: must be a list of strings, got a list holding a number",
			},
		},
		refusalRow{
			name:    "a value on two lines is not printed",
			content: "logging:\n  output: \"a\\nb\"\n",
			want:    []string{"P: logging.output: not an absolute path"},
		},
		refusalRow{
			name:    "a custom tag",
			content: "logging:\n  level: !mytag debug\n",
			want:    []string{"P: logging.level: must be a string, got a value of another type"},
			absent:  []string{"mytag"},
		},
	)

	// No refusal prints a credential key's value, whatever rule it breaks.
	fdbTLS := func(file string) string {
		tls := map[string]string{"cert_file": "/t/c", "key_file": "/t/k", "ca_file": "/t/a"}
		tls[file] = "rel/s3cr3t"

		return "metadata:\n  backend: foundationdb\n  foundationdb:\n    cluster_file: /c\n" +
			"    tls:\n      cert_file: " + tls["cert_file"] + "\n      key_file: " +
			tls["key_file"] + "\n      ca_file: " + tls["ca_file"] + "\n"
	}
	role := func(kind, block, key, value string) string {
		return "keys:\n  storage:\n    kind: " + kind + "\n    " + block + ":\n      " + key +
			": " + value + "\n"
	}

	for _, tc := range []struct {
		key     string
		content string
	}{
		{"metadata.foundationdb.tls.cert_file", fdbTLS("cert_file")},
		{"metadata.foundationdb.tls.key_file", fdbTLS("key_file")},
		{"metadata.foundationdb.tls.ca_file", fdbTLS("ca_file")},
		{"api.tls.cert_file", "api:\n  tls:\n    cert_file: rel/s3cr3t\n"},
		{"api.tls.key_file", "api:\n  tls:\n    key_file: rel/s3cr3t\n"},
		{"keys.storage.local.file", role("local", "local", "file", "rel/s3cr3t")},
		{
			"keys.storage.local.retired_files",
			role("local", "local", "retired_files", "[rel/s3cr3t]"),
		},
		{"keys.storage.kmip.server_ca", role("kmip", "kmip", "server_ca", "rel/s3cr3t")},
		{"keys.storage.kmip.client_cert", role("kmip", "kmip", "client_cert", "rel/s3cr3t")},
		{"keys.storage.kmip.client_key", role("kmip", "kmip", "client_key", "rel/s3cr3t")},
	} {
		rows = append(rows, refusalRow{
			name:    "credential " + tc.key,
			content: tc.content,
			want:    []string{"P: " + tc.key + ": not an absolute path"},
			absent:  []string{"s3cr3t"},
		})
	}

	// The grammar of RFC 13 Appendix B, not time.ParseDuration's nor
	// internal/bytesize's.
	for _, v := range []string{`"-24h"`, `"1.5h"`, `"36H"`, `"7"`, `"5µs"`} {
		rows = append(rows, refusalRow{
			name: "age " + v, content: rotation("max_age", v),
			want: []string{ageType + "a string"},
		})
	}

	for _, v := range []string{`"100"`, `"100mib"`, `"1.5MiB"`, `"1Gi"`, `"10m"`, `"1g"`} {
		rows = append(rows, refusalRow{
			name: "size " + v, content: rotation("max_size", v),
			want: []string{sizeType + "a string"},
		})
	}

	runRefusals(t, rows)
}

// The bounds' own edges load, and an explicit zero is no default.
func TestLoadAcceptsEdges(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		value string
		key   string
		want  int
	}{
		{`"1MiB"`, "max_size", 1},
		{`"1GiB"`, "max_size", 1024},
		{`"0s"`, "max_age", 0},
		{`"720h"`, "max_age", 30},
		{`"48h0m"`, "max_age", 2},
		{"0", "max_backups", 0},
		{"010", "max_backups", 10}, // the variable reads 010 as 10 too
	} {
		t.Run(tc.key+" "+tc.value, func(t *testing.T) {
			t.Parallel()

			b := loads(t, "logging:\n  rotation:\n    "+tc.key+": "+tc.value+"\n", nil)

			got := map[string]int{
				"max_size":    b.Logging.Rotation.MaxSize,
				"max_age":     b.Logging.Rotation.MaxAge,
				"max_backups": b.Logging.Rotation.MaxBackups,
			}[tc.key]

			if got != tc.want {
				t.Errorf("logging.rotation.%s: %s: got %v, want %v", tc.key, tc.value, got, tc.want)
			}

			if got := b.Source["logging.rotation."+tc.key]; got != "file" {
				t.Errorf("source of logging.rotation.%s: got %q, want file", tc.key, got)
			}
		})
	}

	t.Run("one document with its start marker", func(t *testing.T) {
		t.Parallel()

		if b := loads(t, "---\nlogging:\n  level: debug\n", nil); b.Logging.Level != "debug" {
			t.Errorf("logging.level after ---: got %q, want debug", b.Logging.Level)
		}
	})

	t.Run("port zero", func(t *testing.T) {
		t.Parallel()

		b := loads(
			t,
			"api:\n  listen: [\"127.0.0.1:0\"]\nmetrics:\n  listen: \"127.0.0.1:0\"\n",
			nil,
		)

		if want := []string{"127.0.0.1:0"}; !reflect.DeepEqual(b.API.Listen, want) ||
			b.Metrics.Listen != "127.0.0.1:0" {
			t.Errorf("listeners on port 0: got api %q metrics %q, want 127.0.0.1:0 for both",
				b.API.Listen, b.Metrics.Listen)
		}
	})

	t.Run("clean journal and off", func(t *testing.T) {
		t.Parallel()

		b := loads(t, "journals:\n  - path: /data/j1\nmetrics:\n  listen: \"off\"\n", nil)

		if want := []Journal{{Path: "/data/j1"}}; !reflect.DeepEqual(b.Journals, want) {
			t.Errorf("journals: got %+v, want %+v", b.Journals, want)
		}

		if b.Metrics.Listen != "off" {
			t.Errorf("metrics.listen: got %q, want off", b.Metrics.Listen)
		}
	})
}

// Rules that relate two keys: each directory once, one metadata block, one
// key block per role, each run only over keys that passed their own check
// and over the values as the file and the environment leave them.
func TestLoadRelations(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	real := filepath.Join(dir, "d")
	link := filepath.Join(dir, "l")

	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	runRefusals(t, []refusalRow{
		{
			name:    "one entry names path twice",
			content: "journals:\n  - {path: /d/j1, path: /d/j2}\n",
			want:    []string{"P: journals.path: listed twice"},
		},
		{
			name: "one path twice", content: "journals:\n  - path: /d/j1\n  - path: /d/j1\n",
			want: []string{"P: journals: /d/j1 listed twice"},
		},
		{
			name:    "two paths to one directory",
			content: "journals:\n  - path: " + real + "\n  - path: " + link + "\n",
			want:    []string{"P: journals: " + real + " and " + link + " name one directory"},
		},
		{
			name: "badger block beside foundationdb",
			content: "metadata:\n  backend: foundationdb\n  foundationdb:\n    cluster_file: /c\n" +
				"  badger:\n    path: /m\n",
			want: []string{
				"P: metadata.badger.path: read only with metadata.backend: badger; remove it",
			},
		},
		{
			name:    "foundationdb block beside the default backend",
			content: "metadata:\n  foundationdb:\n    cluster_file: /c\n",
			want: []string{"P: metadata.foundationdb.cluster_file: " +
				"read only with metadata.backend: foundationdb; remove it"},
		},
		{
			name:    "local block beside kmip",
			content: "keys:\n  protocol:\n    kind: kmip\n    local:\n      file: /k\n",
			want: []string{
				"P: keys.protocol.local.file: read only with keys.protocol.kind: local; " +
					"remove it or change kind",
			},
		},
		{
			name:    "kmip block beside local",
			content: "keys:\n  storage:\n    kind: local\n    kmip:\n      endpoint: x\n",
			want: []string{
				"P: keys.storage.kmip.endpoint: read only with keys.storage.kind: kmip; " +
					"remove it or change kind",
			},
		},
		{
			name:    "a bad backend refuses once",
			content: "metadata:\n  backend: sqlite\n  badger:\n    path: /m\n",
			want:    []string{"P: metadata.backend: must be badger or foundationdb, got sqlite"},
		},
		{
			name:    "kind from a variable",
			content: "keys:\n  protocol:\n    local:\n      file: /k\n",
			environ: []string{"DITTOFS_KEYS_PROTOCOL_KIND=kmip"},
			want: []string{
				"P: keys.protocol.local.file: read only with keys.protocol.kind: local; " +
					"remove it or change kind",
			},
		},
		{
			name: "key from a variable", content: "keys:\n  protocol:\n    kind: kmip\n",
			environ: []string{"DITTOFS_KEYS_PROTOCOL_LOCAL_FILE=/k"},
			want: []string{"environment: DITTOFS_KEYS_PROTOCOL_LOCAL_FILE: " +
				"read only with keys.protocol.kind: local; remove it or change kind"},
		},
	})
}

// A variable's value is parsed by its key's type, refused without its value
// being printed, and replaces the one leaf it names.
func TestLoadEnvironment(t *testing.T) {
	t.Parallel()

	runRefusals(t, []refusalRow{
		{
			name: "compress 1", environ: []string{"DITTOFS_LOGGING_ROTATION_COMPRESS=1"},
			want: []string{"environment: DITTOFS_LOGGING_ROTATION_COMPRESS: must be true or false"},
		},
		{
			name: "backups", environ: []string{"DITTOFS_LOGGING_ROTATION_MAX_BACKUPS=s3cr3tvalue"},
			want: []string{
				"environment: DITTOFS_LOGGING_ROTATION_MAX_BACKUPS: must be an integer",
			},
			absent: []string{"s3cr3tvalue"},
		},
		{
			name: "level", environ: []string{"DITTOFS_LOGGING_LEVEL=s3cr3tvalue"},
			want: []string{
				"environment: DITTOFS_LOGGING_LEVEL: must be one of debug, info, warn, error",
			},
			absent: []string{"s3cr3tvalue"},
		},
		{
			name: "badger path", environ: []string{"DITTOFS_METADATA_BADGER_PATH=rel/s3cr3tvalue"},
			want:   []string{"environment: DITTOFS_METADATA_BADGER_PATH: not an absolute path"},
			absent: []string{"s3cr3tvalue"},
		},
		{
			name: "api listen", environ: []string{"DITTOFS_API_LISTEN=s3cr3tvalue:80"},
			want:   []string{"environment: DITTOFS_API_LISTEN: the host is not an IP literal"},
			absent: []string{"s3cr3tvalue"},
		},
		{
			name: "roles", environ: []string{"DITTOFS_NODE_ROLES=s3cr3tvalue"},
			want:   []string{"environment: DITTOFS_NODE_ROLES: must be protocol or storage"},
			absent: []string{"s3cr3tvalue"},
		},
		{
			name: "journals relative", environ: []string{"DITTOFS_JOURNALS=rel/s3cr3tvalue"},
			want:   []string{"environment: DITTOFS_JOURNALS: not an absolute path"},
			absent: []string{"s3cr3tvalue"},
		},
		{
			name: "journals trailing comma", environ: []string{"DITTOFS_JOURNALS=/data/j1,"},
			want: []string{"environment: DITTOFS_JOURNALS: an empty entry"},
		},
		{
			name: "journals empty", environ: []string{"DITTOFS_JOURNALS="},
			want: []string{"environment: DITTOFS_JOURNALS: an empty entry"},
		},
		{
			name: "journals doubled comma", environ: []string{"DITTOFS_JOURNALS=/a,,/b"},
			want: []string{"environment: DITTOFS_JOURNALS: an empty entry"},
		},
		{
			name: "empty level", environ: []string{"DITTOFS_LOGGING_LEVEL="},
			want: []string{
				"environment: DITTOFS_LOGGING_LEVEL: must be one of debug, info, warn, error",
			},
		},
	})

	t.Run("stderr is an output", func(t *testing.T) {
		t.Parallel()

		b := loads(t, "logging:\n  output: stderr\n", nil)
		if b.Logging.Output != "stderr" {
			t.Errorf("logging.output: got %q, want stderr", b.Logging.Output)
		}
	})

	t.Run("compress true", func(t *testing.T) {
		t.Parallel()

		b := loads(t, "", []string{"DITTOFS_LOGGING_ROTATION_COMPRESS=true"})
		if !b.Logging.Rotation.Compress {
			t.Errorf("logging.rotation.compress: got false, want true")
		}
	})

	t.Run("journals replace the file's list", func(t *testing.T) {
		t.Parallel()

		b := loads(t, "journals:\n  - path: /j1\n  - path: /j2\n", []string{"DITTOFS_JOURNALS=/j3"})

		if want := []Journal{{Path: "/j3"}}; !reflect.DeepEqual(b.Journals, want) {
			t.Errorf("journals: got %+v, want %+v", b.Journals, want)
		}

		if got := b.Source["journals"]; got != "DITTOFS_JOURNALS" {
			t.Errorf("source of journals: got %q, want DITTOFS_JOURNALS", got)
		}
	})

	t.Run("a variable replaces one leaf", func(t *testing.T) {
		t.Parallel()

		b := loads(
			t,
			"logging:\n  level: warn\n  format: json\n",
			[]string{"DITTOFS_LOGGING_LEVEL=error"},
		)

		if b.Logging.Level != "error" || b.Logging.Format != "json" {
			t.Errorf("logging: got level %q format %q, want error and json",
				b.Logging.Level, b.Logging.Format)
		}

		if got := b.Source["logging.level"]; got != "DITTOFS_LOGGING_LEVEL" {
			t.Errorf("source of logging.level: got %q, want DITTOFS_LOGGING_LEVEL", got)
		}

		if got := b.Source["logging.format"]; got != "file" {
			t.Errorf("source of logging.format: got %q, want file", got)
		}
	})

	t.Run("a variable replaces one leaf of a role", func(t *testing.T) {
		t.Parallel()

		b := loads(
			t,
			"keys:\n  protocol:\n    kind: local\n    local:\n"+
				"      file: /k1\n      retired_files: [/k0]\n",
			[]string{"DITTOFS_KEYS_PROTOCOL_LOCAL_FILE=/k2"},
		)

		if got := b.Keys.Protocol.Local.File; got != "/k2" {
			t.Errorf("keys.protocol.local.file: got %q, want /k2", got)
		}

		if got := b.Keys.Protocol.Local.RetiredFiles; !reflect.DeepEqual(got, []string{"/k0"}) {
			t.Errorf("keys.protocol.local.retired_files: got %q, want [/k0]", got)
		}
	})

	t.Run("an empty metrics variable is not set", func(t *testing.T) {
		t.Parallel()

		b := loads(t, "metrics:\n  listen: \"off\"\n", []string{"DITTOFS_METRICS_LISTEN="})
		if b.Metrics.Listen != "off" {
			t.Errorf("metrics.listen: got %q, want off", b.Metrics.Listen)
		}
	})

	t.Run("a null metrics key is not set", func(t *testing.T) {
		t.Parallel()

		b := loads(t, "metrics:\n  listen:\n", nil)

		if b.Metrics.Listen != "127.0.0.1:9090" {
			t.Errorf("metrics.listen: got %q, want 127.0.0.1:9090", b.Metrics.Listen)
		}

		if got := b.Source["metrics.listen"]; got != "default" {
			t.Errorf("source of metrics.listen: got %q, want default", got)
		}
	})

	t.Run("a list variable", func(t *testing.T) {
		t.Parallel()

		b := loads(t, "", []string{"DITTOFS_API_LISTEN=127.0.0.1:1,[::1]:2"})
		if want := []string{"127.0.0.1:1", "[::1]:2"}; !reflect.DeepEqual(b.API.Listen, want) {
			t.Errorf("api.listen: got %q, want %q", b.API.Listen, want)
		}
	})

	t.Run("each type from a variable", func(t *testing.T) {
		t.Parallel()

		b := loads(t, "keys:\n  protocol:\n    kind: kmip\n", []string{
			"DITTOFS_LOGGING_ROTATION_MAX_SIZE=100MiB",
			"DITTOFS_LOGGING_ROTATION_MAX_AGE=720h",
			"DITTOFS_NODE_ID=" + nodeUUID,
			"DITTOFS_KEYS_PROTOCOL_KMIP_TIMEOUT=5s",
			"DITTOFS_LOGGING_ROTATION_MAX_BACKUPS=7",
		})

		got := []any{
			b.Logging.Rotation.MaxSize, b.Logging.Rotation.MaxAge, b.Node.ID,
			b.Keys.Protocol.KMIP.Timeout, b.Logging.Rotation.MaxBackups,
		}
		want := []any{
			100, 30, entity.NodeID(uuid.MustParse(nodeUUID)), 5 * time.Second, 7,
		}

		if !reflect.DeepEqual(got, want) {
			t.Errorf("values from variables: got %v, want %v", got, want)
		}

		for key, name := range map[string]Source{
			"logging.rotation.max_size":    "DITTOFS_LOGGING_ROTATION_MAX_SIZE",
			"logging.rotation.max_age":     "DITTOFS_LOGGING_ROTATION_MAX_AGE",
			"node.id":                      "DITTOFS_NODE_ID",
			"keys.protocol.kmip.timeout":   "DITTOFS_KEYS_PROTOCOL_KMIP_TIMEOUT",
			"logging.rotation.max_backups": "DITTOFS_LOGGING_ROTATION_MAX_BACKUPS",
		} {
			if got := b.Source[key]; got != name {
				t.Errorf("source of %s: got %q, want %q", key, got, name)
			}
		}
	})

	t.Run("an entry without = is no variable", func(t *testing.T) {
		t.Parallel()

		b := loads(t, "", []string{"DITTOFS_LOGGING_LEVEL", "DITTOFS_LOGGING_FORMAT=json"})

		if b.Logging.Level != "info" || b.Source["logging.level"] != "default" {
			t.Errorf("logging.level: got %q from %q, want the default info",
				b.Logging.Level, b.Source["logging.level"])
		}

		if b.Logging.Format != "json" {
			t.Errorf("logging.format: got %q, want json", b.Logging.Format)
		}
	})

	t.Run("journals from a variable naming one directory twice", func(t *testing.T) {
		t.Parallel()

		dir := filepath.Join(t.TempDir(), "s3cr3t")
		real := filepath.Join(dir, "d")
		link := filepath.Join(dir, "l")

		if err := os.MkdirAll(real, 0o700); err != nil {
			t.Fatal(err)
		}

		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}

		for _, tc := range []struct {
			value string
			want  string
		}{
			{"/s3cr3t/j,/s3cr3t/j", "environment: DITTOFS_JOURNALS: a path listed twice"},
			{real + "," + link, "environment: DITTOFS_JOURNALS: two paths name one directory"},
		} {
			_, got := loadRefused(t, "", []string{"DITTOFS_JOURNALS=" + tc.value})

			if got != tc.want {
				t.Errorf("DITTOFS_JOURNALS: got %q, want %q", got, tc.want)
			}

			if strings.Contains(got, "s3cr3t") {
				t.Errorf("DITTOFS_JOURNALS: the report prints the value: %q", got)
			}
		}
	})

	t.Run("the first of a name twice wins", func(t *testing.T) {
		t.Parallel()

		b := loads(t, "", []string{"DITTOFS_LOGGING_LEVEL=warn", "DITTOFS_LOGGING_LEVEL=error"})
		if b.Logging.Level != "warn" {
			t.Errorf("logging.level: got %q, want warn, the first entry, as os.Getenv reads it",
				b.Logging.Level)
		}
	})

	t.Run("a zero from a variable", func(t *testing.T) {
		t.Parallel()

		b := loads(t, "", []string{"DITTOFS_LOGGING_ROTATION_MAX_BACKUPS=0"})
		if b.Logging.Rotation.MaxBackups != 0 {
			t.Errorf("logging.rotation.max_backups: got %d, want 0", b.Logging.Rotation.MaxBackups)
		}
	})
}

// The refusals of the file and of the environment are one report, sorted by
// where: a relative path sorts after "environment", an absolute one before.
// Not parallel: it changes the working directory.
func TestLoadReportsFileAndEnvironmentTogether(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if err := os.WriteFile("zz.yaml", []byte("loging:\n  level: debug\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	environ := []string{"DITTOFS_LOGGING_ROTATION_COMPRESS=1"}
	envLine := "environment: DITTOFS_LOGGING_ROTATION_COMPRESS: must be true or false"
	fileLine := ": loging.level: unknown key; did you mean logging.level?"

	for _, tc := range []struct {
		path string
		want string
	}{
		{"zz.yaml", envLine + "\nzz.yaml" + fileLine},
		{filepath.Join(dir, "zz.yaml"), filepath.Join(dir, "zz.yaml") + fileLine + "\n" + envLine},
	} {
		_, err := Load(tc.path, environ)
		if err == nil {
			t.Fatalf("Load(%s): got no error, want two refusals", tc.path)
		}

		if err.Error() != tc.want {
			t.Errorf("Load(%s):\ngot:\n%s\nwant:\n%s", tc.path, err, tc.want)
		}
	}
}

// The file is --config's, else DITTOFS_CONFIG's, else XDG_CONFIG_HOME's, else
// HOME's, every name read from environ and never from the process.
func TestLoadFindsFile(t *testing.T) {
	t.Parallel()

	file := func(t *testing.T, dir, level string) string {
		t.Helper()

		path := filepath.Join(dir, "dittofs", "config.yaml")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte("logging:\n  level: "+level+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		return path
	}

	xdg := t.TempDir()
	home := t.TempDir()
	named := file(t, t.TempDir(), "error")
	viaVar := file(t, t.TempDir(), "warn")

	file(t, xdg, "debug")
	file(t, filepath.Join(home, ".config"), "info")

	for _, tc := range []struct {
		name    string
		path    string
		environ []string
		want    string
	}{
		{
			"--config first", named,
			[]string{"DITTOFS_CONFIG=" + viaVar, "XDG_CONFIG_HOME=" + xdg},
			"error",
		},
		{
			"DITTOFS_CONFIG next", "",
			[]string{"DITTOFS_CONFIG=" + viaVar, "XDG_CONFIG_HOME=" + xdg},
			"warn",
		},
		{"XDG_CONFIG_HOME", "", []string{"XDG_CONFIG_HOME=" + xdg}, "debug"},
		{"HOME", "", []string{"HOME=" + home}, "info"},
		{
			"an empty DITTOFS_CONFIG is not set", "",
			[]string{"DITTOFS_CONFIG=", "XDG_CONFIG_HOME=" + xdg},
			"debug",
		},
		{
			"XDG_CONFIG_HOME over HOME", "",
			[]string{"HOME=" + home, "XDG_CONFIG_HOME=" + xdg},
			"debug",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b, err := LoadWithSources(tc.path, tc.environ)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			// The source tells a file read from a file missed, which loads
			// the default level, info, too.
			if b.Logging.Level != tc.want || b.Source["logging.level"] != "file" {
				t.Errorf("logging.level: got %q from %q, want %q from the file",
					b.Logging.Level, b.Source["logging.level"], tc.want)
			}
		})
	}
}

// A named file that is missing, and an unreadable default file, refuse.
func TestLoadMissingFile(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "missing.yaml")

	unreadable := t.TempDir()
	if err := os.MkdirAll(filepath.Join(unreadable, "dittofs", "config.yaml"), 0o700); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		path    string
		environ []string
		want    string
	}{
		{"--config", missing, nil, missing + ": -: the file does not exist"},
		{
			"DITTOFS_CONFIG", "",
			[]string{"DITTOFS_CONFIG=" + missing},
			missing + ": -: the file does not exist",
		},
		{
			"unreadable default", "",
			[]string{"XDG_CONFIG_HOME=" + unreadable},
			filepath.Join(unreadable, "dittofs", "config.yaml") + ": -: cannot read the file",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b, err := Load(tc.path, tc.environ)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Load: got %v, want %q", err, tc.want)
			}

			if b != nil {
				t.Errorf("Load: got a bootstrap beside the error, want nil")
			}
		})
	}
}

// A missing default file loads the defaults: it is no refusal, and every key
// takes its default.
func TestLoadMissingDefault(t *testing.T) {
	t.Parallel()

	b, err := LoadWithSources("", []string{"XDG_CONFIG_HOME=" + t.TempDir()})
	if err != nil {
		t.Fatalf("LoadWithSources with no default file: %v", err)
	}

	if len(b.Source) != 42 {
		t.Errorf("sources: got %d keys, want 42", len(b.Source))
	}

	for key, src := range b.Source {
		if src != "default" {
			t.Errorf("source of %s: got %q, want default", key, src)
		}
	}
}

// An empty file, and YAML in a file named .toml, load: the file is YAML.
func TestLoadEmptyAndNamedFiles(t *testing.T) {
	t.Parallel()

	if n := loads(t, "~\n", nil); n.Logging.Level != "info" {
		t.Errorf(
			"a file holding only null: logging.level %q, want the default info",
			n.Logging.Level,
		)
	}

	b := loads(t, "", nil)
	if len(b.Source) != 42 {
		t.Errorf("sources: got %d keys, want 42", len(b.Source))
	}

	for key, src := range b.Source {
		if src != "default" {
			t.Errorf("source of %s in an empty file: got %q, want default", key, src)
		}
	}

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("logging:\n  level: warn\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load of YAML in config.toml: %v", err)
	}

	if got.Logging.Level != "warn" {
		t.Errorf("logging.level: got %q, want warn", got.Logging.Level)
	}
}

// Each key carries its source: the file, the variable that set it, or the
// default; a refusal returns no bootstrap from either entry point.
func TestLoadWithSources(t *testing.T) {
	t.Parallel()

	b := loads(t, "logging:\n  level: warn\n", []string{"DITTOFS_LOGGING_FORMAT=json"})

	for key, want := range map[string]Source{
		"logging.level":  "file",
		"logging.format": "DITTOFS_LOGGING_FORMAT",
		"metrics.listen": "default",
	} {
		if got := b.Source[key]; got != want {
			t.Errorf("source of %s: got %q, want %q", key, got, want)
		}
	}

	if len(b.Source) != 42 {
		t.Errorf("sources: got %d keys, want 42", len(b.Source))
	}

	bad := writeFile(t, "logging:\n  level: verbose\n")

	if cfg, err := Load(bad, nil); err == nil || cfg != nil {
		t.Errorf("Load of a refused file: got %v and %v, want nil and an error", cfg, err)
	}
}

// Every key of RFC 13 Appendix C, set in a file, reaches the bootstrap, and
// its source is the file. Two files: one names the badger and local blocks,
// the other the foundationdb and kmip ones.
func TestLoadEveryKey(t *testing.T) {
	t.Parallel()

	common := "node:\n  id: " + nodeUUID + "\n  roles: [protocol, storage]\n" +
		"journals:\n  - path: /j/1\n  - path: /j/2\n" +
		"adapters:\n  listen: [127.0.0.1, \"::1\"]\n" +
		"api:\n  listen: [\"127.0.0.1:8080\", \"[::1]:8080\"]\n" +
		"  tls:\n    cert_file: /a/api.crt\n    key_file: /a/api.key\n  socket: /run/d/dfs.sock\n" +
		"metrics:\n  listen: \"127.0.0.1:9091\"\n" +
		"logging:\n  level: debug\n  format: json\n  output: /l/d.log\n" +
		"  rotation:\n    max_size: \"2MiB\"\n    max_backups: 3\n    max_age: \"48h\"\n" +
		"    compress: true\n"

	local := func(role string) string {
		return "  " + role + ":\n    kind: local\n    local:\n      file: /" + role +
			"/file\n      retired_files: [/" + role + "/old]\n"
	}
	kmip := func(role string) string {
		return "  " + role + ":\n    kind: kmip\n    kmip:\n      endpoint: " + role +
			".example:5696\n      server_ca: /" + role + "/ca\n      client_cert: /" + role +
			"/cert\n      client_key: /" + role + "/key\n      key_uid: " + role +
			"-uid\n      timeout: \"5s\"\n      retired_key_uids: [" + role + "-old]\n"
	}

	base := func() Bootstrap {
		var b Bootstrap

		b.Node.ID = entity.NodeID(uuid.MustParse(nodeUUID))
		b.Node.Roles = []string{"protocol", "storage"}
		b.Journals = []Journal{{Path: "/j/1"}, {Path: "/j/2"}}
		b.Adapters.Listen = []string{"127.0.0.1", "::1"}
		b.API.Listen = []string{"127.0.0.1:8080", "[::1]:8080"}
		b.API.TLS.CertFile = "/a/api.crt"
		b.API.TLS.KeyFile = "/a/api.key"
		b.API.Socket = "/run/d/dfs.sock"
		b.Metrics.Listen = "127.0.0.1:9091"
		b.Logging = LoggingConfig{
			Level:  "debug",
			Format: "json",
			Output: "/l/d.log",
			Rotation: LogRotationConfig{
				MaxSize:    2,
				MaxBackups: 3,
				MaxAge:     2,
				Compress:   true,
			},
		}

		return b
	}

	badger := base()
	badger.Metadata.Backend = "badger"
	badger.Metadata.Badger.Path = "/m/badger"

	for _, k := range []*WrappingKey{&badger.Keys.Protocol, &badger.Keys.Storage} {
		k.Kind = keyprovider.KindLocal
	}

	badger.Keys.Protocol.Local.File = "/protocol/file"
	badger.Keys.Protocol.Local.RetiredFiles = []string{"/protocol/old"}
	badger.Keys.Storage.Local.File = "/storage/file"
	badger.Keys.Storage.Local.RetiredFiles = []string{"/storage/old"}

	fdb := base()
	fdb.Metadata.Backend = "foundationdb"
	fdb.Metadata.FoundationDB.ClusterFile = "/m/fdb.cluster"
	fdb.Metadata.FoundationDB.TLS.CertFile = "/m/fdb.crt"
	fdb.Metadata.FoundationDB.TLS.KeyFile = "/m/fdb.key"
	fdb.Metadata.FoundationDB.TLS.CAFile = "/m/fdb-ca.crt"

	for _, r := range []struct {
		k    *WrappingKey
		name string
	}{{&fdb.Keys.Protocol, "protocol"}, {&fdb.Keys.Storage, "storage"}} {
		r.k.Kind = keyprovider.KindKMIP
		r.k.KMIP.Endpoint = r.name + ".example:5696"
		r.k.KMIP.ServerCA = "/" + r.name + "/ca"
		r.k.KMIP.ClientCert = "/" + r.name + "/cert"
		r.k.KMIP.ClientKey = "/" + r.name + "/key"
		r.k.KMIP.KeyUID = r.name + "-uid"
		r.k.KMIP.Timeout = 5 * time.Second
		r.k.KMIP.RetiredKeyUIDs = []string{r.name + "-old"}
	}

	for _, tc := range []struct {
		name    string
		content string
		want    Bootstrap
		keys    int
	}{
		{
			"badger and local",
			common + "metadata:\n  backend: badger\n  badger:\n    path: /m/badger\n" +
				"keys:\n" + local("protocol") + local("storage"),
			badger, 24,
		},
		{
			"foundationdb and kmip",
			common + "metadata:\n  backend: foundationdb\n  foundationdb:\n" +
				"    cluster_file: /m/fdb.cluster\n    tls:\n      cert_file: /m/fdb.crt\n" +
				"      key_file: /m/fdb.key\n      ca_file: /m/fdb-ca.crt\n" +
				"keys:\n" + kmip("protocol") + kmip("storage"),
			fdb, 37,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b := loads(t, tc.content, nil)

			// The file the bootstrap came from is Validate's to read, not a
			// key's value.
			got := b.Bootstrap
			got.where = ""

			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("bootstrap:\ngot  %+v\nwant %+v", got, tc.want)
			}

			files := 0

			for _, src := range b.Source {
				if src == "file" {
					files++
				}
			}

			if files != tc.keys {
				t.Errorf("keys sourced from the file: got %d, want %d", files, tc.keys)
			}
		})
	}
}

// An alias that names a mapping holding it is not followed again: the load
// returns, refusing the key. The deadline fires only when the load hangs.
func TestLoadAliasCycle(t *testing.T) {
	t.Parallel()

	path := writeFile(t, "a: &x\n  b: *x\n")
	done := make(chan error, 1)

	go func() {
		_, err := Load(path, nil)
		done <- err
	}()

	select {
	case err := <-done:
		want := path + ": a.b: unknown key; did you mean node.id?"
		if err == nil || err.Error() != want {
			t.Errorf("Load of an alias cycle: got %v, want %q", err, want)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Load of an alias cycle: no return in 30s, the alias is followed again")
	}
}

// A relative state directory that the environment given names refuses,
// naming the variable that chose it, whatever key the default reaches.
func TestLoadRelativeStateDirectory(t *testing.T) {
	t.Parallel()

	const want = "environment: XDG_STATE_HOME: must be an absolute path"

	if _, err := Load("", []string{"XDG_STATE_HOME=rel"}); err == nil || err.Error() != want {
		t.Errorf("Load with XDG_STATE_HOME=rel: got %v, want %q", err, want)
	}
}

// With XDG_STATE_HOME unset, the state directory is HOME's, and a relative
// HOME is the variable the refusal names.
func TestLoadRelativeHome(t *testing.T) {
	t.Parallel()

	const want = "environment: HOME: must be an absolute path"

	if _, err := Load("", []string{"HOME=rel"}); err == nil || err.Error() != want {
		t.Errorf("Load with HOME=rel: got %v, want %q", err, want)
	}
}

// The defaults are fresh for each caller: changing one caller's list does
// not change the next caller's defaults.
func TestGetDefaultConfigIsFresh(t *testing.T) {
	t.Parallel()

	GetDefaultConfig().Node.Roles[0] = "changed"

	if got := GetDefaultConfig().Node.Roles; got[0] != "protocol" {
		t.Errorf("node.roles after a caller changed its copy: got %q, want [protocol storage]", got)
	}
}

// Each unit of RFC 13 Appendix B is worth what the appendix says: a size in
// bytes, stored as whole MiB, and a duration, an age stored as days.
func TestLoadUnits(t *testing.T) {
	t.Parallel()

	rotation := func(key, value string) string {
		return "logging:\n  rotation:\n    " + key + ": \"" + value + "\"\n"
	}

	for _, tc := range []struct {
		value string
		mib   int
	}{
		{"1048576B", 1},
		{"1024KiB", 1},
		{"100MiB", 100},
		{"131072KB", 125},
		{"16384MB", 15625},
		{"2048GB", 1953125},
		{"256TB", 244140625},
		{"1TiB", 1048576},
	} {
		b := loads(t, rotation("max_size", tc.value), nil)
		if got := b.Logging.Rotation.MaxSize; got != tc.mib {
			t.Errorf("max_size %s: got %d MiB, want %d", tc.value, got, tc.mib)
		}
	}

	for _, v := range []string{"24h", "1440m", "86400s", "86400000ms", "23h60m"} {
		b := loads(t, rotation("max_age", v), nil)
		if got := b.Logging.Rotation.MaxAge; got != 1 {
			t.Errorf("max_age %s: got %d days, want 1", v, got)
		}
	}

	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"250ms", 250 * time.Millisecond},
		{"1m30s", 90 * time.Second},
	} {
		b := loads(t, "keys:\n  protocol:\n    kind: kmip\n    kmip:\n      timeout: \""+
			tc.value+"\"\n", nil)
		if got := b.Keys.Protocol.KMIP.Timeout; got != tc.want {
			t.Errorf("kmip.timeout %s: got %v, want %v", tc.value, got, tc.want)
		}
	}
}

// A load carries the file it read, empty when the default file is missing,
// and the two directories with the variable that chose each, the runtime
// fallback naming the state directory's variable, as MakeRuntimeDir does.
func TestLoadedCarriesFileAndDirs(t *testing.T) {
	t.Parallel()

	file := writeFile(t, "logging:\n  level: warn\n")
	home := t.TempDir()

	for _, tc := range []struct {
		name       string
		path       string
		environ    []string
		file       string
		state      string
		stateVar   string
		runtime    string
		runtimeVar string
	}{
		{
			name:       "a named file, the service manager's directories",
			path:       file,
			environ:    []string{"STATE_DIRECTORY=/x/state", "RUNTIME_DIRECTORY=/x/run"},
			file:       file,
			state:      "/x/state",
			stateVar:   "STATE_DIRECTORY",
			runtime:    "/x/run",
			runtimeVar: "RUNTIME_DIRECTORY",
		},
		{
			name:       "the default file missing, the home's directories",
			environ:    []string{"HOME=" + home},
			state:      filepath.Join(home, ".local", "state", "dittofs"),
			stateVar:   "HOME",
			runtime:    filepath.Join(home, ".local", "state", "dittofs", "run"),
			runtimeVar: "HOME",
		},
		{
			name: "the XDG directories",
			path: file,
			environ: []string{
				"XDG_STATE_HOME=/x/xs",
				"XDG_RUNTIME_DIR=/x/xr",
				"HOME=" + home,
			},
			file:       file,
			state:      "/x/xs/dittofs",
			stateVar:   "XDG_STATE_HOME",
			runtime:    "/x/xr/dittofs",
			runtimeVar: "XDG_RUNTIME_DIR",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := LoadWithSources(tc.path, tc.environ)
			if err != nil {
				t.Fatalf("LoadWithSources: %v", err)
			}

			for _, f := range []struct {
				field     string
				got, want string
			}{
				{"File", got.File, tc.file},
				{"StateDir", got.StateDir, tc.state},
				{"StateVar", got.StateVar, tc.stateVar},
				{"RuntimeDir", got.RuntimeDir, tc.runtime},
				{"RuntimeVar", got.RuntimeVar, tc.runtimeVar},
			} {
				if f.got != f.want {
					t.Errorf("Loaded.%s: got %q, want %q", f.field, f.got, f.want)
				}
			}
		})
	}
}

// ConfigPath names the file the flag names, else DITTOFS_CONFIG's, else the
// default path from XDG_CONFIG_HOME, else from HOME, else none.
func TestConfigPath(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		flag    string
		environ []string
		want    string
	}{
		{
			"the flag over the variable",
			"/f/config.yaml",
			[]string{"DITTOFS_CONFIG=/e/config.yaml", "XDG_CONFIG_HOME=/x", "HOME=/h"},
			"/f/config.yaml",
		},
		{
			"the variable over the default",
			"",
			[]string{"DITTOFS_CONFIG=/e/config.yaml", "XDG_CONFIG_HOME=/x", "HOME=/h"},
			"/e/config.yaml",
		},
		{
			"the config home over the home",
			"",
			[]string{"XDG_CONFIG_HOME=/x", "HOME=/h"},
			"/x/dittofs/config.yaml",
		},
		{"the home", "", []string{"HOME=/h"}, "/h/.config/dittofs/config.yaml"},
		{"nothing set", "", nil, ""},
	} {
		if got := ConfigPath(tc.flag, tc.environ); got != tc.want {
			t.Errorf("%s: ConfigPath(%q): got %q, want %q", tc.name, tc.flag, got, tc.want)
		}
	}
}
