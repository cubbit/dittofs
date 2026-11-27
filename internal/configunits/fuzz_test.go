package configunits

import (
	"errors"
	"math"
	"math/big"
	"strconv"
	"testing"
)

// sizeFactors splits a size into its digits and its unit factor, as big
// integers, recomputing it independently of the package's own arithmetic. A
// value with no digits has neither; digits whose tail is a listed unit have
// both; digits with any other tail name no product.
func sizeFactors(v string) (digits, factor *big.Int, ok bool) {
	i := 0

	for i < len(v) && v[i] >= '0' && v[i] <= '9' {
		i++
	}

	if i == 0 {
		return nil, nil, false
	}

	digits, ok = new(big.Int).SetString(v[:i], 10)
	if !ok {
		return nil, nil, false
	}

	unit, listed := fileSizeUnits[v[i:]]
	if !listed {
		return digits, nil, false
	}

	return digits, new(big.Int).SetUint64(unit), true
}

// FuzzParse reads strings no one at DittoFS wrote, as hostile configuration
// would: no parse panics, every refusal names one of the five causes, a
// record's number of bytes reads back exactly, an accepted size is its
// digits times its unit against the largest uint64, and no accepted
// duration lands outside its range.
func FuzzParse(f *testing.F) {
	for _, v := range []string{
		"1B", "2KB", "3MB", "4GB", "5TB", "1KiB", "1MiB", "1GiB", "1TiB",
		"100MiB", "100", "512", "1048576", "1.5", "1.0", "1.5MiB", "4.1GB",
		"100mib", "1kib", "1Gi", "10m", "1e3", "1.5e3", "1 MiB", "", "abc",
		"MiB", "-", "-1MiB", "1e5", "9223372036854775807",
		"9223372036854775808", "18446744073709551615", "18446744073709551616",
		"9007199254740993", "9223372036854775807B", "18446744073709551615B",
		"9223372036854775807MiB", "18446744073709551615TiB", "17179869184GiB",
		"18446744073709551616KB", "18446744073709551616kb", "1KB", "1Kib",
		"0s", "250ms", "30s", "5m", "1h", "1h30m", "23h60m", "86400000ms",
		"5124096h", "2562047h2562047h", "99999999999999999999s", "7",
		"30", "30s30", "1.5h", "1.0s", "36H", "5µs", "-24h", "1h30", "h",
	} {
		f.Add(v)
	}

	f.Fuzz(func(t *testing.T, v string) {
		n, errFile := ParseFileSize(v)
		rb, errBytes := ParseRecordSizeBytes(v)
		d, errDuration := ParseDuration(v)

		for _, err := range []error{errFile, errBytes, errDuration} {
			if err == nil {
				continue
			}

			var causes int

			for _, cause := range []error{
				ErrNotANumber, ErrNoUnit, ErrUnknownUnit, ErrFraction, ErrOverflow,
			} {
				if errors.Is(err, cause) {
					causes++
				}
			}

			if causes != 1 {
				t.Errorf("parse of %q: got %v, want exactly one named cause", v, err)
			}
		}

		// A size is its digits times its unit, held to the largest uint64:
		// an accepted parse equals that product and fits it, and an
		// overflow names digits, or a product, that pass that largest.
		if digits, factor, ok := sizeFactors(v); ok {
			largest := new(big.Int).SetUint64(math.MaxUint64)

			product := new(big.Int).Mul(digits, factor)

			if errFile == nil && product.Cmp(largest) > 0 {
				t.Errorf("ParseFileSize(%q): accepted %d past the largest uint64", v, n)
			}

			if errFile == nil && product.Uint64() != n {
				t.Errorf(
					"ParseFileSize(%q): got %d, want %s, the digits times the unit",
					v,
					n,
					product,
				)
			}

			if errors.Is(errFile, ErrOverflow) && product.Cmp(largest) <= 0 &&
				digits.Cmp(largest) <= 0 {
				t.Errorf("ParseFileSize(%q): overflow, but digits %s and product %s fit",
					v, digits, product)
			}
		}

		// A record's number of bytes reads back exactly, whatever it is.
		if errBytes == nil {
			want, err := strconv.ParseUint(v, 10, 64)
			if err != nil || want != rb {
				t.Errorf("ParseRecordSizeBytes(%q): got %d, want %d", v, rb, want)
			}
		}

		if errDuration == nil && d < 0 {
			t.Errorf("ParseDuration(%q): got %s, never negative", v, d)
		}
	})
}
