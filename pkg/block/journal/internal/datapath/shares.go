package datapath

import (
	"sync/atomic"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// closedBit is the top bit of a Share's in-flight word; the bits below it
// count the calls in flight.
const closedBit = uint64(1) << 63

// Share is the handle of one share on a core: the tag its writes publish
// under, its limit, and the count of calls running through it.
//
// decision: a Share holds no lock domain and no call takes a lock for it: a
// call never waits on another handle. It carries state, one atomic word
// holding the count of calls in flight and a closed bit, and a channel its
// Close waits on; that state orders a handle's close against its own calls
// only. The one mutex around handles is the core's registryMu, a leaf that
// only Share and Close take, outside the domain order: never held while a lock
// domain is held, while the counters or the index are called, or while Close
// waits for the drain. Revisit when a share gains state that writers must
// serialize.
type Share struct {
	// Tag is the share's tag, fixed for the handle's life.
	Tag core.ShareTag

	owner *Core
	limit atomic.Int64
	// state is the closed bit and the count of calls in flight. A call enters
	// by a compare-and-swap that fails once the bit is set, so the count only
	// falls from then on.
	state atomic.Uint64
	// idle is closed once, by whichever of Close and the last exiting call
	// takes the word to closedBit alone.
	idle chan struct{}
}

// Limit returns the limit the handle was last attached with.
func (s *Share) Limit() int64 { return s.limit.Load() }

// enter admits one call, returning the function that ends it, or
// core.ErrClosed once the handle's close began. A refused call leaves the
// count as it found it.
func (s *Share) enter() (func(), error) {
	for {
		old := s.state.Load()
		if old&closedBit != 0 {
			return nil, core.ErrClosed
		}
		if s.state.CompareAndSwap(old, old+1) {
			return s.exit, nil
		}
	}
}

// exit ends a call. The call that takes a closed handle's count to zero
// wakes Close; Close itself wakes when it finds the count already zero.
func (s *Share) exit() {
	if s.state.Add(^uint64(0)) == closedBit {
		close(s.idle)
	}
}

// Close refuses new calls through the handle, removes it from the registry
// and returns once every call already in flight has returned. The extents the
// handle's writes published stay held. A later Share on the tag makes a fresh
// handle. A second or concurrent Close, from another owner of a shared handle,
// also waits for the calls in flight and then returns core.ErrClosed, so
// whichever Close returns, the handle's calls have ended.
//
// decision: Close waits for the calls in flight without a bound and without a
// context, because it takes none by design, so a Close issued from inside a
// call made through the same handle never returns, and neither does a second
// Close issued from inside one. That is accepted because no caller closes a
// handle from within its own call. Overturned by a caller that does, or by a
// need for a bounded drain, which would give Close a context or a deadline.
//
// decision: Close drops the handle from the registry before it drains, so the
// registry cannot say that a handle of a tag is still draining. A caller that
// must know the calls of a closing handle have ended, such as Forget of the
// tag, has to wait on that handle's own Close, not read the registry.
// Overturned by a registry that keeps a draining handle visible.
func (s *Share) Close() error { return s.owner.closeShare(s) }

// Share attaches a handle for tag with limit, after raising the version
// counter and every floor bucket to floor. When tag already has an open
// handle, that handle is returned with its limit replaced, so two callers of
// one share see one handle.
//
// Tag 0 and a negative limit are refused with core.ErrInvalidSetting; a floor
// with a non-zero epoch is refused with core.ErrUnknownFormat. A refusal
// registers nothing and changes neither the counter nor a bucket. The counter
// and the buckets are raised before the handle is registered, so no write
// through it is issued a version at or below the floor.
//
// ponytail: the registry is an immutable slice replaced by a copy, under a
// mutex only Share and Close take, so an attach or a close costs one slice
// copy and a lookup scans it. Both are rare, and readers take no lock.
// Upgrade to a persistent map if attaches ever become frequent, which is not
// expected.
func (c *Core) Share(tag core.ShareTag, limit int64, floor core.Version) (*Share, error) {
	if tag == 0 || limit < 0 {
		return nil, core.ErrInvalidSetting
	}
	if err := c.counters.RaiseFloor(floor); err != nil {
		return nil, err
	}
	c.index.RaiseFloor(floor)

	c.registryMu.Lock()
	defer c.registryMu.Unlock()
	var open []*Share
	if p := c.registry.Load(); p != nil {
		open = *p
	}
	for _, s := range open {
		if s.Tag == tag {
			s.limit.Store(limit)
			return s, nil
		}
	}
	s := &Share{Tag: tag, owner: c, idle: make(chan struct{})}
	s.limit.Store(limit)
	next := append(open[:len(open):len(open)], s)
	c.registry.Store(&next)
	return s, nil
}

func (c *Core) closeShare(s *Share) error {
	c.registryMu.Lock()
	old := s.state.Load()
	for old&closedBit == 0 && !s.state.CompareAndSwap(old, old|closedBit) {
		old = s.state.Load()
	}
	if old&closedBit != 0 {
		c.registryMu.Unlock()
		<-s.idle
		return core.ErrClosed
	}
	var open []*Share
	if p := c.registry.Load(); p != nil {
		open = *p
	}
	next := make([]*Share, 0, len(open))
	for _, o := range open {
		if o != s {
			next = append(next, o)
		}
	}
	c.registry.Store(&next)
	c.registryMu.Unlock()

	if old == 0 {
		close(s.idle)
	}
	<-s.idle
	return nil
}

// Shares returns the tags of the open handles, in attach order, read from the
// registry without a lock.
func (c *Core) Shares() []core.ShareTag {
	p := c.registry.Load()
	if p == nil {
		return []core.ShareTag{}
	}
	tags := make([]core.ShareTag, len(*p))
	for i, s := range *p {
		tags[i] = s.Tag
	}
	return tags
}
