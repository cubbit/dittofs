package core_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/internal/adapter/adaptertest/testproto"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/resources"
)

// readerKey is the address every test's records are charged to.
var readerKey = netip.MustParseAddr("127.0.0.1") // the package's tests use key elsewhere, another name here

// pair returns two ends of a loopback connection: the server's and the
// client's. The deadline tests use net.Pipe in a synctest bubble instead: its
// deadlines run on the bubble's clock and a read blocked on it is durably
// blocked. A pipe holds no bytes, so a client's write there returns only once
// the reader has read it.
func pair(t *testing.T) (srv, cli net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	acc := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			acc <- c
		}
	}()
	cli, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	srv = <-acc
	t.Cleanup(func() {
		_ = srv.Close()
		_ = cli.Close()
		_ = ln.Close()
	})
	return srv, cli
}

// framedRecord frames body as testproto does: its length, and the flag saying
// more fragments follow when last is false.
func framedRecord(body []byte, last bool) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(len(body)))
	if !last {
		b[0] |= 0x80
	}
	return append(b, body...)
}

// One connection's read buffer is the small class's capacity, so a burst of
// small records is read once (RFC 20 section 6.8): the pool's own buffer for a
// small body pins the coupling, and either side moving without the other
// fails it.
func TestTheReadBufferIsTheSmallClass(t *testing.T) {
	b := resources.NewWaitBudget("probe", 1<<20)
	buf, err := resources.NewBufPool(b).Get(context.Background(), readerKey, 8)
	if err != nil {
		t.Fatal(err)
	}
	c := cap(buf.B)
	buf.Release()
	if c != 4<<10 {
		t.Errorf("the small class holds %d bytes where the reader buffers %d", c, 4<<10)
	}
}

// payload is n bytes.
func payload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

// chargeOf is what a record of n bytes is charged: what a buffer of n reserves
// from a pool of its own.
func chargeOf(t *testing.T, n int) int64 {
	t.Helper()
	b := resources.NewWaitBudget("probe", 1<<30)
	buf, err := resources.NewBufPool(b).Get(context.Background(), readerKey, n)
	if err != nil {
		t.Fatal(err)
	}
	c := b.Reserved()
	buf.Release()
	return c
}

// framer is testproto's.
func framer() core.Framer { return testproto.New(adapter.Deps{}).Framer() }

// readerOf returns a record cut over the pair's server end, charged from a
// receive pool of ceiling, and its budgets.
func readerOf(t *testing.T, srv net.Conn, ceiling int64, timeout time.Duration) (*core.TestReader, *resources.Budgets, *resources.BufPool) {
	t.Helper()
	budgets := &resources.Budgets{Receive: resources.NewWaitBudget("receive", ceiling)}
	adaptertest.CheckLeaks(t, budgets)
	pool := resources.NewBufPool(budgets.Receive)
	return core.NewTestReader(srv, framer(), pool, readerKey, timeout), budgets, pool
}

// Records of every class, written one by one, come out whole and in order,
// each charged at its class while it is held.
func TestRecordsComeOutWhole(t *testing.T) {
	srv, cli := pair(t)
	r, budgets, _ := readerOf(t, srv, 2*chargeOf(t, 1<<20), time.Second)
	for _, n := range []int{0, 40, 4000, 5000, 1 << 20} {
		body := payload(n)
		go func() { _, _ = cli.Write(framedRecord(body, true)) }()
		rec, err := r.Read(context.Background())
		if err != nil {
			t.Fatalf("a record of %d bytes: %v", n, err)
		}
		if !bytes.Equal(rec.B, body) {
			t.Errorf("a record of %d bytes comes out as %d bytes", n, len(rec.B))
		}
		if want := chargeOf(t, n); budgets.Receive.Reserved() != want {
			t.Errorf("a record of %d bytes is charged %d, want %d", n, budgets.Receive.Reserved(), want)
		}
		rec.Release()
		if held := budgets.Receive.Reserved(); held != 0 {
			t.Errorf("a released record leaves %d bytes held", held)
		}
	}
}

