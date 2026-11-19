package node_test

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/sim"
)

// The adapters in the node's start and stop: the real adapters nfs.New and
// smb.New build, bound on loopback as nodetest's bootstrap names it, what
// apply hands their slots set through the node's hooks.

var ready = answer{http.StatusOK, "ready"}

// smbAt is SMB's defaults at port; the node supplies the addresses.
func smbAt(port uint16) settings.SMB {
	s := settings.DefaultSMB()
	s.Port = port
	return s
}

// nfsAt is NFS's defaults at port; the node supplies the addresses.
func nfsAt(port uint16) settings.NFS {
	s := settings.DefaultNFS()
	s.Port = port
	return s
}

// freePort is a loopback port no listener holds.
func freePort(t *testing.T) uint16 {
	t.Helper()
	return netip.MustParseAddrPort(freeAddr(t)).Port()
}

// listens reports whether a dial to port on loopback connects.
func listens(port uint16) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// built is the adapter the node's slots built last for each protocol, as
// OnAdapter hands them.
type built struct {
	mu sync.Mutex
	by map[string]adapter.Adapter
}

func (b *built) keep(protocol string, a adapter.Adapter) adapter.Adapter {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.by == nil {
		b.by = map[string]adapter.Adapter{}
	}
	b.by[protocol] = a
	return a
}

func (b *built) last(protocol string) adapter.Adapter {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.by[protocol]
}

// heldStop is a real adapter with a fault on cue: its Stop stops the adapter,
// says so on stopped, then holds until release closes, as a drain that runs
// long. A second Stop does the same, once the first has stopped it.
type heldStop struct {
	adapter.Adapter
	stopped chan<- struct{}
	release <-chan struct{}
}

func (h heldStop) Stop(ctx context.Context) error {
	err := h.Adapter.Stop(ctx)
	h.stopped <- struct{}{}
	<-h.release
	return err
}

// deadlineStop is a real adapter whose Stop stops the adapter, says so on
// stopped, then drains until its context ends, as a drain with a request
// still running does until the drain deadline, and sends on ended how far
// the node's clock had moved when it did.
type deadlineStop struct {
	adapter.Adapter
	clock   *sim.TestClock
	stopped chan<- struct{}
	ended   chan<- time.Duration
}

func (d deadlineStop) Stop(ctx context.Context) error {
	err := d.Adapter.Stop(ctx)
	d.stopped <- struct{}{}
	<-ctx.Done()
	d.ended <- d.clock.Elapsed()
	return err
}

// endedAt is how far past start a held drain ended, failing the test if
// none has 10 s after the clock passed its deadline.
func endedAt(t *testing.T, ended <-chan time.Duration, start time.Duration) time.Duration {
	t.Helper()
	select {
	case at := <-ended:
		return at - start
	case <-time.After(10 * time.Second):
		t.Fatal("no held drain ended within 10 s of the clock reaching its deadline")
		return 0
	}
}

// waitingSidecar serves both adapters with a fault on cue: while cue is set,
// the next Sync says so on entered and returns only once its context ends,
// as a sidecar that cannot be brought to match.
type waitingSidecar struct {
	cue     atomic.Bool
	entered chan struct{}
}

func (*waitingSidecar) Name() string       { return "waiting" }
func (*waitingSidecar) Adapters() []string { return []string{"nfs", "smb"} }

