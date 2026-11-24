# Implementing Custom Stores

This guide covers implementing a custom metadata backend for DittoFS, and says
where the block tier stands.

## Table of Contents

1. [Overview](#overview)
2. [When to Implement Custom Stores](#when-to-implement-custom-stores)
3. [Understanding the Architecture](#understanding-the-architecture)
4. [Implementing Metadata Stores](#implementing-metadata-stores)
5. [The journal (local tier)](#the-journal-local-tier)
6. [Best Practices](#best-practices)
7. [Additional Resources](#additional-resources)

## Overview

The one pluggable store today is the **metadata store**'s key-value backend,
under the store that holds file and directory structure, attributes and
permissions.

The block tier has nothing to plug in. Every share keeps an on-disk **journal**
(`pkg/journal/`); it is provisioned automatically from the node's
`journals` devices and has no type, no kind, and no operator-facing
configuration. The v1 remote block stores (`s3`, `memory`) and their conformance
suite are deleted, and the engine takes no remote store yet. The remote tier
returns with [RFC 4](rfcs/rfc-4-remote-tier.md), and its contract and
conformance suite with it.

**Key Design Principle**: the engine (`pkg/engine`) runs over the journal
and orchestrates reads and writes; `vfs.Service` is its only caller on the data
path.

## When to Implement Custom Stores

### Metadata Store Use Cases

Implement a custom metadata store when you need:

- **Distributed metadata**: multi-node deployments over a distributed
  transactional key-value store (TiKV is the preferred one; planned, not
  implemented yet)
- **Advanced features**: Full-text search, custom indexing, complex queries
- **Compliance**: Audit logs, versioning, immutability guarantees

There is no block store use case row: the journal (`pkg/journal`) is the
only block tier and it is not pluggable.

## Understanding the Architecture

### Shares, remote stores and the data path

The engine (`pkg/engine`, RFC 8) runs over the journal; the node builds it
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

The journal (`pkg/journal`, [RFC 1](rfcs/rfc-1-journal.md)) is **not a
pluggable store**: one journal per device, with a handle per share, provisioned
by the engine. Its contract is its own package API; its conformance checks run
under the `journal_conformance` tag (`pkg/journal/conformance/`).

## Best Practices

### Thread Safety

All store implementations must be thread-safe. Multiple goroutines will access the store concurrently.

### Context Handling

Always respect context cancellation, especially where a backend's calls go over the network:

```go
func (k *MyKV) View(ctx context.Context, fn func(kv.Reader) error) error {
    if err := ctx.Err(); err != nil {
        return err
    }
    // Proceed with operation
}
```

### Error Handling

- Wrap errors with meaningful context
- Distinguish transient (retry-able) from permanent failures

## Additional Resources

- **Interface Definitions**: `pkg/metadata/kv/kv.go`
- **Reference Implementations**:
  - Journal (local tier): `pkg/journal/`
  - Metadata backends: `pkg/metadata/kv/badgerkv/`, `pkg/metadata/kv/fdbkv/`
- **Conformance Tests**: `pkg/metadata/kv/kvtest/` and `pkg/metadata/kvstore/kvstoretest/`
- **Architecture**: `docs/ARCHITECTURE.md`
- **Configuration**: `docs/CONFIGURATION.md`
- **Contributing**: `docs/CONTRIBUTING.md`
