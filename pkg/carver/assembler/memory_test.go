package assembler_test

import (
	"encoding/binary"
	"runtime"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/carver"
	"github.com/cubbit/dittofs/pkg/carver/assembler"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// foldedGiB is how many chunk bytes each run of TestAllocsFoldMemory folds.
const foldedGiB = 1 << 30

// heldAfterGiB folds foldedGiB of distinct carried chunks of size bytes, 16 to
// a block, dropping every plan Add returns, and reports the live heap the fold
// holds after its last Add: LiveHeap after it minus LiveHeap before New,
// signed, since the heap can end lower than it began.
func heldAfterGiB(t *testing.T, size int32) int64 {
	t.Helper()
	cfg := carver.AssemblerConfig{BlockTarget: 16 * int64(size), MaxChunks: 1024, KeyBudget: 1 << 30}
	var file entity.FileID
	binary.BigEndian.PutUint64(file[:], 1)

	before := prof.LiveHeap()
	f, err := assembler.New(cfg)
	if err != nil {
		t.Fatalf("New(%+v): %v", cfg, err)
	}
	for i := range int64(foldedGiB / int64(size)) {
		var h entity.ChunkHash
		binary.BigEndian.PutUint64(h[:], uint64(i+1))
		f.Add(carver.ChunkInfo{File: file, Chunk: carver.Chunk{Offset: i * int64(size), Length: size, Hash: h}, Keys: 1, Oldest: t0})
	}
	after := prof.LiveHeap()
	runtime.KeepAlive(f)
	return int64(after) - int64(before)
}

// TestAllocsFoldMemory checks that the fold's memory does not grow with the
// bytes it folds or with its chunks' sizes (RFC 2 §5.5): 1 GiB folded as 64 KiB
// chunks into 1 MiB blocks, 64 times as many chunks as the same GiB folded as
// 4 MiB chunks into 64 MiB blocks, leaves the same live heap within 1 MiB. A
// fold that kept every closed plan holds about 3.5 MiB more at 64 KiB chunks,
// and one that kept a buffer per chunk length holds 63 MiB more at 4 MiB.
func TestAllocsFoldMemory(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the race detector's shadow memory makes the live heap unmeasurable")
	}
	const tolerance = 1 << 20

	small := heldAfterGiB(t, 64<<10)
	large := heldAfterGiB(t, 4<<20)
	t.Logf("live heap held after 1 GiB: %d bytes at 64 KiB chunks, %d bytes at 4 MiB chunks", small, large)
	if d := small - large; d > tolerance || d < -tolerance {
		t.Errorf("live heap held after 1 GiB: got %d bytes at 64 KiB chunks and %d at 4 MiB, differing by %d, want within %d", small, large, d, tolerance)
	}
}
