package datapath

import (
	"errors"
)

// BeginClose fences the core: the closed flag goes up — the journal-level
// calls that take no share handle (Draw, SetVersionLimit, Share, Strand)
// refuse from this moment — and every share in the registry is fenced out of
// it, so no new call starts through a handle either. It never waits: the
// calls already in flight are waited for by Close, through the drain
// goroutine the fence starts for every share it fenced here. A share closed
// some other way while the fence ran is left to that close's own wait. It is
// safe to call twice: the second call finds the flag already up and the
// registry already empty.
//
// decision: the fence sets the core's flag before it empties the registry,
// and Core.Share refuses off the flag under the registry's own mutex, so an
// attach racing the fence is either refused or fenced with the registry it
// landed in — a share that registered is one the fence collected, and its
// calls in flight are one of the drain's. The fence is therefore the single
// point after which no new call starts anywhere. Overturned by an attach that
// must succeed against a close already begun, which would need its own
// ordering against the core's shutdown.
func (c *Core) BeginClose() {
	c.closedFlag.Store(true)
	c.registryMu.Lock()
	fenced := c.registry.Swap(&[]*Share{})
	c.registryMu.Unlock()
	if fenced == nil {
		return
	}
	for _, s := range *fenced {
		if !s.fence() {
			continue
		}
		c.drain.Add(1)
		go func(s *Share) {
			defer c.drain.Done()
			<-s.idle
		}(s)
	}
}

// Close stops the journal's core. It begins the close — no new call starts
// anywhere from this moment — waits for every call that was in flight on a
// share the fence collected, and then stops the durability layer, closes the
// streams, stops the idle sealer, the pool and the retirer — every background
// worker joined before the segments it touches are closed, the pool before the
// retirer, as the segment package's group close documents — and last shuts
// the descriptor cache, whose Shutdown waits for every reader's reference and
// then closes every descriptor. It is safe to call twice: a second Close
// returns the first's result and neither fences nor waits again.
//
// decision: the wait below has no bound and no context, because Close takes
// none by design, so each of the drain goroutines waits for its share's
// every call in flight, and a close issued from inside one never returns.
// That is accepted because no caller closes the core from within a call made
// through it. Overturned by a caller that does, or by a need for a bounded
// drain, which would give this close a context or a deadline.
func (c *Core) Close() error {
	c.closeOnce.Do(func() {
		c.BeginClose()
		c.drain.Wait()
		c.dur.Stop()
		serr := c.streams.Close()
		if c.sealer != nil {
			c.sealer.Stop()
		}
		c.pool.Stop()
		if c.retirer != nil {
			c.retirer.Stop()
		}
		cerr := c.cache.Shutdown()
		c.closeErr = errors.Join(serr, cerr)
	})
	return c.closeErr
}
