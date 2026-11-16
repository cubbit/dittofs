package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

// demoSettings keep the journal small on a laptop: two streams, two spares,
// a small headroom, and a bounded footprint. The maximum sits above the
// spare pool's targets by a wide margin — the open resolves the pool's
// targets and reserves them against the maximum, and one below the targets
// leaves the pool permanently short, refusing every write.
var demoSettings = journal.Settings{
	Streams:         2,
	SpareTarget:     2,
	HeadroomRecords: 1024,
	MaxBytes:        4 << 30,
}

// versionWindow is how far each start raises the version limit past the
// open's floor, mirroring what a production caller records durably before
// raising its limit. A talk spends at most a few thousand of it.
const versionWindow uint64 = 1 << 32

// demoInstallID is the fixed installation ID every demo-initialised journal
// carries; the demo is one installation, so a constant is honest.
var demoInstallID = func() (id journal.InstallationID) {
	copy(id[:], "jdemo-install")
	return id
}()

// sparesDeadline bounds how long the open waits for the pool's spares; a
// generous backstop over a condition polled on, not slept through.
const sparesDeadline = 30 * time.Second

// demo is the interactive driver: one journal it owns in its own process,
// a scripted story of beats walked at the presenter's Enter, and the views
// the presenter picks between beats.
type demo struct {
	out     io.Writer
	in      *bufio.Reader
	dir     string
	storyID string

	// Options are extra journal Options the open uses, appended after the
	// demo's own clock and settings. Tests pass the platform probe they
	// cannot choose on their host; a real run adds none.
	Options []journal.Option

	j   *journal.Journal
	h   map[journal.ShareTag]*journal.Handle
	tag journal.ShareTag // the story's one working tag

	views map[rune]func(*demo)
	quit  bool
}

// newDemo wires the driver: journal directory dir, keystrokes from in, the
// show on out.
func newDemo(dir string, in io.Reader, out io.Writer) *demo {
	d := &demo{
		out: out, in: bufio.NewReader(in), dir: dir,
		storyID: "basic",
		h:       make(map[journal.ShareTag]*journal.Handle),
		tag:     1,
	}
	d.views = map[rune]func(*demo){
		'i': viewPlaceholder,
		's': viewPlaceholder,
		'f': viewPlaceholder,
		'd': viewPlaceholder,
		'+': viewPlaceholder,
	}
	return d
}

func viewPlaceholder(*demo) {}

// run opens the journal, walks the story beat by beat, and closes it.
// An empty line advances, `q` or an ended stdin ends the story; either way
// the journal closes and run returns nil.
func (d *demo) run() error {
	ctx := context.Background()
	if err := d.open(ctx); err != nil {
		return err
	}
	defer d.j.Close()
	d.out.Write([]byte("\n--- jdemo: the journal, live ---\n\n"))
	for _, b := range stories[d.storyID] {
		d.beat(b)
		if d.quit {
			break
		}
	}
	return nil
}

// open initialises or opens the journal exactly the way the CLI's daemon
// did, minus the socket: state file → open; fresh empty directory → init,
// record the identity at once, open; anything else refused.
func (d *demo) open(ctx context.Context) error {
	dir := filepath.Clean(d.dir)
	passed, err := loadState(statePath(dir))
	opts := append([]journal.Option{
		journal.WithClock(realClock{}),
		journal.WithSettings(demoSettings),
	}, d.Options...)

	var (
		j     *journal.Journal
		id    journal.JournalID
		floor uint64
	)
	switch {
	case err == nil:
		id = passed.ID
		j, err = journal.Open(ctx, dir, journal.Version{Counter: passed.Limit}, id, nil, opts...)
		if err != nil {
			return fmt.Errorf("open %s: %w", dir, err)
		}
		floor = passed.Limit

	case errors.Is(err, fs.ErrNotExist):
		empty, lerr := dirEmpty(dir)
		if lerr != nil {
			return fmt.Errorf("%s: %w", dir, lerr)
		}
		if !empty {
			return fmt.Errorf("%s holds a journal but the state file %s is missing; refusing to initialise over it", dir, statePath(dir))
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("%s: %w", dir, err)
		}
		id, err = journal.Init(ctx, dir, demoInstallID, opts...)
		if err != nil {
			return fmt.Errorf("init %s: %w", dir, err)
		}
		// The identity is durable the moment it exists: a later refusal or
		// a ctrl-c during the open leaves a journal Open can name again.
		if err := saveState(statePath(dir), stateFile{ID: id}); err != nil {
			return err
		}
		j, err = journal.Open(ctx, dir, journal.Version{}, id, nil, opts...)
		if err != nil {
			return fmt.Errorf("open fresh %s: %w", dir, err)
		}

	default:
		return fmt.Errorf("refusing to start, %w", err)
	}

	limit := journal.Version{Counter: floor + versionWindow}
	if err := j.SetVersionLimit(limit); err != nil {
		_ = j.Close()
		return fmt.Errorf("setting version limit: %w", err)
	}
	// The pool makes its spares asynchronously once the open returns, and a
	// write racing them answers "refused by the spare limit"; the demo's
	// first write is at the talk, so the open waits them out.
	if err := awaitSpares(ctx, j); err != nil {
		_ = j.Close()
		return err
	}
	if err := saveState(statePath(dir), stateFile{ID: id, Limit: limit.Counter}); err != nil {
		_ = j.Close()
		return err
	}
	d.j = j
	return nil
}

