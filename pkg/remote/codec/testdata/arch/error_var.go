package codec

import "errors"

// An error of the codec's own, unexported, so no export pin sees it.
var errShortHeader = errors.New("codec: short header")
