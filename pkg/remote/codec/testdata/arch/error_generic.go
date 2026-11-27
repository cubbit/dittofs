package codec

// An error type of the codec's own declared on a generic type.
type kindErr[K any] struct{ kind K }

func (e *kindErr[K]) Error() string { return "codec: kind" }
