# DittoFS Architecture

This document provides a deep dive into DittoFS's architecture, design patterns, and internal implementation.

> **On this branch the v1 protocol adapters are deleted.** `pkg/adapter`,
> `internal/adapter` and the runtime's adapter half are gone and no file protocol
> is served, so the diagram, the runtime's sub-services, the adapter and
> Kerberos sections and the directory map below still describe what this branch
> removed. RFC 20's v2 adapters replace them; see
> [RFC 20](rfcs/rfc-20-adapters.md).

**Storage terms used throughout** (see the [Glossary](../guide/glossary.md) for protocol and security terms):

- **CAS** (Content-Addressed Storage) — blocks are named by the hash of their contents rather than by location, so identical data is stored once and deduplicated automatically.
- **FastCDC** — a content-defined chunking algorithm that splits file data at content-based boundaries, so small edits only re-chunk the affected region ([FastCDC paper](https://www.usenix.org/conference/atc16/technical-sessions/presentation/xia)).
- **BLAKE3** — the fast cryptographic hash used to address CAS blocks and verify them end-to-end ([BLAKE3 spec](https://github.com/BLAKE3-team/BLAKE3-specs)).

## Table of Contents

- [Core Abstraction Layers](#core-abstraction-layers)
- [Data path](#data-path)
- [Control Plane Pattern](#control-plane-pattern)
- [Service Layer](#service-layer)
- [Built-In and Custom Backends](#built-in-and-custom-backends)
- [Directory Structure](#directory-structure)
- [Multi-Node Metadata](#multi-node-metadata)
- [Durable Handle State Flow](#durable-handle-state-flow)
- [Migration & Block-Layout Routing](#migration--block-layout-routing)

## Core Abstraction Layers

DittoFS uses a **Runtime-centric architecture** where the Runtime is the single entrypoint for all operations. This design ensures that both persistent store and in-memory state stay synchronized.

```
┌─────────────────────────────────────────┐
│         Protocol Adapters               │
│            (NFS, SMB)                   │
│       pkg/adapter/{nfs,smb}/            │
└───────────────┬─────────────────────────┘
                │ (no file operations yet: see Data path)
                ▼
┌─────────────────────────────────────────┐
│              Runtime                    │
│   (Composition layer + sub-services)    │
│   pkg/controlplane/runtime/             │
│                                         │
│  ┌──────────┐ ┌────────┐ ┌──────────┐  │
│  │ adapters │ │ stores │ │  shares  │  │
│  │lifecycle │ │registry│ │ registry │  │
│  └──────────┘ └────────┘ │ + remote │  │
│  ┌──────────┐ ┌────────┐ └──────────┘  │
│  │  mounts  │ │lifecycl│ ┌──────────┐  │
│  │ tracking │ │  serve  │ │ identity │  │
│  └──────────┘ └────────┘ │ mapping  │  │
│                           └──────────┘  │
│  ┌────────────┐  ┌───────────────────┐  │
│  │   Store    │  │   Auth Layer      │  │
│  │ (Persist)  │  │  pkg/auth/        │  │
│  │ 9 sub-ifs  │  │ kerberos,         │  │
│  │            │  │ sid               │  │
│  └────────────┘  └───────────────────┘  │
└───────┬───────────────────┬─────────────┘
        │                   │
        ▼                   ▼
┌────────────────┐  ┌──────────────────────┐
│   Metadata     │  │ Per-Share BlockStore │
│     Stores     │  │  pkg/engine, journal │
│                │  │                      │
│  - BadgerDB    │  │  ┌──────────────┐    │
│    (on disk or │  │  │   Journal    │    │
│     in memory) │  │  │  (on disk)   │    │
│                │  │  └──────┬───────┘    │
│                │  │         │            │
│                │  │  ┌──────▼───────┐    │
│                │  │  │   Syncer     │    │
│                │  │  │ (async xfer) │    │
│                │  │  └──────┬───────┘    │
│                │  │         │            │
│                │  │  ┌──────▼────────┐   │
│                │  │  │  Block Store  │   │
│                │  │  │  s3 / memory  │   │
│                │  │  │ (ref counted) │   │
│                │  │  └───────────────┘   │
└────────────────┘  └──────────────────────┘
```

### Key Interfaces

**1. Runtime** (`pkg/controlplane/runtime/`)
- **Single entrypoint for all operations** - both API handlers and internal code
- Updates both persistent store AND in-memory state together
- Thin composition layer delegating to 6 focused sub-services:
  - `adapters/`: Protocol adapter lifecycle management (create, start, stop, delete)
  - `stores/`: Metadata store registry
  - `shares/`: Share registration and configuration; owns per-share `*engine.BlockStore` instances
  - `mounts/`: Unified mount tracking across protocols
  - `lifecycle/`: Server startup/shutdown orchestration
  - `identity/`: Share-level identity mapping
- Key methods:
  - `Serve(ctx)`: Starts all adapters and servers, blocks until shutdown
  - `CreateAdapter(ctx, cfg)`: Saves to store AND starts immediately
  - `DeleteAdapter(ctx, type)`: Stops adapter AND removes from store
  - `AddAdapter(adapter)`: Direct adapter injection (for testing)

**2. Control Plane Store** (`pkg/controlplane/store/`)
- Persistent configuration (users, groups, permissions, adapters)
- Decomposed into 9 sub-interfaces: `UserStore`, `GroupStore`, `ShareStore`, `PermissionStore`, `MetadataStoreConfigStore`, `BlockStoreConfigStore`, `AdapterStore`, `SettingsStore`, `GuestStore`
- Composite `Store` interface embeds all sub-interfaces
- API handlers accept narrowest interface needed
- SQLite (single-node) or PostgreSQL (distributed)

**3. Adapter Interface** (`pkg/adapter/adapter.go`)
- Each protocol implements the `Adapter` interface (`Serve`, `Stop`, `SetRuntime`, `Protocol`, `Port`, `Healthcheck`)
- Adapters receive a Runtime reference to access services (`SetRuntime(rt any)`, type-asserted to `*runtime.Runtime` to avoid an import cycle)
- `BaseAdapter` provides the shared TCP lifecycle: accept loop with backoff, graceful drain, forced close, and listener-readiness signalling
- Lifecycle: `SetRuntime() -> Serve() -> Stop()`
- Multiple adapters can share the same runtime
- Thread-safe, supports graceful shutdown

**4. Auth** (`pkg/auth/`)
- Shared authentication and identity primitives used across protocols
- Sub-packages:
  - `kerberos/`: Kerberos `Provider` — keytab and krb5.conf state with hot-reload
  - `sid/`: Windows SID types, encoding, and cross-protocol mapping

**5. Block storage** (`pkg/engine/`, `pkg/journal/`)
- `pkg/engine/`: the engine (RFC 8) over `pkg/journal/`, the journal (RFC 1). The node builds it and `vfs.Service` over it; adapters reach it only through `vfs.Service` (see [Data path](#data-path)).

**6. Metadata store** (`pkg/metadata/`, [RFC 16](rfcs/rfc-16-metadata-store.md))
- `kvstore/`: the store over a `kv.KV` — the records, their keys and codecs,
  and the views the node and `vfs.Service` read and write through
- `kv/`: the key-value contract and its backends, `badgerkv/` (embedded) and
  `fdbkv/` (FoundationDB); `kv/kvtest/` is the conformance suite a backend passes
- `kvstore/kvstoretest/`: the store's conformance suite
- `entity/`: the record types; `errors/`: the store's error codes
- File handles are opaque identifiers

## Data path

`node.New` (`internal/node/node.go`) builds the engine and `vfs.Service` over
it, and `vfs.Service`'s Read, Write and Commit read and write file content
through the engine (`internal/vfs/data.go`). No protocol serves that path yet:
the RFC 20 NFS and SMB adapters are built on `vfs.Service`, but their operation
tables are empty, so every NFS procedure answers `PROC_UNAVAIL` and SMB has no
file operations until RFC 20's file operations land.

The journal devices come from `bootstrap.journals`. `blockstore.journal.path`
is still parsed and validated, but the node reads it only at preflight, to
detect a v1 state layout (`probeV1State`, `internal/node/preflight.go`); shares
keep no state directory beneath it.

The engine (`pkg/engine`, [RFC 8](rfcs/rfc-8-engine.md)) and the journal
(`pkg/journal`, [RFC 1](rfcs/rfc-1-journal.md)) are built and tested on
their own, behind the `engine_conformance` and `journal_conformance` tags.

## Share Snapshots

Share snapshots are point-in-time, reference-based protection for a
share's content. The subsystem produces three artifacts per snapshot
on local disk and one row in the control-plane database; it does not
copy any block data. See [SNAPSHOTS.md](../guide/snapshots.md) for the
operator-facing runbook; this section describes the architectural
layout and the orchestration flows.

### Subsystem layout

| Location | Role |
| --- | --- |
| `pkg/snapshot/` | Verify gate, hash-manifest read/write, helper types. |
| `pkg/controlplane/runtime/snapshot.go` | `Runtime.CreateSnapshot`, `WaitForSnapshot`, `RestoreSnapshot`, `GetSnapshot`, `ListSnapshots`, `DeleteSnapshot`. Composition over the metadata store, block store, and snapshot store. |
| `pkg/controlplane/runtime/snapshot_hold.go` | `SnapshotHoldProvider` — per-share delete lock + manifest-on-disk hold surface for GC. |
| `pkg/controlplane/models/snapshot.go` | `Snapshot` GORM model; `SnapshotDir`, `ManifestPath`, `MetadataDumpPath` path helpers. |
| `pkg/controlplane/store/snapshots.go` | `SnapshotStore` CRUD (`GetSnapshot`, `ListSnapshots`, `DeleteSnapshot`). |
| `pkg/controlplane/api/dto/snapshot.go` | Neutral wire DTOs imported by both the REST handler and the apiclient. No GORM types cross the wire. |
| `internal/controlplane/api/handlers/snapshot.go` | Five REST handlers (`Create`, `List`, `Get`, `Delete`, `Restore`), the narrow `SnapshotRuntime` interface (testability seam), and the single `mapSnapshotError` sentinel-to-HTTP table. |
| `pkg/apiclient/snapshots.go` | Typed Go client (6 methods) re-exporting the wire DTOs as type aliases of `dto.Snapshot`. |
| `cmd/dfsctl/commands/share/snapshot/` | Five cobra leaf commands matching the REST surface (`create`, `list`, `show`, `delete`, `restore`). |

### On-disk artifacts

Every snapshot owns a directory under the share's local store:

```
<localStoreDir>/snapshots/<share>/<snap-id>/
  ├─ metadata.dump          ← engine-native metadata serialization
  └─ manifest.json          ← BLAKE3 hashes of every CAS block the share references
```

`SnapshotDir(localStoreDir)`, `ManifestPath(localStoreDir)`, and
`MetadataDumpPath(localStoreDir)` on the `Snapshot` model compute the
canonical paths. Atomic write is via `temp + rename` so a partial
manifest never surfaces to the GC enumeration step. The manifest
file's existence is the GC hold; there is no separate hold record.

### Create orchestration

```
CreateSnapshot ─→ persist Snapshot row (state=creating)
              ─→ DrainAllUploads (always — the dump must carry every block locator)
              ─→ Dump metadata to metadata.dump
              ─→ Build hash manifest from CAS
              ─→ Verify gate: DrainAllUploads + VerifyRemoteDurability
                 (both skipped if NoVerify, concurrency = 16)
              ─→ Update row state=ready (or failed) + remote_durable flag
```

`Runtime.CreateSnapshot` returns the new snapshot ID immediately and
runs the orchestration in a background goroutine. The REST handler
returns `202 Accepted` with a `Location` header pointing at the
record; callers poll `GET /snapshots/{id}` until `state != "creating"`.
The CLI's `WaitForSnapshot` does that polling on the operator's
behalf.

`NoVerify=true` (CLI `--no-verify`) skips the verify gate — its upload
drain and the HEAD-probe phase. It does not skip the drain that runs
before the metadata dump, so a block store that cannot be reached fails
snapshot creation either way. The snapshot still completes with
`remote_durable=false`. Restore of a non-durable snapshot then
requires the explicit `AllowNonDurable` flag (CLI `--force`).

### Restore orchestration

```
RestoreSnapshot ─→ Pre-flight: refuse if share enabled
                ─→ Verify source snapshot's remote durability
                   (skipped if AllowNonDurable)
                ─→ Pre-restore safety snapshot (ID returned to caller)
                ─→ Close metadata store
                ─→ Reset (via Resetable interface)
                ─→ Restore from metadata.dump
                ─→ HashSetFromMetadataStore walk
                ─→ Post-restore block verify
```

`Runtime.RestoreSnapshot` returns `(safetySnapshotID, err)`. The
safety snap ID is set as soon as step 3 succeeds, even if a later
step fails — callers (REST + CLI) surface the ID to the operator so
the rollback path is always available without a separate
`ListSnapshots` filter. On precheck / pre-verify failure (before
step 3) the safety ID is the empty string.

### Per-share delete lock

`SnapshotHoldProvider.AcquireDeleteLock(share)` returns a release
function around a per-share `*sync.RWMutex`. The same mutex
serializes `CreateSnapshot`, `RestoreSnapshot`, and
`DeleteSnapshot` on the same share so that:

- Two concurrent `delete` calls on different snapshots of the same
  share cannot race the per-snapshot directory wipe against each
  other.
- A `delete` cannot race a `create` whose manifest write would
  appear in the snapshots directory mid-sweep.
- A `restore` cannot race a `delete` of the safety snap it is about
  to create.

`Runtime.DeleteSnapshot` is the canonical entry point — handlers
never reach into `r.store.DeleteSnapshot` directly. The wrapper owns
the lock acquisition, the database row delete, the on-disk
directory wipe, and the lock release.

### HTTP surface

Five REST endpoints under `/api/v1/shares/{name}/snapshots` (admin
only, inherits the existing `RequireAdmin` middleware):

| Method | Path | Result |
| --- | --- | --- |
| `POST` | `/` | 202 Accepted + `Location` header |
| `GET` | `/` | 200 OK + JSON array (empty: `[]`, not `null`) |
| `GET` | `/{id}` | 200 OK + full record |
| `DELETE` | `/{id}` | 204 No Content |
| `POST` | `/{id}/restore` | 200 OK + `{snapshot_id, safety_snapshot_id, share}` |

The single `mapSnapshotError` helper handles the 14 typed sentinels
that can cross the boundary (12 snapshot sentinels + share-not-found
- nil-guard). The mapping table lives in the handler file as the
sole source of truth; future sentinels add a single case.

The Restore handler wraps `r.Context()` with
`context.WithTimeout(ctx, cfg.Snapshot.restore_http_timeout)`
(default 30 minutes) to bound runaway restores. The apiclient
mirrors the timeout on the client's `http.Client` for the restore
call only (`WithRestoreTimeout`).

For the full operator runbook see
[SNAPSHOTS.md](../guide/snapshots.md).

### Object ownership: a client-supplied identifier is not authorization

Every protocol here lets a client name a server-side object by an identifier the
server issued earlier — an SMB `TreeID` or `FileId`, an SMB `AsyncId`, an NFSv4
stateid. **An operation must verify that the named object belongs to the
requesting session or client before acting on it, and especially before adopting
its identity.** Resolving the identifier proves the object exists; it says
nothing about who is entitled to it.

The checks live at choke points rather than in each handler, so an operation
added later inherits them:

- **SMB tree-scoped commands** — `prepareDispatch`'s `NeedsTree` branch
  (`internal/adapter/smb/response.go`) rejects a request whose
  `tree.SessionID` differs from the header's. It is the only dispatch gate,
  so TREE_DISCONNECT, CREATE and every tree-scoped command are covered once.
- **SMB file handles** — `primeAuthContextFromOpenFile`
  (`internal/adapter/smb/handlers/auth_helper.go`) refuses with
  `STATUS_FILE_CLOSED` unless `openFileBelongsToRequest` matches both the tree
  and the session. This matters more than a plain existence check because the
  function then *prefills the auth context from the located handle*: without
  the guard, a request naming another session's `FileId` would execute as that
  handle's user.
- **SMB parked requests** — `pendingRegistry.unregisterByAsyncIDOn`
  (`internal/adapter/smb/pending/pending_registry.go`) scopes an `AsyncId`
  lookup to the connection that parked it, as the `MessageID` lookups already
  did, so a CANCEL cannot retire another connection's request.
- **NFSv4 stateids** — `ValidateStateid` takes the caller's client ID and
  compares it through `checkStateidOwner`
  (`internal/adapter/nfs/v4/state/stateid.go`) for all three
  stateid families, so the I/O operations are covered at one point. The
  state-changing operations do not go through `ValidateStateid`, and reach the
  same comparison two ways: `CloseFile`, `ConfirmOpen`, `ConfirmOpenV41`,
  `DowngradeOpen`, `ReturnDelegation` and the three TEST_STATEID probes call
  `checkStateidOwner` themselves, while the byte-range lock paths
  (`LockNew`, `LockExisting`, `UnlockFile`) go through
  `revalidateLockStateLocked`, which additionally compares the lock state and
  lock owner **by pointer identity** — a stateid `other` can be reissued after
  the original state is freed, so re-finding an entry under that key is not
  proof it is the same entry.

Two exceptions are deliberate, and the rule is not true without them:

1. **NFSv4.0 carries no trusted client identity** on the operations above, so
   the caller's client ID is zero there and `checkStateidOwner` skips the
   comparison by design. The binding is real for v4.1 and later only; on v4.0
   the unguessability of the stateid is all that stands behind it.
2. **Per-handler `GetTree(ctx.TreeID)` lookups stay existence-only**, because
   the dispatcher already bound that tree to the session before the handler
   ran. A `ponytail:` comment at those sites names the ceiling.

Unguessability is not the guarantee. A stateid's `other` and an SMB `FileId`
each carry 64 bits from `crypto/rand`, which makes them infeasible to forge but
does nothing once one has been observed — so these checks are defence in depth
layered under the identifier's entropy, not a substitute for it.

**Extending this:** an operation that resolves a client-supplied identifier
routes through the existing choke point instead of adding a comparison of its
own, and ships with a test that fails when the guard is removed. A guard that
has never refused anything in a test is unverified.

## Control Plane Pattern

The Control Plane is the central management component enabling flexible, multi-share configurations.

### How It Works

1. **Named Store Creation**: Stores are created with unique names (e.g., "fast-memory", "s3-archive")
2. **Share-to-Store Mapping**: Each share references metadata and block stores by name
3. **Handle Identity**: File handles encode both the share ID and file-specific data
4. **Store Resolution**: When handling operations, the runtime decodes the handle to identify the share, then routes to the correct stores

### Configuring shares

Each share names its metadata store and its block store, and gets its own
`*engine.Store` and journal directory. The CLI's share and store commands are
unavailable until they are rebuilt on the v2 management API.

### Benefits

- **Per-share isolation**: Each share gets its own BlockStore and its own journal directory
- **Resource Efficiency**: Block stores are shared (ref counted) when multiple shares reference the same config
- **Flexible Topologies**: Different shares can target different block stores
- **Future Multi-Tenancy**: Foundation for per-tenant store isolation

## Service Layer

The service layer provides business logic and coordination between stores.

### Write Coordination Pattern

`vfs.Service.Write` admits the caller and judges the file (type, read-only
share, rights, open state) and reserves its quota before the engine stages a
byte, so a write refused while it is judged or reserved changes no metadata or
content (a conflicting grant may already be recalled); one the engine refuses
may already have cleared setuid and setgid; a zero-count write returns once
judged. No protocol WRITE handler calls it yet.

## Built-In and Custom Backends

### Using Built-In Backends

No custom code required - configure via CLI:

```bash
# Create a share referencing the stores
./dfsctl share create --name /export --metadata default-meta --block-store default-blocks
```

### Implementing Custom Store Backends

See [docs/IMPLEMENTING_STORES.md](implementing-stores.md) for detailed implementation guides for:
- **Metadata Store**: Implement `pkg/metadata/kv.KV` and pass `kv/kvtest`

## Directory Structure

```
dittofs/
├── cmd/
│   ├── dfs/                      # Server CLI binary
│   │   ├── main.go               # Entry point
│   │   └── commands/             # Cobra commands (start, stop, config, logs)
│   └── dfsctl/                   # Client CLI binary
│       ├── main.go               # Entry point
│       ├── cmdutil/              # Shared utilities (auth, output, flags)
│       └── commands/             # Cobra commands (user, group, share, store, adapter)
│
├── pkg/                          # Public API (stable interfaces)
│   ├── adapter/                  # Protocol adapter interface
│   │   ├── adapter.go            # Adapter, OplockBreaker interfaces
│   │   ├── base.go               # BaseAdapter shared TCP lifecycle
│   │   ├── healthcheck.go        # BaseAdapter.Healthcheck
│   │   ├── identity.go           # BuildIdentityResolver, ExtractRealm
│   │   ├── sidecar/              # Sidecar-service lifecycle group
│   │   ├── nfs/                  # NFS adapter: dispatch.go routes RPC procedures
│   │   └── smb/                  # SMB adapter implementation
│   │
│   ├── auth/                     # Shared authentication primitives
│   │   ├── kerberos/             # Kerberos Provider
│   │   │   ├── provider.go       # Provider (keytab/krb5.conf state)
│   │   │   ├── keytab.go         # Keytab hot-reload manager
│   │   │   └── doc.go            # Package doc
│   │   └── sid/                  # Windows SID types and mapping
│   │
│   ├── metadata/                 # Metadata layer (RFC 16)
│   │   ├── kvstore/              # The store over a kv.KV; kvstoretest/ is its suite
│   │   ├── kv/                   # Key-value contract, badgerkv/, fdbkv/, kvtest/
│   │   ├── entity/               # Record types
│   │   └── errors/               # Store error codes
│   │
│   ├── block/                    # Per-share block storage
│   │   ├── engine/               # The engine (RFC 8), not yet wired to the adapters
│   │   └── journal/              # The journal (RFC 1), one per device
│   │       └── journaltest/      # Throwaway real store for outside tests
│   │
│   ├── controlplane/             # Control plane (config + runtime)
│   │   ├── store/                # GORM-based persistent store
│   │   │   ├── interface.go      # 9 sub-interfaces + composite Store
│   │   │   ├── gorm.go           # GORMStore implementation
│   │   │   ├── helpers.go        # Generic GORM helpers
│   │   │   └── ...               # Per-entity implementations
│   │   ├── runtime/              # Ephemeral runtime state
│   │   │   ├── runtime.go        # Composition layer (~500 lines)
│   │   │   ├── adapters/         # Adapter lifecycle sub-service
│   │   │   ├── stores/           # Metadata store registry sub-service
│   │   │   ├── shares/           # Share management sub-service
│   │   │   ├── mounts/           # Unified mount tracking sub-service
│   │   │   ├── lifecycle/        # Serve/shutdown orchestration sub-service
│   │   │   └── identity/         # Identity mapping sub-service
│   │   ├── api/                  # REST API server
│   │   │   ├── server.go         # HTTP server with JWT
│   │   │   └── router.go         # Route definitions
│   │   └── models/               # Domain models (User, Group, Share)
│   │
│   ├── apiclient/                # REST API client library
│   │   ├── client.go             # HTTP client with token auth
│   │   ├── helpers.go            # Generic API client helpers
│   │   └── ...                   # Resource-specific methods
│   │
│   ├── identity/                 # Identity resolution (providers, resolver, cache)
│   ├── discovery/                # Service discovery (mDNS/DNS-SD)
│   ├── health/                   # Health/readiness aggregation
│   ├── metrics/                  # Prometheus metrics
│   ├── snapshot/                 # Snapshot/backup support
│   │
│   └── config/                   # Configuration parsing
│       ├── config.go             # Main config struct
│       ├── blockstore.go         # Block store config
│       ├── metadata.go           # Metadata store config
│       ├── defaults.go           # Defaults + env binding
│       └── ...                   # Per-section config (logging, gc, snapshot, ...)
│
├── internal/                     # Private implementation details
│   ├── adapter/nfs/              # NFS protocol internals
│   │   ├── rpc/                  # RPC layer (call/reply handling)
│   │   │   └── gss/              # RPCSEC_GSS framework
│   │   ├── xdr/core/             # Generic XDR codec
│   │   ├── auth/                 # Auth context, AUTH_UNIX, share permission
│   │   ├── middleware/           # Per-request auth extraction
│   │   ├── types/                # NFS constants and types
│   │   ├── mount/handlers/       # Mount protocol procedures
│   │   ├── nlm/, nsm/, portmap/  # Lock/status/portmapper sidecar services
│   │   ├── v3/handlers/          # NFSv3 procedures (READ, WRITE, etc.)
│   │   └── v4/handlers/          # NFSv4.0 and v4.1 procedures
│   ├── adapter/smb/              # SMB protocol internals
│   │   ├── auth/                 # NTLM/SPNEGO authentication
│   │   ├── handlers/             # SMB2 command handlers
│   │   ├── session/, lease/      # Session + oplock/lease state
│   │   ├── signing/, encryption/, kdf/   # Crypto primitives
│   │   ├── framing.go            # NetBIOS framing
│   │   └── dispatch.go           # Command dispatch
│   ├── controlplane/api/         # API implementation
│   │   ├── handlers/             # HTTP handlers with centralized error mapping
│   │   └── middleware/           # Auth middleware
│   ├── cli/                      # CLI output formatting
│   ├── logger/                   # Logging utilities
│   └── ...                       # tlsconfig/, pathutil/, bytesize/
│
├── docs/                         # Documentation
│   ├── internals/                # This file, protocol and store internals
│   ├── guide/                    # User guides (configuration, NFS, SMB, CLI)
│   └── ...
│
└── test/                         # Test suites
    ├── integration/              # Integration tests (S3, BadgerDB)
    ├── e2e/                      # End-to-end tests (real NFS mounts)
    ├── conformance/              # SMB/NFS conformance harnesses
    ├── posix/, edge/, crash/     # Behavioural and fault-injection suites
    └── spec-citations/           # Verifies spec-section citations in code
```

## Multi-Node Metadata

Badger is embedded in one `dfs` process, so a metadata store serves exactly one
server. A distributed transactional key-value store — TiKV is the preferred
one — is the planned backend for multi-node deployments; it is not implemented
yet.

## Durable Handle State Flow

SMB3 durable handles allow open file state to survive client disconnects and (with persistent backends) server restarts. The lifecycle is:

```
OPEN -[disconnect]-> ORPHANED -[scavenger timeout]-> EXPIRED -[cleanup]-> CLOSED
                         |                                        |
                         +-[reconnect]--> RESTORED --> OPEN       |
                         |                                        |
                         +-[conflict/app-instance]--> FORCE_EXPIRED --> CLOSED
```

**Grant**: CREATE with DHnQ/DH2Q context triggers durability check. If the oplock level and share mode allow it, the server grants a durable handle with a configurable timeout (default 60s).

**Disconnect**: On connection loss, `closeFilesWithFilter` checks `IsDurable`. Durable files are persisted to `DurableHandleStore` (locks and leases preserved) rather than closed.

**Scavenger**: A background goroutine (`DurableHandleScavenger`) runs at 10-second intervals. For each expired handle it performs cleanup: releases byte-range locks, flushes block store caches, then deletes the handle from the store. On server restart, the scavenger adjusts remaining timeouts to account for downtime.

**Reconnect**: A new session sends CREATE with DHnC/DH2C. The server validates the durable-handle context against stored state (share name, path, username, session key hash, FileID, DesiredAccess, ShareAccess, expiry, and file existence) and restores the `OpenFile` without data loss.

**Conflict**: When a new open targets a file with an orphaned durable handle, the scavenger force-expires the orphaned handle to allow the new open to proceed. Cleanup includes releasing byte-range locks and flushing block store caches.

**App Instance ID**: For Hyper-V failover, a CREATE with a matching `AppInstanceId` triggers force-close of the old handle, allowing the new VM instance to take over.

**Admin API**: `GET /api/v1/durable-handles` lists all active handles with remaining timeout. `DELETE /api/v1/durable-handles/{id}` force-closes a specific handle.

## Migration & Block-Layout Routing

DittoFS has had three v1 block layouts (see the migration guide's table):
pre-v0.16 `.blk` files, standalone CAS objects, and packed `blocks/<id>`
containers. This build reads none of them: the v1 block tier, its remote
stores, codec, carver and syncer included, is deleted, and the engine keeps a
share's content in the journal. Content a v1 release wrote must be re-ingested
through a protocol.

See [the migration guide](../guide/block-store-migration.md) for the operator
runbook.

## Performance Characteristics

DittoFS is designed for high performance through several architectural choices:

- **Direct protocol implementation**: No FUSE overhead
- **Goroutine-per-connection model**: Leverages Go's lightweight concurrency
- **Buffer pooling**: Reduces GC pressure for large I/O operations
- **Streaming I/O**: Efficient handling of large files without full buffering
- **Three-tier storage**: Unified CAS-keyed `Cache` + local disk + remote store for optimal read latency
- **Zero-copy aspirations**: Working toward minimal data copying in hot paths

## Why Pure Go?

Go provides significant advantages for a project like DittoFS:

- **Easy deployment**: Single static binary, no runtime dependencies
- **Cross-platform**: Native support for Linux, macOS, Windows
- **Easy integration**: Embed DittoFS directly into existing Go applications
- **Modern concurrency**: Goroutines and channels for natural async I/O
- **Memory safety**: No buffer overflows or use-after-free vulnerabilities
- **Strong ecosystem**: Rich standard library and third-party packages
- **Fast compilation**: Quick iteration during development
- **Built-in tooling**: Testing, profiling, and race detection included
