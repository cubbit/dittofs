package journal

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/cubbit/dittofs/pkg/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/journal/internal/recovery"
)

// ReadFormat returns the identity of the journal in dir and the installation
// that created it, read from its format file, without opening the journal.
// It takes the steps Open takes before it judges the identity, but the check
// of the directory's platform, which guards writes and is the open's to make,
// and answers with the same errors: ErrNoJournal for a directory that is missing or holds
// neither a format file nor a segment, ErrUnidentified for segments without a
// format file, ErrUnknownFormat and ErrCorrupt for a format file this binary
// cannot use. It writes nothing.
//
// The read goes through the retry bound every read at open gets, timed by the
// system clock: it is one bounded read, and no journal runs on that clock.
func ReadFormat(ctx context.Context, dir string) (JournalID, InstallationID, error) {
	// decision: platform.CheckDir is not applied. It guards writes and is the
	// open's to make, and this read writes nothing. Withdraw it if a caller ever
	// takes this read as proof the directory can be written.
	cfg, err := resolveConfig(dir, nil)
	if err != nil {
		return JournalID{}, InstallationID{}, err
	}

	if err := ctx.Err(); err != nil {
		return JournalID{}, InstallationID{}, err
	}

	names, err := cfg.Storage.List()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return JournalID{}, InstallationID{}, fmt.Errorf("%w: %s", ErrNoJournal, dir)
		}

		return JournalID{}, InstallationID{}, err
	}

	hasFormat, hasSegment := classifyListing(names)
	switch {
	case !hasFormat && !hasSegment:
		return JournalID{}, InstallationID{}, fmt.Errorf(
			"%w: %s holds neither a format file nor a segment",
			ErrNoJournal,
			dir,
		)
	case !hasFormat:
		return JournalID{}, InstallationID{}, fmt.Errorf(
			"%w: %s holds a segment and no format file",
			ErrUnidentified,
			dir,
		)
	}

	ff, err := recovery.ReadFormat(ctx, recovery.Deps{
		FS:     cfg.Storage,
		Clock:  systemClock{origin: time.Now()},
		Tracer: cfg.Tracer,
		Log:    cfg.Logger,
	}, JournalID{})
	switch {
	case err == nil:
	case errors.Is(err, format.ErrUnknownVersion):
		return JournalID{}, InstallationID{}, fmt.Errorf(
			"%w: the format file names a version this binary does not read: %v",
			ErrUnknownFormat,
			err,
		)
	case errors.Is(err, format.ErrDamaged):
		return JournalID{}, InstallationID{}, fmt.Errorf(
			"%w: the format file is damaged: %v",
			ErrCorrupt,
			err,
		)
	default:
		return JournalID{}, InstallationID{}, err
	}

	return ff.Identity, ff.Installation, nil
}

// systemClock is the operating system's clock, for ReadFormat's one read.
type systemClock struct {
	origin time.Time
}

func (systemClock) Now() time.Time {
	return time.Now()
}

func (c systemClock) Elapsed() time.Duration {
	return time.Since(c.origin)
}

// ponytail: the timer After starts cannot be cancelled, so it lives to its
// deadline even when its caller stopped waiting. Add a stoppable After to the
// clock interface if loop re-arms show in a heap profile.
func (systemClock) After(d time.Duration) <-chan struct{} {
	done := make(chan struct{})

	time.AfterFunc(d, func() {
		close(done)
	})

	return done
}
