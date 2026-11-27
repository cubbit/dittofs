# SMB Windows-ACL Fidelity

This page lists, item by item, what happens to Windows permissions (security
descriptors) when a client reads them from DittoFS over SMB, changes them, and
reads them back. Use it to predict what Explorer's **Security** tab, `icacls`
or `Set-Acl` will show and keep.

> **Not available yet on this version.** This version does not serve SMB, so no
> client can read or write a security descriptor through it yet. The matrix
> describes the **released server**. On this version a user's or group's SID is
> stored with the user or group rather than computed from its UID, so the SID
> rows below may change when SMB is served again.

For the model behind it (why DittoFS stores NFSv4-style ACLs, how access is
evaluated, how mode bits stay in step), see [Access control](access-control.md).

## How to read the tables

Each row is one part of a security descriptor. The columns are:

| Column | Meaning |
|--------|---------|
| **Aspect** | The part of the security descriptor, or the operation on it. |
| **State** | How faithfully it round-trips: a client writes it, reads it back, and compares. |
| **What happens** | What a client sees, and any condition or caveat. |

The **State** values:

| State | Meaning |
|-------|---------|
| **Works** | Read back exactly as written. Nothing is lost. |
| **Partial** | Kept, with the limit or condition the row states. |
| **Unsupported** | Refused, dropped or not implemented. The row says which. |

A short background: Windows access masks and NFSv4 access masks use the same bit
positions, so masks are stored unchanged. What DittoFS translates is the
principal (a Windows SID or an NFSv4 `user@domain` name) and one inheritance
flag, whose bit differs between the two (`0x10` on Windows, `0x80` in NFSv4).

## Owner and group

| Aspect | State | What happens |
|--------|-------|--------------|
| Read the owner | Works | UID 0 shows as `BUILTIN\Administrators` (`S-1-5-32-544`). Any other UID shows as a SID in the server's own domain: `S-1-5-21-<machine>-<uid×2+1000>`. |
| Read the group | Works | `S-1-5-21-<machine>-<gid×2+1001>`. The different offset means a user and a group with the same number never share a SID. |
| Change the owner | Works, for the server's own SIDs | A SID the server issued is turned back into a UID and stored. Any other SID is **ignored without an error**: the owner stays as it was. |
| Change the group | Works, for the server's own SIDs | Same as the owner. A user SID given as the group is accepted as that UID. |
| Permission to change them | Works | Needs **Take ownership** (`WRITE_OWNER`) on the open file. One right covers both owner and group. |

