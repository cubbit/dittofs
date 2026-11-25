package chain_test

import (
	"context"
	"log/slog"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/chain"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// The signatures the content wiring and every shipped transform are written
// against. A change to one fails this file's build before it fails theirs.
var (
	_ func(transform.ID, string, transform.Factory)                                                 = chain.Register
	_ func(context.Context, []transform.Config, keys.Provider, chain.Deps) (transform.Chain, error) = chain.NewChain
	_                                                                                               = chain.Deps{
		Logger:   (*slog.Logger)(nil),
		Recorder: chain.Recorder(nil),
		Require:  []string(nil),
		Encrypts: func(entity.NamespaceID) bool { return false },
	}
)
