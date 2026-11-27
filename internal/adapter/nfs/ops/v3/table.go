// Package v3 is NFS version 3's operations, one table for its procedures (RFC
// 21 section 11).
package v3

//go:generate go run ../../wire/gen -o codec_gen.go table.go null.go

import (
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/protocol"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
)

// nfs3 is the label of NFSv3 and its auxiliary programs (RFC 20 section 10.1).
const nfs3 = "nfs3"

// Table is NFS version 3's table, labelled nfs3: its NULL, procedure 0. A
// procedure the table lacks is answered PROC_UNAVAIL (RFC 5531 section 9).
func Table() *core.OpTable {
	nullRow := core.Row[void, void](0, "NULL", core.NeedsNothing, null)
	nullRow.Versions = core.Range{First: wire.VersNFS3, Last: wire.VersNFS3}
	t := core.NewOpTable(protocol.ErrorTable(core.Status(wire.NFS3ErrServerFault)), false, nullRow)
	t.Protocol = nfs3

	return t
}
