package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/sim"
)

// A part whose stop misses its bound leaves open the parts it uses, and only
// those: a part started before it that it does not use still stops. A later
// halt, once the stuck stop ended, stops what the first one left.
func TestHaltLeavesOpenWhatAStuckPartUses(t *testing.T) {
	prof.Watch(t, prof.Options{})
	clock := sim.NewTestClock(time.Unix(0, 0))
	n := &Node{cfg: Config{Clock: clock}}
	var mu sync.Mutex
	var stopped []string
	stop := func(name string) func(context.Context) error {
		return func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			stopped = append(stopped, name)
			return nil
		}
	}
	release := make(chan struct{})
	waits := make(chan string, 8)
	n.life.stepped = func(step string) {
		if part, ok := strings.CutPrefix(step, "wait "); ok {
			waits <- part
		}
	}
	n.started("used", stop("used"))
	n.started("unrelated", stop("unrelated"))
	n.started("stuck", func(ctx context.Context) error { <-release; return stop("stuck")(ctx) }, "used")
	n.started("last", stop("last"))

	halted := make(chan error, 1)
	go func() { halted <- n.halt(t.Context(), 0) }()
	backstop := time.After(10 * time.Second)
	for waiting := true; waiting; {
		select {
		case part := <-waits:
			waiting = part != "stuck"
		case <-backstop:
			t.Fatal("halt never waited on the stuck part's stop")
		}
	}
	clock.Advance(stopBound)
	var err error
	select {
	case err = <-halted:
	case <-time.After(10 * time.Second):
		t.Fatal("halt still running 10 s after the stuck part's bound")
	}
	if err == nil || !strings.Contains(err.Error(), "stop stuck: not stopped within") || !strings.Contains(err.Error(), "stop used: left open") {
		t.Errorf("halt with a stuck part: got %v, want it named past its bound and the part it uses left open", err)
	}
	mu.Lock()
	got := slices.Clone(stopped)
	mu.Unlock()
	if want := []string{"last", "unrelated"}; !slices.Equal(got, want) {
		t.Errorf("parts stopped while one was stuck: got %v, want %v", got, want)
	}

	close(release)
	if err := n.halt(t.Context(), 0); err != nil {
		t.Errorf("a second halt once the stuck stop ended: got %v, want nil", err)
	}
	if want := []string{"last", "unrelated", "stuck", "used"}; !slices.Equal(stopped, want) {
		t.Errorf("parts stopped after the second halt: got %v, want %v", stopped, want)
	}
	if len(n.life.parts) != 0 {
		t.Errorf("parts left after the second halt: got %d, want none", len(n.life.parts))
	}
}

// A part whose stop fails is kept, like one past its bound: the parts it uses,
// the store and the storage key stay open, and a later Stop runs its stop
// again, then closes the store and the key.
func TestStopRetriesAPartWhoseStopFailed(t *testing.T) {
	// decision: serial, because localKey sets the key passphrase with
	// t.Setenv, which a parallel test may not call.
	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatal(err)
	}
	clk := sim.NewTestClock(time.Unix(0, 0))
	key := localKey(t)
	n := &Node{cfg: Config{Clock: clk, KV: db}, storageKey: key, tokens: auth.NewTokens(nil, [16]byte{}, entity.SecretRef{}, clk)}
	var used, engineStops int
	n.started("used", func(context.Context) error { used++; return nil })
	n.started("engine", func(context.Context) error {
		engineStops++
		if engineStops == 1 {
			return errors.New("injected")
		}
		return nil
	}, "used")

	err = n.Stop(t.Context())
	for _, want := range []string{"stop engine: injected", "stop used: left open", "stop store: left open"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Stop with the engine's stop failing: got %v, want it to say %q", err, want)
		}
	}
	if err := db.View(t.Context(), func(kv.Reader) error { return nil }); err != nil {
		t.Errorf("the KV after a Stop whose engine stop failed: %v, want it open", err)
	}
	if _, _, err := key.Wrap(t.Context(), []byte("a block key")); err != nil {
		t.Errorf("the storage key after a Stop whose engine stop failed: wrap %v, want it open", err)
	}
	if used != 0 {
		t.Errorf("stops of the part the engine uses, its stop failed: got %d, want 0", used)
	}

	if err := n.Stop(t.Context()); err != nil {
		t.Errorf("a second Stop: got %v, want nil", err)
	}
	if engineStops != 2 || used != 1 {
		t.Errorf("stops after the second Stop: engine %d, used %d; want engine 2, used 1", engineStops, used)
	}
	if err := db.View(t.Context(), func(kv.Reader) error { return nil }); err == nil {
		t.Error("the KV after the second Stop: a View succeeded, want it closed")
	}
	if _, _, err := key.Wrap(t.Context(), []byte("a block key")); err == nil {
		t.Error("the storage key after the second Stop: a wrap succeeded, want it closed")
	}
}

// A part carrying its own bound is waited on for that bound, not stopBound,
// as stop step 2 is bounded by its drain deadline plus 30 s (RFC 23 §2.2):
// one of 120 s whose stop holds is still waited on once the clock has moved
// 31 s, past stopBound, and is named not stopped within its 120 s once the
// clock has moved that far. The test runs in a bubble, so synctest.Wait
// returns once halt waits on the clock, or has answered.
func TestHaltWaitsAPartForItsOwnBound(t *testing.T) {
	prof.Watch(t, prof.Options{})
	synctest.Test(t, func(t *testing.T) {
		clock := sim.NewTestClock(time.Unix(0, 0))
		n := &Node{cfg: Config{Clock: clock}}
		release := make(chan struct{})
		defer close(release)
		n.life.parts = []*part{{name: "drain", bound: func() time.Duration { return 120 * time.Second },
			stop: func(context.Context) error { <-release; return nil }}}

		halted := make(chan error, 1)
		go func() { halted <- n.halt(t.Context(), 0) }()
		synctest.Wait()
		clock.Advance(stopBound + time.Second)
		synctest.Wait()
		select {
		case err := <-halted:
			t.Fatalf("halt %v in, past stopBound: answered %v, want the part still waited on until its own 2m0s", clock.Elapsed(), err)
		default:
		}
		clock.Advance(120*time.Second - clock.Elapsed())
		synctest.Wait()
		select {
		case err := <-halted:
			if err == nil || !strings.Contains(err.Error(), "stop drain: not stopped within 2m0s") {
				t.Errorf("halt past the part's own bound: got %v, want it named not stopped within 2m0s", err)
			}
		default:
			t.Fatal("halt still waiting with the clock at the part's own bound")
		}
	})
}

// localKey is a real storage key, from a key file under t.TempDir(), which
// Stop closes. It sets the key file's passphrase in the environment.
func localKey(t *testing.T) keyprovider.KeyProvider {
	t.Helper()
	const passphrase = "lifecycle test passphrase"
	t.Setenv("DITTOFS_ENCRYPTION_PASSPHRASE", passphrase)
	b, err := keyprovider.GenerateKeyFile(passphrase)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "storage.key")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	kp, err := keyprovider.NewProvider(t.Context(), keyprovider.Config{Kind: keyprovider.KindLocal, File: path})
	if err != nil {
		t.Fatal(err)
	}
	return kp
}
