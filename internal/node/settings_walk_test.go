package node_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/settings"
	"github.com/cubbit/dittofs/pkg/sim"
)

// walkPage is the rows of one page of the settings walk, RFC 23 section 2.3's
// 256.
const walkPage = 256

// padRows writes n installation settings of no section, which the walk
// reads and no section claims, so the walk takes more than one page.
func padRows(t *testing.T, h *nodetest.Harness, n int) {
	t.Helper()

	for i := range n {
		s := entity.Setting{
			Scope: entity.SettingScope{Kind: entity.ScopeInstallation},
			Key:   fmt.Sprintf("pad.k%04d", i),
			Value: []byte("v"),
		}
		if _, err := h.Store().ControlPlane().PutSetting(t.Context(), s, 0); err != nil {
			t.Fatalf("put %s: %v", s.Key, err)
		}
	}
}

// walkWait tells a test the settings walk is waiting on the store with its
// bound armed on the node's clock, after it received a number of rows: the
// event to advance the clock on, since the store stuck on a page does not say
// the walk armed its bound yet.
type walkWait struct {
	target atomic.Int64
	armed  chan struct{}
}

// hookWalkWait installs the hook. Call it before Start.
func hookWalkWait(h *nodetest.Harness) *walkWait {
	w := &walkWait{armed: make(chan struct{}, 1)}
	w.target.Store(-1)
	h.Node().OnSettingsWalkWait(func(seen int) {
		if int64(seen) == w.target.Load() {
			select {
			case w.armed <- struct{}{}:
			default:
			}
		}
	})

	return w
}

// expect has the next wait after seen rows signal, forgetting earlier ones.
// Call it after the walks that must not count, before the one that must.
func (w *walkWait) expect(seen int) {
	w.target.Store(int64(seen))

	select {
	case <-w.armed:
	default:
	}
}

// await answers once the walk waits after the rows expect named.
func (w *walkWait) await(t *testing.T) {
	t.Helper()

	select {
	case <-w.armed:
	case <-time.After(10 * time.Second):
		t.Fatal("the walk armed no wait on the store within 10 s")
	}
}

// armWaits answers once for each wait reconcile's loop sets on the node's
// clock, which is once at the loop's start and once after each pass it
// released, failed or not, ended. Call it before Start.
func armWaits(h *nodetest.Harness) <-chan struct{} {
	ch := make(chan struct{}, 256)

	h.Node().OnReconcileWait(func() { ch <- struct{}{} })

	return ch
}

// waitsSettled answers for every wait reconcile armed so far, so each
// walkTurn after it answers for its own pass alone.
func waitsSettled(t *testing.T, armed <-chan struct{}) {
	t.Helper()

	select {
	case <-armed:
	case <-time.After(10 * time.Second):
		t.Fatal("reconcile armed no wait within 10 s")
	}

	for {
		select {
		case <-armed:
		default:
			return
		}
	}
}

// walkTurn advances the node's clock a second and answers once the reconcile
// pass the advance releases has ended, however long the store it read took
// and whether it failed: the loop arms its next wait only after the pass.
func walkTurn(t *testing.T, h *nodetest.Harness, armed <-chan struct{}) {
	t.Helper()

	h.Clock().Advance(time.Second)

	select {
	case <-armed:
	case <-time.After(30 * time.Second):
		t.Fatal("no reconcile pass within 30 s")
	}
}

// walkTuning is the settings shape of each section these tests declares: one
// live key, level, whose Validate bounds it.
type walkTuning struct {
	Level int `json:"level" class:"live"`
}

func (w walkTuning) Validate() error {
	if w.Level < 0 || w.Level > 100 {
		return fmt.Errorf("level must be between 0 and 100, got %d", w.Level)
	}

	return nil
}

// walkPair is a section with two live keys, one per share: a refusal of one
// key holds the instance whole, so its other key's change reaches nothing.
type walkPair struct {
	A int `json:"a" class:"live"`
	B int `json:"b" class:"live"`
}

func (w walkPair) Validate() error {
	if w.A < 0 || w.A > 100 || w.B < 0 || w.B > 100 {
		return fmt.Errorf("a and b must be between 0 and 100, got %d and %d", w.A, w.B)
	}

	return nil
}

// boundTuning is the settings shape of a section with a live key and a bound
// key, so a change of either runs its section's apply.
type boundTuning struct {
	Level int `json:"level" class:"live"`
	Floor int `json:"floor" class:"bound"`
}

func (b boundTuning) Validate() error {
	if b.Level < 0 || b.Level > 100 || b.Floor < 0 || b.Floor > 100 {
		return fmt.Errorf(
			"level and floor must be between 0 and 100, got %d and %d",
			b.Level,
			b.Floor,
		)
	}

	return nil
}

// bootsTuning is the settings shape of a section with one restart key.
type bootsTuning struct {
	Halt int `json:"halt" class:"restart"`
}

func (b bootsTuning) Validate() error {
	if b.Halt < 0 || b.Halt > 100 {
		return fmt.Errorf("halt must be between 0 and 100, got %d", b.Halt)
	}

	return nil
}

// nextTuning is the settings shape of a section with one next-write key.
type nextTuning struct {
	After int `json:"after" class:"next write"`
}

func (n nextTuning) Validate() error {
	if n.After < 0 || n.After > 100 {
		return fmt.Errorf("after must be between 0 and 100, got %d", n.After)
	}

	return nil
}

// stepTuning is the settings shape of a per-share section with one live key
// and one restart key.
type stepTuning struct {
	Level int `json:"level" class:"live"`
	Halt  int `json:"halt"  class:"restart"`
}

func (s stepTuning) Validate() error {
	if s.Level < 0 || s.Level > 100 || s.Halt < 0 || s.Halt > 100 {
		return fmt.Errorf(
			"level and halt must be between 0 and 100, got %d and %d",
			s.Level,
			s.Halt,
		)
	}

	return nil
}

// declareSections declares the tests' sections, every one read where its
// component uses it: alpha, a node's override of an installation default;
// beta and eps at installation scope; gamma once per share; delta once per
// share with two keys; nest with a section nested under its prefix, and
// alphabet, whose prefix holds alpha's in it. Call it before Start.
func declareSections(t *testing.T, h *nodetest.Harness) {
	t.Helper()
	h.Node().DeclareSections(
		node.Declared{
			Prefix: "alpha",
			Section: settings.Of(
				"alpha",
				settings.Override(entity.ScopeInstallation, entity.ScopeNode),
				walkTuning{Level: 1},
			),
			AtUse: true,
		},
		node.Declared{
			Prefix: "beta",
			Section: settings.Of(
				"beta",
				settings.At(entity.ScopeInstallation),
				walkTuning{Level: 1},
			),
			AtUse: true,
		},
		node.Declared{
			Prefix: "eps",
			Section: settings.Of(
				"eps",
				settings.At(entity.ScopeInstallation),
				walkTuning{Level: 1},
			),
			AtUse: true,
		},
		node.Declared{
			Prefix:  "gamma",
			Section: settings.Of("gamma", settings.At(entity.ScopeShare), walkTuning{Level: 1}),
			AtUse:   true,
		},
		node.Declared{
			Prefix:  "delta",
			Section: settings.Of("delta", settings.At(entity.ScopeShare), walkPair{A: 1, B: 1}),
			AtUse:   true,
		},
		node.Declared{
			Prefix: "nest",
			Section: settings.Of(
				"nest",
				settings.At(entity.ScopeInstallation),
				walkTuning{Level: 1},
			),
			AtUse: true,
		},
		node.Declared{
			Prefix: "nest.sub",
			Section: settings.Of(
				"nest.sub",
				settings.At(entity.ScopeInstallation),
				walkTuning{Level: 1},
			),
			AtUse: true,
		},
		node.Declared{
			Prefix: "alphabet",
			Section: settings.Of(
				"alphabet",
				settings.At(entity.ScopeInstallation),
				walkTuning{Level: 1},
			),
			AtUse: true,
		},
	)
}

// walkAt writes section's level at one scope over the row's own generation,
// as a settings write through the store does.
func walkAt(
	t *testing.T,
	h *nodetest.Harness,
	scope entity.SettingScope,
	section string,
	level int,
) {
	t.Helper()

	key := section + ".level"

	var expect uint64

	for s, err := range h.Store().ControlPlane().Settings(t.Context(), scope) {
		if err != nil {
			t.Fatalf("read %s at %v: %v", key, scope.Kind, err)
		}

		if s.Key == key {
			expect = s.Generation
		}
	}

	s := entity.Setting{
		Scope: scope,
		Key:   key,
		Value: []byte(strconv.Itoa(level)),
	}
	if _, err := h.Store().ControlPlane().PutSetting(t.Context(), s, expect); err != nil {
		t.Fatalf("put %s at %v: %v", key, scope.Kind, err)
	}
}

// putRowGen writes one settings row over the row's own generation, as a
// settings write through the store does, and returns the generation the
// write raised.
func putRowGen(
	t *testing.T,
	h *nodetest.Harness,
	scope entity.SettingScope,
	key string,
	value []byte,
) uint64 {
	t.Helper()

	var expect uint64

	for s, err := range h.Store().ControlPlane().Settings(t.Context(), scope) {
		if err != nil {
			t.Fatalf("read %s: %v", key, err)
		}

		if s.Key == key {
			expect = s.Generation
		}
	}

	s := entity.Setting{Scope: scope, Key: key, Value: value}

	gen, err := h.Store().ControlPlane().PutSetting(t.Context(), s, expect)
	if err != nil {
		t.Fatalf("put %s: %v", key, err)
	}

	return gen
}

// walkKey is the raw row key of one setting, as the store lays it out.
func walkKey(scope entity.SettingScope, key string) []byte {
	k := []byte{keys.KindCFG, byte(scope.Kind)}
	if scope.Kind != entity.ScopeInstallation {
		k = keys.ID(k, scope.ID[:])
	}

	return keys.String(k, []byte(key))
}

