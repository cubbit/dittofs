package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/resources"
)

// tables builds one empty table per program, so a routed table is a pointer the
// test can compare.
func tables() (Tables, *core.ErrorTable) {
	errs := newErrors()
	empty := func() *core.OpTable { return core.NewOpTable(errs, false) }
	return Tables{V3: empty(), V4: empty(), Mount: empty(), NLM: empty(), NSM: empty()}, errs
}

// stageOf is a stage serving the range, with fresh tables.
func stageOf(v settings.VersionRange) *stage {
	t, errs := tables()
	return &stage{served: newServed(v), tables: t, errs: errs}
}

// authSys is an AUTH_SYS credential body: a stamp, a name, a uid, a gid and
// groups.
func authSys(name string, uid, gid uint32, gids ...uint32) []byte {
	b := binary.BigEndian.AppendUint32(nil, 0)
	b = binary.BigEndian.AppendUint32(b, uint32(len(name)))
	b = append(b, name...)
	for i := len(name); i%4 != 0; i++ {
		b = append(b, 0)
	}
	b = binary.BigEndian.AppendUint32(b, uid)
	b = binary.BigEndian.AppendUint32(b, gid)
	b = binary.BigEndian.AppendUint32(b, uint32(len(gids)))
	for _, g := range gids {
		b = binary.BigEndian.AppendUint32(b, g)
	}
	return b
}

// opaque appends an XDR opaque: its length, its body and their padding.
func opaque(dst, body []byte) []byte {
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(body)))
	dst = append(dst, body...)
	for i := len(body); i%4 != 0; i++ {
		dst = append(dst, 0)
	}
	return dst
}

// nullCall is a call with no COMPOUND prefix: xid, CALL, RPC version 2, the
// program, version and procedure, the credential and the verifier, then tail.
func nullCall(prog, vers, proc uint32, credFlavour uint32, cred []byte, verfFlavour uint32, verf []byte, tail []byte) []byte {
	b := binary.BigEndian.AppendUint32(nil, 0x11223344)
	for _, v := range []uint32{0, 2, prog, vers, proc} {
		b = binary.BigEndian.AppendUint32(b, v)
	}
	b = binary.BigEndian.AppendUint32(b, credFlavour)
	b = opaque(b, cred)
	b = binary.BigEndian.AppendUint32(b, verfFlavour)
	b = opaque(b, verf)
	return append(b, tail...)
}

// compoundCall is a COMPOUND call of the given minor version and tag, with no
// operations.
func compoundCall(minor uint32, tag string) []byte {
	b := nullCall(progNFS, versNFS4, 1, wire.AuthNone, nil, wire.AuthNone, nil, nil)
	b = opaque(b, []byte(tag))
	b = binary.BigEndian.AppendUint32(b, minor)
	return binary.BigEndian.AppendUint32(b, 1) // one operation, the tag's own count
}

// decode decodes a call through the stage, as the pipeline does.
func decode(t *testing.T, s *stage, body []byte) (core.Header, error) {
	t.Helper()
	buf := &resources.Buf{B: body}
	return s.Decode(buf, 0)
}

// status maps an error through the stages' table, as the pipeline does.
func status(s *stage, err error) core.Status {
	st, _ := s.errs.Map(err, nil, 0)
	return st
}

// record reads one of the wire package's golden records, hex with # comments,
// and returns its body, past the record mark.
func record(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("../wire/testdata/golden/" + name + ".hex")
	if err != nil {
		t.Fatal(err)
	}
	var digits strings.Builder
	for line := range strings.SplitSeq(string(data), "\n") {
		line, _, _ = strings.Cut(line, "#")
		digits.WriteString(strings.Join(strings.Fields(line), ""))
	}
	b, err := hex.DecodeString(digits.String())
	if err != nil {
		t.Fatalf("golden %s: %v", name, err)
	}
	n, last, err := wire.Fragment(b)
	if err != nil || !last || n != len(b)-wire.RecordMarkLen {
		t.Fatalf("golden %s: a mark of %d bytes, last %v, %v", name, n, last, err)
	}
	return b[wire.RecordMarkLen:]
}

