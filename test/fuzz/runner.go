package fuzz

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"runtime/pprof"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/prof"
)

const defaultStepBound = 10 * time.Second

// Fuzz runs a target under the Go fuzzer: each input is decoded and run
// against a new component and a new model. A program that fails at a step is
// shrunk before it is reported. The target's coverage is checked first, so a
// plain go test runs the check.
func Fuzz[R, M any](f *testing.F, target Target[R, M]) {
	f.Helper()
	CheckCoverage(f, target)

	f.Fuzz(func(t *testing.T, data []byte) {
		if err := executeShrinking(t, target, Decode(target, data)); err != nil {
			t.Fatal(err)
		}
	})
}

// Deterministic fails the test when two runs of an input differ, or when one
// fails: the fuzzer can only replay and learn from an input that always does
// the same thing.
func Deterministic[R, M any](t *testing.T, target Target[R, M], data []byte) {
	t.Helper()

	if err := deterministic(t, target, data); err != nil {
		t.Fatal(err)
	}
}

func deterministic[R, M any](t *testing.T, target Target[R, M], data []byte) error {
	program := Decode(target, data)
	first, firstErr := execute(t, target, program)
	second, secondErr := execute(t, target, program)

	if first != second {
		return fmt.Errorf("fuzz: %s: two runs of one input differ\nfirst:\n%ssecond:\n%s", target.Name, first, second)
	}

	return cmp.Or(firstErr, secondErr)
}

// execute runs a program as one input and returns its trace. The input has a
// test of its own, so everything registered on it is cleaned up before the
// goroutines it left running are counted.
func execute[R, M any](t *testing.T, target Target[R, M], program Program) (trace string, err error) {
	running := prof.Goroutines()

	env := &Env{}
	passed := t.Run("input", func(t *testing.T) {
		env.T = t
		env.TempDir = t.TempDir()
		env.Cleanup = t.Cleanup

		_, trace, err = run(target, env, program)
	})

	if !passed && err == nil {
		err = fmt.Errorf("%sfuzz: %s: the input's test failed", trace, target.Name)
	}

	if left := running(cmp.Or(target.StepBound, defaultStepBound)); len(left) > 0 {
		err = errors.Join(err, fmt.Errorf("%sfuzz: %s: %d goroutine(s) left running after the input:\n\n%s",
			trace, target.Name, len(left), strings.Join(left, "\n\n")))
	}

	// Read after the wait for the goroutines, so a call a cleanup released has
	// had its time to return.
	for _, untaken := range env.unfinished {
		if panicked := untaken(); panicked != "" {
			err = errors.Join(err, fmt.Errorf("%sfuzz: %s: panic: %s", trace, target.Name, panicked))
		}
	}

	return trace, err
}

// guard runs one step under the harness's own checks: a panic becomes an error
// with its stack, and a step still running after bound ends the process. where
// is what the process prints then.
//
// The bound is the one wall clock wait of the harness. It times nothing of the
// component, whose clock is injected: it only turns a hang into a failure. A
// hung step cannot be interrupted and only the test's own goroutine can fail
// the test, so the watchdog panics, and the fuzzer records the input.
func guard(bound time.Duration, where string, step func() error) (err error) {
	watchdog := time.AfterFunc(bound, func() {
		// Printed here, since a panic indents its message and puts it last.
		fmt.Fprintf(os.Stderr, "%s: still running after %s\n\n", where, bound)
		_ = pprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
		panic("fuzz: a step ran past its bound")
	})

	defer watchdog.Stop()

	defer func() {
		if p := recover(); p != nil {
			err = &stepFailure{
				check: "panic",
				err:   fmt.Errorf("panic: %v\n%s", p, debug.Stack()),
			}
		}
	}()

	return step()
}
