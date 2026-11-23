package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	badger "github.com/dgraph-io/badger/v4"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/sim"
)

// TestDecide pins the start check's verdict: one error per row, judged by
// sentinel and by the facts the message names. Rows asserting absence guard
// the two operator traps the not-initialised and disagree messages must not
// fall into.
func TestDecide(t *testing.T) {
	instA := journal.InstallationID{0xAA, 0x01}
	instB := journal.InstallationID{0xBB, 0x02}
	corruptFormat := fmt.Errorf("%w: the format file is damaged", journal.ErrCorrupt)
	ident := func(inst journal.InstallationID, dir string, segments bool) deviceFacts {
		return deviceFacts{Dir: dir,
			OpenErr: &journal.IdentityError{Found: journal.JournalID{0x11, 0x22}, Installation: inst, Segments: segments}}
	}
	noJournal := func(dir string) deviceFacts {
		return deviceFacts{Dir: dir, OpenErr: journal.ErrNoJournal}
	}

	rows := []struct {
		name     string
		facts    startFacts
		want     error
		contains []string
		misses   []string
		neverIs  []error
	}{
		{
			name:  "v1 state found",
			facts: startFacts{V1: "/x/blocks/shares/s1/journal"},
			want:  errV1State,
			contains: []string{
				"refusing to start: a pre-journal block layout at /x/blocks/shares/s1/journal is from v1; " +
					"upgrading from v1 is not supported: start a new installation with dfs init on empty storage",
			},
		},
		{
			name: "v1 state outranks a lost store",
			facts: startFacts{
				V1:       "/x/blocks/shares/s1/journal",
				Store:    storeAbsent,
				StoreDir: "/s",
				Devices:  []deviceFacts{ident(instA, "/d1", false)},
			},
			want:     errV1State,
			contains: []string{"/x/blocks/shares/s1/journal"},
			neverIs:  []error{errStoreLost},
		},
		{
			name: "store unidentifiable",
			facts: startFacts{
				Store:    storeUnidentifiable,
				StoreErr: errors.New("permission denied"),
				StoreDir: "/s",
				Devices:  []deviceFacts{noJournal("/d1")},
			},
			want: errUnidentifiable,
			contains: []string{
				"/s",
				"permission denied",
			},
			misses: []string{"dfs init"},
		},
		{
			name: "unidentifiable store outranks a lost one",
			facts: startFacts{
				Store:    storeUnidentifiable,
				StoreErr: errors.New("permission denied"),
				StoreDir: "/s",
				Devices:  []deviceFacts{ident(instA, "/d1", false)},
			},
			want:     errUnidentifiable,
			contains: []string{"/s", "permission denied"},
			neverIs:  []error{errStoreLost},
		},
		{
			name: "device unidentifiable",
			facts: startFacts{
				Store:   storeAbsent,
				Devices: []deviceFacts{{Dir: "/d1", OpenErr: journal.ErrCorrupt}},
			},
			want: errUnidentifiable,
			contains: []string{
				"/d1",
				journal.ErrCorrupt.Error(),
			},
		},
		{
			name: "corrupt format beside absent store",
			facts: startFacts{
				Store:    storeAbsent,
				StoreDir: "/s",
				Devices:  []deviceFacts{{Dir: "/d1", OpenErr: corruptFormat}},
			},
			want:    errUnidentifiable,
			neverIs: []error{errNotInitialised},
		},
		{
			name: "segments without a format file",
			facts: startFacts{
				Store:   storeAbsent,
				Devices: []deviceFacts{{Dir: "/d1", OpenErr: journal.ErrUnidentified}},
			},
			want: errUnidentifiable,
			contains: []string{
				"/d1",
			},
		},
		{
			name: "devices disagree",
			facts: startFacts{
				Store:   storeAbsent,
				Devices: []deviceFacts{ident(instA, "/d1", false), ident(instB, "/d2", false)},
			},
			want: errDevicesDisagree,
			contains: []string{
				"aa01",
				"bb02",
				"/d1",
				"/d2",
			},
			neverIs: []error{errStoreLost},
		},
		{
			name: "mixed set, unidentifiable beats the rest",
			facts: startFacts{
				Store:   storeAbsent,
				Devices: []deviceFacts{ident(instA, "/d1", false), {Dir: "/d2", OpenErr: journal.ErrCorrupt}, ident(instB, "/d3", false)},
			},
			want:    errUnidentifiable,
			neverIs: []error{errDevicesDisagree, errStoreLost},
		},
		{
			name: "mixed set reversed, same verdict",
			facts: startFacts{
				Store:   storeAbsent,
				Devices: []deviceFacts{ident(instB, "/d3", false), {Dir: "/d2", OpenErr: journal.ErrCorrupt}, ident(instA, "/d1", false)},
			},
			want:    errUnidentifiable,
			neverIs: []error{errDevicesDisagree, errStoreLost},
		},
		{
			name: "store lost, devices hold segments",
			facts: startFacts{
				Store:    storeAbsent,
				StoreDir: "/s",
				Devices:  []deviceFacts{ident(instA, "/d1", true), ident(instA, "/d2", false)},
			},
			want: errStoreLost,
			contains: []string{
				"/s",
				"aa01",
				"1122",
				"/d1",
				"check the store volume's mount first",
				"restore",
				"loses every write since",
				"segment",
			},
		},
		{
			name: "store lost, no segments",
			facts: startFacts{
				Store:    storeAbsent,
				StoreDir: "/s",
				Devices:  []deviceFacts{ident(instA, "/d1", false), ident(instA, "/d2", false)},
			},
			want: errStoreLost,
			contains: []string{
				"/s",
				"aa01",
				"1122",
				"/d1",
				"check the store volume's mount first",
				"restore",
				"loses every write since",
			},
			misses: []string{"segment"},
		},
		{
			name: "some devices hold no journal, one names the installation",
			facts: startFacts{
				Store:    storeAbsent,
				StoreDir: "/s",
				Devices:  []deviceFacts{noJournal("/d1"), ident(instA, "/d2", false)},
			},
			want: errStoreLost,
		},
		{
			name: "nothing initialised",
			facts: startFacts{
				Store:    storeAbsent,
				StoreDir: "/s",
				Devices:  []deviceFacts{noJournal("/d1"), noJournal("/d2")},
			},
			want: errNotInitialised,
			contains: []string{
				"mount",
				"dfs init",
			},
			misses: []string{"/s", "installation"},
		},
		{
			name:  "store present",
			facts: startFacts{Store: storePresent},
			want:  nil,
		},
	}

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			got := decide(r.facts)
			if !errors.Is(got, r.want) {
				t.Fatalf("decide's verdict: got %v, want %v", got, r.want)
			}
			if got == nil {
				return
			}
			for _, never := range r.neverIs {
				if errors.Is(got, never) {
					t.Errorf("decide's verdict: got %v, which must never wrap %v", got, never)
				}
			}
			for _, want := range r.contains {
				if !strings.Contains(got.Error(), want) {
					t.Errorf("verdict message must contain %q: got %q", want, got.Error())
				}
			}
			for _, miss := range r.misses {
				if strings.Contains(got.Error(), miss) {
					t.Errorf("verdict message must not contain %q: got %q", miss, got.Error())
				}
			}
		})
	}
}

