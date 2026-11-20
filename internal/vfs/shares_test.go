package vfs

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	identitysrv "github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/vfs/admit"
	"github.com/cubbit/dittofs/internal/vfs/authz"
	"github.com/cubbit/dittofs/internal/vfs/pseudofs"
	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs"
)

var (
	inLab   = netip.MustParseAddr("10.0.0.5")
	outside = netip.MustParseAddr("192.168.1.7")
)

// shares is a serving filesystem over the fixture's share "data" at /data, its
// control plane, and the users it is given.
type shares struct {
	*fixture
	cp    kvstore.ControlPlane
	share entity.ShareID
}

func newShares(t *testing.T, events vfs.Events, audit func(vfs.ShareID) authz.Policy) *shares {
	t.Helper()
	return startShares(t, newFixture(t, events), audit)
}

// startShares starts f's filesystem, serving it until the test ends.
func startShares(t *testing.T, f *fixture, audit func(vfs.ShareID) authz.Policy) *shares {
	t.Helper()
	f.authz.Policy = audit
	if err := f.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = f.Drain(context.Background())
		_ = f.Stop()
	})
	return &shares{fixture: f, cp: f.fs.Store.ControlPlane(), share: f.fs.Share.ID}
}

func (s *shares) must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// nextUID is the uid user gives a user it is not given one for.
var nextUID atomic.Uint32

// user makes a user holding uid, or a fresh one above 60000 when uid is
// empty, with access to share, and answers it.
func (s *shares) user(t *testing.T, name, uid string, share entity.ShareID, a entity.ShareAccess) entity.User {
	t.Helper()
	if uid == "" {
		uid = strconv.FormatUint(uint64(60000+nextUID.Add(1)), 10)
	}
	u := entity.User{Principal: entity.NewUser(), Name: name, IDs: entity.ProtocolIDs{{Scheme: entity.SchemeUID, Value: uid}, {Scheme: entity.SchemeSID, Value: "sid-" + uid}}}
	s.must(t, errOf(s.cp.PutUser(t.Context(), u, 0, entity.Alloc{})))
	s.must(t, errOf(s.cp.SetGrant(t.Context(), entity.ShareGrant{Share: share, Principal: u.Principal, Access: a}, 0)))
	return u
}