// babble replaces one settings row with bytes that do not decode, straight
// under the row key, as a store's own corruption would: no check, no
// generation.
func babble(t *testing.T, k kv.KV, scope entity.SettingScope, key string) {
	t.Helper()

	if err := k.Update(t.Context(), func(tx kv.Txn) error {
		return tx.Set(walkKey(scope, key), []byte{0xff})
	}); err != nil {
		t.Fatalf("corrupt %s: %v", key, err)
	}
}

// erase removes one settings row straight under its key, as a store's own
// corruption would: no check, no generation.
func erase(t *testing.T, k kv.KV, scope entity.SettingScope, key string) {
	t.Helper()

	if err := k.Update(t.Context(), func(tx kv.Txn) error {
		return tx.Delete(walkKey(scope, key))
	}); err != nil {
		t.Fatalf("erase %s: %v", key, err)
	}
}

// runLevel is section's level in the snapshot the node published, zero with
// the error of a section that holds no value.
func runLevel(
	t *testing.T,
	h *nodetest.Harness,
	snap *settings.Snapshot,
	section string,
) (int, error) {
	t.Helper()

	v, err := settings.Get[walkTuning](snap, section, h.NodeID())

	return v.Level, err
}

// appliedOf is one installation setting's applied count, as the management
// API reports it, over the node's API served to the admin.
func appliedOf(t *testing.T, h *nodetest.Harness, key string) apiv1.Applied {
	t.Helper()

	return appliedAtOf(t, h, "/api/v1/settings/"+key)
}

// appliedAtOf is one setting's applied count at the path of its scope, as
// the management API reports it, over the node's API served to the admin.
func appliedAtOf(t *testing.T, h *nodetest.Harness, path string) apiv1.Applied {
	t.Helper()

	addr, _ := h.Node().Addrs()

	var s apiv1.Setting

	getJSON(t, addr, path, &s)

	if s.Status.Applied == nil {
		t.Fatalf("GET %s: no applied count in %+v", path, s)
	}

	return *s.Status.Applied
}

// servedFaulted is faulted with the node's API answering its caller as the
// admin, so a test reads a setting's applied count through it.
func servedFaulted(t *testing.T, preps ...func(*node.Config)) (*nodetest.Harness, *kvtest.FaultKV) {
	var f *kvtest.FaultKV

	h := nodetest.New(
		t,
		nodetest.Watch(),
		nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
			f = kvtest.NewFaultKV(cfg.KV)

			cfg.KV = f

			for _, p := range preps {
				p(&cfg)
			}

			return node.NewAuthenticating(admin)(cfg)
		}),
	)

	return h, f
}

// cfgScan is the settings range, as a FaultKV rule names it: the one scan
// prefix the walk's pages read.
var cfgScan = keys.Describe([]byte{keys.KindCFG})

// The settings walk reads every scope's settings in pages and the counters
// before its first page and after its last: a write landing after the pass
// read its counters, while the walk's second page holds, makes the walk start
// again, so the snapshot never holds two states of the store. The pass reports
// the generation it read, not the newer one its second walk saw, and the next
// poll reports the new generation.
func TestSettingsWalkRepeatedOnAMovedGeneration(t *testing.T) {
	h, f := faulted(t)
	declareSections(t, h)
	ch := watchStatus(h)
	started(t, h)

	// More rows than a page holds: the walk reads the first walkPage in one
	// page and the rest in a second one, held below.
	padRows(t, h, walkPage+walkPage/8)

	caughtUp(t, h)

	gen0 := h.Node().Positions().Settings

	drain(ch)

	held := sim.NewTestClock(time.Unix(0, 0))

	setFault(t, f, held, sim.Rule{
		Op:    kvtest.OpScan,
		Key:   cfgScan,
		From:  2,
		Count: 1,
		Action: sim.FaultAction{
			Delay: time.Hour,
		},
	})
	defer held.Advance(time.Hour) // a failed test's Stop finds no scan held

	// The pass reads this change in its counters, before its walk.
	walkAt(t, h, entity.SettingScope{Kind: entity.ScopeInstallation}, "beta", 6)
	h.Clock().Advance(time.Second)
	wait(t, "the walk held in its second page", func() bool { return held.Pending() == 1 })
	// This change lands between the walk's first page and its held second.
	walkAt(t, h, entity.SettingScope{Kind: entity.ScopeNode, ID: h.NodeID()}, "alpha", 5)
	moved := h.Node().Moved()

	held.Advance(time.Hour) // the second page reads the newer state

	select {
	case <-moved:
	case <-time.After(10 * time.Second):
		t.Fatal("the held walk did not end within 10 s")
	}

	// The pass read beta's change, one generation up, and reports no more:
	// alpha's, which the walk's second run saw, waits for the next poll.
	if got := h.Node().Positions(); got.Settings != gen0+1 {
		t.Fatalf(
			"after a walk a write landed in: settings position %d, want %d, the generation the pass read",
			got.Settings,
			gen0+1,
		)
	}

	if got, err := runLevel(t, h, h.Node().Settings(), "alpha"); err != nil || got != 5 {
		t.Errorf(
			"alpha after the walk started again: level %d, %v; want 5, the state its second run read",
			got,
			err,
		)
	}

	f.SetFault(nil)

	caughtUp(t, h)

	now := counters(t, h)
	if got := h.Node().Positions(); got.Settings != now.Settings {
		t.Fatalf(
			"after the next poll: settings position %d, want %d, the new generation",
			got.Settings,
			now.Settings,
		)
	}

	drain(ch)

	tr := turnAt(t, h, ch, "holding the new settings generation", func(tr statusTurn) bool {
		return tr.s.Settings == now.Settings
	})
	if !tr.wrote {
		t.Fatalf("the status turn holding the new settings generation wrote nothing")
	}
}

// One undecodable row refuses its section only: the rest of the walk
// publishes, the refused section keeps the value it last ran, and the
// refusal names the row it read, at the generation the pass walked.
func TestSettingsRefusalStopsOnlyItsSection(t *testing.T) {
	h, f := faulted(t)
	declareSections(t, h)
	armed := armWaits(h)
	started(t, h)
	walkAt(t, h, entity.SettingScope{Kind: entity.ScopeNode, ID: h.NodeID()}, "alpha", 3)
	caughtUp(t, h)
	waitsSettled(t, armed)

	// One undecodable row in alpha, beside a live change in beta, in one
	// pass.
	babble(t, f, entity.SettingScope{Kind: entity.ScopeNode, ID: h.NodeID()}, "alpha.level")
	walkAt(t, h, entity.SettingScope{Kind: entity.ScopeInstallation}, "beta", 7)
	walkTurn(t, h, armed)

	now := counters(t, h)

	got, err := runLevel(t, h, h.Node().Settings(), "alpha")
	if err != nil || got != 3 {
		t.Errorf(
			"alpha after its row refused to decode: level %d, %v; want 3, its last value, no error",
			got,
			err,
		)
	}

	got, err = runLevel(t, h, h.Node().Settings(), "beta")
	if err != nil || got != 7 {
		t.Errorf(
			"beta after the same pass: level %d, %v; want 7, its new value, no error",
			got,
			err,
		)
	}

	refused := h.Node().Refused()

	want := entity.SettingRef(
		entity.SettingScope{Kind: entity.ScopeNode, ID: h.NodeID()},
		"alpha.level",
	)
	if len(refused) != 1 || refused[0].Record.Kind != want.Kind ||
		string(refused[0].Record.ID) != string(want.ID) {
		t.Fatalf("refused records: got %s, want alpha's row alone, %s", describe(refused), want.ID)
	}

	if refused[0].Generation != now.Settings {
		t.Errorf(
			"the refusal's generation: got %d, want %d, the generation the pass read",
			refused[0].Generation,
			now.Settings,
		)
	}

	var p apiv1.Problem

	if err := json.Unmarshal(refused[0].Error, &p); err != nil {
		t.Fatalf("the refusal's error %s: %v", refused[0].Error, err)
	}

	if p.Status != http.StatusUnprocessableEntity ||
		!strings.Contains(p.Detail, "does not decode") {
		t.Errorf(
			"the refusal's problem: got %d %q, want 422 naming the row that does not decode",
			p.Status,
			p.Detail,
		)
	}
}

// A section refused before it ever ran holds its component unstarted: the
// snapshot holds no value of it, never its default, and the row is reported
// refused, while a valid section beside it runs.
func TestSettingsRefusedAtStartNeverStarts(t *testing.T) {
	h, f := servedFaulted(t)
	declareSections(t, h)

	turns := watchStatus(h)

	// One undecodable row in alpha, and one its Validate refuses, before
	// the first walk.
	walkAt(t, h, entity.SettingScope{Kind: entity.ScopeNode, ID: h.NodeID()}, "alpha", 3)
	babble(t, f, entity.SettingScope{Kind: entity.ScopeNode, ID: h.NodeID()}, "alpha.level")
	walkAt(
		t,
		h,
		entity.SettingScope{Kind: entity.ScopeInstallation},
		"eps",
		150,
	) // its Validate refuses
	walkAt(t, h, entity.SettingScope{Kind: entity.ScopeInstallation}, "beta", 9)

	started(t, h)
	caughtUp(t, h)
	// The applied counts read the stored status, which its own turn writes.
	for c := counters(t, h); nextTurn(t, h, turns).s.Settings < c.Settings; {
	}

	if got, err := runLevel(t, h, h.Node().Settings(), "alpha"); err == nil {
		t.Errorf("alpha never ran: level %d, %v; want no value, its row refused", got, err)
	}

	if got, err := runLevel(t, h, h.Node().Settings(), "eps"); err == nil {
		t.Errorf("eps never ran: level %d, %v; want no value, its validator refused it", got, err)
	}

	got, err := runLevel(t, h, h.Node().Settings(), "beta")
	if err != nil || got != 9 {
		t.Errorf("beta, valid beside the refused: level %d, %v; want 9, no error", got, err)
	}

	if got := appliedOf(t, h, "eps.level"); got != (apiv1.Applied{Nodes: 0, Of: 1}) {
		t.Errorf(
			"eps.level applied: got %d of %d, want 0 of 1, refused at the generation it reconciled",
			got.Nodes,
			got.Of,
		)
	}

	if got := appliedOf(t, h, "beta.level"); got != (apiv1.Applied{Nodes: 1, Of: 1}) {
		t.Errorf(
			"beta.level applied: got %d of %d, want 1 of 1, applied on this node alone",
			got.Nodes,
			got.Of,
		)
	}

	now := counters(t, h)
	if got := readStatus(t, h).Settings; got != now.Settings {
		t.Errorf(
			"the node's status read back: settings %d, want %d, the refused generation reconciled",
			got,
			now.Settings,
		)
	}

	refused := h.Node().Refused()

	if len(refused) != 2 {
		t.Fatalf("refused records: got %s, want alpha's and eps's rows", describe(refused))
	}

	for _, r := range refused {
		if r.Generation != now.Settings {
			t.Errorf(
				"the refusal of %s: generation %d, want %d, the generation the first walk read",
				r.Record.ID,
				r.Generation,
				now.Settings,
			)
		}
	}
}

