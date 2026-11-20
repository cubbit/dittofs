package commands

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/cubbit/dittofs/internal/install"
	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/keyprovider"
)

func newResetAdminCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reset-admin",
		Short: "Reset the administrator's credentials",
		Long: `Reset the credentials of an administrator, on the host that holds the
installation's metadata store, for when no administrator can sign in.

The node must be stopped: while a node holds the store, the reset refuses and
changes nothing. It resets an existing, enabled account with role admin, and
refuses any other. It sets a new generated password, printed once to
standard output and never to the log, that must be changed at the first
sign-in; refuses every credential issued before it; clears the account's
sign-in failure count and every source's; and records the reset in the audit,
naming this host.

Examples:
  # Reset the administrator "admin"
  dfs reset-admin

  # Reset another administrator
  dfs reset-admin --name ops`,
		Args: cobra.NoArgs,
		RunE: runResetAdmin,
	}
	cmd.Flags().String("name", install.AdminName, "the administrator account to reset")
	return cmd
}

// runResetAdmin reads the bootstrap, opens the role keys and the metadata
// store's KV, and resets the named administrator over them. Each error
// names the step that failed.
func runResetAdmin(cmd *cobra.Command, _ []string) (err error) {
	configPath, _ := cmd.Flags().GetString("config")
	name, _ := cmd.Flags().GetString("name")
	ctx := cmd.Context()

	cfg, err := bootstrap.MustLoad(configPath, os.Environ())
	if err != nil {
		return err
	}
	if err := bootstrap.InitLogger(cfg); err != nil {
		return err
	}
	if err := checkKV(cfg); err != nil {
		return err
	}
	if cfg.Keys.Storage.Kind == "" {
		return errors.New("keys.storage is not set: the administrator's password is sealed under it")
	}
	host, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("reading the host name: %w", err)
	}
	storage, err := keyprovider.NewProvider(ctx, cfg.Keys.Storage.Provider())
	if err != nil {
		return fmt.Errorf("opening the storage key: %w", err)
	}
	defer func() { err = errors.Join(err, storage.Close()) }()
	// The protocol key seals the NT hash, which only an account with a file
	// spelling gets while NTLM is on; a bootstrap naming none refuses then.
	var protocol keyprovider.KeyProvider
	if cfg.Keys.Protocol.Kind != "" {
		if protocol, err = keyprovider.NewProvider(ctx, cfg.Keys.Protocol.Provider()); err != nil {
			return fmt.Errorf("opening the protocol key: %w", err)
		}
		defer func() { err = errors.Join(err, protocol.Close()) }()
	}
	// Badger's directory lock refuses the open while a node holds the store,
	// before anything is read or written.
	kv, err := openKV(cfg, newSystemClock())
	if err != nil {
		return fmt.Errorf("opening the metadata store; is a node running over it? Stop it first: %w", err)
	}
	defer func() { err = errors.Join(err, kv.Close()) }()
	return install.ResetAdmin(ctx, install.ResetDeps{
		KV:          kv,
		StorageKey:  storage,
		ProtocolKey: protocol,
		Name:        name,
		Host:        host,
		Node:        cfg.Node.ID,
		Out:         cmd.OutOrStdout(),
		Logger:      slog.Default(),
	})
}
