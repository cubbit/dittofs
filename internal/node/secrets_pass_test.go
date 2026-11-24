package node_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/settings"
)

// passCred is a test component's section: its single key holds a reference to
// a remote store's credential, and the component's apply resolves it, as a
// component that holds a secret does.
type passCred struct {
	Token entity.SecretRef `json:"token" class:"live"`
}

func (passCred) Validate() error {
	return nil
}

// passSecret is the test component that stands in for one holding a secret:
// its apply resolves the reference the section's row names and records what
// it resolved. Fail, when set, is the component's refusal of a value; behind
// it, gate holds the apply until the test opens it, before the resolve on
// every call after the first.
type passSecret struct {
	mu     sync.Mutex
	node   entity.NodeID
	p      *secrets.Provider
	values [][]byte
	fail   func(value []byte) bool
	gate   chan struct{}
}

// resolve is the reference the section's row names, resolved as the component
// resolves it, recorded as the value it now holds. The credential's kind is a
// password, a storage-role kind that rotates freely: the provider refuses a
// re-sealed remote credential until the location check RFC 13 §5.2 names is
// built, and a prunable kind it does not keep is what the rotation needs.
func (c *passSecret) resolve(ref entity.SecretRef) error {
	value, err := c.p.Resolve(
		context.Background(),
		secrets.Storage,
		entity.SecretPassword,
		entity.SecretOwner{Scope: installationScope, Key: "pass.token"},
		ref,
	)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.values = append(c.values, value)

	if c.fail != nil && c.fail(value) {
		return fmt.Errorf("the component refused the credential")
	}

	return nil
}

// apply is the declared section's apply: the gate, when the test set one,
// then the resolve, the component's own.
func (c *passSecret) apply(snap *settings.Snapshot) error {
	v, err := settings.Get[passCred](snap, "pass", c.node)
	if err != nil {
		return err
	}

	c.mu.Lock()
	after := len(c.values) > 0
	c.mu.Unlock()

	if after && c.gate != nil {
		<-c.gate
	}

	return c.resolve(v.Token)
}

// declaredPass is h's test component declared as a section with an apply,
// with its provider over the harness's store. Call it before Start.
func declaredPass(t *testing.T, h *nodetest.Harness, c *passSecret) {
	t.Helper()
	c.p = sealer(t, h)
	c.node = h.NodeID()
	h.Node().DeclareSections(node.Declared{
		Prefix: "pass",
		Section: settings.Of("pass", settings.At(entity.ScopeInstallation), passCred{},
			settings.SecretRef("Token", entity.SecretPassword)),
		Apply: c.apply,
	})
}

// versions is each version of ref still held, read as the node's pass does.
func heldVersions(t *testing.T, p *secrets.Provider, ref entity.SecretRef) []uint64 {
	t.Helper()

	vs, err := p.Versions(
		t.Context(),
		secrets.Storage,
		entity.SecretPassword,
		entity.SecretOwner{Scope: installationScope, Key: "pass.token"},
		ref,
	)
	if err != nil {
		t.Fatalf("read the credential's versions: %v", err)
	}

	out := make([]uint64, len(vs))
	for i, v := range vs {
		out[i] = v.Number
	}

	return out
}

// passValues is the values the test component resolved, in order.
func (c *passSecret) passValues() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Clone(c.values)
}

// turnWhile is the first of the next n status turns, one clock second each,
// whose status satisfies ok: n is the bound the rule under test sets on the
// node's clock, five turns for a rotation's report, and a turn past it fails
// the test; a failing apply's backoff never runs inside that bound.
func turnWhile(
	t *testing.T,
	h *nodetest.Harness,
	ch <-chan statusTurn,
	what string,
	n int,
	ok func(statusTurn) bool,
) statusTurn {
	t.Helper()

	for range n {
		if tr := pendingTurn(t, h, ch); ok(tr) {
			return tr
		}
	}

	t.Fatalf("no status turn %s within %d turns", what, n)

	return statusTurn{}
}

// statusSecretOf is the version the node's committed status reports for ref,
// and whether it reports ref at all.
func statusSecretOf(s entity.NodeStatus, ref entity.SecretRef) (uint64, bool) {
	for _, sv := range s.Secrets {
		if sv.Secret == ref {
			return sv.Version, true
		}
	}

	return 0, false
}

