package syncer

import "time"

// Stats is a snapshot of the syncer's counters, one Half per direction. Each
// field's comment names the series it carries, without the deployment's
// prefix. Every series is labelled half, the field of Stats the value sits
// under; a series under a StoreStats or a FlowStats is also labelled store,
// its Name. A label value such as a reason, an outcome or a class is a named
// field, so a series with no field is a missing field rather than a string
// nobody emits.
//
// Fields are only ever added, so code that reads a Stats keeps compiling as
// the counters behind it fill in.
type Stats struct {
	Upload, Fetch Half
}

// Half is one direction's counters: its pool, then each registered store and
// each open flow.
type Half struct {
	// Workers is syncer_workers, the half's pool size. Kind: gauge.
	Workers int

	// DetachedBuffers is syncer_detached_buffers, the chunk buffers held by
	// detached callers, against the detached-buffer cap. Only fetches detach,
	// so it is zero in Upload. Kind: gauge.
	DetachedBuffers int

	// Stores holds one entry per registered store.
	Stores []StoreStats

	// Flows holds one entry per open flow, for the queue gauges a share
	// reports.
	Flows []FlowStats
}

// StoreStats is one store's counters in one half.
type StoreStats struct {
	// Store and Name identify the store: the id Register minted, and the name
	// it was registered under, the series' store label.
	Store StoreID
	Name  string

	// WorkersBusy is syncer_workers_busy, the workers transferring. Kind:
	// gauge.
	WorkersBusy int

	// InflightBytes is syncer_inflight_bytes, the bytes held by transfers in
	// flight. Kind: gauge.
	InflightBytes int64

	// QueueDepth is syncer_queue_depth, the transfers waiting, and Waiters is
	// syncer_waiters, the queued transfers plus joined callers. Both are
	// summed over the store's flows, by class. Kind: gauge.
	QueueDepth, Waiters ByClass[int]

	// QueueWait is syncer_queue_wait_seconds, the time from call to dispatch,
	// by class. Kind: histogram.
	QueueWait ByClass[Histogram]

	// Refused is syncer_refused_total, the calls refused, by reason. Kind:
	// counter.
	Refused Refused

	// Transfers is syncer_transfers_total, the transfers ended, by outcome.
	// Kind: counter.
	Transfers Outcomes

	// Bytes is syncer_bytes_total, the encoded bytes transferred, as the
	// transfers' meters report them. Kind: counter.
	Bytes uint64

	// TransferDuration is syncer_transfer_duration_seconds, the time from
	// dispatch to end. Kind: histogram.
	TransferDuration Histogram

	// FirstByte is syncer_first_byte_seconds, the time to the first byte.
	// Kind: histogram.
	FirstByte Histogram

	// Retries is syncer_retries_total, the attempts beyond the first, by
	// reason. Kind: counter.
	Retries Retries

	// RetryBudgetExhausted is syncer_retry_budget_exhausted_total, the failed
	// attempts reported rather than retried because the store's retry budget
	// was empty. Kind: counter.
	RetryBudgetExhausted uint64

	// ThrottleLimit is syncer_throttle_limit, the store's throttle limit in
	// this direction. Kind: gauge.
	ThrottleLimit int

	// Throttled is syncer_throttled_seconds_total, the time flows spent held
	// after a throttling response, summed over flows. Kind: counter.
	Throttled time.Duration

	// Throughput is syncer_store_throughput_bytes_per_second, the store's
	// aggregate throughput in this direction, in bytes per second. Kind:
	// gauge.
	Throughput int64

	// Joined is syncer_fetch_joined_total, the callers that joined a fetch
	// already in flight, and Rehomed is syncer_fetch_rehomed_total, the
	// undispatched fetches moved to a demanding flow. Both are zero in
	// Upload. Kind: counter.
	Joined, Rehomed uint64

	// SpeculationPreempted is syncer_speculation_preempted_total, the
	// speculative fetches that yielded to demand. Zero in Upload. Kind:
	// counter.
	SpeculationPreempted uint64

	// Detached is syncer_fetch_detached_total, the joined callers detached,
	// by reason. Zero in Upload. Kind: counter.
	Detached Detached

	// Healthy is syncer_store_healthy, the direction's current state, true
	// as 1. Kind: gauge.
	Healthy bool

	// Unhealthy is syncer_store_unhealthy_seconds_total, the time spent
	// unhealthy. Kind: counter.
	Unhealthy time.Duration

	// Transitions is syncer_store_transitions_total, the changes of health
	// state. Kind: counter.
	Transitions uint64

	// ProbeDuration is syncer_probe_duration_seconds, the time per probe.
	// Kind: histogram.
	ProbeDuration Histogram
}

// FlowStats is one open flow's queue gauges in one half, the per-flow
// reading of syncer_queue_depth and syncer_waiters, by class.
type FlowStats struct {
	// Flow is the id the flow was opened under, and Store the store it is
	// open on.
	Flow                FlowID
	Store               StoreID
	QueueDepth, Waiters ByClass[int]
}

// ByClass holds one value per scheduling class, the series' class label.
type ByClass[T any] struct {
	Demand, Background, Speculation T
}

// Refused counts refused calls by reason: an unhealthy direction, a full
// queue, a full waiter bound, a queued transfer displaced for a flow below
// its share of the waiter bound, a speculation evicted for a demand, and a
// call on a closed flow.
type Refused struct {
	Unhealthy, QueueFull, WaitersFull, Displaced, Evicted, Closed uint64
}

// Outcomes counts ended transfers by outcome. Unknown is a transfer whose
// result the store never reported, which is not a success.
type Outcomes struct {
	OK, Failed, Unknown uint64
}

// Retries counts attempts beyond the first by reason.
type Retries struct {
	Transient, Throttled, SlowTail, Verify, Floor, Stalled, Trickle uint64
}

// Detached counts detached callers by reason: one that fell behind the
// fetch it joined, and one whose flow closed.
type Detached struct {
	Behind, FlowClosed uint64
}

// Histogram is a distribution of durations over the request bucket set.
// Counts[0] counts the observations at most Bounds()[0], Counts[i] those above
// Bounds()[i-1] and at most Bounds()[i], and the last count those above every
// bound. The counts are per bucket, not cumulative. Sum is the total of every
// observation.
type Histogram struct {
	Counts [19]uint64
	Sum    time.Duration
}

// Bounds returns the upper bounds of the histogram's buckets: the request set,
// for anything a client request waits on, from 100 µs to 60 s. It is a method
// rather than a variable so that the counts and the bounds an exporter reads
// come from one place.
func (Histogram) Bounds() [18]time.Duration {
	return [18]time.Duration{
		100 * time.Microsecond, 250 * time.Microsecond, 500 * time.Microsecond,
		time.Millisecond, 2500 * time.Microsecond, 5 * time.Millisecond,
		10 * time.Millisecond, 25 * time.Millisecond, 50 * time.Millisecond,
		100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond,
		time.Second, 2500 * time.Millisecond, 5 * time.Second,
		10 * time.Second, 30 * time.Second, 60 * time.Second,
	}
}
