package cmdutil

import (
	"context"
	"fmt"

	"github.com/cubbit/dittofs/internal/cli/credentials"
)

// Locked runs fn on the credentials file env.ConfigPath names, under its
// lock (credentials.Update), saying once on env.Err that it waits while
// another dfsctl holds the lock.
func Locked(ctx context.Context, env Env, fn func(*credentials.Store) error) error {
	return credentials.Update(ctx, env.ConfigPath, func(lock string) {
		_, _ = fmt.Fprintf(env.Err, "waiting for %s, which another dfsctl holds\n", lock)
	}, fn)
}
