package journal

import (
	"context"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/capacity"
)

// Reservation holds what Reserve took for a known number of writes. Its writes
// draw from it, and while it holds enough none of them is refused by a limit.
//
// decision: one goroutine at a time owns a reservation — its writes and its
// Close — and Close must not run concurrently with a write of it. The written
// figures are guarded by the journal's own locks, but a reserved roll reads the
// spares its hold has left and the bytes its writes still need and then
// replaces, a read-then-replace that is not one step: two writes of one
// reservation in flight could both replace a spare and raise the hold by one
// each, and a Close racing them would return figures a draw is still moving.
// Bounded and returned at Close, that over-hold is the accepted cost of not
// serialising the pool behind a reservation-wide lock. Overturned by a caller
// that must run one reservation's writes concurrently, which would take a lock
// of the reservation around draw and replace together.
type Reservation struct {
	h *Handle
	r *capacity.Reservation
}

// WriteAt is the handle's WriteAt, drawing its record bytes, index entries,
// version and spares from the reservation. While the reservation holds enough,
// it is refused by none of the journal's limits and by no headroom rule.
//
// A write the reservation no longer holds enough for is refused with
// ErrReservationExhausted before any version is drawn or byte written, and one
// after the reservation was released — by its own Close, by its handle's or by
// the journal's — with ErrClosed. It enters the share as a call through the
// handle does, so a handle whose close began refuses it with ErrClosed and one
// in flight holds that close back.
func (r *Reservation) WriteAt(ctx context.Context, id FileID, off int64, p []byte, mtime int64) (Version, error) {
	return r.h.j.core.WriteReserved(ctx, r.h.s, r.r, id, off, p, mtime)
}

// Close returns what the reservation still holds to the journal: its bytes,
// footer entries, index entries, versions and spare segments. A second Close,
// or one after the handle's or the journal's Close already released it, returns
// ErrClosed.
func (r *Reservation) Close() error {
	if !r.h.release(r) {
		return ErrClosed
	}
	return nil
}
