package identity

import (
	"context"
	"net/netip"
	"time"
	"unsafe"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/internal/identity/netlogon"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// ErrNotApplied is Start's refusal before the first Apply.
var ErrNotApplied = errNotApplied

// OnStateLoaded has every Resolve pass call f just after it loads the
// State, before its first store read, so a test can Apply in between.
func (s *Server) OnStateLoaded(f func()) { s.loaded = f }

// OnSlotTaken has every pass-through call run f just after it takes its
// slot, before it reaches the node's channel.
func (s *Server) OnSlotTaken(f func()) { s.slotTaken = f }

// OnAwaitAnswer has every pass-through call run f just before it waits for
// its answer, its bound or its caller's hang-up.
func (s *Server) OnAwaitAnswer(f func()) { s.awaiting = f }

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

// OnVerify has every VerifyPassword call deliver f the hash it verified the
// password against.
func (s *Server) OnVerify(f func(phc []byte)) { s.verified = f }

// ParsePHC reads an argon2id PHC string into its memory, passes, lanes, salt
// and key.
func ParsePHC(b []byte) (memory, time uint32, threads uint8, salt, key []byte, err error) {
	h, err := parsePHC(b)
	return h.memory, h.time, h.threads, h.salt, h.key, err
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

// LookupSIDs asks the cache who holds each of sids, under grace.
func (c *dirCache) LookupSIDs(ctx context.Context, sids [][]byte, grace time.Duration) ([]directory.Answer, error) {
	return c.lookupSIDs(ctx, sids, grace)
}

// Asking is the number of SIDs a query in flight is registered to.
func (c *dirCache) Asking() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.asking)
}

// Waiting is the number of queries waiting for one of the 64 slots.
func (c *dirCache) Waiting() int { return int(c.waiting.Load()) }

// Conditions is the cache's directory_unreachable, as Server.Conditions
// reports it for the applied State's cache.
func (c *dirCache) Conditions(dst []health.Condition) []health.Condition {
	return c.out.conditions(dst)
}

// DirectoryWaiting is the number of queries waiting for one of the applied
// State's directory cache's 64 slots.
func (s *Server) DirectoryWaiting() int { return s.state.Load().dir.Waiting() }

// ByKerberosName maps user@realm through the directory, as a Kerberos
// sign-in's third step does, minting with mint.
func (s *Server) ByKerberosName(ctx context.Context, user, realm string, mint bool) (entity.Principal, error) {
	return s.byKerberosName(ctx, s.state.Load(), user, realm, mint)
}

// OnNTHash has every NTLM sign-in deliver f the NT hash it computed its
// proof with.
func (s *Server) OnNTHash(f func(nt [16]byte)) { s.ntHashed = f }

// DummyNTHash is the NT hash a sign-in computes its proof with for a user
// that cannot sign in.
func (s *Server) DummyNTHash() [16]byte { return s.dummyNT }

// SignInOutcome signs in as SignIn does and names the attempt's outcome.
func (s *Server) SignInOutcome(ctx context.Context, step identity.SignInStep) (identity.SignInResult, string, error) {
	res, oc, err := s.signIn(ctx, step)
	return res, string(oc), err
}

// The published NTLMv2 example of MS-NLMP §4.2.4.
var (
	NLMPResponse        = nlmpResponse
	NLMPServerChallenge = nlmpServerChallenge
	NLMPFlags           = nlmpFlags
	NLMPRandomKey       = nlmpRandomKey
	NLMPEncryptedSKey   = nlmpEncryptedSKey
)

// OnScheduled has the rotation's schedule call f after each check, once its
// next tick is armed.
func (s *Server) OnScheduled(f func()) { s.scheduled = f }

// ThrottleKey is the logon throttle's key for user, as presented, signing in
// from client.
func ThrottleKey(user string, client netip.Addr) [32]byte { return throttleKeyOf(user, client) }

// ThrottleFootprint is the bytes the logon throttle's table holds: each
// pair's key and its record.
func (s *Server) ThrottleFootprint() int {
	t := s.throttle
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for k := range t.pairs {
		n += len(k) + int(unsafe.Sizeof(throttlePair{}))
	}
	return n
}

// BoundThrottle gives s an empty logon throttle holding at most bound pairs,
// its evictions counted where they were.
func (s *Server) BoundThrottle(bound int) { s.throttle = newThrottle(bound, s.throttle.evictions) }

// BoundReplay gives s an empty replay cache of bound entries, share of them
// per ticket client.
func (s *Server) BoundReplay(bound, share int) { s.replay = newReplayCache(bound, share) }

// ReplayLen is the number of entries s's replay cache holds.
func (s *Server) ReplayLen() int { return s.replay.Len() }

// OnMachineMint has every machine mint's plan run f just before it returns
// the batch it would create.
func (s *Server) OnMachineMint(f func()) { s.beforeMachineMint = f }

// OnMachineLink has every machine link run f just before its write.
func (s *Server) OnMachineLink(f func()) { s.beforeMachineLink = f }

// OnRename has every refresh of a directory principal run f between its
// plan and its Rename.
func (s *Server) OnRename(f func()) { s.beforeRename = f }

// OnCreate has every mint run f with its batch just before its Create.
func (s *Server) OnCreate(f func(entity.MintBatch)) { s.beforeCreate = f }
