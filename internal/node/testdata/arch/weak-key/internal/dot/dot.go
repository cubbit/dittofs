package dot

import . "github.com/cubbit/dittofs/pkg/keyprovider"

// weak calls GenerateWeakKeyFile by its bare name, through the dot-import.
func weak() ([]byte, error) { return GenerateWeakKeyFile("p") }
