package core_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"io"
	"math/big"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/internal/adapter/adaptertest/testproto"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/resources"
)

// tlsConfigs is a server's TLS configuration over a certificate made in memory
// and the client's that trusts it, both at TLS 1.3.
func tlsConfigs(t *testing.T) (server, client *tls.Config) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(leaf)

	server = &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		MinVersion:   tls.VersionTLS13,
	}
	client = &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS13}

	return server, client
}

// tlsHello is the bytes of a client's first handshake record under cfg.
func tlsHello(t *testing.T, cfg *tls.Config) []byte {
	t.Helper()

	a, b := net.Pipe()
	defer func() {
		_ = b.Close()
	}()

	go func() {
		_ = tls.Client(a, cfg).Handshake()
	}()

	head := make([]byte, 5)
	if _, err := io.ReadFull(b, head); err != nil {
		t.Fatal(err)
	}

	body := make([]byte, binary.BigEndian.Uint16(head[3:]))
	if _, err := io.ReadFull(b, body); err != nil {
		t.Fatal(err)
	}

	return append(head, body...)
}

// tlsAsked is what the connection's StartTLS calls returned, in order.
type tlsAsked struct {
	mu   sync.Mutex
	errs []error
}

func (a *tlsAsked) add(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.errs = append(a.errs, err)
}

func (a *tlsAsked) get() []error {
	a.mu.Lock()
	defer a.mu.Unlock()

	return append([]error(nil), a.errs...)
}

// tlsEchoed is what the Echo handler served, by payload: a test tells a call
// the connection did read from one it never read at all.
type tlsEchoed struct {
	mu   sync.Mutex
	seen [][]byte
}

func (e *tlsEchoed) add(payload []byte) {
	// A copy, not the request buffer's slice: the record's buffer goes back
	// to its pool once the reply is served, and the assertion must not read
	// recycled bytes.
	b := bytes.Clone(payload)

	e.mu.Lock()
	defer e.mu.Unlock()

	e.seen = append(e.seen, b)
}

func (e *tlsEchoed) get() [][]byte {
	e.mu.Lock()
	defer e.mu.Unlock()

	return append([][]byte(nil), e.seen...)
}

// tlsRig is a full-protocol connection whose Upgrade messages ask for TLS,
// established, on a clock the test moves.
type tlsRig struct {
	b      *resources.Budgets
	conn   *core.Conn
	peer   net.Conn
	c      *testproto.Client
	clk    *manualClock
	asked  *tlsAsked
	echoed *tlsEchoed
	server *tls.Config
	client *tls.Config
}

// inBubble runs f in a synctest bubble: a stuck handshake or a reader that reads
// nothing is then durably blocked, found by synctest.Wait and asserted on, and
// no test waits on a real-time guard.
func inBubble(t *testing.T, f func(*testing.T)) {
	t.Helper()
	synctest.Test(t, f)
}

// newTLSRig serves a connection of testproto's full protocol with the upgrade
// gated on gate (nil: not gated) and, where edit is not nil, its stages edited.
// It sends a Negotiate and a Session, so the handshake deadline of the accept
// is over and only the upgrade's own can close the connection. Run it in a
// bubble.
func newTLSRig(t *testing.T, gate func(), edit func(*core.Conn, *core.Stages)) *tlsRig {
	t.Helper()
	return newTLSRigWith(t, settings.DefaultNFS(), gate, edit)
}

// newTLSRigWith is newTLSRig under the settings snap.
func newTLSRigWith(
	t *testing.T,
	snap settings.NFS,
	gate func(),
	edit func(*core.Conn, *core.Stages),
) *tlsRig {
	t.Helper()
	return newTLSRigOpts(t, snap, gate, edit, false, false)
}

