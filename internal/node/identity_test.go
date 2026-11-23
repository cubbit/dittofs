package node_test

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	iidentity "github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/sim"
	vfs "github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest"
	"github.com/google/uuid"
)

// A user whose password was derived with NewHashes and sealed as the API
// stores it signs in through the identity the node hands the API, once the
// node has started: the API is handed the identity the node built, over the
// node's secret provider. The filesystem is handed the same identity: it
// finds and spells alice, and passes a sign-in through to it.
func TestIdentityServesTheAPIAndTheFilesystem(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch(), nodetest.InMemory())
	ctx := t.Context()
	const password = "correct horse battery staple"
	u := entity.User{Principal: entity.NewUser(), Name: "alice", Password: entity.SecretRef(uuid.New())}
	hashes, err := identity.NewHashes(u, []byte(password), identity.NTLMSetting{})
	if err != nil {
		t.Fatalf("NewHashes: %v", err)
	}
	kp, err := keyprovider.NewProvider(ctx, keyprovider.Config{Kind: keyprovider.KindLocal, File: h.StorageKeyFile()})
	if err != nil {
		t.Fatalf("open the storage key: %v", err)
	}
	defer func() { _ = kp.Close() }()
	cp := h.Store().ControlPlane()
	p, err := secrets.New(cp, map[secrets.Role]keyprovider.KeyProvider{secrets.Storage: kp})
	if err != nil {
		t.Fatalf("secret provider: %v", err)
	}
	owner := entity.SecretOwner{Record: entity.ResourceRef{Kind: entity.ResourceUser, ID: u.Principal.ID[:]}, Key: "password"}
	sec, err := p.Seal(ctx, secrets.Storage, entity.SecretPassword, owner, u.Password, hashes.Password, secrets.Meta{})
	if err != nil {
		t.Fatalf("seal the password hash: %v", err)
	}
	err = cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		if _, err := tx.PutUser(u, 0, entity.Alloc{UID: true, RID: true, Range: entity.DefaultIDRange}); err != nil {
			return err
		}
		return tx.PutSecret(sec, 0)
	})
	if err != nil {
		t.Fatalf("write alice: %v", err)
	}

	n := h.Node()
	if err := n.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := n.Stop(ctx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()
	got, err := n.Accounts().Lookup(ctx, "alice")
	if err != nil || got != u.Principal {
		t.Fatalf("Accounts().Lookup(alice): got %v, %v, want %v, nil", got, err, u.Principal)
	}
	if err := n.Accounts().VerifyPassword(ctx, u.Principal, []byte(password)); err != nil {
		t.Fatalf("Accounts().VerifyPassword with alice's password: got %v, want nil", err)
	}
	fs := n.Service()
	if got, err := fs.Find(ctx, vfs.Call{}, vfs.Spelling{Scheme: vfs.SchemeUserName, Text: "alice"}); err != nil || got != u.Principal {
		t.Errorf("Service().Find(alice): got %v, %v, want %v, nil", got, err, u.Principal)
	}
	if got, err := fs.Spell(ctx, u.Principal, vfs.SchemeUserName); err != nil || got.Text != "alice" {
		t.Errorf("Service().Spell(alice's principal): got %+v, %v, want alice, nil", got, err)
	}
	// identity refuses a step carrying no mechanism ErrInvalid; a filesystem
	// with no Auth would answer ErrUnavailable.
	if _, err := fs.SignIn(ctx, vfs.SignInStep{}); !errors.Is(err, vfserr.ErrInvalid) {
		t.Errorf("Service().SignIn with an empty step: got %v, want identity's ErrInvalid", err)
	}
}

// putSetting writes an installation row of key holding value, unchecked,
// as a row the store accepted from another binary, over the row stored.
func putSetting(t *testing.T, h *nodetest.Harness, key, value string) {
	t.Helper()
	putSettings(t, h, map[string]string{key: value})
}

// putSettings writes each installation setting of rows, key to value, in
// one transaction, so no reconcile pass reads some of them without the rest.
func putSettings(t *testing.T, h *nodetest.Harness, rows map[string]string) {
	t.Helper()
	scope := entity.SettingScope{Kind: entity.ScopeInstallation}
	err := h.Store().ControlPlane().Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		for key, value := range rows {
			old, _, err := tx.Setting(scope, key)
			if err != nil {
				return err
			}
			if _, err := tx.PutSetting(entity.Setting{Scope: scope, Key: key, Value: []byte(value)}, old.Generation); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("put %v: %v", rows, err)
	}
}

