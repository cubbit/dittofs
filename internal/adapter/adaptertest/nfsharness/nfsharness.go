// Package nfsharness is NFS's handler harness: adaptertest's runner, with
// what NFS adds to it, an NFS result's status in its place and a COMPOUND
// member's request carrying the compound's state.
package nfsharness

import (
	"encoding/binary"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/protocol"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	"github.com/cubbit/dittofs/pkg/resources"
)

// Row runs the row of tab whose code is code, a procedure of NFSv3 or of an
// auxiliary program prog at r's version, over args with r, and answers its
// result as a client reads it after the RPC reply header: what the header
// stage writes for the program's status, laid out by wire.AppendStatus as the
// stage lays it out (the status word, NLM's cookie echoed from args ahead of
// it, or nothing for a result with no status), then the row's reply. It fails
// t as adaptertest's Run does, and stops it where the row's error maps to an
// RPC accept or reject state, which the header stage answers in place of a
// result.
func Row(t testing.TB, rn *adaptertest.Runner, tab *core.OpTable, prog, code uint32, r *core.Request, args []byte, e adaptertest.Expect) []byte {
	t.Helper()
	rep := rn.Run(t, tab, code, r, args, e)
	if rep.Status>>30 != 0 {
		t.Fatalf("nfsharness: row %d answered %#x, an RPC accept or reject state, not a program's status", code, uint32(rep.Status))
	}
	// The call a client would send: xid, CALL, RPC version 2, the program,
	// version and procedure, AUTH_NONE credential and verifier, then args.
	rec := []byte{}
	for _, w := range []uint32{1, 0, 2, prog, uint32(r.Version), code, 0, 0, 0, 0} {
		rec = binary.BigEndian.AppendUint32(rec, w)
	}
	c, err := wire.ParseCall(append(rec, args...))
	if err != nil {
		t.Fatalf("nfsharness: row %d: its call does not parse: %v", code, err)
	}
	return append(wire.AppendStatus(nil, c, uint32(rep.Status)), rep.Body...)
}

// Member runs the member of the NFSv4 members' table members whose code is
// code over args, with r, whose State is the *protocol.State the members
// before it would have left, which the test builds. It answers the member's
// result as the COMPOUND's reply carries it: the head the table's
// EncodeResult writes, its operation code and status, then the member's
// reply. It fails t as adaptertest's Run does.
func Member(t testing.TB, rn *adaptertest.Runner, members *core.OpTable, code uint32, r *core.Request, args []byte, e adaptertest.Expect) []byte {
	t.Helper()
	if _, ok := r.State.(*protocol.State); !ok {
		t.Fatalf("nfsharness: a member's request carries %T, not the COMPOUND's *protocol.State", r.State)
	}
	rep := rn.Run(t, members, code, r, args, e)
	var out resources.Buf
	if err := members.EncodeResult(&out, code, rep.Status, r.Version); err != nil {
		t.Fatalf("nfsharness: member %d: its result head: %v", code, err)
	}
	return append(out.B, rep.Body...)
}
