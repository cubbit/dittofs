package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/cli/output"
	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// nodeUUID is the node identity these tests show, in RFC 13 Appendix C's form.
const nodeUUID = "6f1c2d3e-4b5a-4978-8a6b-1c2d3e4f5a6b"

// show prints node.id as the UUID RFC 13 Appendix C writes, not as the 16
// numbers of its array, in YAML and in JSON.
func TestConfigShowPrintsNodeIDAsUUID(t *testing.T) {
	cfg := bootstrap.GetDefaultConfig()
	cfg.Node.ID = entity.NodeID(uuid.MustParse(nodeUUID))

	for _, tc := range []struct {
		format output.Format
		want   string
	}{
		{output.FormatYAML, "\nnode:\n  id: " + nodeUUID + "\n"},
		{output.FormatJSON, "\"id\": \"" + nodeUUID + "\""},
	} {
		t.Run(string(tc.format), func(t *testing.T) {
			var out bytes.Buffer

			if err := printConfig(&out, &bootstrap.Loaded{Bootstrap: *cfg}, tc.format); err != nil {
				t.Fatalf("printConfig: %v", err)
			}

			if !strings.Contains(out.String(), tc.want) {
				t.Errorf(
					"show in %s: node.id not printed as %q; output:\n%s",
					tc.format,
					tc.want,
					out.String(),
				)
			}
		})
	}
}

// showRun runs show as dfs config show does, with the --config value flag
// and environ, and returns what it printed and its error.
func showRun(t *testing.T, flag string, environ []string, format output.Format) (string, error) {
	t.Helper()

	var out bytes.Buffer

	err := runShow(flag, environ, format, &out)

	return out.String(), err
}

// shown runs show and decodes what it printed, failing the test on a
// refusal.
func shown(
	t *testing.T,
	flag string,
	environ []string,
	format output.Format,
) (string, map[string]any) {
	t.Helper()

	out, err := showRun(t, flag, environ, format)
	if err != nil {
		t.Fatalf("show: %v", err)
	}

	var m map[string]any

	if format == output.FormatJSON {
		err = json.Unmarshal([]byte(out), &m)
	} else {
		err = yaml.Unmarshal([]byte(out), &m)
	}

	if err != nil {
		t.Fatalf("decode show's %s output: %v\n%s", format, err, out)
	}

	return out, m
}

// at is the value under the dotted path in m, nil when a part is missing.
func at(m map[string]any, path string) any {
	var v any = m

	for part := range strings.SplitSeq(path, ".") {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil
		}

		v = mm[part]
	}

	return v
}

// source is the source show lists for key.
func source(m map[string]any, key string) any {
	sources, _ := m["sources"].(map[string]any)

	return sources[key]
}

// wantPlace is a path and its source, as show prints a directory or the socket.
func wantPlace(path, source string) map[string]any {
	return map[string]any{"path": path, "source": source}
}

// show prints the file it read, the two directories with the variable that
// chose each, and the socket path; a socket a variable sets prints the
// variable's name only.
func TestShowPaths(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, "logging:\n  level: warn\n")
	dirs := []string{"STATE_DIRECTORY=/x/state", "RUNTIME_DIRECTORY=/x/run"}

	_, m := shown(t, path, dirs, output.FormatYAML)

	for _, f := range []struct {
		key  string
		want any
	}{
		{"file", path},
		{"directories.state", wantPlace("/x/state", "STATE_DIRECTORY")},
		{"directories.runtime", wantPlace("/x/run", "RUNTIME_DIRECTORY")},
		{"socket", wantPlace("/x/run/dfs.sock", "default")},
	} {
		if got := at(m, f.key); !reflect.DeepEqual(got, f.want) {
			t.Errorf("show's %s: got %v, want %v", f.key, got, f.want)
		}
	}

	out, m := shown(
		t,
		path,
		append(dirs, "DITTOFS_API_SOCKET=/s3cr3t/s.sock"),
		output.FormatYAML,
	)

	if got, want := at(m, "socket"), wantPlace("", "DITTOFS_API_SOCKET"); !reflect.DeepEqual(
		got,
		want,
	) {
		t.Errorf("show's socket set by a variable: got %v, want %v", got, want)
	}

	if got := at(m, "api.socket"); got != "DITTOFS_API_SOCKET" {
		t.Errorf("show's api.socket set by a variable: got %v, want DITTOFS_API_SOCKET", got)
	}

	if strings.Contains(out, "s3cr3t") {
		t.Errorf("show printed the socket variable's value:\n%s", out)
	}
}

