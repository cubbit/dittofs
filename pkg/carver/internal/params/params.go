// Package params holds the carver's settings value. Its fields are unexported
// and the package is internal, so only the carver's own packages can build a
// value or read beyond its methods. It holds data only.
package params

// Settings is one namespace's chunking settings: the configured Target and
// the Min and Max derived from it.
type Settings struct {
	target, min, max int
}

// Target is the configured chunk size; 0 on the zero value.
func (s Settings) Target() int { return s.target }

// Min is the smallest chunk a content-chosen boundary ends; 0 on the zero value.
func (s Settings) Min() int { return s.min }

// Max is the largest chunk, and the least buffer Cut takes; 0 on the zero value.
func (s Settings) Max() int { return s.max }
