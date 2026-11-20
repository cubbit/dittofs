package core_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest/testproto"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/resources"
)

// bound is a writer's queue's bound, by messages and by bytes; 0 for none.
type bound struct{ msgs, bytes int }

// newOut builds a writer of records framed by f: its out and close, the peer
// end of the connection it writes to, and its queue's bound.
type newOut func(f core.Framer) (out func(msg *resources.Buf, lo, hi, before uint64) error, close func(error), peer net.Conn, b bound)

// testOut is the suite of out, which every writer passes (the pipeline's
// seam with its connection):
//   - out never blocks its caller, also past its queue's bound;
//   - it takes msg's reference whatever it returns, and the writer releases
//     it once sent or discarded;
//   - it fails only once the connection is closed;
//   - a message handed with before goes out ahead of the first queued
//     message whose keys span it, lo <= before <= hi: a reply alone, lo and
//     hi its key, or a record of several, lo and hi its least and greatest.
func testOut(t *testing.T, build newOut) {
	f := testproto.New(adapter.Deps{}).Framer()
	msg := func(t *testing.T, pool *resources.BufPool, body string) *resources.Buf {
		t.Helper()
		b, err := pool.Get(context.Background(), netip.Addr{}, len(body))
		if err != nil {
			t.Fatal(err)
		}
		b.B = append(b.B[:0], body...)
		return b
	}
	read := func(t *testing.T, peer net.Conn) string {
		t.Helper()
		hdr := make([]byte, f.HeaderLen())
		if _, err := io.ReadFull(peer, hdr); err != nil {
			t.Fatal(err)
		}
		n, _, err := f.BodyLen(hdr)
		if err != nil {
			t.Fatal(err)
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(peer, body); err != nil {
			t.Fatal(err)
		}
		return string(body)
	}

	t.Run("never blocks, and sends in order", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			budget := resources.NewWaitBudget("reply", 1<<30)
			pool := resources.NewBufPool(budget)
			out, closeOut, peer, b := build(f)
			// Past the writer's bound by messages and by bytes, twice over.
			n := max(1000, 2*b.msgs+1)
			size := max(1, 2*b.bytes/n+1)
			msgs := make([]*resources.Buf, n)
			for i := range msgs {
				msgs[i] = msg(t, pool, strings.Repeat(string(rune('a'+i%26)), size))
			}
			// The peer reads nothing until every out has returned.
			returned := make(chan error, 1)
			go func() {
				for i, m := range msgs {
					if err := out(m, 0, 0, 0); err != nil {
						returned <- fmt.Errorf("message %d: out = %w before the close", i, err)
						return
					}
				}
				returned <- nil
			}()
			synctest.Wait()
			select {
			case err := <-returned:
				if err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("out blocked its caller while the peer read nothing")
			}
			for i := range n {
				if got := read(t, peer); got != strings.Repeat(string(rune('a'+i%26)), size) {
					t.Fatalf("message %d read as %q, out of order", i, got)
				}
			}
			closeOut(nil)
			synctest.Wait()
			if r := budget.Reserved(); r != 0 {
				t.Errorf("%d bytes still reserved once every message was sent", r)
			}
		})
	})

	t.Run("fails only once closed, and takes the reference", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			budget := resources.NewWaitBudget("reply", 1<<30)
			pool := resources.NewBufPool(budget)
			out, closeOut, _, _ := build(f)
			if err := out(msg(t, pool, "queued"), 0, 0, 0); err != nil {
				t.Fatalf("out before the close = %v", err)
			}
			closeOut(nil)
			if err := out(msg(t, pool, "late"), 0, 0, 0); err == nil {
				t.Error("out after the close succeeded")
			}
			synctest.Wait()
			if r := budget.Reserved(); r != 0 {
				t.Errorf("%d bytes still reserved after the close: a message's reference was kept", r)
			}
		})
	})

	t.Run("before goes ahead of its key's reply", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			pool := resources.NewBufPool(resources.NewWaitBudget("reply", 1<<30))
			out, closeOut, peer, _ := build(f)
			defer closeOut(nil)
			// The writer is held on the first message until the peer reads,
			// so the reply of key 7 is still queued when the callback comes.
			for _, m := range []struct {
				body           string
				lo, hi, before uint64
			}{{"first", 0, 0, 0}, {"reply", 7, 7, 0}, {"other", 0, 0, 0}, {"callback", 0, 0, 7}} {
				if err := out(msg(t, pool, m.body), m.lo, m.hi, m.before); err != nil {
					t.Fatal(err)
				}
				if m.body == "first" {
					// The writer holds "first" alone before the rest arrives,
					// so the callback can still go ahead of its reply.
					synctest.Wait()
				}
			}
			var got []string
			for range 4 {
				got = append(got, read(t, peer))
			}
			if want := []string{"first", "callback", "reply", "other"}; !slices.Equal(got, want) {
				t.Errorf("sent %v, want %v: the callback ahead of the reply of its key", got, want)
			}
		})
	})

	t.Run("before goes ahead of a record whose keys span it", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			pool := resources.NewBufPool(resources.NewWaitBudget("reply", 1<<30))
			out, closeOut, peer, _ := build(f)
			defer closeOut(nil)
			// A record of the replies keyed 7, 8 and 9 is queued behind the
			// first message, which holds the writer, when a callback names
			// its middle reply.
			for _, m := range []struct {
				body           string
				lo, hi, before uint64
			}{{"first", 0, 0, 0}, {"record", 7, 9, 0}, {"other", 0, 0, 0}, {"callback", 0, 0, 8}} {
				if err := out(msg(t, pool, m.body), m.lo, m.hi, m.before); err != nil {
					t.Fatal(err)
				}
				if m.body == "first" {
					synctest.Wait()
				}
			}
			var got []string
			for range 4 {
				got = append(got, read(t, peer))
			}
			if want := []string{"first", "callback", "record", "other"}; !slices.Equal(got, want) {
				t.Errorf("sent %v, want %v: the callback ahead of the record holding the reply it names", got, want)
			}
		})
	})

	t.Run("before goes ahead of a record whose least or greatest key it is", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			pool := resources.NewBufPool(resources.NewWaitBudget("reply", 1<<30))
			out, closeOut, peer, _ := build(f)
			defer closeOut(nil)
			// A record of the replies keyed 7 to 9 is queued behind the first
			// message, which holds the writer, when one callback names its
			// least key and another its greatest. Only their order against
			// the record is the writer's to keep (RFC 20 section 6.2).
			for _, m := range []struct {
				body           string
				lo, hi, before uint64
			}{{"first", 0, 0, 0}, {"record", 7, 9, 0}, {"least", 0, 0, 7}, {"greatest", 0, 0, 9}} {
				if err := out(msg(t, pool, m.body), m.lo, m.hi, m.before); err != nil {
					t.Fatal(err)
				}
				if m.body == "first" {
					synctest.Wait()
				}
			}
			var got []string
			for range 4 {
				got = append(got, read(t, peer))
			}
			if at := func(body string) int { return slices.Index(got, body) }; at("least") > at("record") || at("greatest") > at("record") {
				t.Errorf("sent %v, want both callbacks ahead of the record whose least and greatest keys they name", got)
			}
		})
	})
}

