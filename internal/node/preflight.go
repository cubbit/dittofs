// What a start refuses to serve over: a start over absent or contradictory
// storage names what it found instead of creating anything. decide turns the
// facts the start's probes gathered into that one verdict; the probes that
// gather them land in this file alongside it.
package node

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
)

// What the probe of the metadata store's directory found.
type storeState int

const (
	storeAbsent         storeState = iota // no store is there
	storePresent                          // a store of this installation is there
	storeUnidentifiable                   // something is there that is not a readable store
)

// What the probe of one device's journal directory found: errors that say the
// journal belongs to an installation, and anything else Open reported.
type deviceFacts struct {
	Dir      string
	OpenErr  error
	Segments bool
}

// Everything the start's probes gathered about the node's storage, from which
// decide answers.
type startFacts struct {
	// V1 names a journal of the old layout, anywhere the start looks.
	V1 string
	// Store is the metadata store's state; StoreErr carries its probe's error
	// when the state is unidentifiable, nil otherwise.
	Store    storeState
	StoreErr error
	StoreDir string
	Devices  []deviceFacts
}

// The verdicts a start answers, one per refusal, so callers can match them.
var (
	errV1State         = errors.New("the node's journals are in v1 state")
	errUnidentifiable  = errors.New("the node's storage cannot be identified")
	errDevicesDisagree = errors.New("the devices name different installations")
	errStoreLost       = errors.New("the metadata store is lost")
	errNotInitialised  = errors.New("the node is not initialised")
)

// The wiring errors, answered before any storage is read: a start whose own
// caller misconfigured it names the missing piece, never a storage verdict,
// which over storage it never looked at would mislead.
var (
	errNoClock = errors.New("the start check's clock is missing")
	// errNoDevice answers a start whose bootstrap names no journal device.
	errNoDevice   = errors.New("no journal device: the bootstrap names none, and a node keeps its journals on a device of its own")
	errNoStoreDir = errors.New("the store's directory is not set")
)

// manifestName is Badger's MANIFEST, the mark of a store Open created.
const manifestName = "MANIFEST"

// probeV1State returns the path of the first pre-journal (v1) local layout
// under root: a shares/<name> directory holding a populated journal, blobs or
// logs directory. "" is none. Listing errors read as none: the store and
// device probes name their errors, and a root nothing can list is refused
// there.
func probeV1State(root string) string {
	shares, err := os.ReadDir(filepath.Join(root, "shares"))
	if err != nil {
		return ""
	}
	for _, share := range shares {
		for _, leaf := range [...]string{"journal", "blobs", "logs"} {
			dir := filepath.Join(root, "shares", share.Name(), leaf)
			if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
				return dir
			}
		}
	}
	return ""
}

// probeStore stats the store directory: present, absent, unidentifiable.
// fs.ErrNotExist or an empty directory means absent; a directory holding
// MANIFEST means present; any other directory or stat error means
// unidentifiable, returned with the error.
//
// ponytail: a MANIFEST with no format record is left to the store's own
// Open and Init to refuse. Upgrade to a read-only open, once the backend
// tolerates one after an unclean close.
func probeStore(storeDir string) (storeState, error) {
	fi, err := os.Stat(storeDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return storeAbsent, nil
	case err != nil:
		return storeUnidentifiable, err
	case !fi.IsDir():
		return storeUnidentifiable, fmt.Errorf("%s is not a directory", storeDir)
	}
	entries, err := os.ReadDir(storeDir)
	if err != nil {
		return storeUnidentifiable, err
	}
	if len(entries) == 0 {
		// An empty directory is a volume mounted late: no store is there
		// yet, the same refusal a missing one takes.
		return storeAbsent, nil
	}
	if _, err := os.Stat(filepath.Join(storeDir, manifestName)); err == nil {
		return storePresent, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return storeUnidentifiable, err
	}
	return storeUnidentifiable, nil
}

