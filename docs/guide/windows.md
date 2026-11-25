# Connecting a Windows Client to DittoFS

This page shows you how to map a DittoFS share as a drive on Windows, over SMB
(recommended) or NFS. It assumes DittoFS already runs on a Linux or macOS host
and that you have a DittoFS user with a grant on the share. For an Active
Directory domain, follow the [Windows AD setup runbook](windows-ad-setup.md)
first.

> **Not available yet on this version.** This version serves neither SMB nor
> NFS: the adapters cannot be turned on yet (`dfsctl adapter set` is refused
> with `unknown_key`), and they answer no file operation yet. A new
> installation cannot create a share yet either (see
> [Getting started, step 5](getting-started.md#5-stores-and-namespaces)).
> The steps below describe Windows clients of the **released server**.

For the Windows test VM and the manual checklists contributors use, see
[Testing on Windows](../internals/testing-windows.md).

## Table of Contents

- [Before you start](#before-you-start)
- [Mounting an SMB share](#mounting-an-smb-share)
  - [Map a drive with net use](#map-a-drive-with-net-use)
  - [Map a drive with PowerShell](#map-a-drive-with-powershell)
  - [Browse with Explorer](#browse-with-explorer)
  - [Per-user access on a shared workstation](#per-user-access-on-a-shared-workstation-active-directory)
  - [Guest access](#enabling-insecure-guest-logons-if-needed)
- [Mounting an NFS share](#mounting-an-nfs-share)
- [Known limitations](#known-limitations)
- [Troubleshooting](#troubleshooting)

---

## Before you start

### Check your Windows version

DittoFS serves SMB on port **12445** by default. The Windows SMB client uses
port 445 unless told otherwise, and only **Windows 11 24H2 and Windows Server
2025** or later can be told otherwise.

| Your Windows | What to do |
|--------------|------------|
| Windows 11 24H2 / Server 2025 or later | Map the drive with `/TCPPORT:12445`, as below. |
| Anything older | Run DittoFS on port 445; see [Running on standard ports](install.md#running-on-standard-ports-production). |

### Check the network

From PowerShell, check Windows reaches the DittoFS host (here `192.0.2.10`):

```powershell
Test-NetConnection -ComputerName 192.0.2.10 -Port 12445
```

Success looks like this (trimmed):

```console
ComputerName     : 192.0.2.10
RemoteAddress    : 192.0.2.10
RemotePort       : 12445
TcpTestSucceeded : True
```

If `TcpTestSucceeded` is `False`, check that:

- DittoFS is running on the host (`dfs start`);
- the host firewall allows port 12445 (SMB) or 2049 (NFS) from this machine;
- a VM's network reaches the host: bridged and host-only modes work, NAT needs
  a port forward.

---

## Mounting an SMB share

### Map a drive with net use

Open Command Prompt **as administrator** (`/TCPPORT` needs it):

```cmd
net use Z: \\192.0.2.10\archive /user:alice * /TCPPORT:12445
```

| Part | Why |
|------|-----|
| `Z:` | The drive letter to map. |
| `\\192.0.2.10\archive` | The server and the share name. |
| `/user:alice` | The DittoFS user to sign in as. For a domain user, `/user:CORP\alice`. |
| `*` | Prompt for the password, so it is not left in your command history. |
| `/TCPPORT:12445` | DittoFS's SMB port. Leave it out when DittoFS serves port 445. |

Success prints `The command completed successfully.`

If `dfsctl` is installed on the Windows machine, it builds the same command.
`--dry-run` prints it without running it:

```console
> dfsctl share mount archive Z: --protocol smb --username alice --dry-run
net use Z: \\192.0.2.10\archive /user:alice * /TCPPORT:12445
```

Reconnect the drive at every logon with `/persistent:yes`. Disconnect it with:

```cmd
net use Z: /delete
```

> Do not write `\\192.0.2.10@12445\archive`. That is WebDAV syntax: Windows
> tries HTTP and never reaches the SMB server.

### Map a drive with PowerShell

```powershell
New-SmbMapping -LocalPath Z: -RemotePath \\192.0.2.10\archive -UserName alice -Password (Read-Host 'Password') -Persistent $true -TcpPort 12445
```

`Read-Host` shows the password as you type it; use `net use … *` (above) if
someone may be watching. Disconnect it with `Remove-SmbMapping -LocalPath Z:`.

### Browse with Explorer

Type the path in Explorer's address bar:

```
\\192.0.2.10\archive
```

Windows asks for a user and password if it needs them. Explorer cannot set a
port, so this works only when DittoFS serves port 445.

### Per-user access on a shared workstation (Active Directory)

When DittoFS has joined your AD domain, each person who logs on to a Windows
machine reaches the share **with their own permissions**, with no per-user setup
in DittoFS. Each logon holds its own Kerberos ticket, and DittoFS authorizes the
user by their AD user and group SIDs. On one shared machine, alice gets her
access level, bob gets his, and an administrator gets theirs.

To map the drive at logon for everyone, use a Group Policy drive map (**User
Configuration → Preferences → Windows Settings → Drive Maps**) or a logon
script with:

```cmd
net use Z: \\files.corp.example.com\archive /persistent:yes
```

No password is passed: Windows uses the logged-on user's ticket. Use the
server's DNS name, not its IP address, or Windows falls back to NTLM. Setting
this up is covered step by step in the [Windows AD setup runbook](windows-ad-setup.md).

### Enabling insecure guest logons (if needed)

You need this **only to connect as a guest**, with no user. A DittoFS user with
a password, or a domain user, does not need it. DittoFS never turns a failed
sign-in into a guest session.

Windows 11 24H2 and later block guest access by default. Guest access sends
nothing encrypted and proves nothing about the server, so leave it off unless
you need it. To turn it on:

**Group Policy (`gpedit.msc`):**

1. Press `Win+R`, type `gpedit.msc`, press Enter.
2. Go to **Computer Configuration → Administrative Templates → Network →
   Lanman Workstation**.
3. Open **Enable insecure guest logons**, set it to **Enabled**, click OK.
4. Restart the Workstation service, or reboot.

**Registry (Windows Home, or a script), as administrator:**

```powershell
Set-ItemProperty `
  -Path "HKLM:\SYSTEM\CurrentControlSet\Services\LanmanWorkstation\Parameters" `
  -Name "AllowInsecureGuestAuth" -Value 1 -Type DWord
Restart-Service LanmanWorkstation
```

---

## Mounting an NFS share

Use SMB on Windows if you can. The Windows NFS client has fewer features than
the Linux and macOS clients, and it can only mount from port **2049**, while
DittoFS serves NFS on 12049 by default.

1. **Install the NFS client.** Open **Settings → Apps → Optional features →
   More Windows features**, expand **Services for NFS**, tick **Client for
   NFS**, click OK, and restart if asked.
2. **Run the NFS adapter on port 2049** on the DittoFS side:

   ```bash
   dfsctl adapter set nfs --enabled --setting port=2049
   ```

   (Refused with `unknown_key` on this version; see the note at the top.)
3. **Mount the share.** `dfsctl` prints the command. Run it in Command Prompt:
   in PowerShell, `mount` means something else, so type `mount.exe` there.

   ```console
   > dfsctl share mount archive Z: --protocol nfs --dry-run
   mount -o mtype=hard,nolock \\192.0.2.10\archive Z:
   ```

   `mtype=hard` retries instead of failing when the server is slow;
   `nolock` keeps locks on the client, since NFSv3 locking needs a portmapper
   on port 111.
   The share's NFS path (`/archive`) is written with backslashes.

With the adapter still on 12049, `dfsctl` refuses and says why:

```console
> dfsctl share mount archive Z: --protocol nfs --dry-run
Error: dfsctl share mount: the Windows NFS client mounts only from port 2049, and the NFS adapter serves 12049
```

Unmount with `umount Z:`.

The Windows NFS client sends an anonymous user unless it is told your UID and
GID. Files you create may then belong to the anonymous user. See the
[NFS guide](nfs.md#identity-squashing-root_squash-and-friends) for how DittoFS treats such users.

---

## Known limitations

| Limitation | Effect | What to do |
|-----------|--------|------------|
| No SMB multichannel | One TCP connection per session | Throughput is that of one connection |
| Windows NFS is best-effort | Fewer features; port 2049 only | Use SMB |
| No NTFS object IDs | `FSCTL_CREATE_OR_GET_OBJECT_ID` is not supported | Ordinary use is not affected |
| No DFS referrals | DFS namespaces do not work | Connect to the server by name or IP |

SMB 3 encryption and signing, durable handles and server-side copy **are**
supported, and change notifications partly; see the [SMB guide](smb.md).

---

## Troubleshooting

### Connection issues

| Symptom | Likely cause | Fix |
|---------|--------------|-----|
| "The network path was not found" | DittoFS is not running, the firewall blocks the port, or Windows used port 445 | Run `Test-NetConnection` (above); add `/TCPPORT:12445` |
| "Access denied" | Wrong password, or no grant on the share | `dfsctl grant list archive`. A local user must exist (`dfsctl user list`). An AD user needs no local account, but needs a grant on them or a group, made with `dfsctl grant set archive --directory 'CORP\alice' --access read`; see the [AD runbook](windows-ad-setup.md) |
| "System error 1219" | Windows already has a connection to this server as another user | `net use * /delete /y`, then retry |
| "The specified network name is no longer available" | The connection dropped | Retry `net use`; check the DittoFS log |
| "You can't access this shared folder because your organization's security policies block unauthenticated guest access" | Guest access is blocked | Sign in as a user, or see [guest access](#enabling-insecure-guest-logons-if-needed) |

### Permission issues

| Symptom | Likely cause | Fix |
|---------|--------------|-----|
| Explorer's Security tab is empty | The server could not build the file's security descriptor | Check the DittoFS log |
| `icacls` shows permissions you did not set | DittoFS builds the Windows permissions from the Unix mode bits when a file has no ACL | Expected; set an ACL to override |
| `Set-Acl` fails | The ACL holds an entry DittoFS cannot store | See [ACL fidelity](smb-acl-fidelity.md) for what is kept |

### Performance issues

| Symptom | Likely cause | Fix |
|---------|--------------|-----|
| Large copies are slow | One TCP connection (no multichannel) | Check the network bandwidth |
| Explorer feels slow | Many small requests, often from Explorer extensions | Turn off extensions that preview or scan files |
| Office takes long to save | Office saves through a temporary file, a write and a rename | Expected |

### NFS issues

| Symptom | Likely cause | Fix |
|---------|--------------|-----|
| `mount` is not recognized | Client for NFS is not installed | Install it (step 1 above) |
| Mount fails with "Network Error" | The NFS adapter is not on port 2049 | Move it to 2049 (step 2 above) |
| "Permission denied" on the mount | Windows sends the anonymous user, which the file modes do not let in | Use SMB, which signs in as a real user |
