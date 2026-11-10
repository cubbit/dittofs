package lifecycle

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/cubbit/dittofs/internal/logger"
	"github.com/cubbit/dittofs/pkg/auth/sid"
)

const DefaultShutdownTimeout = 30 * time.Second

// AuxiliaryServer is implemented by HTTP servers (API, Metrics).
type AuxiliaryServer interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Port() int
}

type MetadataFlusher interface {
	FlushAllPendingWritesForShutdown(timeout time.Duration) (int, error)
}

type StoreCloser interface {
	CloseMetadataStores()
}

// SnapshotDrainer cancels all in-flight snapshot orchestration goroutines
// and waits (bounded by ctx) for them to drain. Threaded through Serve so
// snapshot orchestration cannot use-after-close the metadata stores or
// control-plane DB during graceful shutdown — without this hook the
// normal server path (signal -> ctx cancel -> lifecycle.shutdown) would
// call CloseMetadataStores directly while snapshot
// goroutines still hold references to the closing stores.
type SnapshotDrainer interface {
	ShutdownSnapshots(ctx context.Context)
}

// BlockStoreCloser closes every share's block store, stopping and draining the
// data-plane work that writes through the metadata stores.
//
// Threaded through Serve so the normal server shutdown path (signal -> ctx
// cancel -> lifecycle.shutdown) quiesces the data plane BEFORE
// CloseMetadataStores. A share's carve dispatcher ticks on its own interval and
// commits FileChunk manifest rows through that share's metadata store; closing
// the block store is what stops it, drains its uploads and joins its
// goroutines. Closing the DB underneath a live dispatcher instead fails every
// commit with "sql: database is closed" and leaves the chunks it was carving
// local and unmirrored.
//
// Called BEFORE the stores close. Pass nil to skip (tests with no data
// plane). The closes run concurrently, so ctx is a single wall-clock budget
// they all share rather than one divided between them. It bounds the WAIT, not
// a close: on expiry the step returns and lets the metadata stores close while
// the stragglers are still running. See the decision marker on
// shares.Service.CloseBlockStores for what that costs and why it beats waiting.
type BlockStoreCloser interface {
	CloseBlockStores(ctx context.Context)
}

// BackgroundWorkerStopper stops the runtime's background workers that are not
// owned by the snapshot or block-store machinery — the recycle-bin
// reaper and any async block GC run in flight. Both write through the metadata
// stores, so they are signalled first, before any teardown step runs.
//
// It joins the block GC run and only signals the reaper — see
// Runtime.StopBackgroundWorkers for why the two differ and what the signalled
// half still permits. The join is bounded, so this step can cost the shutdown
// time it does not get back.
type BackgroundWorkerStopper interface {
	StopBackgroundWorkers()
}

// MachineSIDStore provides access to the SettingsStore for machine SID
// persistence. The lifecycle service uses this to load or generate the
// machine SID on first boot, ensuring consistent identity mapping across
// restarts. When the SID is not operator-pinned, a read or write failure
// aborts startup.
type MachineSIDStore interface {
	GetSetting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
}

// Service orchestrates server startup and graceful shutdown.
type Service struct {
	shutdownTimeout time.Duration
	apiServer       AuxiliaryServer
	serveOnce       sync.Once
	served          bool

	// startupDone is closed by serve() once startup has completed and it is
	// blocked waiting for the shutdown signal. It is NOT closed when startup
	// fails, because at that point Serve has already returned the error — a
	// waiter watches both, and the error is the more informative of the two.
	startupDone chan struct{}

	// sidMapper is the machine SID mapper, initialized on first Serve().
	sidMapper *sid.SIDMapper

	// pinnedMachineSID, when non-empty, is an operator-supplied machine SID
	// (config/env) that initMachineSID seeds in preference to any random
	// generation. Pinning the machine SID lets multiple cluster nodes derive
	// IDENTICAL local/algorithmic SIDs from the same Unix UID/GID — see
	// pkg/auth/sid/mapper.go for the LOCKED RID formula.
	pinnedMachineSID string
}

func New(shutdownTimeout time.Duration) *Service {
	if shutdownTimeout == 0 {
		shutdownTimeout = DefaultShutdownTimeout
	}
	return &Service{
		shutdownTimeout: shutdownTimeout,
		startupDone:     make(chan struct{}),
	}
}

func (s *Service) SetShutdownTimeout(d time.Duration) {
	if d == 0 {
		d = DefaultShutdownTimeout
	}
	s.shutdownTimeout = d
}

// SIDMapper returns the machine SID mapper initialized during Serve().
// Returns nil if Serve() has not been called yet. Reading it from a goroutine
// that runs CONCURRENTLY with Serve is a race, and waiting for startup is what
// StartupDone is for.
func (s *Service) SIDMapper() *sid.SIDMapper {
	return s.sidMapper
}

