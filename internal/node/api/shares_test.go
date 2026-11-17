package api_test

import (
	"encoding/json"
	"net/http"
	"net/netip"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/google/uuid"
)

// policy is an export policy as a test writes it, every field stated.
var policy = apiv1.ExportPolicySpec{
	Flavors: []string{"kerberos", "sys"}, MinProof: "claimed", MinProtection: "none",
	Clients: []apiv1.ClientRule{{CIDR: "10.0.0.0/8", Access: "read-write"}, {Netgroup: "labs", Access: "read"}},
	Squash:  "root", SMB: apiv1.SMBShare{Encrypt: true, Hidden: true, ContinuouslyAvailable: true},
}

// stored is policy as the store holds it for share.
func stored(share entity.ShareID) entity.ExportPolicy {
	return entity.ExportPolicy{Share: share, Flavors: []entity.AuthFlavor{entity.FlavorKerberos, entity.FlavorSys},
		MinProof: entity.ProofClaimed, MinProtection: entity.ProtectionNone, Squash: entity.SquashRoot,
		Anonymous: entity.AnonymousPrincipal, SMB: entity.SMBEncrypt | entity.SMBHidden | entity.SMBContinuouslyAvailable,
		Clients: []entity.ClientRule{
			{Match: entity.ClientMatch{CIDR: netip.MustParsePrefix("10.0.0.0/8")}, Access: entity.AccessReadWrite},
			{Match: entity.ClientMatch{Netgroup: "labs"}, Access: entity.AccessRead}}}
}

// namespace writes a namespace on a remote store through the store, as the
// namespace routes, which fail closed until the settings table lands, would.
func (s *served) namespace(t *testing.T) string {
	t.Helper()
	id := entity.NamespaceID(uuid.New())
	if _, err := s.store.PutNamespace(t.Context(), entity.Namespace{ID: id, Store: uuid.New()}, 0); err != nil {
		t.Fatalf("put namespace: %v", err)
	}
	return uuid.UUID(id).String()
}

// jsonOf is v as a request body.
func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal the body %+v: %v", v, err)
	}
	return string(b)
}

// newShare is the create of a share named name, at /name, in ns, under
// policy.
func newShare(name, ns string) apiv1.NewShare {
	return apiv1.NewShare{Spec: apiv1.ShareSpec{Name: name, Path: "/" + name, Namespace: ns}, ExportPolicy: policy}
}

// post creates the share body names, and returns its ID, its path and the
// create's answer.
func (s *served) post(t *testing.T, body apiv1.NewShare) (entity.ShareID, string, apiv1.Share) {
	t.Helper()
	r := s.do(t, http.MethodPost, "/api/v1/shares", jsonOf(t, body), "Idempotency-Key", "create-"+body.Spec.Name)
	if r.status != http.StatusCreated {
		t.Fatalf("POST /shares %s: answered %d %s, want 201", body.Spec.Name, r.status, r.body)
	}
	loc := r.header.Get("Location")
	id, err := entity.ParseShareID(strings.TrimPrefix(loc, "/api/v1/shares/"))
	if err != nil {
		t.Fatalf("POST /shares %s: Location %q names no share: %v", body.Spec.Name, loc, err)
	}
	var got apiv1.Share
	if err := json.Unmarshal(r.body, &got); err != nil {
		t.Fatalf("POST /shares %s: body %s: %v", body.Spec.Name, r.body, err)
	}
	return id, loc, got
}

// createShare creates a share named name, at /name, in ns, and returns its
// ID and its path.
func (s *served) createShare(t *testing.T, name, ns string) (entity.ShareID, string) {
	t.Helper()
	id, loc, _ := s.post(t, newShare(name, ns))
	return id, loc
}

func (s *served) share(t *testing.T, id entity.ShareID) (entity.Share, entity.ExportPolicy) {
	t.Helper()
	sh, p, err := s.store.Share(t.Context(), id)
	if err != nil {
		t.Fatalf("read share %s: %v", id, err)
	}
	return sh, p
}

func (s *served) shareList(t *testing.T) uint64 {
	t.Helper()
	c, err := s.nodes.Counters(t.Context())
	if err != nil {
		t.Fatalf("read counters: %v", err)
	}
	return c.ShareList
}

func (s *served) shareCount(t *testing.T) int {
	t.Helper()
	n := 0
	for _, err := range s.store.Shares(t.Context()) {
		if err != nil {
			t.Fatalf("list shares: %v", err)
		}
		n++
	}
	return n
}

// removing marks the share being removed through the store.
func (s *served) removing(t *testing.T, id entity.ShareID) {
	t.Helper()
	sh, _ := s.share(t, id)
	if err := s.store.DeleteShare(t.Context(), id, entity.SnapshotsRefuse, sh.Generation); err != nil {
		t.Fatalf("mark share %s removed: %v", id, err)
	}
}

