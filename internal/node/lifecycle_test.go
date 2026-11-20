package node_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// answer is what one HTTP probe answered; status 0 when nothing listens.
type answer struct {
	status int
	body   string
}

// get probes addr's path over a connection it does not keep.
func get(t *testing.T, addr, path string) answer {
	t.Helper()
	if addr == "" {
		return answer{}
	}
	c := http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Get("http://" + addr + path)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) {
			return answer{}
		}
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read GET %s: %v", path, err)
	}
	if path == "/metrics" {
		b = nil
	}
	return answer{resp.StatusCode, string(b)}
}

// steps keeps, per node, the steps and stops it reported and what each
// probe answered right after one, keyed by the step's name.
type steps struct {
	t     *testing.T
	h     **nodetest.Harness
	shard *entity.ShardID // the shard whose incarnation each snapshot reads, if any
	waits chan string     // each part whose stop Stop waits on, as it starts to
	mu    sync.Mutex
	order []string
	seen  map[string]snapshot
}

// snapshot is what a step left behind, read at its consumers: the API's
// readiness, the metrics listener, and the node's record.
type snapshot struct {
	ready, metrics answer
	registered     bool
	epoch          uint64
	address        string
	incarnation    uint64
}

func newSteps(t *testing.T, h **nodetest.Harness) *steps {
	return &steps{t: t, h: h, seen: map[string]snapshot{}, waits: make(chan string, 16)}
}

func (s *steps) stepped(n *node.Node, step string) {
	if part, ok := strings.CutPrefix(step, "wait "); ok {
		s.waits <- part
		return
	}
	s.mu.Lock()
	s.order = append(s.order, step)
	s.mu.Unlock()
	if strings.HasPrefix(step, "stop store") {
		return
	}
	api, met := n.Addrs()
	rec, found, err := (*s.h).Store().Nodes().Node(s.t.Context(), (*s.h).NodeID())
	if err != nil {
		s.t.Errorf("after %s: read the node's record: %v", step, err)
	}
	snap := snapshot{ready: get(s.t, api, "/health/ready"), metrics: get(s.t, met, "/metrics"), registered: found, epoch: rec.Epoch, address: rec.Address}
	if s.shard != nil {
		sh, err := (*s.h).Store().Shard(s.t.Context(), *s.shard)
		if err != nil {
			s.t.Errorf("after %s: read the share's shard: %v", step, err)
		}
		snap.incarnation = sh.Incarnation
	}
	s.mu.Lock()
	s.seen[step] = snap
	s.mu.Unlock()
}

func (s *steps) reported() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.order)
}

// addShare creates a share on a configured remote store in the harness's
// store, which Start binds.
func addShare(t *testing.T, h *nodetest.Harness) entity.Share {
	t.Helper()
	configure(t, h, storeA)
	return createShare(t, h, "a", storeA)
}

var (
	starting = answer{http.StatusServiceUnavailable, "starting"}
	draining = answer{http.StatusServiceUnavailable, "draining"}
	notReady = answer{http.StatusServiceUnavailable, "not_ready"}
	closed   = answer{}
	scraped  = answer{status: http.StatusOK}
)