// userGeneration is the generation a write replacing or deleting user p
// expects.
func userGeneration(t *testing.T, st *kvstore.Store, p entity.Principal) uint64 {
	t.Helper()
	u, err := st.Principals().User(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	return u.Generation
}

// grantVersion, policyVersion and quotaVersion are the version a write
// replacing that record expects, zero when there is none.
func grantVersion(t *testing.T, cp kvstore.ControlPlane, share entity.ShareID, p entity.Principal) uint64 {
	t.Helper()
	g, _, err := cp.ShareGrant(t.Context(), share, p)
	if err != nil {
		t.Fatal(err)
	}
	return g.Version
}

func policyVersion(t *testing.T, cp kvstore.ControlPlane, share entity.ShareID) uint64 {
	t.Helper()
	p, err := cp.ExportPolicy(t.Context(), share)
	if err != nil {
		t.Fatal(err)
	}
	return p.Version
}

func quotaVersion(t *testing.T, cp kvstore.ControlPlane, share entity.ShareID, target kvstore.QuotaTarget) uint64 {
	t.Helper()
	for q, err := range cp.Quotas(t.Context(), share) {
		if err != nil {
			t.Fatal(err)
		}
		if q.Principal == target.Principal && q.Project == target.Project {
			return q.Version
		}
	}
	return 0
}

// addShare makes a share at path, bound in the engine, answering its ID.
func (s *shares) addShare(t *testing.T, name, path string) entity.ShareID {
	t.Helper()
	sh := entity.Share{ID: entity.NewShareID(), Name: name, Path: path, Root: entity.NewFileID()}
	s.must(t, errOf(kvstoretest.CreateShare(t.Context(), s.cp, sh)))
	s.content.Bind(sh.ID)
	return sh.ID
}

func proven(p entity.Principal, at netip.Addr) vfs.Call {
	return vfs.Call{Identity: vfs.Identity{Proof: vfs.ProofTicket, Protection: vfs.ProtectionPrivacy, Evidence: vfs.Evidence{Principal: p}, Client: at}}
}

func claimed(uid uint32, at netip.Addr) vfs.Call {
	return vfs.Call{Identity: vfs.Identity{Proof: vfs.ProofClaimed, Claims: vfs.Claims{UID: uid, GID: uid}, Client: at}}
}

// Root finds a share by path or by name, folded, answers its root and the
// caller's maximal access there, and the zero ShareRef the pseudo-root.
func TestRootFindsAShareByPathOrName(t *testing.T) {
	s := newShares(t, nil, nil)
	admin := proven(s.fs.Admin.Principal, inLab)
	for _, ref := range []vfs.ShareRef{{Path: "/data"}, {Name: "DATA"}} {
		h, info, err := s.Root(t.Context(), admin, ref)
		if err != nil || !bytes.Equal(h, s.fs.Root) || info.Name != "data" || info.MaximalAccess&entity.WriteData == 0 {
			t.Errorf("Root(%+v): %v, %+v, %v", ref, h, info, err)
		}
	}
	if _, _, err := s.Root(t.Context(), admin, vfs.ShareRef{Path: "/nope"}); !errors.Is(err, vfs.ErrNotFound) {
		t.Errorf("Root of no share: %v", err)
	}
	if _, _, err := s.Root(t.Context(), admin, vfs.ShareRef{Path: "/data", Name: "data"}); !errors.Is(err, vfs.ErrInvalid) {
		t.Errorf("Root naming a path and a name: %v", err)
	}
	h, _, err := s.Root(t.Context(), admin, vfs.ShareRef{})
	if err != nil || !pseudofs.IsHandle(h) {
		t.Fatalf("the pseudo-root: %x, %v", h, err)
	}
	if f, err := s.GetAttr(t.Context(), admin, h); err != nil || f.Type != entity.Directory || f.Number == 0 {
		t.Errorf("GetAttr of the pseudo-root: %+v, %v", f, err)
	}
}

// A renamed share keeps serving the handles its clients hold, and a new
// Root needs the new name and path.
func TestARenamedShareKeepsItsHandles(t *testing.T) {
	s := newShares(t, nil, nil)
	admin := proven(s.fs.Admin.Principal, inLab)
	h, _, err := s.Root(t.Context(), admin, vfs.ShareRef{Name: "data"})
	s.must(t, err)
	sh := s.fs.Share
	sh.Name, sh.Path = "media", "/media"
	s.must(t, errOf(s.cp.PutShare(t.Context(), sh, 1)))
	if _, err := s.StatFS(t.Context(), admin, h); err != nil {
		t.Errorf("a held handle after the rename: %v", err)
	}
	for _, ref := range []vfs.ShareRef{{Name: "data"}, {Path: "/data"}} {
		if _, _, err := s.Root(t.Context(), admin, ref); !errors.Is(err, vfs.ErrNotFound) {
			t.Errorf("Root(%+v) after the rename: %v", ref, err)
		}
	}
	for _, ref := range []vfs.ShareRef{{Name: "media"}, {Path: "/media"}} {
		if got, _, err := s.Root(t.Context(), admin, ref); err != nil || !bytes.Equal(got, h) {
			t.Errorf("Root(%+v) after the rename: %v, %v", ref, got, err)
		}
	}
}

// Every path a handle arrives by meets the same admission and the same
// grant: a held handle, Quota, Root by path, Root by name, and a walk from
// the pseudo-root.
func TestAdmissionOnEveryEntryPath(t *testing.T) {
	s := newShares(t, nil, nil)
	bob := s.user(t, "bob", "1000", s.share, entity.AccessReadWrite)
	stranger := s.user(t, "stranger", "", s.share, entity.AccessNone)
	s.must(t, errOf(s.cp.PutNetgroup(t.Context(), entity.Netgroup{Name: "lab", Members: []string{"10.0.0.0/24"}}, 0)))
	ctx := t.Context()
	paths := map[string]func(id vfs.Call) error{
		"held handle": func(id vfs.Call) error { _, err := s.StatFS(ctx, id, s.fs.Root); return err },
		"Quota": func(id vfs.Call) error {
			_, _, err := s.Quota(ctx, id, s.fs.Root, bob.Principal)
			return err
		},
		"Root by path": func(id vfs.Call) error {
			_, _, err := s.Root(ctx, id, vfs.ShareRef{Path: "/data"})
			return err
		},
		"Root by name": func(id vfs.Call) error {
			_, _, err := s.Root(ctx, id, vfs.ShareRef{Name: "data"})
			return err
		},
		"walk": func(id vfs.Call) error {
			root, _, err := s.Root(ctx, id, vfs.ShareRef{})
			if err != nil {
				return err
			}
			_, _, err = s.Lookup(ctx, id, root, []byte("data"))
			return err
		},
	}
	for _, c := range []struct {
		name   string
		policy entity.ExportPolicy
		state  entity.ShareState
		id     vfs.Call
		want   error
	}{
		{"admitted", entity.ExportPolicy{}, entity.ShareEnabled, claimed(1000, inLab), nil},
		{"no grant", entity.ExportPolicy{}, entity.ShareEnabled, proven(stranger.Principal, inLab), vfs.ErrAccess},
		{"quiesced", entity.ExportPolicy{}, entity.ShareQuiesced, claimed(1000, inLab), vfs.ErrDelay},
		{"proof", entity.ExportPolicy{MinProof: entity.ProofSecret}, entity.ShareEnabled, claimed(1000, inLab), vfs.ErrWrongSecurity},
		{"kerberos only, NTLM", entity.ExportPolicy{MinProof: entity.ProofTicket}, entity.ShareEnabled,
			vfs.Call{Identity: vfs.Identity{Proof: vfs.ProofSecret, Protection: vfs.ProtectionPrivacy, Evidence: vfs.Evidence{Principal: bob.Principal}, Client: inLab}}, vfs.ErrWrongSecurity},
		{"krb5p", entity.ExportPolicy{MinProof: entity.ProofTicket, MinProtection: entity.ProtectionPrivacy}, entity.ShareEnabled,
			vfs.Call{Identity: vfs.Identity{Proof: vfs.ProofTicket, Protection: vfs.ProtectionIntegrity, Evidence: vfs.Evidence{Principal: bob.Principal}, Client: inLab}}, vfs.ErrWrongSecurity},
		{"client rules", entity.ExportPolicy{Clients: []entity.ClientRule{{Match: entity.ClientMatch{Netgroup: "lab"}, Access: entity.AccessReadWrite}}},
			entity.ShareEnabled, claimed(1000, outside), vfs.ErrAccess},
	} {
		c.policy.Share = s.share
		s.must(t, errOf(s.cp.PutExportPolicy(ctx, c.policy, policyVersion(t, s.cp, c.policy.Share))))
		s.must(t, s.cp.SetShareState(ctx, s.share, entity.ShareEnabled, c.state))
		for name, call := range paths {
			if err := call(c.id); c.want == nil && err != nil || c.want != nil && !errors.Is(err, c.want) {
				t.Errorf("%s, %s: %v, want %v", c.name, name, err, c.want)
			}
		}
		s.must(t, s.cp.SetShareState(ctx, s.share, c.state, entity.ShareEnabled))
	}
}

// A caller squashed on one share and not on another acts as root on the
// second only, each share resolving it afresh.
func TestSquashIsEachSharesOwn(t *testing.T) {
	s := newShares(t, nil, nil)
	other := s.addShare(t, "other", "/other")
	s.must(t, errOf(s.cp.SetGrant(t.Context(), entity.ShareGrant{Share: other, Principal: entity.Everyone, Access: entity.AccessReadWrite}, 0)))
	nobody := s.user(t, "nobody", "", s.share, entity.AccessReadWrite)
	s.must(t, errOf(s.cp.PutExportPolicy(t.Context(), entity.ExportPolicy{Share: s.share, Squash: entity.SquashRoot, Anonymous: nobody.Principal}, 0)))
	s.must(t, errOf(s.cp.PutExportPolicy(t.Context(), entity.ExportPolicy{Share: other, Squash: entity.SquashNone}, 0)))
	asRoot := claimed(0, inLab)
	_, data, err := s.Root(t.Context(), asRoot, vfs.ShareRef{Path: "/data"})
	s.must(t, err)
	_, oth, err := s.Root(t.Context(), asRoot, vfs.ShareRef{Path: "/other"})
	s.must(t, err)
	if data.MaximalAccess&entity.WriteData != 0 {
		t.Errorf("root squashed on /data may write its 0755 root: %#x", data.MaximalAccess)
	}
	if oth.MaximalAccess&entity.WriteData == 0 {
		t.Errorf("root unsquashed on /other may not write its root: %#x", oth.MaximalAccess)
	}
}

// Admission resolves afresh on every call: a client removed from the share
// grant, or a user disabled, is refused on its next call with the handle it
// already holds.
func TestEveryCallResolvesAfresh(t *testing.T) {
	s := newShares(t, nil, nil)
	for _, c := range []struct {
		name   string
		revoke func(u entity.User)
	}{
		{"grant removed", func(u entity.User) {
			s.must(t, errOf(s.cp.SetGrant(t.Context(), entity.ShareGrant{Share: s.share, Principal: u.Principal, Access: entity.AccessNone}, grantVersion(t, s.cp, s.share, u.Principal))))
		}},
		{"disabled", func(u entity.User) {
			u.Disabled = true
			s.must(t, errOf(s.cp.PutUser(t.Context(), u, userGeneration(t, s.fs.Store, u.Principal), entity.Alloc{})))
		}},
	} {
		u := s.user(t, c.name, "", s.share, entity.AccessReadWrite)
		id := proven(u.Principal, inLab)
		h, _, err := s.Root(t.Context(), id, vfs.ShareRef{Path: "/data"})
		s.must(t, err)
		if _, err := s.StatFS(t.Context(), id, h); err != nil {
			t.Fatalf("%s: before: %v", c.name, err)
		}
		c.revoke(u)
		if _, err := s.StatFS(t.Context(), id, h); !errors.Is(err, vfs.ErrAccess) {
			t.Errorf("%s: the next call with the held handle answered %v", c.name, err)
		}
	}
}

// Admission runs before the handle is resolved: a client outside the
// share's netgroup is refused for a live handle and a dead one alike.
func TestAdmissionRunsBeforeTheHandleIsResolved(t *testing.T) {
	s := newShares(t, nil, nil)
	ctx := t.Context()
	s.must(t, errOf(s.cp.PutNetgroup(ctx, entity.Netgroup{Name: "lab", Members: []string{"10.0.0.5"}}, 0)))
	s.must(t, errOf(s.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: s.share,
		Clients: []entity.ClientRule{{Match: entity.ClientMatch{Netgroup: "lab"}, Access: entity.AccessReadWrite}}}, 0)))
	// An unlinked file is kept until its release, so the removed file's
	// handle is one whose file the store no longer holds.
	gone := entity.NewHandle(s.share, entity.NewFileID())
	for _, h := range []vfs.Handle{s.fs.Root, gone} {
		if _, err := s.StatFS(ctx, proven(s.fs.Admin.Principal, outside), h); !errors.Is(err, vfs.ErrAccess) {
			t.Errorf("outside the netgroup, StatFS(%v): %v", h, err)
		}
	}
	if _, err := s.StatFS(ctx, proven(s.fs.Admin.Principal, inLab), gone); !errors.Is(err, vfs.ErrStale) {
		t.Errorf("inside the netgroup, the removed file's handle: %v", err)
	}
}

