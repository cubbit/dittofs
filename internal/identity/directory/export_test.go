package directory

import (
	"context"
	"time"
)

// DialTLSWithin is DialTLS with ceiling d in place of the package's.
func DialTLSWithin(ctx context.Context, rec Record, d time.Duration) (Conn, error) {
	return dialTLS(ctx, rec, d)
}
