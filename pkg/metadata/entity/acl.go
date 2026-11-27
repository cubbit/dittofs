package entity

import "slices"

// ACL is the one access-control list a file has. Audit and alarm entries
// are kept apart from the ones that grant and deny: they never decide
// access, they select audit events.
type ACL struct {
	Entries []ACE
	Flags   ACLFlags
	Audit   []ACE
}

type ACE struct {
	Type  ACEType
	Flags ACEFlags
	Mask  AccessMask
	Who   Principal // a user, a group, or Owner, OwnerGroup or Everyone
}

type ACEType uint8

const (
	ACEAllow ACEType = iota + 1
	ACEDeny
	ACEAudit
	ACEAlarm
)

var aceTypes = []string{ACEAllow: "allow", ACEDeny: "deny", ACEAudit: "audit", ACEAlarm: "alarm"}

func (t ACEType) String() string                { return enumString(aceTypes, t) }
func (t ACEType) MarshalText() ([]byte, error)  { return enumMarshal(aceTypes, t) }
func (t *ACEType) UnmarshalText(b []byte) error { return enumUnmarshal(aceTypes, t, b) }

// ACEFlags are the NFSv4 ACE flags, which Windows shares.
type ACEFlags uint32

const (
	ACEFileInherit      ACEFlags = 0x01
	ACEDirInherit       ACEFlags = 0x02
	ACENoPropagate      ACEFlags = 0x04
	ACEInheritOnly      ACEFlags = 0x08
	ACESuccessfulAccess ACEFlags = 0x10
	ACEFailedAccess     ACEFlags = 0x20
	ACEInherited        ACEFlags = 0x80
)

// Effective reports whether the entry governs the file it is on, rather than
// only the children that inherit it.
func (e ACE) Effective() bool { return e.Flags&ACEInheritOnly == 0 }

// Inheritable reports whether children inherit the entry.
func (e ACE) Inheritable() bool { return e.Flags&(ACEFileInherit|ACEDirInherit) != 0 }

type ACLFlags uint8

const (
	ACLProtected ACLFlags = 1 << iota
	ACLAutoInherited
)

// AccessMask holds the rights both protocols grant: NFSv4's ACE mask bits,
// which are Windows' file access rights. It is what Authorize is asked for.
type AccessMask uint32

const (
	ReadData        AccessMask = 0x00000001 // on a directory: list it
	WriteData       AccessMask = 0x00000002 // on a directory: add a file
	AppendData      AccessMask = 0x00000004 // on a directory: add a subdirectory
	ReadNamedAttrs  AccessMask = 0x00000008
	WriteNamedAttrs AccessMask = 0x00000010
	Execute         AccessMask = 0x00000020 // on a directory: traverse it
	DeleteChild     AccessMask = 0x00000040
	ReadAttributes  AccessMask = 0x00000080
	WriteAttributes AccessMask = 0x00000100
	Delete          AccessMask = 0x00010000
	ReadACL         AccessMask = 0x00020000
	WriteACL        AccessMask = 0x00040000
	WriteOwner      AccessMask = 0x00080000
	Synchronize     AccessMask = 0x00100000

	ListDirectory   = ReadData
	AddFile         = WriteData
	AddSubdirectory = AppendData

	// EntryRights is the fourteen rights above, every right an entry or the
	// mode can grant. A generic or maximal-access bit is none of them, and is
	// never granted, privilege included.
	EntryRights = ReadData | WriteData | AppendData | ReadNamedAttrs | WriteNamedAttrs | Execute | DeleteChild |
		ReadAttributes | WriteAttributes | Delete | ReadACL | WriteACL | WriteOwner | Synchronize

	// SystemSecurity reads or writes the audit list. It is no entry right:
	// it is granted to an administrator alone, never for privilege,
	// ownership or an entry.
	SystemSecurity AccessMask = 0x01000000
)

// What a mode bit grants (RFC 19 §4.1): any bit grants the minor rights
// every read, write and execute set carries; beyond them read grants read
// data, write the write set and execute execute.
const (
	minorRights = Synchronize | ReadACL | ReadAttributes | ReadNamedAttrs
	writeSet    = WriteData | AppendData | WriteNamedAttrs | WriteAttributes
)

