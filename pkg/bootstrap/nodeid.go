package bootstrap

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/cubbit/dittofs/pkg/internal/dirsync"
	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// The node-id file's layout, RFC 28 §2.1: the magic, the node's identity, and
// the CRC32C of the bytes before it, little-endian, not seeded.
const (
	nodeIDName      = journal.NodeIDFileName
	nodeIDTmpName   = journal.NodeIDTmpName
	nodeIDMagic     = "DNI1"
	nodeIDCRCOffset = 20
	nodeIDSize      = 24
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// The node identity's refusals, one per reason. Each is wrapped with the
// directories it names.
var (
	errCopiesDiffer        = errors.New("node-id copies differ")
	errNoIdentity          = errors.New("no node-id copy and no node identity in the host file")
	errCopyDoesNotParse    = errors.New("node-id copy does not parse")
	errCopyUnreadable      = errors.New("node-id copy cannot be read")
	errCopyDiffersFromHost = errors.New("node-id copy differs from the host file's node identity")
	errLinkFailed          = errors.New("node-id copy cannot be published")
	errForeignDir          = errors.New("journal directory belongs to another installation")
	errNoFormat            = errors.New("journal directory has no readable format file")
)

// NodeID reads every journal directory's node-id copy and compares them, with b's
// identity if it names one. Before the store opens; it writes nothing.
func NodeID(b *Bootstrap) (entity.NodeID, error) {
	var (
		id     entity.NodeID
		from   string
		read   []string
		differ []string
	)

	for _, j := range b.Journals {
		read = append(read, j.Path)

		got, ok, err := readCopy(j.Path)
		if err != nil {
			return entity.NodeID{}, err
		}

		if !ok {
			continue
		}

		if from != "" && got != id {
			return entity.NodeID{}, fmt.Errorf("%w: %s and %s", errCopiesDiffer, from, j.Path)
		}

		if b.Node.ID != (entity.NodeID{}) && got != b.Node.ID {
			differ = append(differ, j.Path)
		}

		id, from = got, j.Path
	}

	switch {
	case len(differ) > 0:
		return entity.NodeID{}, fmt.Errorf(
			"%w: %s",
			errCopyDiffersFromHost,
			strings.Join(differ, ", "),
		)
	case from != "":
		return id, nil
	case b.Node.ID != (entity.NodeID{}):
		return b.Node.ID, nil
	default:
		return entity.NodeID{}, fmt.Errorf("%w: read %s", errNoIdentity, strings.Join(read, ", "))
	}
}

// InitNodeID adopts or generates the identity and writes it into one journal
// directory, between that journal's Init and RecordJournal, for the initialisation
// step and a recovery import. Called once per directory.
func InitNodeID(
	ctx context.Context,
	b *Bootstrap,
	installation [16]byte,
	dir string,
) (entity.NodeID, error) {
	return initNodeID(ctx, b, installation, dir, osDir(dir))
}

// WriteMissingNodeIDs writes a missing copy only beside a journal the store records
// for this node and device, logging each write to log. journalID answers the
// journal the store records for a node's device, zero when it records none.
func WriteMissingNodeIDs(
	ctx context.Context,
	b *Bootstrap,
	id entity.NodeID,
	journalID func(
		ctx context.Context,
		node entity.NodeID,
		device entity.JournalDir,
	) ([16]byte, error),
	log *slog.Logger,
) error {
	// The zero identity names no node, so no copy can be written for it.
	if id == (entity.NodeID{}) {
		return fmt.Errorf("%w: the node's identity is zero", errNoIdentity)
	}

	return writeMissingNodeIDs(ctx, b, id, journalID, func(dir string) copyDir {
		return osDir(dir)
	}, log)
}

func initNodeID(
	ctx context.Context,
	b *Bootstrap,
	installation [16]byte,
	dir string,
	d copyDir,
) (entity.NodeID, error) {
	// A directory with no format file means its Init did not run, and one of
	// another installation belongs to a journal this initialisation does not
	// record: a copy written there would name this node beside a journal the
	// store never maps to it.
	_, inst, err := journal.ReadFormat(ctx, dir)
	if err != nil {
		return entity.NodeID{}, fmt.Errorf("%w: %s: %w", errNoFormat, dir, err)
	}

	if [16]byte(inst) != installation {
		return entity.NodeID{}, fmt.Errorf("%w: %s", errForeignDir, dir)
	}

	id, err := adoptNodeID(ctx, b, installation, dir)
	if err != nil {
		return entity.NodeID{}, err
	}

	if _, err := writeCopy(d, dir, id); err != nil {
		return entity.NodeID{}, err
	}

	return id, nil
}

// adoptNodeID returns the identity a rerun adopts from a copy in a directory
// of this installation, else the host file's, else a new one. A directory
// that holds no usable journal, or another installation's, is not read; any
// other failure to read its format refuses.
func adoptNodeID(
	ctx context.Context,
	b *Bootstrap,
	installation [16]byte,
	dir string,
) (entity.NodeID, error) {
	dirs := make([]string, 0, len(b.Journals)+1)
	for _, j := range b.Journals {
		dirs = append(dirs, j.Path)
	}

	if !slices.Contains(dirs, dir) {
		dirs = append(dirs, dir)
	}

	var (
		id   entity.NodeID
		from string
	)

	for _, d := range dirs {
		_, inst, err := journal.ReadFormat(ctx, d)
		if noUsableJournal(err) {
			continue
		}

		if err != nil {
			return entity.NodeID{}, fmt.Errorf("journal directory %s: %w", d, err)
		}

		if [16]byte(inst) != installation {
			continue
		}

		got, ok, err := readCopy(d)
		if err != nil {
			return entity.NodeID{}, err
		}

		if !ok {
			continue
		}

		if from != "" && got != id {
			return entity.NodeID{}, fmt.Errorf("%w: %s and %s", errCopiesDiffer, from, d)
		}

		id, from = got, d
	}

	host := b.Node.ID
	switch {
	case from != "" && host != (entity.NodeID{}) && id != host:
		return entity.NodeID{}, fmt.Errorf("%w: %s", errCopyDiffersFromHost, from)
	case from != "":
		return id, nil
	case host != (entity.NodeID{}):
		return host, nil
	}

	for id == (entity.NodeID{}) {
		if _, err := rand.Read(id[:]); err != nil {
			return entity.NodeID{}, err
		}
	}

	return id, nil
}

func writeMissingNodeIDs(
	ctx context.Context,
	b *Bootstrap,
	id entity.NodeID,
	journalID func(
		ctx context.Context,
		node entity.NodeID,
		device entity.JournalDir,
	) ([16]byte, error),
	open func(dir string) copyDir,
	log *slog.Logger,
) error {
	for _, j := range b.Journals {
		dir := j.Path

		got, ok, err := readCopy(dir)
		if err != nil {
			return err
		}

		if ok {
			if got != id {
				return fmt.Errorf("%w: %s holds another identity", errCopiesDiffer, dir)
			}

			continue
		}

		jid, _, err := journal.ReadFormat(ctx, dir)
		if noUsableJournal(err) {
			continue
		}

		if err != nil {
			return fmt.Errorf("journal directory %s: %w", dir, err)
		}

		recorded, err := journalID(ctx, id, entity.JournalDir(dir))
		if err != nil {
			return fmt.Errorf("journal directory %s: %w", dir, err)
		}

		if recorded == ([16]byte{}) || recorded != [16]byte(jid) {
			continue
		}

		written, err := writeCopy(open(dir), dir, id)
		if err != nil {
			return err
		}

		if written {
			log.Info("node-id copy written", "dir", dir)
		} else {
			log.Info("node-id copy already written by another process", "dir", dir)
		}
	}

	return nil
}

// noUsableJournal reports a ReadFormat error that names no journal this
// binary can use in the directory: none, segments with no format file, or a
// format file damaged or of an unknown version. The engine's open refuses
// such a journal itself; any other error, a cancellation or a read the device
// did not complete, is the caller's to return.
func noUsableJournal(err error) bool {
	return errors.Is(err, journal.ErrNoJournal) ||
		errors.Is(err, journal.ErrUnidentified) ||
		errors.Is(err, journal.ErrCorrupt) ||
		errors.Is(err, journal.ErrUnknownFormat)
}

func encodeNodeID(id entity.NodeID) []byte {
	b := make([]byte, 0, nodeIDSize)
	b = append(b, nodeIDMagic...)
	b = append(b, id[:]...)

	return binary.LittleEndian.AppendUint32(b, crc32.Checksum(b, castagnoli))
}

func parseNodeID(b []byte) (entity.NodeID, error) {
	if len(b) != nodeIDSize {
		return entity.NodeID{}, fmt.Errorf(
			"%w: %d bytes, want %d",
			errCopyDoesNotParse,
			len(b),
			nodeIDSize,
		)
	}

	if string(b[:len(nodeIDMagic)]) != nodeIDMagic {
		return entity.NodeID{}, fmt.Errorf(
			"%w: magic %q, want %q",
			errCopyDoesNotParse,
			b[:4],
			nodeIDMagic,
		)
	}

	want := binary.LittleEndian.Uint32(b[nodeIDCRCOffset:])
	if got := crc32.Checksum(b[:nodeIDCRCOffset], castagnoli); got != want {
		return entity.NodeID{}, fmt.Errorf(
			"%w: checksum %08x, want %08x",
			errCopyDoesNotParse,
			got,
			want,
		)
	}

	// The zero identity names no node: the host file's absent identity reads
	// as zero, so a copy holding it would pass for none.
	id := entity.NodeID(b[len(nodeIDMagic):nodeIDCRCOffset])
	if id == (entity.NodeID{}) {
		return entity.NodeID{}, fmt.Errorf("%w: the zero identity", errCopyDoesNotParse)
	}

	return id, nil
}

// readCopy reads dir's copy. Only a missing file is no copy: one that cannot
// be read or does not parse refuses, naming dir, and is left as it is. At most
// one byte past the layout is read, so a longer file is seen as too long
// without being read whole.
func readCopy(dir string) (entity.NodeID, bool, error) {
	f, err := os.OpenFile(filepath.Join(dir, nodeIDName), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return entity.NodeID{}, false, nil
	}

	if err != nil {
		return entity.NodeID{}, false, fmt.Errorf("%w: %s: %w", errCopyUnreadable, dir, err)
	}

	// The copy is only read, so its close has nothing to report.
	defer func() {
		_ = f.Close()
	}()

	// Opened without blocking, a FIFO or a device would answer here instead
	// of hanging the open until a writer comes.
	info, err := f.Stat()
	if err != nil {
		return entity.NodeID{}, false, fmt.Errorf("%w: %s: %w", errCopyUnreadable, dir, err)
	}

	if !info.Mode().IsRegular() {
		return entity.NodeID{}, false, fmt.Errorf(
			"%w: %s: %s is not a regular file",
			errCopyUnreadable,
			dir,
			nodeIDName,
		)
	}

	buf := make([]byte, nodeIDSize+1)

	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return entity.NodeID{}, false, fmt.Errorf("%w: %s: %w", errCopyUnreadable, dir, err)
	}

	id, err := parseNodeID(buf[:n])
	if err != nil {
		return entity.NodeID{}, false, fmt.Errorf("journal directory %s: %w", dir, err)
	}

	return id, true, nil
}

