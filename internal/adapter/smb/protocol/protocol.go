package protocol

import (
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/smb/wire"
)

var _ core.Protocol = (*Protocol)(nil)

// Protocol is SMB2 on one listener: the Direct TCP framer and each
// connection's stages over SMB's one table, every dialect's (RFC 20 §4.7,
// §4.8).
type Protocol struct{ table *core.OpTable }

// New returns the protocol serving t.
func New(t *core.OpTable) *Protocol { return &Protocol{table: t} }

// Framer reads and writes the Direct TCP header (MS-SMB2 2.1).
func (*Protocol) Framer() core.Framer { return framer{} }

// Tables is the one table the router returns.
func (p *Protocol) Tables() []*core.OpTable { return []*core.OpTable{p.table} }

// NewConn returns c's stages: the header stage, the router over the table,
// the table's own errors for a refusal before Route, and the label smb. Every
// other stage is nil and takes core's default (RFC 20 §4.7). It reads nothing
// of c, which is nil when NewServer checks the stages.
func (p *Protocol) NewConn(c *core.Conn) core.Stages {
	return core.Stages{
		Header:   headerStage{},
		Router:   &router{table: p.table, conn: c},
		Errors:   p.table.Errors,
		Protocol: "smb",
	}
}

// framer is SMB2's core.Framer: every Direct TCP header ends its record.
type framer struct{}

func (framer) HeaderLen() int { return wire.FrameHeaderLen }

// BodyLen reads a Direct TCP header's length. A first byte that is not zero,
// or a length past MaxRecord, is wire's refusal, and the connection closes
// before anything is reserved (RFC 20 §5.2, MS-SMB2 2.1).
func (framer) BodyLen(hdr []byte) (int, bool, error) {
	n, err := wire.FrameLen(hdr)
	return n, true, err
}

// MaxRecord is 8 MiB and 4 KiB, in one fragment (RFC 22 §3.3).
func (framer) MaxRecord() (int, int) { return wire.MaxRecord, 1 }

// Frame appends the Direct TCP header of a record of body bytes.
//
// decision: wire's refusal of a length past MaxRecord is dropped: core keeps
// every record within MaxRecord, counted as Frame states it (RFC 20 §4.7),
// and Frame returns no error. Were a longer one to reach here, it would go
// out with no header, and the client would read its protocol ID's 0xFE where
// the Direct TCP header's zero byte belongs (MS-SMB2 2.1). Check it, and
// close the connection, if Frame comes to return an error.
func (framer) Frame(dst []byte, body int) []byte {
	dst, _ = wire.AppendFrame(dst, body)
	return dst
}
