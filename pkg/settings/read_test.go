package settings_test

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/codec"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/settings"
)

// rdServer is a section with a port its Validate bounds and a host.
type rdServer struct {
	Port uint16 `json:"port" class:"live"`
	Host string `json:"host" class:"restart"`
}

func (s rdServer) Validate() error {
	if s.Port == 0 || s.Port > 60000 {
		return fmt.Errorf("port must be between 1 and 60000, got %d", s.Port)
	}

	return nil
}

// rdPeer is a section with two required keys.
type rdPeer struct {
	Address string `json:"address" class:"restart"`
	Peer    string `json:"peer"    class:"restart"`
}

func (rdPeer) Validate() error {
	return nil
}

// rdCred holds a secret reference; its Validate never prints it.
type rdCred struct {
	Ref   entity.SecretRef `json:"ref"   class:"live"`
	Limit int              `json:"limit" class:"live"`
}

func (c rdCred) Validate() error {
	if c.Limit > 10 {
		return fmt.Errorf("limit must be at most 10, got %d", c.Limit)
	}

	return nil
}

// rdList holds a slice, so a caller could change what Get hands it.
type rdList struct {
	Items []string `json:"items" class:"live"`
}

func (rdList) Validate() error {
	return nil
}

var (
	rdInstallation = entity.SettingScope{
		Kind: entity.ScopeInstallation,
	}
	rdNodeA = entity.NodeID{0xA}
	rdNodeB = entity.NodeID{0xB}
)

func rdNodeScope(n entity.NodeID) entity.SettingScope {
	return entity.SettingScope{
		Kind: entity.ScopeNode,
		ID:   n,
	}
}

func rdShareScope(b byte) entity.SettingScope {
	return entity.SettingScope{
		Kind: entity.ScopeShare,
		ID:   [16]byte{b},
	}
}

func rdServerDefault() rdServer {
	return rdServer{
		Port: 445,
		Host: "default",
	}
}

// rdStore is a real Badger store in memory, formatted and opened.
type rdStore struct {
	db    kv.KV
	cp    kvstore.ControlPlane
	nodes kvstore.Nodes
}

func newRdStore(t *testing.T) rdStore {
	t.Helper()

	ctx := context.Background()

	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatalf("open badger: %v", err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close badger: %v", err)
		}
	})

	if err := kvstore.Init(ctx, db, [16]byte{1}); err != nil {
		t.Fatalf("init store: %v", err)
	}

	st, err := kvstore.Open(ctx, db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	return rdStore{
		db:    db,
		cp:    st.ControlPlane(),
		nodes: st.Nodes(),
	}
}

// put writes a new row through the store's own write.
func (s rdStore) put(t *testing.T, scope entity.SettingScope, key, value string) uint64 {
	t.Helper()

	gen, err := s.cp.PutSetting(context.Background(), entity.Setting{
		Scope:  scope,
		Key:    key,
		Value:  []byte(value),
		Schema: 1,
	}, 0)
	if err != nil {
		t.Fatalf("put %s: %v", key, err)
	}

	return gen
}

// raw writes value straight under the row key of scope and key, as the
// store lays it out, for rows its own write refuses to make.
func (s rdStore) raw(t *testing.T, scope entity.SettingScope, key string, value []byte) {
	t.Helper()

	k := []byte{keys.KindCFG, byte(scope.Kind)}
	if scope.Kind != entity.ScopeInstallation {
		k = keys.ID(k, scope.ID[:])
	}

	s.rawKey(t, keys.String(k, []byte(key)), value)
}

func (s rdStore) rawKey(t *testing.T, key, value []byte) {
	t.Helper()

	err := s.db.Update(context.Background(), func(tx kv.Txn) error {
		return tx.Set(key, value)
	})
	if err != nil {
		t.Fatalf("raw write: %v", err)
	}
}

// rdRow is a setting row's stored bytes at schema.
func rdRow(value string, schema uint32) []byte {
	return codec.NewWriter(1).
		Bytes([]byte(value)).
		Uint64(1).
		Uint32(schema).
		Uint64(1).
		Encode()
}

func rdRead(
	t *testing.T,
	r entity.SettingsReader,
	table settings.Table,
	node entity.NodeID,
) (*settings.Snapshot, map[string]error) {
	t.Helper()

	snap, refused, err := settings.Read(context.Background(), r, table, node)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	return snap, refused
}

func rdWantPort(
	t *testing.T,
	snap *settings.Snapshot,
	section string,
	node entity.NodeID,
	want uint16,
) {
	t.Helper()

	got, err := settings.Get[rdServer](snap, section, node)
	if err != nil {
		t.Fatalf("Get %s: %v", section, err)
	}

	if got.Port != want {
		t.Fatalf("Get %s: port %d, want %d", section, got.Port, want)
	}
}

