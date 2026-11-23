package context

import (
	"fmt"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/internal/cli/credentials"
	"github.com/spf13/cobra"
)

func newListCmd(env cmdutil.Env) *cobra.Command {
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List all configured contexts",
		Long: `List all configured server contexts stored in the local credential file.

Each row shows the context name, server URL, username, and login status. The active context is marked with an asterisk (*). Use this to identify which servers you have credentials for and which context is currently selected.

Examples:
  # List all contexts as a table
  dfsctl context list

  # List contexts as JSON for scripting
  dfsctl context list -o json`,
		RunE: func(cmd *cobra.Command, args []string) error { return runContextList(cmd, env, args) },
	}
	return listCmd
}

// ContextInfo represents context information for output.
type ContextInfo struct {
	Name      string `json:"name" yaml:"name"`
	Current   bool   `json:"current" yaml:"current"`
	ServerURL string `json:"server_url" yaml:"server_url"`
	Username  string `json:"username,omitempty" yaml:"username,omitempty"`
	LoggedIn  bool   `json:"logged_in" yaml:"logged_in"`
}

// ContextList is a list of contexts for table rendering.
type ContextList []ContextInfo

// Headers implements TableRenderer.
func (cl ContextList) Headers() []string {
	return []string{"", "NAME", "SERVER", "USER", "LOGGED IN"}
}

// Rows implements TableRenderer.
func (cl ContextList) Rows() [][]string {
	rows := make([][]string, 0, len(cl))
	for _, c := range cl {
		current := ""
		if c.Current {
			current = "*"
		}
		rows = append(rows, []string{current, c.Name, c.ServerURL, c.Username, cmdutil.BoolToYesNo(c.LoggedIn)})
	}
	return rows
}

func runContextList(cmd *cobra.Command, env cmdutil.Env, args []string) error {
	store, err := credentials.NewStoreAt(env.ConfigPath)
	if err != nil {
		return fmt.Errorf("failed to initialize credential store: %w", err)
	}

	contextNames := store.ListContexts()
	currentContext := store.GetCurrentContextName()

	contexts := make(ContextList, 0, len(contextNames))
	for _, name := range contextNames {
		ctx, err := store.GetContext(name)
		if err != nil {
			continue
		}

		info := ContextInfo{
			Name:      name,
			Current:   name == currentContext,
			ServerURL: ctx.ServerURL,
			Username:  ctx.Username,
			LoggedIn:  ctx.AccessToken != "",
		}
		contexts = append(contexts, info)
	}

	return cmdutil.PrintOutput(cmd, contexts, len(contexts) == 0, "No contexts configured. Use 'dfsctl login --server <url>' to create one.", contexts)
}
