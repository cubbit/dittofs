package node

import (
	"context"
	"fmt"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/settings"
)

const (
	// stepIdentity names identity's start step and its part.
	stepIdentity = "identity"
	// identitySection is the prefix of identity's settings section.
	identitySection = "identity"
)

// newIdentity builds the identity server over the node's store, the secret
// provider p, the node's clock, metrics registry and logger. It starts
// nothing: its first State arrives at start step 4.
func (n *Node) newIdentity(p *secrets.Provider) (*identity.Server, error) {
	s, err := identity.New(identity.Deps{
		Store:   n.meta,
		Secrets: p,
		Clock:   n.cfg.Clock,
		Metrics: n.cfg.Metrics,
		Logger:  n.cfg.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("node: %w", err)
	}
	return s, nil
}

// applyIdentity hands identity its first State: the principals generation
// in c and the identity section, decoded from one walk of the settings.
//
// decision: an identity section the node cannot decode, or identity refuses,
// fails the start, since identity cannot start before a State is applied and
// none is yet to fall back on. Overturned once a refused section is reported
// refused and identity is applied with the section's defaults instead.
func (n *Node) applyIdentity(ctx context.Context, c kvstore.Counters) error {
	snap, _, err := settings.Read(ctx, n.meta.Nodes(), n.rec.table, n.cfg.Bootstrap.Node.ID)
	if err != nil {
		return err
	}
	ids, err := settings.Get[identity.Settings](snap, identitySection, n.cfg.Bootstrap.Node.ID)
	if err != nil {
		return err
	}
	return n.identity.Apply(identity.State{Generation: c.Principals, Settings: ids})
}

// startIdentity starts identity under the State start step 4 applied.
func (n *Node) startIdentity(ctx context.Context) error {
	if err := n.identity.Start(ctx); err != nil {
		return err
	}
	n.started(stepIdentity, n.identity.Stop)
	return nil
}
