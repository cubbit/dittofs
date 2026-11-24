package cmdutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"

	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
	"github.com/pmezard/go-difflib/difflib"
	"github.com/spf13/cobra"
)

// updatable is what Update reads and writes: a collection's resources, or
// each resource's part of another, such as a share's export policy.
type updatable[S, T any] interface {
	Get(ctx context.Context, id string) (client.Tagged[S, T], error)
	Patch(ctx context.Context, id string, patch any, etag string) (client.Tagged[S, T], error)
}

// Update reads the resource of r id names, a kind, applies change to its
// spec and writes what change changed, as a JSON merge patch of the spec,
// under the ETag the read answered, so a field change empties is cleared on
// the server rather than left as it was. A write the server refuses 412,
// the resource having changed since the read, is not retried: Update reads
// the resource again and refuses, printing the difference between the spec
// it read and the one the server now holds, as a unified diff of the two as
// YAML.
func Update[S, T any](cmd *cobra.Command, env Env, r updatable[S, T], kind, id string, change func(*S) error) (client.Tagged[S, T], error) {
	var zero client.Tagged[S, T]
	ctx, path := cmd.Context(), cmd.CommandPath()
	read, err := r.Get(ctx, id)
	if err != nil {
		return zero, fmt.Errorf("%s: reading %s %s: %w", path, kind, id, err)
	}
	// Encoded before change runs, which may write through a slice or a
	// pointer the spec shares with the read.
	before, err := toYAML(read.Spec)
	if err != nil {
		return zero, err
	}
	was, err := asObject(read.Spec)
	if err != nil {
		return zero, err
	}
	spec := read.Spec
	if err := change(&spec); err != nil {
		return zero, fmt.Errorf("%s: %w", path, err)
	}
	is, err := asObject(spec)
	if err != nil {
		return zero, err
	}
	if env.afterRead != nil {
		env.afterRead()
	}
	written, err := r.Patch(ctx, id, map[string]any{"spec": mergePatch(was, is)}, read.ETag)
	if p := (*apiv1.Problem)(nil); !errors.As(err, &p) || p.Status != http.StatusPreconditionFailed {
		if err != nil {
			return zero, fmt.Errorf("%s: writing %s %s: %w", path, kind, id, err)
		}
		return written, nil
	}
	now, err := r.Get(ctx, id)
	if err != nil {
		return zero, fmt.Errorf("%s: %s %s changed since it was read, and reading it again: %w", path, kind, id, err)
	}
	after, err := toYAML(now.Spec)
	if err != nil {
		return zero, err
	}
	diff, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A: difflib.SplitLines(string(before)), B: difflib.SplitLines(string(after)),
		FromFile: "as read", ToFile: "as the server holds it", Context: 3,
	})
	if err != nil {
		return zero, err
	}
	if diff == "" {
		diff = "(its spec is unchanged; another write since the read changed only its generation)\n"
	}
	return zero, fmt.Errorf("%s: %s %s changed since it was read, so nothing was written; run the command again to change it as it is now:\n%s",
		path, kind, id, diff)
}

// asObject is spec as the JSON object it encodes to.
func asObject(spec any) (map[string]any, error) {
	b, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	var o map[string]any
	return o, json.Unmarshal(b, &o)
}

// mergePatch is the JSON merge patch (RFC 7386) that turns the object a
// into b: each key of b whose value differs from a's, an object's patched
// recursively, and null for each key of a that b lacks.
func mergePatch(a, b map[string]any) map[string]any {
	p := map[string]any{}
	for k := range a {
		if _, ok := b[k]; !ok {
			p[k] = nil
		}
	}
	for k, bv := range b {
		av, ok := a[k]
		am, aObj := av.(map[string]any)
		bm, bObj := bv.(map[string]any)
		switch {
		case ok && aObj && bObj:
			if sub := mergePatch(am, bm); len(sub) > 0 {
				p[k] = sub
			}
		case !ok || !reflect.DeepEqual(av, bv):
			p[k] = bv
		}
	}
	return p
}
