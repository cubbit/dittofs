package secrets

import (
	"context"
	"fmt"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
)

// Prune drops the versions below the lowest one any registered node reports
// holding, never the current one, after checking the caller's kind and
// owner as Resolve does. It commits through tx, inside the caller's own
// Write beside its audit record, so a prune commits with its audit or not
// at all. The current version is read through tx, tracked, so a seal that
// commits first makes the Write run again against the new current version.
//
// ponytail: a registered node that is down keeps its last report and holds
// the prune back. Ceiling: a second node. Upgrade: RFC 15's membership
// decides which nodes count.
//
// decision: a registered node that reports nothing of ref holds nothing back,
// since it resolves the current version when it next resolves ref. Withdraw
// it if a node can keep a version below the current one without reporting
// it.
//
// decision: the statuses are read outside tx, untracked, so a report that
// commits after this read does not rerun the Write. It widens only the gap
// between a node's resolve and its report, which a tracked read would not
// close. Withdraw it if a node's reported version can ever go down.
func (p *Provider) Prune(
	ctx context.Context,
	tx entity.ControlPlaneTx,
	kind entity.SecretKind,
	owner entity.SecretOwner,
	ref entity.SecretRef,
) error {
	cur, ok, err := tx.Secret(ref, 0)
	if err != nil {
		return fmt.Errorf("secrets: read current version: %w", err)
	}

	if !ok {
		return merrs.NewNotFoundError(fmt.Sprintf("%x", ref[:]), "secret")
	}

	if cur.Kind != kind || !owner.Equal(cur.Owner) {
		return ErrWrongKind
	}

	statuses, err := p.cp.RegisteredStatuses(ctx)
	if err != nil {
		return fmt.Errorf("secrets: read node statuses: %w", err)
	}

	floor, reported := lowestHeld(statuses, ref)
	if !reported || floor > cur.Version {
		floor = cur.Version
	}

	if err := tx.PruneSecret(ref, floor); err != nil {
		return fmt.Errorf("secrets: prune below version %d: %w", floor, err)
	}

	return nil
}

// lowestHeld is the lowest version of ref any status reports, and whether
// any status reports ref at all.
func lowestHeld(statuses []entity.NodeStatus, ref entity.SecretRef) (uint64, bool) {
	var (
		lowest   uint64
		reported bool
	)

	for _, st := range statuses {
		for _, held := range st.Secrets {
			if held.Secret != ref {
				continue
			}

			if !reported || held.Version < lowest {
				lowest = held.Version
				reported = true
			}
		}
	}

	return lowest, reported
}
