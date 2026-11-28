package core_test

import (
	"context"
	"errors"
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
	"github.com/cubbit/dittofs/pkg/vfs"
)

// ledgerReplay records how many replies the pipeline stores and how many
// looked-up calls it drops, and passes each call through to the cache below
// it: which runs store and which drop is the requirement (RFC 20 §4.7), and
// the cache itself is the protocol's real one.
type ledgerReplay struct {
	core.ReplayCache
	mu     sync.Mutex
	stores int
	drops  int
}

func (l *ledgerReplay) Store(h core.Header, reply *resources.Buf) {
	l.mu.Lock()
	l.stores++
	l.mu.Unlock()
	l.ReplayCache.Store(h, reply)
}

func (l *ledgerReplay) Drop(h core.Header) {
	l.mu.Lock()
	l.drops++
	l.mu.Unlock()
	l.ReplayCache.Drop(h)
}

// counts is the ledger's counts now.
func (l *ledgerReplay) counts() (stores, drops int) {
	l.mu.Lock()

	defer l.mu.Unlock()

	return l.stores, l.drops
}

// ledgered serves messages whose cache records through l, which wraps the
// protocol's own replay stage, whose counts start after the connection's
// setup.
func ledgered(t *testing.T, l *ledgerReplay) *served {
	t.Helper()

	return newServedOn(t, 1<<20, adapter.Deps{}, func(p core.Protocol) core.Protocol {
		return edited{p, func(st *core.Stages) {
			l.ReplayCache = st.Replay
			st.Replay = l
		}}
	})
}

// A single call, each member of a chain among them, is stored only when its
// row is Cacheable and it ran: a related Create that succeeded is stored,
// its PutFH before it, whose row is not Cacheable, is dropped, and a Create
// whose handler answered ErrDelay, one the service answered with no row's
// error (the fault), and a Create refused before its handler by its row's
// checks are dropped, never stored (RFC 20 §4.7).
func TestASingleCallIsStoredOnlyWhereACacheableRowRan(t *testing.T) {
	adaptertest.CheckLeaks(t, nil)

	l := &ledgerReplay{}
	s := ledgered(t, l)
	s0, d0 := l.counts()

	s.serve(testproto.Msg{
		Op: testproto.OpCreate, Version: 1, ID: 9, Session: 7, Tree: 1, Name: "z",
	})

	if r := s.read(); r.Status != testproto.StatusNoFH {
		t.Fatalf("the Create with no filehandle is answered %+v, want its row's refusal", r)
	}

	s.serve(testproto.Msg{
		Op: testproto.OpPutFH, Version: 1, ID: 8, Session: 7, Tree: 1, Handle: s.root,
	})

	if r := s.read(); r.Status != testproto.StatusOK {
		t.Fatalf("the PutFH is answered %+v", r)
	}

	if s1, d1 := l.counts(); d1-d0 != 2 || s1-s0 != 0 {
		t.Fatalf(
			"after the refusal and the PutFH: %d stored, %d dropped; want both dropped",
			s1-s0, d1-d0,
		)
	}

	s.serve(s.fhThenCreate(1, "a")...)

	okays := []core.Status{testproto.StatusOK, testproto.StatusOK}

	if st := s.statuses(); !slices.Equal(st, okays) {
		t.Fatalf("the chain's Create is answered %v", st)
	}

	if s1, d1 := l.counts(); s1-s0 != 1 || d1-d0 != 3 {
		t.Fatalf(
			"after a successful related Create: %d stored, %d dropped; want its reply alone",
			s1-s0, d1-d0,
		)
	}

	s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
	s.serve(s.fhThenCreate(3, "b")...)

	answers := []core.Status{testproto.StatusOK, testproto.StatusDelay}

	if st := s.statuses(); !slices.Equal(st, answers) {
		t.Fatalf("the delayed Create is answered %v, want the delay status", st)
	}

	s.faults.Disarm()

	if s1, d1 := l.counts(); s1-s0 != 1 || d1-d0 != 5 {
		t.Fatalf(
			"after the delayed Create: %d stored, %d dropped; want both members dropped",
			s1-s0, d1-d0,
		)
	}

	s.faults.Fail(vfs.OpCreate, vfs.ErrExist)
	s.serve(s.fhThenCreate(5, "c")...)

	cases := []core.Status{testproto.StatusOK, testproto.StatusFault}

	if st := s.statuses(); !slices.Equal(st, cases) {
		t.Fatalf("the faulted Create is answered %v, want the fault", st)
	}

	s.faults.Disarm()

	s1, d1 := l.counts()
	if s1-s0 != 1 {
		t.Errorf(
			"the run stored %d replies, want only the successful Create's: a delay, "+
				"a fault and a refusal before the handler are never stored",
			s1-s0,
		)
	}

	if d1-d0 != 7 {
		t.Errorf(
			"the run dropped %d calls, want seven: the refusal, the PutFH, and "+
				"the two members of each of the three chains",
			d1-d0,
		)
	}
}

