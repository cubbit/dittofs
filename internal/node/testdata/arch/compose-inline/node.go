// The node building the engine's Deps itself instead of through Compose.
package node

import (
	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginewire"
)

var _ enginewire.Params

func build() (*engine.Engine, error) { return engine.New(engine.Deps{}) }
