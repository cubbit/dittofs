package journal

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/datapath"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/platform"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/recovery"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
)

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
	if err := platform.CheckDir(dir, cfg.MountProbe); err != nil {
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
		removePartialFormat(cfg.Storage, id)
		return JournalID{}, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		removePartialFormat(cfg.Storage, id)
		return JournalID{}, err
	}
	if err := f.Close(); err != nil {
		removePartialFormat(cfg.Storage, id)
		return JournalID{}, err
	}
	if err := cfg.Storage.Rename(format.FormatTmpName, format.FormatFileName); err != nil {
		removePartialFormat(cfg.Storage, id)
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
		removePartialFormat(cfg.Storage, id)
		return JournalID{}, err
	}
	return id, nil
}

// removePartialFormat takes away the files a failed Init may have left: the
// temporary name, and the finished one when it names id, the identity this
// Init drew. A rename the storage reported failed may still have moved the
// file, and either name standing in a directory the caller believes holds no
// journal is the split state the rename exists to rule out; a format file
// that names another identity is another Init's finished journal, put in
// place while this one ran, and is never this call's to erase.
//
// The removals make no directory sync, so a power cut can resurrect a removed
// name; what the next call then finds whole it must refuse — Init the
// directory holding a format file, Open one whose identity no caller
// recorded, with an IdentityError naming what it found — and that is the
// recovery path. The removals are best effort for the same reason: the
// failure that already reported is the answer the caller needs, so an error
// here is dropped rather than displacing it.
func removePartialFormat(fs storage.FS, id JournalID) {
	_ = fs.Remove(format.FormatTmpName)
	if namesIdentity(fs, id) {
		_ = fs.Remove(format.FormatFileName)
	}
}