// newTLSRigOpts is newTLSRigWith that can also make the upgrade ask with no TLS
// configuration, or twice in one decode.
func newTLSRigOpts(
	t *testing.T,
	snap settings.NFS,
	gate func(),
	edit func(*core.Conn, *core.Stages),
	noCfg, twice bool,
) *tlsRig {
	t.Helper()

	b := budgets()
	adaptertest.CheckLeaks(t, b)
	r := &tlsRig{
		b:      b,
		asked:  &tlsAsked{},
		echoed: &tlsEchoed{},
	}
	r.server, r.client = tlsConfigs(t)

	cfg := r.server
	if noCfg {
		cfg = nil
	}

	var p core.Protocol = testproto.New(adapter.Deps{}).WithUpgrade(testproto.Upgrade{
		Config: cfg,
		Gate:   gate,
		Result: r.asked.add,
		Twice:  twice,
	}).WithEchoSeen(r.echoed.add)
	if edit != nil {
		p = withStages{p, edit}
	}

	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: p}

	srv, err := core.NewServer(adapter.Deps{Budgets: b}, snap, l)
	if err != nil {
		t.Fatal(err)
	}

	core.Serving(srv)

	r.clk = newManualClock()
	core.SetServerClock(srv, r.clk)
	r.conn, r.peer = servedConn(t, srv, &l)

	r.c = testproto.NewClient(r.peer)
	for _, m := range []testproto.Msg{
		{Op: testproto.OpNegotiate, Version: 1, ID: 1},
		{Op: testproto.OpSession, Version: 1, ID: 2, Session: 7},
	} {
		tlsSend(t, r.c, m)

		if rs := tlsRecv(t, r.c); len(rs) != 1 || rs[0].Status != testproto.StatusOK {
			t.Fatalf("setting the connection up: %+v", rs)
		}
	}

	return r
}

// tlsSend writes msgs and fails the test where the write is not taken once
// everything else in the bubble is blocked: the connection reads nothing.
func tlsSend(t *testing.T, c *testproto.Client, msgs ...testproto.Msg) {
	t.Helper()

	sent := make(chan error, 1)

	go func() {
		sent <- c.Send(msgs...)
	}()

	synctest.Wait()

	select {
	case err := <-sent:
		if err != nil {
			t.Fatalf("sending message %d: %v", msgs[0].ID, err)
		}
	default:
		t.Fatalf("message %d was not taken: the connection reads nothing", msgs[0].ID)
	}
}

// tlsRecv reads one record, failing the test where none comes once everything
// else in the bubble is blocked.
func tlsRecv(t *testing.T, c *testproto.Client) []testproto.Reply {
	t.Helper()

	rs, err := tlsRecvErr(t, c)
	if err != nil {
		t.Fatalf("reading a reply: %v", err)
	}

	return rs
}

// tlsRecvErr is tlsRecv with the read's error returned.
func tlsRecvErr(t *testing.T, c *testproto.Client) ([]testproto.Reply, error) {
	t.Helper()

	type result struct {
		rs  []testproto.Reply
		err error
	}

	got := make(chan result, 1)

	go func() {
		rs, err := c.Recv()
		got <- result{rs, err}
	}()

	synctest.Wait()

	select {
	case r := <-got:
		return r.rs, r.err
	default:
		t.Fatal("no record: the connection sent nothing")
		return nil, nil
	}
}

// tlsHandshake runs the client's handshake, with msgs ahead of its ClientHello
// where given, and fails the test where it has not ended once everything else
// in the bubble is blocked.
func tlsHandshake(
	t *testing.T,
	c *testproto.Client,
	cfg *tls.Config,
	msgs ...testproto.Msg,
) []testproto.Reply {
	t.Helper()

	type result struct {
		rs  []testproto.Reply
		err error
	}

	got := make(chan result, 1)

	go func() {
		rs, err := c.StartTLS(cfg, msgs...)
		got <- result{rs, err}
	}()

	synctest.Wait()

	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("the client's handshake: %v", r.err)
		}

		return r.rs
	default:
		t.Fatal("the client's handshake never ended: the connection sent nothing more")
		return nil
	}
}

// tlsClosed reports whether the connection's teardown has ended, once
// everything else in the bubble is blocked.
func tlsClosed(r *tlsRig) bool {
	synctest.Wait()

	select {
	case <-core.ConnDone(r.conn):
		return true
	default:
		return false
	}
}

