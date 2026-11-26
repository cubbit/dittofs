package identity_test

import (
	"context"
	"errors"
	"testing"

	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// whos is the owners fixture plus a user whose name is a SID string.
type whos struct {
	*owners
	sidNamed entity.Principal
}

func newWhos(t *testing.T) *whos {
	t.Helper()
	o := newOwners(t)
	sidNamed := entity.User{Principal: entity.NewUser(), Name: "S-1-5-21-9-9-9-1500", IDs: fileIDs("4000")}
	o.user(t, sidNamed, 0)
	return &whos{owners: o, sidNamed: sidNamed.Principal}
}

// The who schemes spell as the owner schemes do, else by the SID at the
// domain, and a principal no record holds as the null SID; who holds users,
// who_group groups, and either a well-known principal. Each spelling but the
// null SID finds its principal back.
func TestWhoSpell(t *testing.T) {
	w := newWhos(t)
	ctx := context.Background()
	for _, c := range []struct {
		p      entity.Principal
		scheme pid.Scheme
		want   string // "" is ErrUnknownPrincipal
	}{
		{w.alice, pid.SchemeWho, "alice@corp.example"},
		{w.aliceGroup, pid.SchemeWhoGroup, "alice@corp.example"},
		{w.unnamedUser, pid.SchemeWho, "3000"},
		{w.unnamedGroup, pid.SchemeWhoGroup, "3001"},
		{w.sidNamed, pid.SchemeWho, "4000"},
		{entity.RootPrincipal, pid.SchemeWho, "0"},
		{entity.AnonymousPrincipal, pid.SchemeWhoGroup, "65534"},
		{entity.OwnerRights, pid.SchemeWho, "S-1-3-4@corp.example"},
		{entity.OwnerRights, pid.SchemeWhoGroup, "S-1-3-4@corp.example"},
		{entity.Everyone, pid.SchemeWho, "S-1-1-0@corp.example"},
		{w.deleted, pid.SchemeWho, "S-1-0-0@corp.example"},
		{entity.Owner, pid.SchemeWho, ""},
		{entity.OwnerGroup, pid.SchemeWhoGroup, ""},
		{w.alice, pid.SchemeWhoGroup, ""},
		{w.aliceGroup, pid.SchemeWho, ""},
		{w.deleted, pid.SchemeWhoGroup, ""},
	} {
		sp, err := w.srv.Spell(ctx, c.p, c.scheme)
		if c.want == "" {
			if !errors.Is(err, vfserr.ErrUnknownPrincipal) {
				t.Errorf("Spell(%v, %d) = %+v, %v; want ErrUnknownPrincipal", c.p, c.scheme, sp, err)
			}
			continue
		}
		if err != nil || sp.Scheme != c.scheme || sp.Text != c.want {
			t.Errorf("Spell(%v, %d) = %+v, %v; want %q", c.p, c.scheme, sp, err, c.want)
			continue
		}
		if c.p == w.deleted {
			continue
		}
		if p, err := w.srv.Find(ctx, sp, pid.FindOpts{}); err != nil || p != c.p {
			t.Errorf("Find(%d %q) = %v, %v; want %v back", c.scheme, sp.Text, p, err, c.p)
		}
	}
}

// Find reads a well-formed S-…@domain by its SID, in the applied domain and
// in the scheme's own kind, and any other who as the owner schemes do: a
// malformed SID is a name. A miss naming a SID is counted under sid, any
// other under owner.
func TestWhoFind(t *testing.T) {
	w := newWhos(t)
	ctx := context.Background()
	misses := map[string]float64{}
	for _, c := range []struct {
		scheme pid.Scheme
		text   string
		want   entity.Principal // zero is ErrUnknownPrincipal
		label  string           // the miss's label
	}{
		{pid.SchemeWho, "S-1-5-21-9-9-9-1500@corp.example", w.alice, ""},
		{pid.SchemeWho, "S-1-5-21-9-9-9-1500@CORP.EXAMPLE", w.alice, ""},
		{pid.SchemeWhoGroup, "S-1-5-21-9-9-8-1500@corp.example", w.aliceGroup, ""},
		{pid.SchemeWho, "S-1-1-0@corp.example", entity.Everyone, ""},
		{pid.SchemeWhoGroup, "S-1-1-0@corp.example", entity.Everyone, ""},
		{pid.SchemeWho, "alice@corp.example", w.alice, ""},
		{pid.SchemeWhoGroup, "alice@corp.example", w.aliceGroup, ""},
		{pid.SchemeWho, "1500", w.alice, ""},
		{pid.SchemeWhoGroup, "1500", w.aliceGroup, ""},
		// Not canonical, so a name, which no user holds.
		{pid.SchemeWho, "S-1-5-21-9-9-9-01500@corp.example", entity.Principal{}, "owner"},
		{pid.SchemeWhoGroup, "S-1-5-21-9-9-9-1500@corp.example", entity.Principal{}, "sid"},
		{pid.SchemeWho, "S-1-5-21-9-9-8-1500@corp.example", entity.Principal{}, "sid"},
		{pid.SchemeWho, "S-1-5-21-9-9-9-1500@other.example", entity.Principal{}, "sid"},
		{pid.SchemeWho, "S-1-5-21-9-9-9-9999@corp.example", entity.Principal{}, "sid"},
		{pid.SchemeWhoGroup, "S-1-5-21-9-9-9-9999@corp.example", entity.Principal{}, "sid"},
		{pid.SchemeWho, "S-1-0-0@corp.example", entity.Principal{}, "sid"},
		{pid.SchemeWho, "carol@corp.example", entity.Principal{}, "owner"},
		{pid.SchemeWhoGroup, "carol@corp.example", entity.Principal{}, "owner"},
		{pid.SchemeWho, "S-1-5-x@corp.example", entity.Principal{}, "owner"},
		{pid.SchemeWho, "S-1-5-21-9-9-9-1500", entity.Principal{}, "owner"},
	} {
		p, err := w.srv.Find(ctx, pid.Spelling{Scheme: c.scheme, Text: c.text}, pid.FindOpts{})
		if c.want == (entity.Principal{}) {
			misses[c.label]++
			if !errors.Is(err, vfserr.ErrUnknownPrincipal) {
				t.Errorf("Find(%d %q) = %v, %v; want ErrUnknownPrincipal", c.scheme, c.text, p, err)
			}
			continue
		}
		if err != nil || p != c.want {
			t.Errorf("Find(%d %q) = %v, %v; want %v", c.scheme, c.text, p, err, c.want)
		}
	}
	// A name that is a SID string is found by that name only through the
	// owner scheme.
	if p, err := w.srv.Find(ctx, pid.Spelling{Scheme: pid.SchemeOwner, Text: "S-1-5-21-9-9-9-1500@corp.example"}, pid.FindOpts{}); err != nil || p != w.sidNamed {
		t.Errorf("Find(owner S-1-5-21-9-9-9-1500@corp.example) = %v, %v; want the user of that name", p, err)
	}
	for _, label := range []string{"owner", "sid", "uid", "gid"} {
		if got := w.counter(t, "identity_unmapped_total", "scheme", label); got != misses[label] {
			t.Errorf("identity_unmapped_total{scheme=%q} = %v, want %v", label, got, misses[label])
		}
	}
}

// A store error on the SID index reaches the caller as itself, never as
// ErrUnknownPrincipal, and is not counted as unmapped.
func TestWhoLookupErrorPassesThrough(t *testing.T) {
	w := newWhos(t)
	ctx := context.Background()
	w.fail(t, kvtest.OpGet, "PX*")
	for _, sp := range []pid.Spelling{
		{Scheme: pid.SchemeWho, Text: "S-1-5-21-9-9-9-1500@corp.example"},
		{Scheme: pid.SchemeWhoGroup, Text: "S-1-5-21-9-9-8-1500@corp.example"},
	} {
		if p, err := w.srv.Find(ctx, sp, pid.FindOpts{}); err == nil || errors.Is(err, vfserr.ErrUnknownPrincipal) {
			t.Errorf("Find(%d %q) with the SID index failing = %v, %v; want the store's error", sp.Scheme, sp.Text, p, err)
		}
	}
	if got := w.counter(t, "identity_unmapped_total", "scheme", "sid"); got != 0 {
		t.Errorf("identity_unmapped_total{scheme=\"sid\"} after store errors = %v, want 0", got)
	}
}
