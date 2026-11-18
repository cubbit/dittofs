package bootstrap

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// golden is RFC 28 §2.1's node-id layout for the identity 00..0f, written by
// hand: the magic, the identity, and the CRC32C of those 20 bytes,
// little-endian. The checksum was computed apart from the code under test.
var golden = []byte{
	'D', 'N', 'I', '1',
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
	0x75, 0x63, 0xa2, 0x5a,
}

var (
	goldenID = entity.NodeID{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	nodeA    = entity.NodeID{'a'}
	nodeB    = entity.NodeID{'b'}
	install  = [16]byte{'i', 'n', 's', 't'}
	foreign  = [16]byte{'f', 'o', 'r', 'e', 'i', 'g', 'n'}
)

// newJournal makes a journal directory of installation inst and returns it
// with the journal's identity.
func newJournal(t *testing.T, inst [16]byte) (string, journal.JournalID) {
	t.Helper()

	dir := t.TempDir()

	id, err := journal.Init(context.Background(), dir, inst, journaltest.SuitableMount())
	if err != nil {
		t.Fatalf("journal init: %v", err)
	}

	return dir, id
}

func putCopy(t *testing.T, dir string, b []byte) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, nodeIDName), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func copyBytes(t *testing.T, dir string) []byte {
	t.Helper()

	b, err := os.ReadFile(filepath.Join(dir, nodeIDName))
	if err != nil {
		t.Fatalf("read the copy in %s: %v", dir, err)
	}

	return b
}

func noCopy(t *testing.T, dir string) {
	t.Helper()

	if _, err := os.Lstat(filepath.Join(dir, nodeIDName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s: want no node-id, got %v", dir, err)
	}
}

func bootstrapOf(host entity.NodeID, dirs ...string) *Bootstrap {
	b := &Bootstrap{}
	b.Node.ID = host

	for _, d := range dirs {
		b.Journals = append(b.Journals, Journal{Path: d})
	}

	return b
}

// newStore is the metadata store, in memory, of the installation install.
func newStore(t *testing.T) *kvstore.Store {
	t.Helper()

	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})

	if err := kvstore.Init(context.Background(), db, install); err != nil {
		t.Fatal(err)
	}

	s, err := kvstore.Open(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}

	return s
}

func record(t *testing.T, s *kvstore.Store, jid [16]byte, node entity.NodeID, dir string) {
	t.Helper()

	if err := s.RecordJournal(context.Background(), jid, node, entity.JournalDir(dir)); err != nil {
		t.Fatal(err)
	}
}

// logs is a logger and the text it wrote.
func logs() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer

	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func writeMissing(
	t *testing.T,
	s *kvstore.Store,
	b *Bootstrap,
	id entity.NodeID,
	log *slog.Logger,
) error {
	t.Helper()

	return writeMissingNodeIDs(
		context.Background(),
		b,
		id,
		s.Lifecycle().JournalID,
		func(dir string) copyDir {
			return osDir(dir)
		},
		log,
	)
}

// refused checks that err is want and names every directory in dirs.
func refused(t *testing.T, what string, err, want error, dirs ...string) {
	t.Helper()

	if !errors.Is(err, want) {
		t.Fatalf("%s: got %v, want %v", what, err, want)
	}

	for _, d := range dirs {
		if !strings.Contains(err.Error(), d) {
			t.Errorf("%s: the error %q does not name %s", what, err, d)
		}
	}
}

func TestNodeIDGoldenVector(t *testing.T) {
	got, err := parseNodeID(golden)
	if err != nil {
		t.Fatalf("parse the golden vector: %v", err)
	}

	if got != goldenID {
		t.Errorf("golden vector: got identity %x, want %x", got, goldenID)
	}

	dir, _ := newJournal(t, install)

	id, err := InitNodeID(context.Background(), bootstrapOf(goldenID, dir), install, dir)
	if err != nil {
		t.Fatalf("InitNodeID: %v", err)
	}

	if id != goldenID {
		t.Errorf("InitNodeID: got %x, want %x", id, goldenID)
	}

	if b := copyBytes(t, dir); !bytes.Equal(b, golden) {
		t.Errorf("InitNodeID wrote %x, want the golden vector %x", b, golden)
	}
}

func TestNodeIDDoesNotParse(t *testing.T) {
	magic := bytes.Clone(golden)
	magic[3] = '2'

	checksum := bytes.Clone(golden)
	checksum[23] ^= 0x01

	// Another magic under a checksum that verifies, so only the magic check
	// refuses it.
	signed := append([]byte("DNI2"), golden[4:20]...)
	signed = binary.LittleEndian.AppendUint32(
		signed,
		crc32.Checksum(signed, crc32.MakeTable(crc32.Castagnoli)),
	)

	for _, tc := range []struct {
		name string
		b    []byte
	}{
		{"empty", nil},
		{"23 bytes", golden[:23]},
		{"25 bytes", append(bytes.Clone(golden), 0)},
		{"magic DNI2", magic},
		{"magic DNI2 with a checksum that verifies", signed},
		{"the zero identity", zeroIdentity()},
		{"a bad checksum", checksum},
	} {
		if _, err := parseNodeID(tc.b); !errors.Is(err, errCopyDoesNotParse) {
			t.Errorf("parse %s: got %v, want %v", tc.name, err, errCopyDoesNotParse)
		}
	}
}

