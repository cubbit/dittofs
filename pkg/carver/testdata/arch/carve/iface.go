package carve

import "github.com/cubbit/dittofs/pkg/carver"

// Cutter is an interface, which the carver does not declare.
type Cutter interface{ Cut(s carver.Settings) error }