// After a share-list change, concurrent calls rebuild the pseudo-filesystem
// once, not once each.
func TestOneCallRebuildsThePseudoFilesystem(t *testing.T) {
	s := newShares(t, nil, nil)
	admin := proven(s.fs.Admin.Principal, inLab)
	_, _, err := s.Root(t.Context(), admin, vfs.ShareRef{})
	s.must(t, err)
	s.addShare(t, "other", "/other")
	var builds atomic.Int32
	s.beforeBuild = func() {
		builds.Add(1)
		time.Sleep(20 * time.Millisecond)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, _, err := s.Root(t.Context(), admin, vfs.ShareRef{}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := builds.Load(); n != 1 {
		t.Errorf("8 calls after a share was added built the pseudo-filesystem %d times", n)
	}
}

// A call waiting on another's rebuild of the pseudo-filesystem leaves when
// its context ends, though the rebuild is stuck.
func TestAWaitOnAStuckRebuildEndsWithTheContext(t *testing.T) {
	s := newShares(t, nil, nil)
	admin := proven(s.fs.Admin.Principal, inLab)
	_, _, err := s.Root(t.Context(), admin, vfs.ShareRef{})
	s.must(t, err)
	s.addShare(t, "other", "/other")
	entered, stuck := make(chan struct{}), make(chan struct{})
	s.beforeBuild = sync.OnceFunc(func() {
		close(entered)
		<-stuck
	})
	done := make(chan error, 1)
	go func() {
		_, _, err := s.Root(t.Context(), admin, vfs.ShareRef{})
		done <- err
	}()
	<-entered
	unstick := sync.OnceFunc(func() { close(stuck) })
	time.AfterFunc(2*time.Second, unstick) // so that a wait the context cannot end still ends
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := s.StatFS(ctx, admin, s.fs.Root); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Errorf("StatFS behind a stuck rebuild: %v after %v", err, time.Since(start))
	}
	unstick()
	s.must(t, <-done)
}

// A pseudo directory lists only what leads to a share the caller would be
// admitted to; a lookup reaches a refusing share's mount, and is refused
// there.
func TestThePseudoFilesystemListsWhatTheCallerReaches(t *testing.T) {
	s := newShares(t, nil, nil)
	ctx := t.Context()
	secret := s.addShare(t, "secret", "/vault/secret")
	s.must(t, errOf(s.cp.SetGrant(ctx, entity.ShareGrant{Share: secret, Principal: s.fs.Admin.Principal, Access: entity.AccessAdmin}, 0)))
	s.must(t, errOf(s.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: secret,
		Clients: []entity.ClientRule{{Match: entity.ClientMatch{CIDR: netip.MustParsePrefix("10.0.0.0/8")}, Access: entity.AccessReadWrite}}}, 0)))
	names := func(at netip.Addr) []string {
		root, _, err := s.Root(ctx, proven(s.fs.Admin.Principal, at), vfs.ShareRef{})
		s.must(t, err)
		p, err := s.ReadDir(ctx, proven(s.fs.Admin.Principal, at), root, 0, 0, vfs.EntryHandle|vfs.EntryFile)
		s.must(t, err)
		var out []string
		for _, e := range p.Entries {
			out = append(out, string(e.Name))
		}
		slices.Sort(out)
		return out
	}
	if got := names(inLab); !slices.Equal(got, []string{"data", "vault"}) {
		t.Errorf("inside the rule: %v", got)
	}
	if got := names(outside); !slices.Equal(got, []string{"data"}) {
		t.Errorf("outside the rule: %v", got)
	}
	out := proven(s.fs.Admin.Principal, outside)
	root, _, _ := s.Root(ctx, out, vfs.ShareRef{})
	vault, _, err := s.Lookup(ctx, out, root, []byte("vault"))
	s.must(t, err)
	if _, _, err := s.Lookup(ctx, out, vault, []byte("secret")); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("a lookup into the refusing share: %v", err)
	}
	p, err := s.ReadDir(ctx, proven(s.fs.Admin.Principal, inLab), root, 0, 1, vfs.EntryHandle)
	s.must(t, err)
	q, err := s.ReadDir(ctx, proven(s.fs.Admin.Principal, inLab), root, p.Next, 1, vfs.EntryHandle)
	s.must(t, err)
	if len(p.Entries) != 1 || p.EOF || len(q.Entries) != 1 || !q.EOF || bytes.Equal(p.Entries[0].Name, q.Entries[0].Name) {
		t.Errorf("two pages of one: %+v then %+v", p, q)
	}
}

// SecInfo answers the share's flavours for a caller its minimums refuse,
// and in a pseudo directory every flavour of the shares below.
func TestSecInfoAnswersTheRemedy(t *testing.T) {
	s := newShares(t, nil, nil)
	ctx := t.Context()
	s.must(t, errOf(s.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: s.share,
		Flavors: []entity.AuthFlavor{entity.FlavorKerberos}, MinProof: entity.ProofTicket, MinProtection: entity.ProtectionIntegrity}, 0)))
	other := s.addShare(t, "other", "/other")
	s.must(t, errOf(s.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: other, Flavors: []entity.AuthFlavor{entity.FlavorSys}}, 0)))
	weak := claimed(1000, inLab)
	got, err := s.SecInfo(ctx, weak, s.fs.Root, []byte("x"))
	if want := []vfs.AuthFlavor{vfs.FlavorKrb5p, vfs.FlavorKrb5i}; err != nil || !slices.Equal(got, want) {
		t.Errorf("SecInfo in the share: %v, %v, want %v", got, err, want)
	}
	root, _, _ := s.Root(ctx, weak, vfs.ShareRef{})
	if got, err := s.SecInfo(ctx, weak, root, []byte("data")); err != nil || !slices.Equal(got, []vfs.AuthFlavor{vfs.FlavorKrb5p, vfs.FlavorKrb5i}) {
		t.Errorf("SecInfo of the mount: %v, %v", got, err)
	}
	if got, err := s.SecInfo(ctx, weak, root, []byte("..")); err != nil || len(got) != 3 {
		t.Errorf("SecInfo of the pseudo-root, above both: %v, %v", got, err)
	}
	s.must(t, s.cp.SetShareState(ctx, s.share, entity.ShareEnabled, entity.ShareQuiesced))
	if _, err := s.SecInfo(ctx, weak, s.fs.Root, []byte("x")); !errors.Is(err, vfs.ErrDelay) {
		t.Errorf("SecInfo in a quiesced share: %v", err)
	}
}

// ListShares lists every share the caller would be admitted to, a quiesced
// one included, and no hidden one; a quiesced share its client rules refuse
// is not listed, by ListShares or by the pseudo-filesystem.
func TestListSharesListsWhatTheCallerReaches(t *testing.T) {
	s := newShares(t, nil, nil)
	ctx := t.Context()
	s.addShare(t, "admin$", "/admin")
	quiet := s.addShare(t, "quiet", "/quiet")
	s.must(t, s.cp.SetShareState(ctx, quiet, entity.ShareEnabled, entity.ShareQuiesced))
	hidden := s.addShare(t, "hidden", "/hidden")
	s.must(t, errOf(s.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: hidden, SMB: entity.SMBHidden}, 0)))
	closed := s.addShare(t, "closed", "/closed")
	s.must(t, errOf(s.cp.SetGrant(ctx, entity.ShareGrant{Share: closed, Principal: s.fs.Admin.Principal, Access: entity.AccessAdmin}, 0)))
	s.must(t, errOf(s.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: closed,
		Clients: []entity.ClientRule{{Match: entity.ClientMatch{CIDR: netip.MustParsePrefix("10.0.0.0/8")}, Access: entity.AccessNone}}}, 0)))
	admin := proven(s.fs.Admin.Principal, inLab)
	for _, paused := range []bool{false, true} {
		if paused {
			s.must(t, s.cp.SetShareState(ctx, closed, entity.ShareEnabled, entity.ShareQuiesced))
		}
		list, err := s.ListShares(ctx, admin)
		s.must(t, err)
		var got []string
		for _, i := range list {
			got = append(got, i.Name)
		}
		slices.Sort(got)
		if !slices.Equal(got, []string{"data", "quiet"}) {
			t.Errorf("ListShares, the refusing share paused %v: %v", paused, got)
		}
		root, _, err := s.Root(ctx, admin, vfs.ShareRef{})
		s.must(t, err)
		p, err := s.ReadDir(ctx, admin, root, 0, 0, vfs.EntryHandle)
		s.must(t, err)
		for _, e := range p.Entries {
			if string(e.Name) == "closed" {
				t.Errorf("the pseudo-root lists the refusing share, paused %v, with handle %x", paused, e.Handle)
			}
		}
	}
}

