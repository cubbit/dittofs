package core_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/internal/adapter/adaptertest/testproto"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/resources"
)

// chargeOfChunk is what a reply of n bytes reserves, learned from a pool of
// its own.
func chargeOfChunk(n int) int64 {
	b := resources.NewWaitBudget("probe", 1<<30)
	buf, err := resources.NewBufPool(b).Get(context.Background(), netip.Addr{}, n)
	if err != nil {
		panic(err)
	}
	c := b.Reserved()
	buf.Release()
	return c
}

// buildWriter is the connection's writer, held to the suite of out every
// writer passes.
func buildWriter(t *testing.T) newOut {
	return func(f core.Framer) (func(*resources.Buf, uint64, uint64) error, func(error), net.Conn, bound) {
		conn, peer := net.Pipe()
		w := core.NewTestWriter(conn, f, 0, func(error) {})
		closeOut := func(error) {
			w.Stop()
			_ = conn.Close()
		}
		// Every subtest of the suite closes the writer inside its own bubble;
		// a cleanup here would close a bubble's channel after it ends.
		return w.Out, closeOut, peer, bound{msgs: core.WriteBound}
	}
}

// The writer passes the suite of out, and leaves no goroutine or descriptor
// behind.
func TestTheWriterPassesTheSuiteOfOut(t *testing.T) {
	adaptertest.CheckLeaks(t, nil)
	testOut(t, buildWriter(t))
}

// A message the peer holds the write of, and nine queued behind it, go out in
// one gathered write.
func TestTheGatheredWrite(t *testing.T) {
	f := testproto.New(adapter.Deps{}).Framer()
	conn, peer := net.Pipe()
	defer func() { _ = peer.Close() }()
	budget := resources.NewWaitBudget("reply", 1<<30)
	adaptertest.CheckLeaks(t, &resources.Budgets{Reply: budget})
	pool := resources.NewBufPool(budget)
	w := core.NewTestWriter(conn, f, 0, func(error) {})
	defer w.Stop()
	batches := make(chan int, 16)
	core.OnBatch(w, func(n int) { batches <- n })
	msg := func(body string) *resources.Buf {
		b, err := pool.Get(context.Background(), netip.Addr{}, len(body))
		if err != nil {
			t.Fatal(err)
		}
		b.B = append(b.B[:0], body...)
		return b
	}
	if err := w.Out(msg("first"), 0, 0); err != nil {
		t.Fatal(err)
	}
	if n := <-batches; n != 1 {
		t.Fatalf("the first write gathered %d messages, want 1", n)
	}
	for i := range 9 {
		if err := w.Out(msg(string(rune('a'+i))), 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	// A backstop, so a writer that sends one message per wake fails here rather
	// than hanging the test.
	if err := peer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		hdr := make([]byte, f.HeaderLen())
		if _, err := io.ReadFull(peer, hdr); err != nil {
			t.Fatal(err)
		}
		n, _, err := f.BodyLen(hdr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(peer, make([]byte, n)); err != nil {
			t.Fatal(err)
		}
	}
	if n := <-batches; n != 9 {
		t.Errorf("the write after the first gathered %d messages, want 9", n)
	}
	// A later batch's hook follows the one before it, whose messages are
	// released by then: queue one and read it, then only its charge is held.
	if err := w.Out(msg("z"), 0, 0); err != nil {
		t.Fatal(err)
	}
	if n := <-batches; n != 1 {
		t.Fatalf("the last write gathered %d messages, want 1", n)
	}
	if r, want := budget.Reserved(), chargeOfChunk(len("z")); r != want {
		t.Errorf("%d bytes are reserved after the last batch, want the one message's %d", r, want)
	}
	hdr := make([]byte, f.HeaderLen())
	if _, err := io.ReadFull(peer, hdr); err != nil {
		t.Fatal(err)
	}
	n, _, err := f.BodyLen(hdr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(peer, make([]byte, n)); err != nil {
		t.Fatal(err)
	}
}

// A peer that reads nothing closes the connection at write_timeout, not
// before, and every queued message is released.
func TestTheSilentPeerCloses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := testproto.New(adapter.Deps{}).Framer()
		conn, peer := net.Pipe()
		var mu sync.Mutex
		var closeErr error
		closed := make(chan time.Time, 1)
		budget := resources.NewWaitBudget("reply", 1<<30)
		adaptertest.CheckLeaks(t, &resources.Budgets{Reply: budget})
		pool := resources.NewBufPool(budget)
		w := core.NewTestWriter(conn, f, time.Second, func(err error) {
			mu.Lock()
			closeErr = err
			mu.Unlock()
			closed <- time.Now()
		})
		defer w.Stop()
		defer func() { _ = peer.Close() }()
		msg, err := pool.Get(context.Background(), netip.Addr{}, 5)
		if err != nil {
			t.Fatal(err)
		}
		msg.B = append(msg.B[:0], "stuck"...)
		start := time.Now()
		if err := w.Out(msg, 0, 0); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		select {
		case <-closed:
			t.Fatal("the connection closed before the deadline")
		default:
		}
		time.Sleep(900 * time.Millisecond)
		select {
		case <-closed:
			t.Fatal("the connection closed before write_timeout")
		default:
		}
		time.Sleep(200 * time.Millisecond)
		select {
		case at := <-closed:
			if d := at.Sub(start); d < time.Second || d > 1100*time.Millisecond {
				t.Errorf("the connection closed at %v, want write_timeout", d)
			}
			mu.Lock()
			defer mu.Unlock()
			if !errors.Is(closeErr, os.ErrDeadlineExceeded) {
				t.Errorf("the connection closed with %v, want the write's deadline", closeErr)
			}
		case <-time.After(time.Second):
			t.Fatal("the connection did not close at write_timeout")
		}
		if r := budget.Reserved(); r != 0 {
			t.Errorf("%d bytes are still reserved after the close", r)
		}
	})
}