func TestNodeIDTwoCopiesDiffer(t *testing.T) {
	d1, d2 := t.TempDir(), t.TempDir()
	putCopy(t, d1, encodeNodeID(nodeA))
	putCopy(t, d2, encodeNodeID(nodeB))

	_, err := NodeID(bootstrapOf(entity.NodeID{}, d1, d2))
	refused(t, "two copies that differ", err, errCopiesDiffer, d1, d2)
}

func TestNodeIDNoCopyNoKey(t *testing.T) {
	d1, d2 := t.TempDir(), t.TempDir()

	id, err := NodeID(bootstrapOf(entity.NodeID{}, d1, d2))
	refused(t, "no copy and no key", err, errNoIdentity, d1, d2)

	if id != (entity.NodeID{}) {
		t.Errorf("no copy and no key: got identity %x, want zero", id)
	}
}

func TestNodeIDBadCopy(t *testing.T) {
	good := encodeNodeID(nodeA)

	flipped := bytes.Clone(good)
	flipped[22] ^= 0x01

	for _, tc := range []struct {
		name string
		b    []byte
	}{
		{"empty", nil},
		{"torn at 20 bytes", good[:20]},
		{"a flipped bit", flipped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			bad, badJID := newJournal(t, install)
			other, otherJID := newJournal(t, install)
			record(t, s, badJID, nodeA, bad)
			record(t, s, otherJID, nodeA, other)

			putCopy(t, bad, tc.b)
			putCopy(t, other, good)

			// The host file names the identity the bad copy holds, or, for
			// the empty one, the good copy's: a bad copy taken as missing
			// would then pass.
			b := bootstrapOf(nodeA, bad, other)

			_, err := NodeID(b)
			refused(t, "NodeID over a copy "+tc.name, err, errCopyDoesNotParse, bad)

			log, _ := logs()
			err = writeMissing(t, s, b, nodeA, log)
			refused(t, "WriteMissingNodeIDs over a copy "+tc.name, err, errCopyDoesNotParse, bad)

			if got := copyBytes(t, bad); !bytes.Equal(got, tc.b) {
				t.Errorf("the copy %s was changed: got %x, want %x", tc.name, got, tc.b)
			}
		})
	}
}

func TestNodeIDAgainstHostFile(t *testing.T) {
	dir := t.TempDir()
	putCopy(t, dir, encodeNodeID(nodeA))

	_, err := NodeID(bootstrapOf(nodeB, dir))
	refused(t, "a copy that differs from the host file", err, errCopyDiffersFromHost, dir)

	id, err := NodeID(bootstrapOf(nodeA, dir))
	if err != nil {
		t.Fatalf("a copy equal to the host file: %v", err)
	}

	if id != nodeA {
		t.Errorf("a copy equal to the host file: got %x, want %x", id, nodeA)
	}
}

func TestNodeIDIgnoresTmp(t *testing.T) {
	dir := t.TempDir()
	putCopy(t, dir, encodeNodeID(nodeA))

	tmp := filepath.Join(dir, nodeIDTmpName)
	if err := os.WriteFile(tmp, encodeNodeID(nodeB), 0o644); err != nil {
		t.Fatal(err)
	}

	id, err := NodeID(bootstrapOf(entity.NodeID{}, dir))
	if err != nil {
		t.Fatalf("a copy beside a node-id.tmp: %v", err)
	}

	if id != nodeA {
		t.Errorf("a copy beside a node-id.tmp: got %x, want %x", id, nodeA)
	}
}

func TestNodeIDUnreadableCopy(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(t *testing.T, path string)
		want error
	}{
		{"a directory", func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}, errCopyUnreadable},
		{"a 4096-byte file", func(t *testing.T, path string) {
			if err := os.WriteFile(path, make([]byte, 4096), 0o644); err != nil {
				t.Fatal(err)
			}
		}, errCopyDoesNotParse},
		{"a valid copy with one byte more", func(t *testing.T, path string) {
			b := append(encodeNodeID(nodeA), 0)
			if err := os.WriteFile(path, b, 0o644); err != nil {
				t.Fatal(err)
			}
		}, errCopyDoesNotParse},
		{"an empty file", func(t *testing.T, path string) {
			if err := os.WriteFile(path, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}, errCopyDoesNotParse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unreadableCopy(t, tc.make, tc.want)
		})
	}
}

// unreadableCopy puts what mk makes at a directory's node-id, beside a good
// copy elsewhere and with the host file naming that copy's identity, so a
// copy taken as missing would pass, and checks that NodeID refuses with want,
// naming the directory, and leaves it in place.
func unreadableCopy(t *testing.T, mk func(t *testing.T, path string), want error) {
	t.Helper()

	good := t.TempDir()
	putCopy(t, good, encodeNodeID(nodeA))

	dir := t.TempDir()
	path := filepath.Join(dir, nodeIDName)
	mk(t, path)

	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	_, err = NodeID(bootstrapOf(nodeA, good, dir))
	refused(t, "NodeID", err, want, dir)

	after, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("the copy is gone: %v", err)
	}

	if after.Mode() != before.Mode() || after.Size() != before.Size() {
		t.Errorf("the copy changed: mode %v size %d, want mode %v size %d",
			after.Mode(), after.Size(), before.Mode(), before.Size())
	}
}

