package wire

// Sizes.
const (
	// FrameHeaderLen is the Direct TCP header's length (MS-SMB2 2.1).
	FrameHeaderLen = 4
	// MaxRecord is the longest message a Direct TCP header may count: the
	// large buffer class's capacity, 8 MiB and 4 KiB (RFC 22 §3.3).
	MaxRecord = 8<<20 + 4<<10
	// HeaderLen is the SMB2 header's length, which its StructureSize states
	// (MS-SMB2 2.2.1).
	HeaderLen = 64
	// ErrorLen is the length of the ERROR response's body AppendError
	// writes: its StructureSize of 9 counts one byte of ErrorData (MS-SMB2
	// 2.2.2).
	ErrorLen = 9
)

// ProtocolSMB2 is the protocol ID that opens a server's own SMB2 message on
// the Direct TCP stream, 0xFE 'S' 'M' 'B' read as the little-endian field it
// is (MS-SMB2 2.2.1.2), the one header DecodeHeader decodes. A transform
// header the reader unwraps (RFC 22 section 4.1) and an SMB1 NEGOTIATE the
// server answers (RFC 22 section 3.1) are told apart upstream, and their IDs
// arrive with the reader that reads them.
const ProtocolSMB2 = 0x424D53FE

// CommandCancel is CANCEL, which is answered nothing (MS-SMB2 2.2.1.2,
// 3.3.5.16): the protocol's router refuses it, ErrCancel in hand.
const CommandCancel = 0x000C

// lastCommand is OPLOCK_BREAK, the highest of the command codes MS-SMB2
// 2.2.1.2 defines, all those from 0 to it. MS-SMB2 2.2.1.1 adds 0x0013,
// SERVER_TO_CLIENT_NOTIFICATION, which only a server sends (MS-SMB2 2.2.44).
const lastCommand = 0x0012

// Flags bits (MS-SMB2 2.2.1.1).
const (
	FlagServerToRedir     = 0x00000001 // the message is a response
	FlagAsyncCommand      = 0x00000002 // the header is in its async form
	FlagRelatedOperations = 0x00000004 // the request is a related member of a chain
)

// replyFlags is the request's flag bits a reply carries: RELATED_OPERATIONS,
// the priority mask, DFS_OPERATION and REPLAY_OPERATION. ASYNC_COMMAND is not
// one: a reply takes the async form only for a request the server put off,
// which the stage that does it owns, with the AsyncId it chose.
//
// decision: MS-SMB2 3.3.4.4, "Sending an Error Response", writes the reply
// over the request's header without naming which request bits it keeps, and
// the capture at hand backs RELATED_OPERATIONS and the priority mask alone.
// DFS_OPERATION and REPLAY_OPERATION are carried on the reading that a reply
// states nothing the request did not carry. They stay while no capture shows
// a reply dropping them; withdraw the bit one does.
const replyFlags = FlagRelatedOperations | 0x00000070 /* PRIORITY_MASK */ | 0x10000000 /* DFS_OPERATION */ | 0x20000000 /* REPLAY_OPERATION */

// The statuses the server's tables name (MS-ERREF 2.3.1).
const (
	StatusInvalidParameter = 0xC000000D
	StatusNotSupported     = 0xC00000BB
	StatusInternalError    = 0xC00000E5
)
