package protocol_test

//go:generate go run ../wire/gen -o row_gen_test.go row_types_test.go

// AccessArgs is ACCESS4args (RFC 7530 section 16.1), the request of the row
// built over the generated codec.
type AccessArgs struct {
	Access uint32
}

// AccessRes is ACCESS4resok, its reply.
type AccessRes struct {
	Supported uint32
	Access    uint32
}
