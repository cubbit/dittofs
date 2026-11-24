package cmdutil

import (
	"fmt"
	"net/url"

	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
	"github.com/spf13/cobra"
)

// NameOrID makes cmd take the resource it acts on, a kind, by a positional
// name or by --id, exactly one of them.
func NameOrID(cmd *cobra.Command, kind string) {
	cmd.Flags().String("id", "", "The "+kind+"'s ID, in place of its name")
	cmd.Args = func(cmd *cobra.Command, args []string) error {
		id, _ := cmd.Flags().GetString("id")
		if len(args) > 1 || (id == "") == (len(args) == 0) {
			return fmt.Errorf("%s: name the %s by its name or by --id, exactly one", cmd.CommandPath(), kind)
		}
		return nil
	}
}

// Resolve returns the ID of the resource of r that cmd names, a kind: the
// value of --id as it is, or the ID of the one resource the list's name
// filter admits for the positional name. A name that matches nothing is
// refused naming it; an ID that names nothing is refused by the call that
// uses it. A positional argument is always a name, never an ID, since a
// name may spell another resource's ID.
func Resolve[S, T any](cmd *cobra.Command, r client.Resource[S, T], kind string, args []string) (string, error) {
	if id, _ := cmd.Flags().GetString("id"); id != "" {
		return id, nil
	}
	if len(args) != 1 {
		return "", fmt.Errorf("%s: name the %s by its name or by --id, exactly one", cmd.CommandPath(), kind)
	}
	name := args[0]
	var ids []string
	for res, err := range r.List(cmd.Context(), url.Values{"name": {name}}) {
		if err != nil {
			return "", fmt.Errorf("%s: finding the %s named %q: %w", cmd.CommandPath(), kind, name, err)
		}
		ids = append(ids, res.ID)
	}
	switch len(ids) {
	case 0:
		return "", fmt.Errorf("%s: no %s named %q", cmd.CommandPath(), kind, name)
	case 1:
		return ids[0], nil
	}
	return "", fmt.Errorf("%s: %d %ss named %q, want one", cmd.CommandPath(), len(ids), kind, name)
}
