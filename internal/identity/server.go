// Package identity is the identity server: the one component that signs
// callers in, maps principals to spellings and back, resolves an Identity to
// the Caller it acts as on a share, and checks the management API's
// accounts. Only the node builds it, and hands each consumer only the
// interface of pkg/identity it calls.
package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/internal/identity/netlogon"
	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/secrets"
)

// Deps is what the server is built over. The node starts and stops each of
// them, and the server closes none.
type Deps struct {
	Store   Store
	Clock   Clock
	Metrics *metrics.Registry // nil: registers nothing
	Tracer  trace.Tracer      // the identity.resolve span; nil: the no-op tracer
	Logger  *slog.Logger
	// Secrets resolves the secrets identity holds by reference; New does not
	// require it.
	Secrets SecretProvider
	// Directory dials the directory State.Directory names; nil, as with no
	// record, answers every directory question ErrUnavailable.
	Directory directory.Dialer
	// DomainController opens the domain's controllers; New does not require
	// it, and while it is nil pass-through answers ErrUnavailable.
	DomainController netlogon.Dialer
}

// SecretProvider is the secret provider's one call the server makes.
type SecretProvider interface {
	Resolve(ctx context.Context, role secrets.Role, kind entity.SecretKind, owner entity.SecretOwner, ref entity.SecretRef) ([]byte, error)
}

var _ SecretProvider = (*secrets.Provider)(nil)

// Store is the store's calls the server makes: the principals and their
// records, a group's record, and the one write that mints principals. Declaring it here keeps a
// change to the store's write calls inside the store.
type Store interface {
	Principals() kvstore.Principals
	Records() kvstore.Records
	Minter() kvstore.Minter
}

// Clock is the server's source of time.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan struct{} // fires after d on the monotonic clock
}

// State is everything Apply delivers, swapped in as one snapshot: a call
// reads one State for its whole life, never part of one Apply and part of
// another.
type State struct {
	// Generation is the principals generation as of this Apply. Account
	// never checks a record under it: it reads the generation per call.
	Generation uint64
	// Settings is the identity section as of this Apply.
	Settings Settings
	// Keytab is the keytab's versions in force, opened, current first. A
	// new version takes effect at the Apply that delivers it, and one no
	// longer delivered is gone from the ring at that Apply.
	Keytab []secrets.Version
	// keys is the ring Apply builds from Keytab; whatever the caller
	// passes is replaced.
	keys keyRing
	// Machine is the machine password's versions in force, opened, current
	// first. While Join.Joined, Apply derives each one's keys for the names
	// in Join.Names, but for a version marked HandedOver.
	Machine []secrets.Version
	// Join is the machine account's state as of this Apply; zero while the
	// installation is not joined.
	Join entity.MachineJoin
	// machine is the ring Apply derives from Machine and Join; whatever the
	// caller passes is replaced.
	machine machineRing
	// dropped is the imported keys Apply removed for the computer account's
	// names; whatever the caller passes is replaced.
	dropped map[ringKey]bool
	// Directory is the identity.directory section as of this Apply; nil
	// when the installation has no directory.
	Directory *directory.Record
	// dir is Apply's client of Directory, kept across Applies that leave
	// Directory unchanged; nil with no record or no Deps.Directory.
	dir *directory.Client
	// Serial is set by Apply, whatever the caller passes: raised only when
	// the State differs from the current one. A Caller's Built is the Serial
	// of the State it was resolved under.
	Serial uint64
}

// serial is st's Serial, 0 for no State.
func (st *State) serial() uint64 {
	if st == nil {
		return 0
	}
	return st.Serial
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
	deps   Deps
	state  atomic.Pointer[State] // nil until the first Apply, and again after Stop
	mu     sync.Mutex            // Apply's: held across its load, compare, serial and store
	serial uint64                // the last serial Apply handed out, under mu; Stop keeps it
	loaded func()                // a test's pause just after Resolve loads the State; nil in production
	// slotTaken is a test's pause just after a pass-through call takes its
	// slot, before it reaches the channel; nil in production.
	slotTaken func()
	tracer    trace.Tracer
	logger    *slog.Logger
	refusals  *prometheus.CounterVec // by reason
	mints     *prometheus.CounterVec // by kind
	unnamed   *prometheus.CounterVec // by kind
	unmapped  *prometheus.CounterVec // by scheme
	hashing   chan struct{}          // the password verifications' slots, GOMAXPROCS of them
	holds     holds                  // the joins' administrator credentials

	slots     chan struct{}      // the pass-through's, channelSlots of them
	chMu      sync.Mutex         // held across the channel's set-up
	node      *nodeChannel       // the node's secure channel, under chMu; nil until a call sets one up
	unhealthy atomic.Bool        // the last set-up failed at every controller
	lifeMu    sync.Mutex         // life's and end's
	life      context.Context    // ended by Stop, ending every call in flight
	end       context.CancelFunc // ends life
}

// refusalsSpec counts the calls resolution refuses, by the reason refused.
var refusalsSpec = metrics.Spec{
	Name:   "identity_resolution_refusals_total",
	Help:   "Calls refused at resolution, by reason.",
	Labels: []string{"reason"},
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
	if d.Clock == nil {
		return nil, errors.New("identity: Deps.Clock is nil")
	}
	s := &Server{deps: d, tracer: d.Tracer, logger: d.Logger, hashing: make(chan struct{}, runtime.GOMAXPROCS(0)),
		slots: make(chan struct{}, channelSlots)}
	s.life, s.end = context.WithCancel(context.Background())
	if s.logger == nil {
		s.logger = slog.New(slog.DiscardHandler)
	}
	if s.tracer == nil {
		s.tracer = noop.NewTracerProvider().Tracer("")
	}
	s.refusals = counter(d.Metrics, refusalsSpec)
	s.mints = counter(d.Metrics, mintsSpec)
	s.unnamed = counter(d.Metrics, unnamedSpec)
	s.unmapped = counter(d.Metrics, unmappedSpec)
	return s, nil
}