// An identity row the section refuses, or a first State identity refuses,
// fails the start, naming the row: no default stands in for it.
func TestARefusedIdentityRowFailsTheStart(t *testing.T) {
	prof.Watch(t, prof.Options{})
	for _, tc := range []struct {
		name, key, value, want string
		directory              bool // the rest of a directory record is written too
	}{
		{"a row that does not decode", "identity.ntlm", `123`, "identity.ntlm", false},
		{"a directory domain equal to the node's name", "identity.directory.netbios_domain", `"DITTOFS"`, "netbios_domain", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := nodetest.New(t, nodetest.InMemory())
			if tc.directory {
				for k, v := range map[string]string{"address": "ldaps://dc.corp.example", "base": "DC=corp,DC=example", "bind_name": "svc", "realm": "CORP.EXAMPLE"} {
					putSetting(t, h, "identity.directory."+k, strconv.Quote(v))
				}
			}
			putSetting(t, h, tc.key, tc.value)
			err := h.Node().Start(t.Context())
			t.Cleanup(func() { _ = h.Node().Stop(context.Background()) })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Start with %s = %s: got %v, want an error naming %s", tc.key, tc.value, err, tc.want)
			}
		})
	}
}

// A settings write that lands during the walk identity's first State is read
// from discards the walk, and identity is applied from the next one, which
// holds the write. The held scan reads the page in a transaction begun before
// the write, so the first walk misses it. The first State identity is handed
// is checked, since a later pass would apply the write anyway.
func TestAWalkTheSettingsMovedUnderIsReadAgain(t *testing.T) {
	h, f := faulted(t)
	first := make(chan string, 1)
	h.Node().OnIdentityApply(func(st iidentity.State) {
		select {
		case first <- st.Settings.NetBIOSName:
		default:
		}
	})
	held := sim.NewTestClock(time.Unix(0, 0))
	setFault(t, f, held, sim.Rule{Op: kvtest.OpScan, Key: settingsScan, Count: 1, Action: sim.FaultAction{Delay: time.Hour}})
	started := make(chan error, 1)
	go func() { started <- h.Node().Start(t.Context()) }()
	wait(t, "the settings walk to be held", func() bool { return held.Pending() == 1 })
	putSetting(t, h, "identity.netbios_name", `"OTHER"`)
	held.Advance(time.Hour)
	if err := answered(t, started); err != nil {
		t.Fatalf("Start with a setting written during the walk: %v", err)
	}
	t.Cleanup(func() {
		if err := h.Node().Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	select {
	case got := <-first:
		if got != "OTHER" {
			t.Errorf("NetBIOS name in the first State identity was handed: got %q, want OTHER, written during the first walk", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("identity was handed no State within 10 s of Start")
	}
	if got, err := h.Node().ComputerName(); err != nil || got != "OTHER" {
		t.Errorf("NetBIOS name written during the first walk: got %q, %v, want OTHER", got, err)
	}
}

// settingsScan is the settings walk's scan, as a FaultKV rule names it.
var settingsScan = keys.Describe([]byte{keys.KindCFG})

// A disable written through the API refuses the user's next call once
// identity applies it, though admission has his caller cached: bob, proven by
// a ticket ending a day past the node's clock, is served twice, the second
// call from the cache, aged on the node's clock; disabled through the API, his next call is refused,
// the clock never moved and the ticket still fresh.
func TestADisableRefusesACachedTicketCaller(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewAuthenticating(admin)))
	bob := ntlmUser(t, h, sealer(t, h), "bob", "bob's password")
	sh := addShare(t, h)
	if _, err := h.Store().ControlPlane().SetGrant(t.Context(), entity.ShareGrant{Share: sh.ID, Principal: bob.Principal, Access: entity.AccessRead}, 0); err != nil {
		t.Fatalf("grant bob the share: %v", err)
	}
	started(t, h)

	call := vfs.Call{Identity: vfs.Identity{Proof: vfs.ProofTicket, Protection: vfs.ProtectionPrivacy, Client: signInClient,
		Evidence: vfs.Evidence{Principal: bob.Principal, Expires: h.Clock().Now().Add(24 * time.Hour)}}}
	root := entity.NewHandle(sh.ID, sh.Root)
	hits := series(t, h, "dittofs_vfs_caller_cache_hits_total", "")
	for i := range 2 {
		if _, err := h.Node().Service().StatFS(t.Context(), call, root); err != nil {
			t.Fatalf("bob's ticket call %d before the disable: %v", i, err)
		}
	}
	// The cache ages a ticket on the node's clock, so one ending a day past
	// it is served from the cache.
	if got := series(t, h, "dittofs_vfs_caller_cache_hits_total", "") - hits; got < 1 {
		t.Fatalf("cache hits over bob's two ticket calls: got %v, want at least 1", got)
	}

	cur, _, err := h.Store().ControlPlane().User(t.Context(), bob.Principal)
	if err != nil {
		t.Fatalf("read bob: %v", err)
	}
	addr, _ := h.Node().Addrs()
	r := answeredIn(t, "PATCH bob disabled", send(t.Context(), addr, http.MethodPatch, "/api/v1/users/"+uuid.UUID(bob.Principal.ID).String(),
		`{"spec":{"disabled":true}}`, "If-Match", `"`+strconv.FormatUint(cur.Generation, 10)+`"`))
	if r.status != http.StatusOK {
		t.Fatalf("PATCH bob disabled: answered %d %s, want 200", r.status, r.body)
	}
	want := counters(t, h).Principals
	wait(t, "identity applying the disable's principals generation", func() bool { return h.Node().AppliedIdentity().Generation >= want })
	if _, err := h.Node().Service().StatFS(t.Context(), call, root); !errors.Is(err, vfserr.ErrAccess) {
		t.Errorf("bob's ticket call once his disable applied, his caller cached: got %v, want ErrAccess", err)
	}
}

// A disable written through the API reaches identity on the node that wrote
// it before the write answers, the clock never moved: bob, signed in by NTLM,
// is served a call; with reconcile held after it read the counters the
// disable's write does not answer; released, it answers with identity
// applied the principals generation it raised, and bob's next call through
// the filesystem is refused; the comment at that last assertion says what it
// does not prove.
func TestADisableAppliesAtOnceOnTheWritingNode(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewAuthenticating(admin)))
	rh := holdReconcile(h, 4)
	bob := ntlmUser(t, h, sealer(t, h), "bob", "bob's password")
	sh := addShare(t, h)
	if _, err := h.Store().ControlPlane().SetGrant(t.Context(), entity.ShareGrant{Share: sh.ID, Principal: bob.Principal, Access: entity.AccessRead}, 0); err != nil {
		t.Fatalf("grant bob the share: %v", err)
	}
	started(t, h)
	// Registered after started's, so it runs first: Stop finds no pass held.
	t.Cleanup(rh.release)

	res, err := signInNTLM(t, h, "bob", "bob's password")
	if err != nil || res.Identity.Evidence.Principal != bob.Principal {
		t.Fatalf("bob's NTLM sign-in: bound %v, %v; want %v, nil", res.Identity.Evidence.Principal, err, bob.Principal)
	}
	call := vfs.Call{Identity: res.Identity}
	root := entity.NewHandle(sh.ID, sh.Root)
	if _, err := h.Node().Service().StatFS(t.Context(), call, root); err != nil {
		t.Fatalf("bob's call before the disable: %v", err)
	}

	cur, _, err := h.Store().ControlPlane().User(t.Context(), bob.Principal)
	if err != nil {
		t.Fatalf("read bob: %v", err)
	}
	addr, _ := h.Node().Addrs()
	rh.hold()
	ch := send(t.Context(), addr, http.MethodPatch, "/api/v1/users/"+uuid.UUID(bob.Principal.ID).String(), `{"spec":{"disabled":true}}`,
		"If-Match", `"`+strconv.FormatUint(cur.Generation, 10)+`"`)
	select {
	case <-rh.held:
	case r := <-ch:
		t.Fatalf("PATCH bob disabled answered %d %s before a reconcile pass read the counters", r.status, r.body)
	case <-time.After(10 * time.Second):
		t.Fatal("no reconcile pass read the counters within 10 s of the disable")
	}
	select {
	case <-rh.awaited:
	case <-time.After(10 * time.Second):
		t.Fatal("the disable's wait for the node never started within 10 s")
	}
	// No event marks an answer that is never sent, so a bounded wait is the
	// assertion here.
	select {
	case r := <-ch:
		t.Fatalf("PATCH bob disabled answered %d %s with reconcile held, want no answer until it applies", r.status, r.body)
	case <-time.After(500 * time.Millisecond):
	}
	rh.release()
	if r := answeredIn(t, "PATCH bob disabled once reconcile is released", ch); r.status != http.StatusOK {
		t.Fatalf("PATCH bob disabled: answered %d %s, want 200", r.status, r.body)
	}
	if got, want := h.Node().AppliedIdentity().Generation, counters(t, h).Principals; got < want {
		t.Errorf("identity's principals generation when the disable answered: got %d, want at least %d, the one it raised", got, want)
	}
	// Resolution reads the store on every call, so this holds without the new
	// State; the generation check above guards it until the caller cache
	// keys on the principals generation.
	if _, err := h.Node().Service().StatFS(t.Context(), call, root); !errors.Is(err, vfserr.ErrAccess) {
		t.Errorf("bob's call once his disable answered, the clock never moved: got %v, want ErrAccess", err)
	}
}