// A credential the node holds, rotated through the settings route, is
// reported at its new version once, then pruned: the old version stays
// resolvable until the status holding the new one commits, the prune is
// audited naming the reference and the version it dropped, and no log line
// or audit record carries a value.
func TestARotatedCredentialIsReportedThenPruned(t *testing.T) {
	var (
		mu  sync.Mutex
		buf bytes.Buffer
	)

	newNode := func(cfg node.Config) (*node.Node, error) {
		cfg.Logger = slog.New(slog.NewTextHandler(
			&guardedWriter{mu: &mu, b: &buf},
			&slog.HandlerOptions{Level: slog.LevelInfo},
		))

		return node.NewAuthenticating(admin)(cfg)
	}
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(newNode))
	started(t, h)
	putSetting(t, h, "identity.directory.address", `"ldaps://dc.corp.example"`)
	putSetting(t, h, "identity.directory.base", `"DC=corp,DC=example"`)
	putSetting(t, h, "identity.directory.bind_name", `"svc"`)
	putSetting(t, h, "identity.directory.realm", `"CORP.EXAMPLE"`)
	putSetting(t, h, "identity.directory.netbios_domain", `"CORP"`)
	caughtUp(t, h)

	addr, _ := h.Node().Addrs()
	put := func(what, value, tag string) (entity.SecretRef, string) {
		t.Helper()

		with, match := "If-None-Match", "*"
		if tag != "" {
			with, match = "If-Match", tag
		}

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPut,
			"http://"+addr+"/api/v1/settings/"+bindCredentialKey,
			strings.NewReader(`{"spec":{"value":"`+value+`"}}`))
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}

		req.Header.Set(with, match)

		resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}

		defer func() { _ = resp.Body.Close() }()

		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: answered %d %s, want 200", what, resp.StatusCode, b)
		}

		var got apiv1.Setting
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("%s: the answer %s: %v", what, b, err)
		}

		var ref apiv1.SecretRef
		if err := json.Unmarshal(got.Spec.Value, &ref); err != nil || ref.Ref == "" {
			t.Fatalf("%s: the value answers no reference, %s, err %v", what, got.Spec.Value, err)
		}

		id, err := entity.ParseShareID(ref.Ref)
		if err != nil {
			t.Fatalf("%s: the answered reference %s: %v", what, ref.Ref, err)
		}

		return entity.SecretRef(id), resp.Header.Get("ETag")
	}

	const v1, v2 = "route-rotation-first", "route-rotation-second"

	ref, tag := put("PUT the bind credential's first value", v1, "")

	caughtUp(t, h)

	owner := entity.SecretOwner{Scope: installationScope, Key: bindCredentialKey}

	p := sealer(t, h)

	vs, err := p.Versions(
		t.Context(), secrets.Protocol, entity.SecretDirectoryCredential, owner, ref)
	if err != nil ||
		len(vs) != 1 ||
		vs[0].Number != 1 {
		t.Fatalf(
			"the credential after the first put: %d versions, err %v; want version 1 alone",
			len(vs),
			err,
		)
	}

	ch := watchStatus(h)
	// Five turns of the clock: each rotation reports its new version within
	// that bound, one reconcile and one status interval each.
	turnWhile(t, h, ch, "the status holding version 1", 5, func(tr statusTurn) bool {
		_, reported := statusSecretOf(tr.s, ref)
		return tr.wrote && reported
	})

	if got, _ := statusSecretOf(readStatus(t, h), ref); got != 1 {
		t.Fatalf("the status once applied: reports version %d, want 1", got)
	}

	put("PUT the bind credential's second value", v2, tag)

	vs, err = p.Versions(
		t.Context(), secrets.Protocol, entity.SecretDirectoryCredential, owner, ref)
	if err != nil ||
		len(vs) != 2 ||
		vs[0].Number != 2 ||
		vs[1].Number != 1 {
		t.Fatalf(
			"the credential below the new version: %d versions, err %v; want 2 then 1",
			len(vs),
			err,
		)
	}

	if got, _ := statusSecretOf(readStatus(t, h), ref); got != 1 {
		t.Fatalf(
			"the status before the next turn: reports version %d, want 1, the last applied",
			got,
		)
	}

	// Five turns of the clock: a rotation reports its new version within
	// that bound, one reconcile and status interval each, and a report past
	// it fails this.
	turnWhile(t, h, ch, "the status holding version 2", 5, func(tr statusTurn) bool {
		got, reported := statusSecretOf(tr.s, ref)
		return tr.wrote && reported && got == 2
	})

	vs, err = p.Versions(
		t.Context(), secrets.Protocol, entity.SecretDirectoryCredential, owner, ref)
	if err != nil ||
		len(vs) != 1 ||
		vs[0].Number != 2 {
		t.Fatalf(
			"the credential once every node reports version 2: %d versions, err %v; want 2 alone",
			len(vs),
			err,
		)
	}

	cp := h.Store().ControlPlane()

	var audits []entity.AuditRecord

	for a, err := range cp.AuditRecords(t.Context(), entity.AuditFilter{}, nil) {
		if err != nil {
			t.Fatalf("read the audit records: %v", err)
		}

		if a.Target.Kind == entity.ResourceSecret && bytes.Equal(a.Target.ID, ref[:]) {
			audits = append(audits, a)
		}
	}

	hex := fmt.Sprintf(`{"secret":"%x","dropped":[1]}`, ref[:])
	if len(audits) != 1 || string(audits[0].Change) != hex {
		t.Fatalf(
			"the prune's audit records: got %d (%v), want one naming %s",
			len(audits),
			audits,
			hex,
		)
	}

	mu.Lock()
	logged := buf.String()
	mu.Unlock()

	if !strings.Contains(logged, fmt.Sprintf("secret=%x", ref[:])) {
		t.Errorf("the log lines: none names the reference %x", ref[:])
	}

	for _, record := range audits {
		if bytes.Contains(record.Change, []byte(v1)) || bytes.Contains(record.Change, []byte(v2)) {
			t.Error("an audit record carries a value")
		}
	}

	if strings.Contains(logged, v1) || strings.Contains(logged, v2) {
		t.Error("a log line carries a value")
	}
}

