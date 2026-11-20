package node

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/internal/sidecar"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest/memory"
)

// The adapters' rows run the real adapters nfs.New and smb.New build, on the
// filesystem over memory and the node's own budgets, over loopback, with no
// node: both slots over one Deps and one runner, whose one sidecar, declared
// here, serves both protocols.

// loopback is the one address every test's adapters bind.
var loopback = netip.MustParseAddr("127.0.0.1")

// holder is the sidecar the tests declare: it holds what each Sync hands it,
// so a test reads the set the runner last synced (RFC 20 §3.5).
type holder struct {
	mu   sync.Mutex
	held []adapter.Serving
}

func (*holder) Name() string       { return "holder" }
func (*holder) Adapters() []string { return []string{"nfs", "smb"} }

func (h *holder) Sync(_ context.Context, serving []adapter.Serving) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.held = serving
	return nil
}

// holding is what the last Sync left the holder with.
func (h *holder) holding() []adapter.Serving {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.held
}

// faulting is the holder with a fault on cue: while cue is set, its Sync
// fails, as a sidecar that cannot be brought to match.
type faulting struct {
	*holder
	cue atomic.Bool
}

func (f *faulting) Sync(ctx context.Context, serving []adapter.Serving) error {
	if f.cue.Load() {
		return errors.New("the sidecar cannot be synced")
	}
	return f.holder.Sync(ctx, serving)
}

// logs is what the node's logger wrote, safe to write from every adapter's
// goroutines.
type logs struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// rig is what a test drives: the two slots, the sidecar their runner syncs,
// the node's clock, watchdog, registry and logs, and the loop each call is
// handed, the watchdog's.
type rig struct {
	nfs      *slot[settings.NFS]
	smb      *slot[settings.SMB]
	side     *faulting
	runner   *sidecar.Runner
	clock    *sim.TestClock
	watchdog *health.Watchdog
	reg      *metrics.Registry
	logs     *logs
	loop     *health.Loop
}

