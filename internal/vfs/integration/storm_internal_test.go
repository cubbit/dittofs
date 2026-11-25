//go:build engine_conformance

package integration

import (
	"testing"
	"time"
)

// burstAt is a measured burst: how long it lasted, its rate and its chmod
// latency.
type burstAt struct {
	last time.Duration
	rate float64
	lat  time.Duration
}

// sizedAfter feeds the sizer the warm-up and then the bursts, and returns the
// N and the worker target it gives the burst after each of them, burst 1 first.
func sizedAfter(warm burstAt, bursts ...burstAt) (ns, workers []int64) {
	var z stormSizer

	z.measured(-1, warm.last, warm.rate, warm.lat)

	for k := range len(bursts) + 1 {
		n, w := z.next()
		ns, workers = append(ns, n), append(workers, w)

		if k < len(bursts) {
			z.measured(k, bursts[k].last, bursts[k].rate, bursts[k].lat)
		}
	}

	return ns, workers
}

func TestStormSizerBoundsTheStep(t *testing.T) {
	tests := []struct {
		name  string
		warm  burstAt
		burst burstAt
		want1 int64 // burst 1's N, from the warm-up
		want2 int64 // burst 2's N, after the step limit
	}{
		{
			name:  "up: a burst asks 80 times the N before it",
			warm:  burstAt{time.Second, 35, 0},
			burst: burstAt{80 * time.Second, 35, 0},
			want1: 560,
			want2: 1120,
		},
		{
			name:  "down: a burst asks for a fraction of the N before it",
			warm:  burstAt{50 * time.Second, 20, 0},
			burst: burstAt{time.Second, 20, 0},
			want1: 16000,
			want2: 8000,
		},
		{
			name:  "cap: no more than the pool",
			warm:  burstAt{5 * time.Second, 1000, 0},
			burst: burstAt{25 * time.Second, 1000, 0},
			want1: 80000,
			want2: 100000,
		},
		{
			name:  "floor: no fewer than the writers",
			warm:  burstAt{62500 * time.Microsecond, 100, 0},
			burst: burstAt{6250 * time.Microsecond, 100, 0},
			want1: 100,
			want2: 64,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns, _ := sizedAfter(tt.warm, tt.burst)

			if ns[0] != tt.want1 || ns[1] != tt.want2 {
				t.Fatalf("N of burst 1 and 2 = %d and %d, want %d and %d",
					ns[0], ns[1], tt.want1, tt.want2)
			}
		})
	}
}

func TestStormSizerBoundsBurstOne(t *testing.T) {
	tests := []struct {
		name string
		warm burstAt
		want int64
	}{
		{"a warm-up asking more than the pool", burstAt{25 * time.Second, 1000, 0}, 100000},
		{"a warm-up asking fewer than the writers", burstAt{62500 * time.Microsecond, 10, 0}, 64},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns, _ := sizedAfter(tt.warm)

			if ns[0] != tt.want {
				t.Fatalf("N of burst 1 = %d, want %d", ns[0], tt.want)
			}
		})
	}
}