// Start runs its steps in order, each read at its consumer once it ends:
// the listeners answer from step 2, the health endpoint starting until the
// last step ends; the node's record from step 3; its epoch raised by open
// state at step 5; the share's shard served by the filesystem at step 6.
// The node is ready once Start returns, and Stop drains, stops the parts in
// reverse, the listeners still answering until just before the store.
func TestStartAndStopInOrder(t *testing.T) {
	var h *nodetest.Harness
	s := newSteps(t, &h)
	h = nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewStepping(s.stepped)))
	sh := addShare(t, h)
	rec, err := h.Store().Shard(t.Context(), sh.Shard)
	if err != nil {
		t.Fatalf("read the share's shard: %v", err)
	}
	before := rec.Incarnation
	s.shard = &sh.Shard
	if err := h.Node().Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	api, _ := h.Node().Addrs()

	// The shard's incarnation is raised once by the engine's start and
	// once by the filesystem's serve.
	want := map[string]snapshot{
		"refuse":          {ready: closed, metrics: closed, incarnation: before},
		"listeners":       {ready: starting, metrics: scraped, incarnation: before},
		"register":        {ready: starting, metrics: scraped, registered: true, address: api, incarnation: before},
		"apply":           {ready: starting, metrics: scraped, registered: true, address: api, incarnation: before},
		"engine":          {ready: starting, metrics: scraped, registered: true, address: api, epoch: 1, incarnation: before + 1},
		"filesystem":      {ready: starting, metrics: scraped, registered: true, address: api, epoch: 1, incarnation: before + 2},
		"runner":          {ready: starting, metrics: scraped, registered: true, address: api, epoch: 1, incarnation: before + 2},
		"first reconcile": {ready: starting, metrics: scraped, registered: true, address: api, epoch: 1, incarnation: before + 2},
		"adapters":        {ready: starting, metrics: scraped, registered: true, address: api, epoch: 1, incarnation: before + 2},
		"reconcile":       {ready: starting, metrics: scraped, registered: true, address: api, epoch: 1, incarnation: before + 2},
	}
	steps := []string{"refuse", "listeners", "register", "apply", "engine", "filesystem", "runner", "first reconcile", "adapters", "reconcile"}
	if got := s.reported(); !slices.Equal(got, steps) {
		t.Fatalf("start steps: got %v, want %v", got, steps)
	}
	for _, step := range steps {
		if got := s.seen[step]; got != want[step] {
			t.Errorf("after start step %s: got %+v, want %+v", step, got, want[step])
		}
	}
	if got := get(t, api, "/health/ready"); got != (answer{http.StatusOK, "ready"}) {
		t.Errorf("readiness after Start: got %+v, want 200 ready", got)
	}
	if got := get(t, api, "/health/live"); got != (answer{http.StatusOK, "live"}) {
		t.Errorf("liveness after Start: got %+v, want 200 live", got)
	}

	if err := h.Node().Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	stops := []string{"stop reconcile", "stop adapters", "stop runner", "stop filesystem", "stop budgets", "stop engine", "stop listeners", "stop store"}
	if got := s.reported()[len(steps):]; !slices.Equal(got, stops) {
		t.Fatalf("stop steps: got %v, want %v", got, stops)
	}
	for _, step := range stops[:4] {
		if got := s.seen[step]; got.ready != draining || !got.registered {
			t.Errorf("after %s: readiness %+v, record read %v; want draining, and the store open", step, got.ready, got.registered)
		}
	}
	if got := s.seen["stop listeners"]; got.ready != closed || !got.registered {
		t.Errorf("after stop listeners: readiness %+v, record read %v; want closed, and the store still open", got.ready, got.registered)
	}
}

