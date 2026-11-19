package api_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/node/api"
	"github.com/cubbit/dittofs/pkg/auth/sid"
	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/sim"
)

// heldJoins passes each hold through to identity's join hold and records
// the operation it was taken under and whether its release was called: that
// the API holds the credential under the operation it writes, and releases
// it when that write is refused, is the requirement under test.
type heldJoins struct {
	next interface {
		HoldJoinCredential(entity.OperationID, pid.JoinCredential) func()
	}
	mu    sync.Mutex
	holds []*held
}

type held struct {
	id       entity.OperationID
	released bool
}

func (h *heldJoins) HoldJoinCredential(id entity.OperationID, cred pid.JoinCredential) func() {
	rec := &held{id: id}
	h.mu.Lock()
	h.holds = append(h.holds, rec)
	h.mu.Unlock()
	release := h.next.HoldJoinCredential(id, cred)
	return func() {
		h.mu.Lock()
		rec.released = true
		h.mu.Unlock()
		release()
	}
}

func (h *heldJoins) taken() []held {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []held
	for _, r := range h.holds {
		out = append(out, *r)
	}
	return out
}

// sealing is the secret provider as the API seals with it, under the
// protocol role, the machine password's, the one kind these tests seal.
type sealing struct{ p *secrets.Provider }

func (s sealing) Seal(ctx context.Context, kind entity.SecretKind, owner entity.SecretOwner, ref entity.SecretRef, value []byte, handedOver bool) (entity.Secret, error) {
	return s.p.Seal(ctx, secrets.Protocol, kind, owner, ref, value, secrets.Meta{HandedOver: handedOver})
}

// provider is the secret provider over cp, each role's key a real key file
// under the test's directory.
func provider(t *testing.T, cp kvstore.ControlPlane) sealing {
	t.Helper()
	const passphrase = "domain test passphrase"
	t.Setenv("DITTOFS_ENCRYPTION_PASSPHRASE", passphrase)
	ks := map[secrets.Role]keyprovider.KeyProvider{}
	for _, role := range []secrets.Role{secrets.Protocol, secrets.Storage} {
		b, err := keyprovider.GenerateKeyFile(passphrase)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), role.String()+".key")
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		kp, err := keyprovider.NewProvider(t.Context(), keyprovider.Config{Kind: keyprovider.KindLocal, File: path})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = kp.Close() })
		ks[role] = kp
	}
	p, err := secrets.New(cp, ks)
	if err != nil {
		t.Fatal(err)
	}
	return sealing{p}
}

// domain is the API as an admin over Badger on disk, identity's join hold
// behind a recorder and, when it seals, the secret provider.
type domain struct {
	*served
	disk  *onDisk
	joins *heldJoins
}

func serveDomain(t *testing.T, seal bool) *domain {
	t.Helper()
	disk := newOnDisk(t)
	ids, err := identity.New(identity.Deps{Store: disk.st, Clock: sim.NewTestClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))})
	if err != nil {
		t.Fatal(err)
	}
	j := &heldJoins{next: ids}
	edits := []func(*api.Deps){disk.use, func(d *api.Deps) { d.Joins = j }}
	if seal {
		p := provider(t, disk.cp)
		edits = append(edits, func(d *api.Deps) { d.Sealer = p })
	}
	return &domain{serve(t, as(entity.RoleAdmin, false), edits...), disk, j}
}

// operations is every operation the store holds.
func (s *served) operations(t *testing.T) []entity.Operation {
	t.Helper()
	var out []entity.Operation
	for op, err := range s.store.Operations(t.Context(), entity.OperationID{}) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, op)
	}
	return out
}

