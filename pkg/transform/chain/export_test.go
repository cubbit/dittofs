package chain

import (
	"context"

	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// Registry is a registry of a test's own, so no test registers in the
// process's.
type Registry struct{ r *registry }

// NewRegistry returns an empty registry.
func NewRegistry() Registry { return Registry{newRegistry()} }

// Register is Register over this registry.
func (r Registry) Register(id transform.ID, name string, f transform.Factory) {
	r.r.register(id, name, f)
}

// MaxDecodeLen builds this registry's decode set for provider m and returns
// its bound of n bytes, or the build's error.
func (r Registry) MaxDecodeLen(ctx context.Context, m keys.Provider, n int) (int, error) {
	s, err := r.r.build(ctx, m)
	if err != nil {
		return 0, err
	}
	return s.maxDecodeLen(n), nil
}
