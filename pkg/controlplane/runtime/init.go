package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/internal/logger"
	"github.com/cubbit/dittofs/internal/pathutil"
	"github.com/cubbit/dittofs/pkg/block"
	"github.com/cubbit/dittofs/pkg/controlplane/models"
	"github.com/cubbit/dittofs/pkg/controlplane/store"
	"github.com/cubbit/dittofs/pkg/metadata"
	"github.com/cubbit/dittofs/pkg/metadata/store/badger"
)

// InitializeFromStore creates a runtime and loads metadata stores from the database.
func InitializeFromStore(ctx context.Context, s store.Store) (*Runtime, error) {
	rt := New(s)
	if err := loadMetadataStores(ctx, rt, s); err != nil {
		return nil, fmt.Errorf("failed to load metadata stores: %w", err)
	}
	return rt, nil
}

func loadMetadataStores(ctx context.Context, rt *Runtime, s store.Store) error {
	stores, err := s.ListMetadataStores(ctx)
	if err != nil {
		return fmt.Errorf("failed to list metadata stores: %w", err)
	}

	for _, storeCfg := range stores {
		metaStore, err := CreateMetadataStoreFromConfig(ctx, storeCfg.Type, storeCfg)
		if err != nil {
			return fmt.Errorf("failed to create metadata store %q: %w", storeCfg.Name, err)
		}

		if err := rt.RegisterMetadataStore(storeCfg.Name, metaStore); err != nil {
			return fmt.Errorf("failed to register metadata store %q: %w", storeCfg.Name, err)
		}

		logger.Info("Loaded metadata store", "name", storeCfg.Name, "type", storeCfg.Type)
	}

	return nil
}

// ValidateMetadataStoreType rejects every metadata store type but badger, the
// only backend. A row still naming a removed type (memory, sqlite, postgres)
// must fail loudly rather than start its shares on some other backend.
func ValidateMetadataStoreType(storeType string) error {
	if storeType != "badger" {
		return fmt.Errorf("unsupported metadata store type %q: badger is the only metadata store type", storeType)
	}
	return nil
}

// CreateMetadataStoreFromConfig creates a metadata store instance from type and config.
func CreateMetadataStoreFromConfig(ctx context.Context, storeType string, cfg interface {
	GetConfig() (map[string]any, error)
}) (metadata.Store, error) {
	config, err := cfg.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get config: %w", err)
	}

	if err := ValidateMetadataStoreType(storeType); err != nil {
		return nil, err
	}

	if configBoolDefault(config, "in_memory", false) {
		// Nothing survives a restart: for tests and throwaway servers only.
		return badger.NewInMemoryBadgerMetadataStore(ctx)
	}

	dbPath, ok := config["path"].(string)
	if !ok || dbPath == "" {
		dbPath, ok = config["db_path"].(string) // accept legacy key
		if !ok || dbPath == "" {
			return nil, errors.New("badger metadata store requires path as string (or in_memory: true)")
		}
	}
	dbPath, err = pathutil.ExpandPath(dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to expand path %q: %w", dbPath, err)
	}
	// Optional per-store Badger cache overrides (#1245 Bug D). The docs
	// define 0 = unset (fall through to global config then RAM-relative
	// auto-sizing) and positive = explicit size in MiB. A negative value is
	// a config error — reject it here rather than letting it silently fall
	// through to auto-sizing as if it were unset.
	blockCacheMB := configInt64(config, "block_cache_mb")
	if blockCacheMB < 0 {
		return nil, fmt.Errorf("badger metadata store block_cache_mb must be >= 0 (0 = auto), got %d", blockCacheMB)
	}
	indexCacheMB := configInt64(config, "index_cache_mb")
	if indexCacheMB < 0 {
		return nil, fmt.Errorf("badger metadata store index_cache_mb must be >= 0 (0 = auto), got %d", indexCacheMB)
	}
	// Relaxed durability defers namespace-op fsyncs for higher single-thread
	// throughput; data-paired writes stay synchronous (#1573 Wall 1).
	// Shipped default: enabled. Operators restore strict per-txn fsync with
	// relaxed_durability: false.
	relaxedDurability := configBoolDefault(config, "relaxed_durability", true)
	return badger.NewBadgerMetadataStoreWithDefaultsAndCaches(ctx, dbPath, blockCacheMB, indexCacheMB, relaxedDurability)
}

