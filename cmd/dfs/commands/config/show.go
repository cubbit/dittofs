package config

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/cubbit/dittofs/internal/cli/output"
	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func newShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Display current configuration",
		Long: `Display the current DittoFS configuration.

By default outputs YAML format. Use --output to change format.

Examples:
  # Show default config as YAML
  dfs config show

  # Show as JSON
  dfs config show --output json

  # Show specific config file
  dfs config show --config /etc/dittofs/config.yaml`,
		RunE: runConfigShow,
	}
	cmd.Flags().StringP("output", "o", "yaml", "Output format (yaml|json)")
	return cmd
}

func runConfigShow(cmd *cobra.Command, args []string) error {
	// Get config path from parent's persistent flag
	configPath, _ := cmd.Flags().GetString("config")

	// Load configuration
	cfg, err := bootstrap.MustLoad(configPath, os.Environ())
	if err != nil {
		return err
	}

	// Parse output format
	f, _ := cmd.Flags().GetString("output")
	format, err := output.ParseFormat(f)
	if err != nil {
		return err
	}

	return printConfig(os.Stdout, cfg, format)
}

// printConfig writes cfg to w in format, as show prints it.
func printConfig(w io.Writer, cfg *bootstrap.Bootstrap, format output.Format) error {
	switch format {
	case output.FormatJSON:
		// The Config structs carry only mapstructure/yaml tags (no json
		// tags), so a direct json.Marshal emits PascalCase keys that
		// bootstrap.Load cannot re-parse. Round-trip through yaml first to
		// obtain the lowercase yaml-keyed shape (which also applies the
		// secret-redaction MarshalYAML hooks), then emit that as JSON so
		// the output re-parses through Load.
		keyed, err := yamlKeyedView(cfg)
		if err != nil {
			return err
		}

		return output.PrintJSON(w, keyed)
	default:
		view, err := configView(cfg)
		if err != nil {
			return err
		}

		return output.PrintYAML(w, view)
	}
}

// yamlKeyedView converts cfg to a generic map keyed by its yaml tags by
// marshalling to YAML and unmarshalling back into an interface{}. This yields
// the same lowercase key namespace bootstrap.Load consumes, so the JSON encoding
// of the result round-trips through Load.
func yamlKeyedView(cfg *bootstrap.Bootstrap) (interface{}, error) {
	doc, err := configView(cfg)
	if err != nil {
		return nil, err
	}
	var view interface{}
	if err := doc.Decode(&view); err != nil {
		return nil, fmt.Errorf("failed to re-key config: %w", err)
	}
	return view, nil
}

// configView is cfg as the YAML tree show prints, with node.id in the UUID
// form the file takes rather than the 16 numbers of its array. The store's
// own encoding of the identity is left as it is.
func configView(cfg *bootstrap.Bootstrap) (*yaml.Node, error) {
	var doc yaml.Node
	if err := doc.Encode(cfg); err != nil {
		return nil, fmt.Errorf("failed to marshal config: %w", err)
	}

	id := mappingValue(mappingValue(&doc, "node"), "id")
	if id == nil {
		return nil, errors.New("failed to marshal config: no node.id")
	}

	*id = yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   "!!str",
		Value: uuid.UUID(cfg.Node.ID).String(),
	}

	return &doc, nil
}

// mappingValue is the value under key in the YAML mapping n, or nil.
func mappingValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}

	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}

	return nil
}