// A section refused before it ever ran never runs its component: the apply
// of a section whose row is undecodable at start is never called, with its
// defaults or with no value, while a valid section's apply beside it runs
// its row's value.
func TestRefusedAtStartNeverRunsItsComponent(t *testing.T) {
	h, f := servedFaulted(t)
	declareSections(t, h)

	var (
		mu     sync.Mutex
		fresh  int
		steady []int
	)

	count := func(prefix string, into func(int)) func(*settings.Snapshot) error {
		return func(snap *settings.Snapshot) error {
			v, err := settings.Get[walkTuning](snap, prefix, h.NodeID())
			if err != nil {
				return err
			}

			mu.Lock()
			defer mu.Unlock()

			into(v.Level)

			return nil
		}
	}

	h.Node().DeclareSections(
		node.Declared{
			Prefix:  "fresh",
			Section: settings.Of("fresh", settings.At(entity.ScopeInstallation), walkTuning{Level: 1}),
			Apply: func(*settings.Snapshot) error {
				mu.Lock()
				defer mu.Unlock()

				fresh++

				return nil
			},
		},
		node.Declared{
			Prefix:  "steady",
			Section: settings.Of("steady", settings.At(entity.ScopeInstallation), walkTuning{Level: 1}),
			Apply:   count("steady", func(l int) { steady = append(steady, l) }),
		},
	)

	installation := entity.SettingScope{Kind: entity.ScopeInstallation}
	walkAt(t, h, installation, "steady", 7)
	babble(t, f, installation, "fresh.level")

	started(t, h)
	caughtUp(t, h)

	mu.Lock()
	gotFresh, gotSteady := fresh, slices.Clone(steady)
	mu.Unlock()

	if gotFresh != 0 {
		t.Errorf("fresh's apply, its row refused before it ever ran: %d calls, want 0", gotFresh)
	}

	if len(gotSteady) == 0 || gotSteady[len(gotSteady)-1] != 7 {
		t.Errorf("steady's apply beside it: levels %v, want the last at 7, its row's value", gotSteady)
	}

	want := entity.SettingRef(installation, "fresh.level")
	refused := h.Node().Refused()

	if len(refused) != 1 || !hasRef([]entity.ResourceRef{refused[0].Record}, want) {
		t.Errorf("refused records: got %s, want fresh's row alone", describe(refused))
	}
}

// A refusal in one share of a per-share section refuses only that share's
// rows and keeps only that share's last value: the other share runs its new
// value, and neither its rows nor the kept one appear refused.
func TestSettingsInstanceRefusalNamesOnlyItsInstance(t *testing.T) {
	h, f := faulted(t)
	declareSections(t, h)
	armed := armWaits(h)
	started(t, h)
	configure(t, h, storeA)
	shares := createShares(t, h, storeA, "one", "two")
	one, two := shares[0], shares[1]
	walkAt(t, h, entity.SettingScope{Kind: entity.ScopeShare, ID: one.ID}, "gamma", 1)
	walkAt(t, h, entity.SettingScope{Kind: entity.ScopeShare, ID: two.ID}, "gamma", 55)
	caughtUp(t, h)

	waitsSettled(t, armed)

	prev := h.Node().Settings()

	// Share two's row refuses to decode; share one's row changes; one pass.
	babble(t, f, entity.SettingScope{Kind: entity.ScopeShare, ID: two.ID}, "gamma.level")
	walkAt(t, h, entity.SettingScope{Kind: entity.ScopeShare, ID: one.ID}, "gamma", 2)
	walkTurn(t, h, armed)
	now := counters(t, h)

	refused := h.Node().Refused()

	want := entity.SettingRef(
		entity.SettingScope{Kind: entity.ScopeShare, ID: two.ID},
		"gamma.level",
	)
	if len(refused) != 1 || refused[0].Record.Kind != want.Kind ||
		string(refused[0].Record.ID) != string(want.ID) {
		t.Fatalf(
			"refused records: got %s, want share two's row alone: %s",
			describe(refused),
			describe([]entity.RefusedRecord{{Record: want}}),
		)
	}

	if refused[0].Generation != now.Settings {
		t.Errorf(
			"the refusal's generation: got %d, want %d, the generation the pass read",
			refused[0].Generation,
			now.Settings,
		)
	}

	// Share one runs its new value, so the diff of the two snapshots lists
	// its change alone; share two kept its last value, so it lists nothing
	// at it in either direction.
	cur := h.Node().Settings()
	forward := settings.Diff(prev, cur)
	backward := settings.Diff(cur, prev)

	change := settings.Change{
		Section: "gamma",
		Key:     "gamma.level",
		Class:   settings.Live,
		Scope:   entity.SettingScope{Kind: entity.ScopeShare, ID: one.ID},
	}
	for _, diff := range []struct {
		what string
		got  []settings.Change
	}{
		{"the value published", forward},
		{"the value kept", backward},
	} {
		if !slices.Equal(diff.got, []settings.Change{change}) {
			t.Errorf(
				"%s: Diff = %+v, want the change of share one alone, %+v",
				diff.what,
				diff.got,
				change,
			)
		}
	}
}

// A settings read that fails midway is a failed read, not a walk: it moves
// no position and reports no refusal, and the next pass runs full, so a
// change written before the failure is found at the next read that works.
func TestSettingsChangeMissedDuringAnOutage(t *testing.T) {
	h, f := faulted(t)
	declareSections(t, h)
	armed := armWaits(h)
	started(t, h)
	caughtUp(t, h)

	waitsSettled(t, armed)

	gen0 := h.Node().Positions().Settings

	setFault(t, f, nil, sim.Rule{
		Op:    kvtest.OpGet,
		Key:   cfgGen,
		From:  2,
		Count: 1,
		Action: sim.FaultAction{
			Err: errors.New("the counters read was lost"),
		},
	})
	// The pass's own counters read comes; the walk's re-read, the rule's
	// second get, fails.
	walkAt(t, h, entity.SettingScope{Kind: entity.ScopeNode, ID: h.NodeID()}, "alpha", 42)

	walkTurn(t, h, armed)

	if got := h.Node().Positions().Settings; got != gen0 {
		t.Fatalf("after a failed settings read: settings position %d, want %d, unmoved", got, gen0)
	}

	if got := len(h.Node().Refused()); got != 0 {
		t.Fatalf(
			"refused records after a failed read: got %s, want none, a failed read is no refusal",
			describe(h.Node().Refused()),
		)
	}

	if got, err := runLevel(t, h, h.Node().Settings(), "alpha"); err != nil || got != 1 {
		t.Errorf(
			"alpha, its change written before the failure: level %d, %v; want 1, what it last ran",
			got,
			err,
		)
	}

	f.SetFault(nil)

	walkTurn(t, h, armed)

	now := counters(t, h)
	if got := h.Node().Positions().Settings; got != now.Settings {
		t.Fatalf(
			"after the read healed: settings position %d, want %d, the missed change applied",
			got,
			now.Settings,
		)
	}

	if got, err := runLevel(t, h, h.Node().Settings(), "alpha"); err != nil || got != 42 {
		t.Errorf(
			"alpha after the read healed: level %d, %v; want 42, the change written in the failure",
			got,
			err,
		)
	}
}

// Each walk publishes one snapshot, and the reader a component is loads it
// whole: over fifty passes, between a value change, another and a row that
// refuses to decode, the reader always gets the section's live value, never
// an error, and the value it reads right after each pass is the one that
// pass published.
func TestSettingsSnapshotLoadedWhilePassesRun(t *testing.T) {
	h, f := faulted(t)
	declareSections(t, h)
	armed := armWaits(h)
	started(t, h)
	caughtUp(t, h)
	waitsSettled(t, armed)

	stop := make(chan struct{})

	var mu sync.Mutex

	var (
		seen   []error
		reader sync.WaitGroup
	)

	reader.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}

			_, err := settings.Get[walkTuning](h.Node().Settings(), "alpha", h.NodeID())
			if err != nil {
				mu.Lock()

				seen = append(seen, err)

				mu.Unlock()
			}
		}
	})

	// The reader is joined before the test ends, so it never reads the node
	// after the harness stops it.
	t.Cleanup(func() {
		close(stop)
		reader.Wait()
	})

	// A pass writes nothing else between its walk's Keep loop of a refused
	// section and the one Store that publishes, so a reader never loads a
	// snapshot between the two.
	want := []int{1, 2, 2, 1}

	for i := range 50 {
		node := entity.SettingScope{Kind: entity.ScopeNode, ID: h.NodeID()}
		installation := entity.SettingScope{Kind: entity.ScopeInstallation}

		switch i % 4 {
		case 0:
			walkAt(t, h, node, "alpha", 1)
		case 1:
			walkAt(t, h, node, "alpha", 2)
		case 2:
			babble(t, f, node, "alpha.level")
			walkAt(t, h, installation, "beta", i)
		case 3:
			erase(t, f, node, "alpha.level")
			walkAt(t, h, installation, "beta", i)
		}

		walkTurn(t, h, armed)

		got, err := runLevel(t, h, h.Node().Settings(), "alpha")
		if err != nil || got != want[i%4] {
			t.Fatalf(
				"pass %d: alpha's level %d, %v; want %d, the value the pass published",
				i,
				got,
				err,
				want[i%4],
			)
		}

		if refused := len(h.Node().Refused()); i%4 == 2 && refused != 1 ||
			i%4 != 2 && refused != 0 {
			t.Fatalf(
				"pass %d: %d refused records, want alpha's row only when it does not decode",
				i,
				refused,
			)
		}
	}

	mu.Lock()
	defer mu.Unlock()

	if len(seen) != 0 {
		t.Fatalf("the reader at use saw %d Get errors, want none, e.g. %v", len(seen), seen[0])
	}
}