func rdWantNoRefusal(t *testing.T, refused map[string]error) {
	t.Helper()

	if len(refused) != 0 {
		t.Fatalf("refused %v, want none", refused)
	}
}

func TestReadOverride(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("srv", settings.Override(entity.ScopeInstallation, entity.ScopeNode),
			rdServerDefault()),
	}

	s.put(t, rdInstallation, "srv.port", "80")
	s.put(t, rdNodeScope(rdNodeB), "srv.port", "82")

	snap, refused := rdRead(t, s.nodes, table, rdNodeA)
	rdWantNoRefusal(t, refused)
	rdWantPort(t, snap, "srv", rdNodeA, 80)

	gen := s.put(t, rdNodeScope(rdNodeA), "srv.port", "81")

	snap, _ = rdRead(t, s.nodes, table, rdNodeA)
	rdWantPort(t, snap, "srv", rdNodeA, 81)

	err := s.cp.DeleteSetting(context.Background(), rdNodeScope(rdNodeA), "srv.port", gen)
	if err != nil {
		t.Fatalf("delete node row: %v", err)
	}

	snap, _ = rdRead(t, s.nodes, table, rdNodeA)
	rdWantPort(t, snap, "srv", rdNodeA, 80)
}

func TestReadOverrideOnly(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("srv", settings.Override(entity.ScopeInstallation, entity.ScopeNode),
			rdServerDefault(), settings.OverrideOnly("Port")),
	}

	s.put(t, rdInstallation, "srv.port", "80")
	s.put(t, rdInstallation, "srv.host", `"wide"`)
	s.put(t, rdNodeScope(rdNodeA), "srv.port", "81")
	s.put(t, rdNodeScope(rdNodeA), "srv.host", `"narrow"`)

	snap, refused := rdRead(t, s.nodes, table, rdNodeA)
	rdWantNoRefusal(t, refused)

	got, err := settings.Get[rdServer](snap, "srv", rdNodeA)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	want := rdServer{
		Port: 81,
		Host: "wide",
	}
	if got != want {
		t.Fatalf("Get: %+v, want %+v", got, want)
	}
}

func TestReadRequired(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("peer", settings.At(entity.ScopeInstallation), rdPeer{},
			settings.Required("Address", "Peer")),
	}

	_, refused := rdRead(t, s.nodes, table, rdNodeA)

	err := refused["peer"]
	if !errors.Is(err, settings.ErrRequired) {
		t.Fatalf("refused[peer] = %v, want ErrRequired", err)
	}

	want := "setting (installation): peer.address: required and has no value\n" +
		"setting (installation): peer.peer: required and has no value"
	if err.Error() != want {
		t.Fatalf("refusal %q, want %q", err.Error(), want)
	}

	s.put(t, rdInstallation, "peer.address", `"10.0.0.1"`)
	s.put(t, rdInstallation, "peer.peer", `"10.0.0.2"`)

	snap, refused := rdRead(t, s.nodes, table, rdNodeA)
	rdWantNoRefusal(t, refused)

	got, err := settings.Get[rdPeer](snap, "peer", rdNodeA)
	if err != nil || got.Address != "10.0.0.1" {
		t.Fatalf("Get: %+v, %v, want address 10.0.0.1", got, err)
	}
}

func TestReadRequiredOverride(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("ovr", settings.Override(entity.ScopeInstallation, entity.ScopeNode),
			rdPeer{}, settings.Required("Address", "Peer")),
		settings.Of("ovs", settings.Override(entity.ScopeInstallation, entity.ScopeShare),
			rdPeer{}, settings.Required("Address", "Peer")),
		settings.Of("ovm", settings.Override(entity.ScopeInstallation, entity.ScopeNode),
			rdPeer{}, settings.Required("Address")),
	}

	s.put(t, rdInstallation, "ovr.address", `"a"`)
	s.put(t, rdNodeScope(rdNodeA), "ovr.peer", `"p"`)
	s.put(t, rdInstallation, "ovs.address", `"a"`)
	s.put(t, rdShareScope(0xA), "ovs.peer", `"p"`)

	snap, refused := rdRead(t, s.nodes, table, rdNodeA)

	if err := refused["ovr"]; err != nil {
		t.Fatalf("refused[ovr] = %v, want none: address is held at installation", err)
	}

	want := rdPeer{
		Address: "a",
		Peer:    "p",
	}

	got, err := settings.Get[rdPeer](snap, "ovr", rdNodeA)
	if err != nil || got != want {
		t.Fatalf("Get ovr: %+v, %v, want %+v", got, err, want)
	}

	share, err := settings.InstanceValue(snap, "ovs", [16]byte{0xA})
	if err != nil || share != want {
		t.Fatalf("ovs of share a: %v, %v, want %+v", share, err, want)
	}

	// With no node row, the value and so its refusal are the installation's.
	line := "setting (installation): ovm.address: required and has no value"
	if err := refused["ovm"]; err == nil || err.Error() != line {
		t.Fatalf("refused[ovm] = %v, want %q", err, line)
	}
}

