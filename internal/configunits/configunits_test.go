package configunits

import (
	"errors"
	"math"
	"testing"
	"time"
)

// TestParseFileSize covers the unit form: every unit exactly, and every
// cause of a refusal named, so a caller can tell an operator why.
func TestParseFileSize(t *testing.T) {
	t.Parallel()

	rows := []struct {
		size      string
		want      uint64
		wantCause error
	}{
		{"1B", 1, nil},
		{"2KB", 2e3, nil},
		{"3MB", 3e6, nil},
		{"4GB", 4e9, nil},
		{"5TB", 5e12, nil},
		{"1KiB", 1 << 10, nil},
		{"1MiB", 1 << 20, nil},
		{"1GiB", 1 << 30, nil},
		{"1TiB", 1 << 40, nil},
		{"100MiB", 100 << 20, nil},
		{"0B", 0, nil},
		{"9223372036854775807B", math.MaxInt64, nil},
		{"18446744073709551615B", math.MaxUint64, nil},
		{"100", 0, ErrNoUnit},                       // a bare number, an older file's form
		{"512", 0, ErrNoUnit},                       // quoted in a record, the same refusal
		{"1.5", 0, ErrFraction},                     // only a fraction
		{"1.0", 0, ErrFraction},                     // a point with no fraction's digits
		{"1.5MiB", 0, ErrFraction},                  // a fraction before a unit
		{"4.1GB", 0, ErrFraction},                   // a fraction before a unit
		{"100mib", 0, ErrUnknownUnit},               // a unit in another case
		{"1kib", 0, ErrUnknownUnit},                 // a unit in another case
		{"1Gi", 0, ErrUnknownUnit},                  // a binary unit without the B
		{"10m", 0, ErrUnknownUnit},                  // a decimal prefix alone
		{"1e3", 0, ErrUnknownUnit},                  // an exponent, not a listed unit
		{"1 MiB", 0, ErrUnknownUnit},                // whitespace before the unit
		{"MiB", 0, ErrNotANumber},                   // a unit alone
		{"abc", 0, ErrNotANumber},                   // no digit in it
		{"-1MiB", 0, ErrNotANumber},                 // a sign where a digit belongs
		{"", 0, ErrNotANumber},                      // empty
		{"9223372036854775807MiB", 0, ErrOverflow},  // a product above the largest uint64
		{"18446744073709551615TiB", 0, ErrOverflow}, // a product above the largest uint64
		{"17179869184GiB", 0, ErrOverflow},          // a product of exactly 2^64
		{"18446744073709551616KB", 0, ErrOverflow},  // the number read first, past the largest
		{"18446744073709551616kb", 0, ErrOverflow},  // the same number, the same cause
		{"18446744073709551616B", 0, ErrOverflow},   // a number past the largest uint64
	}

	for _, row := range rows {
		t.Run(row.size, func(t *testing.T) {
			t.Parallel()

			n, err := ParseFileSize(row.size)
			if row.wantCause == nil {
				if err != nil || n != row.want {
					t.Errorf("ParseFileSize(%q): got %d, %v, want %d, no cause",
						row.size, n, err, row.want)
				}

				return
			}

			if !errors.Is(err, row.wantCause) {
				t.Errorf("ParseFileSize(%q): got cause %v, want %v", row.size, err, row.wantCause)
			}

			if n != 0 {
				t.Errorf("ParseFileSize(%q): got %d with a refusal, want 0", row.size, n)
			}
		})
	}
}

