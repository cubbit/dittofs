package commands

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cubbit/dittofs/internal/install"
	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/keyprovider"
)

func newInitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize a new installation",
		Long: `Initialize a new DittoFS installation: its metadata store, each journal
directory with the node's identity beside it, the administrator account
"admin" and the signing key, in the stores the server's configuration names.

The administrator's first password is read from a file or an environment
variable. With neither, a password is generated and printed once to standard
output, never to the log, and must be changed at the first sign-in.

Run again after an interruption, init finishes what the interrupted run left;
over a finished installation it refuses and changes nothing.

Examples:
  # Initialize, generating the administrator's password
  dfs init

  # Initialize with the password in a file (one trailing newline is dropped)
  dfs init --admin-password-file /run/secrets/dittofs-admin

  # Initialize with the password in an environment variable
  DFS_ADMIN_PASSWORD=... dfs init --admin-password-env DFS_ADMIN_PASSWORD`,
		Args: cobra.NoArgs,
		RunE: runInit,
	}
	cmd.Flags().String("admin-password-file", "", "file holding the administrator's first password, at most 4 KiB")
	cmd.Flags().String("admin-password-env", "", "environment variable holding the administrator's first password")
	cmd.MarkFlagsMutuallyExclusive("admin-password-file", "admin-password-env")
	return cmd
}

// runInit reads the bootstrap, opens the storage role's key and the metadata
// store's KV, and runs the initialisation step over them and the journal
// directories the bootstrap names. Each error names the step that failed.
func runInit(cmd *cobra.Command, _ []string) (err error) {
	configPath, _ := cmd.Flags().GetString("config")
	file, _ := cmd.Flags().GetString("admin-password-file")
	env, _ := cmd.Flags().GetString("admin-password-env")
	ctx := cmd.Context()
	// A flag given an empty value, as "$UNSET" expands to, names no
	// reference; it is refused rather than read as no flag, which would
	// generate a password.
	for _, flag := range []string{"admin-password-file", "admin-password-env"} {
		if v, _ := cmd.Flags().GetString(flag); cmd.Flags().Changed(flag) && v == "" {
			return fmt.Errorf("%w: --%s is empty", install.ErrPassword, flag)
		}
	}

	// The variable --admin-password-env names holds the password, not a
	// setting, so the bootstrap's scan of DITTOFS_ names must not see it.
	hostEnv := os.Environ()
	if env != "" {
		hostEnv = slices.DeleteFunc(hostEnv, func(kv string) bool { return strings.HasPrefix(kv, env+"=") })
	}

	cfg, err := bootstrap.MustLoad(configPath, hostEnv)
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
		return errors.New("keys.storage is not set: the administrator's password and the signing key are sealed under it")
	}
	key, err := keyprovider.NewProvider(ctx, cfg.Keys.Storage.Provider())
	if err != nil {
		return fmt.Errorf("opening the storage key: %w", err)
	}
	defer func() { err = errors.Join(err, key.Close()) }()
	clock := newSystemClock()
	kv, err := openKV(cfg, clock)
	if err != nil {
		return fmt.Errorf("opening the metadata store: %w", err)
	}
	defer func() { err = errors.Join(err, kv.Close()) }()
	return install.Run(ctx, install.Deps{
		Bootstrap:    cfg,
		KV:           kv,
		StorageKey:   key,
		Clock:        clock,
		PasswordFile: file,
		PasswordEnv:  env,
		Out:          cmd.OutOrStdout(),
		Logger:       slog.Default(),
	})
}
