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

// protocolID is SMB2's ProtocolId, 0xFE 'S' 'M' 'B' in network order, read
// as the little-endian field it is (MS-SMB2 2.2.1.2).
const protocolID = 0x424D53FE

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

// The statuses the server's tables name (MS-ERREF 2.3.1).
const (
	StatusInvalidParameter = 0xC000000D
	StatusNotSupported     = 0xC00000BB
)