`OWNER@` and `GROUP@` inside an access entry show as the file's current owner
and group. They are not the same as `CREATOR OWNER` and `CREATOR GROUP`, which
only matter in inheritable entries (see [Inheritance](#inheritance-flags)).

## Access entries (DACL)

| Aspect | State | What happens |
|--------|-------|--------------|
| Allow entries | Works | Kept as written. |
| Deny entries | Works | Kept as written. Putting deny before allow is the client's job, as on Windows. |
| Access rights (the mask) | Works | Kept bit for bit. Generic rights are expanded when written (see [Generic rights](#generic-rights)). |
| Number of entries | Partial | At most **128 entries** and **64 KiB** per DACL. A larger one is refused with `STATUS_INVALID_PARAMETER`; nothing is stored. |
| Other entry types (object, callback, mandatory label, …) | Unsupported | The whole change, or the whole create, is refused with `STATUS_INVALID_PARAMETER`: nothing is stored and no file is created. This applies to an audit section too, even when the client did not ask to change it. |
| Integrity labels (`icacls /setintegritylevel`) | Unsupported | Refused with `STATUS_INVALID_PARAMETER`, never dropped silently. |
| Empty DACL (no entries) | Works | Read back as empty, which denies everyone. No default is added. |
| A file with no ACL | Partial | Shown with a default DACL: Full Control for the owner and for `SYSTEM`, protected from inheritance. That DACL is for display only: access to such a file is still decided by its Unix mode bits. |

## Inheritance flags

| Flag | Windows bit | NFSv4 bit | State | What happens |
|------|-------------|-----------|-------|--------------|
| Object inherit (OI) | `0x01` | `0x01` | Works | |
| Container inherit (CI) | `0x02` | `0x02` | Works | |
| No propagate (NP) | `0x04` | `0x04` | Works | Inherited by direct children only. |
| Inherit only (IO) | `0x08` | `0x08` | Works | Not applied to the folder itself; passed on to children. |
| Inherited | `0x10` | `0x80` | Works | The bit differs between the two and is translated both ways. |
| Inheritance when a file is created | | | Works | Follows Windows (and Samba). `CREATOR OWNER` and `CREATOR GROUP` are replaced by the creator's identity. |
| Re-applying inheritance after a parent changes | | | Works | Inherited entries are recomputed. Explicit entries, and each child's own protection flag, are kept. |

## Security descriptor flags

| Flag | State | What happens |
|------|-------|--------------|
| Self-relative (`0x8000`) | Works | Always set. |
| DACL present (`0x0004`) | Works | Set when the client asks for the DACL. |
| DACL protected (`0x1000`) | Works | Kept. Blocks inheritance from parents; it is never itself passed to children. |
| DACL auto-inherited (`0x0400`) | Works, normalized | Kept only when the change also carries **auto-inherit required** (`0x0100`), as Windows and Samba do. On the released server, a share created with the `--acl-canonicalize-inherited=false` option keeps it exactly as sent; this version has no such option yet. |
| Auto-inherit required (`0x0100`) | Works, request only | Acted on when written; never shown on read. |
| SACL present (`0x0010`) | Works | Set when the client asks for the audit section. See [SACL](#sacl-audit). |

## Null DACL

A null DACL (present, but with no body) means "everyone has full access".

| Aspect | State | What happens |
|--------|-------|--------------|
| Read | Works | Shown as a null DACL. |
| Write | Works | Stored as a null DACL. A change that asks to set the DACL but carries none also stores a null DACL. |

## SACL (audit)

| Aspect | State | What happens |
|--------|-------|--------------|
| Read the audit entries | Works | Shown in Explorer's **Auditing** tab. A file with none shows an empty audit section. |
| Change the audit entries | Works | Stored. The DACL is left alone when only the SACL changes, and the other way round. Needs **Manage auditing** (`ACCESS_SYSTEM_SECURITY`). |
| Audit and alarm entry types, success and failure flags | Works | Kept both ways. |
| Generating audit events | Unsupported | No audit log is written and no alarm raised, whatever the entries say. Audit entries never affect access. |

## Generic rights

| Aspect | State | What happens |
|--------|-------|--------------|
| `GENERIC_READ`, `WRITE`, `EXECUTE`, `ALL` when written | Works | Expanded to the file rights they stand for, as Windows does. For example, `GENERIC_ALL` (`0x10000000`) is stored as `FILE_ALL_ACCESS` (`0x001F01FF`). |
| Generic rights in inherited entries | Works | Expanded for each child when it inherits. Inherit-only entries keep them for later children. |
| Generic rights on read | n/a | Never shown: they were expanded when written. |

## SIDs from other domains

| Aspect | State | What happens |
|--------|-------|--------------|
| Owner or group SID from another domain | Unsupported | Ignored: it cannot be turned into a UID. The owner or group stays as it was. |
| Access entry naming a SID from another domain | Partial | Kept and read back unchanged, but it matches no local UID or GID. |
| NFSv4 name with no SID (for example `alice@EXAMPLE.COM`) | Partial | Shown as a SID computed from the name. It is stable, but it is not a real AD SID and cannot be turned back into a UID. Two names could in theory get the same SID. |
| SID shown as a name in Explorer | Works | The server's own SIDs, well-known SIDs and domain SIDs show as names. A raw SID shows only for a principal with no directory entry. |
| Well-known SIDs (Everyone, SYSTEM, Administrators, Owner Rights, Creator Owner, Creator Group) | Works | Mapped both ways to their NFSv4 equivalents (`EVERYONE@`, `SYSTEM@`, …). |

## What a client may change

DittoFS checks each part of a change against the rights the client was granted
when it **opened** the file, not against the file's current DACL:

| The client changes | It needs |
|--------------------|----------|
| The DACL | **Change permissions** (`WRITE_DAC`) |
| The owner or group | **Take ownership** (`WRITE_OWNER`) |
| The SACL | **Manage auditing** (`ACCESS_SYSTEM_SECURITY`) |

If any part lacks its right, the whole change is refused with
`STATUS_ACCESS_DENIED`.

## Tested with

- **Windows 11 Explorer, Security tab.** Reading, editing and name display.
  DACLs are shown as allow entries only, as Samba does, so Explorer shows no
  stray "Deny" entries.
- **Active Directory over Kerberos.** A domain user's SID mapped to the right
  UID against a real domain controller.
- **smbtorture `smb2.acls`.** Building and parsing descriptors, inheritance,
  generic rights and the auto-inherited flag.
- **macOS Finder.** Not tested for permissions; treat it as unverified.

## Known limitations

1. **Audit entries are stored, never acted on.** No audit log, no alarm.
2. **SIDs from other domains map to no UID or GID.** Entries naming them are
   kept, but match nobody locally. Their names still display.
3. **SIDs computed from NFSv4 names are not real AD SIDs.** They are stable,
   but cannot be turned back into a UID.
4. **A file with no ACL is checked by its mode bits.** The default DACL Windows
   shows for it is for display only.
5. **Owner and group are stored with the file, not in its ACL.** Changing them
   does not count as an ACL change (see [Access control](access-control.md#known-limitations)).
6. **Clearing the read-only box may not make a file writable.** The Windows
   read-only attribute is stored apart from the Unix mode bits. A file made
   read-only from NFS or a shell (`chmod 0444`) shows as read-only in Windows.
   Clearing the box clears only the Windows attribute: Windows then shows the
   file as writable, but the mode bits still refuse writes. Run `chmod u+w` on
   it from NFS or a shell. An attribute change never widens the mode, by design.

## References

- [Access control](access-control.md): the cross-protocol ACL model.
- [MS-DTYP](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-dtyp/): SID, ACE, ACL and security descriptor formats.
- [RFC 7530, section 6](https://www.rfc-editor.org/rfc/rfc7530#section-6): the NFSv4 ACL model.