func upgradeMsg(id uint64, mode uint8) testproto.Msg {
	return testproto.Msg{Op: testproto.OpUpgrade, Version: 1, ID: id, Mode: mode}
}

func echoMsg(id uint64, payload string) testproto.Msg {
	return testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: id, Payload: []byte(payload)}
}

// tlsEcho sends an Echo and checks its payload comes back byte for byte.
func tlsEcho(t *testing.T, c *testproto.Client, id uint64, payload string) {
	t.Helper()
	tlsSend(t, c, echoMsg(id, payload))

	rs := tlsRecv(t, c)
	if len(rs) != 1 || rs[0].ID != id || rs[0].Status != testproto.StatusOK ||
		string(rs[0].Body) != payload {
		t.Fatalf("Echo %d answered %+v, want its own payload %q with status ok", id, rs, payload)
	}
}

// upgraded takes the connection through a successful upgrade: the reply read in
// plaintext, the handshake run.
func (r *tlsRig) upgraded(t *testing.T) {
	t.Helper()
	tlsSend(t, r.c, upgradeMsg(10, 0))

	if rs := tlsRecv(t, r.c); len(rs) != 1 || rs[0].ID != 10 || rs[0].Status != testproto.StatusOK {
		t.Fatalf("the upgrade's reply is %+v, want status ok for ID 10", rs)
	}

	tlsHandshake(t, r.c, r.client)
}

// The reply to the call that asked for TLS is read in plaintext, and only then
// does the connection switch: the client's handshake gives TLS 1.3 and an Echo
// over it is answered byte for byte.
func TestStartTLSAnswersInPlaintextThenTLS(t *testing.T) {
	inBubble(t, func(t *testing.T) {
		r := newTLSRig(t, nil, nil)

		r.upgraded(t)

		if v := r.c.TLS().Version; v != tls.VersionTLS13 {
			t.Errorf("TLS version %#x, want %#x", v, tls.VersionTLS13)
		}

		tlsEcho(t, r.c, 11, "over tls")

		if seen := r.echoed.get(); len(seen) != 1 || string(seen[0]) != "over tls" {
			t.Errorf("the Echo handler served %q, want %q alone", seen, "over tls")
		}

		if got := r.asked.get(); len(got) != 1 || got[0] != nil {
			t.Errorf("StartTLS returned %v, want one nil", got)
		}
	})
}

// A call written in the same write as the upgrade is never served: the bytes the
// reader already held are handed to the handshake, which refuses them, so the
// client reads the upgrade's reply and then the connection's end, and the
// Echo's handler is never run.
func TestStartTLSPipelinedCallIsNeverServed(t *testing.T) {
	inBubble(t, func(t *testing.T) {
		r := newTLSRig(t, nil, nil)

		raw := testproto.Append(testproto.Append(nil, upgradeMsg(10, 0)), echoMsg(11, "injected"))
		if err := r.c.SendRaw(raw); err != nil {
			t.Fatal(err)
		}

		if rs := tlsRecv(t, r.c); len(rs) != 1 || rs[0].ID != 10 {
			t.Fatalf("the first record is %+v, want the upgrade's reply", rs)
		}

		if rs, err := tlsRecvErr(t, r.c); !errors.Is(err, io.EOF) {
			t.Errorf(
				"the second record is %+v (%v), want the connection's end, "+
					"not the injected Echo's reply",
				rs,
				err,
			)
		}

		if seen := r.echoed.get(); len(seen) != 0 {
			t.Errorf("the Echo handler served %q, want nothing served", seen)
		}
	})
}