func TestWriteMissingWritesBesideARecordedJournal(t *testing.T) {
	s := newStore(t)
	d1, j1 := newJournal(t, install)
	d2, j2 := newJournal(t, install)
	record(t, s, j1, nodeA, d1)
	record(t, s, j2, nodeA, d2)

	putCopy(t, d2, encodeNodeID(nodeA))

	log, out := logs()
	if err := writeMissing(t, s, bootstrapOf(entity.NodeID{}, d1, d2), nodeA, log); err != nil {
		t.Fatalf("WriteMissingNodeIDs: %v", err)
	}

	if got := copyBytes(t, d1); !bytes.Equal(got, encodeNodeID(nodeA)) {
		t.Errorf("the written copy: got %x, want %x", got, encodeNodeID(nodeA))
	}

	if n := strings.Count(out.String(), "node-id copy written"); n != 1 {
		t.Errorf("log lines for one write: got %d, want 1 in %q", n, out)
	}

	if !strings.Contains(out.String(), "dir="+d1) {
		t.Errorf("the log %q does not name %s", out, d1)
	}

	// Every copy lost: the host file names the identity, and the start goes
	// on and writes them all back.
	for _, d := range []string{d1, d2} {
		if err := os.Remove(filepath.Join(d, nodeIDName)); err != nil {
			t.Fatal(err)
		}
	}

	b := bootstrapOf(nodeA, d1, d2)

	id, err := NodeID(b)
	if err != nil {
		t.Fatalf("NodeID with every copy lost and the host file naming it: %v", err)
	}

	if id != nodeA {
		t.Errorf("NodeID: got %x, want %x", id, nodeA)
	}

	out.Reset()

	if err := writeMissing(t, s, b, id, log); err != nil {
		t.Fatalf("WriteMissingNodeIDs: %v", err)
	}

	for _, d := range []string{d1, d2} {
		if got := copyBytes(t, d); !bytes.Equal(got, encodeNodeID(nodeA)) {
			t.Errorf("%s: got %x, want %x", d, got, encodeNodeID(nodeA))
		}
	}

	if n := strings.Count(out.String(), "node-id copy written"); n != 2 {
		t.Errorf("log lines for two writes: got %d, want 2 in %q", n, out)
	}
}

func TestWriteMissingSkipsAReplacedDevice(t *testing.T) {
	s := newStore(t)
	kept, keptJID := newJournal(t, install)
	record(t, s, keptJID, nodeA, kept)
	putCopy(t, kept, encodeNodeID(nodeA))

	// The store still records the old journal of the replaced device, whose
	// directory is empty until its re-initialisation.
	replaced := t.TempDir()
	record(t, s, [16]byte{'o', 'l', 'd'}, nodeA, replaced)

	b := bootstrapOf(nodeA, kept, replaced)

	log, _ := logs()
	if err := writeMissing(t, s, b, nodeA, log); err != nil {
		t.Fatalf("WriteMissingNodeIDs: %v", err)
	}

	noCopy(t, replaced)

	_, err := journal.Init(context.Background(), replaced, install, journaltest.SuitableMount())
	if err != nil {
		t.Fatalf("the re-initialisation's Init: %v", err)
	}

	id, err := InitNodeID(context.Background(), b, install, replaced)
	if err != nil {
		t.Fatalf("InitNodeID: %v", err)
	}

	if id != nodeA {
		t.Errorf("InitNodeID: got %x, want %x", id, nodeA)
	}

	if got := copyBytes(t, replaced); !bytes.Equal(got, encodeNodeID(nodeA)) {
		t.Errorf("the re-initialised copy: got %x, want %x", got, encodeNodeID(nodeA))
	}
}

func TestWriteMissingSkipsAJournalNotRecorded(t *testing.T) {
	t.Run("the store records another journal", func(t *testing.T) {
		s := newStore(t)
		dir, _ := newJournal(t, install)
		record(t, s, [16]byte{'o', 't', 'h', 'e', 'r'}, nodeA, dir)

		log, _ := logs()
		if err := writeMissing(t, s, bootstrapOf(nodeA, dir), nodeA, log); err != nil {
			t.Fatalf("WriteMissingNodeIDs: %v", err)
		}

		noCopy(t, dir)
	})

	t.Run("a new identity the store records nothing under", func(t *testing.T) {
		s := newStore(t)
		d1, j1 := newJournal(t, install)
		d2, j2 := newJournal(t, install)
		record(t, s, j1, nodeA, d1)
		record(t, s, j2, nodeA, d2)

		log, _ := logs()
		if err := writeMissing(t, s, bootstrapOf(nodeB, d1, d2), nodeB, log); err != nil {
			t.Fatalf("WriteMissingNodeIDs: %v", err)
		}

		noCopy(t, d1)
		noCopy(t, d2)
	})
}

