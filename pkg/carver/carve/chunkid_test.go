package carve_test

import (
	"fmt"
	"strings"
	"testing"

	"lukechampine.com/blake3"

	"github.com/cubbit/dittofs/pkg/carver"
	"github.com/cubbit/dittofs/pkg/carver/carve"
	"github.com/cubbit/dittofs/pkg/carver/internal/carvetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// settingsUnder is settings NewSettings builds for a namespace that does not
// encrypt, with idKey as its chunk-ID key.
func settingsUnder(t *testing.T, idKey [32]byte) carver.Settings {
	t.Helper()
	s, err := carve.NewSettings(1<<20, false, [32]byte{}, idKey)
	if err != nil {
		t.Fatalf("NewSettings with chunk-ID key %x: got %v, want nil", idKey, err)
	}

	return s
}

// The committed vector, computed by BLAKE3's reference binding, is the chunk
// ID of its input under its key, through settings NewSettings built: a wrong
// mode, a key read from the wrong place or a length fed into the hash fails.
func TestChunkIDGivesTheVector(t *testing.T) {
	v := carvetest.LoadVectors(t)
	if got := carve.ChunkID(settingsUnder(t, v.ChunkIDKey), v.ChunkInput); got != entity.ChunkHash(v.ChunkID) {
		t.Errorf("ChunkID of the vector's %d-byte input: got %s, want %x", len(v.ChunkInput), got, v.ChunkID)
	}
}

// One input under two keys gives two IDs, and neither is the unkeyed hash of
// the bytes, so a namespace's IDs say nothing to one that holds the same file.
func TestChunkIDIsKeyed(t *testing.T) {
	v := carvetest.LoadVectors(t)
	other := v.ChunkIDKey
	other[0] ^= 1

	a := carve.ChunkID(settingsUnder(t, v.ChunkIDKey), v.ChunkInput)
	b := carve.ChunkID(settingsUnder(t, other), v.ChunkInput)
	if a == b {
		t.Errorf("ChunkID under keys %x and %x: both got %s, want two IDs", v.ChunkIDKey, other, a)
	}
	plain := blake3.Sum256(v.ChunkInput)
	for _, id := range []entity.ChunkHash{a, b} {
		if id == plain {
			t.Errorf("ChunkID: got %s, the unkeyed BLAKE3 hash of the input, want a keyed one", id)
		}
	}
}

// A zero Settings holds no key, and ChunkID refuses to hash under one; any
// other panic, such as a nil dereference, is not that refusal.
func TestChunkIDPanicsOnZeroSettings(t *testing.T) {
	defer func() {
		if r := recover(); !strings.Contains(fmt.Sprint(r), "no chunk-ID key") {
			t.Errorf("ChunkID over a zero Settings: recovered %v, want the no-key panic", r)
		}
	}()
	carve.ChunkID(carver.Settings{}, []byte("x"))
}