// The reader waits on the upgrade its own decode asked for, not on the
// connection's current one: held after the record it served, past the end of
// the handshake, it still takes the switch once released, and answers what
// came over TLS. Without the wait it would read the switched socket in
// plaintext and lose the client.
func TestStartTLSLateReaderStillSwaps(t *testing.T) {
	inBubble(t, func(t *testing.T) {
		r := newTLSRig(t, nil, nil)
		release := core.HoldServed(r.conn)
		tlsSend(t, r.c, upgradeMsg(10, 0))

		if rs := tlsRecv(t, r.c); len(rs) != 1 || rs[0].ID != 10 ||
			rs[0].Status != testproto.StatusOK {
			t.Fatalf("the upgrade's reply is %+v, want status ok for ID 10", rs)
		}

		tlsHandshake(t, r.c, r.client) // it ends while the reader is still held

		synctest.Wait() // the writer took the switch: the upgrade is gone

		release()

		tlsEcho(t, r.c, 11, "over tls")

		if got := r.asked.get(); len(got) != 1 || got[0] != nil {
			t.Errorf("StartTLS returned %v, want one nil", got)
		}
	})
}

// A ClientHello written in the same write as the upgrade is not lost: the held
// bytes go to the handshake first and an Echo over TLS is answered.
func TestStartTLSHeldClientHelloUpgrades(t *testing.T) {
	inBubble(t, func(t *testing.T) {
		r := newTLSRig(t, nil, nil)

		rs := tlsHandshake(t, r.c, r.client, upgradeMsg(10, 0))
		if len(rs) != 1 || rs[0].ID != 10 || rs[0].Status != testproto.StatusOK {
			t.Fatalf("the upgrade's reply is %+v, want status ok for ID 10", rs)
		}

		synctest.Wait()

		if core.RecordHeld(r.conn) {
			t.Error("the reader still holds a record after the upgrade with nothing read")
		}

		tlsEcho(t, r.c, 11, "after a held hello")
	})
}

// A plaintext call after the reply is not a handshake: the connection closes
// and the Echo is never answered.
func TestStartTLSPlaintextAfterReplyCloses(t *testing.T) {
	inBubble(t, func(t *testing.T) {
		r := newTLSRig(t, nil, nil)
		tlsSend(t, r.c, upgradeMsg(10, 0))

		if rs := tlsRecv(t, r.c); len(rs) != 1 || rs[0].ID != 10 {
			t.Fatalf("the upgrade's reply is %+v", rs)
		}

		tlsSend(t, r.c, echoMsg(11, "in the clear"))

		if !tlsClosed(r) {
			t.Fatal("a plaintext call where the handshake was due left the connection open")
		}
	})
}

// A second call that asks for TLS is refused and starts no second handshake: the
// connection goes on over its TLS conn.
func TestStartTLSSecondCallIsRefused(t *testing.T) {
	inBubble(t, func(t *testing.T) {
		r := newTLSRig(t, nil, nil)
		r.upgraded(t)

		tlsSend(t, r.c, upgradeMsg(12, 0))

		if rs := tlsRecv(t, r.c); len(rs) != 1 || rs[0].ID != 12 {
			t.Fatalf("the second upgrade's reply is %+v", rs)
		}

		tlsEcho(t, r.c, 13, "still one handshake")

		if got := r.asked.get(); len(got) != 2 || got[0] != nil ||
			!errors.Is(got[1], core.ErrTLSAgain) {
			t.Errorf("StartTLS returned %v, want nil then the again refusal", got)
		}
	})
}

// A StartTLS that comes after the connection began closing is refused and
// nothing is sent.
func TestStartTLSAfterCloseIsRefused(t *testing.T) {
	inBubble(t, func(t *testing.T) {
		reached, release := make(chan struct{}), make(chan struct{})
		r := newTLSRig(t, func() {
			close(reached)
			<-release
		}, nil)
		tlsSend(t, r.c, upgradeMsg(10, 0))
		<-reached // the Decode is about to ask
		r.conn.Close(nil)
		close(release)

		if !tlsClosed(r) {
			t.Fatal("the connection did not end")
		}

		if got := r.asked.get(); len(got) != 1 || !errors.Is(got[0], core.ErrTLSClosing) {
			t.Errorf("StartTLS after the close returned %v, want the closing refusal", got)
		}

		if got, _ := io.ReadAll(r.peer); len(got) != 0 {
			t.Errorf("the client read %d bytes after the close, want none", len(got))
		}
	})
}

