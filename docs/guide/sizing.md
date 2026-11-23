# Sizing & Hardware

How to size the machine that runs `dfs`. For picking *which* stores to run, see
[Choosing Stores](choosing-stores.md); this page covers the hardware they need.

> **Read this first.** DittoFS is experimental and **not production ready**. There is no
> supported production configuration and no true high-availability today (see
> [single-node](#single-node-not-a-cluster) below). Treat everything here as guidance for a
> **pilot / evaluation / internal single-node** deployment, and load-test with your own
> workload before committing.

## Single-node, not a cluster

Size **one box**, not a fleet. The only metadata store this build opens (`badger`) is
embedded in one process, and it also holds the control plane's records, so there is no
separate database to size and you cannot run active-active `dfs` replicas against the same
data. So "sizing" means sizing a single server.

All file content lives on the node's journal devices (the `journals` list): no remote
block store is wired to the engine yet, and the remote tier returns with
[RFC 4](../internals/rfcs/rfc-4-remote-tier.md). The topology is **badger metadata + the
journal devices**, all on the one box.

## What drives each resource

| Resource | What drives it | How to size it |
|----------|----------------|----------------|
| **RAM** | Badger runs with its own default cache sizes, which no key changes. Plus per-connection NFS/SMB buffers. | Give the process a real RAM budget; the operating system's page cache also serves the metadata store's files, so spare RAM still speeds `lookup`/`getattr`/`readdir`. |
| **Disk (journal)** | The journal devices hold the only copy of every byte written. Metadata (badger LSM, at `metadata.badger.path`) also lives on disk and grows with inode/file count. | Size the journal devices for **total data**, and leave headroom for metadata if it shares a volume. Use NVMe/SSD: every write lands on the journal. |
| **CPU** | Client protocol work and the journal's write path. | Cores help write throughput and concurrent connections. At small scale writes are typically per-op / `fsync`-bound rather than CPU-starved. |
| **Network** | Client NFS/SMB traffic only: nothing syncs off the node. | Size for your clients. |

## Starting points

Anchor points, not guarantees — validate against your workload.

| Tier | vCPU | RAM | Journal disk (≥ total data) | Notes |
|------|------|-----|--------------|-------|
| **Pilot / eval** | 2–4 | 4–8 GiB | 20–50 GiB SSD | |
| **Single-node "serious"** | 8 | 16–32 GiB | 100–500 GiB NVMe | NVMe matters — every write lands on the journal |
| **Heavy** | 16+ | 64 GiB+ | 1 TiB+ NVMe | Past the tested envelope — validate, don't assume |

## Rules of thumb

- **The journal holds everything.** Size the journal devices for total data, not a hot set.
- **RAM buys metadata speed.** The metadata store is the hot path for every filesystem
  operation; leave the page cache room to hold its hot files.
- **NVMe for the journal.** Every write lands there.
- **One box.** No active-active HA today — plan for a single node.

## See also

- [Choosing Stores](choosing-stores.md) — picking metadata and block stores
- [Durability](durability.md) — how far a write must land before it is acknowledged
- [Configuration](configuration.md) — every config key and CLI flag