// A success is the accepted header alone, as the reference server answers frame
// 4's NULL in frame 6, a COMPOUND's too: its encoder writes its status, tag and
// results.
func TestASuccessIsTheHeaderAlone(t *testing.T) {
	s := stageOf(settings.VersionRange{Min: "3", Max: "4.2"})
	frame6 := record(t, "nfsv4-null-reply")
	for _, name := range []string{"nfsv4-null-call", "nfsv4-compound-call", "nfsv4-compound-short-tag"} {
		t.Run(name, func(t *testing.T) {
			call := record(t, name)
			h, err := decode(t, s, call)
			if err != nil {
				t.Fatal(err)
			}
			out := &resources.Buf{}
			if err := s.Encode(h, 0, out); err != nil {
				t.Fatal(err)
			}
			want := append(append([]byte{}, call[:4]...), frame6[4:]...) // frame 6 under this call's xid
			if !bytes.Equal(out.B, want) {
				t.Errorf("a success writes %x, want %x", out.B, want)
			}
		})
	}
}

// A call to each served pair of a range routes to its table, and a version the
// range does not serve is refused with the program's own lowest and highest.
func TestTheServedRanges(t *testing.T) {
	for _, c := range []struct {
		name        string
		min, max    string
		served      []struct{ prog, vers uint32 }
		mismatch    []struct{ prog, lo, hi uint32 }
		unavailable []uint32
	}{
		{
			name: "3 to 4.2", min: "3", max: "4.2",
			served:   []struct{ prog, vers uint32 }{{progNFS, 3}, {progNFS, 4}, {progMount, 3}, {progNLM, 4}, {progNSM, 1}},
			mismatch: []struct{ prog, lo, hi uint32 }{{progNFS, 3, 4}, {progMount, 3, 3}, {progNLM, 4, 4}},
		},
		{
			name: "4.0 to 4.2", min: "4.0", max: "4.2",
			served:      []struct{ prog, vers uint32 }{{progNFS, 4}},
			mismatch:    []struct{ prog, lo, hi uint32 }{{progNFS, 4, 4}},
			unavailable: []uint32{progMount, progNLM, progNSM},
		},
		{
			name: "3 to 3", min: "3", max: "3",
			served:   []struct{ prog, vers uint32 }{{progNFS, 3}, {progMount, 3}, {progNLM, 4}, {progNSM, 1}},
			mismatch: []struct{ prog, lo, hi uint32 }{{progNFS, 3, 3}},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := stageOf(settings.VersionRange{Min: c.min, Max: c.max})
			for _, p := range c.served {
				h, err := decode(t, s, nullCall(p.prog, p.vers, 0, wire.AuthNone, nil, wire.AuthNone, nil, nil))
				if err != nil {
					t.Fatalf("%d/%d: %v", p.prog, p.vers, err)
				}
				if _, _, err := s.Route(h, nil); err != nil {
					t.Errorf("%d/%d is refused: %v", p.prog, p.vers, err)
				}
			}
			for _, m := range c.mismatch {
				// A version below the program's range, or NFS v2.
				req := uint32(2)
				switch m.prog {
				case progMount:
					req = 1
				case progNLM:
					req = 3
				}
				h, _ := decode(t, s, nullCall(m.prog, req, 0, wire.AuthNone, nil, wire.AuthNone, nil, nil))
				if _, _, err := s.Route(h, nil); !errors.Is(err, core.ErrVersion) {
					t.Fatalf("%d v%d: %v, want ErrVersion", m.prog, req, err)
				}
				if got := status(s, core.ErrVersion); got != statusProgMismatch {
					t.Errorf("%d v%d: status %#x", m.prog, req, uint32(got))
				}
				// The reply names this program's own lowest and highest.
				hm, _ := decode(t, s, nullCall(m.prog, req, 0, wire.AuthNone, nil, wire.AuthNone, nil, nil))
				out := &resources.Buf{}
				if err := s.Encode(hm, statusProgMismatch, out); err != nil {
					t.Fatal(err)
				}
				lo, hi := binary.BigEndian.Uint32(out.B[len(out.B)-8:]), binary.BigEndian.Uint32(out.B[len(out.B)-4:])
				if lo != m.lo || hi != m.hi {
					t.Errorf("%d v%d: PROG_MISMATCH names %d and %d, want %d and %d", m.prog, req, lo, hi, m.lo, m.hi)
				}
			}
			for _, prog := range c.unavailable {
				h, _ := decode(t, s, nullCall(prog, 1, 0, wire.AuthNone, nil, wire.AuthNone, nil, nil))
				if _, _, err := s.Route(h, nil); !errors.Is(err, errProgUnavail) {
					t.Errorf("program %d: %v, want PROG_UNAVAIL", prog, err)
				}
			}
		})
	}
}

