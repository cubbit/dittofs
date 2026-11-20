package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"time"

	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/invopop/jsonschema"
	"github.com/spf13/cobra"
)

func newSchemaCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schema",
		Short: "Generate JSON schema for configuration",
		Long: `Generate a JSON schema for the DittoFS configuration file.

The schema covers each key's type, its allowed values and the keys required
whatever another key holds: journals with a path in each entry, and
keys.<role>.kind for both roles. It checks the file alone: a file whose
journals or keys come from DITTOFS_ variables fails its required keys, and
the rules that relate two keys are checked by 'dfs config validate'.

The schema can be used for:
  - IDE autocompletion (VS Code, IntelliJ, etc.)
  - Configuration file validation

Examples:
  # Print schema to stdout
  dfs config schema

  # Save schema to file
  dfs config schema --output config.schema.json`,
		RunE: runSchema,
	}
	cmd.Flags().StringP("output", "o", "", "Output file (default: stdout)")

	return cmd
}

func runSchema(cmd *cobra.Command, _ []string) error {
	path, _ := cmd.Flags().GetString("output")

	return writeSchema(path, cmd.OutOrStdout())
}

// writeSchema writes the schema to the file at path, saying so on out, or
// to out when path is empty; the bytes are the same either way.
func writeSchema(path string, out io.Writer) error {
	schemaJSON, err := buildSchema()
	if err != nil {
		return err
	}

	if path == "" {
		_, _ = out.Write(schemaJSON)

		return nil
	}

	if err := os.WriteFile(path, schemaJSON, 0o644); err != nil {
		return fmt.Errorf("failed to write schema file: %w", err)
	}

	_, _ = fmt.Fprintf(out, "JSON schema written to %s\n", path)

	return nil
}

// buildSchema is RFC 13 Appendix C's table as an indented JSON Schema,
// ending in a newline: the types as the file writes them, the
// enumerations, and the keys required whatever another key holds, from the
// jsonschema tags of Bootstrap's fields. It carries no default.
func buildSchema() ([]byte, error) {
	reflector := jsonschema.Reflector{
		AllowAdditionalProperties: false,
		DoNotReference:            true,
		// The fields carry yaml tags, not json ones: without this the
		// schema would name Go's field names, not the file's keys.
		FieldNameTag: "yaml",
		// A field is required only when its tag says so; by default every
		// field without omitempty would be.
		RequiredFromJSONSchemaTags: true,
		Mapper:                     fileType,
	}

	schema := reflector.Reflect(&bootstrap.Bootstrap{})
	schema.Version = "https://json-schema.org/draft/2020-12/schema"
	schema.Title = "DittoFS Configuration"
	schema.Description = "Configuration schema for DittoFS server"

	// The log rotation's size and age are integers in Bootstrap and strings
	// with their unit in the file ("100MiB", "720h").
	rotation := schemaProperty(schema, "logging", "rotation")
	for _, key := range []string{"max_size", "max_age"} {
		if p := schemaProperty(rotation, key); p != nil {
			p.Type = "string"
		}
	}

	schemaJSON, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to generate schema: %w", err)
	}

	return append(schemaJSON, '\n'), nil
}

// fileType is the schema of a Go type the file writes as a string: the node
// identity as a UUID, a duration such as "5s". Any other type is reflected.
func fileType(t reflect.Type) *jsonschema.Schema {
	switch t {
	case reflect.TypeFor[entity.NodeID]():
		return &jsonschema.Schema{
			Type:   "string",
			Format: "uuid",
		}
	case reflect.TypeFor[time.Duration]():
		return &jsonschema.Schema{Type: "string"}
	}

	return nil
}

// schemaProperty is the schema of the property under the path of names in s,
// nil when one is missing.
func schemaProperty(s *jsonschema.Schema, names ...string) *jsonschema.Schema {
	for _, name := range names {
		if s == nil || s.Properties == nil {
			return nil
		}

		s, _ = s.Properties.Get(name)
	}

	return s
}
