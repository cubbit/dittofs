package nfsclient_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest/nfsclient"
)

const (
	progNFS = 100003

	lastFragment = 1 << 31
)

// golden reads one of the NFS wire package's golden records, hex with #
// comments.
func golden(t *testing.T, name string) []byte {
	t.Helper()

	data, err := os.ReadFile("../../nfs/wire/testdata/golden/" + name + ".hex")
	if err != nil {
		t.Fatal(err)
	}

	return unhex(t, string(data))
}

// unhex is the bytes of hex digits with # comments and spaces among them.
func unhex(t *testing.T, text string) []byte {
	t.Helper()

	var digits strings.Builder

	for line := range strings.SplitSeq(text, "\n") {
		line, _, _ = strings.Cut(line, "#")
		digits.WriteString(strings.Join(strings.Fields(line), ""))
	}

	b, err := hex.DecodeString(digits.String())
	if err != nil {
		t.Fatal(err)
	}

	return b
}

// words is XDR's encoding of each: four bytes, big-endian (RFC 4506 section
// 4.1).
func words(ws ...uint32) []byte {
	var b []byte
	for _, w := range ws {
		b = binary.BigEndian.AppendUint32(b, w)
	}

	return b
}

// peer is a TCP listener standing in for a server: it reads each record the
// client writes, hands its body to the returned channel, and answers it with
// reply(body), framed as one record of two fragments; a nil reply answers with an accepted
// SUCCESS of no results to the call's XID.
func peer(t *testing.T, reply func(rec []byte) []byte) (netip.AddrPort, <-chan []byte) {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_ = l.Close()
	})

	got := make(chan []byte, 8)

	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		for {
			var mark [4]byte
			if _, err := io.ReadFull(conn, mark[:]); err != nil {
				return
			}

			if binary.BigEndian.Uint32(mark[:])&lastFragment == 0 {
				t.Error("the client wrote a record whose one fragment is not the last")
			}

			rec := make([]byte, binary.BigEndian.Uint32(mark[:])&^lastFragment)
			if _, err := io.ReadFull(conn, rec); err != nil {
				return
			}

			got <- rec

			answer := words(binary.BigEndian.Uint32(rec), 1, 0, 0, 0, 0)
			if reply != nil {
				answer = reply(rec)
			}

			// Two fragments, the first not the last, so Read must join them.
			half := len(answer) / 2
			out := binary.BigEndian.AppendUint32(nil, uint32(half))
			out = append(out, answer[:half]...)

			out = binary.BigEndian.AppendUint32(out, lastFragment|uint32(len(answer)-half))
			if _, err := conn.Write(append(out, answer[half:]...)); err != nil {
				return
			}
		}
	}()

	return l.Addr().(*net.TCPAddr).AddrPort(), got
}

// dial connects a client to addr, closed when the test ends.
func dial(t *testing.T, addr netip.AddrPort) *nfsclient.Client {
	t.Helper()

	c, err := nfsclient.Dial(t.Context(), addr)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_ = c.Close()
	})

	return c
}

func ptr(x uint32) *uint32 {
	return &x
}

// written is the record the peer read before it answered the call Do has
// returned from: already in the channel, so a client that wrote nothing is a
// failure here and not a wait.
func written(t *testing.T, got <-chan []byte) []byte {
	t.Helper()

	select {
	case rec := <-got:
		return rec
	default:
		t.Error("the peer read no record")
		return nil
	}
}

