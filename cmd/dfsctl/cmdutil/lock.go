package cmdutil

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/cubbit/dittofs/internal/cli/credentials"
)

// lockPoll is how long a command waiting on the lock file pauses before it
// tries to take it again.
const lockPoll = 50 * time.Millisecond

// Locked runs fn on the credentials file env.ConfigPath names, read after
// taking the lock file beside it, <ConfigPath>.lock, which it holds until fn
// returns. Every change to the file goes through it, so two commands never
// read the same refresh token and both present it. While another command
// holds the lock it waits, saying once on env.Err that it does, until the
// lock is free or ctx ends.
//
// decision: the lock is a file created O_CREATE|O_EXCL, the same on every OS
// and blocking no reader; a lock left by a killed process is never broken,
// so a command waits on it until a person removes it, which the waiting line
// names. Withdraw it for a lease if stale locks bite.
func Locked(ctx context.Context, env Env, fn func(*credentials.Store) error) error {
	lock := env.ConfigPath + ".lock"
	if err := os.MkdirAll(filepath.Dir(lock), credentials.DirPermissions); err != nil {
		return err
	}
	for told := false; ; told = true {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, credentials.FilePermissions)
		if err == nil {
			_ = f.Close()
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		if !told {
			_, _ = fmt.Fprintf(env.Err, "waiting for %s; if no dfsctl is running, remove it\n", lock)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s: %w", lock, ctx.Err())
		case <-time.After(lockPoll):
		}
	}
	defer func() { _ = os.Remove(lock) }()
	s, err := credentials.NewStoreAt(env.ConfigPath)
	if err != nil {
		return err
	}
	return fn(s)
}
