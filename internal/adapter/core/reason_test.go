package core

import (
	"fmt"
	"os"
	"testing"
)

// Every sentinel core closes with reaches the reason section 10.1 names for
// it, none falling through to violation: a close site a later change forgets
// to wrap shows up here first (§10.1).
func TestEveryCoreCloseSentinelMapsPastViolation(t *testing.T) {
	for _, err := range []error{
		errAtWrite,
		fmt.Errorf("%w: %w", errAtWrite, os.ErrDeadlineExceeded),
		errFrame,
		ErrDecode,
		errUnwrapClose,
		errUnwrapNeither,
		errWiring,
		errAtRead,
		fmt.Errorf("%w: %w", errAtRead, os.ErrDeadlineExceeded),
		errHandshake,
		errIdle,
		errStatusWait,
		errReplyWait,
		errDrain,
		errRemoved,
		errStop,
	} {
		if r := closeReason(err); r == rsnViolation {
			t.Errorf("the sentinel %v closed as violation", err)
		}
	}
}