// refutedReplay answers its armed error from Lookup and passes every other
// call through to the cache below it: the refusal a copy of a call still
// running meets at the replay check, injected there, since this protocol's
// cache keeps no in-progress mark of its own to refuse with.
type refutedReplay struct {
	core.ReplayCache
	mu    sync.Mutex
	armed bool
	err   error
}

func (r *refutedReplay) Lookup(ctx context.Context, h core.Header) (*resources.Buf, error) {
	r.mu.Lock()

	defer r.mu.Unlock()

	if !r.armed {
		return r.ReplayCache.Lookup(ctx, h)
	}

	r.armed = false

	return nil, r.err
}

// arm makes the next lookup answer err.
func (r *refutedReplay) arm(err error) {
	r.mu.Lock()

	defer r.mu.Unlock()

	r.armed, r.err = true, err
}

// A request the replay check refused, its lookup answered an error, ends
// clearing no mark: it made none, so its end dropping one would delete the
// mark the call it disturbed left running, which then never answers its own
// retransmissions (RFC 20 §4.7). ErrMoved stands for any lookup error the
// stages map: this protocol's cache refuses no lookup of its own, and the
// delayed copy's refusal its stages map instead would come out of a wait the
// pipeline retries under the request's own deadline.
func TestALookupRefusedByAnErrorDropsNoMark(t *testing.T) {
	adaptertest.CheckLeaks(t, nil)

	synctest.Test(t, func(t *testing.T) {
		l := &ledgerReplay{}
		refuted := &refutedReplay{}
		s := newSeam(t, edited{testproto.New(adapter.Deps{}), func(st *core.Stages) {
			l.ReplayCache = st.Replay
			refuted.ReplayCache = l
			st.Replay = refuted
		}}, 1<<20)

		refuted.arm(core.ErrMoved)
		s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1})

		synctest.Wait()

		if r := s.read(); r.ID != 1 || r.Status != testproto.StatusFault {
			t.Errorf("the refused lookup is answered %+v, want the fault no stages' row maps", r)
		}

		if _, drops := l.counts(); drops != 0 {
			t.Errorf("the refused lookup's end dropped %d marks, want none it did not make", drops)
		}

		s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 2})

		if r := s.read(); r.ID != 2 || r.Status != testproto.StatusOK {
			t.Errorf("the Echo after the refusal is answered %+v, want the connection serving", r)
		}
	})
}

