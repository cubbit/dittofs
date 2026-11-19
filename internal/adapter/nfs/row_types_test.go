package nfs

//go:generate go run ./wire/gen -o row_gen_test.go row_types_test.go

// void is NULL's arguments and result: no bytes (RFC 7530 section 15.1, RFC
// 1813 section 3.3.0).
type void struct{}
