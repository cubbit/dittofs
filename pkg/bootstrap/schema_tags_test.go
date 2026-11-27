package bootstrap

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// schemaTags appends the jsonschema tag of every field of t, by its dotted
// mapstructure key, leaves, blocks and a list's entries alike.
func schemaTags(t reflect.Type, prefix string, out map[string]string) {
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}

		key := joinKey(prefix, f.Tag.Get("mapstructure"))
		if tag := f.Tag.Get("jsonschema"); tag != "" {
			out[key] = tag
		}

		switch {
		case f.Type.Kind() == reflect.Slice && f.Type.Elem().Kind() == reflect.Struct:
			// A list of entries, journals: each entry's fields are named
			// under the list's key.
			schemaTags(f.Type.Elem(), key, out)
		case f.Type.Kind() == reflect.Struct && f.Type != reflect.TypeFor[entity.NodeID]() &&
			f.Type != reflect.TypeFor[time.Duration]():
			schemaTags(f.Type, key, out)
		}
	}
}

// tagParts splits a jsonschema tag into its enum values and whether it says
// required.
func tagParts(tag string) ([]string, bool) {
	var (
		enum     []string
		required bool
	)

	for part := range strings.SplitSeq(tag, ",") {
		if v, ok := strings.CutPrefix(part, "enum="); ok {
			enum = append(enum, v)
		}

		if part == "required" {
			required = true
		}
	}

	return enum, required
}

// The jsonschema tags dfs config schema reads say what the key table says:
// every enumerated key has its enumeration, no other key has one, and the
// keys required whatever another key holds are the journals and each
// entry's path, the keys block, each role's block and each role's kind.
func TestSchemaTagsMatchKeyTable(t *testing.T) {
	t.Parallel()

	tags := map[string]string{}
	schemaTags(reflect.TypeFor[Bootstrap](), "", tags)

	enums := map[string][]string{}

	var required []string

	for key, tag := range tags {
		enum, req := tagParts(tag)
		if enum != nil {
			enums[key] = enum
		}

		if req {
			required = append(required, key)
		}
	}

	for _, s := range keySpecs {
		if s.kind != kindEnum && s.kind != kindEnumList {
			continue
		}

		if !slices.Equal(enums[s.key], s.enum) {
			t.Errorf("%s: jsonschema enum %q, want %q (the key table)", s.key, enums[s.key], s.enum)
		}

		delete(enums, s.key)
	}

	for key, enum := range enums {
		t.Errorf("%s: jsonschema enum %q on a key the table gives no enumeration", key, enum)
	}

	slices.Sort(required)

	want := []string{
		"journals",
		"journals.path",
		"keys",
		"keys.protocol",
		"keys.protocol.kind",
		"keys.storage",
		"keys.storage.kind",
	}
	if !slices.Equal(required, want) {
		t.Errorf("jsonschema required: got %q, want %q", required, want)
	}
}
