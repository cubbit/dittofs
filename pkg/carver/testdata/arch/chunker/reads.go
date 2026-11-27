package chunker

import "io"

// fill reads from r, which the chunker never does.
func fill(r io.Reader, p []byte) (int, error) { return r.Read(p) }
