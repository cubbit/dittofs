package node_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/settings"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// The rows that write a setting through the API of a wired node.
//
// decision: the rows of §2.3 reconcile, §2.4 applied and a bound field
// declare the sections they write, standing in for the components that will
// read them, since the node's own table holds no section at a share's, a
// node's override or a remote store's scope yet: each such section is
// declared by the component that reads it. The write path, the
// reconcile and the applied count they test are the node's own whatever
// section a key is in. Declare none here once the node's table holds a
// section of each scope and class these rows need.

// standIns declares, on h's node, the sections the rows below write: gamma,
// one live level per share; alpha, a node's live override of an
// installation default; boots, a node's restart-class halt; tune, a remote
// store's live level and bound floor. Call it before Start.
func standIns(h *nodetest.Harness) {
	h.Node().DeclareSections(
		node.Declared{Prefix: "gamma", Section: settings.Of("gamma", settings.At(entity.ScopeShare), walkTuning{Level: 1}), AtUse: true},
		node.Declared{Prefix: "alpha", Section: settings.Of("alpha", settings.Override(entity.ScopeInstallation, entity.ScopeNode), walkTuning{Level: 1}), AtUse: true},
		node.Declared{Prefix: "boots", Section: settings.Of("boots", settings.Override(entity.ScopeInstallation, entity.ScopeNode), bootsTuning{Halt: 9}), AtUse: true},
		node.Declared{Prefix: "tune", Section: settings.Of("tune", settings.At(entity.ScopeRemote), boundTuning{Level: 1, Floor: 1}), AtUse: true},
	)
}

// putSetting writes value under key at the setting path through the API as
// the account by, creating it or matching its current ETag.
func (w *wired) putSetting(t *testing.T, by account, path, value string) authReply {
	t.Helper()
	cond := []string{"If-None-Match", "*"}
	if cur := w.send(t, http.MethodGet, path, by.token, nil); cur.status == http.StatusOK {
		cond = []string{"If-Match", cur.header.Get("ETag")}
	}
	return w.send(t, http.MethodPut, path, by.token, json.RawMessage(`{"spec":{"value":`+value+`}}`), append(cond, idemKey()...)...)
}

// appliedAt is the applied count GET path answers the admin.
func (w *wired) appliedAt(t *testing.T, path string) apiv1.Applied {
	t.Helper()
	r := w.send(t, http.MethodGet, path, w.admin.token, nil)
	var s apiv1.Setting
	if r.status != http.StatusOK || json.Unmarshal(r.body, &s) != nil || s.Status.Applied == nil {
		t.Fatalf("GET %s: answered %d %s, want 200 with an applied count", path, r.status, r.body)
	}
	return *s.Status.Applied
}

// appliedReaches turns the node until GET path answers want as the
// setting's applied count: the node's status, which the count is read
// from, is written at the status writer's turns.
func (w *wired) appliedReaches(t *testing.T, path string, want apiv1.Applied) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		got := w.appliedAt(t, path)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s within 30 s: applied %+v, want %+v", path, got, want)
		}
		turn(w.h)
	}
}

// §2.3 reconcile: a live share setting changed while the store fails the
// node's reads of the settings, its writes passing, is not applied by the
// pass that read the counters and failed its walk, and applies at the next pass whose read works, with no write
// after it: the settings counter stands where the change left it, the
// node's snapshot holds the change, and the setting reads applied through
// the API.
// Catches a missed generation lost for good.
func TestWiredSettingChangedWhileReadsFailAppliesAtTheNextRead(t *testing.T) {
	onBackends(t, func(t *testing.T, memory bool) {
		var armed *passes
		w := newWiredThen(t, memory, nil, func(h *nodetest.Harness) {
			standIns(h)
			armed = armWaits(h)
		})
		configure(t, w.h, storeA)
		sh := createShares(t, w.h, storeA, "tuned")[0]
		scope := entity.SettingScope{Kind: entity.ScopeShare, ID: sh.ID}
		path := "/api/v1/shares/" + sh.ID.String() + "/settings/gamma.level"
		if r := w.putSetting(t, w.admin, path, "1"); r.status != http.StatusOK {
			t.Fatalf("PUT %s 1: answered %d %s, want 200", path, r.status, r.body)
		}
		caughtUp(t, w.h)
		waitsSettled(t, armed)
		prev := w.h.Node().Settings()
		changed := []settings.Change{{Section: "gamma", Key: "gamma.level", Class: settings.Live, Scope: scope}}
		// The harness's store is not the node's KV, so its writes pass while
		// the node's scans of the settings rows fail: the pass reads the
		// counters, and its walk of the settings fails.
		restore := w.faulting(t, sim.Rule{Op: kvtest.OpScan, Key: keys.Describe([]byte{keys.KindCFG}) + "*", Action: sim.FaultAction{Err: vfserr.ErrUnavailable}})
		gen := putRowGen(t, w.h, scope, "gamma.level", []byte("42"))
		walkTurn(t, w.h, armed)
		if got := w.h.Node().Positions().Settings; got >= gen {
			t.Errorf("settings position after a pass whose reads failed: %d, want below the change at %d", got, gen)
		}
		if got := settings.Diff(prev, w.h.Node().Settings()); len(got) != 0 {
			t.Errorf("the snapshot after a pass whose walk failed: changes %+v, want none", got)
		}
		restore()

		caughtUp(t, w.h)
		if got := counters(t, w.h).Settings; got != gen {
			t.Fatalf("settings counter once reads work: %d, want %d, no write after the change", got, gen)
		}
		if got := settings.Diff(prev, w.h.Node().Settings()); !slices.Equal(got, changed) {
			t.Errorf("the snapshot once reads work: changes %+v, want the share's gamma.level alone, %+v", got, changed)
		}
		w.appliedReaches(t, path, apiv1.Applied{Nodes: 1, Of: 1})
	})
}

