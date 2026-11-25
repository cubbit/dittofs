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
// the model states still possible after the last step, with the trace of the
// steps run. It stops at the first step that fails, and the error carries the
// trace up to it.
func run[R, M any](t Target[R, M], env *Env, program Program) ([]M, string, error) {
	component, err := t.NewReal(env)
	if err != nil {
		return nil, "", fmt.Errorf("fuzz: %s: new real component: %w", t.Name, err)
	}

	states := []M{t.NewModel()}
	bound := cmp.Or(t.StepBound, defaultStepBound)

	var trace strings.Builder
	for i, step := range program {
		env.Step = i
		line := fmt.Sprintf("#%02d %s", i, step)
		where := fmt.Sprintf("fuzz: %s: step #%02d %s", t.Name, i, step.Name)

		err := guard(bound, trace.String()+line+"\n"+where, func() error {
			got := t.Ops[step.Op].Real(env, component, step.Args)
			line += fmt.Sprintf(" -> %s", got)

			next, err := check(t, env, step, got, component, states)
			states = next

			return err
		})
		fmt.Fprintln(&trace, line)

		if err != nil {
			failed := &stepFailure{
				op:  step.Name,
				err: fmt.Errorf("%s%s: %w\n%s", trace.String(), where, err, seedOf(program[:i+1])),
			}

			var by *stepFailure
			if errors.As(err, &by) {
				failed.check = by.check
			}

			return nil, trace.String(), failed
		}
	}

	return states, trace.String(), nil
}

// check judges one step's answer: it returns the states still possible, or why
// the step fails.
func check[R, M any](t Target[R, M], env *Env, step Step, got Result, component R, states []M) ([]M, error) {
	next, refusal := advance(t, step, got, component, states)

	switch {
	case len(next) == 0:
		return nil, &stepFailure{
			check: "no allowed state",
			err: fmt.Errorf("no allowed state matches\n  got:  %s\n  closest candidate: %w\n"+
				"  candidate states before the step: %d", got, refusal, len(states)),
		}
	case len(next) > maxStates:
		return nil, &stepFailure{
			check: "too many states",
			err:   fmt.Errorf("%d candidate states, more than %d", len(next), maxStates),
		}
	}

	for i, invariant := range t.Invariants {
		if err := invariant(env, component); err != nil {
			return nil, &stepFailure{
				check: fmt.Sprintf("invariant %d", i),
				err:   fmt.Errorf("invariant: %w", err),
			}
		}
	}

	return next, nil
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
