package identitytest

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/internal/identity/netlogon"
	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/sim"
)

// FaultyDomainController wraps a netlogon.Dialer, every DC it opens and every
// Channel those set up. Each call is handed to the fault as an op keyed by
// what it names: "lookup" by the realm, "dial" by the address, "channel" by
// the account, "account", "create" and "reset" by the computer's name,
// "logon" by the user and "setpassword" by the channel's account. The fault's
// delay is waited out on its own clock before the call is sent. Its Err is
// returned once the call has been sent to what it wraps and answered, so the
// controller has really acted (a wrong password counted, a password set) and
// only its answer is lost; an Err that is not a netlogon.ErrRejected is "could
// not be asked". A dial's Err refuses it before anything is sent. ErrHold
// holds the call, unsent, until Release, which sends it, or until its context
// ends, which fails it with the context's error.
type FaultyDomainController struct {
	next  netlogon.Dialer
	fault sim.Fault

	mu      sync.Mutex
	release chan struct{}
}

// NewFaultyDomainController wraps next with fault.
func NewFaultyDomainController(next netlogon.Dialer, fault sim.Fault) *FaultyDomainController {
	return &FaultyDomainController{next: next, fault: fault, release: make(chan struct{})}
}

// Release sends every call ErrHold is holding now.
func (f *FaultyDomainController) Release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	close(f.release)
	f.release = make(chan struct{})
}

// at consults the fault for op on key: whether to send the call, and the
// error to answer with once it is sent, or instead of sending it. A hold
// waits for Release, then sends the call and answers it, or for the context's
// end, which answers its error unsent. A tear or a loss is a mistake in the
// test, and panics.
func (f *FaultyDomainController) at(ctx context.Context, op, key string) (send bool, err error) {
	f.mu.Lock()
	release := f.release
	f.mu.Unlock()
	_, tearAfter, err := f.fault.At(op, []byte(key))
	if tearAfter != 0 {
		panic("identitytest: a tear or loss of the domain controller op " + op + ": none takes one")
	}
	if errors.Is(err, ErrHold) {
		select {
		case <-release:
			return true, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return true, err
}

// call runs send under op on key as at decides.
func call[T any](ctx context.Context, f *FaultyDomainController, op, key string, send func() (T, error)) (T, error) {
	var zero T
	ok, ferr := f.at(ctx, op, key)
	if !ok {
		return zero, ferr
	}
	v, err := send()
	if ferr != nil {
		// A channel the fault's answer loses is closed, so it holds no connection.
		if c, ok := any(v).(io.Closer); ok && err == nil {
			_ = c.Close()
		}
		return zero, ferr
	}
	return v, err
}

// Lookup is a netlogon.Dialer's.
func (f *FaultyDomainController) Lookup(ctx context.Context, realm, dnsServer string) ([]string, error) {
	return call(ctx, f, "lookup", realm, func() ([]string, error) { return f.next.Lookup(ctx, realm, dnsServer) })
}

// Dial is a netlogon.Dialer's.
func (f *FaultyDomainController) Dial(ctx context.Context, rec directory.Record, addr string) (netlogon.DC, error) {
	if ok, err := f.at(ctx, "dial", addr); !ok || err != nil {
		return nil, err
	}
	dc, err := f.next.Dial(ctx, rec, addr)
	if err != nil {
		return nil, err
	}
	return &faultyDC{f: f, next: dc}, nil
}

var _ netlogon.Dialer = (*FaultyDomainController)(nil)

type faultyDC struct {
	f    *FaultyDomainController
	next netlogon.DC
}

func (d *faultyDC) Channel(ctx context.Context, account string, password []byte) (netlogon.Channel, error) {
	ch, err := call(ctx, d.f, "channel", account, func() (netlogon.Channel, error) {
		return d.next.Channel(ctx, account, password)
	})
	if err != nil {
		return nil, err
	}
	return &faultyChannel{f: d.f, next: ch, account: account}, nil
}

func (d *faultyDC) Account(ctx context.Context, bind identity.JoinCredential, name string) (netlogon.Account, error) {
	return call(ctx, d.f, "account", name, func() (netlogon.Account, error) { return d.next.Account(ctx, bind, name) })
}

func (d *faultyDC) Create(ctx context.Context, admin identity.JoinCredential, c netlogon.Computer, password []byte) error {
	_, err := call(ctx, d.f, "create", c.Name, func() (struct{}, error) {
		return struct{}{}, d.next.Create(ctx, admin, c, password)
	})
	return err
}

func (d *faultyDC) Reset(ctx context.Context, admin identity.JoinCredential, name string, password []byte) error {
	_, err := call(ctx, d.f, "reset", name, func() (struct{}, error) {
		return struct{}{}, d.next.Reset(ctx, admin, name, password)
	})
	return err
}

func (d *faultyDC) Close() error { return d.next.Close() }

type faultyChannel struct {
	f       *FaultyDomainController
	next    netlogon.Channel
	account string
}

func (c *faultyChannel) Logon(ctx context.Context, r netlogon.LogonRequest) (netlogon.Reply, error) {
	return call(ctx, c.f, "logon", r.User, func() (netlogon.Reply, error) { return c.next.Logon(ctx, r) })
}

func (c *faultyChannel) SetPassword(ctx context.Context, password []byte) error {
	_, err := call(ctx, c.f, "setpassword", c.account, func() (struct{}, error) {
		return struct{}{}, c.next.SetPassword(ctx, password)
	})
	return err
}

func (c *faultyChannel) Close() error { return c.next.Close() }