// TestParseRecordSizeBytes covers a record's JSON number: a count of bytes,
// exact through the largest uint64, and refused in any other form.
func TestParseRecordSizeBytes(t *testing.T) {
	t.Parallel()

	rows := []struct {
		size      string
		want      uint64
		wantCause error
	}{
		{"1", 1, nil},
		{"0", 0, nil},
		{"100", 100, nil}, // a count the file's 100 means as megabytes
		{"1048576", 1 << 20, nil},
		{"9223372036854775807", math.MaxInt64, nil},
		{"9223372036854775808", 1 << 63, nil},         // exact, above the largest int64
		{"9007199254740993", 9007199254740993, nil},   // 2^53 + 1, exact where a float loses it
		{"18446744073709551615", math.MaxUint64, nil}, // exact, the largest uint64
		{"18446744073709551616", 0, ErrOverflow},      // a count past the largest uint64
		{"1.5", 0, ErrFraction},                       // a JSON fraction is always a fraction
		{"1.0", 0, ErrFraction},                       // a point names a fraction's shape
		{"1.5e3", 0, ErrFraction},                     // a point, then an exponent
		{"512B", 0, ErrNotANumber},                    // a unit is the string form's job
		{"1e3", 0, ErrNotANumber},                     // an exponent names no count of bytes
		{"1e5", 0, ErrNotANumber},                     // an exponent names no count of bytes
		{"-5", 0, ErrNotANumber},                      // a sign
		{"", 0, ErrNotANumber},                        // empty
	}

	for _, row := range rows {
		t.Run(row.size, func(t *testing.T) {
			t.Parallel()

			n, err := ParseRecordSizeBytes(row.size)
			if row.wantCause == nil {
				if err != nil || n != row.want {
					t.Errorf("ParseRecordSizeBytes(%q): got %d, %v, want %d, no cause",
						row.size, n, err, row.want)
				}

				return
			}

			if !errors.Is(err, row.wantCause) {
				t.Errorf("ParseRecordSizeBytes(%q): got cause %v, want %v",
					row.size, err, row.wantCause)
			}

			if n != 0 {
				t.Errorf("ParseRecordSizeBytes(%q): got %d with a refusal, want 0", row.size, n)
			}
		})
	}
}

// TestParseDuration covers the duration grammar: every unit exactly, a
// compound of units, and every cause of a refusal named.
func TestParseDuration(t *testing.T) {
	t.Parallel()

	rows := []struct {
		input     string
		want      time.Duration
		wantCause error
	}{
		{"0s", 0, nil},
		{"250ms", 250 * time.Millisecond, nil},
		{"30s", 30 * time.Second, nil},
		{"5m", 5 * time.Minute, nil},
		{"1h", time.Hour, nil},
		{"1h30m", 90 * time.Minute, nil},
		{"23h60m", 24 * time.Hour, nil},
		{"86400000ms", 24 * time.Hour, nil},
		{"5124096h", 0, ErrOverflow},              // one part past the whole range
		{"2562047h2562047h", 0, ErrOverflow},      // two parts that overflow together
		{"99999999999999999999s", 0, ErrOverflow}, // a number past int64
		{"7", 0, ErrNoUnit},                       // a bare number
		{"30", 0, ErrNoUnit},                      // a bare number
		{"1.5h", 0, ErrFraction},                  // a fraction
		{"1.0s", 0, ErrFraction},                  // a point names a fraction's shape
		{"36H", 0, ErrUnknownUnit},                // a unit in another case
		{"5µs", 0, ErrUnknownUnit},                // a unit Appendix B does not list
		{"-24h", 0, ErrNotANumber},                // a sign where a digit belongs
		{"1h30", 0, ErrNoUnit},                    // a trailing bare number
		{"30s30", 0, ErrNoUnit},                   // a trailing bare number
		{"h", 0, ErrNotANumber},                   // a unit alone
		{"", 0, ErrNotANumber},                    // empty
	}

	for _, row := range rows {
		t.Run(row.input, func(t *testing.T) {
			t.Parallel()

			d, err := ParseDuration(row.input)
			if row.wantCause == nil {
				if err != nil || d != row.want {
					t.Errorf("ParseDuration(%q): got %s, %v, want %s, no cause",
						row.input, d, err, row.want)
				}

				if d < 0 {
					t.Errorf("ParseDuration(%q): got %s, never negative", row.input, d)
				}

				return
			}

			if !errors.Is(err, row.wantCause) {
				t.Errorf("ParseDuration(%q): got cause %v, want %v", row.input, err, row.wantCause)
			}

			if d != 0 {
				t.Errorf("ParseDuration(%q): got %s with a refusal, want 0", row.input, d)
			}
		})
	}
}

// TestParseFileSizeKelvinUnit guards the one character that looks like a
// listed unit's letter but is not its byte: the Kelvin sign, which a paste
// can carry in place of K.
func TestParseFileSizeKelvinUnit(t *testing.T) {
	t.Parallel()

	for _, v := range []string{"1KB", "1Kib", "1KiB"} {
		t.Run(v, func(t *testing.T) {
			t.Parallel()

			n, err := ParseFileSize(v)
			if !errors.Is(err, ErrUnknownUnit) {
				t.Errorf("ParseFileSize(%q): got %d, cause %v, want the unknown unit cause",
					v, n, err)
			}
		})
	}
}
