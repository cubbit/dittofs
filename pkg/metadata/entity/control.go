package entity

import (
	"context"
	"errors"
	"iter"
	"net/netip"
	"time"
)

// User and Group are principals with a name. A file refers to them only by
// Principal, never by name, so renaming one touches no file, and deleting one
// leaves its principal in ACLs as an unknown principal. A machine principal
// is a User record of kind PrincipalMachine: no Name, one Kerberos spelling,
// and no Primary, Password or NTHash.
type User struct {
	Principal Principal
	Name      string
	IDs       ProtocolIDs // each also a protocol-ID index row
	Primary   Principal   // the group a new file gets; zero: the creator's own principal
	Disabled  bool
	Password  SecretRef // the password hash, sealed under the storage key; zero for an account with no password
	NTHash    SecretRef // the NT hash, sealed under the protocol key; zero while NTLM is off, for a directory principal and for an API-only account
	Email     string    // optional, never defaulted, and never a spelling: nothing looks a user up by it
	// Role, NotBefore and MustChange are the management API's: they grant
	// nothing on files.
	Role       APIRole
	NotBefore  time.Time // store time, in whole seconds: a credential issued before it is refused
	MustChange bool      // set with a generated password: until it changes, sign-in permits only that change
	Generation uint64    // raised by every write of the record; the management API's entity tag
	Position   uint64    // the principals generation its last change raised
	Deleted    bool      // marked by its deletion: found by no name or ID and admitted nowhere while its rows are dropped
}

// APIRole is a user's role in the management API, in increasing order.
type APIRole uint8

const (
	RoleNone APIRole = iota
	RoleViewer
	RoleOperator
	RoleAdmin
)

var apiRoles = []string{RoleNone: "none", RoleViewer: "viewer", RoleOperator: "operator", RoleAdmin: "admin"}

func (r APIRole) String() string                { return enumString(apiRoles, r) }
func (r APIRole) MarshalText() ([]byte, error)  { return enumMarshal(apiRoles, r) }
func (r *APIRole) UnmarshalText(b []byte) error { return enumUnmarshal(apiRoles, r, b) }

type Group struct {
	Principal  Principal
	Name       string
	IDs        ProtocolIDs
	Generation uint64 // raised by every write of the record, as a user's is
	Position   uint64 // the principals generation its last change raised
	Deleted    bool   // marked by its deletion: its memberships grant nothing while its rows are dropped
}

// Membership is one member of one group, its own record in both directions,
// so a large group is never a list on the group.
type Membership struct {
	Group  Principal
	Member Principal // a user or a nested group
}

// ProtocolID is one protocol's spelling of a principal. Value is the
// spelling the scheme's index row holds: decimal for UID and GID, the
// binary form for a SID, and "name@REALM" as the KDC spells it.
type ProtocolID struct {
	Scheme ProtocolScheme
	Value  string
	// Linked is the directory user SID a krb spelling was linked for, in
	// its binary form as a sid spelling's Value is, empty for none. A string
	// keeps ProtocolID comparable: == means the same scheme, value and link.
	// Two spellings are one index row when their Scheme and Value agree,
	// whatever their Linked.
	//
	// decision: the SID is a field of the spelling, stored in the record and
	// in the spelling's index row, rather than a record of its own, because
	// sign-in reads it through the user record it reads anyway and its
	// writer, the link, writes that record. The store refuses it on any
	// scheme but krb, as no reader looks for it there. Make it its own
	// record if a reader ever needs it without the user's record, or a
	// second scheme needs a link.
	Linked string
}

type ProtocolIDs []ProtocolID

// ProtocolScheme's codes are the protocol-ID index's scheme component.
type ProtocolScheme uint8

const (
	SchemeUID ProtocolScheme = iota + 1
	SchemeGID
	SchemeSID
	SchemeKerberos
	SchemeDir // a directory entry's immutable ID
)

var protocolSchemes = []string{SchemeUID: "uid", SchemeGID: "gid", SchemeSID: "sid", SchemeKerberos: "krb", SchemeDir: "dir"}

