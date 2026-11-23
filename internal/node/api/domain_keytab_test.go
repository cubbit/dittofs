package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/keytab"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/node/api"
	"github.com/cubbit/dittofs/pkg/auth/sid"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/settings"
)

// opNamed is the operation of kind the store holds, failing when there is
// not exactly one.
func (s *served) opNamed(t *testing.T, kind apiv1.OpKind) entity.Operation {
	t.Helper()
	var out []entity.Operation
	for _, op := range s.operations(t) {
		if op.Kind == string(kind) {
			out = append(out, op)
		}
	}
	if len(out) != 1 {
		t.Fatalf("operations of kind %s: %+v, want one", kind, out)
	}
	return out[0]
}

// A join and a rotation exclude each other at their start: with an offline
// join running, a rotation is refused 409 naming it; with a rotation
// running, an offline join is refused 409 naming it and writes no version,
// and an online one is refused 409, writes no operation and releases its
// hold.
func TestJoinAndRotationExcludeEachOther(t *testing.T) {
	t.Run("rotation while a join runs", func(t *testing.T) {
		d := serveDomain(t, true)
		if r := d.do(t, http.MethodPost, joinPath, offlineJoin("DFS$", "pw-1")); r.status != http.StatusAccepted {
			t.Fatalf("offline join: answered %d %s, want 202", r.status, r.body)
		}
		join := d.opNamed(t, apiv1.OpJoinDomain)
		r := d.do(t, http.MethodPost, rotatePath, "", "Idempotency-Key", "rotate")
		if r.status != http.StatusConflict || r.problem(t).Code != apiv1.CodeOperationRunning || !slices.Contains(r.problem(t).Names, uuid.UUID(join.ID).String()) {
			t.Errorf("rotate while the join runs: answered %d %s, want 409 operation_running naming %v", r.status, r.body, uuid.UUID(join.ID))
		}
		if n := len(d.operations(t)); n != 1 {
			t.Errorf("operations: %d, want the join's alone", n)
		}
	})
	t.Run("join while a rotation runs", func(t *testing.T) {
		d := serveDomain(t, true)
		if r := d.do(t, http.MethodPost, rotatePath, "", "Idempotency-Key", "rotate"); r.status != http.StatusAccepted {
			t.Fatalf("rotate: answered %d %s, want 202", r.status, r.body)
		}
		rot := d.opNamed(t, apiv1.OpRotateMachinePassword)
		for _, tc := range []struct{ name, body string }{
			{"offline", offlineJoin("DFS$", "pw-1")},
			{"online", onlineJoin("", "Administrator@AD.EXAMPLE", "adm1n")},
		} {
			r := d.do(t, http.MethodPost, joinPath, tc.body)
			if r.status != http.StatusConflict || r.problem(t).Code != apiv1.CodeOperationRunning || !slices.Contains(r.problem(t).Names, uuid.UUID(rot.ID).String()) {
				t.Errorf("an %s join while the rotation runs: answered %d %s, want 409 operation_running naming %v", tc.name, r.status, r.body, uuid.UUID(rot.ID))
			}
		}
		if in := d.installationRecord(t); in.MachinePassword != (entity.SecretRef{}) {
			t.Errorf("machine password named: %v, want none", in.MachinePassword)
		}
		if n := len(d.operations(t)); n != 1 {
			t.Errorf("operations: %d, want the rotation's alone", n)
		}
		if h := d.joins.taken(); len(h) != 1 || !h[0].released {
			t.Errorf("holds %+v, want the online join's, released", h)
		}
	})
}

// bothRead is a fault for kvtest's wrapper that holds the first two writes
// of the running index, each made after its transaction read the index,
// until both have arrived, so two creating writes are in flight at once; a
// later write passes. Closing abort lets a held write go when the test
// fails first.
type bothRead struct {
	mu          sync.Mutex
	n           int
	both, abort chan struct{}
}

func (b *bothRead) At(op string, key []byte) (time.Duration, int, error) {
	if op != kvtest.OpSet || !strings.HasPrefix(string(key), "OPR‖") {
		return 0, 0, nil
	}
	b.mu.Lock()
	b.n++
	n := b.n
	if n == 2 {
		close(b.both)
	}
	b.mu.Unlock()
	if n <= 2 {
		select {
		case <-b.both:
		case <-b.abort:
		}
	}
	return 0, 0, nil
}