// A clone after Start is judged on its request: step 4 handed the engine its
// settings, and step 5 bound the share, so the engine refuses it neither for
// no settings applied nor for a share it does not serve.
func TestStartAppliesSettingsAndBindsShares(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	sh := addShare(t, h)
	if err := h.Node().Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := h.Node().Stop(t.Context()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()
	f := entity.NewFileID()
	err := h.Node().Engine().Clone(t.Context(), sh.ID, f, sh.ID, f, vfserr.Extent{Len: 0})
	if !errors.Is(err, vfserr.ErrInvalid) {
		t.Errorf("a zero-length clone on the share after Start: got %v, want ErrInvalid, the engine holding settings and the share bound", err)
	}
}

// startFault is one start step a fault reaches.
type startFault struct {
	step string
	// cfg breaks the step through the node's configuration, fault through
	// a FaultKV rule set once the node is built.
	cfg   func(t *testing.T, cfg *node.Config)
	fault func(nodeKey string) sim.Rule
	// registered and epoch are the node's record after the failed Start.
	registered bool
	epoch      uint64
	// undone are the parts the failed Start stopped, in order.
	undone []string
	// want is text the error carries besides the step's name.
	want string
}

// failOn fails the from-th call of op on key.
func failOn(op, key string, from int) sim.Rule {
	return sim.Rule{Op: op, Key: key, From: from, Count: 1, Action: sim.FaultAction{Err: errInjected}}
}

var errInjected = errors.New("injected")

// A failure at each start step a fault reaches stops the start with an error
// naming the step, stops what earlier steps started in reverse but the
// listeners and the store, answers not_ready and raises start_failed; Stop
// then closes the listeners and the store, and the watch finds nothing left.
func TestStartFailureUndoesTheEarlierSteps(t *testing.T) {
	prof.Watch(t, prof.Options{})
	for _, tc := range []startFault{
		{step: "refuse", cfg: func(t *testing.T, cfg *node.Config) {
			dir := filepath.Join(t.TempDir(), "foreign")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := journal.Init(t.Context(), dir, journal.InstallationID{'f', 'o', 'r', 'e', 'i', 'g', 'n'}, journal.WithClock(sim.NewTestClock(time.Unix(0, 0))), journaltest.SuitableMount()); err != nil {
				t.Fatalf("init a foreign journal: %v", err)
			}

			cfg.Bootstrap.Journals = []bootstrap.Journal{{Path: dir}}
		}},
		// The step reads each device with the node's journal options: one the
		// journal refuses fails it here, before the engine would.
		{step: "refuse", cfg: func(_ *testing.T, cfg *node.Config) {
			cfg.Journal = append(cfg.Journal, journal.WithDescriptorBudget(journal.NewDescriptorBudget(-1)))
		}},
		// A device whose node-id copy names another node fails the step's
		// write of the missing copies.
		{step: "refuse", cfg: func(t *testing.T, cfg *node.Config) {
			path := filepath.Join(cfg.Bootstrap.Journals[0].Path, journal.NodeIDFileName)
			if err := os.WriteFile(path, nodeIDCopy(entity.NodeID{'o', 't', 'h', 'e', 'r'}), 0o644); err != nil {
				t.Fatal(err)
			}
		}, want: "node-id copies differ"},
		{step: "listeners", cfg: func(t *testing.T, cfg *node.Config) {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = l.Close() })

			cfg.Bootstrap.API.Listen = []string{l.Addr().String()}
		}},
		{step: "register", fault: func(k string) sim.Rule { return failOn(kvtest.OpSet, k, 1) }},
		{step: "apply", fault: func(string) sim.Rule { return failOn(kvtest.OpGet, "CFGGEN", 1) }, registered: true},
		{step: "engine", fault: func(k string) sim.Rule { return failOn(kvtest.OpSet, k, 2) }, registered: true},
		{step: "filesystem", fault: func(string) sim.Rule { return failOn(kvtest.OpSet, "SH‖*", 2) }, registered: true, epoch: 1, undone: []string{"stop engine"}},
		{step: "first reconcile", fault: func(string) sim.Rule { return failOn(kvtest.OpGet, "CFGGEN", 2) }, registered: true, epoch: 1, undone: []string{"stop runner", "stop filesystem", "stop budgets", "stop engine"}},
	} {
		t.Run(tc.step, func(t *testing.T) {
			var h *nodetest.Harness
			s := newSteps(t, &h)
			var f *kvtest.FaultKV
			h = nodetest.New(t, nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
				f = kvtest.NewFaultKV(cfg.KV)
				cfg.KV = f
				if tc.cfg != nil {
					tc.cfg(t, &cfg)
				}
				return node.NewStepping(s.stepped)(cfg)
			}))
			addShare(t, h)
			if tc.fault != nil {
				id := h.NodeID()
				sched, err := sim.NewSchedule(1, nil, tc.fault("N‖"+hex.EncodeToString(id[:])))
				if err != nil {
					t.Fatalf("fault schedule: %v", err)
				}
				f.SetFault(sched)
			}
			err := h.Node().Start(t.Context())
			f.SetFault(nil)
			if err == nil || !strings.Contains(err.Error(), "start step "+tc.step+":") || tc.fault != nil && !errors.Is(err, errInjected) {
				t.Fatalf("Start: got %v, want an error at start step %s", err, tc.step)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Start: got %v, want it to say %q", err, tc.want)
			}
			api, _ := h.Node().Addrs()
			wantReady := notReady
			if api == "" {
				wantReady = closed
			}
			if got := get(t, api, "/health/ready"); got != wantReady {
				t.Errorf("readiness after the failed Start: got %+v, want %+v", got, wantReady)
			}
			conds := h.Node().StartFailed()
			if len(conds) != 1 || conds[0].Name != "start_failed" || !strings.Contains(conds[0].Cause.Error(), "start step "+tc.step) {
				t.Errorf("conditions after the failed Start: got %+v, want start_failed naming step %s", conds, tc.step)
			}
			rec, found, err := h.Store().Nodes().Node(t.Context(), h.NodeID())
			if err != nil || found != tc.registered || rec.Epoch != tc.epoch {
				t.Errorf("the node's record after the failed Start: found %v at epoch %d (%v), want found %v at epoch %d", found, rec.Epoch, err, tc.registered, tc.epoch)
			}
			if got := s.reported(); !slices.Equal(stopsIn(got), tc.undone) {
				t.Errorf("parts the failed Start stopped: got %v, want %v", stopsIn(got), tc.undone)
			}
			if err := h.Node().Stop(t.Context()); err != nil {
				t.Fatalf("Stop after the failed Start: %v", err)
			}
			if got := get(t, api, "/health/ready"); got != closed {
				t.Errorf("readiness after Stop: got %+v, want the listener closed", got)
			}
			if _, _, err := h.Store().Nodes().Node(t.Context(), h.NodeID()); err == nil {
				t.Error("a read after Stop: succeeded, want the store closed")
			}
		})
	}
}