// guardedWriter is a buffer one goroutine writes the log through and the
// test reads.
type guardedWriter struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (w *guardedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.b.Write(p)
}

// A version sealed while the row that names it did not change still reaches
// its holding component: the pass runs the section's apply again, and the
// component resolves the new value.
func TestAMovedVersionRunsTheApplyAgain(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	c := &passSecret{}
	declaredPass(t, h, c)
	started(t, h)

	ref := entity.SecretRef(uuid.New())
	seal(t, h, c.p, secrets.Storage, entity.SecretPassword,
		entity.SecretOwner{Scope: installationScope, Key: "pass.token"}, ref, []byte("moved-first"))
	putSetting(t, h, "pass.token", refJSON(ref))
	caughtUp(t, h)

	if got := c.passValues(); len(got) != 1 || string(got[0]) != "moved-first" {
		t.Fatalf("the component after its first apply: resolved %q, want [moved-first]", got)
	}

	seal(
		t,
		h,
		c.p,
		secrets.Storage,
		entity.SecretPassword,
		entity.SecretOwner{
			Scope: installationScope,
			Key:   "pass.token",
		},
		ref,
		[]byte("moved-second"),
	)
	caughtUp(t, h)

	got := c.passValues()

	want := [][]byte{[]byte("moved-first"), []byte("moved-second")}
	if !slices.EqualFunc(got, want, bytes.Equal) {
		t.Fatalf(
			"the component after the rotated version: resolved %q, want [moved-first moved-second]",
			got,
		)
	}
}