// pairAt writes the delta pair at scope, one row per key over the rows'
// own generations, and answers each row's new generation.
func pairAt(
	t *testing.T,
	h *nodetest.Harness,
	scope entity.SettingScope,
	a, b int,
) map[string]uint64 {
	t.Helper()

	gens := map[string]uint64{}

	for key, value := range map[string]int{"delta.a": a, "delta.b": b} {
		var expect uint64

		for row, err := range h.Store().ControlPlane().Settings(t.Context(), scope) {
			if err != nil {
				t.Fatalf("read %s: %v", key, err)
			}

			if row.Key == key {
				expect = row.Generation
			}
		}

		row := entity.Setting{Scope: scope, Key: key, Value: []byte(strconv.Itoa(value))}

		gen, err := h.Store().ControlPlane().PutSetting(t.Context(), row, expect)
		if err != nil {
			t.Fatalf("put %s: %v", key, err)
		}

		gens[key] = gen
	}

	return gens
}

// bRowAt writes delta.b at scope over generation expect, held from before
// its scope's rows refuse to decode, so their generations can be read no
// more.
func bRowAt(t *testing.T, h *nodetest.Harness, scope entity.SettingScope, b int, expect uint64) {
	t.Helper()

	row := entity.Setting{Scope: scope, Key: "delta.b", Value: []byte(strconv.Itoa(b))}

	if _, err := h.Store().ControlPlane().PutSetting(t.Context(), row, expect); err != nil {
		t.Fatalf("put delta.b: %v", err)
	}
}

// hasRef reports whether refs holds the record ref names.
func hasRef(refs []entity.ResourceRef, ref entity.ResourceRef) bool {
	return slices.ContainsFunc(refs, func(got entity.ResourceRef) bool {
		return got.Kind == ref.Kind && bytes.Equal(got.ID, ref.ID)
	})
}

// A refusal of one key holds the instance whole, so every row the walk read
// of that instance is in the refused set, its other key's change named too
// and read unapplied, while a fine share of the same section stays out (RFC
// 23 section 2.4: a record is applied once off both refused and pending sets).
func TestSettingsRefusalNamesEveryRowOfItsInstance(t *testing.T) {
	h, f := servedFaulted(t)
	declareSections(t, h)
	armed := armWaits(h)
	started(t, h)
	configure(t, h, storeA)
	one := createShare(t, h, "one", storeA)
	two := createShare(t, h, "two", storeA)

	oneScope := entity.SettingScope{Kind: entity.ScopeShare, ID: one.ID}
	twoScope := entity.SettingScope{Kind: entity.ScopeShare, ID: two.ID}
	gens := pairAt(t, h, oneScope, 1, 1)

	pairAt(t, h, twoScope, 1, 1)

	caughtUp(t, h)

	waitsSettled(t, armed)

	// Share one's a row refuses to decode while its b row changes to 2; the
	// instance whole keeps the values it ran.
	babble(t, f, oneScope, "delta.a")
	bRowAt(t, h, oneScope, 2, gens["delta.b"])

	walkTurn(t, h, armed)

	refused := h.Node().Refused()

	want := []entity.ResourceRef{
		entity.SettingRef(oneScope, "delta.a"),
		entity.SettingRef(oneScope, "delta.b"),
	}

	var got []entity.ResourceRef

	for _, r := range refused {
		got = append(got, r.Record)
	}

	if len(got) != 2 || !hasRef(got, want[0]) || !hasRef(got, want[1]) {
		t.Fatalf(
			"refused records: got %s, want delta.a and delta.b of share one, held whole",
			describe(refused),
		)
	}

	// The held change reads applied on no node, its row refused while the
	// position covers it (RFC 23 section 2.4: applied once in neither the
	// refused set nor the pending one); the API's own count cannot say it,
	// as its listing of the scope dies on the sibling row that does not
	// decode. The fine share's own row stays out of the refused set and
	// reads applied here, alone.
	twoPath := "/api/v1/shares/" + entity.ShareID(two.ID).String() + "/settings/delta.b"

	if a := appliedAtOf(t, h, twoPath); a != (apiv1.Applied{Nodes: 1, Of: 1}) {
		t.Errorf(
			"delta.b of share two applied: got %d of %d, want 1 of 1, its instance fine",
			a.Nodes,
			a.Of,
		)
	}
}

// A refusal of a node's override row holds the section whole, so the wide
// row that changed in the same pass is named refused as well and reads
// unapplied, while the node keeps running the value it last ran.
func TestSettingsOverrideRefusalNamesTheWideRowToo(t *testing.T) {
	h, f := servedFaulted(t)
	declareSections(t, h)
	armed := armWaits(h)
	started(t, h)

	node := entity.SettingScope{Kind: entity.ScopeNode, ID: h.NodeID()}
	installation := entity.SettingScope{Kind: entity.ScopeInstallation}
	walkAt(t, h, installation, "alpha", 8)
	walkAt(t, h, node, "alpha", 9)
	caughtUp(t, h)

	waitsSettled(t, armed)

	// The node's row refuses to decode while the installation default
	// changes to 7: the section whole keeps the 9 it ran.
	babble(t, f, node, "alpha.level")
	walkAt(t, h, installation, "alpha", 7)
	walkTurn(t, h, armed)

	got, err := runLevel(t, h, h.Node().Settings(), "alpha")

	if err != nil || got != 9 {
		t.Errorf(
			"alpha after its node row refused: level %d, %v; want 9, what it last ran",
			got,
			err,
		)
	}

	refused := h.Node().Refused()

	want := []entity.ResourceRef{
		entity.SettingRef(node, "alpha.level"),
		entity.SettingRef(installation, "alpha.level"),
	}

	var gotRefs []entity.ResourceRef

	for _, r := range refused {
		gotRefs = append(gotRefs, r.Record)
	}

	if len(gotRefs) != 2 || !hasRef(gotRefs, want[0]) || !hasRef(gotRefs, want[1]) {
		t.Fatalf(
			"refused records: got %s, want the node row and the wide row, the section held whole",
			describe(refused),
		)
	}

	if a := appliedOf(t, h, "alpha.level"); a != (apiv1.Applied{Nodes: 0, Of: 1}) {
		t.Errorf(
			"alpha.level at installation applied: got %d of %d, want 0 of 1, held back",
			a.Nodes,
			a.Of,
		)
	}
}

// A section nested under another's prefix belongs to itself: its rows are
// never named with the parent section's refusal, so a nested section that
// runs its new value in the same pass stays out of the refused set, beside
// the corrupt parent row alone.
func TestSettingsRefusalNeverNamesANestedSection(t *testing.T) {
	h, f := servedFaulted(t)
	declareSections(t, h)
	armed := armWaits(h)
	ch := watchStatus(h)
	started(t, h)

	installation := entity.SettingScope{Kind: entity.ScopeInstallation}
	walkAt(t, h, installation, "nest", 3)
	walkAt(t, h, installation, "nest.sub", 4)
	caughtUp(t, h)

	waitsSettled(t, armed)

	// The nested section changes to 7 and the parent's row refuses to
	// decode, rows of one pass: the nested section's own row stays out.
	walkAt(t, h, installation, "nest.sub", 7)
	babble(t, f, installation, "nest.level")
	walkTurn(t, h, armed)
	now := counters(t, h)
	turnAt(t, h, ch, "the status holding the new generation", func(tr statusTurn) bool {
		return tr.s.Settings == now.Settings
	})

	got, err := runLevel(t, h, h.Node().Settings(), "nest")

	if err != nil || got != 3 {
		t.Errorf("nest after its row refused: level %d, %v; want 3, what it last ran", got, err)
	}

	got, err = runLevel(t, h, h.Node().Settings(), "nest.sub")

	if err != nil || got != 7 {
		t.Errorf(
			"nest.sub after the same pass: level %d, %v; want 7, its new value, no error",
			got,
			err,
		)
	}

	refused := h.Node().Refused()
	want := entity.SettingRef(installation, "nest.level")

	if len(refused) != 1 || !hasRef([]entity.ResourceRef{refused[0].Record}, want) {
		t.Fatalf(
			"refused records: got %s, want nest.level alone, the section nested under it fine",
			describe(refused),
		)
	}

	// The nested section's row stays out of the refused set while its
	// position is covered, which is the API's own condition for applied; the
	// API's count itself cannot say it, as its listing of the installation
	// scope dies on the corrupt sibling row of the parent section.
}

// A section whose prefix holds another's in it, alphabet under alpha, is its
// own: its rows are never named with the other's refusal.
func TestSettingsRefusalNamesNoLookAlikeSection(t *testing.T) {
	h, f := servedFaulted(t)
	declareSections(t, h)
	armed := armWaits(h)
	ch := watchStatus(h)
	started(t, h)
	node := entity.SettingScope{Kind: entity.ScopeNode, ID: h.NodeID()}
	installation := entity.SettingScope{Kind: entity.ScopeInstallation}
	walkAt(t, h, installation, "alphabet", 2)
	caughtUp(t, h)

	waitsSettled(t, armed)

	// The section changes to 5 and the look-alike parent's row refuses to
	// decode, rows of one pass.
	walkAt(t, h, installation, "alphabet", 5)
	babble(t, f, node, "alpha.level")
	walkTurn(t, h, armed)
	now := counters(t, h)
	turnAt(t, h, ch, "the status holding the new generation", func(tr statusTurn) bool {
		return tr.s.Settings == now.Settings
	})

	got, err := runLevel(t, h, h.Node().Settings(), "alphabet")

	if err != nil || got != 5 {
		t.Errorf("alphabet after the same pass: level %d, %v; want 5, its new value", got, err)
	}

	refused := h.Node().Refused()
	want := entity.SettingRef(node, "alpha.level")

	if len(refused) != 1 || !hasRef([]entity.ResourceRef{refused[0].Record}, want) {
		t.Fatalf(
			"refused records: got %s, want alpha.level alone, the look-alike section fine",
			describe(refused),
		)
	}

	if a := appliedOf(t, h, "alphabet.level"); a != (apiv1.Applied{Nodes: 1, Of: 1}) {
		t.Errorf(
			"alphabet.level applied: got %d of %d, want 1 of 1, the look-alike section fine",
			a.Nodes,
			a.Of,
		)
	}
}

