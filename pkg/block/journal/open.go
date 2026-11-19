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
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
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
// A recovery that cannot be answered over the directory as it stands — a
// lost synced tail, a range or a record it cannot feed — fails with
// ErrOpenRefused, leaving the directory for the next open, which answers the
// same until the directory or the settings change. Bytes no bounded read
// completed fail with an UnreadableRangesError, matched by ErrUnreadable.
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
		// The recovery runs over the directory: the attach pass's bounded
		// reads and .spare.tmp unlinks, the rebuild, the marks, the tail's
		// proof, the spares phase's refusals, zeroings, unlinks and
		// re-adoptions, the re-append, the assembly and the cut-and-seal.
		// A read the device never completed refuses the open on its ranges.
		failed, err := durability.FailedFor(dir, ff.Identity)
		if err != nil {
			return nil, err
		}
		started := cfg.Clock.Elapsed()
		recoveryDeps := recovery.Deps{
			FS:           cfg.Storage,
			Clock:        cfg.Clock,
			Tracer:       cfg.Tracer,
			Log:          cfg.Logger,
			Failed:       failed,
			ExtentLimit:  s.ExtentLimit,
			SegmentCap:   core.SegmentCapOf(s),
			SpareTargets: buildTargets(s),
			Dir:          dir,
		}
		st, err := recovery.Run(ctx, recoveryDeps, floor, expect, marks)
		if err != nil {
			return nil, err
		}
		found := openedFigures{reporting: st.Reporting(), took: cfg.Clock.Elapsed() - started}
		deps, state := continueFrom(st, datapath.Deps{
			FS:            cfg.Storage,
			Clock:         cfg.Clock,
			Tracer:        cfg.Tracer,
			Log:           cfg.Logger,
			Dir:           dir,
			Identity:      ff.Identity,
			Streams:       s.Streams,
			SegmentCap:    core.SegmentCapOf(s),
			ExtentLimit:   s.ExtentLimit,
			Descriptors:   cfg.Budget,
			Targets:       buildTargets(s),
			MaxBytes:      s.MaxBytes,
			IdleSealBytes: s.IdleSealBytes,
		})
		// The re-append's sealed target reaches the continuation as the
		// active row its all-zero rebuild scan seeded it with, for no fate
		// names it and the walk never read the records the copier wrote:
		// the row is sealed here, at its cap — a full seal's footprint —
		// so the set answers the segment what the file's own marker
		// proves, and no later seal, recycle or stats read takes it for a
		// segment that appends.
		if t := st.Proof.ReappendTarget; t != 0 {
			if err := state.Set.Seal(t, true); err != nil {
				return nil, err
			}
		}
		// The open's own loss records: appended and synced before the open
		// returns, so the loss records the crash left half-told are durable
		// the moment the journal is served. Every stream starts the pass with no
		// active segment, so each stream the records append on rolls once:
		// the pass waits for that many spares, not the pool's whole goal,
		// before the first record it appends, and never
		// for a pass the disk answers whole with found records, keeping
		// every clean reopen off the pool's refill entirely. The corrupt
		// drops past the open reserve defer (the reasoning lives with the
		// pass itself): the emptying below owes each deferred record its
		// copy, and the retire step's registry read keeps its holder
		// standing. Continue runs the pass and the emptying between the core
		// and its retirement worker and closes the core on any failure,
		// leaving the directory to the next open: every record the pass
		// landed is durable, and every failure the answer costs is on the
		// directory's failed list.
		c, err := datapath.Continue(deps, state, func(c *datapath.Core) error {
			// The damaged segments the open reports feed the rows the journal
			// counts until it closes, whatever its own retirements and
			// emptyings then do with the segments: the rows are marked first,
			// before any retirement can fire under the open's own steps, so a
			// retired segment's row keeps the retired flag the count survives
			// on.
			c.Durability().DamagedAtOpen(st.Tails.Damaged)
			if err := recovery.OpenLosses(ctx, &st, c.Durability(), s.HeadroomRecords, func(n int) error {
				return c.WaitRecordSpares(ctx, n)
			}); err != nil {
				return err
			}
			// The emptying: every attached segment the journal's liveness
			// tracker answers not held retires here, after the loss records
			// the pass above made durable — the order the emptying gates on,
			// for a retiree may hold the only copy of a record the crash
			// corrupted and the loss record that masks its older copies must
			// be on disk before the segment's unlink takes it — and then every
			// segment the open ended is copied out into sealed spares of the
			// pool's repack reserve and unlinked with a synced removal, its
			// served pieces repointed to the copies; the takes a durable
			// unlink frees are the copies' own spares. All of it runs before
			// the retirement worker starts, so the open's own retirements and
			// emptyings are under the open's control and never race a
			// worker's hand-over. Each retirement recycles or unlinks as the
			// retirement itself decides and refills the pool behind itself.
			recoveryDeps.Liveness = c.Liveness()
			recoveryDeps.Retire = c.Retire
			recoveryDeps.DrawReserve = func(floor core.SegmentID) (core.SegmentID, error) {
				return c.TakeReserve(ctx, floor)
			}
			recoveryDeps.EndDeferral = func(seg core.SegmentID, l durability.Loss) bool {
				ended := c.Durability().EndDeferral(seg, l)
				if ended {
					c.Liveness().SetDeferred(seg, false)
				}
				return ended
			}
			if err := recovery.Emptying(ctx, recoveryDeps, &st, expect, floor, marks); err != nil {
				return err
			}
			// The recovered marks, read at the settle the emptying ends the
			// open's placements at: every copy the open made is written and
			// sealed, every source unlinked, and an entry names a segment
			// still in the ledger — a record the emptying copied out is
			// named at its copy, and an unlinked segment offers no mark of
			// its own.
			found.marks = st.SyncMarks()
			return holdDeferredHolders(c, dir, expect)
		})
		if err != nil {
			return nil, err
		}
		if cfg.SparesReady != nil {
			cfg.SparesReady(c)
		}
		return newRecoveredJournal(c, found), nil
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

