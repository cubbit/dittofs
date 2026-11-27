package cmdutil

import (
	"io"
	"time"
)

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
	// Now is the clock a command reads the time from, nil for the
	// machine's; a test sets the server's, so a time the command sends,
	// such as an API token's expiry, is in the server's terms.
	Now func() time.Time

	// afterRead, when set, runs in Update between its read and its write.
	afterRead func()
}
