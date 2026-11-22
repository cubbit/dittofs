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

const (
	// stepIdentity names identity's start step and its part.
	stepIdentity = "identity"
	// The identity sections' prefixes and the keytab's key, as the node
	// reports a refusal of each.
	identitySection  = "identity"
	directorySection = "identity.directory"
	keytabSetting    = "identity.keytab"
)

// installation is the installation's settings scope.
var installation = entity.SettingScope{Kind: entity.ScopeInstallation}

// applyIdentity hands identity a State: the principals generation, the
// identity and identity.directory sections and the keytab's versions, read
// from one walk of the settings during which the settings generation did not
// move, and returns the records it refused. The counters are read right
// before each walk and again after it; a walk the generation moved under is
// discarded and walked again, since the walk reads each page in its own
// transaction and could mix two states of the store. While writes keep
// moving the generation the call waits, and ends only with ctx, as a pass's
// position stalls (RFC 23 §2.3).
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
// Apply refuses, never holds a principal change back: the refused record is
// reported with the settings generation it was read at, and identity is
// applied the new principals generation and keytab versions with the last
// section identity accepted in place of the refused one; when Apply refuses
// the State, with both last sections. So identity runs on its last good
// settings while the API repairs the row, and a disable still reaches it.
// Overturned if a refused setting must ever stop sign-in outright.
func (n *Node) applyIdentity(ctx context.Context) ([]refusal, error) {
	node := n.cfg.Bootstrap.Node.ID
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c, err := n.counters(ctx)
		if err != nil {
			return nil, err
		}
		snap, _, err := settings.Read(ctx, n.meta.Nodes(), n.rec.table, node)
		if err != nil {
			return nil, err
		}
		after, err := n.counters(ctx)
		if err != nil {
			return nil, err
		}
		if after.Settings != c.Settings {
			continue
		}
		return n.applyIdentityAt(ctx, c, snap)
	}
}

// applyIdentityAt builds identity's State from snap, read at counters c,
// and applies it, as applyIdentity says.
func (n *Node) applyIdentityAt(ctx context.Context, c kvstore.Counters, snap *settings.Snapshot) ([]refusal, error) {
	node := n.cfg.Bootstrap.Node.ID
	last := n.idState.Load()
	var refusals []refusal
	// kept is the last value identity accepted for a refused section, or the
	// refusal itself while identity holds none.
	kept := func(key string, err error) error {
		if last == nil {
			return err
		}
		refusals = append(refusals, settingRefused(key, c.Settings, err))
		return nil
	}
	st := identity.State{Generation: c.Principals}
	var err error
	if st.Settings, err = identity.SettingsOf(snap, node); err != nil {
		if err := kept(identitySection, err); err != nil {
			return nil, err
		}
		st.Settings = last.Settings
	}
	if st.Directory, err = identity.DirectoryOf(snap, node); err != nil {
		if err := kept(directorySection, err); err != nil {
			return nil, err
		}
		st.Directory = last.Directory
	}
	if st.Keytab, err = n.keytab(ctx, st.Settings.Keytab); err != nil {
		if err := kept(keytabSetting, err); err != nil {
			return nil, err
		}
		st.Keytab = last.Keytab
	}
	if err := n.identity.Apply(st); err != nil {
		if err := kept(identitySection, err); err != nil {
			return nil, err
		}
		st.Settings, st.Directory = last.Settings, last.Directory
		if err := n.identity.Apply(st); err != nil {
			return refusals, err
		}
	}
	n.idState.Store(&st)
	return refusals, nil
}

// keytab is every version of the keytab ref names still held, opened,
// current first; none for the zero ref.
//
// ponytail: a keytab read that fails is refused until the settings
// generation next moves, a store error among them, as the provider's error
// does not tell a store error from a version that does not open. Retry it on
// reconcile's backoff once the two can be told apart.
func (n *Node) keytab(ctx context.Context, ref entity.SecretRef) ([]secrets.Version, error) {
	if ref == (entity.SecretRef{}) {
		return nil, nil
	}
	return n.secrets.Versions(ctx, secrets.Protocol, entity.SecretKeytab, entity.SecretOwner{Scope: installation, Key: keytabSetting}, ref)
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
