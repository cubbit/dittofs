// Package protocol is NFS's ONC RPC protocol: its served versions, the header
// stage that decodes a call and writes its reply, and the router that chooses
// an operation table for it (RFC 20 sections 4.1 and 4.7).
package protocol

import "github.com/cubbit/dittofs/internal/adapter/core"

// Tables are NFS's operation tables, one per served program and version (RFC 20
// section 4.8). A nil table's program and version are not served: a program
// left with none is answered PROG_UNAVAIL, a version of one still served
// PROG_MISMATCH.
type Tables struct {
	V3    *core.OpTable // NFS version 3
	V4    *core.OpTable // NFS version 4, every minor version served at once
	Mount *core.OpTable // MOUNT version 3
	NLM   *core.OpTable // NLM version 4
	NSM   *core.OpTable // NSM version 1
}
