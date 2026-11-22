package node

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/settings"
)

// stepIdentity names identity's start step and its part.
const stepIdentity = "identity"

// installation is the installation's settings scope.
var installation = entity.SettingScope{Kind: entity.ScopeInstallation}

// applyIdentity hands identity a State: the principals generation of c, and
// the identity and identity.directory sections and the keytab's versions
// from snap, the pass's settings walk at c, and returns the records it
// refused.
//
// decision: before identity holds a State, a section the walk refuses, a row
// that does not decode, holds a newer schema or fails the section's checks,
// fails the start, since a refused setting is never replaced by a default
// (RFC 13 §6). Its ceiling: a node rolled back below a newer writer, or one
// whose store accepted a row unchecked, does not start, and the row is
// repaired in the store by hand, as the API is not serving. Overturned once
// identity can be left unapplied, answering unavailable, while the API
// repairs the row.
//
// decision: once identity holds a State, a refused section, or a State
// Apply refuses, never holds a principal change back: each refused row is
// reported under its own key with the settings generation it was read at,
// and identity is applied the new principals generation with the last
// section identity accepted in place of the refused one, and the last
// keytab in place of one that does not read; when Apply refuses the State,
// with both last sections and the last sections' keytab. So identity runs on
// its last good settings while the API repairs the row, and a disable still
// reaches it. Its ceiling: the last section stands in whole, so a refused
// row holds back every other row of its section, and those rows read applied
// through the API while they do not run, as the walk's decode stops at the
// first refused row and leaves no value for the rest; and a refusal the
// section's Validate made names no row, so it is reported under the
// section's prefix, which the API matches to no row, and the section's rows
// read applied. Overturned by a walk that decodes a section with its
// refused rows dropped and Validate naming the field it refuses, or if a
// refused setting must ever stop sign-in outright.
func (n *Node) applyIdentity(ctx context.Context, c kvstore.Counters, snap *settings.Snapshot) ([]refusal, error) {
	node := n.cfg.Bootstrap.Node.ID
	last := n.identity.Applied()
	var refusals []refusal
	// refuse reports the rows err refuses, or key when it names none, or is
	// err itself while identity holds no State.
	refuse := func(key string, err error) error {
		if last == nil {
			return err
		}
		refusals = refusedRows(refusals, key, c.Settings, err)
		return nil
	}
	st := identity.State{Generation: c.Principals}
	var err error
	if st.Settings, err = identity.SettingsOf(snap, node); err != nil {
		if err := refuse(identity.SectionName, err); err != nil {
			return nil, err
		}
		st.Settings = last.Settings
	}
	if st.Directory, err = identity.DirectoryOf(snap, node); err != nil {
		if err := refuse(identity.DirectorySectionName, err); err != nil {
			return nil, err
		}
		st.Directory = last.Directory
	}
	if st.Keytab, err = n.keytab(ctx, st.Settings.Keytab); err != nil {
		if err := refuse(identity.KeytabKey, err); err != nil {
			return nil, err
		}
		st.Settings.Keytab, st.Keytab = last.Settings.Keytab, last.Keytab
	}
	n.handing(st)
	if err := n.identity.Apply(st); err != nil {
		if last == nil {
			return nil, err
		}
		// Both sections passed their Validate in the walk, so Apply refuses
		// only the NetBIOS name equal to the directory's NetBIOS domain: the
		// rows reported are each side that moved, both when both did.
		for _, key := range identity.NetBIOSMoved(st, *last) {
			refusals = refusedRows(refusals, key, c.Settings, err)
		}
		// The keytab the last identity section names; read already, or kept,
		// when it names the one st does.
		if st.Settings.Keytab != last.Settings.Keytab {
			if st.Keytab, err = n.keytab(ctx, last.Settings.Keytab); err != nil {
				refusals = refusedRows(refusals, identity.KeytabKey, c.Settings, err)
				st.Keytab = last.Keytab
			}
		}
		st.Settings, st.Directory = last.Settings, last.Directory
		n.handing(st)
		if err := n.identity.Apply(st); err != nil {
			return refusals, err
		}
	}
	return refusals, nil
}

// handing calls the reconciler's handing hook, when set, with st.
func (n *Node) handing(st identity.State) {
	if n.rec.handing != nil {
		n.rec.handing(st)
	}
}

// keytab is every version of the keytab ref names still held, opened,
// current first; none for the zero ref.
//
// ponytail: a keytab read that fails is refused until the settings or the
// principals generation next moves, a store error among them, as the provider's error
// does not tell a store error from a version that does not open. Retry it on
// reconcile's backoff once the two can be told apart.
func (n *Node) keytab(ctx context.Context, ref entity.SecretRef) ([]secrets.Version, error) {
	if ref == (entity.SecretRef{}) {
		return nil, nil
	}
	return n.secrets.Versions(ctx, secrets.Protocol, entity.SecretKeytab, entity.SecretOwner{Scope: installation, Key: identity.KeytabKey}, ref)
}

// refusedRows is refusals with each row err refuses added, or key when err
// names none, at settings generation gen.
func refusedRows(refusals []refusal, key string, gen uint64, err error) []refusal {
	keys := []string{key}
	if rows := settings.RefusedRows(err); len(rows) > 0 {
		keys = keys[:0]
		for _, r := range rows {
			keys = append(keys, r.Key)
		}
	}
	for _, k := range keys {
		refusals = append(refusals, settingRefused(k, gen, err))
	}
	return refusals
}

// settingRefused is the refusal of the installation setting key at settings
// generation gen, waiting for the record to change.
func settingRefused(key string, gen uint64, err error) refusal {
	p := apiv1.Problem{Status: http.StatusUnprocessableEntity, Title: "the setting was refused", Detail: err.Error()}
	b, _ := json.Marshal(p) // a Problem holds strings, ints and bools, which always marshal
	return refusal{rec: entity.RefusedRecord{Record: entity.SettingRef(installation, key), Generation: gen, Error: b}}
}

// identitySecrets is the secret versions identity holds, as node status
// reports them: the keytab's current version while one is in force.
func (n *Node) identitySecrets() []entity.SecretVersion {
	ref, v, _ := n.identity.AppliedKeytab()
	if v == 0 {
		return nil
	}
	return []entity.SecretVersion{{Secret: ref, Version: v}}
}

// identityConditions is identity's conditions as the health evaluator reads
// them, each of the node's scope naming this node, since identity's name no
// node.
type identityConditions struct{ n *Node }

func (c identityConditions) Conditions(dst []health.Condition) []health.Condition {
	from := len(dst)
	dst = c.n.identity.Conditions(dst)
	for i := from; i < len(dst); i++ {
		if dst[i].Subject.Scope == health.ScopeNode {
			dst[i].Subject.ID = c.n.subject().ID
		}
	}
	return dst
}

// startIdentity starts identity once readRecords has applied its first
// State, and stops it with the node.
func (n *Node) startIdentity(ctx context.Context) error {
	if err := n.identity.Start(ctx); err != nil {
		return err
	}
	n.started(stepIdentity, n.identity.Stop)
	return nil
}