// A minor version the range does not serve is refused with the version 4 table,
// and its answer echoes the request's tag.
func TestTheMinorVersions(t *testing.T) {
	s := stageOf(settings.VersionRange{Min: "4.0", Max: "4.1"})
	h, err := decode(t, s, compoundCall(2, "abcde"))
	if err != nil {
		t.Fatal(err)
	}
	table, _, err := s.Route(h, nil)
	if !errors.Is(err, core.ErrVersion) || table != s.tables.V4 {
		t.Fatalf("minor 2: %p, %v; want the version 4 table and ErrVersion", table, err)
	}
	out := &resources.Buf{}
	if err := s.Encode(h, core.Status(10021), out); err != nil {
		t.Fatal(err)
	}
	if len(out.B) != 24+4+4+8+4 {
		t.Errorf("NFS4ERR_MINOR_VERS_MISMATCH writes %d bytes: %x", len(out.B), out.B)
	}
	if h.Version() != 2 {
		t.Errorf("the header's version is %d, want the minor version 2", h.Version())
	}
	// A minor version the range does serve routes, though the RPC version's own
	// number is not it.
	h1, err := decode(t, s, compoundCall(1, "abcde"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Route(h1, nil); err != nil {
		t.Errorf("minor 1 under 4.0 to 4.1: %v", err)
	}

	// Two COMPOUNDs on one connection hold their own minor versions, and NFS
	// version 4's other procedures read at version 0 right after one.
	s2 := stageOf(settings.VersionRange{Min: "3", Max: "4.2"})
	hc, _ := decode(t, s2, compoundCall(1, "t"))
	if hc.Version() != 1 {
		t.Errorf("a COMPOUND of minor 1 reads as %d", hc.Version())
	}
	hn, _ := decode(t, s2, nullCall(progNFS, versNFS4, 0, wire.AuthNone, nil, wire.AuthNone, nil, nil))
	if hn.Version() != 0 {
		t.Errorf("a NULL after a COMPOUND reads at version %d", hn.Version())
	}
	for _, minor := range []uint32{1, 0} {
		h, err := decode(t, s2, compoundCall(minor, "t"))
		if err != nil {
			t.Fatal(err)
		}
		if h.Version() != core.Version(minor) {
			t.Errorf("a COMPOUND of minor %d reads as %d", minor, h.Version())
		}
		if _, _, err := s2.Route(h, nil); err != nil {
			t.Errorf("minor %d: %v", minor, err)
		}
	}
	// NFS version 4's other procedures are routed at version 0, also after a
	// COMPOUND: no minor version is kept from one call to the next.
	h, _ = decode(t, s2, nullCall(progNFS, versNFS4, 0, wire.AuthNone, nil, wire.AuthNone, nil, nil))
	if h.Version() != 0 {
		t.Errorf("a NULL to NFS v4 reads at version %d", h.Version())
	}
}

// The credential and verifier rules of RFC 21 section 11.1, each refused with
// no table.
func TestTheCredentials(t *testing.T) {
	s := stageOf(settings.VersionRange{Min: "3", Max: "4.2"})
	for _, c := range []struct {
		name string
		body []byte
		want error
	}{
		{"AUTH_DH", nullCall(progNFS, 3, 0, 3, nil, wire.AuthNone, nil, nil), errAuthCred},
		{"flavour 99", nullCall(progNFS, 3, 0, 99, nil, wire.AuthNone, nil, nil), errAuthCred},
		{"RPCSEC_GSS", nullCall(progNFS, 3, 0, wire.AuthGSS, []byte{0, 0, 0, 0}, wire.AuthNone, nil, nil), errAuthCred},
		{"a truncated AUTH_SYS", nullCall(progNFS, 3, 0, wire.AuthSys, []byte{0, 0}, wire.AuthNone, nil, nil), errAuthCred},
		{"17 groups", nullCall(progNFS, 3, 0, wire.AuthSys, authSys("h", 0, 0, make([]uint32, 17)...), wire.AuthNone, nil, nil), errAuthCred},
		{"a 256-byte name", nullCall(progNFS, 3, 0, wire.AuthSys, authSys(string(make([]byte, 256)), 0, 0), wire.AuthNone, nil, nil), errAuthCred},
		{"an AUTH_NONE credential with a body", nullCall(progNFS, 3, 0, wire.AuthNone, []byte{1, 2, 3, 4}, wire.AuthNone, nil, nil), errAuthCred},
		{"an AUTH_SYS verifier", nullCall(progNFS, 3, 0, wire.AuthNone, nil, wire.AuthSys, authSys("h", 0, 0), nil), errAuthVerf},
		{"an AUTH_NONE verifier with a body", nullCall(progNFS, 3, 0, wire.AuthNone, nil, wire.AuthNone, []byte{1, 2, 3, 4}, nil), errAuthVerf},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, err := decode(t, s, c.body)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			table, _, err := s.Route(h, nil)
			if table != nil || !errors.Is(err, c.want) {
				t.Fatalf("%p, %v; want no table and %v", table, err, c.want)
			}
			if got := status(s, err); got != statusAuthCred && got != statusAuthVerf {
				t.Errorf("status %#x, want an AUTH_ERROR", uint32(got))
			}
		})
	}
	// An RPC version other than 2 is the first refusal.
	b := nullCall(progNFS, 3, 0, wire.AuthSys, nil, wire.AuthNone, nil, nil)
	b[11] = 3 // the RPC version's low byte: 3
	h, _ := decode(t, s, b)
	if _, _, err := s.Route(h, nil); !errors.Is(err, errRPCMismatch) {
		t.Errorf("RPC version 3: %v, want RPC_MISMATCH", err)
	}
}

