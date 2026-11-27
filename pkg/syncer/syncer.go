package syncer

import (
	"context"
	"iter"

	"github.com/cubbit/dittofs/pkg/remote"
)

// Store is what the syncer needs from a backend, named for that need: a
// verified get and a probe, with no put and no delete (RFC 3 §1.3). The remote
// tier's codec.Store, the block codec over a remote store, implements it.
type Store interface {
	// Get reads the chunks named in want, or the whole block when want is
	// empty, each decoded and verified against its own hash; m receives the
	// encoded bytes as they arrive (RFC 3 §1.3, §2.4).
	Get(ctx context.Context, name remote.Name, want []remote.ChunkRange, m remote.Meter) iter.Seq2[remote.Chunk, error]

	// Health is the remote store's probe call of direction d, passed straight
	// through; the store keeps no health state (RFC 3 §1.3, §2.8).
	Health(ctx context.Context, d remote.Direction) error
}

// Syncer is the process's one syncer: the stores registered with it and the
// flows open on them (RFC 3 §1.3). Package pool implements it.
//
// The contract is written in the remote tier's own types: names, ranges,
// chunks, directions, the Meter and the error set are package remote's, used
// as they are and never declared again. The syncer declares no error value of
// its own: a call it refuses, on Syncer or on a Flow, wraps one of the remote
// tier's errors with the reason in the message, so a caller tells the cases
// apart with errors.Is:
//
//   - a full flow queue, the half's waiter bound, and a queued transfer
//     displaced or evicted to make room wrap remote.ErrThrottled, which itself
//     wraps remote.ErrTransient;
//   - a call in a direction that is unhealthy returns that direction's
//     Flow.Health error, which names the store and wraps its cause;
//   - a call after Flow.Close or Close, a call naming a StoreID that is not
//     registered, Unregister with a flow open, and a registration with a probe
//     off wrap remote.ErrInvalid: each is the caller's fault and is never worth
//     retrying.
type Syncer interface {
	// Register adds a store under name, its metric label, and mints its id.
	// Stores registered under one probe key share their outage probe, and the
	// zero key shares with nothing; probes says which directions are probed
	// (RFC 3 §1.3, §2.8). A registration with either probe off is refused.
	Register(name string, store Store, probe ProbeKey, probes Probes) (StoreID, error)

	// Unregister drops a store once no flow on it is open, its health state,
	// probes and counters with it, and fails while a flow is open (RFC 3 §1.3).
	Unregister(store StoreID) error

	// OpenFlow opens a flow on a registered store, with its own queue in the
	// fair scheduler. A background flow's transfers run in the background
	// class (RFC 3 §1.3, §2.9).
	OpenFlow(store StoreID, background bool) (Flow, error)

	// Close stops accepting work and returns once every transfer in flight has
	// ended; a call after it fails (RFC 3 §1.3, §2.4).
	Close() error

	// Stats returns a consistent snapshot of the counters (RFC 3 §2.12).
	Stats() Stats
}

// Flow is a handle bound to one store and one queue: a transfer on it goes to
// that store and waits in that queue, so no call carries a store or a key
// (RFC 3 §1.3, §2.9).
//
// A stream is an iter.Seq2 (RFC 3 §1.3): each chunk is verified before it is
// yielded, its Bytes borrowed until the next iteration; an error is yielded
// once and ends the stream; leaving the loop, or ending ctx, detaches the
// caller, so there is no Close to forget. A loop body must not fetch from the
// block it is iterating.
type Flow interface {
	// ID is the flow's id, under which Stats reports its queue gauges.
	ID() FlowID

	// Upload runs the put of block name: it calls attempt once per try, within
	// RFC 3 §2.4's bounds, and returns nil only once a call of attempt returned
	// nil. size is the block's largest encoded length, which the scheduler
	// charges. It returns only once every call of attempt it made has
	// returned, and never calls attempt after (RFC 3 §1.3, §2.4 to §2.6).
	Upload(ctx context.Context, name remote.Name, size int64, attempt func(ctx context.Context, m remote.Meter) error) error

	// Fetch is a demand read of the chunks named in want, or of the whole
	// block when want is empty (RFC 3 §1.3, §4.1 to §4.3).
	Fetch(ctx context.Context, name remote.Name, want []remote.ChunkRange) iter.Seq2[remote.Chunk, error]

	// Prefetch is Fetch as speculation, which never delays demand (RFC 3 §1.3,
	// §4.4).
	Prefetch(ctx context.Context, name remote.Name, want []remote.ChunkRange) iter.Seq2[remote.Chunk, error]

	// Healthy reports whether direction d of the flow's store is healthy
	// (RFC 3 §2.8).
	Healthy(d remote.Direction) bool

	// Health is nil while direction d is healthy, and otherwise the error that
	// made it unhealthy, naming the store and wrapping its cause (RFC 3 §2.8).
	Health(d remote.Direction) error

	// Close takes the flow out of the scheduler: its queued transfers are
	// refused, its running ones finish, and a call after it fails (RFC 3
	// §1.3).
	Close() error
}
