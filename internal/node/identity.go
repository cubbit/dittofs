package node

import (
	"context"
	"fmt"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/settings"
)

const (
	// stepIdentity names identity's start step and its part.
	stepIdentity = "identity"
	// identityWalks is how many settings walks the first State tries before
	// the start fails, each discarded when the settings generation moved
	// during it.
	identityWalks = 3
)

// applyIdentity hands identity its first State: the principals generation
// and the identity section, decoded from one walk of the settings during
// which the settings generation did not move. c is the counters read before
// the first walk; a walk the generation moved under is discarded and walked
// again, up to identityWalks times, since the walk reads each page in its
// own transaction and could mix two states of the store.
//
// decision: an identity section the walk refuses, a row that does not
// decode, holds a newer schema or fails the section's checks, is logged and
// identity runs the section's defaults, so a node rolled back below a newer
// writer, or one row the store accepted unchecked, still starts and its API
// can repair the row. The defaults are only safe for the first State: once
// identity runs one, re-applying must keep it and report the row refused
// instead. A store error, and a State identity refuses, still fail the
// start: neither has a value to fall back on.
func (n *Node) applyIdentity(ctx context.Context, c kvstore.Counters) error {
	node := n.cfg.Bootstrap.Node.ID
	for range identityWalks {
		snap, refused, err := settings.Read(ctx, n.meta.Nodes(), n.rec.table, node)
		if err != nil {
			return err
		}
		after, err := n.counters(ctx)
		if err != nil {
			return err
		}
		if after.Settings != c.Settings {
			c = after
			continue
		}
		ids := identity.DefaultSettings()
		if err := refused[identity.SectionName]; err != nil {
			n.cfg.Logger.Error("identity: settings section refused, running its defaults", "err", err)
		} else if ids, err = settings.Get[identity.Settings](snap, identity.SectionName, node); err != nil {
			return err
		}
		return n.identity.Apply(identity.State{Generation: c.Principals, Settings: ids})
	}
	return fmt.Errorf("the settings generation moved during each of %d walks", identityWalks)
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
