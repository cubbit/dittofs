package identity

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// Applied is the State calls now read; nil before the first Apply and after
// Stop.
func (s *Server) Applied() *State { return s.state.Load() }

// ErrNotApplied is Start's refusal before the first Apply.
var ErrNotApplied = errNotApplied

// OnStateLoaded has every Resolve pass call f just after it loads the
// State, before its first store read, so a test can Apply in between.
func (s *Server) OnStateLoaded(f func()) { s.loaded = f }

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

// DirectoryLookup asks the applied State's directory client a "who holds
// this" question: ErrUnavailable with no record or no Deps.Directory.
func (s *Server) DirectoryLookup(ctx context.Context, by directory.Key, value []byte) (directory.Answer, bool, error) {
	st := s.state.Load()
	if st == nil || st.dir == nil {
		return directory.Answer{}, false, fmt.Errorf("%w: identity: no directory", vfserr.ErrUnavailable)
	}
	return st.dir.Lookup(ctx, by, value)
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