// StartupDone returns a channel closed once Serve has finished starting every
// component and is waiting for the shutdown signal. It does not close when
// startup fails, so a caller waits on it and on Serve's own return together.
func (s *Service) StartupDone() <-chan struct{} {
	return s.startupDone
}

// SetPinnedMachineSID records an operator-supplied machine SID to seed during
// Serve(). Must be called before Serve(). An empty string is a no-op (the
// machine SID is then loaded-or-generated as before). The value is validated
// and applied by initMachineSID.
func (s *Service) SetPinnedMachineSID(machineSID string) {
	if s.served {
		panic("cannot set pinned machine SID after Serve() has been called")
	}
	s.pinnedMachineSID = machineSID
}

// initMachineSID resolves the machine SID with the following precedence:
//
//  1. An operator-pinned machine SID (SetPinnedMachineSID, from config/env)
//     is validated and applied; it is persisted to the settings store so it
//     is authoritative and survives a later boot without the pin. Pinning the
//     same SID on every node makes their local/algorithmic SIDs identical —
//     required for cross-node identity parity (see pkg/auth/sid/mapper.go).
//  2. Otherwise the stored SID (first boot generated + persisted) is loaded,
//     keeping mapping stable across restarts.
//  3. Otherwise a new random SID is generated and persisted.
func (s *Service) initMachineSID(ctx context.Context, store MachineSIDStore) error {
	const machineSIDKey = "machine_sid"

	// Operator pin takes precedence and is honored even without a settings
	// store (ephemeral / test runtimes) so two nodes still derive identical
	// SIDs from the same config.
	if s.pinnedMachineSID != "" {
		mapper, err := sid.NewSIDMapperFromString(s.pinnedMachineSID)
		if err != nil {
			// A pin is an explicit operator intent for cross-node parity.
			// Silently falling back to a random SID would diverge this node's
			// local UID->SID encoding from the rest of the cluster, so abort
			// instead. The config layer normally rejects this earlier
			// (IdentityConfig.Validate); this guards programmatic misuse.
			return fmt.Errorf("invalid pinned machine SID %q: %w", s.pinnedMachineSID, err)
		}
		s.sidMapper = mapper
		if store != nil {
			if prior, _ := store.GetSetting(ctx, machineSIDKey); prior != s.pinnedMachineSID {
				if prior != "" {
					logger.Warn("Pinned machine SID overrides a different stored value; foreign-SID mappings keyed on the old domain remain unaffected, but local UID->SID encoding changes",
						"pinned", s.pinnedMachineSID, "stored", prior)
				}
				if err := store.SetSetting(ctx, machineSIDKey, s.pinnedMachineSID); err != nil {
					logger.Error("Failed to persist pinned machine SID", "sid", s.pinnedMachineSID, "error", err)
				}
			}
		}
		logger.Info("Using pinned machine SID", "sid", s.pinnedMachineSID)
		return nil
	}

	if store == nil {
		logger.Warn("No settings store available, generating ephemeral machine SID")
		s.sidMapper = sid.GenerateMachineSID()
		logger.Info("Generated ephemeral machine SID", "sid", s.sidMapper.MachineSIDString())
		return nil
	}

	stored, err := store.GetSetting(ctx, machineSIDKey)
	if err != nil {
		// A read failure is not an empty store: falling through would generate
		// a fresh SID over the stored one, rebinding every local UID->SID
		// encoding and orphaning the descriptors written against the old machine.
		return fmt.Errorf("failed to read machine SID: %w", err)
	}

	if stored != "" {
		// Load existing machine SID
		mapper, err := sid.NewSIDMapperFromString(stored)
		if err != nil {
			logger.Error("Invalid stored machine SID, generating new one",
				"stored", stored, "error", err)
		} else {
			s.sidMapper = mapper
			logger.Info("Loaded machine SID from store", "sid", stored)
			return nil
		}
	}

	// First boot: generate, persist, then publish. An in-memory-only SID is
	// replaced by a different random one on the next boot, leaving every
	// descriptor written in between naming a machine that no longer exists.
	mapper := sid.GenerateMachineSID()
	sidStr := mapper.MachineSIDString()

	if err := store.SetSetting(ctx, machineSIDKey, sidStr); err != nil {
		return fmt.Errorf("failed to persist machine SID: %w", err)
	}

	s.sidMapper = mapper
	logger.Info("Generated and persisted machine SID", "sid", sidStr)
	return nil
}

// SetAPIServer must be called before Serve().
func (s *Service) SetAPIServer(server AuxiliaryServer) {
	if s.served {
		panic("cannot set API server after Serve() has been called")
	}
	s.apiServer = server
	if server != nil {
		logger.Info("API server registered", "port", server.Port())
	}
}