// §2.4 applied, single-node. A share setting changed past a node frozen at
// the settings counter reads applied on none of one node through the API,
// and once the lag passes 5 s health names the settings counter behind;
// released, it reads applied. A node's restart-class setting changed, then
// a live one: the first reads applied on none, held restart-pending and not
// refused in the node's status, the second on its one node. Catches
// "applied" read past the node's position or past a pending restart.
//
// decision: the RFC's case of a record whose own generation is 2 while the
// counter stands at 100 cannot be posed, as the store raises a record's
// generation to at least the position its change raised; and "applied"
// inferred from the writing node needs a second node. Neither is checked
// here.
func TestWiredAppliedReadFromTheNodeThatRuns(t *testing.T) {
	onBackends(t, func(t *testing.T, memory bool) {
		var frozen atomic.Bool
		var past uint64
		reached, release := make(chan kvstore.Counters, 1), make(chan struct{})
		w := newWiredThen(t, memory, node.NewPausing(func(c kvstore.Counters) {
			if frozen.Load() && c.Settings > past {
				reached <- c
				<-release
			}
		}), standIns)
		defer func() {
			if frozen.Swap(false) {
				close(release)
			}
		}()
		configure(t, w.h, storeA)
		sh := createShares(t, w.h, storeA, "tuned")[0]
		scope := entity.SettingScope{Kind: entity.ScopeShare, ID: sh.ID}
		path := "/api/v1/shares/" + sh.ID.String() + "/settings/gamma.level"
		putRowGen(t, w.h, scope, "gamma.level", []byte("2"))
		caughtUp(t, w.h)
		w.appliedReaches(t, path, apiv1.Applied{Nodes: 1, Of: 1})

		past = counters(t, w.h).Settings
		frozen.Store(true)
		// Through the store, as another node writes: the API's own write waits
		// until this node has applied it.
		putRowGen(t, w.h, scope, "gamma.level", []byte("3"))
		w.h.Node().Wake()
		var row entity.Setting
		for s, err := range w.h.Store().ControlPlane().Settings(t.Context(), scope) {
			if err != nil {
				t.Fatalf("read the share's settings: %v", err)
			}
			if s.Key == "gamma.level" {
				row = s
			}
		}
		if row.Position <= past {
			t.Fatalf("gamma.level changed at %d over a node at %d, want past it", row.Position, past)
		}
		if c := awaitRead(t, reached); c.Settings != row.Position {
			t.Fatalf("frozen pass read settings %d, want the change at %d", c.Settings, row.Position)
		}
		w.h.Clock().Advance(4 * time.Second)
		if w.healthBehind(t, w.admin) {
			t.Error("config_generation_behind named with the node frozen only 4 s behind the change, want it after 5 s")
		}
		w.h.Clock().Advance(2 * time.Second)
		if !w.healthBehind(t, w.admin) {
			t.Error("config_generation_behind not named with the node frozen 6 s behind the change")
		}
		if got := w.appliedAt(t, path); got != (apiv1.Applied{Nodes: 0, Of: 1}) {
			t.Errorf("the change on the frozen node: applied %+v, want on 0 of 1", got)
		}
		frozen.Store(false)
		close(release)
		caughtUp(t, w.h)
		w.appliedReaches(t, path, apiv1.Applied{Nodes: 1, Of: 1})

		nodeAt := "/api/v1/nodes/" + uuid.UUID(w.h.NodeID()).String() + "/settings/"
		for _, kv := range [][2]string{{"boots.halt", "5"}, {"alpha.level", "7"}} {
			if r := w.putSetting(t, w.admin, nodeAt+kv[0], kv[1]); r.status != http.StatusOK {
				t.Fatalf("PUT %s%s: answered %d %s, want 200", nodeAt, kv[0], r.status, r.body)
			}
		}
		w.appliedReaches(t, nodeAt+"alpha.level", apiv1.Applied{Nodes: 1, Of: 1})
		if got := w.appliedAt(t, nodeAt+"boots.halt"); got != (apiv1.Applied{Nodes: 0, Of: 1}) {
			t.Errorf("the node's restart-class boots.halt before a restart: applied %+v, want on 0 of 1", got)
		}
		halt := entity.SettingRef(entity.SettingScope{Kind: entity.ScopeNode, ID: w.h.NodeID()}, "boots.halt")
		is := func(r entity.ResourceRef) bool { return r.Kind == halt.Kind && string(r.ID) == string(halt.ID) }
		sts, err := w.h.Store().Nodes().RegisteredStatuses(t.Context())
		if err != nil || len(sts) != 1 {
			t.Fatalf("the node's status: %d read, %v; want one", len(sts), err)
		}
		if !slices.ContainsFunc(sts[0].RestartPending, is) || slices.ContainsFunc(sts[0].Refused, func(r entity.RefusedRecord) bool { return is(r.Record) }) {
			t.Errorf("boots.halt in the node's status: restart-pending %v, refused %v; want it pending and not refused", sts[0].RestartPending, sts[0].Refused)
		}
	})
}

