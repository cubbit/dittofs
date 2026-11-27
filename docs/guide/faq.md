# Frequently Asked Questions

Short answers to common questions about DittoFS, and the list of its known
limitations. If something does not work, see [Troubleshooting](troubleshooting.md).

> **This version is a rebuild (v2), in progress.** It runs a node and its management
> API, but serves no NFS or SMB yet and cannot create a share yet. Answers about the
> protocols describe the released server and say so.

## Table of Contents

- [General Questions](#general-questions)
- [Technical Questions](#technical-questions)
- [Usage Questions](#usage-questions)
- [Comparison Questions](#comparison-questions)
- [Durability and caching](#durability-and-caching)
- [Known Limitations](#known-limitations)
  - [NFS Protocol Limitations](#nfs-protocol-limitations)
  - [SMB Client Limitations](#smb-client-limitations)
  - [Storage Backend Limitations](#storage-backend-limitations)
  - [General Limitations](#general-limitations)
  - [POSIX Compliance Summary](#posix-compliance-summary)

## General Questions

### What is DittoFS?

A file server written in Go that serves the same files over NFS and SMB, entirely in
userspace. The released server speaks NFSv3, NFSv4.0, v4.1 and v4.2, and SMB 2 and 3.
This version keeps metadata in Badger on disk (or in FoundationDB) and file content in
an on-disk journal; see [Choosing stores](choosing-stores.md).

### Why not use FUSE?

FUSE puts a kernel module between the client and the server. DittoFS implements the
protocols itself, in one process, so it needs no kernel module and no special
privileges, and protocol behaviour is easier to control and to debug.

### Can I use this in production?

**No, not yet.** DittoFS is experimental. It has had no security audit, it needs more
testing and hardening, and its on-disk formats may still change. Use it for
development, testing and evaluation until a stable 1.0 release.

### What license is DittoFS under?

The MIT License, which allows commercial use.

## Technical Questions

### Which NFS versions are supported?

Not served on this version. The released server supports:

- **NFSv3** over TCP, all 28 procedures.
- **NFSv4.0** and **NFSv4.1**: compound operations, sessions, file and directory
  delegations, and ACLs.
- **NFSv4.2**: extended attributes and sparse files.

Kerberos for NFS is not served yet; see
[Does it support Kerberos?](#does-it-support-kerberos-authentication)

### Does it support file locking?

On the released server, yes:

- **NFSv4** has locking built into the protocol. Nothing to set up; this is the
  recommended way.
- **SMB** supports shared and exclusive byte-range locks.
- **NFSv3** locks through a separate protocol, NLM, with NSM for crash recovery, over
  TCP and UDP. macOS and BSD clients reach NLM over UDP and find it through the
  portmapper on port 111. So for those clients, two settings of the NFS adapter must
  be turned on, then the adapter restarted:
  - `udp_enabled`, for the UDP transport;
  - `portmapper_enabled` and `portmapper_port`, for a portmapper on port 111. Binding
    port 111 needs root or `CAP_NET_BIND_SERVICE`.

  Both are off by default. NFSv3 mounting, reading and writing work without them; only
  locking needs them. If you don't need locks across clients, mount with `-o nolock`,
  or use NFSv4 (`dfsctl share mount --nfs-version 4.1`). See
  [the NFS guide](nfs.md#nfsv3-file-locking-nlmnsm).

### Does it support Kerberos authentication?

- **NFS:** not available yet on this version. The NFS adapter refuses Kerberos
  (RPCSEC_GSS). Use `AUTH_SYS` on a trusted network or a VPN.
- **SMB:** the released server supports Kerberos through SPNEGO, beside NTLM. See
  [Identity](identity.md).

### Can I write my own protocol adapter?

Yes, but the adapter interface is still changing. An adapter reaches files only
through the node's file service, never the stores directly, and the node starts,
reconfigures and stops it. See [Architecture](../internals/architecture.md).

### Can I write my own storage backend?

A metadata backend, yes: it must pass the metadata store's conformance suite. File
content has no pluggable backend today: the journal is local, and the released
server's remote stores were removed. See
[Implementing stores](../internals/implementing-stores.md).

### How does performance compare to kernel NFS?

There are no published numbers for this version. The benchmark tools and how to run
them are in [`bench/`](../../bench/README.md); measure with your own workload.

### Does metadata persist across server restarts?

Yes. The metadata lives in Badger on disk, in the directory `metadata.badger.path`
names; there is no in-memory mode. For a FoundationDB cluster instead, set
`metadata.backend: foundationdb`, which only a build with FoundationDB support opens
([Installation](install.md#foundationdb-metadata),
[Configuration](configuration.md#foundationdb-metadata)).

This version does not open a metadata store the released server wrote: see
[Upgrading from v1](upgrading-from-v1.md).

### Can I import an existing filesystem into DittoFS?

Not yet.

### Does DittoFS deduplicate data?

Not on this version. The released server split files into content-addressed chunks
and stored each chunk once; that block store was removed, and the journal stores
what is written as it is.

### How does garbage collection work?

The released server's collector went with its block and metadata stores, and its
`gc.*` and `integrity.*` keys are refused in the config file. See
[Garbage collection](garbage-collection.md) for what frees space today.

### How do I migrate an older store layout?

You can't, in place. This version reads none of the released server's block layouts:
not the `.blk` files from before v0.16, not standalone or packed CAS objects, and not
the `blobs/` and `logs/` cache. Copy the files back in through a protocol instead. See
[the migration guide](block-store-migration.md).

## Usage Questions

### Can I use this with Windows clients?

On the released server, yes. Windows connects over SMB with no extra software.
Windows 10 Pro and Enterprise also include an NFS client. See
[Windows clients](windows.md).

### How do I mount DittoFS shares?

Not served on this version. On the released server, for a share named `archive`:

```bash
# Linux (NFS)
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049 127.0.0.1:/archive /mnt/archive

# macOS (NFS)
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049,resvport 127.0.0.1:/archive /tmp/archive

# Any of them: let dfsctl build the command
dfsctl share mount archive /mnt/archive --protocol nfs
```

```powershell
# Windows (SMB), prompts for alice's password
net use Z: \\192.0.2.10\archive /user:alice
```

See the [NFS guide](nfs.md) and the [SMB guide](smb.md).

### Can I have multiple shares with different backends?

No. A node keeps every share in its one metadata store and its journal, so a share
does not name stores of its own. Shares differ in their export policy: client rules,
squash mode, read-only, and so on. The released server let each share name its own
stores; see [Upgrading from v1](upgrading-from-v1.md).

### Can multiple shares use the same metadata store?

They always do: one metadata store serves the whole node.

### Is there a recycle bin? Can I recover deleted files?

No. The released server's per-share recycle bin (`#recycle`) went with its metadata
store, and nothing replaces it yet.

### How do I enable debug logging?

Set the level in the environment for one run:

```bash
DITTOFS_LOGGING_LEVEL=debug dfs start
```

or in the config file:

```yaml
logging:
  level: debug
  format: text
```

The log goes to `~/.local/state/dittofs/dittofs.log` by default; see
[Troubleshooting](troubleshooting.md#where-the-log-is).

### Why do I get "permission denied" errors?

On the released server, the usual causes are:

1. **Squash mode:** for development, try
   `dfsctl share export-policy update archive --squash none`.
2. **No grant:** a share admits nobody until a grant names them;
   check `dfsctl grant list archive`.
3. **User ID mismatch:** your `id -u` must match the DittoFS user's `--uid`.
4. **Client rules:** check them with `dfsctl share export-policy get archive`.

See [Troubleshooting](troubleshooting.md#permission-issues).

## Comparison Questions

### How does DittoFS compare to a kernel NFS server?

| | Kernel NFS server | DittoFS |
|---|---|---|
| Runs in | the kernel | a userspace process |
| Protocols | NFS only; SMB needs Samba beside it | NFS and SMB over the same files (released server) |
| Metadata | the local filesystem | Badger on disk, or FoundationDB |
| Language | C | Go |
| Maturity | decades of production use | experimental |

### How does DittoFS compare to go-nfs?

Both are NFS servers in Go, with different goals. go-nfs is a library you embed in
your own program, with minimal configuration. DittoFS is a complete server: NFS and
SMB together, many shares, a management API and CLI, users and grants, and metrics.

## Durability and caching

**What durability does a write get?**

DittoFS follows the NFS and SMB durability contract, as a Linux kernel NFS server or
Samba does:

- A write is acknowledged quickly and is made durable when the client asks: at
  `COMMIT` over NFSv3, at `FLUSH` or `CLOSE` over SMB. That is what the protocols
  specify. The kernel server's default `sync` export and Samba's default
  `strict sync = yes` behave the same way. DittoFS does not do what the kernel
  server's unsafe `async` option does.
- At `COMMIT` or `FLUSH` the data is written and `fsync`'d to the journal on the
  node's local disk. A committed write survives a process crash or a power loss.
- On this version there is no remote copy: the journal holds the only copy of every
  byte. Losing the disk that holds the journal loses that data. The released server
  also uploaded data to S3 in the background.

See [Durability](durability.md).

**Do I need to configure durability?**

Usually not. The client already decides through the standard protocol, and DittoFS
does what it asks:

- An NFSv3 client marks each write `UNSTABLE` or `FILE_SYNC` and chooses when to
  `COMMIT`: on `fsync`, on `close`, or under cache pressure. For speed, let the client
  buffer and the application skip `fsync`. For safety, mount with `sync` or have the
  application call `fsync`.
- SMB clients do the same with the write-through flag and `FLUSH`.

**Caching and reads.** Warm reads come from the client's own page cache, as with any
NFS or SMB server, and from the node's journal. The usual client mount options apply:
`actimeo` for the attribute cache, `nconnect` for parallel connections, `rsize` and
`wsize` for transfer size.

## Known Limitations

### NFS Protocol Limitations

These limits come from the NFS protocols. Many are lifted by NFSv4. They describe the
released server, since this version serves no NFS yet.

#### ETXTBSY (text file busy)

| Status | Reason |
|--------|--------|
| Not supported | NFS protocol limitation |

An NFS server cannot know whether a client is running a file, so it cannot refuse
to overwrite it. Every NFS server has this limit. Most package managers remove and
replace an executable rather than overwrite it, so it rarely matters.

#### Share name length

| Status | Reason |
|--------|--------|
| At most 80 characters on this version | the SMB share-name limit |
| At most 27 bytes on the released server | an NFSv3 file handle is at most 64 bytes |

On this version a share name is 1 to 80 characters, none of them a control character
or one of `\ / : * ? " < > |`, and not `IPC$`.

The released server put the share name inside each NFS file handle, beside a
36-byte UUID and a separator, which left 27 bytes for the name, its leading `/`
included. It refused a longer name when the share was created.

#### Timestamps (year 2106)

| Status | Reason |
|--------|--------|
| NFSv3: up to 2106-02-07 | NFSv3 stores seconds in 32 bits |
| NFSv4: no practical limit | NFSv4 stores seconds in 64 bits |

#### File locking (NFSv3)

| Status | Reason |
|--------|--------|
| NFSv3: supported, opt-in | NLM and NSM over TCP and UDP; macOS and BSD clients need UDP and a portmapper on port 111 |
| NFSv4: supported | locking is part of the protocol |

NFSv3 mounting, reading and writing need no extra setup; only locking does. See
[Does it support file locking?](#does-it-support-file-locking)

#### Extended attributes

| Status | Reason |
|--------|--------|
| NFSv3, v4.0, v4.1: not supported | those versions have no xattr operations |
| NFSv4.2: supported | `user.*` names, values up to 64 KiB, 256 KiB per file in total |

Mount with `-o vers=4.2` and use `setfattr` and `getfattr`. Only the `user.*`
namespace is exposed.

- A value larger than 64 KiB is refused with `NFS4ERR_XATTR2BIG`.
- A file's attributes together, names and values, may take up to 256 KiB. Values are
  stored base64-encoded, which adds a third, so in practice about 192 KiB of values
  fit: two attributes at the 64 KiB maximum, or thousands of ordinary ones. A write
  past the limit is refused with `NFS4ERR_XATTR2BIG` and changes nothing; remove an
  attribute to make room.
- That is still much more than ext4 (one 4 KiB block per file) or NTFS (64 KiB - 5).
- NFS extended attributes and SMB named streams share one namespace, so a value set
  over one protocol can be read over the other.

See [the NFS guide](nfs.md#supported-versions).

#### fallocate and posix_fallocate

| Status | Reason |
|--------|--------|
| NFSv3: not supported | NFSv3 has no ALLOCATE operation |
| NFSv4.2: supported, best effort | ALLOCATE and DEALLOCATE |

Over NFSv3, space is allocated only when data is written.

Over NFSv4.2:

- `fallocate <file>` grows the file; the new range reads as zeros.
- `fallocate -p <file>` punches a hole: the range reads as zeros and its storage is
  freed.
- `lseek` with `SEEK_HOLE` and `SEEK_DATA` finds the next hole or data.

**ALLOCATE does not reserve space.** Storage is thin-provisioned, so DittoFS makes
the range readable and grows the file, but sets no space aside. Running out of space
shows up on a later write, as with any sparse file. The NFSv4.2 standard allows this.

#### Concurrent writers and the change attribute

| Status | Reason |
|--------|--------|
| NFSv4: `change` does not move during a write session | timestamps are held for the session, and `change` follows `ctime` |

While a client writes to a file, DittoFS keeps the file's `mtime` and `ctime` at the
value of the session's first write, instead of moving them on every write. The Linux
NFS client drops its cached pages whenever those timestamps move, so otherwise a
client would throw away its own cache after each write it made.

What this means for a second client reading the same file:

- The first write of a session moves `change`, so the second client sees it.
- Later writes in the same session do not move `change`. A second client that cached
  the file after the first write keeps serving that copy.
- A write that grows the file still moves `size`, which the Linux client also
  watches. A write that overwrites bytes in place moves nothing.
- When the session ends, `change` does not jump. It moves again on the first write of
  the next session.

The NFSv4 standard expects `change` to move on every update, so this is a deliberate
difference, not a coarse timestamp. It is why the pynfs test `WRT18` is listed as a
known difference. DittoFS runs on one node and the usual case is one writer per file.
If two clients must see each other's writes to the same file, have them use locks
(NFSv4 locks, or NLM over NFSv3) rather than poll attributes.

### SMB Client Limitations

#### macOS mount owner-only access

| Status | Reason |
|--------|--------|
| Handled by `dfsctl` | macOS lets only a mount's owner reach its files |

macOS lets only the user who made an SMB mount reach its files, whatever their
permissions. `dfsctl share mount` runs `mount_smbfs` as you, never through `sudo`, so
the mount is yours. Run `dfsctl` as yourself, not with `sudo`. See
[the SMB guide](smb.md).

### Storage Backend Limitations

#### Hard links

The metadata store supports hard links; no protocol serves them yet.

#### Special files

| Type | Status | Notes |
|------|--------|-------|
| Character devices | Metadata only | MKNOD creates the entry; it is not a device |
| Block devices | Metadata only | MKNOD creates the entry; it is not a device |
| FIFOs | Metadata only | MKNOD creates the entry; it is not a pipe |
| Sockets | Metadata only | MKNOD creates the entry; it is not a socket |

### General Limitations

#### Single node only

One node serves an installation:

- No clustering and no high availability.
- No replication: on this version the journal on the node's disks holds the only copy
  of the data.
- The node is a single point of failure.

#### Security

DittoFS is experimental and has had no security audit. See [Security](security.md).

### POSIX Compliance Summary

The released server passes **99.99%** of the
[pjdfstest](https://github.com/saidsay-so/pjdfstest) POSIX suite over NFSv3, with the
Badger metadata store: 8,788 of 8,789 tests. This version has not been measured,
since it serves no NFS yet.

| Metric | Value |
|--------|-------|
| Total tests | 8789 |
| Passed | 8788 |
| Failed (expected) | 1 |
| Pass rate | 99.99% |

#### Expected failures

| Test pattern | Reason |
|--------------|--------|
| `utimensat/09.t:test5` | NFSv3 32-bit timestamps (year 2106) |
| `open::etxtbsy` | NFS protocol limitation; not testable |
| `flock/*`, `fcntl/lock*`, `lockf/*` | NFSv3 locking is opt-in (UDP and a portmapper on port 111); use NFSv4 |
| `xattr/*`, `*xattr/*` | not in NFSv3 |
| `fallocate/*` | no ALLOCATE in NFSv3; supported over NFSv4.2 |
| `chflags/*` | BSD file flags, not supported; on Linux pjdfstest ends these early and reports them as passed |

Only `utimensat/09.t:test5` actually fails. The other patterns have no tests in the
suite, or their tests are skipped. The full list, with explanations, is in
`test/posix/known.yaml`.

## Still Have Questions?

- Browse the other guides in [docs/guide/](.).
- Search the [GitHub issues](https://github.com/cubbit/dittofs/issues).
- Open a [new issue](https://github.com/cubbit/dittofs/issues/new) for a bug or a
  feature request.
- To contribute, read [Contributing](../internals/contributing.md).