// A live and a bound key of one section changed in one pass apply once, and
// the bound key alone applies again: the apply is one call a pass for a
// changed section, however many of its keys changed. The component holds
// both values, and the node's status reports the settings generation the
// change reconciled through.
func TestLiveChangeRunsOnTheFirstPass(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	ch := watchStatus(h)
	armed := armWaits(h)

	var (
		mu    sync.Mutex
		calls int
		held  boundTuning
	)

	h.Node().DeclareSections(node.Declared{
		Prefix: "tuned",
		Section: settings.Of(
			"tuned",
			settings.At(entity.ScopeInstallation),
			boundTuning{Level: 1, Floor: 1},
		),
		Apply: func(snap *settings.Snapshot) error {
			v, err := settings.Get[boundTuning](snap, "tuned", h.NodeID())
			if err != nil {
				return err
			}

			mu.Lock()
			defer mu.Unlock()

			calls++
			held = v

			return nil
		},
	})
	started(t, h)
	caughtUp(t, h)
	waitsSettled(t, armed)

	mu.Lock()
	ranAt := calls
	mu.Unlock()

	if ranAt != 1 {
		t.Fatalf(
			"the apply over the start: called %d times, want 1, the first walk's run",
			ranAt,
		)
	}

	installation := entity.SettingScope{Kind: entity.ScopeInstallation}
	putRowGen(t, h, installation, "tuned.level", []byte("6"))
	gen := putRowGen(t, h, installation, "tuned.floor", []byte("7"))
	walkTurn(t, h, armed)

	mu.Lock()
	running := calls
	mu.Unlock()

	if running != ranAt+1 {
		t.Fatalf(
			"one pass with a live and a bound key changed: apply called %d more times, want 1",
			running-ranAt,
		)
	}

	mu.Lock()
	level, floor := held.Level, held.Floor
	mu.Unlock()

	if level != 6 || floor != 7 {
		t.Errorf(
			"the component after the pass: level %d and floor %d, want 6 and 7, both applied",
			level,
			floor,
		)
	}

	// The bound key alone changes: its section's apply runs once again.
	// caughtUp, not one walkTurn, waits for the pass that covers the
	// write, so a pass the earlier status turns' advances left in flight
	// cannot take this write's generation and apply nothing to it.
	gen = putRowGen(t, h, installation, "tuned.floor", []byte("8"))
	caughtUp(t, h)

	mu.Lock()
	running, level, floor = calls, held.Level, held.Floor
	mu.Unlock()

	if running != ranAt+2 {
		t.Fatalf(
			"the bound key alone changed: apply called %d more times in all, want %d",
			running-ranAt,
			2,
		)
	}

	if level != 6 || floor != 8 {
		t.Errorf(
			"the component after the bound key alone: level %d and floor %d, want 6 and 8",
			level,
			floor,
		)
	}

	drain(ch)
	turnAt(t, h, ch, "the status holding the change's generation", func(tr statusTurn) bool {
		return tr.s.Settings == gen
	})
}

// A restart-class change applies nothing: its record sits in the node's
// restart-pending set, on its status, until the value its section first
// runs is back, and its component runs nothing in the meantime.
func TestRestartChangeIsPendingNotApplied(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	ch := watchStatus(h)
	armed := armWaits(h)

	var (
		mu    sync.Mutex
		calls int
	)

	h.Node().DeclareSections(node.Declared{
		Prefix:  "boots",
		Section: settings.Of("boots", settings.At(entity.ScopeInstallation), bootsTuning{Halt: 9}),
		Apply: func(*settings.Snapshot) error {
			mu.Lock()
			defer mu.Unlock()

			calls++

			return nil
		},
	})
	started(t, h)
	caughtUp(t, h)
	waitsSettled(t, armed)

	installation := entity.SettingScope{Kind: entity.ScopeInstallation}
	gen := putRowGen(t, h, installation, "boots.halt", []byte("5"))
	walkTurn(t, h, armed)

	want := entity.SettingRef(installation, "boots.halt")

	turnAt(t, h, ch, "the status holding the restart-pending record", func(tr statusTurn) bool {
		return tr.s.Settings == gen && len(tr.s.RestartPending) == 1 &&
			hasRef(tr.s.RestartPending, want)
	})

	if got := series(t, h, "dittofs_node_restart_pending", ""); got != 1 {
		t.Errorf("restart pending series with boots.halt pending: got %v, want 1", got)
	}

	mu.Lock()
	got := calls
	mu.Unlock()

	if got != 0 {
		t.Fatalf(
			"the apply over a restart-class change: %d calls, want 0, nothing to run",
			got,
		)
	}

	if v, err := settings.Get[bootsTuning](h.Node().Settings(), "boots", h.NodeID()); err != nil ||
		v.Halt != 5 {
		t.Errorf(
			"boots.halt after the change: got %d, %v; want 5, published for the restart",
			v.Halt,
			err,
		)
	}

	// The value back at what the section first runs leaves the set empty.
	waitsSettled(t, armed)
	putRowGen(t, h, installation, "boots.halt", []byte("9"))
	walkTurn(t, h, armed)
	turnAt(t, h, ch, "the status holding no restart-pending record", func(tr statusTurn) bool {
		return len(tr.s.RestartPending) == 0
	})

	if got := series(t, h, "dittofs_node_restart_pending", ""); got != 0 {
		t.Errorf("restart pending series once boots.halt is back: got %v, want 0", got)
	}
}

// A next-write change is published and applies nothing: the writer that
// reads it at its next write sees the new value, and the component's apply
// is never called for it.
func TestNextWriteChangeReadByTheNextWrite(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	armed := armWaits(h)

	var (
		mu    sync.Mutex
		calls int
	)

	h.Node().DeclareSections(node.Declared{
		Prefix:  "later",
		Section: settings.Of("later", settings.At(entity.ScopeInstallation), nextTuning{After: 1}),
		Apply: func(*settings.Snapshot) error {
			mu.Lock()
			defer mu.Unlock()

			calls++

			return nil
		},
	})
	started(t, h)
	caughtUp(t, h)
	waitsSettled(t, armed)

	putRowGen(
		t,
		h,
		entity.SettingScope{Kind: entity.ScopeInstallation},
		"later.after",
		[]byte("7"),
	)
	walkTurn(t, h, armed)

	mu.Lock()
	got := calls
	mu.Unlock()

	if got != 0 {
		t.Fatalf(
			"the apply after a next-write key changed: %d calls, want 0, nothing to run",
			got,
		)
	}

	if v, err := settings.Get[nextTuning](h.Node().Settings(), "later", h.NodeID()); err != nil ||
		v.After != 7 {
		t.Errorf(
			"later.after as the next write reads it: got %d, %v; want 7, published",
			v.After,
			err,
		)
	}
}

// A live section the node holds no way to is refused on change and never
// reported applied: no component exists for it, so no report claims one ran
// the change. An api change in the same pass, read at use, applies.
func TestLiveSectionWithNoPathIsRefused(t *testing.T) { noPathRefused(t, false) }

// An entry holding neither an apply nor a read at use is no way either: the
// section is refused the same, never read as read at use.
func TestEmptyReachIsRefused(t *testing.T) { noPathRefused(t, true) }

func noPathRefused(t *testing.T, empty bool) {
	h, _ := servedFaulted(t)
	declareSections(t, h)
	ch := watchStatus(h)
	armed := armWaits(h)
	h.Node().DeclareSections(node.Declared{
		Prefix:  "orphan",
		Section: settings.Of("orphan", settings.At(entity.ScopeInstallation), walkTuning{Level: 1}),
		Empty:   empty,
	})
	started(t, h)
	caughtUp(t, h)
	waitsSettled(t, armed)

	installation := entity.SettingScope{Kind: entity.ScopeInstallation}

	apiLifetime, err := json.Marshal((13 * time.Hour).String())
	if err != nil {
		t.Fatal(err)
	}

	putRowGen(t, h, installation, "api.session_lifetime", apiLifetime)
	gen := putRowGen(t, h, installation, "orphan.level", []byte("3"))
	walkTurn(t, h, armed)

	now := counters(t, h)
	refused := h.Node().Refused()

	want := entity.SettingRef(installation, "orphan.level")
	if len(refused) != 1 || !hasRef([]entity.ResourceRef{refused[0].Record}, want) {
		t.Fatalf(
			"refused records: got %s, want orphan's row alone, no way to its component",
			describe(refused),
		)
	}

	if refused[0].Generation != now.Settings {
		t.Errorf(
			"the refusal's generation: got %d, want %d, the generation the pass read",
			refused[0].Generation,
			now.Settings,
		)
	}

	if _, err := runLevel(t, h, h.Node().Settings(), "orphan"); err == nil {
		t.Errorf(
			"orphan after its live change refused: a value published, want none, held back",
		)
	}

	turnAt(t, h, ch, "the status holding the refusals", func(tr statusTurn) bool {
		return tr.s.Settings == gen
	})

	if got := appliedOf(t, h, "orphan.level"); got != (apiv1.Applied{Nodes: 0, Of: 1}) {
		t.Errorf(
			"orphan.level applied: got %d of %d, want 0 of 1, refused, no way to its component",
			got.Nodes,
			got.Of,
		)
	}

	if got := appliedOf(t, h, "api.session_lifetime"); got != (apiv1.Applied{Nodes: 1, Of: 1}) {
		t.Errorf(
			"api.session_lifetime applied: got %d of %d, want 1 of 1, read at use in the same pass",
			got.Nodes,
			got.Of,
		)
	}
}

