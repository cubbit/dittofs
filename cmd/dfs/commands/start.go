package commands

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/spf13/cobra"
)

func newStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the DittoFS server",
		Long: `Start the DittoFS server in the foreground with the specified configuration.

The server runs until it receives SIGINT or SIGTERM, then stops and exits
zero once every part stopped. A start that fails for its own reason exits
non-zero, even when a signal arrived while it failed. Run it under a
process supervisor (systemd, Docker) to keep it in the background.

Examples:
  # Start with the default configuration
  dfs start

  # Start with debug logging
  DITTOFS_LOGGING_LEVEL=debug dfs start

  # Start with a custom config file
  dfs start --config /etc/dittofs/config.yaml`,
		Args: cobra.NoArgs,
		RunE: runStart,
	}
}

// runStart reads the bootstrap, refuses v1's control-plane database, runs the
// start check over the store and journals it finds, opens the metadata
// store's KV, builds and starts the node, and stops it on SIGINT or SIGTERM.
// Each error names the step that failed.
func runStart(cmd *cobra.Command, _ []string) error {
	configPath, _ := cmd.Flags().GetString("config")
	// The handler is installed first, so a signal while the store opens or
	// the node is built stops the start rather than killing the process;
	// Start then returns at once and the node is stopped.
	base := cmd.Context()
	ctx, unnotify := signal.NotifyContext(base, syscall.SIGINT, syscall.SIGTERM)
	defer unnotify()
	// Once a signal arrived, a second one ends the process at once.
	context.AfterFunc(ctx, unnotify)

	cfg, err := bootstrap.MustLoad(configPath, os.Environ())
	if err != nil {
		return err
	}
	if configPath == "" {
		// decision: the v1 check probes beside the bootstrap MustLoad read:
		// --config's, else DITTOFS_CONFIG's, else the default. So v1's XDG
		// default database, $XDG_CONFIG_HOME/dittofs/controlplane.db, is probed
		// only when neither names a file; a bootstrap named elsewhere probes
		// beside that file and at the fixed /data paths only. Overturned by a
		// v1 deployment that kept its database at the XDG default while its
		// bootstrap moved elsewhere.
		configPath = cmp.Or(os.Getenv("DITTOFS_CONFIG"), bootstrap.GetDefaultConfigPath())
	}
	// The v1 check reads nothing of the bootstrap, so it runs before the
	// bootstrap's own checks: over a v1 directory it is the first refusal.
	//
	// decision: it still runs after MustLoad, which needs --config's path,
	// and before InitLogger, so a v1 bootstrap that this version's load
	// refuses gets that config error, not the v1 refusal, and the check's
	// permission warning goes to the default handler, as text on stderr,
	// not to the configured log. Both are safe, as nothing is opened yet.
	// Overturned if operators upgrading from v1 report the config error as
	// unhelpful: then the check moves before MustLoad, deriving the path
	// from the flag alone.
	if err := refuseV1(filepath.Join(filepath.Dir(configPath), "controlplane.db"), v1Databases()); err != nil {
		return err
	}
	if err := bootstrap.InitLogger(cfg); err != nil {
		return err
	}
	clock := newSystemClock()
	if err := checkKV(cfg); err != nil {
		return err
	}
	devices := make([]string, 0, len(cfg.Journals))
	for _, j := range cfg.Journals {
		devices = append(devices, j.Path)
	}

	v1Root := v1ProbeRoot(cfg.Metadata.Badger.Path, devices)

	stage(base, stagePreflight, ctx)
	err = node.Preflight(ctx, cfg.Metadata.Badger.Path, v1Root, devices, clock)
	stage(base, stagePreflightReturned, ctx)
	if err != nil {
		// A check the signal cancelled ends the start on the signal: nothing
		// is open yet, so the command exits zero as a stopped start does. A
		// refusal of the check's own exits non-zero under a signal too.
		if ctx.Err() != nil && base.Err() == nil && onlyCanceled(err) {
			slog.Info("dfs: the start ended on a termination signal", "err", err)
			return nil
		}
		return fmt.Errorf("the start check: %w", err)
	}
	// The tracing provider is built after the start's refusals, so a refused
	// start builds no exporter, and before the KV opens: node.New owns the KV
	// from openKV on — a New that fails closes it — so a start refused on its
	// endpoint must refuse before the KV exists, not leak one.
	tracer, stopTracing, err := devTracing()
	if err != nil {
		return fmt.Errorf("building the tracing provider: %w", err)
	}
	defer func() {
		if err := stopTracing(context.Background()); err != nil {
			slog.Warn("dfs: the trace provider did not shut down clean", "err", err)
		}
	}()
	// The local socket's default path is in the runtime directory, which the
	// node does not make, so the start makes it here, after every refusal
	// above and before the store opens.
	if _, err := bootstrap.MakeRuntimeDir(os.Environ()); err != nil {
		return fmt.Errorf("making the runtime directory: %w", err)
	}

	store, err := openKV(ctx, cfg, clock)
	if err != nil {
		return fmt.Errorf("opening the metadata store: %w", err)
	}
	if wrap, ok := base.Value(kvKey{}).(func(kv.KV) kv.KV); ok {
		store = wrap(store)
	}
	n, err := node.New(node.Config{
		Bootstrap: *cfg,
		KV:        store,
		Clock:     clock,
		Metrics:   metrics.New(Version, Commit),
		Logger:    slog.Default(),
		Tracer:    tracer,
	})
	if err != nil {
		return fmt.Errorf("building the node: %w", err)
	}

	stage(base, stageStart, ctx)
	startErr := n.Start(ctx)
	if startErr == nil {
		<-ctx.Done()
	}
	stage(base, stageStartReturned, ctx)
	signalled := ctx.Err() != nil && base.Err() == nil
	// ended decides both the record below and the exit code, so the tests
	// that read the record read the value the exit code is answered from.
	ended := endedBySignal(startErr, signalled)
	if signalled {
		if ended {
			slog.Info("dfs: the start ended on a termination signal", "err", startErr)
		}
		slog.Info("dfs: stopping on a termination signal")
	}
	stopErr := n.Stop(base)
	if stopErr == nil {
		slog.Info("dfs: stopped")
	}
	return answer(startErr, ended, stopErr)
}

