package syncer_test

import (
	"github.com/cubbit/dittofs/pkg/remote/codec"
	"github.com/cubbit/dittofs/pkg/syncer"
)

// The compiler holds the seam between the syncer's contract and the remote
// tier's codec, declared in two packages: the codec's Store is the syncer's
// Store, and a Put's Attempt has exactly the type of Upload's attempt. Either
// side's signature drifting from the other fails the build of this package's
// tests. A method the syncer's Store drops still compiles here: the contract's
// own surface is pinned by its exports, not by this seam.
var (
	_ syncer.Store = (*codec.Store)(nil)
	_              = func() {
		attempt := (*codec.Put)(nil).Attempt
		same(attemptOf(syncer.Flow.Upload), &attempt)
	}
)

// attemptOf returns a nil pointer typed as Upload's attempt parameter.
func attemptOf[F, C, N, S, A any](func(F, C, N, S, A) error) *A { return nil }

// same compiles only when both pointers have one type, so only when the types
// they point to are identical, not merely assignable.
func same[T any](*T, *T) {}