// A failed apply stops only its own section: it keeps the value it last
// ran, its row refused at the generation the pass read and retried on the
// backoff, while another section in the same pass applies and the pass
// reports the settings position it reconciled.
func TestApplyFailureKeepsItsLastValue(t *testing.T) {
	h, _ := servedFaulted(t)
	declareSections(t, h)
	ch := watchStatus(h)
	armed := armWaits(h)

	var (
		mu    sync.Mutex
		calls int
		held  int
	)

	h.Node().DeclareSections(node.Declared{
		Prefix: "stubborn",
		Section: settings.Of(
			"stubborn",
			settings.At(entity.ScopeInstallation),
			walkTuning{Level: 1},
		),
		Apply: func(snap *settings.Snapshot) error {
			v, err := settings.Get[walkTuning](snap, "stubborn", h.NodeID())
			if err != nil {
				return err
			}

			mu.Lock()
			defer mu.Unlock()

			calls++

			if v.Level == 2 {
				return errors.New("the component refused level 2")
			}

			held = v.Level

			return nil
		},
	})
	started(t, h)
	caughtUp(t, h)
	waitsSettled(t, armed)

	mu.Lock()
	ranAt, heldAt := calls, held
	mu.Unlock()

	if ranAt != 1 || heldAt != 1 {
		t.Fatalf(
			"the apply over the start: called %d times holding level %d, want 1 holding 1",
			ranAt,
			heldAt,
		)
	}

	installation := entity.SettingScope{Kind: entity.ScopeInstallation}
	putRowGen(t, h, installation, "stubborn.level", []byte("2"))
	putRowGen(t, h, installation, "beta.level", []byte("4"))
	walkTurn(t, h, armed)
	gen := counters(t, h).Settings

	got, err := runLevel(t, h, h.Node().Settings(), "stubborn")
	if err != nil || got != 1 {
		t.Errorf(
			"stubborn after its apply failed: level %d, %v; want 1, the value it last ran",
			got,
			err,
		)
	}

	mu.Lock()
	running, level := calls, held
	mu.Unlock()

	if level != 1 || running != ranAt+1 {
		t.Errorf(
			"the component after its apply failed: level %d over %d new calls, want 1",
			level,
			running-ranAt,
		)
	}

	refused := h.Node().Refused()

	want := entity.SettingRef(installation, "stubborn.level")
	if len(refused) != 1 || !hasRef([]entity.ResourceRef{refused[0].Record}, want) {
		t.Fatalf(
			"refused records: got %s, want stubborn's row alone, its apply failed",
			describe(refused),
		)
	}

	if refused[0].Generation != gen {
		t.Errorf(
			"the refusal's generation: got %d, want %d, the generation the pass read",
			refused[0].Generation,
			gen,
		)
	}

	turnAt(t, h, ch, "the status holding the failed generation", func(tr statusTurn) bool {
		return tr.s.Settings == gen
	})

	if got := appliedOf(t, h, "stubborn.level"); got != (apiv1.Applied{Nodes: 0, Of: 1}) {
		t.Errorf(
			"stubborn.level applied: got %d of %d, want 0 of 1, refused, its apply failing",
			got.Nodes,
			got.Of,
		)
	}

	if got := appliedOf(t, h, "beta.level"); got != (apiv1.Applied{Nodes: 1, Of: 1}) {
		t.Errorf(
			"beta.level applied: got %d of %d, want 1 of 1, applied in the same pass",
			got.Nodes,
			got.Of,
		)
	}
}

// A failed apply on a section with one value per share keeps every share's
// value: the apply is one call for the whole section, so no share ran the
// new value, and every share's row reads refused.
func TestApplyFailureKeepsEveryInstance(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	declareSections(t, h)
	armed := armWaits(h)

	var (
		mu    sync.Mutex
		boom  bool
		calls int
	)

	h.Node().DeclareSections(node.Declared{
		Prefix:  "duo",
		Section: settings.Of("duo", settings.At(entity.ScopeShare), walkTuning{Level: 1}),
		Apply: func(*settings.Snapshot) error {
			mu.Lock()
			defer mu.Unlock()

			calls++

			if boom {
				return errors.New("the component refused the pair")
			}

			return nil
		},
	})
	started(t, h)
	configure(t, h, storeA)
	shares := createShares(t, h, storeA, "s1", "s2")
	one, two := shares[0], shares[1]
	s1 := entity.SettingScope{Kind: entity.ScopeShare, ID: one.ID}
	s2 := entity.SettingScope{Kind: entity.ScopeShare, ID: two.ID}

	caughtUp(t, h)
	waitsSettled(t, armed)

	// Both shares' values run first, so the section has what it ran to
	// keep when the apply fails.
	putRowGen(t, h, s1, "duo.level", []byte("3"))
	putRowGen(t, h, s2, "duo.level", []byte("3"))
	walkTurn(t, h, armed)

	mu.Lock()
	ranAt := calls
	mu.Unlock()

	if ranAt != 1 {
		t.Fatalf("the apply of the pair's first value: called %d times, want 1", ranAt)
	}

	before := h.Node().Settings()

	// The component stops, both shares change to 4, and the apply fails:
	// it is one call for the whole section, so no share ran its new value.
	mu.Lock()
	boom = true
	mu.Unlock()

	putRowGen(t, h, s1, "duo.level", []byte("4"))
	putRowGen(t, h, s2, "duo.level", []byte("4"))
	walkTurn(t, h, armed)

	refused := h.Node().Refused()
	w1 := entity.SettingRef(s1, "duo.level")
	w2 := entity.SettingRef(s2, "duo.level")

	var got []entity.ResourceRef
	for _, r := range refused {
		got = append(got, r.Record)
	}

	if len(got) != 2 || !hasRef(got, w1) || !hasRef(got, w2) {
		t.Fatalf(
			"refused records: got %s, want duo's row of share %s and %s, every instance held",
			describe(refused),
			one.Name,
			two.Name,
		)
	}

	if refused[0].Generation != counters(t, h).Settings {
		t.Errorf(
			"the refusal's generation: got %d, want %d, the generation the pass read",
			refused[0].Generation,
			counters(t, h).Settings,
		)
	}

	mu.Lock()
	running := calls
	mu.Unlock()

	if running != ranAt+1 {
		t.Errorf(
			"the apply after both shares changed: %d more calls, want 1, one for the section",
			running-ranAt,
		)
	}

	// Both shares keep the values they ran, whichever changed: the diff
	// against the snapshot before lists neither of them, nothing keeping
	// only part of the section back.
	forward := settings.Diff(before, h.Node().Settings())
	if len(forward) != 0 {
		t.Errorf(
			"the published values after the failed apply: Diff = %+v, want none, every share held",
			forward,
		)
	}
}

// An apply that fails once is retried on the backoff with no new write: the
// pass the backoff releases runs the value and clears the refusal, and no
// later pass calls the apply again with the settings unchanged.
func TestApplyFailureRetriedOnTheBackoff(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	armed := armWaits(h)

	var (
		mu         sync.Mutex
		calls      int
		held       int
		failedOnce bool
	)

	h.Node().DeclareSections(node.Declared{
		Prefix:  "once",
		Section: settings.Of("once", settings.At(entity.ScopeInstallation), walkTuning{Level: 1}),
		Apply: func(snap *settings.Snapshot) error {
			v, err := settings.Get[walkTuning](snap, "once", h.NodeID())
			if err != nil {
				return err
			}

			mu.Lock()
			defer mu.Unlock()

			calls++
			held = v.Level

			if v.Level == 2 && !failedOnce {
				failedOnce = true

				return errors.New("the component refused level 2 the first time")
			}

			return nil
		},
	})
	started(t, h)
	caughtUp(t, h)
	waitsSettled(t, armed)

	putRowGen(t, h, entity.SettingScope{Kind: entity.ScopeInstallation}, "once.level", []byte("2"))
	walkTurn(t, h, armed) // the pass that delivers the change, and fails

	mu.Lock()
	failedAt := calls
	mu.Unlock()

	walkTurn(t, h, armed) // the backoff's pass retries without a write

	mu.Lock()
	running, level := calls, held
	mu.Unlock()

	if running != failedAt+1 {
		t.Fatalf(
			"the apply through the backoff: called %d more times, want 1, one retry",
			running-failedAt,
		)
	}

	if level != 2 {
		t.Errorf(
			"the component after the retry: level %d, want 2, the retry running the value",
			level,
		)
	}

	if got, err := runLevel(t, h, h.Node().Settings(), "once"); err != nil || got != 2 {
		t.Errorf(
			"the snapshot's once.level after the retry: got %d, %v; want 2, published",
			got,
			err,
		)
	}

	if got := len(h.Node().Refused()); got != 0 {
		t.Fatalf("refused records after the retry: got %s, want none", describe(h.Node().Refused()))
	}

	walkTurn(t, h, armed) // a pass nothing changed in runs no apply

	mu.Lock()
	running = calls
	mu.Unlock()

	if running != failedAt+1 {
		t.Errorf(
			"one pass after the value ran: %d more calls in all, want 1, no retry loop",
			running-failedAt,
		)
	}
}