// stopsIn is the stops among reported steps.
func stopsIn(reported []string) []string {
	var out []string
	for _, step := range reported {
		if strings.HasPrefix(step, "stop ") {
			out = append(out, step)
		}
	}
	return out
}

// A node starts once: a second Start is refused before it runs a step, so
// the node goes on serving, and a Start after Stop is refused before any
// step, whether the node started before its Stop or not.
func TestStartOnce(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	if err := h.Node().Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := h.Node().Start(t.Context()); err == nil || strings.Contains(err.Error(), "start step") {
		t.Errorf("a second Start: got %v, want refused before any step", err)
	}
	api, _ := h.Node().Addrs()
	if got := get(t, api, "/health/ready"); got != (answer{http.StatusOK, "ready"}) {
		t.Errorf("readiness after a second Start: got %+v, want 200 ready", got)
	}
	if got := h.Node().StartFailed(); len(got) != 0 {
		t.Errorf("conditions after a second Start: got %+v, want none", got)
	}
	if err := h.Node().Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := h.Node().Start(t.Context()); err == nil || strings.Contains(err.Error(), "start step") {
		t.Errorf("Start after Stop: got %v, want refused before any step", err)
	}

	fresh := nodetest.New(t, nodetest.InMemory())
	if err := fresh.Node().Stop(t.Context()); err != nil {
		t.Fatalf("Stop of a node never started: %v", err)
	}
	if err := fresh.Node().Start(t.Context()); err == nil || strings.Contains(err.Error(), "start step") {
		t.Errorf("Start after a Stop with no Start: got %v, want refused before any step", err)
	}
}

// A Stop whose context has already ended still stops every part in reverse,
// and the store, and answers nil: only the node's clock bounds a part's stop.
func TestStopWithAnEndedContext(t *testing.T) {
	var h *nodetest.Harness
	s := newSteps(t, &h)
	h = nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewStepping(s.stepped)))
	if err := h.Node().Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := h.Node().Stop(ctx); err != nil {
		t.Errorf("Stop with an ended context: got %v, want nil", err)
	}
	stops := []string{"stop reconcile", "stop adapters", "stop runner", "stop filesystem", "stop budgets", "stop engine", "stop listeners", "stop store"}
	if got := stopsIn(s.reported()); !slices.Equal(got, stops) {
		t.Errorf("parts stopped with an ended context: got %v, want %v", got, stops)
	}
}