// A burst of small records written at once is cut into every record, in
// order. The reader's deadline is set so a record it never finishes comes back
// as an error rather than hanging the test.
func TestABurstIsEveryRecord(t *testing.T) {
	srv, cli := pair(t)
	r, _, _ := readerOf(t, srv, 2*chargeOf(t, 1<<20), time.Second)
	var all []byte
	for i := range 100 {
		all = append(all, framedRecord(payload(10+i%40), true)...)
	}
	if _, err := cli.Write(all); err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		want := payload(10 + i%40)
		rec, err := r.Read(context.Background())
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		if !bytes.Equal(rec.B, want) {
			t.Errorf("record %d comes out as %x, want %x", i, rec.B, want)
		}
		rec.Release()
	}
}

// A header claiming a byte past the cap is refused before anything is
// reserved.
func TestAHeaderPastTheCapReservesNothing(t *testing.T) {
	srv, cli := pair(t)
	r, budgets, _ := readerOf(t, srv, 2*chargeOf(t, 1<<20), 0)
	hdr := framedRecord(nil, true)
	binary.BigEndian.PutUint32(hdr, 1<<20+1)
	if _, err := cli.Write(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(context.Background()); !errors.Is(err, core.ErrFrame) {
		t.Fatalf("a header past the cap returns %v, want a framing refusal", err)
	}
	if held := budgets.Receive.Reserved(); held != 0 {
		t.Errorf("a header past the cap reserves %d bytes", held)
	}
}

// With the receive budget full, the reader waits after the header and reads
// nothing past it; once room frees the record comes out.
func TestAFullBudgetWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv, cli := net.Pipe()
		defer func() { _, _ = srv.Close(), cli.Close() }()
		budgets := &resources.Budgets{Receive: resources.NewWaitBudget("receive", chargeOf(t, 1<<20))}
		adaptertest.CheckLeaks(t, budgets)
		pool := resources.NewBufPool(budgets.Receive)
		held, err := pool.Get(context.Background(), readerKey, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		r := core.NewTestReader(srv, framer(), pool, readerKey, 0)
		go func() { _, _ = cli.Write(framedRecord(payload(40), true)) }()
		done := make(chan error, 1)
		go func() {
			rec, err := r.Read(context.Background())
			if rec != nil {
				rec.Release()
			}
			done <- err
		}()
		// The bubble says the reader is durably blocked at the budget, not
		// merely ahead of a wall-clock race.
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("the reader returned with %v with no room to reserve", err)
		default:
		}
		held.Release()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("once room frees: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("the record did not come out once room freed")
		}
	})
}

// A record in fragments is one record, charged once at the cap's class, and a
// fragment past the cap or past the fragment count is a framing error that
// releases the reservation.
func TestFragments(t *testing.T) {
	srv, cli := pair(t)
	r, budgets, _ := readerOf(t, srv, 2*chargeOf(t, 1<<20), 0)

	var all, want []byte
	for i, n := range []int{1024, 512, 100} {
		part := payload(n)
		want = append(want, part...)
		all = append(all, framedRecord(part, i == 2)...)
	}
	go func() { _, _ = cli.Write(all) }()
	rec, err := r.Read(context.Background())
	if err != nil {
		t.Fatalf("three fragments: %v", err)
	}
	if !bytes.Equal(rec.B, want) {
		t.Errorf("three fragments come out as %d bytes, want %d", len(rec.B), len(want))
	}
	if got, want := budgets.Receive.Reserved(), chargeOf(t, 1<<20); got != want {
		t.Errorf("a fragmented record is charged %d, want the cap's class %d", got, want)
	}
	rec.Release()
	if held := budgets.Receive.Reserved(); held != 0 {
		t.Errorf("a released fragmented record leaves %d bytes held", held)
	}
}

