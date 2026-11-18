package wire_test

//go:generate go run ./gen -o codec_gen_test.go codec_types_test.go

// The structs the codec's suite is written against (RFC 20 section 5.4): NFS's
// own wherever a captured or specified message carries the tag rule, and
// Sample for the rules none here does. Where the protocol gives a field no
// bound, the bound is the test's.

// SetClientIDArgs is SETCLIENTID4args (RFC 7530 section 16.33).
type SetClientIDArgs struct {
	Client        ClientID
	Callback      CBClient
	CallbackIdent uint32
}

// ClientID is nfs_client_id4.
type ClientID struct {
	Verifier [8]byte
	ID       []byte `xdr:"max=1024"` // NFS4_OPAQUE_LIMIT
}

// CBClient is cb_client4.
type CBClient struct {
	Program  uint32
	Location NetAddr
}

// NetAddr is netaddr4.
type NetAddr struct {
	NetID string `xdr:"max=16"`
	Addr  string `xdr:"max=64"`
}

// SetattrArgs is SETATTR4args (RFC 7530 section 16.32).
type SetattrArgs struct {
	StateID StateID
	Attrs   FAttr
}

// StateID is stateid4.
type StateID struct {
	Seqid uint32
	Other [12]byte
}

// FAttr is fattr4: its attrmask, and its attributes as their bytes.
type FAttr struct {
	Mask []uint32 `xdr:"max=4"`
	List []byte   `xdr:"max=4096"`
}

// ACL is fattr4_acl (RFC 7530 section 6.2.1), as many entries as 1 MiB holds
// at an entry's least, 16 bytes.
type ACL struct {
	ACEs []ACE `xdr:"max=65536"`
}

// ACE is nfsace4.
type ACE struct {
	Type       uint32
	Flag       uint32
	AccessMask uint32
	Who        string `xdr:"max=1024"`
}

// OpenClaimType is open_claim_type4, an enum.
type OpenClaimType uint32

// OpenClaim is open_claim4 (RFC 8881 section 18.16.1): an arm per claim, the
// last three on the wire from minor version 1, and no default.
type OpenClaim struct {
	Claim            OpenClaimType `xdr:"union"`
	File             string        `xdr:"case=0,max=255"` // CLAIM_NULL
	DelegateType     uint32        `xdr:"case=1"`         // CLAIM_PREVIOUS
	DelegateCur      DelegateCur   `xdr:"case=2"`         // CLAIM_DELEGATE_CUR
	FileDelegatePrev string        `xdr:"case=3,max=255"` // CLAIM_DELEGATE_PREV
	_                struct{}      `xdr:"case=4,since=1"` // CLAIM_FH
	DelegateStateID  StateID       `xdr:"case=5,since=1"` // CLAIM_DELEG_CUR_FH
	_                struct{}      `xdr:"case=6,since=1"` // CLAIM_DELEG_PREV_FH
}

// DelegateCur is open_claim_delegate_cur4.
type DelegateCur struct {
	StateID StateID
	File    string `xdr:"max=255"`
}

// SetTime is settime4 (RFC 7530 section 2.2): the client's time, or, by the
// default arm, the server's.
type SetTime struct {
	How  uint32   `xdr:"union"`
	Time NFSTime  `xdr:"case=1"`  // SET_TO_CLIENT_TIME4
	_    struct{} `xdr:"default"` // SET_TO_SERVER_TIME4
}

// NFSTime is nfstime4.
type NFSTime struct {
	Seconds  int64
	NSeconds uint32
}

// PostOpFH is post_op_fh3 (RFC 1813), whose bool arm is optional data.
type PostOpFH struct {
	Handle *FH3
}

// FH3 is nfs_fh3.
type FH3 struct {
	Data []byte `xdr:"max=64"` // NFS3_FHSIZE
}

// StableHow is stable_how4, an enum.
type StableHow uint32

// Write4Args is WRITE4args (RFC 7530 section 16.36), its data up to RFC 21
// section 2.3's 1 MiB.
type Write4Args struct {
	StateID StateID
	Offset  uint64
	Stable  StableHow
	Data    []byte `xdr:"max=1048576"`
}

// Void is NULL's arguments and result (RFC 7530 section 15.1): void, no bytes
// (RFC 4506 section 4.16).
type Void struct{}

// Sample is no protocol's: a fixed-length array of three uint32, a bool, a
// string and an array whose vectors sit at their bounds, and a field on the
// wire from minor version 1.
type Sample struct {
	Words [3]uint32
	Flag  bool
	Name  string   `xdr:"max=8"`
	Mask  []uint32 `xdr:"max=3"`
	Extra uint64   `xdr:"since=1"`
}