// appearingDir is a journal directory in which a copy appears just before
// the link publishes one, as when another process writes it first.
type appearingDir struct {
	osDir
	appears []byte
}

func (d appearingDir) Link(oldName, newName string) error {
	if err := os.WriteFile(filepath.Join(string(d.osDir), newName), d.appears, 0o644); err != nil {
		return err
	}

	return d.osDir.Link(oldName, newName)
}

func TestWriteNeverReplacesACopy(t *testing.T) {
	entries := []struct {
		name string
		run  func(t *testing.T, dir string, d copyDir) error
	}{
		{"InitNodeID", func(t *testing.T, dir string, d copyDir) error {
			_, err := initNodeID(context.Background(), bootstrapOf(nodeA, dir), install, dir, d)

			return err
		}},
		{"WriteMissingNodeIDs", func(t *testing.T, dir string, d copyDir) error {
			s := newStore(t)

			_, jid, err := journalIDs(dir)
			if err != nil {
				t.Fatal(err)
			}

			record(t, s, jid, nodeA, dir)

			log, _ := logs()

			return writeMissingNodeIDs(context.Background(), bootstrapOf(nodeA, dir), nodeA,
				s.Lifecycle().JournalID, func(string) copyDir {
					return d
				}, log)
		}},
	}

	for _, e := range entries {
		t.Run(e.name+", another identity appears", func(t *testing.T) {
			dir, _ := newJournal(t, install)
			appears := encodeNodeID(nodeB)

			err := e.run(t, dir, appearingDir{osDir: osDir(dir), appears: appears})
			refused(t, e.name, err, errCopiesDiffer, dir)

			if got := copyBytes(t, dir); !bytes.Equal(got, appears) {
				t.Errorf("the copy that appeared: got %x, want %x", got, appears)
			}
		})

		t.Run(e.name+", the same identity appears", func(t *testing.T) {
			dir, _ := newJournal(t, install)
			appears := encodeNodeID(nodeA)

			if err := e.run(t, dir, appearingDir{osDir: osDir(dir), appears: appears}); err != nil {
				t.Fatalf("%s: got %v, want nil", e.name, err)
			}

			if got := copyBytes(t, dir); !bytes.Equal(got, appears) {
				t.Errorf("the copy that appeared: got %x, want %x", got, appears)
			}
		})
	}
}

// journalIDs reads dir's format for a test's store.
func journalIDs(dir string) (journal.InstallationID, [16]byte, error) {
	jid, inst, err := journal.ReadFormat(context.Background(), dir)

	return inst, jid, err
}

func TestWriteDoesNotFollowASymlinkedTmp(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir, _ := newJournal(t, install)
	if err := os.Symlink(target, filepath.Join(dir, nodeIDTmpName)); err != nil {
		t.Fatal(err)
	}

	_, err := InitNodeID(context.Background(), bootstrapOf(nodeA, dir), install, dir)
	if err != nil {
		t.Fatalf("InitNodeID over a symlinked node-id.tmp: %v", err)
	}

	if got, err := os.ReadFile(target); err != nil || string(got) != "keep" {
		t.Errorf("the symlink's target: got %q, %v, want %q", got, err, "keep")
	}

	fi, err := os.Lstat(filepath.Join(dir, nodeIDName))
	if err != nil {
		t.Fatal(err)
	}

	if !fi.Mode().IsRegular() {
		t.Errorf("node-id: got mode %v, want a regular file", fi.Mode())
	}

	if got := copyBytes(t, dir); !bytes.Equal(got, encodeNodeID(nodeA)) {
		t.Errorf("node-id: got %x, want %x", got, encodeNodeID(nodeA))
	}
}

func TestWriteDoesNotTruncateThroughALeftoverLink(t *testing.T) {
	dir := t.TempDir()
	putCopy(t, dir, encodeNodeID(nodeA))

	// A crash between the link and the temporary's removal leaves
	// node-id.tmp as a second name of the published copy.
	published, tmp := filepath.Join(dir, nodeIDName), filepath.Join(dir, nodeIDTmpName)
	if err := os.Link(published, tmp); err != nil {
		t.Fatal(err)
	}

	_, err := writeCopy(osDir(dir), dir, nodeB)
	refused(t, "a write over a leftover link", err, errCopiesDiffer, dir)

	if got := copyBytes(t, dir); !bytes.Equal(got, encodeNodeID(nodeA)) {
		t.Errorf("the published copy: got %x, want %x", got, encodeNodeID(nodeA))
	}
}

// failingLinkDir is a journal directory whose link fails with err.
type failingLinkDir struct {
	osDir
	err error
}

func (d failingLinkDir) Link(oldName, newName string) error {
	return &os.LinkError{Op: "link", Old: oldName, New: newName, Err: d.err}
}

func TestWriteRefusesAFailedLink(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EPERM, syscall.ENOTSUP} {
		dir := t.TempDir()

		_, err := writeCopy(failingLinkDir{osDir: osDir(dir), err: errno}, dir, nodeA)
		refused(t, "a link failing with "+errno.Error(), err, errLinkFailed, dir)

		if !errors.Is(err, errno) {
			t.Errorf("a link failing with %v: got %v, want it wrapped", errno, err)
		}

		noCopy(t, dir)
	}
}

