# NFS Guide

This page shows you how to mount a DittoFS share over NFS from Linux, macOS or
Windows, which NFS version to pick, and how to control who can do what on the
share. Read it when you set up a client, or when a mount fails and you need to
know which option matters.

> Implementing or debugging the protocol? See [NFS protocol internals](../internals/nfs-protocol.md).

> **Not available yet on this version.** This version does not serve NFS. You
> cannot mount a share from it today:
>
> - The NFS adapter cannot be turned on: `dfsctl adapter set nfs --enabled` is
>   refused with `unknown_key`, because the node does not declare the adapter
>   settings yet.
> - Once it can be turned on, it still answers no NFS operation until its
>   request handlers are built.
> - A new installation cannot create a share yet either (see
>   [Getting started, step 5](getting-started.md#5-stores-and-namespaces)).
>
> The rest of this page describes NFS as the **released server** serves it, and
> uses this version's `dfsctl` commands. Each section that differs on this
> version says so at its top.

## Table of Contents

- [What works where](#what-works-where)
- [Quick start: mount a share](#quick-start-mount-a-share)
- [Supported Versions](#supported-versions)
- [Mount options explained](#mount-options-explained)
- [NFSv3 File Locking (NLM/NSM)](#nfsv3-file-locking-nlmnsm)
- [Embedded portmapper](#embedded-portmapper)
- [Identity squashing](#identity-squashing-root_squash-and-friends)
- [Permissions over NFS](#permissions-over-nfs)
- [Kerberos (sec=krb5)](#kerberos-seckrb5)
- [NFS over TLS](#nfs-over-tls)
- [Testing your mount](#testing-your-mount)
- [Common problems](#common-problems)
- [Glossary](#glossary)
- [References](#references)

---

## What works where

| Feature | Released server | This version |
|---------|-----------------|--------------|
| Serving NFS at all | Yes | **No**: the adapter cannot be enabled, and answers no operation yet |
| NFSv3, v4.0, v4.1, v4.2 | Yes | All four by default; the `adapters.nfs.versions` setting will narrow them (not settable yet) |
| Default port | 12049 | 12049 (`adapters.nfs.port`) |
| Kerberos (`sec=krb5`, `krb5i`, `krb5p`) | Yes | **No**: every Kerberos (RPCSEC_GSS) request is answered `AUTH_BADCRED` |
| NFS over TLS | Yes | **No**: any `adapters.nfs.tls` mode other than `off` is refused |
| Embedded portmapper | Yes, off by default | **No** |
| UDP (for NFSv3 lock callbacks) | Yes, off by default | **No**: NFS, MOUNT, NLM and NSM share one TCP port |

---

## Quick start: mount a share

DittoFS listens on port **12049**, not the standard 2049, so every mount names
the port. The easiest way is `dfsctl share mount`: it asks the server for the
share's path and the NFS port, builds the right `mount` command for your
operating system, and runs it.

Run `dfsctl` as yourself, **not** under `sudo`. It reads your login, and it runs
`mount` through `sudo` itself when that needs root. Add `--dry-run` to print the
command instead of running it. The dry run still contacts the server, to read
the share's path and the port.

The examples below assume you signed in to a server at `192.0.2.10` and the
share is `archive`, with NFS path `/archive`.

### Linux

```console
$ sudo mkdir -p /mnt/archive
$ dfsctl share mount archive /mnt/archive --protocol nfs --nfs-version 4.1 --dry-run
sudo mount -t nfs -o nfsvers=4.1,tcp,port=12049,actimeo=0 192.0.2.10:/archive /mnt/archive
```

Drop `--dry-run` to mount. To run the same mount by hand:

```bash
sudo mount -t nfs -o vers=4.1,tcp,port=12049 192.0.2.10:/archive /mnt/archive
```

NFSv3 needs two more options, `mountport` and `nolock`:

```console
$ dfsctl share mount archive /mnt/archive --protocol nfs --dry-run
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049,actimeo=0,nolock 192.0.2.10:/archive /mnt/archive
```

Without `--nfs-version`, `dfsctl` uses NFSv3.

### macOS

macOS has a mature NFSv3 client and a limited NFSv4 client, so NFSv3 is the
safer choice:

```console
$ mkdir -p ~/archive
$ dfsctl share mount archive ~/archive --protocol nfs --dry-run
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049,actimeo=0,resvport,locallocks 192.0.2.10:/archive /Users/alice/archive
```

By hand, with NFSv4 (macOS takes `vers=4`; older releases refuse a minor version
such as `4.1`):

```bash
sudo mount -t nfs -o vers=4,tcp,port=12049,resvport 192.0.2.10:/archive ~/archive
```

`mount -t nfs` and `mount_nfs` are the same on macOS: `mount` hands the work to
`mount_nfs`.

### Windows

The Windows NFS client can only mount from port **2049**. It has no option to
pick another port. With the adapter on its default port, `dfsctl` refuses:

```console
> dfsctl share mount archive Z: --protocol nfs --dry-run
Error: dfsctl share mount: the Windows NFS client mounts only from port 2049, and the NFS adapter serves 12049
```

To serve Windows NFS clients, run the adapter on 2049:

```bash
dfsctl adapter set nfs --enabled --setting port=2049
```

(On this version that command is refused with `unknown_key`, as noted at the
top.) With the adapter on 2049, the dry run prints:

```console
> dfsctl share mount archive Z: --protocol nfs --dry-run
mount -o mtype=hard,nolock \\192.0.2.10\archive Z:
```

SMB is the better protocol for Windows. See [Windows clients](windows.md).

### Unmount

```bash
sudo umount /mnt/archive      # Linux
sudo umount ~/archive         # macOS, or: diskutil unmount ~/archive
```

```cmd
umount Z:
```

> Mounting over **SMB** instead? See [Mounting SMB Shares](smb.md#mounting-smb-shares).
> To mount with **no `port=` option**, run DittoFS on the standard port 2049; see
> [Running on standard ports](install.md#running-on-standard-ports-production).
> To mount at boot, see [Mount at boot](persistent-mounts.md).

---

## Supported Versions

DittoFS implements **NFSv3**, **NFSv4.0**, **NFSv4.1** and **NFSv4.2**. The
client picks the version at mount time with `vers=` (or `nfsvers=`). On this
version, the `adapters.nfs.versions` setting limits the versions served; its
default, `3` to `4.2`, serves them all. It cannot be changed yet
(`unknown_key`).

| Version | What it adds |
|---------|--------------|
| NFSv3 | Works with every client. Locking needs a separate protocol (NLM). |
| NFSv4.0 | Locking and ACLs inside the protocol, one TCP port. |
| NFSv4.1 | Sessions, callbacks over the same connection, directory delegations. |
| NFSv4.2 | Sparse files (ALLOCATE, DEALLOCATE, SEEK, READ_PLUS), server-side clone (reflink), extended attributes. Copying between two servers (`OP_COPY`) is not implemented. |

### Which version should I use?

| You want… | Use | Why |
|-----------|-----|-----|
| The simplest setup | **NFSv4.1** | One TCP port, locking built in, nothing else to set up. |
| ACLs | **NFSv4.0 or later** | NFSv3 has no ACLs. |
| Sparse files or reflink | **NFSv4.2** | Those operations arrived in 4.2. |
| An old client, or macOS | **NFSv3** | Works everywhere. Locks stay on the client unless you set up NLM (see [below](#nfsv3-file-locking-nlmnsm)). |

**Rule of thumb:** use NFSv4.1 on Linux unless a client forces NFSv3.

Linux supports every version. macOS supports NFSv3 well; some releases go no
higher than NFSv4.0.

---

## Mount options explained

These are the options `dfsctl share mount` adds, and why:

| Option | When | Why |
|--------|------|-----|
| `nfsvers=` (or `vers=`) | Always | The NFS version the client asks for. |
| `tcp` | Always | DittoFS serves NFS over TCP only. Never add `udp`. |
| `port=12049` | Always | DittoFS does not listen on 2049 by default. |
| `mountport=12049` | NFSv3 | NFSv3 looks up the share with a separate MOUNT protocol. DittoFS serves it on the same port. NFSv4 has no MOUNT step. |
| `nolock` | NFSv3 on Linux | Keeps locks on the client. Without it, the client looks for a lock service through a portmapper on port 111 and the lock call hangs. |
| `locallocks` | NFSv3 on macOS | The macOS spelling of the same thing. |
| `actimeo=0` | Always | Turns off attribute caching, so a change made by one client shows at once on another. Raise it (for example `actimeo=3`) for speed when you can wait a few seconds. |
| `resvport` | macOS | Sends from a port below 1024, which some setups require. |

---

## NFSv3 File Locking (NLM/NSM)

> **Not available yet on this version.** This version runs no portmapper and no
> UDP listener, so NFSv3 locks cannot reach the server. Mount NFSv3 with
> `nolock` (Linux) or `locallocks` (macOS), or use NFSv4, whose locking needs
> none of this.

NFSv3 mounts, reads and writes need none of this section. Only byte-range locks
(`flock`, `fcntl`, `lockf`) do. NFSv3 sends locks over a separate protocol,
**NLM**, and uses **NSM** to clean up locks after a crash. NFSv4 has locking
built in.

If you do not need locks shared between clients, mount with `nolock`. If you do,
use NFSv4:

```bash
dfsctl share mount archive /mnt/archive --protocol nfs --nfs-version 4.1
```

### NFSv3 locks on the released server

The released server serves NLM versions 1, 3 and 4, both the synchronous calls
Linux uses and the asynchronous ones macOS and BSD use, and NSM version 1. NFS
data itself is TCP only.

A client finds the lock service by asking the portmapper on **port 111** of the
server. No mount option changes that. So locks without `nolock` need two
server settings, both off by default:

1. **UDP transport** (`udp_enabled`): macOS and BSD send lock calls over UDP.
   Linux does not need it, but it does no harm.
2. **A portmapper on port 111** (`portmapper_enabled`, `portmapper_port`).
   Binding port 111 needs root or `CAP_NET_BIND_SERVICE`, and clashes with a
   system `rpcbind` already on the host. On Kubernetes the operator exposes 111
   through the adapter's Service, so the pod needs no privileged port.

If the host already runs `rpcbind`, set `portmapper_register_with_system`
instead. DittoFS then registers NFS, MOUNT, NLM and NSM with that `rpcbind` at
startup, as the kernel's own NFS server does, and removes them at shutdown. If
no `rpcbind` answers, DittoFS logs a warning and serves without it.

On the released server they are flags of its NFS adapter settings command:
`--udp-enabled`, `--portmapper-enabled`, `--portmapper-port 111` for macOS
and BSD clients, or `--udp-enabled --portmapper-register-with-system` on a
host that already runs `rpcbind`. See the released server's CLI reference.

Restart the adapter after changing these settings. (This version has neither
the settings nor that command.)

> **Same-host clients:** if you mount an NFSv3 share on the machine that runs
> DittoFS, the kernel's own lock service takes over the lock registration, and
> locks go to the kernel instead of DittoFS. Mount from another host. See
> [NLM lock testing](../internals/testing.md#real-nfsv3-nlm-lock-testing-network-namespace-isolation).

---

## Embedded portmapper

> **Not available yet on this version.** This version has no portmapper. Mount
> with explicit ports, as the [quick start](#quick-start-mount-a-share) shows.

On the released server, a built-in portmapper lets clients and tools find the
DittoFS ports without a system `rpcbind`. It is off by default. When on, it
listens on port **10111** (TCP and UDP) unless you move it.

```bash
rpcinfo -p 192.0.2.10 -n 10111    # list the registered services
showmount -e 192.0.2.10           # list the shares (needs the portmapper on 111)
```

With the portmapper on port 111, a client finds every port by itself:

```bash
sudo mount -t nfs -o tcp 192.0.2.10:/archive /mnt/archive
```

For safety, it accepts registrations only from the local host, does not serve
the forwarding call (`CALLIT`) that can amplify attacks, and caps TCP
connections at 64. If it cannot start, for example because its port is taken,
NFS keeps serving; clients then need explicit ports.

---

## Identity squashing (root_squash and friends)

**Squashing** replaces the user ID a client claims with the anonymous user
before DittoFS checks permissions. Its main use is to stop `root` on a client
from acting as `root` on your files.

Squashing is set per share, in the share's export policy, and applies to every
protocol on the share. It changes who you are on the files. It never grants
access by itself.

### The three modes

| Mode | Effect | Classic NFS name |
|------|--------|------------------|
| `none` | IDs pass through unchanged. | `no_root_squash` |
| `root` **(default)** | Root (UID 0) becomes anonymous. Other IDs are unchanged. | `root_squash` |
| `all` | Every client ID becomes anonymous. | `all_squash` |

### Set it

```bash
# Map remote root to the anonymous user (the usual hardening choice)
dfsctl share export-policy update archive --squash root

# Map every client to the anonymous user (for example, a public read-only share)
dfsctl share export-policy update archive --squash all

# Check the current mode and the rest of the policy
dfsctl share export-policy get archive
```

The change applies to the next request, with no restart.

### Why squashing matters for NFS

- **AUTH_SYS**, the usual NFS login, trusts the client: a client that says it
  is UID 0 is UID 0. Squashing is the guard against that. The claimed ID is
  squashed before the share's grants are checked, so a grant on a claimed ID
  cannot be reached around the squash.
- **AUTH_NULL** (no credentials) always becomes the anonymous user, whatever the
  mode.
- **Kerberos** proves who the user is. The grant is checked against the proven
  user, then the squash applies. (Kerberos is not served on this version; see
  [Kerberos](#kerberos-seckrb5).)

No mode keeps root's administrator rights on a share. Admin access comes only
from a share grant ([access control](access-control.md)). A squashed caller acts
as the installation's anonymous user; you cannot choose another.

### Worked example

A file owned by UID 1000 with mode `0644`, on a share whose grant admits the
caller:

| Client acts as | Squash | Acts on the server as | Result |
|----------------|--------|-----------------------|--------|
| root (UID 0) | `none` | root | Full access. |
| root (UID 0) | `root` | anonymous | Can read the file, cannot write it. |
| UID 1000 | `root` | UID 1000 | Owner: can read and write. |
| UID 1000 | `all` | anonymous | Read only, even though it owns the file. |

---

## Permissions over NFS

Two checks decide every request:

1. **Share grants**: a grant per user or group (`dfsctl grant set`), checked on
   the server on every request. A user with no grant, directly or through a
   group, is refused.
2. **File permissions**: the file's mode bits and, where the protocol carries
   one, its ACL.

```bash
dfsctl grant set archive --user alice --access read-write
dfsctl grant list archive
```

**NFSv3 shows only mode bits, no ACL.** The Linux client checks those bits
itself, before it sends a request. So a grant cannot give a user more than the
mode bits show: if the share's root directory is not writable by alice's UID,
alice cannot write it over NFSv3, whatever her grant says. NFSv4 (and SMB)
carry the ACL, so an ACL can give alice write access the mode bits do not show.

**Rule of thumb:** for per-user access, use NFSv4.

A refused request shows as `Permission denied` (`EACCES`), never as an I/O
error.

---

## Kerberos (sec=krb5)

> **Not available yet on this version.** Every Kerberos request (RPCSEC_GSS) is
> answered `AUTH_BADCRED`, so a `sec=krb5`, `krb5i` or `krb5p` mount fails. For
> now, mount with AUTH_SYS (the default) and, if you need encryption on the
> wire, run NFS through a VPN or WireGuard tunnel.

With Kerberos, the server checks the client's ticket against its keytab and
maps the ticket's user to a DittoFS user. This works on NFSv4.0 and later.

```bash
# DittoFS listens on 12049, so name the port
sudo mount -t nfs4 -o vers=4.1,sec=krb5,port=12049 192.0.2.10:/archive /mnt/archive
```

Without `port=12049` the client connects to 2049 and fails with
`Connection refused` before Kerberos even starts.

### Choose which logins a share accepts

The share's export policy says which logins it accepts:

| Flag | Values | Meaning |
|------|--------|---------|
| `--flavor` | `none`, `sys`, `kerberos` | The login types offered to clients, in order of preference. |
| `--min-proof` | `none`, `claimed`, `secret`, `ticket` | The weakest proof of identity accepted. `claimed` is AUTH_SYS, `ticket` is Kerberos. |
| `--min-protection` | `none`, `integrity`, `privacy` | The weakest message protection accepted. `integrity` is `krb5i`, `privacy` is `krb5p`. |

```bash
# Kerberos only, with encryption
dfsctl share export-policy update archive --flavor kerberos --min-proof ticket --min-protection privacy

# Back to plain AUTH_SYS
dfsctl share export-policy update archive --flavor sys --min-proof none --min-protection none
```

The policy is checked on every request, on NFSv3 and NFSv4. A refused login
gets a wrong-security error (`NFS4ERR_WRONGSEC` on NFSv4), which tells the client
to retry with another login type. The same policy applies to SMB (see
[SMB share access policy](smb.md#share-access-policy)).

### "Access denied" after Kerberos succeeds

A Kerberos mount passes two steps:

1. **The ticket is checked** against the server's keytab.
2. **The user is authorized.** The ticket's name is mapped to a DittoFS user, a
   local one or one from the directory. A name that maps to nobody is refused,
   so the mount fails with `access denied` even though the ticket was good. A
   mapped user still needs a share grant.

A host that mounts with its **machine** ticket (no `kinit`, for example
`nfs/host.example.com@EXAMPLE.COM` from `/etc/krb5.keytab`) logs in as the
machine. A grant cannot name a machine, so mount with a user's ticket instead:
run `kinit alice@EXAMPLE.COM`, then mount.

---

## NFS over TLS

> **Not available yet on this version.** Any `adapters.nfs.tls` mode other than
> `off` is refused, and no certificate file is loaded, so NFS traffic is not
> encrypted. Use a VPN or WireGuard tunnel to protect it.

NFS over TLS (RFC 9289) encrypts NFS traffic with TLS 1.3. The client opens a
normal TCP connection, asks to upgrade, and both sides switch to TLS on the
same connection. The server needs no kernel TLS support or `tlshd`.

**On the released server**, TLS is set in the config file:

```yaml
adapters:
  nfs:
    tls:
      cert_file: /etc/dittofs/tls/tls.crt
      key_file:  /etc/dittofs/tls/tls.key
      client_ca: /etc/dittofs/tls/ca.crt   # optional: clients must present a certificate from this CA
      mode: opportunistic                   # opportunistic (default) | require
```

- `opportunistic` upgrades clients that ask and serves the rest in plain text,
  so you can roll TLS out without breaking mounts.
- `require` drops a connection that does not upgrade first.
- DittoFS only reads the certificate files; issuing and renewing them is your
  platform's job.

**On this version**, the same fields (`mode`, `cert_file`, `key_file`,
`client_ca`, with `off` as the default mode) are an adapter setting, not a
config file key: the config file refuses `adapters.nfs.tls`. They will be set
like this:

```bash
dfsctl adapter set nfs --enabled \
  --setting 'tls={"mode":"opportunistic","cert_file":"/etc/dittofs/tls/tls.crt","key_file":"/etc/dittofs/tls/tls.key"}'
```

Today that command is refused with `unknown_key`, and any mode but `off` would
be refused even once the setting can be written.

TLS 1.3 is the minimum; you cannot lower it.

**Clients:**

- **Linux:** `mount -o vers=4.1,xprtsec=tls …`. Needs `tlshd` (from
  `ktls-utils`) and a kernel with `CONFIG_NET_HANDSHAKE` (RHEL 9.x, or upstream
  6.7 and later).
- **macOS:** no NFS over TLS client. Use a tunnel.

---

## Testing your mount

Once mounted, try the common operations:

```bash
cd /mnt/archive
ls -la                     # list the directory
echo "test" > new.txt      # create and write a file
cat new.txt                # read it back
mkdir docs                 # make a directory
mv new.txt docs/           # rename
ln -s docs/new.txt link    # symbolic link
rm link docs/new.txt       # remove files
rmdir docs                 # remove the directory
```

To watch the server while you test, start it with debug logging:
`DITTOFS_LOGGING_LEVEL=DEBUG dfs start`.

### Tested mount options

These option sets are tested against the released server with the Connectathon
and NetApp `nfstest` suites:

| Set | Options |
|---|---|
| `default` | none: what a plain mount picks, NFSv4.2 against DittoFS |
| `v3` | `vers=3,tcp,mountproto=tcp,nolock` (attribute caching on) |
| `v4.0` | `vers=4.0` |
| `v4.1` | `vers=4.1` |
| `v4.1-noac` | `vers=4.1,noac` |
| `v4.1-smallio` | `vers=4.1,rsize=32768,wsize=32768` |

NFSv3 locking is tested separately. Known gaps and the lock tests are described
in [Testing DittoFS](../internals/testing.md#real-nfsv3-nlm-lock-testing-network-namespace-isolation).

---

## Common problems

- **`Connection refused` on mount.** DittoFS listens on **12049**, not 2049. Pass
  `port=12049` (and `mountport=12049` for NFSv3), or use `dfsctl share mount`.
- **The mount or a lock call hangs on NFSv3.** The client is looking for the
  lock service on port 111. Add `nolock` (Linux) or `locallocks` (macOS), or
  use NFSv4.
- **`Permission denied` over NFSv3 although the user has a read-write grant.**
  NFSv3 shows only mode bits, and the client checks them itself. Use NFSv4, or
  change the mode bits. See [Permissions over NFS](#permissions-over-nfs).
- **`access denied` after Kerberos succeeds.** The ticket's name maps to no
  DittoFS user, or that user has no share grant. See [Kerberos](#kerberos-seckrb5).
- **`NFS4ERR_WRONGSEC`.** The client used a login type the share does not
  accept. Check `--flavor`, `--min-proof` and `--min-protection` with
  `dfsctl share export-policy get archive`.
- **`dfsctl share mount` says "not under sudo".** Run it as yourself; it calls
  `sudo` itself.
- **Windows: the NFS mount fails.** The Windows client only mounts from port
  2049. See [Windows](#windows).

More in [Troubleshooting](troubleshooting.md). Every setting is in
[Configuration](configuration.md); hardening advice is in [Security](security.md).

---

## Glossary

| Term | Meaning |
|------|---------|
| **AUTH_NULL** | No credentials at all |
| **AUTH_SYS** (AUTH_UNIX) | The usual NFS login: the client states a UID and GIDs, unchecked |
| **Callback channel** | A connection the server uses to call the client back (NFSv4.1) |
| **COMPOUND** | One NFSv4 request carrying several operations |
| **Delegation** | Permission the server gives a client to cache a file or directory |
| **Export** | A directory shared over NFS (a share) |
| **File handle** | The opaque ID the server gives a file or directory |
| **NLM** | Network Lock Manager: how NFSv3 sends byte-range locks |
| **NSM / statd** | Network Status Monitor: tells the server a client rebooted, so its locks can be released |
| **Portmapper / rpcbind** | A service on port 111 that tells clients which port each RPC service uses |
| **RPCSEC_GSS** | The Kerberos login for NFS |
| **Session** | NFSv4.1's record of one client's connection state |
| **Stale handle** | A file handle the server no longer recognizes |
| **WCC** | Attributes before and after a change, which let an NFSv3 client check its cache cheaply |

Terms used by every protocol (SID, ACL, …) are in the project [Glossary](glossary.md).

---

## References

### Specifications

- [RFC 1813](https://www.rfc-editor.org/rfc/rfc1813) - NFS version 3
- [RFC 7530](https://www.rfc-editor.org/rfc/rfc7530) - NFS version 4.0
- [RFC 8881](https://www.rfc-editor.org/rfc/rfc8881) - NFS version 4.1
- [RFC 7862](https://www.rfc-editor.org/rfc/rfc7862) - NFS version 4.2
- [RFC 8276](https://www.rfc-editor.org/rfc/rfc8276) - Extended attributes in NFSv4
- [RFC 9289](https://www.rfc-editor.org/rfc/rfc9289) - NFS over TLS
- [RFC 2203](https://www.rfc-editor.org/rfc/rfc2203) - RPCSEC_GSS
- [RFC 1057](https://www.rfc-editor.org/rfc/rfc1057) - RPC and the portmapper
- [Open Group XNFS](https://pubs.opengroup.org/onlinepubs/9629799/) - NLM (chapter 10) and NSM (chapter 11)

### DittoFS documentation

- [Configuration](configuration.md)
- [Access control](access-control.md)
- [Security](security.md)
- [Troubleshooting](troubleshooting.md)
- [NFS protocol internals](../internals/nfs-protocol.md)
- [Architecture](../internals/architecture.md)