// Do writes the bytes of each golden call for the fields the test writes by
// hand from the golden's own comments: the RFC's NFSv4 calls and knfsd's
// captured NFSv3 ones, whose AUTH_SYS credential's body is a field of the
// call. A credential from AuthSys is the golden body in the golden's place.
func TestClientWritesTheGoldenCalls(t *testing.T) {
	unfielded := func(file string) []byte {
		return golden(t, file)
	}
	sys := func(file string) []byte {
		return golden(t, file)
	}

	cred := func(xid uint32, body []byte) []byte {
		b := append(words(xid, 0, 2, progNFS, 3, 0, 1, uint32(len(body))), body...)
		return append(b, words(0, 0)...)
	}

	knfsd := nfsclient.Auth{Flavour: 1, Body: unhex(t, `00000000 00000009 64697474 6f303932 36000000
		00000000 00000000 00000001 00000000`)}
	for _, tc := range []struct {
		name string
		call nfsclient.Call
		want []byte
	}{
		{"an NFSv4.1 COMPOUND of no operation", nfsclient.Call{
			XID: ptr(0x28), Prog: progNFS, Vers: 4, Proc: 1,
			Args: unhex(t, "00000005 6162636465000000 00000001 00000000"),
		}, unfielded("rfc-nfs4-empty-call")},
		{"an NFSv4.1 COMPOUND of one READ", nfsclient.Call{
			XID: ptr(0x2e), Prog: progNFS, Vers: 4, Proc: 1,
			Args: unhex(t, `00000005 6162636465000000 00000001 00000001 00000019
				00000000 00000000 00000000 00000000 00000000 00000000 00001000`),
		}, unfielded("rfc-nfs4-read-call")},
		{"knfsd's NFSv3 ACCESS under AUTH_SYS", nfsclient.Call{
			XID: ptr(0xbe64bf33), Prog: progNFS, Vers: 3, Proc: 4,
			Cred: knfsd,
			Args: unhex(t, "00000008 01000100 92100000 0000001f"),
		}, sys("knfsd-nfs3-nfs3-access-call")},
		{"knfsd's NFSv3 CREATE under AUTH_SYS", nfsclient.Call{
			XID: ptr(0xc064bf33), Prog: progNFS, Vers: 3, Proc: 8,
			Cred: knfsd,
			Args: unhex(t, `00000008 01000100 92100000 00000009 6e667333 2d66696c 65000000 00000000
				00000001 000001a4 00000000 00000000 00000001 00000000 00000000 00000000 00000000`),
		}, sys("knfsd-nfs3-nfs3-create-call")},
		{"AuthSys of uid 1000 and groups 10, 20, 30", nfsclient.Call{
			XID: ptr(7), Prog: progNFS, Vers: 3,
			Cred: nfsclient.AuthSys(1000, 1000, 10, 20, 30),
		}, cred(7, golden(t, "rfc-cred-sys-bare"))},
		{"AuthSys of uid 0 and no group", nfsclient.Call{
			XID: ptr(8), Prog: progNFS, Vers: 3, Proc: 0, Cred: nfsclient.AuthSys(0, 0),
		}, cred(8, golden(t, "rfc-cred-sys-root"))},
		{"AuthSys of a uid and a gid that differ", nfsclient.Call{
			XID: ptr(10), Prog: progNFS, Vers: 3, Proc: 0, Cred: nfsclient.AuthSys(1000, 2000, 7),
		}, cred(10, words(0, 0, 1000, 2000, 1, 7))},
		{"a credential body that needs padding", nfsclient.Call{
			XID: ptr(11), Prog: progNFS, Vers: 3, Proc: 0,
			Cred: nfsclient.Auth{Flavour: 9, Body: []byte{1, 2, 3}},
		}, append(words(11, 0, 2, progNFS, 3, 0, 9, 3, 0x01020300), words(0, 0)...)},
		{"AuthSys of 16 groups", nfsclient.Call{
			XID: ptr(9), Prog: progNFS, Vers: 3, Proc: 0,
			Cred: nfsclient.AuthSys(1000, 1000, 1, 2, 3, 4, 5, 6, 7, 8,
				9, 10, 11, 12, 13, 14, 15, 16),
		}, cred(9, golden(t, "rfc-cred-sys-16-groups"))},
	} {
		addr, got := peer(t, nil)
		if _, err := dial(t, addr).Do(t.Context(), tc.call); err != nil {
			t.Fatalf("%s: Do: %v", tc.name, err)
		}

		if rec := written(t, got); !bytes.Equal(rec, tc.want) {
			t.Errorf("%s: wrote %x, want %x", tc.name, rec, tc.want)
		}
	}
}