// A bound field is refused 409 bound_setting while content exists, naming
// the field: a remote store's bound key, with a namespace naming the store,
// is refused through the API and nothing is committed, while its live key
// is written; the same bound key on a store no namespace names is written.
// Catches a bound field changed under content.
func TestWiredBoundSettingRefusedWhileContentExists(t *testing.T) {
	onBackends(t, func(t *testing.T, memory bool) {
		w := newWiredThen(t, memory, nil, standIns)
		for _, st := range [][16]byte{storeA, storeB} {
			// A store is found by its endpoint, seeded through the store under
			// the file's decision: no route writes one while the table
			// declares no remote store section.
			e := entity.Setting{Scope: entity.SettingScope{Kind: entity.ScopeRemote, ID: st}, Key: "remote.endpoint", Value: []byte(`"https://store.invalid"`)}
			if _, err := w.h.Store().ControlPlane().PutSetting(t.Context(), e, 0); err != nil {
				t.Fatalf("seed a store's endpoint: %v", err)
			}
		}
		namespaceOn(t, w.h, storeA)
		at := func(store [16]byte, key string) string {
			return "/api/v1/stores/" + uuid.UUID(store).String() + "/settings/" + key
		}
		before := counters(t, w.h)
		r := w.putSetting(t, w.admin, at(storeA, "tune.floor"), "5")
		wantStatus(t, "PUT tune.floor of a store a namespace names", r, http.StatusConflict, "bound_setting")
		if r.status == http.StatusConflict && r.problem(t).Field != "tune.floor" {
			t.Errorf("the bound refusal names field %q, want tune.floor", r.problem(t).Field)
		}
		if got := counters(t, w.h); got.Settings != before.Settings {
			t.Errorf("settings counter after the refused bound write: %d, want %d, nothing committed", got.Settings, before.Settings)
		}
		if r := w.putSetting(t, w.admin, at(storeA, "tune.level"), "5"); r.status != http.StatusOK {
			t.Errorf("PUT tune.level, a live key, of a store a namespace names: answered %d %s, want 200", r.status, r.body)
		}
		if r := w.putSetting(t, w.admin, at(storeB, "tune.floor"), "5"); r.status != http.StatusOK {
			t.Errorf("PUT tune.floor of a store no namespace names: answered %d %s, want 200", r.status, r.body)
		}
	})
}