// plantingDir is a journal directory in which a leftover temporary comes back
// in the moment its removal unlinks it: another process writes one, or a
// crash between two starts leaves one, once this process decided the
// temporary was gone.
type plantingDir struct {
	osDir
	plant func() error
}

func (d plantingDir) Remove(name string) error {
	err := d.osDir.Remove(name)

	if name == nodeIDTmpName {
		_ = d.plant()
	}

	return err
}

func TestWriteRefusesATemporaryPlantedAfterTheRemove(t *testing.T) {
	victimDir := t.TempDir()
	victim := filepath.Join(victimDir, "victim")

	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatalf("write the victim: %v", err)
	}

	entries := []struct {
		name  string
		plant func(dir string) error
	}{
		{"a second name of the published copy", func(dir string) error {
			published := filepath.Join(dir, nodeIDName)

			return os.Link(published, filepath.Join(dir, nodeIDTmpName))
		}},
		{"a symbolic link to a victim file", func(dir string) error {
			return os.Symlink(victim, filepath.Join(dir, nodeIDTmpName))
		}},
	}

	for _, e := range entries {
		t.Run(e.name, func(t *testing.T) {
			dir := t.TempDir()
			putCopy(t, dir, encodeNodeID(nodeA))

			d := plantingDir{osDir: osDir(dir), plant: func() error { return e.plant(dir) }}

			// The exclusive create refuses the planted temporary, whoever
			// holds the other name of it: truncating one would write through
			// a second name of the published copy or a symbolic link.
			_, err := writeCopy(d, dir, nodeB)
			if !errors.Is(err, fs.ErrExist) {
				t.Fatalf(
					"%s: writeCopy: got %v, want the create to refuse an existing temporary",
					e.name,
					err,
				)
			}

			want := encodeNodeID(nodeA)
			if got := copyBytes(t, dir); !bytes.Equal(got, want) {
				t.Errorf("%s: the published copy: got %x, want %x", e.name, got, want)
			}

			got, err := os.ReadFile(victim)
			if err != nil || string(got) != "keep" {
				t.Errorf("%s: the victim: got %q, %v, want %q", e.name, got, err, "keep")
			}
		})
	}
}

func TestWriteRefusesTheZeroIdentity(t *testing.T) {
	dir := t.TempDir()

	_, err := writeCopy(osDir(dir), dir, entity.NodeID{})
	if !errors.Is(err, errCopyDoesNotParse) {
		t.Fatalf("the zero identity: got %v, want %v", err, errCopyDoesNotParse)
	}

	noCopy(t, dir)
}

func TestInitNodeIDGenerates(t *testing.T) {
	dir, _ := newJournal(t, install)

	id, err := InitNodeID(context.Background(), bootstrapOf(entity.NodeID{}, dir), install, dir)
	if err != nil {
		t.Fatalf("InitNodeID: %v", err)
	}

	if id == (entity.NodeID{}) {
		t.Fatal("InitNodeID generated the zero identity")
	}

	got, err := parseNodeID(copyBytes(t, dir))
	if err != nil {
		t.Fatalf("the written copy: %v", err)
	}

	if got != id {
		t.Errorf("the written copy: got %x, want %x", got, id)
	}
}

func TestInitNodeIDRerunAdopts(t *testing.T) {
	other, _ := newJournal(t, foreign)
	putCopy(t, other, encodeNodeID(nodeB))

	ours, _ := newJournal(t, install)
	putCopy(t, ours, encodeNodeID(nodeA))

	target, _ := newJournal(t, install)

	id, err := InitNodeID(
		context.Background(),
		bootstrapOf(entity.NodeID{}, other, ours, target),
		install,
		target,
	)
	if err != nil {
		t.Fatalf("InitNodeID: %v", err)
	}

	if id != nodeA {
		t.Errorf("InitNodeID: got %x, want this installation's %x", id, nodeA)
	}

	if got := copyBytes(t, target); !bytes.Equal(got, encodeNodeID(nodeA)) {
		t.Errorf("the written copy: got %x, want %x", got, encodeNodeID(nodeA))
	}
}

