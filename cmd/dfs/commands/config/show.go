package config

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

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
		Long: `Display the bootstrap a start would read, and where each value came from.

Prints the file read (empty when none was), the state and runtime directories
with the variable that chose each, or "default" when none did, the socket
path, the bootstrap's keys, and a sources map naming each key's source:
"file", the DITTOFS_ variable that set it, or "default". A value set by a
variable prints as the variable's name, never as its value, and the socket's
path is then empty. A credential prints as its file's path. The output is a
report, not a file a start can read.

The file is --config, else the one DITTOFS_CONFIG names, else the default.
show checks only what a load checks: run 'dfs config validate' for the rest.

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

func runConfigShow(cmd *cobra.Command, _ []string) error {
	configPath, _ := cmd.Flags().GetString("config")

	f, _ := cmd.Flags().GetString("output")

	format, err := output.ParseFormat(f)
	if err != nil {
		return err
	}

	return runShow(configPath, os.Environ(), format, cmd.OutOrStdout())
}

// runShow loads the bootstrap as a start does, from configPath and environ,
// and prints it to w in format; a refusal is returned and nothing printed.
func runShow(configPath string, environ []string, format output.Format, w io.Writer) error {
	loaded, err := bootstrap.LoadWithSources(configPath, environ)
	if err != nil {
		return err
	}

	return printConfig(w, loaded, format)
}

// printConfig writes loaded to w in format, as show prints it.
func printConfig(w io.Writer, loaded *bootstrap.Loaded, format output.Format) error {
	view, err := showView(loaded)
	if err != nil {
		return err
	}

	if format != output.FormatJSON {
		return output.PrintYAML(w, view)
	}

	var tree any
	if err := view.Decode(&tree); err != nil {
		return fmt.Errorf("failed to convert config: %w", err)
	}

	return output.PrintJSON(w, tree)
}

// fromEnvironment reports whether a key's source is a variable: a source
// that is neither the file nor the default is the variable's name. An empty
// source, a bootstrap shown with no load behind it, is no variable.
func fromEnvironment(s bootstrap.Source) bool {
	return s != "" && s != "file" && s != "default"
}

// showView is what show prints: the file read, the two directories, the
// socket, the bootstrap's tree and the sources of its keys. A value a
// variable set is the variable's name, never its value (RFC 13 Appendix C,
// "The file's commands").
//
// decision: the two directories print as paths, with the variable that
// chose each as their source. They are host facts, not keys, and a path is
// what an operator needs to find the node's state; no refusal prints them.
// The socket is a key: a variable that sets it hides its path.
func showView(loaded *bootstrap.Loaded) (*yaml.Node, error) {
	tree, err := configView(loaded)
	if err != nil {
		return nil, err
	}

	socket := loaded.API.Socket
	if fromEnvironment(loaded.Source["api.socket"]) {
		socket = ""
	}

	dirs := &yaml.Node{Kind: yaml.MappingNode}

	addPair(dirs, "state", place(loaded.StateDir, loaded.StateVar))
	addPair(dirs, "runtime", place(loaded.RuntimeDir, loaded.RuntimeVar))

	root := &yaml.Node{Kind: yaml.MappingNode}

	addPair(root, "file", scalar(loaded.File))
	addPair(root, "directories", dirs)
	addPair(root, "socket", place(socket, string(loaded.Source["api.socket"])))

	root.Content = append(root.Content, tree.Content...)

	sources := &yaml.Node{Kind: yaml.MappingNode}

	for _, key := range slices.Sorted(maps.Keys(loaded.Source)) {
		addPair(sources, key, scalar(string(loaded.Source[key])))
	}

	addPair(root, "sources", sources)

	return root, nil
}

// configView is the bootstrap as the YAML tree show prints: node.id in the
// UUID form the file takes rather than the 16 numbers of its array, the log
// rotation's size and age with their units, as the file writes them, and
// each value a variable set replaced by the variable's name.
func configView(loaded *bootstrap.Loaded) (*yaml.Node, error) {
	cfg := &loaded.Bootstrap

	var doc yaml.Node
	if err := doc.Encode(cfg); err != nil {
		return nil, fmt.Errorf("failed to marshal config: %w", err)
	}

	for key, value := range map[string]string{
		"node.id":                   uuid.UUID(cfg.Node.ID).String(),
		"logging.rotation.max_size": strconv.Itoa(cfg.Logging.Rotation.MaxSize) + "MiB",
		"logging.rotation.max_age":  strconv.Itoa(cfg.Logging.Rotation.MaxAge*24) + "h",
	} {
		if err := replace(&doc, key, value); err != nil {
			return nil, err
		}
	}

	for key, s := range loaded.Source {
		if !fromEnvironment(s) {
			continue
		}

		if err := replace(&doc, key, string(s)); err != nil {
			return nil, err
		}
	}

	return &doc, nil
}

// replace sets the value under the dotted key of the mapping doc to the
// string value.
func replace(doc *yaml.Node, key, value string) error {
	n := doc

	for part := range strings.SplitSeq(key, ".") {
		n = mappingValue(n, part)
	}

	if n == nil {
		return errors.New("failed to marshal config: no " + key)
	}

	*n = *scalar(value)

	return nil
}

// scalar is a YAML string holding v.
func scalar(v string) *yaml.Node {
	return &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   "!!str",
		Value: v,
	}
}

// place is a path with its source, as show prints a directory and the
// socket.
func place(path, source string) *yaml.Node {
	m := &yaml.Node{Kind: yaml.MappingNode}

	addPair(m, "path", scalar(path))
	addPair(m, "source", scalar(source))

	return m
}

// addPair appends key and value to the mapping m.
func addPair(m *yaml.Node, key string, value *yaml.Node) {
	m.Content = append(m.Content, scalar(key), value)
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
