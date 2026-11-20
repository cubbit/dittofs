package install

import "errors"

// ErrCrash is what Run returns when the crash point CrashAfter set stops it.
var ErrCrash = errors.New("install test: stopped at the crash point")

// After returns d with hook run once each write commits, naming it; an error
// it returns stops Run with that error.
func After(d Deps, hook func(write string) error) Deps {
	d.after = hook
	return d
}

// CrashAfter returns d with a crash point: Run appends each write it commits
// to seen, if seen is not nil, and stops with ErrCrash once the n-th has
// committed; n zero never stops it.
func CrashAfter(d Deps, n int, seen *[]string) Deps {
	count := 0
	return After(d, func(write string) error {
		count++
		if seen != nil {
			*seen = append(*seen, write)
		}
		if count == n {
			return ErrCrash
		}
		return nil
	})
}

// ResetAfter returns d with hook run once each write of the reset commits,
// naming it; an error it returns stops ResetAdmin with that error.
func ResetAfter(d ResetDeps, hook func(write string) error) ResetDeps {
	d.after = hook
	return d
}