// StatFS and Quota report the share's usage and quotas.
func TestStatFSAndQuotaReportTheShares(t *testing.T) {
	s := newShares(t, nil, nil)
	ctx := t.Context()
	bob := s.user(t, "bob", "", s.share, entity.AccessReadWrite)
	s.must(t, errOf(s.cp.SetQuota(ctx, s.share, entity.Quota{Hard: entity.Limit{Bytes: 1 << 30}}, 0)))
	s.must(t, errOf(s.cp.SetQuota(ctx, s.share, entity.Quota{Principal: bob.Principal, Hard: entity.Limit{Files: 7}}, 0)))
	admin := proven(s.fs.Admin.Principal, inLab)
	s.fs.Create(t, s.fs.Admin, s.fs.Root, "f", kvstore.Regular(0o644))
	info, err := s.StatFS(ctx, admin, s.fs.Root)
	if err != nil || info.Quota.Hard.Bytes != 1<<30 || info.Share != s.share || !info.Capabilities.CaseSensitive {
		t.Errorf("StatFS: %+v, %v", info, err)
	}
	if _, q, err := s.Quota(ctx, admin, s.fs.Root, bob.Principal); err != nil || q.Hard.Files != 7 {
		t.Errorf("bob's quota: %+v, %v", q, err)
	}
	if _, q, err := s.Quota(ctx, admin, s.fs.Root, s.fs.Admin.Principal); err != nil || !q.Unlimited() {
		t.Errorf("a principal with no quota: %+v, %v", q, err)
	}
	if _, q, err := s.Quota(ctx, admin, s.fs.Root, vfs.Principal{}); !errors.Is(err, vfs.ErrInvalid) {
		t.Errorf("no principal's quota: %+v, %v", q, err)
	}
}

// Snapshots and MoveTarget answer none, after admission; without Deps.Auth
// and Deps.Names the Principals area refuses every call ErrUnavailable.
func TestTheStubsAnswerTheirPlaceholders(t *testing.T) {
	s := newShares(t, nil, nil)
	ctx := t.Context()
	if got, err := s.Snapshots(ctx, proven(s.fs.Admin.Principal, inLab), s.fs.Root); err != nil || len(got) != 0 {
		t.Errorf("Snapshots: %v, %v", got, err)
	}
	if to, err := s.MoveTarget(ctx, proven(s.fs.Admin.Principal, inLab), s.fs.Root); err != nil || to.IsValid() {
		t.Errorf("MoveTarget: %v, %v", to, err)
	}
	s.must(t, s.cp.SetShareState(ctx, s.share, entity.ShareEnabled, entity.ShareQuiesced))
	if _, err := s.Snapshots(ctx, proven(s.fs.Admin.Principal, inLab), s.fs.Root); !errors.Is(err, vfs.ErrDelay) {
		t.Errorf("Snapshots in a quiesced share: %v", err)
	}
	if _, err := s.MoveTarget(ctx, proven(s.fs.Admin.Principal, inLab), s.fs.Root); !errors.Is(err, vfs.ErrDelay) {
		t.Errorf("MoveTarget in a quiesced share: %v", err)
	}
	_, e1 := s.SignIn(ctx, vfs.SignInStep{})
	_, e2 := s.Spell(ctx, s.fs.Admin.Principal, vfs.SchemeUID)
	_, e3 := s.Find(ctx, proven(s.fs.Admin.Principal, inLab), vfs.Spelling{})
	for i, err := range []error{e1, e2, e3} {
		if !errors.Is(err, vfs.ErrUnavailable) {
			t.Errorf("Principals call %d (SignIn, Spell, Find) answered %v, want ErrUnavailable", i, err)
		}
	}
}

// findOpts passes every call through to the identity server and records the
// FindOpts each Find reached it with.
type findOpts struct {
	identity.Identities
	mu  sync.Mutex
	got []identity.FindOpts
}

func (n *findOpts) Find(ctx context.Context, sp identity.Spelling, o identity.FindOpts) (entity.Principal, error) {
	n.mu.Lock()
	n.got = append(n.got, o)
	n.mu.Unlock()
	return n.Identities.Find(ctx, sp, o)
}

// Spell and Find reach Deps.Names and answer what it answers; Find lets the
// directory mint only for a call proved by a secret or a ticket.
func TestSpellAndFindPassThroughToNames(t *testing.T) {
	store := kvstoretest.NewFS(t, memKV)
	srv, err := identitysrv.New(identitysrv.Deps{Store: store.Store, Clock: sim.NewTestClock(time.Unix(0, 0))})
	if err != nil {
		t.Fatal(err)
	}
	names := &findOpts{Identities: srv}
	s := startShares(t, newFixtureOver(t, Deps{Names: names}, store), nil)
	ctx := t.Context()

	// Each answer is the identity server's own for the same call.
	wantSp, wantErr := srv.Spell(ctx, s.fs.Admin.Principal, vfs.SchemeUserName)
	if sp, got := s.Spell(ctx, s.fs.Admin.Principal, vfs.SchemeUserName); sp.Text != wantSp.Text || !errors.Is(got, wantErr) || errors.Is(got, errNoIdentity) {
		t.Errorf("Spell answered %+v, %v; want the identity server's %+v, %v", sp, got, wantSp, wantErr)
	}
	want, err := srv.Find(ctx, vfs.Spelling{Scheme: vfs.SchemeUID, Num: 100}, identity.FindOpts{})
	for _, tc := range []struct {
		proof vfs.ProofKind
		mint  bool
	}{
		{vfs.ProofNone, false},
		{vfs.ProofClaimed, false},
		{vfs.ProofSecret, true},
		{vfs.ProofTicket, true},
	} {
		names.got = nil
		c := vfs.Call{Identity: vfs.Identity{Proof: tc.proof, Evidence: vfs.Evidence{Principal: s.fs.Admin.Principal}, Client: inLab}}
		if p, got := s.Find(ctx, c, vfs.Spelling{Scheme: vfs.SchemeUID, Num: 100}); p != want || !errors.Is(got, err) || errors.Is(got, errNoIdentity) {
			t.Errorf("Find at proof %d answered %v, %v; want the identity server's %v, %v", tc.proof, p, got, want, err)
		}
		if len(names.got) != 1 || names.got[0].Mint != tc.mint {
			t.Errorf("Find at proof %d reached identity with %+v, want one call with Mint %v", tc.proof, names.got, tc.mint)
		}
	}
}

// SignIn reaches Deps.Auth and answers what it answers: a step naming no
// user is the identity server's own ErrInvalid, not errNoIdentity.
func TestSignInPassesThroughToAuth(t *testing.T) {
	store := kvstoretest.NewFS(t, memKV)
	srv, err := identitysrv.New(identitysrv.Deps{Store: store.Store, Clock: sim.NewTestClock(time.Unix(0, 0))})
	if err != nil {
		t.Fatal(err)
	}
	s := startShares(t, newFixtureOver(t, Deps{Auth: srv}, store), nil)
	ctx := t.Context()
	step := vfs.SignInStep{NTLM: &identity.NTLMAuthenticate{}, Client: inLab}
	_, want := srv.SignIn(ctx, step)
	if _, got := s.SignIn(ctx, step); got == nil || want == nil || got.Error() != want.Error() || !errors.Is(got, vfs.ErrInvalid) {
		t.Errorf("SignIn of a step naming no user = %v, want the identity server's %v", got, want)
	}
}

