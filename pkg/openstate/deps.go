package openstate

import (
	"context"
	"io"
	"iter"
	"log/slog"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// Deps is what the table is built over.
type Deps struct {
	Records Records       // the store's installation-wide numbers
	Clients ClientRecords // the durable client records
	Opens   OpenRecords   // the durable opens, their locks and delete marks
	Node    entity.NodeID // this node, whose epoch each Start raises
	Clock   Clock
	Rand    io.Reader    // every random bit of a token or confirm; crypto/rand in production
	Logger  *slog.Logger // slog.Default() when nil
	// ponytail: grants, watches and recalls are refused at RFC 14's fixed caps
	// alone; take RFC 24's Budget as a Dep once its pool exists, so a node-wide
	// limit can refuse before a per-client cap is reached.
}

// Records is the part of the metadata store open state numbers from: the
// installation's ID allocators, its NSM state number and this node's epoch.
type Records interface {
	NextID(ctx context.Context, kind entity.IDKind) (uint64, error)
	NSMState(ctx context.Context) (uint32, error)
	RaiseNSMState(ctx context.Context, n uint32) error // entity.ErrNSMNotRaised unless n is above the number held
	RaiseNodeEpoch(ctx context.Context, node entity.NodeID) (uint64, error)
}

// ClientRecords is the metadata store's view of the durable client records,
// as open state calls it. A conditional write whose expected record is no
// longer the stored one is entity.ErrRecordChanged, with the current record.
type ClientRecords interface {
	Create(ctx context.Context, cl entity.ClientRecord) error
	Reboot(ctx context.Context, old entity.ClientID, cl entity.ClientRecord) error
	Get(ctx context.Context, id entity.ClientID) (entity.ClientRecord, error)
	ByName(ctx context.Context, p entity.Protocol, name []byte) (entity.ClientRecord, error)
	Put(ctx context.Context, prev, next entity.ClientRecord) (entity.ClientRecord, error)
	AddSession(ctx context.Context, id entity.ClientID, node entity.ReachNode, prev, next []byte) (entity.ClientRecord, error)
	MarkShard(ctx context.Context, id entity.ClientID, m entity.ShardMark) error
	DropShards(ctx context.Context, id entity.ClientID) error
	Delete(ctx context.Context, id entity.ClientID) error
	DeleteUnreached(ctx context.Context, id entity.ClientID) (entity.ClientRecord, error)
	ByShard(ctx context.Context, s entity.ShardID) iter.Seq2[entity.ClientRecord, error]
}

// OpenRecords is the metadata store's view of the durable opens, as open
// state calls it.
type OpenRecords interface {
	Put(ctx context.Context, share entity.ShareID, op entity.OpenRecord) error
	Get(ctx context.Context, share entity.ShareID, file entity.FileID, id entity.OpenID) (entity.OpenRecord, error)
	Of(ctx context.Context, share entity.ShareID, file entity.FileID) ([]entity.OpenRecord, error)
	Delete(ctx context.Context, share entity.ShareID, file entity.FileID, id entity.OpenID) (entity.Orphaned, error)
	Durable(ctx context.Context, share entity.ShareID, shard uint64) iter.Seq2[entity.DurableOpen, error]
	SetLocks(ctx context.Context, share entity.ShareID, file entity.FileID, open entity.OpenID, rec *entity.OpenRecord, put, del []entity.LockRecord) error
	Locks(ctx context.Context, share entity.ShareID, file entity.FileID, open entity.OpenID) ([]entity.LockRecord, error)
	SetDeletePending(ctx context.Context, share entity.ShareID, file entity.FileID, by entity.OpenID, d entity.DeleteRecord) error
	ClearDeletePending(ctx context.Context, share entity.ShareID, file entity.FileID) error
	DeletePending(ctx context.Context, share entity.ShareID, file entity.FileID) (entity.DeleteRecord, bool, error)
}

// Clock is the part of the node's clock open state reads. Every lease and
// deadline is an Elapsed reading, never a wall time, so a wall clock's step
// neither expires a client nor keeps one.
type Clock interface {
	Elapsed() time.Duration                // monotonic, from an origin the clock chose
	After(d time.Duration) <-chan struct{} // fires after d on the monotonic clock
}
