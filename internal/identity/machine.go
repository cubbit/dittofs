package identity

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// machineName reports whether a Kerberos name's user part names a machine
// (RFC 18 §2.4): one with a service component, such as host/build01 or
// nfs/build01, or a machine account's, which ends in $.
func machineName(user string) bool {
	return strings.Contains(user, "/") || strings.HasSuffix(user, "$")
}

// mintMachine is the principal holding the krb row name, user@REALM,
// minting one through the mint path when none does: a User record of kind
// machine holding only that spelling, linked for linked, the binary form of
// the PAC's user SID, or for none when empty. The plan looks the row up on
// every run, so a run whose Create another sign-in beat binds whoever took
// the row.
//
// decision: machines are minted on sight, users are not (RFC 18 §2.4). A
// machine needs a principal of its own for state protection to mean
// anything, and minting one grants no file access, since it acts as
// anonymous and no grant can name it. Revisit if a KDC is shown to issue
// machine tickets to names the operator does not control;
// identity_mints_total{kind="machine"} is the signal.
//
// decision: the mint runs after the ticket, its PAC and the PAC's SIDs are
// checked, but before the sign-in is accepted, so a sign-in can still be
// refused after minting: the minted record read back failing, gone or
// disabled, and, when another sign-in's mint took the row first, the PAC's
// user SID not the one that mint linked. Each leaves a record a ticket for
// the name would have minted anyway, linked only for a SID a verified PAC
// carried. Overturned if a refusal that a later sign-in of the same machine
// would not also meet can follow the mint.
//
// decision: the mint goes through identity's one mint path, with this plan,
// so the re-run on ErrExist, the refusal of a re-run that binds nothing and
// identity_mints_total are the path's, never a second copy here. Overturned
// if a machine's mint ever needs a write the path does not make.
func (s *Server) mintMachine(ctx context.Context, name, linked string) (entity.Principal, error) {
	var p entity.Principal
	err := s.mint(ctx, func(ctx context.Context) (entity.MintBatch, int, error) {
		got, err := s.deps.Store.Principals().ByProtocolID(ctx, entity.ProtocolID{Scheme: entity.SchemeKerberos, Value: name})
		switch {
		case err == nil:
			p = got
			return entity.MintBatch{}, 1, nil
		case !merrs.IsNotFoundError(err):
			return entity.MintBatch{}, 0, fmt.Errorf("%w: krb row: %w", vfserr.ErrUnavailable, err)
		}
		p = entity.Principal{Kind: entity.PrincipalMachine, ID: entity.NewPrincipalID()}
		if s.beforeMachineMint != nil {
			s.beforeMachineMint()
		}
		return entity.MintBatch{Users: []entity.MintUser{{User: entity.User{Principal: p,
			IDs: entity.ProtocolIDs{{Scheme: entity.SchemeKerberos, Value: name, Linked: linked}}}}}}, 0, nil
	})
	return p, err
}

// linkMachine links machine p's krb row name for sd, the binary form of a
// verified PAC's user SID, unless the row is already linked, and returns the
// SID the row is linked for afterwards: sd when this call linked it, the
// winner's when another call did first, and none when the record or its row
// is gone. The write is one transaction that reads the record, so two calls
// racing to link the row link it once.
//
// decision: a machine minted without a PAC is linked by its first verified
// PAC, the one write a Kerberos sign-in makes to a record it did not mint.
// Without it the check binding a PAC to its record refuses the machine's
// every PAC-bearing ticket, and nothing else writes a machine's link, so it
// would be locked out for good.
// Its ceiling: whoever first presents a verified PAC under the machine's name
// claims the record, so a computer account deleted and recreated under the
// name before that first sign-in inherits the machine. It is bounded to
// machine records, to a row linked for no SID and to the window before the
// first verified PAC, and the KDC issues a ticket under a machine's name only
// to the account holding that name's key. Overturned if a realm is seen
// issuing PAC-less and PAC-bearing tickets for one machine name to different
// accounts, which would leave an operator's link the only way to set it.
func (s *Server) linkMachine(ctx context.Context, p entity.Principal, name string, sd []byte) (string, error) {
	if s.beforeMachineLink != nil {
		s.beforeMachineLink()
	}
	if s.deps.ControlPlane == nil {
		return "", fmt.Errorf("%w: identity: no control plane to link a machine", vfserr.ErrUnavailable)
	}
	var linked string
	err := s.deps.ControlPlane.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		linked = ""
		u, ok, err := tx.User(p)
		if err != nil || !ok {
			return err
		}
		i := slices.IndexFunc(u.IDs, func(id entity.ProtocolID) bool { return id.Scheme == entity.SchemeKerberos && id.Value == name })
		if i < 0 {
			return nil
		}
		if linked = u.IDs[i].Linked; linked != "" {
			return nil
		}
		u.IDs = slices.Clone(u.IDs)
		u.IDs[i].Linked = string(sd)
		_, err = tx.PutUser(u, u.Generation, entity.Alloc{})
		linked = string(sd)
		return err
	})
	var se *merrs.StoreError
	switch {
	case errors.As(err, &se) && se.Code == merrs.ErrInvalidArgument:
		// The store refuses the PAC's SID as a link: a refusal of the PAC,
		// which no retry clears, not the store's fault.
		return "", logonFailure("link a machine: %v", err)
	case err != nil:
		return "", fmt.Errorf("%w: link a machine: %w", vfserr.ErrUnavailable, err)
	}
	return linked, nil
}

// kerberosAccount is p's current record as Account reads it, a machine's
// included, which Account refuses: a machine signs in over Kerberos only.
func (s *Server) kerberosAccount(ctx context.Context, p entity.Principal) (entity.User, error) {
	if p.Kind != entity.PrincipalUser && p.Kind != entity.PrincipalMachine {
		return entity.User{}, vfserr.ErrLogonFailure
	}
	return s.record(ctx, p)
}