// recorder is the writer the seam tests run the pipeline on: it queues what
// out takes and writes it, in order, framed, to one end of a net.Pipe, whose
// other end a testproto.Client reads. It is a verified implementation of
// out, held to the suite the connection's writer passes, not a stand-in.
type recorder struct {
	f      core.Framer
	conn   net.Conn
	mu     sync.Mutex
	wake   chan struct{}
	queue  []queued
	closed bool
	done   chan struct{}
}

type queued struct {
	msg    *resources.Buf
	lo, hi uint64
}

// newRecorder starts a recorder framing with f; its queue has no bound.
func newRecorder(f core.Framer) (out func(msg *resources.Buf, lo, hi, before uint64) error, close func(error), peer net.Conn, b bound) {
	conn, peer := net.Pipe()
	r := &recorder{f: f, conn: conn, wake: make(chan struct{}, 1), done: make(chan struct{})}
	go r.write()
	return r.out, r.close, peer, bound{}
}

func (r *recorder) out(msg *resources.Buf, lo, hi, before uint64) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		msg.Release()
		return net.ErrClosed
	}
	at := len(r.queue)
	if before != 0 {
		for i, q := range r.queue {
			if q.lo <= before && before <= q.hi {
				at = i
				break
			}
		}
	}
	r.queue = append(r.queue, queued{})
	copy(r.queue[at+1:], r.queue[at:])
	r.queue[at] = queued{msg, lo, hi}
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
	return nil
}

// write sends each queued message as a record of its own, releasing it once
// written, and on a close or a failed write discards the rest.
func (r *recorder) write() {
	defer close(r.done)
	for {
		r.mu.Lock()
		if r.closed {
			r.drop()
			r.mu.Unlock()
			return
		}
		if len(r.queue) == 0 {
			r.mu.Unlock()
			<-r.wake
			continue
		}
		q := r.queue[0]
		r.queue = r.queue[1:]
		r.mu.Unlock()
		bufs := net.Buffers{r.f.Frame(nil, len(q.msg.B)), q.msg.B}
		_, err := bufs.WriteTo(r.conn)
		q.msg.Release()
		if err != nil {
			r.shut()
		}
	}
}

// drop releases every queued message; r.mu is held.
func (r *recorder) drop() {
	for _, q := range r.queue {
		q.msg.Release()
	}
	r.queue = nil
}

// close shuts the recorder and returns once its writer has released every
// message it held.
func (r *recorder) close(error) {
	r.shut()
	<-r.done
}

// shut refuses out from here on, closes the connection, which ends a write in
// progress, and wakes the writer to discard the rest.
func (r *recorder) shut() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.mu.Unlock()
	_ = r.conn.Close()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// The recorder passes the suite of out.
func TestRecorderPassesTheSuiteOfOut(t *testing.T) {
	testOut(t, newRecorder)
}
