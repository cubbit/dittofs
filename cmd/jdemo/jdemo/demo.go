package jdemo

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

// demoSettings is the story-default journal shape, used by the tests'
// handcrafted inits; each story carries its own settings through the
// registry — limits scales the ceiling, the rest keep this shape.
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
type Demo struct {
	out     io.Writer
	in      *bufio.Reader
	Dir     string
	StoryID string

	// Options are extra journal Options the open uses, appended after the
	// demo's own clock and settings. Tests pass the platform probe they
	// cannot choose on their host; a real run adds none.
	Options []journal.Option

	J   *journal.Journal
	H   map[journal.ShareTag]*journal.Handle
	Tag journal.ShareTag // the story's one working tag

	views   map[rune]func(*Demo)
	quit    bool
	Stashed journal.Version // the last mutating beat's issued version

	// lastView holds each view's last-shown rows: the diff layer's
	// snapshots. They survive beats on purpose — a look after a mutating
	// beat is where the diff is the point.
	lastView map[string]map[string]string
}

// story is the registered scenario this demo walks; the default is basic,
// overwritten by runMain's --story.
func (d *Demo) story() Story {
	if s, ok := stories[d.StoryID]; ok {
		return s
	}
	return stories["basic"]
}

// newDemo wires the driver: journal directory dir, keystrokes from in, the
// show on out.
func NewDemo(dir string, in io.Reader, out io.Writer) *Demo {
	d := &Demo{
		out: out, in: bufio.NewReader(in), Dir: dir,
		StoryID: "basic",
		H:       make(map[journal.ShareTag]*journal.Handle),
		Tag:     1,
	}
	d.views = map[rune]func(*Demo){
		'i': (*Demo).viewIndex,
		's': (*Demo).viewSegments,
		'd': (*Demo).viewDump,
		'f': (*Demo).viewFS,
		'+': (*Demo).viewStats,
	}
	return d
}

// run opens the journal, walks the story beat by beat, and closes it.
// An empty line advances, `q` or an ended stdin ends the story; either way
// the journal closes and run returns nil.
func (d *Demo) Run() error {
	ctx := context.Background()
	if err := d.open(ctx); err != nil {
		return err
	}
	defer func() { _ = d.J.Close() }()
	st := d.story()
	for n, b := range st.Beats {
		d.beat(n+1, len(st.Beats), b)
		if d.quit {
			break
		}
	}
	return nil
}