// putStatus writes a node's status through the store, registering the node
// first if it is not, since only a registered node's status counts.
func (s *served) putStatus(t *testing.T, st entity.NodeStatus) {
	t.Helper()
	if _, ok, err := s.nodes.Node(t.Context(), st.Node); err != nil || !ok {
		if _, err := s.db.RaiseNodeEpoch(t.Context(), st.Node); err != nil {
			t.Fatalf("register node %v: %v", st.Node, err)
		}
	}
	if err := s.nodes.PutNodeStatus(t.Context(), st); err != nil {
		t.Fatalf("put node %v's status: %v", st.Node, err)
	}
}

// decodeShare is a share's answer body.
func decodeShare(t *testing.T, what string, body []byte) apiv1.Share {
	t.Helper()
	var sh apiv1.Share
	if err := json.Unmarshal(body, &sh); err != nil {
		t.Fatalf("%s: body %s: %v", what, body, err)
	}
	return sh
}

// A share created in a namespace that does not exist, so on no remote
// store, is refused and commits nothing; created in one that does, it is
// enabled in that namespace, case-sensitive, with the export policy the
// request stated, no grant, and ShareList raised, and the create answers it.
func TestShareCreate(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	list := s.shareList(t)
	r := s.do(t, http.MethodPost, "/api/v1/shares", jsonOf(t, newShare("builds", uuid.NewString())), "Idempotency-Key", "k1")
	if p := r.problem(t); r.status != http.StatusBadRequest || p.Code != "invalid_argument" {
		t.Errorf("POST /shares in a namespace that does not exist: answered %d %q, want 400 invalid_argument", r.status, p.Code)
	}
	if n, l := s.shareCount(t), s.shareList(t); n != 0 || l != list {
		t.Errorf("after the refused create: %d shares, ShareList %d, want 0 and %d", n, l, list)
	}

	ns := s.namespace(t)
	id, _, got := s.post(t, newShare("builds", ns))
	sh, p := s.share(t, id)
	if sh.Name != "builds" || sh.Path != "/builds" || sh.State != entity.ShareEnabled || sh.Fold != entity.FoldIdentity ||
		len(sh.Namespaces) != 1 || uuid.UUID(sh.Namespaces[0].Namespace).String() != ns {
		t.Errorf("created share: %q at %q, %v, fold %v, namespaces %v, want builds at /builds, enabled, identity, in %s", sh.Name, sh.Path, sh.State, sh.Fold, sh.Namespaces, ns)
	}
	want := apiv1.Share{ID: id.String(), Generation: sh.Generation, Status: apiv1.ShareStatus{State: "enabled"},
		Spec: apiv1.ShareSpec{Name: "builds", Path: "/builds", Namespace: ns, CaseSensitivity: "sensitive"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("create's answer: %+v, want %+v", got, want)
	}
	wantPolicy := stored(id)
	wantPolicy.Version = p.Version
	if !reflect.DeepEqual(p, wantPolicy) {
		t.Errorf("created share's export policy: %+v, want %+v", p, wantPolicy)
	}
	if sh.GrantVersion != 0 {
		t.Errorf("created share's grant-list version: %d, want 0, no grant written", sh.GrantVersion)
	}
	if l := s.shareList(t); l <= list {
		t.Errorf("ShareList after the create: %d, want above %d", l, list)
	}
}

// A create that leaves the policy's minimums and squash empty gets none,
// none and root, and an empty client list admits every client.
func TestShareCreateDefaults(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	body := newShare("builds", s.namespace(t))
	body.ExportPolicy = apiv1.ExportPolicySpec{Flavors: []string{"sys"}, Clients: []apiv1.ClientRule{}}
	id, _, _ := s.post(t, body)
	_, p := s.share(t, id)
	if p.MinProof != entity.ProofNone || p.MinProtection != entity.ProtectionNone || p.Squash != entity.SquashRoot || len(p.Clients) != 0 {
		t.Errorf("defaulted policy: min_proof %v, min_protection %v, squash %v, clients %v, want none, none, root and none", p.MinProof, p.MinProtection, p.Squash, p.Clients)
	}
}

// A create naming no namespace ID, a case sensitivity that is not one, no
// flavour, no client rules, a value no field takes, a client rule that is
// not one or minimums that cannot be met together is refused naming the
// field, and nothing is committed.
func TestShareCreateRefuses(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	ns := s.namespace(t)
	rule := func(r apiv1.ClientRule) func(*apiv1.NewShare) {
		return func(b *apiv1.NewShare) { b.ExportPolicy.Clients = []apiv1.ClientRule{policy.Clients[0], r} }
	}
	for i, tc := range []struct {
		edit  func(*apiv1.NewShare)
		field string
	}{
		{func(b *apiv1.NewShare) { b.Spec.Namespace = "x" }, "spec.namespace"},
		{func(b *apiv1.NewShare) { b.Spec.CaseSensitivity = "maybe" }, "spec.case_sensitivity"},
		{func(b *apiv1.NewShare) { b.ExportPolicy.Flavors = nil }, "export_policy.flavors"},
		{func(b *apiv1.NewShare) { b.ExportPolicy.Flavors = []string{"ntlm"} }, "export_policy.flavors"},
		{func(b *apiv1.NewShare) { b.ExportPolicy.MinProtection = "sealed" }, "export_policy.min_protection"},
		{func(b *apiv1.NewShare) { b.ExportPolicy.MinProtection = "privacy" }, "export_policy.min_protection"},
		{func(b *apiv1.NewShare) { b.ExportPolicy.Clients = nil }, "export_policy.clients"},
		{rule(apiv1.ClientRule{CIDR: "10.0.0.0/33", Access: "read"}), "export_policy.clients[1]"},
		{rule(apiv1.ClientRule{Netgroup: "labs", Access: "write"}), "export_policy.clients[1]"},
		{rule(apiv1.ClientRule{CIDR: "10.0.0.0/8", Netgroup: "labs", Access: "read"}), "export_policy.clients[1]"},
		{rule(apiv1.ClientRule{Access: "read"}), "export_policy.clients[1]"},
	} {
		b := newShare("builds", ns)
		tc.edit(&b)
		r := s.do(t, http.MethodPost, "/api/v1/shares", jsonOf(t, b), "Idempotency-Key", "k"+strconv.Itoa(i))
		if pb := r.problem(t); r.status != http.StatusBadRequest || pb.Field != tc.field {
			t.Errorf("POST /shares refused at %s: answered %d field %q, want 400 naming %s", tc.field, r.status, pb.Field, tc.field)
		}
	}
	if n := s.shareCount(t); n != 0 {
		t.Errorf("after the refused creates: %d shares, want 0", n)
	}
}

// A case-insensitive share is folded by the upcase rule, reads as
// insensitive, and keeps it: a PATCH naming another case sensitivity is
// refused naming it.
func TestShareCaseSensitivityBound(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	body := newShare("builds", s.namespace(t))
	body.Spec.CaseSensitivity = "insensitive"
	id, loc, got := s.post(t, body)
	if sh, _ := s.share(t, id); sh.Fold != entity.FoldUpcase || got.Spec.CaseSensitivity != "insensitive" {
		t.Errorf("insensitive share: fold %v, answered %q, want upcase and insensitive", sh.Fold, got.Spec.CaseSensitivity)
	}
	tag := s.do(t, http.MethodGet, loc, "").header.Get("ETag")
	r := s.do(t, http.MethodPatch, loc, `{"spec":{"case_sensitivity":"sensitive"}}`, "If-Match", tag)
	if p := r.problem(t); r.status != http.StatusBadRequest || p.Field != "spec.case_sensitivity" {
		t.Errorf("PATCH case_sensitivity: answered %d field %q, want 400 naming spec.case_sensitivity", r.status, p.Field)
	}
	if sh, _ := s.share(t, id); sh.Fold != entity.FoldUpcase {
		t.Errorf("after the refused PATCH: fold %v, want upcase", sh.Fold)
	}
}

// Quiesce moves an enabled share to quiesced, which its GET reads, and
// resume back, each answering the share; a quiesce of a quiesced share
// answers 200 and changes nothing. None needs If-Match.
func TestShareQuiesce(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	id, loc := s.createShare(t, "builds", s.namespace(t))
	post := func(verb, key string) reply {
		return s.do(t, http.MethodPost, loc+":"+verb, "", "Idempotency-Key", key)
	}
	r := post("quiesce", "q1")
	if r.status != http.StatusOK {
		t.Fatalf("quiesce: answered %d %s, want 200", r.status, r.body)
	}
	if got := decodeShare(t, "quiesce", r.body); got.ID != id.String() || got.Status.State != "quiesced" || got.Spec.Name != "builds" {
		t.Errorf("quiesce's answer: %+v, want share %s builds, quiesced", got, id)
	}
	if got := decodeShare(t, "GET after quiesce", s.do(t, http.MethodGet, loc, "").body); got.Status.State != "quiesced" {
		t.Errorf("GET after quiesce: state %q, want quiesced", got.Status.State)
	}
	sh, _ := s.share(t, id)
	list := s.shareList(t)
	if r := post("quiesce", "q2"); r.status != http.StatusOK {
		t.Errorf("quiesce of a quiesced share: answered %d %s, want 200", r.status, r.body)
	}
	if again, _ := s.share(t, id); again.State != entity.ShareQuiesced || again.Position != sh.Position || s.shareList(t) != list {
		t.Errorf("after a second quiesce: %v at position %d, ShareList %d, want quiesced at %d and %d unchanged", again.State, again.Position, s.shareList(t), sh.Position, list)
	}
	r = post("resume", "r1")
	if got := decodeShare(t, "resume", r.body); r.status != http.StatusOK || got.Status.State != "enabled" {
		t.Errorf("resume: answered %d state %q, want 200 enabled", r.status, got.Status.State)
	}
	if sh, _ = s.share(t, id); sh.State != entity.ShareEnabled {
		t.Errorf("after resume: %v, want enabled", sh.State)
	}
}

// DELETE marks the share and answers 202 with a share.delete operation on
// it; a second DELETE is refused 409 stale; a disposition that is not one is
// refused naming ?snapshots=, and a parameter the DELETE does not take, or
// any on a write that takes none, is refused naming it.
func TestShareDelete(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	id, loc := s.createShare(t, "builds", s.namespace(t))
	tag := s.do(t, http.MethodGet, loc, "").header.Get("ETag")
	for _, tc := range []struct{ method, query, body, field string }{
		{http.MethodDelete, "?snapshots=sideways", "", "snapshots"},
		{http.MethodDelete, "?snapshot=delete", "", "snapshot"},
		{http.MethodPatch, "?snapshots=delete", `{"spec":{"name":"ci"}}`, "snapshots"},
	} {
		r := s.do(t, tc.method, loc+tc.query, tc.body, "Idempotency-Key", "d"+tc.field, "If-Match", tag)
		if p := r.problem(t); r.status != http.StatusBadRequest || p.Field != tc.field {
			t.Errorf("%s %s: answered %d field %q, want 400 naming %s", tc.method, tc.query, r.status, p.Field, tc.field)
		}
	}
	if sh, _ := s.share(t, id); sh.State != entity.ShareEnabled || sh.Name != "builds" {
		t.Fatalf("after the refused writes: %q %v, want builds enabled", sh.Name, sh.State)
	}
	r := s.do(t, http.MethodDelete, loc+"?snapshots=delete", "", "Idempotency-Key", "d1", "If-Match", tag)
	if r.status != http.StatusAccepted {
		t.Fatalf("DELETE: answered %d %s, want 202", r.status, r.body)
	}
	op := decodeOp(t, "DELETE", r.body)
	if op.Kind != apiv1.OpShareDelete || op.Target != (apiv1.Ref{Kind: "share", ID: id.String()}) || r.header.Get("Location") != "/api/v1/operations/"+op.ID {
		t.Errorf("DELETE: operation %s on %+v at %q, want share.delete on share %s at its own path", op.Kind, op.Target, r.header.Get("Location"), id)
	}
	if sh, _ := s.share(t, id); sh.State != entity.ShareRemoving {
		t.Errorf("after DELETE: %v, want removing", sh.State)
	}
	r = s.do(t, http.MethodDelete, loc, "", "Idempotency-Key", "d2", "If-Match", tag)
	if p := r.problem(t); r.status != http.StatusConflict || p.Code != "stale" {
		t.Errorf("a second DELETE: answered %d %q, want 409 stale", r.status, p.Code)
	}
}

// A PATCH renames and re-paths a share, raising ShareList, leaving its
// namespace and answering the share at its new generation; one naming
// another namespace is refused and changes nothing.
func TestSharePatch(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	id, loc := s.createShare(t, "builds", s.namespace(t))
	before, _ := s.share(t, id)
	list := s.shareList(t)
	tag := s.do(t, http.MethodGet, loc, "").header.Get("ETag")
	r := s.do(t, http.MethodPatch, loc, `{"spec":{"namespace":"`+s.namespace(t)+`"}}`, "If-Match", tag)
	if p := r.problem(t); r.status != http.StatusBadRequest || p.Field != "spec.namespace" {
		t.Errorf("PATCH naming another namespace: answered %d field %q, want 400 naming spec.namespace", r.status, p.Field)
	}
	if sh, _ := s.share(t, id); !reflect.DeepEqual(sh, before) {
		t.Errorf("after the refused PATCH: %+v, want %+v unchanged", sh, before)
	}
	r = s.do(t, http.MethodPatch, loc, `{"spec":{"name":"ci","path":"/ci"}}`, "If-Match", tag)
	if r.status != http.StatusOK {
		t.Fatalf("PATCH name and path: answered %d %s, want 200", r.status, r.body)
	}
	sh, _ := s.share(t, id)
	if sh.Name != "ci" || sh.Path != "/ci" || !reflect.DeepEqual(sh.Namespaces, before.Namespaces) {
		t.Errorf("after PATCH: %q at %q in %v, want ci at /ci in %v", sh.Name, sh.Path, sh.Namespaces, before.Namespaces)
	}
	got := decodeShare(t, "PATCH", r.body)
	if got.ID != id.String() || got.Generation != sh.Generation || got.Spec != specOf(sh, before) || got.Status.State != "enabled" ||
		r.header.Get("ETag") != `"`+strconv.FormatUint(sh.Generation, 10)+`"` {
		t.Errorf("PATCH's answer: %+v ETag %s, want share %s at generation %d, ci at /ci, enabled", got, r.header.Get("ETag"), id, sh.Generation)
	}
	if l := s.shareList(t); l <= list {
		t.Errorf("ShareList after the rename: %d, want above %d", l, list)
	}
}

// specOf is sh's spec on the wire, in the namespace before was created in.
func specOf(sh, before entity.Share) apiv1.ShareSpec {
	return apiv1.ShareSpec{Name: sh.Name, Path: sh.Path, Namespace: uuid.UUID(before.Namespaces[0].Namespace).String(), CaseSensitivity: "sensitive"}
}

// A write naming a share being removed is refused 409 stale: a PATCH, ahead
// of the namespace it names, a quiesce, and a PUT of its export policy.
func TestShareRemovedRefusesWrites(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	id, loc := s.createShare(t, "builds", s.namespace(t))
	stag := s.do(t, http.MethodGet, loc, "").header.Get("ETag")
	ptag := s.do(t, http.MethodGet, loc+"/export-policy", "").header.Get("ETag")
	s.removing(t, id)
	for _, r := range []reply{
		s.do(t, http.MethodPatch, loc, `{"spec":{"name":"ci","namespace":"`+s.namespace(t)+`"}}`, "If-Match", stag),
		s.do(t, http.MethodPost, loc+":quiesce", "", "Idempotency-Key", "q"),
		s.do(t, http.MethodPut, loc+"/export-policy", jsonOf(t, map[string]any{"spec": policy}), "If-Match", ptag),
	} {
		if p := r.problem(t); r.status != http.StatusConflict || p.Code != "stale" {
			t.Errorf("%s on a share being removed: answered %d %q, want 409 stale", r.req, r.status, p.Code)
		}
	}
}

// GET /shares lists the shares in name order, each with on how many nodes
// it is applied, a page at a time; ?name= finds one share or none, and
// ?namespace= those counted in a namespace, in ID order; neither lists a
// share being removed.
func TestShareList(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	one, two := s.namespace(t), s.namespace(t)
	// IDs that sort apart from the names, so a namespace page resuming by
	// name rather than by ID reads the wrong shares.
	for _, c := range []struct {
		id       entity.ShareID
		name, ns string
	}{{entity.ShareID{2}, "a", one}, {entity.ShareID{4}, "b", two}, {entity.ShareID{1}, "c", one}, {entity.ShareID{3}, "d", one}} {
		ns, err := entity.ParseShareID(c.ns)
		if err != nil {
			t.Fatalf("namespace %q: %v", c.ns, err)
		}
		sh := entity.Share{ID: c.id, Name: c.name, Path: "/" + c.name, Root: entity.NewFileID(), Namespaces: []entity.NamespaceGen{{Namespace: entity.NamespaceID(ns)}}}
		if _, err := s.store.PutShare(t.Context(), sh, 0); err != nil {
			t.Fatalf("put share %s: %v", c.name, err)
		}
	}
	s.removing(t, entity.ShareID{3})
	s.putStatus(t, entity.NodeStatus{Node: entity.NodeID{1}, ShareList: s.shareList(t)})
	s.putStatus(t, entity.NodeStatus{Node: entity.NodeID{2}})
	page := func(query string) apiv1.Page[apiv1.Share] {
		t.Helper()
		r := s.do(t, http.MethodGet, "/api/v1/shares"+query, "")
		var p apiv1.Page[apiv1.Share]
		if err := json.Unmarshal(r.body, &p); err != nil || r.status != http.StatusOK {
			t.Fatalf("GET /shares%s: answered %d %s, decode err %v, want 200", query, r.status, r.body, err)
		}
		for _, sh := range p.Items {
			if sh.Status.Applied == nil || *sh.Status.Applied != (apiv1.Applied{Nodes: 1, Of: 2}) {
				t.Errorf("GET /shares%s: %s applied %+v, want 1 of 2", query, sh.Spec.Name, sh.Status.Applied)
			}
		}
		return p
	}
	names := func(p apiv1.Page[apiv1.Share]) []string {
		var out []string
		for _, sh := range p.Items {
			out = append(out, sh.Spec.Name)
		}
		return out
	}
	byID := []string{"c", "a"}
	first := page("?limit=2")
	for _, tc := range []struct {
		what string
		got  apiv1.Page[apiv1.Share]
		want []string
		more bool
	}{
		{"every share", page(""), []string{"a", "b", "c"}, false},
		{"a first page of 2", first, []string{"a", "b"}, true},
		{"the page after it", page("?limit=2&after=" + first.Next), []string{"c"}, false},
		{"?name=b", page("?name=b"), []string{"b"}, false},
		{"?name=B, under the share-name fold", page("?name=B"), []string{"b"}, false},
		{"?name= of no share", page("?name=zz"), nil, false},
		{"?name= of a share being removed", page("?name=d"), nil, false},
		{"?name= in another namespace", page("?name=b&namespace=" + one), nil, false},
		{"?namespace=", page("?namespace=" + one), byID, false},
		{"?namespace= a page of 1", page("?limit=1&namespace=" + one), byID[:1], true},
		{"?namespace= the page after it", page("?limit=1&namespace=" + one + "&after=" + page("?limit=1&namespace="+one).Next), byID[1:], false},
	} {
		if got := names(tc.got); !slices.Equal(got, tc.want) || (tc.got.Next != "") != tc.more {
			t.Errorf("GET /shares, %s: %v next %q, want %v, more %v", tc.what, got, tc.got.Next, tc.want, tc.more)
		}
	}
	if r := s.do(t, http.MethodGet, "/api/v1/shares?namespace=x", ""); r.status != http.StatusBadRequest || r.problem(t).Field != "namespace" {
		t.Errorf("GET /shares?namespace=x: answered %d %s, want 400 naming namespace", r.status, r.body)
	}
}

// An export policy reads with its version as its ETag, apart from the
// share's; a PUT or PATCH under it replaces it, answering it, and moves the
// ETag, never the share's, with an audit record naming its fields under
// export_policy between its versions; a PUT under a stale one is refused 412
// naming the current one. The policy of a share that does not exist is 404.
func TestExportPolicy(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	id, loc := s.createShare(t, "builds", s.namespace(t))
	path := loc + "/export-policy"
	if r := s.do(t, http.MethodGet, "/api/v1/shares/"+uuid.NewString()+"/export-policy", ""); r.status != http.StatusNotFound {
		t.Errorf("GET export-policy of no share: answered %d %s, want 404", r.status, r.body)
	}
	shareTag := s.do(t, http.MethodGet, loc, "").header.Get("ETag")
	r := s.do(t, http.MethodGet, path, "")
	var got apiv1.ExportPolicy
	if err := json.Unmarshal(r.body, &got); err != nil || r.status != http.StatusOK {
		t.Fatalf("GET export-policy: answered %d %s, decode err %v, want 200", r.status, r.body, err)
	}
	_, p := s.share(t, id)
	if tag := r.header.Get("ETag"); tag != `"`+strconv.FormatUint(p.Version, 10)+`"` || !reflect.DeepEqual(got.Spec, policy) {
		t.Errorf("GET export-policy: ETag %s spec %+v, want \"%d\" and %+v", tag, got.Spec, p.Version, policy)
	}
	old, version := r.header.Get("ETag"), p.Version
	next := policy
	next.ReadOnly, next.Flavors = true, []string{"kerberos"}
	body := jsonOf(t, map[string]any{"spec": next})
	put := s.do(t, http.MethodPut, path, body, "If-Match", old)
	if put.status != http.StatusOK || put.header.Get("ETag") == old {
		t.Fatalf("PUT export-policy: answered %d ETag %s, want 200 with a new ETag", put.status, put.header.Get("ETag"))
	}
	if _, p = s.share(t, id); !p.ReadOnly || !slices.Equal(p.Flavors, []entity.AuthFlavor{entity.FlavorKerberos}) {
		t.Errorf("after PUT: read-only %v flavours %v, want true and [kerberos]", p.ReadOnly, p.Flavors)
	}
	want := apiv1.ExportPolicy{ID: id.String(), Generation: p.Version, Spec: next}
	if err := json.Unmarshal(put.body, &got); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("PUT's answer: %s, decode err %v, want %+v", put.body, err, want)
	}
	recs := s.audits(t)
	a := recs[len(recs)-1]
	if wantChange := `{"export_policy.flavors":[["kerberos","sys"],["kerberos"]],"export_policy.read_only":[false,true]}`; string(a.Change) != wantChange ||
		a.Before != version || a.After != p.Version || a.Target.Kind != entity.ResourceShare {
		t.Errorf("PUT's audit record: change %s, versions %d to %d, kind %v, want %s, %d to %d, share", a.Change, a.Before, a.After, a.Target.Kind, wantChange, version, p.Version)
	}
	g := s.do(t, http.MethodGet, path, "")
	if err := json.Unmarshal(g.body, &got); err != nil || !reflect.DeepEqual(got.Spec, next) || g.header.Get("ETag") != put.header.Get("ETag") {
		t.Errorf("GET after PUT: ETag %s spec %+v, decode err %v, want %s and %+v", g.header.Get("ETag"), got.Spec, err, put.header.Get("ETag"), next)
	}
	if tag := s.do(t, http.MethodGet, loc, "").header.Get("ETag"); tag != shareTag {
		t.Errorf("share's ETag after its export policy's PUT: %s, want %s unchanged", tag, shareTag)
	}
	stale := s.do(t, http.MethodPut, path, body, "If-Match", old)
	if pb := stale.problem(t); stale.status != http.StatusPreconditionFailed || !slices.Equal(pb.Names, []string{put.header.Get("ETag")}) {
		t.Errorf("PUT under a stale ETag: answered %d naming %q, want 412 naming %s", stale.status, pb.Names, put.header.Get("ETag"))
	}
	patch := s.do(t, http.MethodPatch, path, `{"spec":{"read_only":false}}`, "If-Match", put.header.Get("ETag"))
	if _, p = s.share(t, id); patch.status != http.StatusOK || p.ReadOnly || !slices.Equal(p.Flavors, []entity.AuthFlavor{entity.FlavorKerberos}) {
		t.Errorf("PATCH read_only false: answered %d, read-only %v flavours %v, want 200, false and [kerberos] kept", patch.status, p.ReadOnly, p.Flavors)
	}
}

