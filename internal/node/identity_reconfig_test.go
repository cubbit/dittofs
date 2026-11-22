package node_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/iana/nametype"
	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/jcmturner/gokrb5/v8/types"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/secrets"
	vfs "github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// installationScope is the installation's settings scope.
var installationScope = entity.SettingScope{Kind: entity.ScopeInstallation}

// sealer is a secret provider over the harness's store and both its key
// files, as the API seals with, closed when the test ends.
func sealer(t *testing.T, h *nodetest.Harness) *secrets.Provider {
	t.Helper()
	keys := map[secrets.Role]keyprovider.KeyProvider{}
	for role, file := range map[secrets.Role]string{secrets.Storage: h.StorageKeyFile(), secrets.Protocol: h.ProtocolKeyFile()} {
		kp, err := keyprovider.NewProvider(t.Context(), keyprovider.Config{Kind: keyprovider.KindLocal, File: file})
		if err != nil {
			t.Fatalf("open the %v key: %v", role, err)
		}
		t.Cleanup(func() { _ = kp.Close() })
		keys[role] = kp
	}
	p, err := secrets.New(h.Store().ControlPlane(), keys)
	if err != nil {
		t.Fatalf("secret provider: %v", err)
	}
	return p
}

// seal seals value as the next version of ref, of kind, owned by owner, under
// role, and stores it.
func seal(t *testing.T, h *nodetest.Harness, p *secrets.Provider, role secrets.Role, kind entity.SecretKind, owner entity.SecretOwner, ref entity.SecretRef, value []byte) {
	t.Helper()
	sec, err := p.Seal(t.Context(), role, kind, owner, ref, value, secrets.Meta{})
	if err != nil {
		t.Fatalf("seal a %v: %v", kind, err)
	}
	if err := h.Store().ControlPlane().Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		return tx.PutSecret(sec, sec.Version-1)
	}); err != nil {
		t.Fatalf("store a %v: %v", kind, err)
	}
}

// ntlmUser writes a user named name with a UID and the NT hash of password,
// sealed under the protocol key, and returns it.
func ntlmUser(t *testing.T, h *nodetest.Harness, p *secrets.Provider, name, password string) entity.User {
	t.Helper()
	u := entity.User{Principal: entity.NewUser(), Name: name, NTHash: entity.SecretRef(uuid.New()),
		IDs: entity.ProtocolIDs{{Scheme: entity.SchemeUID, Value: "60010"}}}
	hashes, err := identity.NewHashes(u, []byte(password), identity.NTLMSetting{On: true})
	if err != nil {
		t.Fatalf("NewHashes: %v", err)
	}
	owner := entity.SecretOwner{Record: entity.ResourceRef{Kind: entity.ResourceUser, ID: u.Principal.ID[:]}, Key: "nt_hash"}
	seal(t, h, p, secrets.Protocol, entity.SecretNTHash, owner, u.NTHash, hashes.NTHash)
	if _, err := h.Store().ControlPlane().PutUser(t.Context(), u, 0, entity.Alloc{RID: true}); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return u
}

