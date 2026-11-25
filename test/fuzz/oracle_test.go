package fuzz

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// register is a toy component: one value, durable only once synced. A crash
// keeps an unsynced value or drops it, as the test sets it up.
type register struct {
	value, synced int

	keeps    bool // a crash keeps the unsynced value
	corrupts bool // a bug: a crash leaves a value nobody wrote
}

// belief is the toy's model state.
type belief struct {
	value, synced int
}

type answer string

func (a answer) String() string {
	return string(a)
}

func registerTarget(setup register) Target[*register, belief] {
	return Target[*register, belief]{
		Name: "register",
		NewReal: func(*Env) (*register, error) {
			r := setup

			return &r, nil
		},
		NewModel: func() belief {
			return belief{}
		},
		Compare: func(Step, Result, *register, belief) error {
			return nil
		},
		Key: func(b belief) string {
			return fmt.Sprintf("%d/%d", b.value, b.synced)
		},
		Ops: []Op[*register, belief]{
			{
				Name:   "write",
				Params: []Param{Enum("v", 1, 2, 3)},
				Real: func(_ *Env, r *register, a Args) Result {
					r.value = a.Int("v")

					return answer("ok")
				},
				Model: func(b belief, a Args, _ Result) ([]belief, error) {
					b.value = a.Int("v")

					return []belief{b}, nil
				},
			},
			{
				Name: "crash",
				Real: func(_ *Env, r *register, _ Args) Result {
					switch {
					case r.corrupts:
						r.value, r.synced = 9, 9
					case r.keeps:
						r.synced = r.value
					default:
						r.value = r.synced
					}

					return answer("restarted")
				},
				// The answer says nothing of what survived, so both stay
				// possible.
				Model: func(b belief, _ Args, _ Result) ([]belief, error) {
					kept := belief{
						value:  b.value,
						synced: b.value,
					}
					dropped := belief{
						value:  b.synced,
						synced: b.synced,
					}

					return []belief{kept, dropped}, nil
				},
			},
			{
				Name: "read",
				Real: func(_ *Env, r *register, _ Args) Result {
					return answer(fmt.Sprint(r.value))
				},
				Model: func(b belief, _ Args, got Result) ([]belief, error) {
					if want := fmt.Sprint(b.value); got.String() != want {
						return nil, fmt.Errorf("read %s, the model holds %s", got, want)
					}

					return []belief{b}, nil
				},
			},
		},
	}
}

func keys(target Target[*register, belief], states []belief) []string {
	var out []string
	for _, s := range states {
		out = append(out, target.Key(s))
	}

	return out
}

// runText runs a seed text and returns the keys of the states left.
func runText(target Target[*register, belief], text string) ([]string, error) {
	program, err := Parse(target, text)
	if err != nil {
		return nil, err
	}

	states, err := run(target, &Env{}, program)

	return keys(target, states), err
}

// mustHold fails unless err holds every part.
func mustHold(t *testing.T, err error, parts ...string) {
	t.Helper()

	if err == nil {
		t.Fatalf("the run passed, want a failure holding %q", parts)
	}

	for _, part := range parts {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("failure:\n%v\ndoes not hold %q", err, part)
		}
	}
}

func TestStateSetNarrowsToTheObservation(t *testing.T) {
	for _, keeps := range []bool{true, false} {
		t.Run(fmt.Sprintf("a crash that keeps=%t", keeps), func(t *testing.T) {
			target := registerTarget(register{keeps: keeps})

			got, err := runText(target, "write(v=1)\ncrash()")
			if err != nil {
				t.Fatalf("run: %v", err)
			}

			// Nothing has read the value yet: both outcomes stay, in key
			// order.
			if want := []string{"0/0", "1/1"}; !slices.Equal(got, want) {
				t.Fatalf("states after the crash = %v, want %v", got, want)
			}

			got, err = runText(target, "write(v=1)\ncrash()\nread()")
			if err != nil {
				t.Fatalf("run: %v", err)
			}

			want := []string{"0/0"}
			if keeps {
				want = []string{"1/1"}
			}

			if !slices.Equal(got, want) {
				t.Fatalf("states after the read = %v, want %v", got, want)
			}
		})
	}

	t.Run("equal states collapse", func(t *testing.T) {
		got, err := runText(registerTarget(register{}), "crash()")
		if err != nil {
			t.Fatalf("run: %v", err)
		}

		if want := []string{"0/0"}; !slices.Equal(got, want) {
			t.Fatalf("states = %v, want %v", got, want)
		}
	})

	t.Run("compare narrows at the step", func(t *testing.T) {
		target := registerTarget(register{keeps: true})
		target.Compare = func(_ Step, _ Result, r *register, b belief) error {
			if r.value != b.value {
				return fmt.Errorf("holds %d, the model %d", r.value, b.value)
			}

			return nil
		}

		got, err := runText(target, "write(v=1)\ncrash()")
		if err != nil {
			t.Fatalf("run: %v", err)
		}

		if want := []string{"1/1"}; !slices.Equal(got, want) {
			t.Fatalf("states = %v, want %v", got, want)
		}
	})

	t.Run("an answer no state allows fails", func(t *testing.T) {
		target := registerTarget(register{corrupts: true})

		_, err := runText(target, "write(v=1)\ncrash()\nread()\nwrite(v=2)")
		mustHold(t, err,
			"#00 write(v=1) -> ok\n#01 crash() -> restarted\n#02 read() -> 9\n",
			"fuzz: register: step #02 read: no allowed state matches",
			"got:  9",
			"closest candidate: read 9, the model holds 0",
			"candidate states before the step: 2",
			"seed: fuzz.Seed(target, `write(v=1)\ncrash()\nread()`)",
		)

		if strings.Contains(err.Error(), "#03") {
			t.Errorf("failure:\n%v\nshows a step after the failing one", err)
		}
	})

	t.Run("a refusal of compare is the closest", func(t *testing.T) {
		target := registerTarget(register{})
		target.Compare = func(Step, Result, *register, belief) error {
			return errors.New("the sizes differ")
		}

		_, err := runText(target, "write(v=1)")
		mustHold(t, err, "closest candidate: the sizes differ")
	})

	t.Run("a broken invariant fails", func(t *testing.T) {
		target := registerTarget(register{})
		target.Invariants = []Invariant[*register]{
			func(_ *Env, r *register) error {
				if r.value > 1 {
					return fmt.Errorf("value %d is over 1", r.value)
				}

				return nil
			},
		}

		_, err := runText(target, "write(v=1)\nwrite(v=2)")
		mustHold(t, err, "step #01 write: invariant: value 2 is over 1", "#01 write(v=2) -> ok")
	})

	t.Run("too many states fail", func(t *testing.T) {
		target := registerTarget(register{})
		target.Ops[1].Model = func(belief, Args, Result) ([]belief, error) {
			var many []belief
			for i := range maxStates + 1 {
				many = append(many, belief{value: i})
			}

			return many, nil
		}

		_, err := runText(target, "crash()")
		mustHold(t, err, "step #00 crash: 65 candidate states, more than 64")
	})

	t.Run("a real component that cannot be built fails", func(t *testing.T) {
		target := registerTarget(register{})
		target.NewReal = func(*Env) (*register, error) {
			return nil, errors.New("no disk")
		}

		_, err := runText(target, "crash()")
		mustHold(t, err, "no disk")
	})
}
