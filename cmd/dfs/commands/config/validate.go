package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/spf13/cobra"
)

func newValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Validate configuration file",
		Long: `Validate the DittoFS configuration file.

Checks for syntax errors, missing required fields, and invalid values.

Examples:
  # Validate default config
  dfs config validate

  # Validate specific config file
  dfs config validate --config /etc/dittofs/config.yaml`,
		RunE: runConfigValidate,
	}
}

func runConfigValidate(cmd *cobra.Command, _ []string) error {
	configPath, _ := cmd.Flags().GetString("config")
	out := cmd.OutOrStdout()

	cfg, err := bootstrap.MustLoad(configPath, os.Environ())
	if err != nil {
		return err
	}

	displayPath := configPath
	if displayPath == "" {
		displayPath = bootstrap.GetDefaultConfigPath()
	}

	var warnings []string

	if legacyWarnings := checkLegacyPayloadKey(displayPath); len(legacyWarnings) > 0 {
		warnings = append(warnings, legacyWarnings...)
	}

	_, _ = fmt.Fprintf(out, "Configuration file: %s\n", displayPath)
	if len(warnings) > 0 {
		// Do not print a bare "OK" when warnings are present: a warning
		// means the config loads but carries a key the server ignores.
		// Surfacing the count keeps `config validate` honest for an
		// operator eyeballing the output before `dfs start`.
		_, _ = fmt.Fprintf(out, "Validation: OK (with %d warning(s))\n", len(warnings))
		_, _ = fmt.Fprintln(out, "\nWarnings:")
		for _, w := range warnings {
			_, _ = fmt.Fprintf(out, "  - %s\n", w)
		}
	} else {
		_, _ = fmt.Fprintln(out, "Validation: OK")
	}

	_, _ = fmt.Fprintf(out, "\nConfiguration summary:\n")
	_, _ = fmt.Fprintf(out, "  Log level:       %s\n", cfg.Logging.Level)
	_, _ = fmt.Fprintf(out, "  Journal path:    %s\n", cfg.Blockstore.Journal.Path)

	return nil
}

// checkLegacyPayloadKey scans a config file for the deprecated 'payload:' YAML key.
// Returns warnings if legacy keys are found.
func checkLegacyPayloadKey(configPath string) []string {
	f, err := os.Open(configPath)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	var warnings []string
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		// Detect top-level or nested 'payload:' or 'payload_store:' keys
		if strings.HasPrefix(trimmed, "payload:") {
			warnings = append(warnings, fmt.Sprintf(
				"Line %d: Config key 'payload' has been renamed to 'block_store'. "+
					"Please update your config file. See docs/guide/configuration.md for the new format.",
				lineNum))
		} else if strings.HasPrefix(trimmed, "payload_store:") {
			warnings = append(warnings, fmt.Sprintf(
				"Line %d: Config key 'payload_store' is no longer used. "+
					"Shares are now managed via dfsctl, and each one names its block store "+
					"with --block-store. See docs/guide/configuration.md for the new format.",
				lineNum))
		}
	}
	return warnings
}
