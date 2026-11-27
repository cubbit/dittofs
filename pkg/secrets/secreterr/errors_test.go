package secreterr_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/cubbit/dittofs/pkg/secrets/secreterr"
)

// TestRefusalsAreDistinct holds that each refusal is matched by errors.Is
// through a wrap and by no other refusal, since a caller answers each one
// with its own status.
func TestRefusalsAreDistinct(t *testing.T) {
	refusals := map[string]error{
		"ErrWrongRole":   secreterr.ErrWrongRole,
		"ErrWrongKind":   secreterr.ErrWrongKind,
		"ErrKindChanged": secreterr.ErrKindChanged,
		"ErrUnproven":    secreterr.ErrUnproven,
	}
	for name, err := range refusals {
		wrapped := fmt.Errorf("putting a secret: %w", err)
		for other, target := range refusals {
			if got, want := errors.Is(wrapped, target), name == other; got != want {
				t.Errorf("errors.Is(wrapped %s, %s) = %v, want %v", name, other, got, want)
			}
		}
	}
}
