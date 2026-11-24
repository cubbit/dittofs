package node

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/prof"
	adaptersettings "github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/codec"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/settings"
)

// The nodes the port tests name: thisNode is the node the table is built
// for, the one serving the API; nodeX and nodeY are other nodes a
// node-scope write names.
var (
	thisNode = entity.NodeID{0x01, 15: 0x01}
	nodeX    = entity.NodeID{0x0a, 15: 0x0a}
	nodeY    = entity.NodeID{0x0b, 15: 0x0b}
)

// at is the node-scope override of node.
func at(node entity.NodeID) entity.SettingScope {
	return entity.SettingScope{
		Kind: entity.ScopeNode,
		ID:   node,
	}
}

// portTable is the two adapters' sections as the node declares them, each
// with checkPorts as its write check for thisNode.
func portTable() settings.Table {
	return settings.Table{
		settings.Of(
			"adapters.nfs",
			settings.Override(entity.ScopeInstallation, entity.ScopeNode),
			adaptersettings.DefaultNFS(),
			settings.CheckTx(
				func(tx entity.ControlPlaneTx, w settings.Write, s adaptersettings.NFS) error {
					return checkPorts(tx, w, thisNode, s.Port)
				},
			),
		),
		settings.Of(
			"adapters.smb",
			settings.Override(entity.ScopeInstallation, entity.ScopeNode),
			adaptersettings.DefaultSMB(),
			settings.CheckTx(
				func(tx entity.ControlPlaneTx, w settings.Write, s adaptersettings.SMB) error {
					return checkPorts(tx, w, thisNode, s.Port)
				},
			),
		),
	}
}

// portStore is a control plane over an in-memory Badger, and that Badger,
// which TestCheckPortsOtherOverrideReadTracked writes beneath the store.
type portStore struct {
	cp kvstore.ControlPlane
	db kv.KV
}

func openPorts(t *testing.T) portStore {
	t.Helper()

	ctx := context.Background()

	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})

	if err := kvstore.Init(ctx, db, uuid.New()); err != nil {
		t.Fatal(err)
	}

	st, err := kvstore.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	return portStore{
		cp: st.ControlPlane(),
		db: db,
	}
}

