package entity

import (
	"slices"
	"testing"
)

// bitRights is every right a mode bit grants beyond the minor rights.
const bitRights = ReadData | writeSet | DeleteChild | Execute

func allowOf(m AccessMask, who Principal) ACE { return ACE{Type: ACEAllow, Mask: m, Who: who} }
func denyOf(m AccessMask, who Principal) ACE  { return ACE{Type: ACEDeny, Mask: m, Who: who} }

// RFC 19 §4.1's R(bits), right by right: any bit the minor rights, read
// data, the write set with delete a child on a directory, execute; no bit
// nothing.
func TestModeRights(t *testing.T) {
	const minor = Synchronize | ReadACL | ReadAttributes | ReadNamedAttrs
	const write = WriteData | AppendData | WriteNamedAttrs | WriteAttributes
	for _, c := range []struct {
		bits uint32
		dir  bool
		want AccessMask
	}{
		{0, false, 0},
		{0, true, 0},
		{4, false, minor | ReadData},
		{2, false, minor | write},
		{2, true, minor | write | DeleteChild},
		{1, false, minor | Execute},
		{7, false, minor | ReadData | write | Execute},
		{7, true, minor | ReadData | write | DeleteChild | Execute},
	} {
		if got := ModeRights(c.bits, c.dir); got != c.want {
			t.Errorf("ModeRights(%o, dir %v) = %#x, want %#x", c.bits, c.dir, got, c.want)
		}
	}
}

// RFC 19 §4.1: the entries a mode implies, in order and each left out when
// empty, none inheritable, a deny only where a later class has a bit an
// earlier one lacks.
func TestACLFromModeEntries(t *testing.T) {
	for _, dir := range []bool{false, true} {
		r := func(b uint32) AccessMask { return ModeRights(b, dir) }
		for _, c := range []struct {
			mode uint32
			want []ACE
		}{
			{0o604, []ACE{allowOf(r(6), Owner), denyOf(r(4), OwnerGroup), allowOf(r(4), Everyone)}},
			{0o755, []ACE{allowOf(r(7), Owner), allowOf(r(5), OwnerGroup), allowOf(r(5), Everyone)}},
			{0o640, []ACE{allowOf(r(6), Owner), allowOf(r(4), OwnerGroup)}},
			{0o600, []ACE{allowOf(r(6), Owner)}},
			{0o000, nil},
			{0o064, []ACE{denyOf(r(6), Owner), allowOf(r(6), OwnerGroup), allowOf(r(4), Everyone)}},
		} {
			if got := ACLFromMode(c.mode, dir); !slices.Equal(got.Entries, c.want) || got.Flags != 0 || got.Audit != nil {
				t.Errorf("ACLFromMode(%04o, dir %v) = %+v, want %+v", c.mode, dir, got, c.want)
			}
		}
	}
}

// RFC 19 §4.1: through the walk, the ACL a mode implies grants each class R
// of its bits and nothing a later class adds: the owner, in the group or
// not, the owner bits; a group member the group bits; anyone else the other
// bits. The owner's implicit rights are the walk's to add (§3.1, step 5),
// not an entry's.
func TestACLFromModeGrantsEachClassItsBits(t *testing.T) {
	for _, dir := range []bool{false, true} {
		for mode := uint32(0); mode <= 0o777; mode++ {
			acl := ACLFromMode(mode, dir)
			for _, c := range []struct {
				name  string
				shift uint
				who   []Principal
			}{
				{"the owner, in the group", 6, []Principal{Owner, OwnerGroup, Everyone}},
				{"the owner, not in the group", 6, []Principal{Owner, Everyone}},
				{"a group member", 3, []Principal{OwnerGroup, Everyone}},
				{"another user", 0, []Principal{Everyone}},
			} {
				if got, want := walk(acl, c.who...), ModeRights(mode>>c.shift&7, dir); got != want {
					t.Errorf("ACLFromMode(%03o, dir %v) grants %s %#x, want %#x", mode, dir, c.name, got, want)
				}
			}
		}
	}
}