func (s ProtocolScheme) String() string                { return enumString(protocolSchemes, s) }
func (s ProtocolScheme) MarshalText() ([]byte, error)  { return enumMarshal(protocolSchemes, s) }
func (s *ProtocolScheme) UnmarshalText(b []byte) error { return enumUnmarshal(protocolSchemes, s, b) }

// Share is one exported filesystem. Name is what an SMB client tree-connects
// to; Path is its place in the NFS namespace. Both are indexed and unique,
// both may change, and neither appears in a handle, which carries ID.
type Share struct {
	ID   ShareID
	Name string
	Path string // absolute and normalised: "/photos", "/home/alice"
	Root FileID
	// Fold, Names and DigestKey are bound when the share is created and never
	// change: changing either rule would re-fold or refuse names already
	// stored, and a new key would reorder every listing and void its cookies.
	Fold      FoldRule
	Names     NameRule
	DigestKey [DigestKeyLen]byte
	// Tag is the share's journal tag, minted from the installation's tag
	// counter when the share arrives and never given to another share; a
	// share put never changes it. Zero names no share.
	Tag uint32
	// Shard is the share's one shard, drawn from the installation's shard
	// counter when the share is created and never given to another share:
	// its own record, not part of the one a share put writes, as a share put
	// never changes it. Every file of the share is in it.
	Shard ShardID
	// State is its own record, not part of the one a share put writes: a cut
	// and a deletion change it as well as the management API.
	State ShareState
	// Namespaces lists the block namespaces the share's content is counted
	// in, each with its generation: one outside a re-home, two during one.
	// Write is the generation new puts carve under. Both are their own
	// record, written at create and then only by a re-home or an import,
	// never by a share put.
	Namespaces []NamespaceGen
	Write      uint32
	// Generation is raised by every share put: the info record's, not the
	// state's or the namespaces'.
	Generation uint64
	// GrantVersion is the share's grant-list version, its own record, moved
	// by every grant set or deleted on the share and never by a share put. A
	// cache of the share's grants keys on it, since a deleted grant has no
	// version left of its own.
	GrantVersion uint64
	// Position is the share list version its last create, rename, path
	// change or state change raised.
	Position uint64
}

type ShareState uint8

const (
	ShareEnabled ShareState = iota + 1
	ShareQuiesced
	ShareRemoving
	ShareRetired
)

var shareStates = []string{
	ShareEnabled: "enabled", ShareQuiesced: "quiesced", ShareRemoving: "removing", ShareRetired: "retired",
}

func (s ShareState) String() string                { return enumString(shareStates, s) }
func (s ShareState) MarshalText() ([]byte, error)  { return enumMarshal(shareStates, s) }
func (s *ShareState) UnmarshalText(b []byte) error { return enumUnmarshal(shareStates, s, b) }

// TagState is how far a share's removal has gone, as the engine's start reads
// it from the removal's per-shard progress (RFC 16 §2.3.2).
type TagState uint8

const (
	// TagServing is a share with no removal, or one some shard has not taken
	// past its offload step.
	TagServing TagState = iota + 1
	// TagOffloaded is a removal every shard has taken to offloaded or later,
	// and not every shard to forgotten.
	TagOffloaded
	// TagForgotten is a removal every shard has taken to forgotten, step 4 or
	// done.
	TagForgotten
)

// TagRecord is one share record's journal tag and how far the share's
// removal has gone.
type TagRecord struct {
	Tag   uint32
	State TagState
}

// SnapshotDisposition is what a share's removal does with its snapshots:
// refuse to start while any exist, the default; delete them; or detach them,
// keeping what they see after the share is gone.
type SnapshotDisposition uint8

const (
	SnapshotsRefuse SnapshotDisposition = iota
	SnapshotsDelete
	SnapshotsDetach
)

var snapshotDispositions = []string{SnapshotsRefuse: "refuse", SnapshotsDelete: "delete", SnapshotsDetach: "detach"}

func (d SnapshotDisposition) String() string { return enumString(snapshotDispositions, d) }
func (d SnapshotDisposition) MarshalText() ([]byte, error) {
	return enumMarshal(snapshotDispositions, d)
}
func (d *SnapshotDisposition) UnmarshalText(b []byte) error {
	return enumUnmarshal(snapshotDispositions, d, b)
}

