package identity

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/internal/identity/netlogon"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// The pass-through's bounds.
//
// ponytail: the slots are a constant 10 per node, Windows' default
// MaxConcurrentApi, which a node cannot read from the controller, and every
// wait is a constant. The node holds one channel, and a netlogon channel runs
// one call at a time, so the real ceiling is one call on the wire per node:
// the other slots queue on the channel's lock, and behind a controller that
// answers in 1 s the sixth queued call answers ErrUnavailable. Keep up to
// channelSlots channels, or associations, when a deployment shows sign-ins
// answered ErrUnavailable with the controllers idle.
const (
	channelSlots   = 10
	channelWait    = 5 * time.Second  // for a slot
	channelBound   = 5 * time.Second  // for the call, the channel's set-up included
	channelAttempt = channelBound / 2 // for each controller but the last during a set-up
)

// errBound ends a call's set-up when the call runs past channelBound, and
// errSilent a controller's attempt when it runs past channelAttempt.
var (
	errBound  = errors.New("no answer within the call's bound")
	errSilent = errors.New("no answer within the controller's bound")
)

// conditionMachineAccount is raised while the secure channel to every domain
// controller fails.
const conditionMachineAccount = "machine_account_unhealthy"

// channelInputs is what a node's channel is set up from; a State that differs
// in any of them sets it up again.
type channelInputs struct {
	account, realm, dnsServer, dcAddress string
	versions                             []liveVersion
}

func inputsOf(st *State) channelInputs {
	return channelInputs{account: st.Join.Account, realm: st.Directory.Realm, dnsServer: st.Directory.DNSServer,
		dcAddress: st.Directory.DCAddress, versions: machineVersions(st)}
}

func (a channelInputs) equal(b channelInputs) bool {
	return a.account == b.account && a.realm == b.realm && a.dnsServer == b.dnsServer && a.dcAddress == b.dcAddress &&
		slices.EqualFunc(a.versions, b.versions, func(x, y liveVersion) bool {
			return x.Number == y.Number && bytes.Equal(x.Value, y.Value) && x.notAfter.Equal(y.notAfter)
		})
}

// nodeChannel is a secure channel this node holds: the node's own while it is
// s.node, else retired and closed when its last call returns.
type nodeChannel struct {
	in       channelInputs
	dc       netlogon.DC
	ch       netlogon.Channel
	addr     string    // the controller it is set up to
	version  uint64    // the machine password version it signed in with
	notAfter time.Time // past which that version may no longer sign in; zero for none
	calls    int       // the calls running on it, under chMu
}

func (c *nodeChannel) close() {
	_ = c.ch.Close()
	_ = c.dc.Close()
}

