package api

import "github.com/cubbit/dittofs/pkg/engine"

// Deps reaches the engine's implementation instead of an interface.
type Deps struct{ Engine *engine.Engine }
