// Package setting implements the setting commands for dfsctl: any setting
// at any scope, read, set and reset by its key.
package setting

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
	"github.com/spf13/cobra"
)

// settings is the settings at the scope arg names: installation, or
// node:<id>, namespace:<id>, store:<id> or share:<name>, a share named by
// its name, resolved to its ID.
func settings(cmd *cobra.Command, c *client.Client, arg string) (client.SettingResource, error) {
	if arg == "installation" {
		return c.Settings(client.ScopeInstallation, ""), nil
	}
	kind, id, _ := strings.Cut(arg, ":")
	scope, ok := map[string]client.Scope{
		"node": client.ScopeNode, "namespace": client.ScopeNamespace, "store": client.ScopeStore, "share": client.ScopeShare,
	}[kind]
	if !ok || id == "" {
		return client.SettingResource{}, fmt.Errorf("%s: scope %q: want installation, node:<id>, namespace:<id>, store:<id> or share:<name>", cmd.CommandPath(), arg)
	}
	if scope == client.ScopeShare {
		var err error
		if id, err = cmdutil.Resolve(cmd, c.Shares(), "share", []string{id}); err != nil {
			return client.SettingResource{}, err
		}
	}
	return c.Settings(scope, id), nil
}

// table is a setting as a table.
func table(s apiv1.Setting) cmdutil.Table {
	applied := ""
	if a := s.Status.Applied; a != nil {
		applied = fmt.Sprintf("%d/%d", a.Nodes, a.Of)
	}
	return cmdutil.Table{{"KEY", "VALUE", "APPLIED"}, {s.ID, string(s.Spec.Value), applied}}
}

// New returns the setting command, which reaches every setting by its key,
// so a new setting needs no change here.
func New(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "setting",
		Short: "Read and change settings",
		Long: `Read, set and reset any setting by its scope and key.

The scope is installation, or node:<id>, namespace:<id>, store:<id> or
share:<name>. A value that is JSON, such as 8080, true or {"a":1}, is taken as
it is, and any other as a string; quote a string that reads as JSON: '"true"'.
Unsetting a setting removes it at that scope, so the wider scope's value, or
the default, holds.

Examples:
  # Read the installation's session lifetime
  dfsctl setting get installation api.session_lifetime

  # Bind one node's NFS adapter to another port
  dfsctl setting set node:7f3a9c1e-0b2d-4e5f-8a6b-1c2d3e4f5a6b adapters.nfs.port 2050

  # Reset it to the installation's value
  dfsctl setting unset node:7f3a9c1e-0b2d-4e5f-8a6b-1c2d3e4f5a6b adapters.nfs.port`,
	}
	get := &cobra.Command{
		Use:   "get <scope> <key>",
		Short: "Show a setting",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			r, err := settings(cmd, c, args[0])
			if err != nil {
				return err
			}
			s, err := r.Get(cmd.Context(), args[1])
			if err != nil {
				return fmt.Errorf("%s: setting %s at %s: %w", cmd.CommandPath(), args[1], args[0], err)
			}
			return cmdutil.PrintOutput(cmd, s.Resource, false, "", table(s.Resource))
		},
	}
	set := &cobra.Command{
		Use:   "set <scope> <key> <value>",
		Short: "Set a setting",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			r, err := settings(cmd, c, args[0])
			if err != nil {
				return err
			}
			key, value := args[1], cmdutil.JSONValue(args[2])
			var res client.Result[apiv1.SettingSpec, apiv1.SettingStatus]
			_, err = r.Get(cmd.Context(), key)
			if p := (*apiv1.Problem)(nil); errors.As(err, &p) && p.Status == http.StatusNotFound {
				// Not set at this scope: create it, refused if it is set since.
				res, err = r.Put(cmd.Context(), key, apiv1.SettingSpec{Value: value}, "*")
			}
			if err != nil {
				return fmt.Errorf("%s: setting %s at %s: %w", cmd.CommandPath(), key, args[0], err)
			}
			if res.Operation == nil && res.Resource == nil {
				res, err = cmdutil.Update(cmd, env, r, func(ctx context.Context, key string, spec apiv1.SettingSpec, _ map[string]any, etag string) (client.Result[apiv1.SettingSpec, apiv1.SettingStatus], error) {
					return r.Put(ctx, key, spec, etag)
				}, "setting", key, func(s *apiv1.SettingSpec) error {
					s.Value = value
					return nil
				})
				if err != nil {
					return err
				}
			}
			if res.Operation != nil {
				return cmdutil.Follow(cmd, c, res.Operation)
			}
			return cmdutil.PrintOutput(cmd, res.Resource.Resource, false, "", table(res.Resource.Resource))
		},
	}
	cmdutil.AddWait(set)
	unset := &cobra.Command{
		Use:   "unset <scope> <key>",
		Short: "Reset a setting to the wider scope's value or the default",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			r, err := settings(cmd, c, args[0])
			if err != nil {
				return err
			}
			s, err := r.Get(cmd.Context(), args[1])
			if err != nil {
				return fmt.Errorf("%s: setting %s at %s: %w", cmd.CommandPath(), args[1], args[0], err)
			}
			op, err := r.Delete(cmd.Context(), args[1], s.ETag)
			if err != nil {
				return fmt.Errorf("%s: setting %s at %s: %w", cmd.CommandPath(), args[1], args[0], err)
			}
			return cmdutil.Follow(cmd, c, op)
		},
	}
	cmdutil.AddWait(unset)
	cmd.AddCommand(get, set, unset)
	return cmd
}