// A Close that comes while the upgrade's reply waits to be written wins: the
// reply goes out and no handshake starts, though the ClientHello is in hand, so
// the client reads the reply and nothing after it. The teardown is held, so a
// handshake that did start would have its first flight written to the client.
func TestStartTLSCloseBeforeHandoffWins(t *testing.T) {
	inBubble(t, func(t *testing.T) {
		release := make(chan struct{})
		r := newTLSRig(t, nil, func(_ *core.Conn, st *core.Stages) {
			st.OnClose = func(context.Context) {
				<-release
			}
		})
		hello := tlsHello(t, r.client)

		raw := append(testproto.Append(nil, upgradeMsg(10, 0)), hello...)
		if err := r.c.SendRaw(raw); err != nil {
			t.Fatal(err)
		}

		synctest.Wait() // the writer holds the reply: the client reads nothing yet
		r.conn.Close(nil)
		// The reply is a record of one header: its 4-byte length and 40 bytes.
		reply := make([]byte, 4+40)

		if _, err := io.ReadFull(r.peer, reply); err != nil {
			t.Fatalf("reading the reply: %v", err)
		}

		if id := binary.BigEndian.Uint64(reply[4+12:]); id != 10 {
			t.Errorf("the reply is for message %d, want 10", id)
		}

		synctest.Wait()
		close(release)

		if got, _ := io.ReadAll(r.peer); len(got) != 0 {
			t.Errorf(
				"the client read %d bytes after the reply, want none: a handshake started",
				len(got),
			)
		}
	})
}

// A StartTLS with no TLS configuration is refused, and the call goes on in plaintext.
func TestStartTLSWithoutAConfigIsRefused(t *testing.T) {
	inBubble(t, func(t *testing.T) {
		r := newTLSRigOpts(t, settings.DefaultNFS(), nil, nil, true, false)
		tlsSend(t, r.c, upgradeMsg(10, 0))

		if rs := tlsRecv(t, r.c); len(rs) != 1 || rs[0].ID != 10 {
			t.Fatalf("the call is answered %+v", rs)
		}

		tlsEcho(t, r.c, 11, "plaintext")

		if got := r.asked.get(); len(got) != 1 || !errors.Is(got[0], core.ErrTLSNoConfig) {
			t.Errorf("StartTLS without a config returned %v, want the no-config refusal", got)
		}
	})
}

// A header that asks twice in one decode is refused the second time: the first
// request is pending, and it goes through.
func TestStartTLSAskedTwiceInOneDecodeIsRefused(t *testing.T) {
	inBubble(t, func(t *testing.T) {
		r := newTLSRigOpts(t, settings.DefaultNFS(), nil, nil, false, true)
		r.upgraded(t)

		tlsEcho(t, r.c, 11, "over tls")

		if got := r.asked.get(); len(got) != 2 || got[0] != nil || got[1] == nil {
			t.Errorf("StartTLS returned %v, want nil then an error", got)
		}
	})
}

// A StartTLS made outside any header's decode is refused.
func TestStartTLSOutsideADecodeIsRefused(t *testing.T) {
	inBubble(t, func(t *testing.T) {
		r := newTLSRig(t, nil, nil)
		if err := r.conn.StartTLS(r.server); !errors.Is(err, core.ErrTLSOutside) {
			t.Errorf("StartTLS outside a decode returned %v, want the outside-decode refusal", err)
		}

		tlsEcho(t, r.c, 11, "plaintext")
	})
}

// A zero handshake_timeout leaves the handshake no deadline of its own: it
// succeeds however far the clock moved after the reply.
func TestStartTLSZeroHandshakeTimeoutHasNoDeadline(t *testing.T) {
	inBubble(t, func(t *testing.T) {
		snap := settings.DefaultNFS()
		past := 2 * snap.HandshakeTimeout
		snap.HandshakeTimeout = 0

		r := newTLSRigWith(t, snap, nil, nil)

		tlsSend(t, r.c, upgradeMsg(10, 0))

		if rs := tlsRecv(t, r.c); len(rs) != 1 || rs[0].ID != 10 {
			t.Fatalf("the upgrade's reply is %+v", rs)
		}

		r.clk.Advance(past)
		tlsHandshake(t, r.c, r.client)
		tlsEcho(t, r.c, 11, "over tls")
	})
}