// The version the node reports for a secret is the one its apply resolved
// when a seal lands under the pass: a version sealed while the apply runs
// moves the report only at the next pass, never above what the component
// holds.
func TestTheReportedVersionNeverPassesTheHeldOne(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	c := &passSecret{}
	c.p = sealer(t, h)
	c.node = h.NodeID()

	// held closes once the component has resolved the first value; gate
	// holds every apply after the first resolution, before its own resolve,
	// so the status commits the first report while the component still
	// holds the first value. Neither depends on how many applies the node
	// runs at Start, whose first passes resolve no reference yet.
	var (
		held    = make(chan struct{})
		once    sync.Once
		gate    = make(chan struct{})
		checker = func(snap *settings.Snapshot) error {
			v, err := settings.Get[passCred](snap, "pass", c.node)
			if err != nil {
				return err
			}

			if len(c.passValues()) > 0 {
				<-gate
			}

			if err := c.resolve(v.Token); err != nil {
				return err
			}

			once.Do(func() { close(held) })

			return nil
		}
	)
	h.Node().DeclareSections(node.Declared{
		Prefix: "pass",
		Section: settings.Of("pass", settings.At(entity.ScopeInstallation), passCred{},
			settings.SecretRef("Token", entity.SecretPassword)),
		Apply: checker,
	})
	ch := watchStatus(h)
	started(t, h)

	ref := entity.SecretRef(uuid.New())
	owner := entity.SecretOwner{Scope: installationScope, Key: "pass.token"}
	seal(
		t,
		h,
		c.p,
		secrets.Storage,
		entity.SecretPassword,
		owner,
		ref,
		[]byte("held-first"),
	)
	putSetting(t, h, "pass.token", refJSON(ref))
	h.Clock().Advance(time.Second)

	select {
	case <-held:
	case <-time.After(30 * time.Second):
		t.Fatal("no apply resolved the first value within 30 s")
	}

	// The pass records the held version after the apply returns, before the
	// positions move at the pass's end: wait for the positions before any
	// status turn is asked for the version.
	caughtUp(t, h)

	// The second version seals only once the component holds the first: the
	// pass has read the first version already.
	seal(
		t,
		h,
		c.p,
		secrets.Storage,
		entity.SecretPassword,
		owner,
		ref,
		[]byte("held-second"),
	)

	// The next apply, whichever pass runs it, waits before its resolve, so
	// the status turn that follows commits the version the component holds.
	turnAt(t, h, ch, "the status holding the applied version", func(tr statusTurn) bool {
		got, reported := statusSecretOf(tr.s, ref)
		return tr.wrote && reported && got == 1
	})

	if got := c.passValues(); len(got) != 1 || string(got[0]) != "held-first" {
		t.Fatalf("the component while the seal landed: resolved %q, want [held-first] alone", got)
	}

	close(gate)
	caughtUp(t, h)

	turnAt(t, h, ch, "the status reporting the applied version", func(tr statusTurn) bool {
		got, reported := statusSecretOf(tr.s, ref)
		return tr.wrote && reported && got == 2
	})

	if got := c.passValues(); len(got) != 2 || string(got[1]) != "held-second" {
		t.Fatalf("the component once the next pass ran: resolved %q, want both values", got)
	}
}