func (s *waitingSidecar) Sync(ctx context.Context, _ []adapter.Serving) error {
	if s.cue.CompareAndSwap(true, false) {
		s.entered <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

// holdingSidecar serves both adapters with a fault on cue: while cue is set,
// the next Sync hands the test the set it was given, on synced, and holds
// until release closes, as a sidecar slow to match.
type holdingSidecar struct {
	cue     atomic.Bool
	synced  chan []adapter.Serving
	release chan struct{}
}

func newHoldingSidecar() *holdingSidecar {
	return &holdingSidecar{synced: make(chan []adapter.Serving, 1), release: make(chan struct{})}
}

func (*holdingSidecar) Name() string       { return "holding" }
func (*holdingSidecar) Adapters() []string { return []string{"nfs", "smb"} }

func (s *holdingSidecar) Sync(_ context.Context, set []adapter.Serving) error {
	if s.cue.CompareAndSwap(true, false) {
		s.synced <- set
		<-s.release
	}
	return nil
}

// held is the set the held Sync was handed, failing the test if none is
// held within 10 s of what.
func (s *holdingSidecar) held(t *testing.T, what string) []adapter.Serving {
	t.Helper()
	select {
	case set := <-s.synced:
		return set
	case <-time.After(10 * time.Second):
		t.Fatalf("no sidecar Sync held within 10 s of %s", what)
		return nil
	}
}

// hears waits for the held adapter's Stop to stop it, failing the test after
// 10 s.
func hears(t *testing.T, stopped <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: no held Stop within 10 s", what)
	}
}

// rebuild has the node's next pass rebuild SMB at port: the settings
// generation moves, and reconcile is woken.
func rebuild(t *testing.T, h *nodetest.Harness, port uint16) {
	t.Helper()
	h.Node().SetSMB(true, smbAt(port))
	configure(t, h, storeA)
	h.Node().Wake()
}

// Start runs the first full reconcile, then the adapters' step, then
// reconcile's loop. An enabled adapter listens from the adapters' step on,
// not at step 4 nor in the first full reconcile, so no client reaches a node
// that has not reconciled; once Start returns the node is ready (RFC 23
// §2.2, §12's "§2.2 order").
func TestAnAdapterListensOnlyFromTheAdaptersStep(t *testing.T) {
	port := freePort(t)
	type step struct {
		name    string
		listens bool
	}
	var mu sync.Mutex
	var steps []step
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewStepping(func(_ *node.Node, name string) {
		if strings.HasPrefix(name, "wait ") || strings.HasPrefix(name, "stop ") {
			return
		}
		l := listens(port)
		mu.Lock()
		defer mu.Unlock()
		steps = append(steps, step{name, l})
	})))
	h.Node().SetSMB(true, smbAt(port))
	started(t, h)

	want := []step{{"refuse", false}, {"listeners", false}, {"register", false}, {"apply", false}, {"engine", false},
		{"filesystem", false}, {"runner", false}, {"first reconcile", false}, {"adapters", true}, {"reconcile", true}}
	mu.Lock()
	got := slices.Clone(steps)
	mu.Unlock()
	if !slices.Equal(got, want) {
		t.Errorf("start steps, and whether SMB's port took a dial after each: got %v, want %v", got, want)
	}
	api, _ := h.Node().Addrs()
	if got := get(t, api, "/health/ready"); got != ready {
		t.Errorf("readiness after Start: got %+v, want 200 ready", got)
	}
}

