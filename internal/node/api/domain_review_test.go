package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/keytab"

	"github.com/cubbit/dittofs/internal/node/api"
	"github.com/cubbit/dittofs/pkg/auth/sid"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
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
		{"an offline join given takeover", joinPath, `{"spec":{"mode":"offline","account":"DFS$","takeover":true},"credential":{"password":"` + b64("pw") + `"}}`, "spec", nil},
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

// A machine's Kerberos name moved to a user keeps the machine's linked SID
// with no directory to ask: the user holds the name linked as the machine
// held it, and the machine is found no more.
func TestMachineSpellingMovedToAUser(t *testing.T) {
	s := serve(t, as(entity.RoleAdmin, false))
	var linked bytes.Buffer
	sid.EncodeSID(&linked, sid.ParseSIDMust("S-1-5-21-1-2-3-1199"))
	krb := entity.ProtocolID{Scheme: entity.SchemeKerberos, Value: "host/build01@AD.EXAMPLE", Linked: linked.String()}
	m := entity.Principal{Kind: entity.PrincipalMachine, ID: entity.NewPrincipalID()}
	if _, err := s.store.PutUser(t.Context(), entity.User{Principal: m, IDs: entity.ProtocolIDs{krb}}, 0, entity.Alloc{}); err != nil {
		t.Fatal(err)
	}
	alice, _ := s.createUser(t, apiv1.UserSpec{Name: "alice"}, "alice")
	r := s.do(t, http.MethodPut, "/api/v1/users/"+alice.ID, `{"spec":{"name":"alice","uid":`+strconv.FormatUint(uint64(*alice.Spec.UID), 10)+`,"kerberos":"host/build01@AD.EXAMPLE"}}`,
		"If-Match", strconv.Quote(strconv.FormatUint(alice.Generation, 10)))
	if r.status != http.StatusOK {
		t.Fatalf("PUT moving the machine's name: answered %d %s, want 200", r.status, r.body)
	}
	u, _ := s.userRecord(t, alice.ID)
	if i := slices.IndexFunc(u.IDs, func(id entity.ProtocolID) bool { return id.Scheme == entity.SchemeKerberos }); i < 0 || u.IDs[i] != krb {
		t.Errorf("alice's spellings %v, want %v with the machine's link", u.IDs, krb)
	}
	if _, ok, err := s.store.User(t.Context(), m); err != nil || ok {
		t.Errorf("the machine after the move: found %v (%v), want gone", ok, err)
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

// keytabOf is a keytab of one AES256 key for name, its etype relabelled
// when etype is another.
func keytabOf(t *testing.T, name string, etype int32) []byte {
	t.Helper()
	kt := keytab.New()
	if err := kt.AddEntry(name, "AD.EXAMPLE", "pw", time.Unix(1, 0), 1, etypeID.AES256_CTS_HMAC_SHA1_96); err != nil {
		t.Fatal(err)
	}
	kt.Entries[0].Key.KeyType = etype
	b, err := kt.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A keytab identity's check refuses, and one holding a key for a name the
// computer account holds, are each refused before anything is written; with a sealer, the empty settings table refuses the
// setting naming it, so no version is written either. The keytab's bytes are
// found nowhere.
//
// ponytail: no test reads a sealed keytab back raw, since the settings table
// the setting's write needs is not handed in yet; test it once it is.
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

func TestKeytabRoute(t *testing.T) {
	good := keytabOf(t, "cifs/nas.ad.example", etypeID.AES256_CTS_HMAC_SHA1_96)
	d := serveDomain(t, true)
	j := entity.MachineJoin{Joined: true, Account: "DFS$", Names: []string{"HOST/dfs.ad.example"}}
	if err := d.store.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error { return tx.SetMachineJoin(j) }); err != nil {
		t.Fatal(err)
	}
	rc4 := keytabOf(t, "cifs/nas.ad.example", etypeID.RC4_HMAC)
	held := keytabOf(t, "host/dfs.ad.example", etypeID.AES256_CTS_HMAC_SHA1_96)
	var replies []reply
	for _, tc := range []struct {
		name   string
		b      []byte
		status int
		code   apiv1.Code
	}{
		{"an RC4 keytab", rc4, http.StatusBadRequest, "invalid"},
		{"a keytab for a name the join holds", held, http.StatusBadRequest, "invalid"},
		{"a good keytab, refused by the empty table", good, http.StatusBadRequest, "unknown_key"},
	} {
		r := d.do(t, http.MethodPut, keytabPath, putKeytab(tc.b), "If-None-Match", "*")
		replies = append(replies, r)
		if r.status != tc.status || r.problem(t).Code != tc.code {
			t.Errorf("%s: answered %d %s, want %d %s", tc.name, r.status, r.body, tc.status, tc.code)
		}
	}
	var secrets []string
	for _, b := range [][]byte{good, rc4, held} {
		secrets = append(secrets, string(b[len(b)-32:])) // each keytab's key
	}
	if l := d.leaks(t, replies, secrets...); len(l) != 0 {
		t.Errorf("found %q, want no keytab key anywhere and no idempotency record", l)
	}
	if n := d.secretRows(t); n != 0 {
		t.Errorf("secret versions written: %d, want none", n)
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
