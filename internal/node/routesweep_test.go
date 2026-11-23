package node_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/keytab"

	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// storeC is a remote store the applied sweep configures and leaves empty.
var storeC = [16]byte{0: 'c', 15: 1}

// configureJSON configures store by its endpoint, its value JSON as the API
// writes one.
func configureJSON(t *testing.T, h *nodetest.Harness, store [16]byte) {
	t.Helper()
	s := entity.Setting{Scope: entity.SettingScope{Kind: entity.ScopeRemote, ID: store}, Key: "remote.endpoint", Value: []byte(`"https://store.invalid"`)}
	if _, err := h.Store().ControlPlane().PutSetting(t.Context(), s, 0); err != nil {
		t.Fatalf("configure the remote store: %v", err)
	}
}

// sweepSetting is the setting the sweeps write at every scope.
const sweepSetting = "gc.schedule"

// sweepFixture is the records the sweeps' paths name, each existing, so a
// read by a role that holds its right answers 200.
type sweepFixture struct {
	share, namespace, emptyNamespace, group, op string
	user, member                                entity.Principal
}

// fill names a route's {id} and {sub} by the records of f, the caller's own
// account for a route on the caller's own tokens.
func (f sweepFixture) fill(path string, own entity.Principal, node string) string {
	ids := map[string]string{"shares": f.share, "namespaces": f.namespace, "groups": f.group, "operations": f.op,
		"users": uuid.UUID(f.user.ID).String(), "stores": uuid.UUID(storeA).String(), "nodes": node}
	subs := map[string]string{"grants": uuid.UUID(f.member.ID).String(), "quotas": uuid.UUID(f.member.ID).String(),
		"members": uuid.UUID(f.member.ID).String(), "netgroups": "labs", "adapters": "nfs", "settings": sweepSetting,
		"api-tokens": uuid.NewString()}
	parts := strings.Split(path, "/")
	for i, p := range parts {
		switch {
		case strings.HasPrefix(p, "{id}"):
			id := ids[parts[i-1]]
			if strings.HasPrefix(path, "/api/v1/users/{id}/api-tokens") {
				id = uuid.UUID(own.ID).String()
			}
			parts[i] = id + strings.TrimPrefix(p, "{id}")
		case p == "{sub}":
			parts[i] = subs[parts[i-1]]
		}
	}
	return strings.Join(parts, "/")
}