// A fragment that takes a record past the cap is a framing error, its
// reservation released.
func TestAFragmentPastTheCap(t *testing.T) {
	srv, cli := pair(t)
	r, budgets, _ := readerOf(t, srv, 2*chargeOf(t, 1<<20), 0)
	go func() {
		_, _ = cli.Write(framedRecord(payload(1<<20), false))
		_, _ = cli.Write(framedRecord(payload(1), true))
	}()
	if _, err := r.Read(context.Background()); !errors.Is(err, core.ErrFrame) {
		t.Errorf("a fragment past the cap returns %v, want a framing refusal", err)
	}
	if held := budgets.Receive.Reserved(); held != 0 {
		t.Errorf("a framing error leaves %d bytes held", held)
	}
}

// A record past the fragment count is a framing error, its reservation
// released.
func TestAFifthFragment(t *testing.T) {
	srv, cli := pair(t)
	r, budgets, _ := readerOf(t, srv, 2*chargeOf(t, 1<<20), 0)
	go func() {
		for i := range 5 {
			_, _ = cli.Write(framedRecord(payload(1), i == 4))
		}
	}()
	if _, err := r.Read(context.Background()); !errors.Is(err, core.ErrFrame) {
		t.Errorf("a fifth fragment returns %v, want a framing refusal", err)
	}
	if held := budgets.Receive.Reserved(); held != 0 {
		t.Errorf("a framing error leaves %d bytes held", held)
	}
}

// readerHolds fails the test unless the client's write has returned: a pipe
// hands a write over only as it is read, so its return says the reader holds
// what follows the record it returned.
func readerHolds(t *testing.T, sent <-chan error, what string) {
	t.Helper()
	synctest.Wait()
	select {
	case err := <-sent:
		if err != nil {
			t.Fatalf("the write of %s: %v", what, err)
		}
	default:
		t.Fatalf("%s are not read with the record before them", what)
	}
}

// The frame deadline runs from the record's first byte: a body that stops
// half way is an error at the deadline, and a connection that sends nothing is
// not timed out.
func TestFrameDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv, cli := net.Pipe()
		defer func() { _, _ = srv.Close(), cli.Close() }()
		r, budgets, _ := readerOf(t, srv, 2*chargeOf(t, 1<<20), 100*time.Millisecond)
		go func() { _, _ = cli.Write(framedRecord(payload(100), true)[:4+50]) }()
		start := time.Now()
		done := make(chan error, 1)
		go func() {
			rec, err := r.Read(context.Background())
			if rec != nil {
				rec.Release()
			}
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("a stalled body returns %v, want the deadline", err)
			}
			if d := time.Since(start); d != 100*time.Millisecond {
				t.Errorf("the deadline came at %v, want 100ms", d)
			}
		case <-time.After(time.Second):
			t.Fatal("a stalled body was not timed out")
		}
		if held := budgets.Receive.Reserved(); held != 0 {
			t.Errorf("a stalled body leaves %d bytes held", held)
		}
	})
}

// A connection that sends nothing is not timed out: the deadline starts at
// the record's first byte.
func TestASilentConnectionIsNotTimedOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv, cli := net.Pipe()
		defer func() { _, _ = srv.Close(), cli.Close() }()
		r, _, _ := readerOf(t, srv, 2*chargeOf(t, 1<<20), 20*time.Millisecond)
		done := make(chan error, 1)
		go func() {
			rec, err := r.Read(context.Background())
			if rec != nil {
				rec.Release()
			}
			done <- err
		}()
		time.Sleep(60 * time.Millisecond) // past read_timeout, on the bubble's clock
		select {
		case err := <-done:
			t.Fatalf("a silent connection returns %v", err)
		default:
		}
		go func() { _, _ = cli.Write(framedRecord(payload(10), true)) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("a record after silence: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("the record after silence did not come out")
		}
	})
}

