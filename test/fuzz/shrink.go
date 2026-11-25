package fuzz

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

// shrinkAttempts bounds the programs one shrink runs. It is a count and never
// a deadline, so one failure shrinks to the same program on every machine.
const shrinkAttempts = 2000

// stepFailure is what a step failed by. Two failures are the same one when the
// same check fails at a step of the same operation, so shrinking never trades
// the failure it started from for another.
//
// decision: two panics, or two states the model refuses, at one operation
// count as the same failure even when their messages differ: the messages name
// values a shorter program changes. Tell them apart by more if a shrunk program
// is found showing another bug.
type stepFailure struct {
	op    string
	check string
	err   error
}

func (f *stepFailure) Error() string {
	return f.err.Error()
}

func (f *stepFailure) Unwrap() error {
	return f.err
}

// executeShrinking runs a program as execute does, and shrinks one that fails
// at a step. A program that fails otherwise, by a goroutine left running or a
// component that cannot be built, is reported as it is.
func executeShrinking[R, M any](t *testing.T, target Target[R, M], program Program) error {
	_, err := execute(t, target, program)

	var want *stepFailure
	if !errors.As(err, &want) {
		return err
	}

	var smallest error
	shrunk := shrink(program, shrinkAttempts, func(candidate Program) bool {
		_, candidateErr := execute(t, target, candidate)

		var got *stepFailure
		if !errors.As(candidateErr, &got) || got.op != want.op || got.check != want.check {
			return false
		}
		smallest = candidateErr

		return true
	})

	if smallest == nil {
		return err
	}

	return fmt.Errorf("%w\n\nshrunk from %d steps to %d:\n%w", err, len(program), len(shrunk), smallest)
}

// shrink returns a program that still fails, as short as the search finds in
// attempts runs. It drops chunks of steps, halving the chunk down to one and
// starting again while a drop keeps the failure, then halves each kept value's
// place in its domain toward the first. program must fail.
func shrink(program Program, attempts int, fails func(Program) bool) Program {
	try := func(candidate Program) bool {
		if attempts == 0 {
			return false
		}
		attempts--

		return fails(candidate)
	}

	current := slices.Clone(program)
	for dropped := true; dropped; {
		dropped = false
		for chunk := max(len(current)/2, 1); chunk >= 1; chunk /= 2 {
			for start := 0; start < len(current); {
				end := min(start+chunk, len(current))
				candidate := slices.Concat(current[:start], current[end:])

				if try(candidate) {
					current, dropped = candidate, true
					continue
				}
				start += chunk
			}
		}
	}

	for i := range current {
		for p, param := range current[i].Args.params {
			for place := slices.Index(param.values, current[i].Args.values[p]); place > 0; place /= 2 {
				candidate := slices.Clone(current)
				candidate[i].Args.values = slices.Clone(candidate[i].Args.values)
				candidate[i].Args.values[p] = param.values[place/2]

				if !try(candidate) {
					break
				}
				current = candidate
			}
		}
	}

	return current
}