// hashTree hashes root's whole tree — relative paths, sizes, then contents,
// in sorted order — so any file created, changed, removed or renamed under
// root changes it. A symlink is hashed as the link it is — its name and the
// path it names, never followed — so a tree a start refuses to read on an
// unstat-able path still hashes before and after. A path named in skip, and
// everything under it, is left out: for a subtree a collaborator of the code
// under test rewrites on its own close, whose bytes say nothing about what
// the code did.
func hashTree(t *testing.T, root string, skip ...string) string {
	t.Helper()
	h := sha256.New()
	var walk func(dir, rel string)
	walk = func(dir, rel string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("hash %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if rel != "" {
				name = rel + "/" + e.Name()
			}
			p := filepath.Join(dir, e.Name())
			if slices.Contains(skip, p) {
				continue
			}
			if e.Type()&fs.ModeSymlink != 0 {
				target, err := os.Readlink(p)
				if err != nil {
					t.Fatalf("hash the link %s: %v", p, err)
				}
				_, _ = fmt.Fprintf(h, "%s:%d:link:%s", name, len(target), target)
				continue
			}
			if e.IsDir() {
				h.Write([]byte(name + "/"))
				walk(p, name)
				continue
			}
			content, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("hash %s: %v", p, err)
			}
			_, _ = fmt.Fprintf(h, "%s:%d:", name, len(content))
			h.Write(content)
		}
	}
	walk(root, "")
	return hex.EncodeToString(h.Sum(nil))
}

