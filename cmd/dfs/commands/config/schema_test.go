package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// printedSchema runs schema as dfs config schema does and decodes what it
// printed.
func printedSchema(t *testing.T) map[string]any {
	t.Helper()

	var out bytes.Buffer

	if err := writeSchema("", &out); err != nil {
		t.Fatalf("schema: %v", err)
	}

	var s map[string]any
	if err := json.Unmarshal(out.Bytes(), &s); err != nil {
		t.Fatalf("decode the schema: %v\n%s", err, out.String())
	}

	return s
}

// property is the schema of the dotted key in s, nil when it is missing.
func property(s map[string]any, key string) map[string]any {
	for part := range strings.SplitSeq(key, ".") {
		props, _ := s["properties"].(map[string]any)

		s, _ = props[part].(map[string]any)
		if s == nil {
			return nil
		}
	}

	return s
}

// leaves appends the dotted key of every leaf of s: an object with
// properties is descended, anything else is a leaf.
func leaves(s map[string]any, prefix string, out *[]string) {
	props, _ := s["properties"].(map[string]any)

	for name, p := range props {
		key := name
		if prefix != "" {
			key = prefix + "." + name
		}

		ps, _ := p.(map[string]any)
		if _, ok := ps["properties"]; ok && ps["type"] == "object" {
			leaves(ps, key, out)

			continue
		}

		*out = append(*out, key)
	}
}

// appendixCKeys reads RFC 13 Appendix C's key table: each row's first cell
// names a key, or a key and its siblings written as a suffix, ".key_file"
// after "metadata.foundationdb.tls.cert_file", and <role> stands for both
// roles.
func appendixCKeys(t *testing.T) []string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(
		"..", "..", "..", "..", "docs", "internals", "rfcs", "rfc-13-configuration.md",
	))
	if err != nil {
		t.Fatal(err)
	}

	_, table, ok := strings.Cut(string(raw), "\n| Key | Type | Default | Fact |\n")
	if !ok {
		t.Fatal("RFC 13: no key table")
	}

	table, _, _ = strings.Cut(table, "\n\n")

	var keys []string

	for row := range strings.SplitSeq(table, "\n") {
		cell, _, _ := strings.Cut(strings.TrimPrefix(row, "| "), " |")
		if !strings.HasPrefix(cell, "`") {
			continue
		}

		var last string

		for part := range strings.SplitSeq(cell, ", ") {
			key := strings.Trim(part, "`")
			if suffix, ok := strings.CutPrefix(key, "."); ok {
				key = last[:strings.LastIndex(last, ".")+1] + suffix
			}

			last = key

			if strings.Contains(key, "<role>") {
				keys = append(keys,
					strings.ReplaceAll(key, "<role>", "protocol"),
					strings.ReplaceAll(key, "<role>", "storage"),
				)

				continue
			}

			keys = append(keys, key)
		}
	}

	return keys
}

// The schema's leaves are Appendix C's keys, all 42, under the file's own
// lower-case names.
func TestSchemaPropertiesAreAppendixC(t *testing.T) {
	t.Parallel()

	s := printedSchema(t)

	var got []string
	leaves(s, "", &got)

	want := appendixCKeys(t)

	slices.Sort(got)
	slices.Sort(want)

	if !slices.Equal(got, want) {
		t.Errorf("schema leaves:\ngot  %q\nwant %q (RFC 13 Appendix C)", got, want)
	}

	if len(want) != 42 {
		t.Errorf("RFC 13 Appendix C: got %d keys, want 42", len(want))
	}

	for name := range s["properties"].(map[string]any) {
		if name != "" && name[0] >= 'A' && name[0] <= 'Z' {
			t.Errorf("schema property %q is a Go field name, not the file's key", name)
		}
	}
}

// Each key's type is the one the file writes: a UUID, a size and durations
// as strings, not the Go field's array or integer.
func TestSchemaTypes(t *testing.T) {
	t.Parallel()

	s := printedSchema(t)

	for _, tc := range []struct {
		key, typ, format string
	}{
		{"node.id", "string", "uuid"},
		{"journals", "array", ""},
		{"logging.rotation.max_size", "string", ""},
		{"logging.rotation.max_age", "string", ""},
		{"logging.rotation.max_backups", "integer", ""},
		{"logging.rotation.compress", "boolean", ""},
		{"keys.protocol.kmip.timeout", "string", ""},
		{"keys.storage.kmip.timeout", "string", ""},
	} {
		p := property(s, tc.key)
		if p == nil {
			t.Errorf("%s: no property", tc.key)

			continue
		}

		if p["type"] != tc.typ {
			t.Errorf("%s: type %v, want %s", tc.key, p["type"], tc.typ)
		}

		if f, _ := p["format"].(string); f != tc.format {
			t.Errorf("%s: format %q, want %q", tc.key, f, tc.format)
		}
	}
}