// A message that is not a call is dropped, and a record the wire cannot address
// is answered nothing.
func TestTheNonCalls(t *testing.T) {
	s := stageOf(settings.VersionRange{Min: "3", Max: "4.2"})
	reply := binary.BigEndian.AppendUint32(nil, 0x11223344)
	reply = binary.BigEndian.AppendUint32(reply, 1)
	h, err := decode(t, s, reply)
	if h == nil || !errors.Is(err, errNotCall) {
		t.Fatalf("a reply: %v, %v", h, err)
	}
	if st, answer := s.errs.Map(err, nil, 0); answer {
		t.Errorf("a reply is answered with %#x, want a NoReply row", uint32(st))
	}
	if h, err := decode(t, s, make([]byte, 7)); h != nil {
		t.Errorf("a seven-byte record yields a header with %v", err)
	}
	if got := status(s, decodeErr(t, s, make([]byte, 7))); got != core.Status(wire.StatusAccept|wire.AcceptGarbageArgs) {
		t.Errorf("a seven-byte record maps to %#x, want GARBAGE_ARGS", uint32(got))
	}
}

// decodeErr is the error a decode of body returns.
func decodeErr(t *testing.T, s *stage, body []byte) error {
	t.Helper()
	_, err := decode(t, s, body)
	if err == nil {
		t.Fatal("decode succeeded")
	}
	return err
}

// A call whose later fields do not decode comes back with its header, and the
// stages' table answers GARBAGE_ARGS.
func TestTheDecodeRefusals(t *testing.T) {
	s := stageOf(settings.VersionRange{Min: "3", Max: "4.2"})
	over := nullCall(progNFS, 3, 0, wire.AuthSys, nil, wire.AuthNone, nil, nil)
	// A credential whose length claims 401 bytes, with the body absent.
	over = over[:28]
	over = binary.BigEndian.AppendUint32(over, 401)
	short := nullCall(progNFS, 3, 0, wire.AuthNone, nil, wire.AuthNone, nil, nil)
	short = short[:len(short)-4] // the verifier's length, with nothing after it
	short = short[:len(short)-4]
	for _, c := range []struct {
		name string
		body []byte
	}{
		{"a 401-byte credential", over},
		{"a verifier past the record", short},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, err := decode(t, s, c.body)
			if h == nil || !errors.Is(err, core.ErrDecode) {
				t.Fatalf("%v, %v; want a header and ErrDecode", h, err)
			}
			if got := status(s, err); got != core.Status(wire.StatusAccept|wire.AcceptGarbageArgs) {
				t.Errorf("status %#x, want GARBAGE_ARGS", uint32(got))
			}
		})
	}
}

