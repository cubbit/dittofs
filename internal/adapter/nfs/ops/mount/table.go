// Package mount is the MOUNT protocol's operations, version 3's, one table
// for its procedures (RFC 21 section 11).
package mount

//go:generate go run ../../wire/gen -o codec_gen.go table.go null.go

import (
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/protocol"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
)

// nfs3 is the label of NFSv3 and its auxiliary programs, MOUNT among them
// (RFC 20 section 10.1).
const nfs3 = "nfs3"

// Table is MOUNT version 3's table, labelled nfs3: its NULL, procedure 0, its
// row carrying MOUNT's prefix on its name, since core refuses two tables of
// one label holding one row name, nfs3 covering all four programs. A procedure
// the table lacks is answered PROC_UNAVAIL (RFC 5531 section 9).
//
// The fault is the RPC accept state SYSTEM_ERR (RFC 21 section 11.5, "The
// fault status").
func Table() *core.OpTable {
	nullRow := core.Row[void, void](0, "MOUNT_NULL", core.NeedsNothing, null)
	nullRow.Versions = core.Range{First: wire.VersMount3, Last: wire.VersMount3}
	t := core.NewOpTable(
		protocol.ErrorTable(core.Status(wire.StatusAccept|wire.AcceptSystemErr)),
		false,
		nullRow,
	)
	t.Protocol = nfs3

	return t
}
