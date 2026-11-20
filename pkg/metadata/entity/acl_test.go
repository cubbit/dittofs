package entity

import (
	"slices"
	"testing"
)

func TestACLFromModeImpliesItsMode(t *testing.T) {
	for mode := uint32(0); mode <= 0o777; mode++ {
		if got := ACLFromMode(mode).Mode(); got != mode {
			t.Fatalf("ACLFromMode(%o).Mode() = %o", mode, got)
		}
	}
}

// RFC 19 §4.1: in the ACL a mode implies, any bit grants its class read the
// ACL and synchronize and a class with none is granted neither, a group
// member whose group has no bit taking nothing from everyone's allow; read
// attributes is granted to every class, and the owner reads the ACL whatever
// its bits (§3.1, step 5).
func TestACLFromModeMinorRights(t *testing.T) {
	const asked = modeRWX | ReadAttributes | ReadACL | Synchronize
	for mode := uint32(0); mode <= 0o777; mode++ {
		acl := ACLFromMode(mode)
		for _, c := range []struct {
			name  string
			shift uint
			who   []Principal
		}{
			{"the owner, in the group", 6, []Principal{Owner, OwnerGroup, Everyone}},
			{"a group member", 3, []Principal{OwnerGroup, Everyone}},
			{"another user", 0, []Principal{Everyone}},
		} {
			bits := mode >> c.shift & 7
			want := rights(bits) | ReadAttributes
			if bits != 0 {
				want |= ReadACL | Synchronize
			}
			if c.shift == 6 {
				want |= ReadACL
			}
			if got := walk(acl, c.who...) & asked; got != want {
				t.Errorf("ACLFromMode(%03o) grants %s %#x, want %#x", mode, c.name, got, want)
			}
		}
	}
}

// RFC 19 §4.2: a chmod to the mode an ACL already implies changes what it
// grants nobody, so ACLFromMode's ACL for every mode keeps its grants across
// a chmod to that mode, the minor rights included.
func TestChmodToItsOwnModeKeepsTheGrants(t *testing.T) {
	const asked = modeRWX | ReadAttributes | ReadACL | Synchronize
	for mode := uint32(0); mode <= 0o777; mode++ {
		acl := ACLFromMode(mode)
		for _, who := range [][]Principal{{Owner, OwnerGroup, Everyone}, {OwnerGroup, Everyone}, {Everyone}} {
			if got, want := walk(acl.WithMode(mode), who...)&asked, walk(acl, who...)&asked; got != want {
				t.Errorf("chmod %03o of its own ACL grants %v %#x, want %#x", mode, who, got, want)
			}
		}
	}
}

// walk is what a's effective entries grant a caller they name through who,
// each right decided by the first entry naming it.
func walk(a ACL, who ...Principal) AccessMask {
	var allow, decided AccessMask
	for _, e := range a.Entries {
		if !e.Effective() || !slices.Contains(who, e.Who) {
			continue
		}
		if e.Type == ACEAllow {
			allow |= e.Mask &^ decided
		}
		decided |= e.Mask
	}
	return allow
}

func TestModeFirstEntryDecides(t *testing.T) {
	// A deny for everyone ahead of the owner's allow takes write from all.
	a := ACL{Entries: []ACE{
		{Type: ACEDeny, Mask: modeWrite, Who: Everyone},
		{Type: ACEAllow, Mask: modeRWX, Who: Owner},
	}}
	if got := a.Mode(); got != 0o500 {
		t.Errorf("Mode = %o", got)
	}
}

// RFC 7 §12.1's chmod merge check.
func TestChmodMerge(t *testing.T) {
	bob := NewUser()
	named := ACE{Type: ACEAllow, Mask: modeRead | modeWrite, Who: bob}
	inheritOnly := ACE{Type: ACEAllow, Flags: ACEFileInherit | ACEInheritOnly, Mask: modeRWX, Who: NewGroup()}
	both := ACE{Type: ACEAllow, Flags: ACEDirInherit, Mask: modeRWX, Who: NewUser()}
	deny := ACE{Type: ACEDeny, Mask: Delete, Who: NewUser()}
	audit := ACE{Type: ACEAudit, Flags: ACESuccessfulAccess, Mask: ReadData, Who: Everyone}
	a := ACL{Entries: append([]ACE{named, inheritOnly, both, deny}, ACLFromMode(0o777).Entries...), Audit: []ACE{audit}}

	got := a.WithMode(0o600)
	if m := got.Mode(); m != 0o600 {
		t.Fatalf("merged Mode = %o", m)
	}
	find := func(who Principal, effective bool) []ACE {
		var out []ACE
		for _, e := range got.Entries {
			if e.Who == who && e.Effective() == effective {
				out = append(out, e)
			}
		}
		return out
	}
	// The named user is left with no rwx right at all.
	if es := find(bob, true); len(es) != 1 || es[0].Mask&modeRWX != 0 {
		t.Errorf("named user after chmod 600: %+v", es)
	}
	// The inherit-only entry is unchanged.
	if !slices.Contains(got.Entries, inheritOnly) {
		t.Error("inherit-only entry changed")
	}
	// The effective and inheritable entry is split.
	kept := both
	kept.Flags |= ACEInheritOnly
	if !slices.Contains(got.Entries, kept) {
		t.Error("no unchanged inherit-only copy")
	}
	if es := find(both.Who, true); len(es) != 1 || es[0].Inheritable() || es[0].Mask&modeRWX != 0 {
		t.Errorf("effective copy: %+v", es)
	}
	if !slices.Contains(got.Entries, deny) || !slices.Equal(got.Audit, a.Audit) {
		t.Error("deny or audit entries changed")
	}
	// A chmod adds only what a new bit grants (RFC 19 §4.3): on an ACL that
	// grants the group and the others nothing, a chmod to 600 leaves them
	// neither read the ACL nor synchronize.
	owned := ACL{Entries: []ACE{{Type: ACEAllow, Mask: ReadData, Who: Owner}}}.WithMode(0o600)
	for name, who := range map[string][]Principal{"a group member": {OwnerGroup, Everyone}, "another user": {Everyone}} {
		if g := walk(owned, who...); g&(ReadACL|Synchronize) != 0 {
			t.Errorf("chmod 600 granted %s %#x", name, g)
		}
	}
	// The group bits bound what a named principal keeps.
	if es := (ACL{Entries: []ACE{named}}).WithMode(0o640).Entries[0]; es.Mask&modeRWX != modeRead {
		t.Errorf("chmod 640 left the named user %x", es.Mask&modeRWX)
	}
}

