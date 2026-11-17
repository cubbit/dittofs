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
)

// The protocol IDs that open a message on the Direct TCP stream, read the
// little-endian field they are (MS-SMB2 2.2.1.2): the stage that sorts a
// message by what opens it - the reader the RFC 22 plan's sorting belongs
// to - tells a server's own header, a transform header the reader unwraps
// (RFC 22 section 4.1), and an SMB1 NEGOTIATE the server answers (RFC 22
// section 3.1) apart. DecodeHeader runs after that sort; the sort needs the
// IDs, and no other export does.
const (
	ProtocolSMB2      = 0x424D53FE // SMB2, 0xFE 'S' 'M' 'B'
	ProtocolTransform = 0x424D53FD // the transform header the reader unwraps
	ProtocolSMB1      = 0x424D53FF // SMB1, an 0xFF NEGOTIATE among others
)

// protocolID is SMB2's own ID, the one DecodeHeader compares (MS-SMB2 2.2.1.2).
const protocolID = ProtocolSMB2

// lastCommand is OPLOCK_BREAK, the highest of the command codes MS-SMB2
// 2.2.1.2 defines, all those from 0 to it. MS-SMB2 2.2.1.1 adds 0x0013,
// SERVER_TO_CLIENT_NOTIFICATION, which only a server sends (MS-SMB2 2.2.44).
const lastCommand = 0x0012

// Flags bits (MS-SMB2 2.2.1.1).
const (
	FlagServerToRedir     = 0x00000001 // the message is a response
	FlagAsyncCommand      = 0x00000002 // the header is in its async form
	FlagRelatedOperations = 0x00000004 // the request is a related member of a chain
	FlagSigned            = 0x00000008 // the message is signed
)

// replyFlags is the request's flag bits a reply carries (MS-SMB2 3.3.4.4's
// echoed bits): RELATED_OPERATIONS, the priority mask, DFS_OPERATION and
// REPLAY_OPERATION. ASYNC_COMMAND is not one: a reply takes the async form
// only for a request the server put off, which the stage that does it owns,
// with the AsyncId it chose.
const replyFlags = FlagRelatedOperations | 0x00000070 /* PRIORITY_MASK */ | 0x10000000 /* DFS_OPERATION */ | 0x20000000 /* REPLAY_OPERATION */

// The statuses the server's tables name (MS-ERREF 2.3.1).
const (
	StatusInvalidParameter = 0xC000000D
	StatusNotSupported     = 0xC00000BB
)
