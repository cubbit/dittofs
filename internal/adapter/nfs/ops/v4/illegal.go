package v4

import (
	"context"

	"github.com/cubbit/dittofs/internal/adapter/core"
)

// illegalArgs is ILLEGAL's arguments: every byte left in the COMPOUND after
// an operation code no minor version defines.
type illegalArgs []byte

// DecodeWire takes every byte it is handed.
//
// decision: an undefined code's arguments have no layout to read, so ILLEGAL
// takes the rest of the COMPOUND, the members before it run, and the COMPOUND
// is answered NFS4ERR_OP_ILLEGAL there, rather than refused whole as
// GARBAGE_ARGS for the bytes after it. Overturned if a client is seen relying
// on GARBAGE_ARGS there.
func (a *illegalArgs) DecodeWire(b []byte, _ uint32) (int, error) {
	*a = b
	return len(b), nil
}

// illegalRes is ILLEGAL4res past its status: nothing.
type illegalRes struct{}

// illegal answers an operation code no minor version defines with its error,
// mapped NFS4ERR_OP_ILLEGAL.
func (h *Handlers) illegal(context.Context, *core.Request, illegalArgs) (illegalRes, error) {
	return illegalRes{}, errIllegal
}