// ShareGrant is one principal's access to one share, evaluated on every call
// before any file ACL.
type ShareGrant struct {
	Share     ShareID
	Principal Principal
	Access    ShareAccess
	Version   uint64 // raised by every change; the authorisation cache key carries it
}

// ShareAccess is ordered: each level includes the ones below it.
type ShareAccess uint8

const (
	AccessNone ShareAccess = iota
	AccessRead
	AccessReadWrite
	AccessAdmin
)

var shareAccesses = []string{AccessNone: "none", AccessRead: "read", AccessReadWrite: "read-write", AccessAdmin: "admin"}

func (a ShareAccess) String() string                { return enumString(shareAccesses, a) }
func (a ShareAccess) MarshalText() ([]byte, error)  { return enumMarshal(shareAccesses, a) }
func (a *ShareAccess) UnmarshalText(b []byte) error { return enumUnmarshal(shareAccesses, a, b) }

// ExportPolicy is the share's admission and identity-mapping rule, one per
// share, applied by the filesystem service on every call.
//
// The minimums are stored as such, never derived from Flavors, which lists
// what SECINFO answers and admits nothing: a Kerberos-only share that refuses
// NTLM and one that refuses only AUTH_SYS list the same flavours.
type ExportPolicy struct {
	Share         ShareID
	Flavors       []AuthFlavor // SECINFO's answer, in preference order; admission never reads it
	MinProof      ProofKind    // how strongly every caller must prove who it is
	MinProtection Protection   // the signing or sealing every caller's messages need
	ReadOnly      bool         // every caller is held to read-only, whatever its grant
	Clients       []ClientRule // first match decides; no match refuses. Empty admits every client
	Squash        Squash
	Anonymous     Principal // what a squashed caller becomes
	SMB           SMBShareFlags
	Version       uint64 // raised by every change; the authorisation cache key carries it
}

// SharePolicy is a share's identity policy as resolution reads it, in one
// snapshot, so a squash, an anonymous principal and the versions a cache keys
// on never come from two different commits. ShareIndex.Share reads the share
// and its export policy in one view, and the share's Share.GrantVersion is
// the grant version of that view; resolution builds SharePolicy from the two
// through ExportPolicy.SharePolicy, never field by field.
type SharePolicy struct {
	Share         ShareID
	Squash        Squash
	Anonymous     Principal
	PolicyVersion uint64 // ExportPolicy.Version
	GrantVersion  uint64 // the share's grant version row, S‖id‖gv
}

// SharePolicy is the share's identity policy at grantVersion, which the
// caller read in the same view as p.
func (p ExportPolicy) SharePolicy(grantVersion uint64) SharePolicy {
	return SharePolicy{
		Share:         p.Share,
		Squash:        p.Squash,
		Anonymous:     p.Anonymous,
		PolicyVersion: p.Version,
		GrantVersion:  grantVersion,
	}
}

// Valid reports whether the policy's minimums can be met together. A
// protection floor needs a verified proof: a caller allowed in on a claim, or
// on none, carries no protection, so a share admitting it with a floor
// protects nothing, while the floor still refuses an honest Kerberos caller.
func (p ExportPolicy) Valid() bool {
	return p.MinProof.String() != "" && p.MinProtection.String() != "" &&
		(p.MinProtection == ProtectionNone || p.MinProof >= ProofSecret)
}

type AuthFlavor uint8

const (
	FlavorNone AuthFlavor = iota + 1
	FlavorSys
	FlavorKerberos
)

var authFlavors = []string{FlavorNone: "none", FlavorSys: "sys", FlavorKerberos: "kerberos"}

func (f AuthFlavor) String() string                { return enumString(authFlavors, f) }
func (f AuthFlavor) MarshalText() ([]byte, error)  { return enumMarshal(authFlavors, f) }
func (f *AuthFlavor) UnmarshalText(b []byte) error { return enumUnmarshal(authFlavors, f, b) }

// ProofKind is how strongly a caller proved who it is, in increasing order.
type ProofKind uint8

const (
	ProofNone    ProofKind = iota // anonymous or guest
	ProofClaimed                  // ids asserted, nothing verified: AUTH_SYS
	ProofSecret                   // a shared-secret challenge: NTLM
	ProofTicket                   // a third party's mutual proof: Kerberos
)

