package wire_test

//go:generate go run ./gen -o codec_gen_test.go codec_types_test.go

// The structs the codec's suite runs on, each MS-SMB2 2.2's layout field by
// field, together using every tag the generator honours.

// AccessMask is MS-SMB2 2.2.13.1's access mask.
type AccessMask uint32

// CloseRequest is MS-SMB2 2.2.15's CLOSE request: fixed fields alone.
type CloseRequest struct {
	_      uint16 `smb2:"size=24"`
	Flags  uint16
	_      uint32
	FileID [16]byte
}

// CreateRequest is MS-SMB2 2.2.13's CREATE request.
type CreateRequest struct {
	_                    uint16 `smb2:"size=57"`
	SecurityFlags        uint8
	RequestedOplockLevel uint8
	ImpersonationLevel   uint32
	SmbCreateFlags       uint64
	_                    uint64
	DesiredAccess        AccessMask
	FileAttributes       uint32
	ShareAccess          uint32
	CreateDisposition    uint32
	CreateOptions        uint32
	_                    uint16          `smb2:"name=NameOffset"`
	_                    uint16          `smb2:"name=NameLength"`
	_                    uint32          `smb2:"name=CreateContextsOffset"`
	_                    uint32          `smb2:"name=CreateContextsLength"`
	Name                 string          `smb2:"offset=NameOffset,len=NameLength,max=1024,utf16"`
	Contexts             []CreateContext `smb2:"offset=CreateContextsOffset,len=CreateContextsLength,max=4096,align=8,chain=Next"`
}

// CreateContext is MS-SMB2 2.2.13.2's SMB2_CREATE_CONTEXT, its data decoded
// by its name.
type CreateContext struct {
	_              uint32                 `smb2:"name=Next"`
	_              uint16                 `smb2:"name=NameOffset"`
	_              uint16                 `smb2:"name=NameLength"`
	_              uint16                 // Reserved
	_              uint16                 `smb2:"name=DataOffset"`
	_              uint32                 `smb2:"name=DataLength"`
	Name           []byte                 `smb2:"offset=NameOffset,len=NameLength,max=16,union"`
	DurableV2      DurableHandleRequestV2 `smb2:"offset=DataOffset,len=DataLength,align=8,case=DH2Q,exact"`
	AllocationSize AllocationSize         `smb2:"offset=DataOffset,len=DataLength,align=8,case=AlSi,exact"`
	Data           []byte                 `smb2:"offset=DataOffset,len=DataLength,align=8,max=1024,default"`
}

// DurableHandleRequestV2 is MS-SMB2 2.2.13.2.11's
// SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2, a DH2Q context's data.
type DurableHandleRequestV2 struct {
	Timeout    uint32
	Flags      uint32
	_          uint64
	CreateGUID [16]byte
}

// AllocationSize is MS-SMB2 2.2.13.2.6's SMB2_CREATE_ALLOCATION_SIZE, an AlSi
// context's data.
type AllocationSize struct {
	AllocationSize uint64
}

// NegotiateRequest is MS-SMB2 2.2.3's NEGOTIATE request, whose negotiate
// contexts are on the wire when its Dialects hold 3.1.1, the eight bytes
// before its dialects ClientStartTime, ignored, when they do not.
type NegotiateRequest struct {
	_            uint16 `smb2:"size=36"`
	_            uint16 `smb2:"name=DialectCount"`
	SecurityMode uint16
	_            uint16
	Capabilities uint32
	ClientGUID   [16]byte
	_            uint32             `smb2:"name=NegotiateContextOffset"`
	_            uint16             `smb2:"name=NegotiateContextCount"`
	_            uint16             // Reserved2
	Dialects     []uint16           `smb2:"count=DialectCount,max=64"`
	Contexts     []NegotiateContext `smb2:"offset=NegotiateContextOffset,count=NegotiateContextCount,max=16,align=8,chain,if=Dialects:0x311"`
}

// NegotiateResponse is MS-SMB2 2.2.4's NEGOTIATE response, whose negotiate
// contexts are on the wire at 3.1.1 alone, their count and offset Reserved
// before it.
type NegotiateResponse struct {
	_               uint16 `smb2:"size=65,empty"`
	SecurityMode    uint16
	DialectRevision uint16
	_               uint16 `smb2:"name=NegotiateContextCount"`
	ServerGUID      [16]byte
	Capabilities    uint32
	MaxTransactSize uint32
	MaxReadSize     uint32
	MaxWriteSize    uint32
	SystemTime      uint64
	ServerStartTime uint64
	_               uint16             `smb2:"name=SecurityBufferOffset"`
	_               uint16             `smb2:"name=SecurityBufferLength"`
	_               uint32             `smb2:"name=NegotiateContextOffset"`
	SecurityBuffer  []byte             `smb2:"offset=SecurityBufferOffset,len=SecurityBufferLength,max=4096"`
	Contexts        []NegotiateContext `smb2:"offset=NegotiateContextOffset,count=NegotiateContextCount,max=16,align=8,chain,since=0x311"`
}

// NegotiateContext is MS-SMB2 2.2.3.1's SMB2 NEGOTIATE_CONTEXT, its data
// decoded by its type.
type NegotiateContext struct {
	ContextType uint16 `smb2:"union"`
	_           uint16 `smb2:"name=DataLength"`
	_           uint32
	Preauth     PreauthIntegrityCapabilities `smb2:"len=DataLength,case=1"`
	Encryption  EncryptionCapabilities       `smb2:"len=DataLength,case=2"`
	Data        []byte                       `smb2:"len=DataLength,max=1024,default"`
}

// PreauthIntegrityCapabilities is MS-SMB2 2.2.3.1.1's
// SMB2_PREAUTH_INTEGRITY_CAPABILITIES.
type PreauthIntegrityCapabilities struct {
	_              uint16   `smb2:"name=HashAlgorithmCount"`
	_              uint16   `smb2:"name=SaltLength"`
	HashAlgorithms []uint16 `smb2:"count=HashAlgorithmCount,max=8"`
	Salt           []byte   `smb2:"len=SaltLength,max=64"`
}

// EncryptionCapabilities is MS-SMB2 2.2.3.1.2's SMB2_ENCRYPTION_CAPABILITIES.
type EncryptionCapabilities struct {
	_       uint16   `smb2:"name=CipherCount"`
	Ciphers []uint16 `smb2:"count=CipherCount,max=8"`
}

// WriteRequest is MS-SMB2 2.2.21's WRITE request, its channel information
// after its data, so the data can push that offset past its two bytes.
type WriteRequest struct {
	_              uint16 `smb2:"size=49,empty"`
	_              uint16 `smb2:"name=DataOffset"`
	_              uint32 `smb2:"name=Length"`
	Offset         uint64
	FileID         [16]byte
	Channel        uint32
	RemainingBytes uint32
	_              uint16 `smb2:"name=WriteChannelInfoOffset"`
	_              uint16 `smb2:"name=WriteChannelInfoLength"`
	Flags          uint32
	Data           []byte `smb2:"offset=DataOffset,len=Length,max=8388608"`
	ChannelInfo    []byte `smb2:"offset=WriteChannelInfoOffset,len=WriteChannelInfoLength,max=1024"`
}

// EchoRequest is MS-SMB2 2.2.28's ECHO request: no field but its size and
// Reserved, so no named one.
type EchoRequest struct {
	_ uint16 `smb2:"size=4"`
	_ uint16
}