// A record's deadline stops when the record ends: the next record's first bytes,
// already read, wait as long as the client does.
func TestTheDeadlineStopsAfterARecord(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv, cli := net.Pipe()
		defer func() { _, _ = srv.Close(), cli.Close() }()
		r, _, _ := readerOf(t, srv, 2*chargeOf(t, 1<<20), 50*time.Millisecond)
		first, second := framedRecord(payload(20), true), framedRecord(payload(20), true)
		sent := make(chan error, 1)
		go func() {
			_, err := cli.Write(append(append([]byte{}, first...), second[:2]...))
			sent <- err
		}()
		rec, err := r.Read(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(rec.B, payload(20)) {
			t.Errorf("the first record is %x", rec.B)
		}
		rec.Release()
		readerHolds(t, sent, "two bytes of the next record")
		// The client pauses past read_timeout, two bytes of the next record
		// already read.
		time.Sleep(150 * time.Millisecond)
		go func() { _, _ = cli.Write(second[2:]) }()
		rec, err = r.Read(context.Background())
		if err != nil {
			t.Fatalf("a pause between records: %v", err)
		}
		if !bytes.Equal(rec.B, payload(20)) {
			t.Errorf("the second record is %x", rec.B)
		}
		rec.Release()
		// And with nothing buffered: the deadline does not carry over to a record
		// that comes after a pause either.
		time.Sleep(150 * time.Millisecond)
		go func() { _, _ = cli.Write(framedRecord(payload(30), true)) }()
		rec, err = r.Read(context.Background())
		if err != nil {
			t.Fatalf("a record after a pause: %v", err)
		}
		if !bytes.Equal(rec.B, payload(30)) {
			t.Errorf("the third record is %x", rec.B)
		}
		rec.Release()
	})
}

// A header that trickles is timed out: the record's deadline starts at its
// first byte.
func TestATrickledHeaderTimesOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv, cli := net.Pipe()
		defer func() { _, _ = srv.Close(), cli.Close() }()
		r, budgets, _ := readerOf(t, srv, 2*chargeOf(t, 1<<20), 50*time.Millisecond)
		go func() { _, _ = cli.Write(framedRecord(payload(20), true)[:2]) }()
		// A backstop, so a reader that arms no deadline fails here rather than
		// deadlocking the bubble.
		if err := srv.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		_, err := r.Read(context.Background())
		if d := time.Since(start); !errors.Is(err, os.ErrDeadlineExceeded) || d != 50*time.Millisecond {
			t.Fatalf("a trickled header returns %v after %v, want the deadline at 50ms", err, d)
		}
		if held := budgets.Receive.Reserved(); held != 0 {
			t.Errorf("a trickled header leaves %d bytes held", held)
		}
	})
}

// read_timeout is one deadline per record, counted from the record's first
// byte: a client that trickles a header, each byte inside the last deadline,
// is still cut at the first one, which is the slowloris case the deadline
// exists to stop.
func TestATrickledHeaderIsCutAtItsFirstDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv, cli := net.Pipe()
		defer func() { _, _ = srv.Close(), cli.Close() }()
		r, _, _ := readerOf(t, srv, 2*chargeOf(t, 1<<20), 50*time.Millisecond)
		mark := framedRecord(payload(20), true)[:4]
		go func() {
			_, _ = cli.Write(mark[:2])
			time.Sleep(30 * time.Millisecond)
			_, _ = cli.Write(mark[2:3])
			time.Sleep(30 * time.Millisecond)
			_, _ = cli.Write(mark[3:4])
		}()
		// A backstop, so a reader that arms no deadline fails here rather than
		// deadlocking the bubble.
		if err := srv.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		_, err := r.Read(context.Background())
		if d := time.Since(start); !errors.Is(err, os.ErrDeadlineExceeded) || d != 50*time.Millisecond {
			t.Fatalf("a trickled header returns %v after %v, want its first deadline at 50ms", err, d)
		}
	})
}

