//go:build adapter_conformance

package testproto

import (
	"bytes"
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest"
)

// Every test named TestConformance here is a specification the change that
// fills its stub builds to. Each fails today on the stub it names and is listed in
// test/adapter/known.yaml until the change that fills the stub. It fails on an
// assertion or a returned error, never a panic: a step that reaches a stub
// that panics runs in the test's goroutine and recovers.

// tablesBuilt routes one header, which builds p's tables, and reports the
// panic of a stub they reach as an error.
func tablesBuilt(p *Protocol) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	rec := &resources.Buf{B: Append(nil, Msg{Op: OpEcho, Version: 1})[lengthLen:]}
	h, err := headerCodec{}.Decode(rec, 0)
	if err != nil {
		return err
	}
	_, _, err = p.NewConn(nil).Router.Route(h, nil)

	return err
}

// One Echo accepted over loopback by a core server on testproto is answered,
// and the server leaks nothing when it stops.
func TestConformanceEchoEndToEnd(t *testing.T) {
	budgets := &resources.Budgets{
		Receive:    resources.NewWaitBudget("receive", 8<<20),
		Reply:      resources.NewWaitBudget("reply", 8<<20),
		Connection: resources.NewBudget("connection", 1<<20),
		Hold:       resources.NewBudget("hold", 1<<20),
	}
	deps := adapter.Deps{VFS: vfstest.Null{}, Budgets: budgets}
	p := New(deps)

	// The tables first, in this goroutine: a stub they reach must not be met
	// for the first time by a server's goroutine, where it would end the run.
	if err := tablesBuilt(p); err != nil {
		t.Fatalf("testproto: tables: %v", err)
	}
	adaptertest.CheckLeaks(t, budgets)

	set := Settings{Adapter: settings.DefaultNFS().Adapter}
	set.Port = 0 // the listener's: one the system picks
	set.Addrs = []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	srv, err := core.NewServer(deps, set, core.Listener{Services: []string{"testproto"}, Network: "tcp", Protocol: p})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			if err := srv.Stop(t.Context()); err != nil {
				t.Errorf("Stop: %v", err)
			}
		}
	}
	defer stop()

	key := adapter.Socket{Service: "testproto", Network: "tcp"}
	addrs := srv.Addrs()
	if len(addrs[key]) == 0 {
		t.Fatalf("Addrs has no address for %+v: %v", key, addrs)
	}
	conn, err := net.Dial("tcp", addrs[key][0].String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	// A backstop: a server that binds and never replies fails the read.
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}

	c := NewClient(conn)
	if err := c.Send(Msg{Op: OpEcho, Version: 1, ID: 1, Credits: 1, Payload: []byte("ping")}); err != nil {
		t.Fatal(err)
	}
	replies, err := c.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if len(replies) != 1 || replies[0].ID != 1 || replies[0].Status != StatusOK || !bytes.Equal(replies[0].Body, []byte("ping")) {
		t.Fatalf("the reply to an Echo of \"ping\" is %+v", replies)
	}
	_ = conn.Close()
	stop()
}

// A reply the cache stored is handed back clear, in a fresh buffer each time,
// granting afresh through the connection's credits: wrapping the caller's
// reply in place, or a copy Lookup returned, changes neither the cache's copy
// nor the next Lookup's.
func TestConformanceReplayLookupCopies(t *testing.T) {
	budgets := &resources.Budgets{
		Receive:    resources.NewWaitBudget("receive", 8<<20),
		Reply:      resources.NewWaitBudget("reply", 8<<20),
		Connection: resources.NewBudget("connection", 1<<20),
		Hold:       resources.NewBudget("hold", 1<<20),
	}
	st := New(adapter.Deps{VFS: vfstest.Null{}, Budgets: budgets}).NewConn(nil)

	request := &resources.Buf{B: Append(nil, Msg{Op: OpCreate, Version: 2, ID: 3, Session: 5, Seq: 7, Credits: 1, Name: "n"})[lengthLen:]}
	h, err := st.Header.Decode(request, 0)
	if err != nil {
		t.Fatal(err)
	}
	if h, err = st.Security.Unwrap(h, request, nil); err != nil {
		t.Fatal(err)
	}
	reply := &resources.Buf{}
	if err := st.Header.Encode(h, StatusOK, reply); err != nil {
		t.Fatal(err)
	}
	reply.B = append(reply.B, "the reply's body"...)
	clear := bytes.Clone(reply.B)

	st.Replay.Store(h, reply)
	if err := st.Security.Wrap(h, reply); err != nil {
		t.Fatal(err)
	}

	// Fill the connection's credits, so a copy that grants afresh grants
	// none, where the stored reply granted one.
	scratch := &resources.Buf{}
	for range creditTarget {
		scratch.B = scratch.B[:0]
		if err := st.Header.Encode(h, StatusOK, scratch); err != nil {
			t.Fatal(err)
		}
	}
	clear[offCredits], clear[offCredits+1] = 0, 0

	first, err := st.Replay.Lookup(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}
	if first == nil || !bytes.Equal(first.B, clear) {
		t.Fatalf("the first Lookup did not return the clear reply that was stored")
	}
	if err := st.Security.Wrap(h, first); err != nil {
		t.Fatal(err)
	}
	second, err := st.Replay.Lookup(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}
	if second == nil || !bytes.Equal(second.B, clear) {
		t.Fatalf("the second Lookup returned a reply the first one's wrap had changed")
	}
	if &first.B[0] == &second.B[0] {
		t.Error("two Lookups returned the same buffer")
	}
	first.Release()
	second.Release()
}