// An adapter refused at start, its port held, does not fail the start: the
// node is ready, NFS serves, and the refusal names SMB's record at the
// settings generation the first full reconcile read. It is retried on
// reconcile's backoff, with no counter moving: once the port is freed and the
// clock advances 1 s, SMB serves and the refusal is gone (RFC 20 §3.7, RFC 23
// §2.2, §2.3, §2.4).
func TestAnAdapterRefusedAtStartIsRetriedUntilItServes(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	waiting := make(chan struct{}, 1)
	h.Node().OnReconcileWait(func() {
		select {
		case waiting <- struct{}{}:
		default:
		}
	})
	configure(t, h, storeA) // a settings generation above zero for the refusal to carry
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	port, nfs := uint16(held.Addr().(*net.TCPAddr).Port), freePort(t)
	h.Node().SetNFS(true, nfsAt(nfs))
	h.Node().SetSMB(true, smbAt(port))
	started(t, h)

	api, _ := h.Node().Addrs()
	if got := get(t, api, "/health/ready"); got != ready {
		t.Errorf("readiness with SMB refused at start: got %+v, want 200 ready", got)
	}
	if !listens(nfs) {
		t.Errorf("with SMB refused at start NFS's port %d takes no dial, want NFS serving", nfs)
	}
	gen := counters(t, h).Settings
	refused := h.Node().Refused()
	if len(refused) != 1 || refused[0].Record.Kind != entity.ResourceAdapter || string(refused[0].Record.ID) != "smb" || refused[0].Generation != gen {
		t.Fatalf("refused after Start with SMB's port held: %s, want smb's adapter record at settings generation %d", describe(refused), gen)
	}

	_ = held.Close()
	select {
	case <-waiting: // reconcile's first wait after Start is set on the clock
	case <-time.After(10 * time.Second):
		t.Fatal("reconcile's loop set no wait within 10 s of Start")
	}
	moved := h.Node().Moved()
	h.Clock().Advance(time.Second)
	select {
	case <-moved:
	case <-time.After(10 * time.Second):
		t.Fatal("no reconcile pass within 10 s of the clock advancing 1 s")
	}
	if got := h.Node().Refused(); len(got) != 0 {
		t.Errorf("refused after the port was freed and the clock advanced 1 s: %s, want none", describe(got))
	}
	if !listens(port) {
		t.Errorf("SMB's port %d, freed, takes no dial after the clock advanced 1 s", port)
	}
}

// Stop syncs the sidecar with the empty set while both adapters still serve,
// readiness answering draining meanwhile, and both adapters have stopped
// before the operation runner's part stops (RFC 23 §2.2's stop steps 2 and
// 3, RFC 20 §3.5).
func TestStopSyncsTheEmptySetThenStopsEveryAdapter(t *testing.T) {
	var b built
	type states struct{ nfs, smb adapter.State }
	atRunner := make(chan states, 1)
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewStepping(func(_ *node.Node, step string) {
		if step == "wait runner" {
			atRunner <- states{b.last("nfs").Health().State, b.last("smb").Health().State}
		}
	})))
	side := newHoldingSidecar()
	h.Node().SetSidecar(side)
	h.Node().OnAdapter(b.keep)
	h.Node().SetNFS(true, nfsAt(0))
	h.Node().SetSMB(true, smbAt(0))
	if err := h.Node().Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	api, _ := h.Node().Addrs()

	side.cue.Store(true)
	stopped := make(chan error, 1)
	go func() { stopped <- h.Node().Stop(context.Background()) }()
	set := side.held(t, "Stop")
	during := states{b.last("nfs").Health().State, b.last("smb").Health().State}
	readiness := get(t, api, "/health/ready")
	close(side.release)
	if err := answered(t, stopped); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if len(set) != 0 {
		t.Errorf("Stop's first Sync carried %v, want the empty set", set)
	}
	if during != (states{adapter.StateServing, adapter.StateServing}) {
		t.Errorf("while Stop's first Sync ran: nfs %s, smb %s; want both serving", during.nfs, during.smb)
	}
	if readiness != draining {
		t.Errorf("readiness while the adapters stop: got %+v, want draining", readiness)
	}
	select {
	case got := <-atRunner:
		if got != (states{adapter.StateStopped, adapter.StateStopped}) {
			t.Errorf("as the runner's part stops: nfs %s, smb %s; want both stopped", got.nfs, got.smb)
		}
	default:
		t.Error("Stop never waited on the runner's part")
	}
}

