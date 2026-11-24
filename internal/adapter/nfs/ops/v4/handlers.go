// Package v4 is NFSv4's operations: the one table every minor version shares,
// its COMPOUND row and the members' table each operation is a row of (RFC 20
// section 4.8, RFC 21 section 11).
package v4

//go:generate go run ../../wire/gen -o codec_gen.go compound.go illegal.go table.go

import "github.com/cubbit/dittofs/pkg/vfs"

// Handlers is what NFSv4's handlers share: the service every operation calls.
type Handlers struct{ VFS vfs.Service }
