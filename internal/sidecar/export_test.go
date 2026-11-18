package sidecar

// What the package's external tests reach: the clock the retries' waits run
// on, which a test replaces to move its own time.

type (
	Clock = clock
	Timer = timer
)

// SetClock arms r's retries' waits on c instead of the runtime's clock.
func SetClock(r *Runner, c Clock) { r.clock = c }