// §5.2 roles, its admin-only keys: an operator's write of audit.retention,
// api.session_lifetime, api.token_max_lifetime, api.password_sign_in or
// identity.directory is refused naming the key, and an admin's
// audit.retention of 89 days is refused as invalid, and none of them
// commits; an admin's 90 days is written. Catches an operator shortening
// the audit, or taking a lifetime or sign-in an admin owns.
//
// decision: api.password_sign_in is refused unknown_key, not forbidden: the
// api section declares no such key until OpenID Connect's sign-in policy is
// built. Its row then answers forbidden like the others.
func TestWiredAdminOnlySettingsRefusedToAnOperator(t *testing.T) {
	onBackends(t, func(t *testing.T, memory bool) {
		w := newWired(t, memory, nil)
		before := storeOf(t, w.h)
		for _, c := range []struct {
			by     account
			key    string
			value  string
			status int
			code   apiv1.Code
		}{
			{w.operator, "audit.retention", `"2880h"`, http.StatusForbidden, "forbidden"},
			{w.operator, "api.session_lifetime", `"1h"`, http.StatusForbidden, "forbidden"},
			{w.operator, "api.token_max_lifetime", `"24h"`, http.StatusForbidden, "forbidden"},
			{w.operator, "api.password_sign_in", `["viewer"]`, http.StatusBadRequest, "unknown_key"},
			{w.operator, "identity.directory.address", `"ldaps://dc.example.test"`, http.StatusForbidden, "forbidden"},
			{w.admin, "audit.retention", `"2136h"`, http.StatusBadRequest, "invalid"},
		} {
			what := fmt.Sprintf("PUT %s %s", c.key, c.value)
			r := w.putSetting(t, c.by, "/api/v1/settings/"+c.key, c.value)
			wantStatus(t, what, r, c.status, c.code)
			if r.status == c.status && r.problem(t).Field != c.key {
				t.Errorf("%s: refusal names field %q, want %s", what, r.problem(t).Field, c.key)
			}
		}
		if after := storeOf(t, w.h); after != before {
			t.Errorf("the refused writes changed the store:\n got %s\nwant %s", after, before)
		}
		if r := w.putSetting(t, w.admin, "/api/v1/settings/audit.retention", `"2160h"`); r.status != http.StatusOK {
			t.Errorf("an admin's PUT audit.retention of 90 days: answered %d %s, want 200", r.status, r.body)
		}
	})
}