// A secret broken under a section that keeps running (its version rewritten
// corrupt, the row untouched) refuses the section: the row is reported
// refused, the component keeps its last value and is not applied again, the
// status keeps the held version, and nothing is pruned.
func TestASecretThatNoLongerOpensRefusesItsSection(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	c := &passSecret{}
	declaredPass(t, h, c)
	ch := watchStatus(h)
	started(t, h)

	ref := entity.SecretRef(uuid.New())
	owner := entity.SecretOwner{Scope: installationScope, Key: "pass.token"}
	seal(
		t,
		h,
		c.p,
		secrets.Storage,
		entity.SecretPassword,
		owner,
		ref,
		[]byte("corrupt-first"),
	)
	putSetting(t, h, "pass.token", refJSON(ref))
	caughtUp(t, h)

	if got := c.passValues(); len(got) != 1 || string(got[0]) != "corrupt-first" {
		t.Fatalf("the component after its first apply: resolved %q, want [corrupt-first]", got)
	}

	// The corrupt version: the same kind and owner as the first, one byte of
	// the sealed value flipped, written past every check.
	sec, err := c.p.Seal(t.Context(), secrets.Storage, entity.SecretPassword,
		owner, ref, []byte("corrupt-second"), secrets.Meta{})
	if err != nil {
		t.Fatalf("seal the corrupt version: %v", err)
	}

	tampered := slices.Clone(sec.Value)
	tampered[len(tampered)/2] ^= 0xff
	sec.Value = tampered

	err = h.Store().
		ControlPlane().
		Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
			return tx.PutSecret(sec, 1)
		})
	if err != nil {
		t.Fatalf("store the corrupt version: %v", err)
	}

	caughtUp(t, h)

	want := entity.SettingRef(installationScope, "pass.token")

	refused := h.Node().Refused()
	if len(refused) != 1 || !hasRef(rowsOf(refused), want) {
		t.Fatalf("refused records: got %s, want the row %s alone",
			describe(refused), want)
	}

	if got := c.passValues(); len(got) != 1 {
		t.Fatalf(
			"the component under the broken secret: resolved %q, want its first value alone",
			got,
		)
	}

	v, err := settings.Get[passCred](h.Node().Settings(), "pass", h.NodeID())
	if err != nil || v.Token != ref {
		t.Fatalf(
			"the published setting under the broken secret: got %v,"+
				" want the row's reference, err %v",
			v.Token,
			err,
		)
	}

	turnAt(
		t,
		h,
		ch,
		"the status holding the version under the broken secret",
		func(tr statusTurn) bool {
			got, reported := statusSecretOf(tr.s, ref)
			return tr.wrote && reported && got == 1
		},
	)

	if _, err := h.Store().ControlPlane().Secret(t.Context(), ref, 1); err != nil {
		t.Errorf("the held version after the broken one: %v, want it still resolvable", err)
	}
}

// A bind credential that names a secret of another kind refuses identity's
// section: the row is reported refused and identity keeps the directory it
// last accepted.
func TestABindCredentialOfAnotherKindRefusesIdentity(t *testing.T) {
	d := identitytest.NewDirectory("DC=corp,DC=example")
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewWithDirectory(d.Dial, nil)))
	p := sealer(t, h)
	started(t, h)

	ref := entity.SecretRef(uuid.New())
	seal(
		t,
		h,
		p,
		secrets.Protocol,
		entity.SecretDirectoryCredential,
		entity.SecretOwner{
			Scope: installationScope,
			Key:   bindCredentialKey,
		},
		ref,
		[]byte("bind-first"),
	)
	putSetting(t, h, "identity.directory.address", `"ldaps://dc.corp.example"`)
	putSetting(t, h, "identity.directory.base", `"DC=corp,DC=example"`)
	putSetting(t, h, "identity.directory.bind_name", `"svc"`)
	putSetting(t, h, "identity.directory.realm", `"CORP.EXAMPLE"`)
	putSetting(t, h, "identity.directory.netbios_domain", `"CORP"`)
	putSetting(t, h, "identity.directory.bind_credential", refJSON(ref))
	caughtUp(t, h)

	if got := h.Node().AppliedIdentity().Directory; got == nil ||
		got.Address != "ldaps://dc.corp.example" {
		t.Fatalf("identity before the rewrite: holds directory %+v, want the written record", got)
	}

	// The row rewritten past the write check, naming a secret of another
	// kind than the bind credential a directory's row names.
	foreign := entity.SecretRef(uuid.New())
	seal(
		t,
		h,
		p,
		secrets.Storage,
		entity.SecretRemoteCredential,
		entity.SecretOwner{
			Scope: installationScope,
			Key:   bindCredentialKey,
		},
		foreign,
		[]byte("of another kind"),
	)
	putRowGen(
		t,
		h,
		installationScope,
		"identity.directory.bind_credential",
		[]byte(refJSON(foreign)),
	)
	caughtUp(t, h)

	want := entity.SettingRef(installationScope, "identity.directory.bind_credential")

	refused := h.Node().Refused()
	if !hasRef(rowsOf(refused), want) {
		t.Fatalf(
			"refused records: got %s, want the bind credential's row among them",
			describe(refused),
		)
	}

	if got := h.Node().AppliedIdentity().Directory; got == nil ||
		got.Address != "ldaps://dc.corp.example" || got.BindCredential != ref {
		t.Fatalf(
			"identity after the rewrite: holds directory %+v, want the record it last accepted",
			got,
		)
	}
}

