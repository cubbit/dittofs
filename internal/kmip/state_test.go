package kmip

import (
	"errors"
	"testing"

	"github.com/gemalto/kmip-go/kmip14"

	"github.com/cubbit/dittofs/internal/prof"
)

// The key-state table over every KMIP 1.4 state, a value KMIP 1.4 does not
// name, and two failed reads: only Destroyed and Destroyed Compromised are
// destroyed, only Active is active, and a failed read never reports either.
func TestClassify(t *testing.T) {
	prof.Watch(t, prof.Options{})
	itemNotFound := errors.New("kmip: GetAttributes failed: status=OperationFailed reason=ItemNotFound")
	dialErr := errors.New("kmip: dial 127.0.0.1:1: connection refused")

	for _, tc := range []struct {
		name  string
		state kmip14.State
		err   error
		want  Class
	}{
		{"Pre-Active", kmip14.StatePreActive, nil, Unavailable},
		{"Active", kmip14.StateActive, nil, Active},
		{"Deactivated", kmip14.StateDeactivated, nil, Unavailable},
		{"Compromised", kmip14.StateCompromised, nil, Unavailable},
		{"Destroyed", kmip14.StateDestroyed, nil, Destroyed},
		{"Destroyed Compromised", kmip14.StateDestroyedCompromised, nil, Destroyed},
		{"unknown value", kmip14.State(0x99), nil, Unavailable},
		{"zero value", kmip14.State(0), nil, Unavailable},
		{"Item Not Found", 0, itemNotFound, Unavailable},
		{"Item Not Found beside a Destroyed state", kmip14.StateDestroyed, itemNotFound, Unavailable},
		{"dial error", 0, dialErr, Unavailable},
		{"dial error beside an Active state", kmip14.StateActive, dialErr, Unavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.state, tc.err); got != tc.want {
				t.Fatalf("Classify(%v, %v) = %d, want %d", tc.state, tc.err, got, tc.want)
			}
		})
	}
	var zero Class
	if zero != Unavailable {
		t.Fatalf("the zero Class is %d, want Unavailable", zero)
	}
}
