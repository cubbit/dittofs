package alias

import kp "github.com/cubbit/dittofs/pkg/keyprovider"

// weak calls GenerateWeakKeyFile through keyprovider imported under another
// name: the rule names it.
func weak() ([]byte, error) { return kp.GenerateWeakKeyFile("p") }
