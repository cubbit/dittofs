# Windows Active Directory Setup Runbook

This runbook joins DittoFS to an existing Windows Server Active Directory domain
and gives domain users access to a share, **without creating a DittoFS user for
each of them**. Domain users sign in with their own domain credentials, and
DittoFS authorizes them by their AD user and group SIDs. Follow it when you
deploy DittoFS in an AD domain.

Each step gives the exact command, PowerShell on a domain controller or an
admin workstation with RSAT, or `dfsctl`, and what success looks like.
[Identity](identity.md) has the full reference for every setting used here.

> **Not available yet on this version.** This version does not serve SMB or
> NFS, so the client-side steps (step 10 onward) cannot run yet. Steps 1 to 7
> (the directory and the domain join) run today and are kept for when SMB is
> served. Step 8 (grants) needs a share, which a new installation on this
> version cannot create yet, and step 9 needs the SMB adapter, which cannot be
> turned on yet (`unknown_key`). NFS with Kerberos is not served on this version either.

> DittoFS is **not production ready**. See the [FAQ](faq.md).

The example uses:

| Thing | Example value |
|-------|---------------|
| Domain (Kerberos realm) | `CORP.EXAMPLE.COM` |
| NetBIOS domain | `CORP` |
| Domain controller | `dc1.corp.example.com` |
| DittoFS host | `192.0.2.10`, named `files.corp.example.com` |
| DittoFS computer account | `FILES` |
| Directory read account | `svc-dittofs` |
| Share | `archive` |
| Domain users and group | `alice`, `bob`, group `Engineering` |

Replace them with your own.

---

## Before you begin

### Two kinds of account

| | Management account | Domain users |
|---|---|---|
| Who | The one DittoFS `admin` | alice, bob, Administrator, … |
| Signs in with | A DittoFS password, through `dfsctl login` | Their domain password or Kerberos ticket |
| Created by | `dfs init` | Active Directory. **Never created in DittoFS** |
| Used for | Connecting the directory, joining the domain, granting access | Mounting shares |

You sign in as the DittoFS `admin` to set things up. After that, domain users
just connect.

### What you need

- Domain administrator rights, or the right to create users and computer
  objects in an OU.
