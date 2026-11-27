// Package nsm is the network status monitor's operations, version 1's, one
// table for its procedures (RFC 21 section 11).
package nsm

//go:generate go run ../../wire/gen -o codec_gen.go table.go null.go

import (
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/protocol"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
)

// nfs3 is the label of NFSv3 and its auxiliary programs, NSM among them
// (RFC 20 section 10.1).
const nfs3 = "nfs3"

// Table is NSM version 1's table, labelled nfs3: its NULL, procedure 0, its row
// carrying NSM's prefix on its name, since core refuses two tables of one label
// holding one row name, nfs3 covering all four programs. A procedure the table
// lacks is answered PROC_UNAVAIL (RFC 5531 section 9).
//
// The fault is the RPC accept state SYSTEM_ERR (RFC 21 section 11.5, "The
// fault status").
func Table() *core.OpTable {
	nullRow := core.Row[void, void](0, "NSM_NULL", core.NeedsNothing, null)
	nullRow.Versions = core.Range{First: wire.VersNSM1, Last: wire.VersNSM1}
	t := core.NewOpTable(
		protocol.ErrorTable(core.Status(wire.StatusAccept|wire.AcceptSystemErr)),
		false,
		nullRow,
	)
	t.Protocol = nfs3

	return t
}