// The frame deadline runs across the gap between fragments: a non-last
// fragment, then silence, is cut at the deadline and releases the reservation
// its cap drew (RFC 24 section 3.2).
func TestAFragmentedRecordIsCutAtTheDeadlineBetweenFragments(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv, cli := net.Pipe()
		defer func() { _, _ = srv.Close(), cli.Close() }()
		r, budgets, _ := readerOf(t, srv, 2*chargeOf(t, 1<<20), 100*time.Millisecond)
		go func() { _, _ = cli.Write(framedRecord(payload(10), false)) }()
		// A backstop, so a reader that disarms between fragments fails here
		// rather than deadlocking the bubble.
		if err := srv.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		_, err := r.Read(context.Background())
		if d := time.Since(start); !errors.Is(err, os.ErrDeadlineExceeded) || d != 100*time.Millisecond {
			t.Fatalf("a silent fragmented record returns %v after %v, want its deadline at 100ms", err, d)
		}
		if held := budgets.Receive.Reserved(); held != 0 {
			t.Errorf("a cut fragmented record leaves %d bytes held", held)
		}
	})
}

// A partial fragmented record is refused and releases the reservation its cap
// drew: the peer's end inside it, or a next header past the cap, leaves nothing
// held (RFC 24 section 3.2).
func TestAPartialFragmentedRecordReleases(t *testing.T) {
	past := binary.BigEndian.AppendUint32(nil, 1<<20+4<<10+1)
	for _, tc := range []struct {
		name string
		then []byte
		want error
	}{
		{"the peer ends before the next header", nil, io.ErrUnexpectedEOF},
		{"the next header past the cap", past, core.ErrFrame},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, cli := net.Pipe()
			defer func() { _, _ = srv.Close(), cli.Close() }()
			r, budgets, _ := readerOf(t, srv, 2*chargeOf(t, 1<<20), 0)
			go func() {
				_, _ = cli.Write(framedRecord(payload(10), false))
				_, _ = cli.Write(tc.then)
				if tc.then == nil {
					_ = cli.Close()
				}
			}()
			if _, err := r.Read(context.Background()); !errors.Is(err, tc.want) {
				t.Fatalf("Read returns %v, want %v", err, tc.want)
			}
			if held := budgets.Receive.Reserved(); held != 0 {
				t.Errorf("a partial fragmented record leaves %d bytes held", held)
			}
		})
	}
}

// A record the peer cut inside is told apart from a clean close between
// records: the cut returns io.ErrUnexpectedEOF, the close io.EOF.
func TestACutRecordIsToldApartFromACleanClose(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sends []byte
		want  error
	}{
		{"a cut header", framedRecord(payload(20), true)[:2], io.ErrUnexpectedEOF},
		{"a cut body", framedRecord(payload(20), true)[:14], io.ErrUnexpectedEOF},
		{"a clean close between records", nil, io.EOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, cli := net.Pipe()
			defer func() { _, _ = srv.Close(), cli.Close() }()
			r, _, _ := readerOf(t, srv, 2*chargeOf(t, 1<<20), 0)
			go func() {
				_, _ = cli.Write(framedRecord(payload(4), true))
				if len(tc.sends) > 0 {
					_, _ = cli.Write(tc.sends)
				}
				_ = cli.Close()
			}()
			first, err := r.Read(context.Background())
			if err != nil {
				t.Fatal(err) // the whole record first: the cut one comes next
			}
			first.Release()
			_, err = r.Read(context.Background())
			if !errors.Is(err, tc.want) {
				t.Fatalf("Read returns %v, want %v", err, tc.want)
			}
		})
	}
}

// A record waiting for its receive charge is not timed out: the deadline stops
// while it waits and starts a full read_timeout once the charge is granted.
func TestTheDeadlineStopsAtTheReceiveWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv, cli := net.Pipe()
		defer func() { _, _ = srv.Close(), cli.Close() }()
		budgets := &resources.Budgets{Receive: resources.NewWaitBudget("receive", chargeOf(t, 1<<20))}
		adaptertest.CheckLeaks(t, budgets)
		pool := resources.NewBufPool(budgets.Receive)
		held, err := pool.Get(context.Background(), readerKey, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		r := core.NewTestReader(srv, framer(), pool, readerKey, 100*time.Millisecond)
		// Past the read buffer, so the rest of the record is read from the
		// connection after the grant and the deadline restarted there is what
		// it runs under.
		body := payload(64 << 10)
		go func() { _, _ = cli.Write(framedRecord(body, true)) }()
		done := make(chan error, 1)
		go func() {
			rec, err := r.Read(context.Background())
			if rec != nil {
				rec.Release()
			}
			done <- err
		}()
		time.Sleep(300 * time.Millisecond) // longer than read_timeout, while it waits for the charge
		held.Release()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("after the receive wait: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the record did not come out after the grant")
		}
	})
}