func TestInitNodeIDRefusesBadCopies(t *testing.T) {
	t.Run("a copy of this installation that does not parse", func(t *testing.T) {
		ours, _ := newJournal(t, install)
		putCopy(t, ours, encodeNodeID(nodeA)[:20])
		target, _ := newJournal(t, install)

		_, err := InitNodeID(
			context.Background(),
			bootstrapOf(entity.NodeID{}, ours, target),
			install,
			target,
		)
		refused(t, "a torn copy", err, errCopyDoesNotParse, ours)
		noCopy(t, target)
	})

	t.Run("two copies of this installation that differ", func(t *testing.T) {
		d1, _ := newJournal(t, install)
		d2, _ := newJournal(t, install)
		putCopy(t, d1, encodeNodeID(nodeA))
		putCopy(t, d2, encodeNodeID(nodeB))
		target, _ := newJournal(t, install)

		_, err := InitNodeID(
			context.Background(),
			bootstrapOf(entity.NodeID{}, d1, d2, target),
			install,
			target,
		)
		refused(t, "two copies that differ", err, errCopiesDiffer, d1, d2)
		noCopy(t, target)
	})

	t.Run("a copy that differs from the host file", func(t *testing.T) {
		ours, _ := newJournal(t, install)
		putCopy(t, ours, encodeNodeID(nodeA))
		target, _ := newJournal(t, install)

		_, err := InitNodeID(
			context.Background(),
			bootstrapOf(nodeB, ours, target),
			install,
			target,
		)
		refused(t, "a copy that differs from the host file", err, errCopyDiffersFromHost, ours)
		noCopy(t, target)
	})

	t.Run("a torn copy in another installation's directory", func(t *testing.T) {
		other, _ := newJournal(t, foreign)
		putCopy(t, other, encodeNodeID(nodeB)[:20])
		ours, _ := newJournal(t, install)
		putCopy(t, ours, encodeNodeID(nodeA))
		target, _ := newJournal(t, install)

		id, err := InitNodeID(
			context.Background(),
			bootstrapOf(entity.NodeID{}, other, ours, target),
			install,
			target,
		)
		if err != nil {
			t.Fatalf("InitNodeID: %v", err)
		}

		if id != nodeA {
			t.Errorf("InitNodeID: got %x, want %x", id, nodeA)
		}
	})
}

func TestInitNodeIDOnAForeignOrUnreadableDir(t *testing.T) {
	other, _ := newJournal(t, foreign)

	_, err := InitNodeID(context.Background(), bootstrapOf(nodeA, other), install, other)
	refused(t, "another installation's directory", err, errForeignDir, other)
	noCopy(t, other)

	empty := t.TempDir()

	_, err = InitNodeID(context.Background(), bootstrapOf(nodeA, empty), install, empty)
	refused(t, "a directory with no format file", err, errNoFormat, empty)
	noCopy(t, empty)
}

// seamDir is a journal directory reached through the journal's
// crash-simulating seam.
type seamDir struct {
	s *kit.Seam
}

func (d seamDir) Remove(name string) error {
	return d.s.Remove(name)
}

func (d seamDir) Create(name string) (copyFile, error) {
	f, err := d.s.Create(name)
	if err != nil {
		return nil, err
	}

	return f, nil
}

func (d seamDir) Link(oldName, newName string) error {
	return d.s.Link(oldName, newName)
}

func (d seamDir) SyncDir() error {
	return d.s.SyncDir()
}

var errCrashed = errors.New("the process crashed")

// crashingDir crashes the seam right after the step at, and fails every call
// from then on, as the dead process would.
type crashingDir struct {
	seamDir
	at      string
	removes *int
	crashed *bool
}

func (d crashingDir) after(step string) error {
	if *d.crashed {
		return errCrashed
	}

	if step != d.at {
		return nil
	}

	*d.crashed = true
	if err := d.s.Crash(); err != nil {
		return err
	}

	return errCrashed
}

func (d crashingDir) Remove(name string) error {
	if *d.crashed {
		return errCrashed
	}

	if err := d.seamDir.Remove(name); err != nil {
		return err
	}

	*d.removes++
	if *d.removes == 2 {
		return d.after("remove")
	}

	return nil
}

func (d crashingDir) Create(name string) (copyFile, error) {
	if *d.crashed {
		return nil, errCrashed
	}

	f, err := d.seamDir.Create(name)
	if err != nil {
		return nil, err
	}

	return crashingFile{copyFile: f, d: d}, nil
}

func (d crashingDir) Link(oldName, newName string) error {
	if *d.crashed {
		return errCrashed
	}

	if err := d.seamDir.Link(oldName, newName); err != nil {
		return err
	}

	return d.after("link")
}

func (d crashingDir) SyncDir() error {
	if *d.crashed {
		return errCrashed
	}

	if err := d.seamDir.SyncDir(); err != nil {
		return err
	}

	return d.after("syncdir")
}

type crashingFile struct {
	copyFile
	d crashingDir
}

func (f crashingFile) WriteAt(p []byte, off int64) (int, error) {
	if *f.d.crashed {
		return 0, errCrashed
	}

	n, err := f.copyFile.WriteAt(p, off)
	if err != nil {
		return n, err
	}

	return n, f.d.after("write")
}

func (f crashingFile) Sync() error {
	if *f.d.crashed {
		return errCrashed
	}

	if err := f.copyFile.Sync(); err != nil {
		return err
	}

	return f.d.after("sync")
}

