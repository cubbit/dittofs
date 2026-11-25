# Quotas

A quota caps how much one share may hold: how many bytes and how many files. Set one
when a share is used by several people and one of them must not fill it for everyone,
or when the share itself must stay under a size.

You manage quotas on a running node with `dfsctl quota` (or the management API). No
config file key sets them.

> **On this version:** this build serves no file protocol and cannot create a share yet
> (see [Getting Started](getting-started.md#5-stores-and-namespaces)). The commands below
> are the ones to use once a share exists; the refusals of bad values in
> [Values dfsctl refuses](#values-dfsctl-refuses) run today.

## What a quota limits

A quota is set on one of three things in a share:

| On | Flag | What it counts |
| --- | --- | --- |
| a user | `--user alice` | the files alice owns in that share |
| a group | `--group editors` | the files whose group is `editors` in that share |
| the share | neither flag | every file in the share |

Each quota has up to three limits, each on bytes and on files:

| Limit | Flags | What happens when it is passed |
| --- | --- | --- |
| hard | `--hard-bytes`, `--hard-files` | the write or create is refused at once |
| soft | `--soft-bytes`, `--soft-files`, `--grace` | allowed for the grace period, then refused like a hard limit |
| advisory | `--advisory-bytes`, `--advisory-files` | never refused |

A limit of `0` limits nothing. A user or group with no quota of its own has no limit;
there is no default quota for every user.

Every quota that covers a file applies. A write by alice in a share with a share quota,
a quota on alice and a quota on her file's group must fit under all three, and is
refused by the first one it would pass.

## Set a quota

Limit alice to 10 GiB and 100,000 files on the `archive` share:

```bash
dfsctl quota set archive --user alice --hard-bytes 10737418240 --hard-files 100000
```

```console
ON                                    HARD-BYTES   HARD-FILES  SOFT-BYTES  SOFT-FILES  GRACE  ADVISORY-BYTES  ADVISORY-FILES
8c814c1f-9930-4780-b8b5-b96002bba793  10737418240  100000      0           0           0s     0               0
```

`ON` is the ID of the user, group or share the quota is on. Sizes are in bytes:
1 GiB is `1073741824`.

Give the `editors` group a soft limit of 50 GiB that it may pass for a week:

```bash
dfsctl quota set archive --group editors --soft-bytes 53687091200 --grace 168h
```

Limit the share itself to 1 TiB, by naming no user or group:

```bash
dfsctl quota set archive --hard-bytes 1099511627776
```

`quota set` changes only the limits whose flags you give. Running it again with
`--hard-files 200000` raises the file limit and keeps the byte limit as it was. A quota
that does not exist yet is created.

## Read and remove quotas

```bash
dfsctl quota list archive
```

```console
ON                                    HARD-BYTES     HARD-FILES  SOFT-BYTES   SOFT-FILES  GRACE     ADVISORY-BYTES  ADVISORY-FILES
3e0d6a2c-5b1f-4c7a-9e8d-2f4b6a8c0e1d  1099511627776  0           0            0           0s        0               0
8c814c1f-9930-4780-b8b5-b96002bba793  10737418240    100000      0            0           0s        0               0
fd1591fa-f9b8-4e98-9536-9559fcbff2d7  0              0           53687091200  0           168h0m0s  0               0
```

The share's own quota comes first. `dfsctl user list` and `dfsctl group list` show
which name each ID belongs to. The list shows the limits, not how much is used: no
command shows a quota's current usage yet.

Remove alice's quota:

```bash
dfsctl quota remove archive --user alice
```

Remove the share's own quota with `dfsctl quota remove archive`.

## How the soft limit and grace work

1. alice's usage goes above her soft limit. Nothing is refused. The grace period starts
   from that moment.
2. While the grace period runs, alice may keep writing, up to her hard limit if she has
   one.
3. When the grace period ends and she is still above the soft limit, the soft limit
   acts as a hard one: her next write that adds bytes or files is refused.
4. When she deletes enough to drop back under the soft limit, the grace period resets.
   The next time she passes the limit, she gets a full grace period again.

A soft limit with a `--grace` of `0` is refused as soon as it is passed.

## What a client sees over a quota

> **Not available yet on this version:** no file protocol is served, so no client sees a
> quota refusal yet.

The node refuses the write as "quota exceeded" and names which quota it hit. NFS and
SMB are designed to report it like this:

| Quota hit | NFS client | SMB client |
| --- | --- | --- |
| a user's or a group's | `Disk quota exceeded` (`EDQUOT`) | `STATUS_DISK_QUOTA_EXCEEDED` |
| the share's own | `No space left on device` (`ENOSPC`) | `STATUS_DISK_FULL` |

A share quota looks like a full disk to the client because, for the client, the share is
the disk.

An advisory limit refuses nothing. Crossing it is recorded as a quota event, but no
command or API route shows those events yet, so an advisory limit has no visible effect
today.

## Values dfsctl refuses

`dfsctl` checks the values before it contacts the server.

A negative limit:

```console
$ dfsctl quota set archive --hard-bytes -1
Error: dfsctl quota set: --hard-bytes -1: want 0 or more
```

A grace period that is not a whole number of seconds, or is negative:

```console
$ dfsctl quota set archive --grace 1.5s
Error: dfsctl quota set: --grace 1.5s: want whole seconds, 0 or more
$ dfsctl quota set archive --grace -1h
Error: dfsctl quota set: --grace -1h0m0s: want whole seconds, 0 or more
```

`--grace` takes Go durations: `90s`, `30m`, `168h`. There is no `d` unit, so write a
week as `168h`.

`quota set` with no limit flag at all is refused too: give at least one of the
`--hard-*`, `--soft-*`, `--advisory-*` or `--grace` flags.

## Good to know

- **Usage follows the owner.** A file counts against its owner's quota and its group's
  quota. Changing a file's owner or group (`chown`, `chgrp`) moves its usage to the new
  owner or group.
- **Shares are counted apart.** alice's files in one share never count against her quota
  in another.
- **A limit can be passed by a little.** Writes landing at the same moment are checked
  against the usage committed so far, so a hard limit can be passed by what those
  concurrent writes add.
- **Usage cannot be rebuilt by hand.** It is kept up to date as files are written and
  removed. No command checks it against the files or rebuilds it.

## The management API

`dfsctl quota` uses these routes:

| Route | What it is |
| --- | --- |
| `GET /api/v1/shares/{id}/quota`, `PUT /api/v1/shares/{id}/quota` | the share's own quota |
| `GET /api/v1/shares/{id}/quotas` | the share's user and group quotas |
| `GET /api/v1/shares/{id}/quotas/{sub}`, `PUT /api/v1/shares/{id}/quotas/{sub}` | one user's or group's quota, by principal ID |

Reading a quota needs the `viewer` role; setting one needs `operator`. See
[Security](security.md#3-give-each-person-an-account-with-the-least-role-it-needs) for the roles.
