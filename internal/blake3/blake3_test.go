package blake3

import (
	"math/rand/v2"
	"runtime"
	"runtime/metrics"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	lib "lukechampine.com/blake3"
)

// nameContext is the block name's context, 37 bytes: above Go's 32-byte
// buffer for a string converted on the stack, so a conversion that escapes
// would allocate.
const nameContext = "dittofs content-defined block name v2"

// lengths are the measured input lengths: either side of one BLAKE3 chunk,
// one group, whose sides the splits reach, and the carver's chunk sizes up to
// the largest Max.
var lengths = []int{0, 1, 1023, 1024, 1025, 16384, 64 << 10, 256 << 10, 1 << 20, 4 << 20, 16 << 20}

// splits are where an input is cut into two writes: either side of one BLAKE3
// chunk and of one group.
var splits = []int{1, 1023, 1024, 1025, 16383, 16384, 16385}

// input returns n seeded random bytes.
func input(n int) []byte {
	p := make([]byte, n)
	r := rand.NewChaCha8([32]byte{'b', 'l', 'a', 'k', 'e', '3'})
	_, _ = r.Read(p)

	return p
}

func testKey() [32]byte {
	var k [32]byte
	for i := range k {
		k[i] = byte(i * 7)
	}

	return k
}

// keyed hashes p written as the pieces split names, under key.
func keyed(key *[32]byte, p []byte, split int) [32]byte {
	var h Hasher
	h.InitKeyed(key)
	writeSplit(&h, p, split)

	return h.Sum256()
}

// derived derives a key from p, written as the pieces split names.
func derived(context string, p []byte, split int) [32]byte {
	var h Hasher
	h.InitDeriveKey(context)
	writeSplit(&h, p, split)

	return h.Sum256()
}

func writeSplit(h *Hasher, p []byte, split int) {
	if split > 0 && split < len(p) {
		h.Write(p[:split])
		p = p[split:]
	}
	h.Write(p)
}

// At every measured length, in both modes, the hasher gives what the
// library's own API gives, written whole and split into two writes, and
// reused after a first hash: the answer depends on the bytes alone, never on
// how they were written or on what the hasher held.
func TestHasherMatchesLibrary(t *testing.T) {
	key := testKey()
	data := input(lengths[len(lengths)-1])

	for _, n := range lengths {
		p := data[:n]

		var wantKeyed [32]byte
		lh := lib.New(32, key[:])
		_, _ = lh.Write(p) // a Hasher's Write never fails
		lh.Sum(wantKeyed[:0])

		var wantDerived [32]byte
		lib.DeriveKey(wantDerived[:], nameContext, p)

		for _, split := range append([]int{0}, splits...) {
			if split >= n && split != 0 {
				continue
			}
			if got := keyed(&key, p, split); got != wantKeyed {
				t.Errorf("keyed hash of %d bytes split at %d: got %x, want %x", n, split, got, wantKeyed)
			}
			if got := derived(nameContext, p, split); got != wantDerived {
				t.Errorf("derived key of %d bytes split at %d: got %x, want %x", n, split, got, wantDerived)
			}
		}

		var h Hasher
		h.InitKeyed(&key)
		h.Write(data[:2*group+1])
		_ = h.Sum256()
		h.InitKeyed(&key)
		h.Write(p)
		if got := h.Sum256(); got != wantKeyed {
			t.Errorf("keyed hash of %d bytes on a reused hasher: got %x, want %x", n, got, wantKeyed)
		}
		h.InitDeriveKey(nameContext)
		h.Write(p)
		if got := h.Sum256(); got != wantDerived {
			t.Errorf("derived key of %d bytes on a reused hasher: got %x, want %x", n, got, wantDerived)
		}
	}
}

// Written one byte at a time across two groups, and in pieces that straddle
// chunk and group edges, the input hashes as one write does.
func TestHasherMatchesLibraryOverSmallWrites(t *testing.T) {
	key := testKey()
	p := input(2*group + 3*1024 + 17)

	var want [32]byte
	lh := lib.New(32, key[:])
	_, _ = lh.Write(p) // a Hasher's Write never fails
	lh.Sum(want[:0])

	for _, piece := range []int{1, 63, 64, 1000, 1024, 5000} {
		var h Hasher
		h.InitKeyed(&key)
		for q := p; len(q) > 0; {
			n := min(piece, len(q))
			h.Write(q[:n])
			q = q[n:]
		}
		if got := h.Sum256(); got != want {
			t.Errorf("keyed hash of %d bytes in writes of %d: got %x, want %x", len(p), piece, got, want)
		}
	}
}

// A context longer than one group is hashed a group at a time, as the
// library hashes it.
func TestHasherDerivesUnderALongContext(t *testing.T) {
	ctx := string(input(group + 100))
	material := input(3000)

	var want [32]byte
	lib.DeriveKey(want[:], ctx, material)

	if got := derived(ctx, material, 0); got != want {
		t.Errorf("derived key under a %d-byte context: got %x, want %x", len(ctx), got, want)
	}
}

// Hashing makes no allocation at any measured length, in either mode, the
// hasher on the caller's stack and the context included.
func TestAllocsHasher(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the race detector makes allocation counts meaningless")
	}
	key := testKey()
	data := input(lengths[len(lengths)-1])

	for _, n := range lengths {
		p := data[:n]
		var sum [32]byte
		if a := testing.AllocsPerRun(3, func() {
			var h Hasher
			h.InitKeyed(&key)
			h.Write(p)
			sum = h.Sum256()
		}); a != 0 {
			t.Errorf("keyed hash of %d bytes: %v allocations, want 0", n, a)
		}
		if a := testing.AllocsPerRun(3, func() {
			var h Hasher
			h.InitDeriveKey(nameContext)
			h.Write(p)
			sum = h.Sum256()
		}); a != 0 {
			t.Errorf("derived key of %d bytes: %v allocations, want 0", n, a)
		}
		_ = sum
	}
}

// Hashing starts no goroutine at any measured length, in either mode. The
// count is the process's, so this test never runs in parallel with another.
func TestHasherStartsNoGoroutine(t *testing.T) {
	key := testKey()
	data := input(lengths[len(lengths)-1])

	runtime.GC()
	before := goroutinesCreated(t)
	for _, n := range lengths {
		_ = keyed(&key, data[:n], 0)
		_ = derived(nameContext, data[:n], 0)
	}
	if d := goroutinesCreated(t) - before; d != 0 {
		t.Errorf("hashing every measured length in both modes started %d goroutines, want 0", d)
	}
}

func goroutinesCreated(t *testing.T) uint64 {
	t.Helper()
	s := []metrics.Sample{{Name: "/sched/goroutines-created:goroutines"}}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindUint64 {
		t.Fatalf("the runtime has no %s metric", s[0].Name)
	}

	return s[0].Value.Uint64()
}

// The goroutine count sees a goroutine the library's Write starts, so a
// count of zero above is evidence.
func TestGoroutineCountSeesTheLibrarysWrite(t *testing.T) {
	key := testKey()
	p := input(1 << 20)

	runtime.GC()
	before := goroutinesCreated(t)
	h := lib.New(32, key[:])
	_, _ = h.Write(p) // a Hasher's Write never fails
	if d := goroutinesCreated(t) - before; d == 0 {
		t.Errorf("the library's Write over %d bytes started no goroutine the count saw", len(p))
	}
}
