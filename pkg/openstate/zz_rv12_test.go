package openstate

import (
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

// P1: MS-SMB2 3.3.5.9.13 judges the version before the GENERIC_READ gate, so
// an older-version create from a caller who may not read is FORCED_CLOSED.
func TestRV12VersionJudgedBeforeTheReadGate(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smbMachine("pc-b"))
	file := anyFile()
	as := func(who Caller, v uint64, read bool) OpenRequest {
		q := keep(OpenRequest{Access: rd}, entity.DurabilityDurable, s1, time.Hour, byte(who.Client))
		q.Durable.Instance, q.MaxRead = AppInstance{ID: [16]byte{9}, Version: [2]uint64{1, v}}, read
		return q
	}
	f.mustOpen(t, a, file, as(a, 5, true))
	if _, err := f.open(b, file, as(b, 4, false)); !errors.Is(err, ErrInstanceVersion) {
		t.Errorf("an older-version create by a caller who may not read: %v; want ErrInstanceVersion", err)
	}
}

// P2 (pins sameCreate's parked check, which no test does): a replay on the
// session a disconnected open still names is ErrCreateInUse, not Replayed.
func TestRV12ReplayOfADisconnectedOpenOnItsOwnSession(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, smbMachine("pc-a"))
	file, r := f.fileIn(t, "disk")
	r.Access = rd
	q := keep(r, entity.DurabilityPersistent, s1, time.Hour, 1)
	f.mustOpen(t, a, file, q)
	f.disconnect(t, a, s1)
	q.Replay = true
	if res, err := f.Opens().Open(ctx, a, file, q); !errors.Is(err, ErrCreateInUse) {
		t.Errorf("a replay of a disconnected open on its old session: %+v, %v; want ErrCreateInUse", res, err)
	}
}

// P3 (pins takeOver's stream check): an app-instance create of a stream
// does not close another client's open of the base under the instance.
func TestRV12AppInstanceOfAStreamKeepsTheBase(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smbMachine("pc-b"))
	file := anyFile()
	as := func(who Caller, s entity.FileID) OpenRequest {
		q := keep(OpenRequest{Access: rd, Stream: s}, entity.DurabilityDurable, s1, time.Hour, byte(who.Client))
		q.Durable.Instance, q.MaxRead = AppInstance{ID: [16]byte{9}}, true
		return q
	}
	f.mustOpen(t, a, file, as(a, entity.FileID{}))
	f.mustOpen(t, b, file, as(b, entity.NewFileID()))
	if n := len(f.entryOf(file.ID).opens); n != 2 {
		t.Errorf("%d opens; want the base open kept beside the stream's", n)
	}
}

// P4: a request counted under a sequence the open's comes back round to
// (three forward steps of at most 0x7FFF) is uncounted from the wrong
// bucket, so the earlier bucket never drains and every later replay is stale.
func TestRV12ChannelLeaveAfterTheSequenceComesRound(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, smbMachine("pc-a"))
	file := anyFile()
	o := f.mustOpen(t, a, file, keep(OpenRequest{Access: rd | wr}, entity.DurabilityDurable, s1, time.Hour, 1))
	ch := func(seq uint16, replay bool) (bool, func()) {
		t.Helper()
		stale, done, err := f.Opens().Channel(ctx, a, o.ID, seq, replay)
		if err != nil {
			t.Fatal(err)
		}
		return stale, done
	}
	_, long := ch(0, false) // a request kept in flight, a blocked lock say
	for _, s := range []uint16{0x5555, 0xaaaa, 0} {
		_, d := ch(s, false)
		d()
	}
	long()
	if stale, _ := ch(0, true); stale {
		t.Error("a replay once every request left is stale")
	}
}

// P5: a lock refused by a failing store stores no lock sequence: its retry
// is applied, not answered as a replay of a lock never granted.
func TestRV12LockRefusedByTheStoreIsNoReplay(t *testing.T) {
	fs, st := failingFS(t)
	f := reopen(t, fs, &fakeClock{})
	a := f.caller(t, smbMachine("pc-a"))
	file, r := f.fileIn(t, "disk")
	r.Access = rd | wr
	o := f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, time.Hour, 1))
	q := LockReq{Owner: smbOwner(a, o), Open: byID(o), Ranges: []LockRange{x(0, 10)}, Replay: &LockSequence{Index: 1, Seq: 1}}
	st.fail.Store(true)
	if _, err := f.lock(a, file, q); !errors.Is(err, errWrite) {
		t.Fatalf("lock over a failing store: %v", err)
	}
	st.fail.Store(false)
	f.mustLock(t, a, file, q)
	if h := f.holder(t, file, br(0, 10)); h == nil {
		t.Error("the retry was answered as a replay; want it applied")
	}
}

// P6: a channel step refused by a failing store changes nothing: the same
// step then succeeds and is written.
func TestRV12ChannelStepRefusedByTheStore(t *testing.T) {
	fs, st := failingFS(t)
	f := reopen(t, fs, &fakeClock{})
	a := f.caller(t, smbMachine("pc-a"))
	file, r := f.fileIn(t, "disk")
	r.Access, r.Channel = rd|wr, 5
	o := f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, time.Hour, 1))
	st.fail.Store(true)
	if _, _, err := f.Opens().Channel(ctx, a, o.ID, 6, false); !errors.Is(err, errWrite) {
		t.Fatalf("a step over a failing store: %v", err)
	}
	st.fail.Store(false)
	if stale, _, _ := f.Opens().Channel(ctx, a, o.ID, 5, false); stale {
		t.Error("the refused step moved the open's sequence")
	}
	if recs := f.stored(t, file); len(recs) != 1 || recs[0].ChannelSeq != 5 {
		t.Errorf("stored %+v; want sequence 5", recs)
	}
}
