// Package carve is the carver (RFC 2 §2, §4): the settings constructor, Cut,
// the chunk ID and the block name. It keeps nothing between calls and
// declares no interface. Only the node's content wiring imports it.
//
// Cut is a stub behind its final signature: it returns errors.ErrUnsupported.
package carve

import (
	"errors"
	"io"

	"github.com/cubbit/dittofs/pkg/carver"
)

// Cut cuts one unbroken stretch read from r, whose first byte sits at file
// offset base, into chunks, calling emit once per chunk in order. The bytes
// handed to emit are borrowed: they are good only until emit returns, and a
// caller that keeps them copies them. buf is the caller's, at least s.Max()
// bytes, or Cut fails with io.ErrShortBuffer; a zero s fails it with
// carver.ErrInvalidSettings. realEnd says whether the stretch ends for good;
// on an artificial end the tail after the last content-chosen boundary is not
// emitted, and consumed says how far Cut cut. A reader that keeps returning
// no bytes and no error fails it with io.ErrNoProgress. An error from r or
// emit stops Cut and is returned as it came.
func Cut(r io.Reader, base int64, s carver.Settings, buf []byte, realEnd bool,
	emit func(carver.Chunk, []byte) error,
) (consumed int64, err error) {
	return 0, errors.ErrUnsupported
}