// A user the API created and gave a password signs in by NTLMv2 through the
// filesystem and opens the root of a share granting her read-write: alice is
// created with a uid through POST /users, her password set by an admin's
// :set-password, and an NTLMv2 response computed from that password binds
// her principal, whose open of the share's root its grant admits.
func TestAUserTheAPICreatedSignsInByNTLMAndOpens(t *testing.T) {
	s := newAuthNode(t)
	admin := s.tokens(t, "admin", adminPassword).Access
	alice, _ := s.createUser(t, admin, `{"spec":{"name":"alice","uid":5001}}`, "k-alice")
	if r := s.setPassword(t, admin, alice, "alice's password"); r.status != http.StatusOK {
		t.Fatalf("alice's :set-password: answered %d %s, want 200", r.status, r.body)
	}
	sh := addShare(t, s.h)
	if _, err := s.h.Store().ControlPlane().SetGrant(t.Context(), entity.ShareGrant{Share: sh.ID, Principal: alice, Access: entity.AccessReadWrite}, 0); err != nil {
		t.Fatalf("grant alice the share: %v", err)
	}
	caughtUp(t, s.h)

	res, err := signInNTLM(t, s.h, "alice", "alice's password")
	if err != nil || res.Identity.Evidence.Principal != alice {
		t.Fatalf("alice's NTLM sign-in with the password :set-password set: bound %v, %v; want %v, nil", res.Identity.Evidence.Principal, err, alice)
	}
	call := vfs.Call{Identity: res.Identity, Protocol: entity.ProtocolSMB}
	info := vfs.ClientInfo{Protocol: entity.ProtocolSMB, Name: []byte("alice-pc"), Boot: [8]byte{1}, Recovery: entity.RecoverReconnect}
	if call.Client, _, err = s.h.Node().Service().Connect(t.Context(), call, info, &vfstest.Callbacks{}); err != nil {
		t.Fatalf("connect alice's client: %v", err)
	}
	// The share's root, 0755 and not hers, admits her to read it only, so the
	// open asks to read; her grant is what admits her to the share at all.
	if _, err := s.h.Node().Service().Open(t.Context(), call, vfs.OpenRequest{
		H: entity.NewHandle(sh.ID, sh.Root), Disposition: vfs.OpenExisting, Access: entity.ReadData,
	}); err != nil {
		t.Errorf("alice's open of the root of the share granting her read-write: %v, want admitted", err)
	}
}

