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

// declareSections declares the tests' sections: alpha, a node's override of
// an installation default; beta and eps at installation scope; gamma once
// per share; delta once per share with two keys; nest with a section nested
// under its prefix, and alphabet, whose prefix holds alpha's in it. Call it
// before Start.
func declareSections(t *testing.T, h *nodetest.Harness) {
	t.Helper()
	h.Node().DeclareSections(
		settings.Of("alpha", settings.Override(entity.ScopeInstallation, entity.ScopeNode),
			walkTuning{Level: 1}),
		settings.Of("beta", settings.At(entity.ScopeInstallation), walkTuning{Level: 1}),
		settings.Of("eps", settings.At(entity.ScopeInstallation), walkTuning{Level: 1}),
		settings.Of("gamma", settings.At(entity.ScopeShare), walkTuning{Level: 1}),
		settings.Of("delta", settings.At(entity.ScopeShare), walkPair{A: 1, B: 1}),
		settings.Of("nest", settings.At(entity.ScopeInstallation), walkTuning{Level: 1}),
		settings.Of("nest.sub", settings.At(entity.ScopeInstallation), walkTuning{Level: 1}),
		settings.Of("alphabet", settings.At(entity.ScopeInstallation), walkTuning{Level: 1}),
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
