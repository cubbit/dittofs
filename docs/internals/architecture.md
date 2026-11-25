# DittoFS Architecture

This document provides a deep dive into DittoFS's architecture, design patterns, and internal implementation.

> **On this branch no file protocol is served.** The node builds the NFS and SMB
> adapters of [RFC 20](rfcs/rfc-20-adapters.md) over `vfs.Service`, but their
> operation tables hold no file operation (see [Data path](#data-path)). The adapter
> sections below that describe NFS and SMB state machines describe the protocol
> design, not code this branch runs.

**Storage terms used throughout** (see the [Glossary](../guide/glossary.md) for protocol and security terms):

- **CAS** (Content-Addressed Storage) — blocks are named by the hash of their contents rather than by location, so identical data is stored once and deduplicated automatically.
- **FastCDC** — a content-defined chunking algorithm that splits file data at content-based boundaries, so small edits only re-chunk the affected region ([FastCDC paper](https://www.usenix.org/conference/atc16/technical-sessions/presentation/xia)).
- **BLAKE3** — the fast cryptographic hash used to address CAS blocks and verify them end-to-end ([BLAKE3 spec](https://github.com/BLAKE3-team/BLAKE3-specs)).

## Table of Contents

- [Core Abstraction Layers](#core-abstraction-layers)
- [Data path](#data-path)
- [Object ownership](#object-ownership-a-client-supplied-identifier-is-not-authorization)
- [The control plane](#the-control-plane)
- [Service Layer](#service-layer)
- [Custom Backends](#custom-backends)
- [Directory Structure](#directory-structure)
- [Multi-Node Metadata](#multi-node-metadata)
- [Durable Handle State Flow](#durable-handle-state-flow)
- [Migration & Block-Layout Routing](#migration--block-layout-routing)

## Core Abstraction Layers

One object per process, the **node** (`node.Node`, built by `node.New` in
`internal/node`), builds every component, starts and stops them, and keeps them
matching the records in the metadata store ([RFC 23](rfcs/rfc-23-control-plane.md)
§2). The management API writes records and never calls a component; the node
reads the records and applies them. `dfsctl` is a client of the API and nothing
else.

```
   dfsctl, any HTTP client              NFS / SMB clients
            │                                   │
            ▼                                   ▼
┌───────────────────────┐        ┌───────────────────────────┐
│    Management API     │        │     Protocol adapters     │
│  internal/node/api    │        │  internal/adapter/{nfs,   │
│  (writes records)     │        │   smb}, core              │
└──────────┬────────────┘        └─────────────┬─────────────┘
           │ one transaction per write          │ vfs.Service only
           ▼                                    ▼
┌───────────────────────┐   reads   ┌───────────────────────────┐
│    Metadata store     │◄──────────│    Filesystem service     │
│  pkg/metadata/kvstore │           │  internal/vfs: admission, │
│  over kv.KV (Badger,  │           │  authorization, files     │
│  FoundationDB)        │           └──────┬─────────────┬──────┘
└──────────┬────────────┘                  │             │
           │ records                       ▼             ▼
           ▼                        ┌─────────────┐ ┌──────────────┐
┌───────────────────────┐ applies   │  Identity   │ │    Engine    │
│        Node           │──────────►│  server     │ │  pkg/engine  │
│  internal/node:       │ reconcile │  internal/  │ │  over        │
│  build, start, stop,  │           │  identity   │ │  pkg/journal │
│  reconcile, runner    │           └─────────────┘ └──────────────┘
└───────────────────────┘
```

### Key Interfaces

**1. The node** (`internal/node`)
- `node.New` builds every component once from the host's bootstrap
  (`pkg/bootstrap`) and the metadata store's KV, which `dfs start` opens and
  hands it: the store over the KV, the engine, the identity server, the
  filesystem service, the API and the adapters.
- `Start` runs its steps in order: refuse a journal of another installation, open the listeners,
  register the node, read the records, start the engine, the identity server,
  the filesystem service and the operation runner, reconcile once, then start
  the adapters and the reconcile loop. Readiness turns `ready` after the last
  step. `Stop` reverses it.
- Reconcile reads the generation counters each second, re-reads what changed
  and applies it by its binding class ([RFC 13](rfcs/rfc-13-configuration.md)
  §5): a share's storage binding, a live setting, and, once the adapters'
  section is declared, an adapter started or drained. The node writes one
  node-status record saying what it applied and what it refused.
- The runner (`internal/node/runner.go`; a step speaks `internal/node/step`)
  runs long work as operations, every kind in `pkg/node/apiv1/opkind.go`: a
  share's, a user's or a group's deletion, a domain join, a machine-password or
  signing-key rotation, a directory test or name resolution, the NT hashes'
  deletion. An operation's progress is
  its owning component's durable cursor, so it survives a restart.
- `internal/install` is what `dfs init` and `dfs reset-admin` run over the
  host's own store, outside the API.

**2. The management API** (`internal/node/api`, wire types in `pkg/node/apiv1`)
- Resources under `/api/v1/`: shares, export policies, grants, quotas, users,
  groups, netgroups, API tokens, adapters, remote stores, namespaces, settings,
  nodes, operations, the installation and the audit log. `pkg/node/apiv1/openapi.json`
  is generated from the routes (`go run ./cmd/genopenapi`).
- Every write commits its records and its audit record in one transaction.
  Updates and deletes carry `If-Match`; creates, deletes and actions carry an
  idempotency key, but the credential calls take none; long work answers
  `202` with an operation.
- Callers are API accounts, `User` records with a role, `admin`, `operator` or
  `viewer` (`internal/node/api/auth`). A role grants nothing on files.
- Errors are the components' own values, mapped once to a status and a problem
  code (`problems.go`).
- `GET /health/live` and `GET /health/ready` answer without a token.

**3. The CLI** (`cmd/dfsctl`, client in `pkg/node/apiv1/client`)
- `dfsctl` reaches a node over https, over http on a loopback address, or over
  the node's local socket, and keeps each sign-in in a named context.
- It links none of the node's, the bootstrap's or the store's code.

**4. Adapters** (`internal/adapter`, settings in `pkg/adapter/settings`)
- `core` holds the listeners, connections and request pipeline every adapter
  shares; `nfs` and `smb` add their protocol's wire and operation tables.
- An adapter reaches files only through `vfs.Service` (`pkg/vfs`).
- An adapter's settings will be records under `adapters.<protocol>` once the
  node's settings table declares that section; today no record enables an
  adapter. The node binds each adapter on the bootstrap's `adapters.listen`
  addresses.

**Filesystem service and identity** (`internal/vfs`, `internal/identity`)
- `vfs.Service` admits each call (`internal/vfs/admit`: the share's security
  minimums and client rules), resolves the caller on the share through the
  identity server, authorizes it (`internal/vfs/authz`) and runs the file
  operation against the store and the engine.
- The identity server signs callers in, keeps the directory and the keytab, and
  maps and squashes a caller on a share ([RFC 18](rfcs/rfc-18-identity.md)).

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
tables hold no file operation: NFSv3, MOUNT, NLM and NSM answer `PROC_UNAVAIL`,
NFSv4 decodes COMPOUND with no operation but ILLEGAL, and SMB has no
file operations until RFC 20's file operations land.

The journal devices come from the bootstrap's `journals`. A file that still
names v1's `blockstore.journal.path` is refused, naming `journals`; at start
the node refuses a v1 state layout it finds (`probeV1State`,
`internal/node/preflight.go`).

The engine (`pkg/engine`, [RFC 8](rfcs/rfc-8-engine.md)) and the journal
(`pkg/journal`, [RFC 1](rfcs/rfc-1-journal.md)) are built and tested on
their own, behind the `engine_conformance` and `journal_conformance` tags.

## Share Snapshots

v1's snapshots are deleted; the snapshot resource waits with
[RFC 12](rfcs/rfc-12-snapshots.md).

## Object ownership: a client-supplied identifier is not authorization

The choke points this section and the durable-handle flow below name are v1's
adapters, deleted on this branch; the rule stands for RFC 20's adapters.

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

## The control plane

Every change to what an installation serves is a record written through the
management API ([RFC 23](rfcs/rfc-23-control-plane.md)):

1. **The API writes records.** A write is judged by the settings table and the
   resource's own rules, and commits with its audit record in one transaction
   through the store's control-plane view. It never reaches a running
   component.
2. **The node applies them.** Each node reads the generations each second and
   applies what changed: a share's binding to the engine or a live setting. A restart-class change waits for the next start.
3. **Each node reports.** The node-status record says what each node applied,
   so a setting's read shows how many nodes run it.
4. **File handles route by share.** A handle encodes the share's ID and the
   file's; the filesystem service resolves the share and its binding from it.

A share is created with its export policy, in a namespace on a remote store.
On this branch no namespace can be created yet (the store's and namespace's
settings are not declared), so this command cannot succeed:

```bash
./dfsctl share create export --path /export --namespace <namespace-id> --flavor sys --client 0.0.0.0/0=read-write
```

Every share binds to the node's one engine, over the node's journal devices.
No remote store is wired to the engine yet.

## Service Layer

The service layer provides business logic and coordination between stores.

### Write Coordination Pattern

`vfs.Service.Write` admits the caller and judges the file (type, read-only
share, rights, open state) and reserves its quota before the engine stages a
byte, so a write refused while it is judged or reserved changes no metadata or
content (a conflicting grant may already be recalled); one the engine refuses
may already have cleared setuid and setgid; a zero-count write returns once
judged. No protocol WRITE handler calls it yet.

## Custom Backends

See [Implementing stores](implementing-stores.md): a metadata backend
implements `pkg/metadata/kv.KV` and passes `kv/kvtest`.

## Directory Structure

```
dittofs/
├── cmd/
│   ├── dfs/commands/             # dfs: init, start, reset-admin, config, version
│   ├── dfsctl/                   # dfsctl, the management API's client
│   │   ├── cmdutil/              # Contexts, name resolution, output
│   │   ├── commands/             # One package per resource
│   │   └── internal/clisuite/    # The CLI's shared test rows
│   ├── gendocs/                  # Generates docs/guide/cli.md
│   ├── genopenapi/               # Generates pkg/node/apiv1/openapi.json
│   └── dt/                       # The test harness's runner
│
├── internal/
│   ├── node/                     # The node: build, start, stop, reconcile, runner
│   │   ├── api/                  # The management API: routes, auth, problems
│   │   ├── step/                 # What an operation's step speaks to the runner
│   │   └── nodetest/             # A real node for tests
│   ├── install/                  # dfs init and dfs reset-admin over the store
│   ├── vfs/                      # The filesystem service: admit/, authz/, pseudofs/, quota/
│   ├── identity/                 # The identity server: directory/, netlogon/
│   ├── adapter/                  # core/, nfs/, smb/, adaptertest/
│   └── ...                       # logger/, cli/, tlsconfig/, prof/, sidecar/
│
├── pkg/
│   ├── bootstrap/                # The host's bootstrap file and environment
│   ├── node/apiv1/               # The API's wire types; client/ is its Go client
│   ├── metadata/                 # kvstore/, kv/ (badgerkv, fdbkv, kvtest), entity/, errors/
│   ├── engine/, journal/         # The engine (RFC 8) over the journal (RFC 1)
│   ├── settings/                 # Settings sections, the write check, classes
│   ├── secrets/, keyprovider/    # Sealed secrets and the wrapping keys
│   ├── identity/                 # The identity contract
│   ├── vfs/                      # The filesystem service's contract
│   ├── adapter/                  # nfs/, smb/, settings/
│   └── ...                       # health/, metrics/, openstate/, resources/, sim/
│
├── docs/                         # guide/ for users, internals/ for contributors
└── test/                         # harness/ (dt), integration/, e2e/, conformance suites
```

## Multi-Node Metadata

Badger is embedded in one `dfs` process, so a Badger metadata store serves
exactly one node. `metadata.backend: foundationdb` keeps the store in a
FoundationDB cluster instead, in a `dfs` built with the `foundationdb` tag; every
node still runs both roles ([RFC 15](rfcs/rfc-15-topology.md)).

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

**Admin API**: the management API has no durable-handle routes; `GET /api/v1/nodes/{id}/connections` lists a node's connections.

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
