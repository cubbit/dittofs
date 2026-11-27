package core_test

import (
	"slices"
	"sync"
	"testing"

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
	l := &ledgerReplay{}
	s := ledgered(t, l)
	s0, d0 := l.counts()

	s.serve(testproto.Msg{Op: testproto.OpCreate, Version: 1, ID: 9, Session: 7, Tree: 1, Name: "z"})
	if r := s.read(); r.Status != testproto.StatusNoFH {
		t.Fatalf("the Create with no filehandle is answered %+v, want its row's refusal", r)
	}
	s.serve(testproto.Msg{Op: testproto.OpPutFH, Version: 1, ID: 8, Session: 7, Tree: 1, Handle: s.root})
	if r := s.read(); r.Status != testproto.StatusOK {
		t.Fatalf("the PutFH is answered %+v", r)
	}
	if s1, d1 := l.counts(); d1-d0 != 2 || s1-s0 != 0 {
		t.Fatalf("after the refusal and the PutFH: %d stored, %d dropped; want both dropped, none stored", s1-s0, d1-d0)
	}

	s.serve(s.fhThenCreate(1, "a")...)
	if st := s.statuses(); !slices.Equal(st, []core.Status{testproto.StatusOK, testproto.StatusOK}) {
		t.Fatalf("the chain's Create is answered %v", st)
	}
	if s1, d1 := l.counts(); s1-s0 != 1 || d1-d0 != 3 {
		t.Fatalf("after a successful related Create: %d stored, %d dropped; want its reply stored, all else dropped", s1-s0, d1-d0)
	}

	s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
	s.serve(s.fhThenCreate(3, "b")...)
	if st := s.statuses(); !slices.Equal(st, []core.Status{testproto.StatusOK, testproto.StatusDelay}) {
		t.Fatalf("the delayed Create is answered %v, want the delay status", st)
	}
	s.faults.Disarm()
	if s1, d1 := l.counts(); s1-s0 != 1 || d1-d0 != 5 {
		t.Fatalf("after the delayed Create: %d stored, %d dropped; want both members dropped", s1-s0, d1-d0)
	}

	s.faults.Fail(vfs.OpCreate, vfs.ErrExist)
	s.serve(s.fhThenCreate(5, "c")...)
	if st := s.statuses(); !slices.Equal(st, []core.Status{testproto.StatusOK, testproto.StatusFault}) {
		t.Fatalf("the faulted Create is answered %v, want the fault", st)
	}
	s.faults.Disarm()

	s1, d1 := l.counts()
	if s1-s0 != 1 {
		t.Errorf("the run stored %d replies, want only the successful Create's: a delay, a fault and a refusal before the handler are never stored", s1-s0)
	}
	if d1-d0 != 7 {
		t.Errorf("the run dropped %d calls, want seven: the refusal, the PutFH, and the two members of each of the three chains", d1-d0)
	}
}

// A compound's reply is stored only when a member that ran has a Cacheable
// row, whatever the members after it answered: a PutFH and a Create is
// stored, a compound whose delayed Read ends it after a Create is stored too,
// a compound of members with no Cacheable row among them is dropped, and one
// refused whole at its arguments is dropped (RFC 20 §4.7).
func TestACompoundIsStoredWhereAMemberThatRanIsCacheable(t *testing.T) {
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
	s.compound(3, put, testproto.Msg{Op: testproto.OpCreate, Name: "b"}, testproto.Msg{Op: testproto.OpRead, Count: 4})
	if st, rs := s.results(); st != testproto.StatusDelay || len(rs) < 2 {
		t.Fatalf("the compound ended at the delayed Read with %d and %v", st, rs)
	}
	s.faults.Disarm()

	// A compound whose arguments hold a byte after its last member is refused
	// whole at its arguments: no member runs, so nothing is stored and the
	// call is dropped.
	s.serveBody(append(body(s.f, testproto.Msg{Op: testproto.OpCompound, Version: 1, ID: 4, Session: 7, Tree: 1, Members: []testproto.Msg{put}, Tag: tagOf(4)}), 0))
	if r := s.read(); r.Status != testproto.StatusDecode {
		t.Fatalf("the compound refused whole is answered %+v, want the decode refusal", r)
	}

	s1, d1 := l.counts()
	if s1-s0 != 2 {
		t.Errorf("the compounds stored %d replies, want two: the Create that ran is stored whichever member ended the compound after it", s1-s0)
	}
	if d1-d0 != 2 {
		t.Errorf("the compounds dropped %d calls, want two: the Echo compound and the one refused whole", d1-d0)
	}
}
