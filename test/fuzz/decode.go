package fuzz

import (
	"cmp"
	"fmt"
	"slices"
)

const (
	// opcodeCount is the number of values an opcode byte has, and so the most
	// operations a target can declare.
	opcodeCount = 1 << 8

	defaultMaxSteps = 64
)

// Program is a decoded input: the steps to run, in order.
type Program []Step

// Step is one decoded operation: its index in the target's table, its name and
// its arguments.
type Step struct {
	Op   int
	Name string
	Args Args
}

// Args are the values one step's parameters decoded to.
type Args struct {
	params []Param
	values []int // one per parameter, in declaration order
}

// Int is the value of the named parameter.
func (a Args) Int(name string) int {
	return a.values[a.index(name)]
}

// Named is the name a Named parameter decoded to.
func (a Args) Named(name string) string {
	i := a.index(name)

	return a.params[i].names[a.values[i]]
}

// index panics on a parameter the operation does not declare.
func (a Args) index(name string) int {
	i := slices.IndexFunc(a.params, func(p Param) bool {
		return p.name == name
	})

	if i < 0 {
		panic(fmt.Sprintf("fuzz: the operation has no parameter %q", name))
	}

	return i
}

// Decode turns any bytes into a program of the target's operations. Every step
// takes the same number of bytes, so a changed byte changes one step only. A
// short last step reads as padded with zeros, and bytes past MaxSteps are
// ignored.
func Decode[R, M any](t Target[R, M], data []byte) Program {
	table := opcodes(t.Ops)
	stride := stride(t.Ops)
	maxSteps := cmp.Or(t.MaxSteps, defaultMaxSteps)

	var program Program
	buf := make([]byte, stride)
	for start := 0; start < len(data) && len(program) < maxSteps; start += stride {
		clear(buf)
		copy(buf, data[start:])

		op := table[buf[0]]
		program = append(program, Step{
			Op:   op,
			Name: t.Ops[op].Name,
			Args: decodeArgs(t.Ops[op].Params, buf[1:]),
		})
	}

	return program
}

func decodeArgs(params []Param, b []byte) Args {
	args := Args{
		params: params,
		values: make([]int, len(params)),
	}
	for i, p := range params {
		args.values[i] = p.decode(b)
		b = b[p.width():]
	}

	return args
}

// stride is the opcode byte and the widest operation's parameters.
func stride[R, M any](ops []Op[R, M]) int {
	widest := 0
	for _, op := range ops {
		width := 0
		for _, p := range op.Params {
			width += p.width()
		}

		widest = max(widest, width)
	}

	return 1 + widest
}

// opcodes maps each opcode byte to an operation. Operations share the bytes in
// proportion to their weights, by largest remainder, ties going to the earlier
// one, and each gets at least one.
func opcodes[R, M any](ops []Op[R, M]) [opcodeCount]int {
	if len(ops) == 0 || len(ops) > opcodeCount {
		panic(fmt.Sprintf("fuzz: target has %d operations, want 1 to %d", len(ops), opcodeCount))
	}

	weights := make([]int, len(ops))
	total := 0
	for i, op := range ops {
		weights[i] = max(op.Weight, 1)
		total += weights[i]
	}

	shares := make([]int, len(ops))
	remainders := make([]int, len(ops))
	left := opcodeCount
	for i, w := range weights {
		shares[i] = w * opcodeCount / total
		remainders[i] = w * opcodeCount % total
		left -= shares[i]
	}

	order := indexes(len(ops))
	slices.SortStableFunc(order, func(a, b int) int {
		return cmp.Compare(remainders[b], remainders[a])
	})
	for _, i := range order[:left] {
		shares[i]++
	}

	for i := range shares {
		if shares[i] == 0 {
			shares[slices.Index(shares, slices.Max(shares))]--
			shares[i] = 1
		}
	}

	var table [opcodeCount]int
	next := 0
	for i, n := range shares {
		for range n {
			table[next] = i
			next++
		}
	}

	return table
}