// A share with no export policy stored reads the default one at version 0,
// which a PUT naming "0" replaces.
func TestExportPolicyDefault(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	ns, err := entity.ParseShareID(s.namespace(t))
	if err != nil {
		t.Fatal(err)
	}
	id := entity.NewShareID()
	sh := entity.Share{ID: id, Name: "bare", Path: "/bare", Root: entity.NewFileID(), Namespaces: []entity.NamespaceGen{{Namespace: entity.NamespaceID(ns)}}}
	if _, err := s.store.PutShare(t.Context(), sh, 0); err != nil {
		t.Fatalf("put a share with no policy: %v", err)
	}
	path := "/api/v1/shares/" + id.String() + "/export-policy"
	r := s.do(t, http.MethodGet, path, "")
	var got apiv1.ExportPolicy
	if err := json.Unmarshal(r.body, &got); err != nil || r.header.Get("ETag") != `"0"` || got.Spec.Squash != "root" || len(got.Spec.Flavors) != 0 {
		t.Errorf("GET the default policy: ETag %s %s, decode err %v, want \"0\", squash root and no flavour", r.header.Get("ETag"), r.body, err)
	}
	put := s.do(t, http.MethodPut, path, jsonOf(t, map[string]any{"spec": policy}), "If-Match", `"0"`)
	if _, p := s.share(t, id); put.status != http.StatusOK || !slices.Equal(p.Flavors, []entity.AuthFlavor{entity.FlavorKerberos, entity.FlavorSys}) {
		t.Errorf("PUT under \"0\": answered %d %s, flavours %v, want 200 and [kerberos sys]", put.status, put.body, p.Flavors)
	}
}