// An adapter whose drain holds past the adapters' bound, the drain deadline
// plus 30 s, 130 s with drain_timeouts of 90 s and 100 s, is named not
// stopped within it, and leaves the filesystem, the engine and the store
// open under it, while reconcile, the runner and the listeners, which do not
// use it, stop. A later Stop, once it ended, closes the rest (RFC 23 §2.2).
func TestADrainPastItsBoundLeavesTheFilesystemOpen(t *testing.T) {
	var h *nodetest.Harness
	s := newSteps(t, &h)
	h = nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewStepping(s.stepped)))
	stoppedSMB, release := make(chan struct{}, 2), make(chan struct{})
	h.Node().OnAdapter(func(protocol string, a adapter.Adapter) adapter.Adapter {
		if protocol == "smb" {
			return heldStop{a, stoppedSMB, release}
		}
		return a
	})
	h.Node().SetNFS(true, nfsAt(0))
	smb := smbAt(0)
	smb.DrainTimeout = 100 * time.Second
	h.Node().SetSMB(true, smb)
	if err := h.Node().Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	stopped := make(chan error, 1)
	go func() { stopped <- h.Node().Stop(context.Background()) }()
	waitFor(t, s.waits, "adapters")
	hears(t, stoppedSMB, "the adapters' part")
	h.Clock().Advance(130 * time.Second)
	err := answered(t, stopped)
	for _, want := range []string{"stop adapters: not stopped within 2m10s", "stop filesystem: left open", "stop engine: left open", "stop store: left open"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Stop with SMB's drain held: got %v, want it to say %q", err, want)
		}
	}
	if got, want := stopsIn(s.reported()), []string{"stop reconcile", "stop runner", "stop listeners"}; !slices.Equal(got, want) {
		t.Errorf("parts stopped with SMB's drain held: got %v, want %v", got, want)
	}

	close(release)
	if err := h.Node().Stop(context.Background()); err != nil {
		t.Errorf("Stop once SMB's drain ended: got %v, want nil", err)
	}
}

// The adapters drain until the drain deadline, the step's start plus the
// longest drain_timeout, 100 s here, on the node's clock, and no sooner: SMB,
// draining until its context ends, still drains 31 s in, past the 30 s every
// other part is given, and 99 s in, the filesystem's part not stopped, and
// ends at 100 s, when Stop goes on inside the step's bound (RFC 23 §2.2).
// Each move of the clock waits for the operation runner's turn it wakes, so
// what else the move woke has had that long to run.
func TestTheAdaptersDrainUntilTheDeadline(t *testing.T) {
	var h *nodetest.Harness
	s := newSteps(t, &h)
	h = nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewStepping(s.stepped)))
	turned := make(chan struct{}, 1)
	h.Node().OnRunnerTurn(func() {
		select {
		case turned <- struct{}{}:
		default:
		}
	})
	stoppedSMB, ended := make(chan struct{}, 1), make(chan time.Duration, 1)
	h.Node().OnAdapter(func(protocol string, a adapter.Adapter) adapter.Adapter {
		if protocol == "smb" {
			return deadlineStop{a, h.Clock(), stoppedSMB, ended}
		}
		return a
	})
	h.Node().SetNFS(true, nfsAt(0))
	smb := smbAt(0)
	smb.DrainTimeout = 100 * time.Second
	h.Node().SetSMB(true, smb)
	if err := h.Node().Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	stopped := make(chan error, 1)
	go func() { stopped <- h.Node().Stop(context.Background()) }()
	waitFor(t, s.waits, "adapters") // the deadline is set as the step starts
	hears(t, stoppedSMB, "the adapters' part")
	start := h.Clock().Elapsed()
	for _, d := range []time.Duration{31 * time.Second, 68 * time.Second} {
		select {
		case <-turned:
		default:
		}
		h.Clock().Advance(d)
		select {
		case <-turned:
		case <-time.After(10 * time.Second):
			t.Fatalf("%v into the step the operation runner has not turned, want it running past the adapters' part", h.Clock().Elapsed()-start)
		}
		if slices.Contains(s.reported(), "stop filesystem") {
			t.Fatalf("%v into the step the filesystem's part has stopped, want it open while SMB drains", h.Clock().Elapsed()-start)
		}
	}
	h.Clock().Advance(time.Second)
	if got := endedAt(t, ended, start); got != 100*time.Second {
		t.Errorf("SMB's drain ended %v into the step, want at the deadline, 100 s", got)
	}
	if err := answered(t, stopped); err != nil {
		t.Errorf("Stop with SMB draining until the deadline: got %v, want nil", err)
	}
	want := []string{"stop reconcile", "stop adapters", "stop runner", "stop filesystem", "stop budgets", "stop engine", "stop listeners", "stop store"}
	if got := stopsIn(s.reported()); !slices.Equal(got, want) {
		t.Errorf("parts stopped: got %v, want %v", got, want)
	}
}