// namesIdentity reports whether the directory holds a format file that
// verifies and names id. A file that cannot be opened, read whole or verified
// is not one this Init wrote, since it writes and syncs the whole file before
// the rename, so it is left where it stands.
func namesIdentity(fs storage.FS, id JournalID) bool {
	f, err := fs.Open(format.FormatFileName)
	if err != nil {
		return false
	}
	b := make([]byte, format.FormatFileSize+1)
	n, _ := f.ReadAt(b, 0)
	_ = f.Close()
	ff, err := format.DecodeFormatFile(b[:n])
	return err == nil && ff.Identity == id
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
// marks are the per-stream sync marks the caller recorded durably and hands
// Open, which recovery checks the scanned tails against. Without them a tail
// the device lost reads as torn, and an append lands after records no sync
// proved. A caller that recorded none passes nil.
//
// Open requires WithClock. Without it, Open fails with ErrNoClock and opens
// nothing.
//
// decision: Open returns while the spare pool is still making its first
// spares, so the journal a caller holds may have none ready yet. Until one
// lands, a write on a stream that needs a spare to roll onto is refused at
// once with ErrNoSpace naming LimitSpare — a retryable refusal, not a
// failure: the caller that retries the write takes a spare that has landed
// in the meantime, and the fixture's own wait, handed over through
// WithSparesReady, is the caller that must not retry. Withdrawing the
// refusal altogether — a WriteAt that waits for a spare, or an Open that
// returns only once the pool has made its first ones — is the later change
// that removes it.
func Open(ctx context.Context, dir string, floor Version, expect JournalID, marks []SyncMark, opts ...Option) (*Journal, error) {
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
	if err := platform.CheckDir(dir, cfg.MountProbe); err != nil {
		return nil, err
	}

	hasFormat, hasSegment := classifyListing(names)
	switch {
	case !hasFormat && !hasSegment:
		return nil, fmt.Errorf("%w: %s holds neither a format file nor a segment", ErrNoJournal, dir)
	case !hasFormat:
		return nil, fmt.Errorf("%w: %s holds a segment and no format file", ErrUnidentified, dir)
	}

	// The format file is read through the same retry bound every read at
	// open gets: a read the device cannot complete is tried again, never
	// judged on the bytes it did not return. The attempts open the file
	// inside the bound, so a failed open fails like a failed read.
	ff, err := recovery.ReadFormat(ctx, recovery.Deps{
		FS:     cfg.Storage,
		Clock:  cfg.Clock,
		Tracer: cfg.Tracer,
		Log:    cfg.Logger,
	}, expect)
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
	s := resolveDefaults(cfg.Settings)
	if hasSegment {
		// The attach pass runs over the directory: its bounded reads, the
		// .spare.tmp unlinks and the classes it sorts the segments into. A
		// read the device never completed refuses the open on its ranges;
		// a pass that ran answers ErrNotImplemented below.
		if _, err := recovery.Run(ctx, recovery.Deps{
			FS:          cfg.Storage,
			Clock:       cfg.Clock,
			Tracer:      cfg.Tracer,
			Log:         cfg.Logger,
			ExtentLimit: s.ExtentLimit,
		}, floor, expect, marks); err != nil {
			return nil, err
		}
		// decision: a directory holding a segment is refused with
		// ErrNotImplemented, not recovered: the attach pass and the rebuild
		// above have run, and the recovery after them — the marks, the
		// tail's proof, the spares and the emptying — is not built. Every
		// reopen of a journal that has run meets this, because the spare
		// pool leaves its finished spares as segment files, so no journal
		// that has served can be opened again here. The refusal keeps the
		// directory as the attach and rebuild passes left it, so the
		// recovery that exists later takes it whole.
		return nil, fmt.Errorf("%w: %s holds segments; recovering them is not built", ErrNotImplemented, dir)
	}

	// The counter is raised before the core is built: a refused floor must
	// find the directory exactly as it left it, and a built core makes spare
	// segment files durable in its own background, which a refused open may
	// not leave behind — the next open would find the segments it made.
	counters := &index.Counters{}
	if err := counters.RaiseFloor(floor); err != nil {
		return nil, err
	}

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

		MaxBytes:      s.MaxBytes,
		IdleSealBytes: s.IdleSealBytes,
	}, datapath.State{Counters: counters})
	if err != nil {
		// datapath.New takes its own cleanup when a layer below it fails, and
		// nothing runs after it here, so no error from the open itself can
		// leave the core it stopped. Any step added between the core and the
		// journal it serves must close the core on its error before
		// returning it.
		return nil, err
	}
	// The spare-pool wait is handed over before the caller sees the journal,
	// so a caller that waits on it blocks nothing Open still has to do: the
	// pool has been the open's own background since New, and the hook only
	// gives the caller a handle to its settle event. The callback is nil in
	// every production call.
	if cfg.SparesReady != nil {
		cfg.SparesReady(c)
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
// the append streams, the extent limit, the headroom's records, the idle-seal
// threshold and the spare target, which defaults to one spare per stream. Nothing here refuses: a
// negative setting was already refused when the options were resolved.
//
// ponytail: MaxBytes is passed to the core as the caller named it, and zero
// leaves the journal unbounded, where the settings' default is 80% of the
// filesystem's size at open. The default is not derived here, so a journal
// opened without a maximum admits by every other limit alone. Upgrade by
// deriving the default from the filesystem's size when the open resolves it.
// The maximum is not free for the open: the spare pool's targets are built
// before any write is admitted, and the spares it makes are reserved against
// the maximum, so a maximum below what the targets name leaves the pool short.
// With the default settings — 16 streams, whose spare target defaults to one
// spare per stream, a headroom of 65536 records no larger than the segment
// cap, and a repack reserve of one segment — an unbounded Open makes 18 spares
// of SegmentCap bytes each (16 content + 1 headroom + 1 reserve, about 4.5 GiB
// at the format's 256 MiB cap).
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
	if s.IdleSealBytes < 1 {
		s.IdleSealBytes = defaultIdleSealBytes
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
	defaultIdleSealBytes   = 16 << 20
)