// A join's start and a rotation's start, both writes held until each has
// read the other's running index, cannot both commit: the second to commit
// retries on the tracked read, finds the first running and is refused 409
// operation_running naming it.
func TestJoinAndRotationStartsOverlap(t *testing.T) {
	d := serveDomain(t, true)
	b := &bothRead{both: make(chan struct{}), abort: make(chan struct{})}
	defer close(b.abort)
	d.disk.fault.SetFault(b)
	join := d.async(t, http.MethodPost, joinPath, onlineJoin("", "Administrator@AD.EXAMPLE", "adm1n"))
	rotate := d.async(t, http.MethodPost, rotatePath, "", "Idempotency-Key", "rotate")
	await(t, b.both, "both creating writes")
	var replies []reply
	for _, ch := range []<-chan reply{join, rotate} {
		select {
		case r := <-ch:
			replies = append(replies, r)
		case <-time.After(10 * time.Second):
			t.Fatal("a start not answered in 10s")
		}
	}
	d.disk.fault.SetFault(nil)
	ops := d.operations(t)
	if len(ops) != 1 {
		t.Fatalf("operations %+v, answers %d and %d; want one started", ops, replies[0].status, replies[1].status)
	}
	var refused reply
	for _, r := range replies {
		if r.status != http.StatusAccepted {
			refused = r
		}
	}
	if p := refused.problem(t); refused.status != http.StatusConflict || p.Code != apiv1.CodeOperationRunning || !slices.Contains(p.Names, uuid.UUID(ops[0].ID).String()) {
		t.Errorf("the second start: answered %d %s, want 409 operation_running naming %v", refused.status, refused.body, uuid.UUID(ops[0].ID))
	}
}

// Each malformed join or resolve is refused 400 naming its field, with
// nothing written: a version the request sets, an offline join given what
// only an online one takes, and a name, an account or an OU over 1024 bytes.
func TestDomainRefusalRows(t *testing.T) {
	d := serveDomain(t, true)
	long := strings.Repeat("a", 1025)
	for _, tc := range []struct {
		name, path, body, field string
		hdr                     []string
	}{
		{"a join setting the version", joinPath, `{"spec":{"mode":"offline","account":"DFS$","version":2},"credential":{"password":"` + b64("pw") + `"}}`, "spec.version", nil},
		{"an offline join given an ou", joinPath, `{"spec":{"mode":"offline","account":"DFS$","ou":"OU=x,DC=ad"},"credential":{"password":"` + b64("pw") + `"}}`, "spec", nil},
		{"an offline join given a host name", joinPath, `{"spec":{"mode":"offline","account":"DFS$","dns_host_name":"dfs.ad"},"credential":{"password":"` + b64("pw") + `"}}`, "spec", nil},
		{"a join naming a long account", joinPath, offlineJoin(long, "pw"), "spec.account", nil},
		{"an online join naming a long OU", joinPath, onlineJoin(`,"ou":"OU=`+long+`"`, "Administrator@AD.EXAMPLE", "pw"), "spec.ou", nil},
		{"a join of no mode", joinPath, `{"spec":{"mode":"sideways"},"credential":{"password":"` + b64("pw") + `"}}`, "spec.mode", nil},
		{"a resolve of a long name", resolvePath, `{"name":"` + long + `"}`, "name", []string{"Idempotency-Key", "long"}},
	} {
		r := d.do(t, http.MethodPost, tc.path, tc.body, tc.hdr...)
		if r.status != http.StatusBadRequest || r.problem(t).Field != tc.field {
			t.Errorf("%s: answered %d %s, want 400 naming %s", tc.name, r.status, r.body, tc.field)
		}
	}
	if ops := d.operations(t); len(ops) != 0 {
		t.Errorf("operations: %+v, want none", ops)
	}
	if in := d.installationRecord(t); in.MachinePassword != (entity.SecretRef{}) {
		t.Errorf("machine password named: %v, want none", in.MachinePassword)
	}
	if h := d.joins.taken(); len(h) != 0 {
		t.Errorf("holds %+v, want none", h)
	}
}

// The domain's audit records name the installation by its ID: a refused
// join's, and an offline join's own write of the machine password, which
// counts as an applied audit record beside the operation's.
func TestDomainAuditsNameTheInstallation(t *testing.T) {
	d := serveDomain(t, true)
	in := d.installationRecord(t)
	d.do(t, http.MethodPost, joinPath, onlineJoin(`,"dns_host_name":"no host"`, "Administrator@AD.EXAMPLE", "pw"))
	audits := d.audits(t)
	if len(audits) != 1 || !bytes.Equal(audits[0].Target.ID, in.ID[:]) || audits[0].Target.Kind != entity.ResourceInstallation {
		t.Fatalf("audit after a refused join: %+v, want one record naming the installation %x", audits, in.ID)
	}
	if r := d.do(t, http.MethodPost, joinPath, offlineJoin("DFS$", "pw-1")); r.status != http.StatusAccepted {
		t.Fatalf("offline join: answered %d %s, want 202", r.status, r.body)
	}
	audits = d.audits(t)
	i := slices.IndexFunc(audits, func(a entity.AuditRecord) bool { return string(a.Change) == `{"machine_password":"set"}` })
	if i < 0 || !bytes.Equal(audits[i].Target.ID, in.ID[:]) || audits[i].Target.Kind != entity.ResourceInstallation {
		t.Errorf("audit %+v: want the machine password's set naming the installation %x", audits, in.ID)
	}
	if got := sample(t, d.reg, "dittofs_audit_records_total", map[string]string{"outcome": "applied"}); got != 2 {
		t.Errorf("applied audit records counted: %v, want 2, the set and the operation's", got)
	}
}

