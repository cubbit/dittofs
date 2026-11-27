// The node composing once, then handing the engine Deps it built itself.
package node

import (
	"github.com/cubbit/dittofs/internal/node/content"
	eng "github.com/cubbit/dittofs/pkg/engine"
)

func build(p content.Params) error {
	if _, _, err := content.Compose(p); err != nil {
		return err
	}
	_, err := eng.New(eng.Deps{})
	return err
}