func TestReadPerScopeDefaults(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("pq", settings.At(entity.ScopeShare), rdPeer{},
			settings.Required("Address")),
		settings.Of("rq", settings.Override(entity.ScopeInstallation, entity.ScopeShare),
			rdPeer{}, settings.Required("Address")),
		settings.Of("dq", settings.At(entity.ScopeShare), rdServer{}),
		settings.Of("rw", settings.Override(entity.ScopeInstallation, entity.ScopeShare),
			rdServerDefault()),
	}

	s.put(t, rdShareScope(0xA), "pq.address", `"x"`)
	s.put(t, rdShareScope(0xA), "rq.address", `"y"`)

	snap, refused := rdRead(t, s.nodes, table, rdNodeA)
	rdWantNoRefusal(t, refused)

	got, err := settings.InstanceValue(snap, "pq", [16]byte{0xA})
	if err != nil || got != (rdPeer{Address: "x"}) {
		t.Fatalf("pq of share a: %v, %v, want address x", got, err)
	}

	got, err = settings.InstanceValue(snap, "rq", [16]byte{0xA})
	if err != nil || got != (rdPeer{Address: "y"}) {
		t.Fatalf("rq of share a: %v, %v, want address y", got, err)
	}

	// A share with no rows runs the defaults, which this section refuses.
	_, err = settings.InstanceValue(snap, "pq", [16]byte{0xB})
	if !errors.Is(err, settings.ErrRequired) {
		t.Fatalf("pq of share b: %v, want ErrRequired", err)
	}

	// A stored row the section's Validate refuses is refused under its share,
	// and a bad installation row of a share override with no share rows.
	s.put(t, rdShareScope(0xA), "dq.port", "61000")
	s.raw(t, rdInstallation, "rw.port", []byte{0xFF})

	_, refused = rdRead(t, s.nodes, table, rdNodeA)

	rw := "setting (installation): rw.port: the stored row does not decode"
	if err := refused["rw"]; !errors.Is(err, settings.ErrInvalid) || err.Error() != rw {
		t.Fatalf("refused[rw] = %v, want %q, once", err, rw)
	}

	want := "setting (share 0a000000000000000000000000000000): dq: " +
		"port must be between 1 and 60000, got 61000"
	if err := refused["dq"]; !errors.Is(err, settings.ErrInvalid) || err.Error() != want {
		t.Fatalf("refused[dq] = %v, want %q wrapping ErrInvalid", err, want)
	}

	if len(refused) != 2 {
		t.Fatalf("refused %v, want dq and rw only", refused)
	}
}

// rdSpare is a per-journal section, under a key name RFC 13 writes.
type rdSpare struct {
	SpareTarget uint32 `json:"spare_target" class:"live"`
}

func (rdSpare) Validate() error {
	return nil
}