var (
	ntlmChallenge = [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	signInClient  = netip.MustParseAddr("192.0.2.7")
)

// signInNTLM signs user in through the node's filesystem service with an
// NTLMv2 response to password, in the installation's local domain.
func signInNTLM(t *testing.T, h *nodetest.Harness, user, password string) (identity.SignInResult, error) {
	t.Helper()
	nt, _, _ := identitytest.NTLMv2Response([]byte(password), user, "", ntlmChallenge, [8]byte{0xaa})
	return h.Node().Service().SignIn(t.Context(), identity.SignInStep{
		NTLM:      &identity.NTLMAuthenticate{User: user, NTResponse: nt},
		Challenge: ntlmChallenge,
		Client:    signInClient,
	})
}

// signedIn fails t unless user's NTLM sign-in binds want.
func signedIn(t *testing.T, h *nodetest.Harness, what, user, password string, want entity.Principal) {
	t.Helper()
	res, err := signInNTLM(t, h, user, password)
	if err != nil || res.Identity.Evidence.Principal != want {
		t.Fatalf("%s: %s's NTLM sign-in bound %v, %v; want %v, nil", what, user, res.Identity.Evidence.Principal, err, want)
	}
}

// refusedSignIn fails t unless user's NTLM sign-in is refused
// ErrLogonFailure.
func refusedSignIn(t *testing.T, h *nodetest.Harness, what, user, password string) {
	t.Helper()
	if _, err := signInNTLM(t, h, user, password); !errors.Is(err, vfserr.ErrLogonFailure) {
		t.Fatalf("%s: %s's NTLM sign-in: got %v, want ErrLogonFailure", what, user, err)
	}
}

// statusUntil turns reconcile and the status writer until the node's stored
// status satisfies cond, and returns it.
func statusUntil(t *testing.T, h *nodetest.Harness, what string, cond func(entity.NodeStatus) bool) entity.NodeStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var s entity.NodeStatus
		for got, err := range h.Store().Nodes().NodeStatuses(t.Context(), entity.NodeID{}) {
			if err != nil {
				t.Fatalf("NodeStatuses: %v", err)
			}
			s = got
		}
		if s.Node == h.NodeID() && cond(s) {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("the node status within 10 s: got %+v, want %s", s, what)
		}
		turn(h)
	}
}

// refusedSetting is the refusal of installation setting key among refused,
// if any.
func refusedSetting(refused []entity.RefusedRecord, key string) (entity.RefusedRecord, bool) {
	ref := entity.SettingRef(installationScope, key)
	i := slices.IndexFunc(refused, func(r entity.RefusedRecord) bool {
		return r.Record.Kind == ref.Kind && string(r.Record.ID) == string(ref.ID)
	})
	if i < 0 {
		return entity.RefusedRecord{}, false
	}
	return refused[i], true
}

// An identity setting written while the node runs reaches identity within
// a second: bob signs in by NTLM, identity.ntlm is turned off, and once the
// clock has moved a second his next sign-in is refused.
func TestIdentitySettingsApplyWhileRunning(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch(), nodetest.InMemory())
	p := sealer(t, h)
	bob := ntlmUser(t, h, p, "bob", "bob's password")
	started(t, h)
	signedIn(t, h, "before identity.ntlm is turned off", "bob", "bob's password", bob.Principal)
	putSetting(t, h, "identity.ntlm", `false`)
	caughtUp(t, h)
	refusedSignIn(t, h, "a second after identity.ntlm was turned off", "bob", "bob's password")
}

// A first State is built from the records, never the defaults: with
// identity.ntlm off and identity.netbios_name set, a restarted node's
// identity runs the name from its start step on, before the first reconcile
// could apply it, and bob's NTLM sign-in is refused at its first attempt, the
// clock not moved.
func TestARestartedNodeStartsOnTheRecords(t *testing.T) {
	var atStart atomic.Value
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewStepping(func(n *node.Node, step string) {
		if step == "identity" {
			name, err := n.ComputerName()
			atStart.Store(fmt.Sprint(name, err))
		}
	})))
	p := sealer(t, h)
	bob := ntlmUser(t, h, p, "bob", "bob's password")
	started(t, h)
	signedIn(t, h, "with NTLM on", "bob", "bob's password", bob.Principal)
	putSetting(t, h, "identity.ntlm", `false`)
	putSetting(t, h, "identity.netbios_name", `"RECORDS"`)
	h.Restart(t)
	if err := h.Node().Start(t.Context()); err != nil {
		t.Fatalf("Start after the restart: %v", err)
	}
	if got := atStart.Load(); got != "RECORDS<nil>" {
		t.Errorf("NetBIOS name at the restarted node's identity step: got %v, want RECORDS, as written", got)
	}
	refusedSignIn(t, h, "at the restarted node's first sign-in", "bob", "bob's password")
}