func TestNodeIDWriteCrashes(t *testing.T) {
	want := encodeNodeID(nodeA)

	for _, at := range []string{"write", "sync", "link", "remove", "syncdir", "returned"} {
		t.Run("a crash after the "+at, func(t *testing.T) {
			dir := t.TempDir()

			// A leftover from an earlier crash, durable before the seam
			// starts: the write removes it and writes its own.
			stale := filepath.Join(dir, nodeIDTmpName)
			if err := os.WriteFile(stale, []byte("stale"), 0o644); err != nil {
				t.Fatal(err)
			}

			s, err := kit.NewSeam(dir)
			if err != nil {
				t.Fatal(err)
			}

			var (
				removes int
				crashed bool
			)

			_, err = writeCopy(
				crashingDir{seamDir: seamDir{s}, at: at, removes: &removes, crashed: &crashed},
				dir,
				nodeA,
			)

			if at == "returned" {
				if err != nil {
					t.Fatalf("writeCopy: %v", err)
				}

				if err := s.Crash(); err != nil {
					t.Fatal(err)
				}

				if got := copyBytes(t, dir); !bytes.Equal(got, want) {
					t.Fatalf("a crash after the write returned: got %x, want %x", got, want)
				}

				return
			}

			if !errors.Is(err, errCrashed) {
				t.Fatalf("writeCopy: got %v, want the crash", err)
			}

			got, err := os.ReadFile(filepath.Join(dir, nodeIDName))
			if err == nil && !bytes.Equal(got, want) {
				t.Fatalf("a crash after the %s left a torn copy %x, want none or %x", at, got, want)
			}

			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Fatal(err)
			}

			if _, err := writeCopy(seamDir{s}, dir, nodeA); err != nil {
				t.Fatalf("the write after the crash: %v", err)
			}

			if err := s.Crash(); err != nil {
				t.Fatal(err)
			}

			if got := copyBytes(t, dir); !bytes.Equal(got, want) {
				t.Errorf("after the rewrite and a crash: got %x, want %x", got, want)
			}

			if _, err := os.Lstat(filepath.Join(dir, nodeIDTmpName)); !errors.Is(
				err,
				fs.ErrNotExist,
			) {
				t.Errorf("node-id.tmp after the rewrite: got %v, want it removed", err)
			}
		})
	}
}

func TestWriteMissingLogsACopyAlreadyThere(t *testing.T) {
	s := newStore(t)
	dir, jid := newJournal(t, install)
	record(t, s, jid, nodeA, dir)

	log, out := logs()

	err := writeMissingNodeIDs(context.Background(), bootstrapOf(nodeA, dir), nodeA,
		s.Lifecycle().JournalID, func(string) copyDir {
			return appearingDir{osDir: osDir(dir), appears: encodeNodeID(nodeA)}
		}, log)
	if err != nil {
		t.Fatalf("WriteMissingNodeIDs: %v", err)
	}

	if strings.Contains(out.String(), "node-id copy written") {
		t.Errorf("the log %q says this process wrote the copy, which another wrote first", out)
	}

	if !strings.Contains(out.String(), "node-id copy already written by another process") {
		t.Errorf("the log %q does not say the copy was already there", out)
	}
}

func TestWriteMissingRefusals(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	t.Run("a cancelled context", func(t *testing.T) {
		s := newStore(t)
		dir, jid := newJournal(t, install)
		record(t, s, jid, nodeA, dir)

		log, _ := logs()

		err := writeMissingNodeIDs(cancelled, bootstrapOf(nodeA, dir), nodeA,
			s.Lifecycle().JournalID, func(d string) copyDir {
				return osDir(d)
			}, log)
		refused(t, "a cancelled context", err, context.Canceled, dir)
		noCopy(t, dir)
	})

	t.Run("a damaged format file", func(t *testing.T) {
		s := newStore(t)
		dir, jid := newJournal(t, install)
		record(t, s, jid, nodeA, dir)
		damageFormat(t, dir)

		log, _ := logs()
		if err := writeMissing(t, s, bootstrapOf(nodeA, dir), nodeA, log); err != nil {
			t.Fatalf("WriteMissingNodeIDs over a damaged format: got %v, want nil", err)
		}

		noCopy(t, dir)
	})

	t.Run("segments and no format file", func(t *testing.T) {
		s := newStore(t)
		dir := t.TempDir()
		record(t, s, [16]byte{'s', 'e', 'g'}, nodeA, dir)

		segment := filepath.Join(dir, "00000000000000000001.seg")
		if err := os.WriteFile(segment, nil, 0o644); err != nil {
			t.Fatal(err)
		}

		log, _ := logs()
		if err := writeMissing(t, s, bootstrapOf(nodeA, dir), nodeA, log); err != nil {
			t.Fatalf("WriteMissingNodeIDs over segments with no format: got %v, want nil", err)
		}

		noCopy(t, dir)
	})

	t.Run("an unknown format version", func(t *testing.T) {
		s := newStore(t)
		dir, jid := newJournal(t, install)
		record(t, s, jid, nodeA, dir)
		flipFormat(t, dir, 4)

		log, _ := logs()
		if err := writeMissing(t, s, bootstrapOf(nodeA, dir), nodeA, log); err != nil {
			t.Fatalf("WriteMissingNodeIDs over an unknown format version: got %v, want nil", err)
		}

		noCopy(t, dir)
	})

	t.Run("a copy of another identity", func(t *testing.T) {
		s := newStore(t)
		dir, jid := newJournal(t, install)
		record(t, s, jid, nodeA, dir)
		putCopy(t, dir, encodeNodeID(nodeB))

		log, _ := logs()
		err := writeMissing(t, s, bootstrapOf(nodeA, dir), nodeA, log)
		refused(t, "a copy of another identity", err, errCopiesDiffer, dir)

		if got := copyBytes(t, dir); !bytes.Equal(got, encodeNodeID(nodeB)) {
			t.Errorf("the other copy: got %x, want %x", got, encodeNodeID(nodeB))
		}
	})
}

