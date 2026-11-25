package fuzz

import (
	"fmt"
	"strings"
	"testing"
)

// overOne is the register target with an invariant a write of 2 or 3 breaks.
func overOne() Target[*register, belief] {
	target := registerTarget(register{keeps: true})
	target.Invariants = []Invariant[*register]{
		func(_ *Env, r *register) error {
			if r.value > 1 {
				return fmt.Errorf("value %d is over 1", r.value)
			}

			return nil
		},
	}

	return target
}

func parsed(t *testing.T, target Target[*register, belief], calls ...string) Program {
	t.Helper()

	program, err := Parse(target, strings.Join(calls, "\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	return program
}

func TestShrink(t *testing.T) {
	target := overOne()

	var calls []string
	for range defaultMaxSteps / 2 {
		calls = append(calls, "write(v=1)", "crash()")
	}
	calls[40] = "write(v=3)"

	err := executeShrinking(t, target, parsed(t, target, calls...))

	// The step that matters is left alone, its value moved toward the first of
	// the domain for as long as the failure stays: 2 still breaks the
	// invariant, 1 does not.
	mustHold(t, err,
		"#40 write(v=3) -> ok\n",
		"shrunk from 64 steps to 1:\n#00 write(v=2) -> ok\n",
		"invariant: value 2 is over 1",
		"seed: fuzz.Seed(target, `write(v=2)`)",
	)

	_, again := execute(t, target, Decode(target, Seed(target, "write(v=2)")))
	mustHold(t, again, "step #00 write: invariant: value 2 is over 1")
}

func TestShrinkIsStableForTheSameFailure(t *testing.T) {
	// extra breaks the invariant after a write, and panics without one.
	target := withOp(func(_ *Env, r *register) Result {
		if r.value == 0 {
			panic("nothing written")
		}
		r.value = 9

		return answer("ok")
	})
	target.Invariants = []Invariant[*register]{
		func(_ *Env, r *register) error {
			if r.value == 9 {
				return fmt.Errorf("value is 9")
			}

			return nil
		},
	}
	program := parsed(t, target, "crash()", "write(v=3)", "read()", "extra()", "write(v=2)")

	var messages []string
	for range 2 {
		err := executeShrinking(t, target, program)

		// Dropping the write would leave a shorter program that fails, by a
		// panic: another failure, so the write stays.
		mustHold(t, err,
			"shrunk from 5 steps to 2:\n#00 write(v=1) -> ok\n#01 extra() -> ok\n",
			"seed: fuzz.Seed(target, `write(v=1)\nextra()`)",
		)
		messages = append(messages, fmt.Sprint(err))
	}

	if messages[0] != messages[1] {
		t.Fatalf("two shrinks of one failure differ:\n%s\n%s", messages[0], messages[1])
	}
}

func TestShrinkStartsAgainAfterADrop(t *testing.T) {
	target := overOne()
	program := parsed(t, target, "write(v=1)", "write(v=3)", "crash()")

	// The first step can go only once the last one has: a single pass reaches
	// it too early.
	got := shrink(program, shrinkAttempts, func(candidate Program) bool {
		calls := candidate.String()
		needsFirst := strings.Contains(calls, "crash()") && !strings.Contains(calls, "write(v=1)")

		return strings.Contains(calls, "write(v=3)") && !needsFirst
	})

	if want := "#00 write(v=3)\n"; got.String() != want {
		t.Errorf("program = %q, want %q", got, want)
	}
}

func TestShrinkStopsAtItsAttempts(t *testing.T) {
	target := overOne()
	program := parsed(t, target, "write(v=3)", "write(v=3)", "write(v=3)", "write(v=3)")

	tried := 0
	got := shrink(program, 3, func(Program) bool {
		tried++

		return false
	})

	if tried != 3 {
		t.Errorf("candidates tried = %d, want 3", tried)
	}
	if got.String() != program.String() {
		t.Errorf("program = %q, want it unchanged", got)
	}
}
