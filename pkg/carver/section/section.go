// Package section is the carver's settings section: a namespace's Target, the
// one size its chunker is configured by (RFC 2 §3.2, RFC 13 Appendix B). It
// states the section as plain values, the prefix, the shape with its tags, the
// default and the validator, and imports no settings package: the settings
// table that registers it gives it its scope, a namespace's.
package section

import (
	"strings"

	"github.com/cubbit/dittofs/pkg/carver"
	"github.com/cubbit/dittofs/pkg/carver/carve"
)

// Prefix is the section's prefix in the settings table: the key carver.target.
//
// decision: the section's key is carver.target, the name RFC 13 Appendix B's
// Target row takes once the section is registered in the node's settings
// table. Until then no namespace can have bound it, so the name is still free
// to change; once one has, it is not.
const Prefix = "carver"

// Settings is a namespace's Target, bound once the namespace holds content,
// since a new Target cuts the same bytes into other chunks (RFC 2 §3.6).
type Settings struct {
	Target int `json:"target" class:"bound"`
}

// Default is the section's value where no row sets the key: RFC 13 Appendix
// B's proposed 256 KiB, until RFC 2 Appendix C's sweep freezes it.
func Default() Settings {
	return Settings{Target: 256 << 10}
}

// Validate checks Target alone, as carve.CheckTarget does; it takes no key.
// The refusal wraps
// carver.ErrInvalidSettings and its text names the field, target, since the
// settings table prints it under the section's key, carver.
func (s Settings) Validate() error {
	if err := carve.CheckTarget(s.Target); err != nil {
		return refusal{err}
	}

	return nil
}

// refusal is CheckTarget's error, its text keyed by the field rather than by
// the carver's sentinel.
type refusal struct{ err error }

func (r refusal) Error() string {
	return "target: " + strings.TrimPrefix(r.err.Error(), carver.ErrInvalidSettings.Error()+": ")
}

func (r refusal) Unwrap() error { return r.err }
