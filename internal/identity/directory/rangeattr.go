package directory

import (
	"strconv"
	"strings"
)

// Range is the range option of an attribute description, as Active Directory
// hands back a multi-valued attribute too large for one answer:
// member;range=<Lo>-<Hi>, or member;range=<Lo>-* for the last part.
type Range struct {
	Attr string // the description before ";range=", such as "member"
	Lo   int
	Hi   int // the last value's index; -1 for *, the last part
}

// rangeOpt is the option that names a range, matched case-insensitively.
const rangeOpt = ";range="

// ParseRange reads the attribute description desc as Attr;range=Lo-Hi. It
// refuses a description with no range option, a bound that is not a decimal
// number below 2^31, and Lo > Hi.
func ParseRange(desc string) (Range, bool) {
	// Found byte for byte, ASCII letters folded: lower-casing desc first
	// would move the index wherever desc holds a byte that is not UTF-8.
	i := -1
	for j := 1; j+len(rangeOpt) <= len(desc); j++ {
		if strings.EqualFold(desc[j:j+len(rangeOpt)], rangeOpt) {
			i = j
			break
		}
	}
	if i < 0 {
		return Range{}, false
	}
	lo, hi, ok := strings.Cut(desc[i+len(rangeOpt):], "-")
	if !ok {
		return Range{}, false
	}
	r := Range{Attr: desc[:i], Hi: -1}
	var err error
	if r.Lo, err = bound(lo); err != nil {
		return Range{}, false
	}
	if hi != "*" {
		if r.Hi, err = bound(hi); err != nil || r.Hi < r.Lo {
			return Range{}, false
		}
	}
	return r, true
}

// bound is s as a range bound: decimal digits only, below 2^31.
func bound(s string) (int, error) {
	n, err := strconv.ParseUint(s, 10, 31)
	return int(n), err
}

// String is the description r was parsed from, its bounds in their shortest
// decimal form.
func (r Range) String() string {
	hi := "*"
	if r.Hi >= 0 {
		hi = strconv.Itoa(r.Hi)
	}
	return r.Attr + rangeOpt + strconv.Itoa(r.Lo) + "-" + hi
}
