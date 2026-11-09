package core

import (
	"errors"

	"github.com/cubbit/dittofs/pkg/resources"
)

// HoldCodec is the stage a table that Holds needs. The key doubles as the
// async ID the interim and final replies carry. The interim, like every reply,
// passes Security.Wrap, so an encrypting session sends nothing in clear.
// Cancels is asked after Unwrap, so an unverified cancel ends no hold. A
// cancel that finds no hold or park with its key is remembered for at most
// 5 s, and at most MaxPending per connection, the oldest dropped first; a
// request with that key that holds or parks meanwhile ends as cancelled. A
// cancel whose target was already answered leaves nothing after 5 s.
//
// ponytail: a cancel that arrives more than 5 s before its target holds or
// parks is lost, and the target runs to its answer. Keep the record until the
// connection's message-ID window passes the key if a client is found that
// cancels that early.
type HoldCodec interface {
	Key(h Header) uint64
	// Interim encodes STATUS_PENDING: it grants the request's credits if
	// grant, else only what keeps the client's balance at one, and always
	// those of chain members answered before it. For a drain's refusal it is
	// handed the headers of the members after, and grants theirs too.
	//
	// decision: the credits of the members after a refused one are granted
	// from headers no Security stage verified, since those members are never
	// unwrapped. The grant is bounded by the sum's saturation at the
	// connection's credit target, so a forged value grants a client at most
	// the target it could already hold. Verify each header before granting if
	// a client is found that turns such a grant into more than the target.
	Interim(h Header, grant bool, after []Header, out *resources.Buf) error
	// Final is the final reply's header, with the async ID. If grant (true
	// only when its interim withheld), it grants the request's credits less
	// what the interim granted of them; otherwise none.
	Final(h Header, grant bool) Header
	Cancels(h Header) (key uint64, ok bool) // a CANCEL ends the hold it names and gets no reply
	// Pends says whether a chain member before the last, answered err whose
	// row holds, goes pending (true) or is answered at once as ErrNotLast.
	//
	// decision: Pends is asked only for an error the service returned; a
	// member the interim timer finds still in the service always pends, since
	// it can apply after any answer, and answering it at once could report a
	// failed WRITE whose data lands a moment later. Withdraw it only if the
	// service gains a way to withdraw a call it has not yet applied.
	Pends(h Header, err error) bool
}

// ErrParked is what a handler returns, with its reply value, after Park: it
// waits for a callback (a waiting lock, a change notification) and fills the
// reply value before calling done.
var ErrParked = errors.New("core: parked")
