package netlogon

import (
	"fmt"

	"github.com/oiweiwei/go-msrpc/msrpc/dtyp"
	logon "github.com/oiweiwei/go-msrpc/msrpc/nrpc/logon/v1"

	"github.com/cubbit/dittofs/pkg/auth/sid"
)

// maxSubAuthorities is the most sub-authorities a SID holds (MS-DTYP 2.4.2).
const maxSubAuthorities = 15

// replyFrom is the Reply a SamInfo4 answers: the user and each domain group
// under LogonDomainID, then each extra SID, the user session key's blocks in
// order. It checks nothing a sign-in decides on: a missing LogonDomainID
// leaves the user and the domain groups the zero SID, and a missing or short
// session key is kept as it came. A SID longer than MS-DTYP allows is refused.
func replyFrom(v *logon.ValidationSAMInfo4) (Reply, error) {
	domain, err := toSID(v.LogonDomainID)
	if err != nil {
		return Reply{}, fmt.Errorf("netlogon: LogonDomainId: %w", err)
	}
	rep := Reply{UserFlags: v.UserFlags, LogonDomain: domain, User: withRID(domain, v.UserID)}
	for _, g := range v.GroupIDs {
		if g != nil {
			rep.Groups = append(rep.Groups, SIDAttr{SID: withRID(domain, g.RelativeID), Attrs: g.Attributes})
		}
	}
	for _, x := range v.ExtraSIDs {
		if x == nil {
			continue
		}
		s, err := toSID(x.SID)
		if err != nil {
			return Reply{}, fmt.Errorf("netlogon: ExtraSids: %w", err)
		}
		rep.Groups = append(rep.Groups, SIDAttr{SID: s, Attrs: x.Attributes})
	}
	if v.UserSessionKey != nil {
		for _, b := range v.UserSessionKey.Data {
			if b != nil {
				rep.SessionKey = append(rep.SessionKey, b.Data...)
			}
		}
	}
	return rep, nil
}

// decoded runs f, which decodes a controller's reply with go-msrpc, and
// answers a panic in it as an error: go-msrpc's NDR decoder panics on a reply
// whose pointers alias values of different types, which a controller sends
// only in error but no node should crash on.
func decoded(f func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("netlogon: undecodable reply: %v", r)
		}
	}()
	return f()
}

// toSID is s as a sid.SID; nil is the zero SID.
func toSID(s *dtyp.SID) (sid.SID, error) {
	if s == nil {
		return sid.SID{}, nil
	}
	if len(s.SubAuthority) > maxSubAuthorities {
		return sid.SID{}, fmt.Errorf("a SID of %d sub-authorities, more than %d", len(s.SubAuthority), maxSubAuthorities)
	}
	out := sid.SID{Revision: s.Revision, SubAuthorityCount: uint8(len(s.SubAuthority)),
		SubAuthorities: append([]uint32(nil), s.SubAuthority...)}
	if s.IDAuthority != nil {
		copy(out.IdentifierAuthority[:], s.IDAuthority.Value)
	}
	return out, nil
}

// withRID is domain with rid appended; the zero SID for the zero domain.
func withRID(domain sid.SID, rid uint32) sid.SID {
	if domain.SubAuthorityCount == 0 && domain.Revision == 0 {
		return sid.SID{}
	}
	subs := append(append([]uint32(nil), domain.SubAuthorities...), rid)
	return sid.SID{Revision: domain.Revision, SubAuthorityCount: uint8(len(subs)),
		IdentifierAuthority: domain.IdentifierAuthority, SubAuthorities: subs}
}