// newRig builds the slots on loopback, refusing the ports taken, its leak
// check armed over the node's budgets: called before the test starts
// anything. Both adapters are stopped at the test's end, before the check.
func newRig(t *testing.T, taken ...uint16) *rig {
	t.Helper()
	b, _ := newBudgets()
	adaptertest.CheckLeaks(t, b)
	rg := &rig{side: &faulting{holder: &holder{}}, clock: sim.NewTestClock(time.Unix(0, 0)), reg: metrics.New("test", "test"), logs: &logs{}}
	rg.watchdog = health.NewWatchdog(rg.clock)
	rg.loop = rg.watchdog.Loop(stepReconcile)
	rg.runner = sidecar.New(rg.side)
	log := slog.New(slog.NewTextHandler(rg.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d := adapterDeps(memory.New(t), b, rg.reg, log, rg.watchdog)
	rg.nfs, rg.smb = newSlots(d, rg.runner, []netip.Addr{loopback}, taken, rg.clock, context.Background())
	t.Cleanup(func() {
		rg.side.cue.Store(false)
		if r := rg.nfs.apply(context.Background(), rg.loop, 0, false, settings.NFS{}); r != nil {
			t.Errorf("stopping nfs at the end: refused %s", r.rec.Error)
		}
		if r := rg.smb.apply(context.Background(), rg.loop, 0, false, settings.SMB{}); r != nil {
			t.Errorf("stopping smb at the end: refused %s", r.rec.Error)
		}
		rg.runner.Close()
	})
	return rg
}

// smbAt is SMB's defaults at port; the slot supplies the addresses.
func smbAt(port uint16) settings.SMB {
	s := settings.DefaultSMB()
	s.Port = port
	return s
}

// nfsAt is NFS's defaults at port; the slot supplies the addresses.
func nfsAt(port uint16) settings.NFS {
	s := settings.DefaultNFS()
	s.Port = port
	return s
}

// portOf is the port a serves service on over TCP, failing the test unless it
// is bound on loopback alone.
func portOf(t *testing.T, a adapter.Adapter, service string) uint16 {
	t.Helper()
	if a == nil {
		t.Fatalf("%s: nothing serves", service)
	}
	at := a.Addrs()[adapter.Socket{Service: service, Network: "tcp"}]
	if len(at) != 1 || at[0].Addr() != loopback {
		t.Fatalf("%s over tcp is bound at %v, want one address on %s", service, at, loopback)
	}
	return at[0].Port()
}

// advertises reports whether the sidecar holds a's own listeners for name,
// as the runner builds them from Addrs.
func (rg *rig) advertises(name string, a adapter.Adapter) bool {
	for _, held := range rg.side.holding() {
		if held.Adapter == name {
			return maps.EqualFunc(held.Listeners, a.Addrs(), slices.Equal)
		}
	}
	return false
}

// freePort is a port nothing holds: one the system picked for a listener the
// test closed at once.
func freePort(t *testing.T) uint16 {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return uint16(ln.Addr().(*net.TCPAddr).Port)
}

// heldPort is a port the test holds: a listener on loopback, at a port the
// system picked, closed by free or at the test's end.
func heldPort(t *testing.T) (port uint16, free func()) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	free = func() { _ = ln.Close() }
	t.Cleanup(free)
	return uint16(ln.Addr().(*net.TCPAddr).Port), free
}

// dial connects to port on loopback, every read and write bounded by a
// deadline as a backstop for a server that neither answers nor closes.
func dial(t *testing.T, port uint16) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", netip.AddrPortFrom(loopback, port).String())
	if err != nil {
		t.Fatalf("dial port %d: %v", port, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return c
}

// refuses reports whether a dial to port on loopback fails: nothing listens
// there.
func refuses(port uint16) bool {
	c, err := net.DialTimeout("tcp", netip.AddrPortFrom(loopback, port).String(), 5*time.Second)
	if err != nil {
		return true
	}
	_ = c.Close()
	return false
}

// negotiate sends an SMB2 NEGOTIATE under message ID id on c and returns nil
// once an SMB2 response to it is read. The header is written field by field
// from MS-SMB2 2.2.1.2, the body from 2.2.3 offering dialect 2.1, in a Direct
// TCP record (MS-SMB2 2.1).
func negotiate(c net.Conn, id uint64) error {
	msg := make([]byte, 64+36+2)
	copy(msg, "\xfeSMB")                       // ProtocolId
	binary.LittleEndian.PutUint16(msg[4:], 64) // StructureSize
	binary.LittleEndian.PutUint16(msg[6:], 1)  // CreditCharge
	binary.LittleEndian.PutUint16(msg[14:], 1) // CreditRequest; Command 0 is NEGOTIATE
	binary.LittleEndian.PutUint64(msg[24:], id)
	binary.LittleEndian.PutUint16(msg[64:], 36)      // the body's StructureSize
	binary.LittleEndian.PutUint16(msg[66:], 1)       // DialectCount
	binary.LittleEndian.PutUint16(msg[100:], 0x0210) // Dialects[0]
	n := len(msg)
	if _, err := c.Write(append([]byte{0, byte(n >> 16), byte(n >> 8), byte(n)}, msg...)); err != nil {
		return err
	}
	var hdr [4]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return fmt.Errorf("reading a reply's Direct TCP header: %w", err)
	}
	rep := make([]byte, int(hdr[1])<<16|int(hdr[2])<<8|int(hdr[3]))
	if _, err := io.ReadFull(c, rep); err != nil {
		return fmt.Errorf("reading a reply of %d bytes: %w", len(rep), err)
	}
	const serverToRedir = 1 // Flags: a response
	if len(rep) < 64 || string(rep[:4]) != "\xfeSMB" || binary.LittleEndian.Uint32(rep[16:])&serverToRedir == 0 || binary.LittleEndian.Uint64(rep[24:]) != id {
		return fmt.Errorf("answered % x, want an SMB2 response under message ID %d", rep[:min(len(rep), 64)], id)
	}
	return nil
}

// RFC 5531's numbers the NULL calls use: the record mark's last-fragment bit
// (§11), NFS's program and PROG_MISMATCH's accept state (§9, Appendix A).
const (
	lastFragment = 1 << 31
	progNFS      = 100003
	progMismatch = 2
)

// null sends an RPC call (RFC 5531 §9) to NFS's procedure 0, NULL, of version
// vers under xid on c, in a record of one fragment with AUTH_NONE's empty
// credential and verifier, and returns the accepted reply's accept state and
// the words after it.
func null(c net.Conn, xid, vers uint32) ([]uint32, error) {
	var call []byte
	for _, w := range []uint32{lastFragment | 40, xid, 0, 2, progNFS, vers, 0, 0, 0, 0, 0} {
		call = binary.BigEndian.AppendUint32(call, w)
	}
	if _, err := c.Write(call); err != nil {
		return nil, err
	}
	var mark [4]byte
	if _, err := io.ReadFull(c, mark[:]); err != nil {
		return nil, fmt.Errorf("reading a reply's record mark: %w", err)
	}
	body := make([]byte, binary.BigEndian.Uint32(mark[:])&^lastFragment)
	if _, err := io.ReadFull(c, body); err != nil {
		return nil, fmt.Errorf("reading a reply of %d bytes: %w", len(body), err)
	}
	var ws []uint32
	for i := 0; i+4 <= len(body); i += 4 {
		ws = append(ws, binary.BigEndian.Uint32(body[i:]))
	}
	// xid, REPLY, MSG_ACCEPTED, the verifier's flavor and its empty body.
	if len(ws) < 6 || ws[0] != xid || ws[1] != 1 || ws[2] != 0 || ws[4] != 0 {
		return nil, fmt.Errorf("answered %x, want an accepted reply to xid %d with an empty verifier", body, xid)
	}
	return ws[5:], nil
}

// describe is what a test reports of a call's refusal.
func describe(r *refusal) string {
	if r == nil {
		return "applied"
	}
	return fmt.Sprintf("refused %s %q, retry %v: %s", r.rec.Record.Kind, r.rec.Record.ID, r.retry, r.rec.Error)
}

// Enabled at port 0 on loopback, SMB serves on a port the system picked, the
// sidecar holds what it serves, and a NEGOTIATE is answered. Applied again
// with the same settings it does nothing: the port stays, and a sidecar
// failing every Sync is not failed, so it was not synced (RFC 23 §2.3).
func TestAnEnabledAdapterServesAndAgainDoesNothing(t *testing.T) {
	rg := newRig(t)
	if r := rg.smb.apply(t.Context(), rg.loop, 0, true, smbAt(0)); r != nil {
		t.Fatalf("enable smb at port 0: refused %s", r.rec.Error)
	}
	port := portOf(t, rg.smb.running, "smb")
	if port == 0 || port == 12445 {
		t.Errorf("smb serves at port %d, want one the system picked", port)
	}
	if !rg.advertises("smb", rg.smb.running) {
		t.Errorf("the sidecar holds %v, want smb's %v", rg.side.holding(), rg.smb.running.Addrs())
	}
	if err := negotiate(dial(t, port), 1); err != nil {
		t.Errorf("a NEGOTIATE on port %d: %v", port, err)
	}

	rg.side.cue.Store(true)
	if r := rg.smb.apply(t.Context(), rg.loop, 0, true, smbAt(0)); r != nil {
		t.Fatalf("the same settings again: refused %s", r.rec.Error)
	}
	if again := portOf(t, rg.smb.running, "smb"); again != port {
		t.Errorf("the same settings again: smb serves at port %d, want %d still", again, port)
	}
	if failed := rg.runner.Failed(); len(failed) != 0 {
		t.Errorf("the same settings again synced the sidecar: %v failed, want none", failed)
	}
}

// Disabled, SMB's port refuses a dial and the sidecar holds nothing; enabled
// again, SMB serves from a new adapter, since a stopped one never starts
// again (RFC 20 §3.1, RFC 23 §2.3).
func TestADisabledAdapterStopsAndServesAnewWhenEnabled(t *testing.T) {
	rg := newRig(t)
	if r := rg.smb.apply(t.Context(), rg.loop, 0, true, smbAt(0)); r != nil {
		t.Fatalf("enable smb: refused %s", r.rec.Error)
	}
	port := portOf(t, rg.smb.running, "smb")
	if r := rg.smb.apply(t.Context(), rg.loop, 0, false, smbAt(0)); r != nil {
		t.Fatalf("disable smb: refused %s", r.rec.Error)
	}
	if !refuses(port) {
		t.Errorf("after the disable, port %d still accepts a dial", port)
	}
	if held := rg.side.holding(); len(held) != 0 {
		t.Errorf("after the disable the sidecar holds %v, want nothing", held)
	}
	if r := rg.smb.apply(t.Context(), rg.loop, 0, true, smbAt(0)); r != nil {
		t.Fatalf("enable smb again: refused %s", r.rec.Error)
	}
	if err := negotiate(dial(t, portOf(t, rg.smb.running, "smb")), 1); err != nil {
		t.Errorf("a NEGOTIATE after the second enable: %v", err)
	}
}

// NFS's Versions.Max lowered from 4.2 to 3 keeps its adapter on its port: an
// NFSv4 NULL on a connection accepted after the change is answered
// PROG_MISMATCH, 3 to 3, and on one accepted before it is not (RFC 20 §3.6).
// A connection counts as accepted once a call on it is answered: until the
// server accepts it, it waits in the listener's backlog. Applied again, the
// change does nothing: a sidecar failing every Sync is not failed. A further
// change goes through the runner, which syncs the sidecar after it: the
// sidecar is failed (RFC 20 §3.5, RFC 23 §2.3).
func TestALiveSettingReachesOnlyConnectionsAcceptedAfterIt(t *testing.T) {
	rg := newRig(t)
	if r := rg.nfs.apply(t.Context(), rg.loop, 0, true, nfsAt(0)); r != nil {
		t.Fatalf("enable nfs: refused %s", r.rec.Error)
	}
	port := portOf(t, rg.nfs.running, "nfs")
	before := dial(t, port)
	if _, err := null(before, 1, 3); err != nil {
		t.Fatalf("an NFSv3 NULL before the change: %v", err)
	}
	next := nfsAt(0)
	next.Versions.Max = "3"
	if r := rg.nfs.apply(t.Context(), rg.loop, 0, true, next); r != nil {
		t.Fatalf("Versions.Max 3: refused %s", r.rec.Error)
	}
	if got := portOf(t, rg.nfs.running, "nfs"); got != port {
		t.Errorf("after the change nfs serves at port %d, want %d still", got, port)
	}
	got, err := null(dial(t, port), 2, 4)
	if want := []uint32{progMismatch, 3, 3}; err != nil || !slices.Equal(got, want) {
		t.Errorf("an NFSv4 NULL accepted after: %v, %v; want accept state and versions %v", got, err, want)
	}
	got, err = null(before, 3, 4)
	if err != nil || len(got) == 0 || got[0] == progMismatch {
		t.Errorf("an NFSv4 NULL accepted before: %v, %v; want answered, not PROG_MISMATCH", got, err)
	}

	rg.side.cue.Store(true)
	if r := rg.nfs.apply(t.Context(), rg.loop, 0, true, next); r != nil {
		t.Fatalf("Versions.Max 3 again: refused %s", r.rec.Error)
	}
	if failed := rg.runner.Failed(); len(failed) != 0 {
		t.Errorf("Versions.Max 3 again synced the sidecar: %v failed, want none", failed)
	}
	next.Versions.Max = "4.2"
	if r := rg.nfs.apply(t.Context(), rg.loop, 0, true, next); r != nil {
		t.Fatalf("Versions.Max back to 4.2: refused %s", r.rec.Error)
	}
	if failed := rg.runner.Failed(); !slices.Equal(failed, []string{"holder"}) {
		t.Errorf("Versions.Max back to 4.2: %v failed, want the holder, synced through the runner", failed)
	}
}

// SMB's port changed to a free one: SMB serves on it and its old port refuses
// a dial, the sidecar holds the new one, and an NFS connection opened before
// the change has a NULL answered after it. Applied again, the change does
// nothing: a connection on the new port stays open (RFC 23 §2.3, §3.4).
func TestAPortChangeRebuildsThatAdapterAlone(t *testing.T) {
	rg := newRig(t)
	if r := rg.nfs.apply(t.Context(), rg.loop, 0, true, nfsAt(0)); r != nil {
		t.Fatalf("enable nfs: refused %s", r.rec.Error)
	}
	client := dial(t, portOf(t, rg.nfs.running, "nfs"))
	if r := rg.smb.apply(t.Context(), rg.loop, 0, true, smbAt(0)); r != nil {
		t.Fatalf("enable smb: refused %s", r.rec.Error)
	}
	old, port := portOf(t, rg.smb.running, "smb"), freePort(t)
	if r := rg.smb.apply(t.Context(), rg.loop, 0, true, smbAt(port)); r != nil {
		t.Fatalf("smb to port %d: refused %s", port, r.rec.Error)
	}
	if got := portOf(t, rg.smb.running, "smb"); got != port {
		t.Errorf("after the change smb serves at port %d, want %d", got, port)
	}
	c := dial(t, port)
	if err := negotiate(c, 1); err != nil {
		t.Errorf("a NEGOTIATE on the new port: %v", err)
	}
	if !refuses(old) {
		t.Errorf("after the change the old port %d still accepts a dial", old)
	}
	if !rg.advertises("smb", rg.smb.running) {
		t.Errorf("the sidecar holds %v, want smb's %v", rg.side.holding(), rg.smb.running.Addrs())
	}
	if _, err := null(client, 1, 3); err != nil {
		t.Errorf("an NFSv3 NULL on the connection opened before the change: %v", err)
	}
	if r := rg.smb.apply(t.Context(), rg.loop, 0, true, smbAt(port)); r != nil {
		t.Fatalf("smb at port %d again: refused %s", port, r.rec.Error)
	}
	if err := negotiate(c, 2); err != nil {
		t.Errorf("a NEGOTIATE after the change applied again, on the connection opened before: %v", err)
	}
}

// SMB's port changed to a port the test holds is refused, not retried, and
// SMB serves again on the port it served on, the sidecar holding it. Applied
// again, the same refusal comes back and nothing moves: a connection stays
// open, and a sidecar failing every Sync is not failed. Once the records
// moved back and the port is freed, the change applies (RFC 23 §2.3).
func TestAPortChangeOntoAHeldPortFallsBack(t *testing.T) {
	rg := newRig(t)
	port := freePort(t)
	held, free := heldPort(t)
	if r := rg.smb.apply(t.Context(), rg.loop, 0, true, smbAt(port)); r != nil {
		t.Fatalf("enable smb at port %d: refused %s", port, r.rec.Error)
	}
	r := rg.smb.apply(t.Context(), rg.loop, 0, true, smbAt(held))
	want := entity.ResourceRef{Kind: entity.ResourceAdapter, ID: []byte("smb")}
	if r == nil || r.retry || r.rec.Record.Kind != want.Kind || !bytes.Equal(r.rec.Record.ID, want.ID) {
		t.Fatalf("smb to the held port %d: %s, want smb's adapter record refused without retry", held, describe(r))
	}
	if got := portOf(t, rg.smb.running, "smb"); got != port {
		t.Errorf("after the refusal smb serves at port %d, want %d", got, port)
	}
	c := dial(t, port)
	if err := negotiate(c, 1); err != nil {
		t.Errorf("a NEGOTIATE on the port smb served on: %v", err)
	}
	if !rg.advertises("smb", rg.smb.running) {
		t.Errorf("the sidecar holds %v, want smb's %v", rg.side.holding(), rg.smb.running.Addrs())
	}

	rg.side.cue.Store(true)
	if again := rg.smb.apply(t.Context(), rg.loop, 0, true, smbAt(held)); describe(again) != describe(r) {
		t.Errorf("the held port again: %s, want %s again", describe(again), describe(r))
	}
	if failed := rg.runner.Failed(); len(failed) != 0 {
		t.Errorf("the held port again synced the sidecar: %v failed, want none", failed)
	}
	if err := negotiate(c, 2); err != nil {
		t.Errorf("a NEGOTIATE after the held port again: %v", err)
	}
	rg.side.cue.Store(false)

	if r := rg.smb.apply(t.Context(), rg.loop, 0, true, smbAt(port)); r != nil {
		t.Fatalf("smb back to port %d: refused %s", port, r.rec.Error)
	}
	free()
	if r := rg.smb.apply(t.Context(), rg.loop, 0, true, smbAt(held)); r != nil {
		t.Errorf("smb to port %d once freed: %s, want applied", held, describe(r))
	}
}

// SMB disabled while its change to a port the test holds stands refused: the
// disable is applied, not answered with that refusal, and SMB's port refuses
// a dial (RFC 23 §2.3).
func TestADisableAfterARefusalStopsTheAdapter(t *testing.T) {
	rg := newRig(t)
	held, _ := heldPort(t)
	if r := rg.smb.apply(t.Context(), rg.loop, 0, true, smbAt(0)); r != nil {
		t.Fatalf("enable smb: refused %s", r.rec.Error)
	}
	port := portOf(t, rg.smb.running, "smb")
	if r := rg.smb.apply(t.Context(), rg.loop, 0, true, smbAt(held)); r == nil {
		t.Fatalf("smb to the held port %d: applied, want refused", held)
	}
	if r := rg.smb.apply(t.Context(), rg.loop, 0, false, smbAt(held)); r != nil {
		t.Fatalf("disable smb: %s, want applied", describe(r))
	}
	if !refuses(port) {
		t.Errorf("after the disable, port %d still accepts a dial", port)
	}
}

// An enable on a port the test holds, with nothing serving SMB, raises
// adapter_refused naming it, and a disable then clears it: the condition
// stands only for an enabled adapter nothing serves (RFC 20 §3.7).
func TestADisableClearsAnAdapterRefused(t *testing.T) {
	rg := newRig(t)
	held, _ := heldPort(t)
	refused := func() bool {
		return slices.ContainsFunc(rg.smb.conditions(nil, health.Subject{}), func(c health.Condition) bool {
			return c.Name == "adapter_refused" && c.Subject.Name == "smb"
		})
	}
	if r := rg.smb.apply(t.Context(), rg.loop, 0, true, smbAt(held)); r == nil {
		t.Fatalf("enable smb on the held port %d: applied, want refused", held)
	}
	if !refused() {
		t.Fatal("smb refused with nothing serving it raises no adapter_refused")
	}
	if r := rg.smb.apply(t.Context(), rg.loop, 0, false, smbAt(held)); r != nil {
		t.Fatalf("disable smb: %s, want applied", describe(r))
	}
	if refused() {
		t.Error("smb disabled after its refusal still raises adapter_refused")
	}
}

// An enable on a port the test holds, with nothing serving SMB, is refused
// and retried, and the retry serves once the port is freed (RFC 23 §2.3).
func TestAnEnableOnAHeldPortIsRetried(t *testing.T) {
	rg := newRig(t)
	held, free := heldPort(t)
	if r := rg.smb.apply(t.Context(), rg.loop, 0, true, smbAt(held)); r == nil || !r.retry {
		t.Fatalf("enable smb on the held port %d: %s, want refused and retried", held, describe(r))
	}
	free()
	if r := rg.smb.apply(t.Context(), rg.loop, 0, true, smbAt(held)); r != nil {
		t.Fatalf("the retry once port %d is freed: %s, want applied", held, describe(r))
	}
	if err := negotiate(dial(t, held), 1); err != nil {
		t.Errorf("a NEGOTIATE on port %d: %v", held, err)
	}
}

// failingStart is a real adapter with a fault on cue: while cue is set, its
// Start fails, binding nothing, as one whose port another socket holds.
type failingStart struct {
	adapter.Adapter
	cue *atomic.Bool
}

func (f failingStart) Start(ctx context.Context) error {
	if f.cue.Load() {
		return errors.New("the port is held")
	}
	return f.Adapter.Start(ctx)
}

// SMB's port changed while every start fails: the old adapter is stopped,
// the new one's start fails and so does the start of one with the settings
// that last served, so nothing serves SMB, and the change is refused and
// retried (RFC 23 §2.3).
func TestAPortChangeWhoseFallbackFailsIsRetried(t *testing.T) {
	rg := newRig(t)
	var fails atomic.Bool
	build := rg.smb.build
	rg.smb.build = func(d adapter.Deps, s settings.SMB) (adapter.Adapter, error) {
		a, err := build(d, s)
		if err != nil {
			return nil, err
		}
		return failingStart{a, &fails}, nil
	}
	if r := rg.smb.apply(t.Context(), rg.loop, 0, true, smbAt(0)); r != nil {
		t.Fatalf("enable smb: refused %s", r.rec.Error)
	}
	fails.Store(true)
	if r := rg.smb.apply(t.Context(), rg.loop, 0, true, smbAt(freePort(t))); r == nil || !r.retry {
		t.Fatalf("a port change whose start and fallback both fail: %s, want refused and retried", describe(r))
	}
	if rg.smb.running != nil {
		t.Errorf("after both starts failed smb runs %v; want nothing serving", rg.smb.running)
	}
}

// SMB's port changed together with a DrainTimeout Validate refuses: refused,
// not retried, and the old adapter serves on its old port, its connection
// open (RFC 23 §2.3: New accepts the settings before the old one drains).
func TestARefusedNewLeavesTheOldAdapterServing(t *testing.T) {
	rg := newRig(t)
	port := freePort(t)
	if r := rg.smb.apply(t.Context(), rg.loop, 0, true, smbAt(port)); r != nil {
		t.Fatalf("enable smb: refused %s", r.rec.Error)
	}
	c := dial(t, port)
	if err := negotiate(c, 1); err != nil {
		t.Fatalf("a NEGOTIATE before the change: %v", err)
	}
	next := smbAt(freePort(t))
	next.DrainTimeout = time.Second
	r := rg.smb.apply(t.Context(), rg.loop, 0, true, next)
	if r == nil || r.retry || !strings.Contains(string(r.rec.Error), "DrainTimeout") {
		t.Fatalf("a new port with DrainTimeout 1s: %s, want refused naming DrainTimeout, without retry", describe(r))
	}
	if got := portOf(t, rg.smb.running, "smb"); got != port {
		t.Errorf("after the refusal smb serves at port %d, want %d", got, port)
	}
	if err := negotiate(c, 2); err != nil {
		t.Errorf("a NEGOTIATE on the connection opened before: %v", err)
	}
}

// A port that is the API's is refused before anything is built, though no
// socket on loopback holds it: SMB serves on, at its old port, its
// connection open, and the API's port refuses a dial (RFC 13 §6).
func TestTheAPIsPortIsRefused(t *testing.T) {
	api := freePort(t)
	rg := newRig(t, api)
	if r := rg.smb.apply(t.Context(), rg.loop, 0, true, smbAt(0)); r != nil {
		t.Fatalf("enable smb: refused %s", r.rec.Error)
	}
	port := portOf(t, rg.smb.running, "smb")
	c := dial(t, port)
	if r := rg.smb.apply(t.Context(), rg.loop, 0, true, smbAt(api)); r == nil || r.retry {
		t.Fatalf("smb to the API's port %d: %s, want refused without retry", api, describe(r))
	}
	if got := portOf(t, rg.smb.running, "smb"); got != port {
		t.Errorf("after the refusal smb serves at port %d, want %d", got, port)
	}
	if err := negotiate(c, 1); err != nil {
		t.Errorf("a NEGOTIATE on the connection opened before: %v", err)
	}
	if !refuses(api) {
		t.Errorf("the API's port %d accepts a dial, want nothing bound there", api)
	}
}

// Port 0 is never the API's or the metrics listener's, though one of them
// listening on a port the system picked names 0: SMB at port 0 serves on a
// port the system picks for it (RFC 13 §6).
func TestPortZeroIsNeverTaken(t *testing.T) {
	rg := newRig(t, 0)
	if r := rg.smb.apply(t.Context(), rg.loop, 0, true, smbAt(0)); r != nil {
		t.Fatalf("enable smb at port 0 beside a listener at port 0: %s, want applied", describe(r))
	}
	if err := negotiate(dial(t, portOf(t, rg.smb.running, "smb")), 1); err != nil {
		t.Errorf("a NEGOTIATE: %v", err)
	}
}

// heldStop is a real adapter with a fault on cue: its Stop drains and stops
// the adapter, tells stopped, then holds until release closes, as a drain
// that runs long.
type heldStop struct {
	adapter.Adapter
	stopped chan<- struct{}
	release <-chan struct{}
}

func (h heldStop) Stop(ctx context.Context) error {
	err := h.Adapter.Stop(ctx)
	h.stopped <- struct{}{}
	<-h.release
	return err
}

// A disable waiting on a drain that runs two minutes on the node's clock
// reports its loop idle within every 30 s of it, RFC 25 §3.5's cadence for a
// waiting loop, and returns once the drain ends. The loop's watchdog reads a
// clock of its own, moved past the 60 s stall before each 30 s of the node's,
// so only an idle report landing inside those 30 s keeps the loop live.
func TestADrainWaitKeepsTheLoopLive(t *testing.T) {
	rg := newRig(t)
	stopped, release := make(chan struct{}, 1), make(chan struct{})
	build := rg.smb.build
	rg.smb.build = func(d adapter.Deps, s settings.SMB) (adapter.Adapter, error) {
		a, err := build(d, s)
		if err != nil {
			return nil, err
		}
		return heldStop{a, stopped, release}, nil
	}
	if r := rg.smb.apply(t.Context(), rg.loop, 0, true, smbAt(0)); r != nil {
		t.Fatalf("enable smb: refused %s", r.rec.Error)
	}
	watched := sim.NewTestClock(time.Unix(0, 0))
	watchdog := health.NewWatchdog(watched)
	loop := watchdog.Loop(stepReconcile)
	done := make(chan *refusal, 1)
	go func() { done <- rg.smb.apply(context.Background(), loop, 0, false, smbAt(0)) }()
	t.Cleanup(func() {
		close(release)
		if r := <-done; r != nil {
			t.Errorf("the disable: refused %s", r.rec.Error)
		}
	})
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the disable never stopped SMB")
	}
	// Once the clock has moved, the wait it woke is armed again only after
	// its idle report.
	armed := func() { waitFor(t, "the drain's wait on the clock", func() bool { return rg.clock.Pending() == 1 }) }
	armed()
	for range 4 {
		watched.Advance(health.Stall + time.Second)
		rg.clock.Advance(health.IdleEvery)
		armed()
		if !watchdog.Live() {
			t.Errorf("%v into the drain the loop reported no idle within the last %v", rg.clock.Elapsed(), health.IdleEvery)
			return
		}
	}
}