// seedSweep writes, through the store and as setup through the API, the
// records the sweeps' paths name: storeA configured by its endpoint and
// holding a credential, a share in a namespace of its own on it, a second
// namespace holding nothing, a setting at every scope, each value JSON as the
// API writes one, two users, a group, a netgroup, a grant, quotas and a user
// deletion pending.
func (w *wired) seedSweep(t *testing.T, setup string) sweepFixture {
	t.Helper()
	cp := w.h.Store().ControlPlane()
	node := entity.NodeID(w.h.NodeID())
	remote := entity.SettingScope{Kind: entity.ScopeRemote, ID: storeA}
	cred := entity.SecretRef(uuid.New())
	for _, st := range []entity.Setting{
		{Scope: remote, Key: "remote.endpoint", Value: []byte(`"https://store.invalid"`)},
		{Scope: remote, Key: "remote.credential", Value: []byte(strconv.Quote(uuid.UUID(cred).String()))},
		{Scope: remote, Key: sweepSetting, Value: []byte(`"daily"`)},
		{Scope: entity.SettingScope{Kind: entity.ScopeInstallation}, Key: sweepSetting, Value: []byte(`"daily"`)},
		{Scope: entity.SettingScope{Kind: entity.ScopeNode, ID: node}, Key: sweepSetting, Value: []byte(`"daily"`)},
	} {
		if _, err := cp.PutSetting(t.Context(), st, 0); err != nil {
			t.Fatalf("put setting %s: %v", st.Key, err)
		}
	}
	if err := cp.PutSecret(t.Context(), entity.Secret{ID: cred, Kind: entity.SecretRemoteCredential, Version: 1,
		Owner: entity.SecretOwner{Scope: remote, Key: "remote.credential"}, Value: []byte("sealed")}, 0); err != nil {
		t.Fatalf("put the store's credential: %v", err)
	}
	sh := createShare(t, w.h, "a", storeA)
	for _, sc := range []entity.SettingScope{{Kind: entity.ScopeNamespace, ID: sh.Namespaces[0].Namespace}, {Kind: entity.ScopeShare, ID: sh.ID}} {
		if _, err := cp.PutSetting(t.Context(), entity.Setting{Scope: sc, Key: sweepSetting, Value: []byte(`"daily"`)}, 0); err != nil {
			t.Fatalf("put setting %s: %v", sweepSetting, err)
		}
	}
	member, _ := putUser(t, w.h, "carol")
	f := sweepFixture{share: sh.ID.String(), namespace: uuid.UUID(sh.Namespaces[0].Namespace).String(), member: member,
		emptyNamespace: uuid.UUID(namespaceOn(t, w.h, storeA)).String()}
	f.user, _ = putUser(t, w.h, "dave")
	op := entity.Operation{ID: entity.OperationID(uuid.New()), Kind: string(apiv1.OpUserDelete),
		Target: entity.ResourceRef{Kind: entity.ResourceUser, ID: make([]byte, len(entity.PrincipalID{}))}, State: entity.OpPending}
	if _, err := cp.PutOperation(t.Context(), op, 0); err != nil {
		t.Fatalf("create the operation: %v", err)
	}
	f.op = uuid.UUID(op.ID).String()
	r := w.send(t, http.MethodPost, "/api/v1/groups", setup, map[string]any{"spec": apiv1.GroupSpec{Name: "staff"}}, idemKey()...)
	var g apiv1.Group
	if r.status != http.StatusCreated || json.Unmarshal(r.body, &g) != nil {
		t.Fatalf("create group staff: answered %d %s, want 201", r.status, r.body)
	}
	f.group = g.ID
	m := uuid.UUID(member.ID).String()
	for _, c := range []struct{ path, body string }{
		{"/api/v1/groups/" + f.group + "/members/" + m, `{"spec":{}}`},
		{"/api/v1/netgroups/labs", `{"spec":{"members":["10.0.0.0/8"]}}`},
		{"/api/v1/shares/" + f.share + "/grants/" + m, `{"spec":{"access":"read"}}`},
		{"/api/v1/shares/" + f.share + "/quotas/" + m, quotaBody},
		{"/api/v1/shares/" + f.share + "/quota", quotaBody},
	} {
		if r := w.send(t, http.MethodPut, c.path, setup, json.RawMessage(c.body), "If-None-Match", "*"); r.status/100 != 2 {
			t.Fatalf("PUT %s: answered %d %s, want it written", c.path, r.status, r.body)
		}
	}
	return f
}

// quotaBody is a quota's PUT body the API accepts.
const quotaBody = `{"spec":{"hard":{"bytes":1,"files":0},"soft":{"bytes":0,"files":0},"grace_seconds":0,"advisory":{"bytes":0,"files":0}}}`

// sweepCall is one call of the role sweep and what it answered.
type sweepCall struct {
	who                 account
	role, need          entity.APIRole
	route, method, path string
	reply               authReply
}