// Preflight checks, before anything opens, that the store and the journal
// devices a start finds are the ones its installation wrote: a pre-journal
// (v1) local layout is refused naming the path; a present Badger store passes;
// an absent store with the journals that name an installation is the store
// lost; nothing else is initialised or identifiable. It creates, changes and
// removes nothing, and builds no component.
func Preflight(ctx context.Context, storeDir string, devices []string, clock engine.Clock) error {
	switch {
	case ctx.Err() != nil:
		// A canceled context is the caller's answer, never a device verdict:
		// the probes would misread its cancellation as an unidentifiable one.
		return ctx.Err()
	case clock == nil:
		// The wiring errors are answered before the probes because the
		// probes would misread them: an open without a clock calls every
		// device unidentifiable, and an empty store directory is the root
		// of the very filesystem the caller configured.
		return errNoClock
	case storeDir == "":
		return errNoStoreDir
	case len(devices) == 0:
		// decision: the no-device refusal is answered before the probes, so
		// it does not depend on what storage a deviceless start has no
		// business of its own reading: a start refusing for a missing wiring
		// piece must not consult the storage it is about to refuse over.
		return errNoDevice
	}
	facts := startFacts{StoreDir: storeDir}
	roots := append(slices.Clone(devices), filepath.Dir(storeDir), filepath.Join(filepath.Dir(storeDir), "blocks"))
	for _, root := range roots {
		if p := probeV1State(root); p != "" {
			facts.V1 = p
			break
		}
	}
	state, err := probeStore(storeDir)
	switch state {
	case storePresent:
		// A present store already identifies the node's storage: no device
		// probe runs, so a start over an installed store reads none of its
		// devices.
		facts.Store = storePresent
		return decide(facts)
	case storeUnidentifiable:
		// The device probes cannot change this verdict, so they do not run.
		facts.Store, facts.StoreErr = storeUnidentifiable, err
		return decide(facts)
	}
	for _, dir := range devices {
		_, err := journal.Open(ctx, dir, journal.Version{}, journal.JournalID{}, nil, journal.WithClock(clock))
		// A zero expected identity makes Open read and answer the journal's
		// own format and nothing else: no recovery runs, and no byte is
		// written.
		fact := deviceFacts{Dir: dir, OpenErr: err}
		var id *journal.IdentityError
		if errors.As(err, &id) {
			fact.Segments = id.Segments
		}
		facts.Devices = append(facts.Devices, fact)
	}
	return decide(facts)
}

// decision: the severity order below is deliberate — an unidentifiable store
// outranks a lost one, because an operator who runs dfs init over storage
// that cannot be identified destroys data it may still name. Reordering the
// checks reorders which advice that operator gets.
func decide(f startFacts) error {
	if f.V1 != "" {
		return fmt.Errorf("%w: a v1 journal lives at %s, from before the current layout", errV1State, f.V1)
	}
	if f.Store == storePresent {
		return nil
	}
	if f.Store == storeUnidentifiable {
		reason := f.StoreErr
		if reason == nil {
			reason = errors.New("no Badger MANIFEST")
		}
		return fmt.Errorf("%w: the metadata store in %s cannot be identified: %v", errUnidentifiable, f.StoreDir, reason)
	}
	for _, d := range f.Devices {
		var id *journal.IdentityError
		if errors.Is(d.OpenErr, journal.ErrNoJournal) || errors.As(d.OpenErr, &id) {
			continue
		}
		return fmt.Errorf("%w: the journal in %s cannot be identified: %v", errUnidentifiable, d.Dir, d.OpenErr)
	}
	seen := make(map[journal.InstallationID]bool)
	var installs []journal.InstallationID
	dirs := make(map[journal.InstallationID]string)
	segments := false
	for _, d := range f.Devices {
		segments = segments || d.Segments
		var id *journal.IdentityError
		if !errors.As(d.OpenErr, &id) || seen[id.Installation] {
			continue
		}
		seen[id.Installation] = true
		installs = append(installs, id.Installation)
		dirs[id.Installation] = d.Dir
	}
	switch {
	case len(installs) >= 2:
		return fmt.Errorf("%w: %s names installation %x and %s names installation %x", errDevicesDisagree, dirs[installs[0]], installs[0], dirs[installs[1]], installs[1])
	case len(installs) == 1:
		held := ""
		if segments {
			held = ", while its segments still live on the devices"
		}
		return fmt.Errorf("%w: the store volume at %s holds none of installation %x the devices name%s; check the store volume's mount first, then restore it from a backup, a restore loses every write since the backup", errStoreLost, f.StoreDir, installs[0], held)
	default:
		return fmt.Errorf("%w: no device holds a journal and no store holds one; check that each volume is mounted where the configuration's mount paths point, then run dfs init to initialise the node", errNotInitialised)
	}
}