// Every adapter registers with the node's watchdog, records in its registry
// and logs to its logger: SMB's accept loop holds liveness back once the
// clock passes 60 s and lets it go once SMB stops, its answer counts under
// smb, and its connection's close is logged (RFC 20 §3.4, §3.7). The accept
// loop reports idle every 10 s on the runtime's clock, which no adapter takes
// from Deps. The test runs in a bubble, whose runtime clock moves only while
// every goroutine in it waits on another: the accept loop, waiting on its
// socket, never does, so it reports no idle while SMB serves.
func TestAnAdapterReportsThroughTheNode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rg := newRig(t)
		if r := rg.smb.apply(t.Context(), rg.loop, 0, true, smbAt(0)); r != nil {
			t.Fatalf("enable smb: refused %s", r.rec.Error)
		}
		c := dial(t, portOf(t, rg.smb.running, "smb"))
		if err := negotiate(c, 1); err != nil {
			t.Fatalf("a NEGOTIATE: %v", err)
		}
		_ = c.Close()
		rg.clock.Advance(61 * time.Second)
		rg.loop.Turn()
		if rg.watchdog.Live() {
			t.Error("61 s on, the node is live, want SMB's accept loop, which reports on its own clock, watched and stalled")
		}
		if r := rg.smb.apply(t.Context(), rg.loop, 0, false, smbAt(0)); r != nil {
			t.Fatalf("disable smb: refused %s", r.rec.Error)
		}
		if !rg.watchdog.Live() {
			t.Error("with SMB stopped the node is not live, want its accept loop no longer watched")
		}
		mfs, err := rg.reg.Registry().Gather()
		if err != nil {
			t.Fatal(err)
		}
		counted := false
		for _, mf := range mfs {
			for _, m := range mf.GetMetric() {
				for _, lp := range m.GetLabel() {
					counted = counted || strings.HasPrefix(mf.GetName(), "dittofs_adapter_") && lp.GetName() == "protocol" && lp.GetValue() == "smb"
				}
			}
		}
		if !counted {
			t.Error("the node's registry holds no adapter series under protocol smb")
		}
		if l := rg.logs.String(); !strings.Contains(l, `msg="a connection closed"`) || !strings.Contains(l, "protocol=smb") {
			t.Errorf("the node's log is %q, want smb's connection close", l)
		}
	})
}