// §5.2 roles, its sweep, and §6 audit, its refused half: every documented
// route but the public ones, called by a viewer, an operator and an admin
// with no body and no precondition on records that exist, answers as §5.2
// says: a role under the route's least is refused, 404 where it may not
// know of what the route reaches and 403 elsewhere; a role holding it is
// never refused for its role, and reads the record 200. Each call writes
// exactly one audit record under its caller, but a read that answered.
// Catches a route missing its check, and a refusal with no record.
func TestWiredRoleSweep(t *testing.T) {
	onBackends(t, func(t *testing.T, memory bool) {
		w := newWired(t, memory, nil)
		_, setup := w.h.SignedIn(t, w.addr, "setup", entity.RoleAdmin)
		f := w.seedSweep(t, setup.Access)
		node := entity.ShareID(w.h.NodeID()).String()
		var calls []sweepCall
		for _, who := range []struct {
			a    account
			role entity.APIRole
		}{{w.viewer, entity.RoleViewer}, {w.operator, entity.RoleOperator}, {w.admin, entity.RoleAdmin}} {
			for _, op := range documented(t) {
				need := leastRole[op.key()]
				if need == entity.RoleNone {
					continue
				}
				// A route whose role is its operation's kind's needs the
				// fixture operation's, a user's deletion, an operator's; it
				// checks that role in its handler, after the key every POST
				// carries.
				var hdr []string
				if op.byKind {
					need, hdr = entity.RoleOperator, idemKey()
				}
				path := f.fill(op.path, who.a.p, node)
				calls = append(calls, sweepCall{who.a, who.role, need, op.key(), op.method, path, w.send(t, op.method, path, who.a.token, nil, hdr...)})
			}
		}
		for _, c := range calls {
			what := c.role.String() + " " + c.method + " " + c.path
			switch {
			case c.role < c.need && c.role < unreadable[c.route]:
				wantStatus(t, what, c.reply, http.StatusNotFound, codeNotFound)
			case c.role < c.need:
				wantStatus(t, what, c.reply, http.StatusForbidden, apiv1.CodeForbidden)
			case c.reply.status == http.StatusForbidden || c.reply.status == http.StatusNotFound && c.method != http.MethodGet:
				t.Errorf("%s: answered %d %s, want the role's right held", what, c.reply.status, c.reply.body)
			case c.method == http.MethodGet && c.reply.status != http.StatusOK:
				t.Errorf("%s: answered %d %s, want 200", what, c.reply.status, c.reply.body)
			}
		}

		reader := w.tokens(t, "admin", adminPassword).Access
		for _, who := range []account{w.viewer, w.operator, w.admin} {
			got := map[string]int{}
			for _, rec := range w.auditsBy(t, reader, who.p) {
				rq := requestOf(t, rec)
				got[rq.Method+" "+rq.Path]++
			}
			for _, c := range calls {
				if c.who.p != who.p {
					continue
				}
				want := 1
				if c.method == http.MethodGet && c.reply.status < 400 {
					want = 0
				}
				if n := got[c.method+" "+c.path]; n != want {
					t.Errorf("%s %s %s, answered %d: %d audit records, want %d", c.role, c.method, c.path, c.reply.status, n, want)
				}
			}
		}
		if n := series(t, w.h, "dittofs_api_unmapped_errors_total", ""); n != 0 {
			t.Errorf("dittofs_api_unmapped_errors_total after the sweep: %v, want 0", n)
		}
	})
}

// wiredWrite is one write the applied sweep makes: the documented route it
// calls, its path, body and headers, the account it is sent as, and the
// secret values its body or answer carries, which no audit record may hold.
// kept marks a DELETE that answers 202 and leaves its record for the
// operation it starts to remove.
type wiredWrite struct {
	route, path string
	body        any
	hdr         []string
	by          account
	secrets     []string
	kept        bool
	reply       authReply // what it answered, set once sent
}

// waitingWrite is a mutating route that cannot yet succeed on this node: the
// write that would apply it, the refusal it answers today, and what it waits
// on.
type waitingWrite struct {
	wiredWrite
	status int
	code   apiv1.Code
	why    string
}

// endOperation writes the operation id names ended in state through the
// store, rereading it on a write the runner raced.
func (w *wired) endOperation(t *testing.T, id string, state entity.OpState) {
	t.Helper()
	oid, err := uuid.Parse(id)
	if err != nil {
		t.Fatalf("operation ID %q: %v", id, err)
	}
	cp := w.h.Store().ControlPlane()
	for range 10 {
		op, found, err := cp.Operation(t.Context(), entity.OperationID(oid))
		if err != nil || !found {
			t.Fatalf("read operation %s: found %v, %v", id, found, err)
		}
		gen := op.Generation
		op.State = state
		if _, err := cp.PutOperation(t.Context(), op, gen); err == nil {
			return
		}
	}
	t.Fatalf("operation %s: not ended in ten writes", id)
}

// applier sends writes, wanting each applied, and keeps what each answered.
type applier struct {
	t    *testing.T
	w    *wired
	done []wiredWrite
}

// do sends d as d.by, wanting 2xx, and returns its answer.
func (a *applier) do(d wiredWrite) authReply {
	a.t.Helper()
	method, _, _ := strings.Cut(d.route, " ")
	r := a.w.send(a.t, method, d.path, d.by.token, d.body, d.hdr...)
	if r.status/100 != 2 {
		a.t.Fatalf("%s %s: answered %d %s, want it applied", method, d.path, r.status, r.body)
	}
	d.reply = r
	// The caller's own bearer token is a secret no record may hold.
	d.secrets = append(d.secrets, d.by.token)
	a.done = append(a.done, d)
	return r
}

// tagIn is the generation the ETag v names, quoted, and false for none.
func tagIn(v string) (uint64, bool) {
	n, err := strconv.ParseUint(strings.Trim(v, `"`), 10, 64)
	return n, err == nil && v != ""
}