// stageKey keys a hook in the command's context that runs at each stage of
// runStart, given the stage and the signal context: a test delivers a signal
// there, so it lands at a known stage rather than at a timing. Production
// sets none.
type stageKey struct{}

// kvKey keys a hook in the command's context that wraps the metadata
// store's KV before the node opens it: a test holds one store call there, so
// the process is killed at a known call rather than at a timing. Production
// sets none.
type kvKey struct{}

// The stages a stageKey hook is called at: before the start check and once
// it returned, and before Start and once it returned, each "returned" stage
// before the command reads whether a signal arrived.
const (
	stagePreflight         = "preflight"
	stagePreflightReturned = "preflight-returned"
	stageStart             = "start"
	stageStartReturned     = "start-returned"
)

// stage runs base's stageKey hook, if any, at name.
func stage(base context.Context, name string, signal context.Context) {
	if f, ok := base.Value(stageKey{}).(func(string, context.Context)); ok {
		f(name, signal)
	}
}

// endedBySignal reports whether a termination signal, not a failure of its
// own, ended a start that failed: a signal arrived and every error Start
// returned is the signal's cancellation. A start that failed for its own
// reason while a signal arrived did not end on it, even when another of its
// errors is a cancellation.
func endedBySignal(startErr error, signalled bool) bool {
	return signalled && onlyCanceled(startErr)
}

// onlyCanceled reports whether err is non-nil and every leaf of its tree,
// through both forms of Unwrap, is context.Canceled.
func onlyCanceled(err error) bool {
	switch u := err.(type) {
	case interface{ Unwrap() []error }:
		errs := u.Unwrap()
		for _, e := range errs {
			if !onlyCanceled(e) {
				return false
			}
		}
		return len(errs) > 0
	case interface{ Unwrap() error }:
		if inner := u.Unwrap(); inner != nil {
			return onlyCanceled(inner)
		}
	}
	return errors.Is(err, context.Canceled)
}

// answer is the command's error once the node stopped: a Start that failed
// but did not end on a termination signal, naming its step, joined with
// Stop's; otherwise Stop's alone, so a signal exits zero once every part
// stopped.
func answer(startErr error, ended bool, stopErr error) error {
	if startErr != nil && !ended {
		return errors.Join(fmt.Errorf("starting the node: %w", startErr), stopped(stopErr))
	}
	return stopped(stopErr)
}

// stopped is the command's answer to Stop's error: nil when every part
// stopped, else the error naming the stop step that failed or missed its
// bound, which exits non-zero.
func stopped(err error) error {
	if err != nil {
		return fmt.Errorf("stopping the node: %w", err)
	}
	return nil
}

// v1ProbeRoot is the one extra root the start check reads for a pre-journal
// (v1) layout, beside the devices: `blocks` in the store directory's parent.
//
// decision: v1 kept its data under `<state dir>/blocks` unless its
// blockstore.journal.path said otherwise. That key is gone, so the root is
// derived from the store's path, which the file or
// `DITTOFS_METADATA_BADGER_PATH` sets, or the loader defaulting it to
// `<state dir>/metadata`: `blocks` beside it. It is v1's default root when
// the store sits at `<state dir>/metadata`; with the store anywhere else the
// probe can miss a default v1 layout. A layout found there refuses a start
// before anything opens. The check takes one extra root, so the store
// directory's parent itself and a custom v1 root stay unprobed until the
// start check takes more than one.
// Without an absolute store path with a parent below the root (a store at
// /x would probe /blocks), the first device stands in, probed twice with no
// effect, and "" is left for no device at all, which the check refuses
// before it probes: an empty root would be read as the working directory.
func v1ProbeRoot(store string, devices []string) string {
	if store != "" {
		p := filepath.Clean(store)
		parent := filepath.Dir(p)

		if filepath.IsAbs(p) && filepath.Dir(parent) != parent {
			return filepath.Join(parent, "blocks")
		}
	}

	if len(devices) > 0 {
		return devices[0]
	}

	return ""
}
