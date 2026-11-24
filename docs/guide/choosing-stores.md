# Choosing Stores

A DittoFS node keeps file and directory metadata, and every control-plane record, in one
**metadata store**, and file content in a **journal** on its own devices. This guide says
what there is to pick. For the exact config keys and CLI flags, see
[Configuration](configuration.md).

> **Two different things — don't confuse them:**
> | Layer | What it holds | Choices | Configured by |
> |-------|---------------|---------|---------------|
> | Metadata store (per node) | Inodes, names, attributes, ACLs; users, groups, shares, permissions | `badger` (on disk); `foundationdb`, in a build with the `foundationdb` tag | `metadata.*` in config |
> | Journal (per node) | File content | none — always on disk | `journals` devices in config |

## Metadata store (per node)

This is the hot path for every `lookup`, `getattr`, `readdir`, and `create`, and it also
holds the control plane's records: there is no separate control-plane database.
`metadata.backend` picks the store:

| Backend | Durable? | Nodes | Status |
|---------|----------|-------|--------|
| `badger` | ✅ embedded LSM on disk, at `metadata.badger.path` | one | **Default** |
| `foundationdb` | ✅ a FoundationDB cluster, named by `metadata.foundationdb.cluster_file` | several | Opened only by a build with the `foundationdb` tag: the `-foundationdb` image and tarballs ([Deployment](install.md#foundationdb-metadata)) |

**Best practices**

- Put `metadata.badger.path` on fast local storage, in a directory on the volume rather
  than at its mount point.
- There is no in-memory mode and no cache-size knob: v1's `in_memory` stores and
  `block_cache_mb` / `index_cache_mb` are gone.
- A v1 metadata store is not opened; see [Upgrading from v1](upgrading-from-v1.md).

## File content

The engine keeps every share's content in the journal, on the node's `journals` devices,
and no remote block store is wired to it yet: v1's `s3` and `memory` block stores, its
content-addressed chunking and deduplication, and its syncer are deleted. There is nothing
to choose: give the node fast local devices ([Sizing](sizing.md)). The remote tier returns
with [RFC 4](../internals/rfcs/rfc-4-remote-tier.md). A v1 block layout is not read; see
[Block store migration](block-store-migration.md).

Building a custom backend instead of choosing a built-in one? See
[Implementing stores](../internals/implementing-stores.md).