// generationsHeld fails unless rec's generations are those d's call saw:
// before, the one its If-Match named, or 0 under If-None-Match: *; after,
// the one its answer's ETag names, 0 for a record its DELETE removed, and
// for a DELETE whose record stays for the operation it started, the one
// before.
func generationsHeld(t *testing.T, d wiredWrite, rec apiv1.AuditRecord) {
	t.Helper()
	method, _, _ := strings.Cut(d.route, " ")
	what := method + " " + d.path
	for i := 0; i+1 < len(d.hdr); i += 2 {
		switch d.hdr[i] {
		case "If-Match":
			if n, ok := tagIn(d.hdr[i+1]); ok && rec.Before != n {
				t.Errorf("%s: audited generation before %d, want %d, the one its If-Match named", what, rec.Before, n)
			}
		case "If-None-Match":
			if rec.Before != 0 {
				t.Errorf("%s: audited generation before %d, want 0 for a record it created", what, rec.Before)
			}
		}
	}
	switch n, ok := tagIn(d.reply.header.Get("ETag")); {
	case ok && rec.After != n:
		t.Errorf("%s: audited generation after %d, want %d, its answer's ETag", what, rec.After, n)
	case !ok && method == http.MethodDelete && d.kept && rec.After != rec.Before:
		t.Errorf("%s: audited generations %d before and %d after, want the record kept for the removal it started", what, rec.Before, rec.After)
	case !ok && method == http.MethodDelete && !d.kept && rec.After != 0:
		t.Errorf("%s: audited generation after %d, want 0 for the record it removed", what, rec.After)
	}
	// Sign-out, a password change and an action on the installation, which
	// starts an operation and writes no record of its own, carry no
	// generation.
	if rec.Before == 0 && rec.After == 0 && d.route != signOut && d.route != changePassword && !strings.HasPrefix(d.route, "POST /api/v1/installation:") {
		t.Errorf("%s: audited generations 0 before and 0 after, want the record's", what)
	}
}

// sweepApplied is §6 audit's applied half over every mutating route: each
// route but the public ones is either among done, applied, or among waits,
// never both and never neither. Each applied write wrote exactly one audit
// record, applied or naming the operation it started, with the generations
// its call saw and no secret value it carried. Each waiting write is sent as
// the admin, which holds every right, and must still answer the refusal it
// waits on, audited once: once the route can succeed here, this fails, and
// the route moves into the applied writes.
func sweepApplied(t *testing.T, w *wired, done []wiredWrite, waits []waitingWrite) {
	t.Helper()
	routes := map[string]int{}
	for _, d := range done {
		routes[d.route] |= 1
	}
	for _, c := range waits {
		routes[c.route] |= 2
	}
	for _, op := range documented(t) {
		if op.method == http.MethodGet || leastRole[op.key()] == entity.RoleNone {
			continue
		}
		switch routes[op.key()] {
		case 0:
			t.Errorf("%s: neither applied nor waiting; apply it, or say what it waits on", op.key())
		case 3:
			t.Errorf("%s: both applied and waiting", op.key())
		}
	}

	for i := range waits {
		c := &waits[i]
		c.by = w.admin
		method, _, _ := strings.Cut(c.route, " ")
		c.reply = w.send(t, method, c.path, c.by.token, c.body, c.hdr...)
		if c.reply.status != c.status || c.reply.problem(t).Code != c.code {
			t.Errorf("%s %s, waiting on %s: answered %d %s, want its refusal %d %s; once it succeeds, apply it in the sweep",
				method, c.path, c.why, c.reply.status, c.reply.body, c.status, c.code)
		}
	}

	reader := w.tokens(t, "admin", adminPassword).Access
	seen := map[entity.Principal]bool{}
	var records []apiv1.AuditRecord
	for _, d := range append(done, waitsOf(waits)...) {
		if !seen[d.by.p] {
			seen[d.by.p] = true
			records = append(records, w.auditsBy(t, reader, d.by.p)...)
		}
	}
	mine := func(method, path string) []apiv1.AuditRecord {
		var out []apiv1.AuditRecord
		for _, rec := range records {
			if rq := requestOf(t, rec); rq.Method == method && rq.Path == path {
				out = append(out, rec)
			}
		}
		return out
	}
	for _, c := range waits {
		method, _, _ := strings.Cut(c.route, " ")
		got := mine(method, c.path)
		if len(got) != 1 || got[0].Outcome != "refused: "+string(c.code) {
			t.Errorf("%s %s, refused: %d audit records %+v, want one refused %s", method, c.path, len(got), got, c.code)
		}
	}
	for _, d := range done {
		method, _, _ := strings.Cut(d.route, " ")
		got := mine(method, d.path)
		// Two writes of one route and path each write their own record.
		want := 0
		for _, o := range done {
			if o.route == d.route && o.path == d.path {
				want++
			}
		}
		if len(got) != want {
			t.Errorf("%s %s: %d audit records, want %d", method, d.path, len(got), want)
			continue
		}
		for _, rec := range got {
			if _, err := uuid.Parse(rec.Outcome); rec.Outcome != "applied" && err != nil {
				t.Errorf("%s %s: outcome %q, want applied or the operation it started", method, d.path, rec.Outcome)
			}
			generationsHeld(t, d, rec)
			raw, _ := json.Marshal(rec)
			for _, s := range d.secrets {
				if strings.Contains(string(raw), s) {
					t.Errorf("%s %s: its audit record holds a secret value the call carried: %s", method, d.path, raw)
				}
			}
		}
	}
}

