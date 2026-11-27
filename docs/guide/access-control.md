# Access Control

This page shows how to decide who can read and write a share: which users get in, from
which machines, as whom, and what the files themselves allow. Each section is a task
you will recognise, with the `dfsctl` commands to run and what the user sees afterwards.

For the design behind it, see [the security model](../internals/security-model.md) and
[the ACL design](../internals/acl-design.md). Terms such as ACL, ACE and SID are in the
[glossary](glossary.md).

> **On this version:**
>
> - This build serves no file protocol and cannot create a share yet (see
>   [Getting Started](getting-started.md#5-stores-and-namespaces)). `dfsctl user`,
>   `dfsctl group` and `dfsctl netgroup` work today; the share, grant and mount steps
>   below are the commands to use once a share exists.
> - A client that only *claims* a user ID, as NFS `AUTH_SYS` does, is refused: mapping a
>   claimed uid to a DittoFS user is not built yet. Only a caller that signs in (an SMB
>   password or Kerberos sign-in) or an anonymous one reaches a share.
> - NFS Kerberos is not served yet (see [NFS](nfs.md)), so on this version a signed-in
>   caller arrives over SMB.

## Two checks on every request

Every request passes two checks, and both must allow it:

1. **Share access.** Is this caller allowed into the share, and at what level? This is
   set with `dfsctl`: the share's **grants** (who), and its **export policy** (from
   which addresses, with which kind of sign-in, and as whom).
2. **File permissions.** Do the file's mode bits and ACL allow this operation for this
   caller? This is set from a client, with `chmod`, `chown`, `nfs4_setfacl` or
   Windows' Security tab.

A grant never changes a file's owner, mode or ACL. A user granted `read-write` on a
share still cannot write a file whose permissions deny it.

The grant levels:

| Level | What it allows |
| --- | --- |
| `none` | nothing: the caller is refused, even if one of its groups is granted more |
| `read` | read files and list directories |
| `read-write` | read and change files, as the file permissions allow |
| `admin` | everything, past file permissions too: an admin can change any file's owner, mode or ACL (not on a share that squashes the caller, see [squash](#map-users-to-an-anonymous-user-squash)) |

A caller's **own** grant decides, if it has one. With no grant of its own, the highest
grant among its groups decides. With neither, a grant to all signed-in users or to
everyone decides, if the share has one (`dfsctl grant` has no flag naming those yet).
Otherwise it is refused. A new share admits nobody
until a grant names someone.

## Give alice read-write on the archive share

1. Create the user, with the Unix user ID her files are owned by, and give her a
   password for SMB sign-in:

   ```bash
   dfsctl user create alice --uid 1000
   dfsctl user set-password alice
   ```

   ```console
   NAME   ID                                    UID   ROLE  EMAIL  DISABLED
   alice  8c814c1f-9930-4780-b8b5-b96002bba793  1000              no
   ```

   `set-password` prompts for the password. It needs `keys.protocol` in the config file
   (see [Getting Started](getting-started.md#2-initialize-and-start-the-server)).

2. Grant her read-write:

   ```bash
   dfsctl grant set archive --user alice --access read-write
   dfsctl grant list archive
   ```

3. Let her write at the top of the share. A new share's top directory is mode `0755`
   and owned by nobody, so a `read-write` grant alone does not let alice create files
   there. No `dfsctl` command sets its owner yet. Instead, a user granted `admin` on the
   share does it from a client, for example from Windows:

   ```cmd
   icacls \\server\archive /setowner alice
   ```

   Or that admin creates a directory for alice, owned by her, and leaves the top as it
   is.

4. alice mounts the share and works in it:

   ```bash
   dfsctl share mount archive /mnt/archive --protocol smb --username alice
   ```

   She can create and change files she owns. Writing a file someone else owns with mode
   `0644` fails with `Permission denied`, because the file permissions say no.

## A group of read-only users

Create a group, add its members, and grant the group `read`:

```bash
dfsctl group create auditors --gid 2000
```

```console
NAME      ID                                    GID
auditors  fd1591fa-f9b8-4e98-9536-9559fcbff2d7  2000
```

```bash
dfsctl group add-member auditors bob
dfsctl group add-member auditors carol
dfsctl grant set archive --group auditors --access read
```

bob and carol can now mount `archive`, list it and read files the file permissions let
them read. Any change, such as creating, editing or deleting a file, fails with
`Permission denied`.

To keep one member out while the rest of the group keeps its access, give that member a
grant of its own. An own grant beats the group's:

```bash
dfsctl grant set archive --user carol --access none
```

To let carol back in through the group, remove her own grant rather than setting it to
something else:

```bash
dfsctl grant remove archive --user carol
```

## Make a whole share read-only

To stop every change to a share, whatever the grants say:

```bash
dfsctl share export-policy update archive --read-only
```

A write then fails with `Read-only file system` (`EROFS`) rather than
`Permission denied`, so the user can tell the share is read-only rather than that they
lack a permission. To allow writes again:

```bash
dfsctl share export-policy update archive --read-only=false
```

## Allow only some machines

An export policy holds **client rules**: an address range, or a netgroup, and the
access it allows. A share with no client rules admits every address. Once it has
rules, the first one that matches the client's address decides, and a client that
matches none is refused. A rule naming a netgroup that does not exist admits nobody.

Allow the office network read-write, and one build machine read-only:

```bash
dfsctl share export-policy update archive \
  --client 192.0.2.0/24=read-write \
  --client 198.51.100.7/32=read
```

`--client` replaces the whole list of rules, so name every rule you want to keep. A
`read` rule turns a client's access into read-only even for a user granted `read-write`,
and its writes fail with `Read-only file system`.

To reuse one list of machines on several shares, put it in a **netgroup**:

```bash
dfsctl netgroup create office --member 192.0.2.0/24 --member 203.0.113.15
dfsctl share export-policy update archive --client @office=read-write
```

```console
NAME    MEMBERS
office  192.0.2.0/24,203.0.113.15
```

`dfsctl netgroup update office --member …` replaces the members. Use addresses and
CIDR ranges.

> **Host names in a netgroup are not resolved on this version.** A host name in an
> allowing rule (`=read`, `=read-write`) matches no client, and in a refusing rule
> (`=none`) it matches every client. Write the machine's address instead.

To see the whole policy:

```bash
dfsctl share export-policy get archive
```

## Require a stronger sign-in

The export policy also says how a caller must prove who it is:

| Flag | Values | Meaning |
| --- | --- | --- |
| `--min-proof` | `none`, `claimed`, `secret`, `ticket` | the weakest sign-in allowed: none at all, a claimed uid (NFS `AUTH_SYS`), a password (SMB NTLM), or a Kerberos ticket |
| `--min-protection` | `none`, `integrity`, `privacy` | whether messages must be signed (`integrity`) or encrypted (`privacy`); above `none` needs `--min-proof secret` or `ticket` |
| `--flavor` | `none`, `sys`, `kerberos` | the NFS sign-in methods the share offers, in order of preference |
| `--smb-encrypt` | | every SMB message to this share is encrypted |

Require a password or Kerberos sign-in, so anonymous and guest sessions are refused:

```bash
dfsctl share export-policy update archive --min-proof secret
```

Require Kerberos with encryption:

```bash
dfsctl share export-policy update archive --flavor kerberos --min-proof ticket --min-protection privacy
```

## Map users to an anonymous user (squash)

**Squash** makes callers act as the share's anonymous user for file permissions. It is
set with `--squash`:

| `--squash` | Who is squashed |
| --- | --- |
| `none` | nobody |
| `root` *(default)* | a caller signed in as root |
| `all` | every caller |

Map every user to the anonymous user, for a share where who wrote a file should not
matter:

```bash
dfsctl share export-policy update archive --squash all
```

Grants are read **before** the squash, against who the caller really is. So on an `all`
share, alice still needs her own grant (or her group's) to get in. Once in, her files
are created and checked as the anonymous user, not as alice.

How grants and squash combine for a caller who signed in as alice (a user with a uid):

| alice's grant | `--squash none` | `--squash root` | `--squash all` |
| --- | --- | --- | --- |
| `read-write` | gets in, acts as alice | gets in, acts as alice | gets in read-write, acts as anonymous |
| `read` | gets in read-only, acts as alice | gets in read-only, acts as alice | gets in read-only, acts as anonymous |
| `admin` | gets in, acts as alice, past file permissions | gets in, acts as alice, past file permissions | gets in, acts as anonymous, no bypass |
| `none`, or no grant at all | refused | refused | refused |

Two kinds of caller always act as the anonymous user, whatever `--squash` says:

- an anonymous or guest session, which proves no identity. It gets only what the share
  grants to everyone; `dfsctl grant` has no flag for that yet.
- a signed-in account with no uid, such as a computer account. It gets the anonymous
  user's access too.

`--squash root` only matters for a caller that maps to root, and no sign-in maps to root
on this version, so today it behaves like `none`. No squash mode keeps root's
administrator rights: only an `admin` grant lets a caller past file permissions, and
only when the caller is not squashed.

## Grant users and groups from Active Directory

With a directory connected (see [Identity](identity.md)), grant a directory user or
group by its `DOMAIN\name`. No local user is created:

```bash
dfsctl grant set archive --directory 'CORP\alice' --access read-write
dfsctl grant set archive --directory 'CORP\Engineering' --access read
```

The node looks the name up in the directory first. If the directory cannot find it, the
command fails and grants nothing. A directory user's groups come from the directory, so
a grant to `CORP\Engineering` reaches every member, including members of groups nested
inside it. [windows-ad-setup.md](windows-ad-setup.md) walks through a whole Active
Directory setup.

## File permissions and ACLs

Once a caller is in the share, the file's own permissions decide. Each file has one
ACL, which NFSv4 clients and SMB clients both read and change, so an ACL set from
Windows is enforced for an NFS client too.

A file with no ACL is checked by its mode bits, as on any Unix system.

### From Linux over NFSv4

```bash
# Let alice read and write a file
nfs4_setfacl -a 'A::alice@example.com:rwatTnNcCy' /mnt/archive/report.txt

# Let the file's group read it
nfs4_setfacl -a 'A::GROUP@:r' /mnt/archive/report.txt

# Show the ACL
nfs4_getfacl /mnt/archive/report.txt
```

### From Windows over SMB

```cmd
:: Give alice full control
icacls \\server\archive\report.txt /grant alice:(F)

:: Deny Everyone write
icacls \\server\archive\report.txt /deny Everyone:(W)

:: Show the ACL
icacls \\server\archive\report.txt
```

Or use the file's **Properties > Security** tab in Explorer.

### chmod on a file with an ACL

`chmod` keeps the ACL's named entries, such as one for alice, but none of them then
grants more than the new mode bits allow, as a POSIX ACL mask does. `ls -l` shows the
mode bits the ACL works out to.

## Known limitations

1. **Audit entries do nothing yet.** An ACL's audit entries (the SACL, Windows'
   Auditing tab) are stored and shown back to SMB clients, but no audit event is
   written when an operation matches one. See
   [SMB ACL fidelity](smb-acl-fidelity.md#sacl-audit).
2. **The share's anonymous user cannot be chosen.** The export policy has no `dfsctl`
   flag for it, so squashed callers act as the built-in anonymous user.
3. **No grant to everyone from `dfsctl`.** `dfsctl grant` names a user, a group or a
   directory principal; it has no flag for "everyone" or "all signed-in users".
4. **No command sets a share's top-directory owner.** It starts owned by nobody; an
   `admin` grantee sets it from a client.

## Common problems

- **"Permission denied" when creating a file at the top of a new share.** The top
  directory is owned by nobody. A user granted `admin` must give it an owner from a
  client, or create subdirectories for users. See step 3 of [Give alice read-write](#give-alice-read-write-on-the-archive-share).
- **A group member is refused although the group is granted.** The member has a grant
  of its own, which beats the group's. Check `dfsctl grant list archive` and remove it
  with `dfsctl grant remove archive --user <name>`, for example
  `dfsctl grant remove archive --user carol`.
- **Every client is refused after adding a netgroup rule.** The netgroup holds host
  names, which are not resolved, or a rule above it refuses the client. Use addresses.
- **An NFS mount with `sec=sys` is refused.** A claimed uid is not accepted on this
  version. Use SMB with a password or Kerberos sign-in.
