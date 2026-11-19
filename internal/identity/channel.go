package identity

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/internal/identity/netlogon"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// The pass-through's bounds.
//
// ponytail: the slots are a constant 10 per node, Windows' default
// MaxConcurrentApi, which a node cannot read from the controller, and both
// waits a constant 5 s. A domain whose controllers raise MaxConcurrentApi
// still gets no more than 10 calls at once from a node. Make them settings
// when a deployment shows sign-ins answered ErrUnavailable with the
// controllers idle.
const (
	channelSlots = 10
	channelWait  = 5 * time.Second // for a slot
	channelBound = 5 * time.Second // for the call, the channel's set-up included
)

// conditionMachineAccount is raised while the secure channel to every domain
// controller fails.
const conditionMachineAccount = "machine_account_unhealthy"

// channelInputs is what a node's channel is set up from; a State that differs
// in any of them sets it up again.
type channelInputs struct {
	machine []secrets.Version
	join    entity.MachineJoin
	dir     directory.Record
}

// nodeChannel is the secure channel this node holds.
type nodeChannel struct {
	in       channelInputs
	dc       netlogon.DC
	ch       netlogon.Channel
	addr     string    // the controller it is set up to
	version  uint64    // the machine password version it signed in with
	notAfter time.Time // past which that version may no longer sign in; zero for none
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
// netlogon.ErrRejected; any other failure is ErrUnavailable.
//
// ponytail: pass-through reaches only the one directory's domain; RFC 18
// §5.1's ponytail says when to follow trusts.
func (s *Server) logon(ctx context.Context, r netlogon.LogonRequest) (netlogon.Reply, error) {
	st := s.state.Load()
	if s.deps.DomainController == nil || st == nil || !st.Join.Joined || st.Directory == nil {
		return netlogon.Reply{}, fmt.Errorf("%w: the installation is not joined to a domain", vfserr.ErrUnavailable)
	}
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
	s.lifeMu.Lock()
	life := s.life
	s.lifeMu.Unlock()
	cctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(life, cancel)
	type answer struct {
		reply netlogon.Reply
		err   error
	}
	done := make(chan answer, 1)
	go func() {
		defer func() { <-s.slots }()
		defer stop()
		defer cancel()
		c, err := s.channel(cctx, st)
		if err != nil {
			done <- answer{err: fmt.Errorf("%w: %w", vfserr.ErrUnavailable, err)}
			return
		}
		reply, err := c.ch.Logon(cctx, r)
		if err != nil && !errors.Is(err, netlogon.ErrRejected) {
			s.drop(c)
			err = fmt.Errorf("%w: %w", vfserr.ErrUnavailable, err)
		}
		done <- answer{reply, err}
	}()
	select {
	case a := <-done:
		return a.reply, a.err
	case <-s.deps.Clock.After(channelBound):
		return netlogon.Reply{}, fmt.Errorf("%w: no answer from the domain controller within %v", vfserr.ErrUnavailable, channelBound)
	case <-ctx.Done():
		return netlogon.Reply{}, ctx.Err()
	}
}

// channel is the node's channel for st, set up again when st's machine
// password, join or directory record differ from those it was set up from, or
// its version has passed its retention. A set-up that fails at every
// controller raises machine_account_unhealthy, and one that succeeds clears
// it.
func (s *Server) channel(ctx context.Context, st *State) (*nodeChannel, error) {
	in := channelInputs{machine: st.Machine, join: st.Join, dir: *st.Directory}
	now := s.deps.Clock.Now()
	s.chMu.Lock()
	defer s.chMu.Unlock()
	if c := s.node; c != nil {
		if reflect.DeepEqual(c.in, in) && (c.notAfter.IsZero() || !now.After(c.notAfter)) {
			return c, nil
		}
		c.close()
		s.node = nil
	}
	c, err := s.setUp(ctx, st, now)
	if err != nil {
		if ctx.Err() == nil {
			s.unhealthy.Store(true)
		}
		return nil, err
	}
	c.in = in
	s.node = c
	s.unhealthy.Store(false)
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
	for _, addr := range addrs {
		dc, err := s.deps.DomainController.Dial(ctx, *rec, addr)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", addr, err))
			continue
		}
		for _, v := range versions {
			ch, err := dc.Channel(ctx, account, v.Value)
			if err == nil {
				return &nodeChannel{dc: dc, ch: ch, addr: addr, version: v.Number, notAfter: v.notAfter}, nil
			}
			errs = append(errs, fmt.Errorf("%s, version %d: %w", addr, v.Number, err))
			if !errors.Is(err, netlogon.ErrRejected) {
				break
			}
		}
		_ = dc.Close()
	}
	if len(errs) == 0 {
		return nil, errors.New("no domain controller found")
	}
	return nil, errors.Join(errs...)
}

// drop closes c if it is still the node's channel, so the next call sets one
// up again.
func (s *Server) drop(c *nodeChannel) {
	s.chMu.Lock()
	defer s.chMu.Unlock()
	if s.node == c {
		c.close()
		s.node = nil
	}
}

// closeChannel ends every call in flight and closes the node's channel.
func (s *Server) closeChannel() {
	s.lifeMu.Lock()
	s.end()
	s.life, s.end = context.WithCancel(context.Background())
	s.lifeMu.Unlock()
	s.chMu.Lock()
	defer s.chMu.Unlock()
	if s.node != nil {
		s.node.close()
		s.node = nil
	}
	s.unhealthy.Store(false)
}

// Conditions appends machine_account_unhealthy while the installation is
// joined and the last set-up of the secure channel failed at every domain
// controller.
func (s *Server) Conditions(dst []health.Condition) []health.Condition {
	if st := s.state.Load(); st == nil || !st.Join.Joined || !s.unhealthy.Load() {
		return dst
	}
	return append(dst, health.Condition{
		Name:    conditionMachineAccount,
		Subject: health.Subject{Scope: health.ScopeNode},
		Detail:  "the secure channel to every domain controller failed",
	})
}
