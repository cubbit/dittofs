package cmdutil

import (
	"encoding/json"
	"fmt"

	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
	"github.com/spf13/cobra"
)

// PrincipalFlags makes cmd name the principal it acts on by --user or
// --group, a local account's name, or, when directory is set, by
// --directory, a directory principal's qualified name, at most one of them.
func PrincipalFlags(cmd *cobra.Command, directory bool) {
	cmd.Flags().String("user", "", "The user's name")
	cmd.Flags().String("group", "", "The group's name")
	flags := []string{"user", "group"}
	if directory {
		cmd.Flags().String("directory", "", `A directory principal's qualified name, DOMAIN\name`)
		flags = append(flags, "directory")
	}
	cmd.MarkFlagsMutuallyExclusive(flags...)
}

// Principal returns the ID of the principal cmd's flags name: a user's or a
// group's through the list's name filter, a directory principal's through
// resolve-principal, whose operation it follows to its end; "" when they
// name none.
func Principal(cmd *cobra.Command, c *client.Client) (string, error) {
	if name, _ := cmd.Flags().GetString("user"); name != "" {
		return ByName(cmd, c.Users().Resource, "user", name)
	}
	if name, _ := cmd.Flags().GetString("group"); name != "" {
		return ByName(cmd, c.Groups(), "group", name)
	}
	name, _ := cmd.Flags().GetString("directory")
	if name == "" {
		return "", nil
	}
	path := cmd.CommandPath()
	op, err := c.InstallationAction(cmd.Context(), "resolve-principal", apiv1.ResolvePrincipalParams{Name: name})
	if err != nil {
		return "", fmt.Errorf("%s: resolving %q: %w", path, name, err)
	}
	// decision: the resolution is waited on whatever --wait says, since what
	// the command does next needs its result, the principal's ID. An
	// interrupted wait leaves the resolution running and writes nothing more;
	// running the command again resolves the name again, which finds the
	// principal the first resolution minted.
	ended, err := c.Wait(cmd.Context(), *op)
	if err != nil {
		return "", fmt.Errorf("%s: resolving %q, operation %s: %w", path, name, op.ID, err)
	}
	if ended.State != apiv1.OpSucceeded {
		if ended.Error != nil {
			return "", fmt.Errorf("%s: resolving %q, operation %s %s: %w", path, name, ended.ID, ended.State, ended.Error)
		}
		return "", fmt.Errorf("%s: resolving %q, operation %s %s", path, name, ended.ID, ended.State)
	}
	var p apiv1.Principal
	if err := json.Unmarshal(ended.Result, &p); err != nil || p.ID == "" {
		return "", fmt.Errorf("%s: resolving %q, operation %s answered %q, not a principal", path, name, ended.ID, ended.Result)
	}
	return p.ID, nil
}
