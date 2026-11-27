//go:build ignore

// An error of the codec's own behind a build tag.

package codec

type taggedError struct{}

func (*taggedError) Error() string { return "codec: tagged" }