// logon passes r to a domain controller over the node's secure channel: at
// most channelSlots at once, a call waiting at most channelWait for a slot and
// at most channelBound for its answer, either answering ErrUnavailable. A
// call that runs past its bound keeps its slot until the call underneath
// returns, so a controller that stops answering is sent no more than
// channelSlots calls. A refusal is the controller's, matching
// netlogon.ErrRejected; any other failure is ErrUnavailable alone.
//
// ponytail: pass-through reaches only the one directory's domain; RFC 18
// §5.1's ponytail says when to follow trusts.
func (s *Server) logon(ctx context.Context, r netlogon.LogonRequest) (netlogon.Reply, error) {
	// life is read before the State: Stop and an Apply that leaves the domain
	// store the State before they end life, so a call that reads the life they
	// install also reads the State they stored.
	s.lifeMu.Lock()
	life := s.life
	s.lifeMu.Unlock()
	st := s.state.Load()
	if s.deps.DomainController == nil || st == nil || !st.Join.Joined || st.Directory == nil {
		return netlogon.Reply{}, fmt.Errorf("%w: the installation is not joined to a domain", vfserr.ErrUnavailable)
	}
	// A call that finds a free slot takes it without arming a timer.
	select {
	case s.slots <- struct{}{}:
	default:
		select {
		case s.slots <- struct{}{}:
		case <-s.deps.Clock.After(channelWait):
			return netlogon.Reply{}, fmt.Errorf("%w: no secure channel slot within %v", vfserr.ErrUnavailable, channelWait)
		case <-ctx.Done():
			return netlogon.Reply{}, ctx.Err()
		}
	}
	cctx, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(life, func() { cancel(context.Canceled) })
	// The bound ends the call's set-up, which holds the channel's lock, but
	// not a call on the wire, which keeps its slot until it returns.
	sctx, endSetUp := context.WithCancelCause(cctx)
	type answer struct {
		reply netlogon.Reply
		err   error
	}
	done := make(chan answer, 1)
	bound := s.deps.Clock.After(channelBound) // armed before the call starts
	go func() {
		defer func() { <-s.slots }()
		defer stop()
		defer cancel(nil)
		defer endSetUp(nil)
		if s.slotTaken != nil {
			s.slotTaken()
		}
		c, err := s.channel(sctx, life, st)
		if err != nil {
			done <- answer{err: fmt.Errorf("%w: %v", vfserr.ErrUnavailable, err)}
			return
		}
		reply, err := c.ch.Logon(cctx, r)
		failed := err != nil && !errors.Is(err, netlogon.ErrRejected)
		// decision: a call its caller hung up on leaves the channel to the
		// other calls, because the failure is the caller's, not the channel's,
		// and the netlogon channel binds its association again after a failed
		// call. Overturned if a call ended by its caller can leave the
		// channel unable to answer the next.
		s.release(c, failed && ctx.Err() == nil)
		if failed {
			err = fmt.Errorf("%w: %w", vfserr.ErrUnavailable, err)
		}
		done <- answer{reply, err}
	}()
	select {
	case a := <-done:
		return a.reply, a.err
	case <-bound:
		endSetUp(errBound)
		return netlogon.Reply{}, fmt.Errorf("%w: no answer from the domain controller within %v", vfserr.ErrUnavailable, channelBound)
	case <-ctx.Done():
		return netlogon.Reply{}, ctx.Err()
	}
}

// channel is the node's channel for st, with a call counted on it, set up
// again when st's machine password versions, account or directory record
// differ from those it was set up from, or its version has passed its
// retention; none once life has ended. A set-up that fails at every
// controller, or that the call's bound ends, raises machine_account_unhealthy,
// and one that succeeds clears it.
func (s *Server) channel(ctx, life context.Context, st *State) (*nodeChannel, error) {
	in := inputsOf(st)
	now := s.deps.Clock.Now()
	s.chMu.Lock()
	defer s.chMu.Unlock()
	// closeChannel ends life before it takes chMu, so a call that gets here
	// after it sets up nothing, though its context may not have ended yet.
	if err := life.Err(); err != nil {
		return nil, err
	}
	if c := s.node; c != nil {
		if c.in.equal(in) && (c.notAfter.IsZero() || !now.After(c.notAfter)) {
			c.calls++
			return c, nil
		}
		s.retire()
	}
	c, err := s.setUp(ctx, st, now)
	if err != nil {
		// A set-up its caller or Stop ended says nothing of the controllers.
		if cause := context.Cause(ctx); cause == nil || errors.Is(cause, errBound) {
			s.unhealthy.Store(true)
		}
		return nil, err
	}
	c.in, c.calls = in, 1
	s.node = c
	s.unhealthy.Store(false)
	s.logger.Info("identity: secure channel set up", "dc", c.addr, "version", c.version)
	return c, nil
}

