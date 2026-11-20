package commands

import (
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
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/spf13/cobra"
)

func newStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the DittoFS server",
		Long: `Start the DittoFS server in the foreground with the specified configuration.

The server runs until it receives SIGINT or SIGTERM, then stops and exits
zero once every part stopped. Run it under a process supervisor (systemd,
Docker) to keep it in the background.

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
		// decision: v1's XDG default database, $XDG_CONFIG_HOME/dittofs/controlplane.db,
		// is probed only without --config, as the file beside the default
		// bootstrap; a --config elsewhere probes beside that file and at the
		// fixed /data paths only. Overturned by a v1 deployment that kept its
		// database at the XDG default while its bootstrap moved elsewhere.
		configPath = bootstrap.GetDefaultConfigPath()
	}
	// The v1 check reads nothing of the bootstrap, so it runs before the
	// bootstrap's own checks: over a v1 directory it is the first refusal.
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
	if err := node.Preflight(ctx, cfg.Metadata.Badger.Path, cfg.Blockstore.Journal.Path, devices, clock); err != nil {
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
	kv, err := openKV(cfg, clock)
	if err != nil {
		return fmt.Errorf("opening the metadata store: %w", err)
	}
	n, err := node.New(node.Config{
		Bootstrap: *cfg,
		KV:        kv,
		Clock:     clock,
		Metrics:   metrics.New(Version, Commit),
		Logger:    slog.Default(),
		Tracer:    tracer,
	})
	if err != nil {
		return fmt.Errorf("building the node: %w", err)
	}

	startErr := n.Start(ctx)
	if startErr == nil {
		<-ctx.Done()
	}
	signalled := ctx.Err() != nil && base.Err() == nil
	if signalled {
		if errors.Is(startErr, context.Canceled) {
			slog.Info("dfs: the start ended on a termination signal", "err", startErr)
		}
		slog.Info("dfs: stopping on a termination signal")
	}
	stopErr := n.Stop(base)
	if stopErr == nil {
		slog.Info("dfs: stopped")
	}
	return answer(startErr, signalled, stopErr)
}

// endedBySignal reports whether a termination signal, not a failure of its
// own, ended the start: a signal arrived and Start succeeded or returned the
// signal's cancellation. A start that failed for its own reason while a signal
// arrived did not end on it.
func endedBySignal(startErr error, signalled bool) bool {
	return signalled && (startErr == nil || errors.Is(startErr, context.Canceled))
}

// answer is the command's error once the node stopped: a Start that failed
// for any reason but a termination signal, naming its step, joined with
// Stop's; otherwise Stop's alone, so a signal exits zero once every part
// stopped.
func answer(startErr error, signalled bool, stopErr error) error {
	if startErr != nil && !endedBySignal(startErr, signalled) {
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
