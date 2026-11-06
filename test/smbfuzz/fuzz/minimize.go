package fuzz

import (
	"fmt"
	"io"
	"strings"
)

// Executor runs a plan once.
type Executor interface {
	Run(p *Plan) *Result
}

// Replays is how many times a failing round runs in all before it is called
// stable (RFC 31 §11.1).
const Replays = 3

// Stability replays a failing plan and returns the signature of every run,
// the first one included.
func Stability(x Executor, first *Result) (bool, []string) {
	sigs := []string{first.Signature()}
	for i := 1; i < Replays; i++ {
		res := x.Run(first.Plan)
		if res.Err != nil {
			sigs = append(sigs, "error: "+res.Err.Error())
			continue
		}
		sigs = append(sigs, res.Signature())
	}
	for _, s := range sigs {
		if s != sigs[0] {
			return false, sigs
		}
	}
	return true, sigs
}

// Minimized is the result of shrinking a plan.
type Minimized struct {
	Plan   *Plan
	Result *Result  // the last run of the minimised plan
	Leads  []string // other signatures met on the way
	Runs   int
}

// Minimize shrinks a stable failing plan (RFC 31 §11.2, §11.3): it removes
// one step at a time, from the last, keeping a removal when the replay still
// diverges with the same signature, until a pass removes nothing; then it
// tries each parameter at a simpler value under the same rule.
func Minimize(x Executor, failing *Result, log io.Writer) *Minimized {
	sig := failing.Signature()
	m := &Minimized{Plan: failing.Plan.Clone(), Result: failing}
	leads := map[string]bool{}
	try := func(cand *Plan) bool {
		m.Runs++
		res := x.Run(cand)
		got := res.Signature()
		if res.Err == nil && got == sig {
			m.Plan, m.Result = cand, res
			return true
		}
		if res.Err == nil && got != "" && !leads[got] {
			leads[got] = true
			m.Leads = append(m.Leads, got)
		}
		return false
	}
	for changed := true; changed; {
		changed = false
		for i := len(m.Plan.Steps) - 1; i >= 0; i-- {
			if len(m.Plan.Steps) == 1 {
				break
			}
			cand := m.Plan.Clone()
			cand.Steps = append(cand.Steps[:i], cand.Steps[i+1:]...)
			if try(cand) {
				fmt.Fprintf(log, "  minimise: removed step %d, %d left\n", i+1, len(m.Plan.Steps))
				changed = true
			}
		}
	}
	for i := range m.Plan.Steps {
		for _, k := range sortedKeys(m.Plan.Steps[i].Args) {
			for _, simpler := range simplerValues(m.Plan.Steps[i].Op, k, m.Plan.Steps[i].Args[k]) {
				cand := m.Plan.Clone()
				cand.Steps[i].Args[k] = simpler
				if try(cand) {
					fmt.Fprintf(log, "  minimise: step %d %s=%s\n", i+1, k, simpler)
					break
				}
			}
		}
	}
	if m.Plan.Root != "open" {
		cand := m.Plan.Clone()
		cand.Root = "open"
		try(cand)
	}
	return m
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// simplerValues returns candidates simpler than v, most simple first.
func simplerValues(op, key, v string) []string {
	var out []string
	add := func(c string) {
		if c != v {
			out = append(out, c)
		}
	}
	switch key {
	case "off", "off2", "len":
		// Drop one term of a sum, then try the smallest points.
		terms := strings.Split(v, "+")
		if len(terms) > 1 {
			for i := range terms {
				rest := append(append([]string(nil), terms[:i]...), terms[i+1:]...)
				add(strings.Join(rest, "+"))
			}
		}
		for _, p := range []string{"0", "1", "4096"} {
			add(p)
		}
	case "flags", "attrs", "how", "mode":
		if op == "open" || key == "attrs" || op == "fallocate" {
			parts := strings.Split(v, "|")
			for i := range parts {
				if op == "open" && i == 0 {
					continue // the access mode stays
				}
				rest := append(append([]string(nil), parts[:i]...), parts[i+1:]...)
				if len(rest) == 0 {
					add("0")
				} else {
					add(strings.Join(rest, "|"))
				}
			}
		}
	case "path", "path2":
		add("f0")
		add("d0")
	case "sd":
		out = append(out, simplerSDs(v)...)
	case "data":
		add("zero")
	}
	return out
}

// simplerSDs removes one ACE at a time, then the owner and group.
func simplerSDs(v string) []string {
	var out []string
	var aces [][2]int
	depth := 0
	start := 0
	for i, ch := range v {
		switch ch {
		case '(':
			if depth == 0 {
				start = i
			}
			depth++
		case ')':
			depth--
			if depth == 0 {
				aces = append(aces, [2]int{start, i + 1})
			}
		}
	}
	for i := len(aces) - 1; i >= 0; i-- {
		out = append(out, v[:aces[i][0]]+v[aces[i][1]:])
	}
	if strings.HasPrefix(v, "O:") {
		if i := strings.Index(v, "D:"); i > 0 {
			out = append(out, v[i:])
		}
	}
	return out
}
