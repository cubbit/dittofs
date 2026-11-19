package netlogon

import (
	"errors"
	"fmt"
	"testing"
)

// A controller's NTSTATUS refusal is a rejection: a caller probing whether a
// password is accepted tests errors.Is(err, ErrRejected), through any wrapping,
// and must never read a missing account as one.
func TestStatusMatchesErrRejected(t *testing.T) {
	for _, s := range []Status{StatusAccessDenied, StatusWrongPassword, StatusNologonWorkstationTrust} {
		err := fmt.Errorf("channel: %w", s)
		if !errors.Is(err, ErrRejected) {
			t.Errorf("errors.Is(%v, ErrRejected) = false, want true", err)
		}
		if errors.Is(err, ErrNoAccount) {
			t.Errorf("errors.Is(%v, ErrNoAccount) = true, want false", err)
		}
		var got Status
		if !errors.As(err, &got) || got != s {
			t.Errorf("errors.As(%v, *Status) = %v, want %v", err, got, s)
		}
	}
}

func TestStatusErrorNamesTheNTSTATUS(t *testing.T) {
	if got, want := StatusLogonFailure.Error(), "netlogon: rejected with NTSTATUS 0xc000006d"; got != want {
		t.Errorf("StatusLogonFailure.Error() = %q, want %q", got, want)
	}
}
