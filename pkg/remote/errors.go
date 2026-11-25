package remote

import (
	"errors"
	"fmt"
	"strconv"
)

// Errors: every failure a store returns wraps exactly one of these (RFC 4
// §4.8), ErrThrottled counting as itself though it also matches ErrTransient,
// and ErrMalformed as itself though it also matches ErrCorrupt.
// A local failure (a cancelled context, a failing body reader) is returned as
// itself. Every ErrDenied is a *DeniedError, which carries its Cause.
var (
	ErrNotFound  = errors.New("remote: block not found")
	ErrInvalid   = errors.New("remote: invalid request")
	ErrDenied    = errors.New("remote: denied")
	ErrTransient = errors.New("remote: transient")
	ErrCorrupt   = errors.New("remote: corrupt in transit")
	// ErrThrottled wraps ErrTransient: the service asked to be sent less.
	ErrThrottled = fmt.Errorf("remote: throttled: %w", ErrTransient)
	// ErrMalformed wraps ErrCorrupt: a recorded range the store refused as past
	// the end, which a re-get cannot cure (RFC 4 §3.4).
	ErrMalformed = fmt.Errorf("remote: malformed: %w", ErrCorrupt)
)

// Cause says why a store refused (ErrDenied), so a caller can choose between
// waiting and failing a write as out of space (RFC 8 §10.2).
type Cause uint8

const (
	CauseAccess Cause = iota + 1 // credentials, permissions, a missing bucket, a wrong endpoint
	CauseQuota                   // a quota or storage limit of the service is reached
	CauseSize                    // a body-size limit refused a block put (RFC 4 §4.8)
)

// String is the cause's word as RFC 4 §4.8 writes it, which a metric label and
// a per-direction health report carry.
func (c Cause) String() string {
	switch c {
	case CauseAccess:
		return "access"
	case CauseQuota:
		return "quota"
	case CauseSize:
		return "size"
	}

	return "Cause(" + strconv.Itoa(int(c)) + ")"
}

// DeniedError is the error behind every ErrDenied: its Cause, and Code, the
// service's error code. It carries neither the reply's message nor a header.
type DeniedError struct {
	Cause Cause
	Code  string
}

func (e *DeniedError) Error() string { return "remote: denied (" + e.Cause.String() + "): " + e.Code }

// Unwrap makes every DeniedError match ErrDenied.
func (e *DeniedError) Unwrap() error { return ErrDenied }