// A write to a peer that has closed calls the connection's close with the
// write's error.
func TestTheClosedPeerCloses(t *testing.T) {
	f := testproto.New(adapter.Deps{}).Framer()
	conn, peer := net.Pipe()
	defer func() { _ = conn.Close() }()
	closed := make(chan error, 1)
	budget := resources.NewWaitBudget("reply", 1<<30)
	adaptertest.CheckLeaks(t, &resources.Budgets{Reply: budget})
	pool := resources.NewBufPool(budget)
	w := core.NewTestWriter(conn, f, 0, func(err error) { closed <- err })
	defer w.Stop()
	msg, err := pool.Get(context.Background(), netip.Addr{}, 4)
	if err != nil {
		t.Fatal(err)
	}
	msg.B = append(msg.B[:0], "gone"...)
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Out(msg, 0, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("the connection closed with %v, want the write's error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("a write to a closed peer did not close the connection")
	}
	if r := budget.Reserved(); r != 0 {
		t.Errorf("%d bytes are still reserved after a failed write", r)
	}
}

// readRecord reads one framed record from peer.
func readRecord(peer net.Conn, f core.Framer) error {
	hdr := make([]byte, f.HeaderLen())
	if _, err := io.ReadFull(peer, hdr); err != nil {
		return err
	}
	n, _, err := f.BodyLen(hdr)
	if err != nil {
		return err
	}
	_, err = io.ReadFull(peer, make([]byte, n))
	return err
}

// The deadline is armed for each gathered write, not once for the connection: a
// second batch starts its own write_timeout when the first ends.
func TestTheDeadlineIsArmedForEachBatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := framer()
		conn, peer := net.Pipe()
		defer func() { _ = peer.Close() }()
		closed := make(chan time.Time, 1)
		budget := resources.NewWaitBudget("reply", 1<<30)
		adaptertest.CheckLeaks(t, &resources.Budgets{Reply: budget})
		pool := resources.NewBufPool(budget)
		w := core.NewTestWriter(conn, f, time.Second, func(error) { closed <- time.Now() })
		defer w.Stop()
		msg := func(body string) *resources.Buf {
			b, err := pool.Get(context.Background(), netip.Addr{}, len(body))
			if err != nil {
				t.Fatal(err)
			}
			b.B = append(b.B[:0], body...)
			return b
		}
		start := time.Now()
		if err := w.Out(msg("first"), 0, 0); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		time.Sleep(500 * time.Millisecond)
		if err := readRecord(peer, f); err != nil { // the first batch's write ends here
			t.Fatal(err)
		}
		if err := w.Out(msg("second"), 0, 0); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		time.Sleep(600 * time.Millisecond) // 1.1s: a deadline armed once would have closed
		select {
		case <-closed:
			t.Fatal("the connection closed at the first batch's deadline")
		default:
		}
		time.Sleep(600 * time.Millisecond) // 1.7s: past the second batch's 1.5s
		select {
		case at := <-closed:
			if d := at.Sub(start); d < 1400*time.Millisecond || d > 1700*time.Millisecond {
				t.Errorf("the connection closed at %v, want the second batch's deadline", d)
			}
		case <-time.After(time.Second):
			t.Fatal("the second batch's write never timed out")
		}
	})
}

