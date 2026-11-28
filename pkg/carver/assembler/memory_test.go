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

// heldAfterGiB folds 1 GiB of distinct carried chunks of size bytes, 16 to a
// block, dropping every plan Add returns, and returns the live heap the fold
// holds after its last Add, signed because the heap can end lower than it
// began.
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
	// Chunks are made one at a time, not by stream, so no input slice that
	// grows with the chunk count sits in the measured heap.
	for i := range int64(1<<30) / int64(size) {
		var h entity.ChunkHash
		binary.BigEndian.PutUint64(h[:], uint64(i+1))
		f.Add(carver.ChunkInfo{File: file, Chunk: carver.Chunk{Offset: i * int64(size), Length: size, Hash: h}, Keys: 1, Oldest: t0})
	}
	after := prof.LiveHeap()
	runtime.KeepAlive(f)
	return int64(after) - int64(before)
}

// TestAllocsFoldMemory checks that the fold's memory grows neither with the
// bytes or chunks it folds nor with its chunks' sizes (RFC 2 §5.5): 1 GiB
// folded as 64 KiB chunks into 1 MiB blocks, and as 4 MiB chunks into 64 MiB
// blocks, each leaves at most 256 KiB live, and the two differ by at most
// 1 MiB. A constant fold holds a few KiB; one that kept a 48-byte record per
// chunk holds about 840 KiB at 64 KiB chunks, one that kept every closed plan
// about 3.5 MiB, and one that kept a buffer of each chunk's length about 4 MiB
// at 4 MiB chunks.
func TestAllocsFoldMemory(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the race detector changes how much heap the runtime keeps")
	}
	const held, apart = 256 << 10, 1 << 20

	small := heldAfterGiB(t, 64<<10)
	large := heldAfterGiB(t, 4<<20)
	t.Logf("live heap held after 1 GiB: %d bytes at 64 KiB chunks, %d bytes at 4 MiB chunks", small, large)
	for _, r := range []struct {
		name string
		got  int64
	}{{"64 KiB", small}, {"4 MiB", large}} {
		if r.got > held {
			t.Errorf("live heap held after 1 GiB at %s chunks: got %d bytes, want at most %d", r.name, r.got, held)
		}
	}
	if d := small - large; d > apart || d < -apart {
		t.Errorf("live heap held after 1 GiB: 64 KiB and 4 MiB chunks differ by %d bytes, want within %d", d, apart)
	}
}