// rowsOf is each refusal's record, as hasRef takes them.
func rowsOf(refused []entity.RefusedRecord) []entity.ResourceRef {
	out := make([]entity.ResourceRef, len(refused))
	for i, f := range refused {
		out[i] = f.Record
	}

	return out
}

// An apply that fails once the version moved keeps the held version: the
// status reports the old one, the new version stays beside it, and only an
// apply that passes moves the report and then prunes.
func TestAFailedApplyKeepsTheHeldVersion(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	c := &passSecret{
		fail: func(value []byte) bool {
			return string(value) == "failed-second"
		},
	}
	declaredPass(t, h, c)
	ch := watchStatus(h)
	started(t, h)

	ref := entity.SecretRef(uuid.New())
	seal(
		t,
		h,
		c.p,
		secrets.Storage,
		entity.SecretPassword,
		entity.SecretOwner{
			Scope: installationScope,
			Key:   "pass.token",
		},
		ref,
		[]byte("failed-first"),
	)
	putSetting(t, h, "pass.token", refJSON(ref))
	caughtUp(t, h)

	seal(
		t,
		h,
		c.p,
		secrets.Storage,
		entity.SecretPassword,
		entity.SecretOwner{
			Scope: installationScope,
			Key:   "pass.token",
		},
		ref,
		[]byte("failed-second"),
	)
	caughtUp(t, h)

	turnAt(t, h, ch, "the status under the failed apply", func(tr statusTurn) bool {
		got, reported := statusSecretOf(tr.s, ref)
		return tr.wrote && reported && got == 1
	})

	if got := heldVersions(t, c.p, ref); !slices.Equal(got, []uint64{2, 1}) {
		t.Fatalf("the versions under the failed apply: got %v, want 2 then 1, nothing pruned", got)
	}

	if got := gaugeValue(t, h, "dittofs_config_secrets_behind"); got != 1 {
		t.Fatalf("secrets behind under the failed apply: got %v, want 1", got)
	}

	c.mu.Lock()
	c.fail = nil
	c.mu.Unlock()

	// The failed applies left reconcile on its backoff, whose wait only the
	// node's clock releases: wake it, as a write does, and await the pass the
	// wake handed it end, so the status turns that follow carry what it
	// applied, never the scan outrunning the pass's last write.
	moved := h.Node().Moved()
	h.Node().Wake()

	select {
	case <-moved:
	case <-time.After(30 * time.Second):
		t.Fatal("no reconcile pass ended within 30 s of the wake with the refusal cleared")
	}

	turnAt(t, h, ch, "the status once the apply passes", func(tr statusTurn) bool {
		got, reported := statusSecretOf(tr.s, ref)
		return tr.wrote && reported && got == 2
	})

	if got := heldVersions(t, c.p, ref); !slices.Equal(got, []uint64{2}) {
		t.Fatalf(
			"the versions once the apply passes: got %v, want 2 alone, pruned below the report",
			got,
		)
	}
}

// gaugeValue is the unlabelled gauge family's value from the node's registry.
func gaugeValue(t *testing.T, h *nodetest.Harness, name string) float64 {
	t.Helper()

	fams, err := h.Metrics().Registry().Gather()
	if err != nil {
		t.Fatalf("gather the node's metrics: %v", err)
	}

	for _, fam := range fams {
		if fam.GetName() != name {
			continue
		}

		if len(fam.Metric) != 1 || len(fam.Metric[0].Label) != 0 {
			t.Fatalf(
				"the %s family: %d metrics with labels, want one alone with none",
				name,
				len(fam.Metric),
			)
		}

		return fam.Metric[0].GetGauge().GetValue()
	}

	t.Fatalf("the %s family: absent from the registry", name)

	return 0
}

