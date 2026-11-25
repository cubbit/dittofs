# Security Guide

**Current status:** DittoFS is experimental and has not undergone formal security auditing.
Exercise caution before deploying in production environments without thorough testing and review.

For the internal mechanism design and threat model, see
[`../internals/security-model.md`](../internals/security-model.md).

> **Not served on this branch.** This build serves no file protocol. The NFS and
> SMB sections below, and their `adapters:` settings, describe the released
> server.

## Table of Contents

- [What is implemented](#what-is-implemented)
- [Remaining limitations](#remaining-limitations)
- [Authentication](#authentication)
  - [NFS: RPCSEC\_GSS (Kerberos)](#nfs-rpcsec_gss-kerberos)
  - [NFS: AUTH\_UNIX](#nfs-auth_unix)
  - [NFS: AUTH\_NULL](#nfs-auth_null)
  - [SMB: Kerberos via SPNEGO](#smb-kerberos-via-spnego)
  - [SMB: NTLM and guest fallback](#smb-ntlm-and-guest-fallback)
  - [Active Directory and LDAP](#active-directory-and-ldap)
- [Message integrity and encryption](#message-integrity-and-encryption)
  - [SMB signing](#smb-signing)
  - [SMB encryption](#smb-encryption)
- [Access control](#access-control)
  - [NFSv4 ACLs](#nfsv4-acls)
  - [POSIX file permissions](#posix-file-permissions)
  - [Export-level access control](#export-level-access-control)
  - [IP-based restrictions](#export-level-access-control)
  - [Identity mapping](#identity-mapping)
  - [Read-only shares](#read-only-shares)
- [Network security](#network-security)
  - [NFS-over-TLS (RFC 9289)](#nfs-over-tls-rfc-9289)
  - [Control plane API TLS](#control-plane-api-tls)
  - [Network-level alternatives](#network-level-alternatives)
  - [Firewall configuration](#firewall-configuration)
- [Kerberos configuration](#kerberos-configuration)
  - [Server configuration](#server-configuration)
  - [Keytab management](#keytab-management)
  - [NFS client setup](#nfs-client-setup)
  - [SMB client setup](#smb-client-setup)
- [Production hardening checklist](#production-hardening-checklist)
- [Secure configuration example](#secure-configuration-example)
- [Planned security features](#planned-security-features)
- [Reporting security issues](#reporting-security-issues)

---

## What is implemented

- Kerberos authentication for NFS via RPCSEC_GSS (RFC 2203)
- Kerberos authentication for SMB via SPNEGO
- Active Directory / LDAP idmap (`idmap_ad` RFC2307 attributes or `idmap_rid` RID derivation), with Kerberos keytab and NTLM machine-account pass-through
- SMB3 encryption with AES-128-GCM, AES-128-CCM, AES-256-GCM, AES-256-CCM
- SMB3 signing with AES-128-CMAC and AES-128-GMAC; SMB 2.x signing with HMAC-SHA256
- SMB 3.1.1 preauth integrity (SHA-512 hash chain) for downgrade protection
- SP800-108 key derivation for per-session cryptographic keys
- VALIDATE_NEGOTIATE_INFO for SMB 3.0/3.0.2 downgrade detection
- NFSv4 ACL-based access control
- POSIX file permission enforcement (owner/group/other)
- Export-level IP-based access restrictions
- Identity mapping (root squash, all squash)
- AUTH_UNIX for trusted-network deployments
- TLS for the management API (`api.tls.*` certificate and key files), reloaded when they change; no mutual TLS
- API accounts with a role (`viewer`, `operator`, `admin`) and an audit record of every management action

## Remaining limitations

- No formal security audit performed
- NFS-over-TLS (RFC 9289) is not served yet: every adapter TLS mode but `off` is refused until StartTLS is built. Once served, it requires a client that supports RFC 9289 (`xprtsec=tls`); macOS has no such client (use a network tunnel instead)
- No encryption at rest
- No audit logging for file operations

---

## Authentication

### NFS: RPCSEC_GSS (Kerberos)

> **Not served yet.** v2 answers every RPCSEC_GSS call with `AUTH_BADCRED` until its GSS stage
> is built, so a `sec=krb5`, `krb5i` or `krb5p` mount fails today. The rest of this section
> describes the design and cannot be used yet.

DittoFS implements RPCSEC_GSS per RFC 2203, enabling Kerberos-based strong authentication for
NFSv4 clients. This is the recommended authentication method for any deployment outside of a
fully trusted network.

When enabled, clients authenticate using Kerberos tickets. The server validates tickets against
its keytab and maps Kerberos principals to Unix UID/GID for authorization decisions.

Key properties:

- Mutual authentication (server identity is also verified by the client)
- Cryptographic credential verification (no trust-based UID spoofing)
- Configurable context lifetime and clock skew tolerance
- Hot-reload support for keytab rotation without server restart

See [Kerberos configuration](#kerberos-configuration) for setup instructions.

### NFS: AUTH_UNIX

AUTH_UNIX is the traditional NFS authentication mechanism. The client provides UID, GID, and
supplementary GIDs with each request. The server trusts these values without independent
verification.

- Suitable for trusted networks only
- Clients can impersonate any user by sending arbitrary UID/GID values
- Use identity mapping (root squash, all squash) to limit exposure

### NFS: AUTH_NULL

AUTH_NULL provides anonymous access with no authentication. All requests are treated as coming
from an unauthenticated user. Use with extreme caution and only for public read-only shares.

### SMB: Kerberos via SPNEGO

The SMB adapter supports Kerberos authentication through SPNEGO during `SESSION_SETUP`. When a
keytab is imported or the installation has joined a domain, SMB clients authenticate using
Kerberos tickets. Otherwise a client signs in with NTLM, while `identity.ntlm` is on.

See [Kerberos configuration](#kerberos-configuration) for shared setup that applies to both NFS
and SMB.

### SMB: NTLM and guest fallback

**Guest sessions:** a failed logon is refused and never mapped to guest. A session that
proves nothing holds no session key, so it can neither sign nor encrypt; a share's
`--min-proof` refuses it.

**NTLM fallback** occurs when Kerberos keytab is not configured, the client has no valid TGT, or
DNS resolution prevents Kerberos service ticket acquisition.

NTLM authenticates two classes of user:

- **Local DittoFS users:** the NTLMv2 response is validated directly against the user's NT hash,
  kept sealed in the metadata store. `identity.ntlm` turns NTLM off for the installation.
- **AD domain users:** once the installation has joined the domain, the
  challenge/response is forwarded to the Domain Controller over a sealed NETLOGON secure channel
  (MS-NRPC sign+seal AES, post-ZeroLogon). The DC performs the validation; no local NT hash is
  stored or used. **Without a join, domain-user NTLM fails with
  `STATUS_LOGON_FAILURE`.** A domain user is never mapped to guest.

> **Machine secret sensitivity:** the computer account's password is sealed at rest by the
> secret provider, like the LDAP bind password and the Kerberos keytab, and is never returned by
> the API. It rotates by itself. Kerberos keys for the account's service names are
> derived from it, and **AES only** (`msDS-SupportedEncryptionTypes` `0x18`): DES and RC4 keys
> are refused.
>
> **Joining has two operator duties the code cannot check**, listed in
> [identity.md, Part C2](identity.md#part-c2-join-the-domain).

NTLM security tradeoffs:

- No mutual authentication (client cannot verify server identity)
- Vulnerable to relay attacks without channel binding
- Session key derived from password hash (weaker than Kerberos session key)
- Signing uses HMAC-SHA256 (weaker than AES-CMAC/GMAC)
- NETLOGON passthrough requires a joined computer account

**Recommendation:** configure Kerberos for all production deployments. Use NTLM only as a
transition mechanism or for clients that cannot use Kerberos.

### Active Directory and LDAP

DittoFS can authenticate Active Directory domain users directly, with no local DittoFS
account for them. Three pieces are independent and can be enabled in any combination:

| Piece | What it does | Credential at rest |
|---|---|---|
| **Kerberos service keytab** | Verifies AD-issued service tickets over SMB (SPNEGO) and NFS (RPCSEC\_GSS) | Keytab file |
| **Machine account** (a domain join) | Forwards the NTLM challenge/response to the Domain Controller over a sealed NETLOGON channel, for clients that cannot use Kerberos | Computer account secret |
| **LDAP idmap** | Maps the authenticated principal or SID to a Unix UID/GID and resolves nested group membership | Read-only bind DN + password |

Security notes:

- Once joined, AD-user NTLM is validated by the Domain Controller —
  **no local NT hash is stored or used**. Without one, domain-user NTLM fails with
  `STATUS_LOGON_FAILURE`.
- The keytab and the computer account's password are sealed at rest by the secret provider,
  write-only, and never returned by the API. Joining and its operator duties are in
  [identity.md, Part C2](identity.md#part-c2-join-the-domain).
- The LDAP bind account is read-only; reach the directory over LDAPS or a private network.
- A principal is resolved through the same idmap regardless of the protocol it arrived on,
  so an AD user gets one Unix identity across SMB and NFS.

See [identity.md](identity.md) for the provider reference and
[windows-ad-setup.md](windows-ad-setup.md) for the end-to-end operator runbook.

---

## Message integrity and encryption

### SMB signing

DittoFS supports SMB message signing, providing integrity protection against man-in-the-middle
attacks and message tampering.

| Algorithm | Dialect | Notes |
|-----------|---------|-------|
| HMAC-SHA256 | SMB 2.x | Legacy |
| AES-128-CMAC | SMB 3.0+ | SP800-108 derived key |
| AES-128-GMAC | SMB 3.1.1 | Preferred; leverages GCM hardware acceleration |

Signing behavior:

- **Enabled** (default `true`): server advertises signing capability during `NEGOTIATE`.
- **Required** (default `false`): when `true`, the server rejects unsigned messages from
  established sessions.

The two states above are stored as one tri-state SMB adapter setting
(`disabled` | `enabled` | `required`); this build has no command to change it.

**Recommendation:** set `required: true` for all production deployments to prevent tampering even
when encryption is not used.

### SMB encryption

SMB3 provides AEAD encryption delivering both confidentiality and integrity for all messages on
an encrypted session.

**Supported cipher suites:**

| Cipher | Dialect | Performance |
|--------|---------|-------------|
| AES-128-GCM | 3.1.1 (default) | Fastest (AES-NI + CLMUL) |
| AES-128-CCM | 3.0/3.0.2 (default) | Good (AES-NI) |
| AES-256-GCM | 3.1.1 | Fast, higher security |
| AES-256-CCM | 3.0+ | Highest security AES-CCM variant |

**Encryption modes:**

- **`disabled`**: no encryption. Suitable for testing only. `dfs` logs a startup warning when
  SMB is bound to a non-loopback address in this mode.
- **`preferred`** *(shipped default)*: SMB 3.x sessions are encrypted; SMB 2.x sessions are
  still accepted. Secure by default while remaining wire-compatible with SMB 2.x.
- **`required`**: only encrypted SMB 3.x clients can connect. SMB 2.x clients are rejected at
  `NEGOTIATE`. Recommended for production with sensitive data.

**Per-share encryption:** individual shares can require encryption with
`dfsctl share export-policy update <share> --smb-encrypt`. This forces encryption on that tree regardless of the
global mode.

These are SMB adapter settings, records under `adapters.smb`, not keys of the
config file, which refuses them; the API does not declare them yet.

See [`../internals/security-model.md`](../internals/security-model.md) for cipher/key derivation
internals and the full transport security comparison table.

---

## Access control

### NFSv4 ACLs

DittoFS supports NFSv4 ACL-based access control with:

- Per-user and per-group access control entries (ACEs)
- Explicit ALLOW and DENY entries evaluated in order
- Granular permission bits (read data, write data, append, execute, delete, read/write
  attributes, read/write ACL, etc.)
- Inheritance flags for directories

ACLs are enforced at the metadata layer. DENY entries take precedence when encountered before a
matching ALLOW entry, following the NFSv4 specification. When both NFSv4 ACLs and POSIX
permissions are present, ACLs take precedence for NFSv4 operations.

### POSIX file permissions

Traditional Unix file permissions are enforced at the metadata layer: owner, group, other
permission bits are checked in that order. When Kerberos is enabled, the Kerberos principal is
mapped to a UID/GID before the permission check.

### Export-level access control

Client access is restricted per share by attaching a **netgroup** — a named list of
IP addresses, CIDR ranges and hostnames. A share with a netgroup accepts only
clients whose address matches a member; a share with no netgroup accepts any
client.

A netgroup is created with `dfsctl netgroup create` and named in a share's
export policy as a client rule:

```bash
dfsctl netgroup create trusted-net --member 192.168.1.0/24 --member build01.example.com
dfsctl share export-policy update export --client @trusted-net=read-write
```

The netgroup is enforced on both NFS versions and on SMB, on paths that do not
share code:

- **NFSv3** — checked at MOUNT and refused with `MNT3ERR_ACCES`. The check runs
  **only** at mount time; the per-operation path re-checks the auth-flavor
  policy but not the client allowlist.
- **NFSv4** — there is no MOUNT, so the check runs per request instead: when a
  share handle enters a compound (PUTFH, or a LOOKUP crossing an export
  junction out of the pseudo-fs) and again whenever an operation builds an auth
  context. A client outside the allowlist gets `NFS4ERR_ACCESS`.
- **SMB**: checked at TREE_CONNECT and again on every request on the tree,
  against the address of the connection carrying it, and refused with
  `STATUS_ACCESS_DENIED`. Attaching or changing the share's netgroup applies
  to the next request; an edit to the netgroup's members applies once the
  cached verdict for that address expires (30 seconds). See
  [SMB § Share access policy](smb.md#share-access-policy).

**Consequence for tightening an export.** Because v4 re-checks per request,
adding or narrowing a netgroup takes effect on a v4 client's next operation,
including one already holding a file handle. On v3 it takes effect for new
mounts only — an already-mounted v3 client keeps working until it remounts.
Remount is what re-reads the allowlist on v3.

Which authentication flavors an export accepts is a separate policy from the
client allowlist. Unlike the address gate, the flavor gate *is* re-checked per
operation on both versions: set it with `--flavor`, `--min-proof` and
`--min-protection` on the same `dfsctl share export-policy update` command, and see
[configuration](configuration.md) for the Kerberos floor.

### Identity mapping

Squash is part of a share's export policy: `none`, `root` or `all`. A squashed
caller acts as the policy's anonymous principal.

A claimed identity (AUTH_SYS) is squashed before its share grant is read, since
any client can claim any uid. A proven identity (Kerberos, NTLM) is squashed after
its grant is read, so the grant still names who the proof established.

All squash — every client's *file-level* identity is anonymous:

```bash
dfsctl share export-policy update export --squash all
```

Root squash — root (UID 0) is mapped to anonymous for file checks, everyone else passes through:

```bash
dfsctl share export-policy update export --squash root
```

No squashing — client UIDs pass through unchanged; use only on trusted networks
or with Kerberos:

```bash
dfsctl share export-policy update export --squash none
```

The modes are `none`, `root` and `all`. No mode keeps root's administrator
rights: admin access to a share comes only from a share grant.

### Read-only shares

```bash
dfsctl share export-policy update export --read-only
```

A read-only share fails write and create operations with `EROFS`
(`ErrReadOnly`), distinct from the `EACCES` a permission denial returns.

---

## Network security

### NFS-over-TLS (RFC 9289)

NFS-over-TLS is **not served yet**. The adapter's TLS settings exist, but v2 refuses every `mode`
except `off` until StartTLS is built, so NFS data travels in cleartext today (see
[Network-level alternatives](#network-level-alternatives)). What follows describes the design, and
none of it can be used yet.

The design is the opportunistic `AUTH_TLS` STARTTLS upgrade from RFC 9289: a client sends
a `NULL` RPC with `auth_flavor = AUTH_TLS`, the server replies with the `"STARTTLS"` verifier,
and both perform a TLS 1.3 handshake on the same connection, and all subsequent traffic is
encrypted. The handshake runs in userspace (no kernel TLS / `tlshd` on the server). The adapter's
TLS settings (`mode`, `cert_file`, `key_file`, `client_ca`) are settings, not host facts:
`adapters.nfs.tls` in the config file is refused, so they are set through the management API.
Setting `client_ca` makes the server require and verify a client certificate (mutual TLS) on NFS.
Mutual TLS is not available on the REST API.

- **`opportunistic`** (future) still serves non-TLS clients in plaintext (smooth rollout).
- **`require`** (future) rejects plaintext connections until they upgrade.
- Future: DittoFS only loads cert files; issuance and renewal are the platform's
  responsibility.
- **Client support (future):** Linux `mount -o vers=4.1,xprtsec=tls` needs `tlshd` + `CONFIG_NET_HANDSHAKE`
  (RHEL 9.x / kernel 6.7+). macOS has no NFS-over-TLS client — use a VPN tunnel.

### Control plane API TLS

The management API (default `127.0.0.1:8080`, plus a local socket) carries the highest-value credentials in the
system. It binds to `127.0.0.1` (loopback only) by default — a fresh `dfs start` does not
expose credentials off-host.

Any deployment that accepts connections from another machine (multi-host, Kubernetes) should set
`api.listen` to a non-loopback address and pair it with TLS. The API serves an address that
is not loopback only over TLS, so a start without both files is refused:

```yaml
api:
  listen: ["0.0.0.0:8080"]
  tls:
    cert_file: /etc/dittofs/tls/tls.crt
    key_file: /etc/dittofs/tls/tls.key
```

DittoFS does not generate self-signed certificates or perform ACME. The API has no mutual TLS
and no minimum-version key; see [Configuration](configuration.md#the-keys) for the keys it
reads. The node reloads the certificate files when they change.

**Recommended deployment model:**

- **Kubernetes / internet-facing edge:** terminate TLS at an ingress, service mesh, or reverse
  proxy (NGINX) in front of DittoFS.
- **Direct `dfsctl` access / non-Kubernetes hosts:** use DittoFS native TLS. `dfsctl` presents
  no client certificate: it trusts the server through `--cacert` and signs in with a password or
  an API token. A credentials file from an older `dfsctl` loses its `client_cert`,
  `client_key`, `expires_at` and `preferences` fields on the next save, and one holding
  `tls_skip_verify: true` is refused until that field is removed.

`dfsctl` TLS is captured once at login and reused by every later command:

```bash
# Private CA
dfsctl login --server https://host:port --cacert /etc/dittofs/tls/ca.crt -u admin

dfsctl share list   # reuses the stored CA — no flags needed
```

The login stores the CA bundle's file path (not its contents); each command re-reads it, so
cert-manager rotation is picked up automatically.

See [`./configuration.md`](./configuration.md) for the full option reference.

### Network-level alternatives

Because NFS-over-TLS is not served yet, NFS data travels in cleartext. Use one of:

1. **WireGuard (recommended):** set up a WireGuard VPN between client and server, then mount
   over the VPN interface.

2. **IPsec:** configure an IPsec tunnel so NFS traffic flows through an encrypted tunnel.

3. **SSH tunnel:**
   ```bash
   ssh -L 12049:localhost:12049 user@server
   sudo mount -t nfs -o nfsvers=4,tcp,port=12049 localhost:/export /mnt/test
   ```

### Firewall configuration

Restrict access to DittoFS ports:

```bash
# Linux (iptables)
sudo iptables -A INPUT -p tcp --dport 12049 -s 192.168.1.0/24 -j ACCEPT
sudo iptables -A INPUT -p tcp --dport 12049 -j DROP

# Linux (firewalld)
sudo firewall-cmd --permanent --add-rich-rule='rule family="ipv4" source address="192.168.1.0/24" port protocol="tcp" port="12049" accept'
sudo firewall-cmd --reload

# macOS (pf) — add to /etc/pf.conf:
# pass in proto tcp from 192.168.1.0/24 to any port 12049
# block in proto tcp from any to any port 12049
sudo pfctl -f /etc/pf.conf
```

---

## Kerberos configuration

Kerberos serves the SMB adapter (SPNEGO). The NFS adapter does not serve
RPCSEC_GSS on this branch yet: it refuses it with `AUTH_BADCRED`. NFS gains
Kerberos when the adapter's GSS stage lands (RFC 21).

### Server configuration

Kerberos is not configured in the config file: a `kerberos` or `ldap` block there
refuses the start. The installation's keytab is a secret of the management API,
imported with `PUT /api/v1/installation/keytab` and named by the `identity.keytab`
setting; the directory is the `identity.directory` setting. Both are written by an
`admin`. The clock skew is fixed at 5 minutes. See [Identity](identity.md).

### Keytab management

The node seals the keytab in the metadata store when it is imported, and opens it
under the protocol wrapping key (`keys.protocol`). A copy left on disk after the
import is no longer read, so remove it or keep it readable only by its owner
(`chmod 600`). A joined installation derives its keys from the computer account's
password instead, and rotates that password itself.

Create a keytab with MIT Kerberos:

```bash
# On the KDC or using kadmin
kadmin -q "addprinc -randkey nfs/server.example.com@EXAMPLE.COM"
kadmin -q "ktadd -k /etc/dittofs/dittofs.keytab nfs/server.example.com@EXAMPLE.COM"

chmod 600 /etc/dittofs/dittofs.keytab
```

### NFS client setup

```bash
# Mount with Kerberos (krb5 security flavor)
sudo mount -t nfs -o sec=krb5,nfsvers=4,tcp,port=12049 server.example.com:/export /mnt/secure

# Verify mount
mount | grep /mnt/secure

# Ensure client has a valid ticket
kinit user@EXAMPLE.COM
klist
```

### SMB client setup

SMB clients that support Kerberos (Windows, smbclient, CIFS kernel module) automatically
negotiate Kerberos via SPNEGO during session setup when the client has a valid TGT.

```bash
# Linux: mount with Kerberos
sudo mount -t cifs -o sec=krb5,vers=3.0 //server.example.com/export /mnt/secure

# smbclient with Kerberos
smbclient -k //server.example.com/export
```

---

## Production hardening checklist

- [ ] Enable Kerberos authentication for NFS and SMB
- [ ] Enable SMB3 encryption with `encryption_mode: required` for sensitive data
- [ ] Enable SMB message signing with `required: true`
- [ ] Deploy behind VPN for NFS data confidentiality (NFS-over-TLS is not served yet)
- [ ] Restrict export access by IP address (attach a netgroup to each share)
- [ ] Use root squash on all exports (`--squash root`)
- [ ] Configure NFSv4 ACLs for fine-grained access control
- [ ] Use read-only exports where writes are not needed
- [ ] Keep the management API on loopback, or give it a certificate (`api.tls.*`)
- [ ] Give each operator an API account with the least role it needs, and read the audit (`dfsctl audit list`)
- [ ] Delete the keytab file once it is imported
- [ ] Enable Prometheus metrics and monitor failed authentication attempts
- [ ] Keep DittoFS updated; apply patches promptly

## Secure configuration example

```yaml
logging:
  level: warn
  format: json
  output: /var/log/dittofs/security.log

api:
  listen: ["127.0.0.1:8080"]

# Shares, their squash, the keytab and the directory are records managed
# through the management API, not keys of this file. For example:
#
#   dfsctl share create export --path /export --namespace <namespace-id> --flavor sys --client 0.0.0.0/0=read \
#     --read-only --squash root
```

See [`./configuration.md`](./configuration.md) for the full configuration reference.

---

## Planned security features

- [ ] Encryption at rest for content stores
- [ ] Encrypted metadata storage
- [ ] Audit logging for file operations (management actions and sign-ins are audited)
- [ ] Integration with SIEM systems

---

## Reporting security issues

If you discover a security vulnerability in DittoFS:

1. **Do not** open a public GitHub issue.
2. Email security concerns to the maintainers (see repository for contact).
3. Include: description of the vulnerability, steps to reproduce, potential impact, and suggested
   fixes if any.

We will acknowledge receipt within 48 hours and provide a timeline for a fix.
