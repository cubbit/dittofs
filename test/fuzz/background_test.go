package fuzz

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// parking is the register target with two more operations: "begin" starts a
// call that waits at a point of its own until release is closed, then runs
// body; "end" releases it and answers what it answered.
type parking struct {
	// release and call are of the input running now.
	release chan struct{}
	call    *Call[string]

	body func() string

	// onBegin is what an input does besides starting the call.
	onBegin func(env *Env, release func())
}

func (p *parking) target() Target[*register, belief] {
	target := withOp(func(env *Env, _ *register) Result {
		release := make(chan struct{})
		entered := make(chan struct{})
		p.release = release
		p.call = Begin(env, func() string {
			close(entered)
			<-release

			return p.body()
		})

		// A begin step returns only once its call is where it parks, so the
		// next step never races it.
		p.call.WaitParked(entered)

		if p.onBegin != nil {
			p.onBegin(env, func() {
				close(release)
			})
		}

		return answer("parked")
	})
	target.Ops[len(target.Ops)-1].Name = "begin"

	end := target.Ops[len(target.Ops)-1]
	end.Name = "end"
	end.Real = func(*Env, *register, Args) Result {
		close(p.release)

		return answer(p.call.Finish())
	}
	target.Ops = append(target.Ops, end)

	return target
}

func (p *parking) run(t *testing.T, calls string) (string, error) {
	t.Helper()

	target := p.target()

	return execute(t, target, Decode(target, Seed(target, calls)))
}

func panicsInTheCall() string {
	panic("boom")
}

func TestBeginFinishReturnsTheCallsResult(t *testing.T) {
	t.Run("the result reaches the step that finishes", func(t *testing.T) {
		p := &parking{
			body: func() string {
				return "seven"
			},
		}

		trace, err := p.run(t, "begin()\nwrite(v=2)\nend()")
		if err != nil {
			t.Fatalf("execute: %v", err)
		}

		want := "#00 begin() -> parked\n#01 write(v=2) -> ok\n#02 end() -> seven\n"
		if trace != want {
			t.Fatalf("trace = %q, want %q", trace, want)
		}
	})

	t.Run("done closes when the call returns", func(t *testing.T) {
		call := Begin(&Env{}, func() string {
			return "ok"
		})

		select {
		case <-call.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("Done is not closed 5s after a call that returns at once")
		}

		if got := call.Finish(); got != "ok" {
			t.Fatalf("Finish = %q, want %q", got, "ok")
		}
	})

	t.Run("a panic reaches the step that finishes", func(t *testing.T) {
		p := &parking{
			body: panicsInTheCall,
		}

		_, err := p.run(t, "begin()\nend()")
		mustHold(t, err,
			"step #01 end: panic: in a background call: boom",
			// The stack of the call, not only of the step that finished it.
			"fuzz.panicsInTheCall",
		)

		if got := strings.Count(fmt.Sprint(err), "in a background call"); got != 1 {
			t.Errorf("the panic is reported %d times, want once:\n%v", got, err)
		}
	})
}

func TestWaitParked(t *testing.T) {
	waitParked := func(t *testing.T, call *Call[string], entered <-chan struct{}) bool {
		t.Helper()

		parked := make(chan bool, 1)

		go func() {
			parked <- call.WaitParked(entered)
		}()

		select {
		case got := <-parked:
			return got
		case <-time.After(5 * time.Second):
			t.Fatal("WaitParked has not returned after 5s")

			return false
		}
	}

	t.Run("a call that parks", func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		call := Begin(&Env{}, func() string {
			close(entered)
			<-release

			return "ok"
		})

		defer call.Finish()

		defer close(release)

		if !waitParked(t, call, entered) {
			t.Error("WaitParked = false for a call at the hook, want true")
		}
	})

	t.Run("a call that returns without parking", func(t *testing.T) {
		call := Begin(&Env{}, func() string {
			return "ok"
		})

		if waitParked(t, call, make(chan struct{})) {
			t.Error("WaitParked = true for a call that never reached the hook, want false")
		}
	})

	// A hook that does not hold the call leaves both signals ready. The answer
	// must not depend on which one is looked at first.
	t.Run("a call that passed the hook and returned", func(t *testing.T) {
		for i := range 100 {
			entered := make(chan struct{})
			call := Begin(&Env{}, func() string {
				close(entered)

				return "ok"
			})
			<-call.Done()

			if !waitParked(t, call, entered) {
				t.Fatalf("attempt %d: WaitParked = false for a call that reached the hook, want true", i)
			}
		}
	})
}

func TestACallNothingFinishes(t *testing.T) {
	t.Run("left parked, it is a leak", func(t *testing.T) {
		var release func()
		t.Cleanup(func() {
			release()
		})
		p := &parking{
			body: func() string {
				return "ok"
			},
			onBegin: func(_ *Env, r func()) {
				release = r
			},
		}

		_, err := p.run(t, "begin()")
		mustHold(t, err, "1 goroutine(s) left running", "#00 begin() -> parked")
	})

	t.Run("released by a cleanup, it passes", func(t *testing.T) {
		p := &parking{
			body: func() string {
				return "ok"
			},
			onBegin: func(env *Env, release func()) {
				env.Cleanup(release)
			},
		}

		if _, err := p.run(t, "begin()"); err != nil {
			t.Fatalf("execute: %v", err)
		}
	})

	t.Run("its panic still fails the input", func(t *testing.T) {
		p := &parking{
			body: func() string {
				panic("boom")
			},
			onBegin: func(env *Env, release func()) {
				env.Cleanup(release)
			},
		}

		_, err := p.run(t, "begin()")
		mustHold(t, err, "#00 begin() -> parked", "panic: in a background call: boom")
	})
}
