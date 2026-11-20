package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/auth/sid"
	pkgidentity "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/google/uuid"
	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/keytab"
)

type protocolSealer struct{ p *secrets.Provider }

func (s protocolSealer) Seal(ctx context.Context, kind entity.SecretKind, owner entity.SecretOwner, ref entity.SecretRef, value []byte, handedOver bool) (entity.Secret, error) {
	return s.p.Seal(ctx, secrets.Protocol, kind, owner, ref, value, secrets.Meta{HandedOver: handedOver})
}

// sealThen is a sealer that runs then once, after its first seal and before
// returning it, so a test can write a version between a seal and its write.
type sealThen struct {
	next sealer
	then func()
}

func (s *sealThen) Seal(ctx context.Context, kind entity.SecretKind, owner entity.SecretOwner, ref entity.SecretRef, value []byte, handedOver bool) (entity.Secret, error) {
	out, err := s.next.Seal(ctx, kind, owner, ref, value, handedOver)
	if f := s.then; f != nil {
		s.then = nil
		f()
	}
	return out, err
}

// joinEnv is a control plane over an in-memory Badger, with a real
// protocol-role secret provider and identity's join hold as Deps, the
// write a pre-write step commits of its own going straight to the store.
type joinEnv struct {
	cp kvstore.ControlPlane
	d  Deps
}

func newJoinEnv(t *testing.T) joinEnv {
	t.Helper()
	db, err := badgerkv.Open(badgerkv.Options{NodeID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := kvstore.Init(t.Context(), db, uuid.New()); err != nil {
		t.Fatal(err)
	}
	st, err := kvstore.Open(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	cp := st.ControlPlane()
	const passphrase = "race test passphrase"
	t.Setenv("DITTOFS_ENCRYPTION_PASSPHRASE", passphrase)
	b, err := keyprovider.GenerateKeyFile(passphrase)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "protocol.key")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	kp, err := keyprovider.NewProvider(t.Context(), keyprovider.Config{Kind: keyprovider.KindLocal, File: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kp.Close() })
	prov, err := secrets.New(cp, map[secrets.Role]keyprovider.KeyProvider{secrets.Protocol: kp})
	if err != nil {
		t.Fatal(err)
	}
	ids, err := identity.New(identity.Deps{Store: st, Clock: sim.NewTestClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))})
	if err != nil {
		t.Fatal(err)
	}
	return joinEnv{cp: cp, d: Deps{Store: cp, Sealer: protocolSealer{prov}, Joins: ids}}
}

// request is a join's request carrying spec and the credential's user and
// password.
func (e joinEnv) request(spec apiv1.JoinDomainSpec, user string, pw []byte) request[apiv1.JoinDomain] {
	rq := request[apiv1.JoinDomain]{Body: apiv1.JoinDomain{Spec: spec, Credential: apiv1.JoinCredential{User: user, Password: pw}}, d: e.d}
	rq.own = func(ctx context.Context, _ entity.ResourceRef, _ []byte, fn func(entity.ControlPlaneTx) error) error {
		return e.cp.Write(ctx, entity.WriteMeta{}, fn)
	}
	return rq
}

// offline is an offline join of DFS$ with pw, prepared.
func (e joinEnv) offline(t *testing.T, pw []byte) opStart {
	t.Helper()
	s, err := prepareJoin(t.Context(), e.request(apiv1.JoinDomainSpec{Mode: apiv1.JoinOffline, Account: "DFS$"}, "", pw))
	if err != nil {
		t.Fatalf("prepare the join: %v", err)
	}
	return s
}

// check runs s's check in a write of its own on the installation read there.
func (e joinEnv) check(t *testing.T, s opStart) error {
	t.Helper()
	return e.cp.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		in, err := tx.Installation()
		if err != nil {
			return err
		}
		return s.check(tx, in)
	})
}

