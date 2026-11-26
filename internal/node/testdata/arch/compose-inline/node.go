// The node building the engine's Deps itself instead of through Compose.
package node

import (
	"github.com/cubbit/dittofs/internal/node/content"
	"github.com/cubbit/dittofs/pkg/engine"
)

var _ content.Params

func build() (*engine.Engine, error) { return engine.New(engine.Deps{}) }
