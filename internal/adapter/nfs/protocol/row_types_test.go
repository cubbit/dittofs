package protocol_test

//go:generate go run ../wire/gen -o row_gen_test.go row_types_test.go

// AccessArgs is ACCESS4args (RFC 7530 section 16.1), the request of a row
// built over the generated codec.
type AccessArgs struct {
	Access uint32
}

// AccessRes is ACCESS4res past its status: ACCESS4resok on a success, its
// fail arm void.
type AccessRes struct {
	Ok *AccessResOk `xdr:"ok"`
}

// AccessResOk is ACCESS4resok.
type AccessResOk struct {
	Supported uint32
	Access    uint32
}

// Write3Args is WRITE3args (RFC 1813 section 3.3.7), its data up to RFC 21
// section 2.3's 1 MiB.
type Write3Args struct {
	File   []byte `xdr:"max=64"` // nfs_fh3
	Offset uint64
	Count  uint32
	Stable uint32 // stable_how
	Data   []byte `xdr:"max=1048576"`
}

// Write3Res is WRITE3res past its status: file_wcc in either arm, and on a
// success the rest of WRITE3resok.
type Write3Res struct {
	FileWcc WccData
	Ok      *Write3ResOk `xdr:"ok"`
}

// Write3ResOk is WRITE3resok past its file_wcc.
type Write3ResOk struct {
	Count     uint32
	Committed uint32 // stable_how
	Verf      [8]byte
}

// WccData is wcc_data: pre_op_attr and post_op_attr, each optional.
type WccData struct {
	Before *WccAttr
	After  *Fattr3
}

// WccAttr is wcc_attr.
type WccAttr struct {
	Size  uint64
	Mtime NFSTime3
	Ctime NFSTime3
}

// Fattr3 is fattr3 (RFC 1813 section 2.6).
type Fattr3 struct {
	Type   uint32 // ftype3
	Mode   uint32
	Nlink  uint32
	UID    uint32
	GID    uint32
	Size   uint64
	Used   uint64
	Rdev   SpecData3
	Fsid   uint64
	Fileid uint64
	Atime  NFSTime3
	Mtime  NFSTime3
	Ctime  NFSTime3
}

// SpecData3 is specdata3.
type SpecData3 struct {
	Specdata1 uint32
	Specdata2 uint32
}

// NFSTime3 is nfstime3.
type NFSTime3 struct {
	Seconds  uint32
	Nseconds uint32
}