// A call with no XID takes the client's next, which counts up from 1; one that
// sets its XID sends it, 0 included, and takes none of the client's.
func TestACallsXIDIsTheClientsNextOrItsOwn(t *testing.T) {
	addr, got := peer(t, nil)

	c := dial(t, addr)

	for i, tc := range []struct {
		xid  *uint32
		want uint32
	}{{nil, 1}, {nil, 2}, {ptr(0), 0}, {ptr(77), 77}, {nil, 3}} {
		call := nfsclient.Call{XID: tc.xid, Prog: progNFS, Vers: 3}
		if _, err := c.Do(t.Context(), call); err != nil {
			t.Fatal(err)
		}

		if xid := binary.BigEndian.Uint32(append(written(t, got), 0, 0, 0, 0)); xid != tc.want {
			t.Errorf("a call %d (want XID %d) went out as XID %d", i, tc.want, xid)
		}
	}
}

// Do reads each reply golden file to its fields: an accepted reply's accept
// state, range and results, a denied one's reject state.
func TestClientReadsTheGoldenReplies(t *testing.T) {
	const xid = 0x33b50de8

	tag := "00000005 6162636465000000"
	for _, tc := range []struct {
		file string
		want nfsclient.Reply
	}{
		{"accept-prog-unavail", nfsclient.Reply{XID: xid, Stat: 1}},
		{"accept-prog-mismatch", nfsclient.Reply{XID: xid, Stat: 2, Low: 3, High: 4}},
		{"accept-proc-unavail", nfsclient.Reply{XID: xid, Stat: 3}},
		{"accept-garbage-args", nfsclient.Reply{XID: xid, Stat: 4}},
		{"accept-system-err", nfsclient.Reply{XID: xid, Stat: 5}},
		{"deny-rpc-mismatch", nfsclient.Reply{XID: xid, Denied: true, Stat: 0, Low: 2, High: 2}},
		{"deny-auth-badcred", nfsclient.Reply{XID: xid, Denied: true, Stat: 1}},
		{"deny-auth-badverf", nfsclient.Reply{XID: xid, Denied: true, Stat: 3}},
		{"rfc-nfs4-empty-reply", nfsclient.Reply{
			XID: 0x28, Body: unhex(t, "00000000 "+tag+" 00000000"),
		}},
		{"rfc-nfs4-read-reply", nfsclient.Reply{
			XID: 0x2e, Body: unhex(t, "00002714 "+tag+" 00000001 00000019 00002714"),
		}},
		{"knfsd-nfs3-nfs3-access-reply", nfsclient.Reply{XID: 0xbe64bf33, Body: unhex(t, `
			00000000 00000001 00000002 000003ff 00000002 00000000 00000000 00000000 00000006
			00000000 00000000 00000000 00000000 00000000 00001092 00000000 00000080
			6b0700f3 077f99f8
			6b0700f3 077f99f8 6b0700f3 07d798dc 0000001f`)}},
	} {
		addr, _ := peer(t, func([]byte) []byte {
			return golden(t, tc.file)
		})

		got, err := dial(t, addr).Do(t.Context(), nfsclient.Call{Prog: progNFS, Vers: 3})
		if err != nil {
			t.Fatalf("%s: Do: %v", tc.file, err)
		}

		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: read %+v, want %+v", tc.file, got, tc.want)
		}
	}
}