// A pass rebuilding SMB for a new port waits on the old adapter's drain,
// held here, reporting reconcile's loop idle every 10 s on the node's clock,
// so 90 s into the drain the node is live; once the drain ends SMB serves on
// the new port (RFC 25 §3.5, RFC 23 §2.3). NFS stays disabled: an accept
// loop reports on the runtime's clock, which the test does not move.
func TestARebuildWaitingOnADrainKeepsTheNodeLive(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	stoppedSMB, release := make(chan struct{}, 4), make(chan struct{})
	h.Node().OnAdapter(func(_ string, a adapter.Adapter) adapter.Adapter { return heldStop{a, stoppedSMB, release} })
	from, to := freePort(t), freePort(t)
	h.Node().SetSMB(true, smbAt(from))
	started(t, h)
	end := sync.OnceFunc(func() { close(release) })
	t.Cleanup(end)

	rebuild(t, h, to)
	hears(t, stoppedSMB, "the rebuild")
	api, _ := h.Node().Addrs()
	for i := range 9 {
		h.Clock().Advance(10 * time.Second)
		wait(t, fmt.Sprintf("liveness %d s into the drain", 10*(i+1)), func() bool {
			return get(t, api, "/health/live") == answer{http.StatusOK, "live"}
		})
	}
	end()
	wait(t, "SMB serving on its new port", func() bool { return listens(to) })
	if listens(from) {
		t.Errorf("after the rebuild SMB's old port %d takes a dial", from)
	}
}

// A Stop while a pass waits on a rebuild's drain stops reconcile within its
// bound, the clock still, and does not cut the drain short: the old adapter
// drains on to stop step 2's deadline, 90 s, its drain_timeout, and the
// adapters' part joins it, stopping that adapter again (RFC 23 §2.2, RFC 20
// §3.4).
func TestAStopJoinsTheDrainOfARebuild(t *testing.T) {
	var h *nodetest.Harness
	s := newSteps(t, &h)
	h = nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewStepping(s.stepped)))
	stoppedSMB, ended := make(chan struct{}, 2), make(chan time.Duration, 2)
	var first atomic.Bool
	h.Node().OnAdapter(func(protocol string, a adapter.Adapter) adapter.Adapter {
		if protocol == "smb" && first.CompareAndSwap(false, true) { // the adapter the rebuild replaces
			return deadlineStop{a, h.Clock(), stoppedSMB, ended}
		}
		return a
	})
	from, to := freePort(t), freePort(t)
	h.Node().SetSMB(true, smbAt(from))
	if err := h.Node().Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	rebuild(t, h, to)
	hears(t, stoppedSMB, "the rebuild")
	stopped := make(chan error, 1)
	go func() { stopped <- h.Node().Stop(context.Background()) }()
	waitFor(t, s.waits, "adapters")
	hears(t, stoppedSMB, "the adapters' part")
	start := h.Clock().Elapsed()
	for _, d := range []time.Duration{31 * time.Second, 58 * time.Second, time.Second} {
		h.Clock().Advance(d)
	}
	for range 2 {
		if got := endedAt(t, ended, start); got != 90*time.Second {
			t.Errorf("a Stop of the rebuild's old adapter ended its drain %v into stop step 2, want at its deadline, 90 s", got)
		}
	}
	if err := answered(t, stopped); err != nil {
		t.Errorf("Stop during the rebuild: got %v, want nil", err)
	}
}

