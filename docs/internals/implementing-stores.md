# Implementing Custom Stores

This guide provides comprehensive instructions for implementing custom metadata stores and block stores for DittoFS. Whether you're building a database-backed metadata store or a custom cloud storage integration, this document will walk you through the process with best practices and practical examples.

## Table of Contents

1. [Overview](#overview)
2. [When to Implement Custom Stores](#when-to-implement-custom-stores)
3. [Understanding the Architecture](#understanding-the-architecture)
4. [Implementing Metadata Stores](#implementing-metadata-stores)
5. [The journal (local tier)](#the-journal-local-tier)
6. [Implementing a Remote Store](#implementing-a-remote-store)
7. [Implementing a Remote Block Store (block-keyed)](#implementing-a-remote-block-store-block-keyed)
8. [Best Practices](#best-practices)
9. [Testing Your Implementation](#testing-your-implementation)
10. [Common Pitfalls](#common-pitfalls)
11. [Integration with DittoFS](#integration-with-dittofs)

## Overview

There are **two** store types an operator configures and therefore two you can
implement:

- **Metadata Stores**: a key-value backend under the metadata store, which holds file and directory structure, attributes and permissions
- **Block Stores**: Durable storage in S3 or a compatible object store, shared across shares via ref counting

The local tier is **not** one of them. Every share keeps an on-disk **journal**
(`pkg/block/journal/`) in front of its block store; it is provisioned
automatically from the node's `journals` devices and has no type, no kind, and no
operator-facing configuration. A block store has no kind either — the old
local/remote split is gone, and the valid types are `s3` and `memory`. If you are
here looking for how to implement a custom *local* store type, that concept no
longer exists; see [The journal](#the-journal-local-tier) for what replaced it.

**Key Design Principle**: the engine (`pkg/block/engine`) runs over the journal
and orchestrates reads and writes; the remote tier is not wired yet;
`vfs.Service` is its only caller on the data path.

This separation enables:
- Independent scaling of metadata and block storage
- Per-share isolation with shared block-store backends
- Simple store implementations (just implement the interface, the engine handles coordination)

## When to Implement Custom Stores

### Metadata Store Use Cases

Implement a custom metadata store when you need:

- **Distributed metadata**: multi-node deployments over a distributed
  transactional key-value store (TiKV is the preferred one; planned, not
  implemented yet)
- **Advanced features**: Full-text search, custom indexing, complex queries
- **Compliance**: Audit logs, versioning, immutability guarantees

### Block Store Use Cases

Implement a custom block store when you need:

- **Cloud storage integration**: Azure Blob, Google Cloud Storage, custom object stores
- **Specialized storage**: Tape archives, HSM systems, data lakes
- **Tiering**: Automatic hot/cold data movement based on access patterns

**Reference implementation**: `pkg/block/remote/s3/` (S3-backed block store)

There is no "local store use case" row. The journal (`pkg/block/journal`) is the
only local tier and it is not pluggable.

## Understanding the Architecture

### Shares, remote stores and the data path

The engine (`pkg/block/engine`, RFC 8) runs over the journal; the node builds it
and `vfs.Service` over it, and the adapters reach it only through
`vfs.Service`. The remote tier is not wired yet. The adapters have no file
operations yet (see [Data path](architecture.md#data-path)).

## Implementing Metadata Stores

The metadata store (`pkg/metadata/kvstore`, [RFC 16](rfcs/rfc-16-metadata-store.md))
is written once, over the ordered transactional key-value contract `kv.KV`
(`pkg/metadata/kv/kv.go`). A new backend implements `kv.KV`, not the store:

- `pkg/metadata/kv/badgerkv/`: Badger, embedded, for one node
- `pkg/metadata/kv/fdbkv/`: FoundationDB, for a cluster

A backend passes `pkg/metadata/kv/kvtest`'s suite (`kvtest.Run`), and the store
over it `pkg/metadata/kvstore/kvstoretest`'s (`kvstoretest.Run`).

## The journal (local tier)

The journal (`pkg/block/journal`, [RFC 1](rfcs/rfc-1-journal.md)) is **not a
pluggable store**: one journal per device, with a handle per share, provisioned
by the engine. Its contract is its own package API; its conformance checks run
under the `journal_conformance` tag (`pkg/block/journal/conformance/`), and the
one `blockstoretest` suite (`RemoteBlockStoreConformance`) does not apply to it.

## Implementing a Remote Store

Remote stores provide durable block storage shared across shares via ref counting.

### The RemoteStore Interface

The `pkg/block/remote.RemoteStore` interface defines the contract. Its surface is
entirely block-keyed: packed block objects under the `blocks/` prefix, read and
written through `RemoteBlockStore`, plus the per-chunk `ChunkReader` /
`ChunkSealer` transform seam and the lifecycle/health probes. See
`pkg/block/remote/remote.go` for the authoritative definition:

```go
type RemoteStore interface {
    RemoteBlockStore // PutBlock/GetBlock/GetBlockRange/DeleteBlock/WalkBlocks
    ChunkReader      // ReadChunk: one chunk's plaintext out of a block object
    ChunkSealer      // SealChunk: one chunk's plaintext into its wire bytes

    // HealthCheck is the legacy error-returning probe used by the syncer.
    HealthCheck(ctx context.Context) error

    // Healthcheck returns a structured health.Report (satisfies health.Checker).
    Healthcheck(ctx context.Context) health.Report

    // Close releases resources.
    Close() error
}
```

There is no hash-keyed CAS operation on this surface, and no method on it
verifies content: `ReadChunk` returns chunk plaintext and the engine recomputes
its BLAKE3 afterwards (`readChunkVerified` in `pkg/block/engine/fetch.go`),
because no single decorator layer holds both the wire bytes and the
plaintext-hash domain. Per-method semantics are documented under
[Implementing a Remote Block Store](#implementing-a-remote-block-store-block-keyed);
implement that interface and the three methods above.

### Ref Counting

Remote stores are shared across shares via ref counting:
- When a share is created referencing a remote store, the ref count increments
- When a share is removed, the ref count decrements
- When the ref count reaches zero, `Close()` is called

This means your `Close()` implementation should release all resources (connections, goroutines, etc.).

### Reference Implementation

See `pkg/block/remote/s3/` for a production S3 remote store implementation with:
- Configurable retry with exponential backoff
- Health check via HEAD bucket
- Efficient multipart uploads for large blocks

### Conformance Tests

There is no conformance suite for the legacy CAS surface — it was deleted along with
the `block.Store` interface. The only suite is `RemoteBlockStoreConformance`, covered
under [Implementing a Remote Block Store](#implementing-a-remote-block-store-block-keyed).

## Implementing a Remote Block Store (block-keyed)

The `pkg/block/remote.RemoteBlockStore` interface is the **block-keyed** (non-CAS) remote store surface used by the live write path. Every new write is packed into block objects under the `blocks/` prefix, separate from the legacy CAS `cas/` namespace — the two namespaces never collide. New remote backends should implement `RemoteBlockStore`.

> **Legacy CAS objects:** `cas/<hash>` objects written before the blocks-only flip are no longer reachable — the hash-keyed accessors that read them were removed along with the `block.Store` interface, and no backend carries them any more. A deployment holding pre-flip data must be migrated before upgrading; see [architecture.md](architecture.md) for the consequences of skipping that. A new backend implements `RemoteBlockStore` only.

### The RemoteBlockStore Interface

```go
type RemoteBlockStore interface {
    PutBlock(ctx context.Context, blockID string, r io.Reader) error
    GetBlock(ctx context.Context, blockID string) ([]byte, error)
    GetBlockRange(ctx context.Context, blockID string, offset, length int64) ([]byte, error)
    DeleteBlock(ctx context.Context, blockID string) error
    WalkBlocks(ctx context.Context, fn func(blockID string, meta block.Meta) error) error
}
```

See `pkg/block/remote/remote.go` for the authoritative definition and per-method docstrings. Current implementations: `pkg/block/remote/s3/` and `pkg/block/remote/memory/`.

#### Key shape

Objects are keyed by an opaque `blockID` string. The on-disk/on-wire key is `block.FormatBlockKey(blockID)` = `"blocks/<blockID>"`. Backends derive this key internally; callers and the engine never construct raw S3/object-store keys.

#### PutBlock

Writes the content of `r` under `blocks/<blockID>`. Implementations **must** stream `r` without buffering the full body — callers may supply an unbounded reader (e.g., an in-progress packing file). The call is idempotent: a second `PutBlock` for the same `blockID` overwrites silently.

#### GetBlock

Returns the full bytes of the named block object. Returns `block.ErrChunkNotFound` when the block is absent. The returned slice must be freshly allocated and must not alias internal storage.

#### GetBlockRange

Returns `[offset, offset+length)` bytes of the block object. Bounds validation:

- **Negative offset** — must return `block.ErrInvalidOffset` (client-validated before any network call).
- **Past-EOF offset** — a past-EOF offset cannot be detected here without a `HEAD`, so backends surface a native error (S3: HTTP 416) rather than `ErrInvalidOffset`. The contract only requires _some_ error for `offset >= EOF`.
- **Non-positive length** — must return `block.ErrInvalidSize`.
- **Past-EOF length** — clamped to the object's remaining bytes on backends that support partial-content (S3 `Range` header); no error.
- **Absent blockID** — must return `block.ErrChunkNotFound`.

#### DeleteBlock

Removes the block object. Idempotent: deleting an absent `blockID` must return `nil`.

#### WalkBlocks

Enumerates every block object under the `blocks/` prefix. The callback receives the `blockID` (with the `blocks/` prefix stripped) and a `block.Meta` (size, last-modified timestamp). Ordering is unspecified.

- Returning `block.ErrStopWalk` from the callback is a clean early exit — `WalkBlocks` returns `nil`.
- Any other callback error halts enumeration and is returned wrapped as `"walk halted at <blockID>: %w"`.
- Context cancellation aborts immediately.

### Block codec wire format

Block objects are written by `pkg/block/blockcodec`. A single block packs one or more chunk records into a continuous byte stream:

```
Block = [ preamble ][ record_0 ][ record_1 ] … [ record_{N-1} ]

preamble:
  magic      [4]byte = {'D','F','B','1'}   // "DFB1" — identifies a DittoFS block object
  flags      uint8                          // bit0 = 1 → record headers are AEAD-sealed
  blockID    uvarint(len) + len bytes (UTF-8)

record (plaintext, flags bit0 = 0):
  hash       [32]byte                       // chunk BLAKE3 content hash
  wireLen    uvarint
  wire       [wireLen]byte                  // enc(comp(chunk)) — already-transformed body

record (sealed, flags bit0 = 1):
  sealedHdrLen  uvarint
  sealedHdr     [sealedHdrLen]byte          // AEAD seal of {hash[32], wireLen uvarint}
                                            // AAD = blockID || recordIndex(uvarint)
  wire          [wireLen]byte               // wireLen recovered by opening sealedHdr
```

**Sealed-header framing** is used on encrypted shares. The AEAD tag covers `hash + wireLen` with `blockID||recordIndex` as additional authenticated data, while the `wire` body is already the encrypted chunk body (the encryption decorator applies its transform before `Builder.Add` is called). The chunk metadata is therefore authenticated even though the wire body is opaque.

**Locators.** Each call to `Builder.Add(hash, wire)` returns a `block.ChunkLocator{BlockID, WireOffset, WireLength}` — the byte range of the wire body within the block object. `WireOffset` and `WireLength` are over wire bytes (after the per-record header), not plaintext bytes. The engine stores these locators in the metadata store and recovers an individual chunk via `GetBlockRange(blockID, WireOffset, WireLength)` (surfaced through the optional `ChunkReader.ReadChunk` extension on the same backend).

### Conformance suite

Every new `RemoteBlockStore` backend must pass `blockstoretest.RemoteBlockStoreConformance`:

```go
package myremote_test

import (
    "testing"
    "github.com/cubbit/dittofs/pkg/block/blockstoretest"
)

func TestMyRemoteBlockStore(t *testing.T) {
    factory := func(t *testing.T) (blockstoretest.RemoteBlockStore, func()) {
        store, cleanup := createTestStore(t)
        return store, cleanup
    }
    blockstoretest.RemoteBlockStoreConformance(t, factory)
}
```

The suite covers: `PutBlock`/`GetBlock` round-trip, no-aliasing, `GetBlockRange` mid-range/past-EOF-clamped/invalid-offset/invalid-size/absent, `DeleteBlock` durability and idempotency, `WalkBlocks` enumeration and `ErrStopWalk`, idempotent `PutBlock`, zero-byte blocks, and concurrent same-ID `PutBlock`.

## Best Practices

### Thread Safety

All store implementations must be thread-safe. Multiple goroutines will access the store concurrently.

### Context Handling

Always respect context cancellation, especially for remote stores where network calls can be slow:

```go
func (s *MyStore) ReadBlock(ctx context.Context, blockID string) ([]byte, error) {
    if err := ctx.Err(); err != nil {
        return nil, err
    }
    // Proceed with operation
}
```

### Error Handling

- Journal errors should be wrapped with meaningful context
- Block store errors should distinguish transient (retry-able) from permanent failures
- Delete operations should be idempotent (deleting a non-existent block is not an error)

### Performance

- **Block stores**: Use connection pooling, implement retry with backoff, batch operations where possible

## Testing Your Implementation

1. **Conformance tests**: Run the provided test suite (`pkg/block/blockstoretest`)
2. **Concurrency tests**: Verify thread safety with parallel reads/writes
3. **Error handling tests**: Test behavior with canceled contexts, network failures
4. **Integration tests**: Test with the full DittoFS stack (create share, mount, read/write)

## Common Pitfalls

1. **Not making Delete idempotent**: Deleting a non-existent block should succeed
2. **Ignoring context cancellation**: Long operations should check `ctx.Err()` periodically
3. **Unsafe concurrent access**: Use proper synchronization for shared state
4. **Resource leaks**: Ensure `Close()` releases all resources (connections, goroutines, file handles)

## Integration with DittoFS

### Register Your Store

Add your store type to the block-store factory, and to the type whitelist the
REST layer validates against:

```go
// pkg/controlplane/runtime/shares/blockstore_config.go
func CreateRemoteStoreFromConfig(ctx context.Context, storeType string, cfg …) (remote.RemoteStore, error) {
    switch storeType {
    case "memory":
        return remotememory.New(), nil
    case "s3":
        // …
    case "myremote":
        return myremote.New(config)
    default:
        return nil, fmt.Errorf("unknown block store type: %s", storeType)
    }
}

// internal/controlplane/api/handlers/block_stores.go
func validateBlockStoreType(storeType string) bool {
    return storeType == "s3" || storeType == "memory" || storeType == "myremote"
}
```

There is no kind to register alongside the type: a block store row is unique by
name alone.

### Selecting the store

The CLI no longer creates block stores: they return as resource commands of the
v2 management API. The share commands that attach one to a share are unavailable
until then too. The journal has no equivalent — it is not a named entity the CLI
can create.

## Additional Resources

- **Interface Definitions**: `pkg/block/journal/localstore.go`, `pkg/block/remote/remote.go`
- **Reference Implementations**:
  - Journal (local tier): `pkg/block/journal/`
  - Block stores: `pkg/block/remote/s3/`, `pkg/block/remote/memory/`
  - Metadata backends: `pkg/metadata/kv/badgerkv/`, `pkg/metadata/kv/fdbkv/`
- **Conformance Tests**: `pkg/block/blockstoretest/` (block stores), `pkg/metadata/kv/kvtest/` and `pkg/metadata/kvstore/kvstoretest/` (metadata)
- **Architecture**: `docs/ARCHITECTURE.md`
- **Configuration**: `docs/CONFIGURATION.md`
- **Contributing**: `docs/CONTRIBUTING.md`
