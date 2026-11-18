// Package settings is how a component declares its settings and how the node
// and the management API read and check them: the binding classes, the scope
// forms of RFC 13 §3, a section declared from the component's typed settings,
// the table of sections, the snapshot a node runs on and the write check.
//
// It reads the store only through what its caller hands it, the settings scan
// for Read and the write's transaction for Check, so it declares no store
// interface.
package settings

// Class is what a change to a setting does, RFC 13 §5's binding class. A
// settings struct declares each field's class in a class struct tag beside its
// json tag; the tag's spellings and the parser that reads them are the
// settings epic's, and the adapters' tags use RFC 13 §5's words, live and
// restart. The zero Class names none.
type Class uint8

const (
	// Live takes effect on the next operation that reads it, on every node.
	Live Class = iota + 1
	// Restart takes effect when the process next starts.
	Restart
	// NextWrite governs content written from then on; stored content keeps
	// what it was written with.
	NextWrite
	// Bound never changes once content exists.
	Bound
)
