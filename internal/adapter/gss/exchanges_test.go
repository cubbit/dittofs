package gss_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/gss"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest"
)

const handle gss.Key = "a context handle"

// continuation steps the exchange under handle with no mechanism, on a fresh
// valid token, and wants ErrNoExchange with no token and no Context. Catches
// an exchange kept under the key, which would refuse the token as a second
// leg or answer a Context, and a continuation that starts a new exchange,
// which would accept it.
func continuation(t *testing.T, f *fixture, x *gss.Exchanges) {
	t.Helper()
	init, _ := f.initiator(t, identitytest.TicketOpts{Client: "admin"})
	out, c, err := x.Step(t.Context(), handle, nil, firstToken(t, init))
	if !errors.Is(err, gss.ErrNoExchange) || out != nil || c != nil {
		t.Errorf("a continuation under %q = token %x, context %+v, %v; want ErrNoExchange and nothing else", handle, out, c, err)
	}
	if !errors.Is(err, vfs.ErrLogonFailure) {
		t.Errorf("ErrNoExchange = %v, want it a logon failure", err)
	}
}

// An initial token through Step on the Kerberos mechanism answers its AP-REP
// and its Context, and a later step under the same key without a mechanism is
// ErrNoExchange: nothing is kept once done.
func TestExchangesStepOneLeg(t *testing.T) {
	f := newFixture(t)
	x := gss.NewExchanges()
	init, session := f.initiator(t, identitytest.TicketOpts{Client: "admin"})
	out, c, err := x.Step(t.Context(), handle, func() gss.Mechanism { return gss.Kerberos(f.svc, nfsStep) }, firstToken(t, init))
	if err != nil || c == nil {
		t.Fatalf("Step = context %+v, %v; want a Context", c, err)
	}
	if c.Evidence.Principal != f.svc.Store.Admin.Principal {
		t.Errorf("Context.Evidence.Principal = %v, want the administrator %v", c.Evidence.Principal, f.svc.Store.Admin.Principal)
	}
	if !bytes.Equal(c.Keys.Session, session.KeyValue) {
		t.Errorf("Context.Keys.Session = %x, want the ticket's session key %x", c.Keys.Session, session.KeyValue)
	}
	if _, idone, err := init.Step(t.Context(), out); err != nil || !idone {
		t.Errorf("the initiator's check of the token out = done %v, %v; want the AP-REP accepted", idone, err)
	}
	continuation(t, f, x)
}

// A mechanism's error comes back as it is, with no token and no Context, and
// nothing is kept under the key. Catches an error rewritten on its way out
// and a failed exchange left in progress.
func TestExchangesStepMechanismError(t *testing.T) {
	f := newFixture(t)
	faults := vfstest.Faults(f.svc)
	for _, tc := range []struct {
		name   string
		signIn error // armed for each Step, nil for none
		tok    func(t *testing.T) []byte
	}{
		{name: "a framing the mechanism refuses", tok: func(t *testing.T) []byte {
			init, _ := f.initiator(t, identitytest.TicketOpts{Client: "admin"})
			tok := firstToken(t, init)
			return tok[:len(tok)-1]
		}},
		{name: "SignIn's ErrUnavailable", signIn: vfs.ErrUnavailable, tok: func(t *testing.T) []byte {
			init, _ := f.initiator(t, identitytest.TicketOpts{Client: "admin"})
			return firstToken(t, init)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok := tc.tok(t)
			newMech := func() gss.Mechanism { return gss.Kerberos(faults, nfsStep) }
			if tc.signIn != nil {
				faults.Fail(vfs.OpSignIn, tc.signIn)
			}
			_, _, want := newMech().Step(t.Context(), tok)
			if want == nil {
				t.Fatal("the mechanism stepped on its own accepted the token, want an error")
			}
			if tc.signIn != nil {
				faults.Fail(vfs.OpSignIn, tc.signIn)
			}
			x := gss.NewExchanges()
			out, c, err := x.Step(t.Context(), handle, newMech, tok)
			if err == nil || err.Error() != want.Error() {
				t.Errorf("Step = %v, want the mechanism's error %v", err, want)
			}
			if out != nil || c != nil {
				t.Errorf("Step refused left token %x, context %+v; want none", out, c)
			}
			if left := faults.Disarm(); len(left) != 0 {
				t.Errorf("SignIn's armed answer never fired: %v", left)
			}
			continuation(t, f, x)
		})
	}
}

// A mechanism not done after its first leg is refused as a logon failure,
// with no token and no Context, and nothing is kept. gsstest's initiator is
// such a mechanism: its first leg answers the AP-REQ and waits for the
// AP-REP. Catches a second leg's token sent out, or the exchange held.
func TestExchangesStepRefusesSecondLeg(t *testing.T) {
	f := newFixture(t)
	x := gss.NewExchanges()
	init, _ := f.initiator(t, identitytest.TicketOpts{Client: "admin"})
	out, c, err := x.Step(t.Context(), handle, func() gss.Mechanism { return init }, nil)
	if !errors.Is(err, vfs.ErrLogonFailure) {
		t.Errorf("Step on a mechanism not done = %v, want ErrLogonFailure", err)
	}
	if out != nil || c != nil {
		t.Errorf("Step on a mechanism not done = token %x, context %+v; want none", out, c)
	}
	continuation(t, f, x)
}