// Two offline joins race: each writes its machine password version, the
// second after the first, before either starts its operation. The first's
// start, which reads the version tracked in its own transaction, is refused
// 409, since the version it would join with is no longer current; the
// second's starts.
func TestOfflineJoinStartsOnlyOnItsVersion(t *testing.T) {
	e := newJoinEnv(t)
	first, second := e.offline(t, []byte("pw-1")), e.offline(t, []byte("pw-2"))
	if p, _ := problemOf(e.check(t, first)); p.Code != apiv1.CodeOperationRunning {
		t.Errorf("the first join's start, its version superseded: %+v, want 409 operation_running", p)
	}
	if err := e.check(t, second); err != nil {
		t.Errorf("the second join's start on the current version: %v, want admitted", err)
	}
}

// A rotation started between an offline join's hand-over and its start is
// caught by the join's start, which reads the running rotation tracked in
// its own transaction: refused 409 operation_running naming it.
func TestOfflineJoinStartRefusedByARotationSinceItsHandOver(t *testing.T) {
	e := newJoinEnv(t)
	join := e.offline(t, []byte("pw-1"))
	var rot string
	if err := e.cp.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		in, err := tx.Installation()
		if err != nil {
			return err
		}
		op, err := start(t.Context(), tx, apiv1.OpRotateMachinePassword, installationRef(in), opStart{})
		rot = op.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	p, _ := problemOf(e.check(t, join))
	if p.Code != apiv1.CodeOperationRunning || !slices.Contains(p.Names, rot) {
		t.Errorf("the join's start with a rotation running: %+v, want 409 operation_running naming %s", p, rot)
	}
}

// A rotation's start is admitted while the machine password's current
// version is an orphan hand-over, one handed to an offline join whose
// operation never started: the rotation supersedes it.
func TestRotationAdmittedOverAnOrphanHandOver(t *testing.T) {
	e := newJoinEnv(t)
	e.offline(t, []byte("pw-1")) // the hand-over commits; its operation is never started
	rotate, err := prepareRotate(t.Context(), request[none]{d: e.d})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.check(t, rotate); err != nil {
		t.Errorf("a rotation's start over the orphan hand-over: %v, want admitted", err)
	}
}

