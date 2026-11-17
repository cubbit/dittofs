package apiclient

import (
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"net/url"

	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// Resource is one kind of resource of the API, at its collection's path.
type Resource[S, T any] struct {
	c    *Client
	path string
	// sub follows the ID in one resource's path, for a resource that is
	// part of another, such as a share's export policy.
	sub string
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

func (r Resource[S, T]) at(id string) string { return r.path + "/" + url.PathEscape(id) + r.sub }

// List yields every resource the filter admits, page by page; a filter's
// limit sets the page's size.
func (r Resource[S, T]) List(ctx context.Context, filter url.Values) iter.Seq2[apiv1.Resource[S, T], error] {
	return list[apiv1.Resource[S, T]](ctx, r.c, r.path, filter)
}

// Get reads the resource id names.
func (r Resource[S, T]) Get(ctx context.Context, id string) (Tagged[S, T], error) {
	return tagged[S, T](r.c.do(ctx, call{method: http.MethodGet, path: r.at(id)}))
}

// Create creates a resource of spec.
func (r Resource[S, T]) Create(ctx context.Context, spec S, opts ...CallOption) (Tagged[S, T], error) {
	return create[S, T](ctx, r.c, r.path, map[string]any{"spec": spec}, opts)
}

// Put replaces the spec of the resource id names, whose tag is etag; an
// etag of "*" creates it, and is refused if it exists.
func (r Resource[S, T]) Put(ctx context.Context, id string, spec S, etag string) (Tagged[S, T], error) {
	b, err := json.Marshal(map[string]any{"spec": spec})
	if err != nil {
		return Tagged[S, T]{}, err
	}
	return tagged[S, T](r.c.do(ctx, call{method: http.MethodPut, path: r.at(id), body: b, etag: etag}))
}

// Patch merges patch, a JSON merge patch of the resource's body, into the
// resource id names, whose tag is etag.
func (r Resource[S, T]) Patch(ctx context.Context, id string, patch any, etag string) (Tagged[S, T], error) {
	b, err := json.Marshal(patch)
	if err != nil {
		return Tagged[S, T]{}, err
	}
	return tagged[S, T](r.c.do(ctx, call{method: http.MethodPatch, path: r.at(id), body: b, etag: etag}))
}

// Delete deletes the resource id names, whose tag is etag, returning the
// operation the deletion started, nil when it finished in the call.
func (r Resource[S, T]) Delete(ctx context.Context, id, etag string) (*apiv1.Operation, error) {
	rp, err := r.c.do(ctx, call{method: http.MethodDelete, path: r.at(id), etag: etag})
	if err != nil || rp.status != http.StatusAccepted {
		return nil, err
	}
	var op apiv1.Operation
	return &op, json.Unmarshal(rp.body, &op)
}

// Action runs verb on the resource id names, with body, nil for none, and
// with If-Match when etag is set, as an action that changes the spec needs.
func (r Resource[S, T]) Action(ctx context.Context, id, verb string, body any, etag string, opts ...CallOption) (Result[S, T], error) {
	cl := call{method: http.MethodPost, path: r.at(id) + ":" + verb, etag: etag}
	if body != nil {
		var err error
		if cl.body, err = json.Marshal(body); err != nil {
			return Result[S, T]{}, err
		}
	}
	for _, o := range opts {
		o(&cl)
	}
	rp, err := r.c.do(ctx, cl)
	if err != nil {
		return Result[S, T]{}, err
	}
	if rp.status == http.StatusAccepted {
		var op apiv1.Operation
		return Result[S, T]{Operation: &op}, json.Unmarshal(rp.body, &op)
	}
	t, err := tagged[S, T](rp, nil)
	return Result[S, T]{Resource: &t}, err
}

// create POSTs body on the collection at path and returns what it created.
func create[S, T any](ctx context.Context, c *Client, path string, body any, opts []CallOption) (Tagged[S, T], error) {
	cl := call{method: http.MethodPost, path: path}
	var err error
	if cl.body, err = json.Marshal(body); err != nil {
		return Tagged[S, T]{}, err
	}
	for _, o := range opts {
		o(&cl)
	}
	return tagged[S, T](c.do(ctx, cl))
}

// tagged decodes rp as a resource with its ETag, ignoring any field this
// client does not know.
func tagged[S, T any](rp reply, err error) (Tagged[S, T], error) {
	if err != nil {
		return Tagged[S, T]{}, err
	}
	t := Tagged[S, T]{ETag: rp.header.Get("ETag")}
	return t, json.Unmarshal(rp.body, &t.Resource)
}

// get reads the body at path as a T.
func get[T any](ctx context.Context, c *Client, path string) (T, error) {
	var v T
	rp, err := c.do(ctx, call{method: http.MethodGet, path: path})
	if err != nil {
		return v, err
	}
	return v, json.Unmarshal(rp.body, &v)
}

// list yields every item of the listing at path the filter admits,
// following each page's cursor to the next.
func list[T any](ctx context.Context, c *Client, path string, filter url.Values) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		q := url.Values{}
		for k, v := range filter {
			q[k] = v
		}
		for {
			page, err := get[apiv1.Page[T]](ctx, c, path+"?"+q.Encode())
			if err != nil {
				var zero T
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
			q.Set("after", page.Next)
		}
	}
}
