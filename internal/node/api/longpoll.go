package api

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// Long-poll bounds: the longest ?wait= a read accepts, and how many polls a
// node holds parked at once.
const (
	maxWait   = 60 * time.Second
	maxParked = 256
)

// clock is the API's time: Elapsed, the monotonic time a wait is measured
// on, which a step of the wall clock does not move; a channel that closes
// once d has passed; and Now, the wall time, which no wait reads.
//
// decision: Now is here only for the operations gauge, which ages an
// operation from its recorded start, a wall-clock stamp the store wrote, so
// it has nothing to measure against but wall time. A wall-clock step moves
// that gauge and never a parked poll.
type clock interface {
	Now() time.Time
	Elapsed() time.Duration
	After(d time.Duration) <-chan struct{}
}

// watching is what a read a client may wait on knows of its record: read,
// which reads it from the server's Deps; tag, its tag, which the answer carries as its ETag; and
// until, which parses ?wait=, answering how long the call may park, above
// zero, and done, whether a read now answers the wait, first being the read
// the call began with.
type watching[R any] struct {
	read  func(context.Context, Deps, request[none]) (R, error)
	tag   func(R) uint64
	until func(wait string) (time.Duration, func(first, now R) bool, error)
}

// changes is the ?wait= of a record a client waits on to change: a duration
// above 0 and at most 60 s, done once differ reports the record changed.
func changes[R any](differ func(first, now R) bool) func(string) (time.Duration, func(first, now R) bool, error) {
	return func(w string) (time.Duration, func(first, now R) bool, error) {
		d, err := time.ParseDuration(w)
		if err != nil || d <= 0 || d > maxWait {
			return 0, nil, &fieldError{field: "wait", detail: "not a duration above 0 and at most " + maxWait.String()}
		}
		return d, differ, nil
	}
}

// watch adapts a read a client may wait on. Its answer is 200 with the
// record and its tag as the ETag. Without ?wait= it reads once. With it, a
// first read that is not already done parks the call, which then re-reads
// once a second on the API's clock, under the context the first read had,
// and answers the first read that is done, or the last one once the wait
// has passed, on the clock's elapsed time, or the API listener shuts down.
// No query parameter but wait is accepted; with several at fault, the 400
// names the first in name order. A node parks at most 256 calls,
// apart from the calls it runs, and refuses the next 503.
//
// ponytail: a parked poll re-reads its record once a second on the API's
// clock, with no notification hub, so the 256 parked polls cost at most 256
// reads a second. Upgrade by signalling parked polls from the node's
// per-second read of the store's counters. The parking set is first come,
// first served, so one account can hold every parked slot; upgrade to a
// per-account share of the set when accounts contend for it.
func watch[R any](w watching[R]) adapter {
	return func(ctx context.Context, s *Server, r *http.Request, _ *route, id [16]byte) (answer, error) {
		var wait time.Duration
		var done func(first, now R) bool
		q := r.URL.Query()
		for _, k := range slices.Sorted(maps.Keys(q)) {
			v := q[k]
			var err error
			switch {
			case len(v) != 1:
				return answer{}, &fieldError{field: k, detail: "given more than once"}
			case k != "wait":
				return answer{}, &fieldError{field: k, detail: "not a parameter of this read"}
			}
			if wait, done, err = w.until(v[0]); err != nil {
				return answer{}, err
			}
		}
		if s.d.Store == nil {
			return answer{}, errNoStore
		}
		rq := request[none]{ID: id}
		first, err := w.read(ctx, s.d, rq)
		if err != nil || wait == 0 || done(first, first) {
			return w.answer(first, err)
		}
		if s.d.Clock == nil {
			return answer{}, &refusal{code: apiv1.CodeUnavailable, detail: "the node holds no clock to park a poll on"}
		}
		return answer{wait: wait, park: func() (answer, error) {
			select {
			case s.parked <- struct{}{}:
				defer func() { <-s.parked }()
			default:
				return answer{}, &refusal{code: apiv1.CodeUnavailable, detail: "the node holds as many parked polls as it may"}
			}
			last, end := first, s.d.Clock.Elapsed()+wait
			for {
				left := end - s.d.Clock.Elapsed()
				if left <= 0 {
					return w.answer(last, nil)
				}
				select {
				case <-s.d.Clock.After(min(time.Second, left)):
				case <-s.quit:
					return w.answer(last, nil)
				case <-ctx.Done():
					return answer{}, ctx.Err()
				}
				if last, err = w.read(ctx, s.d, rq); err != nil || done(first, last) {
					return w.answer(last, err)
				}
			}
		}}, nil
	}
}

// answer is 200 with v and its tag as the ETag, or err.
func (w watching[R]) answer(v R, err error) (answer, error) {
	a, err := jsonOr(v, err)
	if err == nil {
		a.header.Set("ETag", etag(w.tag(v)))
	}
	return a, err
}
