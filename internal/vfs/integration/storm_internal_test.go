//go:build engine_conformance

package integration

import (
	"slices"
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

func TestStormSizerLeavesTheWarmUpOut(t *testing.T) {
	tests := []struct {
		name        string
		warm        burstAt
		bursts      []burstAt
		wantN       []int64
		wantWorkers []int64
	}{
		{
			name: "the warm-up's rate and latency do not size burst 2",
			warm: burstAt{10 * time.Second, 300, 2 * time.Second},
			bursts: []burstAt{
				{10 * time.Second, 500, time.Second},
			},
			wantN:       []int64{48000, 80000},
			wantWorkers: []int64{2000, 1000},
		},
		{
			name: "the median is of the last three real bursts",
			warm: burstAt{5 * time.Second, 600, 0},
			bursts: []burstAt{
				{5 * time.Second, 600, 0},
				{5 * time.Second, 650, 0},
				{5 * time.Second, 700, 0},
				{5 * time.Second, 760, 0},
			},
			wantN:       []int64{48000, 48000, 50000, 52000, 56000},
			wantWorkers: []int64{0, 0, 0, 0, 0},
		},
		{
			name: "a warm-up with nothing measured leaves burst 1's own values in",
			warm: burstAt{time.Second, 0, 0},
			bursts: []burstAt{
				{2 * time.Second, 500, time.Second},
			},
			wantN:       []int64{16000, 16000},
			wantWorkers: []int64{0, 1000},
		},
		{
			name: "a long chmod latency is capped at the most workers",
			warm: burstAt{time.Second, 500, 0},
			bursts: []burstAt{
				{time.Second, 500, 5 * time.Second},
			},
			wantN:       []int64{8000, 8000},
			wantWorkers: []int64{0, 4096},
		},
		{
			name: "a measured rate above the storm's rate sizes N as the storm's rate",
			warm: burstAt{time.Second, 500, 0},
			bursts: []burstAt{
				{750 * time.Millisecond, 2000, 0},
			},
			wantN:       []int64{8000, 12000},
			wantWorkers: []int64{0, 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns, workers := sizedAfter(tt.warm, tt.bursts...)

			if !slices.Equal(ns, tt.wantN) {
				t.Fatalf("N of each burst = %v, want %v", ns, tt.wantN)
			}

			if !slices.Equal(workers, tt.wantWorkers) {
				t.Fatalf("worker target of each burst = %v, want %v", workers, tt.wantWorkers)
			}
		})
	}
}
