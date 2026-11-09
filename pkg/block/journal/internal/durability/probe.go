package durability

import "github.com/cubbit/dittofs/pkg/block/journal/internal/core"

// Probe states.
const (
	probeIdle int32 = iota
	probeRunning
	probeStopped
)

// startProbe starts the read-only probe, once, however many times and from
// however many goroutines the journal is entered. It takes no lock, so Enter
// stays callable from a storage-call sink under a segment append domain: one
// compare-and-swap decides who starts it, and a Stop that ran first wins, so
// no probe starts after Stop.
func (d *Durability) startProbe() {
	if !d.probe.CompareAndSwap(probeIdle, probeRunning) {
		return
	}
	go d.runProbe()
}

// stopProbe keeps a probe from starting and waits for the one that started to
// return, so none calls Check or MakeSpare once Stop has returned. It runs
// after done is closed.
func (d *Durability) stopProbe() {
	if d.probe.CompareAndSwap(probeIdle, probeStopped) {
		return
	}
	<-d.probeDone
}

// runProbe is the probe: every core.ProbeInterval it runs the checks open
// makes of the mount and directory and, once they pass, makes one spare
// segment for the reopen. It leaves the state in place whatever it finds: the
// journal is read-only until the caller closes and reopens it.
//
// With a real core.Clock, the After(core.ProbeInterval) wait a Stop
// interrupts stays armed, a timer on a time-based clock rather than a
// goroutine of the probe, for up to that interval after Stop returns; it then
// closes a channel nobody reads, which is harmless.
//
// ponytail: the interval is a fixed core.ProbeInterval, and the journal stays
// read-only, reads included, until its caller closes and reopens it. Upgrade
// to a Resume that leaves the state in place, with a backoff on the probe,
// when an operator workflow needs reads uninterrupted or the state is seen to
// cycle.
//
// decision: a journal closed by a refused reopen answers retry-later whatever
// refused it, though a check refusal such as a mount without cache flushes is
// one only an operator clears and would otherwise be an immediate error. It is
// reached only by a race: a check that passed at the probe and failed at the
// open moments later. Overturn it if the failed check is seen to hold a
// journal closed, which would map the refusal the way the entering error is
// mapped.
func (d *Durability) runProbe() {
	defer close(d.probeDone)
	for {
		select {
		case <-d.done:
			return
		case <-d.clock.After(core.ProbeInterval):
		}
		// A tick and the stop can be ready together; select picks either.
		select {
		case <-d.done:
			return
		default:
		}
		d.probeOnce()
	}
}

// probeOnce runs one probe. It holds no lock across Check or MakeSpare.
//
// decision: a nil Config.Check runs no check before the spare, so the probe
// makes one at its first tick. That is right only for a journal whose open
// makes no check of its mount or directory, as in tests of the layer alone.
// The caller that wires a real journal must always pass the checks its open
// makes: a spare made on a mount the open would refuse is wasted, and the
// journal stays read-only however often it is reopened. The ceiling is that
// nothing here can tell the two callers apart. Overturned by a real journal
// wired with no Check, which would make Check required in New.
func (d *Durability) probeOnce() {
	if d.check != nil {
		if err := d.check(); err != nil {
			d.log.Debug("journal read-only probe: check failed", "err", err)
			d.setCheck(err)
			return
		}
	}
	d.setCheck(nil)
	if d.makeSpare == nil || d.ReadOnlyState().SpareMade {
		return
	}
	if err := d.makeSpare(); err != nil {
		// An EROFS reaches Enter through the wrapped storage; anything else
		// is tried again at the next tick.
		d.log.Debug("journal read-only probe: spare not made", "err", err)
		return
	}
	d.setSpareMade()
}
