# Durability

This page says what happens to your data when the server crashes or loses power: which
writes survive and which may be lost. Read it before you put data on DittoFS that you
cannot recreate.

> **On this version:** this build serves no file protocol, so no client writes to it
> yet. What follows is how the node stores what it is given. Every byte stays on the
> node's own journal disks: nothing is copied to object storage (S3) or to another node
> yet, and no setting changes how durable a write is.

## In short

| What happened to the write | Survives a process crash | Survives power loss or a kernel crash | Survives losing the journal disk |
| --- | --- | --- | --- |
| written, not yet committed | not promised | not promised | no |
| committed (`fsync`, NFS `COMMIT`, SMB `FLUSH`, or a stable write) | yes | yes | no |
| a create, rename, delete or attribute change, once answered | yes | yes | no |

A write is **committed** when the application asks for it to be on disk and the server
answers. Until then, the server holds it but promises nothing about a crash.

## Writing a file

When an application writes to a file, the node stores the bytes in the share's
**journal**, a set of files on the disks the `journals` key of the config file lists.
It answers the write as soon as the journal holds the bytes. It does not wait for them
to reach the disk.

```yaml
# /etc/dittofs/config.yaml
journals:
  - path: /var/lib/dittofs/journal
```

So a crash right after a write can lose that write, unless it was committed.

## Committing a write

The application decides when a write must be safe. It does that with `fsync` (or
`fdatasync`) on Linux and macOS, or `FlushFileBuffers` on Windows. The client turns
that into an NFS `COMMIT` or an SMB `FLUSH`. The node then syncs the file's data in the
journal to the disk and only answers once it is there.

For example, on a mounted share:

```bash
# cp alone: the copy may still be lost in a crash
cp report.pdf /mnt/archive/

# cp, then sync the file: once sync returns, the copy survives a crash or power loss
cp report.pdf /mnt/archive/ && sync /mnt/archive/report.pdf
```

`sync` with a file name needs GNU coreutils 8.24 or later. Many applications, such as
databases and editors, call `fsync` themselves when they save.

A client can also send a write as **stable** (an NFS `FILE_SYNC` write, or an SMB
write-through write). The node answers such a write only after it is committed.

Closing a file does not commit it by itself. On Linux the NFS client usually flushes
and commits a file's data when the file is closed, so data an application closed is
normally committed, but the server only promises what was committed.

## Creating, renaming and deleting

A create, rename, delete or attribute change (`chmod`, `chown`, `touch`) is a change to
the metadata store, not to the journal. The node writes it to disk before it answers,
so once the client sees it succeed, it survives a crash or a power loss.

## What a disk loss does

The journal is the only copy of a file's data on this version. If a journal disk dies,
the data written to it is lost, committed or not. To protect against that:

- Put the journal on a mirrored device (RAID 1, a mirrored ZFS pool, or a replicated
  cloud volume).
- Keep a copy of important data somewhere else. DittoFS has no snapshots or backups yet
  (see [Snapshots](snapshots.md)).

## Settings

None. Earlier releases had per-share and per-store durability settings (`commit_ack`,
`relaxed_metadata_commit`, `relaxed_durability`, `blockstore.journal.dirty_expire`).
This version has none of them: the config file refuses
`blockstore.journal.dirty_expire`, and no `dfsctl` command or API route sets the
others. There is also no timer that commits written data in the
background: data that is never committed stays uncommitted until the application
commits it.

## See also

- [Choosing stores](choosing-stores.md): where the metadata store and journals live
- [Configuration](configuration.md#the-keys): the `journals` key
- [Upgrading from v1](upgrading-from-v1.md): what changed from the earlier releases
