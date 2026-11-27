package v3

import (
	"context"

	"github.com/cubbit/dittofs/internal/adapter/core"
)

// void is NULL's argument and result: nothing (RFC 5531 section 12.1).
type void struct{}

// null answers NULL: no call to the service, no result. A row that calls the
// service comes in its own file with its own arguments, never this one.
func null(context.Context, *core.Request, void) (void, error) {
	return void{}, nil
}