// config_secrets_behind counts the references the node holds at an older
// version than the store's current one, read at the scrape: it moves when a
// seal lands, before the next pass applies it, and back to zero once it does.
func TestSecretsBehindCountsUntilTheNextPass(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	c := &passSecret{}
	declaredPass(t, h, c)
	started(t, h)

	ref := entity.SecretRef(uuid.New())
	seal(
		t,
		h,
		c.p,
		secrets.Storage,
		entity.SecretPassword,
		entity.SecretOwner{
			Scope: installationScope,
			Key:   "pass.token",
		},
		ref,
		[]byte("counted-first"),
	)
	putSetting(t, h, "pass.token", refJSON(ref))
	caughtUp(t, h)

	if got := gaugeValue(t, h, "dittofs_config_secrets_behind"); got != 0 {
		t.Fatalf("secrets behind at the held version: got %v, want 0", got)
	}

	seal(
		t,
		h,
		c.p,
		secrets.Storage,
		entity.SecretPassword,
		entity.SecretOwner{
			Scope: installationScope,
			Key:   "pass.token",
		},
		ref,
		[]byte("counted-second"),
	)

	if got := gaugeValue(t, h, "dittofs_config_secrets_behind"); got != 1 {
		t.Fatalf("secrets behind before the pass: got %v, want 1", got)
	}

	caughtUp(t, h)

	if got := gaugeValue(t, h, "dittofs_config_secrets_behind"); got != 0 {
		t.Fatalf("secrets behind once applied: got %v, want 0", got)
	}
}

// A keytab is reported at the version identity applied and its old versions
// are never pruned: both stay resolvable once every node reports the new one.
func TestAKeytabIsReportedAndKept(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	p := sealer(t, h)
	started(t, h)

	owner := entity.SecretOwner{Scope: installationScope, Key: "identity.keytab"}
	ref := entity.SecretRef(uuid.New())
	seal(
		t,
		h,
		p,
		secrets.Protocol,
		entity.SecretKeytab,
		owner,
		ref,
		nodeKeytab(t, "cifs/nas.ad.example", "AD.EXAMPLE"),
	)
	putSetting(t, h, "identity.keytab", refJSON(ref))
	caughtUp(t, h)

	seal(
		t,
		h,
		p,
		secrets.Protocol,
		entity.SecretKeytab,
		owner,
		ref,
		nodeKeytab(t, "cifs/nas.ad.example", "AD.EXAMPLE"),
	)
	caughtUp(t, h)

	ch := watchStatus(h)
	turnAt(t, h, ch, "the status reporting the rotated keytab", func(tr statusTurn) bool {
		return len(tr.s.Secrets) > 0 &&
			slices.ContainsFunc(tr.s.Secrets, func(sv entity.SecretVersion) bool {
				return sv.Secret == ref && sv.Version == 2
			})
	})

	s := readStatus(t, h)

	var got []entity.SecretVersion

	for _, sv := range s.Secrets {
		if sv.Secret == ref {
			got = append(got, sv)
		}
	}

	_, applied, _ := h.Node().AppliedKeytab()
	if len(got) != 1 || got[0].Version != applied {
		t.Fatalf(
			"the keytab's status entries: got %v, want one at the applied version %d",
			got,
			applied,
		)
	}

	if got := gaugeValue(t, h, "dittofs_config_secrets_behind"); got != 0 {
		t.Fatalf("secrets behind for a kept kind: got %v, want 0", got)
	}

	if v, err := h.Store().ControlPlane().Secret(t.Context(), ref, 1); err != nil ||
		v.Version != 1 {
		t.Errorf(
			"the keytab's first version after the report: %v, err %v, want it still held",
			v.Version,
			err,
		)
	}

	if v, err := h.Store().ControlPlane().Secret(t.Context(), ref, 2); err != nil ||
		v.Version != 2 {
		t.Errorf(
			"the keytab's second version after the report: %v, err %v, want it held",
			v.Version,
			err,
		)
	}
}

