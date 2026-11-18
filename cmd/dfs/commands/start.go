package commands

import (
	"fmt"
	"log/slog"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/spf13/cobra"
)

func newStartCmd() *cobra.Command {
	return &cobra.Command{
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
}

func runStart(cmd *cobra.Command, _ []string) error {
	configPath, _ := cmd.Flags().GetString("config")

	cfg, err := bootstrap.MustLoad(configPath)
	if err != nil {
		return err
	}

	if err := bootstrap.InitLogger(cfg); err != nil {
		return err
	}
	// The KV, the clock and the metrics registry join the node's
	// configuration once the start command opens the host's store; until
	// then New refuses.
	n, err := node.New(node.Config{Bootstrap: *cfg, Logger: slog.Default()})
	if err != nil {
		return fmt.Errorf("building the node: %w", err)
	}
	ctx := cmd.Context()
	if err := n.Start(ctx); err != nil {
		return fmt.Errorf("starting the node: %w", err)
	}
	// ponytail: Stop runs at once, because New refuses until this command
	// opens the host's KV and metrics registry, so no node gets here yet.
	// Wait for a termination signal before Stop once this command does.
	return n.Stop(ctx)
}