// A machine's Kerberos name moved to a user by an admin keeps the machine's
// linked SID with no directory to ask: the write answers 202 with the
// machine's user deletion and a Location naming it, its audit record's
// outcome is that operation's ID, the user holds the name linked as the
// machine held it, and the machine is found no more. A second user then
// naming the name is refused 409, as another user holds it.
func TestMachineSpellingMovedToAUser(t *testing.T) {
	s := serve(t, as(entity.RoleAdmin, false))
	krb, m := putMachine(t, s, "host/build01@AD.EXAMPLE")
	alice, _ := s.createUser(t, apiv1.UserSpec{Name: "alice"}, "alice")
	r := s.do(t, http.MethodPut, "/api/v1/users/"+alice.ID, `{"spec":{"name":"alice","uid":`+strconv.FormatUint(uint64(*alice.Spec.UID), 10)+`,"kerberos":"host/build01@AD.EXAMPLE"}}`,
		"If-Match", strconv.Quote(strconv.FormatUint(alice.Generation, 10)))
	if r.status != http.StatusAccepted {
		t.Fatalf("PUT moving the machine's name: answered %d %s, want 202", r.status, r.body)
	}
	del := s.opNamed(t, apiv1.OpUserDelete)
	if del.Target.Kind != entity.ResourceUser || !bytes.Equal(del.Target.ID, m.ID[:]) {
		t.Errorf("deletion %+v, want a user deletion of the machine %v", del.Target, m.ID)
	}
	id := uuid.UUID(del.ID).String()
	var op apiv1.Operation
	if err := json.Unmarshal(r.body, &op); err != nil || op.ID != id {
		t.Errorf("answer %s: want the deletion %s", r.body, id)
	}
	if loc := r.header.Get("Location"); loc != "/api/v1/operations/"+id {
		t.Errorf("Location %q, want the deletion %s", loc, id)
	}
	audits := s.audits(t)
	if last := audits[len(audits)-1]; string(last.Outcome) != id || last.Target.Kind != entity.ResourceUser {
		t.Errorf("the write's audit: outcome %q on %v, want the deletion's ID %s on the user", last.Outcome, last.Target.Kind, id)
	}
	u, _ := s.userRecord(t, alice.ID)
	if i := slices.IndexFunc(u.IDs, func(id entity.ProtocolID) bool { return id.Scheme == entity.SchemeKerberos }); i < 0 || u.IDs[i] != krb {
		t.Errorf("alice's spellings %v, want %v with the machine's link", u.IDs, krb)
	}
	if _, ok, err := s.store.User(t.Context(), m); err != nil || ok {
		t.Errorf("the machine after the move: found %v (%v), want gone", ok, err)
	}
	r = s.do(t, http.MethodPost, "/api/v1/users", `{"spec":{"name":"bob","kerberos":"host/build01@AD.EXAMPLE"}}`, "Idempotency-Key", "bob")
	if r.status != http.StatusConflict || r.problem(t).Field != "spec.kerberos" {
		t.Errorf("a second user naming the moved name: answered %d %s, want 409 naming spec.kerberos", r.status, r.body)
	}
}

// A machine's Kerberos name named with its realm in lower case is the
// machine's name: an admin's create naming it so moves it, answered 202
// with the machine's deletion, the user holding the stored spelling.
func TestMachineSpellingMovedUnderALowerCaseRealm(t *testing.T) {
	s := serve(t, as(entity.RoleAdmin, false))
	krb, m := putMachine(t, s, "host/build01@AD.EXAMPLE")
	r := s.do(t, http.MethodPost, "/api/v1/users", `{"spec":{"name":"alice","kerberos":"host/build01@ad.example"}}`, "Idempotency-Key", "alice")
	if r.status != http.StatusAccepted {
		t.Fatalf("POST moving the machine's name under a lower-case realm: answered %d %s, want 202", r.status, r.body)
	}
	if del := s.opNamed(t, apiv1.OpUserDelete); !bytes.Equal(del.Target.ID, m.ID[:]) {
		t.Errorf("deletion %+v, want a user deletion of the machine %v", del.Target, m.ID)
	}
	var h entity.User
	if err := s.store.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		var err error
		h, _, err = tx.Holder(entity.ProtocolID{Scheme: entity.SchemeKerberos, Value: krb.Value})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if h.Principal.Kind != entity.PrincipalUser || h.Name != "alice" || !slices.Contains(h.IDs, krb) {
		t.Errorf("the name's holder %+v, want alice holding %v with the machine's link", h, krb)
	}
}

