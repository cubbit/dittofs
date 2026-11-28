package v4

import (
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// base is what a CREATE_SESSION builds on: the client as the service knows
// it or as its proposal held, the adapter bytes the build must replace, and
// the proposal to confirm, nil where the client is confirmed already. It is not on the wire, so it
// lives outside the files the codec is generated from.
type base struct {
	info     vfs.ClientInfo
	prev     []byte
	rec      wire.ClientRec
	proposal *vfs.Proposal
}