// ModeRights is R(bits), the rights three permission bits grant whoever
// they apply to, owner or not, on a directory when dir (RFC 19 §4.1): the
// minor rights with any bit, read data for read, the write set for write,
// with delete a child on a directory, and execute for execute. No bit
// grants nothing.
func ModeRights(bits uint32, dir bool) AccessMask {
	var m AccessMask
	if bits&4 != 0 {
		m |= ReadData
	}
	if bits&2 != 0 {
		m |= writeSet
		if dir {
			m |= DeleteChild
		}
	}
	if bits&1 != 0 {
		m |= Execute
	}
	if m != 0 {
		m |= minorRights
	}
	return m
}

// bits is the three permission bits a granted mask implies (RFC 19 §4.2):
// read from read data, write from write data, execute from execute.
func bits(m AccessMask) uint32 {
	var b uint32
	if m&ReadData != 0 {
		b |= 4
	}
	if m&WriteData != 0 {
		b |= 2
	}
	if m&Execute != 0 {
		b |= 1
	}
	return b
}

// ACLFromMode is the ACL a file with no stored ACL is evaluated against and
// shown as, a directory when dir (RFC 19 §4.1). With u, g and o the rights
// R the owner, group and other bits grant, it is, in this order and each
// left out when its mask is empty: deny OWNER@ g|o minus u, allow OWNER@ u,
// deny GROUP@ o minus g, allow GROUP@ g, allow EVERYONE@ o. A deny appears
// only where a later class has a bit an earlier one lacks, as in 0604, so
// the owner gets the owner bits and a group member the group bits, nothing
// a later class adds. The owner's implicit rights are no entry: the walk
// grants them (RFC 19 §3.1, step 5).
func ACLFromMode(mode uint32, dir bool) ACL {
	u, g, o := ModeRights(mode>>6&7, dir), ModeRights(mode>>3&7, dir), ModeRights(mode&7, dir)
	var out ACL
	for _, e := range []ACE{
		{Type: ACEDeny, Mask: (g | o) &^ u, Who: Owner},
		{Type: ACEAllow, Mask: u, Who: Owner},
		{Type: ACEDeny, Mask: o &^ g, Who: OwnerGroup},
		{Type: ACEAllow, Mask: g, Who: OwnerGroup},
		{Type: ACEAllow, Mask: o, Who: Everyone},
	} {
		if e.Mask != 0 {
			out.Entries = append(out.Entries, e)
		}
	}
	return out
}

// Mode is the nine permission bits a implies on a file owner owns (RFC 19
// §4.2): each class's bits are those of what the walk of the effective
// entries grants it, the owner's implicit rights left out.
//   - The owner class is a caller who is owner, in no group.
//   - The group class is the union over GROUP@ and every named principal an
//     effective entry names other than owner, each a caller matched by that
//     principal alone: the mask POSIX ACLs show in the group bits.
//   - The other class is the union over three strangers: an authenticated
//     one, an anonymous one and one with an asserted identity.
//
// Every caller is matched by EVERYONE@ and NETWORK too. Setuid, setgid and
// sticky are not in an ACL: the caller keeps them from the File.
func (a ACL) Mode(owner Principal) uint32 {
	u := a.grants(Owner, OwnerRights, owner)
	g := a.grants(OwnerGroup)
	for _, e := range a.Entries {
		if e.Effective() && e.Who != owner && e.Who.named() {
			g |= a.grants(e.Who)
		}
	}
	o := a.grants(AuthenticatedUsers, BuiltinUsers) | a.grants(AnonymousPrincipal) | a.grants()
	return bits(u)<<6 | bits(g)<<3 | bits(o)
}

// grants is what the walk (RFC 19 §3.3) of a's effective entries grants a
// caller matched by EVERYONE@, NETWORK and the principals who, each right
// decided by the first entry naming it.
func (a ACL) grants(who ...Principal) AccessMask {
	var allow, decided AccessMask
	for _, e := range a.Entries {
		if !e.Effective() || e.Who != Everyone && e.Who != Network && !slices.Contains(who, e.Who) {
			continue
		}
		if e.Type == ACEAllow {
			allow |= e.Mask &^ decided
		}
		decided |= e.Mask
	}
	return allow
}

// named reports a named principal (RFC 19 §2.3): a user, a group, root or
// Administrators, each matching only some callers.
func (p Principal) named() bool {
	return p.Kind != PrincipalSpecial || p == RootPrincipal || p == Administrators
}

// everyoneLike reports a principal of RFC 19 §2.3's other class, matching
// callers whatever their memberships.
func (p Principal) everyoneLike() bool {
	return p == Everyone || p == AuthenticatedUsers || p == BuiltinUsers || p == Network || p == AnonymousPrincipal
}

