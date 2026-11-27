//go:build !linux && !darwin

package benchdir

import "runtime"

// platformBox reads nothing the platform gives cheaply but its name.
func platformBox(string) Box { return Box{Kernel: runtime.GOOS} }
