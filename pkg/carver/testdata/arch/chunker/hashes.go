package chunker

import "lukechampine.com/blake3"

// sum hashes p, which the chunker never does.
func sum(p []byte) [32]byte { return blake3.Sum256(p) }
