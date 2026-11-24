// Package clisuite imports the harness, which the binary never links, since
// only tests import this package.
package clisuite

import _ "github.com/cubbit/dittofs/internal/node/nodetest"