// A refused identity section never holds a principal change back: with
// identity.netbios_name written invalid past the write check, a disable
// still reaches identity, the record is reported refused with its settings
// generation, and identity keeps its last NetBIOS name. Writing the name
// valid repairs it: the refusal goes and the name applies.
func TestARefusedIdentitySectionKeepsTheLastAndReportsIt(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch(), nodetest.InMemory())
	p := sealer(t, h)
	bob := ntlmUser(t, h, p, "bob", "bob's password")
	started(t, h)
	putSetting(t, h, "identity.netbios_name", `123`)
	gen := counters(t, h).Settings
	cp := h.Store().ControlPlane()
	cur, _, err := cp.User(t.Context(), bob.Principal)
	if err != nil {
		t.Fatalf("read bob: %v", err)
	}
	cur.Disabled = true
	if _, err := cp.PutUser(t.Context(), cur, cur.Generation, entity.Alloc{}); err != nil {
		t.Fatalf("disable bob: %v", err)
	}
	disabled := counters(t, h).Principals
	caughtUp(t, h)
	if got := h.Node().AppliedIdentity().Generation; got < disabled {
		t.Errorf("identity's principals generation with identity.netbios_name refused: got %d, want at least %d, bob's disable", got, disabled)
	}
	if got, err := h.Node().ComputerName(); err != nil || got != "DITTOFS" {
		t.Errorf("NetBIOS name with identity.netbios_name refused: got %q, %v, want the last applied, DITTOFS", got, err)
	}
	s := statusUntil(t, h, "identity refused", func(s entity.NodeStatus) bool {
		_, ok := refusedSetting(s.Refused, "identity")
		return ok
	})
	if r, _ := refusedSetting(s.Refused, "identity"); r.Generation != gen || !strings.Contains(string(r.Error), "identity.netbios_name") {
		t.Errorf("the refusal of identity: generation %d, error %s; want generation %d and an error naming identity.netbios_name", r.Generation, r.Error, gen)
	}
	refusedSignIn(t, h, "bob disabled", "bob", "bob's password")

	putSetting(t, h, "identity.netbios_name", `"REPAIRED"`)
	caughtUp(t, h)
	if got, err := h.Node().ComputerName(); err != nil || got != "REPAIRED" {
		t.Errorf("NetBIOS name once repaired: got %q, %v, want REPAIRED", got, err)
	}
	if _, ok := refusedSetting(h.Node().Refused(), "identity"); ok {
		t.Error("the identity section once repaired: among the refusals, want out")
	}
}

// directoryRecord writes the identity.directory record, row by row, naming
// the in-memory directory d by its account and a bind password sealed under
// the protocol key, its NetBIOS domain domain.
func directoryRecord(t *testing.T, h *nodetest.Harness, p *secrets.Provider, d *identitytest.Directory, domain string) {
	t.Helper()
	name, pw := d.Account()
	ref := entity.SecretRef(uuid.New())
	seal(t, h, p, secrets.Protocol, entity.SecretDirectoryCredential, entity.SecretOwner{Scope: installationScope, Key: "identity.directory"}, ref, pw)
	for k, v := range map[string]string{
		"address": "ldaps://dc.corp.example", "base": "DC=corp,DC=example", "bind_name": name,
		"realm": "CORP.EXAMPLE", "netbios_domain": domain,
	} {
		putSetting(t, h, "identity.directory."+k, strconv.Quote(v))
	}
	putSetting(t, h, "identity.directory.bind_credential", refJSON(ref))
}