// Each key's source is the one the load chose: the file, a variable, the
// default, for all 42 keys of RFC 13 Appendix C.
func TestShowSources(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, "logging:\n  level: warn\n")
	environ := []string{"STATE_DIRECTORY=" + t.TempDir(), "DITTOFS_LOGGING_FORMAT=json"}

	_, m := shown(t, path, environ, output.FormatYAML)

	for key, want := range map[string]string{
		"logging.level":                "file",
		"logging.format":               "DITTOFS_LOGGING_FORMAT",
		"logging.rotation.max_backups": "default",
	} {
		if got := source(m, key); got != want {
			t.Errorf("show's source of %s: got %v, want %s", key, got, want)
		}
	}

	if sources, _ := m["sources"].(map[string]any); len(sources) != 42 {
		t.Errorf("show's sources: got %d keys, want 42", len(sources))
	}
}

// A variable wins over the file, and show prints the variable's name as
// the value, neither the file's value nor the variable's.
func TestShowEnvironmentWinsOverFile(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, "logging:\n  level: warn\n")
	environ := []string{"STATE_DIRECTORY=" + t.TempDir(), "DITTOFS_LOGGING_LEVEL=error"}

	_, m := shown(t, path, environ, output.FormatYAML)

	if got := at(m, "logging.level"); got != "DITTOFS_LOGGING_LEVEL" {
		t.Errorf("show's logging.level: got %v, want DITTOFS_LOGGING_LEVEL", got)
	}

	if got := source(m, "logging.level"); got != "DITTOFS_LOGGING_LEVEL" {
		t.Errorf("show's source of logging.level: got %v, want DITTOFS_LOGGING_LEVEL", got)
	}
}

// A value from the environment prints as its variable's name, in YAML and
// in JSON, and no byte of the output holds the value.
func TestShowNeverPrintsEnvironmentValue(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, "logging:\n  level: warn\n")
	environ := []string{
		"STATE_DIRECTORY=" + t.TempDir(),
		"DITTOFS_METADATA_BADGER_PATH=/s3cr3t/badger",
		"DITTOFS_JOURNALS=/s3cr3t/j1",
	}

	for _, format := range []output.Format{output.FormatYAML, output.FormatJSON} {
		out, m := shown(t, path, environ, format)

		for key, want := range map[string]string{
			"metadata.badger.path": "DITTOFS_METADATA_BADGER_PATH",
			"journals":             "DITTOFS_JOURNALS",
		} {
			if got := at(m, key); got != want {
				t.Errorf("show in %s, %s: got %v, want %s", format, key, got, want)
			}
		}

		if strings.Contains(out, "s3cr3t") {
			t.Errorf("show in %s printed an environment value:\n%s", format, out)
		}
	}
}

// A credential's file prints by its path, never its content.
func TestShowCredentialByPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	var files []string

	for _, name := range []string{"cert", "key", "ca"} {
		f := filepath.Join(dir, name+".pem")
		if err := os.WriteFile(f, []byte("PRIVATE-MARKER\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		files = append(files, f)
	}

	path := writeConfig(t, "metadata:\n  backend: foundationdb\n  foundationdb:\n    tls:\n"+
		"      cert_file: "+files[0]+"\n"+
		"      key_file: "+files[1]+"\n"+
		"      ca_file: "+files[2]+"\n")

	out, m := shown(t, path, []string{"STATE_DIRECTORY=" + dir}, output.FormatYAML)

	if got := at(m, "metadata.foundationdb.tls.cert_file"); got != files[0] {
		t.Errorf("show's metadata.foundationdb.tls.cert_file: got %v, want %s", got, files[0])
	}

	if strings.Contains(out, "PRIVATE-MARKER") {
		t.Errorf("show printed a credential file's content:\n%s", out)
	}
}

// show checks nothing past the load: a file with no journal and no key
// prints.
func TestShowNoJournalsNoKeys(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, "logging:\n  level: warn\n")

	out, err := showRun(t, path, []string{"STATE_DIRECTORY=" + t.TempDir()}, output.FormatYAML)
	if err != nil {
		t.Fatalf("show of a file with no journal and no key: %v", err)
	}

	if out == "" {
		t.Error("show printed nothing")
	}
}

// A file the start refuses returns the start's refusal, its four lines, and
// show prints nothing.
func TestShowRefusal(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, fourRefusals)

	out, err := showRun(t, path, []string{"STATE_DIRECTORY=" + t.TempDir()}, output.FormatYAML)
	if err == nil {
		t.Fatal("show: got no error, want the four refusals")
	}

	if want := lines(path, fourRefusalLines); err.Error() != want {
		t.Errorf("show's error:\ngot  %q\nwant %q", err.Error(), want)
	}

	if out != "" {
		t.Errorf("show printed %q beside a refusal, want nothing", out)
	}
}