// Two rotations sealed before one pass apply together: the component binds
// the newest version at that one apply, and once the status commits it every
// older version falls, its audit record naming each of them, so the old
// credential may be revoked at its service without holding the passes back.
func TestTwoRotationsBeforeAPassDropBothOldVersions(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	c := &passSecret{}
	declaredPass(t, h, c)
	started(t, h)

	ref := entity.SecretRef(uuid.New())
	owner := entity.SecretOwner{Scope: installationScope, Key: "pass.token"}
	seal(t, h, c.p, secrets.Storage, entity.SecretPassword, owner, ref, []byte("twice-first"))
	putSetting(t, h, "pass.token", refJSON(ref))
	caughtUp(t, h)

	seal(t, h, c.p, secrets.Storage, entity.SecretPassword, owner, ref, []byte("twice-second"))
	seal(t, h, c.p, secrets.Storage, entity.SecretPassword, owner, ref, []byte("twice-third"))

	// Await the one pass that binds the third: it reads both rotations under
	// no version it ever applied, so it hands the component the section
	// once, at the move.
	moved := h.Node().Moved()
	h.Clock().Advance(time.Second)

	select {
	case <-moved:
	case <-time.After(30 * time.Second):
		t.Fatal("no reconcile pass read both rotations within 30 s")
	}

	// The turn that carries the report prunes below it: the equal-bytes
	// check runs after the commit's write lands, on the commit's own turn.
	ch := watchStatus(h)
	turnAt(t, h, ch, "the status reporting version 3", func(tr statusTurn) bool {
		return slices.ContainsFunc(tr.s.Secrets, func(sv entity.SecretVersion) bool {
			return sv == entity.SecretVersion{Secret: ref, Version: 3}
		})
	})

	if got := heldVersions(t, c.p, ref); !slices.Equal(got, []uint64{3}) {
		t.Fatalf(
			"the versions after both rotations applied: got %v,"+
				" want [3] alone, both old ones pruned",
			got,
		)
	}

	if got := c.passValues(); len(got) != 2 || string(got[1]) != "twice-third" {
		t.Fatalf(
			"the component under both rotations: resolved %q, want the first and third values",
			got,
		)
	}

	cp := h.Store().ControlPlane()
	want := fmt.Sprintf(`{"secret":"%x","dropped":[1,2]}`, ref[:])

	var found int

	for a, err := range cp.AuditRecords(t.Context(), entity.AuditFilter{}, nil) {
		if err != nil {
			t.Fatalf("read the audit records: %v", err)
		}

		if a.Target.Kind == entity.ResourceSecret && bytes.Equal(a.Target.ID, ref[:]) {
			found++

			if string(a.Change) != want {
				t.Fatalf(
					"the prune's audit record: got %s, want %s naming both dropped versions",
					a.Change,
					want,
				)
			}
		}
	}

	if found != 1 {
		t.Fatalf("the prune's audit records: got %d for the reference, want one", found)
	}
}

// A reference the section's published value no longer names leaves the node's
// status: the status reports the referenced value alone, and a later rotation
// of the abandoned one moves neither it nor the count of secrets behind.
func TestAReferenceTheSectionNoLongerNamesLeavesTheStatus(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	c := &passSecret{}
	declaredPass(t, h, c)
	ch := watchStatus(h)
	started(t, h)

	owner := entity.SecretOwner{Scope: installationScope, Key: "pass.token"}
	old, now := entity.SecretRef(uuid.New()), entity.SecretRef(uuid.New())
	seal(t, h, c.p, secrets.Storage, entity.SecretPassword, owner, old, []byte("gone-first"))
	putSetting(t, h, "pass.token", refJSON(old))
	caughtUp(t, h)

	seal(t, h, c.p, secrets.Storage, entity.SecretPassword, owner, now, []byte("kept-first"))
	putRowGen(t, h, installationScope, "pass.token", []byte(refJSON(now)))
	caughtUp(t, h)

	turnAt(t, h, ch, "the status of the value the section names now", func(tr statusTurn) bool {
		return tr.wrote &&
			len(tr.s.Secrets) == 1 &&
			tr.s.Secrets[0].Secret == now &&
			tr.s.Secrets[0].Version == 1
	})

	// The abandoned reference rotates again: this node holds none of it, so
	// nothing is behind and the reported entry stays the one the row names.
	seal(t, h, c.p, secrets.Storage, entity.SecretPassword, owner, old, []byte("gone-second"))

	if got := gaugeValue(t, h, "dittofs_config_secrets_behind"); got != 0 {
		t.Fatalf("secrets behind after a rotation of an unnamed reference: got %v, want 0", got)
	}

	if got := c.passValues(); len(got) != 2 || string(got[1]) != "kept-first" {
		t.Fatalf(
			"the component after the switch and the rotation: resolved %q,"+
				" want both, ending at kept-first",
			got,
		)
	}
}