// counter is spec registered on reg, or an unregistered one for a nil reg.
func counter(reg *metrics.Registry, spec metrics.Spec) *prometheus.CounterVec {
	if reg != nil {
		return reg.Counter(spec)
	}
	return prometheus.NewCounterVec(prometheus.CounterOpts{Name: spec.Name, Help: spec.Help}, spec.Labels)
}

// Apply makes st the State every later call reads, in one swap. A State
// equal to the current one but for its Serial is not swapped in, so the
// serial stays; any other gets a serial greater than every one before it.
//
// A State whose Settings or Directory fail Validate, or whose NetBIOSName is
// the directory's NetBIOS domain, is refused and the current one kept.
//
// A State whose Directory differs from the current one's gets a new
// directory client, which dials nothing until its first question; the old
// one is closed once the questions running on it return.
//
// decision: the serial is raised per differing State, not per Apply, so a
// once-a-second reconcile that changed nothing empties no cache, and under
// one lock across the load, the compare, the serial and the store, so two
// concurrent Applies never store two differing States under one serial. A
// serial survives Stop, so a restarted server never repeats one a cache may
// still hold. Overturned if Apply ever needs to run without that lock.
func (s *Server) Apply(st State) error {
	// The State keeps its own copy of the record, so the caller editing its
	// record later changes nothing applied. A Record holds only values, no
	// slice or map, so one level copies it whole.
	if st.Directory != nil {
		d := *st.Directory
		st.Directory = &d
	}
	if err := st.validate(); err != nil {
		return err
	}
	st.keys = buildRing(st.Keytab, s.logger)
	st.machine = buildMachineRing(&st, s.logger)
	st.dropped = st.keys.dropMachineNames(&st.machine)
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.state.Load()
	for k := range st.dropped {
		if cur == nil || !cur.dropped[k] {
			s.logger.Error("identity: imported key for a computer account's name skipped", "name", k.name, "realm", k.realm, "kvno", k.kvno)
		}
	}
	if cur != nil {
		st.Serial = cur.Serial
		st.dir = cur.dir
		// DeepEqual, since a State's keytab versions and Directory are
		// compared by value.
		if reflect.DeepEqual(st, *cur) {
			return nil
		}
	}
	if cur == nil || !reflect.DeepEqual(st.Directory, cur.Directory) {
		st.dir = s.newDirectoryClient(st.Directory)
	}
	s.serial++
	st.Serial = s.serial
	s.state.Store(&st)
	if cur != nil && cur.dir != st.dir && cur.dir != nil {
		cur.dir.Close()
	}
	if !st.Join.Joined || st.Directory == nil {
		s.closeChannel() // the channel is held only while joined, with a record
	}
	return nil
}

// newDirectoryClient is a client of rec, its bind password resolved through
// Deps.Secrets at each dial; nil with no record or no Deps.Directory.
func (s *Server) newDirectoryClient(rec *directory.Record) *directory.Client {
	if rec == nil || s.deps.Directory == nil {
		return nil
	}
	ref := rec.BindCredential
	return directory.NewClient(*rec, s.deps.Directory, func(ctx context.Context) ([]byte, error) {
		if s.deps.Secrets == nil {
			return nil, errors.New("no secret provider")
		}
		return s.deps.Secrets.Resolve(ctx, secrets.Protocol, entity.SecretDirectoryCredential,
			entity.SecretOwner{Scope: entity.SettingScope{Kind: entity.ScopeInstallation}, Key: "identity.directory"}, ref)
	}, s.deps.Clock, s.logger)
}

// validate refuses settings or a directory record that fail their Validate,
// and an installation NetBIOS name that is the directory's NetBIOS domain,
// which would make a local NTLMv2 sign-in's domain name both.
func (st *State) validate() error {
	if err := st.Settings.Validate(); err != nil {
		return err
	}
	d := st.Directory
	if d == nil {
		return nil
	}
	if err := d.Validate(); err != nil {
		return err
	}
	if equalFoldASCII(st.Settings.NetBIOSName, d.NetBIOSDomain) {
		return fmt.Errorf("identity: netbios_name %q is identity.directory's netbios_domain %q", st.Settings.NetBIOSName, d.NetBIOSDomain)
	}
	return nil
}

// equalFoldASCII compares a and b with ASCII letters folded and every other
// byte as itself, so a Unicode letter that folds to an ASCII one, the Kelvin
// sign to k among them, never matches.
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		x, y := a[i], b[i]
		if 'a' <= x && x <= 'z' {
			x -= 'a' - 'A'
		}
		if 'a' <= y && y <= 'z' {
			y -= 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// Current is the serial of the State a Resolve starting now reads; 0 before
// the first Apply and after Stop.
func (s *Server) Current() uint64 { return s.state.Load().serial() }

// Start starts the server under the State last applied, and refuses
// errNotApplied when none has been.
func (s *Server) Start(ctx context.Context) error {
	if s.state.Load() == nil {
		return errNotApplied
	}
	return nil
}

// Stop drops every in-memory table, the applied State and the held join
// credentials among them, so a restart needs an Apply before its Start, ends
// every call to a domain controller in flight, closes the node's secure
// channel, and closes its directory client once the questions running on it
// return. It closes nothing it was handed.
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old := s.state.Swap(nil); old != nil && old.dir != nil {
		old.dir.Close()
	}
	s.dropHolds()
	s.closeChannel()
	return nil
}
