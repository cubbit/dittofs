package install

import "errors"

// ErrCrash is what Run returns when the crash point CrashAfter set stops it.
var ErrCrash = errors.New("install test: stopped at the crash point")

// CrashAfter returns d with a crash point: Run appends each write it commits
// to seen, if seen is not nil, and stops with ErrCrash once the n-th has
// committed; n zero never stops it.
func CrashAfter(d Deps, n int, seen *[]string) Deps {
	count := 0
	d.after = func(write string) error {
		count++
		if seen != nil {
			*seen = append(*seen, write)
		}
		if count == n {
			return ErrCrash
		}
		return nil
	}
	return d
}