// A device whose journal cannot be read, a segment and no format file, fails
// the start at its first step, naming the device, before anything opens.
func TestStartRefusesAnUnreadableDevice(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "segment-only")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x.seg"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		cfg.Bootstrap.Journals = append(cfg.Bootstrap.Journals, bootstrap.Journal{Path: dir})
		return node.New(cfg)
	}))
	err := h.Node().Start(t.Context())
	if err == nil || !strings.Contains(err.Error(), "start step refuse:") || !strings.Contains(err.Error(), dir) {
		t.Errorf("Start with a device holding only a segment: got %v, want start step refuse naming %s", err, dir)
	}
	if api, _ := h.Node().Addrs(); api != "" {
		t.Errorf("listeners after the refused Start: API bound at %s, want none opened", api)
	}
	if err := h.Node().Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// foreignJournal creates, over its own temporary directory, a journal of an
// installation the harness's store does not hold: its installation ID has
// byte 15 set to 9, which the harness's own installation does not.
func foreignJournal(t *testing.T) (dir string, id journal.JournalID) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "foreign")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	id, err := journal.Init(t.Context(), dir, journal.InstallationID{15: 9},
		journal.WithClock(sim.NewTestClock(time.Unix(0, 0))), journaltest.SuitableMount())
	if err != nil {
		t.Fatalf("init a journal of another installation: %v", err)
	}
	return dir, id
}

// A device whose journal belongs to another installation fails the start at
// its first step, naming the device, the journal's identity, its installation
// and the store's.
func TestStartRefusesAJournalOfAnotherInstallation(t *testing.T) {
	dir, jid := foreignJournal(t)
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		cfg.Bootstrap.Journals = []bootstrap.Journal{{Path: dir}}
		return node.New(cfg)
	}))
	err := h.Node().Start(t.Context())
	if err == nil {
		t.Fatal("Start over a journal of another installation: nil error, want the refusal")
	}
	for what, want := range map[string]string{
		"the start step":           "start step refuse:",
		"the device's dir":         dir,
		"the journal's identity":   fmt.Sprintf("%x", jid),
		"the journal installation": fmt.Sprintf("%x", journal.InstallationID{15: 9}),
		"the store's installation": fmt.Sprintf("%x", [16]byte{'n', 'o', 'd', 'e', 't', 'e', 's', 't'}),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %s: %q missing from %q", what, want, err.Error())
		}
	}
	if api, _ := h.Node().Addrs(); api != "" {
		t.Errorf("listeners after the refused Start: API bound at %s, want none opened", api)
	}
	if err := h.Node().Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// decision: the refused start's bytes are hashed over every device and the
// store's parent tree, the live store's own directory left out — as the
// comment below names — because Badger settles it when Stop closes it no
// matter what the start did, so its bytes hold no evidence either way. The
// row therefore proves the refusal changed no device and no store sibling;
// it does not prove the store's own files were untouched. Upgrade to a
// committed-keys before/after scan, as the kvstore Init rows do, when this
// row must prove that too.
func TestStartRefusalLeavesEverythingByteIdentical(t *testing.T) {
	foreign, _ := foreignJournal(t)
	var device string
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		device = cfg.Bootstrap.Journals[0].Path
		cfg.Bootstrap.Journals = []bootstrap.Journal{{Path: foreign}}
		return node.New(cfg)
	}))
	// The store's own directory is left out of the parent's hash: Badger
	// settles it when Stop closes it — memtable to SST, LOCK removed — no
	// matter what the start did, so its bytes hold no evidence either way.
	// Everything the refusal could create or change is inside the parent's
	// tree — the harness's device under it among them — except the foreign
	// journal's own tree, hashed beside it.
	parent := filepath.Dir(device)
	store := filepath.Join(parent, "store")
	tree := func() map[string]string {
		return map[string]string{
			parent:  node.HashTree(t, parent, store),
			foreign: node.HashTree(t, foreign),
		}
	}
	before := tree()
	err := h.Node().Start(t.Context())
	if err == nil || !strings.Contains(err.Error(), "start step refuse:") {
		t.Fatalf("Start over a journal of another installation: got %v, want a start step refuse", err)
	}
	if serr := h.Node().Stop(t.Context()); serr != nil {
		t.Fatalf("Stop after the refused Start: %v", serr)
	}
	for root, after := range tree() {
		if after != before[root] {
			t.Errorf("tree %s across the refused start: hash %s after %s before, want the refusal to touch nothing", root, after, before[root])
		}
	}
}

