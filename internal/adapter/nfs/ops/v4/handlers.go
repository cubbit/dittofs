// Package v4 is NFSv4's operations: the one table every minor version shares,
// its COMPOUND row and the members' table each operation is a row of (RFC 20
// section 4.8, RFC 21 section 11).
package v4

//go:generate go run ../../wire/gen -o codec_gen.go -types codec_types_gen_test.go compound.go create_session.go exchange_id.go illegal.go table.go

import (
	"github.com/cubbit/dittofs/internal/adapter/nfs/protocol"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// Handlers is what NFSv4's handlers share: the service every operation calls,
// and the constructor of the callbacks value Connect registers per client.
type Handlers struct {
	VFS vfs.Service
	// Callbacks is set by nfs.New; the value Connect registers.
	Callbacks func(cl vfs.ClientID, p vfs.Protocol) vfs.Callbacks
	// Sessions holds the proposals EXCHANGE_ID answered until their confirm.
	Sessions *protocol.Sessions
	// Server is what EXCHANGE_ID names the node by.
	Server Server
}

// Server is the installation's ID and this node's ID, which EXCHANGE_ID
// answers as the server owner and scope (RFC 21 section 3.3).
type Server struct {
	Installation [16]byte
	Node         [16]byte
}
