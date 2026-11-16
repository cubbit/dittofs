package api

import (
	"context"
	"net/http"
	"time"

	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// Long-poll bounds: the longest ?wait= a read accepts, and how many polls a
// node holds parked at once.
const (
	maxWait   = 60 * time.Second
	maxParked = 256
)

// clock is the API's time: Now, and a channel that closes once d has
// passed.
type clock interface {
	Now() time.Time
	After(d time.Duration) <-chan struct{}
}

type controllerKey struct{}

// watch adapts a read a client may wait on. Without ?wait= it answers as
// read does. With ?wait=d, d a duration up to 60 s, a first read that
// changed would not answer at once parks the call, which then re-reads once
// a second on the API's clock and answers the first read for which changed
// reports true, or the last one once d has passed. A node parks at most 256
// calls, apart from the calls it runs, and refuses the next 503. A parked
// call's write deadline is moved past its wait, so the server's write
// timeout, which bounds every other call, does not cut it off.
//
// ponytail: a parked poll re-reads its record once a second on the API's
// clock, with no notification hub, so the 256 parked polls cost at most 256
// reads a second. Upgrade by signalling parked polls from the node's
// per-second read of the store's counters.
func watch[R any](h func(context.Context, reads, request[none]) (R, error), changed func(was, now R) bool) adapter {
	return func(ctx context.Context, s *Server, r *http.Request, id [16]byte) (answer, error) {
		var wait time.Duration
		if q := r.URL.Query(); q.Has("wait") {
			var err error
			if wait, err = time.ParseDuration(q.Get("wait")); err != nil || wait <= 0 || wait > maxWait {
				return answer{}, &fieldError{field: "wait", detail: "not a duration above 0 and at most " + maxWait.String()}
			}
		}
		if s.d.Reads == nil {
			return answer{}, &refusal{code: apiv1.CodeUnavailable, detail: "the node holds no control-plane store"}
		}
		rq := request[none]{ID: id}
		first, err := h(ctx, s.d.Reads, rq)
		if err != nil || wait == 0 {
			return jsonOr(first, err)
		}
		select {
		case s.parked <- struct{}{}:
			defer func() { <-s.parked }()
		default:
			return answer{}, &refusal{code: apiv1.CodeUnavailable, detail: "the node holds as many parked polls as it may"}
		}
		if rc, ok := ctx.Value(controllerKey{}).(*http.ResponseController); ok {
			// The server's deadline is the wall clock's, whatever clock the
			// wait is on.
			if err := rc.SetWriteDeadline(time.Now().Add(wait + s.api.WriteTimeout)); err != nil {
				return answer{}, err
			}
		}
		last, end := first, s.d.Clock.Now().Add(wait)
		for {
			left := end.Sub(s.d.Clock.Now())
			if left <= 0 {
				return jsonAnswer(http.StatusOK, last)
			}
			select {
			case <-s.d.Clock.After(min(time.Second, left)):
			case <-ctx.Done():
				return answer{}, ctx.Err()
			}
			if last, err = h(ctx, s.d.Reads, rq); err != nil || changed(first, last) {
				return jsonOr(last, err)
			}
		}
	}
}

// jsonOr is 200 with v, or err.
func jsonOr[R any](v R, err error) (answer, error) {
	if err != nil {
		return answer{}, err
	}
	return jsonAnswer(http.StatusOK, v)
}
