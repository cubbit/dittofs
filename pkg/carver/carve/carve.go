// Package carve is the carver (RFC 2 §2, §4): the settings constructor, Cut,
// the chunk ID and the block name. It keeps nothing between calls and
// declares no interface. Only the node's content wiring imports it.
//
// Every body is a stub behind its final signature: a function with an error
// returns errors.ErrUnsupported, one without returns its zero value.
package carve

import (
	"errors"
	"io"

	"github.com/cubbit/dittofs/pkg/carver"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// NewSettings builds a namespace's settings from its Target, whether it
// encrypts and its two keys. A namespace that does not encrypt passes a zero
// chunking key. It refuses, with carver.ErrInvalidSettings, a Target
// CheckTarget refuses, a zero chunk-ID key, a chunking key without encryption,
// and encryption with a zero chunking key (RFC 2 §3.7).
func NewSettings(target int, encrypting bool, chunkingKey, chunkIDKey [32]byte) (carver.Settings, error) {
	return carver.Settings{}, errors.ErrUnsupported
}

// CheckTarget refuses, with carver.ErrInvalidSettings, a Target that is not a
// power of two, below 64 KiB or above 4 MiB. It needs no key, so a settings
// validator can call it.
func CheckTarget(target int) error {
	return errors.ErrUnsupported
}

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

// ChunkID is the chunk ID of p: BLAKE3's keyed hash under s's chunk-ID key,
// over p and nothing else. It panics on a zero Settings, which only a caller
// that skipped NewSettings holds.
func ChunkID(s carver.Settings, p []byte) entity.ChunkHash {
	return entity.ChunkHash{}
}

// BlockName is the name of a block holding ids, in block order, under scope,
// for the put attempt that drew nonce, with the chain ID chain (RFC 2 §4.2).
// It draws no nonce itself.
func BlockName(scope entity.NamespaceID, nonce [16]byte, chain [32]byte, ids []entity.ChunkHash) entity.BlockName {
	return entity.BlockName{}
}