// makeDir is an empty directory on the path p, made.
func makeDir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatal(err)
	}
}

// writeFile makes every directory on the way to p and writes content into it.
func writeFile(t *testing.T, p, content string) {
	t.Helper()
	makeDir(t, filepath.Dir(p))
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// initJournal creates a real journal for install in dir, over a mount the
// journal's platform check accepts: the row's host may hold the test's
// temporary directory where the check is the strictest.
func initJournal(t *testing.T, dir string, install journal.InstallationID) {
	t.Helper()
	makeDir(t, dir)
	if _, err := journal.Init(context.Background(), dir, install, journaltest.SuitableMount()); err != nil {
		t.Fatalf("journal init on %s: %v", dir, err)
	}
}

// openStore opens and closes a Badger store at dir, whose Open leaves the
// MANIFEST the store probe reads.
func openStore(t *testing.T, dir string) {
	t.Helper()
	db, err := badgerkv.Open(badgerkv.Options{Dir: dir})
	if err != nil {
		t.Fatalf("open the store at %s: %v", dir, err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close the store at %s: %v", dir, err)
	}
}

// manifestName is Badger's MANIFEST, held to Badger's own constant so a Badger
// upgrade that renames it fails here rather than making the store probe read
// every store as absent.
func TestManifestName(t *testing.T) {
	if manifestName != badger.ManifestFilename {
		t.Errorf("manifestName = %q, want badger.ManifestFilename %q: the store probe would misread every store", manifestName, badger.ManifestFilename)
	}
}

// TestPreflight pins the start check's behaviour: the verdict it answers over
// whatever storage a start finds, the order it answers in — wiring errors
// before probes, the no-device refusal before any storage is read, v1 before
// a present store — and that it changes no byte of that storage: every row's
// trees are hashed before and after. Storage is real, journals by journal.Init
// and the store directory by a Badger open.
func TestPreflight(t *testing.T) {
	instA := journal.InstallationID{0xAA, 0x01}

	rows := []struct {
		name string
		// setup builds the row's storage under parent and returns the store
		// directory and the devices the check is handed.
		setup func(t *testing.T, parent string) (storeDir string, devices []string)
		// noClock leaves the check without its clock.
		noClock bool
		// cancelCtx hands the check a context its caller already canceled.
		cancelCtx bool
		// v1Root names the row's blockstore journal root, the configured
		// v1 location the check is handed; nil defaults to the row's
		// <parent>/blocks.
		v1Root func(parent string) string
		// want is the verdict, matched by sentinel through errors.Is.
		want error
		// neverIs are the verdicts this row must not answer.
		neverIs []error
		// contains names what the verdict's message must contain, of the
		// row's parent directory.
		contains func(parent string) []string
		// postSetup runs after the trees are hashed, before the check: the
		// row's storage states that a hash cannot survive.
		postSetup func(t *testing.T, storeDir string, devices []string)
		// postRun runs after the check, before the trees are re-hashed.
		postRun func(t *testing.T, storeDir string, devices []string)
		// after asserts what the refusal must leave behind.
		after func(t *testing.T, storeDir string, devices []string)
	}{
		{
			name: "a canceled context is the caller's answer",
			setup: func(t *testing.T, parent string) (string, []string) {
				writeFile(t, filepath.Join(parent, "blocks", "shares", "s1", "blobs", "f1"), "v1 data")
				dir := filepath.Join(parent, "device")
				initJournal(t, dir, instA)
				return filepath.Join(parent, "metadata"), []string{dir}
			},
			cancelCtx: true,
			// the storage planted would refuse — the v1 layout below anything
			// else — and a canceled probe would read as an unidentifiable
			// device: the context's own error is the answer instead, so no
			// verdict is spoken over storage the caller already stopped.
			want: context.Canceled,
			neverIs: []error{
				errV1State, errUnidentifiable, errDevicesDisagree,
				errStoreLost, errNotInitialised, errNoDevice,
			},
		},
		{
			name: "no clock",
			setup: func(t *testing.T, parent string) (string, []string) {
				dir := filepath.Join(parent, "device")
				initJournal(t, dir, instA)
				return filepath.Join(parent, "store"), []string{dir}
			},
			noClock:  true,
			want:     errNoClock,
			contains: func(string) []string { return []string{"clock"} },
			neverIs:  []error{errUnidentifiable},
		},
		{
			name: "empty storeDir",
			setup: func(t *testing.T, parent string) (string, []string) {
				dir := filepath.Join(parent, "device")
				initJournal(t, dir, instA)
				return "", []string{dir}
			},
			want:     errNoStoreDir,
			contains: func(string) []string { return []string{"not set"} },
			// the ruling of the order: a wiring error beats the no-device
			// refusal when both apply.
			neverIs: []error{errNoDevice},
		},
		{
			name: "no device, no store directory created",
			setup: func(t *testing.T, parent string) (string, []string) {
				return filepath.Join(parent, "store"), nil
			},
			want:    errNoDevice,
			neverIs: []error{errNotInitialised},
			after: func(t *testing.T, storeDir string, _ []string) {
				if _, err := os.Stat(storeDir); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("store directory %s after the refusal: %v, want none: the refusal creates nothing", storeDir, err)
				}
			},
		},
		{
			name: "no device, store present",
			setup: func(t *testing.T, parent string) (string, []string) {
				openStore(t, filepath.Join(parent, "store"))
				return filepath.Join(parent, "store"), nil
			},
			// the refusal fires before the store probe answers, so it does
			// not wait on the storage a deviceless start has no business of
			// its own reading.
			want:    errNoDevice,
			neverIs: []error{errNotInitialised},
		},
		{
			name: "absent store, journal-less devices",
			setup: func(t *testing.T, parent string) (string, []string) {
				dir := filepath.Join(parent, "device")
				makeDir(t, dir)
				return filepath.Join(parent, "store"), []string{dir}
			},
			want:     errNotInitialised,
			contains: func(string) []string { return []string{"dfs init"} },
			after: func(t *testing.T, storeDir string, _ []string) {
				if _, err := os.Stat(storeDir); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("store directory %s after the refusal: %v, want none: the refusal creates nothing", storeDir, err)
				}
			},
		},
		{
			name: "store directory holds a MANIFEST",
			setup: func(t *testing.T, parent string) (string, []string) {
				openStore(t, filepath.Join(parent, "store"))
				dir := filepath.Join(parent, "device")
				initJournal(t, dir, instA)
				return filepath.Join(parent, "store"), []string{dir}
			},
			want: nil,
		},
		{
			name: "only tables in the store directory",
			setup: func(t *testing.T, parent string) (string, []string) {
				writeFile(t, filepath.Join(parent, "store", "00001.sst"), "a table no store open wrote")
				dir := filepath.Join(parent, "device")
				makeDir(t, dir)
				return filepath.Join(parent, "store"), []string{dir}
			},
			want:     errUnidentifiable,
			neverIs:  []error{errNotInitialised},
			contains: func(string) []string { return []string{manifestName} },
		},
		{
			name: "installed store, a v1 layout under the configured root",
			setup: func(t *testing.T, parent string) (string, []string) {
				openStore(t, filepath.Join(parent, "store"))
				writeFile(t, filepath.Join(parent, "mnt-fast", "shares", "s1", "blobs", "f1"), "v1 data")
				dir := filepath.Join(parent, "device")
				initJournal(t, dir, instA)
				return filepath.Join(parent, "store"), []string{dir}
			},
			// the probe reads the root the configuration names, not the
			// store's parent: this row's v1 layout is where the bootstrap
			// points blockstore.journal.path, and the refusal runs before
			// the store-present verdict.
			v1Root:  func(parent string) string { return filepath.Join(parent, "mnt-fast") },
			want:    errV1State,
			neverIs: []error{errStoreLost},
			contains: func(parent string) []string {
				return []string{filepath.Join(parent, "mnt-fast", "shares", "s1", "blobs")}
			},
		},
		{
			name: "a v1-shaped tree the configuration does not name is not the start's to read",
			setup: func(t *testing.T, parent string) (string, []string) {
				openStore(t, filepath.Join(parent, "store"))
				writeFile(t, filepath.Join(parent, "shares", "web", "logs", "access.log"), "someone else's data")
				dir := filepath.Join(parent, "device")
				initJournal(t, dir, instA)
				return filepath.Join(parent, "store"), []string{dir}
			},
			// only the devices and the configured blockstore root are
			// probed: an unrelated shares-like tree beside the store does
			// not refuse a node that is already installed.
			want:    nil,
			neverIs: []error{errV1State},
		},
		{
			name: "device is an old v1 root",
			setup: func(t *testing.T, parent string) (string, []string) {
				dir := filepath.Join(parent, "device")
				writeFile(t, filepath.Join(dir, "shares", "s1", "journal", "x.seg"), "v1")
				return filepath.Join(parent, "store"), []string{dir}
			},
			want:    errV1State,
			neverIs: []error{errNotInitialised},
			contains: func(parent string) []string {
				return []string{filepath.Join(parent, "device", "shares", "s1", "journal")}
			},
		},
		{
			name: "v1 under the blockstore's default root",
			setup: func(t *testing.T, parent string) (string, []string) {
				writeFile(t, filepath.Join(parent, "blocks", "shares", "s1", "blobs", "f1"), "v1 data")
				dir := filepath.Join(parent, "device")
				initJournal(t, dir, instA)
				return filepath.Join(parent, "metadata"), []string{dir}
			},
			// the row leaves the root at its default, the state directory's
			// blocks, which the bootstrap resolves when the operator sets
			// nothing: the device checks alone would answer not-initialised
			// and v1 data would be left behind with no warning.
			want: errV1State,
			contains: func(parent string) []string {
				return []string{filepath.Join(parent, "blocks", "shares", "s1", "blobs")}
			},
		},
		{
			name: "an empty v1 blobs directory is no v1 layout",
			setup: func(t *testing.T, parent string) (string, []string) {
				makeDir(t, filepath.Join(parent, "blocks", "shares", "s1", "blobs"))
				dir := filepath.Join(parent, "device")
				initJournal(t, dir, instA)
				return filepath.Join(parent, "metadata"), []string{dir}
			},
			// no file in it, so no refusal names it: the verdict falls
			// through to whatever else the row planted.
			want:    errStoreLost,
			neverIs: []error{errV1State},
		},
		{
			name: "store lost",
			setup: func(t *testing.T, parent string) (string, []string) {
				dir := filepath.Join(parent, "device")
				initJournal(t, dir, instA)
				return filepath.Join(parent, "store"), []string{dir}
			},
			want: errStoreLost,
			contains: func(parent string) []string {
				return []string{filepath.Join(parent, "store"), "aa01", "restore"}
			},
		},
		{
			name: "journal open writes nothing",
			setup: func(t *testing.T, parent string) (string, []string) {
				dir := filepath.Join(parent, "device")
				initJournal(t, dir, instA)
				// A taken segment the identity answer must name still on
				// the device: under the header-slot ruling a segment counts
				// once a stream took it — a header that verifies, and not
				// a name with junk inside — so the fixture plants a
				// verifying header at a segment's own name, the shape the
				// classifier answers NameSegment for.
				if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%020d.seg", 1)),
					raw.SegmentHeader{Identity: [16]byte{9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9}, ID: 1}.Bytes(),
					0o600); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(parent, "store"), []string{dir}
			},
			// this row's real assertion is the hash equality around the
			// check: an open that answers the device's identity changed no
			// byte of the device's tree.
			want:     errStoreLost,
			contains: func(string) []string { return []string{"segment"} },
		},
		{
			name: "a device whose format is damaged",
			setup: func(t *testing.T, parent string) (string, []string) {
				dir := filepath.Join(parent, "device")
				initJournal(t, dir, instA)
				writeFile(t, filepath.Join(dir, "format"), "damaged beyond reading")
				return filepath.Join(parent, "store"), []string{dir}
			},
			want:    errUnidentifiable,
			neverIs: []error{errNotInitialised},
			contains: func(parent string) []string {
				return []string{filepath.Join(parent, "device")}
			},
		},
		{
			name: "an unreadable v1-shaped tree the configuration does not name reads as none",
			setup: func(t *testing.T, parent string) (string, []string) {
				openStore(t, filepath.Join(parent, "store"))
				makeDir(t, filepath.Join(parent, "shares", "s1", "blobs"))
				device := filepath.Join(parent, "device")
				makeDir(t, device)
				return filepath.Join(parent, "store"), []string{device}
			},
			// The shares tree is made unreadable between the hashes: the
			// v1 probe tolerates what it cannot read — the store and device
			// probes name their own errors — so beside a store the check
			// accepts, an unreadable tree it does not own refuses nothing.
			postSetup: func(t *testing.T, storeDir string, _ []string) {
				shares := filepath.Join(filepath.Dir(storeDir), "shares")
				if err := os.Chmod(shares, 0o000); err != nil {
					t.Fatal(err)
				}
				if entries, err := os.ReadDir(shares); err == nil {
					t.Skipf("this host reads a 0-mode directory (%d entries), which root does; the tolerance below cannot be shown here", len(entries))
				}
			},
			postRun: func(t *testing.T, storeDir string, _ []string) {
				if err := os.Chmod(filepath.Join(filepath.Dir(storeDir), "shares"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
			want:    nil,
			neverIs: []error{errV1State, errUnidentifiable, errNoDevice},
		},
		{
			name: "a store directory holding only a volume's lost+found",
			setup: func(t *testing.T, parent string) (string, []string) {
				dir := filepath.Join(parent, "store")
				makeDir(t, filepath.Join(dir, "lost+found"))
				device := filepath.Join(parent, "device")
				initJournal(t, device, instA)
				return dir, []string{device}
			},
			// a mount point is not a store location: the refusal names
			// lost+found and the directory-on-the-volume rule, so the
			// operator is not told to initialise over the volume's own
			// directory.
			want:    errUnidentifiable,
			neverIs: []error{errNotInitialised},
			contains: func(parent string) []string {
				return []string{filepath.Join(parent, "store"), "lost+found", "mount root"}
			},
		},
		{
			name: "a store path that cannot be stat'ed",
			setup: func(t *testing.T, parent string) (string, []string) {
				dir := filepath.Join(parent, "store")
				// A symlink to itself: stat answers ELOOP on every host, so
				// the probe's stat fails without the test needing a mode
				// this host's user could read around.
				if err := os.Symlink(dir, dir); err != nil {
					t.Fatal(err)
				}
				device := filepath.Join(parent, "device")
				initJournal(t, device, instA)
				return dir, []string{device}
			},
			// a path the probe cannot stat is unidentifiable, never absent
			// with the not-initialised advice to run dfs init over what
			// cannot even be read.
			want:    errUnidentifiable,
			neverIs: []error{errNotInitialised, errStoreLost},
			contains: func(parent string) []string {
				return []string{filepath.Join(parent, "store")}
			},
		},
		{
			name: "a v1 layout's logs leaf under a device",
			setup: func(t *testing.T, parent string) (string, []string) {
				dir := filepath.Join(parent, "device")
				writeFile(t, filepath.Join(dir, "shares", "s1", "logs", "l1"), "v1")
				return filepath.Join(parent, "store"), []string{dir}
			},
			want:    errV1State,
			neverIs: []error{errNotInitialised},
			contains: func(parent string) []string {
				return []string{filepath.Join(parent, "device", "shares", "s1", "logs")}
			},
		},
		{
			name: "unreadable store directory",
			setup: func(t *testing.T, parent string) (string, []string) {
				dir := filepath.Join(parent, "store")
				makeDir(t, dir)
				device := filepath.Join(parent, "device")
				initJournal(t, device, instA)
				return dir, []string{device}
			},
			postSetup: func(t *testing.T, storeDir string, _ []string) {
				if err := os.Chmod(storeDir, 0o000); err != nil {
					t.Fatal(err)
				}
				if entries, err := os.ReadDir(storeDir); err == nil {
					t.Skipf("this host reads a 0-mode directory (%d entries), which root does; the unreadable-store refusal cannot be shown here", len(entries))
				}
			},
			// the mode is restored before the trees are re-hashed: no byte
			// inside it could have changed while nothing could read it.
			postRun: func(t *testing.T, storeDir string, _ []string) {
				if err := os.Chmod(storeDir, 0o700); err != nil {
					t.Fatal(err)
				}
			},
			want:    errUnidentifiable,
			neverIs: []error{errNotInitialised},
			contains: func(parent string) []string {
				return []string{filepath.Join(parent, "store")}
			},
		},
	}

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			parent := t.TempDir()
			storeDir, devices := r.setup(t, parent)
			hashRoots := append(slices.Clone(devices), parent)
			before := make(map[string]string, len(hashRoots))
			for _, root := range hashRoots {
				before[root] = hashTree(t, root)
			}
			if r.postSetup != nil {
				r.postSetup(t, storeDir, devices)
				t.Cleanup(func() { _ = os.Chmod(storeDir, 0o700) })
			}
			var clock engine.Clock = sim.NewTestClock(time.Unix(0, 0))
			if r.noClock {
				clock = nil
			}
			v1Root := filepath.Join(parent, "blocks")
			if r.v1Root != nil {
				v1Root = r.v1Root(parent)
			}
			ctx, cancel := context.WithCancel(context.Background())
			if r.cancelCtx {
				cancel()
			} else {
				defer cancel()
			}
			got := Preflight(ctx, storeDir, v1Root, devices, clock, journaltest.SuitableMount())
			if r.postRun != nil {
				r.postRun(t, storeDir, devices)
			}
			if !errors.Is(got, r.want) {
				t.Fatalf("Preflight's verdict: got %v, want %v", got, r.want)
			}
			if got != nil {
				for _, never := range r.neverIs {
					if errors.Is(got, never) {
						t.Errorf("Preflight's verdict: got %v, which must never wrap %v", got, never)
					}
				}
			}
			if got != nil && r.contains != nil {
				for _, want := range r.contains(parent) {
					if !strings.Contains(got.Error(), want) {
						t.Errorf("verdict message must contain %q: got %q", want, got.Error())
					}
				}
			}
			if r.after != nil {
				r.after(t, storeDir, devices)
			}
			for _, root := range hashRoots {
				if after := hashTree(t, root); after != before[root] {
					t.Errorf("tree %s changed across the start check: hash %s after %s before, want the check to touch nothing", root, after, before[root])
				}
			}
		})
	}
}

