package journal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/platform/platformtest"
)

func TestReadFormatReturnsTheIDs(t *testing.T) {
	dir := t.TempDir()
	install := InstallationID{'r', 'e', 'a', 'd'}

	id, err := Init(
		context.Background(),
		dir,
		install,
		WithMountProbe(platformtest.SuitableGather(dir)),
	)
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	gotID, gotInstall, err := ReadFormat(context.Background(), dir)
	if err != nil {
		t.Fatalf("ReadFormat: %v", err)
	}

	if gotID != id {
		t.Errorf("journal ID: got %x, want %x", gotID, id)
	}

	if gotInstall != install {
		t.Errorf("installation ID: got %x, want %x", gotInstall, install)
	}
}

func TestReadFormatRefusesWhatIsNoJournal(t *testing.T) {
	flipped := func(off int) func(t *testing.T) string {
		return func(t *testing.T) string {
			return flippedFormat(t, off)
		}
	}

	for _, tc := range []struct {
		name string
		dir  func(t *testing.T) string
		want error
	}{
		{"a missing directory", func(t *testing.T) string {
			return filepath.Join(t.TempDir(), "gone")
		}, ErrNoJournal},
		{"an empty directory", func(t *testing.T) string {
			return t.TempDir()
		}, ErrNoJournal},
		{"a segment and no format file", func(t *testing.T) string {
			dir := t.TempDir()

			segment := filepath.Join(dir, format.SegmentName(1))
			if err := os.WriteFile(segment, nil, 0o644); err != nil {
				t.Fatal(err)
			}

			return dir
		}, ErrUnidentified},
		{"a flipped bit in the format file", flipped(20), ErrCorrupt},
		{"an unknown version in the format file", flipped(4), ErrUnknownFormat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.dir(t)

			id, install, err := ReadFormat(context.Background(), dir)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ReadFormat(%s): got %v, want %v", tc.name, err, tc.want)
			}

			if id != (JournalID{}) || install != (InstallationID{}) {
				t.Errorf(
					"ReadFormat(%s) with an error: got IDs %x and %x, want zero",
					tc.name,
					id,
					install,
				)
			}
		})
	}
}

// flippedFormat makes a journal directory whose format file has one bit
// flipped in the byte at off.
func flippedFormat(t *testing.T, off int) string {
	t.Helper()

	dir := t.TempDir()

	_, err := Init(
		context.Background(),
		dir,
		InstallationID{'f'},
		WithMountProbe(platformtest.SuitableGather(dir)),
	)
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	path := filepath.Join(dir, format.FormatFileName)

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	b[off] ^= 0x01
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}

	return dir
}

func TestReadFormatAnswersACancelledContext(t *testing.T) {
	dir := t.TempDir()

	_, err := Init(
		context.Background(),
		dir,
		InstallationID{'c'},
		WithMountProbe(platformtest.SuitableGather(dir)),
	)
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, err := ReadFormat(ctx, dir); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadFormat with a cancelled context: got %v, want %v", err, context.Canceled)
	}
}

func TestSystemClock(t *testing.T) {
	c := systemClock{origin: time.Now()}

	<-c.After(0)

	if d := c.Elapsed(); d < 0 {
		t.Errorf("Elapsed: got %v, want at least 0", d)
	}

	if c.Now().IsZero() {
		t.Error("Now: got the zero time")
	}
}