// A failed write releases the queue behind the one it held.
func TestAFailedWriteReleasesTheQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := framer()
		conn, peer := net.Pipe()
		budget := resources.NewWaitBudget("reply", 1<<30)
		adaptertest.CheckLeaks(t, &resources.Budgets{Reply: budget})
		pool := resources.NewBufPool(budget)
		w := core.NewTestWriter(conn, f, 0, func(error) {})
		defer w.Stop()
		msg := func(body string) *resources.Buf {
			b, err := pool.Get(context.Background(), netip.Addr{}, len(body))
			if err != nil {
				t.Fatal(err)
			}
			b.B = append(b.B[:0], body...)
			return b
		}
		if err := w.Out(msg("held"), 0, 0); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if err := w.Out(msg("behind"), 0, 0); err != nil {
			t.Fatal(err)
		}
		_ = peer.Close() // the write fails; both messages must be released
		synctest.Wait()
		if r := budget.Reserved(); r != 0 {
			t.Errorf("%d bytes are still reserved after a failed write", r)
		}
	})
}

// stop ends a write the peer holds rather than waiting for write_timeout, and
// releases every message before it returns.
func TestStopEndsAHeldWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := framer()
		conn, peer := net.Pipe()
		defer func() { _ = peer.Close() }()
		budget := resources.NewWaitBudget("reply", 1<<30)
		adaptertest.CheckLeaks(t, &resources.Budgets{Reply: budget})
		pool := resources.NewBufPool(budget)
		w := core.NewTestWriter(conn, f, time.Minute, func(error) {})
		b, err := pool.Get(context.Background(), netip.Addr{}, 4)
		if err != nil {
			t.Fatal(err)
		}
		b.B = append(b.B[:0], "held"...)
		if err := w.Out(b, 0, 0); err != nil {
			t.Fatal(err)
		}
		synctest.Wait() // the writer is in the write; the peer reads nothing
		before := time.Now()
		w.Stop()
		// The stop ended the write rather than waiting out write_timeout: the
		// bubble's clock has not moved.
		if moved := time.Since(before); moved != 0 {
			t.Errorf("stop waited %v on the held write, want none", moved)
		}
		if r := budget.Reserved(); r != 0 {
			t.Errorf("%d bytes are still reserved after stop returned", r)
		}
	})
}

// The bound counts every message out took, the batch being written included,
// and a reader paused at it is released when the messages fall below.
func TestTheBoundReleasesTheReader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := framer()
		conn, peer := net.Pipe()
		defer func() { _ = peer.Close() }()
		budget := resources.NewWaitBudget("reply", 1<<30)
		adaptertest.CheckLeaks(t, &resources.Budgets{Reply: budget})
		pool := resources.NewBufPool(budget)
		w := core.NewTestWriter(conn, f, 0, func(error) {})
		defer w.Stop()
		for i := range core.WriteBound {
			b, err := pool.Get(context.Background(), netip.Addr{}, 1)
			if err != nil {
				t.Fatal(err)
			}
			b.B = append(b.B[:0], byte('a'+i%26))
			if err := w.Out(b, 0, 0); err != nil {
				t.Fatal(err)
			}
		}
		synctest.Wait()
		room, at := core.Pause(w)
		if !at || room == nil {
			t.Fatalf("%d messages held, pause says %v", core.WriteBound, at)
		}
		select {
		case <-room:
			t.Fatal("the bound's channel is closed while every message is still held")
		default:
		}
		read := make(chan error, 1)
		go func() {
			for range core.WriteBound {
				if err := readRecord(peer, f); err != nil {
					read <- err
					return
				}
			}
			read <- nil
		}()
		synctest.Wait()
		select {
		case <-room:
		default:
			t.Fatal("the bound never released the reader once the messages were written")
		}
		if err := <-read; err != nil {
			t.Fatalf("reading the records: %v", err)
		}
	})
}

