package api

import (
	"context"
	"net/netip"
	"slices"
	"strconv"

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
// required, none is defaulted, and none may be named twice; an empty minimum is none, and an empty
// squash root. The client rules must be stated, an empty list admitting every
// client. A rule naming other than exactly one match, and minimums that
// cannot be met together, are refused naming their field here, ahead of the
// store's own check. A client rule names a netgroup by its name, the key the
// store holds it under, and a rule naming one that does not exist fails
// closed.
func policyFrom(share entity.ShareID, spec apiv1.ExportPolicySpec, at string) (entity.ExportPolicy, error) {
	p := entity.ExportPolicy{Share: share, ReadOnly: spec.ReadOnly, Squash: entity.SquashRoot}
	if len(spec.Flavors) == 0 {
		return p, &fieldError{field: at + "flavors", detail: "a policy names at least one flavour: none, sys or kerberos"}
	}
	p.Flavors = make([]entity.AuthFlavor, len(spec.Flavors))
	for i, f := range spec.Flavors {
		if p.Flavors[i].UnmarshalText([]byte(f)) != nil {
			return p, &fieldError{field: at + "flavors", detail: "not a flavour: none, sys or kerberos"}
		}
		if slices.Contains(p.Flavors[:i], p.Flavors[i]) {
			return p, &fieldError{field: at + "flavors", detail: "names " + f + " twice"}
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
		if e.value != "" && e.into.UnmarshalText([]byte(e.value)) != nil {
			return p, &fieldError{field: at + e.field, detail: "not one of " + e.want}
		}
	}
	if !p.Valid() {
		return p, &fieldError{field: at + "min_protection", detail: "a protection above none needs a min_proof of secret or ticket"}
	}
	if spec.Clients == nil {
		return p, &fieldError{field: at + "clients", detail: "a policy states its client rules; [] admits every client"}
	}
	for i, c := range spec.Clients {
		field := at + "clients[" + strconv.Itoa(i) + "]"
		r := entity.ClientRule{Match: entity.ClientMatch{Netgroup: c.Netgroup}}
		if c.CIDR != "" {
			cidr, err := netip.ParsePrefix(c.CIDR)
			if err != nil {
				return p, &fieldError{field: field, detail: "not a CIDR: " + c.CIDR}
			}
			r.Match.CIDR = cidr
		}
		if r.Access.UnmarshalText([]byte(c.Access)) != nil {
			return p, &fieldError{field: field, detail: "not an access: none, read or read-write"}
		}
		if !r.Valid() {
			return p, &fieldError{field: field, detail: "a rule names exactly one of cidr or netgroup"}
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
// its own, its version, apart from the share's generation. Its audit
// record's before and after are the policy's versions, not the share's
// generations, and its change names the policy's fields under
// export_policy, as the share's create records them. A share with no policy
// stored has the default one, at version zero, so a PUT naming "0" replaces
// it; a PUT under If-None-Match: * is 412, as the policy always exists.
var policies = writing[policyDoc]{kind: entity.ResourceShare, auditAs: "export_policy", read: func(tx entity.ControlPlaneTx, id [16]byte, _ string) (policyDoc, uint64, bool, error) {
	_, ok, err := tx.Share(entity.ShareID(id))
	if err != nil || !ok {
		return policyDoc{}, 0, false, err
	}
	p, err := tx.ExportPolicy(entity.ShareID(id))
	return policyDoc{policyOf(p)}, p.Version, err == nil, err
}}

// putPolicy is PUT and PATCH /shares/{id}/export-policy: the policy replaced
// whole, under the version If-Match names, answered as its GET reads it. A
// share being removed or retired is refused stale by the store.
func putPolicy(_ context.Context, rq request[policyDoc], _ struct{}, tx entity.ControlPlaneTx) (apiv1.ExportPolicy, [16]byte, error) {
	p, err := policyFrom(entity.ShareID(rq.ID), rq.Body.Spec, "spec.")
	if err != nil {
		return apiv1.ExportPolicy{}, rq.ID, err
	}
	v, err := tx.PutExportPolicy(p, rq.Expect)
	return apiv1.ExportPolicy{ID: idString(rq.ID), Generation: v, Spec: policyOf(p)}, rq.ID, err
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