func TestReadPerThing(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const (
		j1 = "3f2a00000000000000000000000000a1"
		j2 = "3f2a00000000000000000000000000a2"
		j3 = "3f2a00000000000000000000000000a3"
	)

	table := settings.Table{
		settings.Of("journal", settings.PerThing(entity.ScopeNode, "journal"), rdSpare{}),
	}

	wantSpare := func(t *testing.T, snap *settings.Snapshot, thing string, want uint32) {
		t.Helper()

		got, err := settings.ThingValue(snap, "journal", rdNodeA, thing)
		if err != nil || got != (rdSpare{SpareTarget: want}) {
			t.Fatalf("journal %s: %v, %v, want spare target %d", thing, got, err, want)
		}
	}

	s := newRdStore(t)
	s.put(t, rdNodeScope(rdNodeA), "journal."+j1+".spare_target", "5")
	s.put(t, rdNodeScope(rdNodeA), "journal."+j2+".spare_target", "7")
	s.put(t, rdNodeScope(rdNodeB), "journal."+j1+".spare_target", "9")
	s.raw(t, rdNodeScope(rdNodeB), "journal."+j2+".spare_target", []byte{0xFF})

	snap, refused := rdRead(t, s.nodes, table, rdNodeA)
	rdWantNoRefusal(t, refused)
	wantSpare(t, snap, j1, 5)
	wantSpare(t, snap, j2, 7)

	// A bad row of journal j1 refuses j1 alone, naming its key as stored;
	// with j3 bad too, both lines come in thing order.
	bad := newRdStore(t)
	bad.raw(t, rdNodeScope(rdNodeA), "journal."+j3+".spare_target", []byte{0xFF})
	bad.raw(t, rdNodeScope(rdNodeA), "journal."+j1+".spare_target", []byte{0xFF})
	bad.put(t, rdNodeScope(rdNodeA), "journal."+j2+".spare_target", "7")

	snap, refused = rdRead(t, bad.nodes, table, rdNodeA)
	wantSpare(t, snap, j2, 7)

	_, err := settings.ThingValue(snap, "journal", rdNodeA, j1)
	if !errors.Is(err, settings.ErrInvalid) {
		t.Fatalf("journal %s: %v, want ErrInvalid", j1, err)
	}

	want := "setting (node 0a000000000000000000000000000000): journal." + j1 +
		".spare_target: the stored row does not decode\n" +
		"setting (node 0a000000000000000000000000000000): journal." + j3 +
		".spare_target: the stored row does not decode"
	if err := refused["journal"]; err == nil || err.Error() != want {
		t.Fatalf("refused[journal] = %v, want %q", err, want)
	}

	// A row whose key carries no thing is skipped; a key the section does not
	// declare is refused for its thing alone, named as stored, quoted.
	odd := newRdStore(t)
	odd.put(t, rdNodeScope(rdNodeA), "journal.spare_target", "3")
	odd.put(t, rdNodeScope(rdNodeA), "journal."+j2+".spare_target", "7")

	snap, refused = rdRead(t, odd.nodes, table, rdNodeA)
	rdWantNoRefusal(t, refused)
	wantSpare(t, snap, j2, 7)
	wantSpare(t, snap, "", 0)

	odd.put(t, rdNodeScope(rdNodeA), "journal."+j1+".nosuch", "1")

	_, refused = rdRead(t, odd.nodes, table, rdNodeA)

	quoted := "setting (node 0a000000000000000000000000000000): " +
		`"journal.` + j1 + `.nosuch": unknown key; did you mean journal.spare_target?`
	if err := refused["journal"]; !errors.Is(err, settings.ErrUnknownKey) ||
		err.Error() != quoted {
		t.Fatalf("refused[journal] = %v, want %q", err, quoted)
	}

	// A thing with no rows runs the defaults, refused here for a required key,
	// but no thing anyone configured is refused, so nothing is reported.
	required := settings.Table{
		settings.Of("jreq", settings.PerThing(entity.ScopeNode, "journal"), rdPeer{},
			settings.Required("Address")),
	}

	req := newRdStore(t)
	req.put(t, rdNodeScope(rdNodeA), "jreq."+j1+".address", `"a"`)

	snap, refused = rdRead(t, req.nodes, required, rdNodeA)
	rdWantNoRefusal(t, refused)

	_, err = settings.ThingValue(snap, "jreq", rdNodeA, j2)

	line := "setting (node 0a000000000000000000000000000000): jreq.address: " +
		"required and has no value"
	if !errors.Is(err, settings.ErrRequired) || err.Error() != line {
		t.Fatalf("jreq %s with no rows: %v, want %q", j2, err, line)
	}
}

func TestReadInvalidRefused(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("srv", settings.At(entity.ScopeInstallation), rdServerDefault()),
		settings.Of("web", settings.At(entity.ScopeInstallation), rdServerDefault()),
	}

	s.put(t, rdInstallation, "srv.port", "61000")
	s.put(t, rdInstallation, "web.port", "8080")

	snap, refused := rdRead(t, s.nodes, table, rdNodeA)

	want := "setting (installation): srv: port must be between 1 and 60000, got 61000"
	if err := refused["srv"]; !errors.Is(err, settings.ErrInvalid) || err.Error() != want {
		t.Fatalf("refused[srv] = %v, want %q wrapping ErrInvalid", err, want)
	}

	if len(refused) != 1 {
		t.Fatalf("refused %v, want srv only", refused)
	}

	got, err := settings.Get[rdServer](snap, "srv", rdNodeA)
	if err == nil || err.Error() != want {
		t.Fatalf("Get srv: %+v, %v, want the refusal", got, err)
	}

	rdWantPort(t, snap, "web", rdNodeA, 8080)
}

func TestReadNoRows(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("srv", settings.At(entity.ScopeInstallation), rdServerDefault()),
		settings.Of("bad", settings.At(entity.ScopeInstallation), rdServer{}),
	}

	snap, refused := rdRead(t, s.nodes, table, rdNodeA)
	rdWantPort(t, snap, "srv", rdNodeA, 445)

	if err := refused["bad"]; !errors.Is(err, settings.ErrInvalid) {
		t.Fatalf("refused[bad] = %v, want ErrInvalid for a default Validate refuses", err)
	}
}

