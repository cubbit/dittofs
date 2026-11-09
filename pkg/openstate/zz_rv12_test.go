package openstate

import (
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

// A break that closes a disconnected durable open with delete on close
// makes the file delete pending and hands back its unlink; the open that
// forced the break must not be granted on the file being unlinked.
func TestRV12BreakCloseDeleteOnCloseThenGrant(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smb("pc-a")), f.caller(t, smb("pc-b"))
	file, r := f.fileIn(t, "doc")
	r.Access, r.Deny, r.Want, r.DeleteOnClose = rd|del, wr, rwh, true
	f.mustOpen(t, a, file, keep(r, entity.DurabilityDurable, s1, time.Minute, 1))
	f.disconnect(t, a, s1)
	f.sink.closed = nil
	ob, err := f.open(b, file, OpenRequest{Access: wr, Via: r.Via, Name: r.Name})
	var unlinked bool
	for _, c := range f.sink.closed {
		unlinked = unlinked || c.r.Unlink != nil
	}
	t.Logf("b's open: err=%v; closed=%+v", err, f.sink.closed)
	if err == nil && unlinked {
		t.Errorf("b granted an open (%d) of the file the same step handed back for unlink", ob.ID)
	}
	if err != nil && !errors.Is(err, ErrDeletePending) {
		t.Logf("b refused with %v", err)
	}
}

// A break whose lease write is refused starts no recall, and so holds no
// place under maxUnsent: unsent is back where it was.
func TestRV12RefusedBreakWriteReleasesItsUnsent(t *testing.T) {
	fs, st := failingFS(t)
	f := reopen(t, fs, &fakeClock{})
	a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smbMachine("pc-b"))
	file, r := f.fileIn(t, "doc")
	r.Access, r.Lease, r.Epoch, r.Want = rd|wr, LeaseKey{5}, ptr[uint16](3), rwh
	if _, err := f.Opens().Open(ctx, a, file, keep(r, entity.DurabilityPersistent, s1, time.Minute, 1)); err != nil {
		t.Fatal(err)
	}
	st.fail.Store(true)
	_, err := f.open(b, file, OpenRequest{Access: rd})
	st.fail.Store(false)
	if !errors.Is(err, errWrite) {
		t.Fatalf("break over a refusing store: %v", err)
	}
	f.idx.Lock()
	n := f.unsent
	f.idx.Unlock()
	if n != 0 {
		t.Errorf("unsent after a refused break: %d, want 0", n)
	}
}

// Disconnect, reconnect, conflicting opens, closes and sweeps on one file
// race, for -race and the table's invariants.
func TestRV12DurableRaces(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smbMachine("pc-b"))
	file, r := f.fileIn(t, "doc")
	r.Access, r.Deny, r.Want, r.Lease = rd|wr, wr, rwh, LeaseKey{7}
	for i := 0; i < 200; i++ {
		s := entity.SessionRef{Node: 1, NodeEpoch: 1, ID: uint64(10 + 2*i)}
		s2 := entity.SessionRef{Node: 1, NodeEpoch: 1, ID: uint64(11 + 2*i)}
		d := entity.DurabilityDurable
		if i%2 == 0 {
			d = entity.DurabilityPersistent
		}
		o, err := f.open(a, file, keep(r, d, s, time.Minute, byte(i)))
		if err != nil {
			f.idle(2 * time.Minute)
			continue
		}
		done := make(chan struct{}, 4)
		go func() { _ = f.Disconnect(ctx, a.Client, s); done <- struct{}{} }()
		go func() {
			_, _ = f.reconnect(a, o, ReconnectMatch{Share: file.Share, Create: [16]byte{byte(i)}, Lease: LeaseKey{7}}, s2)
			done <- struct{}{}
		}()
		go func() {
			if ob, err := f.open(b, file, OpenRequest{Access: wr}); err == nil {
				_, _ = f.Opens().Close(ctx, b, refOf(ob), Seqs{})
			}
			done <- struct{}{}
		}()
		go func() { f.idle(30 * time.Second); done <- struct{}{} }()
		for j := 0; j < 4; j++ {
			<-done
		}
		_ = f.Disconnect(ctx, a.Client, s2)
		f.idle(2 * time.Minute)
		f.idx.Lock()
		left := len(f.byID)
		f.idx.Unlock()
		if left != 0 {
			t.Fatalf("round %d: %d opens left after every Timeout passed", i, left)
		}
		if got := f.stored(t, file); len(got) != 0 {
			t.Fatalf("round %d: records left: %+v", i, got)
		}
	}
}
