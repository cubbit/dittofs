// A seed of the adapters' import check, read and never built: a command file
// that decodes by hand with the codec's runtime.
package ops

import "github.com/cubbit/dittofs/internal/adapter/smb/wire"

func decode(b []byte) error {
	d := wire.Decoder{B: b}
	return d.Err
}