func TestReadNewerSchema(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("srv", settings.At(entity.ScopeInstallation), rdServerDefault()),
		settings.Of("web", settings.At(entity.ScopeInstallation), rdServerDefault()),
	}

	s.raw(t, rdInstallation, "srv.port", rdRow("81", 2))
	s.raw(t, rdInstallation, "web.port", rdRow("82", 1))

	snap, refused := rdRead(t, s.nodes, table, rdNodeA)

	want := "setting (installation): srv.port: schema 2 is newer than this node reads, up to 1"
	if err := refused["srv"]; !errors.Is(err, settings.ErrNewerSchema) || err.Error() != want {
		t.Fatalf("refused[srv] = %v, want %q wrapping ErrNewerSchema", err, want)
	}

	if len(refused) != 1 {
		t.Fatalf("refused %v, want srv only", refused)
	}

	rdWantPort(t, snap, "web", rdNodeA, 82)
}

func TestReadOlderRow(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("srv", settings.At(entity.ScopeInstallation), rdServerDefault()),
	}

	s.raw(t, rdInstallation, "srv.port", rdRow("81", 0))

	snap, refused := rdRead(t, s.nodes, table, rdNodeA)
	rdWantNoRefusal(t, refused)

	got, err := settings.Get[rdServer](snap, "srv", rdNodeA)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	want := rdServer{
		Port: 81,
		Host: "default",
	}
	if got != want {
		t.Fatalf("Get: %+v, want %+v", got, want)
	}
}

func TestReadUnclaimedRows(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("srv", settings.At(entity.ScopeInstallation), rdServerDefault()),
	}

	s.put(t, rdInstallation, "srv.port", "81")
	s.put(t, rdInstallation, "nosuch", "1")
	s.put(t, rdInstallation, "gone.port", "1")

	snap, refused := rdRead(t, s.nodes, table, rdNodeA)
	rdWantNoRefusal(t, refused)
	rdWantPort(t, snap, "srv", rdNodeA, 81)

	_, err := settings.Get[rdServer](snap, "gone", rdNodeA)
	if !errors.Is(err, settings.ErrUnknownKey) || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("Get gone: %v, want ErrUnknownKey naming gone", err)
	}
}

func TestReadWrongScopeRow(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("srv", settings.At(entity.ScopeInstallation), rdServerDefault()),
		settings.Of("quota", settings.At(entity.ScopeShare), rdServerDefault()),
	}

	s.put(t, rdInstallation, "srv.port", "80")
	s.put(t, rdNodeScope(rdNodeA), "srv.port", "99")
	s.put(t, rdInstallation, "quota.port", "98")

	snap, refused := rdRead(t, s.nodes, table, rdNodeA)
	rdWantNoRefusal(t, refused)
	rdWantPort(t, snap, "srv", rdNodeA, 80)

	got, err := settings.InstanceValue(snap, "quota", [16]byte{})
	if err != nil || got.(rdServer).Port != 445 {
		t.Fatalf("quota with no share row: %v, %v, want the default port 445", got, err)
	}
}

func TestReadGarbageRow(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("srv", settings.At(entity.ScopeInstallation), rdServerDefault()),
		settings.Of("web", settings.At(entity.ScopeInstallation), rdServerDefault()),
	}

	s.raw(t, rdInstallation, "srv.port", []byte{0xFF, 0x00, 0x13})
	s.put(t, rdInstallation, "web.port", "8080")

	snap, refused := rdRead(t, s.nodes, table, rdNodeA)

	if err := refused["srv"]; !errors.Is(err, settings.ErrInvalid) {
		t.Fatalf("refused[srv] = %v, want ErrInvalid", err)
	}

	if len(refused) != 1 {
		t.Fatalf("refused %v, want srv only", refused)
	}

	rdWantPort(t, snap, "web", rdNodeA, 8080)

	// Every refused row of one level is named, not only the first.
	s.raw(t, rdInstallation, "srv.host", []byte{0xFF})

	_, refused = rdRead(t, s.nodes, table, rdNodeA)

	lines := strings.Split(refused["srv"].Error(), "\n")
	if len(lines) != 2 {
		t.Fatalf("refused[srv] lines %q, want srv.host's and srv.port's", lines)
	}

	// A key of an unknown scope kind does not parse: it is skipped.
	s2 := newRdStore(t)
	s2.rawKey(t, keys.String([]byte{keys.KindCFG, 0x09}, []byte("srv.port")), []byte{0xFF})
	s2.put(t, rdInstallation, "srv.port", "81")

	snap, refused = rdRead(t, s2.nodes, table, rdNodeA)
	rdWantNoRefusal(t, refused)
	rdWantPort(t, snap, "srv", rdNodeA, 81)
}