func TestAdoptNodeIDRefusals(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	ours, _ := newJournal(t, install)

	_, err := adoptNodeID(cancelled, bootstrapOf(entity.NodeID{}, ours), install, ours)
	refused(t, "a cancelled context", err, context.Canceled, ours)

	// A journal path that is a file: listing it fails with an error that is
	// none of the journal's "no usable journal" answers.
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = adoptNodeID(context.Background(), bootstrapOf(nodeA, notADir, ours), install, ours)
	if err == nil {
		t.Fatalf(
			"a directory whose format cannot be read: got nil, want a refusal naming %s",
			notADir,
		)
	}

	refused(t, "a directory whose format cannot be read", err, syscall.ENOTDIR, notADir)
}

// damageFormat flips one bit in dir's format file, inside the journal's
// identity, so it no longer verifies.
func damageFormat(t *testing.T, dir string) {
	t.Helper()

	flipFormat(t, dir, 20)
}

// flipFormat flips one bit in the byte at off of dir's format file.
func flipFormat(t *testing.T, dir string, off int) {
	t.Helper()

	path := filepath.Join(dir, "format")

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	b[off] ^= 0x01
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestWriteMissingNeverCountsAZeroRecordedID writes, by hand, a format file
// that names the zero journal ID, which the format's decoder accepts: a store
// that records no journal for the device answers zero too, and must not match.
func TestWriteMissingNeverCountsAZeroRecordedID(t *testing.T) {
	dir := t.TempDir()

	// RFC 28 §2.1's format file: the magic, version 1, two reserved bytes,
	// the journal ID (zero here), the installation, the generation and four
	// reserved bytes, all zero, then the CRC32C of the 52 bytes before it.
	f := append([]byte("DJF1"), 1, 0, 0, 0)
	f = append(f, make([]byte, 16)...)
	f = append(f, install[:]...)
	f = append(f, make([]byte, 12)...)
	f = binary.LittleEndian.AppendUint32(f, crc32.Checksum(f, crc32.MakeTable(crc32.Castagnoli)))

	if err := os.WriteFile(filepath.Join(dir, "format"), f, 0o644); err != nil {
		t.Fatal(err)
	}

	jid, _, err := journal.ReadFormat(context.Background(), dir)
	if err != nil || jid != (journal.JournalID{}) {
		t.Fatalf(
			"the handmade format: got ID %x, error %v, want the zero ID and no error",
			jid,
			err,
		)
	}

	s := newStore(t)

	log, _ := logs()
	if err := writeMissing(t, s, bootstrapOf(nodeA, dir), nodeA, log); err != nil {
		t.Fatalf("WriteMissingNodeIDs: %v", err)
	}

	noCopy(t, dir)
}

// swappingDir is a journal directory in which another process removes the
// temporary and writes its own just before this process links it.
type swappingDir struct {
	osDir
	theirs []byte
}

func (d swappingDir) Link(oldName, newName string) error {
	tmp := filepath.Join(string(d.osDir), oldName)
	if err := os.Remove(tmp); err != nil {
		return err
	}

	if err := os.WriteFile(tmp, d.theirs, 0o644); err != nil {
		return err
	}

	return d.osDir.Link(oldName, newName)
}

func TestWriteRefusesATemporaryReplacedBeforeTheLink(t *testing.T) {
	dir := t.TempDir()
	theirs := encodeNodeID(nodeB)

	_, err := writeCopy(swappingDir{osDir: osDir(dir), theirs: theirs}, dir, nodeA)
	refused(t, "a temporary replaced before the link", err, errCopiesDiffer, dir)

	if got := copyBytes(t, dir); !bytes.Equal(got, theirs) {
		t.Errorf("the published copy: got %x, want the other process's %x", got, theirs)
	}
}

func TestSyncDirOfAJournalDirectory(t *testing.T) {
	dir := t.TempDir()

	if err := osDir(dir).SyncDir(); err != nil {
		t.Fatalf("SyncDir of %s: %v", dir, err)
	}

	if err := osDir(filepath.Join(dir, "gone")).SyncDir(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("SyncDir of a missing directory: got %v, want fs.ErrNotExist", err)
	}
}

// zeroIdentity is a node-id copy of the zero identity, its checksum
// verifying, so only the zero check refuses it.
func zeroIdentity() []byte {
	b := append([]byte("DNI1"), make([]byte, 16)...)

	return binary.LittleEndian.AppendUint32(b, crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli)))
}

// A journal path that is a file fails the copy's open with an error that is
// not "no such file": only a missing copy is no copy, so the start refuses.
func TestNodeIDCopyThatCannotBeOpened(t *testing.T) {
	good := t.TempDir()
	putCopy(t, good, encodeNodeID(nodeA))

	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := NodeID(bootstrapOf(nodeA, good, notADir))
	refused(t, "a copy that cannot be opened", err, errCopyUnreadable, notADir)
}
