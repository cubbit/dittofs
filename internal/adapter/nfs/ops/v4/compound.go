package v4

import (
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/resources"
)

// compoundArgs is COMPOUND4args up to its operations: the tag, the minor
// version, which the header already read as the request's Version, and the
// count of operations (RFC 8881 section 16.2). The tag has no cap of its own:
// its maximum is RFC 21 section 2.3's record of 1 MiB and 4 KiB, which bounds
// it first.
type compoundArgs struct {
	Tag   []byte `xdr:"max=1052672"`
	Minor uint32
	Count uint32
}

// compoundHead is COMPOUND4res up to its results: the status, the tag echoed
// whole and the count of results, its size the same whatever the status and
// the count.
type compoundHead struct {
	Status uint32
	Tag    []byte `xdr:"max=1052672"`
	Count  uint32
}

// decodeCompound is the COMPOUND row's Decode: its arguments' tag, minor
// version and count, the members left to the runner, which decodes them after
// the bytes this took; its head echoes the tag (RFC 21 sections 11 and 11.5,
// RFC 20 section 4.7).
func decodeCompound(b []byte, v core.Version) (core.Invocation, int, error) {
	var a compoundArgs
	n, err := a.DecodeWire(b, v)
	if err != nil {
		return core.Invocation{}, 0, err
	}
	return core.Invocation{
		Count: int(a.Count),
		EncodeHead: func(out *resources.Buf, s core.Status, n int, v core.Version) error {
			return (&compoundHead{Status: uint32(s), Tag: a.Tag, Count: uint32(n)}).EncodeWire(v, out)
		},
	}, n, nil
}