func TestACLValid(t *testing.T) {
	ok := ACL{Entries: ACLFromMode(0o644).Entries, Audit: []ACE{{Type: ACEAlarm, Who: Everyone}}}
	if !ok.Valid() {
		t.Error("valid ACL refused")
	}
	for _, bad := range []ACL{
		{Entries: []ACE{{Type: ACEAudit, Who: Everyone}}},
		{Audit: []ACE{{Type: ACEAllow, Who: Everyone}}},
		{Entries: []ACE{{Type: ACEAllow}}},
	} {
		if bad.Valid() {
			t.Errorf("accepted %+v", bad)
		}
	}
}

// RFC 8881 §6.4.3 and RFC 7 §2.6's inheritance at create.
func TestInherit(t *testing.T) {
	alice, staff, finance := NewUser(), NewGroup(), NewGroup()
	parent := ACL{Entries: []ACE{
		{Type: ACEAllow, Flags: ACEFileInherit | ACEDirInherit | ACEInheritOnly, Mask: modeRWX | Delete, Who: CreatorOwner},
		{Type: ACEAllow, Flags: ACEFileInherit | ACEDirInherit, Mask: modeRead | modeWrite, Who: finance},
		{Type: ACEAllow, Flags: ACEDirInherit | ACENoPropagate, Mask: modeRead, Who: staff},
		{Type: ACEAllow, Mask: modeRWX, Who: Everyone}, // not inheritable
	}}

	file, ok := parent.Inherit(false, alice, staff, 0o777)
	if !ok {
		t.Fatal("a file inherited nothing")
	}
	want := []ACE{
		{Type: ACEAllow, Flags: ACEInherited, Mask: modeRWX | Delete, Who: alice},
		{Type: ACEAllow, Flags: ACEInherited, Mask: modeRead | modeWrite, Who: finance},
	}
	if !slices.Equal(file.Entries, want) {
		t.Errorf("a file inherits\n %+v\nwant\n %+v", file.Entries, want)
	}

	dir, _ := parent.Inherit(true, alice, staff, 0o777)
	creatorKept := false
	for _, e := range dir.Entries {
		if e.Who == CreatorOwner {
			creatorKept = !e.Effective() && e.Inheritable()
		}
		if e.Who == staff && e.Inheritable() {
			t.Error("a no-propagate entry propagated past the new directory")
		}
	}
	if !creatorKept {
		t.Errorf("the propagating copy lost CREATOR OWNER or was effective: %+v", dir.Entries)
	}
	if !slices.ContainsFunc(dir.Entries, func(e ACE) bool { return e.Who == staff && e.Effective() }) {
		t.Errorf("a directory-inherit entry gave the new directory nothing: %+v", dir.Entries)
	}

	// A requested 0640 limits but never adds: alice, a named owner principal,
	// and finance keep only the group bits' read; nothing new appears.
	limited, _ := parent.Inherit(false, alice, staff, 0o640)
	for _, e := range limited.Entries {
		if e.Mask&modeRWX != modeRead {
			t.Errorf("mode 0640 left %v with %v", e.Who, e.Mask&modeRWX)
		}
		if e.Mask&Delete == 0 && e.Who == alice {
			t.Error("the mode took away a right outside read, write and execute")
		}
	}
	if len(limited.Entries) != len(file.Entries) {
		t.Errorf("the mode added or removed entries: %+v", limited.Entries)
	}

	if _, ok := (ACL{Entries: []ACE{{Type: ACEAllow, Flags: ACEDirInherit, Mask: modeRead, Who: staff}}}).Inherit(false, alice, staff, 0o644); ok {
		t.Error("a file inherited a directory-only entry")
	}
}
