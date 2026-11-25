# SMB

This page shows you how to mount a DittoFS share over SMB from Linux, macOS or
Windows, how users sign in, and how to turn on encryption and signing. Read it
when you set up an SMB client, or when a mount or a sign-in fails.

> **Contributors:** protocol internals, wire formats and key derivation are in
> [SMB protocol internals](../internals/smb-protocol.md).

> **Not available yet on this version.** This version does not serve SMB. You
> cannot mount a share from it today:
>
> - The SMB adapter cannot be turned on: `dfsctl adapter set smb --enabled` is
>   refused with `unknown_key`, because the node does not declare the adapter
>   settings yet.
> - Once it can be turned on, it answers every SMB command with
>   `STATUS_NOT_SUPPORTED` until its command handlers are built.
> - A new installation cannot create a share yet either (see
>   [Getting started, step 5](getting-started.md#5-stores-and-namespaces)).
>
> What does work today is the identity side: users, groups, grants, the
> directory and the domain join are all set through `dfsctl` and kept for when
> SMB is served. The rest of this page describes SMB as the **released server**
> serves it, and uses this version's `dfsctl` commands. Each section that
> differs on this version says so at its top.

## Table of Contents

- [What works where](#what-works-where)
- [Mounting SMB Shares](#mounting-smb-shares)
- [Dialects](#dialects)
- [Authentication](#authentication)
- [Share access policy](#share-access-policy)
- [Users, groups and grants](#users-groups-and-grants)
- [Encryption](#encryption)
- [Signing](#signing)
- [Leases and durable handles](#leases-and-durable-handles)
- [Feature support](#feature-support)
- [Testing your mount](#testing-your-mount)
- [Common problems](#common-problems)
- [Glossary](#glossary)
- [References](#references)

---

## What works where

| Feature | Released server | This version |
|---------|-----------------|--------------|
| Serving SMB at all | Yes | **No**: the adapter cannot be enabled, and answers every command `STATUS_NOT_SUPPORTED` |
| Default port | 12445 | 12445 (`adapters.smb.port`) |
| Dialects | SMB 2.0.2 to 3.1.1 | `adapters.smb.dialects`, from `2.1` to `3.1.1`; SMB 2.0.2 is not offered |
| Encryption, signing, leases, durable handles config blocks | In the config file | **Not read**: the config file refuses them as unknown keys |
| Per-share encryption | `--smb-encrypt` | `--smb-encrypt` on the export policy (kept for when SMB is served) |
| Users, groups, grants, directory, domain join | Yes | Yes, through `dfsctl` |

---

## Mounting SMB Shares

DittoFS listens on port **12445** by default, because port 445 needs root. Every
example below uses that port. To serve 445, so clients need no port, see
[Running on standard ports](install.md#running-on-standard-ports-production).

SMB always needs a user with a password (or a Kerberos ticket). Create one and
grant it the share first; see [Users, groups and grants](#users-groups-and-grants).

### The easy way: `dfsctl share mount`

`dfsctl share mount --protocol smb` builds the right command for your operating
system and runs it. Run it as yourself, **not** under `sudo`: it reads your
login, and runs `mount` through `sudo` itself where that needs root. The mount
command asks for the SMB password; `dfsctl` never sees it.

Pass `--username`: without it, `dfsctl` uses the account you signed in to
`dfsctl` with, which is usually `admin`. Add `--dry-run` to print the command
instead of running it. The dry run still contacts the server, to read the
share and the port.

The examples assume you signed in to a server at `192.0.2.10`, the share is
`archive`, and the SMB user is `alice`.

#### Linux

```console
$ sudo mkdir -p /mnt/archive
$ dfsctl share mount archive /mnt/archive --protocol smb --username alice --dry-run
sudo mount -t cifs -o vers=3.1.1,port=12445,username=alice,uid=1000,gid=1000 //192.0.2.10/archive /mnt/archive
```

What the options do:

| Option | Why |
|--------|-----|
| `vers=3.1.1` | The newest dialect, with the strongest encryption. SMB 3.0 is the first that can encrypt, which a share may require. |
| `port=12445` | DittoFS's default SMB port. |
| `username=alice` | The DittoFS user to sign in as. `mount.cifs` prompts for the password. |
| `uid=`, `gid=` | Your own IDs, so the mounted files belong to you locally and you can write without `sudo`. |

To keep the password out of the prompt, use a credentials file only you can read:

```bash
printf 'username=alice\npassword=ALICE_PASSWORD\n' > ~/.smbcredentials
chmod 600 ~/.smbcredentials
sudo mount -t cifs //192.0.2.10/archive /mnt/archive \
  -o port=12445,credentials=$HOME/.smbcredentials,vers=3.1.1,uid=$(id -u),gid=$(id -g)
```

Add `seal` to force encryption on this mount.

#### macOS

macOS lets **only the user who mounted** an SMB share reach its files, whatever
the file permissions say. So `dfsctl` runs `mount_smbfs` as you, never through
`sudo`. Mount to a directory you own:

```console
$ mkdir -p ~/archive
$ dfsctl share mount archive ~/archive --protocol smb --username alice --dry-run
mount_smbfs smb://alice@192.0.2.10:12445/archive /Users/alice/archive
```

`mount_smbfs` prompts for the password. A domain user is written `CORP;alice`
in the URL; `dfsctl` converts `--username 'CORP\alice'` for you.

To open the share in Finder instead: `open smb://alice@192.0.2.10:12445/archive`.

#### Windows

```console
> dfsctl share mount archive Z: --protocol smb --username alice --dry-run
net use Z: \\192.0.2.10\archive /user:alice * /TCPPORT:12445
```

The `*` makes `net use` prompt for the password. `/TCPPORT` exists only on
**Windows 11 24H2 and Windows Server 2025** or later, and needs an
administrator prompt. Older Windows can only reach DittoFS on port 445. See
[Windows clients](windows.md) for Explorer, PowerShell and Active Directory.

#### Unmount

```bash
sudo umount /mnt/archive       # Linux
diskutil unmount ~/archive     # macOS
```

```cmd
net use Z: /delete
```

### With `smbclient`

`smbclient` is handy for a quick check without mounting:

```bash
smbclient -L 192.0.2.10 -p 12445 -U alice                    # list shares
smbclient //192.0.2.10/archive -p 12445 -U alice -c "ls"      # list files
smbclient //192.0.2.10/archive -p 12445 -U alice              # interactive
```

To mount at boot, see [Mount at boot](persistent-mounts.md).

---

## Dialects

> **On this version:** the dialects served are the `adapters.smb.dialects`
> setting, from `2.1` to `3.1.1` by default. SMB 2.0.2 is not offered.

SMB picks the dialect by itself: client and server agree on the newest one both
support. Most clients need no version option.

| Dialect | What it adds |
|---------|--------------|
| SMB 2.0.2 | Basic file operations, HMAC-SHA256 signing (released server only) |
| SMB 2.1 | Leases, large reads and writes |
| SMB 3.0 | AES-128-CCM encryption, AES-128-CMAC signing |
| SMB 3.0.2 | Protection against dialect downgrade |
| SMB 3.1.1 | Stronger downgrade protection, AES-256 and AES-128 GCM/CCM encryption, GMAC signing |

SMB 3.1.1 is preferred: it is the most secure, and the fastest on CPUs with AES
instructions.

Pin a dialect only to troubleshoot:

| Client | How to pin | By default |
|--------|-----------|------------|
| Linux (`mount.cifs`) | `-o vers=3.1.1` (or `2.1`, `3.0`) | Negotiates 2.1 or newer |
| macOS (`mount_smbfs`) | No option | Negotiates up to 3.1.1 |
| Windows | No per-mount option | Negotiates up to 3.1.1 |
| `dfsctl share mount` | No option | Asks for `vers=3.1.1` on Linux; macOS and Windows negotiate |

---

## Authentication

Clients sign in with **Kerberos** or **NTLM**. The server offers both. A client
holding a Kerberos ticket for the server uses Kerberos; any other client uses
NTLM.

**No fallback to guest.** A failed sign-in (a wrong password, an unknown user,
an unreachable domain controller) is refused. DittoFS never turns it into a
guest session. A client gets a guest session only by asking for one.

### Kerberos

DittoFS checks the client's ticket against the keys it holds for `cifs/<host>`.
Those keys come from a domain join or from an imported keytab (see
[Identity](identity.md#part-c-configure-kerberos)). The keytab is not a config
file key: it is imported through the API, `PUT /api/v1/installation/keytab`.

The ticket's user becomes:

- the **local DittoFS user** of that name, if there is one;
- otherwise the **directory user** the connected directory returns. AD users
  need no local account;
- otherwise nobody, and the sign-in is refused.

Kerberos is the best choice for domain users: one round trip, and the client
also checks the server.

### NTLM

NTLM is used when Kerberos is not possible: no keytab, no ticket on the client,
or the client connected by IP address.

- **Local users** sign in with the password set by `dfsctl user set-password`.
- **AD domain users** sign in once the installation has joined the domain.
  DittoFS passes the user's answer to a domain controller over a secure channel,
  and the user gets the same identity as over Kerberos. Without a join, a domain
  user's NTLM sign-in fails. Join with `dfsctl domain join`; see
  [Identity: join the domain](identity.md#part-c2-join-the-domain) and the
  [Windows AD runbook](windows-ad-setup.md).

The `identity.ntlm` setting turns NTLM off for the whole installation:

```bash
dfsctl setting set installation identity.ntlm false
```

> The domain-controller pass-through has not been fully validated against a
> Samba AD domain controller yet.

---

## Share access policy

A share's export policy applies to SMB as it does to NFS: every request is
checked against it, whichever protocol carried it. Change it with
`dfsctl share export-policy update`:

| Flag | Values | Over SMB |
|------|--------|----------|
| `--min-proof` | `none`, `claimed`, `secret`, `ticket` | The weakest sign-in accepted. `secret` is NTLM, `ticket` is Kerberos. |
| `--min-protection` | `none`, `integrity`, `privacy` | The weakest protection accepted. `integrity` is signing, `privacy` is encryption. |
| `--client` | `CIDR=access` or `@netgroup=access` | Which client addresses may connect. The first rule that matches decides; no match refuses. |
| `--smb-encrypt` | (flag) | Encrypt every message on this share. |
| `--smb-hidden` | (flag) | Leave the share out of the share list clients browse. |

```bash
# Kerberos only, and encrypted
dfsctl share export-policy update archive --min-proof ticket --min-protection privacy

# Only the office network may connect
dfsctl share export-policy update archive --client 192.0.2.0/24=read-write

# Check the result
dfsctl share export-policy get archive
```

---

## Users, groups and grants

SMB uses the same users and groups as every DittoFS protocol. They live in the
metadata store and are managed with `dfsctl`, never in the config file.

```bash
dfsctl user create alice
dfsctl user set-password alice          # prompts for the password
dfsctl group create editors --gid 1001
dfsctl group add-member editors alice
dfsctl grant set archive --group editors --access read-write
```

`dfsctl user set-password` needs the node's protocol key (`keys.protocol` in the
config file), because the node stores the password's NT hash sealed under it.
See [Encryption: local key files](encryption.md#local-key-files).

Access levels, lowest to highest: `none`, `read`, `read-write`, `admin`. A user's
own grant decides; `none` refuses the user. With no grant of its own, its
groups' grants decide. A new share admits nobody until a grant names them.

---

## Encryption

> **On this version:** the `adapters.smb.encryption` block shown below is a
> released-server config key. This version's config file refuses it as an
> unknown key, and there is no setting for it yet. Per-share encryption,
> `--smb-encrypt`, is set and kept.

SMB 3 clients encrypt with AES-GCM or AES-CCM. When encryption is on, a packet
capture shows only ciphertext. SMB encrypts inside the protocol; it does not use
TLS. See [Security: SMB encryption](security.md#smb-encryption).

| Cipher | Used by default for |
|--------|---------------------|
| AES-128-CCM | SMB 3.0, 3.0.2 |
| AES-128-GCM | SMB 3.1.1 |
| AES-256-CCM, AES-256-GCM | When the client prefers them |

For SMB 3.1.1, DittoFS prefers AES-256-GCM, then AES-256-CCM, then the AES-128
ciphers.

### Server-wide mode (released server)

| Mode | Effect |
|------|--------|
| `disabled` | No session is encrypted. |
| `preferred` **(default)** | SMB 3 sessions are encrypted; SMB 2 clients still connect unencrypted. |
| `required` | SMB 2 clients are refused; every session is encrypted. |

```yaml
adapters:
  smb:
    encryption:
      encryption_mode: preferred   # disabled | preferred | required
```

### Per share

```bash
dfsctl share export-policy update sensitive --smb-encrypt
```

Every message on that share is then encrypted, whatever the server-wide mode.

**Guest sessions are never encrypted.** A guest has no session key to encrypt
with.

---

## Signing

> **On this version:** the `adapters.smb.signing` block below is a
> released-server config key, refused by this version's config file. To require
> signing on a share, use `--min-protection integrity` on its export policy.

Signing adds a tag to every message, so the receiver detects tampering.

| Dialect | Signing algorithm |
|---------|-------------------|
| SMB 2.0.2, 2.1 | HMAC-SHA256 |
| SMB 3.0, 3.0.2 | AES-128-CMAC |
| SMB 3.1.1 | AES-128-GMAC (preferred) or AES-128-CMAC |

An encrypted session is already tamper-proof; DittoFS signs it too, as Windows
Server does.

```yaml
adapters:
  smb:
    signing:
      enabled: true      # offer signing
      required: false    # require every client to sign
```

---

## Leases and durable handles

> **On this version:** the `adapters.smb.leases` and
> `adapters.smb.durable_handles` blocks below are released-server config keys,
> refused by this version's config file.

### Leases

SMB 2.1 and later clients get **leases**: permission to cache a file locally.

| Lease | The client may |
|-------|----------------|
| Read (R) | Cache what it read |
| Write (W) | Cache writes and send them later |
| Handle (H) | Keep the file open locally after the application closes it |

When another client (over SMB or NFS) opens the file in a conflicting way, the
server **breaks** the lease: the client sends its cached writes and lets go
before the other open goes ahead. **Directory leases** (SMB 3) let a client
cache a directory listing; any change in the directory breaks them.

```yaml
adapters:
  smb:
    leases:
      enabled: true
      directory_leases: true
      lease_break_timeout: 35s   # how long to wait for the client to let go
```

### Durable handles

A **durable handle** lets a client keep its open files through a short network
drop: it reconnects and carries on. The server keeps the handle for up to the
timeout. A durable handle does **not** survive a server restart.

```yaml
adapters:
  smb:
    durable_handles:
      enabled: true
      default_timeout: 60s
      scavenger_interval: 30s        # how often expired handles are cleaned up
      max_handles_per_session: 1000
```

---

## Feature support

What the released server supports:

| Feature | Supported |
|---------|-----------|
| SMB 2.0.2 to 3.1.1 | Yes |
| Kerberos and NTLM sign-in | Yes |
| Encryption (AES-128/256, GCM and CCM) and signing (HMAC, CMAC, GMAC) | Yes |
| Leases, directory leases, oplocks | Yes |
| Durable handles (v1 and v2) | Yes |
| Byte-range locks | Yes, shared and exclusive; a conflicting lock fails at once, it never waits |
| Change notifications | Partly |
| Server-side copy | Yes |
| Owner, group and DACL (Windows permissions) | Yes, see [ACL fidelity](smb-acl-fidelity.md) |
| Audit entries (SACL) | Stored and shown, but no audit event is ever generated |
| Cross-protocol lease and delegation breaks (SMB and NFS on one file) | Yes |
| SMB 1 | No (insecure) |
| Compression | No |
| Multichannel | No: one TCP connection per session |
| Persistent handles, clustering | No: single node only |
| RDMA, SMB over QUIC | No |
| DFS referrals | No |
| Encrypting single files | No: encryption is per session or per share |

Locks, oplocks and durable-handle state live in memory. A server restart drops
them.

---

## Testing your mount

```bash
cd /mnt/archive
ls -la                     # list the directory
echo "test" > new.txt      # create and write a file
cat new.txt                # read it back
mkdir docs                 # make a directory
mv new.txt docs/           # rename
rm docs/new.txt            # delete a file
rmdir docs                 # delete the directory
```

To watch the server while you test, start it with debug logging:
`DITTOFS_LOGGING_LEVEL=DEBUG dfs start`. The automated SMB tests are described
in [Testing DittoFS](../internals/testing.md).

---

## Common problems

### "Connection refused"

1. Check the server listens on the SMB port: `nc -vz 192.0.2.10 12445`.
2. Check the host firewall allows port 12445.
3. Pass the port: `port=12445` (Linux), `:12445` in the URL (macOS),
   `/TCPPORT:12445` (Windows).

### The sign-in fails

1. Check the user exists: `dfsctl user list`.
2. Set or reset the password: `dfsctl user set-password alice`.
3. Check the user, or one of its groups, has a grant: `dfsctl grant list archive`.
4. Check the share's policy accepts the sign-in type: `dfsctl share export-policy get archive`.
5. For Kerberos: check the keytab holds `cifs/` for the name the client used,
   and that the client reaches the KDC.
6. For an AD user over NTLM: check the installation has joined the domain.

### macOS

```bash
security delete-internet-password -s 192.0.2.10   # forget a saved password
mount | grep smbfs                                 # find stale mounts
umount -f ~/archive                                # force an unmount
```

### Linux

```bash
sudo apt-get install cifs-utils    # Debian, Ubuntu
sudo dnf install cifs-utils        # RHEL, Fedora
```

### SMB and NFS on the same files

See [Troubleshooting: cross-protocol issues](troubleshooting.md#cross-protocol-issues)
for files locked by the other protocol, slow lease breaks and stale data.

---

## Glossary

| Term | Meaning |
|------|---------|
| **ACL / ACE** | Access control list, and one allow or deny entry in it |
| **CIFS** | An old name for SMB |
| **DACL** | The part of a Windows security descriptor that grants or denies access |
| **Dialect** | An SMB protocol version, such as 3.1.1 |
| **Durable handle** | An open file a client can reclaim after a network drop |
| **Kerberos** | Ticket-based sign-in, used by Active Directory |
| **Lease** | Permission for a client to cache a file (read, write, handle) |
| **NTLM** | Microsoft's password challenge sign-in, used when Kerberos is not possible |
| **Oplock** | The older form of a lease |
| **SACL** | The audit part of a security descriptor |
| **Security descriptor** | A file's owner, group, DACL and SACL together |
| **SID** | Security identifier: a Windows user or group ID |
| **SPNEGO** | The wrapper that lets client and server choose Kerberos or NTLM |

Terms used by every protocol are in the project [Glossary](glossary.md).

---

## References

- [MS-SMB2](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/) - SMB 2 and 3
- [MS-NLMP](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-nlmp/) - NTLM
- [MS-DTYP](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-dtyp/) - SIDs, ACLs and security descriptors
- [RFC 4178](https://www.rfc-editor.org/rfc/rfc4178) - SPNEGO
- [RFC 4120](https://www.rfc-editor.org/rfc/rfc4120) - Kerberos 5
- [Samba](https://www.samba.org/) - SMB for Unix
