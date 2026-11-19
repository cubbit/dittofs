package commands

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/spf13/cobra"
)

// v1Database is where the Kubernetes operator kept v1's control-plane
// database, which a start refuses wherever its bootstrap file is.
const v1Database = "/data/controlplane/controlplane.db"

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
  DITTOFS_LOGGING_LEVEL=DEBUG dfs start

  # Start with a custom config file
  dfs start --config /etc/dittofs/config.yaml`,
		Args: cobra.NoArgs,
		RunE: runStart,
	}
}

// runStart reads the bootstrap, refuses v1's control-plane database, opens
// the metadata store's KV, builds and starts the node, and stops it on
// SIGINT or SIGTERM. Each error names the step that failed.
func runStart(cmd *cobra.Command, _ []string) error {
	configPath, _ := cmd.Flags().GetString("config")

	cfg, err := bootstrap.MustLoad(configPath, os.Environ())
	if err != nil {
		return err
	}
	if err := bootstrap.InitLogger(cfg); err != nil {
		return err
	}
	if configPath == "" {
		configPath = bootstrap.GetDefaultConfigPath()
	}
	if err := refuseV1(filepath.Join(filepath.Dir(configPath), "controlplane.db"), v1Database); err != nil {
		return err
	}

	if b := cfg.Metadata.Backend; b != "" && b != "badger" {
		return fmt.Errorf("opening the metadata store: metadata.backend %q is not one this build opens; it opens badger", b)
	}
	if cfg.Metadata.Badger.Path == "" {
		return errors.New("opening the metadata store: metadata.badger.path is not set")
	}
	clock := newSystemClock()
	kv, err := badgerkv.Open(badgerkv.Options{Dir: cfg.Metadata.Badger.Path, NodeID: cfg.Node.ID, Clock: clock.Now})
	if err != nil {
		return fmt.Errorf("opening the metadata store: %w", err)
	}
	n, err := node.New(node.Config{
		Bootstrap: *cfg,
		KV:        kv,
		Clock:     clock,
		Metrics:   metrics.New(Version, Commit),
		Logger:    slog.Default(),
	})
	if err != nil {
		return fmt.Errorf("building the node: %w", err)
	}

	base := cmd.Context()
	ctx, unnotify := signal.NotifyContext(base, syscall.SIGINT, syscall.SIGTERM)
	defer unnotify()
	if err := n.Start(ctx); err != nil {
		return errors.Join(fmt.Errorf("starting the node: %w", err), stopped(n.Stop(base)))
	}
	<-ctx.Done()
	unnotify() // a second signal ends the process at once
	slog.Info("dfs: stopping on a termination signal")
	return stopped(n.Stop(base))
}

// refuseV1 refuses a start when v1's control-plane database is at any of
// paths, naming it, before anything is opened.
func refuseV1(paths ...string) error {
	for _, p := range paths {
		_, err := os.Stat(p)
		switch {
		case err == nil:
			return fmt.Errorf("refusing to start: %s is v1's control-plane database, which this version does not read; move it away to start", p)
		case !errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("refusing to start: checking for v1's control-plane database at %s: %w", p, err)
		}
	}
	return nil
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
