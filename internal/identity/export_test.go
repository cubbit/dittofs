package identity

import (
	"context"
	"net/netip"
	"time"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/internal/identity/netlogon"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// Applied is the State calls now read; nil before the first Apply and after
// Stop.
func (s *Server) Applied() *State { return s.state.Load() }

// ErrNotApplied is Start's refusal before the first Apply.
var ErrNotApplied = errNotApplied

// OnStateLoaded has every Resolve pass call f just after it loads the
// State, before its first store read, so a test can Apply in between.
func (s *Server) OnStateLoaded(f func()) { s.loaded = f }

// OnSlotTaken has every pass-through call run f just after it takes its
// slot, before it reaches the node's channel.
func (s *Server) OnSlotTaken(f func()) { s.slotTaken = f }

// Mint runs the mint path over plan.
func (s *Server) Mint(ctx context.Context, plan func(context.Context) (entity.MintBatch, int, error)) error {
	return s.mint(ctx, plan)
}

// DefaultSettings is the identity section's value where no row sets a key.
var DefaultSettings = defaultSettings

// KeyOf is the key the applied State's ring holds for a service name, realm,
// key version number and encryption type.
func (s *Server) KeyOf(name, realm string, kvno uint32, etype int32) ([]byte, bool) {
	k, ok := s.state.Load().keys.key(name, realm, kvno, etype)
	return k.KeyValue, ok
}

// Len is the number of entries the replay cache holds.
func (c *replayCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// HoldHashSlots takes every password verification's slot but free of them,
// and returns their release.
func (s *Server) HoldHashSlots(free int) (release func()) {
	n := cap(s.hashing) - free
	for range n {
		s.hashing <- struct{}{}
	}
	return func() {
		for range n {
			<-s.hashing
		}
	}
}

// HashSlot takes a hashing slot as a password verification does, and gives
// it back at once.
func (s *Server) HashSlot(ctx context.Context) error {
	release, err := s.hashSlot(ctx)
	if err == nil {
		release()
	}
	return err
}

// HeldCredential is the join credential held under id.
func (s *Server) HeldCredential(id entity.OperationID) (identity.JoinCredential, bool) {
	return s.heldCredential(id)
}

// DirectoryLookup asks the applied State's directory cache a "who holds
// this" question: ErrUnavailable with no record or no Deps.Directory.
func (s *Server) DirectoryLookup(ctx context.Context, by directory.Key, value []byte) (directory.Answer, bool, error) {
	return s.state.Load().dirLookup(ctx, by, value, false)
}

// VerifyAPReq verifies an AP-REQ under the applied State at now.
func (s *Server) VerifyAPReq(b []byte, svc identity.Service, tr identity.Transport, client netip.Addr, now time.Time) error {
	_, err := verifyAPReq(s.state.Load(), b, svc, tr, client, now)
	return err
}

// StringToKey is RFC 3962's string-to-key of password under salt with
// iterations, for etype, as the machine password's keys are derived.
func StringToKey(etype int32, password []byte, salt string, iterations uint32) ([]byte, error) {
	k, err := stringToKey(etype, password, salt, iterations)
	return k.KeyValue, err
}

// Logon passes r through the node's secure channel, as a pass-through does.
func (s *Server) Logon(ctx context.Context, r netlogon.LogonRequest) (netlogon.Reply, error) {
	return s.logon(ctx, r)
}

// ChannelAt is the controller the node's secure channel is set up to and the
// machine password version it signed in with; ok false when none is.
func (s *Server) ChannelAt() (addr string, version uint64, ok bool) {
	s.chMu.Lock()
	defer s.chMu.Unlock()
	if s.node == nil {
		return "", 0, false
	}
	return s.node.addr, s.node.version, true
}

// PassThroughs is how many pass-through calls hold a slot: those running and
// those that answered at their bound but whose call underneath has not
// returned.
func (s *Server) PassThroughs() int { return len(s.slots) }

// ChannelAttempt is how long a set-up waits on each controller but the last.
const ChannelAttempt = channelAttempt

// DirectoryCache is the server's cache of a directory client's answers.
type DirectoryCache = dirCache

// NewDirectoryCache is a cache of c's answers observed through d's clock,
// registry and tracer, as Apply builds one with each client.
func NewDirectoryCache(c *directory.Client, d Deps) *DirectoryCache { return newDirCache(c, d) }

// Lookup asks the cache, under grace; fresh asks for a fresh answer only.
func (c *dirCache) Lookup(ctx context.Context, by directory.Key, value []byte, grace time.Duration, fresh bool) (directory.Answer, bool, error) {
	return c.lookup(ctx, by, value, grace, fresh)
}

// Waiting is the number of queries waiting for one of the 64 slots.
func (c *dirCache) Waiting() int { return int(c.waiting.Load()) }

// Conditions is the cache's directory_unreachable, as Server.Conditions
// reports it for the applied State's cache.
func (c *dirCache) Conditions(dst []health.Condition) []health.Condition {
	return c.out.conditions(dst)
}

// UseDirectoryCache puts c in place of the applied State's directory cache,
// so a test reaches the directory through a client whose bind password it
// passes in.
//
// ponytail: this replaces the cache Apply builds, whose client resolves its
// bind password through Deps.Secrets.Resolve, which no secrets provider
// implements yet, so these tests run on a client production never builds.
// Delete it, and drive Apply with the real secrets provider, once Resolve
// is implemented.
func (s *Server) UseDirectoryCache(c *DirectoryCache) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := *s.state.Load()
	st.dir = c
	s.state.Store(&st)
}

// ByKerberosName maps user@realm through the directory, as a Kerberos
// sign-in's third step does, minting with mint.
func (s *Server) ByKerberosName(ctx context.Context, user, realm string, mint bool) (entity.Principal, error) {
	return s.byKerberosName(ctx, user, realm, mint)
}
