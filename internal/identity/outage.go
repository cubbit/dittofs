package identity

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// conditionDirectory is RFC 25 §3.9's name for a node whose directory
// queries fail.
const conditionDirectory = "directory_unreachable"

// dirProbeEvery is how often a lookup asks the directory while the outage is
// raised (RFC 18 §5.3).
const dirProbeEvery = 10 * time.Second

// dirQuietProbe is how long after the last probe the cache's own loop sends
// one, for a node no lookup probes on.
const dirQuietProbe = 2 * dirProbeEvery

// errDirUnreachable answers a lookup sent no query because the outage is
// raised and the probe is not yet due.
var errDirUnreachable = fmt.Errorf("%w: identity: directory unreachable, not asked until the next probe", vfserr.ErrUnavailable)

// The condition's causes. RFC 25 §3.2's registry rule reads the severity from
// the cause: unhealthy when it is ErrUnavailable, since a sign-in was
// refused, and degraded otherwise.
var (
	errDirServedStale = errors.New("identity: directory unreachable, lookups served stale within the grace")
	errDirRefused     = fmt.Errorf("%w: identity: directory unreachable, a lookup was refused", vfserr.ErrUnavailable)
)

// outage is one directory cache's directory_unreachable: raised by the
// cache's own failed queries, so by this node's and no other's, and cleared
// by the first query the directory answers.
type outage struct {
	mu      sync.Mutex
	raised  bool
	refused bool      // a lookup was refused while raised
	probed  time.Time // when the directory was last asked while raised
	last    error     // the last failure that raised it or kept it raised
	cleared time.Time // when it last cleared
	probing bool      // the cache's probe loop runs
	closed  bool      // the cache is closed: no probe loop starts
	loops   sync.WaitGroup
}

// skip reports whether a lookup at now goes unasked: true while the outage is
// raised and the last probe is less than dirProbeEvery old. A lookup it does
// not skip while raised is the probe.
func (o *outage) skip(now time.Time) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.raised {
		return false
	}
	if now.Sub(o.probed) < dirProbeEvery {
		return true
	}
	o.probed = now
	return false
}

// raise raises the outage for a query that started at start and failed with
// err, the failed query standing as its first probe; a raised one keeps its
// last probe. A query that started before the last clear raises nothing: the
// directory answered after it was sent (RFC 18 §5.3). It reports whether the
// caller starts the probe loop, which it counts in loops.
func (o *outage) raise(start time.Time, err error) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if start.Before(o.cleared) {
		return false
	}
	if !o.raised {
		o.raised, o.refused, o.probed = true, false, start
	}
	o.last = err
	if o.probing || o.closed {
		return false
	}
	o.probing = true
	o.loops.Add(1)
	return true
}

// clear clears the outage at now.
//
// ponytail: an outage longer than the TTL leaves every entry past it when the
// condition clears, and each one's next lookup queries the directory, one per
// user, in the sign-in that needs it. Serve in-grace entries while refreshing
// them in the background at a capped rate when a recovery's query burst is
// shown to load the directory or delay sign-ins.
func (o *outage) clear(now time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.raised, o.refused, o.last, o.cleared = false, false, nil, now
}

// quietWait is how long the probe loop waits from now for the quiet probe;
// ok false ends the loop, once the outage clears or the cache closes.
func (o *outage) quietWait(now time.Time) (wait time.Duration, ok bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.raised || o.closed {
		o.probing = false
		return 0, false
	}
	return o.probed.Add(dirQuietProbe).Sub(now), true
}

// quiet reports whether the probe loop asks the directory at now: true while
// the outage is raised and no probe was sent for dirQuietProbe.
func (o *outage) quiet(now time.Time) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.raised || now.Sub(o.probed) < dirQuietProbe {
		return false
	}
	o.probed = now
	return true
}

// close starts no more probe loops and waits for the one running, which its
// caller has ended.
func (o *outage) close() {
	o.mu.Lock()
	o.closed = true
	o.mu.Unlock()
	o.loops.Wait()
}

// refuse records a refusal, which makes a raised outage unhealthy until it
// clears. A refusal while none is raised is dropped by the next raise.
//
// decision: once a lookup is refused the condition stays unhealthy until the
// directory answers, even while later lookups are served stale: some user is
// being turned away, which is what the unhealthy severity reports (RFC 25
// §3.2), and a severity that went back and forth with each lookup would page
// and resolve in turn. Overturned if RFC 25's registry asks for the severity
// of the last lookup instead.
func (o *outage) refuse() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.refused = true
}

// conditions appends directory_unreachable while the outage is raised.
func (o *outage) conditions(dst []health.Condition) []health.Condition {
	o.mu.Lock()
	raised, refused, last := o.raised, o.refused, o.last
	o.mu.Unlock()
	if !raised {
		return dst
	}
	c := health.Condition{
		Name:    conditionDirectory,
		Subject: health.Subject{Scope: health.ScopeNode},
		Cause:   errDirServedStale,
		Detail:  "the directory failed this node's queries; lookups are served stale within the grace",
	}
	if refused {
		c.Cause = errDirRefused
		c.Detail = "the directory failed this node's queries; a lookup was refused"
	}
	// The cause stays the sentinel the severity is read from; the failure
	// itself, a bind credential or a timeout, is named here.
	c.Detail += ": " + last.Error()
	return append(dst, c)
}

// directoryConditions appends directory_unreachable while this node's
// queries to the applied directory fail (RFC 18 §5.3, §9.2). A changed
// record's new client starts with none.
func (s *Server) directoryConditions(dst []health.Condition) []health.Condition {
	st := s.state.Load()
	if st == nil || st.dir == nil {
		return dst
	}
	return st.dir.out.conditions(dst)
}
