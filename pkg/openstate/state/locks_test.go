package state_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/cubbit/dittofs/pkg/openstate/state"
)

// TestLockedErrorIsErrLocked pins that a conflict, wrapped or not, matches
// ErrLocked and hands back its holder, and that another error does not.
func TestLockedErrorIsErrLocked(t *testing.T) {
	holder := state.Lock{Owner: state.LockOwner{Name: []byte("h")}, Open: 7}
	err := fmt.Errorf("lock: %w", &state.LockedError{Holder: holder})
	if !errors.Is(err, state.ErrLocked) {
		t.Errorf("errors.Is(%v, ErrLocked) = false, want true", err)
	}
	var le *state.LockedError
	if !errors.As(err, &le) || le.Holder.Open != 7 {
		t.Errorf("errors.As holder = %+v, want Open 7", le)
	}
	if errors.Is(errors.New("other"), state.ErrLocked) {
		t.Error("an unrelated error matches ErrLocked")
	}
}
