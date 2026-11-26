package segment

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/cubbit/dittofs/pkg/journal/internal/core"
)

// ReadAheadWindow is how far ahead of a stream's append position the
// read-ahead worker asks for a segment's pages: the first window from the
// header at the roll, and the next one each time the position passes half of
// the last.
//
// decision: one window is 256 MiB, the default segment cap, so at that cap the
// hint is the single whole-segment hint a roll asks for, the form measured at
// 2.06× the large sequential row on darwin and 5.4× its small-write control.
// A smaller window was measured only outside the journal, an 8 MiB hint every
// 4 MiB reaching a quarter of the resident speed with its p99 still near a
// millisecond, so no smaller figure carries evidence at the default cap. Above
// it the window bounds what a stream holds resident: one whole-segment hint
// over 1 GiB left 60% resident and gained nothing, the cache evicting the
// front the writer needs first. Overturned by a smaller window measured to
// match at 256 MiB, or by a cap above the window measured to gain nothing.
const ReadAheadWindow = int64(256 << 20)

// Hinter is the worker that asks the platform to bring a stream's active
// segment into the page cache ahead of its records. A segment the Maker made
// was allocated and never written, so none of its pages is resident, and a
// record that ends mid-page makes the kernel fill the rest of that page first,
// on the writer's own call; the hint lets that fill run ahead of the writes.
// One per journal, over the journal's own streams, as the idle sealer is: a
// stream names the range it wants under its own append lock and wakes the
// worker, and the worker takes the range and makes the call outside every
// lock, so no append waits on a hint. darwin's F_RDADVISE over never-written
// space returns only once its pages are resident, tens of milliseconds for a
// 256 MiB window, which is why the call runs here rather than on the writer.
//
// decision: the hint is advisory and its failure is logged at Debug and
// dropped. It changes no byte, no ordering and no durability, so nothing reads
// its answer and no refusal path waits on it. Only a platform file that
// implements storage.ReadAheader takes it, which is darwin's alone: Linux's
// spares are made by writing zeros over the whole segment, so their pages are
// resident unless the cache evicted them since, and Windows' spares carry no
// measurement of the cost. Give Linux posix_fadvise(WILLNEED), and Windows its
// own, when a measurement on either shows record writes paying for
// non-resident pages.
type Hinter struct {
	streams *Streams
	window  int64
	log     *slog.Logger

	// decision: mu is a leaf lock over the worker's lifecycle flags alone, as
	// the idle sealer's is. It guards started and stopped, is never held
	// across I/O or a stream call, and the ranges the worker takes stay under
	// each stream's own append domain.
	mu      sync.Mutex // guards started, stopped
	started bool
	stopped bool

	wake chan struct{} // one pending wake; a stream's send never blocks
	done chan struct{}
	stop sync.Once
	wg   sync.WaitGroup
}

// NewHinter returns the read-ahead worker over streams, asking window bytes
// ahead of each stream's append position, and installs it on every stream: a
// segment a stream takes from then on is hinted, one taken before is not. A
// nil streams or a window below one is a caller bug and panics; a nil log
// discards. It does not start the goroutine — Start does, and Stop joins it.
func NewHinter(streams *Streams, window int64, log *slog.Logger) *Hinter {
	if streams == nil {
		panic("segment: a read-ahead worker needs its streams")
	}
	if window < 1 {
		panic(fmt.Sprintf("segment: the read-ahead window %d is below 1", window))
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	h := &Hinter{
		streams: streams,
		window:  window,
		log:     log,
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	for i := range streams.Len() {
		s := streams.Stream(i)
		s.appendMu.Lock()
		s.hinter, s.hintStep = h, max(window/2, 1)
		s.appendMu.Unlock()
	}
	return h
}

// Start launches the worker. It is idempotent, and a no-op after Stop.
func (h *Hinter) Start() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.started || h.stopped {
		return
	}
	// Registered under mu, so a Stop racing Start cannot return from its Wait
	// before the worker it must join exists.
	h.started = true
	h.wg.Add(1)
	go h.run()
}

// Stop stops the worker and joins it: a hint in flight finishes, and releases
// the descriptor reference it holds, before Stop returns, so a stop that runs
// before the descriptor cache's Shutdown leaves the cache no reference to
// wait on. A range a stream asks for afterwards is never taken. It returns at
// once before Start, and a second Stop is a no-op.
func (h *Hinter) Stop() {
	h.stop.Do(func() {
		h.mu.Lock()
		h.stopped = true
		h.mu.Unlock()
		close(h.done)
	})
	h.wg.Wait()
}

// run waits for a wake and walks every stream once per wake, until Stop closes
// done. The done check between streams lets a Stop interrupt a walk after the
// hint in flight rather than after every stream's.
//
// ponytail: each wake walks every stream, O(streams) per wake, with at most
// one wake per window per stream. Upgrade to a queue of the streams that
// asked when a profile shows the walk.
func (h *Hinter) run() {
	defer h.wg.Done()
	for {
		select {
		case <-h.done:
			return
		case <-h.wake:
		}
		for i := range h.streams.Len() {
			select {
			case <-h.done:
				return
			default:
			}
			h.hint(h.streams.Stream(i))
		}
	}
}

// hint takes what the stream asked for and asks it of the segment's file,
// through a descriptor reference of its own, so the segment stays open under
// the call however the stream moves meanwhile. A reference the cache refuses
// skips the hint, as its failure does.
func (h *Hinter) hint(s *Stream) {
	id, off, n := s.takeHint()
	if n <= 0 {
		return
	}
	ref, err := s.cache.Acquire(id)
	if err != nil {
		h.log.Debug("segment: read-ahead hint skipped", "segment", id, "err", err)
		return
	}
	err = ref.ReadAhead(off, n)
	ref.Release()
	if err != nil {
		h.log.Debug("segment: read-ahead hint refused", "segment", id, "off", off, "len", n, "err", err)
	}
}

// hintAheadLocked moves the active segment's hinted front to a window past the
// append position, capped at the segment's cap, adds the span to what the
// worker has yet to take, and wakes the worker. With no worker installed it
// moves the front to the cap, so no later reservation asks again. The caller
// holds appendMu; the wake is a send that never blocks and allocates nothing,
// so a reservation that crosses a step pays a few comparisons and the send.
func (s *Stream) hintAheadLocked() {
	if s.hinter == nil {
		s.hinted = s.set.cap
		return
	}
	if s.hintFrom == s.hintTo {
		s.hintFrom = s.hinted
	}
	s.hinted = min(s.set.cap, s.next+s.hinter.window)
	s.hintTo = s.hinted
	select {
	case s.hinter.wake <- struct{}{}:
	default:
	}
}

// takeHint returns the range of the active segment the worker has yet to
// hint, and marks it taken; n is zero when there is none or the stream is
// closed.
func (s *Stream) takeHint() (id core.SegmentID, off, n int64) {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	if s.closed || s.hintFrom == s.hintTo {
		return 0, 0, 0
	}
	id, off, n = s.id, s.hintFrom, s.hintTo-s.hintFrom
	s.hintFrom = s.hintTo
	return id, off, n
}