// A reply's verifier is read with its padding, an accepted reply's range and a
// denied one's versions each in their own field: the golden replies hold an
// empty verifier and ranges that read the same in either order.
func TestClientReadsTheFieldsOfAReply(t *testing.T) {
	const xid = 5

	for _, tc := range []struct {
		name  string
		reply []byte
		want  nfsclient.Reply
	}{
		{
			"a verifier of three bytes, then results",
			append(words(xid, 1, 0, 1, 3, 0x09080700, 0), words(0xaa)...),
			nfsclient.Reply{
				XID:  xid,
				Verf: nfsclient.Auth{Flavour: 1, Body: []byte{9, 8, 7}},
				Body: words(0xaa),
			},
		},
		{
			"RPC_MISMATCH of 2 to 3",
			words(xid, 1, 1, 0, 2, 3),
			nfsclient.Reply{XID: xid, Denied: true, Low: 2, High: 3},
		},
		{
			"PROG_MISMATCH of 3 to 4",
			words(xid, 1, 0, 0, 0, 2, 3, 4),
			nfsclient.Reply{XID: xid, Stat: 2, Low: 3, High: 4},
		},
	} {
		addr, _ := peer(t, func([]byte) []byte {
			return tc.reply
		})

		got, err := dial(t, addr).Do(t.Context(), nfsclient.Call{Prog: progNFS, Vers: 3})
		if err != nil {
			t.Fatalf("%s: Do: %v", tc.name, err)
		}

		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: read %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// A record that is not a whole reply is an error, never a reply of what it
// holds.
func TestClientRefusesWhatIsNotAReply(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply []byte
	}{
		{"a call", words(5, 0, 2, progNFS, 3, 0, 0, 0, 0, 0)},
		{"an accepted reply cut before its verifier", words(5, 1, 0, 0)},
		{"a verifier cut inside its padding", append(words(5, 1, 0, 1, 3), 9, 8, 7)},
		{"an accepted reply cut before its state", words(5, 1, 0, 0, 0)},
		{"a reply of no message type", words(5)},
		{"a reply_stat no RPC defines", words(5, 1, 2)},
		{"a reject_stat no RPC defines", words(5, 1, 1, 9)},
		{"bytes after a denied reply", words(5, 1, 1, 0, 2, 3, 7, 7)},
		{"bytes after a PROG_MISMATCH", words(5, 1, 0, 0, 0, 2, 3, 4, 7)},
		{"bytes after a GARBAGE_ARGS", words(5, 1, 0, 0, 0, 4, 7)},
		{"an accept_stat no RPC defines", words(5, 1, 0, 0, 0, 9)},
	} {
		addr, _ := peer(t, func([]byte) []byte {
			return tc.reply
		})

		c := dial(t, addr)

		if got, err := c.Do(t.Context(), nfsclient.Call{Prog: progNFS, Vers: 3}); err == nil {
			t.Errorf("%s: read %+v with no error", tc.name, got)
		}
	}
}

// server accepts one connection, writes b to it, and closes it, or holds it
// open until the test ends.
func server(t *testing.T, b []byte, hold bool) netip.AddrPort {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_ = l.Close()
	})

	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		_, _ = conn.Write(b)

		if hold {
			<-t.Context().Done()
		}
	}()

	return l.Addr().(*net.TCPAddr).AddrPort()
}

// within reads one record from the client, failing the test, which cannot
// wait on a read that never ends, past five seconds.
func within(ctx context.Context, t *testing.T, c *nfsclient.Client) ([]byte, error) {
	t.Helper()

	type result struct {
		rec []byte
		err error
	}

	done := make(chan result, 1)

	go func() {
		rec, err := c.Read(ctx)
		done <- result{rec, err}
	}()

	select {
	case r := <-done:
		return r.rec, r.err
	case <-time.After(5 * time.Second):
		t.Fatal("Read did not end in five seconds")
		return nil, nil
	}
}

