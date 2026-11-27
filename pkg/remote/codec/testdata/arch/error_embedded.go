package codec

// An error type of the codec's own whose Error is promoted from an embedded error.
type wrapErr struct{ error }
