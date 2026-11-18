package node

import (
	"context"

	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// drop calls a store write, which is allowed, and hands one out as a value,
// which is not.
func drop(ctx context.Context, cp kvstore.ControlPlane) func(context.Context, string, uint64) error {
	_ = cp.DeleteNetgroup(ctx, "staff", 0)
	return cp.DeleteNetgroup
}