// Deps are the collaborators Serve drives through startup and shutdown. Every
// field is optional; a nil field skips the step it owns.
type Deps struct {
	// MetadataFlusher drains pending metadata writes during shutdown.
	MetadataFlusher MetadataFlusher

	// StoreCloser closes the metadata stores, last of the data-plane steps.
	StoreCloser StoreCloser

	// MachineSIDStore loads or generates the machine SID used for Windows
	// identity mapping. Nil yields an ephemeral SID (testing).
	MachineSIDStore MachineSIDStore

	// SnapshotDrainer cancels and drains in-flight snapshot orchestration
	// goroutines BEFORE CloseMetadataStores — otherwise those
	// goroutines would race a closing metadata store / control-plane DB.
	SnapshotDrainer SnapshotDrainer

	// BlockStoreCloser quiesces the per-share data plane before the metadata
	// stores close.
	BlockStoreCloser BlockStoreCloser

	// BackgroundWorkerStopper is invoked as the FIRST shutdown step, so the
	// workers it signals have the whole teardown in which to notice.
	BackgroundWorkerStopper BackgroundWorkerStopper
}

// Serve starts all components and blocks until shutdown.
func (s *Service) Serve(ctx context.Context, deps Deps) error {
	var err error

	s.serveOnce.Do(func() {
		s.served = true
		err = s.serve(ctx, deps)
	})

	return err
}

func (s *Service) serve(ctx context.Context, deps Deps) error {
	logger.Info("Starting DittoFS runtime")

	// Initialize the machine SID before startup completes, so every reader
	// that waits on StartupDone sees it.
	if err := s.initMachineSID(ctx, deps.MachineSIDStore); err != nil {
		return fmt.Errorf("failed to initialize machine SID: %w", err)
	}

	apiErrChan := make(chan error, 1)
	if s.apiServer != nil {
		go func() {
			if err := s.apiServer.Start(ctx); err != nil {
				logger.Error("API server error", "error", err)
				apiErrChan <- err
			}
		}()
	}

	// Startup is complete: every step that can still fail has run, and the
	// only thing left is to wait. A cancellation arriving from here on is a
	// shutdown signal rather than a startup abort, which is the distinction a
	// waiter needs before it cancels.
	close(s.startupDone)

	var shutdownErr error
	select {
	case <-ctx.Done():
		logger.Info("Shutdown signal received", "reason", ctx.Err())
		shutdownErr = ctx.Err()
	case err := <-apiErrChan:
		logger.Error("API server failed, initiating shutdown", "error", err)
		shutdownErr = fmt.Errorf("API server error: %w", err)
	}

	s.shutdown(deps)

	logger.Info("DittoFS runtime stopped")
	return shutdownErr
}

func (s *Service) shutdown(deps Deps) {
	// See BackgroundWorkerStopper for why this is first and why it does not wait.
	if deps.BackgroundWorkerStopper != nil {
		deps.BackgroundWorkerStopper.StopBackgroundWorkers()
	}

	// Drain in-flight snapshot orchestration goroutines BEFORE closing
	// metadata stores — those goroutines hold
	// references to both. ShutdownSnapshots cancels runtimeCtx (every
	// per-snap ctx derives from it) and waits, bounded by the shutdown
	// timeout. Orphans after the timeout will still exit on their own
	// since runtimeCtx is already cancelled; we just may proceed before
	// every wg.Done fires.
	if deps.SnapshotDrainer != nil {
		drainCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
		deps.SnapshotDrainer.ShutdownSnapshots(drainCtx)
		cancel()
	}

	if deps.MetadataFlusher != nil {
		flushed, err := deps.MetadataFlusher.FlushAllPendingWritesForShutdown(s.shutdownTimeout)
		if err != nil {
			logger.Warn("Error flushing pending writes", "error", err, "flushed", flushed)
		} else if flushed > 0 {
			logger.Info("Flushed pending metadata writes", "count", flushed)
		}
	}

	// Quiesce the data plane BEFORE the stores it writes through close. See
	// BlockStoreCloser. Bounded so one wedged share cannot cost every share its
	// metadata-store close: the process self-exits on its own deadline, and
	// reaching that means nothing below runs at all.
	if deps.BlockStoreCloser != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
		deps.BlockStoreCloser.CloseBlockStores(closeCtx)
		cancel()
	}

	if deps.StoreCloser != nil {
		deps.StoreCloser.CloseMetadataStores()
	}

	if s.apiServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
		defer cancel()
		if err := s.apiServer.Stop(ctx); err != nil {
			logger.Error("API server shutdown error", "error", err)
		}
	}
}
