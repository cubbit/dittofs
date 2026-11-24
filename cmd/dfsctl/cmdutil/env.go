package cmdutil

import "io"

// Env is everything one invocation of dfsctl reads from and writes to outside
// its arguments. main fills it from the process; a test fills it with buffers,
// so two roots built in one process share nothing.
type Env struct {
	In       io.Reader
	Out, Err io.Writer
	// ConfigPath is the credentials file's path.
	ConfigPath string
	// Interactive says a person is at In. Without one, a command that would
	// prompt refuses instead, naming the flag that answers the prompt.
	Interactive bool

	// afterRead, when set, runs in Update between its read and its write.
	afterRead func()
}
