package journal

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/datapath"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/platform"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
)

// checkDir answers whether dir suits a journal: an error matching
// ErrUnsuitableStorage for a directory a rule of the platform profile
// refuses, the error met while checking as itself, nil for a suitable one.
// It is a variable so a test can inject an answer the platform of the host
// running it cannot name; production code never writes it.
var checkDir = platform.CheckDir

// resolveConfig resolves the options Init or Open was called with, over
// dir, the directory the call names. A negative setting, or a negative
// descriptor limit in a budget the caller passed, is refused with
// ErrInvalidSetting; each refusal names the setting it refused. Without
// WithStorage the journal reaches dir through the operating system's files.
//
// Nothing in the options is read but the memory it sets, so a refusal made
// here has touched no directory and made no system call.
func resolveConfig(dir string, opts []Option) (core.Config, error) {
	var cfg core.Config
	for _, o := range opts {
		if o.apply != nil {
			o.apply(&cfg)
		}
	}

	s := cfg.Settings
	for _, bad := range []struct {
		name string
		n    int64
	}{
		{"MaxBytes", s.MaxBytes},
		{"Streams", int64(s.Streams)},
		{"ExtentLimit", s.ExtentLimit},
		{"HeadroomRecords", int64(s.HeadroomRecords)},
		{"IdleSealBytes", s.IdleSealBytes},
		{"SpareTarget", int64(s.SpareTarget)},
	} {
		if bad.n < 0 {
			return core.Config{}, fmt.Errorf("%w: %s is negative", core.ErrInvalidSetting, bad.name)
		}
	}
	if cfg.Budget != nil && cfg.Budget.Limit() < 0 {
		return core.Config{}, fmt.Errorf("%w: the descriptor budget's limit is negative", core.ErrInvalidSetting)
	}

	if cfg.Storage == nil {
		cfg.Storage = platform.NewOS(dir)
	}
	return cfg, nil
}

// Init creates a new journal in dir, writing into its format file a fresh
// journal identity and install, the installation ID the same initialisation
// step records in the metadata store. It returns the identity, for the caller
// to record as the journal it expects in dir.
//
// Init reads no time and needs no clock. It is the only call that creates a
// journal, and only in an empty directory: it refuses, with ErrNotEmpty and
// writing nothing, a directory that holds any entry but a format.tmp, which
// an Init that crashed before its rename leaves. A negative setting is
// refused with ErrInvalidSetting.
func Init(ctx context.Context, dir string, install InstallationID, opts ...Option) (JournalID, error) {
	cfg, err := resolveConfig(dir, opts)
	if err != nil {
		return JournalID{}, err
	}
	if err := ctx.Err(); err != nil {
		return JournalID{}, err
	}
	if err := checkDir(dir); err != nil {
		return JournalID{}, err
	}

	names, err := cfg.Storage.List()
	if err != nil {
		return JournalID{}, err
	}
	staleTmp := false
	for _, name := range names {
		if name == format.FormatTmpName {
			staleTmp = true
			continue
		}
		return JournalID{}, fmt.Errorf("%w: %s holds %s", ErrNotEmpty, dir, name)
	}

	// A stale format.tmp is overwritten, never read or kept: the new
	// journal's whole format file is written under that name and renamed
	// over whatever the old one held.
	if staleTmp {
		if err := cfg.Storage.Remove(format.FormatTmpName); err != nil {
			return JournalID{}, err
		}
	}

	var id JournalID
	if _, err := rand.Read(id[:]); err != nil {
		return JournalID{}, err
	}

	f, err := cfg.Storage.Create(format.FormatTmpName)
	if err != nil {
		return JournalID{}, err
	}
	b := format.AppendFormatFile(nil, format.FormatFile{Identity: id, Installation: install})

	// Every failure from the write on leaves a file behind, so each takes
	// the write away before returning, the failure itself as the answer.
	// The refusals above — the settings, the context, the platform check,
	// the empty-directory check — have already written nothing. On the
	// write's and the sync's failures the file is closed only to release
	// the handle on the way out; its own error is dropped so the caller
	// sees the failure that caused the cleanup, not one that follows it.
	if _, err := f.WriteAt(b, 0); err != nil {
		_ = f.Close()
		removePartialFormat(cfg.Storage)
		return JournalID{}, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		removePartialFormat(cfg.Storage)
		return JournalID{}, err
	}
	if err := f.Close(); err != nil {
		removePartialFormat(cfg.Storage)
		return JournalID{}, err
	}
	if err := cfg.Storage.Rename(format.FormatTmpName, format.FormatFileName); err != nil {
		removePartialFormat(cfg.Storage)
		return JournalID{}, err
	}
	if err := cfg.Storage.SyncDir(); err != nil {
		// decision: the format file is taken away and the identity not
		// returned: the caller was told the journal was never made, so a
		// format file standing in the directory is a journal whose
		// creation no caller recorded and whose bytes no sync made
		// durable. Keeping it would make the next Init refuse a directory
		// the caller believes empty. The removal is not synced, so a power
		// cut can resurrect the file; what the next call then finds whole
		// it must refuse — Init the directory holding a format file, Open
		// one whose identity no caller recorded, with an IdentityError
		// naming what it found. Withdraw the erase if a caller ever needs
		// a failed directory sync to leave the half-registered journal
		// for an operator instead.
		removePartialFormat(cfg.Storage)
		return JournalID{}, err
	}
	return id, nil
}