// The API refuses a password to a directory user and stores nothing: dave,
// minted from the in-memory directory by a proven caller's Find, is refused
// :set-password 400 invalid, and the store holds the same secret rows and
// the same record of dave as before it.
func TestTheAPIRefusesADirectoryUsersPassword(t *testing.T) {
	d := identitytest.NewDirectory("DC=corp,DC=example")
	d.AddUser(t, "dave", nil)
	var raw kv.KV
	s := newAuthNode(t, nodetest.InMemory(), nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		raw = cfg.KV
		return node.NewWithDirectory(d.Dial, nil)(cfg)
	}))
	// Through the store: the composed node hands the API no settings table,
	// so the settings route refuses the directory record.
	directoryRecord(t, s.h, sealer(t, s.h), d, "KORP")
	caughtUp(t, s.h)
	proven := vfs.Call{Identity: vfs.Identity{Proof: vfs.ProofSecret}}
	dave, err := s.h.Node().Service().Find(t.Context(), proven, vfs.Spelling{Scheme: vfs.SchemeQualified, Text: `KORP\dave`})
	if err != nil || dave.IsZero() {
		t.Fatalf(`Find(KORP\dave) once the directory record applied: got %v, %v; want a minted principal`, dave, err)
	}
	before, sealed := s.stored(t, dave), secretRows(t, raw)

	admin := s.tokens(t, "admin", adminPassword).Access
	wantStatus(t, "a minted directory user's :set-password", s.setPassword(t, admin, dave, "a password"), http.StatusBadRequest, "invalid")
	if after := s.stored(t, dave); !reflect.DeepEqual(after, before) {
		t.Errorf("dave's record after the refused :set-password: %+v, want it unchanged, %+v", after, before)
	}
	if after := secretRows(t, raw); !maps.Equal(after, sealed) {
		t.Errorf("secret rows after the refused :set-password: %d, want the %d before it, unchanged", len(after), len(sealed))
	}
}

// secretRows is every secret row the KV holds, key to value.
func secretRows(t *testing.T, raw kv.KV) map[string]string {
	t.Helper()
	rows := map[string]string{}
	err := raw.View(t.Context(), func(r kv.Reader) error {
		for kvp, err := range r.Scan([]byte{keys.KindSEC}, nil) {
			if err != nil {
				return err
			}
			rows[string(kvp.Key)] = string(kvp.Value)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan the secret rows: %v", err)
	}
	return rows
}
