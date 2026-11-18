package config

import (
	"encoding/json"
	"testing"

	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/invopop/jsonschema"
)

// expectedTopLevelYAMLKeys mirrors the yaml/mapstructure tags on the top-level
// bootstrap.Bootstrap fields. If a field is added/removed there this test fails
// loudly, which is the point: the schema must track the real YAML namespace.
var expectedTopLevelYAMLKeys = []string{
	"logging",
	"blockstore",
	"node",
	"metadata",
	"journals",
	"adapters",
	"api",
	"metrics",
	"keys",
}

// TestConfigSchema_TopLevelKeysAreYAMLKeys asserts the generated JSON schema's
// top-level property keys equal the lowercase yaml keys (not Go PascalCase
// field names), so an IDE/validator using the schema accepts a real
// config.yaml.
func TestConfigSchema_TopLevelKeysAreYAMLKeys(t *testing.T) {
	reflector := jsonschema.Reflector{
		AllowAdditionalProperties: false,
		DoNotReference:            true,
		FieldNameTag:              "yaml",
	}
	schema := reflector.Reflect(&bootstrap.Bootstrap{})

	raw, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	var decoded struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}

	for _, key := range expectedTopLevelYAMLKeys {
		if _, ok := decoded.Properties[key]; !ok {
			t.Errorf(
				"schema missing expected lowercase yaml key %q; got keys %v",
				key,
				keysOf(decoded.Properties),
			)
		}
	}
	for got := range decoded.Properties {
		// PascalCase Go field names must not leak into the schema.
		if got != "" && got[0] >= 'A' && got[0] <= 'Z' {
			t.Errorf("schema emitted PascalCase key %q (should be the lowercase yaml key)", got)
		}
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
