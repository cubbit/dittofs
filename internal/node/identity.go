package node

import (
	"context"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/settings"
)

// stepIdentity names identity's start step and its part.
const stepIdentity = "identity"

// applyIdentity hands identity its first State: the principals generation
// and the identity section, decoded from one walk of the settings during
// which the settings generation did not move, and returns the counters that
// State was read at. The counters are read right before each walk and again
// after it; a walk the generation moved under is discarded and walked again,
// since the walk reads each page in its own transaction and could mix two
// states of the store. While writes keep moving the generation the start
// waits, and ends only with ctx, as a pass's position stalls (RFC 23 §2.3).
//
// decision: an identity section the walk refuses, a row that does not
// decode, holds a newer schema or fails the section's checks, fails the
// start, since a refused setting is never replaced by a default (RFC 13 §6).
// Its ceiling: a node rolled back below a newer writer, or one whose store
// accepted a row unchecked, does not start, and the row is repaired in the
// store by hand, as the API is not serving. Overturned once identity can be
// left unapplied, answering unavailable, while the API repairs the row and
// re-applying hands identity its State.
func (n *Node) applyIdentity(ctx context.Context) (kvstore.Counters, error) {
	node := n.cfg.Bootstrap.Node.ID
	for {
		if err := ctx.Err(); err != nil {
			return kvstore.Counters{}, err
		}
		c, err := n.counters(ctx)
		if err != nil {
			return kvstore.Counters{}, err
		}
		snap, _, err := settings.Read(ctx, n.meta.Nodes(), n.rec.table, node)
		if err != nil {
			return kvstore.Counters{}, err
		}
		after, err := n.counters(ctx)
		if err != nil {
			return kvstore.Counters{}, err
		}
		if after.Settings != c.Settings {
			continue
		}
		ids, err := identity.SettingsOf(snap, node)
		if err != nil {
			return kvstore.Counters{}, err
		}
		return c, n.identity.Apply(identity.State{Generation: c.Principals, Settings: ids})
	}
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