// A sidecar Sync that a rebuild's drain holds, one that returns only once its
// context ends, ends with stop step 2's drain deadline and holds the step no
// longer: once the clock reaches the deadline Stop finishes inside the step's
// bound, NFS and both of SMB's ports stopped (RFC 23 §2.2, RFC 20 §3.5).
func TestARebuildsSyncEndsByTheDrainDeadline(t *testing.T) {
	var h *nodetest.Harness
	s := newSteps(t, &h)
	h = nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewStepping(s.stepped)))
	side := &waitingSidecar{entered: make(chan struct{}, 1)}
	h.Node().SetSidecar(side)
	nfs, from, to := freePort(t), freePort(t), freePort(t)
	h.Node().SetNFS(true, nfsAt(nfs))
	h.Node().SetSMB(true, smbAt(from))
	if err := h.Node().Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	side.cue.Store(true)
	rebuild(t, h, to)
	select {
	case <-side.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the rebuild synced no sidecar within 10 s")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- h.Node().Stop(context.Background()) }()
	waitFor(t, s.waits, "adapters") // the deadline is set as the step starts
	h.Clock().Advance(90 * time.Second)
	if err := answered(t, stopped); err != nil {
		t.Errorf("Stop with the rebuild's Sync held: got %v, want nil", err)
	}
	for _, p := range []struct {
		name string
		port uint16
	}{{"NFS's", nfs}, {"SMB's old", from}, {"SMB's new", to}} {
		if listens(p.port) {
			t.Errorf("after Stop %s port %d takes a dial", p.name, p.port)
		}
	}
}

// A pass held past reconcile's bound, here in a sidecar's Sync after a live
// change, leaves the adapters open, rather than stopping them under it, and
// what they use; a later Stop, once the Sync returned, stops the rest (RFC
// 23 §2.2).
func TestAPassHeldPastItsBoundLeavesTheAdaptersOpen(t *testing.T) {
	var h *nodetest.Harness
	s := newSteps(t, &h)
	h = nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewStepping(s.stepped)))
	side := newHoldingSidecar()
	h.Node().SetSidecar(side)
	h.Node().SetSMB(true, smbAt(0))
	if err := h.Node().Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	end := sync.OnceFunc(func() { close(side.release) })
	t.Cleanup(end)

	side.cue.Store(true)
	next := smbAt(0)
	next.IdleTimeout += time.Minute
	h.Node().SetSMB(true, next)
	configure(t, h, storeA)
	h.Node().Wake()
	side.held(t, "the live change")
	stopped := make(chan error, 1)
	go func() { stopped <- h.Node().Stop(context.Background()) }()
	waitFor(t, s.waits, "reconcile")
	h.Clock().Advance(30 * time.Second) // reconcile's bound
	err := answered(t, stopped)
	for _, want := range []string{"stop reconcile: not stopped within 30s", "stop adapters: left open", "stop filesystem: left open", "stop store: left open"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Stop with a pass held in a Sync: got %v, want it to say %q", err, want)
		}
	}

	end()
	if err := h.Node().Stop(context.Background()); err != nil {
		t.Errorf("Stop once the Sync returned: got %v, want nil", err)
	}
}