// A client that closes its end without reading the upgrade reply fails the
// reply's write: the connection closes for that, and no handshake is tried on it.
func TestStartTLSWriteFailureClosesBeforeTheHandshake(t *testing.T) {
	inBubble(t, func(t *testing.T) {
		r := newTLSRig(t, nil, nil)
		go func() {
			_ = r.c.Send(upgradeMsg(10, 0))
		}()

		synctest.Wait()

		_ = r.peer.Close()

		if !tlsClosed(r) {
			t.Fatal("the connection stayed open after its reply's write failed")
		}

		if err := core.ConnCloseErr(r.conn); !strings.Contains(err.Error(), "the write failed") {
			t.Errorf("the connection closed with %q, want the write failure", err)
		}
	})
}

// A handshake that fails closes the connection at once: the bytes the client
// sent behind its junk, a valid call here, are never read as plaintext.
func TestStartTLSFailedHandshakeClosesAtOnce(t *testing.T) {
	inBubble(t, func(t *testing.T) {
		r := newTLSRig(t, nil, nil)
		tlsSend(t, r.c, upgradeMsg(10, 0))

		if rs := tlsRecv(t, r.c); len(rs) != 1 || rs[0].ID != 10 {
			t.Fatalf("the upgrade's reply is %+v", rs)
		}
		go func() {
			_ = r.c.SendRaw(append(make([]byte, 5), testproto.Append(nil, upgradeMsg(11, 0))...))
		}()

		if !tlsClosed(r) {
			t.Fatal("the connection stayed open after a failed handshake")
		}

		if err := core.ConnCloseErr(r.conn); !strings.Contains(err.Error(), "tls handshake") {
			t.Errorf("the connection closed with %q, want the handshake failure", err)
		}

		if got := r.asked.get(); len(got) != 1 {
			t.Errorf(
				"StartTLS was asked %d times, want 1: a call was read after the failed handshake",
				len(got),
			)
		}
	})
}

// A chain's first member that asks for TLS is withdrawn: both replies leave, the
// calls after them are answered in plaintext and nothing upgrades.
func TestStartTLSFromAChainFirstMemberStaysWithdrawn(t *testing.T) {
	inBubble(t, func(t *testing.T) {
		r := newTLSRig(t, nil, nil)
		tlsSend(t, r.c, upgradeMsg(10, 0), echoMsg(11, "second"))

		if rs := tlsRecv(t, r.c); len(rs) != 2 || rs[0].ID != 10 || rs[1].ID != 11 {
			t.Fatalf("the chain is answered %+v, want replies 10 and 11", rs)
		}

		tlsEcho(t, r.c, 12, "plaintext")

		tlsEcho(t, r.c, 13, "still plaintext")

		if got := r.asked.get(); len(got) != 1 || got[0] != nil {
			t.Errorf("StartTLS from the first member returned %v, want one nil", got)
		}
	})
}

// A chain member that asks for TLS is refused: its StartTLS returns an error and
// the calls after it are answered in plaintext, none of them taken for the
// upgrade.
func TestStartTLSFromAChainMemberIsRefused(t *testing.T) {
	inBubble(t, func(t *testing.T) {
		r := newTLSRig(t, nil, nil)
		tlsSend(t, r.c, echoMsg(10, "first"), upgradeMsg(11, 0))

		if rs := tlsRecv(t, r.c); len(rs) != 2 || rs[0].ID != 10 || rs[1].ID != 11 {
			t.Fatalf("the chain is answered %+v, want replies 10 and 11", rs)
		}

		tlsEcho(t, r.c, 12, "plaintext")

		tlsEcho(t, r.c, 13, "still plaintext")

		if got := r.asked.get(); len(got) != 1 || !errors.Is(got[0], core.ErrTLSOutside) {
			t.Errorf("StartTLS from the member returned %v, want the member's refusal", got)
		}
	})
}

