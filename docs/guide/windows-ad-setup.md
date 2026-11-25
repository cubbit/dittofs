# Windows Active Directory Setup Runbook

> **Not served on this branch.** This build serves no file protocol; the SMB and
> NFS adapters are being rebuilt (RFC 20, RFC 21, RFC 22). This runbook
> describes the released server.

End-to-end guide for a Windows administrator to join **DittoFS** to an existing
**Windows Server Active Directory** domain and give domain users SMB (and NFS)
access to a share — **without creating any local DittoFS user for them**
(issue #1528). Domain users authenticate with their own Kerberos credentials and
are authorized directly by their AD user/group **SID**.

This is the production Windows-Server companion to
[identity.md](identity.md) (which uses a Samba AD-DC dev fixture). DittoFS-side
steps use the `dfsctl` CLI; Windows-side steps use the native tools (`ktpass`,
ADUC/ADSI Edit, Group Policy Management Console).

> **Status.** AD/LDAP/Kerberos support is functional but the project is **not
> production ready**. See [faq.md](faq.md).

The worked example uses realm **`CUBBIT.LOCAL`** / NetBIOS **`CUBBIT`**, a
member server **`vm2.cubbit.local`**, a service account **`svc-dittofs`**, and
domain users **alice** / **bob** in an AD group **Cubbit**. Substitute your own.

---

## 0. Accounts: what you create and what you don't

There are **two kinds of account**, and #1528 only removes one of them:

| | Control-plane (management) | Data-plane (mounting) |
|---|---|---|
| Who | The **one native DittoFS admin** | AD users (alice, bob, Administrator) |
| Auth | Local password → `dfsctl login` | Kerberos ticket from the domain |
| Created how | Created by `dfs init`, which takes its password from `--admin-password-file` or `--admin-password-env` | **Never created in DittoFS** |
| Used for | Connect the directory, join the domain, create grants | Mount shares |

So the workflow is: **sign in as the native admin to connect the directory and
create grants; from then on, domain users just mount.** You do **not** create AD-backed
admin logins, and you do **not** create a DittoFS user for each AD user.

---

## 1. Create the service account, SPN, and AES keytab (in AD)

DittoFS authenticates SMB/NFS clients as a Kerberos service. It needs a domain
**service account** that holds the service principal name (SPN) and an exported
**keytab**.

1. **Create the service account** (Active Directory Users and Computers, or
   PowerShell). Give it a strong password; it needs no special privileges.
   ```powershell
   New-ADUser -Name svc-dittofs -SamAccountName svc-dittofs `
     -AccountPassword (Read-Host -AsSecureString "svc-dittofs password") `
     -Enabled $true -PasswordNeverExpires $true
   ```

2. **Map the SPN and export an AES keytab** with `ktpass` (run on a DC or any
   RSAT-equipped admin box). This binds `cifs/<host>` (SMB) to the account and
   writes the keytab. **Use AES** — Windows 11 rejects RC4-only service tickets
   (#1318):
   ```cmd
   ktpass -princ cifs/vm2.cubbit.local@CUBBIT.LOCAL ^
     -mapuser CUBBIT\svc-dittofs -pass * ^
     -crypto AES256-SHA1 -ptype KRB5_NT_PRINCIPAL ^
     -out C:\dittofs\dittofs.keytab
   ```
   For NFS as well, repeat with `-princ nfs/vm2.cubbit.local@CUBBIT.LOCAL` and
   `-in`/`-out` the same file to append (or export a second keytab and merge).

3. **Advertise AES on the account** so the KDC issues AES tickets. In ADUC enable
   *"This account supports Kerberos AES 256 bit encryption"* (Account tab), or
   set `msDS-SupportedEncryptionTypes` via ADSI Edit / Attribute Editor. `ktpass`
   usually sets this; verify it.

4. Copy `dittofs.keytab` to the DittoFS host (e.g. `C:\ProgramData\dittofs\`).
   Treat it as a **secret** — it is the service's Kerberos key. Restrict its ACL
   to the DittoFS service account.

> If SMB port 445 on the host is held by the Windows *Server* service, stop and
> disable `LanmanServer` so DittoFS can bind 445 (or run DittoFS on a host that
> is not itself an SMB server).

---

## 2. Start the server and sign in

The bootstrap file holds no identity: v1's `kerberos:` and `ldap:` blocks are
refused. The realm, the directory and the keytab are records the management
API writes, so the server starts with none of them.

Initialise the node (this creates the native admin, with the password read
from a file) and start the server:

```powershell
dfs init --config C:\dittofs\config.yaml --admin-password-file C:\dittofs\admin-password.txt
dfs start --config C:\dittofs\config.yaml
```

Sign in with the native admin, the one management account from step 0. Over
loopback the API answers plain HTTP on `127.0.0.1:8080`:

```powershell
dfsctl login --server http://127.0.0.1:8080 --username admin
```

Import the keytab from step 1. `dfsctl` has no keytab command; the import is
`PUT /api/v1/installation/keytab` with the keytab as base64, as
[identity.md, Part C](identity.md#import-the-keytab) shows. The node applies it
with no restart.

---

## 3. Connect the LDAP directory (read-only)

DittoFS reads the directory to (a) resolve an AD **name to a principal** when
you write a grant, and (b) resolve a domain user's groups and uid/gid at sign-in.
Use a **read-only** service account (the same `svc-dittofs` is fine: it only
needs to read directory objects) and **LDAPS**.

A user's uid is its `uidNumber` when the entry has one. An entry without one
gets a uid allocated from `identity.id_range`, which DittoFS keeps, so you do
**not** need to stamp POSIX attributes on your AD objects.

The directory is the `identity.directory` record, written one key at a time.
Set the address first:

```powershell
dfsctl setting set installation identity.directory.address ldaps://vm1.cubbit.local:636
dfsctl setting set installation identity.directory.base 'DC=cubbit,DC=local'
dfsctl setting set installation identity.directory.realm CUBBIT.LOCAL
dfsctl setting set installation identity.directory.netbios_domain CUBBIT
dfsctl setting set installation identity.directory.bind_name 'svc-dittofs@cubbit.local'
dfsctl setting set installation identity.directory.bind_credential '<svc-dittofs password>'
dfsctl domain test --wait
```

**Security notes:**
- `ldaps://` dials TLS and `ldap://` always upgrades with StartTLS; no field
  turns either off. Set `identity.directory.trust_roots` to the PEM of the DC's
  issuing CA, or the host's roots are trusted.
- The bind account needs **read only**. Never use a domain-admin bind.
- The bind password is a secret: the API seals it and answers only its
  reference and version.

See [identity.md, Part B](identity.md#part-b-connect-the-directory) for every
field.

---

## 4. Grant AD users and groups directly (no local users)

Now grant share access straight to AD principals. `--directory` takes an AD
name, `DOMAIN\name`; the server resolves it through the directory and grants
the principal it returns. `--user` and `--group` name local users and groups.
You create no DittoFS user or group by hand.

Grant **distinct levels per principal** — this is what makes the same share
behave differently for each person:

```powershell
# AD user alice → read-only
dfsctl grant set ditto --directory 'CUBBIT\alice' --access read

# AD user bob → read-write
dfsctl grant set ditto --directory 'CUBBIT\bob' --access read-write

# AD group (everyone in Cubbit) → read-write, resolved to the group SID via LDAP
dfsctl grant set ditto --directory 'CUBBIT\Cubbit' --access read-write

# Domain Admins → admin (full control)
dfsctl grant set ditto --directory 'CUBBIT\Domain Admins' --access admin

dfsctl grant list ditto     # shows each grant: principal ID and access
```

Levels, lowest to highest: `none < read < read-write < admin`.

Revoke symmetrically: `dfsctl grant remove ditto --directory 'CUBBIT\Cubbit'`.

---

## 5. Per-user "mount on login" (the target experience)

The goal: on a shared workstation, **each user who logs in gets the share mounted
with their own permissions.** This works because each interactive Windows logon
holds its own Kerberos ticket, so mounting `\\vm2.cubbit.local\ditto` under that
logon presents *that user's* identity; DittoFS reads their PAC user + group SIDs
and applies the grants keyed to those SIDs. No configuration ties a mount to a
specific person — DittoFS simply authorizes whoever connects.

- **alice logs in →** the share mounts **read-only** (her grant).
- **bob logs in on the same box →** his own logon, his own ticket → **read-write**.
- **Administrator logs in →** matched via *Domain Admins* → **admin**.

Mount on login is a **client-side** Windows step (nothing DittoFS-specific):

**Option A — Group Policy Drive Maps (recommended).** In the Group Policy
Management Console, edit a GPO applied to the workstations:
*User Configuration → Preferences → Windows Settings → Drive Maps → New → Mapped
Drive.* Set Location `\\vm2.cubbit.local\ditto`, Action *Update*, a drive letter,
and **Reconnect**. Use **item-level targeting** if you want different drives for
different groups. Because it is under *User Configuration*, the drive maps under
each user's own logon and ticket.

**Option B — logon script.** A per-user logon script running:
```cmd
net use Z: \\vm2.cubbit.local\ditto /persistent:yes
```
No credentials are passed — Windows uses the logged-in user's Kerberos ticket
(single sign-on).

**Verify the resolved identity** in Explorer: right-click a file →
*Properties → Security*. Owner/ACE SIDs resolve to `CUBBIT\<name>` when the LDAP
directory is configured (step 3); without it they show as raw `S-1-5-21-…`.

---

## 6. NTLM pass-through: Explorer double-click for AD users (optional)

Everything above authenticates over **Kerberos**, which needs a service ticket
for the SPN — so it works when you mount by the FQDN (`\\vm2.cubbit.local\ditto`).
But when a user **double-clicks the server in Explorer → Network** (LAN
discovery), or connects by **IP**, Windows connects by a name with **no SPN** and
falls back to **NTLM** — which the KDC never sees. Without extra setup, an AD
domain user fails that path with `STATUS_LOGON_FAILURE` (only local DittoFS users
with a stored password work over NTLM).

To make double-click work for **domain** users, enable **NETLOGON pass-through**:
DittoFS forwards the client's NTLM response to a DC (`NetrLogonSamLogon`) over a
secure channel, gets back the user + group SIDs, and authorizes them with the
**same SID grants** from step 4 (so `Domain Admins` etc. apply unchanged).

This needs a dedicated **machine (computer) account** — *not* the `svc-dittofs`
service account, because NETLOGON requires a workstation-trust secure channel
that only a machine account can establish. DittoFS joins the domain with a
management-API operation, online or offline; the steps, the request bodies and
the ports are in [identity.md, Part C2](identity.md#part-c2-join-the-domain).

**Step 1 — pick a join.**

- **Online:** DittoFS creates the computer account itself. Give it an
  administrator that may create computer objects in the target OU.
- **Offline:** you create the account and hand DittoFS its password. Create it
  with AES enabled, `msDS-SupportedEncryptionTypes` = `0x18`. DittoFS accepts
  AES only, so an account without the attribute, which is what Active
  Directory Users and Computers creates, is refused:

  ```powershell
  New-ADComputer -Name DITTOFS `
    -AccountPassword (ConvertTo-SecureString 'M4chine!Pass2026' -AsPlainText -Force) `
    -Enabled $true `
    -OtherAttributes @{'msDS-SupportedEncryptionTypes'=0x18}
  ```

  Keep the account's creator off the network path between DittoFS and the DC
  while the join runs: the join rotates the password twice, and the first
  rotation is authenticated by the password you handed over.

**Step 2 — join.** Online, as an administrator who may create computer objects:

```powershell
dfsctl domain join --user administrator@cubbit.local --ou 'OU=Servers,DC=cubbit,DC=local' --wait
```

Offline, with the account you created:

```powershell
dfsctl domain join --offline --account DITTOFS --password-file machine.txt --wait
```

The password then rotates by itself; `dfsctl domain rotate-machine-password`
rotates it now.

**After the join, transfer the computer object's ownership** (for example to
*Domain Admins*) and check its `msDS-AllowedToActOnBehalfOfOtherIdentity`: the
owner can have a DC sign a ticket for any user to the account's names.

**Step 3 — test the NTLM path.** From a domain member, connect by **IP** (forces
NTLM) or double-click the server in Explorer → Network:

```powershell
net use * /delete /y
net use \\192.168.100.50\ditto /user:CUBBIT\alice P4ssword!
```


### Ports and firewall

This feature is **server-OS-agnostic** — the DittoFS server can run on **Linux or
Windows**; the AD/Kerberos/NETLOGON setup is identical and only the host firewall
differs. Pass-through needs the DittoFS host to reach the **DC outbound** on
RPC endpoint mapper `135`, the DC's dynamic RPC range (`49152-65535` unless the
site narrowed it) for the NETLOGON channel, Kerberos `88`, LDAPS `636` (or `389` for an `ldap://` directory record, with StartTLS), and DNS
`53`, and to accept SMB `445` **inbound** from clients. The Explorer **double-click** additionally needs LAN
discovery: inbound WS-Discovery UDP `3702`, mDNS UDP `5353`, and the metadata fetch on TCP
`5357`.

- **DittoFS on Windows** — the built-in "Network Discovery" rules are scoped to
  `System`/`svchost`, so inbound discovery traffic to a third-party `dfs.exe` is
  dropped by default. Add **program-scoped** inbound rules:
  ```powershell
  $dfs = "C:\path\to\dfs.exe"
  New-NetFirewallRule -DisplayName "DittoFS Discovery (WSD meta)"  -Direction Inbound -Action Allow -Protocol TCP -LocalPort 5357 -Program $dfs -Profile Any
  New-NetFirewallRule -DisplayName "DittoFS Discovery (WSD probe)" -Direction Inbound -Action Allow -Protocol UDP -LocalPort 3702 -Program $dfs -Profile Any
  New-NetFirewallRule -DisplayName "DittoFS Discovery (mDNS)"      -Direction Inbound -Action Allow -Protocol UDP -LocalPort 5353 -Program $dfs -Profile Any
  ```
- **DittoFS on Linux** — no program-scoping quirk; just open the ports (e.g. with
  firewalld):
  ```bash
  firewall-cmd --permanent --add-port=445/tcp --add-port=5357/tcp --add-port=3702/udp --add-port=5353/udp
  firewall-cmd --reload
  ```
  Outbound to the DC is usually already permitted; if you restrict egress, allow
  `135`, the DC's dynamic RPC range, `88`, `636` (or `389` for an `ldap://` record), and `53` to the DC.

> **Known limitation:** SMB multichannel **session-bind** for pass-through domain
> users is not yet supported — a client that opens extra channels logs a benign
> `LOGON_FAILURE` on the spare connection and keeps working on the primary. It
> does not affect browsing or mounting.

---

## 7. Verify and troubleshoot

**Prove no local users are needed:** you never ran `dfsctl user create` for alice
or bob — they mount purely as AD principals.

Check each logon gets the right level: as alice, reading works but writing is
denied; as bob, both work; as Administrator, full control. The decision comes
from each session's own sign-in, so two users on one workstation never share
permissions.

| Symptom | Cause / fix |
|---|---|
| `System error 1219` on `net use` | A pre-existing/"Disconnected" session to the same server under different creds. `net use * /delete /y` and `cmdkey /delete:vm2.cubbit.local`, then retry. Not a DittoFS bug. |
| Mount falls back to NTLM or fails with a signing error | Keytab lacks AES keys (RC4-only) — Windows 11 rejects RC4 (#1318). Re-export with `-crypto AES256-SHA1` and set `msDS-SupportedEncryptionTypes`. |
| Grant by name returns "not found" | The directory is not connected or not reachable (`dfsctl domain test --wait`), or the name isn't a directory object. The server resolves the name through the directory before granting, so the directory must be reachable. |
| Access denied despite a grant | Confirm the grant with `dfsctl grant list <share>`; confirm the user's ticket carries the granted group (their PAC group SIDs). |
| `ldaps://` cert error | Set `identity.directory.trust_roots` to the PEM of the DC's issuing CA. Verification cannot be turned off. |

---

## How authorization works (reference)

- A grant names a principal by its ID. A grant by `--directory` name resolves
  the name to a principal first, so a directory user or group is a principal of
  the installation, with the SID the directory gives it.
- A caller's grants on a share are those naming its own principal or one of
  its groups: for a Kerberos sign-in, the user and group SIDs of its PAC; for
  NTLM pass-through, those the domain controller returns.
- **NFS** carries uids and gids, not SIDs, so an NFS caller is matched by the
  principal its uid maps to. Keep `identity.id_range` clear of the uids and
  gids your clients use locally, or a local id could match a directory
  principal's grant over NFS.
- The management role grants nothing on files: even the `admin` account reads
  only what a share grant gives it.

See [access-control.md](access-control.md) for the full SID/ACL model.
