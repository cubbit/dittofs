// Package carvetest holds the carver's test components, which only tests
// import: the golden vectors computed outside the code under test
// (LoadVectors), read by the chunker's checks for the gear tables and by the
// carver's for the chunk ID and the block name. It hands out bytes, words and
// hashes, never the carver's own types, and imports the standard library
// alone, so a check written with it needs no store, network, clock or
// goroutine (RFC 2 §1.3).
//
// gen_vectors.py writes testdata/vectors.json with the BLAKE3 team's Python
// binding; vectors_test.go recomputes every value in Go and holds the BLAKE3
// library the carver uses to BLAKE3's published vectors.
package carvetest
