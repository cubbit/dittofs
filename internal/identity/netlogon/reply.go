package netlogon

import (
	"context"
	"errors"
	"fmt"

	"github.com/oiweiwei/go-msrpc/dcerpc"
	"github.com/oiweiwei/go-msrpc/msrpc/dtyp"
	logon "github.com/oiweiwei/go-msrpc/msrpc/nrpc/logon/v1"
	"github.com/oiweiwei/go-msrpc/ndr"

	"github.com/cubbit/dittofs/pkg/auth/sid"
)

// maxSubAuthorities is the most sub-authorities a SID holds (MS-DTYP 2.4.2).
const maxSubAuthorities = 15

// replyFrom is the Reply a SamInfo4 answers: the user and each domain group
// under LogonDomainID, then each extra SID, the user session key's blocks in
// order. It checks nothing a sign-in decides on: a missing LogonDomainID
// leaves the user and the domain groups the zero SID, and a missing or short
// session key is kept as it came. A SID longer than MS-DTYP allows is
// refused, and so is a LogonDomainID with no room left for a RID.
func replyFrom(v *logon.ValidationSAMInfo4) (Reply, error) {
	domain, err := toSID(v.LogonDomainID)
	if err != nil {
		return Reply{}, fmt.Errorf("netlogon: LogonDomainId: %w", err)
	}
	if domain.SubAuthorityCount >= maxSubAuthorities {
		return Reply{}, fmt.Errorf("netlogon: a LogonDomainId of %d sub-authorities leaves no room for a RID", domain.SubAuthorityCount)
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

// decoded runs f and answers a panic in it as an error: go-msrpc's NDR
// decoder panics on a reply whose pointers alias values of different types,
// which a controller sends only in error but no node should crash on.
func decoded(f func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("netlogon: undecodable reply: %v", r)
		}
	}()
	return f()
}

// request and response are a go-msrpc call's request and response types.
type request interface {
	OpNum() int
	OpName() string
	MarshalNDR(context.Context, ndr.Writer) error
}

type response interface {
	UnmarshalNDR(context.Context, ndr.Reader) error
}

// invoke sends req on cc and decodes the reply into resp, every call this
// package makes going through it in place of go-msrpc's generated client
// method. go-msrpc decodes a reply on a goroutine of its own (dcerpc.NewBody),
// where no recover of the caller's reaches, so the decode runs under decoded
// inside the operation that goroutine calls. A refused call is not an error
// here: it is resp's Return, which the caller reads.
func invoke(ctx context.Context, cc dcerpc.Conn, req request, resp response) error {
	op := &guardedOp{req: req, resp: resp, done: make(chan struct{})}
	if err := cc.Invoke(ctx, op); err != nil {
		return err
	}
	// Invoke returned nil, so it started the decode and closed its input on
	// return: the decode ends, and resp is ours once it has.
	<-op.done
	return op.err
}

// guardedOp is the go-msrpc operation invoke sends: req's request out, and
// the reply decoded into resp under decoded, its outcome in err once done is
// closed.
type guardedOp struct {
	req  request
	resp response
	done chan struct{}
	err  error
}

func (o *guardedOp) OpNum() int     { return o.req.OpNum() }
func (o *guardedOp) OpName() string { return o.req.OpName() }

func (o *guardedOp) MarshalNDRRequest(ctx context.Context, w ndr.Writer) error {
	return o.req.MarshalNDR(ctx, w)
}

func (o *guardedOp) UnmarshalNDRResponse(ctx context.Context, r ndr.Reader) error {
	defer close(o.done)
	o.err = decoded(func() error { return o.resp.UnmarshalNDR(ctx, r) })
	return o.err
}

// The server's halves are never called on a client's operation.
func (o *guardedOp) UnmarshalNDRRequest(context.Context, ndr.Reader) error {
	return errors.ErrUnsupported
}

func (o *guardedOp) MarshalNDRResponse(context.Context, ndr.Writer) error {
	return errors.ErrUnsupported
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
