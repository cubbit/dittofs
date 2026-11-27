package fuzz

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// String is the step as a call, the form a seed is written in.
func (s Step) String() string {
	args := make([]string, len(s.Args.params))
	for i, p := range s.Args.params {
		args[i] = p.name + "=" + p.format(s.Args.values[i])
	}

	return s.Name + "(" + strings.Join(args, ", ") + ")"
}

// String is the program as a trace, one numbered step per line.
func (p Program) String() string {
	var trace strings.Builder
	for i, step := range p {
		fmt.Fprintf(&trace, "#%02d %s\n", i, step)
	}

	return trace.String()
}

// Parse reads a program back from a trace or a seed text: one call per line,
// with or without its step number and its result. Unlike Decode it refuses what
// the table does not declare, since such a line is a mistake in the test.
func Parse[R, M any](t Target[R, M], text string) (Program, error) {
	maxSteps := cmp.Or(t.MaxSteps, defaultMaxSteps)

	var program Program
	number := 0
	for line := range strings.Lines(text) {
		number++

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		step, err := parseStep(t.Ops, line)
		if err != nil {
			return nil, fmt.Errorf("fuzz: line %d %q: %w", number, line, err)
		}

		if len(program) == maxSteps {
			return nil, fmt.Errorf("fuzz: line %d: more than %d steps", number, maxSteps)
		}

		program = append(program, step)
	}

	return program, nil
}

func parseStep[R, M any](ops []Op[R, M], line string) (Step, error) {
	if strings.HasPrefix(line, "#") {
		_, line, _ = strings.Cut(line, " ")
	}

	name, rest, opened := strings.Cut(line, "(")
	list, result, closed := strings.Cut(rest, ")")
	result = strings.TrimSpace(result)

	if !opened || !closed || (result != "" && !strings.HasPrefix(result, "->")) {
		return Step{}, fmt.Errorf("not a call: want name(param=value, ...)")
	}

	name = strings.TrimSpace(name)
	op := slices.IndexFunc(ops, func(o Op[R, M]) bool {
		return o.Name == name
	})

	if op < 0 {
		return Step{}, fmt.Errorf("no operation %q", name)
	}

	args, err := parseArgs(ops[op].Params, list)
	if err != nil {
		return Step{}, err
	}

	return Step{
		Op:   op,
		Name: name,
		Args: args,
	}, nil
}

// parseArgs reads every parameter, in declaration order.
func parseArgs(params []Param, list string) (Args, error) {
	var fields []string
	if strings.TrimSpace(list) != "" {
		fields = strings.Split(list, ",")
	}

	if len(fields) != len(params) {
		return Args{}, fmt.Errorf("%d parameters, want %d", len(fields), len(params))
	}

	args := Args{
		params: params,
		values: make([]int, len(params)),
	}
	for i, p := range params {
		name, text, _ := strings.Cut(fields[i], "=")

		if strings.TrimSpace(name) != p.name {
			return Args{}, fmt.Errorf("%q: want parameter %q", fields[i], p.name)
		}

		value, err := p.parse(strings.TrimSpace(text))
		if err != nil {
			return Args{}, err
		}

		args.values[i] = value
	}

	return args, nil
}

// Seed encodes a seed text into the bytes that decode to it, taking the
// smallest byte values that do. It panics on a text Parse refuses, so a wrong
// seed stops the test that declares it.
func Seed[R, M any](t Target[R, M], text string) []byte {
	program, err := Parse(t, text)
	if err != nil {
		panic(err)
	}

	table := opcodes(t.Ops)
	stride := stride(t.Ops)

	data := make([]byte, len(program)*stride)
	for i, step := range program {
		buf := data[i*stride:]
		buf[0] = byte(slices.Index(table[:], step.Op))

		buf = buf[1:]
		for j, p := range step.Args.params {
			p.encode(buf, step.Args.values[j])
			buf = buf[p.width():]
		}
	}

	return data
}
