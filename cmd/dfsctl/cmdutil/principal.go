package cmdutil

import (
	"encoding/json"
	"fmt"

	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
	"github.com/spf13/cobra"
)

// PrincipalFlags makes cmd name the principal it acts on by --user or
// --group, a local account's name, by --principal-id, its ID as a listing
// prints it, or, when directory is set, by --directory, a directory
// principal's qualified name, at most one of them. It returns the flags.
func PrincipalFlags(cmd *cobra.Command, directory bool) []string {
	cmd.Flags().String("user", "", "The user's name")
	cmd.Flags().String("group", "", "The group's name")
	cmd.Flags().String("principal-id", "", "The principal's ID, as a listing prints it")
	flags := []string{"user", "group", "principal-id"}
	if directory {
		cmd.Flags().String("directory", "", `A directory principal's qualified name, DOMAIN\name`)
		flags = append(flags, "directory")
	}
	cmd.MarkFlagsMutuallyExclusive(flags...)
	return flags
}

// Principal returns the ID of the principal cmd's flags name: a user's or a
// group's through the list's name filter, a directory principal's through
// resolve-principal, whose operation it follows to its end; "" when they
// name none. A flag given empty is refused, so an unset shell variable never
// turns a principal's quota into the share's own.
func Principal(cmd *cobra.Command, c *client.Client) (string, error) {
	path := cmd.CommandPath()
	for _, f := range []string{"user", "group", "principal-id", "directory"} {
		if v, err := cmd.Flags().GetString(f); err == nil && v == "" && cmd.Flags().Changed(f) {
			return "", fmt.Errorf("%s: --%s names no principal", path, f)
		}
	}
	if name, _ := cmd.Flags().GetString("user"); name != "" {
		return ByName(cmd, c.Users().Resource, "user", name)
	}
	if name, _ := cmd.Flags().GetString("group"); name != "" {
		return ByName(cmd, c.Groups(), "group", name)
	}
	if id, _ := cmd.Flags().GetString("principal-id"); id != "" {
		return id, nil
	}
	name, _ := cmd.Flags().GetString("directory")
	if name == "" {
		return "", nil
	}
	op, err := c.InstallationAction(cmd.Context(), "resolve-principal", apiv1.ResolvePrincipalParams{Name: name})
	if err != nil {
		return "", fmt.Errorf("%s: resolving %q: %w", path, name, err)
	}
	// decision: the resolution is waited on whatever --wait says, since what
	// the command does next needs its result, the principal's ID. An
	// interrupted wait leaves the resolution running and writes nothing more;
	// running the command again resolves the name again, which finds the
	// principal the first resolution minted. A resolution mints the principal
	// when it is new, so a grant remove by a directory name never granted
	// leaves a principal with no grant; --principal-id removes a grant
	// without resolving.
	ended, err := c.Wait(cmd.Context(), *op)
	if err != nil {
		return "", fmt.Errorf("%s: resolving %q, operation %s: %w", path, name, op.ID, err)
	}
	if err := endedErr(fmt.Sprintf("%s: resolving %q", path, name), ended); err != nil {
		return "", err
	}
	var p apiv1.Principal
	if err := json.Unmarshal(ended.Result, &p); err != nil || p.ID == "" {
		return "", fmt.Errorf("%s: resolving %q, operation %s answered %q, not a principal", path, name, ended.ID, ended.Result)
	}
	return p.ID, nil
}
