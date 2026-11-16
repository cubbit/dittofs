package core

// What the package's external tests reach of core's unexported parts. It is
// the standard library's export_test.go by another name, since
// exports_test.go here is the surface test.

// NewCapped is the FlowControl a connection gets where its protocol has none.
func NewCapped() FlowControl { return newCapped() }

var (
	ReserveReply  = reserveReply
	ReserveStatus = reserveStatus
	ErrReplyWait  = errReplyWait
)
