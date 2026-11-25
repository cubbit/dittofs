# Quotas

DittoFS enforces **per-identity** quotas on a share — by user, by group, or on the
share itself — limiting both **bytes** and **inodes (file count)**. Each limit
has a **soft** threshold, a **hard** ceiling, and a **grace** window between them.

> Quotas are managed live on a running server via `dfsctl` (or the REST API). There is no
> config-file or Kubernetes CRD form.

## Set a quota

```bash
# A user: 10 GiB / 100k files hard, soft at 8 GiB / 90k files,
# 7-day grace before the soft limits are refused.
dfsctl quota set export --user alice \
    --hard-bytes 10737418240 --soft-bytes 8589934592 \
    --hard-files 100000 --soft-files 90000 --grace 168h

# A group: 50 GiB, no file-count limit.
dfsctl quota set export --group editors --hard-bytes 53687091200

# The share itself, with no --user or --group:
dfsctl quota set export --hard-bytes 5368709120
```

Limits are in bytes and files; 0 limits nothing. A flag not given keeps that
limit's value.

## Inspect and remove

```bash
dfsctl quota list export
dfsctl quota remove export --user alice
```

## How enforcement works

- **Soft → grace → hard.** When an identity crosses its soft threshold, a grace timer
  starts; once the grace window elapses the soft limit is enforced as hard. Drop back
  under soft and the timer resets. Default-user grace is tracked per-user and survives a
  server restart.
- **Most-specific wins.** A user is limited by its own quota if set, else its group's;
  the share's own quota bounds the share as a whole.
- **Usage is by owner, within one share.** Bytes and inode counts are keyed by share and
  by file owner UID/GID, and stored durably alongside the files they account for — updated
  in the same transaction that writes the file, and read back at startup rather than
  recomputed, so start-up cost does not grow with the number of files. A `chown` moves a file's
  usage between identities. Every share lives in the node's one metadata store, but
  each share's usage is counted separately: one share's bytes never count against
  another share's quota, and `df` on a share reports only that share.
- **Best-effort.** Under heavy concurrent writes an identity may briefly exceed a limit
  before usage catches up — normal for a userspace NFS/SMB server.

Quota limits live in the metadata store, beside the node's other records (there is no
separate control-plane database), and are also reachable via the REST API at
`/api/v1/shares/{id}/quotas`.

## Repairing a drifted usage figure

Usage counters are maintained transactionally as files are written and removed, so they
are normally already correct. The CLI no longer has a command to check them against the
file rows or rebuild them, and nothing runs a rebuild automatically.
