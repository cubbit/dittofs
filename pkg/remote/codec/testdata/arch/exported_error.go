package codec

import "errors"

// An exported error of the codec's own.
var ErrHeader = errors.New("codec: bad header")