// WithMode is chmod's merge into a stored ACL on a file owner owns, a
// directory when dir (RFC 19 §4.3). It moves no entry and writes no deny:
//   - Every effective allow is limited to the bits mode gives its class
//     (limited). One both effective and inheritable that the limit narrows
//     is split in place into an unchanged inherit-only copy and the limited
//     effective one.
//   - Inherit-only, deny, audit and alarm entries are left as they are.
//   - For each class, the bits mode sets that the limited ACL does not
//     already imply for it are granted, R of those bits, by one allow naming
//     OWNER@, GROUP@ or EVERYONE@ after every deny (grant). GROUP@ takes none
//     while an effective named allow not naming owner holds the group class,
//     whose bits are then the mask.
//
// A chmod to the ACL's own mode therefore changes what it grants nobody. The
// caller stores the result's Mode(owner), and refuses a result that does not
// Fit.
func (a ACL) WithMode(mode uint32, owner Principal, dir bool) ACL {
	out := ACL{Flags: a.Flags, Audit: slices.Clone(a.Audit)}
	masked := false
	for _, e := range a.Entries {
		limited := e.Mask
		if e.Type == ACEAllow && e.Effective() {
			masked = masked || e.Who != owner && e.Who.named()
			limited = e.limited(mode, owner, dir)
		}
		if limited == e.Mask {
			out.Entries = append(out.Entries, e)
			continue
		}
		if e.Inheritable() {
			kept := e
			kept.Flags |= ACEInheritOnly
			out.Entries = append(out.Entries, kept)
			e.Flags &^= ACEFileInherit | ACEDirInherit | ACENoPropagate
		}
		e.Mask = limited
		out.Entries = append(out.Entries, e)
	}
	has := out.Mode(owner)
	for _, c := range []struct {
		who   Principal
		shift uint
	}{{Owner, 6}, {OwnerGroup, 3}, {Everyone, 0}} {
		if add := mode >> c.shift & 7 &^ (has >> c.shift); add != 0 && (c.who != OwnerGroup || !masked) {
			out.grant(c.who, ModeRights(add, dir))
		}
	}
	return out
}

// limited is e's mask limited to the bits mode gives its class on a file
// owner owns (RFC 19 §4.3): the everyone-like principals the other bits,
// even one that is owner; OWNER@, owner rights and owner the owner bits;
// GROUP@ and the other named principals the group bits. It removes only the
// rights of R a missing bit withholds, never the minor rights, and keeps
// every other right. A principal of no class matches no caller and keeps
// its mask.
func (e ACE) limited(mode uint32, owner Principal, dir bool) AccessMask {
	var shift uint
	switch w := e.Who; {
	case w.everyoneLike():
		shift = 0
	case w == Owner || w == OwnerRights || w == owner:
		shift = 6
	case w == OwnerGroup || w.named():
		shift = 3
	default:
		return e.Mask
	}
	return e.Mask &^ (ModeRights(7, dir) &^ ModeRights(mode>>shift&7, dir) &^ minorRights)
}

// grant gives who the rights m after every deny (RFC 19 §4.3). It adds them
// to the last effective allow naming who, neither inheritable nor
// inherited, that no effective deny follows, which then grants exactly what
// an allow appended at the end would, so a chmod that narrows and then
// widens again adds no entry; with no such allow it appends one.
func (a *ACL) grant(who Principal, m AccessMask) {
	for i := len(a.Entries) - 1; i >= 0; i-- {
		e := &a.Entries[i]
		if !e.Effective() {
			continue
		}
		if e.Type == ACEDeny {
			break
		}
		if e.Who == who && !e.Inheritable() && e.Flags&ACEInherited == 0 {
			e.Mask |= m
			return
		}
	}
	a.Entries = append(a.Entries, ACE{Type: ACEAllow, Mask: m, Who: who})
}

// Fits reports whether each list is within RFC 19 §2.8's limit, counted as
// a security descriptor spells it: at most 128 entries, an effective and
// inheritable OWNER@ or GROUP@ entry counting two. The limit's 64 KiB bound
// is never the one reached first: 128 entries naming the longest SID, 68
// bytes, spell under 10 KiB.
func (a ACL) Fits() bool {
	for _, es := range [][]ACE{a.Entries, a.Audit} {
		n := len(es)
		for _, e := range es {
			if (e.Who == Owner || e.Who == OwnerGroup) && e.Effective() && e.Inheritable() {
				n++
			}
		}
		if n > 128 {
			return false
		}
	}
	return true
}