// recorder is an event sink recording the access events it receives.
type recorder struct {
	mu  sync.Mutex
	got []vfs.AccessEvent
}

func (r *recorder) Quota(context.Context, vfs.QuotaEvent) {}
func (r *recorder) Access(_ context.Context, e vfs.AccessEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, e)
}

// A decision the share's audit policy selects reaches the node's sink before
// the call that made it answers, naming the client and protocol the call came
// on: the filesystem keeps no queue in front of the sink. With no sink
// supplied, the null sink takes it.
func TestAnAuditedDecisionReachesTheSink(t *testing.T) {
	rec := &recorder{}
	audit := func(vfs.ShareID) authz.Policy { return authz.AuditDenials }
	s := newShares(t, rec, audit)
	stranger := s.user(t, "stranger", "", s.share, entity.AccessNone)
	call := proven(stranger.Principal, inLab)
	call.Client, call.Protocol = 7, entity.ProtocolSMB
	if _, err := s.StatFS(t.Context(), call, s.fs.Root); !errors.Is(err, vfs.ErrAccess) {
		t.Fatalf("a caller with no grant: %v", err)
	}
	rec.mu.Lock()
	got := rec.got
	rec.mu.Unlock()
	if len(got) != 1 || got[0].Op != vfs.OpStatFS || got[0].Allowed || got[0].Addr != inLab ||
		got[0].Client != 7 || got[0].Protocol != entity.ProtocolSMB {
		t.Errorf("the sink received %+v by the time StatFS answered", got)
	}

	n := newShares(t, nil, audit)
	stranger = n.user(t, "stranger", "", n.share, entity.AccessNone)
	if _, err := n.StatFS(t.Context(), proven(stranger.Principal, inLab), n.fs.Root); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("an audited refusal with no sink supplied: %v", err)
	}
}

// A claimed uid gains nothing from its user's admin grant, and a squashed
// proven caller nothing from its own: only an unsquashed proven caller is
// admin, and privileged with it.
func TestOnlyAnUnsquashedProvenCallerIsAdmin(t *testing.T) {
	s := newShares(t, nil, nil)
	ctx := t.Context()
	ops := s.user(t, "ops", "1500", s.share, entity.AccessAdmin)
	caller := func(id vfs.Call) authz.Caller {
		t.Helper()
		a, release, err := s.admit(ctx, id, s.share, true, false)
		s.must(t, err)
		release()
		return a.caller
	}
	if c := caller(claimed(1500, inLab)); c.Admin || c.Privileged {
		t.Errorf("a claimed uid with an admin grant: admin %v, privileged %v", c.Admin, c.Privileged)
	}
	if c := caller(proven(ops.Principal, inLab)); !c.Admin || !c.Privileged || !c.Authenticated {
		t.Errorf("a proven admin: %+v", c.Identity)
	}
	rw := s.user(t, "rw", "1600", s.share, entity.AccessReadWrite)
	if c := caller(proven(rw.Principal, inLab)); c.Admin || c.Privileged || !c.Authenticated {
		t.Errorf("a proven read-write caller: %+v", c.Identity)
	}
	nobody := s.user(t, "nobody", "", s.share, entity.AccessReadWrite)
	s.must(t, errOf(s.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: s.share, Squash: entity.SquashAll, Anonymous: nobody.Principal}, 0)))
	c := caller(proven(ops.Principal, inLab))
	if c.Admin || c.Privileged || c.Authenticated || c.Principal != nobody.Principal || c.Mapped != ops.Principal {
		t.Errorf("a squashed proven admin: %+v, mapped %v", c.Identity, c.Mapped)
	}
}

// A proven caller's share grant is read for whom it signed in as, before
// the squash: on an all-squash share its own grant of none refuses it,
// whatever the anonymous principal's, and its own read-write admits it
// where the anonymous principal's is none. A grant to everyone admits a
// caller of either proof that no other grant names, and neither one whose
// grant is none.
func TestAProvenCallersGrantIsReadBeforeTheSquash(t *testing.T) {
	s := newShares(t, nil, nil)
	ctx := t.Context()
	nobody := s.user(t, "nobody", "", s.share, entity.AccessReadWrite)
	eve := s.user(t, "eve", "", s.share, entity.AccessNone)
	bob := s.user(t, "bob", "", s.share, entity.AccessReadWrite)
	s.must(t, errOf(s.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: s.share, Squash: entity.SquashAll, Anonymous: nobody.Principal}, 0)))
	if _, _, err := s.Root(ctx, proven(eve.Principal, inLab), vfs.ShareRef{Path: "/data"}); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("a proven caller granted none, through anonymous's read-write: %v", err)
	}
	s.must(t, errOf(s.cp.SetGrant(ctx, entity.ShareGrant{Share: s.share, Principal: nobody.Principal, Access: entity.AccessNone}, grantVersion(t, s.cp, s.share, nobody.Principal))))
	if _, err := s.StatFS(ctx, proven(bob.Principal, inLab), s.fs.Root); err != nil {
		t.Errorf("a proven caller granted read-write, anonymous granted none: %v", err)
	}
	s.must(t, errOf(s.cp.SetGrant(ctx, entity.ShareGrant{Share: s.share, Principal: entity.Everyone, Access: entity.AccessRead}, 0)))
	calls := map[string]vfs.Call{"proven": proven(eve.Principal, inLab), "claimed": claimed(4242, inLab)}
	for name, c := range calls {
		if _, err := s.StatFS(ctx, c, s.fs.Root); !errors.Is(err, vfs.ErrAccess) {
			t.Errorf("a %s caller granted none, everyone granted read: %v", name, err)
		}
	}
	s.must(t, s.cp.DeleteGrant(ctx, s.share, eve.Principal, grantVersion(t, s.cp, s.share, eve.Principal)))
	s.must(t, s.cp.DeleteGrant(ctx, s.share, nobody.Principal, grantVersion(t, s.cp, s.share, nobody.Principal)))
	for name, c := range calls {
		if _, err := s.StatFS(ctx, c, s.fs.Root); err != nil {
			t.Errorf("a %s caller with no grant, everyone granted read: %v", name, err)
		}
	}
}

// On a squashed share, a proven caller with no grant of its own is held to
// the widest of its groups' grants, read before the squash, though the
// anonymous principal it then acts as is granted none.
func TestAProvenCallersGroupGrantIsReadBeforeTheSquash(t *testing.T) {
	s := newShares(t, nil, nil)
	ctx := t.Context()
	nobody := s.user(t, "nobody", "", s.share, entity.AccessNone)
	dan := s.user(t, "dan", "", s.share, entity.AccessNone)
	s.must(t, s.cp.DeleteGrant(ctx, s.share, dan.Principal, grantVersion(t, s.cp, s.share, dan.Principal)))
	staff := entity.Group{Principal: entity.NewGroup(), Name: "staff"}
	// A group holds a gid and a SID, so the store allocates both.
	s.must(t, errOf(s.cp.PutGroup(ctx, staff, 0, entity.Alloc{GID: true, RID: true, Range: entity.IDRange{Lo: 1_000_000, Hi: 1_999_999_999}})))
	s.must(t, s.cp.AddMember(ctx, entity.Membership{Group: staff.Principal, Member: dan.Principal}))
	s.must(t, errOf(s.cp.SetGrant(ctx, entity.ShareGrant{Share: s.share, Principal: staff.Principal, Access: entity.AccessReadWrite}, 0)))
	s.must(t, errOf(s.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: s.share, Squash: entity.SquashAll, Anonymous: nobody.Principal}, 0)))
	if _, err := s.StatFS(ctx, proven(dan.Principal, inLab), s.fs.Root); err != nil {
		t.Errorf("a proven caller through its group's read-write, anonymous granted none: %v", err)
	}
}