// §4.1 NTLM back on. Turning identity.ntlm off answers 202 and starts one
// identity.nt-hash-delete; turning it off again while that runs starts none;
// turning it on, or deleting the installation's identity.ntlm record, while
// it runs answers 409 operation_running naming it, and so do both once it
// has failed on a user record its codec refuses; the deletion has no
// cancel. A turn-off after the failure starts a new deletion, which, the
// record repaired, completes; 8 days later the sweep has removed both
// operations' records and turning NTLM on is accepted. Catches NTLM on
// again while NT hashes remain, a second deletion beside the first, and the
// rule lost with the swept operation.
func TestWiredNTLMBackOn(t *testing.T) {
	onBackends(t, func(t *testing.T, memory bool) {
		var swept <-chan []kvstore.Swept
		w := newWiredThen(t, memory, nil, func(h *nodetest.Harness) { swept = watchSweep(h) })
		const ntlm = "/api/v1/settings/identity.ntlm"
		bob, _ := w.createUser(t, w.admin.token, `{"spec":{"name":"bob","uid":5002}}`, "k-bob")
		if r := w.setPassword(t, w.admin.token, bob, "bob's password"); r.status != http.StatusOK {
			t.Fatalf("bob's :set-password: answered %d %s, want 200", r.status, r.body)
		}
		deletions := func() []entity.OperationID {
			t.Helper()
			var ids []entity.OperationID
			for op, err := range w.h.Store().ControlPlane().Operations(t.Context(), entity.OperationID{}) {
				if err != nil {
					t.Fatalf("list operations: %v", err)
				}
				if op.Kind == string(apiv1.OpNTHashDelete) {
					ids = append(ids, op.ID)
				}
			}
			return ids
		}
		started := func(what string, r authReply) entity.OperationID {
			t.Helper()
			id, err := uuid.Parse(strings.TrimPrefix(r.header.Get("Location"), "/api/v1/operations/"))
			if r.status != http.StatusAccepted || err != nil {
				t.Fatalf("%s: answered %d %s at %q, want 202 with the deletion's Location", what, r.status, r.body, r.header.Get("Location"))
			}
			return entity.OperationID(id)
		}
		refusedNaming := func(what string, id entity.OperationID) {
			t.Helper()
			for _, r := range []authReply{
				w.putSetting(t, w.admin, ntlm, "true"),
				w.send(t, http.MethodDelete, ntlm, w.admin.token, nil, append([]string{"If-Match", w.send(t, http.MethodGet, ntlm, w.admin.token, nil).header.Get("ETag")}, idemKey()...)...),
			} {
				wantStatus(t, what, r, http.StatusConflict, apiv1.CodeOperationRunning)
				if id != (entity.OperationID{}) && r.status == http.StatusConflict && !slices.Contains(r.problem(t).Names, uuid.UUID(id).String()) {
					t.Errorf("%s: names %v, want the deletion %v", what, r.problem(t).Names, uuid.UUID(id))
				}
			}
		}

		// The runner picks an operation up at its next full turn, a second
		// on the node's clock, which nothing advances until the row lets it.
		first := started("PUT identity.ntlm off", w.putSetting(t, w.admin, ntlm, "false"))
		if r := w.putSetting(t, w.admin, ntlm, "false"); r.status != http.StatusOK {
			t.Errorf("PUT identity.ntlm off again while the deletion runs: answered %d %s, want 200", r.status, r.body)
		}
		if got := deletions(); !slices.Equal(got, []entity.OperationID{first}) {
			t.Errorf("deletions after a second turn-off while one runs: %v, want the first alone", got)
		}
		refusedNaming("turning NTLM on while the deletion runs", first)
		wantStatus(t, "POST :cancel of the deletion", w.send(t, http.MethodPost, "/api/v1/operations/"+uuid.UUID(first).String()+":cancel", w.admin.token, nil, idemKey()...),
			http.StatusConflict, apiv1.CodeNotCancellable)

		// bob's record, which the deletion zeroes the NT hash of, made
		// undecodable under its key, as a store's corruption would.
		bobKey := keys.ID([]byte{keys.KindU}, bob.ID[:])
		var good []byte
		raw := func(v []byte) {
			t.Helper()
			if err := w.fault.Update(t.Context(), func(tx kv.Txn) error {
				if good == nil {
					b, err := tx.Get(bobKey)
					if err != nil {
						return err
					}
					good = slices.Clone(b)
				}
				return tx.Set(bobKey, v)
			}); err != nil {
				t.Fatalf("write bob's record raw: %v", err)
			}
		}
		raw([]byte("not a user"))
		if op := ended(t, w.h, first); op.State != entity.OpFailed {
			t.Fatalf("the deletion over an undecodable user: ended %v (%s), want failed", op.State, op.Error)
		} else if !strings.Contains(string(op.Error), "codec") {
			t.Errorf("the deletion's error: %s, want the user record's codec refusal", op.Error)
		}
		// decision: once the deletion has failed the refusal names no
		// operation, as a transaction lists none (the ponytail on the API's
		// NTLM rule); the failed one is found in the operation list. Check
		// the name here once the store keeps the last deletion's ID.
		refusedNaming("turning NTLM on once the deletion failed", entity.OperationID{})

		raw(good)
		second := started("PUT identity.ntlm off after the failed deletion", w.putSetting(t, w.admin, ntlm, "false"))
		if second == first {
			t.Fatalf("the turn-off after the failure answered the failed deletion %v, want a new one", uuid.UUID(first))
		}
		if op := ended(t, w.h, second); op.State != entity.OpSucceeded {
			t.Fatalf("the second deletion: ended %v (%s), want succeeded", op.State, op.Error)
		}

		// Each sweep turn is released by an advance; the store's time, which
		// the sweep reads, catches up with the clock on real time.
		w.h.Clock().Advance(8 * 24 * time.Hour)
		storeReached(t, w.h, w.h.Clock().Now())
		gone := func(id entity.OperationID) bool {
			_, found, err := w.h.Store().ControlPlane().Operation(t.Context(), id)
			if err != nil && !merrs.IsNotFoundError(err) {
				t.Fatalf("read deletion %v: %v", uuid.UUID(id), err)
			}
			return !found
		}
		deadline := time.Now().Add(30 * time.Second)
		for !gone(first) || !gone(second) {
			if time.Now().After(deadline) {
				t.Fatalf("8 days after they ended, within 30 s of sweep turns: deletion %v gone %v, %v gone %v, want both swept",
					uuid.UUID(first), gone(first), uuid.UUID(second), gone(second))
			}
			w.h.Clock().Advance(sweepInterval)
			awaitSweep(t, swept)
		}
		// The 8 days expired the admin's token, so sign in again at the new time.
		admin := account{p: w.admin.p, token: w.tokens(t, "admin", adminPassword).Access}
		if r := w.putSetting(t, admin, ntlm, "true"); r.status != http.StatusOK {
			t.Errorf("PUT identity.ntlm on once the deletion completed and was swept: answered %d %s, want 200", r.status, r.body)
		}
	})
}