// metrics.listen reads a YAML null as not set (RFC 13 Appendix C), so the
// schema takes a string or a null there.
func TestSchemaMetricsListenTakesNull(t *testing.T) {
	t.Parallel()

	p := property(printedSchema(t), "metrics.listen")
	if p == nil {
		t.Fatal("metrics.listen: no property")
	}

	var got []string

	options, _ := p["oneOf"].([]any)
	for _, o := range options {
		m, _ := o.(map[string]any)
		typ, _ := m["type"].(string)
		got = append(got, typ)
	}

	if want := []string{"string", "null"}; !slices.Equal(got, want) {
		t.Errorf("metrics.listen: oneOf types %q, want %q", got, want)
	}
}

// stringList converts a decoded JSON list to strings.
func stringList(v any) []string {
	list, _ := v.([]any)

	out := make([]string, 0, len(list))

	for _, e := range list {
		s, _ := e.(string)
		out = append(out, s)
	}

	return out
}

// Each enumerated key lists its values, in the lower case the file takes.
func TestSchemaEnumerations(t *testing.T) {
	t.Parallel()

	s := printedSchema(t)

	for _, tc := range []struct {
		key   string
		items bool
		want  []string
	}{
		{"logging.level", false, []string{"debug", "info", "warn", "error"}},
		{"logging.format", false, []string{"text", "json"}},
		{"metadata.backend", false, []string{"badger", "foundationdb"}},
		{"node.roles", true, []string{"protocol", "storage"}},
		{"keys.protocol.kind", false, []string{"local", "kmip"}},
		{"keys.storage.kind", false, []string{"local", "kmip"}},
	} {
		p := property(s, tc.key)
		if tc.items && p != nil {
			p, _ = p["items"].(map[string]any)
		}

		if got := stringList(p["enum"]); !slices.Equal(got, tc.want) {
			t.Errorf("%s: enum %q, want %q", tc.key, got, tc.want)
		}
	}
}

// requiredAt appends the required list of every object of s, by its dotted
// key, the root as "" and a list's entries as the list's key and "items".
func requiredAt(s map[string]any, key string, out map[string][]string) {
	if r := stringList(s["required"]); len(r) > 0 {
		slices.Sort(r)
		out[key] = r
	}

	if items, ok := s["items"].(map[string]any); ok {
		requiredAt(items, key+".items", out)
	}

	props, _ := s["properties"].(map[string]any)

	for name, p := range props {
		ps, _ := p.(map[string]any)

		sub := name
		if key != "" {
			sub = key + "." + name
		}

		requiredAt(ps, sub, out)
	}
}

// The schema requires only what no other key decides: the journals and each
// entry's path, the keys, both roles' keys, and each role's kind. FoundationDB's cluster file
// depends on metadata.backend, so it is not required.
func TestSchemaRequired(t *testing.T) {
	t.Parallel()

	got := map[string][]string{}
	requiredAt(printedSchema(t), "", got)

	want := map[string][]string{
		"":               {"journals", "keys"},
		"journals.items": {"path"},
		"keys":           {"protocol", "storage"},
		"keys.protocol":  {"kind"},
		"keys.storage":   {"kind"},
	}

	for key, w := range want {
		if !slices.Equal(got[key], w) {
			t.Errorf("required at %q: got %q, want %q", key, got[key], w)
		}

		delete(got, key)
	}

	for key, r := range got {
		t.Errorf("required at %q: got %q, want none", key, r)
	}
}

// The schema ends in a newline, and --output writes the bytes schema prints
// to stdout, and prints where.
func TestSchemaOutputFile(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer

	if err := writeSchema("", &stdout); err != nil {
		t.Fatalf("schema: %v", err)
	}

	if !bytes.HasSuffix(stdout.Bytes(), []byte("}\n")) {
		t.Errorf("schema's output does not end in a newline after the JSON")
	}

	path := filepath.Join(t.TempDir(), "schema.json")

	var out bytes.Buffer

	if err := writeSchema(path, &out); err != nil {
		t.Fatalf("schema --output: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(got, stdout.Bytes()) {
		t.Errorf("schema --output wrote %d bytes, stdout printed %d; they differ",
			len(got), stdout.Len())
	}

	if want := "JSON schema written to " + path + "\n"; out.String() != want {
		t.Errorf("schema --output printed %q, want %q", out.String(), want)
	}
}