// putMachine writes a machine holding the krb spelling name, linked, and
// returns the spelling and the machine.
func putMachine(t *testing.T, s *served, name string) (entity.ProtocolID, entity.Principal) {
	t.Helper()
	var linked bytes.Buffer
	sid.EncodeSID(&linked, sid.ParseSIDMust("S-1-5-21-1-2-3-1199"))
	krb := entity.ProtocolID{Scheme: entity.SchemeKerberos, Value: name, Linked: linked.String()}
	m := entity.Principal{Kind: entity.PrincipalMachine, ID: entity.NewPrincipalID()}
	if _, err := s.store.PutUser(t.Context(), entity.User{Principal: m, IDs: entity.ProtocolIDs{krb}}, 0, entity.Alloc{}); err != nil {
		t.Fatal(err)
	}
	return krb, m
}

// An operator may not move a machine's Kerberos name to a user: the write is
// refused 409 naming spec.kerberos, the machine kept and no deletion
// started.
func TestMachineSpellingMoveRefusedToAnOperator(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	_, m := putMachine(t, s, "host/build01@AD.EXAMPLE")
	r := s.do(t, http.MethodPost, "/api/v1/users", `{"spec":{"name":"alice","kerberos":"host/build01@AD.EXAMPLE"}}`, "Idempotency-Key", "alice")
	if r.status != http.StatusConflict || r.problem(t).Field != "spec.kerberos" {
		t.Errorf("an operator moving a machine's name: answered %d %s, want 409 naming spec.kerberos", r.status, r.body)
	}
	if _, ok, err := s.store.User(t.Context(), m); err != nil || !ok {
		t.Errorf("the machine after the refusal: found %v (%v), want kept", ok, err)
	}
	if ops := s.operations(t); len(ops) != 0 {
		t.Errorf("operations: %+v, want none", ops)
	}
}