// ?wait=serving on a share returns once every node reporting a status has
// reconciled the share list through the share, on the clock's next second,
// or once a node refuses it, counting the refusal in the status; a node refusing a record of another kind with
// the share's ID leaves it parked.
func TestShareWaitServing(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	for _, tc := range []struct {
		name, what string
		then       func(entity.NodeStatus, entity.Share) entity.NodeStatus
		want       *apiv1.Applied
		refused    int
	}{
		{"a", "the second node reconciled through it", func(st entity.NodeStatus, sh entity.Share) entity.NodeStatus {
			st.ShareList = sh.Position
			return st
		}, &apiv1.Applied{Nodes: 2, Of: 2}, 0},
		{"b", "the second node refused it", func(st entity.NodeStatus, sh entity.Share) entity.NodeStatus {
			st.ShareList = sh.Position
			st.Refused = []entity.RefusedRecord{{Record: entity.ResourceRef{Kind: entity.ResourceShare, ID: sh.ID[:]}}}
			return st
		}, &apiv1.Applied{Nodes: 1, Of: 2}, 1},
		{"c", "the second node refused another kind's record of its ID", func(st entity.NodeStatus, sh entity.Share) entity.NodeStatus {
			st.Refused = []entity.RefusedRecord{{Record: entity.ResourceRef{Kind: entity.ResourceSetting, ID: sh.ID[:]}}}
			return st
		}, nil, 0},
	} {
		id, loc := s.createShare(t, tc.name, s.namespace(t))
		sh, _ := s.share(t, id)
		one, two := entity.NodeStatus{Node: entity.NodeID{1}, ShareList: sh.Position}, entity.NodeStatus{Node: entity.NodeID{2}, ShareList: sh.Position - 1}
		s.putStatus(t, one)
		s.putStatus(t, two)
		base := s.clock.Pending()
		ch := s.poll(t.Context(), loc+"?wait=serving")
		for i, at := range []string{"at once", "a second on"} {
			if i > 0 {
				s.clock.Advance(time.Second)
			}
			if p := s.settle(t, "poll "+at, ch, base); p.status != 0 || p.err != nil {
				t.Fatalf("poll %s, before %s: answered %d %s err %v, want it parked", at, tc.what, p.status, p.body, p.err)
			}
		}
		s.putStatus(t, tc.then(two, sh))
		s.clock.Advance(time.Second)
		p := s.settle(t, "poll after "+tc.what, ch, base)
		if tc.want == nil {
			if p.status != 0 {
				t.Errorf("poll after %s: answered %d %s, want it parked", tc.what, p.status, p.body)
			}
			continue
		}
		if p.status == 0 {
			t.Fatalf("poll after %s: parked again, want answered", tc.what)
		}
		var got apiv1.Share
		if p.err != nil || p.status != http.StatusOK || json.Unmarshal(p.body, &got) != nil || got.Status.Applied == nil || *got.Status.Applied != *tc.want || got.Status.Refused != tc.refused {
			t.Errorf("poll after %s: answered %d %s err %v, want 200 applied %+v refused %d", tc.what, p.status, p.body, p.err, *tc.want, tc.refused)
		}
	}
}

