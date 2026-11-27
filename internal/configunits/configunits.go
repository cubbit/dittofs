// Package configunits reads the sizes and durations of RFC 13 Appendix B,
// the one grammar every section and the host's file write them in. It is
// deliberately stricter than time.ParseDuration, which also takes a sign,
// a fraction and µs, and than internal/bytesize, whose grammar other parts
// of the code use: a value valid in the configuration is the same wherever
// it is written, so Appendix B's units are matched exactly as listed.
//
// Every parse names the cause of its refusal: not a number, no unit, an
// unknown unit, a fraction, or an overflow. No cause prints the value: the
// caller knows which key it refused and decides, since a value is printed
// only for a key that holds no secret.
package configunits

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
)

// ErrNotANumber refuses a value with no number in it: empty, or a sign
// where a digit belongs.
var ErrNotANumber = errors.New("names no number")

// ErrNoUnit refuses a value that ends after its number: a bare number in
// the file's form, or a duration part with nothing behind it.
var ErrNoUnit = errors.New("names no unit")

// ErrUnknownUnit refuses a value that names no listed unit: a unit in
// another case, or a unit Appendix B does not list.
var ErrUnknownUnit = errors.New("names no listed unit")

// ErrFraction refuses a value with a fractional part: a whole number is
// required, in a record's JSON number as everywhere else.
var ErrFraction = errors.New("names a fraction, not a whole number")

// ErrOverflow refuses a number, or a number and unit's product, past the
// largest value of its kind.
var ErrOverflow = errors.New("does not fit the largest value")

// fileSizeUnits are RFC 13 Appendix B's size units, written exactly so: the
// decimal ones are powers of 1000, the binary ones powers of 1024.
var fileSizeUnits = map[string]uint64{
	"B":   1,
	"KB":  1e3,
	"MB":  1e6,
	"GB":  1e9,
	"TB":  1e12,
	"KiB": 1 << 10,
	"MiB": 1 << 20,
	"GiB": 1 << 30,
	"TiB": 1 << 40,
}

// durationUnits are RFC 13 Appendix B's duration units, in lower case; "ms"
// is matched before "m".
var durationUnits = []struct {
	name string
	d    time.Duration
}{
	{"ms", time.Millisecond},
	{"s", time.Second},
	{"m", time.Minute},
	{"h", time.Hour},
}

// leadingDigits splits the whole number at the front of v from the rest.
// Digits past the largest uint64 name a number too large, not no number.
func leadingDigits(v string) (uint64, string, error) {
	i := 0
	for i < len(v) && v[i] >= '0' && v[i] <= '9' {
		i++
	}

	if i == 0 {
		return 0, "", ErrNotANumber
	}

	n, err := strconv.ParseUint(v[:i], 10, 64)
	if err != nil {
		return 0, "", ErrOverflow
	}

	return n, v[i:], nil
}

// ParseFileSize reads a size as Appendix B writes it: one whole number and
// one unit, exactly as listed. The whole file form travels as a string, in
// the host's file and in a record alike. A bare number is refused, since
// an older file's max_size: 100 meant megabytes, not bytes, and in a record
// a byte count is valid only as a JSON number, which ParseRecordSizeBytes
// reads; a quoted 512 reaches this function and is refused the same way.
func ParseFileSize(v string) (uint64, error) {
	n, rest, err := leadingDigits(v)
	if err != nil {
		return 0, err
	}

	if rest == "" {
		return 0, ErrNoUnit
	}

	if strings.HasPrefix(rest, ".") {
		return 0, ErrFraction
	}

	unit, ok := fileSizeUnits[rest]
	if !ok {
		return 0, ErrUnknownUnit
	}

	if n > math.MaxUint64/unit {
		return 0, ErrOverflow
	}

	return n * unit, nil
}

// ParseRecordSizeBytes reads a record's JSON number as a count of bytes,
// the one form Appendix B accepts in a record alone. It is exact through
// the largest uint64: a size off a disk is not bounded by the largest
// int64. A quoted 512 never reaches this function; if it did, the record's
// codec would have taken it for a string and handed it to ParseFileSize.
func ParseRecordSizeBytes(v string) (uint64, error) {
	n, rest, err := leadingDigits(v)
	if err != nil {
		return 0, err
	}

	if strings.HasPrefix(rest, ".") {
		return 0, ErrFraction
	}

	if rest != "" {
		return 0, ErrNotANumber
	}

	return n, nil
}

// ParseDuration reads a duration as RFC 13 Appendix B writes it: one or
// more whole numbers, each followed by ms, s, m or h, in lower case, never
// negative, so 0s is a zero duration; a bare number names no unit and is
// refused.
func ParseDuration(v string) (time.Duration, error) {
	if v == "" {
		return 0, ErrNotANumber
	}

	var total time.Duration

	for v != "" {
		n, rest, err := leadingDigits(v)
		if err != nil {
			return 0, err
		}

		if rest == "" {
			return 0, ErrNoUnit
		}

		if strings.HasPrefix(rest, ".") {
			return 0, ErrFraction
		}

		var unit time.Duration

		for _, u := range durationUnits {
			if strings.HasPrefix(rest, u.name) {
				unit, v = u.d, rest[len(u.name):]

				break
			}
		}

		if unit == 0 {
			return 0, ErrUnknownUnit
		}

		// The int64 bound comes first and on its own, so the conversion below
		// is checked against a constant; the bound by the unit is tighter.
		if n > math.MaxInt64 || n > uint64(math.MaxInt64/unit) {
			return 0, ErrOverflow
		}

		step := time.Duration(n) * unit
		if total > math.MaxInt64-step {
			return 0, ErrOverflow
		}

		total += step
	}

	return total, nil
}
