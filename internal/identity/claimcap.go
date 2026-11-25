package identity

import (
	"container/list"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// The caps on what a Claimed caller's resolution asks the directory and
// mints (RFC 18 §5.5, §7.3): at most claimLimit directory lookups in any
// claimWindow per client unit, the table holding at most claimUnits units,
// and at most claimLimit records minted in any claimWindow per node.
//
// ponytail: the per-address lookup cap and the per-node mint cap are
// constants. Make them settings when a legitimate client, or a first-day
// migration of AUTH_SYS users, is shown to trip them.
const (
	claimLimit  = 100
	claimWindow = time.Minute
	claimUnits  = 1 << 16
)

// reasonLookupCap counts a call refused at either cap.
//
// decision: a call refused at the per-node mint cap is counted lookup_cap,
// as one refused at the per-address lookup cap is, since RFC 18 §9.2 names
// no other reason for it. Add a reason of its own when a dashboard must tell
// the two caps apart.
const reasonLookupCap = "lookup_cap"

// The two caps' refusals: ErrUnavailable, retryable, counted lookup_cap,
// never mapped to anonymous.
var (
	errLookupCap = errors.New("identity: the client's directory lookup cap is reached")
	errMintCap   = errors.New("identity: the node's mint cap is reached")
)

// capRefusal is a call refused at the cap whose error is cause.
func capRefusal(cause error) error {
	return &refusal{reasonLookupCap, fmt.Errorf("%w: %w", vfserr.ErrUnavailable, cause)}
}

// charges is what a Claimed caller's resolution charges its directory
// lookups and mints to: the client its lookups count against. A nil one, a
// proven caller's, charges nothing.
type charges struct {
	s      *Server
	client netip.Addr
}

// lookup charges one directory lookup to c's client, refusing it past the
// lookup cap.
func (c *charges) lookup() error {
	if !c.s.lookupCap.charge(c.client, c.s.deps.Clock.Now()) {
		return capRefusal(errLookupCap)
	}
	return nil
}

// lookupFn is c's lookup, nil for a nil c.
func (c *charges) lookupFn() func() error {
	if c == nil {
		return nil
	}
	return c.lookup
}

// ring is the times of the charges made within the last claimWindow, oldest
// first, at most claimLimit of them, in nanoseconds of the injected clock.
// A charge is admitted only while fewer than claimLimit are held, so no
// claimWindow ever holds more than claimLimit charges: a fixed window would
// admit twice the limit across its boundary, and a token bucket nearly so.
//
// ponytail: a ring holds up to claimLimit eight-byte times, about 52 MB
// with the lookup cap's table full. Count charges per second, summed over
// the last 60, if the table's memory shows in a profile.
type ring struct{ at []int64 }

// room is how many more charges r admits at now, dropping those claimWindow
// old or more.
func (r *ring) room(now int64) int {
	i := 0
	for i < len(r.at) && now-r.at[i] >= int64(claimWindow) {
		i++
	}
	r.at = append(r.at[:0], r.at[i:]...)
	return claimLimit - len(r.at)
}

// charge records n charges at now; room must have admitted them.
func (r *ring) charge(now int64, n int) {
	for range n {
		r.at = append(r.at, now)
	}
}

// refund takes back n charges made at at, which their caller made and did
// not spend.
func (r *ring) refund(at int64, n int) {
	for i := len(r.at) - 1; i >= 0 && n > 0; i-- {
		if r.at[i] == at {
			r.at = append(r.at[:i], r.at[i+1:]...)
			n--
		}
	}
}

// lookupCap is the per-address cap on the directory lookups a Claimed
// caller's resolution sends, one ring per client unit, throttleUnit's.
//
// decision: a full table makes room by evicting its least recently charged
// unit. That unit is idle, its newest charge claimWindow old or more and its
// ring bounding nothing, unless every unit has charged within the window;
// only then is a live ring reset, logged, which takes claimUnits units
// charging at once, a flood that one unit's reset adds little to. So no
// client fills the table to refuse every other, as the logon throttle's full
// table never refuses either. Overturned if a flood across that many units
// is seen, which needs the table charged to RFC 24's connection-state pool.
type lookupCap struct {
	log   *slog.Logger
	bound int

	mu     sync.Mutex
	units  map[netip.Addr]*list.Element // of *capUnit
	recent list.List                    // most recently charged first
}

type capUnit struct {
	unit netip.Addr
	ring ring
	last int64 // the newest charge
}

func newLookupCap(bound int, log *slog.Logger) *lookupCap {
	return &lookupCap{log: log, bound: bound, units: map[netip.Addr]*list.Element{}}
}

// charge charges one lookup to client's unit at now, false when the unit
// has claimLimit lookups within the last claimWindow.
func (c *lookupCap) charge(client netip.Addr, now time.Time) bool {
	unit, t := throttleUnit(client), now.UnixNano()
	c.mu.Lock()
	defer c.mu.Unlock()
	el := c.units[unit]
	if el == nil {
		if len(c.units) >= c.bound {
			old := c.recent.Back().Value.(*capUnit)
			if t-old.last < int64(claimWindow) {
				c.log.Warn("identity: the lookup cap's full table evicted a unit charged within its window", "units", len(c.units))
			}
			c.recent.Remove(c.units[old.unit])
			delete(c.units, old.unit)
		}
		el = c.recent.PushFront(&capUnit{unit: unit})
		c.units[unit] = el
	}
	u := el.Value.(*capUnit)
	if u.ring.room(t) < 1 {
		return false
	}
	u.ring.charge(t, 1)
	u.last = t
	c.recent.MoveToFront(el)
	return true
}

// mintCap is the per-node cap on the records a Claimed caller's resolution
// mints, whatever its client: one ring, charged per record.
type mintCap struct {
	mu   sync.Mutex
	ring ring
}

// take charges up to want records at now and answers how many it charged,
// and the time to refund them at.
func (c *mintCap) take(now time.Time, want int) (got int, at int64) {
	at = now.UnixNano()
	c.mu.Lock()
	defer c.mu.Unlock()
	got = min(want, c.ring.room(at))
	c.ring.charge(at, got)
	return got, at
}

// refund takes back n records take charged at at and did not mint.
func (c *mintCap) refund(at int64, n int) {
	if n == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ring.refund(at, n)
}
