package assembler

import "github.com/cubbit/dittofs/pkg/carver"

// Assembler is the interface RFC 2 §5.1's indicative block invites.
type Assembler interface {
	Add(c carver.ChunkInfo) ([]carver.BlockPlan, carver.Disposition)
}
