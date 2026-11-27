package api

import "github.com/cubbit/dittofs/pkg/metadata/kv"

// The API may name KV's error values, and not its interface or the
// implementation beside them.
var (
	closed  = kv.ErrClosed
	store   kv.KV
	counted = kv.Counting{}
)
