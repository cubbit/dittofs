package api

import (
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// netbiosNameKey is the installation's own NetBIOS name, the computer
// account's name while it is joined.
const netbiosNameKey = "identity.netbios_name"

// netbiosRule is the rule for the installation's NetBIOS name, run in the
// transaction of a put or a delete of key at scope before the write: a write
// of the installation's identity.netbios_name is refused 409 joined while
// Installation.Join, read tracked in tx, says joined, since the name is then
// the computer account's. A join committing between that read and the
// write's commit aborts the write. A write of any other key passes.
//
// The join writes the name itself in the transaction that records it
// joined, through the store rather than this route, so it never meets the
// rule.
func netbiosRule(tx entity.ControlPlaneTx, scope entity.SettingScope, key string) error {
	if scope.Kind != entity.ScopeInstallation || key != netbiosNameKey {
		return nil
	}
	in, err := tx.Installation()
	if err != nil || !in.Join.Joined {
		return err
	}
	return &refusal{code: apiv1.CodeJoined, detail: "the installation is joined as " + in.Join.Account + ": " + netbiosNameKey +
		" is the computer account's name and does not change while joined; a new join with the takeover writes its account's name"}
}