// Args begins past the verifier, which for a COMPOUND is its tag.
func TestArgsBeginAtTheTag(t *testing.T) {
	s := stageOf(settings.VersionRange{Min: "3", Max: "4.2"})
	body := nullCall(progNFS, versNFS4, 1, wire.AuthSys, authSys("dittofs-knfsd-ref", 0, 0), wire.AuthNone, nil, nil)
	body = opaque(body, []byte("abcde"))
	body = binary.BigEndian.AppendUint32(body, 0)
	body = binary.BigEndian.AppendUint32(body, 1)
	h, err := decode(t, s, body)
	if err != nil {
		t.Fatal(err)
	}
	args := s.Args(&resources.Buf{B: body}, h)
	if len(args) != 20 {
		t.Errorf("args are %d bytes, want the tag, the minor version and the count", len(args))
	}
	if binary.BigEndian.Uint32(args) != 5 {
		t.Errorf("args begin with %#x, not the tag's length", binary.BigEndian.Uint32(args))
	}
}

// ReplyLen bounds what Encode writes, for every status and a COMPOUND's tag.
func TestReplyLenBoundsEncode(t *testing.T) {
	s := stageOf(settings.VersionRange{Min: "3", Max: "4.2"})
	for _, c := range []struct {
		name string
		body []byte
		st   []core.Status
	}{
		{"a NULL", nullCall(progNFS, 3, 0, wire.AuthNone, nil, wire.AuthNone, nil, nil), []core.Status{
			core.Status(wire.StatusProgram), statusProgUnavail, statusProgMismatch, statusRPCMismatch, statusAuthCred, statusAuthVerf,
			core.Status(wire.StatusAccept | wire.AcceptGarbageArgs), core.Status(wire.StatusAccept | wire.AcceptSystemErr),
		}},
		{"a five-byte-tag COMPOUND", compoundCall(0, "abcde"), []core.Status{0, core.Status(10021)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, err := decode(t, s, c.body)
			if err != nil {
				t.Fatal(err)
			}
			bound := s.ReplyLen(h)
			for _, st := range c.st {
				out := &resources.Buf{}
				if err := s.Encode(h, st, out); err != nil {
					t.Fatal(err)
				}
				if len(out.B) > bound {
					t.Errorf("status %#x writes %d bytes, past ReplyLen %d", uint32(st), len(out.B), bound)
				}
			}
		})
	}
}

// The header stage's decode, its credential check and its encode of an
// AUTH_SYS call allocate nothing, and Route allocates once: the call's own
// request.
func TestHeaderStageAllocs(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the race detector changes allocation counts")
	}
	s := stageOf(settings.VersionRange{Min: "3", Max: "4.2"})
	body := nullCall(progNFS, versNFS4, 1, wire.AuthSys, authSys("host", 1000, 100, 27), wire.AuthNone, nil, nil)
	body = opaque(body, []byte("abcde"))
	body = binary.BigEndian.AppendUint32(body, 0)
	body = binary.BigEndian.AppendUint32(body, 1)
	buf := &resources.Buf{B: body}
	h, err := s.Decode(buf, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := &resources.Buf{B: make([]byte, 0, s.ReplyLen(h))}
	if n := testing.AllocsPerRun(100, func() {
		h1, _ := s.Decode(buf, 0)
		if err := checkAuth(h1.(header).call()); err != nil {
			t.Fatal(err)
		}
		out.B = out.B[:0]
		_ = s.Encode(h1, 0, out)
	}); n != 0 {
		t.Errorf("decode, the credential check and encode allocate %v per call", n)
	}
	if n := testing.AllocsPerRun(100, func() {
		if _, _, err := s.Route(h, nil); err != nil {
			t.Fatal(err)
		}
	}); n != 1 {
		t.Errorf("route allocates %v per call, want its request alone", n)
	}
}