// put stores one row through the store's own write, with no check.
func (s portStore) put(t *testing.T, scope entity.SettingScope, key, value string) {
	t.Helper()

	err := s.cp.Write(
		context.Background(),
		entity.WriteMeta{},
		func(tx entity.ControlPlaneTx) error {
			cur, _, err := tx.Setting(scope, key)
			if err != nil {
				return err
			}

			_, err = tx.PutSetting(portRow(scope, key, value), cur.Generation)

			return err
		},
	)
	if err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

// check runs the table's Check on w inside one Write, as the API's route
// does, and returns its error.
func (s portStore) check(w settings.Write) error {
	return s.cp.Write(
		context.Background(),
		entity.WriteMeta{},
		func(tx entity.ControlPlaneTx) error {
			return portTable().Check(tx, entity.RoleAdmin, w, nil)
		},
	)
}

// attempts runs the put of row as the route does, Check then the put, and on
// the first attempt only, between the two, runs hook; it returns how many
// times the write's function ran and the write's error.
func (s portStore) attempts(row entity.Setting, hook func()) (int, error) {
	n := 0

	err := s.cp.Write(
		context.Background(),
		entity.WriteMeta{},
		func(tx entity.ControlPlaneTx) error {
			n++

			if err := portTable().Check(tx, entity.RoleAdmin, settings.Put(row), nil); err != nil {
				return err
			}

			if n == 1 {
				hook()
			}

			cur, _, err := tx.Setting(row.Scope, row.Key)
			if err != nil {
				return err
			}

			_, err = tx.PutSetting(row, cur.Generation)

			return err
		},
	)

	return n, err
}

// raw writes one setting row straight to the Badger, with no check and no
// settings generation raised, so only a read of that very row conflicts. It
// mirrors kvstore's settingKey for a node scope and its encodeSetting (the v1
// row: value, generation, schema, position), both private to kvstore.
func (s portStore) raw(t *testing.T, scope entity.SettingScope, key, value string, schema uint32) {
	t.Helper()

	k := keys.ID([]byte{keys.KindCFG, byte(scope.Kind)}, scope.ID[:])
	k = keys.String(k, []byte(key))
	v := codec.NewWriter(1).
		Bytes([]byte(value)).
		Uint64(1).
		Uint32(schema).
		Uint64(1).
		Encode()

	err := s.db.Update(context.Background(), func(tx kv.Txn) error {
		return tx.Set(k, v)
	})
	if err != nil {
		t.Fatalf("raw row %s: %v", key, err)
	}
}

// currentSchema is the newest settings schema this binary reads.
func currentSchema() uint32 {
	return settings.Versions()[entity.FormatSettings].Max
}

// portRow is a row of value at the current schema.
func portRow(scope entity.SettingScope, key, value string) entity.Setting {
	return entity.Setting{
		Scope:  scope,
		Key:    key,
		Value:  []byte(value),
		Schema: currentSchema(),
	}
}

// refusedLine fails the test unless err matches settings.ErrInvalid and
// prints line whole.
func refusedLine(t *testing.T, what string, err error, line string) {
	t.Helper()

	if !errors.Is(err, settings.ErrInvalid) {
		t.Fatalf("%s: err = %v, want settings.ErrInvalid", what, err)
	}

	if err.Error() != line {
		t.Errorf("%s: line = %q, want %q", what, err.Error(), line)
	}
}

// accepted fails the test unless err is nil.
func accepted(t *testing.T, what string, err error) {
	t.Helper()

	if err != nil {
		t.Errorf("%s: err = %v, want nil", what, err)
	}
}

// A node-scope write is judged against the other port's installation value,
// not only the rows at the written scope.
func TestCheckPortsNodeWriteMeetsInstallation(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := openPorts(t)
	s.put(t, installation, "adapters.nfs.port", "7000")

	err := s.check(settings.Put(portRow(at(nodeX), "adapters.smb.port", "7000")))

	refusedLine(
		t,
		"node X put smb 7000",
		err,
		portLine("node "+nodeXHex, "adapters.smb.port", nodeXHex, 7000),
	)
}

// An installation write is judged against this node's override of the other
// port, which is the value this node runs.
func TestCheckPortsInstallationWriteMeetsOverride(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := openPorts(t)
	s.put(t, at(thisNode), "adapters.nfs.port", "5000")

	err := s.check(settings.Put(portRow(installation, "adapters.smb.port", "5000")))

	refusedLine(
		t,
		"installation put smb 5000",
		err,
		portLine("installation", "adapters.smb.port", thisNodeHex, 5000),
	)
}

// Two ports at 0 let the system pick each, so they never clash.
func TestCheckPortsZeroPortsPass(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := openPorts(t)
	s.put(t, installation, "adapters.smb.port", "0")

	accepted(t, "installation put nfs 0",
		s.check(settings.Put(portRow(installation, "adapters.nfs.port", "0"))))

	s.put(t, at(nodeX), "adapters.nfs.port", "0")

	accepted(t, "node X put smb 0",
		s.check(settings.Put(portRow(at(nodeX), "adapters.smb.port", "0"))))
}

// The other port's override is read tracked: rewritten beneath the store
// between Check and the commit, it makes the write run again, and the second
// run refuses the port it now equals.
func TestCheckPortsOtherOverrideReadTracked(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := openPorts(t)
	s.put(t, at(nodeX), "adapters.nfs.port", "7000")

	n, err := s.attempts(portRow(at(nodeX), "adapters.smb.port", "7001"), func() {
		s.raw(t, at(nodeX), "adapters.nfs.port", "7001", currentSchema())
	})

	if n != 2 {
		t.Errorf("write function runs = %d, want 2", n)
	}

	refusedLine(
		t,
		"node X put smb 7001 after nfs moved to 7001",
		err,
		portLine("node "+nodeXHex, "adapters.smb.port", nodeXHex, 7001),
	)
}

// A port with no row anywhere is its default, never 0.
func TestCheckPortsMissingRowIsDefault(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := openPorts(t)

	err := s.check(settings.Put(portRow(installation, "adapters.nfs.port", "12445")))

	refusedLine(
		t,
		"installation put nfs 12445",
		err,
		portLine("installation", "adapters.nfs.port", thisNodeHex, 12445),
	)
}

// An installation write this node's own override shadows does not change the
// port this node runs, so it passes.
func TestCheckPortsOverrideShadowsInstallation(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := openPorts(t)
	s.put(t, installation, "adapters.nfs.port", "12049")
	s.put(t, at(thisNode), "adapters.smb.port", "6000")

	accepted(t, "installation put smb 12049 under this node's 6000",
		s.check(settings.Put(portRow(installation, "adapters.smb.port", "12049"))))
}

// A delete of an override exposes the installation's value, which is judged.
func TestCheckPortsDeleteChecked(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := openPorts(t)
	s.put(t, installation, "adapters.smb.port", "12049")
	s.put(t, at(nodeX), "adapters.nfs.port", "12049")
	s.put(t, at(nodeX), "adapters.smb.port", "6000")

	err := s.check(settings.Delete(at(nodeX), "adapters.smb.port"))

	refusedLine(
		t,
		"delete of node X's smb override",
		err,
		portLine("node "+nodeXHex, "adapters.smb.port", nodeXHex, 12049),
	)
}

// A node-scope write is judged on the node it names, not on this node.
func TestCheckPortsTargetIsWrittenNode(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := openPorts(t)
	s.put(t, at(thisNode), "adapters.nfs.port", "7000")

	accepted(t, "node Y put smb 7000",
		s.check(settings.Put(portRow(at(nodeY), "adapters.smb.port", "7000"))))
}

// A null row of the other port, written around Check, is an error, never
// port 0 with the rule off.
func TestCheckPortsNullRowRefused(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := openPorts(t)
	s.raw(t, at(nodeX), "adapters.nfs.port", "null", currentSchema())

	err := s.check(settings.Put(portRow(at(nodeX), "adapters.smb.port", "7000")))
	if err == nil || errors.Is(err, settings.ErrInvalid) {
		t.Fatalf("node X put smb 7000 over a null nfs row: err = %v, want a read error", err)
	}

	if want := "node: read adapters.nfs.port: null"; err.Error() != want {
		t.Errorf("err = %q, want %q", err.Error(), want)
	}
}

// A row of the other port at a schema newer than this binary reads is
// refused as the settings read refuses it, never decoded.
func TestCheckPortsNewerSchemaRowRefused(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := openPorts(t)
	s.raw(t, at(nodeX), "adapters.nfs.port", "7000", currentSchema()+1)

	err := s.check(settings.Put(portRow(at(nodeX), "adapters.smb.port", "7001")))
	if !errors.Is(err, settings.ErrNewerSchema) {
		t.Fatalf("node X put smb 7001 over a newer nfs row: err = %v, want settings.ErrNewerSchema", err)
	}
}

// Two different ports pass.
func TestCheckPortsDifferentPortsPass(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := openPorts(t)
	s.put(t, installation, "adapters.nfs.port", "7000")

	accepted(t, "installation put smb 7001",
		s.check(settings.Put(portRow(installation, "adapters.smb.port", "7001"))))
}

// The nodes' IDs as a refusal prints them.
const (
	nodeXHex    = "0a00000000000000000000000000000a"
	thisNodeHex = "01000000000000000000000000000001"
)

// portLine is the refusal of a write of key at scope that leaves both ports
// of node on port, naming the written key, both port keys and the node.
func portLine(scope, key, node string, port int) string {
	return fmt.Sprintf("setting (%s): %s: node %s would run adapters.nfs.port and "+
		"adapters.smb.port both on %d; the two adapters' ports of one node must differ unless 0",
		scope, key, node, port)
}
