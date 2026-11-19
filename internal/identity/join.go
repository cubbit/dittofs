package identity

import (
	"sync"

	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// holds is the administrator credentials held for running joins, by the
// operation each was taken for. The credential lives here alone: no method
// writes it to a record, a log or another table.
type holds struct {
	mu sync.Mutex
	m  map[entity.OperationID]*identity.JoinCredential
}

// HoldJoinCredential holds cred in this node's memory under the operation id
// until release is called or the server stops. release drops the hold and
// zeroes cred.Password, the caller's slice, which the hold keeps rather than
// copies; it is safe to call more than once. A second hold under the same id
// replaces the first and zeroes its password, unless both share one slice, so
// a forgotten release cannot leave it in memory; the first's release then
// leaves the second alone.
func (s *Server) HoldJoinCredential(id entity.OperationID, cred identity.JoinCredential) (release func()) {
	c := &cred
	s.holds.mu.Lock()
	if s.holds.m == nil {
		s.holds.m = make(map[entity.OperationID]*identity.JoinCredential)
	}
	if old := s.holds.m[id]; old != nil && !samePassword(old.Password, c.Password) {
		clear(old.Password)
	}
	s.holds.m[id] = c
	s.holds.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.holds.mu.Lock()
			cur := s.holds.m[id]
			if cur == c {
				delete(s.holds.m, id)
			}
			// A live hold over the same buffer keeps it: zeroing it here
			// would clear the password that hold still answers.
			shared := cur != nil && cur != c && samePassword(cur.Password, c.Password)
			s.holds.mu.Unlock()
			if !shared {
				clear(c.Password)
			}
		})
	}
}

// samePassword reports whether a and b are the same slice, so replacing a
// hold with one over the caller's same buffer does not zero the new hold.
func samePassword(a, b []byte) bool {
	return len(a) > 0 && len(b) > 0 && &a[0] == &b[0]
}

// heldCredential is the credential held under id. The caller uses it for one
// call and keeps no copy: its Password is zeroed in place at release.
func (s *Server) heldCredential(id entity.OperationID) (identity.JoinCredential, bool) {
	s.holds.mu.Lock()
	defer s.holds.mu.Unlock()
	c, ok := s.holds.m[id]
	if !ok {
		return identity.JoinCredential{}, false
	}
	return *c, true
}

// dropHolds zeroes and drops every hold, so none outlives the server.
func (s *Server) dropHolds() {
	s.holds.mu.Lock()
	defer s.holds.mu.Unlock()
	for id, c := range s.holds.m {
		clear(c.Password)
		delete(s.holds.m, id)
	}
}
