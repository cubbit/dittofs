package fuzz

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// bound is the step bound of the toys that run into it, long enough that a
// healthy step on a loaded machine stays under it.
const bound = time.Second

// withOp is the register target with one more operation, named "extra", whose
// model allows any answer.
func withOp(real func(*Env, *register) Result) Target[*register, belief] {
	target := registerTarget(register{})
	target.StepBound = bound
	target.Ops = append(target.Ops, Op[*register, belief]{
		Name: "extra",
		Real: func(env *Env, r *register, _ Args) Result {
			return real(env, r)
		},
		Model: func(b belief, _ Args, _ Result) ([]belief, error) {
			return []belief{b}, nil
		},
	})

	return target
}

func FuzzRegister(f *testing.F) {
	target := registerTarget(register{keeps: true})
	f.Add(Seed(target, "write(v=2)\ncrash()\nread()"))

	Fuzz(f, target)
}

func TestDeterministicRunsTwice(t *testing.T) {
	t.Run("equal runs pass", func(t *testing.T) {
		target := registerTarget(register{keeps: true})
		if err := deterministic(t, target, Seed(target, "write(v=2)\ncrash()\nread()")); err != nil {
			t.Fatalf("deterministic: %v", err)
		}
	})

	t.Run("runs that differ fail", func(t *testing.T) {
		// What the answer depends on is outside the input: how many runs there
		// were.
		runs := 0
		target := withOp(func(*Env, *register) Result {
			runs++

			return answer(strings.Repeat("x", runs))
		})

		mustHold(t, deterministic(t, target, Seed(target, "extra()")),
			"two runs of one input differ",
			"#00 extra() -> x\n",
			"#00 extra() -> xx\n",
		)
	})

	t.Run("each run is cleaned up before the next", func(t *testing.T) {
		var events []string
		target := withOp(func(env *Env, _ *register) Result {
			events = append(events, "start")
			env.Cleanup(func() {
				events = append(events, "cleanup")
			})

			file := filepath.Join(env.TempDir, "f")
			if err := os.WriteFile(file, nil, 0o600); err != nil {
				return answer(err.Error())
			}

			return answer("ok")
		})

		if err := deterministic(t, target, Seed(target, "extra()")); err != nil {
			t.Fatalf("deterministic: %v", err)
		}

		want := "start cleanup start cleanup"
		if got := strings.Join(events, " "); got != want {
			t.Fatalf("events = %q, want %q", got, want)
		}
	})
}

func TestAPanicFailsWithItsTrace(t *testing.T) {
	target := withOp(func(*Env, *register) Result {
		panic("boom")
	})
	program := Decode(target, Seed(target, "write(v=1)\nextra()"))

	_, err := execute(t, target, program)
	mustHold(t, err,
		"#00 write(v=1) -> ok\n#01 extra()",
		"step #01 extra: panic: boom",
		"runner_test.go",
		"seed: fuzz.Seed(target, `write(v=1)\nextra()`)",
	)
}

func TestAGoroutineLeftBehindFails(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() {
		close(block)
	})
	wait := func() {
		<-block
	}

	t.Run("left running", func(t *testing.T) {
		target := withOp(func(*Env, *register) Result {
			go wait()

			return answer("ok")
		})

		_, err := execute(t, target, Decode(target, Seed(target, "extra()")))
		mustHold(t, err, "1 goroutine(s) left running", "#00 extra() -> ok")
	})

	t.Run("ended by a cleanup", func(t *testing.T) {
		target := withOp(func(env *Env, _ *register) Result {
			stop, done := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(done)

				<-stop
			}()

			// What a collaborator that takes a test registers on it counts too.
			env.T.Cleanup(func() {
				close(stop)
				<-done
			})

			return answer("ok")
		})

		if _, err := execute(t, target, Decode(target, Seed(target, "extra()"))); err != nil {
			t.Fatalf("execute: %v", err)
		}
	})
}

func TestAStepPastItsBoundEndsTheProcess(t *testing.T) {
	const child = "FUZZ_TEST_HANGING_STEP"
	if os.Getenv(child) != "" {
		target := withOp(func(*Env, *register) Result {
			select {}
		})

		_, _ = execute(t, target, Decode(target, Seed(target, "write(v=1)\nextra()")))

		return
	}

	// A hung step cannot be interrupted, so it is watched from outside its
	// process.
	cmd := exec.Command(os.Args[0], "-test.run=^TestAStepPastItsBoundEndsTheProcess$", "-test.timeout=30s")
	cmd.Env = append(os.Environ(), child+"=1")

	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the process with a hung step passed:\n%s", out)
	}

	for _, part := range []string{
		"#00 write(v=1) -> ok\n#01 extra()",
		"step #01 extra: still running after 1s",
		"goroutine ",
	} {
		if !strings.Contains(string(out), part) {
			t.Errorf("output does not hold %q:\n%s", part, out)
		}
	}
}

// logRecorder keeps what a test logs.
type logRecorder struct {
	testing.TB
	logged []string
}

func (r *logRecorder) Log(args ...any) {
	for _, arg := range args {
		r.logged = append(r.logged, arg.(string))
	}
}

func TestTheTraceIsLoggedOnlyWhenAsked(t *testing.T) {
	target := registerTarget(register{})
	program := Decode(target, Seed(target, "write(v=1)"))

	t.Setenv("DITTOFS_FUZZ_TRACE", "")

	quiet := &logRecorder{TB: t}
	logTrace(quiet, program)

	if len(quiet.logged) != 0 {
		t.Fatalf("logged %q with DITTOFS_FUZZ_TRACE unset, want nothing", quiet.logged)
	}

	t.Setenv("DITTOFS_FUZZ_TRACE", "1")

	traced := &logRecorder{TB: t}
	logTrace(traced, program)

	if len(traced.logged) != 1 || !strings.Contains(traced.logged[0], "#00 write(v=1)") {
		t.Fatalf("logged %q with DITTOFS_FUZZ_TRACE=1, want the trace of write(v=1)", traced.logged)
	}
}