// copyFile is the part of a file the copy's write uses.
type copyFile interface {
	WriteAt(p []byte, off int64) (int, error)
	Sync() error
	Close() error
}

// copyDir is the part of a journal directory the copy's write uses.
type copyDir interface {
	// Remove unlinks name.
	Remove(name string) error

	// Create creates name for writing, failing if it exists, which also
	// refuses to create through a symbolic link.
	Create(name string) (copyFile, error)

	// Link gives oldName the second name newName, failing if newName exists.
	Link(oldName, newName string) error

	// SyncDir makes the directory's changes durable.
	SyncDir() error
}

// writeCopy writes id's copy into dir, as RFC 28 §2.1 has the format file
// written, but published by a link so that it never replaces a copy. A leftover
// temporary is removed first rather than truncated: after a crash between the
// link and its removal it is a second name of the published copy. The zero
// identity is refused before anything is written. It reports
// false when another process published the same copy first.
func writeCopy(d copyDir, dir string, id entity.NodeID) (bool, error) {
	// The zero identity names no node: a published copy of it would pass for
	// none, as the read of the host file's absent identity does.
	if id == (entity.NodeID{}) {
		return false, fmt.Errorf(
			"journal directory %s: %w: the zero identity",
			dir,
			errCopyDoesNotParse,
		)
	}

	if err := d.Remove(nodeIDTmpName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("journal directory %s: remove %s: %w", dir, nodeIDTmpName, err)
	}

	f, err := d.Create(nodeIDTmpName)
	if err != nil {
		return false, fmt.Errorf("journal directory %s: create %s: %w", dir, nodeIDTmpName, err)
	}

	if _, err := f.WriteAt(encodeNodeID(id), 0); err != nil {
		_ = f.Close()

		return false, fmt.Errorf("journal directory %s: write %s: %w", dir, nodeIDTmpName, err)
	}

	if err := f.Sync(); err != nil {
		_ = f.Close()

		return false, fmt.Errorf("journal directory %s: sync %s: %w", dir, nodeIDTmpName, err)
	}

	if err := f.Close(); err != nil {
		return false, fmt.Errorf("journal directory %s: close %s: %w", dir, nodeIDTmpName, err)
	}

	written := true

	if err := d.Link(nodeIDTmpName, nodeIDName); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return false, fmt.Errorf("%w: %s: %w", errLinkFailed, dir, err)
		}

		written = false
	}

	// The published copy is read back, whoever linked it: a copy may have
	// appeared since the caller found none, or another process may have
	// replaced the temporary before this link. It stays as it is, and is
	// taken only if it names this identity.
	got, ok, err := readCopy(dir)
	if err != nil {
		return false, err
	}

	if !ok {
		return false, fmt.Errorf(
			"%w: %s: no readable %s after the link",
			errCopyUnreadable,
			dir,
			nodeIDName,
		)
	}

	if got != id {
		return false, fmt.Errorf("%w: %s holds another identity", errCopiesDiffer, dir)
	}

	if err := d.Remove(nodeIDTmpName); err != nil {
		return false, fmt.Errorf("journal directory %s: remove %s: %w", dir, nodeIDTmpName, err)
	}

	if err := d.SyncDir(); err != nil {
		return false, fmt.Errorf("journal directory %s: sync: %w", dir, err)
	}

	return written, nil
}

// osDir is a journal directory reached through the operating system.
type osDir string

func (d osDir) Remove(name string) error {
	return os.Remove(filepath.Join(string(d), name))
}

func (d osDir) Create(name string) (copyFile, error) {
	return os.OpenFile(filepath.Join(string(d), name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
}

func (d osDir) Link(oldName, newName string) error {
	return os.Link(filepath.Join(string(d), oldName), filepath.Join(string(d), newName))
}

func (d osDir) SyncDir() error {
	return dirsync.Sync(string(d))
}