// awaitSpares waits until the journal's spare pool holds its settings'
// target. The pool's ready hook takes a type only the journal's tree can
// name, so Stats is the wait's eye on it — a poll of the real condition,
// and the deadline only the backstop.
func awaitSpares(ctx context.Context, j *journal.Journal) error {
	deadline := time.Now().Add(sparesDeadline)
	for {
		if st := j.Stats(); st.Spares >= demoSettings.SpareTarget {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("spare pool not ready after %s (Spares=%d)", sparesDeadline, j.Stats().Spares)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// dirEmpty answers whether dir holds no entry, existing or not: a directory
// that does not exist is one Init may create.
//
// decision: the demo initialises only where it can see the directory to be
// empty, rather than trusting Init to refuse a directory holding a journal,
// so it can never initialise over data it failed to recognise.
func dirEmpty(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	return len(entries) == 0, nil
}

// beat shows what it is about to do, waits for the presenter's Enter, runs
// it, and then offers the views until an Enter moves the story on. An ended
// stdin still runs the beat — a piped, ended script deserves the full story
// — and then the story ends.
func (d *demo) beat(b beat) {
	d.printf("%s\n%s\n\n", b.title, b.caption)
	if d.waitEnter() {
		b.run(d)
	}
	d.printf("\n")
	if d.quit {
		return
	}
	d.promptLoop()
}

// promptLoop offers the views. An empty line is the next beat; `q` ends the
// story; anything else picks a view. An ended stdin ends the story.
func (d *demo) promptLoop() {
	for !d.quit {
		d.printf("(i) index   (s) segments   (f) filesystem   (d) data dump   (+) stats   ⏎ next / q quit\n")
		line, ok := d.readLine()
		if !ok {
			d.quit = true
			return
		}
		switch line {
		case "":
			return
		case "q":
			d.quit = true
			return
		default:
			if v, ok := d.views[firstRune(line)]; ok && v != nil {
				v(d)
			} else {
				d.printf("<<view wired later>>\n")
			}
		}
	}
}

// waitEnter blocks until the presenter's Enter. An ended stdin runs the
// beat anyway and ends the story after; `q` skips the beat and ends it.
func (d *demo) waitEnter() bool {
	d.printf("⏎  run ")
	line, ok := d.readLine()
	if !ok {
		if !d.quit {
			d.quit = true
			d.printf("run\n")
		}
		return true
	}
	if line == "q" {
		d.printf("quit\n")
		d.quit = true
		return false
	}
	return true
}

// readLine reads one keystroke line; false answers when stdin is done.
func (d *demo) readLine() (string, bool) {
	line, err := d.in.ReadString('\n')
	if err != nil && line == "" {
		return "", false
	}
	if err == io.EOF && line != "" && !strings.HasSuffix(line, "\n") {
		return strings.TrimSpace(line), true
	}
	return strings.TrimSpace(line), true
}

// printf is the one writer every screen line goes through; a pane that
// fails to write has nowhere to report it.
func (d *demo) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(d.out, format, args...)
}

func firstRune(s string) rune {
	for _, r := range s {
		return r
	}
	return 0
}