// The slot's outcome is read from goroutines other than the calls', as the
// node's health, readiness and status read it, while the calls store it:
// under the race detector, a read the outcome does not order after the store
// fails here.
func TestAnOutcomeIsReadWhileTheCallsStoreIt(t *testing.T) {
	rg := newRig(t)
	stop, read := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(read)
		for {
			select {
			case <-stop:
				return
			default:
				rg.smb.conditions(nil, health.Subject{})
			}
		}
	}()
	for _, enabled := range []bool{true, false} {
		if r := rg.smb.apply(t.Context(), rg.loop, 0, enabled, smbAt(0)); r != nil {
			t.Errorf("smb enabled %t: refused %s", enabled, r.rec.Error)
		}
	}
	close(stop)
	<-read
}

// listenAddrs reads an unset adapters.listen as 0.0.0.0, with :: beside it
// only where tcp6 opens; a list that is set is bound as written; and each
// literal that does not parse is refused, all of them together, naming the
// key and its index but never the value (RFC 13 §6, Appendix C).
func TestListenAddrs(t *testing.T) {
	any4, any6 := netip.IPv4Unspecified(), netip.IPv6Unspecified()
	for _, tc := range []struct {
		name   string
		listen []string
		tcp6   bool
		want   []netip.Addr
	}{
		{"unset, tcp6 opens", nil, true, []netip.Addr{any4, any6}},
		{"unset, tcp6 does not open", nil, false, []netip.Addr{any4}},
		{"set", []string{"::", "192.0.2.1"}, false, []netip.Addr{any6, netip.MustParseAddr("192.0.2.1")}},
	} {
		got, err := listenAddrs(tc.listen, tc.tcp6)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
	_, err := listenAddrs([]string{"192.0.2.1", "secret-host", "192.0.2.300"}, true)
	if err == nil || !strings.Contains(err.Error(), "adapters.listen[1]") || !strings.Contains(err.Error(), "adapters.listen[2]") ||
		strings.Contains(err.Error(), "adapters.listen[0]") || strings.Contains(err.Error(), "secret-host") || strings.Contains(err.Error(), "300") {
		t.Errorf("two literals that do not parse: %v, want both refused by index, never by value", err)
	}
}
