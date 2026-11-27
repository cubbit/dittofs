package syncer_test

import (
	"context"

	"github.com/cubbit/dittofs/pkg/remote"
	"github.com/cubbit/dittofs/pkg/remote/codec"
	"github.com/cubbit/dittofs/pkg/syncer"
)

// The compiler holds the seam between the syncer's contract and the remote
// tier's codec, declared in two packages: the codec's Store is the syncer's
// Store as it is, and a Put's Attempt is the attempt Upload runs. Either side's
// signature drifting from the other fails the build of this package's tests.
var (
	_ syncer.Store = (*codec.Store)(nil)
	_              = func(f syncer.Flow, p *codec.Put) error {
		return f.Upload(context.Background(), remote.Name{}, 0, p.Attempt)
	}
)