// The start_failed condition answers while Start holds the node at a step:
// it does not wait for the Start to end.
func TestStartFailedAnswersDuringStart(t *testing.T) {
	answered := make(chan int, 1)
	h := nodetest.New(t, nodetest.WithNew(node.NewStepping(func(n *node.Node, step string) {
		if step != "apply" {
			return
		}
		go func() { answered <- len(n.StartFailed()) }()
		select {
		case got := <-answered:
			if got != 0 {
				t.Errorf("start_failed during a Start that has not failed: got %d conditions, want none", got)
			}
		case <-time.After(10 * time.Second):
			t.Error("start_failed during Start: no answer 10 s later, want one while Start holds the node")
		}
	})))
	if err := h.Node().Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := h.Node().Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// Start logs the addresses the API and the metrics listeners bound, which a
// host reads to reach a node it gave port 0.
func TestStartLogsTheBoundAddresses(t *testing.T) {
	var logged syncBuffer
	h := nodetest.New(t, nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		cfg.Logger = slog.New(slog.NewTextHandler(&logged, nil))
		return node.New(cfg)
	}))
	if err := h.Node().Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	api, met := h.Node().Addrs()
	if err := h.Node().Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	out := logged.String()
	for name, addr := range map[string]string{"api": api, "metrics": met} {
		if !strings.Contains(out, name+"="+addr) {
			t.Errorf("the log after Start: %s=%s missing from %q", name, addr, out)
		}
	}
}

// The API listens on api.listen and the metrics listener on metrics.listen,
// each on its own address.
func TestStartListensOnEachKeysAddress(t *testing.T) {
	apiAddr, metAddr := freeAddr(t), freeAddr(t)
	h := nodetest.New(t, nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		cfg.Bootstrap.API.Listen = []string{apiAddr}
		cfg.Bootstrap.Metrics.Listen = metAddr

		return node.New(cfg)
	}))

	if err := h.Node().Start(t.Context()); err != nil {
		t.Fatalf("Start with the API on %s and metrics on %s: %v", apiAddr, metAddr, err)
	}

	api, met := h.Node().Addrs()
	if err := h.Node().Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if api != apiAddr || met != metAddr {
		t.Errorf(
			"bound addresses: got api %s, metrics %s; want api %s, metrics %s",
			api,
			met,
			apiAddr,
			metAddr,
		)
	}
}

// freeAddr is a loopback address with a port no listener holds: one the
// test bound and closed.
func freeAddr(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	return addr
}

// syncBuffer is a buffer the node's goroutines can log to while the test
// reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// deleteUser creates a user, marks it deleted and records its pending
// deletion, as the management API's delete does.
func deleteUser(t *testing.T, h *nodetest.Harness) entity.OperationID {
	t.Helper()
	cp := h.Store().ControlPlane()
	u := entity.User{Principal: entity.NewUser(), Name: "alice"}
	gen, err := cp.PutUser(t.Context(), u, 0, entity.Alloc{})
	if err != nil {
		t.Fatalf("create a user: %v", err)
	}
	op := entity.Operation{ID: entity.OperationID(uuid.New()), Kind: string(apiv1.OpUserDelete),
		Target: entity.ResourceRef{Kind: entity.ResourceUser, ID: u.Principal.ID[:]}, Account: entity.NewUser(), State: entity.OpPending}
	if err := cp.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		if err := tx.DeleteUser(u.Principal, gen); err != nil {
			return err
		}
		_, err := tx.PutOperation(op, 0)
		return err
	}); err != nil {
		t.Fatalf("mark the user deleted: %v", err)
	}
	return op.ID
}

