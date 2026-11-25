// Package fuzz turns the bytes the Go fuzzer supplies into a sequence of
// operations a target declares, so the target can run each against a real
// component and a model of it. It is imported only by tests. See DESIGN.md.
package fuzz

import (
	"fmt"
	"testing"
	"time"
)

// Target is what a test declares about one component: R is the real component
// and M its model.
type Target[R, M any] struct {
	Name string
	Ops  []Op[R, M]

	// NewReal builds the component with its real collaborators and registers
	// their cleanup on env.
	NewReal  func(env *Env) (R, error)
	NewModel func() M

	// Compare reports whether what the component shows after a step matches
	// one candidate model state.
	Compare func(step Step, got Result, real R, model M) error

	// Key identifies a model state, so equal candidates collapse.
	Key func(M) string

	Invariants []Invariant[R] // checked after every step

	MaxSteps  int           // default 64
	StepBound time.Duration // default 10s, a deadlock backstop and not a timing
}

// Op is one operation of a target.
type Op[R, M any] struct {
	Name   string
	Weight int // relative frequency, default 1
	Covers []string
	Params []Param

	Real func(env *Env, r R, a Args) Result

	// Model returns the states the contract allows after the real component
	// answered got, or an error when it does not allow that answer.
	Model func(m M, a Args, got Result) ([]M, error)
}

// Env is what the harness hands the real side. It is built for every input.
type Env struct {
	// T is the test of this input alone, for a collaborator that takes one:
	// what it registers on it is cleaned up before the leak check.
	T *testing.T

	TempDir string
	Cleanup func(func())
	Step    int
}

// Result is what a step's real call answered, reduced to what the contract
// defines. Its value must be comparable.
type Result interface {
	fmt.Stringer
}
