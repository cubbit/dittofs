package journal

import (
	"context"
	"crypto/rand"
	"fmt"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/platform"
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

	// Every failure from here on leaves a file behind, so each takes the
	// write away before returning, the failure itself as the answer; the
	// Cancelled and ErrNotEmpty paths above have already written nothing.
	// The close on those paths releases the handle only, as a descriptor
	// that no longer exists does, so its own error is dropped for the one
	// the caller needs.
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
		// the caller believes empty; withdraw the erase if a caller ever
		// needs a failed directory sync to leave the half-registered
		// journal for an operator instead.
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
// decision: the format file's name is removed too, erasing a journal the
// storage wrote before reporting the rename's failure. Keeping it would
// leave a journal an Init answered failure for, with an identity the caller
// never received; the caller that retries Init then finds a directory it must
// refuse. Withdraw the erasure if a caller ever needs the half-registered
// journal kept instead.
//
// The removal is best effort: the failure that already reported is the
// answer the caller needs, so an error here is dropped rather than displacing
// it, and whatever the removal cannot take is left for the next Init to
// refuse the directory for or replace.
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
	return nil, ErrNotImplemented
}