// open initialises or opens the journal exactly the way the CLI's daemon
// did, minus the socket: state file → open; fresh empty directory → init,
// record the identity at once, open; anything else refused.
func (d *Demo) open(ctx context.Context) error {
	dir := filepath.Clean(d.Dir)
	passed, err := loadState(StatePath(dir))
	opts := append([]journal.Option{
		journal.WithClock(realClock{}),
		journal.WithSettings(d.story().Settings),
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
			return fmt.Errorf("%s holds a journal but the state file %s is missing; refusing to initialise over it", dir, StatePath(dir))
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
		if err := saveState(StatePath(dir), stateFile{ID: id}); err != nil {
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
	if err := awaitSpares(ctx, j, d.story()); err != nil {
		_ = j.Close()
		return err
	}
	if err := saveState(StatePath(dir), stateFile{ID: id, Limit: limit.Counter}); err != nil {
		_ = j.Close()
		return err
	}
	d.J = j
	return nil
}

// awaitSpares waits until the journal's spare pool holds its settings'
// target. The pool's ready hook takes a type only the journal's tree can
// name, so Stats is the wait's eye on it — a poll of the real condition,
// and the deadline only the backstop.
func awaitSpares(ctx context.Context, j *journal.Journal, story Story) error {
	deadline := time.Now().Add(sparesDeadline)
	for {
		if st := j.Stats(); st.Spares >= story.Settings.SpareTarget {
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

// beat shows what it is about to do — where it stands in the story, its
// title, its caption, what the room is told to look for — waits for the
// presenter's Enter, runs it, confirms the watched facts with measured
// deltas, and then offers the views until an Enter moves the story on. An
// ended stdin still runs the beat — a piped, ended script deserves the
// full story — and then the story ends. A real terminal clears before
// EVERY beat, the first included, so one step fills the screen however
// much history the terminal held; a piped one draws a rule between beats.
func (d *Demo) beat(n, total int, b Beat) {
	c := coloursFor(d.out)
	if c.on {
		d.Printf("\x1b[2J\x1b[H")
	} else if n > 1 {
		d.Printf("──────────────────────────────────────────────\n\n")
	}
	if n == 1 {
		// The welcome rides the first beat's own clear, so the opening
		// screen is one frame: the story's shape, then its first step.
		d.Printf("%s: the journal, live — %d beats\n\n", c.dim("jdemo"), total)
	}
	d.Printf("%s STEP %d OF %d %s\n\n", c.dim("·"), n, total, c.dim("·"))
	d.Printf("%s\n%s\n", c.green(b.Title), c.dim(b.Caption))
	if len(b.Watch) > 0 {
		d.Printf("\n  %s ", c.dim("look for:"))
		for i, w := range b.Watch {
			if i > 0 {
				d.Printf(" %s ", c.dim("·"))
			}
			d.Printf("%s", c.dim(w.Hint))
		}
	}
	d.Printf("\n")
	if !d.waitEnter() {
		return
	}
	before := d.J.Stats()
	b.Run(d)
	if len(b.Watch) > 0 {
		after := d.J.Stats()
		d.Printf("\n")
		for _, w := range b.Watch {
			text := w.confirmed(before, after)
			if strings.HasPrefix(text, "✓") {
				d.Printf("  %s\n", c.green(text))
			} else {
				d.Printf("  %s\n", c.dim(text))
			}
		}
	}
	d.Printf("\n")
	if d.quit {
		return
	}
	d.promptLoop()
}

// promptLoop offers the views. An empty line is the next beat; `q` ends the
// story; anything else picks a view. An ended stdin ends the story.
func (d *Demo) promptLoop() {
	for !d.quit {
		d.Printf("(i) index   (s) segments   (f) filesystem   (d) data dump   (+) stats   ⏎ next / q quit\n")
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
				d.Printf("<<view wired later>>\n")
			}
		}
	}
}

// waitEnter blocks until the presenter's Enter. An ended stdin runs the
// beat anyway and ends the story after; `q` skips the beat and ends it.
func (d *Demo) waitEnter() bool {
	d.Printf("⏎  run ")
	line, ok := d.readLine()
	if !ok {
		if !d.quit {
			d.quit = true
			d.Printf("run\n")
		}
		return true
	}
	if line == "q" {
		d.Printf("quit\n")
		d.quit = true
		return false
	}
	return true
}

// readLine reads one keystroke line; false answers when stdin is done.
func (d *Demo) readLine() (string, bool) {
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
func (d *Demo) Printf(format string, args ...any) {
	_, _ = fmt.Fprintf(d.out, format, args...)
}

func firstRune(s string) rune {
	for _, r := range s {
		return r
	}
	return 0
}

// codef prints a source-style call in the code colour, set apart from
// the narration around it: on a real terminal the journal's API reads as
// a different voice than the caption prose.
func (d *Demo) Codef(format string, args ...any) {
	c := d.colours()
	_, _ = io.WriteString(d.out, c.code(fmt.Sprintf(format, args...))+"\n")
}

// yellowf prints a result line — the journal's answer — in the colour
// the room learns to read as "what happened".
func (d *Demo) Yellowf(format string, args ...any) {
	c := d.colours()
	_, _ = io.WriteString(d.out, c.yellow(fmt.Sprintf(format, args...))+"\n")
}

// colours is the demo's palette, computed per render from the writer.
func (d *Demo) colours() colour { return coloursFor(d.out) }
