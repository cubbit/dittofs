// Package identity is the identity server: the one component that signs
// callers in, maps principals to spellings and back, resolves an Identity to
// the Caller it acts as on a share, and checks the management API's
// accounts. Only the node builds it, and hands each consumer only the
// interface of pkg/identity it calls.
package identity

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"sync/atomic"
	"time"

	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metrics"
)

// Deps is what the server is built over. The node starts and stops each of
// them, and the server closes none.
type Deps struct {
	Store   Store
	Clock   Clock
	Metrics *metrics.Registry
	Logger  *slog.Logger
}

// Store is the store's calls the server makes: the principals and their
// records, and, as identity comes to need them, the calls that mint
// principals. Declaring it here keeps a change to the store's write calls
// inside the store.
type Store interface {
	Principals() kvstore.Principals
}

// Clock is the server's source of time.
type Clock interface {
	Now() time.Time
}

// State is everything Apply delivers, swapped in as one snapshot: a call
// reads one State for its whole life, never part of one Apply and part of
// another.
type State struct {
	// Generation is the principals generation as of this Apply. Account
	// never checks a record under it: it reads the generation per call.
	Generation uint64
}

// errNotApplied is a Start before the first Apply: the server has no State
// to serve under.
var errNotApplied = errors.New("identity: started before any State was applied")

// Server is the identity server.
//
// decision: identity is one component shared by the filesystem and the
// management API, not part of the filesystem. The API signs in over no file
// protocol, so a sign-in path inside the filesystem would have the API
// depend on it; a handshake round is per connection and acts on no share;
// and identity is tested without a filesystem. Overturned if the API stops
// calling identity, leaving the filesystem its only consumer; identity then
// folds into it.
type Server struct {
	deps  Deps
	state atomic.Pointer[State] // nil until the first Apply, and again after Stop
}

var (
	_ identity.Authenticator = (*Server)(nil)
	_ identity.Identities    = (*Server)(nil)
	_ identity.Resolver      = (*Server)(nil)
	_ identity.APISignIn     = (*Server)(nil)
)

// New builds the server from d. It takes no settings and starts nothing: the
// first State arrives through Apply, before Start.
func New(d Deps) (*Server, error) {
	if d.Store == nil {
		return nil, errors.New("identity: Deps.Store is nil")
	}
	// A nil pointer in the interface passes the check above and would panic on
	// the first Principals call, so it is refused here instead.
	if v := reflect.ValueOf(d.Store); v.Kind() == reflect.Pointer && v.IsNil() {
		return nil, errors.New("identity: Deps.Store is a nil pointer")
	}
	return &Server{deps: d}, nil
}

// Apply makes st the State every later call reads, in one swap.
func (s *Server) Apply(st State) error {
	s.state.Store(&st)
	return nil
}

// Start starts the server under the State last applied, and refuses
// errNotApplied when none has been.
func (s *Server) Start(ctx context.Context) error {
	if s.state.Load() == nil {
		return errNotApplied
	}
	return nil
}

// Stop drops every in-memory table, the applied State among them, so a
// restart needs an Apply before its Start. It closes nothing it was handed.
func (s *Server) Stop(ctx context.Context) error {
	s.state.Store(nil)
	return nil
}