func waitsOf(waits []waitingWrite) []wiredWrite {
	out := make([]wiredWrite, len(waits))
	for i, c := range waits {
		out[i] = c.wiredWrite
	}
	return out
}

// keytabOf is a keytab of one AES256 key for name in AD.EXAMPLE.
func keytabOf(t *testing.T, name string) []byte {
	t.Helper()
	kt := keytab.New()
	if err := kt.AddEntry(name, "AD.EXAMPLE", "pw", time.Unix(1, 0), 1, etypeID.AES256_CTS_HMAC_SHA1_96); err != nil {
		t.Fatal(err)
	}
	b, err := kt.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// §6 audit, its applied half: every mutating route that can succeed on this
// node is applied once through the API, and every other one answers the
// refusal it waits on (sweepApplied). Catches a change with no record.
//
// decision: three groups of routes wait rather than apply. The
// settings-backed writes (a setting at every scope, an adapter, a remote
// store, a namespace, a store's credential's delete) are refused by the
// empty settings table the node hands its API until the table is wired, and
// a store's first credential by the store's capability check, not built; the
// keytab's PUT needs the secret provider the node does not yet hand its API,
// and its DELETE a keytab only that PUT writes; :cancel has no success
// answer while no operation kind is cancellable. Each waits as a refused
// write, so the row fails the day its route succeeds, and the route then
// moves into the applied writes.
func TestWiredEveryWriteAudited(t *testing.T) {
	onBackends(t, func(t *testing.T, memory bool) {
		w := newWired(t, memory, nil)
		_, setup := w.h.SignedIn(t, w.addr, "setup", entity.RoleAdmin)
		f := w.seedSweep(t, setup.Access)
		const frankPassword, frankNext, erinPassword = "frank's first password", "frank's second password", "erin's password"
		w.h.Account(t, "frank", frankPassword, entity.RoleViewer)
		frank := account{token: w.tokens(t, "frank", frankPassword).Access}
		frank.p = w.principalNamed(t, "frank")

		ap := &applier{t: t, w: w}
		a := w.admin
		idOf := func(r authReply) string {
			var v struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(r.body, &v); err != nil || v.ID == "" {
				t.Fatalf("answer %s: no ID (%v)", r.body, err)
			}
			return v.ID
		}
		etag := func(path string) string {
			t.Helper()
			r := w.send(t, http.MethodGet, path, a.token, nil)
			if r.status != http.StatusOK {
				t.Fatalf("GET %s: answered %d %s, want 200", path, r.status, r.body)
			}
			return r.header.Get("ETag")
		}
		match := func(path string, more ...string) []string { return append([]string{"If-Match", etag(path)}, more...) }
		create := []string{"If-None-Match", "*"}

		r := ap.do(wiredWrite{by: a, route: "POST /api/v1/shares", path: "/api/v1/shares", hdr: idemKey(), body: apiv1.NewShare{
			Spec: apiv1.ShareSpec{Name: "audited", Path: "/audited", Namespace: f.namespace}, ExportPolicy: exportPolicy}})
		share := "/api/v1/shares/" + idOf(r)
		ap.do(wiredWrite{by: a, route: "PATCH /api/v1/shares/{id}", path: share, hdr: match(share), body: json.RawMessage(`{"spec":{"case_sensitivity":"sensitive"}}`)})
		ap.do(wiredWrite{by: a, route: "POST /api/v1/shares/{id}:quiesce", path: share + ":quiesce", hdr: idemKey()})
		ap.do(wiredWrite{by: a, route: "POST /api/v1/shares/{id}:resume", path: share + ":resume", hdr: idemKey()})
		policy := share + "/export-policy"
		ap.do(wiredWrite{by: a, route: "PUT /api/v1/shares/{id}/export-policy", path: policy, hdr: match(policy), body: map[string]any{"spec": exportPolicy}})
		ap.do(wiredWrite{by: a, route: "PATCH /api/v1/shares/{id}/export-policy", path: policy, hdr: match(policy), body: json.RawMessage(`{"spec":{"read_only":true}}`)})

		r = ap.do(wiredWrite{by: a, route: "POST /api/v1/users", path: "/api/v1/users", hdr: idemKey(), body: map[string]any{"spec": apiv1.UserSpec{Name: "erin"}}})
		erinID := idOf(r)
		erin := "/api/v1/users/" + erinID
		var u apiv1.User
		if err := json.Unmarshal(w.send(t, http.MethodGet, erin, a.token, nil).body, &u); err != nil {
			t.Fatalf("GET erin: %v", err)
		}
		ap.do(wiredWrite{by: a, route: "PUT /api/v1/users/{id}", path: erin, hdr: match(erin), body: map[string]any{"spec": u.Spec}})
		ap.do(wiredWrite{by: a, route: "PATCH /api/v1/users/{id}", path: erin, hdr: match(erin), body: json.RawMessage(`{"spec":{"email":"erin@example.com"}}`)})
		ap.do(wiredWrite{by: a, route: "POST /api/v1/users/{id}:set-password", path: erin + ":set-password", body: apiv1.SetPassword{Password: erinPassword}, secrets: []string{erinPassword}})

		r = ap.do(wiredWrite{by: a, route: "POST /api/v1/groups", path: "/api/v1/groups", hdr: idemKey(), body: map[string]any{"spec": apiv1.GroupSpec{Name: "ops"}}})
		group := "/api/v1/groups/" + idOf(r)
		var g apiv1.Group
		if err := json.Unmarshal(w.send(t, http.MethodGet, group, a.token, nil).body, &g); err != nil {
			t.Fatalf("GET the group: %v", err)
		}
		g.Spec.Name = "ops2"
		ap.do(wiredWrite{by: a, route: "PUT /api/v1/groups/{id}", path: group, hdr: match(group), body: map[string]any{"spec": g.Spec}})
		ap.do(wiredWrite{by: a, route: "PATCH /api/v1/groups/{id}", path: group, hdr: match(group), body: json.RawMessage(`{"spec":{"name":"ops3"}}`)})
		member := group + "/members/" + erinID
		r = ap.do(wiredWrite{by: a, route: "PUT /api/v1/groups/{id}/members/{sub}", path: member, hdr: create, body: json.RawMessage(`{"spec":{}}`)})
		ap.do(wiredWrite{by: a, route: "DELETE /api/v1/groups/{id}/members/{sub}", path: member, hdr: append([]string{"If-Match", r.header.Get("ETag")}, idemKey()...)})

		grant := share + "/grants/" + erinID
		r = ap.do(wiredWrite{by: a, route: "PUT /api/v1/shares/{id}/grants/{sub}", path: grant, hdr: create, body: json.RawMessage(`{"spec":{"access":"read"}}`)})
		ap.do(wiredWrite{by: a, route: "DELETE /api/v1/shares/{id}/grants/{sub}", path: grant, hdr: append([]string{"If-Match", r.header.Get("ETag")}, idemKey()...)})
		ap.do(wiredWrite{by: a, route: "PUT /api/v1/shares/{id}/quotas/{sub}", path: share + "/quotas/" + erinID, hdr: create, body: json.RawMessage(quotaBody)})
		ap.do(wiredWrite{by: a, route: "PUT /api/v1/shares/{id}/quota", path: share + "/quota", hdr: create, body: json.RawMessage(quotaBody)})
		r = ap.do(wiredWrite{by: a, route: "PUT /api/v1/netgroups/{sub}", path: "/api/v1/netgroups/web", hdr: create, body: json.RawMessage(`{"spec":{"members":["10.0.0.1"]}}`)})
		ap.do(wiredWrite{by: a, route: "DELETE /api/v1/netgroups/{sub}", path: "/api/v1/netgroups/web", hdr: append([]string{"If-Match", r.header.Get("ETag")}, idemKey()...)})

		tokens := "/api/v1/users/" + uuid.UUID(a.p.ID).String() + "/api-tokens"
		r = ap.do(wiredWrite{by: a, route: "POST /api/v1/users/{id}/api-tokens", path: tokens, body: apiv1.APITokenSpec{Expires: w.h.Clock().Now().Add(time.Hour)}})
		var tok apiv1.APIToken
		if err := json.Unmarshal(r.body, &tok); err != nil || tok.Status.Token == "" {
			t.Fatalf("the API token's answer %s: %v, want its value", r.body, err)
		}
		ap.done[len(ap.done)-1].secrets = append(ap.done[len(ap.done)-1].secrets, tok.Status.Token)
		ap.do(wiredWrite{by: a, route: "DELETE /api/v1/users/{id}/api-tokens/{sub}", path: tokens + "/" + tok.ID, hdr: append([]string{"If-Match", `"1"`}, idemKey()...)})

		ap.do(wiredWrite{by: a, route: "DELETE /api/v1/groups/{id}", path: group, hdr: match(group, idemKey()...)})
		ap.do(wiredWrite{by: a, route: "DELETE /api/v1/users/{id}", path: erin, hdr: match(erin, idemKey()...)})
		ap.do(wiredWrite{by: a, route: "DELETE /api/v1/shares/{id}", path: share, hdr: match(share, idemKey()...), kept: true})
		// The directory's actions start their operations here, whatever the
		// operations later meet; the join carries the administrator's
		// password, which no record may hold.
		const joinPassword = "adm1n-pw"
		r = ap.do(wiredWrite{by: a, route: "POST /api/v1/installation:rotate-machine-password", path: "/api/v1/installation:rotate-machine-password", hdr: idemKey()})
		// A join is refused while a rotation runs; the rotation is ended
		// failed, as a node with no domain controller leaves it.
		w.endOperation(t, idOf(r), entity.OpFailed)
		ap.do(wiredWrite{by: a, route: "POST /api/v1/installation:test-directory", path: "/api/v1/installation:test-directory", hdr: idemKey()})
		ap.do(wiredWrite{by: a, route: "POST /api/v1/installation:resolve-principal", path: "/api/v1/installation:resolve-principal", body: apiv1.ResolvePrincipalParams{Name: "bob@AD.EXAMPLE"}, hdr: idemKey()})
		ap.do(wiredWrite{by: a, route: "POST /api/v1/installation:join-domain", path: "/api/v1/installation:join-domain", secrets: []string{joinPassword, base64.StdEncoding.EncodeToString([]byte(joinPassword))},
			body: apiv1.JoinDomain{Spec: apiv1.JoinDomainSpec{Mode: apiv1.JoinOnline}, Credential: apiv1.JoinCredential{User: "Administrator@AD.EXAMPLE", Password: []byte(joinPassword)}}})
		ap.do(wiredWrite{by: frank, route: changePassword, path: "/api/v1/auth/password",
			body: apiv1.PasswordChange{Current: frankPassword, New: frankNext}, secrets: []string{frankPassword, frankNext}})
		frank.token = w.tokens(t, "frank", frankNext).Access
		ap.do(wiredWrite{by: frank, route: signOut, path: "/api/v1/auth/sign-out"})

		node := "/api/v1/nodes/" + entity.ShareID(w.h.NodeID()).String()
		store := "/api/v1/stores/" + uuid.UUID(storeA).String()
		setting := func(scope string) string { return scope + "/settings/" + sweepSetting }
		putSetting := json.RawMessage(`{"spec":{"value":"weekly"}}`)
		var waits []waitingWrite
		for _, sc := range []struct{ route, scope string }{
			{"/api/v1", "/api/v1"}, {"/api/v1/nodes/{id}", node}, {"/api/v1/namespaces/{id}", "/api/v1/namespaces/" + f.namespace},
			{"/api/v1/stores/{id}", store}, {"/api/v1/shares/{id}", "/api/v1/shares/" + f.share},
		} {
			path := setting(sc.scope)
			waits = append(waits,
				waitingWrite{wiredWrite: wiredWrite{route: "PUT " + sc.route + "/settings/{sub}", path: path, body: putSetting, hdr: match(path)}, status: http.StatusBadRequest, code: "unknown_key", why: "the settings table"},
				waitingWrite{wiredWrite: wiredWrite{route: "DELETE " + sc.route + "/settings/{sub}", path: path, hdr: match(path, idemKey()...)}, status: http.StatusBadRequest, code: "unknown_key", why: "the settings table"})
		}
		cred := store + "/credential"
		// A store holding no namespace and no credential, so neither its
		// delete nor a first credential is refused for what it holds.
		bare := "/api/v1/stores/" + uuid.UUID(storeC).String()
		configureJSON(t, w.h, storeC)
		waits = append(waits,
			waitingWrite{wiredWrite: wiredWrite{route: "PUT /api/v1/adapters/{sub}", path: "/api/v1/adapters/nfs", body: json.RawMessage(`{"spec":{"enabled":true,"settings":{"port":12049}}}`), hdr: match("/api/v1/adapters/nfs")}, status: http.StatusBadRequest, code: "unknown_key", why: "the settings table"},
			waitingWrite{wiredWrite: wiredWrite{route: "POST /api/v1/stores", path: "/api/v1/stores", body: json.RawMessage(`{"spec":{"settings":{"remote.endpoint":"https://s3.example","remote.bucket":"b"}}}`), hdr: idemKey()}, status: http.StatusBadRequest, code: "unknown_key", why: "the settings table"},
			waitingWrite{wiredWrite: wiredWrite{route: "DELETE /api/v1/stores/{id}", path: bare, hdr: match(bare, idemKey()...)}, status: http.StatusBadRequest, code: "unknown_key", why: "the settings table"},
			waitingWrite{wiredWrite: wiredWrite{route: "PUT /api/v1/stores/{id}/credential", path: bare + "/credential", body: map[string]any{"spec": apiv1.CredentialSpec{Value: []byte("a new credential")}}, hdr: []string{"If-None-Match", "*"}}, status: http.StatusConflict, code: "unproven", why: "the store's capability check"},
			waitingWrite{wiredWrite: wiredWrite{route: "DELETE /api/v1/stores/{id}/credential", path: cred, hdr: append([]string{"If-Match", `"1"`}, idemKey()...)}, status: http.StatusBadRequest, code: "unknown_key", why: "the settings table"},
			waitingWrite{wiredWrite: wiredWrite{route: "POST /api/v1/namespaces", path: "/api/v1/namespaces", body: json.RawMessage(`{"spec":{"store":"` + uuid.UUID(storeA).String() + `","settings":{"namespace.prefix":"p/"}}}`), hdr: idemKey()}, status: http.StatusBadRequest, code: "unknown_key", why: "the settings table"},
			waitingWrite{wiredWrite: wiredWrite{route: "DELETE /api/v1/namespaces/{id}", path: "/api/v1/namespaces/" + f.emptyNamespace, hdr: match("/api/v1/namespaces/"+f.emptyNamespace, idemKey()...)}, status: http.StatusBadRequest, code: "unknown_key", why: "the settings table"},
			waitingWrite{wiredWrite: wiredWrite{route: "PUT /api/v1/installation/keytab", path: "/api/v1/installation/keytab", body: map[string]any{"spec": map[string]any{"value": base64.StdEncoding.EncodeToString(keytabOf(t, "cifs/nas.ad.example"))}}, hdr: []string{"If-None-Match", "*"}},
				status: http.StatusServiceUnavailable, code: apiv1.CodeUnavailable, why: "the secret provider the node does not yet hand its API"},
			waitingWrite{wiredWrite: wiredWrite{route: "DELETE /api/v1/installation/keytab", path: "/api/v1/installation/keytab", hdr: append([]string{"If-Match", `"1"`}, idemKey()...)},
				status: http.StatusPreconditionFailed, code: apiv1.CodeGenerationMismatch, why: "a keytab, which only the keytab's PUT writes"},
			waitingWrite{wiredWrite: wiredWrite{route: "POST /api/v1/operations/{id}:cancel", path: "/api/v1/operations/" + f.op + ":cancel", hdr: idemKey()},
				status: http.StatusConflict, code: apiv1.CodeNotCancellable, why: "a cancellable operation kind"},
		)
		sweepApplied(t, w, ap.done, waits)
	})
}
