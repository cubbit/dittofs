# Identity: Active Directory, LDAP, Kerberos and NTLM

This page shows how to let the users of an Active Directory (or another LDAP directory)
use DittoFS with their own accounts, with no local DittoFS user for each of them. It
covers connecting the directory, importing a Kerberos keytab and joining the domain.
You need it when your users already have directory accounts; for a handful of local
users, `dfsctl user create` is enough (see [Access Control](access-control.md)).

A directory user gets one Unix identity (uid and gid) that is the same over SMB and NFS,
so a file alice creates from Windows shows alice as its owner from Linux too.

For a step-by-step Windows Server walkthrough, see
[windows-ad-setup.md](windows-ad-setup.md). This page is the reference for every setting
it uses.

> **On this version:**
>
> - This build serves no file protocol yet, so nobody mounts a share. The directory,
>   keytab and join steps below run today.
> - NFS Kerberos is not served: the NFS server answers every Kerberos (RPCSEC_GSS)
>   request with `AUTH_BADCRED`, so `sec=krb5` mounts fail. Kerberos works for SMB.
> - The Kubernetes operator does not run this release (see
>   [Part D](#part-d-kubernetes-operator-dittoserver-cr)).
> - The directory's bind password is sealed under `keys.protocol`, so add that key to
>   the config file before you set it (see [Encryption](encryption.md)).

## What you set up, and why

| Piece | What it does | Set with |
| --- | --- | --- |
| **The directory** | Finds a user or group by name or SID, gives it a uid or gid, and lists its groups, nested ones included | `dfsctl setting set installation identity.directory.*` |
| **A Kerberos keytab** | Lets clients sign in with a Kerberos ticket for a service name such as `cifs/files.example.com` | the management API (`PUT /api/v1/installation/keytab`) |
| **A domain join** | Gives DittoFS a computer account. Its service names accept Kerberos tickets, and it lets domain users sign in over NTLM | `dfsctl domain join` |

You can use the directory alone, or with a keytab, a join, or both. Each part below is
one of them. All of these settings are written by an `admin` account and are applied
without a restart. None of them goes in the config file: an `ldap:` or `kerberos:` block
there is refused.

---

## Part A: A test domain to try it on

To try this without a production domain, the repository has a Samba AD domain
controller for testing, in `test/integration/ad-dc/` (Docker) and `k8s/samba-ad-dc/`
(a dev Kubernetes cluster). It is for testing only: in production you point DittoFS at
your existing Active Directory and install no Samba.

The test domain has realm `DITTOFS.AD` and NetBIOS domain `DITTOFS`, and contains:

| Account | What it is |
| --- | --- |
| `alice` | user with `uidNumber 10001`, `gidNumber 10000`, member of `devs` (nested in `engineering`) |
| `bob` | user with no uid in the directory, so DittoFS allocates one; member of `engineering` |
| `dittofs-svc` | service account holding `cifs/dittofs.dittofs.ad` and `nfs/dittofs.dittofs.ad`, AES keys only; its keytab is written to `/keytabs/dittofs.keytab` |
| `dittofs$` | computer account for joins (password `MachinePass01!`) |

The users' password is `TestPassword01!` and Administrator's is `Passw0rd!2024`. The
container also writes its CA certificate to `/keytabs/ca.pem`, which you need for
`identity.directory.trust_roots` below. Set `AD_REALM`, `AD_DOMAIN`,
`AD_ADMIN_PASSWORD` and `USER_PASSWORD` to change the defaults. The container must run
privileged, since it binds ports 53, 88, 389, 464 and 636.

The test domain's NetBIOS name, `DITTOFS`, is also DittoFS's default server name
(`identity.netbios_name`), and the two must differ. Before you connect it, give the
server another name:

```bash
dfsctl setting set installation identity.netbios_name FILES
```

Contributors run the integration suite against this domain on a Linux Docker host with
`go test -tags=ad_dc -timeout 20m ./test/integration/ad-dc/`. Its containers and
networks are named with the prefix `dittofs-ad-dc-test`; on a Docker host shared with
others, set `DITTOFS_AD_DC_NAME_PREFIX` to use another.

The examples below use a production-style domain, `EXAMPLE.COM` (NetBIOS `EXAMPLE`),
with a domain controller `dc1.example.com` and DittoFS at `files.example.com`.

---

## Part B: Connect the directory

1. Set the directory's address first; the other settings are refused until it is set.
   Then the rest:

   ```bash
   dfsctl setting set installation identity.directory.address ldaps://dc1.example.com:636
   dfsctl setting set installation identity.directory.base 'DC=example,DC=com'
   dfsctl setting set installation identity.directory.realm EXAMPLE.COM
   dfsctl setting set installation identity.directory.netbios_domain EXAMPLE
   dfsctl setting set installation identity.directory.bind_name 'CN=svc-dittofs,CN=Users,DC=example,DC=com'
   dfsctl setting set installation identity.directory.bind_credential "$BIND_PASSWORD"
   dfsctl setting set installation identity.directory.trust_roots -- "$(cat /etc/dittofs/directory-ca.pem)"
   ```

   ```console
   KEY                         VALUE                          APPLIED
   identity.directory.address  "ldaps://dc1.example.com:636"
   ```

   The `--` before the certificate is needed because a PEM file starts with dashes.
   `APPLIED` fills in, as `1/1`, once the node has applied the setting.

2. Test the connection:

   ```bash
   dfsctl domain test --wait
   ```

   It binds to the directory and reports the result. With no directory set, it fails
   with `no directory is configured on this node`.

3. Look a user up:

   ```bash
   dfsctl domain resolve 'EXAMPLE\alice' --wait
   dfsctl domain resolve --sid S-1-5-21-1004336348-1177238915-682003330-1105 --wait
   ```

The directory settings:

| Key | Meaning |
| --- | --- |
| `identity.directory.address` | `ldaps://host[:port]`, or `ldap://host[:port]`, which is always upgraded with StartTLS before the bind. Required, and set first |
| `identity.directory.base` | the distinguished name every search starts from |
| `identity.directory.schema` | `ad` (the default) or `rfc2307`, for a plain LDAP server |
| `identity.directory.bind_name` | the distinguished name or UPN of a read-only account DittoFS binds as |
| `identity.directory.bind_credential` | that account's password. It is stored sealed and never shown again. A password that looks like JSON, such as `12345` or `true`, must be quoted: `'"12345"'` |
| `identity.directory.trust_roots` | the PEM certificates the directory's certificate must chain to; empty trusts the host's CAs |
| `identity.directory.realm` | the Kerberos realm, in upper case |
| `identity.directory.netbios_domain` | the domain's NetBIOS name. It must differ from `identity.netbios_name` (default `DITTOFS`) |
| `identity.directory.user_name_attr`, `identity.directory.group_name_attr` | the attributes holding a user's and a group's name; empty means `sAMAccountName` for `ad`, `uid` and `cn` for `rfc2307` |
| `identity.directory.dc_address`, `identity.directory.dns_server` | a domain controller, or a DNS server, to use instead of the domain's DNS service records |
| `identity.directory.require_pac` | whether a Kerberos ticket from the realm must carry a PAC (group information); unset means `true` |

The connection is always encrypted and the certificate always checked. No setting turns
that off.

> **Samba's own certificate is refused.** A default Samba AD DC serves a certificate with
> a negative serial number, which Go refuses, so the bind fails. Give the DC a properly
> issued certificate, such as the test domain's CA above.

### How a directory user gets a uid

A directory user or group gets a DittoFS identity the first time it signs in or a grant
names it. Its uid or gid is the entry's `uidNumber` or `gidNumber`. An entry with none,
or with `0`, `65534` or a number another user already holds, gets one from
`identity.id_range` instead. That number is kept, so it does not change at the next
sign-in.

### Other identity settings

| Key | Default | Meaning |
| --- | --- | --- |
| `identity.id_range` | `{"lo":1000000,"hi":1999999999}` | where missing uids and gids are allocated from; changing it moves no existing ID |
| `identity.ntlm` | `true` | whether NTLM (password) sign-in is accepted; admin only. Turning it off deletes every stored NT hash |
| `identity.netbios_name` | `DITTOFS` | the server's NetBIOS name, at most 15 bytes; a join sets it |
| `identity.keytab` | none | the imported keytab; written only through the keytab route in [Part C](#import-the-keytab) |
| `identity.directory_stale_grace` | `15m` | how long past its 5-minute lifetime a cached directory answer may still be used while the directory is down. So a user disabled in the directory during an outage can keep access for up to 5 minutes plus this; `0` limits it to the 5 minutes |

---

## Part B2: Grant AD principals directly to shares (no local users)

With the directory connected, grant a share to a directory user or group by its
`DOMAIN\name`:

```bash
dfsctl grant set archive --directory 'EXAMPLE\engineering' --access read-write
dfsctl grant set archive --directory 'EXAMPLE\alice' --access read
dfsctl grant list archive
```

DittoFS looks the name up in the directory and grants what it finds. If the directory
cannot find the name, the command fails and grants nothing. A grant to a group reaches
every member, including members of nested groups. See
[Access Control](access-control.md) for how grants combine.

---

## Part C: Configure Kerberos

Kerberos lets a client that already holds a ticket (a Windows user logged on to the
domain, or a Linux user after `kinit`) sign in with no password prompt. DittoFS needs
the keys of the service names clients ask tickets for. They come from two places:

- a **domain join** ([Part C2](#part-c2-join-the-domain)) gives the keys of the computer
  account's own names (`host/…`, `cifs/…`);
- an **imported keytab** gives the keys of every other name, such as one registered on a
  separate service account.

The realm is `identity.directory.realm`. Clocks may differ by at most 5 minutes.

### One keytab, both protocols

One keytab can hold several service names. Export the SMB (`cifs/`) and NFS (`nfs/`)
names into one file:

```bash
samba-tool domain exportkeytab /etc/dittofs/dittofs.keytab \
    --principal=cifs/files.example.com@EXAMPLE.COM
samba-tool domain exportkeytab /etc/dittofs/dittofs.keytab \
    --principal=nfs/files.example.com@EXAMPLE.COM
klist -ke /etc/dittofs/dittofs.keytab     # check: aes256-cts and aes128-cts keys only
```

On Windows Server, `ktpass` does the same (see
[windows-ad-setup.md](windows-ad-setup.md)). The keytab must hold AES keys only: one
with a DES or RC4 key is refused whole. Set the service account's
`msDS-SupportedEncryptionTypes` to `0x18` (AES128 and AES256) so the domain issues AES
tickets for it.

### Import the keytab

`dfsctl` has no keytab command, so the import is a call to the management API as an
admin:

1. Sign in and keep the access token:

   ```bash
   URL=http://127.0.0.1:8080/api/v1
   TOKEN=$(curl -s -X POST "$URL/auth/sign-in" -H 'Content-Type: application/json' \
     -d '{"name":"admin","password":"my-secure-password"}' | jq -r .access_token)
   ```

2. Send the keytab, base64-encoded:

   ```bash
   curl -s -X PUT "$URL/installation/keytab" \
     -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
     -H 'If-None-Match: *' \
     -d '{"spec": {"value": "'"$(base64 < /etc/dittofs/dittofs.keytab | tr -d '\n')"'"}}'
   ```

   A file that is not a keytab of AES keys is refused:

   ```json
   {"status":400,"code":"invalid","title":"Bad Request","detail":"not a keytab of AES keys alone","field":"spec.value", ...}
   ```

3. Delete `/etc/dittofs/dittofs.keytab`, or keep it readable by its owner only. The
   node stores the keytab sealed and does not read the file again.

`If-None-Match: *` says this is the first import; without it the API answers `428`. An
import over an existing keytab is refused with `409`. To replace a keytab, delete the
stored one first, then import the new one. A DELETE needs the stored keytab's version,
which the `identity.keytab` setting's `ETag` header gives, and an `Idempotency-Key` of
your choice:

```bash
curl -si "$URL/settings/identity.keytab" -H "Authorization: Bearer $TOKEN" | grep -i '^etag'
# ETag: "3"
curl -s -X DELETE "$URL/installation/keytab" -H "Authorization: Bearer $TOKEN" \
  -H 'If-Match: "3"' -H 'Idempotency-Key: delete-keytab-1'
```

### Domain-aware SMB sign-in

When an SMB client signs in with NTLM, DittoFS tells it the server's name
(`identity.netbios_name`) and the domain (`identity.directory.netbios_domain`), so a
domain user signs in against the right domain. See [SMB](smb.md).

---

## Part C2: Join the domain

Joining creates (or takes over) a **computer account** for DittoFS in the domain. It
does two things:

- Its service names (`host/…`, `cifs/…` and any others the account holds) accept
  Kerberos tickets. DittoFS derives their keys from the account's password, so no keytab
  is needed for them.
- It lets a domain user sign in over **NTLM**: DittoFS forwards the sign-in to a domain
  controller over a secure channel. Without a join, a domain user's NTLM sign-in fails
  with `STATUS_LOGON_FAILURE`. This matters when a Windows user connects by IP address,
  where Kerberos cannot be used.

### Before you join

- The directory is connected ([Part B](#part-b-connect-the-directory)) with
  `identity.directory.realm` and `identity.directory.base` set, and its schema is `ad`.
  An online join's `--ou` must lie under `identity.directory.base`.
- DittoFS finds domain controllers through DNS, unless `identity.directory.dc_address`
  names one.
- The account uses AES keys only (`msDS-SupportedEncryptionTypes` = `0x18`). An online
  join sets this itself.
- A firewalled site opens these ports from the DittoFS host to **each** domain
  controller:

  | Port | Used for |
  | --- | --- |
  | 53 | DNS, to find the domain controllers |
  | 88 | Kerberos |
  | 135 | the RPC endpoint mapper |
  | the DC's dynamic RPC range (`49152-65535` on Windows unless narrowed) | the secure channel for NTLM sign-ins |
  | 636 (or 389 for an `ldap://` address) | LDAP |

### Online join

DittoFS creates the computer account itself, as a domain administrator whose password
you type. The administrator needs the right to create computer objects in the
organisational unit:

```bash
dfsctl domain join --user Administrator@EXAMPLE.COM \
  --account 'FILES$' --ou 'OU=Servers,DC=example,DC=com' --wait
```

- The password is read from a prompt, or from the file `--password-file` names. It is
  used for this join only and never stored or logged.
- `--account` defaults to the installation's NetBIOS name (`DITTOFS`). `--ou` defaults
  to the domain's `Computers` container. `--dns-host-name` defaults to the account name
  in lower case, a dot and the realm in lower case (`files.example.com`).
- If an account of that name already exists, the join is refused, naming it. This stops
  a second installation from taking over the first one's account by accident. Pass
  `--takeover` to replace it on purpose.

### Offline join

An administrator creates the computer account, so DittoFS never holds a credential that
can create accounts. Create it with AES enabled; an account made in *Active Directory
Users and Computers* lacks the setting, and the join refuses it:

```powershell
New-ADComputer -Name FILES -Enabled $true `
  -AccountPassword (ConvertTo-SecureString 'a-long-random-password' -AsPlainText -Force) `
  -OtherAttributes @{'msDS-SupportedEncryptionTypes'=0x18}
```

Put the password in a file and hand it over:

```bash
dfsctl domain join --offline --account 'FILES$' --password-file machine.txt --wait
```

DittoFS checks the password with a domain controller, then **changes it twice**, so that
whoever created the account no longer knows it. The account accepts no Kerberos ticket
until the second change is done. An offline join takes no `--user`, `--ou` or
`--dns-host-name`.

**Two things you must do yourself**, because DittoFS cannot check them:

- **Keep the account's creator off the network path to the domain controller while the
  join runs.** The first password change is authenticated with the handed-over
  password, so someone who can watch that exchange can learn the next password too. Run
  the join from a network the creator is not on, or use an online join.
- **Change the computer object's owner after any join.** The owner can set up
  delegation (`msDS-AllowedToActOnBehalfOfOtherIdentity`) and get tickets to DittoFS as
  any user. The account's creator usually stays its owner. In *Active Directory Users and
  Computers*, set the owner to *Domain Admins* and check that attribute. DittoFS does not
  report the owner or the attribute yet.

### After a join

`GET /api/v1/installation` shows the join under `status.join`: whether it is joined, the
account, the service names it holds, and when the password last changed.

The join sets `identity.netbios_name` to the account's name (upper case, no `$`). While
joined, that setting cannot be changed: `dfsctl setting set` or `unset` of it is refused
with `409 joined`. To use another name, join again with `--takeover --account` naming the
new account. There is no way to leave the domain.

The joined account's names get their keys from the join, so a keytab must not hold keys
for them. Importing such a keytab is refused, and so is a join of an account whose names
the stored keytab already holds (`409`). Every other service name, such as `nfs/…` or a
name on a separate service account, still needs an imported keytab.

### Rejoin and takeover

Joining an installation that is already joined is refused with `409 joined` unless you
pass `--takeover`. A takeover is also how you recover: after an administrator reset the
account's password (offline join with the new password), or after a domain controller
stopped answering for an hour. A join is refused with `409 operation_running` while
another join or a password change runs.

### Password rotation

The computer account's password changes by itself every **seven days**, well inside
Windows' 30-day limit. A failed change is retried after an hour. To change it now:

```bash
dfsctl domain rotate-machine-password --wait
```

A rotation is refused while a join runs, and fails when the installation is not
joined. Tickets issued under the previous password keep working for ten hours plus the clock
skew. Two health conditions, shown by `dfsctl status`, report trouble:

| Condition | Meaning | What to do |
| --- | --- | --- |
| `machine_password_rotation_failed` | the last password change failed | read the operation's error (`dfsctl operation list`), which names the controller or the cause; fix it and rotate again. A join with `--takeover` is the fallback |
| `machine_account_unhealthy` | this node cannot reach a domain controller over the secure channel | check the ports above, the account in the directory, and the clock |

---

## Part D: Kubernetes operator (DittoServer CR)

The Kubernetes operator does not run this release: it writes the previous release's
config file, which this release refuses. Until it is rebuilt, set up the directory and
the keytab through the management API as in Parts B and C. Never put a keytab in a
ConfigMap: it is secret key material.

---

## Part E: Verify

Once SMB is served, check from a Linux client that a domain user signs in with Kerberos
and that files show the same owner over both protocols:

```bash
# 1. Get a ticket for the domain user
kinit alice@EXAMPLE.COM
klist                        # shows a ticket for EXAMPLE.COM

# 2. Mount over SMB with Kerberos
sudo mount -t cifs //files.example.com/archive /mnt/smb -o sec=krb5,port=12445,vers=3.0

# 3. Check the owner
ls -ln /mnt/smb              # alice's files show uid 10001
```

The client's `/etc/krb5.conf` must name the realm's KDC, and the name you mount must
match a service name in the keytab or the join (`cifs/files.example.com`). `smbclient`
can test the sign-in without mounting:

```bash
smbclient //files.example.com/archive --use-kerberos=required
```

A Windows client joined to the domain connects with its logon ticket:

```powershell
net use \\files.example.com\archive
icacls \\files.example.com\archive\report.txt
```

The Security tab shows owners and ACL entries as `EXAMPLE\name` while the directory is
reachable. If it is not configured or not reachable, they show as raw `S-1-5-21-…` SIDs.

Connecting by IP address, or by a name with no service name registered, falls back to
NTLM, which needs a domain join for domain users.

NFS with `sec=krb5` cannot be verified on this version, as NFS Kerberos is not served.
The client-side steps it will need are in [NFS](nfs.md).

---

## Known limitations

- **NFS Kerberos is not served on this version**; Kerberos works for SMB only.
- **The test domain is for testing.** The Samba AD DC under `test/integration/ad-dc/`
  and `k8s/samba-ad-dc/` is not a file server and is not managed by the operator.
- **AES only.** DittoFS refuses DES and RC4 keys. Windows 11 refuses RC4
  keytabs too, so give the service account AES keys (`0x18`).
- **No leave operation.** A joined installation stays joined; `--takeover` moves it to
  another account.
- **Names need the directory.** Showing SIDs as `DOMAIN\name` depends on the directory
  being connected and reachable.