// An online join's start reads the directory's base again, tracked, and is
// refused 503 when it changed since the unit was checked against it.
func TestOnlineJoinStartRereadsTheBase(t *testing.T) {
	e := newJoinEnv(t)
	putBase := func(v string) {
		t.Helper()
		if err := e.cp.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
			old, _, err := tx.Setting(installation, directoryBase)
			if err != nil {
				return err
			}
			_, err = tx.PutSetting(entity.Setting{Scope: installation, Key: directoryBase, Value: []byte(v)}, old.Generation)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	putBase(`"DC=ad,DC=example"`)
	join, err := prepareJoin(t.Context(), e.request(apiv1.JoinDomainSpec{Mode: apiv1.JoinOnline, OU: "OU=Servers,DC=ad,DC=example"}, "Administrator", []byte("pw")))
	if err != nil {
		t.Fatal(err)
	}
	defer join.release()
	if err := e.check(t, join); err != nil {
		t.Fatalf("the join's start on the base it read: %v, want admitted", err)
	}
	putBase(`"DC=other,DC=example"`)
	if p, _ := problemOf(e.check(t, join)); p.Code != apiv1.CodeUnavailable {
		t.Errorf("the join's start after the base changed: %+v, want 503 unavailable", p)
	}
}

// An offline join's password is cleared once it is sealed and written, and
// a keytab's value once it is sealed, even when the set is then refused.
func TestSealedPlaintextCleared(t *testing.T) {
	e := newJoinEnv(t)
	pw := []byte("pw-1")
	e.offline(t, pw)
	if !bytes.Equal(pw, make([]byte, len(pw))) {
		t.Errorf("the offline join's password after its hand-over: %q, want zeroed", pw)
	}
	kt := keytab.New()
	if err := kt.AddEntry("cifs/nas.ad.example", "AD.EXAMPLE", "pw", time.Unix(1, 0), 1, etypeID.AES256_CTS_HMAC_SHA1_96); err != nil {
		t.Fatal(err)
	}
	v, err := kt.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	rq := request[apiv1.Credential]{Body: apiv1.Credential{Spec: apiv1.CredentialSpec{Value: v}}, d: e.d}
	if _, err := keytabs.seal(t.Context(), rq); err != nil {
		t.Fatalf("seal the keytab: %v", err)
	}
	if !bytes.Equal(v, make([]byte, len(v))) {
		t.Errorf("the keytab after its seal: %x, want zeroed", v)
	}
}

// wipe zeroes every byte slice a request's body holds, through its structs
// and pointers, so a keyless row's credential is cleared when the request is
// refused before its step takes it.
func TestWipe(t *testing.T) {
	pw, extra := []byte("s3cret"), []byte("more")
	body := struct {
		apiv1.JoinDomain
		P *struct{ B []byte }
		N string
	}{JoinDomain: apiv1.JoinDomain{Credential: apiv1.JoinCredential{User: "Administrator", Password: pw}}, P: &struct{ B []byte }{extra}, N: "kept"}
	wipe(reflect.ValueOf(&body).Elem())
	if !bytes.Equal(pw, make([]byte, len(pw))) || !bytes.Equal(extra, make([]byte, len(extra))) {
		t.Errorf("after wipe: password %q, nested %q, want both zeroed", pw, extra)
	}
	if body.N != "kept" || body.Credential.User != "Administrator" {
		t.Errorf("after wipe: strings %q, %q, want kept", body.N, body.Credential.User)
	}
}

// rdns compares two spellings of one name equal: an escape written as a
// character or as its hex byte, the pairs of a multi-valued RDN in either
// order, and any case; it refuses a name with a pair lacking a part or a
// trailing escape.
func TestRDNs(t *testing.T) {
	for _, tc := range []struct{ a, b string }{
		{`OU=a\,b,DC=ad`, `ou=A\2Cb, dc=AD`},
		{`CN=x+OU=y,DC=ad`, `OU=y+CN=x,DC=ad`},
	} {
		a, okA := rdns(tc.a)
		b, okB := rdns(tc.b)
		if !okA || !okB || !slices.EqualFunc(a, b, slices.Equal) {
			t.Errorf("rdns(%q) = %q, %v; rdns(%q) = %q, %v; want equal", tc.a, a, okA, tc.b, b, okB)
		}
	}
	for _, bad := range []string{"", "OU", "OU=", "=x", `OU=a\`, "OU=a,", "OU=a+"} {
		if got, ok := rdns(bad); ok {
			t.Errorf("rdns(%q) = %q, want refused", bad, got)
		}
	}
}

// A computer account's names written after the keytab's pre-write check
// passed are caught by its put, which runs the check again on the
// installation read tracked in the write's transaction.
func TestKeytabNamesCheckedInTheWrite(t *testing.T) {
	cp := ntlmStore(t)
	kt := keytab.New()
	if err := kt.AddEntry("host/dfs.ad.example", "AD.EXAMPLE", "pw", time.Unix(1, 0), 1, etypeID.AES256_CTS_HMAC_SHA1_96); err != nil {
		t.Fatal(err)
	}
	b, err := kt.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		_, err := tx.PutSetting(entity.Setting{Scope: installation, Key: directoryRealm, Value: []byte(`"AD.EXAMPLE"`)}, 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	inTx, err := keytabs.check(t.Context(), Deps{Store: cp}, b)
	if err != nil {
		t.Fatalf("the check before any join: %v, want admitted", err)
	}
	j := entity.MachineJoin{Joined: true, Account: "DFS$", Names: []string{"host/dfs.ad.example"}}
	if err := cp.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error { return tx.SetMachineJoin(j) }); err != nil {
		t.Fatal(err)
	}
	err = cp.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		_, _, err := keytabs.put(t.Context(), request[apiv1.Credential]{d: Deps{Store: cp}}, sealedSecret{inTx: inTx}, tx)
		return err
	})
	if p, _ := problemOf(err); p.Status != 400 || p.Field != "spec.value" {
		t.Errorf("the keytab's put, the join since naming the key: %v, want 400 naming spec.value, ahead of the settings table", err)
	}
}

// A realm setting changed after the keytab's pre-write check passed is
// caught by its put, which reads the realm again tracked in the write's
// transaction: a key the check admitted under another realm is refused
// once the realm names the account's.
func TestKeytabRealmCheckedInTheWrite(t *testing.T) {
	cp := ntlmStore(t)
	kt := keytab.New()
	if err := kt.AddEntry("host/dfs.ad.example", "AD.EXAMPLE", "pw", time.Unix(1, 0), 1, etypeID.AES256_CTS_HMAC_SHA1_96); err != nil {
		t.Fatal(err)
	}
	b, err := kt.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	putRealm := func(v string) {
		t.Helper()
		if err := cp.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
			old, _, err := tx.Setting(installation, directoryRealm)
			if err != nil {
				return err
			}
			_, err = tx.PutSetting(entity.Setting{Scope: installation, Key: directoryRealm, Value: []byte(v)}, old.Generation)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	putRealm(`"OTHER.EXAMPLE"`)
	j := entity.MachineJoin{Joined: true, Account: "DFS$", Names: []string{"host/dfs.ad.example"}}
	if err := cp.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error { return tx.SetMachineJoin(j) }); err != nil {
		t.Fatal(err)
	}
	inTx, err := keytabs.check(t.Context(), Deps{Store: cp}, b)
	if err != nil {
		t.Fatalf("the check, the key in another realm than the account's: %v, want admitted", err)
	}
	putRealm(`"AD.EXAMPLE"`)
	err = cp.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		_, _, err := keytabs.put(t.Context(), request[apiv1.Credential]{d: Deps{Store: cp}}, sealedSecret{inTx: inTx}, tx)
		return err
	})
	if p, _ := problemOf(err); p.Status != 400 || p.Field != "spec.value" {
		t.Errorf("the keytab's put, the realm since naming the key's: %v, want 400 naming spec.value", err)
	}
}

// Every refusal of a join before its hold or its seal clears the password
// the request carried.
func TestJoinRefusalClearsThePassword(t *testing.T) {
	cp := ntlmStore(t)
	for _, tc := range []struct {
		name string
		spec apiv1.JoinDomainSpec
		user string
	}{
		{"online, an OU outside the base", apiv1.JoinDomainSpec{Mode: apiv1.JoinOnline, OU: "OU=x,DC=other"}, "Administrator"},
		{"online, no identity", apiv1.JoinDomainSpec{Mode: apiv1.JoinOnline}, "Administrator"},
		{"offline, no identity", apiv1.JoinDomainSpec{Mode: apiv1.JoinOffline, Account: "DFS$"}, ""},
	} {
		pw := []byte("s3cret")
		rq := request[apiv1.JoinDomain]{Body: apiv1.JoinDomain{Spec: tc.spec, Credential: apiv1.JoinCredential{User: tc.user, Password: pw}}, d: Deps{Store: cp}}
		if _, err := prepareJoin(t.Context(), rq); err == nil {
			t.Fatalf("%s: prepared, want refused", tc.name)
		}
		if string(pw) != "\x00\x00\x00\x00\x00\x00" {
			t.Errorf("%s: password after the refusal %q, want zeroed", tc.name, pw)
		}
	}
}

// withKerberos refuses 503, keeping the machine and starting nothing, a
// link the step took from a machine that no longer holds it as taken: one
// whose spelling changed since, and one deleted since.
func TestWithKerberosRefusesAStaleMachineLink(t *testing.T) {
	name := "host/build01@AD.EXAMPLE"
	var linked bytes.Buffer
	sid.EncodeSID(&linked, sid.ParseSIDMust("S-1-5-21-1-2-3-1199"))
	for _, tc := range []struct {
		name   string
		holder entity.ProtocolIDs // the machine's spellings in the store; none, no machine
	}{
		{"a machine whose link changed", entity.ProtocolIDs{{Scheme: entity.SchemeKerberos, Value: name, Linked: linked.String()}}},
		{"a machine deleted since", nil},
	} {
		cp := ntlmStore(t)
		m := entity.Principal{Kind: entity.PrincipalMachine, ID: entity.NewPrincipalID()}
		if tc.holder != nil {
			if _, err := cp.PutUser(t.Context(), entity.User{Principal: m, IDs: tc.holder}, 0, entity.Alloc{}); err != nil {
				t.Fatal(err)
			}
		}
		step := krbLink{id: entity.ProtocolID{Scheme: entity.SchemeKerberos, Value: name}, machine: true}
		err := cp.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
			_, err := withKerberos(tx, func(k apiv1.OpKind, target entity.ResourceRef) error {
				_, err := start(t.Context(), tx, k, target, opStart{})
				return err
			}, nil, name, step)
			return err
		})
		if p, _ := problemOf(err); p.Code != apiv1.CodeUnavailable {
			t.Errorf("%s: %v, want 503 unavailable", tc.name, err)
		}
		for op, err := range cp.Operations(t.Context(), entity.OperationID{}) {
			t.Errorf("%s: operation %+v (%v), want none started", tc.name, op, err)
		}
		if _, ok, err := cp.User(t.Context(), m); err != nil || ok != (tc.holder != nil) {
			t.Errorf("%s: the machine found %v (%v), want %v", tc.name, ok, err, tc.holder != nil)
		}
	}
}

// accountHolds refuses a key for a name the joined account holds in the
// recorded realm only, and in every realm when no realm is recorded or the
// recorded one is empty; it
// refuses nothing while not joined.
func TestAccountHolds(t *testing.T) {
	j := entity.MachineJoin{Joined: true, Account: "DFS$", Names: []string{"HOST/dfs.ad.example"}}
	realm := entity.Setting{Value: []byte(`"AD.EXAMPLE"`)}
	in := func(name, r string) []pkgidentity.KeytabName { return []pkgidentity.KeytabName{{Name: name, Realm: r}} }
	for _, tc := range []struct {
		name    string
		join    entity.MachineJoin
		realm   entity.Setting
		ok      bool
		names   []pkgidentity.KeytabName
		refused bool
	}{
		{"a held name in the recorded realm", j, realm, true, in("host/dfs.ad.example", "ad.example"), true},
		{"a held name in another realm", j, realm, true, in("host/dfs.ad.example", "OTHER.EXAMPLE"), false},
		{"the account's name with no realm recorded", j, realm, false, in("dfs$", "OTHER.EXAMPLE"), true},
		{"another name with no realm recorded", j, realm, false, in("cifs/nas.ad.example", "AD.EXAMPLE"), false},
		{"the account's name under an empty realm", j, entity.Setting{Value: []byte(`""`)}, true, in("dfs$", "OTHER.EXAMPLE"), true},
		{"a held name while not joined", entity.MachineJoin{Account: "DFS$"}, realm, false, in("dfs$", "AD.EXAMPLE"), false},
	} {
		err := accountHolds(tc.join, tc.realm, tc.ok, tc.names)
		if (err != nil) != tc.refused {
			t.Errorf("%s: accountHolds = %v, want refused %v", tc.name, err, tc.refused)
		}
	}
}

// A write refused before its step clears the body it read, and what it
// decoded, when the body carries a credential or a secret: a join refused
// for its Idempotency-Key, whose body holds the administrator's password,
// and a keytab put refused for a precondition it does not carry, whose body
// holds the keytab.
func TestRefusedWriteClearsItsBody(t *testing.T) {
	s := newServer(Deps{Store: ntlmStore(t), Metrics: metrics.New("test", "test"), Logger: slog.New(slog.DiscardHandler), Ready: func() string { return "ready" },
		Authenticate: func(*http.Request) (auth.Caller, error) {
			return auth.Caller{Principal: entity.Principal{Kind: entity.PrincipalUser, ID: entity.NewPrincipalID()}, Role: entity.RoleAdmin}, nil
		}}, routes)
	var seen []any
	s.seen = func(v any) { seen = append(seen, v) }
	for _, tc := range []struct {
		name, method, path, body, header string
		decoded                          func(any) []byte
	}{
		{"a join carrying an Idempotency-Key", http.MethodPost, "/api/v1/installation:join-domain",
			`{"spec":{"mode":"offline","account":"DFS$"},"credential":{"password":"czNjcmV0"}}`, "Idempotency-Key", nil},
		{"a keytab put carrying no precondition", http.MethodPut, "/api/v1/installation/keytab", `{"spec":{"value":"czNjcmV0"}}`, "",
			func(v any) []byte { return v.(*apiv1.Credential).Spec.Value }},
	} {
		seen = nil
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		if tc.header != "" {
			r.Header.Set(tc.header, "k")
		}
		w := httptest.NewRecorder()
		s.api.Handler.ServeHTTP(w, r)
		if w.Code < 400 {
			t.Fatalf("%s: answered %d %s, want refused", tc.name, w.Code, w.Body)
		}
		if len(seen) == 0 {
			t.Fatalf("%s: the write read no body", tc.name)
		}
		if body := seen[0].([]byte); len(body) != len(tc.body) || !bytes.Equal(body, make([]byte, len(body))) {
			t.Errorf("%s: the body after the refusal %q, want %d bytes zeroed", tc.name, body, len(tc.body))
		}
		if tc.decoded == nil {
			continue
		}
		if len(seen) < 2 {
			t.Fatalf("%s: the write decoded no body", tc.name)
		}
		if v := tc.decoded(seen[1]); len(v) != len("s3cret") || !bytes.Equal(v, make([]byte, len(v))) {
			t.Errorf("%s: the decoded value after the refusal %q, want s3cret's 6 bytes zeroed", tc.name, v)
		}
	}
}

// withKerberos refuses 503, writing no spelling, a link the step took for
// another name than the one the spec names, such as none at all.
func TestWithKerberosRefusesALinkForAnotherName(t *testing.T) {
	cp := ntlmStore(t)
	for _, link := range []entity.ProtocolID{{}, {Scheme: entity.SchemeKerberos, Value: "bob@AD.EXAMPLE"}} {
		var ids entity.ProtocolIDs
		err := cp.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
			var err error
			ids, err = withKerberos(tx, nil, nil, "alice@AD.EXAMPLE", krbLink{id: link})
			return err
		})
		if p, _ := problemOf(err); p.Code != apiv1.CodeUnavailable || ids != nil {
			t.Errorf("a link %+v for alice@AD.EXAMPLE: spellings %v, %v; want 503 unavailable and none", link, ids, err)
		}
	}
}

// Two offline joins seal the machine password's next version at once: the
// one whose write comes second is refused 409 operation_running, as its
// request carries no If-Match whose precondition could fail, and the
// version the first wrote stays current.
func TestOfflineJoinsSealingAtOnce(t *testing.T) {
	e := newJoinEnv(t)
	e.offline(t, []byte("pw-0")) // the machine password's reference, at version 1
	var second error
	race := e
	race.d.Sealer = &sealThen{next: e.d.Sealer, then: func() {
		_, second = prepareJoin(t.Context(), e.request(apiv1.JoinDomainSpec{Mode: apiv1.JoinOffline, Account: "DFS$"}, "", []byte("pw-2")))
	}}
	_, err := prepareJoin(t.Context(), race.request(apiv1.JoinDomainSpec{Mode: apiv1.JoinOffline, Account: "DFS$"}, "", []byte("pw-1")))
	if second != nil {
		t.Fatalf("the join sealing second, written first: %v, want written", second)
	}
	if p, _ := problemOf(err); p.Status != http.StatusConflict || p.Code != apiv1.CodeOperationRunning {
		t.Errorf("the join written second: %+v (%v), want 409 operation_running", p, err)
	}
	in, err := e.cp.Installation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if cur, err := e.cp.Secret(t.Context(), in.MachinePassword, 0); err != nil || cur.Version != 2 {
		t.Errorf("the machine password: version %d (%v), want 2, the first written", cur.Version, err)
	}
}
