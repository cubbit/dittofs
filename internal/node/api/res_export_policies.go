package api

import (
	"context"
	"net/netip"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// policyDoc is an export policy as its PUT and PATCH carry it: its spec
// alone.
type policyDoc struct {
	Spec apiv1.ExportPolicySpec `json:"spec"`
}

// policyOf is p on the wire, its lists empty rather than absent.
//
// ponytail: the wire carries no anonymous principal, so a squashed caller
// always becomes the anonymous one, the store's default, and a PUT writes
// it back as that. Add the field when an operator needs to squash callers to
// an account of their own.
func policyOf(p entity.ExportPolicy) apiv1.ExportPolicySpec {
	v := apiv1.ExportPolicySpec{Flavors: []string{}, MinProof: p.MinProof.String(), MinProtection: p.MinProtection.String(),
		ReadOnly: p.ReadOnly, Clients: []apiv1.ClientRule{}, Squash: p.Squash.String(),
		SMB: apiv1.SMBShare{Encrypt: p.SMB&entity.SMBEncrypt != 0, Hidden: p.SMB&entity.SMBHidden != 0,
			ContinuouslyAvailable: p.SMB&entity.SMBContinuouslyAvailable != 0}}
	for _, f := range p.Flavors {
		v.Flavors = append(v.Flavors, f.String())
	}
	for _, c := range p.Clients {
		r := apiv1.ClientRule{Netgroup: c.Match.Netgroup, Access: c.Access.String()}
		if c.Match.CIDR.IsValid() {
			r.CIDR = c.Match.CIDR.String()
		}
		v.Clients = append(v.Clients, r)
	}
	return v
}

// policyFrom is spec as the store holds share's export policy, a value that
// does not parse refused naming its field below at. At least one flavour is
// required, and none is defaulted. Whether a rule names exactly one match,
// and whether the minimums can be met together, is the store's to refuse.
//
// ponytail: a client rule names a netgroup by its name, the only key the
// store holds a netgroup under, and a rule naming one that does not exist
// fails closed. Name it by an ID once netgroups carry one.
func policyFrom(share entity.ShareID, spec apiv1.ExportPolicySpec, at string) (entity.ExportPolicy, error) {
	p := entity.ExportPolicy{Share: share, ReadOnly: spec.ReadOnly}
	if len(spec.Flavors) == 0 {
		return p, &fieldError{field: at + "flavors", detail: "a policy names at least one flavour: none, sys or kerberos"}
	}
	p.Flavors = make([]entity.AuthFlavor, len(spec.Flavors))
	for i, f := range spec.Flavors {
		if p.Flavors[i].UnmarshalText([]byte(f)) != nil {
			return p, &fieldError{field: at + "flavors", detail: "not a flavour: none, sys or kerberos"}
		}
	}
	for _, e := range []struct {
		field, value, want string
		into               interface{ UnmarshalText([]byte) error }
	}{
		{"min_proof", spec.MinProof, "none, claimed, secret or ticket", &p.MinProof},
		{"min_protection", spec.MinProtection, "none, integrity or privacy", &p.MinProtection},
		{"squash", spec.Squash, "none, root or all", &p.Squash},
	} {
		if e.into.UnmarshalText([]byte(e.value)) != nil {
			return p, &fieldError{field: at + e.field, detail: "not one of " + e.want}
		}
	}
	for _, c := range spec.Clients {
		r := entity.ClientRule{Match: entity.ClientMatch{Netgroup: c.Netgroup}}
		if c.CIDR != "" {
			cidr, err := netip.ParsePrefix(c.CIDR)
			if err != nil {
				return p, &fieldError{field: at + "clients", detail: "not a CIDR: " + c.CIDR}
			}
			r.Match.CIDR = cidr
		}
		if r.Access.UnmarshalText([]byte(c.Access)) != nil {
			return p, &fieldError{field: at + "clients", detail: "not an access: none, read or read-write"}
		}
		p.Clients = append(p.Clients, r)
	}
	if spec.SMB.Encrypt {
		p.SMB |= entity.SMBEncrypt
	}
	if spec.SMB.Hidden {
		p.SMB |= entity.SMBHidden
	}
	if spec.SMB.ContinuouslyAvailable {
		p.SMB |= entity.SMBContinuouslyAvailable
	}
	return p, nil
}

// policies is what the write adapter knows of an export policy: one of the
// share's records, so its writes are audited as the share's, with a tag of
// its own, its version, apart from the share's generation.
var policies = writing[policyDoc]{kind: entity.ResourceShare, read: func(tx entity.ControlPlaneTx, id [16]byte) (policyDoc, uint64, bool, error) {
	p, ok, err := tx.ExportPolicy(entity.ShareID(id))
	if err != nil || !ok {
		return policyDoc{}, 0, false, err
	}
	return policyDoc{policyOf(p)}, p.Version, true, nil
}}

// putPolicy is PUT and PATCH /shares/{id}/export-policy: the policy replaced
// whole, under the version If-Match names. A share being removed or retired
// is refused stale by the store.
func putPolicy(_ context.Context, rq request[policyDoc], _ struct{}, tx entity.ControlPlaneTx) (policyDoc, [16]byte, error) {
	p, err := policyFrom(entity.ShareID(rq.ID), rq.Body.Spec, "spec.")
	if err != nil {
		return policyDoc{}, rq.ID, err
	}
	_, err = tx.PutExportPolicy(p, rq.Expect)
	return rq.Body, rq.ID, err
}

// exportPolicy is GET /shares/{id}/export-policy, its version the ETag, 404
// for a share that does not exist; ?wait= returns once the version changes.
var exportPolicy = watching[apiv1.ExportPolicy]{
	read: func(ctx context.Context, d Deps, rq request[none]) (apiv1.ExportPolicy, error) {
		_, p, err := d.Store.Share(ctx, entity.ShareID(rq.ID))
		return apiv1.ExportPolicy{ID: idString(rq.ID), Generation: p.Version, Spec: policyOf(p)}, err
	},
	tag: func(p apiv1.ExportPolicy) uint64 { return p.Generation },
	until: changes(func(first, now apiv1.ExportPolicy) bool {
		return first.Generation != now.Generation
	}),
}