var proofs = []string{ProofNone: "none", ProofClaimed: "claimed", ProofSecret: "secret", ProofTicket: "ticket"}

func (p ProofKind) String() string                { return enumString(proofs, p) }
func (p ProofKind) MarshalText() ([]byte, error)  { return enumMarshal(proofs, p) }
func (p *ProofKind) UnmarshalText(b []byte) error { return enumUnmarshal(proofs, p, b) }

// Protection is how a caller's messages are protected, in increasing order.
type Protection uint8

const (
	ProtectionNone      Protection = iota
	ProtectionIntegrity            // every message signed
	ProtectionPrivacy              // every message sealed
)

var protections = []string{ProtectionNone: "none", ProtectionIntegrity: "integrity", ProtectionPrivacy: "privacy"}

func (p Protection) String() string                { return enumString(protections, p) }
func (p Protection) MarshalText() ([]byte, error)  { return enumMarshal(protections, p) }
func (p *Protection) UnmarshalText(b []byte) error { return enumUnmarshal(protections, p, b) }

type Squash uint8

const (
	SquashNone Squash = iota
	SquashRoot
	SquashAll
)

var squashes = []string{SquashNone: "none", SquashRoot: "root", SquashAll: "all"}

func (s Squash) String() string                { return enumString(squashes, s) }
func (s Squash) MarshalText() ([]byte, error)  { return enumMarshal(squashes, s) }
func (s *Squash) UnmarshalText(b []byte) error { return enumUnmarshal(squashes, s, b) }

type SMBShareFlags uint8

const (
	SMBEncrypt SMBShareFlags = 1 << iota
	SMBHidden                // left out of share enumeration
	SMBContinuouslyAvailable
)

// ClientRule admits or refuses clients by address range or netgroup, and may
// narrow the share grant for them.
type ClientRule struct {
	Match  ClientMatch
	Access ShareAccess // none, read or read-write: never wider than the grant
}

// ClientMatch is a CIDR or a netgroup name, exactly one of them.
type ClientMatch struct {
	CIDR     netip.Prefix
	Netgroup string
}

// Valid reports whether the rule names exactly one match and an access a
// client rule may give.
func (r ClientRule) Valid() bool {
	return r.Match.CIDR.IsValid() != (r.Match.Netgroup != "") && r.Access <= AccessReadWrite
}

// Netgroup is a named set of hosts referenced by name from client rules. A
// rule naming a netgroup that does not exist fails closed: an allow rule
// matches no host, and a rule of access none matches every host, so deleting
// a netgroup an early deny names cannot let its hosts reach a later, broader
// allow.
type Netgroup struct {
	Name    string
	Members []string // host names, addresses or CIDRs
	Version uint64
}

// Installation is the installation's identity, one record, minted by the
// explicit initialisation step and never changed.
type Installation struct {
	ID [16]byte
	// Instance is minted afresh whenever a start finds the platform's
	// machine-generation identifier differs from Generation, so a cloned
	// image is a new instance.
	Instance   [16]byte
	Generation []byte // empty where the platform offers none
	// NTLMOff is what turning identity.ntlm back on reads: a write turning it
	// off sets Off and clears Deleted, and the NT-hash deletion's last step
	// sets Deleted. Turning it on is refused while Off is set and Deleted is
	// zero.
	NTLMOff struct {
		Off     uint64      // the settings generation of the last write turning identity.ntlm off; zero if none has
		Deleted OperationID // the NT-hash deletion completed since that write; zero while none has
	}
	// MachinePassword names the computer account's password, a secret owned
	// by this record under "machine_password"; zero until a join first seals
	// a version, which a failed join keeps.
	MachinePassword SecretRef
	// SigningKey names the management API's access-token signing key, a
	// secret owned by this record under "signing_key"; zero until the first
	// version is sealed.
	SigningKey SecretRef
	// Join is the machine account's state, written by the domain join's and
	// the rotation's steps; zero while no join has run.
	Join MachineJoin
}