// A chain member decoded while the reader is about to decode the next call's
// first header is still refused, and that call is the one that upgrades: the
// member's decode and the call's never overlap, so the member's ask is never
// taken for the call's.
func TestStartTLSOfAMemberNeverTakesTheNextCalls(t *testing.T) {
	inBubble(t, func(t *testing.T) {
		entered := make(chan chan struct{}, 2)
		r := newTLSRig(t, func() {
			release := make(chan struct{})
			entered <- release

			<-release
		}, nil)

		tlsSend(t, r.c, echoMsg(10, "first"), upgradeMsg(11, 0))
		synctest.Wait() // the chain's goroutine is in the member's decode

		release := <-entered

		tlsSend(t, r.c, upgradeMsg(12, 0))

		synctest.Wait() // the reader holds the next call's record

		if len(entered) != 0 {
			t.Fatal("the next call's first header decoded while the member's did")
		}

		close(release)

		if rs := tlsRecv(t, r.c); len(rs) != 2 || rs[0].ID != 10 || rs[1].ID != 11 {
			t.Fatalf("the chain is answered %+v, want replies 10 and 11", rs)
		}

		close(<-entered)

		if rs := tlsRecv(t, r.c); len(rs) != 1 || rs[0].ID != 12 {
			t.Fatalf("the next call is answered %+v, want the upgrade's reply 12", rs)
		}

		tlsHandshake(t, r.c, r.client)
		tlsEcho(t, r.c, 13, "over tls")

		if got := r.asked.get(); len(got) != 2 || got[0] == nil || got[1] != nil {
			t.Errorf("StartTLS returned %v, want an error for the member then nil", got)
		}
	})
}

// A call the router refuses, one its row answers from the error table and one
// its row answers no reply leave the connection in plaintext: the next call is
// read and answered in the clear, with no handshake.
func TestStartTLSStaysPlaintextWhenTheCallFails(t *testing.T) {
	for _, c := range []struct {
		name  string
		msg   testproto.Msg
		reply bool
		want  core.Status
	}{
		{
			"RefusedRoute",
			testproto.Msg{Op: testproto.OpUpgrade, Version: 3, ID: 10},
			true,
			testproto.StatusVersion,
		},
		{"ErrorTable", upgradeMsg(10, 1), true, testproto.StatusNotSupported},
		{"NoReply", upgradeMsg(10, 2), false, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			inBubble(t, func(t *testing.T) {
				r := newTLSRig(t, nil, nil)
				tlsSend(t, r.c, c.msg)

				if c.reply {
					if rs := tlsRecv(t, r.c); len(rs) != 1 || rs[0].ID != 10 ||
						rs[0].Status != c.want {
						t.Fatalf(
							"the refused call is answered %+v, want status %d for ID 10",
							rs,
							c.want,
						)
					}
				}

				tlsEcho(t, r.c, 11, "in the clear")

				if got := r.asked.get(); len(got) != 1 || got[0] != nil {
					t.Errorf("StartTLS returned %v, want one nil", got)
				}
			})
		})
	}
}