// A claimed caller is not authenticated: a share whose only grant names
// authenticated users refuses it, and admits a proven one.
func TestAClaimedCallerIsNotAuthenticated(t *testing.T) {
	s := newShares(t, nil, nil)
	ctx := t.Context()
	carol := s.user(t, "carol", "4242", s.share, entity.AccessNone)
	s.must(t, s.cp.DeleteGrant(ctx, s.share, carol.Principal, grantVersion(t, s.cp, s.share, carol.Principal)))
	s.must(t, errOf(s.cp.SetGrant(ctx, entity.ShareGrant{Share: s.share, Principal: entity.AuthenticatedUsers, Access: entity.AccessRead}, 0)))
	if _, err := s.StatFS(ctx, claimed(4242, inLab), s.fs.Root); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("a claimed caller took authenticated users' grant: %v", err)
	}
	if _, err := s.StatFS(ctx, proven(carol.Principal, inLab), s.fs.Root); err != nil {
		t.Errorf("a proven caller under authenticated users' grant: %v", err)
	}
}

// A proven principal holding no uid, such as a machine principal, acts as
// the share's anonymous principal with every flag false, whatever grants
// would reach it as itself, and keeps its own principal as the one it
// mapped to.
func TestAProvenPrincipalWithNoUIDIsAnonymous(t *testing.T) {
	s := newShares(t, nil, nil)
	ctx := t.Context()
	nobody := s.user(t, "nobody", "", s.share, entity.AccessRead)
	s.must(t, errOf(s.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: s.share, Squash: entity.SquashNone, Anonymous: nobody.Principal}, 0)))
	host := entity.User{Principal: entity.NewUser(), Name: "host/build01.corp.example"}
	s.must(t, errOf(s.cp.PutUser(ctx, host, 0, entity.Alloc{})))
	s.must(t, errOf(s.cp.SetGrant(ctx, entity.ShareGrant{Share: s.share, Principal: host.Principal, Access: entity.AccessAdmin}, 0)))
	s.must(t, errOf(s.cp.SetGrant(ctx, entity.ShareGrant{Share: s.share, Principal: entity.AuthenticatedUsers, Access: entity.AccessReadWrite}, 0)))
	a, release, err := s.admit(ctx, proven(host.Principal, inLab), s.share, true, false)
	s.must(t, err)
	release()
	if c := a.caller; c.Principal != nobody.Principal || c.Authenticated || c.Admin || c.Privileged || c.Mapped != host.Principal {
		t.Errorf("a proven principal with no uid: %+v, mapped %v", c.Identity, c.Mapped)
	}
}

// A machine principal deleted or disabled is refused as a user is: admission
// does not admit it as the share's anonymous principal, and Mapped, which a
// call naming no share acts through, refuses it too.
func TestADeletedOrDisabledMachineIsRefused(t *testing.T) {
	s := newShares(t, nil, nil)
	ctx := t.Context()
	nobody := s.user(t, "nobody", "", s.share, entity.AccessRead)
	s.must(t, errOf(s.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: s.share, Squash: entity.SquashNone, Anonymous: nobody.Principal}, 0)))
	for i, c := range []struct {
		name   string
		revoke func(m entity.User)
	}{
		{"deleted", func(m entity.User) {
			s.must(t, s.cp.DeleteUser(ctx, m.Principal, userGeneration(t, s.fs.Store, m.Principal)))
		}},
		{"disabled", func(m entity.User) {
			m.Disabled = true
			s.must(t, errOf(s.cp.PutUser(ctx, m, userGeneration(t, s.fs.Store, m.Principal), entity.Alloc{})))
		}},
	} {
		m := entity.User{Principal: entity.Principal{Kind: entity.PrincipalMachine, ID: entity.NewPrincipalID()},
			IDs: entity.ProtocolIDs{{Scheme: entity.SchemeKerberos, Value: "host/build0" + strconv.Itoa(i) + ".corp.example@CORP.EXAMPLE"}}}
		s.must(t, errOf(s.cp.PutUser(ctx, m, 0, entity.Alloc{})))
		id := proven(m.Principal, inLab)
		a, release, err := s.admit(ctx, id, s.share, true, false)
		if err != nil {
			t.Fatalf("%s: a live machine before: %v", c.name, err)
		}
		release()
		if a.caller.Principal != nobody.Principal {
			t.Fatalf("%s: a live machine acts as %v, want the anonymous %v", c.name, a.caller.Principal, nobody.Principal)
		}
		c.revoke(m)
		if _, _, err := s.admit(ctx, id, s.share, true, false); !errors.Is(err, vfs.ErrAccess) {
			t.Errorf("%s machine: admission on a share with an anonymous principal answered %v, want ErrAccess", c.name, err)
		}
		if p, err := admit.Mapped(ctx, principals{s.meta.Principals()}, id.Identity); !errors.Is(err, vfs.ErrAccess) {
			t.Errorf("%s machine: Mapped = %v, %v; want ErrAccess", c.name, p, err)
		}
	}
}

// A share lists only the flavours its minimums admit.
func TestFlavorsListWhatTheMinimumsAdmit(t *testing.T) {
	both := []entity.AuthFlavor{entity.FlavorSys, entity.FlavorKerberos}
	for _, c := range []struct {
		p    entity.ExportPolicy
		list []vfs.AuthFlavor
	}{
		{entity.ExportPolicy{},
			[]vfs.AuthFlavor{vfs.FlavorSys, vfs.FlavorKrb5p, vfs.FlavorKrb5i, vfs.FlavorKrb5, vfs.FlavorNone}},
		{entity.ExportPolicy{Flavors: both},
			[]vfs.AuthFlavor{vfs.FlavorSys, vfs.FlavorKrb5p, vfs.FlavorKrb5i, vfs.FlavorKrb5}},
		{entity.ExportPolicy{MinProof: entity.ProofClaimed},
			[]vfs.AuthFlavor{vfs.FlavorSys, vfs.FlavorKrb5p, vfs.FlavorKrb5i, vfs.FlavorKrb5}},
		{entity.ExportPolicy{MinProof: entity.ProofSecret},
			[]vfs.AuthFlavor{vfs.FlavorKrb5p, vfs.FlavorKrb5i, vfs.FlavorKrb5}},
		{entity.ExportPolicy{Flavors: both, MinProof: entity.ProofTicket, MinProtection: entity.ProtectionIntegrity},
			[]vfs.AuthFlavor{vfs.FlavorKrb5p, vfs.FlavorKrb5i}},
		{entity.ExportPolicy{MinProof: entity.ProofTicket, MinProtection: entity.ProtectionPrivacy},
			[]vfs.AuthFlavor{vfs.FlavorKrb5p}},
	} {
		if got := flavors(c.p); !slices.Equal(got, c.list) {
			t.Errorf("flavors(%+v): %v; want %v", c.p, got, c.list)
		}
	}
}

// A client rule, or the share's policy, narrowing a caller to read-only
// takes every write right from it at the filesystem, whatever its grant, and
// names itself as the cause, so the store answers a write read-only.
func TestAReadOnlyClientRuleNarrowsWrites(t *testing.T) {
	s := newShares(t, nil, nil)
	ctx := t.Context()
	lab := []entity.ClientRule{{Match: entity.ClientMatch{CIDR: netip.MustParsePrefix("10.0.0.0/8")}, Access: entity.AccessRead}}
	for _, c := range []struct {
		name string
		p    entity.ExportPolicy
		want entity.ReadOnlyCause
	}{
		{"client rule", entity.ExportPolicy{Clients: lab}, entity.NarrowedByClient},
		{"share", entity.ExportPolicy{ReadOnly: true}, entity.NarrowedByShare},
		{"both", entity.ExportPolicy{ReadOnly: true, Clients: lab}, entity.NarrowedByShare},
		{"neither", entity.ExportPolicy{}, entity.NotNarrowed},
	} {
		c.p.Share = s.share
		s.must(t, errOf(s.cp.PutExportPolicy(ctx, c.p, policyVersion(t, s.cp, c.p.Share))))
		_, info, err := s.Root(ctx, proven(s.fs.Admin.Principal, inLab), vfs.ShareRef{Path: "/data"})
		s.must(t, err)
		if writes := info.MaximalAccess&entity.WriteData != 0; writes != (c.want == entity.NotNarrowed) || info.MaximalAccess&entity.ReadData == 0 {
			t.Errorf("%s: an admin's maximal access %#x", c.name, info.MaximalAccess)
		}
		a, release, err := s.admit(ctx, proven(s.fs.Admin.Principal, inLab), s.share, true, false)
		s.must(t, err)
		release()
		if a.caller.ReadOnly != c.want {
			t.Errorf("%s: narrowed by %v, want %v", c.name, a.caller.ReadOnly, c.want)
		}
	}
}

