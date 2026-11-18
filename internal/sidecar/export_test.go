package sidecar

// What the package's external tests reach: the clock the retries' waits run
// on, which a test replaces to move its own time, the point a retry reads at,
// and a sidecar's live enabled setting and mutex.

type (
	Clock = clock
	Timer = timer
)

// SetClock arms r's retries' waits on c instead of the runtime's clock.
func SetClock(r *Runner, c Clock) { r.clock = c }

// SetRetryRead runs read once a retry has read the failed answer and the set
// it carries, before it syncs them.
func SetRetryRead(r *Runner, read func()) { r.retryRead = read }

// Enabled reports the sidecar named name's live enabled setting, under the
// runner's mutex: a test waits a flip it interleaves against.
func Enabled(r *Runner, name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.named(name).enabled
}

// Held reports whether the sidecar named name's mutex is held. A sync that
// waits on it reports no event, so a test asks the mutex itself whether a
// read or a build sits behind it.
func Held(r *Runner, name string) bool {
	sc := r.named(name)
	if sc.mu.TryLock() {
		sc.mu.Unlock()
		return false
	}
	return true
}

// named is the sidecar named name.
func (r *Runner) named(name string) *sidecar {
	for _, sc := range r.sidecars {
		if sc.Name() == name {
			return sc
		}
	}
	panic("sidecar: no sidecar named " + name)
}
