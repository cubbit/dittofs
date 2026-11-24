package commands

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cubbit/dittofs/internal/install"
	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
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

When keys.storage.local.file names a file that does not exist and the
metadata store is not yet initialised, init creates it, a master key
wrapped under DITTOFS_ENCRYPTION_PASSPHRASE, and prints once where it is:
copy it off the host, since losing it or the passphrase loses the data. An
existing file is used, never overwritten. With the passphrase unset, init
refuses.

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
	// The storage key is checked before the metadata store opens, so a
	// refused key leaves no store directory behind and is reported ahead of
	// any store or journal error: an existing key is opened now, and a key
	// file still to create needs the passphrase.
	var key keyprovider.KeyProvider
	defer func() {
		if key != nil {
			err = errors.Join(err, key.Close())
		}
	}()
	keyFile, passphrase := "", os.Getenv("DITTOFS_ENCRYPTION_PASSPHRASE")
	if cfg.Keys.Storage.Kind == keyprovider.KindLocal {
		keyFile = cfg.Keys.Storage.Local.File
	}
	if _, serr := os.Lstat(keyFile); keyFile == "" || !errors.Is(serr, fs.ErrNotExist) {
		k, err := keyprovider.NewProvider(ctx, cfg.Keys.Storage.Provider())
		if err != nil {
			return fmt.Errorf("opening the storage key: %w", err)
		}
		key = k
	} else if passphrase == "" {
		return fmt.Errorf("creating the key file %s: %w", keyFile, keyprovider.ErrPassphraseMissing)
	}
	clock := newSystemClock()
	kv, err := openKV(ctx, cfg, clock)
	if err != nil {
		return fmt.Errorf("opening the metadata store: %w", err)
	}
	defer func() { err = errors.Join(err, kv.Close()) }()
	created := false
	storageKey := func(ctx context.Context, fresh bool) (keyprovider.KeyProvider, error) {
		if key != nil {
			return key, nil
		}
		if fresh {
			var err error
			created, err = keyprovider.CreateKeyFile(keyFile, passphrase)
			// decision: a file linked in place is announced even when a
			// later step fails, but a run stopped between the link and this
			// notice leaves a file no later run announces: the rerun finds
			// a file it cannot tell from one the operator placed, and
			// announces neither. Overturned if init records which key file
			// it created, so a rerun can name it.
			if created {
				_, perr := fmt.Fprintf(cmd.OutOrStdout(), "created the storage key file %s\n"+
					"Copy it off this host now and keep its passphrase: losing either loses the data, and nothing recovers it.\n", keyFile)
				err = errors.Join(err, perr)
			}
			if err != nil {
				return nil, err
			}
		}
		k, err := keyprovider.NewProvider(ctx, cfg.Keys.Storage.Provider())
		if err != nil {
			return nil, fmt.Errorf("opening the storage key: %w", err)
		}
		key = k
		return k, nil
	}
	err = install.Run(ctx, install.Deps{
		Bootstrap:    cfg,
		KV:           kv,
		StorageKey:   storageKey,
		Clock:        clock,
		PasswordFile: file,
		PasswordEnv:  env,
		Out:          cmd.OutOrStdout(),
		Logger:       slog.Default(),
	})
	// decision: the key file this run created is left in place, never
	// removed: another init on this host may have found it and initialised
	// the store under it, and removing a key the store uses loses the data.
	// The operator, who knows which runs there were, removes it.
	if created && errors.Is(err, install.ErrConcurrentInit) {
		return fmt.Errorf("%w: unless the run that initialised the store used the key file %s this run created, remove that file and every copy of it, put that run's key file in its place, and run dfs init again", err, keyFile)
	}
	// decision: a store whose first key has one of v1's prefixes is refused
	// as v1's over Badger only, as the node refuses it. This format's records
	// with the format record lost, and any store over another backend, keep
	// ErrForeignStore's own wording: neither is v1's to clear.
	if b := cfg.Metadata.Backend; errors.Is(err, kvstore.ErrV1Store) && (b == "" || b == "badger") {
		return refuseV1Found(cfg.Metadata.Badger.Path, "a metadata store")
	}
	return err
}
