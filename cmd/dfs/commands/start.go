package commands

import (
	"fmt"
	"log/slog"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/pkg/config"
	"github.com/spf13/cobra"
)

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the DittoFS server",
	Long: `Start the DittoFS server in the foreground with the specified configuration.

The server runs until it is stopped. Run it under a process supervisor
(systemd, Docker) to keep it in the background.

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

func runStart(cmd *cobra.Command, _ []string) error {
	cfg, err := config.MustLoad(GetConfigFile())
	if err != nil {
		return err
	}
	if err := config.InitLogger(cfg); err != nil {
		return err
	}
	// The KV, the clock and the metrics registry join the node's
	// configuration once the node opens its stores.
	n, err := node.New(node.Config{Bootstrap: *cfg, Logger: slog.Default()})
	if err != nil {
		return fmt.Errorf("building the node: %w", err)
	}
	ctx := cmd.Context()
	if err := n.Start(ctx); err != nil {
		return fmt.Errorf("starting the node: %w", err)
	}
	// ponytail: a node that starts is stopped at once, since none starts yet
	// and so none has anything to serve. Wait for a termination signal
	// before Stop once Start starts anything.
	return n.Stop(ctx)
}