// A context that dies mid-probe — this row ends it during the devices'
// opens — is the caller's answer, never a device verdict: without the guard
// after the loop, decide would read the cancellation as an unidentified
// device and misadvise an operator whose process is merely stopping.
func TestPreflightCancelDuringTheProbes(t *testing.T) {
	parent := t.TempDir()
	store := filepath.Join(parent, "store")
	d1 := filepath.Join(parent, "device-one")
	d2 := filepath.Join(parent, "device-two")
	initJournal(t, d1, journal.InstallationID{0xAA, 0x01})
	makeDir(t, d2)
	before := map[string]string{parent: hashTree(t, parent)}
	ctx, cancel := context.WithCancel(context.Background())
	afterDeviceOpen = cancel
	t.Cleanup(func() { afterDeviceOpen = nil })
	err := Preflight(ctx, store, filepath.Join(parent, "blocks"), []string{d1, d2},
		sim.NewTestClock(time.Unix(0, 0)), journaltest.SuitableMount())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Preflight over a context ended mid-probe: got %v, want the context's own error", err)
	}
	for _, never := range []error{errUnidentifiable, errDevicesDisagree, errStoreLost, errNotInitialised, errV1State} {
		if errors.Is(err, never) {
			t.Errorf("Preflight over a context ended mid-probe: got %v, which must never wrap %v", err, never)
		}
	}
	if after := hashTree(t, parent); after != before[parent] {
		t.Errorf("the trees changed across an ended probe: hash %s after %s before, want the check to touch nothing", after, before[parent])
	}
}
