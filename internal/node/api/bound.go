package api

import (
	"sync"

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// maxCalls is how many API calls a node runs at once.
const maxCalls = 64

var (
	errStarting = &refusal{code: apiv1.CodeStarting, detail: "the node is starting"}
	errBusy     = &refusal{code: apiv1.CodeUnavailable, detail: "the node runs as many API calls as it may"}
)

// enter admits a call on rt, nil for a path no row names, and returns what
// gives its call slot back, which does nothing once it has. While the node
// is starting, only a row that answers before ready, or a public one, is
// admitted. A public row takes no slot, so a full bound never refuses it;
// every other call takes one of 64 or is refused.
//
// ponytail: one node-wide bound of 64 calls, with no per-account share, so
// one account's script can hold every slot. Upgrade to a per-account share of
// the 64 when another account is refused while one account's script runs.
//
// decision: only the word starting refuses a call; draining and not_ready do
// not. A draining node's calls run to their answer and its listener stops
// taking new ones, and a node not ready after it was ready answers each call
// by what failed, a store unreachable among them. Overturned if a call
// answered on a node not ready can do harm a refusal would have prevented.
func (s *Server) enter(rt *route) (release func(), err error) {
	public := rt != nil && rt.right == auth.Public
	if !public && (rt == nil || !rt.beforeReady) && s.d.Ready() == "starting" {
		return nil, errStarting
	}
	if public {
		return func() {}, nil
	}
	select {
	case s.calls <- struct{}{}:
		return sync.OnceFunc(func() { <-s.calls }), nil
	default:
		return nil, errBusy
	}
}
