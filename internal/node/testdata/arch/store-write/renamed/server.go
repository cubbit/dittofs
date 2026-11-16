package api

import "context"

// Deps holds a store write behind an interface method of another name.
type Deps struct {
	Store interface {
		Forget(ctx context.Context, name string) error
	}
}
