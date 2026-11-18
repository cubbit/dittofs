package adapter

import (
	"testing"
)

func TestStateString(t *testing.T) {
	for s, want := range map[State]string{
		StateConfigured: "configured", StateServing: "serving",
		StateDraining: "draining", StateStopped: "stopped", 9: "State(9)",
	} {
		if got := s.String(); got != want {
			t.Errorf("State(%d).String() = %q, want %q", s, got, want)
		}
	}
}

// Ready is serving with no condition raised.
func TestHealthReady(t *testing.T) {
	for _, c := range []struct {
		h    Health
		want bool
	}{
		{Health{State: StateServing}, true},
		{Health{State: StateServing, Conditions: []string{"adapter_listener_lost"}}, false},
		{Health{State: StateConfigured}, false},
		{Health{State: StateDraining}, false},
		{Health{State: StateStopped}, false},
	} {
		if got := c.h.Ready(); got != c.want {
			t.Errorf("%+v.Ready() = %v, want %v", c.h, got, c.want)
		}
	}
}