// Inherit is the ACL a new file, or a new directory when dir, created under a
// directory holding a, gets, as RFC 8881 §6.4.3 and MS-DTYP 2.5.3.4 derive
// it; ok is false when nothing is inherited, and the new file's mode then
// governs with no stored ACL.
//   - A new file gets an effective copy of each file-inherit entry.
//   - A new directory gets an effective copy of each directory-inherit
//     entry, which, unless it is no-propagate, it also keeps for its own
//     children; a file-inherit entry passes through it inherit-only.
//   - In an effective copy CREATOR OWNER and CREATOR GROUP become owner and
//     group; the copy that propagates keeps them, inherit-only.
//   - mode then applies as an and, limiting each effective allow to the
//     bits mode gives its class as chmod's merge would (limited), never
//     adding an entry or a right.
//
// Audit and alarm entries are inherited by the same rules, unlimited.
func (a ACL) Inherit(dir bool, owner, group Principal, mode uint32) (ACL, bool) {
	out := ACL{
		Entries: inherit(a.Entries, dir, owner, group),
		Audit:   inherit(a.Audit, dir, owner, group),
	}
	if len(out.Entries) == 0 && len(out.Audit) == 0 {
		return ACL{}, false
	}
	for i, e := range out.Entries {
		if e.Effective() && e.Type == ACEAllow {
			out.Entries[i].Mask = e.limited(mode, owner, dir)
		}
	}
	return out, true
}

func inherit(es []ACE, dir bool, owner, group Principal) []ACE {
	const flow = ACEFileInherit | ACEDirInherit | ACENoPropagate | ACEInheritOnly
	var out []ACE
	for _, e := range es {
		file, sub := e.Flags&ACEFileInherit != 0, e.Flags&ACEDirInherit != 0
		propagates := dir && e.Flags&ACENoPropagate == 0
		effective := (!dir && file) || (dir && sub)
		if effective {
			c := e
			c.Flags = c.Flags&^flow | ACEInherited
			switch c.Who {
			case CreatorOwner:
				c.Who = owner
			case CreatorGroup:
				c.Who = group
			}
			out = append(out, c)
		}
		if propagates && (file || sub) {
			c := e
			c.Flags = c.Flags | ACEInheritOnly | ACEInherited
			out = append(out, c)
		}
	}
	return out
}

// Stored is the ACL as the store keeps it: an inheritable entry naming
// CREATOR OWNER or CREATOR GROUP is inherit-only, since those match no caller
// and act only through inheritance, which turns them into effective entries
// for the new file's owner or group.
func (a ACL) Stored() ACL {
	out := ACL{Flags: a.Flags, Entries: append([]ACE(nil), a.Entries...), Audit: append([]ACE(nil), a.Audit...)}
	for i, e := range out.Entries {
		if e.Who.IsCreator() && e.Inheritable() {
			out.Entries[i].Flags |= ACEInheritOnly
		}
	}
	return out
}

// LimitsOwner reports an effective entry naming owner rights: the owner then
// gets what such entries grant, without the rights ownership implies.
func (a ACL) LimitsOwner() bool {
	for _, e := range a.Entries {
		if e.Effective() && e.Who == OwnerRights {
			return true
		}
	}
	return false
}

// Valid reports whether every entry is of the list it sits in: allow and
// deny in Entries, audit and alarm in Audit, each naming a principal. A deny
// naming a special principal that is not well known, or naming root, is
// invalid: no caller matches it, so every walk would skip it and it would
// fail open. An allow
// naming one is valid and grants nothing.
func (a ACL) Valid() bool {
	for _, e := range a.Entries {
		if (e.Type != ACEAllow && e.Type != ACEDeny) || e.Who.IsZero() {
			return false
		}
		// decision: a deny naming root is refused like an unknown one, since no
		// access check matches root until resolution maps a claimed uid 0 to
		// it. Accept it once the check matches root's callers.
		if e.Type == ACEDeny && e.Who.Kind == PrincipalSpecial && (!e.Who.WellKnown() || e.Who == RootPrincipal) {
			return false
		}
	}
	for _, e := range a.Audit {
		if (e.Type != ACEAudit && e.Type != ACEAlarm) || e.Who.IsZero() {
			return false
		}
	}
	return true
}