// removePartialFormat takes away the files a failed Init may have left, both
// by name: a rename the storage reported failed may still have moved the file,
// and either the temporary name or the finished one standing in a directory
// the caller believes holds no journal is the split state the rename exists
// to rule out.
//
// decision: the format file's name is removed as well as the temporary one,
// erasing unconditionally the journal a half-done rename may have put under
// it. That is safe because two Inits never race on one directory: only an
// explicit initialisation step makes a journal, one caller runs it, and the
// identity it would erase is one no call ever returned. Withdraw the
// unconditional erase — keep a format file that verifies, and only remove
// the temporary name — if a caller ever needs a failed Init to leave a
// half-registered journal for an operator instead of cleaning up after
// itself.
//
// The removals make no directory sync, so a power cut can resurrect a
// removed name; what the next call then finds whole it must refuse — Init
// the directory holding a format file, Open one whose identity no caller
// recorded, with an IdentityError naming what it found — and that is the
// recovery path. The removal is best effort for the same reason: the
// failure that already reported is the answer the caller needs, so an
// error here is dropped rather than displacing it, and whatever it cannot
// take is left for the next call to refuse the directory for.
func removePartialFormat(fs storage.FS) {
	_ = fs.Remove(format.FormatTmpName)
	_ = fs.Remove(format.FormatFileName)
}

// Open opens the journal in dir, rebuilding its state from the segments on
// disk. It never creates one.
//
// expect is the identity the caller recorded for dir. With expect non-zero,
// Open fails with ErrNoJournal when dir holds no journal, empty, missing, or
// holding neither a format file nor a segment, and with an IdentityError
// when it holds another journal. A directory holding a segment and no format
// file is a journal Open cannot identify: it fails with ErrUnidentified and
// is left as it is. With expect zero,
// Open always fails: with an IdentityError naming the identity and
// installation ID it found, and whether any segment is present, or with
// ErrNoJournal. A format file of an unknown format version, or a record of an
// unknown kind or with a non-zero epoch, fails with ErrUnknownFormat; a
// format file whose checksum does not verify fails with ErrCorrupt.
//
// floor is the larger of the highest content version the caller has recorded
// for any file of the journal's shares and the smallest durable version limit
// among them. Every version the journal issues after Open exceeds both floor
// and every version on disk, so none issued before a crash whose record was
// lost is issued again.
//
// Open requires WithClock. Without it, Open fails with ErrNoClock and opens
// nothing.
func Open(ctx context.Context, dir string, floor Version, expect JournalID, opts ...Option) (*Journal, error) {
	cfg, err := resolveConfig(dir, opts)
	if err != nil {
		return nil, err
	}
	if cfg.Clock == nil {
		return nil, ErrNoClock
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// The order of the first two steps is the contract's, not convenience: a
	// directory that does not exist fails as one that holds no journal — the
	// same refusal an empty directory takes — and is never checked on, its
	// platform included. The listing is the probe: storage is the only reach
	// the journal trusts, whatever route the options named, and a listing of a
	// missing directory reports not-found, so the check on the platform follows
	// it rather than running first as the init path's does.
	names, err := cfg.Storage.List()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNoJournal, dir)
		}
		return nil, err
	}
	if err := checkDir(dir); err != nil {
		return nil, err
	}

	hasFormat, hasSegment := classifyListing(names)
	switch {
	case !hasFormat && !hasSegment:
		return nil, fmt.Errorf("%w: %s holds neither a format file nor a segment", ErrNoJournal, dir)
	case !hasFormat:
		return nil, fmt.Errorf("%w: %s holds a segment and no format file", ErrUnidentified, dir)
	}

	f, err := cfg.Storage.Open(format.FormatFileName)
	if err != nil {
		return nil, err
	}
	b := make([]byte, format.FormatFileSize)
	n, rerr := f.ReadAt(b, 0)
	if err := f.Close(); err != nil {
		return nil, err
	}
	// A file shorter than a format file is damaged like one whose bytes do
	// not verify: the decoder judges its length, so a short read is handed
	// to it rather than answered as an error of the storage's own.
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		return nil, rerr
	}
	ff, err := format.DecodeFormatFile(b[:n])
	switch {
	case err == nil:
	case errors.Is(err, format.ErrUnknownVersion):
		return nil, fmt.Errorf("%w: the format file names a version this binary does not read: %v", ErrUnknownFormat, err)
	case errors.Is(err, format.ErrDamaged):
		return nil, fmt.Errorf("%w: the format file is damaged: %v", ErrCorrupt, err)
	default:
		return nil, err
	}

	if expect == (JournalID{}) || expect != ff.Identity {
		return nil, &IdentityError{Found: ff.Identity, Installation: ff.Installation, Segments: hasSegment}
	}
	if hasSegment {
		// decision: a directory holding a segment is refused with
		// ErrNotImplemented, not recovered: rebuilding the state on disk is
		// the one piece of a recovering open this implementation does not
		// have yet. Every reopen of a journal that has run meets this,
		// because the spare pool leaves its finished spares as segment
		// files, so no journal that has served can be opened again here.
		// The refusal keeps the directory as it found it, so the recovery
		// that exists later takes it whole.
		return nil, fmt.Errorf("%w: %s holds a segment; opening it", ErrNotImplemented, dir)
	}

	// The counter is raised before the core is built: a refused floor must
	// find the directory exactly as it left it, and a built core makes spare
	// segment files durable in its own background, which a refused open may
	// not leave behind — the next open would find the segments it made.
	counters := &index.Counters{}
	if err := counters.RaiseFloor(floor); err != nil {
		return nil, err
	}

	s := resolveDefaults(cfg.Settings)
	c, err := datapath.New(datapath.Deps{
		FS:          cfg.Storage,
		Clock:       cfg.Clock,
		Tracer:      cfg.Tracer,
		Log:         cfg.Logger,
		Dir:         dir,
		Identity:    ff.Identity,
		Streams:     s.Streams,
		SegmentCap:  core.SegmentCapOf(s),
		ExtentLimit: s.ExtentLimit,
		Descriptors: cfg.Budget,
		Targets:     buildTargets(s),
	}, datapath.State{Counters: counters})
	if err != nil {
		// datapath.New takes its own cleanup when a layer below it fails, and
		// nothing runs after it here, so no error from the open itself can
		// leave the core it stopped. Any step added between the core and the
		// journal it serves must close the core on its error before
		// returning it.
		return nil, err
	}
	return newJournal(c), nil
}

