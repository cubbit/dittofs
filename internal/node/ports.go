package node

import (
	"encoding/json"
	"fmt"

	adaptersettings "github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/settings"
)

// The two adapters' port keys the port rule relates (RFC 13 §6).
const (
	keyNFSPort = "adapters.nfs.port"
	keySMBPort = "adapters.smb.port"
)

// checkPorts is the adapters' sections' write check, run by their CheckTx
// with port, the written key's effective value at the write's scope, and
// node, the node serving the API. It refuses a write of either adapter's
// port that leaves the two effective ports of the target node equal and not
// 0, so the API does not accept, on the target node, a value whose restart
// fails one adapter's bind (RFC 13 §6). The target node is the one a
// node-scope write names, else node. Every row it reads is read through tx,
// tracked, so a concurrent write of one makes the write retry; a missing row
// is the adapter's default.
//
// decision: two narrowings of RFC 13 §6's rule. The port mapper's port is
// not compared, since its section is not declared and so has no value; it
// joins the rule once its section is. And only the target node is judged,
// since ControlPlaneTx reads one node's record by its ID and lists none:
// with one node that is every node the write reaches. Judge every
// registered node's override once a second node can register.
func checkPorts(tx entity.ControlPlaneTx, w settings.Write, node entity.NodeID, port uint16) error {
	var other string

	switch w.Key {
	case keyNFSPort:
		other = keySMBPort
	case keySMBPort:
		other = keyNFSPort
	default:
		return nil
	}

	target := node
	if w.Scope.Kind == entity.ScopeNode {
		target = w.Scope.ID
	}

	override := entity.SettingScope{
		Kind: entity.ScopeNode,
		ID:   target,
	}

	// An installation write reaches the target node only where no override
	// of the written key shadows it.
	if w.Scope.Kind == entity.ScopeInstallation {
		own, found, err := readPort(tx, override, w.Key)
		if err != nil {
			return err
		}

		if found {
			port = own
		}
	}

	if port == 0 {
		return nil
	}

	otherPort, err := effectivePort(tx, override, other)
	if err != nil {
		return err
	}

	if otherPort != port {
		return nil
	}

	return w.Refuse(fmt.Sprintf("node %x would run %s and %s both on %d; "+
		"the two adapters' ports of one node must differ unless 0",
		target, keyNFSPort, keySMBPort, port))
}

// effectivePort is key's value on the node override names: its override,
// else the installation's row, else the adapter's default.
func effectivePort(
	tx entity.ControlPlaneTx,
	override entity.SettingScope,
	key string,
) (uint16, error) {
	port, found, err := readPort(tx, override, key)
	if err != nil || found {
		return port, err
	}

	port, found, err = readPort(tx, installation, key)
	if err != nil || found {
		return port, err
	}

	if key == keyNFSPort {
		return adaptersettings.DefaultNFS().Port, nil
	}

	return adaptersettings.DefaultSMB().Port, nil
}

// readPort is the port key's row at scope, read tracked in tx; false when no
// row is stored. A row that does not decode is an error, not a refusal.
func readPort(
	tx entity.ControlPlaneTx,
	scope entity.SettingScope,
	key string,
) (uint16, bool, error) {
	row, found, err := tx.Setting(scope, key)
	if err != nil || !found {
		return 0, false, err
	}

	var port uint16
	if err := json.Unmarshal(row.Value, &port); err != nil {
		return 0, false, fmt.Errorf("node: read %s: %w", key, err)
	}

	return port, true, nil
}