// A compound's reply is stored only when a member that ran has a Cacheable
// row, whatever the members after it answered: a PutFH and a Create is
// stored, a compound whose delayed Read ends it after a Create is stored too,
// a compound whose delayed member is the Create itself is dropped, a delay
// being no final reply, a compound of members with no Cacheable row among
// them is dropped, and one refused whole at its arguments is dropped
// (RFC 20 §4.7).
func TestACompoundIsStoredWhereAMemberThatRanIsCacheable(t *testing.T) {
	adaptertest.CheckLeaks(t, nil)

	l := &ledgerReplay{}
	s := ledgered(t, l)
	s0, d0 := l.counts()
	put := testproto.Msg{Op: testproto.OpPutFH, Handle: s.root}
	create := testproto.Msg{Op: testproto.OpCreate, Name: "a"}
	echo := testproto.Msg{Op: testproto.OpEcho, Payload: []byte("x")}

	s.compound(1, put, create)

	if st, rs := s.results(); st != testproto.StatusOK || len(rs) != 2 {
		t.Fatalf("the compound of a Create is answered %d with %v", st, rs)
	}

	if s1, _ := l.counts(); s1-s0 != 1 {
		t.Fatalf("the compound of a Create stored %d replies, want its one reply", s1-s0)
	}

	s.compound(2, put, echo)

	if st, _ := s.results(); st != testproto.StatusOK {
		t.Fatalf("the compound of an Echo is answered %d", st)
	}

	_, d1 := l.counts()

	if d1-d0 != 1 {
		t.Fatalf("the compound of an Echo dropped %d calls, want its one", d1-d0)
	}

	s.faults.Fail(vfs.OpRead, vfs.ErrDelay)
	s.compound(3, put,
		testproto.Msg{Op: testproto.OpCreate, Name: "b"},
		testproto.Msg{Op: testproto.OpRead, Count: 4},
	)

	if st, rs := s.results(); st != testproto.StatusDelay || len(rs) < 2 {
		t.Fatalf("the compound ended at the delayed Read with %d and %v", st, rs)
	}

	// The delayed member itself, on the row a compound stores, keeps its
	// reply out of the cache as well: a delay is nothing final, so the
	// compound ends at it dropped, and a PutFH before it ran no Cacheable
	// member the cache counts.
	s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
	s.compound(4, put, testproto.Msg{Op: testproto.OpCreate, Name: "c"})

	if st, rs := s.results(); st != testproto.StatusDelay || len(rs) != 2 {
		t.Fatalf("the compound ended at the delayed Create with %d and %v", st, rs)
	}

	s.faults.Disarm()

	if s1, d1 := l.counts(); s1-s0 != 2 || d1-d0 != 2 {
		t.Fatalf(
			"after the delayed Create: %d stored, %d dropped; want the compound dropped, "+
				"the delayed Create stored no reply",
			s1-s0,
			d1-d0,
		)
	}

	// A compound whose arguments hold a byte after its last member is refused
	// whole at its arguments: no member runs, so nothing is stored and the
	// call is dropped.
	refused := testproto.Msg{
		Op: testproto.OpCompound, Version: 1, ID: 5, Session: 7, Tree: 1,
		Members: []testproto.Msg{put}, Tag: tagOf(5),
	}
	s.serveBody(append(body(s.f, refused), 0))

	if r := s.read(); r.Status != testproto.StatusDecode {
		t.Fatalf("the compound refused whole is answered %+v, want the decode refusal", r)
	}

	s1, d1 := l.counts()

	if s1-s0 != 2 {
		t.Errorf(
			"the compounds stored %d replies, want two: the Create that ran is stored "+
				"whichever member ended the compound after it",
			s1-s0,
		)
	}

	if d1-d0 != 3 {
		t.Errorf(
			"the compounds dropped %d calls, want three: the Echo compound, "+
				"the delayed Create and the one refused whole",
			d1-d0,
		)
	}
}

// A status whose buffer the wait's bound never granted ends its request with
// the mark dropped: nothing was ever built to answer the retransmission with,
// so the mark cannot hold the call answering nothing forever. The bound's own
// close is the only end a status of a connection whose reply budget holds
// full has (RFC 20 §4.7). The select's own minute timer is what moves the
// bubble's clock while every goroutine in it blocks, so the bound's 30 s
// pass without the test sleeping them.
func TestAStatusHeldToItsBoundDropsTheMark(t *testing.T) {
	adaptertest.CheckLeaks(t, nil)

	synctest.Test(t, func(t *testing.T) {
		l := &ledgerReplay{}
		s := newSeam(t, edited{testproto.New(adapter.Deps{}), func(st *core.Stages) {
			l.ReplayCache = st.Replay
			st.Replay = l
		}}, smallCharge)

		defer s.full()()

		s.serve(testproto.Msg{Op: testproto.OpTree, Version: 1, ID: 1, Session: 7})

		select {
		case err := <-s.closed:
			if !errors.Is(err, core.ErrReplyWait) {
				t.Fatalf("the status closed with %v, want the reply-wait error", err)
			}
		case <-time.After(time.Minute):
			t.Fatal("the status never closed within a minute of the bubble's time")
		}

		synctest.Wait()

		if _, drops := l.counts(); drops != 1 {
			t.Errorf(
				"the status held to its bound dropped %d marks, want the one its own lookup made",
				drops,
			)
		}
	})
}

// A status the wait's bound ends drops no mark the request did not make: a
// request whose lookup refused, so never marked, ends the same wait, and its
// own mark is nothing, while dropping would delete the mark of a call still
// running whose next copy would then run it again (RFC 20 §4.7). The mark it
// found running belongs to the refused reply the stage below answers, whose
// count the ledger keeps.
func TestAStatusHeldToItsBoundDropsNoMarkItDidNotMake(t *testing.T) {
	adaptertest.CheckLeaks(t, nil)

	synctest.Test(t, func(t *testing.T) {
		l := &ledgerReplay{}
		refuted := &refutedReplay{}
		s := newSeam(t, edited{testproto.New(adapter.Deps{}), func(st *core.Stages) {
			l.ReplayCache = st.Replay
			refuted.ReplayCache = l
			st.Replay = refuted
		}}, smallCharge)

		defer s.full()()

		refuted.arm(core.ErrMoved)
		s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1})

		select {
		case err := <-s.closed:
			if !errors.Is(err, core.ErrReplyWait) {
				t.Fatalf("the status closed with %v, want the reply-wait error", err)
			}
		case <-time.After(time.Minute):
			t.Fatal("the status never closed within a minute of the bubble's time")
		}

		synctest.Wait()

		if _, drops := l.counts(); drops != 0 {
			t.Errorf(
				"the refused lookup's status dropped %d marks, want none the request made",
				drops,
			)
		}
	})
}