// A section whose first apply fails runs nothing: it enters the node's
// record of what each section first runs only at an apply that succeeds,
// so its restart key, off its default before the node started, sits in no
// restart-pending set once the backoff retry runs the section, and nothing
// of it stays refused (RFC 23 §2.4: the set holds only what changed since
// the node started).
func TestTheFirstApplyFailureLeavesNothingPending(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	ch := watchStatus(h)

	var (
		mu         sync.Mutex
		failedOnce bool
		calls      int
	)

	h.Node().DeclareSections(node.Declared{
		Prefix: "moody",
		Section: settings.Of("moody", settings.At(entity.ScopeInstallation),
			stepTuning{Level: 1, Halt: 2}),
		Apply: func(snap *settings.Snapshot) error {
			v, err := settings.Get[stepTuning](snap, "moody", h.NodeID())
			if err != nil {
				return err
			}

			mu.Lock()
			defer mu.Unlock()

			calls++

			if v.Level == 6 && !failedOnce {
				failedOnce = true

				return errors.New("the component refused level 6 the first time")
			}

			return nil
		},
	})

	// Both keys off their default before the node starts, the restart key
	// among them: they are the section's first value, not a change after
	// it started.
	installation := entity.SettingScope{Kind: entity.ScopeInstallation}
	putRowGen(t, h, installation, "moody.level", []byte("6"))
	gen := putRowGen(t, h, installation, "moody.halt", []byte("3"))

	started(t, h)
	caughtUp(t, h)

	mu.Lock()

	running, failedOnceNow := calls, failedOnce
	mu.Unlock()

	if running != 2 || !failedOnceNow {
		t.Fatalf(
			"the apply over the start and its retry: %d calls, failed once %v, want 2",
			running,
			failedOnceNow,
		)
	}

	v, err := settings.Get[stepTuning](h.Node().Settings(), "moody", h.NodeID())
	if err != nil || v.Level != 6 || v.Halt != 3 {
		t.Errorf(
			"the snapshot's moody after the retry: got %+v, %v; want level 6, halt 3",
			v,
			err,
		)
	}

	tr := turnAt(t, h, ch, "the status past the retry", func(tr statusTurn) bool {
		return tr.s.Settings == gen
	})

	if got := len(tr.s.RestartPending); got != 0 {
		t.Errorf(
			"the restart key, run by the retry: RestartPending holds %d, want none",
			got,
		)
	}

	if got := len(h.Node().Refused()); got != 0 {
		t.Errorf(
			"refused records after the retry: got %s, want none, the section ran",
			describe(h.Node().Refused()),
		)
	}
}

// A section with no row at all, running only its defaults, is refused when
// its apply fails and starts nothing: the refusal names the section's
// changed keys, with the generation the pass read and the apply's error,
// and the backoff retries the delivery, there being no row to name the
// refusal by (RFC 23 section 2.4: a node reports every change it did not
// apply).
func TestADefaultsOnlyApplyFailureIsRefusedAndRetried(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	armed := armWaits(h)

	var (
		mu    sync.Mutex
		calls int
	)

	h.Node().DeclareSections(node.Declared{
		Prefix: "bare",
		Section: settings.Of("bare", settings.At(entity.ScopeInstallation),
			walkTuning{Level: 1}),
		Apply: func(*settings.Snapshot) error {
			mu.Lock()
			defer mu.Unlock()

			calls++

			return errors.New("the component refused its defaults")
		},
	})
	started(t, h)
	caughtUp(t, h)
	waitsSettled(t, armed)

	installation := entity.SettingScope{Kind: entity.ScopeInstallation}

	refused := h.Node().Refused()

	want := entity.SettingRef(installation, "bare.level")
	if len(refused) != 1 || !hasRef([]entity.ResourceRef{refused[0].Record}, want) {
		t.Fatalf(
			"refused records for a section with no row: got %s, want bare's changed key alone",
			describe(refused),
		)
	}

	if refused[0].Generation != counters(t, h).Settings {
		t.Errorf(
			"the refusal's generation: got %d, want %d, the generation the pass read",
			refused[0].Generation,
			counters(t, h).Settings,
		)
	}

	var p apiv1.Problem
	if err := json.Unmarshal(refused[0].Error, &p); err != nil {
		t.Fatalf("the refusal's error %s: %v", refused[0].Error, err)
	}

	if p.Status != http.StatusUnprocessableEntity ||
		!strings.Contains(p.Detail, "refused its defaults") {
		t.Errorf(
			"the refused section's problem: got %d %q, want 422 naming the apply's error",
			p.Status,
			p.Detail,
		)
	}

	if _, err := settings.Get[walkTuning](h.Node().Settings(), "bare", h.NodeID()); err == nil {
		t.Errorf("bare after its defaults failed to apply: a value published, want none, held back")
	}

	// The start already delivered twice, the records walk and the first
	// reconcile's, both refused: the second walk lists the section again,
	// the first having published nothing of it.
	mu.Lock()
	first := calls
	mu.Unlock()

	if first != 2 {
		t.Fatalf("the apply over the start: %d calls, want 2, the start's two walks", first)
	}

	// The backoff pass retries the failed delivery, though no row changed.
	walkTurn(t, h, armed)

	mu.Lock()
	second := calls
	mu.Unlock()

	if second != first+1 {
		t.Fatalf(
			"a section with no row through the backoff: %d calls, want %d, the delivery retried",
			second,
			first+1,
		)
	}

	if got, err := settings.Get[walkTuning](h.Node().Settings(), "bare", h.NodeID()); err == nil {
		_ = got

		t.Errorf("bare after the retry still fails: a value published, want none, held back")
	}
}

// A section with no row and no way to its component is refused the same
// way: without a row to name the refusal by, its changed keys carry it,
// with the no-path error. Nothing of it is published, no apply runs, and
// the refusal is not retried: no write can clear what only a new binary
// gives a path to, so no backoff pass runs for it.
func TestADefaultsOnlySectionWithNoPathIsRefused(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	armed := armWaits(h)

	var (
		mu      sync.Mutex
		applies int
	)

	h.Node().OnApply(func(kvstore.Counters) {
		mu.Lock()
		defer mu.Unlock()

		applies++
	})

	h.Node().DeclareSections(node.Declared{
		Prefix: "nopath",
		Section: settings.Of("nopath", settings.At(entity.ScopeInstallation),
			walkTuning{Level: 1}),
	})
	started(t, h)
	caughtUp(t, h)
	waitsSettled(t, armed)

	installation := entity.SettingScope{Kind: entity.ScopeInstallation}

	refused := h.Node().Refused()

	want := entity.SettingRef(installation, "nopath.level")
	if len(refused) != 1 || !hasRef([]entity.ResourceRef{refused[0].Record}, want) {
		t.Fatalf(
			"refused records for a pathless section with no row: got %s, want nopath's key alone",
			describe(refused),
		)
	}

	if refused[0].Generation != counters(t, h).Settings {
		t.Errorf(
			"the refusal's generation: got %d, want %d, the generation the pass read",
			refused[0].Generation,
			counters(t, h).Settings,
		)
	}

	var p apiv1.Problem
	if err := json.Unmarshal(refused[0].Error, &p); err != nil {
		t.Fatalf("the refusal's error %s: %v", refused[0].Error, err)
	}

	if p.Status != http.StatusUnprocessableEntity ||
		!strings.Contains(p.Detail, "no way to its component") {
		t.Errorf(
			"the pathless section's problem: got %d %q, want 422 naming the missing way",
			p.Status,
			p.Detail,
		)
	}

	if _, err := settings.Get[walkTuning](h.Node().Settings(), "nopath", h.NodeID()); err == nil {
		t.Errorf("nopath, held back at start: a value published, want none, the section never ran")
	}

	// One step of the clock, no write: the loop runs its interval pass and
	// no apply, the refusal of a pathless section asking for no retry.
	mu.Lock()
	ranAt := applies
	mu.Unlock()

	walkTurn(t, h, armed)

	mu.Lock()
	running := applies
	mu.Unlock()

	if running != ranAt {
		t.Errorf(
			"one step of the clock, no write: %d applies, want %d, no retry for a pathless section",
			running,
			ranAt,
		)
	}
}

// A share whose removal deleted its settings rows reconfigures nothing: the
// share no longer listed, its changes back to the section's base are
// dropped, so no apply runs, nothing reads refused and nothing sits
// restart-pending.
func TestRemovedShareChangesDiscarded(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	ch := watchStatus(h)
	armed := armWaits(h)

	var (
		mu      sync.Mutex
		applies int
	)

	h.Node().DeclareSections(node.Declared{
		Prefix:  "pair",
		Section: settings.Of("pair", settings.At(entity.ScopeShare), stepTuning{Level: 1, Halt: 2}),
		Apply: func(*settings.Snapshot) error {
			mu.Lock()
			defer mu.Unlock()

			applies++

			return nil
		},
	})
	declareSections(t, h)
	configure(t, h, storeA)
	one := createShare(t, h, "one", storeA)
	share := entity.SettingScope{Kind: entity.ScopeShare, ID: one.ID}
	putRowGen(t, h, share, "pair.level", []byte("5"))
	putRowGen(t, h, share, "pair.halt", []byte("7"))

	g := gateRunner(h)
	defer g.release()

	started(t, h)
	caughtUp(t, h)
	waitsSettled(t, armed)
	g.took(t)

	mu.Lock()
	ranAt := applies
	mu.Unlock()

	if ranAt != 1 {
		t.Fatalf("the apply of the share's live change: called %d times, want 1", ranAt)
	}

	// Remove the share, driving its removal past the step that deletes its
	// settings rows: the runner turns run its batches to the live state's
	// end, and the shard's forgotten is recorded here as the kvstore's own
	// batch tests do, the journal's forget being the step that waits.
	deleteShare(t, h, one)

	for range 100 {
		g.next(t)

		if s := tagState(t, h, one.ID); s == entity.TagOffloaded || s == entity.TagForgotten {
			break
		}
	}

	if s := tagState(t, h, one.ID); s != entity.TagOffloaded && s != entity.TagForgotten {
		t.Fatalf(
			"the deleted share's shard: removal at %v, want offloaded or later",
			s,
		)
	}

	// One turn more, which stands the removal at its forget step, before
	// the shard's forgotten can be recorded.
	g.next(t)

	sh, _, err := h.Store().ShareIndex().Share(t.Context(), one.ID)
	if err != nil {
		t.Fatalf("read the share back: %v", err)
	}

	if err := h.Store().Nodes().RecordShardRemoval(
		t.Context(), one.ID, sh.Shard, entity.TagForgotten); err != nil {
		t.Fatalf("record the share's shard forgotten: %v", err)
	}

	for range 100 {
		shardDone, err := h.Store().Nodes().ContinueShardRemoval(t.Context(), one.ID, sh.Shard)
		if err != nil {
			t.Fatalf("drop the share's shard's files: %v", err)
		}

		if shardDone {
			break
		}
	}

	rows := 1
	for range 100 {
		rows = 0

		for _, err := range h.Store().ControlPlane().Settings(t.Context(), share) {
			if err != nil {
				t.Fatalf("read the removed share's settings: %v", err)
			}

			rows++
		}

		if rows == 0 {
			break
		}

		g.next(t)
	}

	if rows != 0 {
		t.Fatal("the removed share's settings rows: still in the store after 100 runner turns")
	}

	waitsSettled(t, armed)
	walkTurn(t, h, armed)

	mu.Lock()
	running := applies
	mu.Unlock()

	if running != ranAt {
		t.Errorf(
			"after the removed share's rows were deleted: %d more calls, want none",
			running-ranAt,
		)
	}

	if got := len(h.Node().Refused()); got != 0 {
		t.Fatalf(
			"refused records after the removed share's rows were deleted: got %s, want none",
			describe(h.Node().Refused()),
		)
	}

	gen := counters(t, h).Settings
	turnAt(t, h, ch, "the status past the deleted rows", func(tr statusTurn) bool {
		return tr.s.Settings == gen && len(tr.s.RestartPending) == 0
	})
}

