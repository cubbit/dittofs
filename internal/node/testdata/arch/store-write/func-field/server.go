package api

import "context"

// Deps holds a store write as a function.
type Deps struct {
	Drop func(ctx context.Context, name string, expect uint64) error
}
