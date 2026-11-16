package core_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/internal/adapter/adaptertest/testproto"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/resources"
)

// key is the address every test's records are charged to.
var key = netip.MustParseAddr("127.0.0.1")

// pair returns two ends of a loopback connection: the server's and the
// client's.
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

// record frames body as testproto does: its length, and the flag saying more
// fragments follow when last is false.
func record(body []byte, last bool) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(len(body)))
	if !last {
		b[0] |= 0x80
	}
	return append(b, body...)
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
	buf, err := resources.NewBufPool(b).Get(context.Background(), key, n)
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
	return core.NewTestReader(srv, framer(), pool, key, timeout), budgets, pool
}

// Records of every class, written one by one, come out whole and in order,
// each charged at its class while it is held.
func TestRecordsComeOutWhole(t *testing.T) {
	srv, cli := pair(t)
	r, budgets, _ := readerOf(t, srv, 2*chargeOf(t, 1<<20), 0)
	for _, n := range []int{0, 40, 4000, 5000, 1 << 20} {
		body := payload(n)
		go func() { _, _ = cli.Write(record(body, true)) }()
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
// order.
func TestABurstIsEveryRecord(t *testing.T) {
	srv, cli := pair(t)
	r, _, _ := readerOf(t, srv, 2*chargeOf(t, 1<<20), 0)
	var all []byte
	for i := range 100 {
		all = append(all, record(payload(10+i%40), true)...)
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
	hdr := record(nil, true)
	binary.BigEndian.PutUint32(hdr, 1<<20+1)
	if _, err := cli.Write(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(context.Background()); err == nil {
		t.Fatal("a header past the cap is accepted")
	}
	if held := budgets.Receive.Reserved(); held != 0 {
		t.Errorf("a header past the cap reserves %d bytes", held)
	}
}

// With the receive budget full, the reader waits after the header and reads
// nothing past it; once room frees the record comes out.
func TestAFullBudgetWaits(t *testing.T) {
	srv, cli := pair(t)
	budgets := &resources.Budgets{Receive: resources.NewWaitBudget("receive", chargeOf(t, 1<<20))}
	adaptertest.CheckLeaks(t, budgets)
	pool := resources.NewBufPool(budgets.Receive)
	held, err := pool.Get(context.Background(), key, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	r := core.NewTestReader(srv, framer(), pool, key, 0)
	body := payload(40)
	if _, err := cli.Write(record(body, true)); err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("the reader returned with %v with no room to reserve", err)
	case <-time.After(50 * time.Millisecond):
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
		all = append(all, record(part, i == 2)...)
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
		_, _ = cli.Write(record(payload(1<<20), false))
		_, _ = cli.Write(record(payload(1), true))
	}()
	if _, err := r.Read(context.Background()); err == nil {
		t.Error("a fragment past the cap is accepted")
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
			_, _ = cli.Write(record(payload(1), i == 4))
		}
	}()
	if _, err := r.Read(context.Background()); err == nil {
		t.Error("a fifth fragment is accepted")
	}
	if held := budgets.Receive.Reserved(); held != 0 {
		t.Errorf("a framing error leaves %d bytes held", held)
	}
}

// The frame deadline runs from the record's first byte: a body that stops
// half way is an error at the deadline, and a connection that sends nothing is
// not timed out.
func TestFrameDeadline(t *testing.T) {
	srv, cli := pair(t)
	r, budgets, _ := readerOf(t, srv, 2*chargeOf(t, 1<<20), 100*time.Millisecond)
	body := payload(100)
	if _, err := cli.Write(record(body, true)[:4+50]); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := r.Read(context.Background())
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("a stalled body returns %v, want the deadline", err)
	}
	if d := time.Since(start); d < 80*time.Millisecond || d > 2*time.Second {
		t.Errorf("the deadline came at %v, want about 100ms", d)
	}
	if held := budgets.Receive.Reserved(); held != 0 {
		t.Errorf("a stalled body leaves %d bytes held", held)
	}
}

// A connection that sends nothing is not timed out: the deadline starts at
// the record's first byte.
func TestASilentConnectionIsNotTimedOut(t *testing.T) {
	srv, cli := pair(t)
	r, _, _ := readerOf(t, srv, 2*chargeOf(t, 1<<20), 20*time.Millisecond)
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
		t.Fatalf("a silent connection returns %v", err)
	case <-time.After(60 * time.Millisecond):
	}
	if _, err := cli.Write(record(payload(10), true)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a record after silence: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("the record after silence did not come out")
	}
}