// A directory record written while the node runs reaches identity: a
// proven caller's Find of a directory user's qualified name, never minted
// before, mints it.
func TestADirectoryRecordAppliesWhileRunning(t *testing.T) {
	d := identitytest.NewDirectory("DC=corp,DC=example")
	h := nodetest.New(t, nodetest.Watch(), nodetest.InMemory(), nodetest.WithNew(node.NewWithDialers(d.Dial, nil)))
	p := sealer(t, h)
	d.AddUser(t, "dave", nil)
	started(t, h)
	directoryRecord(t, h, p, d, "KORP")
	caughtUp(t, h)
	proven := vfs.Call{Identity: vfs.Identity{Proof: vfs.ProofSecret}}
	got, err := h.Node().Service().Find(t.Context(), proven, vfs.Spelling{Scheme: vfs.SchemeQualified, Text: `KORP\dave`})
	if err != nil || got.IsZero() {
		t.Fatalf(`Find(KORP\dave) with Mint once the directory record applied: got %v, %v; want a minted principal`, got, err)
	}
}

// A State identity refuses whole, an installation NetBIOS name equal to the
// directory's NetBIOS domain, keeps both last sections and still delivers
// the new principals generation; the record is reported refused.
func TestAStateIdentityRefusesKeepsTheLastSections(t *testing.T) {
	d := identitytest.NewDirectory("DC=corp,DC=example")
	h := nodetest.New(t, nodetest.Watch(), nodetest.InMemory(), nodetest.WithNew(node.NewWithDialers(d.Dial, nil)))
	p := sealer(t, h)
	started(t, h)
	directoryRecord(t, h, p, d, "DITTOFS")
	putUser(t, h, "carol")
	moved := counters(t, h).Principals
	caughtUp(t, h)
	st := h.Node().AppliedIdentity()
	if st.Directory != nil || st.Generation < moved {
		t.Errorf("identity's State with a directory whose NetBIOS domain is the node's name: directory %+v, generation %d; want none and at least %d", st.Directory, st.Generation, moved)
	}
	if _, ok := refusedSetting(h.Node().Refused(), "identity"); !ok {
		t.Errorf("refusals with a directory whose NetBIOS domain is the node's name: %+v, want identity among them", h.Node().Refused())
	}
}

// krbService is the service name the test realm's keytab holds a key for.
const krbService = "cifs/nas.test"

// signInKerberos signs alice in with a ticket of realm r for krbService,
// encrypted under key at kvno when key is set.
func signInKerberos(t *testing.T, h *nodetest.Harness, r *identitytest.Realm, key []byte, kvno int) (identity.SignInResult, error) {
	t.Helper()
	tk := r.Ticket(identitytest.TicketOpts{Client: "alice", Service: krbService, Key: key, KVNO: kvno, Now: h.Clock().Now()})
	return h.Node().Service().SignIn(t.Context(), identity.SignInStep{Kerberos: tk, Service: identity.ServiceCIFS, Transport: identity.TransportSMB, Client: signInClient})
}