func TestReadUndeclaredField(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("srv", settings.At(entity.ScopeInstallation), rdServerDefault()),
		settings.Of("web", settings.At(entity.ScopeInstallation), rdServerDefault()),
	}

	s.put(t, rdInstallation, "srv.nosuch", "1")
	s.put(t, rdInstallation, "web.port", "8080")

	snap, refused := rdRead(t, s.nodes, table, rdNodeA)

	if err := refused["srv"]; !errors.Is(err, settings.ErrUnknownKey) {
		t.Fatalf("refused[srv] = %v, want ErrUnknownKey", err)
	}

	if len(refused) != 1 {
		t.Fatalf("refused %v, want srv only", refused)
	}

	rdWantPort(t, snap, "web", rdNodeA, 8080)
}

// rdScanFault passes AllSettings through to the real store, counting the
// calls, and, when after is set, ends the walk with fail after that many
// rows.
type rdScanFault struct {
	r     entity.SettingsReader
	calls int
	rows  int
	after int
	fail  error
}

func (f *rdScanFault) AllSettings(
	ctx context.Context,
	after entity.SettingKey,
) iter.Seq2[entity.Setting, error] {
	f.calls++

	return func(yield func(entity.Setting, error) bool) {
		for s, err := range f.r.AllSettings(ctx, after) {
			if f.fail != nil && f.rows == f.after {
				yield(entity.Setting{}, f.fail)

				return
			}

			f.rows++

			if !yield(s, err) {
				return
			}
		}
	}
}

func TestReadScanFailure(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("srv", settings.At(entity.ScopeInstallation), rdServerDefault()),
		settings.Of("local", settings.At(entity.ScopeNode), rdServerDefault()),
	}

	s.put(t, rdInstallation, "srv.host", `"h"`)
	s.put(t, rdInstallation, "srv.port", "81")

	healthy := &rdScanFault{
		r: s.nodes,
	}

	snap, _ := rdRead(t, healthy, table, rdNodeA)
	rdWantPort(t, snap, "srv", rdNodeA, 81)

	if healthy.calls != 1 {
		t.Fatalf("AllSettings calls %d, want 1", healthy.calls)
	}

	gone := errors.New("disk gone")
	failing := &rdScanFault{
		r:     s.nodes,
		after: 1,
		fail:  gone,
	}

	snap, refused, err := settings.Read(context.Background(), failing, table, rdNodeA)
	if !errors.Is(err, gone) || snap != nil || refused != nil {
		t.Fatalf("Read = %v, %v, %v, want nil, nil, the scan error", snap, refused, err)
	}
}

func TestReadBeyondOnePage(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("srv", settings.At(entity.ScopeInstallation), rdServerDefault()),
		settings.Of("zz", settings.At(entity.ScopeShare), rdServerDefault()),
	}

	kinds := []entity.ScopeKind{
		entity.ScopeInstallation,
		entity.ScopeNode,
		entity.ScopeNamespace,
		entity.ScopeRemote,
		entity.ScopeShare,
	}

	s.put(t, rdInstallation, "srv.port", "81")

	for i := range 300 {
		scope := entity.SettingScope{
			Kind: kinds[i%len(kinds)],
		}
		if scope.Kind != entity.ScopeInstallation {
			scope.ID = [16]byte{byte(i % 7), 1}
		}

		s.put(t, scope, fmt.Sprintf("filler.k%03d", i), "1")
	}

	last := rdShareScope(0xFF)
	s.put(t, last, "zz.port", "99")

	walk := &rdScanFault{
		r: s.nodes,
	}

	snap, refused := rdRead(t, walk, table, rdNodeA)
	rdWantNoRefusal(t, refused)
	rdWantPort(t, snap, "srv", rdNodeA, 81)

	if walk.rows != 302 {
		t.Fatalf("rows read %d, want 302", walk.rows)
	}

	got, err := settings.InstanceValue(snap, "zz", last.ID)
	if err != nil || got.(rdServer).Port != 99 {
		t.Fatalf("zz of the last share: %v, %v, want port 99", got, err)
	}
}

