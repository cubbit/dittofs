package cmdutil

import (
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/cubbit/dittofs/pkg/node/apiv1"
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
// name may spell another resource's ID. A listed resource whose own name is
// not the one asked for is refused rather than counted, so a list that
// ignored or loosened the filter never resolves a name to another
// resource, which a delete would then remove.
func Resolve[S, T any](cmd *cobra.Command, r client.Resource[S, T], kind string, args []string) (string, error) {
	if id, _ := cmd.Flags().GetString("id"); id != "" {
		return id, nil
	}
	if len(args) != 1 {
		return "", fmt.Errorf("%s: name the %s by its name or by --id, exactly one", cmd.CommandPath(), kind)
	}
	return ByName(cmd, r, kind, args[0])
}

// ByName returns the ID of the one resource of r, a kind, that the list's
// name filter admits for name, refusing a name that matches nothing, or more
// than one, naming it.
func ByName[S, T any](cmd *cobra.Command, r client.Resource[S, T], kind, name string) (string, error) {
	var ids []string
	for res, err := range r.List(cmd.Context(), url.Values{"name": {name}}) {
		if err != nil {
			return "", fmt.Errorf("%s: finding the %s named %q: %w", cmd.CommandPath(), kind, name, err)
		}
		got, ok := nameOf(res.Spec)
		if !ok {
			return "", fmt.Errorf("%s: dfsctl cannot check the name of a %s; name it by --id", cmd.CommandPath(), kind)
		}
		if !sameName(got, name) {
			return "", fmt.Errorf("%s: asked for the %s named %q, the server listed %s named %q; refusing to act on it", cmd.CommandPath(), kind, name, res.ID, got)
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

// sameName reports whether the listed name got is the asked name, under
// strings.EqualFold; a name that is not UTF-8 must match byte for byte,
// since EqualFold reads each invalid byte as U+FFFD, joining names the
// server keys apart.
//
// decision: a listed name is compared with the asked one under
// strings.EqualFold, not under the server's own folds, which dfsctl does not
// link: a share's name is unique under an upcase table and a user's or a
// group's under a frozen simple case folding. Each pair either fold maps
// together EqualFold also holds equal, so
// the check refuses no name the server matched. Overturned if a fold ever
// maps a pair EqualFold keeps apart.
func sameName(got, name string) bool {
	if !utf8.ValidString(name) || !utf8.ValidString(got) {
		return got == name
	}
	return strings.EqualFold(got, name)
}

// nameOf is the name spec declares, for each kind Resolve resolves by name.
func nameOf(spec any) (string, bool) {
	switch s := spec.(type) {
	case apiv1.ShareSpec:
		return s.Name, true
	case apiv1.UserSpec:
		return s.Name, true
	case apiv1.GroupSpec:
		return s.Name, true
	}
	return "", false
}
