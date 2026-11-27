package chunker

// Boundary is an interface, which the carver does not declare.
type Boundary interface{ Next() int }
