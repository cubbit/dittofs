package v4

//go:generate go run ../../wire/gen -o harness_gen_test.go harness_types_test.go

// write4Args is WRITE4args (RFC 8881 section 18.32), the arguments of a test
// member, its data up to RFC 21 section 2.3's 1 MiB.
type write4Args struct {
	Stateid stateid4
	Offset  uint64
	Stable  uint32 // stable_how4
	Data    []byte `xdr:"max=1048576"`
}

// stateid4 is a stateid4.
type stateid4 struct {
	Seqid uint32
	Other [12]byte
}

// write4Res is WRITE4res past its status: WRITE4resok on a success, its fail
// arm void.
type write4Res struct {
	Ok *write4ResOk `xdr:"arm"`
}

// write4ResOk is WRITE4resok.
type write4ResOk struct {
	Count     uint32
	Committed uint32 // stable_how4
	Verf      [8]byte
}
