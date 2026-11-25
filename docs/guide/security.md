# Security

This page is a checklist for securing a DittoFS installation. Walk it before anyone but
you can reach the node: each item says what to do, with the command or config to copy.

DittoFS is experimental and has had no security audit. Do not rely on it to protect
data that matters without your own review. For how the mechanisms work inside, see
[the security model](../internals/security-model.md).

## Not available yet on this version

Know these gaps before you plan a deployment:

- **No file protocol is served.** This build serves neither NFS nor SMB yet (see
  [Getting Started](getting-started.md#5-stores-and-namespaces)). The NFS and SMB items
  below describe how to set the share up for when they are.
- **No NFS Kerberos.** A `sec=krb5`, `krb5i` or `krb5p` mount fails: the server answers
  every Kerberos (RPCSEC_GSS) request with `AUTH_BADCRED`. Kerberos works for SMB.
- **No NFS-over-TLS.** Every NFS TLS mode except `off` is refused, so NFS traffic is
  not encrypted. Run NFS over a VPN or an SSH tunnel instead (see
  [Protect NFS traffic](#protect-nfs-traffic)).
- **No mutual TLS on the management API.** The API does not ask `dfsctl` for a client
  certificate. `dfsctl` signs in with a password or an API token.
- **No SMB signing or encryption settings.** The server-wide SMB signing and encryption
  modes cannot be changed on this version. Per share, `--smb-encrypt` works (see
  [SMB encryption](#smb-encryption)).
- **File content is not encrypted at rest.** Put the journal and metadata directories
  on an encrypted filesystem (see [Encryption](encryption.md)).
- **File operations are not audited.** The audit log records management actions and
  sign-ins to the API, not reads and writes of files.

## The checklist

- [ ] [1. Keep the management API on loopback, or give it TLS](#1-keep-the-management-api-on-loopback-or-give-it-tls)
- [ ] [2. Change the admin password](#2-change-the-admin-password)
- [ ] [3. Give each person an account with the least role it needs](#3-give-each-person-an-account-with-the-least-role-it-needs)
- [ ] [4. Use API tokens for scripts, with an expiry](#4-use-api-tokens-for-scripts-with-an-expiry)
- [ ] [5. Shorten sessions if you need to](#5-shorten-sessions-if-you-need-to)
- [ ] [6. Read the audit log](#6-read-the-audit-log)
- [ ] [7. Turn off people who leave](#7-turn-off-people-who-leave)
- [ ] [8. Protect the key files and the passphrase](#8-protect-the-key-files-and-the-passphrase)
- [ ] [9. Lock down each share](#9-lock-down-each-share)
- [ ] [10. Firewall the ports](#10-firewall-the-ports)

### 1. Keep the management API on loopback, or give it TLS

The management API holds every credential of the installation. By default it listens
on `127.0.0.1:8080` and on a local socket, so nothing off the host can reach it.

To reach it from another machine, give it a certificate. The node refuses to start with
an address off loopback and no certificate:

```yaml
# /etc/dittofs/config.yaml
api:
  listen: ["0.0.0.0:8080"]
  tls:
    cert_file: /etc/dittofs/tls/tls.crt
    key_file: /etc/dittofs/tls/tls.key
```

Without the `tls` block, `dfs config validate` says:

```console
$ dfs config validate
Error: /etc/dittofs/config.yaml: api.tls.cert_file: required, api.listen has an address off loopback
/etc/dittofs/config.yaml: api.tls.key_file: required, api.listen has an address off loopback
```

DittoFS does not make certificates. Bring your own (a private CA, cert-manager, ACME).
The node reloads the files when they change, so a renewed certificate needs no restart.
The metrics listener (`metrics.listen`) uses the same certificate when it is off
loopback.

Sign in from another machine with the CA that signed the certificate. `dfsctl`
remembers it for later commands:

```bash
dfsctl login --server https://dfs.example.com:8080 --username admin --cacert /etc/dittofs/tls/ca.crt
dfsctl share list     # no TLS flags needed now
```

`dfsctl` stores the CA file's path, not its contents, and reads it again on each
command, so a rotated CA file is picked up. A host name is never treated as loopback:
plain `http://` works only with `127.0.0.1` or `::1`.

In Kubernetes, or behind a reverse proxy, you can end TLS at the ingress instead.

### 2. Change the admin password

If `dfs init` generated the admin password, the account can do nothing but change it
until you do:

```bash
dfsctl login --server http://127.0.0.1:8080 --username admin
dfsctl password change
```

Changing a password ends the account's other sessions. If you lose the admin password,
stop the node and run `dfs reset-admin` on its host: it prints a new password once,
which must be changed at the next sign-in.

### 3. Give each person an account with the least role it needs

Do not share the `admin` account. Create an account for each person, with a role:

| Role | Can |
| --- | --- |
| `viewer` | read shares, stores, settings, operations, and users and groups that hold no role; cannot change anything |
| `operator` | also create and change shares, grants, export policies, quotas, netgroups, stores, settings, and users and groups that hold no role |
| `admin` | also manage accounts that hold a role, other accounts' API tokens, the installation (such as rotating the signing key) and the identity secrets, and read the audit log |

```bash
dfsctl user create carl --role viewer
dfsctl user set-password carl
```

```console
NAME  ID                                    UID  ROLE    EMAIL  DISABLED
carl  ea66f7c8-d0c7-4512-8e0d-0d71a11e0e5f       viewer         no
```

A user with a role and no `--uid` can use the management API but owns no files. A user
with a `--uid` and no role is a file user only. One account can be both.

### 4. Use API tokens for scripts, with an expiry

A script or a CI job should sign in with an API token, not a person's password. Make it
a **service account**, a user with a role and no password, and give it a token:

```bash
dfsctl user create ci-bot --role operator
(umask 077; dfsctl token create --user ci-bot --expires-in 720h > token.txt)
```

```console
API token 7a6bb95a-075f-e685-39f0-3bfc3c57c945, expiring 2026-12-25T10:28:04+01:00; it is not shown again
```

The token itself goes to `token.txt`; the line above goes to the terminal. Use it from
the script:

```bash
dfsctl login --server https://dfs.example.com:8080 --token-file token.txt -c ci
dfsctl share list
```

List and revoke tokens:

```bash
dfsctl token list --user ci-bot
dfsctl token revoke 7a6bb95a-075f-e685-39f0-3bfc3c57c945 --user ci-bot
```

```console
ID                                    CREATED               EXPIRES
7a6bb95a-075f-e685-39f0-3bfc3c57c945  2026-11-25T09:28:04Z  2026-12-25T09:28:04Z
```

A token lives at most `api.token_max_lifetime`, a year by default. An admin can lower
it; a longer `--expires-in` is then refused:

```bash
dfsctl setting set installation api.token_max_lifetime 2160h   # 90 days
```

```console
$ dfsctl token create --expires-in 100000h
Error: dfsctl token create: --expires-in 100000h0m0s: the server refused the expiry it gives, which must fall after the server's time and within its api.token_max_lifetime; ...
```

Setting a user's password (`dfsctl user set-password`) cancels all of that user's
sessions and API tokens.

If you think the key that signs sign-in sessions leaked, rotate it. A new key signs
from then on, and what the old key signed stops being accepted once it expires, about
15 minutes later:

```bash
dfsctl installation rotate-signing-key --wait
```

To cut off one leaked API token, revoke it with `dfsctl token revoke` instead.

### 5. Shorten sessions if you need to

A `dfsctl login` session lasts `api.session_lifetime`, 12 hours by default. `dfsctl`
refreshes it on its own until then. To make people sign in more often:

```bash
dfsctl setting set installation api.session_lifetime 8h
```

### 6. Read the audit log

Every management action is recorded: who did it, to what, and whether it was applied
or refused. Only an `admin` can read it.

```bash
dfsctl audit list --from 2026-11-25T00:00:00Z
```

```console
TIME                  ACTOR  TARGET                                                  OUTCOME
2026-11-25T09:27:54Z  admin  session:a686d111-aaaf-3d63-4ade-bcb36a223f5d            applied
2026-11-25T09:28:00Z  admin  user:8c814c1f-9930-4780-b8b5-b96002bba793               applied
2026-11-25T09:28:00Z  admin  share:                                                  refused: invalid_argument
2026-11-25T09:28:04Z  admin  api-token:<token-id>                                    applied
```

Narrow it to one person or one thing:

```bash
dfsctl audit list --actor alice
dfsctl audit list --target user:8c814c1f-9930-4780-b8b5-b96002bba793
```

Records are kept for `audit.retention`, 400 days by default and at least 90. Watch for
`refused` outcomes you do not expect.

### 7. Turn off people who leave

Disable an account at once. It is refused on every protocol and on the API from its
next request:

```bash
dfsctl user update alice --disabled
```

Re-enable it with `--disabled=false`, or delete it with
`dfsctl user delete alice --force --wait`.

### 8. Protect the key files and the passphrase

The node seals its secrets (password hashes, the signing key, NT hashes, the keytab,
the directory's bind password) under the key files that `keys.storage` and
`keys.protocol` name. Each key file is protected by the passphrase in
`DITTOFS_ENCRYPTION_PASSPHRASE`.

- Copy the key files off the host and keep the passphrase somewhere safe. Losing either
  loses what they seal, and nothing recovers it.
- Keep the key files readable by the `dfs` user only (`chmod 600`).
- Pass the passphrase through your service manager's secret mechanism, not a shell
  history.

See [Encryption](encryption.md) for KMIP and key rotation.

### 9. Lock down each share

[Access Control](access-control.md) has the commands. In short, for each share:

- Grant only the users and groups who need it, at the lowest level that works
  (`dfsctl grant set archive --group auditors --access read`).
- Allow only the client addresses that need it
  (`dfsctl share export-policy update archive --client 192.0.2.0/24=read-write`). Use
  addresses, not host names.
- Require a real sign-in (`--min-proof secret` or `ticket`), so guests are refused.
- Make shares read-only where nobody needs to write (`--read-only`).
- Encrypt SMB traffic to shares holding sensitive data (`--smb-encrypt`).

### 10. Firewall the ports

Open the file-protocol ports only to the clients that use them, and the API port only to
the machines that manage the node. The defaults are NFS `12049`, SMB `12445` and API
`8080`.

```bash
# Linux (iptables): NFS from the office network only
sudo iptables -A INPUT -p tcp --dport 12049 -s 192.0.2.0/24 -j ACCEPT
sudo iptables -A INPUT -p tcp --dport 12049 -j DROP

# Linux (firewalld)
sudo firewall-cmd --permanent --add-rich-rule='rule family="ipv4" source address="192.0.2.0/24" port protocol="tcp" port="12049" accept'
sudo firewall-cmd --reload
```

## Protect NFS traffic

NFS-over-TLS is not served on this version, so NFS data crosses the network in clear
text. Carry it through an encrypted tunnel:

1. **WireGuard** (recommended): connect client and server over WireGuard and mount
   through the tunnel's address.
2. **IPsec**: route NFS traffic through an IPsec tunnel.
3. **SSH tunnel**, for one client:

   ```bash
   ssh -L 12049:localhost:12049 alice@dfs.example.com
   sudo mount -t nfs -o nfsvers=4,tcp,port=12049 localhost:/archive /mnt/archive
   ```

macOS has no NFS-over-TLS client at all, so on macOS a tunnel is the only option even
once the server supports it.

## SMB sign-in and encryption

SMB accepts these sign-ins:

- **Kerberos**, once a keytab is imported or the installation has joined a domain. This
  is the strongest choice. See [Identity](identity.md).
- **NTLM with a password** of a DittoFS user, or of a domain user once the installation
  has joined the domain. NTLM has no mutual authentication and is weaker than Kerberos.
  An admin can turn it off for the installation with
  `dfsctl setting set installation identity.ntlm false`. That also deletes every stored
  NT hash, so users need a new password (`dfsctl user set-password`) if you turn it
  back on.
- **Guest.** A failed sign-in is refused, never turned into a guest session. A guest
  session can neither sign nor encrypt; `--min-proof secret` refuses it on a share.

### SMB encryption

SMB 3 can encrypt every message of a session (AES-128-GCM, AES-128-CCM, AES-256-GCM or
AES-256-CCM). The server-wide encryption mode cannot be changed on this version. To
require encryption on one share:

```bash
dfsctl share export-policy update archive --smb-encrypt
```

See [SMB](smb.md) for client support.

## Kerberos keytab

The Kerberos keytab is imported through the management API and stored sealed. The copy
on disk is no longer read after the import, so delete it or keep it `chmod 600`. A
joined installation needs no keytab for its own names and rotates its computer
account's password by itself. [Identity](identity.md#import-the-keytab) has the steps.

## Reporting security issues

If you find a vulnerability in DittoFS:

1. Do **not** open a public GitHub issue.
2. Email the maintainers (see the repository for contact details).
3. Include what you found, how to reproduce it, its impact, and a fix if you have one.

We acknowledge reports within 48 hours and give a timeline for the fix.
