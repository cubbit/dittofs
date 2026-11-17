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
	Squash:  "root", SMB: apiv1.SMBShare{Encrypt: true},
}

// stored is policy as the store holds it for share.
func stored(share entity.ShareID) entity.ExportPolicy {
	return entity.ExportPolicy{Share: share, Flavors: []entity.AuthFlavor{entity.FlavorKerberos, entity.FlavorSys},
		MinProof: entity.ProofClaimed, MinProtection: entity.ProtectionNone, Squash: entity.SquashRoot,
		Anonymous: entity.AnonymousPrincipal, SMB: entity.SMBEncrypt,
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

func newShareBody(t *testing.T, name, ns string, p apiv1.ExportPolicySpec) string {
	t.Helper()
	b, err := json.Marshal(apiv1.NewShare{Spec: apiv1.ShareSpec{Name: name, Path: "/" + name, Namespace: ns}, ExportPolicy: p})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// createShare creates a share named name, at /name, in a new namespace, and
// returns its ID and its path.
func (s *served) createShare(t *testing.T, name string) (entity.ShareID, string) {
	t.Helper()
	r := s.do(t, http.MethodPost, "/api/v1/shares", newShareBody(t, name, s.namespace(t), policy), "Idempotency-Key", "create-"+name)
	if r.status != http.StatusCreated {
		t.Fatalf("POST /shares %s: answered %d %s, want 201", name, r.status, r.body)
	}
	loc := r.header.Get("Location")
	id, err := entity.ParseShareID(strings.TrimPrefix(loc, "/api/v1/shares/"))
	if err != nil {
		t.Fatalf("POST /shares %s: Location %q names no share: %v", name, loc, err)
	}
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

// A share created in a namespace that does not exist, so on no remote
// store, is refused and commits nothing; created in one that does, it is
// enabled in that namespace with the export policy the request stated, no
// grant, and ShareList raised.
func TestShareCreate(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	list := s.shareList(t)
	r := s.do(t, http.MethodPost, "/api/v1/shares", newShareBody(t, "builds", uuid.NewString(), policy), "Idempotency-Key", "k1")
	if p := r.problem(t); r.status != http.StatusBadRequest || p.Code != "invalid_argument" {
		t.Errorf("POST /shares in a namespace that does not exist: answered %d %q, want 400 invalid_argument", r.status, p.Code)
	}
	if n, l := s.shareCount(t), s.shareList(t); n != 0 || l != list {
		t.Errorf("after the refused create: %d shares, ShareList %d, want 0 and %d", n, l, list)
	}

	ns := s.namespace(t)
	r = s.do(t, http.MethodPost, "/api/v1/shares", newShareBody(t, "builds", ns, policy), "Idempotency-Key", "k2")
	if r.status != http.StatusCreated {
		t.Fatalf("POST /shares in an existing namespace: answered %d %s, want 201", r.status, r.body)
	}
	id, err := entity.ParseShareID(strings.TrimPrefix(r.header.Get("Location"), "/api/v1/shares/"))
	if err != nil {
		t.Fatalf("Location %q names no share", r.header.Get("Location"))
	}
	sh, p := s.share(t, id)
	if sh.Name != "builds" || sh.Path != "/builds" || sh.State != entity.ShareEnabled ||
		len(sh.Namespaces) != 1 || uuid.UUID(sh.Namespaces[0].Namespace).String() != ns {
		t.Errorf("created share: %q at %q, %v, namespaces %v, want builds at /builds, enabled, in %s", sh.Name, sh.Path, sh.State, sh.Namespaces, ns)
	}
	want := stored(id)
	want.Version = p.Version
	if !reflect.DeepEqual(p, want) {
		t.Errorf("created share's export policy: %+v, want %+v", p, want)
	}
	if sh.GrantVersion != 0 {
		t.Errorf("created share's grant-list version: %d, want 0, no grant written", sh.GrantVersion)
	}
	if l := s.shareList(t); l <= list {
		t.Errorf("ShareList after the create: %d, want above %d", l, list)
	}
}

// A create whose export policy names no flavour, or a value no field
// takes, is refused naming the field, and nothing is committed.
func TestShareCreateRefusesPolicy(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	ns := s.namespace(t)
	for i, tc := range []struct {
		edit  func(*apiv1.ExportPolicySpec)
		field string
	}{
		{func(p *apiv1.ExportPolicySpec) { p.Flavors = nil }, "export_policy.flavors"},
		{func(p *apiv1.ExportPolicySpec) { p.Flavors = []string{"ntlm"} }, "export_policy.flavors"},
		{func(p *apiv1.ExportPolicySpec) { p.MinProof = "" }, "export_policy.min_proof"},
		{func(p *apiv1.ExportPolicySpec) { p.MinProtection = "sealed" }, "export_policy.min_protection"},
		{func(p *apiv1.ExportPolicySpec) { p.Squash = "" }, "export_policy.squash"},
		{func(p *apiv1.ExportPolicySpec) { p.Clients = []apiv1.ClientRule{{CIDR: "10.0.0.0/33", Access: "read"}} }, "export_policy.clients"},
		{func(p *apiv1.ExportPolicySpec) { p.Clients = []apiv1.ClientRule{{Netgroup: "labs", Access: "write"}} }, "export_policy.clients"},
	} {
		p := policy
		tc.edit(&p)
		r := s.do(t, http.MethodPost, "/api/v1/shares", newShareBody(t, "builds", ns, p), "Idempotency-Key", "k"+string(rune('a'+i)))
		if pb := r.problem(t); r.status != http.StatusBadRequest || pb.Field != tc.field {
			t.Errorf("POST /shares with %s refused: answered %d field %q, want 400 naming %s", tc.field, r.status, pb.Field, tc.field)
		}
	}
	if n := s.shareCount(t); n != 0 {
		t.Errorf("after the refused creates: %d shares, want 0", n)
	}
}

// Quiesce moves an enabled share to quiesced and resume back; a quiesce of
// a quiesced share answers 200 and changes nothing; a quiesce of a share
// being removed answers 409 stale. None needs If-Match.
func TestShareQuiesce(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	id, loc := s.createShare(t, "builds")
	post := func(verb, key string) reply {
		return s.do(t, http.MethodPost, loc+":"+verb, "", "Idempotency-Key", key)
	}
	if r := post("quiesce", "q1"); r.status != http.StatusOK {
		t.Fatalf("quiesce: answered %d %s, want 200", r.status, r.body)
	}
	sh, _ := s.share(t, id)
	if sh.State != entity.ShareQuiesced {
		t.Fatalf("after quiesce: %v, want quiesced", sh.State)
	}
	list := s.shareList(t)
	if r := post("quiesce", "q2"); r.status != http.StatusOK {
		t.Errorf("quiesce of a quiesced share: answered %d %s, want 200", r.status, r.body)
	}
	if again, _ := s.share(t, id); again.State != entity.ShareQuiesced || again.Position != sh.Position || s.shareList(t) != list {
		t.Errorf("after a second quiesce: %v at position %d, ShareList %d, want quiesced at %d and %d unchanged", again.State, again.Position, s.shareList(t), sh.Position, list)
	}
	if r := post("resume", "r1"); r.status != http.StatusOK {
		t.Errorf("resume: answered %d %s, want 200", r.status, r.body)
	}
	if sh, _ = s.share(t, id); sh.State != entity.ShareEnabled {
		t.Errorf("after resume: %v, want enabled", sh.State)
	}
	if err := s.store.DeleteShare(t.Context(), id, entity.SnapshotsRefuse, sh.Generation); err != nil {
		t.Fatalf("mark the share removed: %v", err)
	}
	r := post("quiesce", "q3")
	if p := r.problem(t); r.status != http.StatusConflict || p.Code != "stale" {
		t.Errorf("quiesce of a share being removed: answered %d %q, want 409 stale", r.status, p.Code)
	}
}

// DELETE marks the share and answers 202 with a share.delete operation on
// it; a second DELETE is refused 409 stale; a disposition that is not one
// is refused naming ?snapshots=.
func TestShareDelete(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	id, loc := s.createShare(t, "builds")
	tag := s.do(t, http.MethodGet, loc, "").header.Get("ETag")
	r := s.do(t, http.MethodDelete, loc+"?snapshots=sideways", "", "Idempotency-Key", "d0", "If-Match", tag)
	if p := r.problem(t); r.status != http.StatusBadRequest || p.Field != "snapshots" {
		t.Errorf("DELETE ?snapshots=sideways: answered %d field %q, want 400 naming snapshots", r.status, p.Field)
	}
	r = s.do(t, http.MethodDelete, loc+"?snapshots=delete", "", "Idempotency-Key", "d1", "If-Match", tag)
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

// A PATCH renames and re-paths a share, raising ShareList and leaving its
// namespace; one naming another namespace is refused and changes nothing.
func TestSharePatch(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	id, loc := s.createShare(t, "builds")
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
	if l := s.shareList(t); l <= list {
		t.Errorf("ShareList after the rename: %d, want above %d", l, list)
	}
}

// A write naming a share being removed is refused 409 stale by the store's
// check: a PATCH, and a PUT of its export policy.
func TestShareRemovedRefusesWrites(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	id, loc := s.createShare(t, "builds")
	stag := s.do(t, http.MethodGet, loc, "").header.Get("ETag")
	ptag := s.do(t, http.MethodGet, loc+"/export-policy", "").header.Get("ETag")
	sh, _ := s.share(t, id)
	if err := s.store.DeleteShare(t.Context(), id, entity.SnapshotsRefuse, sh.Generation); err != nil {
		t.Fatalf("mark the share removed: %v", err)
	}
	body, _ := json.Marshal(map[string]any{"spec": policy})
	for _, r := range []reply{
		s.do(t, http.MethodPatch, loc, `{"spec":{"name":"ci"}}`, "If-Match", stag),
		s.do(t, http.MethodPut, loc+"/export-policy", string(body), "If-Match", ptag),
	} {
		if p := r.problem(t); r.status != http.StatusConflict || p.Code != "stale" {
			t.Errorf("%s on a share being removed: answered %d %q, want 409 stale", r.req, r.status, p.Code)
		}
	}
}

// An export policy reads with its version as its ETag, apart from the
// share's; a PUT or PATCH under it replaces it and moves the ETag, never the
// share's; a PUT under a stale one is refused 412 naming the current one.
func TestExportPolicy(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	id, loc := s.createShare(t, "builds")
	path := loc + "/export-policy"
	shareTag := s.do(t, http.MethodGet, loc, "").header.Get("ETag")
	r := s.do(t, http.MethodGet, path, "")
	var got apiv1.ExportPolicy
	if err := json.Unmarshal(r.body, &got); err != nil || r.status != http.StatusOK {
		t.Fatalf("GET export-policy: answered %d %s", r.status, r.body)
	}
	_, p := s.share(t, id)
	if tag := r.header.Get("ETag"); tag != `"`+strconv.FormatUint(p.Version, 10)+`"` || !reflect.DeepEqual(got.Spec, policy) {
		t.Errorf("GET export-policy: ETag %s spec %+v, want \"%d\" and %+v", tag, got.Spec, p.Version, policy)
	}
	old := r.header.Get("ETag")
	next := policy
	next.ReadOnly, next.Flavors = true, []string{"kerberos"}
	body, _ := json.Marshal(map[string]any{"spec": next})
	put := s.do(t, http.MethodPut, path, string(body), "If-Match", old)
	if put.status != http.StatusOK || put.header.Get("ETag") == old {
		t.Fatalf("PUT export-policy: answered %d ETag %s, want 200 with a new ETag", put.status, put.header.Get("ETag"))
	}
	if _, p = s.share(t, id); !p.ReadOnly || !slices.Equal(p.Flavors, []entity.AuthFlavor{entity.FlavorKerberos}) {
		t.Errorf("after PUT: read-only %v flavours %v, want true and [kerberos]", p.ReadOnly, p.Flavors)
	}
	if g := s.do(t, http.MethodGet, path, ""); json.Unmarshal(g.body, &got) != nil || !reflect.DeepEqual(got.Spec, next) || g.header.Get("ETag") != put.header.Get("ETag") {
		t.Errorf("GET after PUT: ETag %s spec %+v, want %s and %+v", g.header.Get("ETag"), got.Spec, put.header.Get("ETag"), next)
	}
	if tag := s.do(t, http.MethodGet, loc, "").header.Get("ETag"); tag != shareTag {
		t.Errorf("share's ETag after its export policy's PUT: %s, want %s unchanged", tag, shareTag)
	}
	stale := s.do(t, http.MethodPut, path, string(body), "If-Match", old)
	if pb := stale.problem(t); stale.status != http.StatusPreconditionFailed || !slices.Equal(pb.Names, []string{put.header.Get("ETag")}) {
		t.Errorf("PUT under a stale ETag: answered %d naming %q, want 412 naming %s", stale.status, pb.Names, put.header.Get("ETag"))
	}
	patch := s.do(t, http.MethodPatch, path, `{"spec":{"read_only":false}}`, "If-Match", put.header.Get("ETag"))
	if _, p = s.share(t, id); patch.status != http.StatusOK || p.ReadOnly || !slices.Equal(p.Flavors, []entity.AuthFlavor{entity.FlavorKerberos}) {
		t.Errorf("PATCH read_only false: answered %d, read-only %v flavours %v, want 200, false and [kerberos] kept", patch.status, p.ReadOnly, p.Flavors)
	}
}

// ?wait=serving on a share returns once every node reporting a status has
// reconciled the share list through the share, on the clock's next second,
// or once a node refuses it.
func TestShareWaitServing(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	base := s.clock.Pending()
	for _, tc := range []struct {
		name, what string
		then       func(entity.NodeStatus, entity.Share) entity.NodeStatus
		want       apiv1.Applied
	}{
		{"a", "the second node reconciled through it", func(st entity.NodeStatus, sh entity.Share) entity.NodeStatus {
			st.ShareList = sh.Position
			return st
		}, apiv1.Applied{Nodes: 2, Of: 2}},
		{"b", "the second node refused it", func(st entity.NodeStatus, sh entity.Share) entity.NodeStatus {
			st.ShareList = sh.Position
			st.Refused = []entity.RefusedRecord{{Record: entity.ResourceRef{Kind: entity.ResourceShare, ID: sh.ID[:]}}}
			return st
		}, apiv1.Applied{Nodes: 1, Of: 2}},
	} {
		id, loc := s.createShare(t, tc.name)
		sh, _ := s.share(t, id)
		one, two := entity.NodeStatus{Node: entity.NodeID{1}, ShareList: sh.Position}, entity.NodeStatus{Node: entity.NodeID{2}, ShareList: sh.Position - 1}
		for _, st := range []entity.NodeStatus{one, two} {
			if err := s.nodes.PutNodeStatus(t.Context(), st); err != nil {
				t.Fatalf("put node status: %v", err)
			}
		}
		ch := s.poll(t.Context(), loc+"?wait=serving")
		for i, at := range []string{"at once", "a second on"} {
			if i > 0 {
				s.clock.Advance(time.Second)
			}
			if p := s.settle(t, "poll "+at, ch, base); p.status != 0 || p.err != nil {
				t.Fatalf("poll %s, before %s: answered %d %s err %v, want it parked", at, tc.what, p.status, p.body, p.err)
			}
		}
		if err := s.nodes.PutNodeStatus(t.Context(), tc.then(two, sh)); err != nil {
			t.Fatalf("put node status: %v", err)
		}
		s.clock.Advance(time.Second)
		p := answered(t, "poll after "+tc.what, ch)
		var got apiv1.Share
		if p.err != nil || p.status != http.StatusOK || json.Unmarshal(p.body, &got) != nil || got.Status.Applied == nil || *got.Status.Applied != tc.want {
			t.Errorf("poll after %s: answered %d %s err %v, want 200 applied %+v", tc.what, p.status, p.body, p.err, tc.want)
		}
	}
}
