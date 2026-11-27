package transformtest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
)

// A Fixture written through its JSON form loads back unchanged, its byte
// strings in hex, and the loader refuses every file and directory that is not
// a case under its version's directory, while passing over hidden entries.
func TestFixturesLoadOnlyWellFormedCases(t *testing.T) {
	prof.Watch(t, prof.Options{})

	want := Fixture{
		Version:  1,
		Settings: map[string]any{"pad": 7.0},
		Material: []struct {
			Kind, ID string
			Bytes    []byte
		}{{"k", "id", []byte{0xab, 0xcd}}},
		Input: []byte("in"),
		Body:  []byte("body"),
	}
	want.Namespace[0], want.Hash[31] = 0x11, 0x22

	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}

	for _, h := range []string{`"input":"696e"`, `"bytes":"abcd"`, `"namespace":"11000000000000000000000000000000"`} {
		if !bytes.Contains(raw, []byte(h)) {
			t.Errorf("Fixture's JSON form %s holds no %s", raw, h)
		}
	}

	good := string(raw)
	field := func(name, value string) string {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}

		m[name] = value
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}

		return string(b)
	}

	for _, c := range []struct {
		name  string
		files map[string]string // path under the fixture directory to content
		want  string            // a substring of the loader's error; none for a load
	}{
		{"a case, beside hidden entries", map[string]string{"v1/a.json": good, ".DS_Store": "", "v1/.keep": ""}, ""},
		{"a version its directory does not name", map[string]string{"v2/a.json": good}, "version 1 under the directory of version 2"},
		{"a short namespace", map[string]string{"v1/a.json": field("namespace", "11")}, "namespace of 1 bytes, want 16"},
		{"a short hash", map[string]string{"v1/a.json": field("hash", "22")}, "hash of 1 bytes, want 32"},
		{"a field the form does not have", map[string]string{"v1/a.json": field("extra", "")}, `unknown field "extra"`},
		{"a byte string not in hex", map[string]string{"v1/a.json": field("input", "zz")}, "invalid byte"},
		{"data after the case", map[string]string{"v1/a.json": good + good}, "after top-level value"},
		{"a directory that is not v<n>", map[string]string{"w1/a.json": good}, "holds w1, want only v<n> directories"},
		{"a version with a leading zero", map[string]string{"v01/a.json": good}, "holds v01, want only v<n> directories"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for p, content := range c.files {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755); err != nil {
					t.Fatal(err)
				}

				if err := os.WriteFile(filepath.Join(dir, p), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			cases, err := loadFixtures(dir)

			switch {
			case c.want == "" && err != nil:
				t.Fatalf("loadFixtures: %v, want a load", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("loadFixtures: %v, want an error containing %q", err, c.want)
			case c.want == "" && (len(cases) != 1 || !reflect.DeepEqual(cases[0].Fixture, want)):
				t.Fatalf("loadFixtures = %+v, want the one case %+v", cases, want)
			}
		})
	}
}
