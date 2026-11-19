package directory

import (
	"fmt"
	"strings"

	"github.com/go-ldap/ldap/v3"
)

// filter is the RFC 4515 filter that finds what holds value under by, every
// value escaped: text by go-ldap's EscapeFilter, a binary value as \xx for
// each byte. An empty value gives "", which no entry holds.
func (r Record) filter(by Key, value []byte) (string, error) {
	if len(value) == 0 {
		return "", nil
	}
	user, group := "user", "group"
	if !r.IsAD() {
		user, group = "posixAccount", "posixGroup"
	}
	switch by {
	case ByUserName:
		return "(&(objectClass=" + user + ")(" + r.UserAttr() + "=" + ldap.EscapeFilter(string(value)) + "))", nil
	case ByGroupName:
		return "(&(objectClass=" + group + ")(" + r.GroupAttr() + "=" + ldap.EscapeFilter(string(value)) + "))", nil
	case BySID:
		return "(objectSid=" + escapeBinary(value) + ")", nil
	case ByEntryID:
		if r.IsAD() {
			return "(objectGUID=" + escapeBinary(value) + ")", nil
		}
		return "(entryUUID=" + ldap.EscapeFilter(string(value)) + ")", nil
	}
	return "", fmt.Errorf("identity.directory: no lookup by key %d", by)
}

// escapeBinary is v with every byte a \xx escape (RFC 4515 §3).
func escapeBinary(v []byte) string {
	var b strings.Builder
	for _, c := range v {
		fmt.Fprintf(&b, `\%02x`, c)
	}
	return b.String()
}

// entryIDAttr is the attribute holding an entry's immutable ID.
func (r Record) entryIDAttr() string {
	if r.IsAD() {
		return "objectGUID"
	}
	return "entryUUID"
}

// attrs is the attribute list of a "who holds this" question.
//
// ponytail: NT hashes are never read from the directory, so this list names
// no attribute holding one (unicodePwd, ntPwdHistory, a Samba-style schema's
// sambaNTPassword). Reading one needs a privileged bind, an NT hash is
// equivalent to the password, and NT hashes exist only while identity.ntlm
// is on. Add it when a deployment needs NTLM for directory users without a
// domain controller, with a sealed bind credential that may read only that
// attribute and the hash held only in memory.
func (r Record) attrs() []string {
	a := []string{"objectClass", r.UserAttr(), "objectSid", r.entryIDAttr(), "mail", "uidNumber", "gidNumber"}
	if r.GroupAttr() != r.UserAttr() {
		a = append(a, r.GroupAttr())
	}
	if r.IsAD() {
		a = append(a, "userAccountControl", "accountExpires")
	}
	return a
}
