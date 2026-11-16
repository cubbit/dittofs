// The node composing once, then handing the engine Deps it built itself.
package node

import (
	eng "github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginewire"
)

func build(p enginewire.Params) error {
	if _, _, err := enginewire.Compose(p); err != nil {
		return err
	}
	_, err := eng.New(eng.Deps{})
	return err
}