// secretRows counts the secret versions the store holds, read raw.
func (d *domain) secretRows(t *testing.T) int {
	t.Helper()
	n := 0
	if err := d.disk.raw.View(t.Context(), func(r kv.Reader) error {
		for _, err := range r.Scan([]byte{keys.KindSEC}, nil) {
			if err != nil {
				return err
			}
			n++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// keytabOf is a keytab of one AES256 key for name in AD.EXAMPLE, its etype
// relabelled when etype is another.
func keytabOf(t *testing.T, name string, etype int32) []byte {
	t.Helper()
	b := keytabIn(t, name, "AD.EXAMPLE")
	kt := keytab.New()
	if err := kt.Unmarshal(b); err != nil {
		t.Fatal(err)
	}
	kt.Entries[0].Key.KeyType = etype
	b, err := kt.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// keytabIn is a keytab of one AES256 key for name in realm.
func keytabIn(t *testing.T, name, realm string) []byte {
	t.Helper()
	kt := keytab.New()
	if err := kt.AddEntry(name, realm, "pw", time.Unix(1, 0), 1, etypeID.AES256_CTS_HMAC_SHA1_96); err != nil {
		t.Fatal(err)
	}
	b, err := kt.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const keytabPath = "/api/v1/installation/keytab"

func putKeytab(b []byte) string { return `{"spec":{"value":"` + b64(string(b)) + `"}}` }

// The keytab's PUT is refused to an operator, 403, and, with no sealer, to
// an admin, 503, each writing no setting.
func TestKeytabRouteRefusals(t *testing.T) {
	good := keytabOf(t, "cifs/nas.ad.example", etypeID.AES256_CTS_HMAC_SHA1_96)
	for _, tc := range []struct {
		role   entity.APIRole
		status int
	}{{entity.RoleOperator, http.StatusForbidden}, {entity.RoleAdmin, http.StatusServiceUnavailable}} {
		t.Run(tc.role.String(), func(t *testing.T) {
			s := serve(t, as(tc.role, false))
			if r := s.do(t, http.MethodPut, keytabPath, putKeytab(good), "If-None-Match", "*"); r.status != tc.status {
				t.Errorf("PUT as %s with no sealer: answered %d %s, want %d", tc.role, r.status, r.body, tc.status)
			}
			for st, err := range s.store.Settings(t.Context(), installation) {
				if err != nil || st.Key == "identity.keytab" {
					t.Errorf("setting %+v (%v), want no identity.keytab", st, err)
				}
			}
		})
	}
}

// A keytab identity's check refuses, and one holding a key for a name the
// computer account holds in the directory's realm, compared with ASCII
// letters folded, are each refused before anything is written; with a
// sealer but no settings table, any other, one for the account's name in
// another realm among them, is refused 503 before it is sealed, so no
// version is written either. The keytab's bytes are found nowhere.
func TestKeytabRoute(t *testing.T) {
	good := keytabOf(t, "cifs/nas.ad.example", etypeID.AES256_CTS_HMAC_SHA1_96)
	d := serveDomain(t, true)
	d.putSetting(t, installation, "identity.directory.realm", `"ad.example"`)
	j := entity.MachineJoin{Joined: true, Account: "DFS$", Names: []string{"HOST/dfs.ad.example"}}
	if err := d.store.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error { return tx.SetMachineJoin(j) }); err != nil {
		t.Fatal(err)
	}
	rc4 := keytabOf(t, "cifs/nas.ad.example", etypeID.RC4_HMAC)
	held := keytabOf(t, "host/dfs.ad.example", etypeID.AES256_CTS_HMAC_SHA1_96)
	account := keytabOf(t, "dfs$", etypeID.AES256_CTS_HMAC_SHA1_96)
	elsewhere := keytabIn(t, "host/dfs.ad.example", "OTHER.EXAMPLE")
	before := d.secretRows(t)
	var replies []reply
	for _, tc := range []struct {
		name   string
		b      []byte
		status int
		code   apiv1.Code
	}{
		{"an RC4 keytab", rc4, http.StatusBadRequest, "invalid"},
		{"a keytab for a name the join holds", held, http.StatusBadRequest, "invalid"},
		{"a keytab for the computer account's own name", account, http.StatusBadRequest, "invalid"},
		{
			"a good keytab, refused by the missing table", good,
			http.StatusServiceUnavailable, "unavailable",
		},
		{
			"the account's name in another realm, refused by the missing table", elsewhere,
			http.StatusServiceUnavailable, "unavailable",
		},
	} {
		r := d.do(t, http.MethodPut, keytabPath, putKeytab(tc.b), "If-None-Match", "*")
		replies = append(replies, r)
		if r.status != tc.status || r.problem(t).Code != tc.code {
			t.Errorf("%s: answered %d %s, want %d %s", tc.name, r.status, r.body, tc.status, tc.code)
		}
	}
	var secrets []string
	for _, b := range [][]byte{good, rc4, held, account, elsewhere} {
		secrets = append(secrets, string(b[len(b)-32:])) // each keytab's key
	}
	if l := d.leaks(t, replies, secrets...); len(l) != 0 {
		t.Errorf("found %q, want no keytab key anywhere and no idempotency record", l)
	}
	if n := d.secretRows(t); n != before {
		t.Errorf("secret versions: %d, want %d, none written", n, before)
	}
	for s, err := range d.store.Settings(t.Context(), installation) {
		if err != nil {
			t.Fatal(err)
		}
		if s.Key == "identity.keytab" {
			t.Errorf("identity.keytab written: %+v, want none", s)
		}
	}
}

// The keytab's set succeeds once the node hands in the table its setting's
// write is judged by: with the identity section declared and a sealer
// present, a good keytab is stored as its sealed first version beside the
// reference row naming it, written in the form a reference field decodes,
// and the answer carries the reference and the version, never the value.
// The secret is committed first, so the table's reference gate reads, in
// the same transaction, the version the write just sealed.
func TestKeytabSetStoresItsReference(t *testing.T) {
	good := keytabOf(t, "cifs/nas.ad.example", etypeID.AES256_CTS_HMAC_SHA1_96)

	var p sealing

	s := serve(t, as(entity.RoleAdmin, false), func(deps *api.Deps) {
		p = provider(t, deps.Store.(kvstore.ControlPlane))
		deps.Sealer = p
		deps.Settings = settings.Table{identity.Section()}
	})

	r := s.do(t, http.MethodPut, keytabPath, putKeytab(good), "If-None-Match", "*")

	if r.status != http.StatusOK {
		t.Fatalf(
			"PUT the keytab over the declared table: answered %d %s, want 200",
			r.status,
			r.body,
		)
	}

	var got apiv1.SecretRef
	if err := json.Unmarshal(r.body, &got); err != nil {
		t.Fatalf("the set's answer %s: %v", r.body, err)
	}

	if got.Version != 1 || got.Ref == "" {
		t.Errorf("the set's answer %+v, want the reference at version 1", got)
	}

	row, ok := s.settingRow(t, installation, identity.KeytabKey)
	if !ok {
		t.Fatal("no identity.keytab row stored, want the one naming the sealed keytab")
	}

	var ref entity.SecretRef
	if err := json.Unmarshal([]byte(row), &ref); err != nil {
		t.Fatalf("the identity.keytab row %s: %v", row, err)
	}

	if uuid.UUID(ref).String() != got.Ref {
		t.Errorf(
			"the identity.keytab row names %s, want the answered reference %s",
			uuid.UUID(ref).String(),
			got.Ref,
		)
	}

	sec, err := s.store.Secret(t.Context(), ref, 0)
	if err != nil || sec.Version != 1 || sec.Kind != entity.SecretKeytab {
		t.Errorf(
			"the sealed keytab: version %d kind %s err %v, want version 1, a keytab",
			sec.Version,
			sec.Kind,
			err,
		)
	}

	if bytes.Contains(r.body, good) {
		t.Errorf("the set's answer carries the keytab's value: %s", r.body)
	}
}

// A write whose row may start an operation answers 202 with it when its
// handler starts the kind the row declares, as the document says it may;
// it is refused, writing nothing, when the handler starts any other kind.
func TestMayStartAnswersAsDocumented(t *testing.T) {
	b, err := api.SwitchDoc()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if got := at(doc, "paths", "/api/v1/switches/{id}", "put", "x-dittofs-may-start"); got != string(apiv1.OpShareDelete) {
		t.Fatalf("x-dittofs-may-start %v, want %s", got, apiv1.OpShareDelete)
	}
	s := serve(t, as(entity.RoleOperator, false))
	id := uuid.NewString()
	r := s.do(t, http.MethodPut, "/api/v1/switches/"+id, `{"spec":{"value":"start"}}`, "If-None-Match", "*")
	if r.status != http.StatusAccepted {
		t.Fatalf("PUT starting its kind: answered %d %s, want 202", r.status, r.body)
	}
	conforms(t, doc, http.MethodPut, "/api/v1/switches/{id}", r, "Location")
	var op apiv1.Operation
	if err := json.Unmarshal(r.body, &op); err != nil || op.Kind != apiv1.OpShareDelete {
		t.Errorf("answer %s: want the %s operation", r.body, apiv1.OpShareDelete)
	}
	other := uuid.NewString()
	r = s.do(t, http.MethodPut, "/api/v1/switches/"+other, `{"spec":{"value":"other"}}`, "If-None-Match", "*")
	if r.status != http.StatusInternalServerError {
		t.Errorf("PUT starting another kind: answered %d %s, want 500", r.status, r.body)
	}
	if got := s.knob(t, other); got.Generation != 0 {
		t.Errorf("knob after the refused start: %+v, want none", got)
	}
	if n := len(s.operations(t)); n != 1 {
		t.Errorf("operations: %d, want 1", n)
	}
}

// The document's installation setting PUT and DELETE, whose role is an
// operator's, say that identity.ntlm takes an admin.
func TestNTLMSettingNamedAdminOnly(t *testing.T) {
	doc := openAPIDoc(t)
	for _, m := range []string{"put", "delete"} {
		if got, _ := at(doc, "paths", "/api/v1/settings/{sub}", m, "description").(string); !strings.Contains(got, "identity.ntlm takes the admin role") {
			t.Errorf("%s /api/v1/settings/{sub} description %q, want it naming identity.ntlm as an admin's", m, got)
		}
	}
}

// The document names a join's modes, the closed set the route takes.
func TestJoinModeIsAnEnum(t *testing.T) {
	got := at(openAPIDoc(t), "components", "schemas", "JoinDomainSpecRequest", "properties", "mode", "enum")
	if vs, ok := got.([]any); !ok || !slices.Equal(vs, []any{string(apiv1.JoinOnline), string(apiv1.JoinOffline)}) {
		t.Errorf("join mode's enum %v, want [online offline]", got)
	}
}

// The keytab's DELETE is the installation's write, refused 403 to an
// operator, the refusal audited against the installation's ID.
func TestKeytabDeleteRefusedToAnOperator(t *testing.T) {
	op := serve(t, as(entity.RoleOperator, false))
	if r := op.do(t, http.MethodDelete, keytabPath, "", "If-Match", `"1"`, "Idempotency-Key", "del"); r.status != http.StatusForbidden {
		t.Errorf("an operator's DELETE: answered %d %s, want 403", r.status, r.body)
	}
	in := op.installationRecord(t)
	if a := op.audits(t); len(a) != 1 || a[0].Target.Kind != entity.ResourceInstallation || !bytes.Equal(a[0].Target.ID, in.ID[:]) {
		t.Errorf("the refusal's audit %+v, want one naming the installation %x", a, in.ID)
	}
}

// The keytab's DELETE, for an admin: 412 while no keytab is named, as no
// record holds the tag, and, with one named, judged by the settings table,
// whose empty table refuses it, the refusal audited against the
// installation's ID.
func TestKeytabDelete(t *testing.T) {
	s := serve(t, as(entity.RoleAdmin, false))
	if r := s.do(t, http.MethodDelete, keytabPath, "", "If-Match", `"1"`, "Idempotency-Key", "del-none"); r.status != http.StatusPreconditionFailed {
		t.Errorf("a DELETE with no keytab: answered %d %s, want 412, no record holding the tag", r.status, r.body)
	}
	ref := entity.SecretRef(uuid.New())
	s.putSetting(t, installation, "identity.keytab", refRow(t, ref))
	sealed := entity.Secret{
		ID: ref, Kind: entity.SecretKeytab, Version: 1,
		Owner: entity.SecretOwner{Scope: installation, Key: "identity.keytab"},
		Value: []byte("sealed"),
	}

	if err := s.store.PutSecret(t.Context(), sealed, 0); err != nil {
		t.Fatal(err)
	}

	r := s.do(t, http.MethodDelete, keytabPath, "",
		"If-Match", `"1"`, "Idempotency-Key", "del-one")

	if r.problem(t).Code != "unavailable" || r.status != http.StatusServiceUnavailable {
		t.Errorf(
			"a DELETE of the keytab: answered %d %s, want 503 naming the missing table",
			r.status, r.body)
	}
	in := s.installationRecord(t)
	audits := s.audits(t)
	if last := audits[len(audits)-1]; last.Target.Kind != entity.ResourceInstallation || !bytes.Equal(last.Target.ID, in.ID[:]) {
		t.Errorf("the refusal's audit %+v, want it naming the installation %x", last.Target, in.ID)
	}
}

// A join naming an account the stored keytab holds a key for is refused 409
// naming spec.account, online and offline, starting no operation, once
// identity has applied that keytab; before, and while identity has applied
// another keytab of the same version number, it is refused 503, since the
// names checked are those of the keytab identity applied. A join naming
// another account starts.
func TestJoinRefusedForAnAccountTheKeytabHolds(t *testing.T) {
	d := serveDomain(t, true)
	kt := keytabOf(t, "dfs$", etypeID.AES256_CTS_HMAC_SHA1_96)
	ref := entity.SecretRef(uuid.New())
	sealed, err := d.seal.Seal(t.Context(), entity.SecretKeytab, entity.SecretOwner{Scope: installation, Key: "identity.keytab"}, ref, slices.Clone(kt), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.store.PutSecret(t.Context(), sealed, 0); err != nil {
		t.Fatal(err)
	}

	d.putSetting(t, installation, "identity.keytab", refRow(t, ref))

	if r := d.do(t, http.MethodPost, joinPath, offlineJoin("DFS$", "pw-1")); r.status != http.StatusServiceUnavailable {
		t.Errorf("a join before identity applied the keytab: answered %d %s, want 503", r.status, r.body)
	}
	// identity applies another keytab, of the stored one's version number,
	// holding no key for DFS$: its names are not the stored keytab's.
	st := d.state
	st.Settings.Keytab = entity.SecretRef(uuid.New())
	st.Keytab = []secrets.Version{{Number: sealed.Version, Value: keytabOf(t, "cifs/nas.ad.example", etypeID.AES256_CTS_HMAC_SHA1_96)}}
	if err := d.ids.Apply(st); err != nil {
		t.Fatal(err)
	}
	if r := d.do(t, http.MethodPost, joinPath, offlineJoin("DFS$", "pw-1")); r.status != http.StatusServiceUnavailable {
		t.Errorf("a join while identity applied another keytab of the same version: answered %d %s, want 503", r.status, r.body)
	}
	st.Settings.Keytab = ref
	st.Keytab = []secrets.Version{{Number: sealed.Version, Value: kt}}
	if err := d.ids.Apply(st); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, body string }{
		{"offline", offlineJoin("DFS$", "pw-2")},
		{"online", onlineJoin(`,"account":"DFS$"`, "Administrator", "pw-3")},
	} {
		r := d.do(t, http.MethodPost, joinPath, tc.body)
		if r.status != http.StatusConflict || r.problem(t).Field != "spec.account" {
			t.Errorf("an %s join of DFS$: answered %d %s, want 409 naming spec.account", tc.name, r.status, r.body)
		}
	}
	if ops := d.operations(t); len(ops) != 0 {
		t.Fatalf("operations: %+v, want none", ops)
	}
	if r := d.do(t, http.MethodPost, joinPath, offlineJoin("NAS$", "pw-4")); r.status != http.StatusAccepted {
		t.Errorf("a join of NAS$: answered %d %s, want 202", r.status, r.body)
	}
}

// storeKeytab seals kt as the installation's keytab and names it, as a PUT
// would, without identity applying it.
func (d *domain) storeKeytab(t *testing.T, kt []byte) (entity.SecretRef, uint64) {
	t.Helper()
	ref := entity.SecretRef(uuid.New())
	sealed, err := d.seal.Seal(t.Context(), entity.SecretKeytab, entity.SecretOwner{Scope: installation, Key: "identity.keytab"}, ref, slices.Clone(kt), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.store.PutSecret(t.Context(), sealed, 0); err != nil {
		t.Fatal(err)
	}

	d.putSetting(t, installation, "identity.keytab", refRow(t, ref))
	return ref, sealed.Version
}

// A join's account is compared in its $ spelling, ASCII letters folded: a
// keytab key for BUILD01$ refuses a join naming the account in each
// spelling, 409 naming spec.account. An offline join refused so, and one
// refused 503 while identity has applied no stored keytab, which says so
// rather than asking for a retry, write no machine-password version.
func TestJoinRefusedForEachSpellingOfTheAccount(t *testing.T) {
	d := serveDomain(t, true)
	kt := keytabOf(t, "BUILD01$", etypeID.AES256_CTS_HMAC_SHA1_96)
	ref, ver := d.storeKeytab(t, kt)
	before := d.secretRows(t)
	r := d.do(t, http.MethodPost, joinPath, offlineJoin("build01", "pw-0"))
	if r.status != http.StatusServiceUnavailable || !strings.Contains(r.problem(t).Detail, "identity has not applied") {
		t.Errorf("a join before identity applied the keytab: answered %d %s, want 503 saying identity has not applied it", r.status, r.body)
	}
	if n := d.secretRows(t); n != before {
		t.Errorf("secret versions after the 503: %d, want %d, no machine password handed over", n, before)
	}
	st := d.state
	st.Settings.Keytab = ref
	st.Keytab = []secrets.Version{{Number: ver, Value: kt}}
	if err := d.ids.Apply(st); err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{"build01", "BUILD01", "build01$", "BUILD01$"} {
		r := d.do(t, http.MethodPost, joinPath, offlineJoin(account, "pw-1"))
		if r.status != http.StatusConflict || r.problem(t).Field != "spec.account" {
			t.Errorf("an offline join of %s: answered %d %s, want 409 naming spec.account", account, r.status, r.body)
		}
		r = d.do(t, http.MethodPost, joinPath, onlineJoin(`,"account":"`+account+`"`, "Administrator", "pw-2"))
		if r.status != http.StatusConflict || r.problem(t).Field != "spec.account" {
			t.Errorf("an online join of %s: answered %d %s, want 409 naming spec.account", account, r.status, r.body)
		}
	}
	if n := d.secretRows(t); n != before {
		t.Errorf("secret versions after the refused joins: %d, want %d, no machine password handed over", n, before)
	}
	if in := d.installationRecord(t); in.MachinePassword != (entity.SecretRef{}) {
		t.Errorf("machine password named: %v, want none", in.MachinePassword)
	}
	if ops := d.operations(t); len(ops) != 0 {
		t.Errorf("operations: %+v, want none", ops)
	}
}

// A stored identity.directory.realm that is no JSON string answers 500
// corrupt naming it, to a keytab's put and to a join naming an account,
// rather than an unclassified error.
func TestNonStringRealmAnswersCorrupt(t *testing.T) {
	d := serveDomain(t, true)
	d.putSetting(t, installation, "identity.directory.realm", `5`)
	if err := d.store.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		return tx.SetMachineJoin(entity.MachineJoin{Joined: true, Account: "DFS$"})
	}); err != nil {
		t.Fatal(err)
	}
	kt := keytabOf(t, "cifs/nas.ad.example", etypeID.AES256_CTS_HMAC_SHA1_96)
	r := d.do(t, http.MethodPut, keytabPath, putKeytab(kt), "If-None-Match", "*")
	if p := r.problem(t); r.status != http.StatusInternalServerError || p.Code != "corrupt" || p.Field != "identity.directory.realm" {
		t.Errorf("a keytab put under a numeric realm: answered %d %s, want 500 corrupt naming identity.directory.realm", r.status, r.body)
	}
	ref, ver := d.storeKeytab(t, kt)
	st := d.state
	st.Settings.Keytab = ref
	st.Keytab = []secrets.Version{{Number: ver, Value: kt}}
	if err := d.ids.Apply(st); err != nil {
		t.Fatal(err)
	}
	r = d.do(t, http.MethodPost, joinPath, offlineJoin("NAS$", "pw"))
	if p := r.problem(t); r.status != http.StatusInternalServerError || p.Code != "corrupt" || p.Field != "identity.directory.realm" {
		t.Errorf("a join under a numeric realm: answered %d %s, want 500 corrupt naming identity.directory.realm", r.status, r.body)
	}
}
