package carvetest

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"slices"
	"testing"
)

// Boundaries cuts p, a stretch with a real end, under gear table g and
// target, and returns each chunk's end offset in order, the last len(p), or
// nil for an empty p. It
// follows RFC 2 §3.2 one byte at a time: fp ← (fp << 1) + G[b] mod 2⁶⁴; a
// byte is a boundary when the top m bits of fp are zero, m being t + 1 while
// the chunk's length before the byte is under target (2^t) and t − 3 from
// target on; the first byte tested is the chunk's at index Min − 1; a chunk
// reaching Max ends there. Min is target / 4 and Max 4 × target. It panics on
// a target that is not one of Targets.
//
// decision: the fingerprint starts at zero at each chunk's first byte and is
// built over every byte of the chunk, the literal reading of §3.2 with nothing
// optimised. §3.4's "MUST NOT warm it from the start of the chunk" binds the
// shipped chunker, not this oracle: the gear window is 64 bytes, so a 64-byte
// warm-up gives the same ends, and this is the side the chunker's warm-up is
// compared against. Withdraw the exemption if the reference ever ships.
func Boundaries(g *[256]uint64, target int, p []byte) []int {
	if !slices.Contains(Targets[:], target) {
		panic(fmt.Sprintf("carvetest: target %d is not a supported Target %v", target, Targets))
	}

	t := bits.TrailingZeros(uint(target))
	minLen, maxLen := target/4, 4*target

	var ends []int

	start, fp := 0, uint64(0)
	for i, b := range p {
		fp = fp<<1 + g[b]

		before := i - start // the chunk's bytes before b
		m := t + 1
		if before >= target {
			m = t - 3
		}

		if before+1 == maxLen || (before >= minLen-1 && fp>>(64-m) == 0) {
			ends = append(ends, i+1)
			start, fp = i+1, 0
		}
	}

	if start < len(p) {
		ends = append(ends, len(p))
	}

	return ends
}

// boundariesJSON is the golden boundaries, written by TestGoldenBoundaries
// with -update.
//
//go:embed testdata/boundaries.json
var boundariesJSON []byte

// Golden is one committed profile: its input and the chunks the reference
// cut.
type Golden struct {
	Target int
	Keyed  bool // cut under Vectors' keyed table, else the unkeyed one
	Seed   uint64
	Length int // Random bytes of Input at Seed
	Ends   []int
	Hashes [][32]byte // keyed BLAKE3 under Vectors' chunk-ID key
}

// goldenFile is one profile's layout in the file, the hashes in hex.
type goldenFile struct {
	Target int      `json:"target"`
	Keyed  bool     `json:"keyed"`
	Seed   uint64   `json:"seed"`
	Length int      `json:"length"`
	Ends   []int    `json:"ends"`
	Hashes []string `json:"hashes"`
}

// LoadBoundaries decodes the embedded golden boundaries and fails tb on a
// malformed file.
func LoadBoundaries(tb testing.TB) []Golden {
	tb.Helper()

	gs, err := parseBoundaries(boundariesJSON)
	if err != nil {
		tb.Fatalf("boundaries: %v", err)
	}

	return gs
}

// parseBoundaries decodes raw, refusing a hash that is not 32 bytes of hex, a
// profile with no chunks, and one whose hashes and ends differ in number.
func parseBoundaries(raw []byte) ([]Golden, error) {
	var fs []goldenFile
	if err := json.Unmarshal(raw, &fs); err != nil {
		return nil, fmt.Errorf("decoding testdata/boundaries.json: %w", err)
	}
	if len(fs) == 0 {
		return nil, errors.New("testdata/boundaries.json holds no profile")
	}

	var d decoder

	gs := make([]Golden, len(fs))
	for i, f := range fs {
		if len(f.Ends) == 0 || len(f.Ends) != len(f.Hashes) {
			d.fail(fmt.Errorf("profile %d has %d ends and %d hashes, want the same number, at least one", i, len(f.Ends), len(f.Hashes)))
		}

		gs[i] = Golden{Target: f.Target, Keyed: f.Keyed, Seed: f.Seed, Length: f.Length, Ends: f.Ends, Hashes: make([][32]byte, len(f.Hashes))}
		for j, h := range f.Hashes {
			d.fixed(gs[i].Hashes[j][:], fmt.Sprintf("profile %d hashes[%d]", i, j), h)
		}
	}

	return gs, d.err
}
