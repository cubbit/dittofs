package core_test

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/internal/adapter/adaptertest/testproto"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/resources"
)

// errSealFails is the refusal sealFails's Wrap answers once armed.
var errSealFails = errors.New("the seal fails")

// sealFails is the stages' Security whose next Wrap, once armed, refuses
// with errSealFails: a fault injected into testproto's own Security, which
// runs every other call.
type sealFails struct {
	core.Security
	armed atomic.Bool
}

func (s *sealFails) Wrap(h core.Header, msg *resources.Buf) error {
	if s.armed.CompareAndSwap(true, false) {
		return errSealFails
	}
	return s.Security.Wrap(h, msg)
}

// A message Send's Wrap refuses is released, never written, and the refusal
// returned: the reply budget is back at zero once Send returns, and the
// connection stays open, its caller deciding, so an Echo after it is answered
// and is the first record the client reads (RFC 20 section 4.7).
func TestAMessageWrapRefusesInSendIsReleased(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := budgets()
		adaptertest.CheckLeaks(t, b)
		seal := &sealFails{}
		l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: withStages{testproto.New(adapter.Deps{}), func(_ *core.Conn, st *core.Stages) {
			seal.Security = st.Security
			st.Security = seal
		}}}
		srv, err := core.NewServer(adapter.Deps{Budgets: b}, settings.DefaultNFS(), l)
		if err != nil {
			t.Fatal(err)
		}
		core.Serving(srv)
		conn, peer := servedConn(t, srv, &l)
		msg, err := resources.NewBufPool(b.Reply).Get(context.Background(), netip.Addr{}, 8)
		if err != nil {
			t.Fatal(err)
		}
		msg.B = append(msg.B[:0], "refused"...)
		seal.armed.Store(true)
		if err := conn.Send(nil, msg, 0); !errors.Is(err, errSealFails) {
			t.Errorf("Send of a message Wrap refuses = %v, want Wrap's refusal", err)
		}
		if r := b.Reply.Reserved(); r != 0 {
			t.Errorf("Send returned with %d reply bytes still reserved: the message Wrap refused was kept", r)
		}
		c := testproto.NewClient(peer)
		if err := c.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1, Payload: []byte("after")}); err != nil {
			t.Fatalf("sending an Echo after the refused Send: %v, want the connection open", err)
		}
		if rs, err := c.Recv(); err != nil || len(rs) != 1 || rs[0].ID != 1 || string(rs[0].Body) != "after" {
			t.Errorf("the Echo after the refused Send is answered %+v (%v), want its own reply on an open connection", rs, err)
		}
	})
}

// A Notify goes ahead of the record holding the reply it names when it names
// the least or the greatest of that record's keys: a chain's record is
// queued under the least and the greatest of its members' keys, here its
// first member's and its last's, and a reply alone under its own key (RFC 20
// section 6.2). Each Notify's message is checked only to come before that
// record, the one order the writer keeps: a callback may overtake the other
// messages queued. Behind the Echo whose reply holds the writer: a chain of
// 21, 22 and 23, then 30 alone, then Notifies naming 21, 23 and 30.
func TestANotifyNamingTheLeastOrGreatestKeyGoesAheadOfItsRecord(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := budgets()
		adaptertest.CheckLeaks(t, b)
		d := adapter.Deps{Budgets: b}
		l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: testproto.New(d)}
		srv, err := core.NewServer(d, settings.DefaultNFS(), l)
		if err != nil {
			t.Fatal(err)
		}
		core.Serving(srv)
		_, peer := servedConn(t, srv, &l)
		c := testproto.NewClient(peer)
		for _, m := range []testproto.Msg{
			{Op: testproto.OpNegotiate, Version: 2, ID: 1},
			{Op: testproto.OpSession, Version: 2, ID: 2, Session: 7},
		} {
			if err := c.Send(m); err != nil {
				t.Fatal(err)
			}
			if rs, err := c.Recv(); err != nil || len(rs) != 1 || rs[0].Status != testproto.StatusOK {
				t.Fatalf("setting the connection up: %+v (%v)", rs, err)
			}
		}
		echo := func(id uint64) testproto.Msg {
			return testproto.Msg{Op: testproto.OpEcho, Version: 2, ID: id, Session: 7, Payload: []byte("e")}
		}
		notify := func(id, target uint64) testproto.Msg {
			return testproto.Msg{Op: testproto.OpNotify, Version: 2, ID: id, Session: 7, Target: target}
		}
		// The client reads nothing yet: the first Echo's reply holds the
		// writer in its write, and the chain's record and the reply alone
		// wait in the queue behind it when each Notify comes.
		for _, rec := range [][]testproto.Msg{
			{echo(10)},
			{echo(21), echo(22), echo(23)},
			{echo(30)},
			{notify(40, 21)},
			{notify(41, 23)},
			{notify(42, 30)},
		} {
			if err := c.Send(rec...); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
		}
		// Each record by the message IDs it holds, a Notify's message, which
		// carries its target's ID and no body, marked as such.
		var got []string
		for range 9 {
			rs, err := c.Recv()
			if err != nil {
				t.Fatalf("after %v: %v", got, err)
			}
			var ids []uint64
			for _, r := range rs {
				ids = append(ids, r.ID)
			}
			name := fmt.Sprint(ids)
			if len(rs) == 1 && len(rs[0].Body) == 0 && rs[0].ID < 40 {
				name = "notify " + name
			}
			got = append(got, name)
		}
		at := func(name string) int { return slices.Index(got, name) }
		for _, w := range []struct{ notify, named string }{
			{"notify [21]", "[21 22 23]"},
			{"notify [23]", "[21 22 23]"},
			{"notify [30]", "[30]"},
		} {
			if n, r := at(w.notify), at(w.named); n < 0 || r < 0 || n > r {
				t.Errorf("records %v: want %s ahead of %s, the record holding the reply it names", got, w.notify, w.named)
			}
		}
	})
}
