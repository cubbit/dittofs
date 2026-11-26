package engine_test

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/engine"
	"github.com/cubbit/dittofs/pkg/engine/enginetest"
)

// Apply refuses each invalid setting with an error naming its key, and the
// engine keeps the settings it already had rather than any part of the
// refused ones or a default.
func TestApplyRefusesInvalidAndKeepsLast(t *testing.T) {
	u := newUnitWith(t, unitConfig{unstarted: true})

	good := enginetest.Settings
	if err := u.e.Apply(good); err != nil {
		t.Fatalf("Apply(%+v) = %v", good, err)
	}

	for _, c := range []struct {
		key string
		bad func(*engine.Settings)
	}{
		{"existence_age", func(s *engine.Settings) { s.ExistenceAge = 0 }},
		{"existence_age", func(s *engine.Settings) { s.ExistenceAge = -time.Second }},
		{"group_commit.max_files", func(s *engine.Settings) { s.GroupCommitMaxFiles = 0 }},
		{"group_commit.max_files", func(s *engine.Settings) { s.GroupCommitMaxFiles = -1 }},
		{"clone_max_len", func(s *engine.Settings) { s.CloneMaxLen = 0 }},
		{"clone_max_len", func(s *engine.Settings) { s.CloneMaxLen = -1 }},
		{"offload.retry_min", func(s *engine.Settings) { s.OffloadRetryMin = 0 }},
		{"offload.retry_min", func(s *engine.Settings) { s.OffloadRetryMin = -time.Second }},
		{"offload.retry_max", func(s *engine.Settings) { s.OffloadRetryMax = 0 }},
		{"offload.retry_max", func(s *engine.Settings) { s.OffloadRetryMax = -time.Second }},
		{"offload.retry_max_pressure", func(s *engine.Settings) { s.OffloadRetryMaxPressure = 0 }},
		{"offload.retry_max_pressure", func(s *engine.Settings) { s.OffloadRetryMaxPressure = -time.Second }},
		// The floor above the pressure cap, the cap below the floor: pressure lowers the cap.
		{"offload.retry_min", func(s *engine.Settings) { s.OffloadRetryMin = s.OffloadRetryMaxPressure + 1 }},
		// The pressure cap above the normal cap.
		{"offload.retry_max_pressure", func(s *engine.Settings) { s.OffloadRetryMaxPressure = s.OffloadRetryMax + 1 }},
		{"removal.rescan", func(s *engine.Settings) { s.RemovalRescan = 0 }},
		{"removal.rescan", func(s *engine.Settings) { s.RemovalRescan = -time.Second }},
		{"eviction.repack_threshold", func(s *engine.Settings) { s.RepackThreshold = 0 }},
		{"eviction.repack_threshold", func(s *engine.Settings) { s.RepackThreshold = 1 }},
		{"eviction.repack_threshold", func(s *engine.Settings) { s.RepackThreshold = -0.1 }},
		{"eviction.repack_threshold", func(s *engine.Settings) { s.RepackThreshold = math.NaN() }},
		{"eviction occupancy target", func(s *engine.Settings) { s.EvictionTarget = 0 }},
		{"eviction occupancy target", func(s *engine.Settings) { s.EvictionTarget = 1 }},
		{"eviction occupancy target", func(s *engine.Settings) { s.EvictionTarget = 1.5 }},
		{"eviction occupancy target", func(s *engine.Settings) { s.EvictionTarget = math.NaN() }},
		{"fill low-water mark", func(s *engine.Settings) { s.FillLowWater = 0 }},
		{"fill low-water mark", func(s *engine.Settings) { s.FillLowWater = 1 }},
		{"fill low-water mark", func(s *engine.Settings) { s.FillLowWater = -0.5 }},
		{"fill low-water mark", func(s *engine.Settings) { s.FillLowWater = math.NaN() }},
	} {
		// Every other field differs from good, so a refused Apply that stored
		// part of its value shows.
		s := engine.Settings{
			ExistenceAge:            2 * time.Second,
			GroupCommitMaxFiles:     128,
			CloneMaxLen:             1 << 20,
			OffloadRetryMin:         2 * time.Second,
			OffloadRetryMax:         2 * time.Minute,
			OffloadRetryMaxPressure: 10 * time.Second,
			RemovalRescan:           2 * time.Minute,
			RepackThreshold:         0.2,
			EvictionTarget:          0.9,
			FillLowWater:            0.2,
		}
		c.bad(&s)

		err := u.e.Apply(s)
		if err == nil || !strings.Contains(err.Error(), c.key) {
			t.Errorf("Apply(%+v) = %v; want refused naming %s", s, err, c.key)
		}

		if got := u.e.AppliedSettings(); got != good {
			t.Errorf("after refused Apply(%+v): settings %+v, want the last %+v kept", s, got, good)
		}
	}

	// The orderings' bounds are inclusive: a floor equal to the pressure cap,
	// and a pressure cap equal to the normal cap, are taken.
	next := enginetest.Settings
	next.OffloadRetryMin = time.Minute
	next.OffloadRetryMaxPressure = time.Minute
	next.OffloadRetryMax = time.Minute
	if err := u.e.Apply(next); err != nil {
		t.Fatalf("Apply(%+v) = %v", next, err)
	}

	if got := u.e.AppliedSettings(); got != next {
		t.Fatalf("settings %+v after Apply, want %+v", got, next)
	}
}