- A DittoFS host the domain controllers can reach, with a fixed IP address.
- The DittoFS host's **protocol key** (`keys.protocol` in its config file).
  DittoFS seals the directory password and the machine password under it.
  Without it, step 5 and step 6 fail with `500 internal error`. No `dfs`
  command creates this key file yet: write it with the helper shown in
  [Encryption: local key files](encryption.md#local-key-files).

---

## 1. Create a read-only directory account

DittoFS reads the directory to find users and their groups. It needs an
ordinary domain user for that, with no special rights.

On a domain controller, in PowerShell:

```powershell
New-ADUser -Name svc-dittofs -SamAccountName svc-dittofs `
  -UserPrincipalName svc-dittofs@corp.example.com `
  -AccountPassword (Read-Host -AsSecureString "svc-dittofs password") `
  -Enabled $true -PasswordNeverExpires $true
```

**Success:** `Get-ADUser svc-dittofs` shows `Enabled : True`.

Never use a domain administrator for this account. It only reads.

---

## 2. Add a DNS name for the DittoFS host

Kerberos works only when clients connect by a name the domain knows. Give the
DittoFS host one:

```powershell
Add-DnsServerResourceRecordA -ZoneName corp.example.com -Name files -IPv4Address 192.0.2.10
```

**Success:** `Resolve-DnsName files.corp.example.com` answers `192.0.2.10`.

---

## 3. Start DittoFS and sign in

On the DittoFS host, initialise and start the server, as the user that runs
`dfs`, with the admin password in a file only that user can read:

```bash
export DITTOFS_ENCRYPTION_PASSPHRASE='choose-a-passphrase'
dfs init --admin-password-file /etc/dittofs/admin-password
dfs start
```

[Getting started](getting-started.md) explains the config file and the
passphrase.

The config file holds no identity settings. The realm, the directory and the
join are set through `dfsctl` in the next steps, and the server applies them
without a restart. (A v1 `kerberos:` or `ldap:` block in the config file is
refused.)

Sign in as `admin`. Over the loopback address the API answers plain HTTP:

```console
$ dfsctl login --server http://127.0.0.1:8080 --username admin
password:
Logged in to http://127.0.0.1:8080 as admin (context default)
```

---

## 4. Point DittoFS at the directory

Set the directory's address first, then the rest:

```bash
dfsctl setting set installation identity.directory.address ldaps://dc1.corp.example.com:636
dfsctl setting set installation identity.directory.base 'DC=corp,DC=example,DC=com'
dfsctl setting set installation identity.directory.realm CORP.EXAMPLE.COM
dfsctl setting set installation identity.directory.netbios_domain CORP
dfsctl setting set installation identity.directory.bind_name svc-dittofs@corp.example.com
```

**Success:** each command prints the key and its value:

```console
KEY                         VALUE                               APPLIED
identity.directory.address  "ldaps://dc1.corp.example.com:636"
```

`APPLIED` counts the nodes that have picked the change up. It is empty right
after the write; `dfsctl setting get installation identity.directory.address`
shows `1/1` a few seconds later.

If the domain controller's certificate is issued by your own CA, give DittoFS
that CA. Export the CA certificate as **Base-64 encoded X.509**, and save it as
`corp-root-ca.pem` on the DittoFS host:

```bash
dfsctl setting set installation identity.directory.trust_roots -- "$(cat corp-root-ca.pem)"
```

The `--` is needed because the PEM text starts with dashes. Without this
setting, the host's trusted CAs are used. Certificate checks cannot be turned
off: `ldaps://` always uses TLS, and `ldap://` always upgrades with StartTLS.

---

## 5. Give DittoFS the directory password, and test

Keep the password in a file only you can read, so it stays out of your shell
history:

```bash
dfsctl setting set installation identity.directory.bind_credential "$(cat ~/svc-dittofs.pw)"
```

**Success:** the value comes back as a sealed reference, never the password:

```console
KEY                                 VALUE                                                        APPLIED
identity.directory.bind_credential  {"ref":"14908706-e6af-4e00-8595-ea4fc8cf71fc","version":1}
```

A password that looks like JSON, such as `12345`, must be quoted twice:
`'"12345"'`.

Now test the connection:

```bash
dfsctl domain test --wait -o yaml
```

**Success:** the result says `reached: true` (output trimmed):

```yaml
result:
  reached: true
state: succeeded
```

`state: succeeded` alone only means the test ran. A failed test still has
`state: succeeded`, with `reached: false` and an `error` saying why, for example:

```yaml
result:
  error: 'vfs: unavailable: identity.directory: dial: identity.directory: dial ldaps://dc1.corp.example.com:636: dial tcp: lookup dc1.corp.example.com: no such host'
  reached: false
```

---

## 6. Join the domain

Joining creates a **computer account** for DittoFS. It does two jobs:

- **Kerberos:** clients get tickets for `cifs/files.corp.example.com`, and
  DittoFS checks them with keys derived from the account's password. You need no
  keytab for SMB.
- **NTLM for domain users:** when a client connects by IP address, or by
  double-clicking the server under **Network** in Explorer, Windows uses NTLM.
  DittoFS passes the user's answer to a domain controller over the computer
  account's secure channel. Without a join, a domain user's NTLM sign-in fails.

Join as an administrator who may create computer objects in the OU:

```bash
dfsctl domain join --user administrator@corp.example.com \
  --account FILES --ou 'OU=Servers,DC=corp,DC=example,DC=com' --wait
```

`dfsctl` prompts for the administrator's password (or reads `--password-file`).
DittoFS uses it once and never stores it. The account's DNS name defaults to
`files.corp.example.com`, the account name and the realm in lower case; set
another with `--dns-host-name`.

**Success:** the operation ends in `succeeded`:

```console
ID                                    KIND                         TARGET                                             STATE      UPDATED
5b0e2c7a-…                            installation.join-domain     installation/02478ac7-…                            succeeded  2026-11-25T09:40:12Z
```

Check the account from PowerShell:

```powershell
Get-ADComputer FILES -Properties msDS-SupportedEncryptionTypes, servicePrincipalName
```

`msDS-SupportedEncryptionTypes` is `24` (AES128 and AES256), and
`servicePrincipalName` lists `HOST/files.corp.example.com`.

The machine password then changes by itself every 7 days. To change it now:
`dfsctl domain rotate-machine-password --wait`.

### Alternative: an offline join

If DittoFS must never hold an administrator's password, create the account
yourself, with AES enabled. An account created in **Active Directory Users and
Computers** lacks the AES attribute, and DittoFS refuses it:

```powershell
New-ADComputer -Name FILES -Path 'OU=Servers,DC=corp,DC=example,DC=com' -Enabled $true `
  -AccountPassword (Read-Host -AsSecureString "FILES password") `
  -OtherAttributes @{'msDS-SupportedEncryptionTypes'=0x18}
```

Write that password to a file on the DittoFS host, readable only by you, then:

```bash
dfsctl domain join --offline --account FILES --password-file machine-password.txt --wait
```

DittoFS changes the password twice during the join, because you know the one
you handed over. Keep the account's creator off the network path between DittoFS
and the domain controller while the join runs: the first change is
authenticated by that handed-over password.

---

## 7. Hand the computer object to Domain Admins

Whoever owns the computer object can make the domain controller issue a ticket
for **any** user to DittoFS. After the join, the owner is usually the
administrator who ran it. Change that:

1. Open **Active Directory Users and Computers**, and turn on **View → Advanced
   Features**.
2. Open the `FILES` computer object, **Security → Advanced**.
3. Next to **Owner**, click **Change**, enter `Domain Admins`, click OK twice.
4. On the **Attribute Editor** tab, check that
   `msDS-AllowedToActOnBehalfOfOtherIdentity` is `<not set>`.

**Success:** the Advanced Security dialog shows `Owner: Domain Admins`.

DittoFS cannot check this for you.

---

## 8. Grant domain users and groups

Grant access straight to AD names. `--directory` takes `DOMAIN\name`; DittoFS
looks the name up in the directory and grants the user or group it finds. Give
each principal its own level:

```bash
dfsctl grant set archive --directory 'CORP\alice' --access read
dfsctl grant set archive --directory 'CORP\bob' --access read-write
dfsctl grant set archive --directory 'CORP\Engineering' --access read-write
dfsctl grant set archive --directory 'CORP\Domain Admins' --access admin
```

**Success:** `dfsctl grant list archive` lists one line per grant, with the
principal's ID and its access.

Levels, lowest to highest: `none`, `read`, `read-write`, `admin`. A user's own
grant decides, and `none` refuses the user; with no grant of its own, its
groups' grants decide. To remove one:
`dfsctl grant remove archive --directory 'CORP\Engineering'`.

A user with no `uidNumber` in AD gets a UID from `identity.id_range`
(`1000000` to `1999999999` by default), which DittoFS keeps. You do not need to
add POSIX attributes to your AD users.

---

## 9. Serve SMB on port 445 and open the firewall

Group Policy drive maps, Explorer and Windows before 11 24H2 can only connect
to port 445, so in an AD domain run the SMB adapter there:

```bash
dfsctl adapter set smb --enabled --setting port=445
```

On this version that command is refused with `unknown_key` (see the note at
the top). Binding 445 needs root or `CAP_NET_BIND_SERVICE` on Linux; see
[Running on standard ports](install.md#running-on-standard-ports-production).
If you keep the default 12445 instead, open 12445 below, and map drives with
`net use … /TCPPORT:12445` (Windows 11 24H2 or later) rather than Group Policy.


The DittoFS host can run Linux or Windows; only the firewall commands differ.

**From the DittoFS host to each domain controller (outbound):**

| Port | For |
|------|-----|
| 53 | DNS, to find the domain controllers |
| 88 | Kerberos |
| 135 | RPC endpoint mapper |
| 49152–65535 (or your site's narrower range) | The secure channel for NTLM pass-through |
| 636 (or 389 for an `ldap://` address) | LDAP |

**From clients to the DittoFS host (inbound):** SMB on 445, or the port
DittoFS serves (12445 by default). Double-clicking the server under **Network**
in Explorer also needs network discovery: UDP 3702, UDP 5353 and TCP 5357.

DittoFS on **Linux** (firewalld):

```bash
firewall-cmd --permanent --add-port=445/tcp --add-port=5357/tcp --add-port=3702/udp --add-port=5353/udp
firewall-cmd --reload
```

DittoFS on **Windows**: the built-in Network Discovery rules apply only to
Windows' own services, so add rules for `dfs.exe`:

```powershell
$dfs = "C:\Program Files\DittoFS\dfs.exe"
New-NetFirewallRule -DisplayName "DittoFS SMB"            -Direction Inbound -Action Allow -Protocol TCP -LocalPort 445  -Program $dfs -Profile Domain
New-NetFirewallRule -DisplayName "DittoFS discovery meta" -Direction Inbound -Action Allow -Protocol TCP -LocalPort 5357 -Program $dfs -Profile Domain
New-NetFirewallRule -DisplayName "DittoFS discovery"      -Direction Inbound -Action Allow -Protocol UDP -LocalPort 3702 -Program $dfs -Profile Domain
New-NetFirewallRule -DisplayName "DittoFS mDNS"           -Direction Inbound -Action Allow -Protocol UDP -LocalPort 5353 -Program $dfs -Profile Domain
```

If Windows' own file server holds port 445 on that host, stop it so DittoFS can
bind 445: `Stop-Service LanmanServer; Set-Service LanmanServer -StartupType Disabled`.
Do this only on a host that serves no other Windows shares.

---

## 10. Map the share at logon

Each person who logs on gets the share with **their own** permissions: each
logon holds its own Kerberos ticket, and DittoFS authorizes whoever connects. On
one shared workstation, alice gets read-only, bob gets read-write, and an
administrator gets admin, through `Domain Admins`.

**Option A: Group Policy drive map (recommended).**

1. In **Group Policy Management**, edit a GPO linked to the users' OU.
2. Go to **User Configuration → Preferences → Windows Settings → Drive Maps**,
   and choose **New → Mapped Drive**.
3. Set **Action** to *Update*, **Location** to `\\files.corp.example.com\archive`,
   tick **Reconnect**, and pick a drive letter.
4. Use **Item-level targeting** to give different groups different drives.

It must be under **User** Configuration, so the drive maps with each user's own
ticket.

**Option B: a logon script.**

```cmd
net use Z: \\files.corp.example.com\archive /persistent:yes
```

No password is passed: Windows uses the logged-on user's ticket.

Use the DNS name, never the IP address. With an IP address, Windows uses NTLM
instead of Kerberos.

---

## 11. Check it works

1. **Kerberos.** Log on to a domain workstation as alice and run:

   ```cmd
   net use Z: \\files.corp.example.com\archive
   klist
   ```

   **Success:** `net use` prints `The command completed successfully.`, and
   `klist` lists a ticket for `cifs/files.corp.example.com @ CORP.EXAMPLE.COM`.

2. **Access levels.** As alice, open a file on `Z:` (works) and save a change
   (refused: she has `read`). Log on as bob: both work.

3. **NTLM pass-through.** Connect by IP address, which forces NTLM:

   ```cmd
   net use * /delete /y
   net use \\192.0.2.10\archive /user:CORP\alice *
   ```

   **Success:** `The command completed successfully.`

4. **Names in Explorer.** Right-click a file on `Z:`, **Properties →
   Security**. Owners and entries show as `CORP\alice`. Raw `S-1-5-21-…` SIDs
   mean the directory is not connected (step 4).

You never ran `dfsctl user create` for alice or bob: they reach the share as AD
users only.

---

## Optional: a keytab for other service names

The join covers the computer account's own names, such as
`cifs/files.corp.example.com`. Any other name, such as `nfs/files.corp.example.com`
for NFS with Kerberos (not served on this version), needs a keytab from a
service account. Do **not** put the computer account's names in it: DittoFS
refuses a keytab that holds them.

1. Map the name to the service account and export an AES keytab:

   ```cmd
   ktpass -princ nfs/files.corp.example.com@CORP.EXAMPLE.COM ^
     -mapuser CORP\svc-dittofs -pass * ^
     -crypto AES256-SHA1 -ptype KRB5_NT_PRINCIPAL ^
     -out C:\dittofs\dittofs.keytab
   ```

   Use AES only: DittoFS refuses a keytab holding a DES or RC4 key, and
   Windows 11 refuses RC4 tickets. **Success:** `ktpass` ends with
   `Output keytab to C:\dittofs\dittofs.keytab`.

2. Make the account ask for AES tickets:

   ```powershell
   Set-ADUser svc-dittofs -KerberosEncryptionType AES128,AES256
   ```

3. Copy the keytab to the DittoFS host. It is a secret: anyone holding it can
   act as the service.

4. Import it on the DittoFS host. `dfsctl` has no keytab command, so call the
   API with a token:

   ```bash
   TOKEN=$(dfsctl token create --expires-in 1h)
   curl -s -X PUT http://127.0.0.1:8080/api/v1/installation/keytab \
     -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
     -H 'If-None-Match: *' \
     -d '{"spec": {"value": "'"$(base64 < dittofs.keytab | tr -d '\n')"'"}}'
   ```

   **Success:** the API answers the stored keytab's reference:

   ```json
   {"ref":"4863a4d8-8106-48e3-bcd1-67ea9e427140","version":1}
   ```

   To replace a keytab later, delete the stored one, then import the new one
   with the same `PUT`. The delete names the stored keytab's `version` (from
   the answer above) in `If-Match`, and carries a fresh `Idempotency-Key`:

   ```bash
   curl -s -X DELETE http://127.0.0.1:8080/api/v1/installation/keytab \
     -H "Authorization: Bearer $TOKEN" \
     -H 'If-Match: "1"' -H "Idempotency-Key: $(uuidgen)"
   ```

   **Success:** the API answers `{}`.

---

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `System error 1219` on `net use` | Windows already holds a connection to the server as another user. Run `net use * /delete /y` and `cmdkey /delete:files.corp.example.com`, then retry. |
| Kerberos fails, or Windows reports a signing error | The account has no AES keys. Check `msDS-SupportedEncryptionTypes` is `24` (step 6). Windows 11 refuses RC4. |
| `dfsctl grant set --directory` says not found | The directory is unreachable or the name does not exist. Run `dfsctl domain test --wait -o yaml` and check `reached: true`. |
| Access denied despite a grant | Check the grant with `dfsctl grant list archive`. For a group grant, check the user is in the group: `Get-ADPrincipalGroupMembership alice`. Group changes reach the user's ticket at the next logon. |
| A domain user fails when connecting by IP | NTLM needs the join (step 6). |
| `ldaps://` certificate error | Set `identity.directory.trust_roots` to your CA's PEM (step 4). |
| `500 internal error` when setting the bind password | The DittoFS host has no protocol key (`keys.protocol`). See [Before you begin](#what-you-need). |
| A Windows client logs `LOGON_FAILURE` on a second connection but keeps working | SMB multichannel session binding is not supported for NTLM pass-through users. The spare connection fails; the main one keeps working. Browsing and mounting are not affected. |

---

## How authorization works

- A grant names a principal. A grant by `--directory` name looks the name up
  first, so a directory user or group becomes a principal of the installation,
  with the SID the directory gives it.
- A caller's grants are those on its own principal or on one of its groups. For
  Kerberos, the groups are the SIDs in the ticket; for NTLM pass-through, those
  the domain controller returns.
- **NFS** carries UIDs and GIDs, not SIDs, so an NFS caller is matched by the
  principal its UID maps to. Keep `identity.id_range` clear of the UIDs and
  GIDs your clients use locally, or a local ID could match a domain user's
  grant over NFS.
- The `admin` management account grants nothing on files: it reads only what a
  share grant gives it.

See [Access control](access-control.md) for how SIDs and ACLs fit together.