// A join with no call counted answers at once; a call counted before the
// share's state is read holds a removal's join, though its read comes after
// the mark and refuses it.
func TestAJoinWaitsForACallCountedBeforeItsShareRead(t *testing.T) {
	s := newShares(t, nil, nil)
	ctx := t.Context()
	admin := proven(s.fs.Admin.Principal, inLab)
	if err := s.JoinShare(ctx, s.share); err != nil {
		t.Errorf("a join with no call counted: %v", err)
	}
	counted, proceed := make(chan struct{}), make(chan struct{})
	s.beforeShareRead = func() {
		close(counted)
		<-proceed
	}
	answered := make(chan error)
	go func() {
		_, err := s.StatFS(context.Background(), admin, s.fs.Root)
		answered <- err
	}()
	<-counted
	s.beforeShareRead = nil
	s.must(t, s.cp.DeleteShare(ctx, s.share, entity.SnapshotsRefuse, 1))
	joined := make(chan error)
	go func() { joined <- s.JoinShare(context.Background(), s.share) }()
	select {
	case err := <-joined:
		t.Errorf("the join returned with a call counted and not answered: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(proceed)
	if err := <-answered; !errors.Is(err, vfs.ErrStale) {
		t.Errorf("the call reading the share after the mark: %v", err)
	}
	select {
	case err := <-joined:
		if err != nil {
			t.Errorf("the join: %v", err)
		}
	case <-time.After(time.Second):
		t.Error("the join never returned")
	}
}

// SecInfo at a share's mount refuses a client the share's rules refuse, as
// SecInfo inside it does, and a pseudo directory above lists no flavour of
// that share.
func TestSecInfoAtAMountRunsTheClientRules(t *testing.T) {
	s := newShares(t, nil, nil)
	ctx := t.Context()
	s.must(t, errOf(s.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: s.share, Flavors: []entity.AuthFlavor{entity.FlavorKerberos},
		Clients: []entity.ClientRule{{Match: entity.ClientMatch{CIDR: netip.MustParsePrefix("10.0.0.0/8")}, Access: entity.AccessReadWrite}}}, 0)))
	vault := s.addShare(t, "secret", "/vault/secret")
	s.must(t, errOf(s.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: vault, Flavors: []entity.AuthFlavor{entity.FlavorKerberos},
		Clients: []entity.ClientRule{{Match: entity.ClientMatch{CIDR: netip.MustParsePrefix("10.0.0.0/8")}, Access: entity.AccessReadWrite}}}, 0)))
	out := proven(s.fs.Admin.Principal, outside)
	root, _, _ := s.Root(ctx, out, vfs.ShareRef{})
	if got, err := s.SecInfo(ctx, out, root, []byte("data")); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("SecInfo at the mount of a share refusing the client: %v, %v", got, err)
	}
	if got, err := s.SecInfo(ctx, out, root, []byte("vault")); err != nil || len(got) != 0 {
		t.Errorf("SecInfo above only a share refusing the client: %v, %v", got, err)
	}
	if got, err := s.SecInfo(ctx, proven(s.fs.Admin.Principal, inLab), root, []byte("vault")); err != nil || len(got) != 3 {
		t.Errorf("SecInfo above a share admitting the client: %v, %v", got, err)
	}
}

// A cursor past every entry of a pseudo directory lists nothing, the last
// cursor a uint64 holds included.
func TestAPseudoCursorPastTheEndListsNothing(t *testing.T) {
	s := newShares(t, nil, nil)
	admin := proven(s.fs.Admin.Principal, inLab)
	root, _, err := s.Root(t.Context(), admin, vfs.ShareRef{})
	s.must(t, err)
	p, err := s.ReadDir(t.Context(), admin, root, vfs.Cursor(^uint64(0)), 0, vfs.EntryHandle)
	if err != nil || len(p.Entries) != 0 || !p.EOF {
		t.Errorf("a listing from the last cursor: %+v, %v", p, err)
	}
}

// A paused share's mount is listed whatever the listing asks of its
// entries, with a pseudo directory's attributes where they are asked, and
// only to a caller its grants admit.
func TestAPausedSharesMountIsListedWhateverIsAsked(t *testing.T) {
	s := newShares(t, nil, nil)
	ctx := t.Context()
	admin := proven(s.fs.Admin.Principal, inLab)
	root, _, err := s.Root(ctx, admin, vfs.ShareRef{})
	s.must(t, err)
	s.must(t, s.cp.SetShareState(ctx, s.share, entity.ShareEnabled, entity.ShareQuiesced))
	h, err := s.ReadDir(ctx, admin, root, 0, 0, vfs.EntryHandle)
	s.must(t, err)
	f, err := s.ReadDir(ctx, admin, root, 0, 0, vfs.EntryHandle|vfs.EntryFile)
	s.must(t, err)
	if len(h.Entries) != 1 || len(f.Entries) != 1 || f.Entries[0].File == nil || f.Entries[0].File.Type != entity.Directory {
		t.Errorf("the paused share's mount: %+v by handle, %+v with attributes", h.Entries, f.Entries)
	}
	stranger := s.user(t, "stranger", "", s.share, entity.AccessNone)
	if p, err := s.ReadDir(ctx, proven(stranger.Principal, inLab), root, 0, 0, vfs.EntryHandle); err != nil || len(p.Entries) != 0 {
		t.Errorf("the paused share's mount, to a caller with no grant on it: %+v, %v", p.Entries, err)
	}
}

// A share with no stored policy squashes root to the anonymous principal,
// and a policy naming no anonymous principal names it too: neither a
// claimed uid 0 nor a squashed caller acts as the zero principal, which
// owns every share's root.
func TestANewShareSquashesRootToAnonymous(t *testing.T) {
	s := newShares(t, nil, nil)
	ctx := t.Context()
	fresh := s.addShare(t, "fresh", "/fresh")
	a, release, err := s.admit(ctx, claimed(0, inLab), fresh, true, false)
	s.must(t, err)
	release()
	if a.caller.Privileged || a.caller.Principal != entity.AnonymousPrincipal {
		t.Errorf("a claimed uid 0 on a share with no policy: privileged %v, acting as %v", a.caller.Privileged, a.caller.Principal)
	}
	bob := s.user(t, "bob", "", s.share, entity.AccessReadWrite)
	s.must(t, errOf(s.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: s.share, Squash: entity.SquashAll}, 0)))
	_, info, err := s.Root(ctx, proven(bob.Principal, inLab), vfs.ShareRef{Path: "/data"})
	if err != nil || info.MaximalAccess&(entity.WriteData|entity.WriteACL) != 0 {
		t.Errorf("bob squashed under a policy naming no anonymous principal: maximal access %#x, %v", info.MaximalAccess, err)
	}
}

