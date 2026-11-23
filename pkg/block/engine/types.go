package engine

import (
	"context"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// Settings are the engine's node-level settings, handed to Apply. Every one
// must be positive.
type Settings struct {
	ExistenceAge        time.Duration // existence_age: how long a write's existence may stay uncommitted
	GroupCommitMaxFiles int           // group_commit.max_files: the most files one group commit batch holds
	CloneMaxLen         int64         // clone_max_len: the longest extent one Clone takes, in bytes
}

// Share is one share's storage binding, as Start and Bind take it.
type Share struct {
	ID        entity.ShareID
	Tag       journal.ShareTag   // the share's tag in every journal
	Shard     entity.ShardID     // the share's own shard, which keys a clone's source open
	Namespace entity.NamespaceID // the share's write namespace: a clone's two shares must share it
	Limit     int64              // the share's journal limit, in bytes
	State     ShareState
}

// ShareState is what a bound share admits.
type ShareState uint8

const (
	Serving  ShareState = iota + 1 // reads and writes
	Quiesced                       // nothing new admitted: every content call but LossSeq answers ErrDelay (DelayFrozen)
)

// Stats is the engine's counters and conditions, per journal and per share.
// Every map belongs to the caller.
type Stats struct {
	Journals map[string]JournalStats // per device, as Deps.Devices names it
	Shares   map[entity.ShareID]ShareStats
	Node     NodeStats
	// SelfFences is engine_self_fences_total{reason="store"}: each time the
	// node fenced for a store that committed no write transaction.
	SelfFences int64
	// StabilityPoints is engine_stability_points_total.
	StabilityPoints int64
	// WriteRefusals is engine_write_refusals_total.
	WriteRefusals int64
	// PendingExistenceOps is engine_pending_existence_ops' sum: the
	// operations the existence commits committed.
	PendingExistenceOps int64
	// GroupCommitSplits is engine_group_commit_splits_total.
	GroupCommitSplits int64
	// Reads is engine_reads_total by its class label, and ReadSeconds
	// engine_read_seconds.
	Reads       map[string]int64
	ReadSeconds Series
	// FileStateLockWaits is engine_lock_wait_seconds{area="file_state"}: the
	// contended takes of a file's state lock.
	FileStateLockWaits Series
	// RemovalRuns is engine_removal_runs_total by its result label, over
	// every journal.
	RemovalRuns map[string]int64
}

// Series is a duration series as a count and a sum.
type Series struct {
	Count int64
	Total time.Duration
}

// WriteOpts is what a write carries besides its bytes.
type WriteOpts struct {
	Suspend entity.TimeMask // the times this write leaves alone
	// Done is called exactly once for every write Write accepted: by the
	// existence commit that records its usage or the commit recording its
	// loss, or by the engine, charging nothing, when it drops the write
	// uncommitted. It is never called if Write fails. Close does not call
	// it for a write it leaves to the next start's recovery: the quota
	// reservation it releases went with the filesystem service, which stops
	// before Close.
	Done func()
}

// suspendedKey is the context key of the times a call leaves alone.
type suspendedKey struct{}

// WithSuspended returns ctx carrying m, the times a Truncate, Deallocate,
// Clone or Copy called with it leaves alone, as WriteOpts.Suspend is a
// write's: those the open the call came through suspended. Of them only the
// modification time is one these calls move; the change time moves whatever
// m names.
//
// decision: the mask rides the context, as the admitted hook does, rather
// than a parameter of each of the four calls, which every caller of them
// would pass as none. Make it a parameter if a second option joins it.
//
// decision: only the journal's removal and clone records lack the mask, so a
// truncate, deallocate or clone replayed or rebuilt at a start, a grow
// included (stageGrow), moves the modification time. A suspended write is not
// in this gap: its record holds modification time 0, and replay keeps it
// (stageWrite). So a persistent open (RFC 14 §2.2), whose mask outlives a
// crash, can see its frozen Modify move after one, for an operation still
// uncommitted then. Overturned once those records carry the mask.
func WithSuspended(ctx context.Context, m entity.TimeMask) context.Context {
	return context.WithValue(ctx, suspendedKey{}, m)
}

// Suspended is the times ctx carries for its call to leave alone
// (WithSuspended), none when it carries none: what the engine reads, and
// what another Content reads at the same calls.
func Suspended(ctx context.Context) entity.TimeMask {
	m, _ := ctx.Value(suspendedKey{}).(entity.TimeMask)
	return m
}

// Overlay is a file's size, times and version as its uncommitted operations
// leave them, replayed in version order: a truncate sets Size and SizeSet, and
// each later write raises Size to its end.
type Overlay struct {
	Size         int64 // SizeSet: the size; else the end of the pending writes
	SizeSet      bool  // a truncate is pending, so Size is exact
	Mtime, Ctime time.Time
	Version      entity.JournalVersion
}