// A keytab version written while the node runs takes effect: alice's
// ticket signs in under version 1; version 2, a key of another number, is
// sealed and, a second on, a ticket under it signs in, node status reports
// version 2 and the State identity holds lists version 2 first. The sign-in
// is counted on the node's metrics registry with no label holding alice's
// name.
func TestAKeytabVersionAppliesWhileRunning(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch(), nodetest.InMemory())
	p := sealer(t, h)
	realm := identitytest.NewRealm("TEST.REALM", krbService)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: entity.ProtocolIDs{{Scheme: entity.SchemeUID, Value: "60011"}}}
	if _, err := h.Store().ControlPlane().PutUser(t.Context(), alice, 0, entity.Alloc{RID: true}); err != nil {
		t.Fatalf("write alice: %v", err)
	}
	ref := entity.SecretRef(uuid.New())
	owner := entity.SecretOwner{Scope: installationScope, Key: "identity.keytab"}
	seal(t, h, p, secrets.Protocol, entity.SecretKeytab, owner, ref, realm.Keytab())
	putSetting(t, h, "identity.keytab", refJSON(ref))
	started(t, h)
	if res, err := signInKerberos(t, h, realm, nil, 0); err != nil || res.Identity.Evidence.Principal != alice.Principal {
		t.Fatalf("alice's ticket under version 1: bound %v, %v; want alice, nil", res.Identity.Evidence.Principal, err)
	}

	kt := keytab.New()
	if err := kt.AddEntry(krbService, realm.Name, "version 2", time.Unix(1, 0), 2, etypeID.AES256_CTS_HMAC_SHA1_96); err != nil {
		t.Fatalf("keytab version 2: %v", err)
	}
	v2, err := kt.Marshal()
	if err != nil {
		t.Fatalf("keytab version 2: %v", err)
	}
	key, _, err := kt.GetEncryptionKey(types.NewPrincipalName(nametype.KRB_NT_SRV_INST, krbService), realm.Name, 2, etypeID.AES256_CTS_HMAC_SHA1_96)
	if err != nil {
		t.Fatalf("version 2's key: %v", err)
	}
	seal(t, h, p, secrets.Protocol, entity.SecretKeytab, owner, ref, v2)
	caughtUp(t, h)
	if res, err := signInKerberos(t, h, realm, key.KeyValue, 2); err != nil || res.Identity.Evidence.Principal != alice.Principal {
		t.Fatalf("alice's ticket under version 2, a second after it was sealed: bound %v, %v; want alice, nil", res.Identity.Evidence.Principal, err)
	}
	if kt := h.Node().AppliedIdentity().Keytab; len(kt) != 2 || kt[0].Number != 2 || kt[1].Number != 1 {
		t.Errorf("identity's keytab versions: got %d of them, want versions 2 then 1", len(kt))
	}
	statusUntil(t, h, "the keytab at version 2", func(s entity.NodeStatus) bool {
		return slices.Contains(s.Secrets, entity.SecretVersion{Secret: ref, Version: 2})
	})

	got := scrapeSeries(t, h, "identity_authentications_total")
	if !strings.Contains(got, `mechanism="kerberos"`) {
		t.Errorf("the metrics listener's identity_authentications_total: %q, want a kerberos series", got)
	}
	if strings.Contains(got, "alice") || strings.Contains(got, uuid.UUID(alice.Principal.ID).String()) {
		t.Errorf("the metrics listener's identity_authentications_total: %q, want no label naming alice", got)
	}
}

// scrapeSeries is the lines of the node's metrics listener's answer that
// name series name.
func scrapeSeries(t *testing.T, h *nodetest.Harness, name string) string {
	t.Helper()
	_, addr := h.Node().Addrs()
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read GET /metrics: %v", err)
	}
	var lines []string
	for l := range strings.Lines(string(body)) {
		if strings.Contains(l, name) {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "")
}

// refJSON is ref as a setting holds a secret reference.
func refJSON(ref entity.SecretRef) string {
	b, _ := json.Marshal(ref) // an array of bytes always marshals
	return string(b)
}

// downDirectory fails every directory call with its error while down is
// set, as a directory that stopped answering.
type downDirectory struct{ down atomic.Bool }

var errDirectoryDown = errors.New("the directory is down")

func (d *downDirectory) At(string, []byte) (time.Duration, int, error) {
	if d.down.Load() {
		return 0, 0, errDirectoryDown
	}
	return 0, 0, nil
}