// classifyListing says whether the names a directory holds include a format
// file and a segment. A segment ending in .seg or a spare's .spare.tmp
// counts whether or not the journal could have written the name: a file that
// shape is a segment or a segment in the making, and the open refuses the
// directory rather than sorting data it cannot read from data it cannot
// write. A format.tmp counts as neither.
func classifyListing(names []string) (hasFormat, hasSegment bool) {
	for _, name := range names {
		switch kind, _ := format.ClassifyName(name); kind {
		case format.NameFormat:
			hasFormat = true
		case format.NameSegment, format.NameSpare:
			hasSegment = true
		}
		if !hasSegment && looksSegmental(name) {
			// A name of the same shape the classifier refuses — a segment or
			// spare of the wrong width or with the id zero.
			hasSegment = true
		}
	}
	return hasFormat, hasSegment
}

// looksSegmental reports whether name is shaped like a segment or a spare the
// classifier refused: ending in .seg or .spare.tmp already marks the file as
// a segment-shaped one, so the open fails closed on it.
func looksSegmental(name string) bool {
	return strings.HasSuffix(name, ".seg") || strings.HasSuffix(name, ".spare.tmp")
}

// buildTargets returns the spare pool's targets from the settings, which
// resolveDefaults has already given their defaults: the spare target one per
// append stream, so every stream can roll once without waiting for a spare to
// be made. The headroom keeps the reserved records of settings standing:
// each is charged the largest of them with its footer entry, so they take a
// whole number of segments rounded up and never less than one. The repack
// reserve keeps at least one segment's cap, the most live payload one segment
// can hold.
func buildTargets(s core.Settings) segment.Targets {
	segCap := core.SegmentCapOf(s)
	headroom := (int64(headroomRecordBytes)*int64(s.HeadroomRecords) + segCap - 1) / segCap
	return segment.Targets{
		Content:  s.SpareTarget,
		Headroom: int(max(headroom, 1)),
		Reserve:  1,
	}
}

// resolveDefaults returns the settings with every zero naming its default:
// the append streams, the extent limit, the headroom's records and the spare
// target, which defaults to one spare per stream. Nothing here refuses: a
// negative setting was already refused when the options were resolved.
//
// decision: MaxBytes is left at what the caller named or zero. The journal's
// maximum bounds the storage a write may allocate, and the core that
// allocates it takes no maximum from the open yet — no reservation a write
// path makes is checked against it. Admitting the maximum, so a write past
// it is refused naming the journal, is the one piece of the capacity path
// this open leaves out; until it lands, a journal opened with MaxBytes set is
// no smaller than one opened without it.
func resolveDefaults(s core.Settings) core.Settings {
	if s.Streams < 1 {
		s.Streams = defaultStreams
	}
	if s.ExtentLimit < 1 {
		s.ExtentLimit = defaultExtentLimit
	}
	if s.HeadroomRecords < 1 {
		s.HeadroomRecords = defaultHeadroomRecords
	}
	if s.SpareTarget < 1 {
		s.SpareTarget = s.Streams
	}
	return s
}

// headroomRecordBytes is what one reserved header-only record is charged: the
// largest of them with its footer entry, a 96-byte header, a payload of at
// most 56 bytes and a 72-byte footer entry.
const headroomRecordBytes = 96 + 56 + 72

// The settings' defaults, where the caller named none.
const (
	defaultStreams         = 16
	defaultExtentLimit     = 1 << 26
	defaultHeadroomRecords = 65536
)
