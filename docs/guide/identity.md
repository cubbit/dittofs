# Active Directory / LDAP / Kerberos / NTLM Integration

Operator guide for joining DittoFS to an Active Directory (or compatible LDAP)
directory so that **one AD user resolves to the same Unix UID/GID over both SMB
and NFS**. This covers the two halves of AD integration:

- **The directory** — resolves a directory principal (a `user@REALM` form or an
  AD SID) to a Unix identity, reading RFC2307 `uidNumber`/`gidNumber` attributes
  where the entry has them and allocating from `identity.id_range` where it does
  not, and resolves nested group membership.
- **Kerberos** — accepts Kerberos service tickets for the server's SPN over both
  SMB (SPNEGO) and NFS (RPCSEC_GSS), from a single service keytab. NTLM remains
  available as a fallback for SMB.

> **Status.** AD/LDAP/Kerberos support is functional but the project is **not
> production ready**. Read [docs/FAQ.md](faq.md) for known limitations.

See also: [docs/CONFIGURATION.md](configuration.md) (every config key + env var),
[docs/SMB.md](smb.md), [docs/NFS.md](nfs.md), and [docs/ACLS.md](access-control.md) (the
cross-protocol ACL/SID model).

> **Joining the domain** (the computer account, the ports a firewalled site opens,
> rotation): see [Part C2](#part-c2-join-the-domain).

> **Joining a real Windows Server AD?** For an end-to-end operator runbook
> (`ktpass` keytab, LDAP, granting AD users/groups directly, and per-user
> "mount on login"), see [windows-ad-setup.md](windows-ad-setup.md). This page
> is the reference for every provider field; that page is the step-by-step.

---

## Overview: cross-protocol identity unification

DittoFS resolves every authenticated principal through a single identity
resolver chain. The LDAP/AD provider is registered in that chain after Kerberos,
so a directory principal or SID with no local mapping is resolved against the
directory. Because **both** the NFS and SMB adapters consume the same resolver,
an AD user maps to the **same Unix UID/GID regardless of which protocol they
arrive on** — the property the `go test -tags=ad_dc` suite asserts (alice →
uid 10001, gid 10000 over LDAP; the same identity over an SMB Kerberos session).

The two providers play complementary roles:

| Provider | What it does | When it applies |
|---|---|---|
| **Kerberos** | Authenticates the ticket (SMB SPNEGO / NFS RPCSEC_GSS), yields a principal + PAC group SIDs | At session/RPC auth time |
| **LDAP idmap** | Maps the principal / SID to a Unix UID/GID + group GIDs | When resolving the authenticated identity to POSIX |

You can enable LDAP alone (idmap for NTLM/AUTH_UNIX-named principals), Kerberos
alone, or both together (the full AD-joined deployment).

---

## Prerequisites

- A reachable directory: a real Windows Server AD-DC, a Samba AD-DC, or any
  RFC2307-capable LDAP server. For a throwaway dev directory see **Part A**.
- LDAP idmap: a read-only **service account** (bind DN + password) and the
  directory's **base DN**. RFC2307 mode additionally requires `uidNumber` /
  `gidNumber` POSIX attributes to be stamped on the user/group objects.
- Kerberos: a joined computer account, a **service keytab** containing the
  DittoFS SPN(s), or both. See **Part C** and **Part C2**.
- For verification: `kinit` (the `krb5-user` package) and `mount.cifs` /
  `smbclient` on the client.

---

## Part A: Stand up a dev Samba AD-DC

> **DEV / TEST ONLY.** The Samba AD-DC described here is an identity authority
> (Kerberos KDC on 88, LDAP on 389/636, AD DNS on 53, kpasswd on 464) used to
> exercise the LDAP-idmap and Kerberos paths against a *real* AD without a
> Windows Server license. **It is not a file server, is not a DittoFS component,
> and is not managed by the dittofs-operator.** In production you install **no**
> Samba — you point DittoFS at your existing enterprise AD. Port 445 (SMB) is
> deliberately not published on the host; DittoFS owns 445.

The provisioning logic below lives in `test/integration/ad-dc/entrypoint.sh`. It
provisions realm **`DITTOFS.AD`** / domain **`DITTOFS`** and creates:

- user `alice` — RFC2307 `uidNumber=10001`, `gidNumber=10000`, member of `devs`
  (which is nested under `engineering`).
- user `bob` — no RFC2307 attrs (his uid is allocated), member of `engineering`.
- user `joiner` — no RFC2307 attrs, password `TestPassword01!` (`USER_PASSWORD`); its only grant
  beyond Domain Users is create-child for the computer class on the `Computers`
  container, which is what makes it `offjoinowned$`'s owner below.
- groups `engineering` and `devs` (with `devs` nested under `engineering`).
- a service account `dittofs-svc` holding the `cifs/` and `nfs/` SPNs, AES only,
  and a combined keytab of their keys at `/keytabs/dittofs.keytab`.
- a CA, exported as `/keytabs/ca.pem`, that issues the DC's TLS certificate for
  its host name (`dc1.dittofs.ad` when the container runs with `--hostname dc1`)
  and its IPs.
- computer accounts for the joins: `dittofs$` (password `MachinePass01!`), and
  `offjoinaes$` (password `OffJoinAES01!`, `msDS-SupportedEncryptionTypes`
  `0x18`) and `offjoinlegacy$` (password `OffJoinLegacy01!`, none), pre-staged
  for an offline join; `offjoinowned$` (password `OffJoinOwned01!`, `0x18`),
  created by the user `joiner`, so `joiner` owns it, with
  `msDS-AllowedToActOnBehalfOfOtherIdentity` naming `dittofs$`; and `BUILD01$`
  (password `Build01Pass01!`), a plain workstation account. `MACHINE_PASSWORD`,
  `OFFJOIN_AES_PASSWORD`, `OFFJOIN_LEGACY_PASSWORD`, `OFFJOIN_OWNED_PASSWORD`
  and `BUILD01_PASSWORD` override the passwords.

NETLOGON's dynamic RPC port comes from the pinned range `50000-50019`, beside
135 for the endpoint mapper.

### What the provisioning does

The one-shot provision (run on first start) is:

```bash
samba-tool domain provision \
    --use-rfc2307 \
    --realm=DITTOFS.AD \
    --domain=DITTOFS \
    --server-role=dc \
    --dns-backend=SAMBA_INTERNAL \
    --adminpass="$ADMIN_PASSWORD" \
    --option="rpc server dynamic port range = 50000-50019"
```

`--use-rfc2307` enables the POSIX (`uidNumber`/`gidNumber`) schema so those
attributes can be stamped on objects. Users and groups are then created:

```bash
samba-tool group add engineering
samba-tool group add devs
# Nest devs under engineering so a member of devs carries BOTH group SIDs
# in their Kerberos PAC (AD resolves the nesting at the DC):
samba-tool group addmembers engineering devs

# alice: RFC2307 attrs (idmap_ad path), member of devs (=> nested in engineering)
samba-tool user create alice "$USER_PASSWORD" \
    --uid-number=10001 --gid-number=10000 \
    --unix-home=/home/alice --login-shell=/bin/bash
samba-tool group addmembers devs alice

# bob: no RFC2307 attrs (DittoFS allocates his uid)
samba-tool user create bob "$USER_PASSWORD"
samba-tool group addmembers engineering bob
```

### The AES-only service keytab

The DittoFS SPNs are registered on a service account of their own, whose
`msDS-SupportedEncryptionTypes` is `24` (`0x18` = AES128-CTS + AES256-CTS), so
the KDC refuses a ticket for them under RC4 or DES, and `exportkeytab` writes
their AES keys only:

```bash
samba-tool user create dittofs-svc --random-password
samba-tool spn add cifs/dittofs.dittofs.ad dittofs-svc
samba-tool spn add nfs/dittofs.dittofs.ad  dittofs-svc
ldbmodify -H /var/lib/samba/private/sam.ldb <<EOF
dn: CN=dittofs-svc,CN=Users,DC=dittofs,DC=ad
changetype: modify
replace: msDS-SupportedEncryptionTypes
msDS-SupportedEncryptionTypes: 24
EOF
```

Then export both principals into one keytab (a second `exportkeytab` into the
same path **appends**):

```bash
samba-tool domain exportkeytab /keytabs/dittofs.keytab --principal=cifs/dittofs.dittofs.ad@DITTOFS.AD
samba-tool domain exportkeytab /keytabs/dittofs.keytab --principal=nfs/dittofs.dittofs.ad@DITTOFS.AD
```

Verify the keytab carries AES keys for both SPNs (the entrypoint fast-fails if
`aes256-cts` / `aes128-cts` are missing):

```bash
klist -ke /keytabs/dittofs.keytab
```

### Route 1 — Docker (the test fixture image)

The image is built from `test/integration/ad-dc/`. The provisioning realm,
domain, and passwords are controlled by environment variables (defaults shown):

| Env var | Default |
|---|---|
| `AD_REALM` | `DITTOFS.AD` |
| `AD_DOMAIN` | `DITTOFS` |
| `AD_ADMIN_PASSWORD` | `Passw0rd!2024` |
| `USER_PASSWORD` | `TestPassword01!` |
| `KEYTAB_DIR` | `/keytabs` |
| `DITTOFS_UID` / `DITTOFS_GID` | `65532` / `65532` (keytab ownership) |

The container needs to bind privileged ports (88/389/636/464/53) and set NT
ACLs, so it runs privileged. The combined keytab lands in the `KEYTAB_DIR`
volume; mount that volume (or copy the keytab out) so the `dfs` server can read
it.

### Route 2 — In-cluster (k8s/samba-ad-dc manifests)

For a Kubernetes dev cluster, apply the manifests in `k8s/samba-ad-dc/`:

- `samba-ad-dc.yaml` — a `StatefulSet` + headless `Service` exposing only the
  directory roles (88, 389, 636, 464, 53). The pod runs privileged, uses Samba's
  internal DNS (`dnsPolicy: None`, nameserver `127.0.0.1`, search `dittofs.ad`),
  and persists `/var/lib/samba` and `/keytabs` on PVCs. The image
  (`docker.io/library/dittofs-ad-dc:dev`, `imagePullPolicy: Never`) is the same
  one built from `test/integration/ad-dc/`, imported locally (e.g. via
  `k3s ctr images import`).
- `krb5-test-client.yaml` — a throwaway pod that reaches the in-cluster AD-DC and
  the DittoFS adapters, mounting the AD-generated `krb5.conf` from a Secret
  (`demo-krb5-conf`) at `/etc/krb5.conf` (`KRB5_CONFIG=/etc/krb5.conf`). Use it
  to `kinit` and then exercise SMB/NFS from inside the cluster.

Both manifests are headed **DEV / TEST only** — the same caveat as above.

---

## Part B: Connect the directory

The directory is the `identity.directory` record, a set of installation
settings written through the management API. Only an `admin` account may
write it. A node applies a change at its next reconcile, with no restart.
The bootstrap file holds no identity: v1's `ldap:` and `kerberos:` blocks are
refused, naming the management API.

Set each field with `dfsctl setting set installation <key> <value>`. Set
`identity.directory.address` first: a record with no address is refused.

```bash
dfsctl setting set installation identity.directory.address ldaps://dc1.dittofs.ad:636
dfsctl setting set installation identity.directory.base 'DC=dittofs,DC=ad'
dfsctl setting set installation identity.directory.realm DITTOFS.AD
dfsctl setting set installation identity.directory.netbios_domain DITTOFSAD   # must differ from identity.netbios_name
dfsctl setting set installation identity.directory.bind_name 'CN=svc-dittofs,CN=Users,DC=dittofs,DC=ad'
dfsctl setting set installation identity.directory.bind_credential "$BIND_PASSWORD"
dfsctl setting set installation identity.directory.trust_roots -- "$(cat /keytabs/ca.pem)"   # -- as the PEM starts with dashes
dfsctl domain test --wait
```

| Key | Meaning |
|---|---|
| `identity.directory.schema` | `ad` (the default) or `rfc2307` |
| `identity.directory.address` | `ldaps://host[:port]`, or `ldap://host[:port]`, which is always upgraded with StartTLS before the bind; required |
| `identity.directory.base` | the distinguished name every search starts from |
| `identity.directory.bind_name` | the distinguished name or UPN the bind sends |
| `identity.directory.bind_credential` | the bind password, a secret: the API seals the value and answers only its reference and version. The value must reach the API as a JSON string, so quote a password that reads as JSON, such as `'"12345"'` |
| `identity.directory.trust_roots` | the PEM certificates the directory's chain must end in; empty trusts the host's roots |
| `identity.directory.realm` | the Kerberos realm, in upper case |
| `identity.directory.netbios_domain` | the domain's NetBIOS name; it must differ from `identity.netbios_name` |
| `identity.directory.user_name_attr`, `identity.directory.group_name_attr` | the attributes holding a name; empty reads `sAMAccountName` for `ad`, `uid` and `cn` for `rfc2307` |
| `identity.directory.dc_address`, `identity.directory.dns_server` | a domain controller, or a DNS server, used instead of the domain's DNS service records |
| `identity.directory.require_pac` | whether a ticket in the realm must carry a PAC; unset reads as `true` |

> **TLS is always on.** `ldaps://` dials TLS and `ldap://` upgrades with
> StartTLS before the bind. No field skips certificate verification or allows a
> plaintext bind.

> **Samba AD-DC self-signed certs.** A default Samba AD-DC serves a certificate
> with a negative serial number, which Go's `crypto/x509` refuses, so a bind to
> it fails. Use a properly issued DC certificate, such as the fixture's CA above.

`dfsctl domain test` binds to the directory and reports the result as an
operation. `dfsctl domain resolve 'DITTOFS\alice' --wait` looks a name up, and
`--sid` looks a SID up.

### How a directory user gets a uid

A directory user or group that signs in, or that a grant names, becomes a
principal of the installation. Its uid or gid is the entry's `uidNumber` or
`gidNumber`. An entry with none, or with `0`, `65534` or a number another
principal holds, gets one allocated from `identity.id_range` instead. The
allocated number is kept, so it does not change on the next sign-in.

### The identity settings

The other identity keys are installation settings too:

| Key | Default | Meaning |
|---|---|---|
| `identity.id_range` | `{"lo":1000000,"hi":1999999999}` | the span a missing uid or gid is allocated from; a change moves no existing ID |
| `identity.ntlm` | `true` | whether NTLMv2 sign-in is accepted; admin only. Turning it off starts an operation that deletes the stored NT hashes |
| `identity.netbios_name` | `DITTOFS` | the installation's NetBIOS name, at most 15 bytes; a join sets it |
| `identity.keytab` | none | the imported keytab, a secret; written only through the keytab route below |
| `identity.directory_stale_grace` | `15m` | how long past its 5-minute lifetime a directory answer may be served while the directory fails |

---

## Part B2: Grant AD principals directly to shares (no local users)

With the directory connected, grant share access straight to an AD user or
group. `--directory` takes the name as `DOMAIN\name`; the server resolves it
through the directory and grants the principal it returns. No local user is
created by hand.

```bash
dfsctl grant set ditto --directory 'CUBBIT\Cubbit' --access read-write
dfsctl grant set ditto --directory 'CUBBIT\alice' --access read

dfsctl grant list ditto     # shows each grant: principal ID and access
```

A grant by name fails, and grants nothing, when the directory cannot resolve
the name. For the full Windows Server walkthrough, including per-user "mount
on login", see [windows-ad-setup.md](windows-ad-setup.md); for the ACE model,
see [access-control.md](access-control.md).

---

## Part C: Configure Kerberos

Kerberos backs both NFS RPCSEC_GSS and SMB SPNEGO. The keys come from two
places: a joined computer account's names derive theirs from its password
([Part C2](#part-c2-join-the-domain)), and every other service name, such as
`nfs/<host>`, needs an imported keytab. The realm is
`identity.directory.realm`. The clock skew allowed is a fixed 5 minutes.

### One keytab, both protocols

A keytab can hold multiple service principals, so a single file serves SMB and
NFS. Export a keytab containing **both** the `cifs/` (SMB) and `nfs/` (NFS) SPNs:

```bash
samba-tool domain exportkeytab /etc/dittofs/dittofs.keytab \
    --principal=cifs/server.example.com@EXAMPLE.COM
samba-tool domain exportkeytab /etc/dittofs/dittofs.keytab \
    --principal=nfs/server.example.com@EXAMPLE.COM
```

The keytab holds AES keys only; one with a DES or RC4 key is refused whole.

### Import the keytab

`dfsctl` has no keytab command; the import is a call to the management API
with an administrator's token. `PUT /api/v1/installation/keytab` takes the
keytab as base64, with `If-None-Match: *` for a first import, or it answers
`428`:

```bash
curl -s -X PUT "$URL/installation/keytab" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -H 'If-None-Match: *' \
  -d '{"spec": {"value": "'"$(base64 < /etc/dittofs/dittofs.keytab | tr -d '\n')"'"}}'
```

`URL` is the API's address, such as `http://127.0.0.1:8080/api/v1` (see
[configuration.md](configuration.md)). A second import over a stored keytab is
refused `409`: `DELETE` the keytab, then import the new one. The keytab is
stored sealed, as `identity.keytab`, and the node applies it at its next
reconcile, with no restart.

### Domain-aware SMB

An SMB CHALLENGE advertises `identity.netbios_name` as the computer name and
the directory record's `netbios_domain` as the domain, so domain users
authenticate against the correct domain. See [smb.md](smb.md).
---

## Part C2: Join the domain

Joining gives DittoFS a **computer account** in the domain. The account does two
jobs: its service names (`host/`, `cifs/` and whatever the account's
`servicePrincipalName` and the domain's SPN mappings add) accept Kerberos
tickets under keys DittoFS **derives from the account's password**, and its
secure channel to a domain controller validates NTLM sign-ins of domain users
(NETLOGON pass-through). Without a join, a domain user's NTLM sign-in fails.

A join is an operation of the management API, which `dfsctl domain join`
starts as an administrator. With `--wait` the command follows the operation to
its end; `dfsctl operation wait <id>` resumes an interrupted wait.

### Before you join

- The directory record names the realm and the base:
  `identity.directory.realm` (for example `DITTOFS.AD`) and
  `identity.directory.base` (for example `DC=dittofs,DC=ad`). An online join's
  organisational unit must lie strictly under the base. A join is refused while
  the record's schema is `rfc2307`, which has no domain controller.
- Domain controllers are found from the domain's DNS SRV records, with
  fail-over, unless the directory record names a controller address, which is
  then used alone.
- **AES only.** DittoFS accepts and derives AES128 and AES256 keys
  (`msDS-SupportedEncryptionTypes` = `0x18`) and refuses DES and RC4. An online
  join sets the attribute on the account it creates; an offline join refuses an
  account whose value lacks either bit or is absent.
- A firewalled site opens these ports from the DittoFS host to **each** domain
  controller:

  | Port | Used for |
  |---|---|
  | 135 | RPC endpoint mapper, which names NETLOGON's port |
  | the controller's dynamic RPC range | the NETLOGON secure channel. On Windows this is `49152-65535` unless the site narrowed it; on Samba it is `rpc server dynamic port range` |
  | 88 | Kerberos |
  | 636, or 389 for an `ldap://` record (StartTLS) | LDAP, which the online join creates the account over and the directory reads use |

  Also open DNS (53) if the controllers are found by SRV records.

### Online join

DittoFS creates the computer account as an administrator and takes its
password. The administrator needs the right to create computer objects in the
organisational unit:

```bash
dfsctl domain join --user Administrator@DITTOFS.AD \
  --account 'DITTOFS$' --ou 'OU=Servers,DC=dittofs,DC=ad' --wait
```

- The administrator's password is read from a prompt, or from the file
  `--password-file` names. It is held in memory for this one operation, never
  stored or logged, and sent with no idempotency key.
- `--account` is the account's name, with or without the `$`; without it the
  join takes the installation's. `--ou` defaults to the domain's `Computers`
  container. `--dns-host-name` (a name of two labels or more) defaults to the
  account's name in lower case, a dot and the realm in lower case.
- A failed operation names the controller, the account or the attribute that
  ended it.
- A join of a computer account that already exists is refused, naming it, unless
  the command passes `--takeover`: the default NetBIOS name is `DITTOFS`, so
  a second installation joining the same domain would otherwise reset the
  first one's password.

### Offline join

An administrator creates the computer account, so DittoFS never holds a
credential that can create or delete accounts. Create it with the attribute
set; an account created in Active Directory Users and Computers has none, and
the join refuses it:

```powershell
New-ADComputer -Name DITTOFS -Enabled $true `
  -AccountPassword (ConvertTo-SecureString 'a-long-random-password' -AsPlainText -Force) `
  -OtherAttributes @{'msDS-SupportedEncryptionTypes'=0x18}
```

Then hand DittoFS the account and its password. An offline join takes no
`--user`, `--ou` or `--dns-host-name`:

```bash
dfsctl domain join --offline --account 'DITTOFS$' --password-file machine.txt --wait
```

The join verifies the password against a controller, reports which service
names the account holds, then **rotates the password twice**, because whoever
created the account knows the password you handed over. The handed-over
password derives no Kerberos key, so the account's names accept no ticket until
the second rotation has confirmed.

Two things the code cannot check for you:

- **Keep the account's creator off the network path to the domain controller
  while the join runs.** The first rotation is authenticated by the handed-over
  password, so a creator who can observe or answer that exchange can learn the
  password, and the next one. Run the join from a network the creator is not on,
  or use an online join.
- **Transfer the computer object's ownership after any join.** The owner can
  write `msDS-AllowedToActOnBehalfOfOtherIdentity` and, through resource-based
  constrained delegation, have a controller sign a ticket for any user to the
  account's names. The account's creator usually stays its owner, and whoever
  may write the organisational unit can do the same. In Active Directory Users
  and Computers, set the object's owner to *Domain Admins* and review the
  attribute. DittoFS does not yet report the owner or the attribute.

### After a join

`GET /api/v1/installation` answers `status.join`: `joined`, the `account`, the
service `names` it holds and `last_rotation`. A join also writes the account's
name, upper case and without the `$`, as `identity.netbios_name` (letters,
digits and `-`, at most 15 bytes). While the installation is joined, the name is
the computer account's, so the settings API refuses to change it: `dfsctl setting set` or
`dfsctl setting unset` of `identity.netbios_name` is answered `409` with the code
`joined`, naming the account, and the name stays as it was. To change the name,
join again with `--takeover` naming the new account in `--account`: the
join writes that account's name as `identity.netbios_name` when it ends. There is
no leave operation.

The account's names are derived, so a keytab must **not** hold keys for them: a
keytab import ([Part C](#import-the-keytab)) holding a key for one of the joined
account's names is refused, and so is a join naming an account whose names the
stored keytab already holds (`409`, naming the account). Every other service name, such as `nfs/<host>`, another host
name, or a name registered on a separate service account, still needs an
imported keytab. The keytab holds AES keys only; one with a DES or RC4 key is
refused whole. Export one as in [Part C](#one-keytab-both-protocols).

### Rejoin and takeover

A join of an installation that is already joined, online or offline, is refused
`409` with the code `joined` unless the command passes `--takeover`. The
takeover is also how you recover: an offline join after an administrator reset
the account's password, or a join whose domain controller stopped answering for
an hour. A join is also refused `409` while another join or a rotation runs
(`operation_running`), and `503` (`unavailable`) when the node has no identity
service, or, for a join that names its account, has a keytab stored that identity
has not applied.

### Rotation

The machine password rotates by itself **seven days** after the last
confirmed rotation, inside Windows' 30-day maximum, and a failed rotation is
retried after an hour. To rotate now:

```bash
dfsctl domain rotate-machine-password --wait
```

A rotation seals the new password first, waits for this node to hold it, sets
it at one pinned controller and then reads the account's key version number to
confirm. The wait is for this node only; with several nodes, the others pick the
new version up on their next reconcile. Tickets issued under the previous password keep verifying for the
retention (ten hours plus the clock skew). A rotation is refused while a join
runs, and fails, saying the installation is not joined, when it is not.

Two health conditions report the account:

| Condition | Meaning | What to do |
|---|---|---|
| `machine_password_rotation_failed` | the last rotation failed, or could not start because a node lagged | read the operation's failure, which names the controller or version; fix it and rotate again; a join with a takeover is the fallback |
| `machine_account_unhealthy` | this node's secure channel to a controller is failing | check the ports above, the account in the directory and the clock |

---

## Part D: Kubernetes operator (DittoServer CR)

The dittofs-operator does not run this release: it renders v1's config file
(`database:`, `controlplane:`, and `ldap:`/`kerberos:` from `spec.identity`),
which this release's bootstrap refuses, and no `node.id`, `journals` or `keys`.
Once it is rebuilt, the directory and the keytab are set through the
management API (Parts B and C). Never put a keytab in a
ConfigMap: it is secret key material, and the API stores it sealed.

---

## Part E: Verify

The proven end-to-end fixture is the `ad_dc` integration suite, which provisions
the domain above and checks the fixture's promises and the NETLOGON paths
against it:

```bash
go test -tags=ad_dc -v -timeout 20m ./test/integration/ad-dc/
```

The suite needs a Linux Docker host. The tests reach the DC on its container
IP, with no port published on the host, and Docker Desktop (macOS, Windows)
does not route the host to container IPs, so there the tests fail rather than
skip. Every image, container and network the fixture makes is named and
labelled with the prefix `dittofs-ad-dc-test`; set `DITTOFS_AD_DC_NAME_PREFIX`
to use another on a Docker host shared between users. A run force-removes any
container whose name starts with the prefix and a dash, and prunes the networks
labelled with it, once they are more than an hour old: what an earlier run
under the same prefix left behind when it was killed. A killed run's image tag
stays.

It verifies:

- **The fixture's promises** (`TestADFixture`): LDAPS verifies against the
  exported CA by the DC's host name; Administrator holds no `cifs/` or `nfs/`
  name, the KDC refuses alice a ticket for either under RC4 and issues one under
  AES256, and the exported keytab holds AES keys only; NETLOGON is served from
  the pinned port range; the pre-staged join accounts carry their passwords,
  encryption types, owner and delegation; a restart keeps a machine password.
- **The captured PACs** (`TestADPACCapture`): the PACs of alice's TGT and of her
  `cifs/` service ticket each verify under the key that decrypts their ticket;
  the TGT's carries `PAC_ATTRIBUTES_INFO` and `PAC_REQUESTOR`, the service
  ticket's does not.
- **NETLOGON pass-through** (`TestNetlogonPassthroughAlice`,
  `TestNetlogonHotReload*`, `TestOnlineJoinAndMemberLogon`): a secure channel
  as `DITTOFS$`, or as an account an online join just created, validates alice's
  NTLMv2 logon and returns her SID and groups, across a credential reload and a
  machine password rotation.
- **NETLOGON over TCP** (`TestNetlogonTCPSchannel`): NETLOGON on
  `ncacn_ip_tcp` with a schannel-sealed binding and no SMB session.

Unit tests that need a real PAC can replay captures from this fixture. To
capture them, with the keys that verify them, run

```bash
test/integration/ad-dc/capture-pacs.sh <output dir>
```

which starts a fixture DC and writes `tgt.pac` with `krbtgt.keytab`, and
`service.pac` with `service.keytab`. Every run provisions a new domain, so its
keys, SIDs and PACs all change.

### Manual verification (Linux client)

Obtain a ticket, then exercise both protocols. The Kerberos client config must
point at the AD realm/KDC (the AD-DC's generated `krb5.conf`, e.g. via
`KRB5_CONFIG`).

The exact mount forms below are the ones the e2e suites use (`test/e2e/`).
`KRB5_CONFIG` / `KRB5CCNAME` must point at the AD realm config and the ticket
cache. Substitute your server host, share name, and ports (SMB defaults to 445;
the DittoFS NFS default is 12049, not 2049).

```bash
# 1. Get a TGT for the domain user
kinit alice@DITTOFS.AD
klist                       # should show a TGT for DITTOFS.AD

# 2. Mount over SMB with Kerberos
mount -t cifs //server/share /mnt/smb -o sec=krb5,port=12445,vers=2.1,cache=none

# 3. Mount over NFSv4 with Kerberos.
#    Use vers=4.0 explicitly (the e2e suite mounts with vers=4.0, NOT vers=4):
mount -t nfs //server/share /mnt/nfs -o vers=4.0,port=12049,sec=krb5,actimeo=0
```

For an ad-hoc SMB check once a ticket is present, `smbclient` can force the
Kerberos path:

```bash
smbclient //server/share -I <server-ip> --use-kerberos=required
```

Confirm that files created over one protocol show the **same owner UID/GID**
when read over the other — that is the cross-protocol identity unification goal.
Expected: alice resolves to `uid 10001` on both.

### NFS `sec=krb5` from a Linux client — verified procedure

An NFSv4.0 `sec=krb5` mount has client-side prerequisites beyond `kinit` that are
easy to miss. The following sequence is **verified end-to-end** against the demo
(an external Linux host mounting the DittoFS NFS adapter as the AD user `alice`,
files resolving to `uid 10001 / gid 10000`):

1. **Kernel modules** (the client node, not a container): `rpcsec_gss_krb5` and
   `auth_rpcgss` must load (`modprobe rpcsec_gss_krb5`). A pod whose node lacks
   them — and which cannot `modprobe` — cannot do `sec=krb5` even though the
   server is correct.
2. **`krb5.conf`** mapping the realm to the KDC (use the AD-DC's generated
   `krb5.conf`, or point `kdc =` straight at the DC):
   ```ini
   [libdefaults]
     default_realm = DITTOFS.AD
   [realms]
   DITTOFS.AD = { kdc = <dc-host-or-ip> }
   [domain_realm]
     .dittofs.ad = DITTOFS.AD
   ```
3. **The server SPN must resolve to the NFS endpoint.** The client requests a
   ticket for `nfs/<the-name-you-mount>@REALM`; that name must match the keytab
   SPN (`nfs/dittofs.dittofs.ad`). Add a hosts entry if needed:
   `echo "<nfs-endpoint-ip> dittofs.dittofs.ad" >> /etc/hosts`, then mount
   `dittofs.dittofs.ad:/<share>`.
4. **A machine credential at `/etc/krb5.keytab`.** NFSv4 establishes its client
   lease with a *machine* principal, so `rpc.gssd` needs a host keytab — and the
   stock `rpc-gssd.service` is **gated on `/etc/krb5.keytab` existing**
   (`ConditionPathExists`), so without it the service silently never starts.
   Create a machine account in AD and export its `host/` key:
   ```bash
   samba-tool computer create <client-shortname>
   samba-tool domain exportkeytab /etc/krb5.keytab \
       --principal=host/<client-shortname>@DITTOFS.AD
   ```
5. **A running `rpc.gssd`.** Start it cleanly (`systemctl restart rpc-gssd`, now
   that the keytab exists). If `rpc.gssd` is not actually serving the upcall, the
   client silently falls back to **AUTH_SYS `uid=0`**, which a root-squashing
   export refuses — surfacing as the misleading `mount.nfs4: access denied by
   server`. If you see that, check `pgrep -a rpc.gssd` and the daemon's `-vvv`
   log for `do_downcall ... acceptor=nfs@<server>` (success).
6. **`kinit` + mount:**
   ```bash
   kinit alice@DITTOFS.AD
   mount -t nfs4 -o vers=4.0,sec=krb5,port=12049 dittofs.dittofs.ad:/<share> /mnt/nfs
   ls -lan /mnt/nfs          # alice's files show uid 10001 / gid 10000
   ```


### NTLM fallback (SMB)

When a client cannot use Kerberos, SMB falls back to NTLM. With
`netbios_domain` set, the server advertises the AD domain in the NTLM challenge
so domain users authenticate against the correct domain (see Part C / docs/SMB.md).

## Part F: Windows client — domain-join (Kerberos) [dev]

A real Windows client gets the cleanest experience by **joining the domain** and
authenticating to DittoFS over **Kerberos** — the server side is fully proven
(SMB SPNEGO/PAC + AES keytab, Part C). Connecting by a name with no Kerberos SPN
(such as an IP) falls back to **NTLM**, which
is now supported for AD domain users via **NETLOGON pass-through** — join the
domain ([Part C2](#part-c2-join-the-domain) and
[windows-ad-setup.md](windows-ad-setup.md)). For Kerberos acceptance testing,
domain-join is still the path with no extra server dependency.

The in-cluster AD-DC is not publicly reachable by default. To let an external
test VM join, expose the directory roles via the **scoped** dev LoadBalancer
`k8s/samba-ad-dc/samba-ad-dc-lb.yaml` (Kerberos 88, kpasswd 464, LDAP 389/636,
DNS 53):

The manifest ships with a closed TEST-NET placeholder in
`loadBalancerSourceRanges`, so applying it never exposes the AD-DC to the
internet — the LB routes to nobody until you patch in the client `/32`:

```bash
# 1. Apply (still closed — placeholder range routes nowhere), then open ONLY
#    to the Windows VM's public IP.
kubectl apply -f k8s/samba-ad-dc/samba-ad-dc-lb.yaml
kubectl -n dittofs patch svc samba-ad-lb --type=merge \
  -p '{"spec":{"loadBalancerSourceRanges":["<WINDOWS_VM_IP>/32"]}}'
kubectl -n dittofs get svc samba-ad-lb -o wide      # confirm range + note EXTERNAL-IP
```

On the Windows VM (PowerShell, Administrator):

```powershell
# Point DNS at the AD-DC LB so DITTOFS.AD SRV records resolve.
# Discover your NIC first — the alias is not always "Ethernet".
$adlb = "<samba-ad-lb EXTERNAL-IP>"
$nic  = (Get-NetAdapter -Physical | Where-Object Status -eq 'Up')[0].ifIndex
Set-DnsClientServerAddress -InterfaceIndex $nic -ServerAddresses $adlb

# Join the realm and reboot.
$cred = Get-Credential DITTOFS\Administrator        # Passw0rd!2024 in the dev fixture
Add-Computer -DomainName dittofs.ad -Credential $cred -Restart
```

After reboot, log in as `DITTOFS\alice` (dev password `TestPassword01!`). Map the
DittoFS share and open a file's **Properties > Security** tab. **Prerequisite:**
the LDAP/AD idmap resolver must be configured (Part B) — with it, owner/ACE SIDs
resolve to `DITTOFS\<name>` via LSARPC (LsarLookupSids2/3; #1291 + #1341). If the
directory resolver is unconfigured or unreachable, the server falls back to raw
`S-1-5-21-…` SIDs (or `unix_user:*` / `unix_group:*`), so confirm the idmap is up
before reading the GUI as an acceptance signal:

```powershell
net use \\<dittofs-smb-ip>\<share>           # Kerberos SSO as the logged-in domain user
icacls \\<dittofs-smb-ip>\<share>\<file>     # CLI equivalent of the Security tab
```

> **Tear down after capture:** `kubectl -n dittofs delete svc samba-ad-lb` — the
> LB publishes a KDC + LDAP directory and must not be left exposed. The AD-DC is
> dev-only (see Known limitations).

---

## Known limitations

- **AD-DC is dev-only.** The Samba AD-DC in `test/integration/ad-dc/` and
  `k8s/samba-ad-dc/` is an identity authority for testing — not a file server and
  not operator-managed. In production, point DittoFS at your existing enterprise
  AD; install no Samba.
- **NFS krb5 over the wire needs kernel support.** A Linux NFS client mounting
  with `sec=krb5` requires the node kernel modules `rpcsec_gss_krb5` /
  `auth_rpcgss`. An in-cluster k3s node that lacks these modules cannot complete
  an `sec=krb5` NFS mount even though the server side is correct.
- **SID → name resolution** is implemented over the LSARPC pipe: `LsarOpenPolicy`
  (opnum 6/44), `LsarLookupSids` (15), and `LsarLookupSids2/3` (57/76, the EX
  forms Windows Explorer/`rpcclient` use) translate machine-domain and AD
  foreign-domain SIDs to `DITTOFS\<name>` (#1291, #1341, #1342). This depends on
  the directory-backed idmap resolver (Part B) being configured and reachable;
  when it is not, the server falls back to raw `S-1-5-21-…` SIDs (or
  `unix_user:*` / `unix_group:*`). See [docs/ACLS.md](access-control.md).
- **RC4-only keytabs are rejected by Windows 11 (#1318).** Ensure the keytab
  carries AES256/AES128 keys for the SPNs (Part A).
- **Machine account (NETLOGON) for NTLM pass-through:** the keytab covers
  Kerberos; for AD domain users authenticating over **NTLM** (Explorer
  double-click / connect by IP, no SPN) join the domain. Both **offline**
  (pre-created computer account + its password) and **online** (DittoFS creates
  the computer object over LDAPS) joins are supported, and the password rotates
  by itself. See [Part C2](#part-c2-join-the-domain) and
  [windows-ad-setup.md](windows-ad-setup.md).
