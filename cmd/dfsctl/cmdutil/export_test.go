package cmdutil

// PauseAfterRead returns env with Update running pause between its read and
// its write, so a test can change the resource in between.
func PauseAfterRead(env Env, pause func()) Env {
	env.afterRead = pause
	return env
}