// MachineJoin is the machine account's state, outliving the operations that
// write it and a restart.
type MachineJoin struct {
	Joined     bool
	Account    string   // the computer account's name, with its $
	Owner      []byte   // the computer object's owner, a binary SID, as the join read it
	Delegation []byte   // its msDS-AllowedToActOnBehalfOfOtherIdentity as read; empty when unset
	Names      []string // the account's service names with sPNMappings applied
	// LastRotation is the last join's or rotation's outcome. A failed
	// rotation moves At and Failed and leaves Version and Confirmed as they
	// were.
	LastRotation struct {
		At        time.Time // store time
		Failed    bool      // cleared only by a rotation that completed
		Tag       uint32    // the key version number of the sealed version
		Read      uint32    // the number the confirm read; differs from Tag on a reported mismatch
		Version   uint64    // the secret version the last join or rotation confirmed
		Confirmed time.Time // the store time of that confirm
	}
}

// ShareList is raised in the transaction of every share create, delete,
// rename, path change and state change: the pseudo-filesystem's change
// attribute.
type ShareList struct {
	Version uint64
}

// Setting is one configuration record at one scope.
type Setting struct {
	Scope      SettingScope
	Key        string
	Value      []byte // secrets appear only as references
	Generation uint64 // raised by every change
	Schema     uint32 // a reader that does not know it refuses the record
	Position   uint64 // the settings generation its last change raised
}

// SettingScope is one of the five scopes and, below installation, the ID of
// the node, namespace, remote store or share it is the scope of.
type SettingScope struct {
	Kind ScopeKind
	ID   [16]byte // zero at installation scope
}

// SettingKey is the cursor of a cross-scope settings read: the scope and key
// of the last setting a page returned.
type SettingKey struct {
	Scope SettingScope
	Key   string
}

// SettingsReader is the one read settings.Read makes of the store.
type SettingsReader interface {
	// AllSettings lists every scope's settings in key order, resuming after
	// after; the zero key starts from the first. A row that does not decode
	// is yielded with an error wrapping ErrBadRow, and the listing goes on.
	// Resume only after a pair whose error is nil: a bad row's Setting may be
	// zero. A bad row is not an absent setting, and the caller must not treat
	// it as one.
	AllSettings(ctx context.Context, after SettingKey) iter.Seq2[Setting, error]
}

// ErrBadRow is a setting row that does not decode. An error wrapping it also
// wraps the cause, and its Setting carries the scope and key when the row's
// key parses.
var ErrBadRow = errors.New("entity: a setting row does not decode")

type ScopeKind uint8

const (
	ScopeInstallation ScopeKind = iota + 1
	ScopeNode
	ScopeNamespace
	ScopeRemote
	ScopeShare
)

var scopeKinds = []string{
	ScopeInstallation: "installation", ScopeNode: "node", ScopeNamespace: "namespace",
	ScopeRemote: "remote", ScopeShare: "share",
}

func (k ScopeKind) String() string                { return enumString(scopeKinds, k) }
func (k ScopeKind) MarshalText() ([]byte, error)  { return enumMarshal(scopeKinds, k) }
func (k *ScopeKind) UnmarshalText(b []byte) error { return enumUnmarshal(scopeKinds, k, b) }

// Usage is what a share, a principal or a project holds: logical bytes,
// holes excluded and shared content counted in full, and files.
type Usage struct {
	Bytes, Files int64
}

// Quota is one share's limits on a user or group in it, or on a tree, a
// project. Limits are written by the management API and usage is counted,
// apart, because they have different writers. The store keeps them and
// when usage first went above Soft; the filesystem service enforces them.
type Quota struct {
	Share     ShareID
	Principal Principal     // a user or group quota; zero otherwise
	Project   uint64        // a tree quota; zero otherwise
	Hard      Limit         // refused beyond it
	Soft      Limit         // allowed beyond it for Grace, then refused as Hard
	Grace     time.Duration // how long usage may stay above Soft
	Advisory  Limit         // never refused: crossing it only emits an event
	Version   uint64        // raised by every change
}

// Limit is a byte and a file count; zero in either means no limit on it.
type Limit struct {
	Bytes, Files int64
}

// Unlimited reports a quota that limits nothing, which is no quota at all.
func (q Quota) Unlimited() bool {
	return q.Hard == Limit{} && q.Soft == Limit{} && q.Advisory == Limit{}
}

// SecretRef names a sealed credential; the zero ref names none.
type SecretRef [16]byte
