package codec

// An error type of the codec's own, and a sentinel constant of it, neither a
// variable nor exported.
type codecError string

func (e codecError) Error() string { return string(e) }

const errShort codecError = "codec: short"