// RFC 19 §4.2's first identity, for every mode and both file types: the ACL
// a mode implies implies that mode. Mode reads no membership, so the
// owner's groups do not enter it; the walk above has the owner in the group
// and out of it.
func TestACLFromModeImpliesItsMode(t *testing.T) {
	o := NewUser()
	for _, dir := range []bool{false, true} {
		for mode := uint32(0); mode <= 0o777; mode++ {
			if got := ACLFromMode(mode, dir).Mode(o); got != mode {
				t.Fatalf("ACLFromMode(%03o, dir %v).Mode = %03o", mode, dir, got)
			}
		}
	}
}

// RFC 19 §4.2: each class's bits are what the walk grants it, read from
// read data, write from write data and execute from execute; the group
// class is the union over GROUP@ and the named principals other than the
// owner, the other class over three strangers; inherit-only entries, a
// principal no caller matches and the owner's implicit rights count for
// nothing.
func TestModeClasses(t *testing.T) {
	alice, bob, staff := NewUser(), NewUser(), NewGroup()
	for _, c := range []struct {
		name    string
		entries []ACE
		owner   Principal
		want    uint32
	}{
		{"a named user on another's file", []ACE{allowOf(ReadData, bob)}, alice, 0o040},
		{"a named user on its own file", []ACE{allowOf(ReadData, bob)}, bob, 0o400},
		{"GROUP@ and a named group, their union", []ACE{allowOf(ReadData, OwnerGroup), allowOf(WriteData, staff)}, alice, 0o060},
		{"Administrators, a named principal", []ACE{allowOf(Execute, Administrators)}, alice, 0o010},
		{"owner rights", []ACE{allowOf(ReadData, OwnerRights)}, alice, 0o400},
		{"a deny ahead of an allow", []ACE{denyOf(WriteData, Everyone), allowOf(ReadData|WriteData|Execute, Owner)}, alice, 0o500},
		{"an allow ahead of a deny", []ACE{allowOf(WriteData, Owner), denyOf(WriteData, Everyone)}, alice, 0o200},
		{"a deny for one named user", []ACE{denyOf(ReadData, bob), allowOf(ReadData, Everyone)}, alice, 0o444},
		{"Authenticated Users, one stranger", []ACE{allowOf(ReadData, AuthenticatedUsers)}, alice, 0o004},
		{"ANONYMOUS LOGON, another", []ACE{allowOf(WriteData, AnonymousPrincipal)}, alice, 0o002},
		{"NETWORK, every caller", []ACE{allowOf(Execute, Network)}, alice, 0o111},
		{"a group deny the other class passes", ACLFromMode(0o604, false).Entries, alice, 0o604},
		{"append without write data", []ACE{allowOf(AppendData, Owner)}, alice, 0},
		{"SYSTEM, matching no caller", []ACE{allowOf(ReadData, System)}, alice, 0},
		{"inherit-only", []ACE{{Type: ACEAllow, Flags: ACEFileInherit | ACEInheritOnly, Mask: ReadData, Who: Everyone}}, alice, 0},
		{"no entries, the owner's implicit rights no bits", nil, alice, 0},
	} {
		if got := (ACL{Entries: c.entries}).Mode(c.owner); got != c.want {
			t.Errorf("%s: Mode = %03o, want %03o", c.name, got, c.want)
		}
	}
}

