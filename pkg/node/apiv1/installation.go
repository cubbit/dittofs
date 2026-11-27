package apiv1

import "time"

// Installation is the installation as a resource: its ID, and what the
// system records of it. An operator declares nothing of it through the API.
type Installation = Resource[struct{}, InstallationStatus]

// InstallationStatus is the installation's state: its domain join, and
// what turning identity.ntlm back on reads.
type InstallationStatus struct {
	Join    Join    `json:"join"`
	NTLMOff NTLMOff `json:"ntlm_off"`
}

// Join is the machine account's state, which outlives the operations that
// wrote it: whether the installation is joined, the computer account's name
// with its $, its service names, and the last join's or rotation's outcome,
// absent while none has run.
type Join struct {
	Joined       bool      `json:"joined"`
	Account      string    `json:"account,omitempty"`
	Names        []string  `json:"names,omitempty"`
	LastRotation *Rotation `json:"last_rotation,omitempty"`
}

// Rotation is a join's or a rotation's outcome: when it ran, whether it
// failed, the machine password's secret version it confirmed and when.
type Rotation struct {
	At        time.Time `json:"at"`
	Failed    bool      `json:"failed"`
	Version   uint64    `json:"version"`
	Confirmed time.Time `json:"confirmed"`
}

// NTLMOff is the settings generation of the last write turning
// identity.ntlm off, zero if none has, and the NT-hash deletion completed
// since, absent while none has. Turning NTLM on is refused while Off is set
// and Deleted is absent.
type NTLMOff struct {
	Off     uint64 `json:"off"`
	Deleted string `json:"deleted,omitempty"`
}

// JoinMode is how a domain join takes its computer account.
type JoinMode string

// The join modes: online creates the computer account as an administrator;
// offline takes one an administrator created, with its password.
const (
	JoinOnline  JoinMode = "online"
	JoinOffline JoinMode = "offline"
)

// JoinDomain is the body of POST /installation:join-domain: the join's
// spec, and the credential it takes, which no record keeps: an online join's
// administrator, handed to identity in memory, or an offline join's computer
// account password, sealed as a version of the machine password.
type JoinDomain struct {
	Spec       JoinDomainSpec `json:"spec"`
	Credential JoinCredential `json:"credential"`
}

// JoinCredential is a directory account's name and password: an online
// join's administrator, its name a distinguished name or a UPN; an offline
// join's computer account password, with no name. The password is base64
// in JSON.
type JoinCredential struct {
	User     string `json:"user,omitempty"`
	Password []byte `json:"password"`
}

// JoinDomainSpec is a domain join as a request states it: its mode; the
// computer account's name, with or without its $, empty for identity's
// default; whether it takes over a joined installation, and for an online
// join an account of that name it did not create; and an online join's
// organisational unit, a distinguished name under the directory record's
// base, and its DNS host name, each empty for identity's default.
type JoinDomainSpec struct {
	Mode        JoinMode `json:"mode"`
	Account     string   `json:"account,omitempty"`
	OU          string   `json:"ou,omitempty"`
	DNSHostName string   `json:"dns_host_name,omitempty"`
	Takeover    bool     `json:"takeover,omitempty"`
}

// JoinDomainParams is a domain join as its operation records it, and as
// identity's steps decode it: its spec, and an offline join's machine
// password secret version, which the route writes. It holds no credential.
type JoinDomainParams struct {
	JoinDomainSpec
	Version uint64 `json:"version,omitempty"`
}

// ResolvePrincipalParams is the body of POST
// /installation:resolve-principal, and its operation's parameters: a
// directory principal by name or by SID in its string form, exactly one of
// them. The name is its qualified spelling, DOMAIN\name with DOMAIN the
// directory's NetBIOS domain, as identity's Find takes it in the qualified
// scheme, its domain compared ignoring the case of ASCII letters; neither a
// UPN nor a Kerberos name is taken.
type ResolvePrincipalParams struct {
	Name string `json:"name,omitempty"`
	SID  string `json:"sid,omitempty"`
}