func TestGet(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("srv", settings.At(entity.ScopeInstallation), rdServerDefault()),
		settings.Of("bad", settings.At(entity.ScopeInstallation), rdServerDefault()),
		settings.Of("local", settings.At(entity.ScopeNode), rdServerDefault()),
		settings.Of("quota", settings.At(entity.ScopeShare), rdServerDefault()),
		settings.Of("ovr", settings.Override(entity.ScopeInstallation, entity.ScopeNode),
			rdServerDefault()),
		settings.Of("spare", settings.PerThing(entity.ScopeNode, "journal"), rdSpare{}),
		settings.Of("rehome", settings.Override(entity.ScopeInstallation, entity.ScopeShare),
			rdServerDefault()),
	}

	s.put(t, rdInstallation, "srv.port", "81")
	s.put(t, rdInstallation, "bad.port", "0")
	s.put(t, rdNodeScope(rdNodeA), "local.port", "82")
	s.put(t, rdNodeScope(rdNodeA), "ovr.port", "83")
	s.put(t, rdInstallation, "rehome.port", "84")
	s.put(t, rdShareScope(0xA), "rehome.port", "85")

	snap, refused := rdRead(t, s.nodes, table, rdNodeA)

	lists := newRdStore(t)
	lists.put(t, rdInstallation, "list.items", `["a"]`)

	listSnap, _ := rdRead(t, lists.nodes, settings.Table{
		settings.Of("list", settings.At(entity.ScopeInstallation), rdList{}),
	}, rdNodeA)

	t.Run("valid", func(t *testing.T) {
		rdWantPort(t, snap, "srv", rdNodeA, 81)
		rdWantPort(t, snap, "srv", rdNodeB, 81)
		rdWantPort(t, snap, "local", rdNodeA, 82)
	})

	t.Run("refused", func(t *testing.T) {
		got, err := settings.Get[rdServer](snap, "bad", rdNodeA)
		if err == nil || err != refused["bad"] || got != (rdServer{}) {
			t.Fatalf("Get bad: %+v, %v, want the zero value and %v", got, err, refused["bad"])
		}
	})

	t.Run("absent", func(t *testing.T) {
		_, err := settings.Get[rdServer](snap, "nosuch", rdNodeA)
		if !errors.Is(err, settings.ErrUnknownKey) || !strings.Contains(err.Error(), "nosuch") {
			t.Fatalf("Get nosuch: %v, want ErrUnknownKey naming nosuch", err)
		}
	})

	t.Run("wrong type", func(t *testing.T) {
		got, err := settings.Get[rdPeer](snap, "srv", rdNodeA)
		if err == nil || got != (rdPeer{}) {
			t.Fatalf("Get srv as rdPeer: %+v, %v, want an error", got, err)
		}
	})

	t.Run("other node", func(t *testing.T) {
		for _, section := range []string{"local", "ovr"} {
			_, err := settings.Get[rdServer](snap, section, rdNodeB)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%x", rdNodeA)) ||
				!strings.Contains(err.Error(), fmt.Sprintf("%x", rdNodeB)) {
				t.Fatalf("Get %s for node B: %v, want an error naming both nodes", section, err)
			}
		}
	})

	t.Run("copy", func(t *testing.T) {
		got, err := settings.Get[rdList](listSnap, "list", rdNodeA)
		if err != nil {
			t.Fatalf("Get list: %v", err)
		}

		got.Items[0] = "changed"

		again, err := settings.Get[rdList](listSnap, "list", rdNodeA)
		if err != nil || again.Items[0] != "a" {
			t.Fatalf("Get list after the caller changed its copy: %+v, %v, want items [a]",
				again, err)
		}
	})

	t.Run("per instance", func(t *testing.T) {
		_, err := settings.Get[rdServer](snap, "quota", rdNodeA)
		if err == nil || !strings.Contains(err.Error(), "per instance") {
			t.Fatalf("Get quota: %v, want an error saying it is per instance", err)
		}

		got, err := settings.Get[rdSpare](snap, "spare", rdNodeA)
		if err == nil || got != (rdSpare{}) {
			t.Fatalf("Get spare: %+v, %v, want an error, never the defaults", got, err)
		}

		wide, err := settings.Get[rdServer](snap, "rehome", rdNodeA)
		if err == nil || !strings.Contains(err.Error(), "per instance") || wide != (rdServer{}) {
			t.Fatalf("Get rehome: %+v, %v, want the per instance error and the zero value",
				wide, err)
		}
	})

	t.Run("no snapshot", func(t *testing.T) {
		_, err := settings.Get[rdServer](nil, "srv", rdNodeA)
		if !errors.Is(err, settings.ErrUnknownKey) {
			t.Fatalf("Get from no snapshot: %v, want ErrUnknownKey", err)
		}
	})
}

