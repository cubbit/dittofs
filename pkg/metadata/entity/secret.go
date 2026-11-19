package entity

// Secret is one credential: a password hash, an NT hash, a key. The value
// is envelope-encrypted under a key held outside the KV, so a copy of the
// store alone reveals nothing. Referenced by SecretRef from User and Setting.
// Each secret kind is sealed under the wrapping key of the role that needs it
// (RFC 13 §7); every node holds both keys.
type Secret struct {
	ID   SecretRef
	Kind SecretKind
	// Version is raised by every rotation; the previous version stays
	// resolvable until every node reports this one, a keytab's and a machine
	// password's for their retention, the API signing key's until its last
	// token expires (RFC 13 §7).
	Version uint64
	// KeyID names the wrapping key that sealed it: one per role or kind.
	KeyID string
	// Owner is the record or setting key whose field names it. A new version
	// keeps both Kind and Owner, and a read whose reference comes from
	// another owner or expects another kind is refused, so a reference
	// cannot be pointed at another holder's secret (RFC 16 §2.3).
	Owner SecretOwner
	Value []byte // sealed
	// KeyVersion is a machine password's key version number, set when sealed
	// and never changed; zero for every other kind.
	KeyVersion uint32
	// HandedOver marks a machine password an offline join was handed: it
	// derives no Kerberos key and backs no previous-version fallback. Written
	// with the version, bound into its sealed value's additional data, never
	// changed.
	HandedOver bool
}

// SecretOwner is the one holder a secret belongs to: a record's field (a
// user's password hash and NT hash, under the keys "password" and "nt_hash";
// the installation's signing key and machine password, under "signing_key"
// and "machine_password") or a setting's key (a keytab, the directory's bind
// credential, an OpenID Connect client's secret). Encoded with the version,
// as the Secret's other fields are, and compared whole.
type SecretOwner struct {
	Record ResourceRef  // the record's kind and ID; zero for a setting
	Scope  SettingScope // the setting's scope; zero for a record
	Key    string       // the record's field name, or the setting's key
}

// SecretKind is what a secret holds. Its value is stored, so a new kind is
// appended and none is renumbered; the zero kind names none. A kind is
// declared only once RFC 13 §7 gives it a wrapping-key role, so the node
// credential and the OpenID Connect client secret wait for theirs.
type SecretKind uint8

const (
	SecretPassword            SecretKind = iota + 1 // a slow password hash
	SecretNTHash                                    // only with NTLM enabled
	SecretKeytab                                    // a Kerberos keytab
	SecretMachinePassword                           // the computer account's password
	SecretRemoteCredential                          // a remote store's credential
	SecretKey                                       // a master-key reference or the API signing key
	SecretDirectoryCredential                       // the directory's bind password
)

var secretKinds = []string{
	SecretPassword:            "password",
	SecretNTHash:              "nt-hash",
	SecretKeytab:              "keytab",
	SecretMachinePassword:     "machine password",
	SecretRemoteCredential:    "remote credential",
	SecretKey:                 "key",
	SecretDirectoryCredential: "directory credential",
}

func (k SecretKind) String() string {
	return enumString(secretKinds, k)
}

func (k SecretKind) MarshalText() ([]byte, error) {
	return enumMarshal(secretKinds, k)
}

func (k *SecretKind) UnmarshalText(b []byte) error {
	return enumUnmarshal(secretKinds, k, b)
}