// A partial header already buffered is timed out from its own first byte: the
// reader arms when it starts cutting, not only when it reads.
func TestABufferedPartialHeaderTimesOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv, cli := net.Pipe()
		defer func() { _, _ = srv.Close(), cli.Close() }()
		r, _, _ := readerOf(t, srv, 2*chargeOf(t, 1<<20), 50*time.Millisecond)
		first, second := framedRecord(payload(20), true), framedRecord(payload(20), true)
		sent := make(chan error, 1)
		go func() {
			_, err := cli.Write(append(append([]byte{}, first...), second[:3]...))
			sent <- err
		}()
		rec, err := r.Read(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		rec.Release()
		// Three bytes of the next header are read; the client stalls.
		readerHolds(t, sent, "three bytes of the next header")
		if err := srv.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		_, err = r.Read(context.Background())
		if d := time.Since(start); !errors.Is(err, os.ErrDeadlineExceeded) || d != 50*time.Millisecond {
			t.Fatalf("a stalled buffered header returns %v after %v, want the deadline at 50ms", err, d)
		}
	})
}

// A context that ended before the read started stops it without blocking,
// on a client that sends nothing, where no deadline is armed yet.
func TestAReadUnderAnEndedContextDoesNotBlock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv, cli := net.Pipe()
		defer func() { _, _ = srv.Close(), cli.Close() }()
		r, _, _ := readerOf(t, srv, 2*chargeOf(t, 1<<20), 5*time.Second)
		// A backstop, so a read that blocks fails here rather than deadlocking
		// the bubble.
		if err := srv.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		start := time.Now()
		if _, err := r.Read(ctx); !errors.Is(err, context.Canceled) || time.Since(start) != 0 {
			t.Fatalf("a read under an ended context returns %v after %v", err, time.Since(start))
		}
	})
}

// A context that ends while the reader waits for its receive charge stops it
// after the grant rather than reading on.
func TestACancelDuringTheWaitStopsTheRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv, cli := net.Pipe()
		defer func() { _, _ = srv.Close(), cli.Close() }()
		budgets := &resources.Budgets{Receive: resources.NewWaitBudget("receive", chargeOf(t, 1<<20))}
		adaptertest.CheckLeaks(t, budgets)
		pool := resources.NewBufPool(budgets.Receive)
		held, err := pool.Get(context.Background(), readerKey, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		r := core.NewTestReader(srv, framer(), pool, readerKey, 5*time.Second)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			rec, err := r.Read(ctx)
			if rec != nil {
				rec.Release()
			}
			done <- err
		}()
		go func() { _, _ = cli.Write(framedRecord(payload(20), true)) }()
		synctest.Wait() // the reader waits for its receive charge
		cancel()
		held.Release()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("a read cancelled at the wait returns %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the read continued after its context ended")
		}
	})
}

// A context that ends while the reader waits for a record's first byte stops
// it when the byte arrives, before it arms the deadline.
func TestACancelWhileTheHeaderWaitsStopsTheRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv, cli := net.Pipe()
		defer func() { _, _ = srv.Close(), cli.Close() }()
		r, _, _ := readerOf(t, srv, 2*chargeOf(t, 1<<20), 5*time.Second)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			rec, err := r.Read(ctx)
			if rec != nil {
				rec.Release()
			}
			done <- err
		}()
		synctest.Wait() // the reader waits for the first byte
		cancel()
		go func() { _, _ = cli.Write(framedRecord(payload(20), true)) }()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("a read cancelled before its first byte returns %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the read continued after its context ended")
		}
	})
}