// A squashed caller carries no group of the principal it replaced, and its
// anonymous principal deleted refuses every call it would serve, even one
// whose own grant would admit it.
func TestASquashedCallerIsTheAnonymousPrincipalOnly(t *testing.T) {
	s := newShares(t, nil, nil)
	ctx := t.Context()
	staff := entity.Group{Principal: entity.NewGroup(), Name: "staff", IDs: entity.ProtocolIDs{{Scheme: entity.SchemeGID, Value: "1000"}, {Scheme: entity.SchemeSID, Value: "sid-staff"}}}
	s.must(t, errOf(s.cp.PutGroup(ctx, staff, 0, entity.Alloc{})))
	s.must(t, errOf(s.cp.SetGrant(ctx, entity.ShareGrant{Share: s.share, Principal: staff.Principal, Access: entity.AccessReadWrite}, 0)))
	bob := s.user(t, "bob", "1000", s.share, entity.AccessReadWrite)
	s.must(t, s.cp.AddMember(ctx, entity.Membership{Group: staff.Principal, Member: bob.Principal}))
	nobody := s.user(t, "nobody", "", s.share, entity.AccessRead)
	s.must(t, errOf(s.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: s.share, Squash: entity.SquashAll, Anonymous: nobody.Principal}, 0)))
	a, release, err := s.admit(ctx, claimed(1000, inLab), s.share, true, false)
	s.must(t, err)
	release()
	if a.caller.Principal != nobody.Principal || a.caller.InGroup(staff.Principal) {
		t.Errorf("a squashed claimed caller: acting as %v, group %v, groups %v", a.caller.Principal, a.caller.Group, a.caller.Groups)
	}
	s.must(t, s.cp.DeleteUser(ctx, nobody.Principal, userGeneration(t, s.fs.Store, nobody.Principal)))
	if _, err := s.StatFS(ctx, proven(bob.Principal, inLab), s.fs.Root); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("a proven caller, granted read-write, squashed to a deleted anonymous principal: %v", err)
	}
}

// A share's root reports no right beyond the fourteen entry rights as an
// admin's maximal access.
func TestMaximalAccessIsEntryRightsOnly(t *testing.T) {
	s := newShares(t, nil, nil)
	_, info, err := s.Root(t.Context(), proven(s.fs.Admin.Principal, inLab), vfs.ShareRef{Path: "/data"})
	if err != nil || info.MaximalAccess != entity.EntryRights {
		t.Errorf("an admin's maximal access: %#x, %v; want %#x", info.MaximalAccess, err, entity.EntryRights)
	}
}

// A write a read-only share refuses reaches the caller as ErrReadOnly, and
// one a read grant refuses as ErrAccess, as a write call decides it.
func TestANarrowedWriteIsErrReadOnly(t *testing.T) {
	s := newShares(t, nil, nil)
	ctx := t.Context()
	reader := s.user(t, "reader", "", s.share, entity.AccessRead)
	for _, c := range []struct {
		readOnly bool
		who      entity.Principal
		want     error
	}{{true, s.fs.Admin.Principal, vfs.ErrReadOnly}, {false, reader.Principal, vfs.ErrAccess}} {
		s.must(t, errOf(s.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: s.share, ReadOnly: c.readOnly}, policyVersion(t, s.cp, s.share))))
		a, release, err := s.admit(ctx, proven(c.who, inLab), s.share, true, false)
		s.must(t, err)
		err = storeErr(s.authz.Decide(ctx, a.caller, vfs.OpWrite, a.root(), nil, nil, entity.WriteData))
		release()
		if !errors.Is(err, c.want) || c.want == vfs.ErrReadOnly && errors.Is(err, vfs.ErrAccess) {
			t.Errorf("a write by %v: %v, want %v", c.who, err, c.want)
		}
	}
}

// The pseudo-filesystem lists the same shares whatever a listing asks of
// its entries: a mount whose crossing the caller is refused is left out
// either way.
func TestAPseudoListingIsTheSameWhateverIsAsked(t *testing.T) {
	s := newShares(t, nil, nil)
	ctx := t.Context()
	eve := proven(s.user(t, "eve", "", s.share, entity.AccessNone).Principal, inLab)
	root, _, err := s.Root(ctx, eve, vfs.ShareRef{})
	s.must(t, err)
	for _, want := range []vfs.EntryFields{vfs.EntryHandle, vfs.EntryHandle | vfs.EntryFile, 0} {
		if p, err := s.ReadDir(ctx, eve, root, 0, 0, want); err != nil || len(p.Entries) != 0 {
			t.Errorf("listing asking %v for a caller the share grant refuses: %+v, %v", want, p.Entries, err)
		}
	}
}

// Crossing from the pseudo-filesystem into a share is decided through the
// audited chokepoint, by a lookup and by a listing alike.
func TestCrossingIntoAShareIsAudited(t *testing.T) {
	rec := &recorder{}
	s := newShares(t, rec, func(vfs.ShareID) authz.Policy { return authz.AuditAll })
	ctx := t.Context()
	admin := proven(s.fs.Admin.Principal, inLab)
	root, _, err := s.Root(ctx, admin, vfs.ShareRef{})
	s.must(t, err)
	_, _, err = s.Lookup(ctx, admin, root, []byte("data"))
	s.must(t, err)
	_, err = s.ReadDir(ctx, admin, root, 0, 0, vfs.EntryHandle)
	s.must(t, err)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var ops []vfs.Op
	for _, e := range rec.got {
		if e.Share == s.share && string(e.Name) == "data" && e.Allowed {
			ops = append(ops, e.Op)
		}
	}
	if !slices.Equal(ops, []vfs.Op{vfs.OpLookup, vfs.OpReadDir}) {
		t.Errorf("the crossings audited: %v, want a lookup then a listing", ops)
	}
}

// failKV fails the next View once armed: a store failing a read. With fails
// set, a View or an Update fails with what fails answers for it, update set
// for an Update.
type failKV struct {
	kv.KV
	mu    sync.Mutex
	err   error
	fails func(update bool) error
}

func (f *failKV) arm(err error) { f.mu.Lock(); f.err = err; f.mu.Unlock() }

// failing sets fails.
func (f *failKV) failing(fails func(update bool) error) { f.mu.Lock(); f.fails = fails; f.mu.Unlock() }

func (f *failKV) View(ctx context.Context, fn func(kv.Reader) error) error {
	f.mu.Lock()
	err := f.err
	f.err = nil
	if f.fails != nil && err == nil {
		err = f.fails(false)
	}
	f.mu.Unlock()
	if err != nil {
		return err
	}
	return f.KV.View(ctx, fn)
}

func (f *failKV) Update(ctx context.Context, fn func(kv.Txn) error) error {
	f.mu.Lock()
	var err error
	if f.fails != nil {
		err = f.fails(true)
	}
	f.mu.Unlock()
	if err != nil {
		return err
	}
	return f.KV.Update(ctx, fn)
}

// A listing fails when a share's read fails, a store failure or an
// inconsistency alike, rather than leaving the share out.
func TestAListingFailsWithTheStore(t *testing.T) {
	db := &failKV{}
	fx := kvstoretest.NewFS(t, func(tb testing.TB) kv.KV { db.KV = memKV(tb); return db })
	clock := sim.NewTestClock(time.Unix(0, 0))
	tab, err := openstate.New(openstate.Deps{Records: fx.Store, Node: entity.NodeID{15: 1}, Clock: clock, Rand: rand.Reader})
	s, err2 := New(Deps{Meta: fx.Store, Open: tab, Engine: contentOver(clock, fx.Store, fx.Share.ID)})
	if err = errors.Join(err, err2); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(tab.Start(t.Context(), s, s.Admission), s.Start(t.Context())); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Drain(context.Background()); _ = s.Stop(); tab.Stop() })
	admin := proven(fx.Admin.Principal, inLab)
	root, _, err := s.Root(t.Context(), admin, vfs.ShareRef{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		inject error
		want   error
	}{{errors.New("disk gone"), vfs.ErrUnavailable}, {kvstore.ErrInconsistent, vfs.ErrInconsistent}} {
		s.beforeShareRead = func() { db.arm(c.inject) }
		_, err := s.ListShares(t.Context(), admin)
		if !errors.Is(err, c.want) {
			t.Errorf("ListShares with a share's read failing %v: %v", c.inject, err)
		}
		_, err = s.ReadDir(t.Context(), admin, root, 0, 0, vfs.EntryHandle)
		if !errors.Is(err, c.want) {
			t.Errorf("ReadDir with a share's read failing %v: %v", c.inject, err)
		}
	}
}

// errOf is a write's error alone, its returned generation dropped.
func errOf[T any](_ T, err error) error { return err }