// configInt64 extracts an integer value from a type-specific config map under
// key, returning 0 when the key is absent or not a number. Config maps are
// typically produced by json.Unmarshal (numbers decode to float64), but a
// directly-set map may carry int/int64, so all are accepted.
func configInt64(config map[string]any, key string) int64 {
	switch v := config[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// configBoolDefault extracts a bool value from a type-specific config map under
// key, returning def when the key is absent. Used for opt-out flags whose
// shipped default is not the zero value (e.g. relaxed_durability defaults to
// true — #1573 Wall 1).
func configBoolDefault(config map[string]any, key string, def bool) bool {
	if v, ok := config[key].(bool); ok {
		return v
	}
	return def
}

// LoadSharesFromStore loads shares from the database into the runtime.
func LoadSharesFromStore(ctx context.Context, rt *Runtime, s store.Store) error {
	shares, err := s.ListShares(ctx)
	if err != nil {
		return fmt.Errorf("failed to list shares: %w", err)
	}

	for _, share := range shares {
		shareConfig, err := buildShareConfig(ctx, s, share)
		if err != nil {
			return err
		}
		if shareConfig == nil {
			// Share references an unknown metadata store; already logged.
			rt.markShareSkipped(share.Name, "metadata store "+share.MetadataStoreID+" is not configured")
			continue
		}

		if err := rt.AddShare(ctx, shareConfig); err != nil {
			// A share whose on-disk format this build cannot read is a hard
			// boot stop, not a per-share warn-and-skip: warn-and-skip leaves
			// the daemon running and looking healthy with the share silently
			// absent. Surface it so cmd/dfs/commands/start.go can exit 78 with
			// the operator directive. Every other AddShare failure stays a
			// warn-and-skip.
			if errors.Is(err, block.ErrFutureFormat) {
				return fmt.Errorf("share %q: %w", share.Name, err)
			}
			// A binding that names no configured store is the one failure an
			// operator can act on directly, and the construction error wraps it
			// four levels deep. Say which share and which reference, matching
			// how an unknown metadata store is reported. Matched on the
			// resolver's own sentinel so no other failure is relabelled, and
			// only once the add has failed, so the format sentinels above keep
			// stopping the boot ahead of it.
			if errors.Is(err, models.ErrStoreNotFound) {
				logger.Warn("Share references unknown block store",
					"share", share.Name,
					"block_store_id", share.BlockStoreID)
				rt.markShareSkipped(share.Name, "block store "+share.BlockStoreID+" is not configured")
				continue
			}
			logger.Warn("Failed to add share to runtime",
				"share", share.Name,
				"error", err)
			rt.markShareSkipped(share.Name, err.Error())
			continue
		}

		logger.Info("Loaded share", "name", share.Name, "metadata_store", shareConfig.MetadataStore)
	}

	return nil
}

// buildShareConfig assembles the runtime ShareConfig for a persisted share row,
// resolving its metadata store, NFS export options, and netgroup. It returns
// (nil, nil) when the share references an unknown metadata store so the caller
// can skip it (already logged). Extracted so both startup load and live
// block-store rebind (#1532) build an identical config from the same row.
//
// Any other failure to look up the metadata store, a cancelled context among
// them, is an error: read as an unknown store, it would let a purge skip the
// share's records and delete the rest.
func buildShareConfig(ctx context.Context, s store.Store, share *models.Share) (*ShareConfig, error) {
	// Try by ID first, fall back to name lookup.
	metaStoreCfg, err := s.GetMetadataStoreByID(ctx, share.MetadataStoreID)
	if errors.Is(err, models.ErrStoreNotFound) {
		metaStoreCfg, err = s.GetMetadataStore(ctx, share.MetadataStoreID)
	}
	if errors.Is(err, models.ErrStoreNotFound) {
		logger.Warn("Share references unknown metadata store",
			"share", share.Name,
			"metadata_store_id", share.MetadataStoreID)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("look up the metadata store of share %q: %w", share.Name, err)
	}

	nfsOpts := models.DefaultNFSExportOptions()
	// A share's NFS options carry its squash and auth-flavor policy, so a lookup
	// or parse that fails is an error rather than the defaults, which would
	// serve the share without them.
	nfsCfg, err := s.GetShareAdapterConfig(ctx, share.ID, "nfs")
	if err != nil {
		return nil, fmt.Errorf("look up the NFS options of share %q: %w", share.Name, err)
	}
	if nfsCfg != nil {
		if err := nfsCfg.ParseConfig(&nfsOpts); err != nil {
			return nil, fmt.Errorf("parse the NFS options of share %q: %w", share.Name, err)
		}
	}

	var netgroupName string
	if nfsOpts.NetgroupID != nil && *nfsOpts.NetgroupID != "" {
		if ns, ok := s.(store.NetgroupStore); ok {
			ng, ngErr := ns.GetNetgroupByID(ctx, *nfsOpts.NetgroupID)
			if ngErr == nil {
				netgroupName = ng.Name
			} else {
				logger.Warn("Share references unknown netgroup",
					"share", share.Name,
					"netgroup_id", *nfsOpts.NetgroupID)
			}
		}
	}

	// Re-apply the persisted root owner so a restart does not reset the share
	// root to UID/GID 0: prepareShare defaults a nil RootAttr to the zero value
	// and the metadata stores force-sync existing root ownership to it (#1534).
	var rootAttr *metadata.FileAttr
	if share.OwnerUID != nil {
		uid := *share.OwnerUID
		gid := uid // default the group to the owner, never root
		if share.OwnerGID != nil {
			gid = *share.OwnerGID
		}
		rootAttr = &metadata.FileAttr{UID: uid, GID: gid}
	}

	return &ShareConfig{
		Name:                             share.Name,
		MetadataStore:                    metaStoreCfg.Name,
		RootAttr:                         rootAttr,
		ReadOnly:                         share.ReadOnly,
		Enabled:                          share.Enabled, // propagate DB Enabled flag so the runtime's enabled checks read the correct value.
		EncryptData:                      share.EncryptData,
		AclFlagInheritedCanonicalization: share.AclFlagInheritedCanonicalization,
		AccessBasedEnumeration:           share.AccessBasedEnumeration,
		ChangeNotifyDisabled:             share.ChangeNotifyDisabled,
		StreamsDisabled:                  share.StreamsDisabled,
		ContinuousAvailability:           share.ContinuousAvailability,
		AllowMFsymlink:                   share.AllowMFsymlink,
		TrashEnabled:                     share.TrashEnabled,
		TrashRetentionDays:               share.TrashRetentionDays,
		TrashRestrictToAdmin:             share.TrashRestrictToAdmin,
		TrashMaxBytes:                    share.TrashMaxBytes,
		TrashExcludePatterns:             share.GetTrashExcludePatterns(),
		DefaultPermission:                share.DefaultPermission,
		Squash:                           nfsOpts.GetSquashMode(),
		AnonymousUID:                     nfsOpts.GetAnonymousUID(),
		AnonymousGID:                     nfsOpts.GetAnonymousGID(),
		AllowAuthSys:                     nfsOpts.AllowAuthSys,
		AllowAuthSysSet:                  true,
		RequireKerberos:                  nfsOpts.RequireKerberos,
		MinKerberosLevel:                 nfsOpts.MinKerberosLevel,
		DisableReaddirplus:               nfsOpts.DisableReaddirplus,
		NetgroupName:                     netgroupName,
		BlockedOperations:                share.GetBlockedOps(),
		RetentionPolicy:                  share.GetRetentionPolicy(),
		RetentionTTL:                     share.GetRetentionTTL(),
		JournalSize:                      share.JournalSize,
		ReadBufferSize:                   share.ReadBufferSize,
		QuotaBytes:                       share.QuotaBytes,
		BlockStoreID:                     share.BlockStoreID,
		CommitAck:                        share.CommitAck,
		RelaxedMetadataCommit:            share.RelaxedMetadataCommit,
	}, nil
}