// Read ends on how the server ended the record: a close before a record is
// io.EOF, one inside it io.ErrUnexpectedEOF, and a record past the cap, as the
// sum of its fragments, is an error before the cap's worth is held.
func TestReadEndsWhereTheRecordEnds(t *testing.T) {
	big := make([]byte, 600<<10)

	for _, tc := range []struct {
		name string
		sent []byte
		want error
	}{
		{"a close before a record", nil, io.EOF},
		{"a close inside a fragment's mark", []byte{0x80, 0}, io.ErrUnexpectedEOF},
		{
			"a close inside a fragment's body",
			append(words(lastFragment|8), 1, 2, 3, 4),
			io.ErrUnexpectedEOF,
		},
		{"a close between fragments", append(words(4), 1, 2, 3, 4), io.ErrUnexpectedEOF},
		{
			"two fragments past the cap",
			slices.Concat(
				words(uint32(len(big))), big, words(lastFragment|uint32(len(big))), big,
			),
			nil,
		},
	} {
		c := dial(t, server(t, tc.sent, false))

		rec, err := within(t.Context(), t, c)
		if tc.want == nil &&
			(err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
			t.Errorf("%s: Read returned %d bytes and %v, want a cap error", tc.name, len(rec), err)
		}

		if tc.want == nil && len(rec) != 0 {
			t.Errorf("%s: Read held %d bytes of a record past the cap", tc.name, len(rec))
		}

		if tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("%s: Read returned %v, want %v", tc.name, err, tc.want)
		}
	}
}

// Read ends with its context's own error once the context has ended, a
// deadline or a cancel, with the server silent: contexts already ended, so no
// wait is on the clock, and one client for both, so the second Read shows the
// first left no deadline behind.
func TestReadEndsWithItsContext(t *testing.T) {
	c := dial(t, server(t, nil, true))

	expired, stop := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer stop()

	if _, err := within(expired, t, c); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Read past a deadline returned %v, want %v", err, context.DeadlineExceeded)
	}

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := within(cancelled, t, c); !errors.Is(err, context.Canceled) {
		t.Errorf("Read of a cancelled context returned %v, want %v", err, context.Canceled)
	}
}

// A Read that ended with its context leaves no deadline behind: the next call
// on the same client, with a live context, is answered.
func TestAReadAfterAnEndedContextReadsOn(t *testing.T) {
	addr, _ := peer(t, nil)
	c := dial(t, addr)

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := within(cancelled, t, c); !errors.Is(err, context.Canceled) {
		t.Fatalf("Read of a cancelled context returned %v, want %v", err, context.Canceled)
	}

	if _, err := c.Do(t.Context(), nfsclient.Call{Prog: progNFS, Vers: 3}); err != nil {
		t.Errorf("Do after a cancelled Read: %v", err)
	}
}

// gated is a connection whose SetReadDeadline of a time already past waits for
// the gate to open: the closure of a context that ended is held in the middle
// of its call.
type gated struct {
	net.Conn

	gate chan struct{}
}

func (g gated) SetReadDeadline(t time.Time) error {
	if !t.IsZero() {
		<-g.gate
	}

	return g.Conn.SetReadDeadline(t)
}

// Read does not return while the closure its context started is still running:
// that closure would otherwise set a past deadline on the client's next Read.
// The peer writes a whole record, the closure is held, and Read must be blocked
// on it; once the closure ends, Read returns the record.
func TestReadWaitsForTheClosureOfItsContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		near, far := net.Pipe()
		defer func() { _ = near.Close() }()
		defer func() { _ = far.Close() }()

		gate := make(chan struct{})
		open := sync.OnceFunc(func() { close(gate) })

		// A failed test still lets the held closure end, or the bubble deadlocks.
		defer open()

		c := nfsclient.Over(gated{Conn: near, gate: gate})

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		type result struct {
			rec []byte
			err error
		}

		done := make(chan result, 1)

		go func() {
			rec, err := c.Read(ctx)
			done <- result{rec, err}
		}()

		go func() {
			_, _ = far.Write(append(words(lastFragment|4), 1, 2, 3, 4))
		}()

		synctest.Wait()

		select {
		case r := <-done:
			t.Fatalf("Read returned %x, %v while the closure of its context was held", r.rec, r.err)
		default:
		}

		open()
		synctest.Wait()

		select {
		case r := <-done:
			if r.err != nil || !bytes.Equal(r.rec, []byte{1, 2, 3, 4}) {
				t.Errorf("Read returned %x, %v, want the record and no error", r.rec, r.err)
			}
		default:
			t.Error("Read did not return once the closure ended")
		}
	})
}
