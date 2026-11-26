// Package carver is the carver's contract (RFC 2 §1.1): the value types and
// the error other components name. It declares no function, no interface and
// no state, and imports the standard library, the metadata store's entity
// package and the carver's own settings value only.
//
// The engine imports this package alone. Cut, the chunk ID and the block name
// live in carver/carve, the block assembler in carver/assembler; only the
// node's content wiring imports those, and hands their functions to the engine
// and the codec.
package carver