// The walk's pages are store calls, held to storeBound: a page that hangs
// fails the pass at 20 s on the node's clock, so a store that stops
// answering mid-walk holds no reconcile past the bound, as one that stops
// before it holds no pass.
func TestSettingsWalkBoundsPageReads(t *testing.T) {
	logs := make(logLines, 256)
	h, f := faulted(t, func(cfg *node.Config) { cfg.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
	declareSections(t, h)
	armed := hookWalkWait(h)
	started(t, h)
	caughtUp(t, h)

	walkAt(t, h, entity.SettingScope{Kind: entity.ScopeInstallation}, "beta", 3)
	held := sim.NewTestClock(time.Unix(0, 0))
	setFault(t, f, held, sim.Rule{Op: kvtest.OpScan, Key: cfgScan, Count: 1, Action: sim.FaultAction{Delay: time.Hour}})
	defer held.Advance(time.Hour) // a failed test's Stop finds no scan held

	armed.expect(0)

	// A write's wake starts the pass the fault holds in its first page, so
	// the node's clock stands still from the call's start.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = h.Node().Applied(ctx) }()
	wait(t, "the woken pass's walk held", func() bool { return held.Pending() > 0 })
	armed.await(t)
	timedOut := func(within time.Duration) bool {
		deadline := time.After(within)
		for {
			select {
			case l := <-logs:
				if strings.Contains(l, "did not answer within") {
					return true
				}
			case <-deadline:
				return false
			}
		}
	}
	h.Clock().Advance(19 * time.Second)
	// No clock reaches a pass that has not failed, so a bounded wait is the
	// assertion: a pass failed by then would have logged at once.
	if timedOut(200 * time.Millisecond) {
		t.Fatalf("the walk's held page failed its pass 19 s after it started, want it held until 20 s")
	}
	h.Clock().Advance(time.Second)
	if !timedOut(10 * time.Second) {
		t.Fatalf("the walk's held page did not fail its pass 20 s after it started on the node's clock")
	}
}

// A later page of the walk is bound from when it starts, not from the walk's
// start: the first page takes 15 s, the second is held, and the walk fails 20
// s after the second began, 35 s after the walk did.
func TestSettingsWalkBoundsALaterPage(t *testing.T) {
	logs := make(logLines, 256)
	h, f := faulted(t, func(cfg *node.Config) { cfg.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
	declareSections(t, h)
	armed := hookWalkWait(h)
	started(t, h)
	padRows(t, h, walkPage+walkPage/8)
	caughtUp(t, h)

	walkAt(t, h, entity.SettingScope{Kind: entity.ScopeInstallation}, "beta", 3)
	armed.expect(walkPage)
	held := sim.NewTestClock(time.Unix(0, 0))
	setFault(t, f, held,
		sim.Rule{Op: kvtest.OpScan, Key: cfgScan, From: 1, Count: 1, Action: sim.FaultAction{Delay: 15 * time.Second}},
		sim.Rule{Op: kvtest.OpScan, Key: cfgScan, From: 2, Count: 1, Action: sim.FaultAction{Delay: time.Hour}},
	)
	defer held.Advance(time.Hour) // a failed test's Stop finds no scan held

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = h.Node().Applied(ctx) }()
	wait(t, "the woken pass's walk held in its first page", func() bool { return held.Pending() == 1 })

	h.Clock().Advance(15 * time.Second)
	held.Advance(15 * time.Second)
	wait(t, "the walk held in its second page", func() bool { return held.Pending() == 1 })
	armed.await(t) // the walk waits for the second page, its bound armed

	timedOut := func(within time.Duration) bool {
		deadline := time.After(within)
		for {
			select {
			case l := <-logs:
				if strings.Contains(l, "did not answer within") {
					return true
				}
			case <-deadline:
				return false
			}
		}
	}

	h.Clock().Advance(19 * time.Second)
	// No clock reaches a pass that has not failed, so a bounded wait is the
	// assertion: a pass failed by then would have logged at once.
	if timedOut(200 * time.Millisecond) {
		t.Fatalf("the walk failed 34 s after it started, 19 s after its second page did, want it held until 20 s into the page")
	}

	h.Clock().Advance(time.Second)

	if !timedOut(10 * time.Second) {
		t.Fatalf("the second page did not fail the walk 20 s after it started on the node's clock")
	}
}

// A walk of many pages, each answering within the bound, finishes however long
// the whole takes: three pages of 15 s each are 45 s, over the 20 s bound of
// one store call.
func TestSettingsWalkOfSlowPagesFinishes(t *testing.T) {
	logs := make(logLines, 256)
	h, f := faulted(t, func(cfg *node.Config) { cfg.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
	declareSections(t, h)
	started(t, h)
	padRows(t, h, 2*walkPage+walkPage/8)
	caughtUp(t, h)

	walkAt(t, h, entity.SettingScope{Kind: entity.ScopeInstallation}, "beta", 3)
	want := counters(t, h).Settings
	held := sim.NewTestClock(time.Unix(0, 0))
	setFault(t, f, held, sim.Rule{Op: kvtest.OpScan, Key: cfgScan, From: 1, Count: 3, Action: sim.FaultAction{Delay: 15 * time.Second}})
	defer held.Advance(time.Hour) // a failed test's Stop finds no scan held

	done := make(chan error, 1)
	go func() { done <- h.Node().Applied(t.Context()) }()

	for page := 1; page <= 3; page++ {
		wait(t, fmt.Sprintf("the walk held in page %d", page), func() bool { return held.Pending() == 1 })
		h.Clock().Advance(15 * time.Second)
		held.Advance(15 * time.Second)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("applied: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the walk of three pages of 15 s each did not finish within 10 s of its last page")
	}

	if got := h.Node().Positions().Settings; got < want {
		t.Fatalf("settings position %d after the slow walk, want at least %d", got, want)
	}

	select {
	case l := <-logs:
		if strings.Contains(l, "did not answer within") {
			t.Fatalf("a page that answered in 15 s failed the pass: %s", l)
		}
	default:
	}
}

// A refusal holds a section whole, but never names a row its section does
// not read: one at a scope kind the section does not have is a row Read
// skips, this node runs it nowhere, so it cannot read refused, while the
// wide and the node rows still go named.
func TestSettingsRefusalNeverNamesASkippedRow(t *testing.T) {
	h, f := faulted(t)
	declareSections(t, h)
	armed := armWaits(h)
	started(t, h)
	configure(t, h, storeA)
	sh := createShare(t, h, "one", storeA)
	node := entity.SettingScope{Kind: entity.ScopeNode, ID: h.NodeID()}
	installation := entity.SettingScope{Kind: entity.ScopeInstallation}
	share := entity.SettingScope{Kind: entity.ScopeShare, ID: sh.ID}
	walkAt(t, h, installation, "alpha", 8)
	walkAt(t, h, share, "alpha", 5)
	caughtUp(t, h)

	waitsSettled(t, armed)

	// The node's row refuses to decode while a row the section does not read
	// sits at share scope: the section is held whole, the share row stays
	// out of the refused set. A write of a section beside it moves the
	// generation, so the pass walks.
	walkAt(t, h, installation, "beta", 4)
	babble(t, f, node, "alpha.level")
	walkTurn(t, h, armed)

	refused := h.Node().Refused()
	want := []entity.ResourceRef{
		entity.SettingRef(node, "alpha.level"),
		entity.SettingRef(installation, "alpha.level"),
	}

	var got []entity.ResourceRef

	for _, r := range refused {
		got = append(got, r.Record)
	}

	if len(got) != 2 || !hasRef(got, want[0]) || !hasRef(got, want[1]) {
		t.Fatalf(
			"refused records: got %s, want the wide and the node rows alone, the share row one the section does not read",
			describe(refused),
		)
	}
}

// A refusal names only the rows this node runs: another node's row of the
// same section, which the node does not read, stays out of the refused set,
// its own corrupt row alone beside it.
func TestSettingsRefusalNeverNamesAnotherNodesRow(t *testing.T) {
	h, f := faulted(t)
	declareSections(t, h)
	armed := armWaits(h)
	started(t, h)

	node := entity.SettingScope{Kind: entity.ScopeNode, ID: h.NodeID()}
	other := h.NodeID()
	other[0] ^= 0xff // any node but this one
	walkAt(t, h, entity.SettingScope{Kind: entity.ScopeNode, ID: other}, "alpha", 5)
	caughtUp(t, h)

	waitsSettled(t, armed)

	// This node's row refuses to decode while another node holds one of the
	// same section: the refusal names this node's row alone. A write of a
	// section beside it moves the generation, so the pass walks.
	walkAt(t, h, entity.SettingScope{Kind: entity.ScopeInstallation}, "beta", 4)
	babble(t, f, node, "alpha.level")
	walkTurn(t, h, armed)

	refused := h.Node().Refused()
	want := entity.SettingRef(node, "alpha.level")

	if len(refused) != 1 || !hasRef([]entity.ResourceRef{refused[0].Record}, want) {
		t.Fatalf(
			"refused records: got %s, want this node's row alone, another node's never named",
			describe(refused),
		)
	}
}