func opState(t *testing.T, h *nodetest.Harness, id entity.OperationID) entity.OpState {
	t.Helper()
	op, _, err := h.Store().ControlPlane().Operation(t.Context(), id)
	if err != nil {
		t.Fatalf("read the operation: %v", err)
	}
	return op.State
}

// An operation recorded before Start is driven once step 7 starts the
// runner, on the node's clock, to its end; the runner's turns keep the node
// live past the watchdog's stall.
func TestStartRunsTheOperationRunner(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	id := deleteUser(t, h)
	turns := make(chan struct{}, 1)
	h.Node().OnRunnerTurn(func() { turns <- struct{}{} })
	if err := h.Node().Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// parked waits for the runner to end a turn and set its wait on the
	// clock, so the clock never moves before the runner waits on it.
	parked := func() {
		t.Helper()
		select {
		case <-turns:
		case <-time.After(10 * time.Second):
			t.Fatal("no runner turn within 10 s")
		}
	}
	for i := 0; ; i++ {
		parked()
		if opState(t, h, id) == entity.OpSucceeded {
			break
		}
		if i == 10 {
			t.Fatalf("the user's deletion after Start: state %v after 10 runner turns, want succeeded", opState(t, h, id))
		}
		h.Clock().Advance(time.Second)
	}
	for range 61 {
		h.Clock().Advance(time.Second)
		parked()
	}
	api, _ := h.Node().Addrs()
	if got := get(t, api, "/health/live"); got != (answer{http.StatusOK, "live"}) {
		t.Errorf("liveness 61 s of runner turns after Start: got %+v, want 200 live", got)
	}
	if err := h.Node().Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// A stop step that misses its bound is named in Stop's error and leaves the
// store open under it, while the parts that do not use it stop; a later Stop,
// once it ended, closes the store.
func TestStopStepPastItsBound(t *testing.T) {
	var h *nodetest.Harness
	s := newSteps(t, &h)
	var f *kvtest.FaultKV
	h = nodetest.New(t, nodetest.Watch(), nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		f = kvtest.NewFaultKV(cfg.KV)
		cfg.KV = f
		return node.NewStepping(s.stepped)(cfg)
	}))
	if err := h.Node().Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The delay waits on a clock of its own, so advancing the node's clock
	// past the stop bound does not release it. It holds the operation's own
	// write, so the status writer's commits pass.
	held := sim.NewTestClock(time.Unix(0, 0))
	sched, err := sim.NewSchedule(1, held, sim.Rule{Op: kvtest.OpSet, Key: keys.Describe([]byte{keys.KindOP}) + "‖*", Count: 1, Action: sim.FaultAction{Delay: time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	f.SetFault(sched)
	deleteUser(t, h)
	h.Clock().Advance(time.Second)
	wait(t, "the runner's commit to be held", func() bool { return held.Pending() == 1 })

	stopped := make(chan error, 1)
	go func() { stopped <- h.Node().Stop(t.Context()) }()
	waitFor(t, s.waits, "runner")
	h.Clock().Advance(30 * time.Second)
	stopErr := answered(t, stopped)
	if stopErr == nil || !strings.Contains(stopErr.Error(), "stop runner: not stopped within") || !strings.Contains(stopErr.Error(), "stop store: left open") {
		t.Errorf("Stop with the runner held: got %v, want the runner named past its bound and the store left open", stopErr)
	}
	stops := []string{"stop reconcile", "stop adapters", "stop filesystem", "stop budgets", "stop engine", "stop listeners"}
	if got := stopsIn(s.reported()); !slices.Equal(got, stops) {
		t.Errorf("parts stopped with the runner held: got %v, want %v", got, stops)
	}
	if _, _, err := h.Store().Nodes().Node(t.Context(), h.NodeID()); err != nil {
		t.Errorf("a read once Stop returned: %v, want the store open", err)
	}

	held.Advance(time.Hour)
	go func() { stopped <- h.Node().Stop(t.Context()) }()
	if err := answered(t, stopped); err != nil {
		t.Errorf("Stop once the runner's commit ended: got %v, want nil", err)
	}
	if got := stopsIn(s.reported())[len(stops):]; !slices.Equal(got, []string{"stop runner", "stop store"}) {
		t.Errorf("parts the second Stop stopped: got %v, want [stop runner stop store]", got)
	}
}

// The filesystem uses the engine and the zero check's part: a filesystem
// whose Drain misses its bound, as a call it admitted is held on a read,
// leaves both open under it, and the store, while the runner and the
// listeners stop; a later Stop, once the call answered, stops the rest.
func TestStopLeavesTheEngineUnderAHeldFilesystem(t *testing.T) {
	var h *nodetest.Harness
	s := newSteps(t, &h)
	var f *kvtest.FaultKV
	h = nodetest.New(t, nodetest.Watch(), nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		f = kvtest.NewFaultKV(cfg.KV)
		cfg.KV = f
		return node.NewStepping(s.stepped)(cfg)
	}))
	if err := h.Node().Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The delay waits on a clock of its own, so advancing the node's clock
	// past the stop bound does not release it.
	held := sim.NewTestClock(time.Unix(0, 0))
	sched, err := sim.NewSchedule(1, held, sim.Rule{Op: kvtest.OpGet, Key: "*", Count: 1, Action: sim.FaultAction{Delay: time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	f.SetFault(sched)
	// The read answers whatever it answers, no client 1 being recorded: only
	// that it returned matters.
	recorded := make(chan struct{})
	go func() {
		defer close(recorded)
		_, _ = h.Node().Filesystem().Record(context.Background(), vfs.Call{}, 1)
	}()
	wait(t, "the filesystem's read to be held", func() bool { return held.Pending() == 1 })

	stopped := make(chan error, 1)
	go func() { stopped <- h.Node().Stop(t.Context()) }()
	waitFor(t, s.waits, "filesystem")
	h.Clock().Advance(30 * time.Second)
	stopErr := answered(t, stopped)
	for _, want := range []string{"stop filesystem: not stopped within", "stop budgets: left open", "stop engine: left open", "stop store: left open"} {
		if stopErr == nil || !strings.Contains(stopErr.Error(), want) {
			t.Errorf("Stop with the filesystem's call held: got %v, want it to say %q", stopErr, want)
		}
	}
	stops := []string{"stop reconcile", "stop adapters", "stop runner", "stop listeners"}
	if got := stopsIn(s.reported()); !slices.Equal(got, stops) {
		t.Errorf("parts stopped with the filesystem held: got %v, want %v", got, stops)
	}

	held.Advance(time.Hour)
	select {
	case <-recorded:
	case <-time.After(10 * time.Second):
		t.Fatal("the filesystem's call still held 10 s after its read was released")
	}
	go func() { stopped <- h.Node().Stop(t.Context()) }()
	if err := answered(t, stopped); err != nil {
		t.Errorf("Stop once the filesystem's call answered: got %v, want nil", err)
	}
	if got, want := stopsIn(s.reported())[len(stops):], []string{"stop filesystem", "stop budgets", "stop engine", "stop store"}; !slices.Equal(got, want) {
		t.Errorf("parts the second Stop stopped: got %v, want %v", got, want)
	}
}

// waitFor waits until Stop reports it waits on part's stop, bounded in real
// time.
func waitFor(t *testing.T, waits <-chan string, part string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case p := <-waits:
			if p == part {
				return
			}
		case <-deadline:
			t.Fatalf("Stop never waited on the %s", part)
		}
	}
}

// answered is what Stop answered, bounded in real time.
func answered(t *testing.T, stopped <-chan error) error {
	t.Helper()
	select {
	case err := <-stopped:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Stop still running 10 s later")
		return nil
	}
}

// wait polls cond until it holds, failing after a backstop.
func wait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		runtime.Gosched()
	}
}
