package a

import "testing"

func TestLocalFlag(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts")
	}
}

func TestProfFlag(t *testing.T) {
	if prof.RaceEnabled {
		t.Skipf("allocation counts %d", 1)
	}
}

// A branch that changes no check is no gate.
func TestRaceBranchWithoutSkip(t *testing.T) {
	if raceEnabled {
		n = 2
	}
}

func TestNestedSkip(t *testing.T) {
	t.Run("x", func(t *testing.T) {
		if raceEnabled {
			t.Skip("in a closure")
		}
	})
}

func TestShortOrRace(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("large")
	}
}

func TestNegatedAssertion(t *testing.T) {
	if !raceEnabled && allocs >= 1 {
		t.Errorf("allocated")
	}
}

func TestContinueInLoop(t *testing.T) {
	for range 2 {
		if prof.RaceEnabled {
			continue
		}
		check()
	}
}

// A switch is not an if, so a case on the flag is no gate.
func TestSwitch(t *testing.T) {
	switch {
	case raceEnabled:
		n = 2
	}
}
