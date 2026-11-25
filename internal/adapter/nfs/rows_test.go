package nfs

import (
	"context"
	"encoding/binary"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/protocol"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	"github.com/cubbit/dittofs/pkg/adapter"
)

// goldenCalls is every golden file that holds one whole RPC call, past a
// record mark that frames the whole file: a reply or a codec vector is no
// call and reaches no row. File names are not read, so a capture counts the moment
// it is cut.
func goldenCalls(t *testing.T) [][]byte {
	t.Helper()
	files, err := filepath.Glob("wire/testdata/golden/*.hex")
	if err != nil || len(files) == 0 {
		t.Fatalf("no golden file found: %v", err)
	}
	var calls [][]byte
	for _, f := range files {
		b := golden(t, strings.TrimSuffix(filepath.Base(f), ".hex"))
		// A capture cut with its record mark counts too: a last fragment's
		// mark whose length is the rest of the file is stripped.
		if len(b) >= 4 && b[0]&0x80 != 0 && int(binary.BigEndian.Uint32(b)&^lastFragment) == len(b)-4 {
			b = b[4:]
		}
		if c, err := wire.ParseCall(b); err == nil && c.RPCVersion() == 2 {
			calls = append(calls, b)
		}
	}
	return calls
}

// unreached names each row of ts that no call in calls reaches, as
// "<label> <name>": a call reaches the row of its program, version and
// procedure, and a COMPOUND's members, read through its members' table, reach
// theirs, each decoded at the COMPOUND's minor version until one does not.
//
// decision: the check lives here, the one package that imports every table,
// and not in wire beside the files, since wire cannot import the ops
// packages. Overturned if the tables move to a package wire may import.
func unreached(ts protocol.Tables, calls [][]byte) []string {
	reached := map[*core.Op]bool{}
	tables := map[[2]uint32]*core.OpTable{
		{wire.ProgNFS, wire.VersNFS3}: ts.V3, {wire.ProgNFS, wire.VersNFS4}: ts.V4,
		{wire.ProgMount, wire.VersMount3}: ts.Mount, {wire.ProgNLM, wire.VersNLM4}: ts.NLM,
		{wire.ProgNSM, wire.VersNSM1}: ts.NSM,
	}
	for _, b := range calls {
		c := wire.Call(b)
		t := tables[[2]uint32{c.Program(), c.Version()}]
		if t == nil {
			continue
		}
		op := t.Ops[c.Procedure()]
		if op == nil {
			continue
		}
		reached[op] = true
		if op.Members == nil {
			continue
		}
		minor, _ := c.MinorVersion()
		args := c.Args()
		inv, n, err := op.Decode(args, core.Version(minor))
		if err != nil {
			continue
		}
		rest := args[n:]
		for range inv.Count {
			code, n, err := op.Members.ReadOp(rest)
			if err != nil {
				break
			}
			m := op.Members.Ops[code]
			if m == nil {
				break
			}
			reached[m] = true
			_, k, err := m.Decode(rest[n:], core.Version(minor))
			if err != nil {
				break
			}
			rest = rest[n+k:]
		}
	}
	var missing []string
	var walk func(label string, t *core.OpTable)
	walk = func(label string, t *core.OpTable) {
		if t == nil {
			return
		}
		for _, op := range t.Ops {
			if !reached[op] {
				missing = append(missing, label+" "+op.Name)
			}
			walk(label, op.Members)
		}
	}
	for _, t := range []*core.OpTable{ts.V3, ts.V4, ts.Mount, ts.NLM, ts.NSM} {
		if t != nil {
			walk(t.Protocol, t)
		}
	}
	slices.Sort(missing)
	return missing
}

// Every row of every NFS table is reached by a golden call, written from a
// specification or cut from a capture (RFC 20 section 5.4): a row added
// without one fails here, naming it.
func TestEveryRowHasAVector(t *testing.T) {
	if missing := unreached(tables(adapter.Deps{}), goldenCalls(t)); len(missing) > 0 {
		t.Errorf("rows no golden call reaches: %s", strings.Join(missing, ", "))
	}
}

// The check names a row no golden call reaches, a NULL on NFSv3 here, and
// only that row: GETATTR beside it is reached by nfs-v3-getattr-call, a
// golden file that keeps its record mark. A check that passed vacuously, or
// skipped a file cut with its mark, would pass the tree too.
func TestTheVectorCheckNamesAnUnreachedRow(t *testing.T) {
	ts := tables(adapter.Deps{})
	ts.V3 = core.NewOpTable(ts.V3.Errors, false,
		core.Row[void, void](0, "null", core.NeedsNothing,
			func(context.Context, *core.Request, void) (void, error) { return void{}, nil }),
		core.Row[void, void](1, "getattr", core.NeedsNothing,
			func(context.Context, *core.Request, void) (void, error) { return void{}, nil }))
	ts.V3.Protocol = "nfs3"
	if got := unreached(ts, goldenCalls(t)); !slices.Equal(got, []string{"nfs3 null"}) {
		t.Errorf("unreached rows %v, want [nfs3 null]", got)
	}
}