// failingEncode is the real header codec with its Encode failing once armed:
// a fault wrapper around the implementation, which everything else answers.
type failingEncode struct {
	core.HeaderCodec
	fail bool
}

func (f *failingEncode) Encode(h core.Header, s core.Status, out *resources.Buf) error {
	if f.fail {
		return errNoEncode
	}

	return f.HeaderCodec.Encode(h, s, out)
}

// A reply whose encoding fails ends its request with the mark dropped: no
// reply was ever built to answer the retransmission with, and the mark
// holding the call would answer nothing forever (RFC 20 §4.7).
func TestAReplyThatDoesNotEncodeDropsTheMark(t *testing.T) {
	adaptertest.CheckLeaks(t, nil)

	synctest.Test(t, func(t *testing.T) {
		l := &ledgerReplay{}
		enc := &failingEncode{}
		s := newSeam(t, edited{testproto.New(adapter.Deps{}), func(st *core.Stages) {
			l.ReplayCache = st.Replay
			st.Replay = l
			enc.HeaderCodec = st.Header
			st.Header = enc
		}}, 1<<20)

		enc.fail = true

		mark := testproto.Msg{
			Op: testproto.OpEcho, Version: 1, ID: 1, Payload: []byte("x"),
		}
		s.serve(mark)
		synctest.Wait()

		if _, drops := l.counts(); drops != 1 {
			t.Errorf(
				"the reply that did not encode dropped %d marks, want the one its own lookup made",
				drops,
			)
		}
	})
}

var errNoEncode = errors.New("a reply that does not encode")

// A reply whose encoding fails drops no mark the request did not make: a
// request whose lookup refused, so never marked, ends at the same failed
// encode, and dropping would delete the mark of a call still running whose
// next copy would then run it again (RFC 20 §4.7).
func TestAReplyThatDoesNotEncodeDropsNoMarkItDidNotMake(t *testing.T) {
	adaptertest.CheckLeaks(t, nil)

	synctest.Test(t, func(t *testing.T) {
		l := &ledgerReplay{}
		refuted := &refutedReplay{}
		enc := &failingEncode{}
		s := newSeam(t, edited{testproto.New(adapter.Deps{}), func(st *core.Stages) {
			l.ReplayCache = st.Replay
			refuted.ReplayCache = l
			st.Replay = refuted
			enc.HeaderCodec = st.Header
			st.Header = enc
		}}, 1<<20)

		refuted.arm(core.ErrMoved)

		enc.fail = true

		mark := testproto.Msg{
			Op: testproto.OpEcho, Version: 1, ID: 1, Payload: []byte("x"),
		}
		s.serve(mark)
		synctest.Wait()

		if _, drops := l.counts(); drops != 0 {
			t.Errorf("the reply that did not encode dropped %d marks, want none it made", drops)
		}
	})
}

// A compound whose reply header does not encode ends before any member
// runs, its own mark dropped: a compound held as a call still running would
// answer every retransmission of it with the connection's close (RFC 20
// section 4.7).
func TestACompoundWhoseReplyDoesNotEncodeDropsTheMark(t *testing.T) {
	adaptertest.CheckLeaks(t, nil)

	l := &ledgerReplay{}
	enc := &failingEncode{}
	s := newServedOn(t, 1<<20, adapter.Deps{}, func(p core.Protocol) core.Protocol {
		return edited{p, func(st *core.Stages) {
			l.ReplayCache = st.Replay
			st.Replay = l
			enc.HeaderCodec = st.Header
			st.Header = enc
		}}
	})

	_, d0 := l.counts()

	enc.fail = true

	put := testproto.Msg{Op: testproto.OpPutFH, Handle: s.root}
	s.compound(1, put)

	// A dropped connection raises no event the served seam can wait for, so
	// the ledger's count is the condition polled, one backstop the wait.
	_, drops := l.counts()

	deadline := time.Now().Add(5 * time.Second)
	for drops-d0 == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)

		_, drops = l.counts()
	}

	if drops-d0 != 1 {
		t.Errorf(
			"the compound that did not encode dropped %d marks, want the one its own lookup made",
			drops-d0,
		)
	}
}