// installationRecord is the installation's record as the store holds it.
func (s *served) installationRecord(t *testing.T) entity.Installation {
	t.Helper()
	in, err := s.store.Installation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// leaks lists where the store holds an idempotency record, and where any
// of secrets appears, raw or in base64: a key or value of the store read
// raw, an audit record, the log, a metric or one of replies' bodies.
func (d *domain) leaks(t *testing.T, replies []reply, secrets ...string) []string {
	t.Helper()
	var needles [][]byte
	for _, s := range secrets {
		needles = append(needles, []byte(s), []byte(base64.StdEncoding.EncodeToString([]byte(s))))
	}
	var out []string
	has := func(where string, b []byte) {
		for _, n := range needles {
			if bytes.Contains(b, n) {
				out = append(out, where)
				return
			}
		}
	}
	if err := d.disk.raw.View(t.Context(), func(r kv.Reader) error {
		for e, err := range r.Scan(nil, nil) {
			if err != nil {
				return err
			}
			if len(e.Key) > 0 && e.Key[0] == keys.KindIK {
				out = append(out, fmt.Sprintf("an idempotency record %x", e.Key))
			}
			has(fmt.Sprintf("the store's row %x", e.Key), append(slices.Clone(e.Key), e.Value...))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(d.audits(t))
	if err != nil {
		t.Fatal(err)
	}
	has("an audit record", b)
	has("the log", []byte(d.log.String()))
	mfs, err := d.reg.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	has("a metric", []byte(fmt.Sprint(mfs)))
	for _, r := range replies {
		has("the answer to "+r.req, r.body)
	}
	return out
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

const (
	resolvePath = "/api/v1/installation:resolve-principal"
	testDirPath = "/api/v1/installation:test-directory"
	joinPath    = "/api/v1/installation:join-domain"
	rotatePath  = "/api/v1/installation:rotate-machine-password"
)

func onlineJoin(spec, user, password string) string {
	return `{"spec":{"mode":"online"` + spec + `},"credential":{"user":"` + user + `","password":"` + b64(password) + `"}}`
}

func offlineJoin(account, password string) string {
	return `{"spec":{"mode":"offline","account":"` + account + `"},"credential":{"password":"` + b64(password) + `"}}`
}

// With none of the domain's interfaces handed in, every domain route is
// refused 503 unavailable and writes nothing: no operation, no machine
// password, no user and no Kerberos name.
func TestDomainRoutesRefusedWithoutTheirInterface(t *testing.T) {
	s := serve(t, as(entity.RoleAdmin, false))
	alice, _ := s.createUser(t, apiv1.UserSpec{Name: "alice"}, "alice")
	users := s.userCount(t)
	for _, tc := range []struct {
		name, method, path, body string
		hdr                      []string
	}{
		{"resolve-principal", http.MethodPost, resolvePath, `{"name":"bob@AD.EXAMPLE"}`, []string{"Idempotency-Key", "resolve"}},
		{"test-directory", http.MethodPost, testDirPath, "", []string{"Idempotency-Key", "test"}},
		{"rotate-machine-password", http.MethodPost, rotatePath, "", []string{"Idempotency-Key", "rotate"}},
		{"an online join-domain", http.MethodPost, joinPath, onlineJoin("", "Administrator@AD.EXAMPLE", "adm1n-pw"), nil},
		{"an offline join-domain", http.MethodPost, joinPath, offlineJoin("DFS$", "c0mputer-pw"), nil},
		{"a user created with a Kerberos name", http.MethodPost, "/api/v1/users", `{"spec":{"name":"carol","kerberos":"carol@AD.EXAMPLE"}}`, []string{"Idempotency-Key", "carol"}},
		{"a user given a Kerberos name", http.MethodPut, "/api/v1/users/" + alice.ID, `{"spec":{"name":"alice","kerberos":"alice@AD.EXAMPLE"}}`, []string{"If-Match", strconv.Quote(strconv.FormatUint(alice.Generation, 10))}},
	} {
		r := s.do(t, tc.method, tc.path, tc.body, tc.hdr...)
		if r.status != http.StatusServiceUnavailable || r.problem(t).Code != apiv1.CodeUnavailable {
			t.Errorf("%s: answered %d %s, want 503 unavailable", tc.name, r.status, r.body)
		}
	}
	if ops := s.operations(t); len(ops) != 0 {
		t.Errorf("operations written: %+v, want none", ops)
	}
	if in := s.installationRecord(t); in.MachinePassword != (entity.SecretRef{}) {
		t.Errorf("machine password named: %v, want none", in.MachinePassword)
	}
	if got := s.userCount(t); got != users {
		t.Errorf("users: %d, want %d", got, users)
	}
	if u, _ := s.userRecord(t, alice.ID); slices.ContainsFunc(u.IDs, func(id entity.ProtocolID) bool { return id.Scheme == entity.SchemeKerberos }) {
		t.Errorf("alice's spellings %v, want no Kerberos name", u.IDs)
	}
}

// An online join is refused 400 with an Idempotency-Key, an organisational
// unit outside the directory's base, or a host name that is not a DNS name,
// each holding no credential and writing no operation; with neither given
// it answers 202, its credential held under the operation it wrote, whose
// params are the request's spec and hold no credential. A second while it
// runs is refused 409 naming it, and the hold it took is released. Neither
// keeps an idempotency record, and the administrator's password is found
// nowhere.
func TestOnlineJoinDomain(t *testing.T) {
	d := serveDomain(t, false)
	d.putSetting(t, installation, "identity.directory.base", `"DC=ad,DC=example"`)
	const user, pw = "Administrator@AD.EXAMPLE", "Adm1n-s3cret-7f2a"
	var replies []reply
	for _, tc := range []struct {
		name, body, field string
		hdr               []string
	}{
		{"with an Idempotency-Key", onlineJoin("", user, pw), "Idempotency-Key", []string{"Idempotency-Key", "join"}},
		{"with an OU outside the base", onlineJoin(`,"ou":"OU=Servers,DC=other,DC=example"`, user, pw), "spec.ou", nil},
		{"with an OU that is no DN", onlineJoin(`,"ou":"Servers"`, user, pw), "spec.ou", nil},
		{"with a host name that is no DNS name", onlineJoin(`,"dns_host_name":"not a host"`, user, pw), "spec.dns_host_name", nil},
	} {
		r := d.do(t, http.MethodPost, joinPath, tc.body, tc.hdr...)
		replies = append(replies, r)
		if r.status != http.StatusBadRequest || r.problem(t).Field != tc.field {
			t.Errorf("%s: answered %d %s, want 400 naming %s", tc.name, r.status, r.body, tc.field)
		}
	}
	if ops, h := d.operations(t), d.joins.taken(); len(ops) != 0 || len(h) != 0 {
		t.Fatalf("after the refusals: operations %+v, holds %+v, want none", ops, h)
	}

	r := d.do(t, http.MethodPost, joinPath, onlineJoin("", user, pw))
	replies = append(replies, r)
	if r.status != http.StatusAccepted {
		t.Fatalf("join with neither given: answered %d %s, want 202", r.status, r.body)
	}
	ops := d.operations(t)
	if len(ops) != 1 {
		t.Fatalf("operations: %+v, want one", ops)
	}
	op, in := ops[0], d.installationRecord(t)
	if loc := r.header.Get("Location"); loc != "/api/v1/operations/"+uuid.UUID(op.ID).String() {
		t.Errorf("Location %q, want the operation %v", loc, uuid.UUID(op.ID))
	}
	if op.Kind != string(apiv1.OpJoinDomain) || op.Target.Kind != entity.ResourceInstallation || !bytes.Equal(op.Target.ID, in.ID[:]) {
		t.Errorf("operation kind %q target %v, want %s on the installation %x", op.Kind, op.Target, apiv1.OpJoinDomain, in.ID)
	}
	var params apiv1.JoinDomainParams
	if err := json.Unmarshal(op.Params, &params); err != nil || params != (apiv1.JoinDomainParams{JoinDomainSpec: apiv1.JoinDomainSpec{Mode: apiv1.JoinOnline}}) {
		t.Errorf("params %s (%v), want the online join's spec alone", op.Params, err)
	}
	if h := d.joins.taken(); len(h) != 1 || h[0].id != op.ID || h[0].released {
		t.Errorf("holds %+v, want one under the operation %v, not released", h, op.ID)
	}

	r = d.do(t, http.MethodPost, joinPath, onlineJoin(`,"ou":"OU=Servers, DC=AD,DC=Example","dns_host_name":"dfs.ad.example"`, user, pw))
	replies = append(replies, r)
	if r.status != http.StatusConflict || r.problem(t).Code != apiv1.CodeOperationRunning || !slices.Contains(r.problem(t).Names, uuid.UUID(op.ID).String()) {
		t.Errorf("a second join while it runs: answered %d %s, want 409 operation_running naming %v", r.status, r.body, uuid.UUID(op.ID))
	}
	if h := d.joins.taken(); len(h) != 2 || h[1].id == op.ID || !h[1].released {
		t.Errorf("holds %+v, want the second under its own ID, released", h)
	}
	if got := d.operations(t); len(got) != 1 {
		t.Errorf("operations after the refused join: %d, want 1", len(got))
	}
	if l := d.leaks(t, replies, pw); len(l) != 0 {
		t.Errorf("found %q, want no idempotency record and the password nowhere", l)
	}
}

// An offline join writes its computer account's password as a new version
// of the machine password, marked handed over, in an audited write of its
// own before the operation's, whose params name that version; a retry while
// it runs is refused 409 naming it and writes no version. Neither keeps an
// idempotency record, and the password is found only sealed.
func TestOfflineJoinDomain(t *testing.T) {
	d := serveDomain(t, true)
	const account, pw = "DFS$", "C0mputer-pw-91c3"
	r := d.do(t, http.MethodPost, joinPath, offlineJoin(account, pw), "Idempotency-Key", "join")
	replies := []reply{r}
	if r.status != http.StatusBadRequest || r.problem(t).Field != "Idempotency-Key" {
		t.Errorf("with an Idempotency-Key: answered %d %s, want 400 naming it", r.status, r.body)
	}
	if in := d.installationRecord(t); in.MachinePassword != (entity.SecretRef{}) {
		t.Fatalf("machine password named after the refusal: %v, want none", in.MachinePassword)
	}

	r = d.do(t, http.MethodPost, joinPath, offlineJoin(account, pw))
	replies = append(replies, r)
	if r.status != http.StatusAccepted {
		t.Fatalf("offline join: answered %d %s, want 202", r.status, r.body)
	}
	in := d.installationRecord(t)
	sec, err := d.store.Secret(t.Context(), in.MachinePassword, 0)
	if err != nil {
		t.Fatalf("the machine password: %v", err)
	}
	owner := entity.SecretOwner{Record: entity.ResourceRef{Kind: entity.ResourceInstallation, ID: in.ID[:]}, Key: "machine_password"}
	if sec.Version != 1 || !sec.HandedOver || sec.Kind != entity.SecretMachinePassword || !sec.Owner.Equal(owner) {
		t.Errorf("machine password version %d handed over %v kind %v owner %+v, want version 1 handed over, the installation's machine password", sec.Version, sec.HandedOver, sec.Kind, sec.Owner)
	}
	ops := d.operations(t)
	if len(ops) != 1 {
		t.Fatalf("operations: %+v, want one", ops)
	}
	var params apiv1.JoinDomainParams
	if err := json.Unmarshal(ops[0].Params, &params); err != nil || params != (apiv1.JoinDomainParams{JoinDomainSpec: apiv1.JoinDomainSpec{Mode: apiv1.JoinOffline, Account: account}, Version: 1}) {
		t.Errorf("params %s (%v), want the offline join naming version 1", ops[0].Params, err)
	}
	audits := d.audits(t)
	set := slices.IndexFunc(audits, func(a entity.AuditRecord) bool {
		return string(a.Change) == `{"machine_password":"set"}` && string(a.Outcome) == "applied"
	})
	started := slices.IndexFunc(audits, func(a entity.AuditRecord) bool { return string(a.Outcome) == uuid.UUID(ops[0].ID).String() })
	if set < 0 || started < set || audits[set].Target.Kind != entity.ResourceInstallation {
		t.Errorf("audit %q: want the machine password's set, on the installation, before the operation's start", outcomes(audits))
	}

	r = d.do(t, http.MethodPost, joinPath, offlineJoin(account, pw))
	replies = append(replies, r)
	if r.status != http.StatusConflict || r.problem(t).Code != apiv1.CodeOperationRunning || !slices.Contains(r.problem(t).Names, uuid.UUID(ops[0].ID).String()) {
		t.Errorf("a retry while it runs: answered %d %s, want 409 operation_running naming %v", r.status, r.body, uuid.UUID(ops[0].ID))
	}
	if again, err := d.store.Secret(t.Context(), in.MachinePassword, 0); err != nil || again.Version != 1 {
		t.Errorf("machine password after the retry: version %d (%v), want 1", again.Version, err)
	}
	if !slices.Contains(outcomes(d.audits(t)), "refused: operation_running") {
		t.Errorf("audit %q: want the retry's refusal", outcomes(d.audits(t)))
	}
	if l := d.leaks(t, replies, pw); len(l) != 0 {
		t.Errorf("found %q, want no idempotency record and the password only sealed", l)
	}
}

// resolve-principal, test-directory and rotate-machine-password each write
// their operation on the installation, resolve-principal's params the
// principal it names; a principal named by both or neither, or by a SID that
// does not parse, is refused 400 with nothing written.
func TestDomainOperationsRecordTheirTarget(t *testing.T) {
	d := serveDomain(t, false)
	for _, tc := range []struct{ name, body, field string }{
		{"by name and SID", `{"name":"bob","sid":"S-1-5-21-1-2-3-1105"}`, "name"},
		{"by neither", `{}`, "name"},
		{"by a SID that does not parse", `{"sid":"S-1-x"}`, "sid"},
	} {
		if r := d.do(t, http.MethodPost, resolvePath, tc.body, "Idempotency-Key", tc.name); r.status != http.StatusBadRequest || r.problem(t).Field != tc.field {
			t.Errorf("resolve-principal %s: answered %d %s, want 400 naming %s", tc.name, r.status, r.body, tc.field)
		}
	}
	if ops := d.operations(t); len(ops) != 0 {
		t.Fatalf("operations after the refusals: %+v, want none", ops)
	}
	in := d.installationRecord(t)
	for _, tc := range []struct {
		path, body string
		kind       apiv1.OpKind
		params     string
	}{
		{resolvePath, `{"sid":"S-1-5-21-1-2-3-1105"}`, apiv1.OpResolvePrincipal, `{"sid":"S-1-5-21-1-2-3-1105"}`},
		{testDirPath, "", apiv1.OpTestDirectory, ""},
		{rotatePath, "", apiv1.OpRotateMachinePassword, ""},
	} {
		r := d.do(t, http.MethodPost, tc.path, tc.body, "Idempotency-Key", string(tc.kind))
		if r.status != http.StatusAccepted {
			t.Errorf("%s: answered %d %s, want 202", tc.path, r.status, r.body)
			continue
		}
		i := slices.IndexFunc(d.operations(t), func(op entity.Operation) bool { return op.Kind == string(tc.kind) })
		if i < 0 {
			t.Errorf("%s: no %s operation written", tc.path, tc.kind)
			continue
		}
		op := d.operations(t)[i]
		if op.Target.Kind != entity.ResourceInstallation || !bytes.Equal(op.Target.ID, in.ID[:]) || string(op.Params) != tc.params {
			t.Errorf("%s: target %v params %q, want the installation %x and %q", tc.kind, op.Target, op.Params, in.ID, tc.params)
		}
	}
}

// A user's Kerberos name it already holds is kept, link and all, without
// asking the directory, which the node does not hold; a name removed is
// gone; a new name with no directory is refused 503 and changes nothing.
func TestKerberosNameKeptWithoutTheDirectory(t *testing.T) {
	s := serve(t, as(entity.RoleAdmin, false))
	var linked bytes.Buffer
	sid.EncodeSID(&linked, sid.ParseSIDMust("S-1-5-21-1-2-3-1105"))
	krb := entity.ProtocolID{Scheme: entity.SchemeKerberos, Value: "alice@AD.EXAMPLE", Linked: linked.String()}
	u := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: entity.ProtocolIDs{{Scheme: entity.SchemeUID, Value: "5001"}, krb}}
	gen, err := s.store.PutUser(t.Context(), u, 0, entity.Alloc{RID: true})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/users/" + uuid.UUID(u.Principal.ID).String()
	match := func(g uint64) []string { return []string{"If-Match", strconv.Quote(strconv.FormatUint(g, 10))} }
	krbOf := func() (entity.ProtocolID, bool) {
		rec, _ := s.userRecord(t, uuid.UUID(u.Principal.ID).String())
		i := slices.IndexFunc(rec.IDs, func(id entity.ProtocolID) bool { return id.Scheme == entity.SchemeKerberos })
		if i < 0 {
			return entity.ProtocolID{}, false
		}
		return rec.IDs[i], true
	}

	r := s.do(t, http.MethodPut, path, `{"spec":{"name":"alice","uid":5001,"kerberos":"alice@AD.EXAMPLE","disabled":true}}`, match(gen)...)
	if r.status != http.StatusOK {
		t.Fatalf("PUT keeping the Kerberos name: answered %d %s, want 200", r.status, r.body)
	}
	if got, ok := krbOf(); !ok || got != krb {
		t.Errorf("Kerberos name after the PUT: %+v, want %+v, its link kept", got, krb)
	}
	var back apiv1.User
	if err := json.Unmarshal(r.body, &back); err != nil || back.Spec.Kerberos != krb.Value {
		t.Errorf("answer %s: want the Kerberos name %s", r.body, krb.Value)
	}

	r = s.do(t, http.MethodPatch, path, `{"spec":{"disabled":false}}`, match(back.Generation)...)
	if r.status != http.StatusOK {
		t.Fatalf("PATCH leaving the Kerberos name: answered %d %s, want 200", r.status, r.body)
	}
	if got, ok := krbOf(); !ok || got != krb {
		t.Errorf("Kerberos name after the PATCH: %+v, want %+v, its link kept", got, krb)
	}
	if err := json.Unmarshal(r.body, &back); err != nil {
		t.Fatal(err)
	}

	r = s.do(t, http.MethodPut, path, `{"spec":{"name":"alice","uid":5001,"kerberos":"alice2@AD.EXAMPLE"}}`, match(back.Generation)...)
	if r.status != http.StatusServiceUnavailable || r.problem(t).Code != apiv1.CodeUnavailable {
		t.Errorf("PUT naming a new Kerberos name: answered %d %s, want 503 unavailable", r.status, r.body)
	}
	if got, _ := krbOf(); got != krb {
		t.Errorf("Kerberos name after the refusal: %+v, want %+v", got, krb)
	}

	r = s.do(t, http.MethodPut, path, `{"spec":{"name":"alice","uid":5001}}`, match(back.Generation)...)
	if r.status != http.StatusOK {
		t.Fatalf("PUT removing the Kerberos name: answered %d %s, want 200", r.status, r.body)
	}
	if got, ok := krbOf(); ok {
		t.Errorf("Kerberos name after its removal: %+v, want none", got)
	}
}

// GET /installation answers the installation's ID and its state, as the
// store holds it.
func TestInstallationRead(t *testing.T) {
	s := serve(t, as(entity.RoleViewer, false))
	if err := s.store.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		return tx.SetNTLMOff(7)
	}); err != nil {
		t.Fatal(err)
	}
	r := s.do(t, http.MethodGet, "/api/v1/installation", "")
	if r.status != http.StatusOK {
		t.Fatalf("GET: answered %d %s, want 200", r.status, r.body)
	}
	var got apiv1.Installation
	if err := json.Unmarshal(r.body, &got); err != nil {
		t.Fatal(err)
	}
	in := s.installationRecord(t)
	if got.ID != uuid.UUID(in.ID).String() || got.Status.Join.Joined || got.Status.NTLMOff != (apiv1.NTLMOff{Off: 7}) {
		t.Errorf("installation %+v, want ID %v, not joined, NTLM off at 7", got, uuid.UUID(in.ID))
	}
	if strings.Contains(string(r.body), "machine_password") {
		t.Errorf("answer %s names the machine password", r.body)
	}
}