// New refuses an adapters.listen entry that is not an IP address, or that
// overlaps one listed before it, as a repeat or beside its family's
// unspecified address, naming the key and the indexes, never the value, and
// closes the KV it was handed (RFC 13 §6, Appendix C).
func TestNewRefusesAnAdaptersListenItCannotBind(t *testing.T) {
	for _, tc := range []struct {
		name   string
		listen []string
		want   string
	}{
		{"not an address", []string{"192.0.2.1", "nas.example"}, "node: adapters.listen[1]: not an IP address"},
		{"a repeat", []string{"192.0.2.1", "192.0.2.1"}, "node: adapters.listen[1]: overlaps adapters.listen[0]"},
		{"a repeat, IPv4-mapped", []string{"192.0.2.1", "::ffff:192.0.2.1"}, "node: adapters.listen[1]: overlaps adapters.listen[0]"},
		{"beside 0.0.0.0", []string{"0.0.0.0", "192.0.2.1"}, "node: adapters.listen[1]: overlaps adapters.listen[0]"},
		{"beside ::", []string{"192.0.2.1", "2001:db8::1", "::"}, "node: adapters.listen[2]: overlaps adapters.listen[1]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := badgerkv.Open(badgerkv.Options{})
			if err != nil {
				t.Fatal(err)
			}
			b := listening("127.0.0.1:0", "127.0.0.1:0")
			b.Adapters.Listen = tc.listen
			_, err = node.New(node.Config{
				Bootstrap: b, KV: db, Clock: sim.NewTestClock(time.Unix(0, 0)), Metrics: metrics.New("test", "test"),
				Logger: slog.New(slog.NewTextHandler(t.Output(), nil)),
			})
			if err == nil || err.Error() != tc.want {
				t.Fatalf("New with adapters.listen %q: got %v, want %q", tc.listen, err, tc.want)
			}
			for _, v := range tc.listen {
				if strings.Contains(err.Error(), v) {
					t.Errorf("the refusal %q prints %q, which may come from the environment", err, v)
				}
			}
			if err := db.View(t.Context(), func(kv.Reader) error { return nil }); err == nil {
				t.Error("the KV after a refused New: a View succeeded, want it closed")
			}
		})
	}
}

// An unset adapters.listen binds every interface: 0.0.0.0, and :: beside it
// where a tcp6 socket opens on this host (RFC 13 Appendix C).
func TestAnUnsetAdaptersListenBindsEveryInterface(t *testing.T) {
	want := []netip.Addr{netip.IPv4Unspecified()}
	if l, err := net.Listen("tcp6", "[::]:0"); err == nil {
		_ = l.Close()
		want = append(want, netip.IPv6Unspecified())
	}
	var b built
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		cfg.Bootstrap.Adapters.Listen = nil
		return node.New(cfg)
	}))
	h.Node().OnAdapter(b.keep)
	h.Node().SetSMB(true, smbAt(0))
	started(t, h)

	var got []netip.Addr
	for _, at := range b.last("smb").Addrs()[adapter.Socket{Service: "smb", Network: "tcp"}] {
		got = append(got, at.Addr())
	}
	slices.SortFunc(got, netip.Addr.Compare)
	if !slices.Equal(got, want) {
		t.Errorf("SMB with adapters.listen unset is bound on %v, want %v", got, want)
	}
}

// An adapter on the API's or the metrics listener's port is refused by RFC
// 13 §6's port rule before it is built, not by its bind: each refusal names
// the port as theirs.
func TestAnAdapterOnTheAPIsOrTheMetricsPortIsRefused(t *testing.T) {
	apiAddr, metAddr := freeAddr(t), freeAddr(t)
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		cfg.Bootstrap.API.Listen = []string{apiAddr}
		cfg.Bootstrap.Metrics.Listen = metAddr
		return node.New(cfg)
	}))
	h.Node().SetNFS(true, nfsAt(netip.MustParseAddrPort(apiAddr).Port()))
	h.Node().SetSMB(true, smbAt(netip.MustParseAddrPort(metAddr).Port()))
	started(t, h)

	refused := h.Node().Refused()
	var names []string
	for _, r := range refused {
		if strings.Contains(string(r.Error), "is the API's or the metrics listener's port") {
			names = append(names, string(r.Record.ID))
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"nfs", "smb"}) {
		t.Errorf("refused with NFS on the API's port and SMB on the metrics listener's: %s, want both by the port rule", describe(refused))
	}
}
