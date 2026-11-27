//go:build !foundationdb

package commands

import (
	"context"
	"errors"

	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
)

// foundationDB reports whether this build opens metadata.backend foundationdb.
const foundationDB = false

// openFoundationDB refuses: checkKV refuses the backend first, so no command
// reaches here.
func openFoundationDB(context.Context, *bootstrap.Bootstrap, systemClock) (kv.KV, error) {
	return nil, errors.New(noFoundationDB)
}
