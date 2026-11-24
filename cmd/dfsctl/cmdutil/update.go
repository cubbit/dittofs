package cmdutil

import (
	"bytes"
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

// getter reads a resource by its ID, as a collection does and as each
// resource's part of another does, such as a share's export policy.
type getter[S, T any] interface {
	Get(ctx context.Context, id string) (client.Tagged[S, T], error)
}

// Write writes a changed spec of the resource id names under etag, as its
// route takes it: the whole spec, or patch, the JSON merge patch of the
// spec's changes, each under the key "spec". It answers the resource, or
// the operation the write started, as a user's write may.
type Write[S, T any] func(ctx context.Context, id string, spec S, patch map[string]any, etag string) (client.Result[S, T], error)

// ByPatch is the Write of a route that takes PATCH and answers the resource.
func ByPatch[S, T any](r interface {
	Patch(ctx context.Context, id string, patch any, etag string) (client.Tagged[S, T], error)
}) Write[S, T] {
	return func(ctx context.Context, id string, _ S, patch map[string]any, etag string) (client.Result[S, T], error) {
		t, err := r.Patch(ctx, id, patch, etag)
		return client.Result[S, T]{Resource: &t}, err
	}
}

// ByPut is the Write of a route that takes only PUT, the whole spec, and
// answers the resource.
func ByPut[S, T any](r interface {
	Put(ctx context.Context, id string, spec S, etag string) (client.Tagged[S, T], error)
}) Write[S, T] {
	return func(ctx context.Context, id string, spec S, _ map[string]any, etag string) (client.Result[S, T], error) {
		t, err := r.Put(ctx, id, spec, etag)
		return client.Result[S, T]{Resource: &t}, err
	}
}

// Update reads the resource of r id names, a kind, applies change to its
// spec and, when change changed it, writes it with write under the ETag the
// read answered; a patching write sends only what change changed, so a field
// change empties is cleared on the server rather than left as it was. A
// change that changes nothing writes nothing and answers the resource as
// read. A write the server refuses 412, the resource having changed since
// the read, is not retried: Update reads the resource again and refuses,
// printing the difference between the spec it read and the one the server
// now holds, as a unified diff of the two as YAML.
func Update[S, T any](cmd *cobra.Command, env Env, r getter[S, T], write Write[S, T], kind, id string, change func(*S) error) (client.Result[S, T], error) {
	var zero client.Result[S, T]
	ctx, path := cmd.Context(), cmd.CommandPath()
	read, err := r.Get(ctx, id)
	if err != nil {
		return zero, fmt.Errorf("%s: reading %s %s: %w", path, kind, id, err)
	}
	// A snapshot before change runs, which may write through a slice or a
	// pointer the spec shares with the read.
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
	patch := mergePatch(was, is)
	if len(patch) == 0 {
		return client.Result[S, T]{Resource: &read}, nil
	}
	if env.afterRead != nil {
		env.afterRead()
	}
	written, err := write(ctx, id, spec, map[string]any{"spec": patch}, read.ETag)
	if err == nil {
		return written, nil
	}
	if p := (*apiv1.Problem)(nil); !errors.As(err, &p) || p.Status != http.StatusPreconditionFailed {
		return zero, fmt.Errorf("%s: writing %s %s: %w", path, kind, id, err)
	}
	now, err := r.Get(ctx, id)
	if err != nil {
		return zero, fmt.Errorf("%s: %s %s changed since it was read, and reading it again: %w", path, kind, id, err)
	}
	before, err := toYAML(was)
	if err != nil {
		return zero, err
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
		diff = "(no difference in the fields this dfsctl knows; another write since the read changed the resource)\n"
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
	// Numbers as written, so an integer past 2^53 is not rounded.
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var o map[string]any
	return o, d.Decode(&o)
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