// The JSON output is the YAML output's tree.
func TestShowJSONMatchesYAML(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, "logging:\n  level: warn\n")
	environ := []string{"STATE_DIRECTORY=" + t.TempDir(), "DITTOFS_LOGGING_FORMAT=json"}

	_, fromYAML := shown(t, path, environ, output.FormatYAML)
	_, fromJSON := shown(t, path, environ, output.FormatJSON)

	raw, err := json.Marshal(fromYAML)
	if err != nil {
		t.Fatal(err)
	}

	var yamlAsJSON map[string]any
	if err := json.Unmarshal(raw, &yamlAsJSON); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(yamlAsJSON, fromJSON) {
		t.Errorf("show's JSON differs from its YAML:\nyaml %v\njson %v", yamlAsJSON, fromJSON)
	}
}

// With no file at the default path, show prints the file as empty, and
// every key's source is the default or a variable.
func TestShowNoFile(t *testing.T) {
	t.Parallel()

	environ := []string{"HOME=" + t.TempDir(), "DITTOFS_LOGGING_LEVEL=warn"}

	for _, tc := range []struct {
		format output.Format
		line   string
	}{
		{output.FormatYAML, "\nfile: \"\"\n"},
		{output.FormatJSON, "\"file\": \"\""},
	} {
		out, m := shown(t, "", environ, tc.format)

		if !strings.Contains("\n"+out, tc.line) {
			t.Errorf("show in %s with no file: no line %q in\n%s", tc.format, tc.line, out)
		}

		sources, _ := m["sources"].(map[string]any)
		if len(sources) == 0 {
			t.Errorf("show in %s with no file: no sources", tc.format)
		}

		for key, s := range sources {
			if name, _ := s.(string); name != "default" && !strings.HasPrefix(name, "DITTOFS_") {
				t.Errorf("show in %s with no file: source of %s is %v", tc.format, key, s)
			}
		}
	}
}

// The log rotation's size and age print as the file writes them, with
// their unit.
func TestShowUnits(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		body          string
		maxSize, days string
	}{
		{"the defaults", "logging:\n  level: warn\n", "100MiB", "720h"},
		{
			"a file's values",
			"logging:\n  rotation:\n    max_size: \"250MiB\"\n    max_age: \"48h\"\n",
			"250MiB",
			"48h",
		},
	} {
		path := writeConfig(t, tc.body)

		_, m := shown(t, path, []string{"STATE_DIRECTORY=" + t.TempDir()}, output.FormatYAML)

		if got := at(m, "logging.rotation.max_size"); got != tc.maxSize {
			t.Errorf("%s: show's max_size: got %v, want %s", tc.name, got, tc.maxSize)
		}

		if got := at(m, "logging.rotation.max_age"); got != tc.days {
			t.Errorf("%s: show's max_age: got %v, want %s", tc.name, got, tc.days)
		}
	}
}

// Each directory's source is what chose it: the variable that named it, or
// "default" for the temp directory a load falls back to when no variable
// and no home name one, never HOME, which named nothing there; a runtime
// variable still names its own directory. Not
// parallel: the fallback is reached only with the process's HOME unset.
func TestShowStateDirSource(t *testing.T) {
	temp := filepath.Join(os.TempDir(), "dittofs")

	for _, tc := range []struct {
		name           string
		processHome    bool
		environ        []string
		state, runtime map[string]any
	}{
		{
			name:        "the home",
			processHome: true,
			environ:     []string{"HOME=/h"},
			state:       wantPlace("/h/.local/state/dittofs", "HOME"),
			runtime:     wantPlace("/h/.local/state/dittofs/run", "HOME"),
		},
		{
			name:        "the service manager's variable",
			processHome: true,
			environ:     []string{"STATE_DIRECTORY=/x/state", "HOME=/h"},
			state:       wantPlace("/x/state", "STATE_DIRECTORY"),
			runtime:     wantPlace("/x/state/run", "STATE_DIRECTORY"),
		},
		{
			name:    "no variable and no home",
			state:   wantPlace(temp, "default"),
			runtime: wantPlace(filepath.Join(temp, "run"), "default"),
		},
		{
			name:    "no home, a runtime variable",
			environ: []string{"XDG_RUNTIME_DIR=/x/xr"},
			state:   wantPlace(temp, "default"),
			runtime: wantPlace("/x/xr/dittofs", "XDG_RUNTIME_DIR"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.processHome {
				t.Setenv("HOME", "")
			}

			_, m := shown(t, "", tc.environ, output.FormatYAML)

			if got := at(m, "directories.state"); !reflect.DeepEqual(got, tc.state) {
				t.Errorf("show's directories.state: got %v, want %v", got, tc.state)
			}

			if got := at(m, "directories.runtime"); !reflect.DeepEqual(got, tc.runtime) {
				t.Errorf("show's directories.runtime: got %v, want %v", got, tc.runtime)
			}
		})
	}
}