// Calls on one connection run at once, as core runs them: the reader decodes
// each record in turn while the calls before it still run, and each call is
// routed and answered on a goroutine of its own. Each reply carries its own
// call's xid, and each request its own call's version.
func TestConcurrentCallsKeepTheirOwn(t *testing.T) {
	s := stageOf(settings.VersionRange{Min: "3", Max: "4.2"})
	calls := []struct {
		body []byte
		vers core.Version
	}{
		{nullCall(progNFS, versNFS3, 0, wire.AuthNone, nil, wire.AuthNone, nil, nil), 3},
		{compoundCall(1, "a"), 1},
		{nullCall(progNFS, versNFS4, 0, wire.AuthSys, authSys("h", 1, 1), wire.AuthNone, nil, nil), 0},
		{nullCall(progMount, versMount3, 0, wire.AuthNone, nil, wire.AuthNone, nil, nil), 3},
		{compoundCall(2, "abcde"), 2},
		{nullCall(progNLM, versNLM4, 0, wire.AuthNone, nil, wire.AuthNone, nil, nil), 4},
		{nullCall(progNSM, versNSM1, 0, wire.AuthNone, nil, wire.AuthNone, nil, nil), 1},
		{compoundCall(0, "t"), 0},
	}
	running := make(chan struct{})
	replies := make([][]byte, len(calls))
	versions := make([]core.Version, len(calls))
	var wg sync.WaitGroup
	for i, c := range calls {
		binary.BigEndian.PutUint32(c.body, uint32(0x1000+i))
		h, err := s.Decode(&resources.Buf{B: c.body}, 0)
		if err != nil {
			t.Fatal(err)
		}
		wg.Go(func() {
			_, r, err := s.Route(h, nil)
			if err != nil {
				t.Errorf("call %d: %v", i, err)
				return
			}
			<-running // the call runs on while the reader decodes the next
			out := &resources.Buf{}
			if err := s.Encode(h, 0, out); err != nil {
				t.Errorf("call %d: %v", i, err)
				return
			}
			replies[i], versions[i] = out.B, r.Version
		})
	}
	close(running)
	wg.Wait()
	for i, c := range calls {
		if len(replies[i]) < 4 {
			continue
		}
		if got := binary.BigEndian.Uint32(replies[i]); got != uint32(0x1000+i) {
			t.Errorf("call %d is answered with xid %#x, want %#x", i, got, 0x1000+i)
		}
		if versions[i] != c.vers {
			t.Errorf("call %d is routed at version %d, want %d", i, versions[i], c.vers)
		}
	}
}

// A header's version is its call's, but for NFS version 4: a COMPOUND's minor
// version and 0 for its other procedures; the request Route builds carries it.
func TestTheHeaderVersions(t *testing.T) {
	s := stageOf(settings.VersionRange{Min: "3", Max: "4.2"})
	for _, c := range []struct {
		name string
		body []byte
		want core.Version
	}{
		{"NFS v3", nullCall(progNFS, versNFS3, 0, wire.AuthNone, nil, wire.AuthNone, nil, nil), 3},
		{"MOUNT v3", nullCall(progMount, versMount3, 0, wire.AuthNone, nil, wire.AuthNone, nil, nil), 3},
		{"NLM v4", nullCall(progNLM, versNLM4, 0, wire.AuthNone, nil, wire.AuthNone, nil, nil), 4},
		{"NSM v1", nullCall(progNSM, versNSM1, 0, wire.AuthNone, nil, wire.AuthNone, nil, nil), 1},
		{"NFS v4 NULL", nullCall(progNFS, versNFS4, 0, wire.AuthNone, nil, wire.AuthNone, nil, nil), 0},
		{"a COMPOUND of minor 2", compoundCall(2, "t"), 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, err := decode(t, s, c.body)
			if err != nil {
				t.Fatal(err)
			}
			if h.Version() != c.want {
				t.Errorf("the header's version is %d, want %d", h.Version(), c.want)
			}
			_, r, err := s.Route(h, nil)
			if err != nil {
				t.Fatal(err)
			}
			if r.Version != c.want {
				t.Errorf("the request's version is %d, want %d", r.Version, c.want)
			}
		})
	}
}
