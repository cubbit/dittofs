package client

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"net/url"

	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// Resource is one kind of resource of the API, at its collection's path.
type Resource[S, T any] struct {
	c    *Client
	path string
	// keyless is a create whose route carries or issues a credential, a
	// store's, so it goes with no idempotency key.
	keyless bool
}

// Singleton is a resource that is part of another, one per resource of its
// parent's collection at path, at sub under that resource's own path, such
// as a share's export policy. It is created and deleted with its parent, so
// it is read and written only.
type Singleton[S any] struct {
	c         *Client
	path, sub string
}

// Tagged is a resource as one read or write answered it, with the ETag the
// answer carried, which a later write of it echoes, opaque, as If-Match.
type Tagged[S, T any] struct {
	apiv1.Resource[S, T]
	ETag string
}

// Result is what an action answered: the operation it started, or the
// resource it changed.
type Result[S, T any] struct {
	Operation *apiv1.Operation
	Resource  *Tagged[S, T]
}

func (r Resource[S, T]) at(id string) string { return r.path + "/" + url.PathEscape(id) }

func (r Singleton[S]) at(id string) string { return r.path + "/" + url.PathEscape(id) + r.sub }

// List yields every resource the filter admits, page by page; a filter's
// limit sets the page's size.
func (r Resource[S, T]) List(ctx context.Context, filter url.Values) iter.Seq2[apiv1.Resource[S, T], error] {
	return list[apiv1.Resource[S, T]](ctx, r.c, r.path, filter)
}

// Get reads the resource id names.
func (r Resource[S, T]) Get(ctx context.Context, id string) (Tagged[S, T], error) {
	return tagged[S, T](r.c.do(ctx, call{method: http.MethodGet, path: r.at(id)}))
}

// Create creates a resource of spec; a keyless resource, a store's, goes
// with no idempotency key, as its spec carries its credential.
func (r Resource[S, T]) Create(ctx context.Context, spec S, opts ...CallOption) (Tagged[S, T], error) {
	if r.keyless {
		opts = append(opts, WithoutIdempotencyKey())
	}
	return tagged[S, T](r.c.write(ctx, http.MethodPost, r.path, map[string]any{"spec": spec}, "", opts))
}

// Put replaces the spec of the resource id names, whose tag is etag, or,
// with etag "*", creates it, refused if it exists.
func (r Resource[S, T]) Put(ctx context.Context, id string, spec S, etag string) (Tagged[S, T], error) {
	return tagged[S, T](r.c.write(ctx, http.MethodPut, r.at(id), map[string]any{"spec": spec}, etag, nil))
}

// Patch merges patch, a JSON merge patch of the resource's body, into the
// resource id names, whose tag is etag.
func (r Resource[S, T]) Patch(ctx context.Context, id string, patch any, etag string) (Tagged[S, T], error) {
	return tagged[S, T](r.c.write(ctx, http.MethodPatch, r.at(id), patch, etag, nil))
}

// Delete deletes the resource id names, whose tag is etag, returning the
// operation the deletion started, nil when it finished in the call.
func (r Resource[S, T]) Delete(ctx context.Context, id, etag string, opts ...CallOption) (*apiv1.Operation, error) {
	rp, err := r.c.write(ctx, http.MethodDelete, r.at(id), nil, etag, opts)
	if err != nil {
		return nil, err
	}
	return accepted(rp)
}

// Action runs verb on the resource id names, with body, nil for none, and
// with If-Match when etag is set, as an action that changes the spec needs.
func (r Resource[S, T]) Action(ctx context.Context, id, verb string, body any, etag string, opts ...CallOption) (Result[S, T], error) {
	return result[S, T](r.c.write(ctx, http.MethodPost, r.at(id)+":"+verb, body, etag, opts))
}

// Get reads the resource of the parent id names.
func (r Singleton[S]) Get(ctx context.Context, id string) (Tagged[S, struct{}], error) {
	return tagged[S, struct{}](r.c.do(ctx, call{method: http.MethodGet, path: r.at(id)}))
}

// Put replaces the spec of the resource of the parent id names, whose tag
// is etag.
func (r Singleton[S]) Put(ctx context.Context, id string, spec S, etag string) (Tagged[S, struct{}], error) {
	return tagged[S, struct{}](r.c.write(ctx, http.MethodPut, r.at(id), map[string]any{"spec": spec}, etag, nil))
}

// Patch merges patch, a JSON merge patch of the resource's body, into the
// resource of the parent id names, whose tag is etag.
func (r Singleton[S]) Patch(ctx context.Context, id string, patch any, etag string) (Tagged[S, struct{}], error) {
	return tagged[S, struct{}](r.c.write(ctx, http.MethodPatch, r.at(id), patch, etag, nil))
}

// write sends body, as JSON, nil for none, to path with method under etag,
// configured by opts.
func (c *Client) write(ctx context.Context, method, path string, body any, etag string, opts []CallOption) (reply, error) {
	cl := call{method: method, path: path, etag: etag}
	if body != nil {
		var err error
		if cl.body, err = json.Marshal(body); err != nil {
			return reply{}, err
		}
	}
	for _, o := range opts {
		o(&cl)
	}
	return c.do(ctx, cl)
}

// accepted is the operation rp carries when it is a 202, and nil for any
// other answer.
func accepted(rp reply) (*apiv1.Operation, error) {
	if rp.status != http.StatusAccepted {
		return nil, nil
	}
	var op apiv1.Operation
	return &op, json.Unmarshal(rp.body, &op)
}

// result decodes rp as the operation it started, when it is a 202, or as
// the resource it changed.
func result[S, T any](rp reply, err error) (Result[S, T], error) {
	if err != nil {
		return Result[S, T]{}, err
	}
	if op, err := accepted(rp); op != nil || err != nil {
		return Result[S, T]{Operation: op}, err
	}
	t, err := tagged[S, T](rp, nil)
	return Result[S, T]{Resource: &t}, err
}

// tagged decodes rp as a resource with its ETag, ignoring any field this
// client does not know.
func tagged[S, T any](rp reply, err error) (Tagged[S, T], error) {
	if err != nil {
		return Tagged[S, T]{}, err
	}
	t := Tagged[S, T]{ETag: rp.header.Get("ETag")}
	err = json.Unmarshal(rp.body, &t.Resource)
	return t, err
}

// get reads the body at path as a T.
func get[T any](ctx context.Context, c *Client, path string) (T, error) {
	var v T
	rp, err := c.do(ctx, call{method: http.MethodGet, path: path})
	if err != nil {
		return v, err
	}
	err = json.Unmarshal(rp.body, &v)
	return v, err
}

// list yields every item of the listing at path the filter admits,
// following each page's cursor to the next. A page naming a cursor the
// listing has already followed ends it with an error, since following it
// would read the same pages again forever.
func list[T any](ctx context.Context, c *Client, path string, filter url.Values) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		q := url.Values{}
		for k, v := range filter {
			q[k] = v
		}
		seen := map[string]bool{q.Get("after"): true}
		for {
			page, err := get[apiv1.Page[T]](ctx, c, path+"?"+q.Encode())
			if err != nil {
				yield(zero, err)
				return
			}
			for _, it := range page.Items {
				if !yield(it, nil) {
					return
				}
			}
			if page.Next == "" {
				return
			}
			if seen[page.Next] {
				yield(zero, fmt.Errorf("client: the listing at %s repeated its cursor %q", path, page.Next))
				return
			}
			seen[page.Next] = true
			q.Set("after", page.Next)
		}
	}
}
