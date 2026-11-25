// The node composing twice, the table it keeps not the one the engine's
// holds act on.
package node

import (
	wire "github.com/cubbit/dittofs/internal/node/content"
	"github.com/cubbit/dittofs/pkg/engine"
)

func build(p wire.Params) error {
	table, _, err := wire.Compose(p)
	if err != nil {
		return err
	}
	_, deps, err := wire.Compose(p)
	if err != nil {
		return err
	}
	_, _ = table, deps
	_, err = engine.New(deps)
	return err
}