// holdDeferredHolders raises the tracker's deferred count, once per registry
// entry, on each segment the emptying left standing under a deferral. The
// retirement worker the core starts next decides on the tracker's Held answer
// alone, and a holder of a deferred loss may carry no live record of its own:
// without the count the worker unlinks it within a tick, taking the dropped
// record the deferral still owes a loss record, and a reopen then serves the
// older bytes over the synced write. An entry naming a segment the set no
// longer holds raises nothing: the tracker answers no state for it.
//
// ponytail: a deferral held here ends only through EndDeferral, which the
// emptying calls for an ended holder, or a sealed one holding a failed record
// past the open reserve, alone. Any other sealed holder's deferral therefore
// never ends in this process, its loss is never recorded, and the next open
// re-defers it, so it stands across restarts too. Upgrade to an emptying that copies sealed
// holders, ending the deferral in the copy, when a holder standing this way
// shows up in the disk accounting.
func holdDeferredHolders(c *datapath.Core, dir string, id core.JournalID) error {
	deferred, err := durability.DeferredFor(dir, id)
	if err != nil {
		return err
	}
	for seg, losses := range deferred {
		for range losses {
			c.Liveness().SetDeferred(seg, true)
		}
	}
	return nil
}

// continueFrom is the core an assembled recovery continues as: the deps the
// core is made from, with LastSegment raised to the highest segment id the
// directory holds — so the spare maker draws its first spare above every
// segment the recovery rebuilt, and a fresh spare never meets a rebuilt
// segment's id — and, in the state, the rebuilt index and its counters, the
// assembled set, the open state the assembly ended with, and the dirty seed
// the rebuilt index spells. It is the mapping the journal's open step calls
// once it has run the recovery over a directory that holds segments.
//
// decision: this mapping is the production open's own now, and the tests'
// before it — the reopen seam's rows and the inspector's planted fixture,
// each building the deps of its own journal, were its only callers while
// the open refused every directory holding segments. A copy of the mapping
// anywhere else that dropped LastSegment would leave its spares colliding
// with the rebuilt ids, and no row would name it, so this function stays
// the one mapping.
func continueFrom(st recovery.State, deps datapath.Deps) (datapath.Deps, datapath.State) {
	deps.LastSegment = st.Attached.MaxID
	spares := make([]datapath.AdoptedSpare, len(st.Spares.Adopted))
	for i, a := range st.Spares.Adopted {
		spares[i] = datapath.AdoptedSpare{ID: a.ID, Class: a.Class}
	}
	return deps, datapath.State{
		Index: st.Rebuilt.Index, Counters: st.Rebuilt.Counters, Set: st.Assembled.Set,
		Open: &datapath.OpenState{Live: st.Assembled.Live, Forgets: st.Assembled.Forgets,
			Unsynced: st.Assembled.Unsynced, Dirty: dirtySeed(st.Rebuilt.Index), Spares: spares},
	}
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