// RFC 19 §4.2's second identity: a chmod to the mode an ACL implies grants
// every caller exactly what the ACL grants, and only limits: no entry
// added, moved or raised. The corpus is every list of up to three entries
// from a pool, class allows and denies, named allows and denies, the
// owner's own, inheritable, inherit-only and inherited entries, owner
// rights, the everyone-like principals, SYSTEM, a directory's list and add
// a file alone and a right no bit grants, with every ACL a mode implies,
// for two owners and both file types. Every entry keeps §4.2's
// precondition: an allow holding a right the write bit adds holds write
// data, and a deny holding write data holds every right the write bit adds.
func TestChmodToTheACLsOwnModeKeepsEveryGrant(t *testing.T) {
	alice, bob, carol, staff := NewUser(), NewUser(), NewUser(), NewGroup()
	both := ACEFileInherit | ACEDirInherit
	for _, dir := range []bool{false, true} {
		r := func(b uint32) AccessMask { return ModeRights(b, dir) }
		w := r(2) &^ minorRights
		pool := []ACE{
			allowOf(r(7), Owner),
			{Type: ACEAllow, Flags: both, Mask: r(6), Who: Owner},
			allowOf(r(5), OwnerGroup),
			allowOf(r(4), Everyone),
			denyOf(w, Everyone),
			denyOf(r(4), OwnerGroup),
			denyOf(Execute, Owner),
			allowOf(r(6), bob),
			denyOf(ReadData, bob),
			allowOf(ReadData, alice),
			{Type: ACEAllow, Flags: both, Mask: r(7) | Delete, Who: staff},
			{Type: ACEAllow, Flags: ACEFileInherit | ACEInheritOnly, Mask: r(7), Who: carol},
			allowOf(Delete, carol),
			allowOf(r(4), OwnerRights),
			allowOf(r(7), AuthenticatedUsers),
			allowOf(r(4), AnonymousPrincipal),
			allowOf(Execute, Network),
			allowOf(r(6), Administrators),
			allowOf(r(7), System),
			allowOf(ReadData|WriteData, Everyone),
			{Type: ACEAllow, Flags: ACEInherited, Mask: w, Who: Everyone},
		}
		var corpus [][]ACE
		for _, a := range pool {
			corpus = append(corpus, []ACE{a})
			for _, b := range pool {
				corpus = append(corpus, []ACE{a, b})
				for _, c := range pool {
					corpus = append(corpus, []ACE{a, b, c})
				}
			}
		}
		for mode := uint32(0); mode <= 0o777; mode++ {
			corpus = append(corpus, ACLFromMode(mode, dir).Entries)
		}
		for _, o := range []Principal{alice, bob} {
			x := bob
			if o == bob {
				x = alice
			}
			var callers [][]Principal
			for _, who := range [][]Principal{
				{Owner, OwnerRights, o},
				{Owner, OwnerRights, o, OwnerGroup, staff, AuthenticatedUsers, BuiltinUsers},
				{x},
				{x, OwnerGroup, staff, AuthenticatedUsers, BuiltinUsers},
				{carol, OwnerGroup},
				{carol, staff},
				{AuthenticatedUsers, BuiltinUsers},
				{AnonymousPrincipal},
				{},
				{Administrators, AuthenticatedUsers, BuiltinUsers},
			} {
				callers = append(callers, append(who, Everyone, Network))
			}
			for _, es := range corpus {
				a := ACL{Entries: es}
				mode := a.Mode(o)
				merged := a.WithMode(mode, o, dir)
				if !limitsOnly(a, merged) {
					t.Fatalf("chmod %03o of %+v, dir %v: %+v adds, moves or raises an entry", mode, es, dir, merged.Entries)
				}
				for _, c := range callers {
					if got, want := walk(merged, c...), walk(a, c...); got != want {
						t.Fatalf("chmod %03o of %+v, dir %v, grants %v %#x, want %#x", mode, es, dir, c, got, want)
					}
				}
			}
		}
	}
}

// limitsOnly reports whether merged is a with each entry in its place,
// kept, limited, or split into an unchanged inherit-only copy and a limited
// one, every deny kept as it was, and nothing more.
func limitsOnly(a, merged ACL) bool {
	i := 0
	for _, e := range a.Entries {
		split := e
		split.Flags |= ACEInheritOnly
		if e.Effective() && e.Inheritable() && i < len(merged.Entries) && merged.Entries[i] == split {
			i++
		}
		if i == len(merged.Entries) {
			return false
		}
		m := merged.Entries[i]
		if m.Type != e.Type || m.Who != e.Who || m.Mask&^e.Mask != 0 || e.Type == ACEDeny && m != e {
			return false
		}
		i++
	}
	return i == len(merged.Entries) && slices.Equal(a.Audit, merged.Audit)
}