// A failed write calls the connection's close once the writer has ended, so a
// close that stops the writer, as the connection's teardown does, returns and
// every queued message is released.
func TestACloseThatStopsTheWriterReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := framer()
		conn, peer := net.Pipe()
		defer func() { _ = conn.Close() }()
		budget := resources.NewWaitBudget("reply", 1<<30)
		adaptertest.CheckLeaks(t, &resources.Budgets{Reply: budget})
		pool := resources.NewBufPool(budget)
		var w *core.TestWriter
		stopped := make(chan error, 1)
		w = core.NewTestWriter(conn, f, 0, func(err error) {
			w.Stop()
			stopped <- err
		})
		_ = peer.Close()
		for _, body := range []string{"gone", "behind"} {
			b, err := pool.Get(context.Background(), netip.Addr{}, len(body))
			if err != nil {
				t.Fatal(err)
			}
			b.B = append(b.B[:0], body...)
			if err := w.Out(b, 0, 0); err != nil {
				t.Fatal(err)
			}
		}
		synctest.Wait()
		select {
		case err := <-stopped:
			if !errors.Is(err, io.ErrClosedPipe) {
				t.Errorf("the connection closed with %v, want the write's error", err)
			}
		default:
			t.Fatal("a close that stops the writer never returned")
		}
		if r := budget.Reserved(); r != 0 {
			t.Errorf("%d bytes are still reserved after the close", r)
		}
	})
}

// A gathered write carries bodies up to the protocol's record cap, so a peer
// that reads slowly but steadily gets a write_timeout per batch, not one over
// everything queued, and is not closed.
func TestABatchStopsAtTheRecordCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := framer()
		conn, peer := net.Pipe()
		defer func() { _ = peer.Close() }()
		budget := resources.NewWaitBudget("reply", 1<<30)
		adaptertest.CheckLeaks(t, &resources.Budgets{Reply: budget})
		pool := resources.NewBufPool(budget)
		closed := make(chan error, 1)
		w := core.NewTestWriter(conn, f, time.Second, func(err error) { closed <- err })
		defer w.Stop()
		var mu sync.Mutex
		var batches []int
		core.OnBatch(w, func(n int) {
			mu.Lock()
			batches = append(batches, n)
			mu.Unlock()
		})
		msg := func(n int) *resources.Buf {
			b, err := pool.Get(context.Background(), netip.Addr{}, n)
			if err != nil {
				t.Fatal(err)
			}
			b.B = append(b.B[:0], make([]byte, n)...)
			return b
		}
		if err := w.Out(msg(1), 0, 0); err != nil { // holds the writer while the rest queues
			t.Fatal(err)
		}
		synctest.Wait()
		capBytes, _ := f.MaxRecord()
		for range 4 {
			if err := w.Out(msg(capBytes/2), 0, 0); err != nil {
				t.Fatal(err)
			}
		}
		// One record each 400 ms: 1.6 s for the four, past write_timeout, but
		// 0.8 s for each batch of two. The read deadline is a backstop, so a
		// write cut at write_timeout fails here rather than hanging the test.
		_ = peer.SetReadDeadline(time.Now().Add(10 * time.Second))
		for range 5 {
			if err := readRecord(peer, f); err != nil {
				select {
				case cerr := <-closed:
					t.Fatalf("a peer reading steadily was closed: %v", cerr)
				default:
					t.Fatal(err)
				}
			}
			time.Sleep(400 * time.Millisecond)
		}
		select {
		case err := <-closed:
			t.Fatalf("a peer reading steadily was closed: %v", err)
		default:
		}
		mu.Lock()
		defer mu.Unlock()
		if want := []int{1, 2, 2}; !slices.Equal(batches, want) {
			t.Errorf("the gathered writes took %v messages, want %v", batches, want)
		}
	})
}
