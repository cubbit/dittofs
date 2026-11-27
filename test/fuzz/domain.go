package fuzz

import (
	"fmt"
	"slices"
	"strconv"
)

const (
	// maxByteDomain is how many values one input byte can tell apart.
	maxByteDomain = 1 << 8

	// maxDomain is how many values two bytes can: past it a test is not
	// choosing its values.
	maxDomain = 1 << 16
)

// Param is one parameter of an operation: a name and a finite domain.
type Param struct {
	name   string
	values []int
	names  []string // a Named parameter's names, indexed by value
	hex    bool     // its values print in hexadecimal
}

// Enum is a parameter over at most 256 listed values.
func Enum(name string, values ...int) Param {
	return newParam(name, values, maxByteDomain)
}

// Choice is a parameter over listed values, boundaries and their neighbours.
func Choice(name string, values ...int) Param {
	return newParam(name, values, maxDomain)
}

// Pattern is a parameter over the 256 seeds of a payload generator.
func Pattern(name string) Param {
	p := newParam(name, indexes(maxByteDomain), maxByteDomain)
	p.hex = true

	return p
}

// Named is a parameter over listed names, at most 256.
func Named(name string, names ...string) Param {
	p := newParam(name, indexes(len(names)), maxByteDomain)
	p.names = names

	return p
}

// newParam panics on a domain no input could decode: a wrong declaration is a
// bug in the target.
func newParam(name string, values []int, limit int) Param {
	if len(values) == 0 || len(values) > limit {
		panic(fmt.Sprintf("fuzz: parameter %q has %d values, want 1 to %d", name, len(values), limit))
	}

	return Param{
		name:   name,
		values: values,
	}
}

// indexes is 0 to n-1.
func indexes(n int) []int {
	values := make([]int, n)
	for i := range values {
		values[i] = i
	}

	return values
}

// width is how many input bytes the parameter takes.
func (p Param) width() int {
	if len(p.values) > maxByteDomain {
		return 2
	}

	return 1
}

// decode maps the parameter's bytes, little endian, into its domain.
func (p Param) decode(b []byte) int {
	v := 0
	for i := range p.width() {
		v |= int(b[i]) << (8 * i)
	}

	return p.values[v%len(p.values)]
}

// encode writes the smallest bytes that decode to value, which must be in the
// domain.
func (p Param) encode(b []byte, value int) {
	v := slices.Index(p.values, value)
	for i := range p.width() {
		b[i] = byte(v >> (8 * i))
	}
}

// format is the value as a trace prints it.
func (p Param) format(value int) string {
	switch {
	case p.names != nil:
		return p.names[value]
	case p.hex:
		return fmt.Sprintf("0x%02x", value)
	}

	return strconv.Itoa(value)
}

// parse reads a value format printed, and refuses one outside the domain.
func (p Param) parse(text string) (int, error) {
	i := slices.IndexFunc(p.values, func(v int) bool {
		return p.format(v) == text
	})

	if i < 0 {
		return 0, fmt.Errorf("%s=%q is not in the domain", p.name, text)
	}

	return p.values[i], nil
}