// RFC 19 §4.2: a chmod to the mode an ACL already implies changes what it
// grants nobody, so ACLFromMode's ACL for every mode keeps its grants across
// a chmod to that mode, the minor rights included.
func TestChmodToItsOwnModeKeepsTheGrants(t *testing.T) {
	o := NewUser()
	for _, dir := range []bool{false, true} {
		asked := ModeRights(7, dir)
		for mode := uint32(0); mode <= 0o777; mode++ {
			acl := ACLFromMode(mode, dir)
			for _, who := range [][]Principal{{Owner, OwnerGroup, Everyone}, {OwnerGroup, Everyone}, {Everyone}} {
				if got, want := walk(acl.WithMode(mode, o, dir), who...)&asked, walk(acl, who...)&asked; got != want {
					t.Errorf("chmod %03o of its own ACL grants %v %#x, want %#x", mode, who, got, want)
				}
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

// RFC 19 §4.3: a chmod to the file's own mode adds nothing, so the ACL any
// mode implies comes back from a chmod to that mode as it was.
func TestChmodToTheFilesOwnModeAddsNothing(t *testing.T) {
	o := NewUser()
	for _, dir := range []bool{false, true} {
		for mode := uint32(0); mode <= 0o777; mode++ {
			acl := ACLFromMode(mode, dir)
			if got := acl.WithMode(mode, o, dir); !slices.Equal(got.Entries, acl.Entries) {
				t.Errorf("chmod %03o of its own ACL, dir %v: %+v, want %+v", mode, dir, got.Entries, acl.Entries)
			}
		}
	}
}

// RFC 19 §4.3's chmod 750 example (img/rfc19-mode-acl.svg, and §6's step
// 7): on a directory alice owns, builders' allow is narrowed to the group
// bits, the mask, by a split into an unchanged inherit-only copy and a
// limited effective one; alice's own allow, in the owner class, is limited
// to the owner bits, rwx, and stays whole; bob's deny is untouched; and the
// owner class already holding rwx, nothing is appended.
func TestChmod750Example(t *testing.T) {
	alice, bob, builders := NewUser(), NewUser(), NewGroup()
	fd := ACEFileInherit | ACEDirInherit
	const (
		waNT   = WriteData | AppendData | WriteNamedAttrs | WriteAttributes
		rx     = ReadData | ReadNamedAttrs | ReadAttributes | ReadACL | Synchronize | Execute
		modify = rx | waNT | Delete
	)
	a := ACL{Entries: []ACE{
		{Type: ACEDeny, Flags: fd, Mask: waNT, Who: bob},
		{Type: ACEAllow, Flags: fd, Mask: EntryRights, Who: alice},
		{Type: ACEAllow, Flags: fd, Mask: modify, Who: builders},
	}}
	if m := a.Mode(alice); m != 0o770 {
		t.Fatalf("before the chmod: Mode %03o, want 770", m)
	}
	got := a.WithMode(0o750, alice, true)
	want := []ACE{
		a.Entries[0],
		a.Entries[1],
		{Type: ACEAllow, Flags: fd | ACEInheritOnly, Mask: modify, Who: builders},
		allowOf(rx|Delete, builders),
	}
	if !slices.Equal(got.Entries, want) {
		t.Errorf("chmod 750:\n %+v\nwant\n %+v", got.Entries, want)
	}
	if m := got.Mode(alice); m != 0o750 {
		t.Errorf("after the chmod: Mode %03o, want 750", m)
	}
}

// RFC 19 §9's row (c) for §4.3: with an inherited deny of bob's read and no
// EVERYONE@ allow, chmod o+r appends one EVERYONE@ allow after the deny, so
// bob is still refused read. The merge writes no deny, so the group class
// has the read everyone's allow gives too.
func TestChmodAppendsAfterEveryDeny(t *testing.T) {
	alice, bob := NewUser(), NewUser()
	a := ACL{Entries: []ACE{
		allowOf(ModeRights(6, false), Owner),
		{Type: ACEDeny, Flags: ACEInherited, Mask: ReadData, Who: bob},
	}}
	got := a.WithMode(0o604, alice, false)
	if want := append(slices.Clone(a.Entries), allowOf(ModeRights(4, false), Everyone)); !slices.Equal(got.Entries, want) {
		t.Errorf("chmod o+r:\n %+v\nwant\n %+v", got.Entries, want)
	}
	if walk(got, bob, Everyone)&ReadData != 0 {
		t.Error("bob was granted read past his deny")
	}
	if m := got.Mode(alice); m != 0o644 {
		t.Errorf("after the chmod: Mode %03o, want 644", m)
	}
}

// RFC 19 §4.3: a chmod that widens grants the new bits on a stored ACL a
// mode implied. With no deny after a class's allow, the allow takes them in
// place and the result is the ACL the new mode implies: +x and 644 to 755
// among them. With one, as GROUP@'s in 0604 after the owner's allow, they
// come in a new allow at the end.
func TestChmodWidensAStoredDefaultACL(t *testing.T) {
	o := NewUser()
	for _, dir := range []bool{false, true} {
		for _, c := range []struct{ from, to uint32 }{
			{0o644, 0o744}, {0o644, 0o755}, {0o644, 0o777}, {0o600, 0o640},
		} {
			got := ACLFromMode(c.from, dir).WithMode(c.to, o, dir)
			if want := ACLFromMode(c.to, dir); !slices.Equal(got.Entries, want.Entries) || got.Mode(o) != c.to {
				t.Errorf("chmod %03o of %03o's ACL, dir %v: %+v, Mode %03o; want %+v", c.to, c.from, dir, got.Entries, got.Mode(o), want.Entries)
			}
		}
		got := ACLFromMode(0o604, dir).WithMode(0o704, o, dir)
		if want := append(ACLFromMode(0o604, dir).Entries, allowOf(ModeRights(1, dir), Owner)); !slices.Equal(got.Entries, want) || got.Mode(o) != 0o704 {
			t.Errorf("chmod 704 of 604's ACL, dir %v: %+v, Mode %03o; want %+v", dir, got.Entries, got.Mode(o), want)
		}
	}
	// An allow a child inherits, or one inherited, takes no new bit in place:
	// the bits come in a new allow at the end, so what a child inherits does
	// not widen and inheritance applied again loses nothing.
	for _, f := range []ACEFlags{ACEFileInherit | ACEDirInherit, ACEInherited} {
		e := ACE{Type: ACEAllow, Flags: f, Mask: ModeRights(4, true), Who: Owner}
		got := ACL{Entries: []ACE{e}}.WithMode(0o700, o, true)
		if want := []ACE{e, allowOf(ModeRights(3, true), Owner)}; !slices.Equal(got.Entries, want) {
			t.Errorf("chmod 700 of an allow with flags %#x: %+v; want %+v", f, got.Entries, want)
		}
	}
}

// RFC 19 §4.3: a chmod that narrows limits each class's allow in place to
// its new bits and keeps the minor rights every bit grants: 755 to 700
// leaves the group and the others those alone, and adds no entry.
func TestChmodNarrows(t *testing.T) {
	const minor = Synchronize | ReadACL | ReadAttributes | ReadNamedAttrs
	o := NewUser()
	for _, dir := range []bool{false, true} {
		got := ACLFromMode(0o755, dir).WithMode(0o700, o, dir)
		want := []ACE{allowOf(ModeRights(7, dir), Owner), allowOf(minor, OwnerGroup), allowOf(minor, Everyone)}
		if !slices.Equal(got.Entries, want) || got.Mode(o) != 0o700 {
			t.Errorf("chmod 700 of 755's ACL, dir %v: %+v, want %+v", dir, got.Entries, want)
		}
	}
}

// RFC 19 §4.3: a chmod to any mode keeps every deny, class and named,
// explicit and inherited, as and in the order it was, and writes none.
func TestChmodKeepsDenies(t *testing.T) {
	o, bob := NewUser(), NewUser()
	denies := func(a ACL) []ACE {
		return slices.DeleteFunc(slices.Clone(a.Entries), func(e ACE) bool { return e.Type != ACEDeny })
	}
	a := ACL{Entries: append(ACLFromMode(0o064, false).Entries, // deny OWNER@ ahead of the group's allow
		denyOf(ReadData, bob),
		allowOf(ModeRights(7, false), bob),
		ACE{Type: ACEDeny, Flags: ACEInherited, Mask: Execute, Who: Everyone},
		denyOf(ModeRights(4, false), OwnerGroup),
	)}
	for mode := uint32(0); mode <= 0o777; mode++ {
		if got := a.WithMode(mode, o, false); !slices.Equal(denies(got), denies(a)) {
			t.Fatalf("chmod %03o: denies %+v, want %+v", mode, denies(got), denies(a))
		}
	}
}

// RFC 19 §4.3: chmods that narrow and widen again add no entry. From the
// ACL 644 implies, through 600, 644, 600, 644, 755 and 777, the list stays
// at three entries and each mode is the ACL's; on one holding a class deny
// and a named allow, after a first round of the same chmods no further one
// changes the number of entries.
func TestChmodSequenceDoesNotGrow(t *testing.T) {
	o, bob := NewUser(), NewUser()
	seq := []uint32{0o600, 0o644, 0o600, 0o644, 0o755, 0o777}
	for _, dir := range []bool{false, true} {
		a := ACLFromMode(0o644, dir)
		for _, m := range seq {
			if a = a.WithMode(m, o, dir); len(a.Entries) != 3 || a.Mode(o) != m {
				t.Errorf("chmod %03o, dir %v: %d entries, Mode %03o", m, dir, len(a.Entries), a.Mode(o))
			}
		}
		b := ACL{Entries: append(ACLFromMode(0o604, dir).Entries, allowOf(ReadData, bob))}
		for _, m := range seq {
			b = b.WithMode(m, o, dir)
		}
		n := len(b.Entries)
		for round := 0; round < 3; round++ {
			for _, m := range seq {
				if b = b.WithMode(m, o, dir); len(b.Entries) != n {
					t.Fatalf("round %d, chmod %03o, dir %v: %d entries, want %d", round+2, m, dir, len(b.Entries), n)
				}
			}
		}
	}
}

// RFC 19 §2.8's limit on a list a chmod builds: 64 effective and
// inheritable named allows a chmod narrows split into 128 entries, within
// it, and 65 into 130, past it; an effective and inheritable OWNER@ or
// GROUP@ entry counts two; the audit list is held to the same limit.
func TestFits(t *testing.T) {
	staff, o := NewGroup(), NewUser()
	of := func(n int, who Principal) ACL {
		var a ACL
		for range n {
			a.Entries = append(a.Entries, ACE{Type: ACEAllow, Flags: ACEFileInherit | ACEDirInherit, Mask: ModeRights(7, true), Who: who})
		}
		return a
	}
	if m := of(64, staff).WithMode(0o050, o, true); len(m.Entries) != 128 || !m.Fits() {
		t.Errorf("64 named allows split: %d entries, fits %v", len(m.Entries), m.Fits())
	}
	if m := of(65, staff).WithMode(0o050, o, true); m.Fits() {
		t.Errorf("65 named allows split into %d entries and fit", len(m.Entries))
	}
	for _, who := range []Principal{Owner, OwnerGroup} {
		if !of(64, who).Fits() || of(65, who).Fits() {
			t.Errorf("%v: 64 effective and inheritable entries fit %v, 65 fit %v", who, of(64, who).Fits(), of(65, who).Fits())
		}
	}
	if (ACL{Audit: make([]ACE, 129)}).Fits() {
		t.Error("an audit list of 129 entries fit")
	}
}

// RFC 7 §12.1's chmod merge check.
func TestChmodMerge(t *testing.T) {
	alice, bob := NewUser(), NewUser()
	named := allowOf(ModeRights(6, false), bob)
	inheritOnly := ACE{Type: ACEAllow, Flags: ACEFileInherit | ACEInheritOnly, Mask: ModeRights(7, false), Who: NewGroup()}
	both := ACE{Type: ACEAllow, Flags: ACEDirInherit, Mask: ModeRights(7, false), Who: NewUser()}
	deny := denyOf(Delete, NewUser())
	audit := ACE{Type: ACEAudit, Flags: ACESuccessfulAccess, Mask: ReadData, Who: Everyone}
	a := ACL{Entries: append([]ACE{named, inheritOnly, both, deny}, ACLFromMode(0o777, false).Entries...), Audit: []ACE{audit}}

	got := a.WithMode(0o600, alice, false)
	if m := got.Mode(alice); m != 0o600 {
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
	// The named user is left with no right a mode bit grants beyond the
	// minor ones.
	if es := find(bob, true); len(es) != 1 || es[0].Mask&bitRights != 0 {
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
	if es := find(both.Who, true); len(es) != 1 || es[0].Inheritable() || es[0].Mask&bitRights != 0 {
		t.Errorf("effective copy: %+v", es)
	}
	if !slices.Contains(got.Entries, deny) || !slices.Equal(got.Audit, a.Audit) {
		t.Error("deny or audit entries changed")
	}
	// A chmod adds only what a new bit grants (RFC 19 §4.3): on an ACL that
	// grants the group and the others nothing, a chmod to 600 leaves them
	// neither read the ACL nor synchronize.
	owned := ACL{Entries: []ACE{allowOf(ReadData, Owner)}}.WithMode(0o600, alice, false)
	for name, who := range map[string][]Principal{"a group member": {OwnerGroup, Everyone}, "another user": {Everyone}} {
		if g := walk(owned, who...); g&(ReadACL|Synchronize) != 0 {
			t.Errorf("chmod 600 granted %s %#x", name, g)
		}
	}
	// The group bits bound what a named principal keeps.
	if es := (ACL{Entries: []ACE{named}}).WithMode(0o640, alice, false).Entries[0]; es.Mask&bitRights != ReadData {
		t.Errorf("chmod 640 left the named user %x", es.Mask&bitRights)
	}
}

func TestACLValid(t *testing.T) {
	ok := ACL{Entries: ACLFromMode(0o644, false).Entries, Audit: []ACE{{Type: ACEAlarm, Who: Everyone}}}
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
		{Type: ACEAllow, Flags: ACEFileInherit | ACEDirInherit | ACEInheritOnly, Mask: ModeRights(7, true) | Delete, Who: CreatorOwner},
		{Type: ACEAllow, Flags: ACEFileInherit | ACEDirInherit, Mask: ModeRights(6, true), Who: finance},
		{Type: ACEAllow, Flags: ACEDirInherit | ACENoPropagate, Mask: ModeRights(4, true), Who: staff},
		allowOf(ModeRights(7, true), Everyone), // not inheritable
	}}

	file, ok := parent.Inherit(false, alice, staff, 0o777)
	if !ok {
		t.Fatal("a file inherited nothing")
	}
	want := []ACE{
		{Type: ACEAllow, Flags: ACEInherited, Mask: ModeRights(7, true) | Delete, Who: alice},
		{Type: ACEAllow, Flags: ACEInherited, Mask: ModeRights(6, true), Who: finance},
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

	// A requested 0640 limits as a chmod's merge would and never adds:
	// alice, the new file's owner, keeps the owner bits' read and write,
	// finance the group bits' read; nothing new appears.
	limited, _ := parent.Inherit(false, alice, staff, 0o640)
	for _, e := range limited.Entries {
		want := ReadData
		if e.Who == alice {
			want |= writeSet
		}
		if e.Mask&(ReadData|writeSet|Execute) != want {
			t.Errorf("mode 0640 left %v with %#x, want %#x", e.Who, e.Mask&(ReadData|writeSet|Execute), want)
		}
		if e.Mask&Delete == 0 && e.Who == alice {
			t.Error("the mode took away a right outside read, write and execute")
		}
	}
	if len(limited.Entries) != len(file.Entries) {
		t.Errorf("the mode added or removed entries: %+v", limited.Entries)
	}

	if _, ok := (ACL{Entries: []ACE{{Type: ACEAllow, Flags: ACEDirInherit, Mask: ModeRights(4, true), Who: staff}}}).Inherit(false, alice, staff, 0o644); ok {
		t.Error("a file inherited a directory-only entry")
	}
}
