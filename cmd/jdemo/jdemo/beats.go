package jdemo

import (
	"encoding/hex"
	"fmt"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

// StoryFile is the one file the stories work on, spelled once: the bytes
// "journal-demo-fil" (16), shown everywhere as their hex.
var StoryFile = func() journal.FileID {
	var raw [16]byte
	copy(raw[:], "journal-demo-fil")
	return journal.NewFileID(raw)
}()

// StoryFileHex is StoryFile as the 32 hex chars every view prints.
func hexOf(id journal.FileID) string {
	b := id.Bytes()
	return hex.EncodeToString(b[:])
}

var StoryFileHex = hexOf(StoryFile)

// beat is one step of a story: a title, the caption the presenter narrates,
// the facts the room is told to look for before the beat runs, and the work
// the beat does once the presenter's Enter lands.
type Beat struct {
	Title   string
	Caption string
	// mutates marks a beat whose effect the views should re-diff.
	Mutates bool
	// Watch names the facts this beat is expected to move — printed as
	// hints before the Enter, confirmed with measured deltas after.
	Watch []Watching
	// Run is the beat's work, handed the running demo.
	Run func(*Demo)
}

// Watching is one fact a beat moves, with the prose the presenter narrates.
type Watching struct {
	Kind WatchKind
	Hint string
}

// WatchKind is a Stats field a beat asks to be measured across its run.
type WatchKind uint8

const (
	WatchHeld WatchKind = iota
	WatchDirty
	WatchFiles
	WatchEntries
	WatchSegments
	WatchSealed
	WatchSpares
	WatchMarkers
	WatchVersions
)

func (k WatchKind) of(st journal.Stats) int64 {
	switch k {
	case WatchHeld:
		return st.HeldBytes
	case WatchDirty:
		return st.DirtyBytes
	case WatchFiles:
		return int64(st.Files)
	case WatchEntries:
		return st.ExtentCount
	case WatchSegments:
		return int64(st.Segments)
	case WatchSealed:
		return int64(st.Sealed)
	case WatchSpares:
		return int64(st.Spares)
	case WatchMarkers:
		return int64(st.RemovalMarkers)
	case WatchVersions:
		return int64(st.VersionsLeft)
	}
	return 0
}

// stories holds a story by name; the basic one is the session sheet's.
// A second story is another entry in this map, not another binary.
// stash records the version the last mutating beat drew, for the beats that
// name it back (settle names the truncate's version; offload a through).
func (d *Demo) Stash(v journal.Version, err error) {
	if err != nil {
		d.Yellowf("    // → %v", err)
		return
	}
	d.Stashed = v
	d.Yellowf("    // → v%d", v.Counter)
}

// ChangeLine renders one change the way Since prints it: kind, extent,
// version, and a write's mtime when it carries one.
func ChangeLine(ch journal.Change) string {
	line := fmt.Sprintf("%s off=%d len=%d v%d", changeKindWord(ch.Kind), ch.Extent.Off, ch.Extent.Len, ch.Version.Counter)
	if ch.Mtime != 0 {
		line += fmt.Sprintf(" mtime=%d", ch.Mtime)
	}
	return line
}

func changeKindWord(k journal.ChangeKind) string {
	switch k {
	case journal.ChangeWrite:
		return "write"
	case journal.ChangeTruncate:
		return "truncate"
	case journal.ChangeDeallocate:
		return "deallocate"
	case journal.ChangeDelete:
		return "delete"
	case journal.ChangeCloneTarget:
		return "clone"
	case journal.ChangeLoss:
		return "loss"
	}
	return fmt.Sprintf("kind%d", uint8(k))
}

// confirmed renders one watched fact after the beat ran: the journal's own
// measurement of what moved, so the confirmation can never claim something
// that did not happen. A watched value that stayed put says so plainly.
func (w Watching) confirmed(before, after journal.Stats) string {
	var delta int64
	if w.Kind == WatchVersions {
		// Versions left counts DOWN what the window still holds: one
		// drawn version shows as −1 in the line's terms.
		delta = int64(after.VersionsLeft) - int64(before.VersionsLeft)
	} else {
		delta = w.Kind.of(after) - w.Kind.of(before)
	}
	if delta == 0 {
		return "· " + w.Hint + " — unchanged"
	}
	name, value := "", ""
	switch w.Kind {
	case WatchHeld:
		name, value = "held", signedBytes(delta)
	case WatchDirty:
		name, value = "dirty", signedBytes(delta)
	case WatchFiles:
		name, value = "files", signedCount(delta)
	case WatchEntries:
		name, value = "index", signedCount(delta)+" entry"
		if delta != 1 {
			value += "ies"
		}
	case WatchSegments:
		name, value = "segments", signedCount(delta)
	case WatchSealed:
		name, value = "sealed", signedCount(delta)
	case WatchSpares:
		name, value = "spares", signedCount(delta)
	case WatchMarkers:
		name, value = "removal markers", signedCount(delta)
	case WatchVersions:
		name, value = "versions", signedCount(-delta)
	}
	return "✓ " + name + " " + value
}

func signedCount(delta int64) string {
	if delta > 0 {
		return "+" + fmt.Sprint(delta)
	}
	return fmt.Sprint(delta)
}

func signedBytes(delta int64) string {
	if delta > 0 {
		return "+" + humanSize(delta)
	}
	return "-" + humanSize(-delta)
}

// story is one registered scenario: its beats and the journal settings
// it walks in.
// Story is one registered scenario: its beats and the journal settings it
// walks in.
type Story struct {
	Beats    []Beat
	Settings journal.Settings
}

// Stories holds every registered scenario by name. A second story is one
// registration.
var stories = map[string]Story{}

// Stories is the registry's read side: main's --story lookups.
func Stories() map[string]Story { return stories }

// SortedStoryNames lists what exists, the unknown-story message's list.
func SortedStoryNames() []string {
	names := make([]string, 0, len(stories))
	for name := range stories {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// RegisterStory names one scenario; scenarios registers each of its
// stories once, at its own init.
func RegisterStory(name string, fn func() ([]Beat, journal.Settings)) {
	beats, settings := fn()
	stories[name] = Story{Beats: beats, Settings: settings}
}
