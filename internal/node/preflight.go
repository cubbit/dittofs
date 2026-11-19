package node

// What a start refuses to serve over: a start over absent or contradictory
// storage names what it found instead of creating anything. decide turns the
// facts the probes gathered — the v1 layouts, the store directory, each
// journal device — into that one verdict, and nothing under the node's
// storage is opened, created or changed to gather them.

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
	Dir     string
	OpenErr error
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
// logs directory. "" is none. The probe is a heuristic over directories whose
// contents the start does not own, so it tolerates what it cannot read — a
// missing, malformed or unreadable tree reads as no v1 state — while the store
// and device probes, over directories the start does own, name their reading
// errors.
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
// unidentifiable, returned with the error that says why.
//
// ponytail: a MANIFEST with no format record is left to the store's own
// Open and Init to refuse. Upgrade to a read-only open, once the backend
// tolerates one after an unclean close.
//
// decision: a directory whose only entry is a volume's own lost+found is the
// volume's mount point, and the store belongs in a directory on the volume,
// never at its mount root (RFC 1 §3): the refusal names lost+found and says
// so, refusing a start that would otherwise read every fresh mount point as
// a store to initialise. Withdraw the naming once a start can be trusted to
// initialise over foreign files, which it cannot be asked to today.
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
	if len(entries) == 1 && entries[0].Name() == "lost+found" {
		return storeUnidentifiable, fmt.Errorf(
			"%s holds only %s, a volume's mount point: point the store at a directory on the volume, not at its mount root",
			storeDir, entries[0].Name(),
		)
	}
	if _, err := os.Stat(filepath.Join(storeDir, manifestName)); err == nil {
		return storePresent, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return storeUnidentifiable, err
	}
	return storeUnidentifiable, errors.New("no Badger MANIFEST")
}

// afterDeviceOpen runs after each device's journal opens, in device order;
// a test replaces it so a row can end its context mid-probe. Production
// leaves it unset.
var afterDeviceOpen func()

// Preflight checks, before anything opens, that the store and the journal
// devices a start finds are the ones its installation wrote: a pre-journal
// (v1) local layout, at a device or at the journal root the blockstore's
// configuration names, is refused naming the path; a present Badger store
// passes; an absent store with the journals that name an installation is the
// store lost; nothing else is initialised or identifiable. It creates,
// changes and removes nothing, and builds no component.
func Preflight(ctx context.Context, storeDir, v1Root string, devices []string, clock engine.Clock) error {
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
		// decision: a bootstrap that names no journal device is a missing
		// wiring piece, refused before any probe runs: a deviceless start
		// must refuse over a present store exactly as over an absent one, so
		// its refusal may not depend on storage it is about to refuse over
		// either way. Overturned when a caller can name its devices to a
		// different gate — a device list the metadata store itself holds,
		// say — that would let the probes answer first.
		return errNoDevice
	}
	facts := startFacts{StoreDir: storeDir}
	for _, root := range append(slices.Clone(devices), v1Root) {
		if p := probeV1State(root); p != "" {
			facts.V1 = p
			break
		}
	}
	state, err := probeStore(storeDir)
	if state != storeAbsent {
		// The device probes cannot change a present or an unidentifiable
		// verdict, so they do not run: a present store already identifies the
		// node's storage, and one that cannot be identified leaves nothing
		// for the devices to decide.
		facts.Store, facts.StoreErr = state, err
		return decide(facts)
	}
	for _, dir := range devices {
		_, err := journal.Open(ctx, dir, journal.Version{}, journal.JournalID{}, nil, journal.WithClock(clock))
		// A zero expected identity makes Open read and answer the journal's
		// own format and nothing else: no recovery runs, and no byte is
		// written.
		if afterDeviceOpen != nil {
			afterDeviceOpen()
		}
		facts.Devices = append(facts.Devices, deviceFacts{Dir: dir, OpenErr: err})
	}
	// A context that dies mid-probe is the caller's answer, never a device
	// verdict: decide would read its cancellation as an unidentified device.
	if err := ctx.Err(); err != nil {
		return err
	}
	return decide(facts)
}

// decision: the severity order below is deliberate — an unidentifiable store
// outranks a lost one, because an operator who runs dfs init over storage
// that cannot be identified destroys data it may still name. Reordering the
// checks reorders which advice that operator gets.
func decide(f startFacts) error {
	if f.V1 != "" {
		return fmt.Errorf("%w: v1 state lives at %s, from before the current layout", errV1State, f.V1)
	}
	if f.Store == storePresent {
		// decision: a present store settles the start check on its own and
		// the devices are left unconsulted: Start's refuse step compares each
		// device's journal with the installation the store holds, so deciding
		// over the devices here would answer a second time what comes with no
		// new fact. Overturned when a start-time state exists in which a
		// present store must not be trusted on its own.
		return nil
	}
	if f.Store == storeUnidentifiable {
		return fmt.Errorf("%w: the metadata store in %s cannot be identified: %v", errUnidentifiable, f.StoreDir, f.StoreErr)
	}
	var (
		first    *journal.IdentityError // the first device's journal identity, by device order
		firstDir string
		segments bool // any identified device still holds segments
	)
	for _, d := range f.Devices {
		var id *journal.IdentityError
		if errors.Is(d.OpenErr, journal.ErrNoJournal) {
			continue
		}
		if !errors.As(d.OpenErr, &id) {
			return fmt.Errorf("%w: the journal in %s cannot be identified: %v", errUnidentifiable, d.Dir, d.OpenErr)
		}
		segments = segments || id.Segments
		if first == nil {
			first, firstDir = id, d.Dir
			continue
		}
		if id.Installation != first.Installation {
			return fmt.Errorf("%w: %s names installation %x and %s names installation %x", errDevicesDisagree, firstDir, first.Installation, d.Dir, id.Installation)
		}
	}
	if first == nil {
		return fmt.Errorf("%w: no device holds a journal and no store holds one; check that each volume is mounted where the configuration's mount paths point, then run dfs init to initialise the node", errNotInitialised)
	}
	held := ""
	if segments {
		held = ", while its segments still live on the devices"
	}
	return fmt.Errorf("%w: the store volume at %s holds none of installation %x the devices name — the journal at %s carries identity %x%s; check the store volume's mount first, then restore it from a backup, which loses every write since that backup", errStoreLost, f.StoreDir, first.Installation, firstDir, first.Found, held)
}
