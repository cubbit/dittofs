package fuzz

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// maxStates bounds how many model states a step may leave possible. A model
// driven by what the component answered stays far below it, so passing it is a
// bug in the target: it needs an earlier read back or smaller domains.
const maxStates = 64

// Invariant is a property of the real component that holds after every step.
type Invariant[R any] func(env *Env, r R) error

var errNoState = errors.New("the model allows no state after this answer")

// run runs a program against a new real component and a new model, and returns
// the model states still possible after the last step. It stops at the first
// step no state allows, and the error carries the trace up to it.
func run[R, M any](t Target[R, M], env *Env, program Program) ([]M, error) {
	component, err := t.NewReal(env)
	if err != nil {
		return nil, fmt.Errorf("fuzz: %s: new real component: %w", t.Name, err)
	}

	states := []M{t.NewModel()}
	var trace strings.Builder
	for i, step := range program {
		env.Step = i
		got := t.Ops[step.Op].Real(env, component, step.Args)
		fmt.Fprintf(&trace, "#%02d %s -> %s\n", i, step, got)

		// fail reports what went wrong at this step.
		fail := func(format string, args ...any) error {
			return fmt.Errorf("%sfuzz: %s: step #%02d %s: %s\n%s", trace.String(), t.Name, i, step.Name,
				fmt.Sprintf(format, args...), seedOf(program[:i+1]))
		}

		next, refusal := advance(t, step, got, component, states)

		switch {
		case len(next) == 0:
			return nil, fail("no allowed state matches\n  got:  %s\n  closest candidate: %v\n"+
				"  candidate states before the step: %d", got, refusal, len(states))
		case len(next) > maxStates:
			return nil, fail("%d candidate states, more than %d", len(next), maxStates)
		}

		states = next

		for _, invariant := range t.Invariants {
			if err := invariant(env, component); err != nil {
				return nil, fail("invariant: %v", err)
			}
		}
	}

	return states, nil
}

// advance returns the states allowed after got: what each state's model allows
// and the real component agrees with, one per key, in key order, so a run never
// depends on map order. When none is left it says why: a refusal of Compare,
// which a state the model allowed reached, before one of Model.
func advance[R, M any](t Target[R, M], step Step, got Result, component R, states []M) ([]M, error) {
	allowed := map[string]M{}
	var refused, mismatched error
	for _, state := range states {
		candidates, err := t.Ops[step.Op].Model(state, step.Args, got)
		if err != nil {
			refused = cmp.Or(refused, err)
			continue
		}

		for _, candidate := range candidates {
			if err := t.Compare(step, got, component, candidate); err != nil {
				mismatched = cmp.Or(mismatched, err)
				continue
			}

			allowed[t.Key(candidate)] = candidate
		}
	}

	if len(allowed) == 0 {
		return nil, cmp.Or(mismatched, refused, errNoState)
	}

	next := make([]M, 0, len(allowed))
	for _, key := range slices.Sorted(maps.Keys(allowed)) {
		next = append(next, allowed[key])
	}

	return next, nil
}

// seedOf is the program as the seed that replays it.
func seedOf(program Program) string {
	calls := make([]string, len(program))
	for i, step := range program {
		calls[i] = step.String()
	}

	return "  seed: fuzz.Seed(target, `" + strings.Join(calls, "\n") + "`)"
}
