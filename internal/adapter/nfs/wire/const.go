// Package wire is NFS's codecs: the record mark RFC 5531 section 11 writes, the
// call section 9 carries, the AUTH_SYS credential Appendix A defines, and the
// replies section 9.2 answers with. It holds no state, does no I/O, and reads
// no clock (RFC 20 section 2.5).
package wire

// The programs and the versions NFS serves, MOUNT, NLM and NSM being NFS
// version 3's auxiliary programs, and the procedure whose calls carry more
// than a header (RFC 5531 Appendix A, RFC 21 sections 2.1 and 2.2).
const (
	ProgNFS   uint32 = 100003
	ProgMount uint32 = 100005
	ProgNLM   uint32 = 100021
	ProgNSM   uint32 = 100024

	VersNFS3   uint32 = 3
	VersNFS4   uint32 = 4
	VersMount3 uint32 = 3
	VersNLM4   uint32 = 4
	VersNSM1   uint32 = 1

	ProcCompound uint32 = 1
)

// NFS3ErrServerFault is NFSv3's fault, NFS3ERR_SERVERFAULT (RFC 1813 section
// 2.6), which NFSv3's table answers an error no row names with (RFC 21
// section 11.5).
const NFS3ErrServerFault uint32 = 10006

// The message types, reply states, accept states, reject states, auth stats
// and flavours are RFC 5531's section 9 and Appendix A, and RFC 2203's.
const (
	callType  uint32 = 0 // CALL
	replyType uint32 = 1 // REPLY

	replyAccepted uint32 = 0 // MSG_ACCEPTED
	replyDenied   uint32 = 1 // MSG_DENIED

	// AcceptSuccess is RPC executed successfully; the program's reply follows.
	AcceptSuccess uint32 = 0
	// AcceptProgUnavail is a program the server does not serve.
	AcceptProgUnavail uint32 = 1
	// AcceptProgMismatch is a version the program does not serve, with the lowest and highest it does.
	AcceptProgMismatch uint32 = 2
	// AcceptProcUnavail is a procedure the program does not define.
	AcceptProcUnavail uint32 = 3
	// AcceptGarbageArgs is a call whose later fields did not decode.
	AcceptGarbageArgs uint32 = 4
	// AcceptSystemErr is a fault of the server's own.
	AcceptSystemErr uint32 = 5

	rejectRPCMismatch uint32 = 0 // RPC_MISMATCH
	rejectAuthError   uint32 = 1 // AUTH_ERROR

	// AuthBadCred is a credential the server refuses: malformed, or over RFC 5531's limits.
	AuthBadCred uint32 = 1
	// AuthBadVerf is a verifier the server refuses.
	AuthBadVerf uint32 = 3

	rpcVersion uint32 = 2 // the only RPC version

	// AuthNone is AUTH_NONE, the verifier's flavour and the anonymous credential's.
	AuthNone uint32 = 0
	// AuthSys is AUTH_SYS.
	AuthSys uint32 = 1
	// AuthGSS is RPCSEC_GSS (RFC 2203).
	AuthGSS uint32 = 6

	// maxAuthBody is the most an opaque_auth's body may be (RFC 5531 section 8.2).
	maxAuthBody = 400
)

// How a Status tells a program's status from the RPC reply state that stands
// in its place, for the header codec's Encode: the two high bits name the
// layer. Layer 0 is a program's status inside an accepted success, so 0 stays
// success; 1 an accept state; 2 RPC_MISMATCH; 3 AUTH_ERROR, its auth_stat in
// the low bits. No program's status reaches bit 30: nfsstat3, nfsstat4,
// mountstat3 and NLM's and NSM's codes all lie below 2^16.
//
// decision: one uint32 carries both layers. It is this surface's own type, so
// no prior art was sought. Overturned if a program's status ever needs bit 30.
const (
	StatusProgram  uint32 = 0 << 30
	StatusAccept   uint32 = 1 << 30
	StatusMismatch uint32 = 2 << 30
	StatusAuth     uint32 = 3 << 30
)