func TestReadOverrideWide(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("rehome", settings.Override(entity.ScopeInstallation, entity.ScopeShare),
			rdServerDefault()),
	}

	s.put(t, rdInstallation, "rehome.port", "10")
	s.put(t, rdInstallation, "rehome.host", `"wide"`)
	s.put(t, rdShareScope(0xA), "rehome.port", "20")
	s.raw(t, rdShareScope(0xC), "rehome.port", []byte{0xFF})

	snap, refused := rdRead(t, s.nodes, table, rdNodeA)

	want := map[byte]uint16{
		0xA: 20,
		0xB: 10,
	}
	for id, port := range want {
		got, err := settings.InstanceValue(snap, "rehome", [16]byte{id})

		want := rdServer{
			Port: port,
			Host: "wide",
		}
		if err != nil || got != want {
			t.Fatalf("share %x: %v, %v, want %+v", id, got, err, want)
		}
	}

	_, err := settings.InstanceValue(snap, "rehome", [16]byte{0xC})
	if !errors.Is(err, settings.ErrInvalid) {
		t.Fatalf("share c: %v, want ErrInvalid", err)
	}

	if err := refused["rehome"]; !errors.Is(err, settings.ErrInvalid) ||
		!strings.Contains(err.Error(), "share 0c000000") {
		t.Fatalf("refused[rehome] = %v, want ErrInvalid naming share c", err)
	}
}

func TestReadSecretNotEchoed(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("cred", settings.At(entity.ScopeInstallation), rdCred{},
			settings.SecretRef("Ref", entity.SecretKind(1))),
		settings.Of("cred2", settings.At(entity.ScopeInstallation), rdCred{}),
		settings.Of("cred3", settings.At(entity.ScopeInstallation), rdCred{}),
	}

	s.put(t, rdInstallation, "cred.ref", `"PLAINTEXT-1"`)
	s.put(t, rdInstallation, "cred2.nosuch", `"PLAINTEXT-2"`)
	s.raw(t, rdInstallation, "cred3.limit", []byte("PLAINTEXT-3"))

	_, refused := rdRead(t, s.nodes, table, rdNodeA)

	if len(refused) != 3 {
		t.Fatalf("refused %v, want cred, cred2 and cred3", refused)
	}

	for section, err := range refused {
		if strings.Contains(err.Error(), "PLAINTEXT") {
			t.Fatalf("refused[%s] = %q prints a stored value", section, err.Error())
		}
	}
}

func TestReadPerInstance(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("quota", settings.At(entity.ScopeShare), rdServerDefault()),
	}

	s.raw(t, rdShareScope(0xA), "quota.port", []byte{0xFF})
	s.put(t, rdShareScope(0xB), "quota.port", "30")

	snap, _ := rdRead(t, s.nodes, table, rdNodeA)

	_, err := settings.InstanceValue(snap, "quota", [16]byte{0xA})
	if !errors.Is(err, settings.ErrInvalid) {
		t.Fatalf("share a: %v, want ErrInvalid", err)
	}

	got, err := settings.InstanceValue(snap, "quota", [16]byte{0xB})
	if err != nil || got.(rdServer).Port != 30 {
		t.Fatalf("share b: %v, %v, want port 30", got, err)
	}
}

func TestReadTwoRefused(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("quota", settings.At(entity.ScopeShare), rdServerDefault()),
	}

	s.raw(t, rdShareScope(0xA), "quota.port", rdRow("1", 2))
	s.raw(t, rdShareScope(0xB), "quota.port", []byte{0xFF})

	_, refused := rdRead(t, s.nodes, table, rdNodeA)

	err := refused["quota"]
	if !errors.Is(err, settings.ErrNewerSchema) || !errors.Is(err, settings.ErrInvalid) {
		t.Fatalf("refused[quota] = %v, want ErrNewerSchema and ErrInvalid", err)
	}

	lines := strings.Split(err.Error(), "\n")
	if len(lines) != 2 ||
		!strings.HasPrefix(lines[0], "setting (share 0a000000") ||
		!strings.HasPrefix(lines[1], "setting (share 0b000000") {
		t.Fatalf("refusal lines %q, want share a's then share b's", lines)
	}
}

func TestReadOtherNodeRows(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("local", settings.At(entity.ScopeNode), rdServerDefault()),
		settings.Of("peer", settings.At(entity.ScopeNode), rdServerDefault()),
	}

	s.raw(t, rdNodeScope(rdNodeB), "local.port", []byte{0xFF})
	s.raw(t, rdNodeScope(rdNodeB), "peer.port", rdRow("1", 2))

	snap, refused := rdRead(t, s.nodes, table, rdNodeA)
	rdWantNoRefusal(t, refused)
	rdWantPort(t, snap, "local", rdNodeA, 445)
	rdWantPort(t, snap, "peer", rdNodeA, 445)

	_, refused = rdRead(t, s.nodes, table, rdNodeB)

	if !errors.Is(refused["local"], settings.ErrInvalid) ||
		!errors.Is(refused["peer"], settings.ErrNewerSchema) {
		t.Fatalf("refused for node b %v, want local ErrInvalid and peer ErrNewerSchema", refused)
	}
}