// Messages sent through Conn.Send while the upgrade happens are read intact: the
// one queued before the reply in plaintext, the twenty queued behind it over TLS.
// The writer is held in the write of the first message, since the client reads
// nothing, so the reply and the twenty are all queued before it wakes: it must
// still write the reply alone in plaintext.
func TestStartTLSWithRepliesQueued(t *testing.T) {
	const behind = 20

	inBubble(t, func(t *testing.T) {
		r := newTLSRig(t, nil, nil)
		pool := resources.NewBufPool(r.b.Reply)
		send := func(id uint64) {
			msg, err := pool.Get(t.Context(), netip.Addr{}, 64)
			if err != nil {
				t.Fatal(err)
			}

			msg.B = append(msg.B[:0], testproto.Append(nil, echoMsg(id, "queued"))[4:]...)
			if err := r.conn.Send(nil, msg, 0); err != nil {
				t.Fatalf("Send %d: %v", id, err)
			}
		}
		send(99)
		synctest.Wait() // the writer is in its write of message 99
		tlsSend(t, r.c, upgradeMsg(10, 0))
		waitsFor(t, "the upgrade call to end", func() bool {
			return core.Runs(core.ConnPipeline(r.conn)) == 0 && !core.RecordHeld(r.conn)
		})

		for i := range behind {
			send(uint64(100 + i))
		}
		// An Echo's arguments are its role byte, 0, then the payload.
		if rs := tlsRecv(t, r.c); len(rs) != 1 || rs[0].ID != 99 ||
			string(rs[0].Body) != "\x00queued" {
			t.Fatalf("the first record is %+v, want message 99 with its payload", rs)
		}

		if rs := tlsRecv(t, r.c); len(rs) != 1 || rs[0].ID != 10 {
			t.Fatalf("the second record is %+v, want the upgrade's reply", rs)
		}

		tlsHandshake(t, r.c, r.client)

		for i := range behind {
			rs := tlsRecv(t, r.c)
			if len(rs) != 1 || rs[0].ID != uint64(100+i) || string(rs[0].Body) != "\x00queued" {
				t.Fatalf(
					"record %d over TLS is %+v, want message %d with its payload",
					i,
					rs,
					100+i,
				)
			}
		}
	})
}

// A write_timeout shorter than the time the client takes to start its handshake
// does not fail the handshake: the reply's write deadline is over once the reply
// is written.
func TestStartTLSHandshakeOutlastsTheWriteTimeout(t *testing.T) {
	inBubble(t, func(t *testing.T) {
		snap := settings.DefaultNFS()
		snap.WriteTimeout = time.Second
		r := newTLSRigWith(t, snap, nil, nil)

		tlsSend(t, r.c, upgradeMsg(10, 0))

		if rs := tlsRecv(t, r.c); len(rs) != 1 || rs[0].ID != 10 {
			t.Fatalf("the upgrade's reply is %+v", rs)
		}
		// Past the write deadline the reply's write armed. It is the bubble's
		// own time that passes, not a real wait: the pipe's deadline is not on
		// the server's injected clock, and inside the bubble it cannot fire.
		time.Sleep(3 * time.Second)
		tlsHandshake(t, r.c, r.client)
		tlsEcho(t, r.c, 11, "after a slow client")
	})
}

// A client that goes silent after the reply is closed when handshake_timeout
// passes on the server's clock, counted from the reply, not from accept.
func TestStartTLSHandshakeDeadline(t *testing.T) {
	inBubble(t, func(t *testing.T) {
		r := newTLSRig(t, nil, nil)
		tlsSend(t, r.c, upgradeMsg(10, 0))

		if rs := tlsRecv(t, r.c); len(rs) != 1 || rs[0].ID != 10 {
			t.Fatalf("the upgrade's reply is %+v", rs)
		}

		handshake, _ := deadlines(r.conn)

		waitsFor(
			t,
			"the handshake deadline to be armed",
			func() bool {
				return r.clk.nextIn() == handshake
			},
		)
		r.clk.Advance(handshake - time.Second)

		if tlsClosed(r) {
			t.Fatal("a connection 1 s short of the handshake deadline closed")
		}

		r.clk.Advance(2 * time.Second)

		if !tlsClosed(r) {
			t.Fatal("a silent client's connection stayed open past the handshake deadline")
		}
	})
}

// Once the handshake succeeded its deadline is over: the connection stays up
// and answers past handshake_timeout.
func TestStartTLSHandshakeDeadlineEndsWithTheHandshake(t *testing.T) {
	inBubble(t, func(t *testing.T) {
		r := newTLSRig(t, nil, nil)
		r.upgraded(t)
		tlsEcho(t, r.c, 11, "first")
		handshake, _ := deadlines(r.conn)
		r.clk.Advance(2 * handshake)

		if tlsClosed(r) {
			t.Fatal("the handshake deadline closed a connection whose handshake had succeeded")
		}

		tlsEcho(t, r.c, 12, "past the deadline")
	})
}