// setUp signs in to a domain controller: the record's controller address
// alone when it names one, else each controller the domain's DNS service
// records answer, in order. At each it tries the versions machineVersions
// allows at now, current first, the next only when the controller refuses
// one, and moves on to the next controller when it refuses them all or
// cannot be asked.
func (s *Server) setUp(ctx context.Context, st *State, now time.Time) (*nodeChannel, error) {
	rec := st.Directory
	var versions []liveVersion
	for _, v := range machineVersions(st) {
		if v.notAfter.IsZero() || !now.After(v.notAfter) {
			versions = append(versions, v)
		}
	}
	if len(versions) == 0 {
		return nil, errors.New("no machine password version may sign in")
	}
	addrs := []string{rec.DCAddress}
	if rec.DCAddress == "" {
		var err error
		if addrs, err = s.deps.DomainController.Lookup(ctx, rec.Realm, rec.DNSServer); err != nil {
			return nil, fmt.Errorf("find the domain controllers: %w", err)
		}
	}
	account := strings.TrimSuffix(st.Join.Account, "$")
	var errs []error
	for i, addr := range addrs {
		c, err := s.attempt(ctx, rec, addr, account, versions, i < len(addrs)-1)
		if c != nil {
			return c, nil
		}
		errs = append(errs, err...)
	}
	if len(errs) == 0 {
		return nil, errors.New("no domain controller found")
	}
	return nil, errors.Join(errs...)
}

// attempt signs in to the controller at addr, bounded by channelAttempt when
// bounded, so a silent controller leaves time to ask the next; the last one
// is bounded by the call's own bound.
func (s *Server) attempt(ctx context.Context, rec *directory.Record, addr, account string, versions []liveVersion, bounded bool) (*nodeChannel, []error) {
	if bounded {
		actx, cancel := context.WithCancelCause(ctx)
		defer cancel(nil)
		bound := s.deps.Clock.After(channelAttempt) // armed before the dial
		go func() {
			select {
			case <-bound:
				cancel(errSilent)
			case <-actx.Done():
			}
		}()
		ctx = actx
	}
	dc, err := s.deps.DomainController.Dial(ctx, *rec, addr)
	if err != nil {
		return nil, []error{fmt.Errorf("%s: %w", addr, causeOf(ctx, err))}
	}
	var errs []error
	for _, v := range versions {
		ch, err := dc.Channel(ctx, account, v.Value)
		if err == nil {
			return &nodeChannel{dc: dc, ch: ch, addr: addr, version: v.Number, notAfter: v.notAfter}, nil
		}
		errs = append(errs, fmt.Errorf("%s, version %d: %w", addr, v.Number, causeOf(ctx, err)))
		if !errors.Is(err, netlogon.ErrRejected) {
			break
		}
	}
	_ = dc.Close()
	return nil, errs
}

// causeOf is err, or the reason ctx ended when it ended it.
func causeOf(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); cause != nil && errors.Is(err, ctx.Err()) {
		return cause
	}
	return err
}

// release ends a call on c, retiring c first when drop is set and c is still
// the node's channel, so the next call sets one up again.
func (s *Server) release(c *nodeChannel, drop bool) {
	s.chMu.Lock()
	defer s.chMu.Unlock()
	if drop && s.node == c {
		s.retire()
	}
	c.calls--
	if c.calls == 0 && s.node != c {
		c.close()
	}
}

// retire takes the node's channel out of use, closing it now when no call
// runs on it and else when its last call returns. chMu is held.
func (s *Server) retire() {
	c := s.node
	s.node = nil
	if c.calls == 0 {
		c.close()
	}
}

// closeChannel ends every call in flight and retires the node's channel.
func (s *Server) closeChannel() {
	s.lifeMu.Lock()
	s.end()
	s.life, s.end = context.WithCancel(context.Background())
	s.lifeMu.Unlock()
	s.chMu.Lock()
	defer s.chMu.Unlock()
	if s.node != nil {
		s.retire()
	}
	s.unhealthy.Store(false)
}

// Conditions appends machine_account_unhealthy while the last set-up of the
// secure channel failed at every domain controller. Stop, and an Apply that
// leaves the domain or drops its record, clear it. The condition's subject
// names no node: the node's own conditions carry its ID.
func (s *Server) Conditions(dst []health.Condition) []health.Condition {
	if !s.unhealthy.Load() {
		return dst
	}
	return append(dst, health.Condition{
		Name:    conditionMachineAccount,
		Subject: health.Subject{Scope: health.ScopeNode},
		Detail:  "the secure channel to every domain controller failed",
	})
}