// A directory that stops answering raises directory_unreachable in node
// status, naming this node; once it answers a probe again, the condition is
// gone.
func TestTheDirectoryOutageReachesNodeStatus(t *testing.T) {
	d := identitytest.NewDirectory("DC=corp,DC=example")
	fault := &downDirectory{}
	f := identitytest.NewFaultyDirectory(d.Dial, fault)
	h := nodetest.New(t, nodetest.Watch(), nodetest.InMemory(), nodetest.WithNew(node.NewWithDialers(f.Dial, nil)))
	p := sealer(t, h)
	d.AddUser(t, "dave", nil)
	directoryRecord(t, h, p, d, "KORP")
	started(t, h)
	proven := vfs.Call{Identity: vfs.Identity{Proof: vfs.ProofSecret}}
	find := func() error {
		_, err := h.Node().Service().Find(t.Context(), proven, vfs.Spelling{Scheme: vfs.SchemeQualified, Text: `KORP\dave`})
		return err
	}
	unreachable := func(s entity.NodeStatus) bool {
		return slices.Contains(conditionsOf(t, s), storedCondition{Name: "directory_unreachable", Scope: "node", ID: fmt.Sprintf("%x", h.NodeID())})
	}
	fault.down.Store(true)
	if err := find(); !errors.Is(err, vfserr.ErrUnavailable) {
		t.Fatalf("Find with the directory down: got %v, want ErrUnavailable", err)
	}
	statusUntil(t, h, "directory_unreachable naming this node", unreachable)
	fault.down.Store(false)
	h.Clock().Advance(time.Minute) // past the probe's wait
	if err := find(); err != nil {
		t.Fatalf("Find once the directory answers again: %v", err)
	}
	statusUntil(t, h, "no directory_unreachable", func(s entity.NodeStatus) bool { return !unreachable(s) })
}

// A Kerberos name written through the API for a local user is linked for
// the SID the directory the node applied answers for it: the API is handed
// identity's link.
func TestAKerberosLinkThroughTheAPIAsksTheDirectory(t *testing.T) {
	d := identitytest.NewDirectory("DC=corp,DC=example")
	h := nodetest.New(t, nodetest.Watch(), nodetest.InMemory(), nodetest.WithNew(node.NewWithDialers(d.Dial, admin)))
	p := sealer(t, h)
	want := directory.WithRID(d.DomainSID(), 5000)
	d.AddUser(t, "alice", map[string][][]byte{"objectSid": {want}})
	directoryRecord(t, h, p, d, "KORP")
	started(t, h)
	u, tag := putUser(t, h, "alice")
	addr, _ := h.Node().Addrs()
	r := answeredIn(t, "PATCH alice's Kerberos name", send(t.Context(), addr, http.MethodPatch, "/api/v1/users/"+uuid.UUID(u.ID).String(),
		`{"spec":{"kerberos":"alice@CORP.EXAMPLE"}}`, "If-Match", tag))
	if r.status != http.StatusOK {
		t.Fatalf("PATCH alice's Kerberos name: answered %d %s, want 200", r.status, r.body)
	}
	got, _, err := h.Store().ControlPlane().User(t.Context(), u)
	if err != nil {
		t.Fatalf("read alice: %v", err)
	}
	i := slices.IndexFunc(got.IDs, func(id entity.ProtocolID) bool { return id.Scheme == entity.SchemeKerberos })
	if i < 0 || got.IDs[i].Value != "alice@CORP.EXAMPLE" || got.IDs[i].Linked != string(want) {
		t.Errorf("alice's spellings %+v, want alice@CORP.EXAMPLE linked for the directory entry's SID %x", got.IDs, want)
	}
}

// A keytab reference whose secret cannot be read is reported refused, and
// the identity settings written beside it still apply.
func TestAnUnreadableKeytabIsRefusedAlone(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch(), nodetest.InMemory())
	started(t, h)
	putSetting(t, h, "identity.keytab", refJSON(entity.SecretRef(uuid.New())))
	putSetting(t, h, "identity.netbios_name", `"AFTER"`)
	caughtUp(t, h)
	if got, err := h.Node().ComputerName(); err != nil || got != "AFTER" {
		t.Errorf("NetBIOS name written beside an unreadable keytab: got %q, %v, want AFTER", got, err)
	}
	if _, ok := refusedSetting(h.Node().Refused(), "identity.keytab"); !ok {
		t.Errorf("refusals with an unreadable keytab: %+v, want identity.keytab among them", h.Node().Refused())
	}
}