// ?wait=serving parks while no node reports a status; it ends at once on a
// quiesced share, claiming nothing, and is refused 409 stale once the share
// is being removed. ?wait= a duration returns on a change of state.
func TestShareWaitEnds(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	parked := func(what string, ch <-chan polled, base int) {
		t.Helper()
		if p := s.settle(t, what, ch, base); p.status != 0 || p.err != nil {
			t.Fatalf("%s: answered %d %s err %v, want it parked", what, p.status, p.body, p.err)
		}
	}

	_, loc := s.createShare(t, "idle", s.namespace(t))
	base := s.clock.Pending()
	ch := s.poll(t.Context(), loc+"?wait=serving")
	parked("?wait=serving with no node reporting", ch, base)
	s.clock.Advance(time.Second)
	parked("?wait=serving with no node reporting, a second on", ch, base)

	_, loc = s.createShare(t, "paused", s.namespace(t))
	if r := s.do(t, http.MethodPost, loc+":quiesce", "", "Idempotency-Key", "q"); r.status != http.StatusOK {
		t.Fatalf("quiesce: answered %d %s, want 200", r.status, r.body)
	}
	base = s.clock.Pending()
	p := s.settle(t, "?wait=serving on a quiesced share", s.poll(t.Context(), loc+"?wait=serving"), base)
	if p.status == 0 {
		t.Fatal("?wait=serving on a quiesced share: parked, want answered at once")
	}
	if got := decodeShare(t, "?wait=serving on a quiesced share", p.body); p.status != http.StatusOK || got.Status.State != "quiesced" {
		t.Errorf("?wait=serving on a quiesced share: answered %d state %q, want 200 quiesced at once", p.status, got.Status.State)
	}

	id, loc := s.createShare(t, "doomed", s.namespace(t))
	base = s.clock.Pending()
	ch = s.poll(t.Context(), loc+"?wait=serving")
	parked("?wait=serving before the removal", ch, base)
	s.removing(t, id)
	s.clock.Advance(time.Second)
	if p := s.settle(t, "?wait=serving after the removal", ch, base); p.status != http.StatusConflict || p.problem(t).Code != "stale" {
		t.Errorf("?wait=serving after the removal: answered %d %s, want 409 stale", p.status, p.body)
	}

	id, loc = s.createShare(t, "watched", s.namespace(t))
	base = s.clock.Pending()
	ch = s.poll(t.Context(), loc+"?wait=5s")
	parked("?wait=5s before the quiesce", ch, base)
	if err := s.store.SetShareState(t.Context(), id, entity.ShareEnabled, entity.ShareQuiesced); err != nil {
		t.Fatalf("quiesce through the store: %v", err)
	}
	s.clock.Advance(time.Second)
	p = s.settle(t, "?wait=5s after the quiesce", ch, base)
	if p.status == 0 {
		t.Fatal("?wait=5s after the quiesce: parked again, want answered")
	}
	if got := decodeShare(t, "?wait=5s after the quiesce", p.body); p.status != http.StatusOK || got.Status.State != "quiesced" {
		t.Errorf("?wait=5s after the quiesce: answered %d state %q, want 200 quiesced", p.status, got.Status.State)
	}
}
