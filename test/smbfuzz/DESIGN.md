# smbfuzz — differential SMB fuzzing against a Samba reference

**Audience:** anyone changing the SMB adapter, the ACL model or the file operations behind
them, and anyone triaging a divergence the fuzzer reports.

How `smbfuzz` works and how to use it; [README.md](README.md) is the quick start. The
document is about the repository's test tooling, so it names the tools, files and Linux
interfaces it is built from. [§16](#16.%20Open%20questions) lists what is still undecided.

## Summary

**The problem.** The SMB suites DittoFS runs today (smbtorture, WPTS, the Go e2e tests) check
behaviour someone thought to write a test for. Windows ACLs, DOS attributes and byte-range
locks, and their interaction with ordinary file operations, are a space nobody enumerates by
hand: an ACE has 14 rights, 5 flags, 2 types and any trustee; a security descriptor has any
number of them in any order; and what it means depends on who asks, with which open flags,
after which earlier operation, while which lock is held by whom.

**The proposal.**

- **One Go tool, `smbfuzz`,** generates random *plans* of 12 file operations, runs each plan
  against two SMB shares joined to the same Active Directory domain — a stock **Samba** share
  on ext4 (the reference) and a **DittoFS** share — and compares every result.
- **Every step is a system call** (`open`, `pwrite`, `fallocate`, `fcntl` lock, `setxattr` of
  a security descriptor, …) on the Linux kernel's SMB client, so each flag of each call is
  under test.
- **Several users in one plan:** each domain user is a separate worker process with its own
  uid and its own SMB session on a `multiuser` mount, so "alice creates, bob reads" is two
  steps of one plan.
- **Both authentications:** a run uses NTLM or Kerberos from the AD-DC fixture, on both
  shares alike, so the users' groups reach each server through NETLOGON or through the
  ticket's PAC.
- **Same errno, same output:** a step whose errno or observed output differs between the two
  shares is a divergence, and the round fails.
- **Repeatable:** test number N is always the same plan. A plan is a pure function of its
  round number and the generator version, and every value comes from a finite list: offsets
  and lengths from powers of two, DittoFS's own size boundaries and their neighbours; names,
  users, modes and trustees from fixed sets.
- **Minimised:** a failing round is replayed, then shrunk one step at a time to the smallest
  plan that still diverges the same way, and that plan is what is reported.
- **Registered with `dt`** as suite `smb-fuzz`; which tiers run it, and how many rounds, is
  decided later.

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

### In plain words

Two shops sell the same product; one is the original. To find where the copy differs you
don't read the manual: you walk into both with the same shopping list, ask for the same things
in the same order, and write down every time the two answers differ. `smbfuzz` does that with
two SMB servers. The original is Samba, the most widely deployed SMB server that is not
Windows, configured the way its documentation recommends for Windows ACLs. The copy is
DittoFS. The shopping list is a **plan**: 12 operations, each done by one of a few domain
users. The plans look random, but plan number 4242 is the same plan every time anyone asks
for it, on any machine.

Where the analogy stops: a difference is not automatically DittoFS's bug. Samba has its own
bugs and its own choices, and DittoFS deliberately differs in places
([RFC 22](../../docs/internals/rfcs/rfc-22-smb.md)). Every difference is either fixed, or written down as a known
divergence with its reason, and the list of those is part of the suite.

### The cast and the picture

The examples use the set's cast: users alice and bob, the `profiles` share over SMB. The
fixture adds carol and the domain Administrator.

```
                 ┌──────────────────────────────┐
                 │ Samba AD DC   DITTOFS.AD     │
                 │ alice bob carol Administrator│
                 └───────┬───────────────┬──────┘
             NTLM / LDAP │               │ NTLM / LDAP
       ┌─────────────────┴───┐     ┌─────┴────────────────┐
       │ Samba member        │     │ DittoFS               │
       │ [profiles] on ext4  │     │ [profiles]            │
       │ acl_xattr           │     │                       │
       └──────────┬──────────┘     └──────────┬────────────┘
                  │ SMB 3.1.1                 │ SMB 3.1.1
       ┌──────────┴───────────────────────────┴────────────┐
       │ client host: two cifs mounts, multiuser,          │
       │ /mnt/ref (Samba)          /mnt/dut (DittoFS)       │
       │                                                    │
       │ smbfuzz ──► worker alice (uid 20001) ──┐           │
       │   plan    ► worker bob   (uid 20002) ──┤ syscalls  │
       │   compare ► worker carol (uid 20003) ──┤ on both   │
       │   minimise► worker admin (uid 20000) ──┘ mounts    │
       └────────────────────────────────────────────────────┘
```

### One round, end to end

Round 4242. The generator draws this plan (format in [§5.3](#5.3%20The%20plan%20file)):

```
# smbfuzz plan gen=1 round=4242 profile=sd root=open
01 alice  mkdir    d0 0755
02 alice  open     d0/f0 O_RDWR|O_CREAT|O_EXCL 0644 -> alice.h0
03 bob    stat     d0/f1
04 alice  pwrite   alice.h0 off=4096+1 len=8192 data=seq
05 alice  setacl   d0/f0 part=dacl D:P(A;;FA;;;alice)(D;;FR;;;bob)(A;;FR;;;WD)
06 carol  readdir  d0
07 alice  lock     alice.h0 kind=excl off=0 len=4096
08 bob    open     d0/f0 O_RDONLY -> bob.h0
09 bob    pread    bob.h0 off=0 len=16384
10 carol  unlink   d0/f1
11 alice  ftruncate alice.h0 len=2
12 bob    getacl   d0/f0 part=ntsd
```

1. `smbfuzz` creates `r4242-a0/` on both shares as Administrator, with the round root's
   security descriptor named in the header ([§10](#10.%20Rounds%2C%20isolation%20and%20cleanup))
   and the tree of files and directories the plan's `# tree` lines list
   ([§6.3](#6.3%20Names%3A%20the%20pool%20and%20the%20tree); this one has none), and every path
   in the plan is relative to it.
2. Step by step, the step's user's worker runs the call on `/mnt/ref/…`, then on
   `/mnt/dut/…`, and returns errno and output for each.
3. Steps 1–7 agree. Step 3 is `ENOENT` on both: a plan need not make sense, the two shares
   only have to agree on what it does.
4. Step 8: Samba answers `EACCES`, because bob's deny entry comes before Everyone's allow;
   DittoFS answers `0`. That is a divergence with signature `open:errno:EACCES/0`, and the
   round stops ([§9.1](#9.1%20Lockstep%2C%20and%20the%20first%20divergence%20ends%20the%20round)).
5. The round is replayed twice more; it diverges the same way every time, so it is stable
   ([§11.1](#11.1%20Replay%20before%20shrinking)).
6. The minimiser removes one step at a time and keeps each removal after which the replay
   still ends in `open:errno:EACCES/0`. What is left:

   ```
   01 alice  open     f0 O_RDWR|O_CREAT|O_EXCL 0644 -> alice.h0
   02 alice  setacl   f0 part=dacl D:P(D;;FR;;;bob)(A;;FR;;;WD)
   03 bob    open     f0 O_RDONLY -> bob.h0
   ```

   (step 2 shrank too: [§11.3](#11.3%20Shrinking%20parameters)).
7. The report carries the original plan, the minimised one, both sides' trace, and the
   one-line command that replays it. The minimised plan goes into the corpus once the bug is
   fixed, and is replayed by every run after that.

Had DittoFS let bob in, step 9 would have tested something else: alice holds an exclusive
lock on the first 4 KiB, and an SMB byte-range lock is mandatory, so bob's read of that range
must fail on both servers ([§7.5](#7.5%20Byte-range%20locks)).

### Terms

- **Reference** (`ref`): the Samba share. **Device under test** (`dut`): the DittoFS share.
- **Plan:** an ordered list of steps; **step:** one user, one operation, its parameters.
- **Round:** one plan, run once on both shares in a fresh directory, numbered from 1. Round N
  and test N are the same thing.
- **Divergence:** a step whose errno or compared output differs. **Signature:** the short
  string naming a divergence, used to tell "the same failure" from "another failure".
- **Domain:** the finite list a parameter is drawn from.
- **Worker:** the per-user process that makes the system calls.

### How this document is organised

§1–2 say what the tool is for and what it must do. §3 builds the four machines in the
picture, §4 the per-user workers. §5 and §6 are the plan and its domains, §7 every operation
with every parameter, §8 the ACL generator. §9 says what is compared and how it is
normalised, §10 how rounds are kept apart and cleaned up, §11 how a failure is minimised, §12
how it runs. §13–16 are alternatives, risks, plan and open questions.

## 1. Purpose

Find every place where an SMB client, doing ordinary file operations as ordinary domain users
under arbitrary Windows ACLs, DOS attributes and byte-range locks, sees DittoFS behave
differently from Samba — and reduce each one to a plan short enough to read.

### 1.1 Non-goals

- **Not a protocol fuzzer.** Malformed SMB2 messages, credit abuse and dialect negotiation
  belong to smbtorture, WPTS and a future wire-level fuzzer. Every request here is one the
  Linux client builds from a well-formed system call.
- **Not a crash finder first.** A server crash or hang is reported (it is a divergence, §9.5),
  but plans are shaped to reach interesting *semantics*, not to maximise code coverage.
- **Not a correctness oracle.** Samba is a reference, not the specification. Where Samba is
  wrong or DittoFS deliberately differs, the divergence is listed as known (§12.2), with its
  reason, and does not fail the suite.
- **Not NFS.** The same machinery could drive an NFS mount against knfsd; that is a later
  RFC, and nothing here may assume it will not happen (§7 is protocol-neutral where it can be).
- **Not performance.** No timing is compared.

## 2. Requirements

| # | Requirement | Met when |
|---|---|---|
| R1 | Written in Go | `smbfuzz` and its worker are one Go binary under `test/smbfuzz/` |
| R2 | A temporary domain with a Samba reference share and DittoFS, both joined to it | §3's fixture brings all four up from nothing and tears them down; its probes (§3.6) pass before round 1 |
| R3 | Operations are low-level system calls, so each flag is tested | every operation in §7 is one system call (or one `ioctl`) with its flags as plan parameters |
| R4 | Operations run as different users within one plan | §4's workers; a plan names at least two distinct users (§5.2) |
| R5 | Same errno on both sides for every step | §9.2 |
| R6 | Same output on both sides: attributes, ACLs, DOS attributes, locks, bytes read, listings | §9.3–§9.4 |
| R7 | Repeatable: test N is always the same plan, and a run can start from any N | §5.1; `smbfuzz plan --round N` prints the same plan on any machine for a given generator version, and `smbfuzz run --from N` starts there |
| R8 | Plans of a fixed length, 12 operations | §5.2 |
| R9 | Every parameter comes from a finite domain | §6; the generator has no other source of values |
| R10 | Offsets and lengths from powers of two, DittoFS's size boundaries and their neighbours, or a sum of two | §6.4 |
| R11 | A failing round is minimised by removing one operation at a time until none can be removed | §11 |
| R12 | Random ACLs and ownership changes, compared with the reference | §7.6, §8 |
| R13 | Beyond ACLs: read, write, ranged write, truncate, punch hole, delete, mkdir, chown, setattr, DOS attributes, byte-range locks and more | §7's tables |
| R14 | Not run is never passed | §12.3, following [RFC 29](../../docs/internals/rfcs/rfc-29-test-harness.md) |

## 3. Topology

### 3.1 The domain controller

The DC is the existing AD-DC fixture image (`test/integration/ad-dc/`): Samba as an Active
Directory domain controller, realm `DITTOFS.AD`, domain `DITTOFS`. Its entrypoint already
creates alice (member of devs, nested in engineering, with RFC 2307 attributes) and bob
(engineering, without). For this suite it **MUST** also create:

- **carol**, a member of Domain Users only, without RFC 2307 attributes;
- the machine account of the Samba member (§3.2), or allow it to join with
  `net ads join` as Administrator;
- the group **fuzzers**, holding alice and carol, so a group ACE can match two of the users
  and miss the third;
- for Kerberos (§3.5): a keytab per user (alice, bob, carol, Administrator) exported to the
  shared keytab volume beside the service keytab it already exports, and a DNS record for
  each server's name, so the client asks for the service principal each server holds.

The domain's users, groups and their memberships are part of the generator's domains (§6.1,
§6.2). Changing them changes what plans mean, so they are listed in one place, the fixture,
and the generator version is bumped when they change (§5.1).

### 3.2 The Samba reference

A second container from the same image, run as a **member server** of the domain (not a DC:
the DC's own file server enforces different defaults). Its share is a directory on an ext4
filesystem, which **MUST** support `user_xattr` (the default on ext4); a loop-mounted ext4
image file is the default, so the reference never depends on the host's filesystem.

The configuration is the one Samba documents for shares whose permissions are Windows ACLs
only:

```ini
[global]
    security = ads
    realm = DITTOFS.AD
    workgroup = DITTOFS
    idmap config * : backend = tdb
    idmap config * : range = 3000-7999
    idmap config DITTOFS : backend = rid
    idmap config DITTOFS : range = 10000-999999
    server min protocol = SMB3_11
    kernel oplocks = no
    ea support = yes
    store dos attributes = yes
    map acl inherit = yes

[profiles]
    path = /srv/fuzz
    read only = no
    vfs objects = acl_xattr
    acl_xattr:ignore system acls = yes
    acl_xattr:default acl style = windows
```

Rules:

- The share's own share-level ACL **MUST** grant Everyone full control, so the only access
  decision is the file's security descriptor, as on DittoFS's side (§3.3).
- No `admin users`, `force user`, `inherit owner` or `inherit permissions`: each rewrites the
  answer the ACL would have given.
- `store dos attributes = yes` keeps every DOS attribute in an extended attribute instead of
  deriving some from the Unix mode, so all of §7.4's attributes can be set and read back.
- Any setting added later is a change to the reference and **MUST** say in a comment beside it
  which divergence it was added for.

### 3.3 DittoFS

DittoFS is built from the checkout under test and started by the harness's fixture code
([RFC 29 §4.2](../../docs/internals/rfcs/rfc-29-test-harness.md#4.2%20Fixtures%3A%20the%20DittoFS%20a%20suite%20runs%20against)),
joined to the same domain:

- NTLM authentication passed through to the DC over NETLOGON with the `dittofs$` machine
  account, `netbios_domain = DITTOFS`;
- the LDAP identity provider pointed at the DC, so SIDs and uids resolve the way the
  reference's idmap resolves them for users with RFC 2307 attributes, and by RID otherwise;
- one share, `profiles`, with a share permission granting Domain Users read-write, so the
  share ceiling never decides (the reference's share ACL is Everyone full control);
- the metadata and block stores of the fixture's profile. The default is `badger` metadata
  and a local block store, the production profile minus S3; `memory` is allowed for a fast
  local run, but never the only profile a scheduled run uses (the index's "what must not
  stand in");
- every size setting at its default, because §6.4's boundaries are the defaults (§6.4).

### 3.4 The client host

The client host mounts both shares with the kernel's SMB client, and runs `smbfuzz` and its
workers. It **MUST** be a Linux machine (or VM) of its own, not a container: the kernel
client's credential keys and its idmap upcalls run in the host's initial namespaces, so a
container cannot give each worker its own session.

Both shares **MUST** be mounted with the same options, and the options are part of the suite
(a different option is a different test):

| Option | Why |
|---|---|
| `vers=3.1.1` | the dialect both servers are tested at |
| `multiuser` | each worker's uid picks its own SMB session (§4.2) |
| `sec=ntlmssp` or `sec=krb5` | the run's authentication (§3.5) |
| `noperm` | the client does no permission check of its own: every `EACCES` comes from the server |
| `cache=none` | no page cache: every read and write reaches the server |
| `actimeo=0` | no attribute cache: every `stat` reaches the server |
| `nolease`, `nohandlecache` | no leases or cached handles, so one user's open never changes what the next step sees through client-side caching |
| `rsize=1048576`, `wsize=1048576` | the client splits I/O into the same requests for both servers, whatever each advertises; 1 MiB is DittoFS's advertised maximum |
| `serverino` | inode numbers from the server, so hard links are visible (compared as relations, §9.4) |
| `nosharesock` | each mount its own connection, so one server's reconnect never stalls the other |
| `soft`, `echo_interval=10` | a hung server becomes an error instead of an unkillable process (§9.5) |
| `cifsacl` | the `posix` profile only (below) |

`nobrl` is **not** set: byte-range locks must reach the server (§7.5). Root mounts both shares
with Administrator's credentials, so the mount's own session — the one the passthrough ioctl
uses (§7.2) — is admin's.

**Two profiles**, chosen per run:

- **`sd`** (default): no `cifsacl`. Ownership and permissions are read and written only as
  security descriptors (§7.6). `stat`'s uid, gid and permission bits are made up by the
  client from mount options, so they are not compared. Needs nothing on the client but
  `cifs-utils` and `keyutils`.
- **`posix`**: `cifsacl` on both mounts, and the client host joined to the domain with
  winbind (`idmap config DITTOFS : backend = rid`, the reference's mapping) so the client's
  `cifs.idmap` upcall maps SIDs to uids. Adds `chown`, `chmod` and the uid, gid and mode of
  `stat` to the compared surface, because the client then translates them into security
  descriptor changes and back.

### 3.5 Authentication: NTLM and Kerberos

A run uses one of two authentications, on both mounts alike; it is a dimension of the run, like
the profile, and every combination is valid (`ntlm`/`krb5` × `sd`/`posix`):

- **`ntlm`** (`sec=ntlmssp`): the reference checks the user's response against the DC as a
  member server does; DittoFS passes it through to the DC over NETLOGON. Group SIDs come from
  the NETLOGON answer on both.
- **`krb5`** (`sec=krb5`): each worker holds a ticket from the AD-DC fixture's KDC, and the
  client's `cifs.upcall` asks for a service ticket per server. The reference decrypts it with
  its machine account's keys, made by its domain join (`cifs/<reference name>`); DittoFS with
  the service keytab the DC fixture exports (`cifs/dittofs.dittofs.ad`). Group SIDs come from
  the ticket's PAC on both.

The two reach the user's groups through different code in DittoFS, so a plan that passes under
one and diverges under the other points at identity, not at the file operation. Kerberos
needs the client to name each server by the name its principal carries, so the mounts use the
DNS names of §3.1, never an address.

`cifs.upcall`, which gets the service tickets, reads only the host's own `/etc/krb5.conf` and
looks the calling uid up in the password database before it reads that uid's credential
cache. Under `krb5` the fixture therefore changes the client host, and `fixture down` undoes
each change: it writes `/etc/krb5.conf` only when the host has none, adds the servers' names
in a marked block of `/etc/hosts`, and creates a system user per worker uid. Each worker's
ticket comes from its keytab through the domain controller's own `kinit`, so the client needs
no Kerberos tools of its own; a credential cache is portable and is copied to the worker's.

### 3.6 Probes before round 1

The fixture **MUST** check, before any round, and stop with "not run" if any check fails:

1. each of alice, bob, carol and Administrator has a working session on both mounts (each
   worker creates and removes a file in a probe directory);
2. on the reference, alice cannot read a file whose DACL denies her read even though its mode
   would allow it, and bob can read one whose DACL allows him — that is what proves
   `ignore system acls` and `noperm` took effect. DittoFS is not probed for it: the mount
   options are the same for both, so a DittoFS that lets alice in is a divergence for the
   rounds to find, not a broken fixture;
3. a `getxattr("system.cifs_ntsd")` on the probe directory returns a parseable descriptor on
   both sides (not `ntsd_full`: reading a SACL is under test, and DittoFS refuses it today,
   §7.6);
4. an exclusive lock taken by alice makes bob's write over it fail on both shares — that is
   what proves the server enforces locks (§7.5). The probe writes rather than taking a
   conflicting lock, which the client refuses itself, and rather than reading, which Samba's
   default `strict locking = auto` does not check under a read oplock;
5. setting `HIDDEN` through `user.cifs.dosattrib` reads back on both shares — that is what
   proves the client can set DOS attributes (§7.4);
6. in the `posix` profile, `getpwnam("DITTOFS\\alice")` on the client resolves, and
   `stat` of alice's probe file reports her uid on both mounts;
7. under `krb5`, after check 1 every worker's credential cache holds a `cifs/` service ticket
   for each of the two servers — that is what proves no session fell back to NTLM;
8. on the reference, admin can read the probe directory's SACL — that is what proves the
   privilege grant of §7.6 took effect. DittoFS is not probed for it: there it is a finding.

## 4. Running as different users: workers

### 4.1 One process per user

`smbfuzz` runs as root and starts one **worker** per user: the same binary re-executed with a
hidden `worker` subcommand, with `SysProcAttr.Credential` set to that user's uid, gid and
supplementary groups. All system calls of a step run in its user's worker. The orchestrator
makes none itself.

A process, not a thread, because:

- the kernel client picks the SMB session from the calling task's fsuid and finds its
  password in that uid's keyring; a credential that is not the whole process's is a
  credential some Go runtime thread will not have;
- the Go runtime applies `setuid` to every thread of the process, so one process cannot be
  two users at once;
- a worker stuck in a system call, or killed, takes only its own user with it.

### 4.2 Credentials

In the `sd` profile the workers' uids are local and fixed — admin 20000, alice 20001,
bob 20002, carol 20003 — because the kernel client uses the uid only to pick the session. In
the `posix` profile they are the uids winbind maps the domain users to, so the client's
translation of ownership agrees with the worker's identity.

At start, each worker adds its own NTLM credential to its user keyring (`add_key` of type
`logon`, description `cifs:a:<server address>`, payload `<DOMAIN\user>:<password>`, once per
server), which is what `cifscreds add` does. Its first call on each mount then opens its
session. Kerberos (`sec=krb5`) instead gives each worker a credential cache from the user's
keytab, exported by the DC fixture (§3.1): the worker runs `kinit -k` into its own cache,
`FILE:/tmp/krb5cc_<uid>`, which is where `cifs.upcall` looks for the calling uid's tickets.
Root does the same as Administrator before mounting. A worker renews its ticket when it starts
a round with less than an hour left, so a long run never sees an expiry mid-round.

### 4.3 The worker protocol

The orchestrator and a worker exchange one JSON object per line over the worker's standard
input and output:

```go
type Request struct {
    Step   int             // 1-based, for the trace
    Side   string          // "ref" or "dut"
    Op     string          // §7's name
    Args   json.RawMessage // the op's parameters, already resolved to paths and fds
}

type Reply struct {
    Errno  int             // 0 on success; the raw errno otherwise
    Out    json.RawMessage // the op's output (§9.3), absent on error
    Took   time.Duration   // recorded, never compared
}
```

A worker keeps its own open files in a table of **handle slots** keyed by (side, slot name);
§5.4 says how slots are named in plans. A step that names a slot nobody opened gets `EBADF`,
on both sides, from the worker itself — a consistent answer, not an error of the tool.

Each request has a deadline (default 30 s). A worker that misses it is killed and restarted,
the step is recorded as a timeout, and the round ends (§9.5).

## 5. The plan

### 5.1 The round number is the seed

A plan is a pure function of two values:

- the **round number** `N`, from 1 upwards;
- the **generator version** `gen`, an integer constant in the code.

The random source is PCG (`math/rand/v2`'s `PCG`) seeded with `(N, 0x534D4246555A5A31)`; the
second word is a fixed constant and never changes. There is no other seed: test 1 is test 1
for everyone, and `smbfuzz run --from N --count M` runs tests N to N+M−1. The generator
**MUST** read only `Uint64` from the source and derive every draw with its own helpers
(uniform below `n` by rejection, weighted choice, subset), because the convenience methods of
`rand.Rand` are not promised to return the same values across Go releases, while PCG's output
for a seed is fixed by its algorithm.

**A generator version is frozen once released.** Anything that would make round N produce a
different plan — a new operation, a changed weight, a new domain value, a changed fixture
cast (§3.1) — is made in a new version, never in a released one. The old version stays in the
code and runnable with `--gen`; the default is the newest. So "test 4242 of version 1" names
one plan forever, and a report always states both numbers.

> ponytail: freezing keeps one copy of the generator per version, and the copies differ in
> little. Upgrade to versioned tables (operations, weights, domains) under one shared drawing
> code when a third version would make the copies a maintenance cost; until then a copy is
> the cheapest way to be sure a released version cannot drift.

### 5.2 Shape

- A plan has **12 steps**.
- Each step draws its **user** from §6.1 by weight, and its **operation** from §7 by weight.
- A plan **MUST** name at least two distinct users. If the 12 draws picked one, the generator
  redraws the user of the last step from the others. (A rule, not a retry loop: the draw
  stays a function of the round number.)
- The round root's descriptor (§10) is drawn first and written in the plan's header; the
  tree (§6.3) is drawn next and written as the header's `# tree` lines.
- Operations are drawn without regard to whether they make sense: reading before creating,
  closing twice, removing a directory that was never made, unlocking what was never locked.
  The only bias is §5.4's.

### 5.3 The plan file

A plan is a text file, one step per line, so a human can read, edit and replay it:

```
# smbfuzz plan gen=1 round=4242 profile=sd root=open
<nn> <user> <op> <positional args...> [key=value ...] [-> <slot>]
```

- Paths are relative to the round's directory and drawn from §6.3's pool.
- `# tree dir <path> [fill=<n>]` and `# tree file <path> len=<size> data=<pattern>` lines,
  after the header and parents before children, are what exists before step 1 (§6.3).
- Flags are written symbolically (`O_RDWR|O_CREAT`), modes in octal, offsets and lengths as
  the expression they were drawn as (`4096+1`, `chunk.max-1`), security descriptors as SDDL
  with trustees by their fixture names (§8.3), DOS attributes by name (`HIDDEN|SYSTEM`), data
  as a pattern name (§6.6).
- `-> alice.h0` names the slot an `open` fills; handle operations name a slot as their first
  argument.

`smbfuzz run --plan FILE` runs a plan file; `smbfuzz plan --round N` prints round N's plan
without running it. Both read and write the same format, so a minimised plan, a hand-written
regression and a generated plan are interchangeable.

### 5.4 The biases: reuse what the plan already named

Without a bias, most handle operations would hit an empty slot, most path operations a name
nothing created, and most locks a range no read or write touches. The generator therefore
keeps a **symbolic history** of the plan so far — which (user, slot) pairs an earlier step
opened, which paths an earlier step named, which ranges an earlier step locked, which paths a
namespace step changed — and never a model of results (it does not know whether the open or
the lock succeeded). It knows the tree (§6.3), because the tree is in the plan:

- a handle operation picks, with probability 0.85, a (user, slot) pair an earlier `open`
  named, and runs as that user; otherwise any user and any of that user's slots `h0`–`h3`;
- a path argument picks, with probability 0.5, a path an earlier step named; otherwise a pool
  name: one the tree created with probability 0.7, one it did not otherwise, so a creation, a
  rename or a link also meets names that are free. An operation meant for one kind of object
  (`mkdir`, `rmdir`, `readdir` a directory; `open`, `truncate`, `unlink`, `link` a file) draws
  a pool name of that kind with probability 0.8;
- an operation that reads attributes, a listing or a descriptor (`stat`, `lstat`,
  `getdosattr`, `getbtime`, `listxattr`, `getacl`, `readdir`) picks, with probability 0.5, a
  path an earlier namespace step changed: a rename's source and target, their parents and the
  pool names that moved with it; a link's names; what an `unlink`, a `mkdir`, an `rmdir` or a
  creating `open` touched, and its parent. So what a rename did to the objects around it is
  looked at, and §9.4 compares their attributes against what they were before;
- `rename` and `link` draw their two names together, §7.3;
- `unlink` picks, with probability 0.3, a file an earlier step opened;
- `getacl` picks, with probability 0.5, a path an earlier step wrote a descriptor on
  (`setacl`, `setowner`, an ACE edit), and is then made, with probability 0.7, by a user other
  than the writer if the draw named the writer: whether the descriptor just written lets
  someone else read it is the test. An ACE edit picks such a path with probability 0.5, so
  entries are added to and taken from lists the plan built as well as inherited ones;
- `readdir` picks, with probability 0.3, the wide directory when the tree filled it;
- a data, truncate or lock operation on a file some earlier step locked picks, with
  probability 0.5, an offset and length that overlap that lock: the same range, a range
  starting inside it, or one ending inside it;
- a `pread` on a file an earlier `pwrite` or `fallocate` named, by any user, starts with
  probability 0.6 where one of those ranges starts, one byte into it, or at its last byte, and
  reads that range's length or a fresh one, so reads land on written data rather than mostly on
  holes and past the end of the file.

- a handle operation drawn before any step of the plan opened a slot becomes an `open`, with
  probability 0.8.

Every probability here is a generator constant (part of `gen`), not a setting.

## 6. Domains

Every parameter of every operation is drawn from one of these lists. A list is part of the
generator version (§5.1).

### 6.1 Users

| Name | Account | Groups | Weight |
|---|---|---|---|
| `alice` | `DITTOFS\alice` | Domain Users, devs, engineering, fuzzers | 35 |
| `bob` | `DITTOFS\bob` | Domain Users, engineering | 30 |
| `carol` | `DITTOFS\carol` | Domain Users, fuzzers | 20 |
| `admin` | `DITTOFS\Administrator` | Domain Admins, Domain Users, Administrators | 15 |

### 6.2 Trustees

Who an ACE or an owner may name. Each is written in plans by its name — the SDDL alias where
SDDL has one — and translated to its SID when the descriptor is built:

| Name | SID | Why it is in the list |
|---|---|---|
| `alice`, `bob`, `carol`, `Administrator` | domain user SIDs | the users who act |
| `devs`, `engineering`, `fuzzers` | domain group SIDs | nested and flat group membership |
| `DU` (Domain Users), `DA` (Domain Admins) | `<domain>-513`, `-512` | every user / only admin |
| `WD` (Everyone) | `S-1-1-0` | |
| `AU` (Authenticated Users) | `S-1-5-11` | |
| `CO` (Creator Owner), `CG` (Creator Group) | `S-1-3-0`, `S-1-3-1` | replaced on inheritance |
| `OW` (Owner Rights) | `S-1-3-4` | restricts the owner's implicit rights |
| `BA` (Administrators), `BU` (Users) | `S-1-5-32-544`, `-545` | builtin groups, mapped differently by each server |
| `SY` (System) | `S-1-5-18` | |
| `ghost` | `<domain>-99999` | a well-formed SID nobody holds: setting it as owner or in an ACE must fail or succeed the same way on both |
| `unix-1000` | `S-1-22-1-1000` | Samba's unmapped-uid SID form, which DittoFS must at least store |

### 6.3 Names: the pool and the tree

A round names objects from a fixed **pool** of paths, so a plan revisits the same few objects,
and starts with a **tree**: the part of the pool admin creates before step 1. The rest of the
pool is free names, for a step to create, or to rename or link onto.

**The pool** has three directory levels with the same names at every level, so renaming a
directory to another name at its depth carries every name below it onto another pool name, and
later steps can still reach what moved:

- directories `d0`, `d1` at the top, in each of them, and in each of those: 14 directories,
  `d0/d1/d0` the deepest;
- files `f0`, `f1` at the top and in every directory: 30 files, `d1/d0/d1/f1` the deepest;
- case: `F0`, `D0`, `D0/f0`, `d0/F0`, `d1/F0` (each share's case rule decides whether these are
  new names, [RFC 22 §12.2](../../docs/internals/rfcs/rfc-22-smb.md#12.2%20Case));
- awkward but legal on both: `a b` (space), `é` (NFC), `É` (case of a non-ASCII letter),
  `.hidden`;
- long: a 255-byte name (`l` repeated);
- the wide directory `w`, with `w/e0`, `w/e1` and `w/new`.

Names the client rewrites or refuses before the server sees them — a trailing dot or space,
a component over 255 bytes, characters SMB reserves — are not drawn: they test the client.

**The tree** is drawn per round, parents before children: a top-level directory exists with
probability 0.85, a deeper one with 0.7, each if its parent does; a file with 0.55; `a b`, `é`,
`.hidden` and the long name with 0.3. The case names and `w/new` are never created, since the
first would collide with what they alias and the second is the wide directory's free name. A
file's length is 0 with probability 0.2, a §6.4 size up to `2^21` otherwise, and with
probability 0.05 any write length; its content is a §6.6 pattern, written as step `100+k` for
the tree's `k`-th object, so no two objects and no plan step share bytes.

**The wide directory** holds `fill` entries `x0000`, `x0001`, … that no step names: 0 with
probability 0.6, 128 with 0.2, 600 with 0.15, 1500 with 0.05. They exist so a listing spans
several `QUERY_DIRECTORY` responses, and a creation, removal or rename among them is followed by
a listing that has to resume correctly. With `fill=0` the directory exists with probability
0.3, empty.

The tree's setup, §10, gives every object fixed times, so a step's first look at a tree object
compares exact values (§9.4).

### 6.4 Offsets and lengths

Values are drawn from named **points**. The generic points:

| Point | Value | Note |
|---|---|---|
| `0`, `1`, `2` | 0, 1, 2 | |
| `4096` | 4 KiB | one page and the reference's ext4 block |
| `2^13`, `2^14`, `2^15` | 8 KiB, 16 KiB, 32 KiB | |
| `2^20`, `2^21` | 1 MiB, 2 MiB | `2^20` is also the mount's `rsize`/`wsize` (§3.4), so the largest single SMB request |

DittoFS's own size boundaries:

| Point | Value at generator version 1 | What changes at that size |
|---|---|---|
| `smb.io` | 1 MiB | the largest read or write DittoFS advertises; a write past it is split by the client, and each piece is one journal record |
| `chunk.min` | 1 MiB | the chunker's smallest chunk, except the last of a stretch |
| `chunk.avg` | 4 MiB | where the chunker's boundary mask changes |
| `chunk.max` | 16 MiB | the chunker's largest chunk; a cut is forced here |
| `carve.block` | 4 MiB | the journal's carve block: content is offered to the carver in pieces of this size |
| `block` | 8 MiB | the block size of the block store |
| `rfc2.min`, `rfc2.target`, `rfc2.max` | 64 KiB, 256 KiB, 1 MiB | the chunk bounds [RFC 2](../../docs/internals/rfcs/rfc-2-carver.md) specifies, which the code is moving to |

No point has its neighbours as points of its own. A value is one point (probability 0.5) or the
sum of two (0.5), and the second term of a sum is `1` or `-1` with probability 0.25 each, so
`4096-1`, `block+1` and `2^20+2^13` are all reachable and every point is met as often at its
neighbours as on itself. `-1` is only ever a second term, and `0` is never summed, so a value is
never negative. The boundaries are the defaults of the build when
the generator version was released; the fixture runs DittoFS with default sizes (§3.3), and a
change of a default is a new generator version (§5.1), so a plan never silently tests a
boundary that has moved.

**Offsets** use every point, so a file can reach 32 MiB, written sparsely. **Lengths** of
`pread`, `pwrite`, `write`, `copy_file_range` and `fallocate` use only points up to `block`
(and sums up to 16 MiB), and points above 4 MiB are drawn with weight 0.1 of the others, so a
round writes at most a few tens of MiB on each side. The journal's segment size (256 MiB) is
left out: crossing it needs files too large for a round.

### 6.5 Modes

`0000`, `0400`, `0444`, `0600`, `0640`, `0644`, `0700`, `0750`, `0755`, `0777`. In the `sd`
profile a mode reaches the server only through `open`'s create and `mkdir`, where the client
uses it at most for the read-only attribute; in the `posix` profile it becomes a DACL.

### 6.6 Data

A write's bytes are a **pattern**, so a plan never stores data and a read can be checked
without remembering what was written:

| Pattern | Byte `i` of the buffer written by step `s` |
|---|---|
| `zero` | `0x00` |
| `ones` | `0xFF` |
| `seq` | `(i + s) mod 251` |
| `rand` | PCG seeded with `(N, s)`, byte `i` of its stream |

A byte depends on its place in the write, not in the file, so `write` (whose file offset
the plan does not know) and `pwrite` are filled the same way. An extended attribute's value
is written `<pattern>:<length>` (`seq:4096`) and filled the same way.

`zero` is in the list on purpose: all-zero content is where a sparse-aware path differs from a
plain one (the index's fixtures rule).

### 6.7 Times

For `utimensat` and `setbtime`: seconds from `0`, `1`, `1000000000`, `2147483647` (2³¹−1),
`2147483648`, `4294967296` (2³²), `-11644473600` (FILETIME 0, 1601-01-01); nanoseconds from
`0`, `1`, `100` (one FILETIME tick), `999999999`; or, for `utimensat` only, `UTIME_OMIT` or
`UTIME_NOW`.

### 6.8 Extended attribute names and values

Names: `user.a`, `user.b`, `user.A` (EA names are case-insensitive over SMB), `user.` plus 200
bytes. Values: empty, 1 byte, 255 bytes, 4096 bytes, 65000 bytes — each a §6.6 pattern.

### 6.9 DOS attributes

| Attribute | Value | Drawn because |
|---|---|---|
| `READONLY` | `0x0001` | denies write and delete regardless of the ACL |
| `HIDDEN` | `0x0002` | |
| `SYSTEM` | `0x0004` | |
| `DIRECTORY` | `0x0010` | not settable: setting it on a file must be refused or ignored alike |
| `ARCHIVE` | `0x0020` | set by servers on write; whether each sets it, and when, is compared |
| `NORMAL` | `0x0080` | valid only alone |
| `TEMPORARY` | `0x0100` | |
| `SPARSE_FILE` | `0x0200` | not settable this way; set by `FSCTL_SET_SPARSE` |
| `OFFLINE` | `0x1000` | |
| `NOT_CONTENT_INDEXED` | `0x2000` | |
| `ENCRYPTED`, `COMPRESSED` | `0x4000`, `0x0800` | not settable this way |
| `INTEGRITY_STREAM`, `NO_SCRUB_DATA` | `0x8000`, `0x20000` | ReFS attributes, unsupported on both: equal errors are the test |
| `0x00400000` | undefined bit | a server either strips it, keeps it or refuses it |

A value is a subset: each settable attribute with p = 0.25, each other one with p = 0.05, and
the empty set with p = 0.1 (which the client sends as `NORMAL`).

## 7. Operations

Every operation is one system call, made by the step's user's worker on the step's side. The
tables list every parameter and its domain. "Compared" names the output that §9 compares
besides the errno, which every operation compares.

### 7.1 Opening and closing

**`open(path, flags, mode) -> slot`** — `openat(AT_FDCWD, …)`.

| Flag | Drawn as | What the client sends |
|---|---|---|
| access mode | one of `O_RDONLY` 40, `O_WRONLY` 25, `O_RDWR` 35 | desired access `GENERIC_READ` / `GENERIC_WRITE` / both |
| `O_CREAT` | p = 0.5 | disposition `FILE_OPEN_IF` |
| `O_EXCL` | p = 0.3 with `O_CREAT`, p = 0.05 without (the meaningless combination) | `FILE_CREATE` |
| `O_TRUNC` | p = 0.2 | `FILE_OVERWRITE` / `FILE_OVERWRITE_IF` |
| `O_APPEND` | p = 0.15 | `FILE_APPEND_DATA` access instead of `FILE_WRITE_DATA` |
| `O_DIRECTORY` | p = 0.1 | `FILE_DIRECTORY_FILE` |
| `O_NOFOLLOW` | p = 0.05 | |
| `O_SYNC` / `O_DSYNC` | p = 0.05 each | `FILE_WRITE_THROUGH` |

`mode` from §6.5. Fills `slot`; if the slot is already open, the old descriptor is closed
first, silently, on both sides. Compared: nothing beyond errno.

`O_PATH`, `O_TMPFILE` and `O_NOATIME` are not drawn: the client answers them itself without a
request.

**`close(slot)`**. Compared: errno only (a deferred write error surfaces here). Closing
releases the slot's byte-range locks, which later steps observe.

### 7.2 Data

| Operation | Parameters | Compared |
|---|---|---|
| `pread(slot, off, len)` | §6.4, §6.4 | bytes returned: count, and the bytes (§9.3) |
| `pwrite(slot, off, len, data)` | §6.4, §6.4, §6.6 | bytes written |
| `write(slot, len, data)` | §6.4, §6.6 — at the descriptor's offset, so `O_APPEND` matters | bytes written |
| `lseek(slot, off, whence)` | §6.4; `SEEK_END` 40, `SEEK_DATA` 30, `SEEK_HOLE` 30 | resulting offset (§9.4 for DATA/HOLE) |
| `ftruncate(slot, len)` | §6.4 | — |
| `truncate(path, len)` | §6.3, §6.4 | — |
| `fallocate(slot, mode, off, len)` | mode one of: `0` 20, `KEEP_SIZE` 15, `PUNCH_HOLE\|KEEP_SIZE` 30, `ZERO_RANGE` 15, `ZERO_RANGE\|KEEP_SIZE` 10, `COLLAPSE_RANGE` 5, `INSERT_RANGE` 5; §6.4, §6.4 | — |
| `setsparse(path, on)` | **admin only**; `ioctl(CIFS_QUERY_INFO)` with `PASSTHRU_FSCTL`, `FSCTL_SET_SPARSE`; `true`, `false` | — |
| `fsync(slot)`, `fdatasync(slot)` | | — |
| `copy_file_range(slot_in, off_in, slot_out, off_out, len)` | both slots of the step's user; §6.4 ×3 | bytes copied (server-side copy, `FSCTL_SRV_COPYCHUNK_WRITE`) |
| `ficlone_range(slot_in, off_in, slot_out, off_out, len)` | as above; `ioctl(FICLONERANGE)` | — (`FSCTL_DUPLICATE_EXTENTS_TO_FILE`, unsupported on ext4: equal errors are the test) |

`SEEK_SET` and `SEEK_CUR` are not drawn: the client answers them without a request.

**How `FSCTL_SET_SPARSE` reaches the server.** The Linux client has no ioctl of its own for
it. It sends it **implicitly**, and these are the paths that test it:

- `ftruncate`/`truncate` that extend a file by more than 8 KiB past its size send
  `SET_SPARSE(1)` before the new end of file;
- `fallocate(PUNCH_HOLE)` and `INSERT_RANGE` send `SET_SPARSE(1)`;
- `fallocate` mode `0` over a whole file can send `SET_SPARSE(0)`.

The client remembers the sparse state of each inode, sends the request only when it believes
the state changes, and, after a server answers `EOPNOTSUPP` once, never sends it again on that
tree connection. The result is visible as `SPARSE_FILE` in `getdosattr`, compared like any
other attribute (§9.3).

An **explicit** `SET_SPARSE` goes through the client's passthrough ioctl (`CIFS_QUERY_INFO`
with `PASSTHRU_FSCTL`, the one `smbinfo` uses). It needs `CAP_SYS_ADMIN` in the caller and
runs on the mount's own session, not the caller's, so it is drawn only for `admin`. The mount
is made with Administrator's credentials (§3.4), and only the admin worker gets
`CAP_SYS_ADMIN`, as an ambient capability. `setsparse` on a path the worker has not opened
opens it inside the same compound, so it takes a path.

### 7.3 Namespace

| Operation | Parameters | Compared |
|---|---|---|
| `mkdir(path, mode)` | §6.3, §6.5 | — |
| `rmdir(path)` | §6.3 | — |
| `unlink(path)` | §6.3 | — |
| `rename(old, new, flags)` | below; `0` 75, `RENAME_NOREPLACE` 25 (`renameat2`) | — |
| `link(old, new)` | below | — |
| `readdir(path)` | §6.3 (`opendir` + `getdents64` to the end) | the sorted list of (name, type) without `.` and `..` (§9.4) |
| `statfs(path)` | §6.3 (`statfs`, `FileFsFullSizeInformation`) | — (sizes and free space are each server's own) |

**What a rename's names are.** The source is, with probability 0.25, a directory above a file an
earlier step opened — a directory with an open handle below it, which Windows and Samba refuse
to rename; with 0.15 a file an earlier step opened; with 0.35 a directory, which the tree has
usually filled with subdirectories and files with content; otherwise any path. The target is,
with probability 0.3, the other name of the same kind in the same directory (`d0` to `d1`, `f0`
to `f1`), which the tree may have created: a file to replace, a directory empty or not; with 0.2
the same name in another directory at the same depth, a move across parents that keeps the
subtree in the pool; with 0.1 a name inside the source itself; with 0.05 the source with the
case of its last name changed; with 0.05 the source itself; otherwise any path. A hard link's
source is a file; its new name is, with probability 0.4, the same name in another directory,
with 0.2 the other name in the same directory, otherwise any file name.

Together with the tree, these reach a directory moved with its subtree, a move into its own
subtree, over a non-empty directory, a directory over a file and back, a case-only rename of a
full directory, and a rename or unlink of a file open through another handle (which the client
turns into a rename aside and a delete on close).

`symlink`, `mknod` and `RENAME_EXCHANGE` are not drawn in generator version 1: the first two
need a client-side encoding (`mfsymlinks`, reparse points) that is a test of the client, and
the third is refused by the client.

### 7.4 Attributes

| Operation | Parameters | Compared |
|---|---|---|
| `stat(path)`, `lstat(path)` | §6.3; `statx` with `STATX_BASIC_STATS\|STATX_BTIME` | §9.4's attribute set |
| `fstat(slot)` | | as `stat` |
| `utimensat(path, atime, mtime)` | §6.3, §6.7, §6.7 | — |
| `getdosattr(path)` | `getxattr("user.cifs.dosattrib")` | the DOS attribute bits (§9.4) |
| `setdosattr(path, attrs)` | `setxattr("user.cifs.dosattrib")`; §6.3, §6.9 | — |
| `getbtime(path)` | `getxattr("user.cifs.creationtime")` | the creation time, relative (§9.4) |
| `setbtime(path, t)` | `setxattr("user.cifs.creationtime")`; §6.3, §6.7 | — |
| `getxattr(path, name)` | §6.3, §6.8 | value |
| `setxattr(path, name, value, flags)` | §6.3, §6.8, §6.8; `0`, `XATTR_CREATE`, `XATTR_REPLACE` | — |
| `removexattr(path, name)` | §6.3, §6.8 | — |
| `listxattr(path)` | §6.3 | the sorted `user.*` names, without the client's own `user.cifs.*` |
| `chmod(path, mode)` | **posix profile only**; §6.3, §6.5 | — |
| `chown(path, owner, group)` | **posix profile only**; §6.3; owner and group from §6.2's users and groups or `-1` (unchanged) | — |

DOS attributes interact with the rest of the plan, and that interaction is most of their
test: a `READONLY` file must refuse a write open, a truncate and a delete on both servers
whatever its ACL grants; `HIDDEN` and `SYSTEM` must survive a rename and a write; `ARCHIVE`
must appear, or not, after the same writes.

### 7.5 Byte-range locks

SMB byte-range locks are **mandatory**: while one open holds an exclusive lock on a range, a
read or write of that range through any other open fails, and while it holds a shared lock,
writes through any other open fail. Locks belong to an open, not to a process or a user, so two
handles of the same user conflict like two users. The Linux client sends `SMB2 LOCK` for each
`fcntl` lock it cannot settle itself, but it keeps every lock granted to any open of an inode on
its mount and refuses a conflicting one with `EACCES` before either server sees it, and it
answers an unlock that matches no lock it recorded. Every user and slot of a plan shares one
mount per side, so **a lock-against-lock conflict, and an unlock of a range nothing locked, are
settled by the same client logic on both sides** and test neither server. What reaches the
servers is I/O through another open over a locked range: with `cache=none`, reads and writes
go to the server without a lock check on the client. Lock against lock on the server needs a
second client per side (§13), as share modes and oplocks do.

The operations use open-file-description locks (`F_OFD_*`), whose owner is the descriptor,
because that is the SMB model; classic `F_SETLK` locks belong to a process, and would merge
the locks of a worker's slots before the client sends them.

| Operation | Parameters | SMB request | Compared |
|---|---|---|---|
| `lock(slot, kind, off, len)` | `fcntl(F_OFD_SETLK)`; kind `shared` (`F_RDLCK`) 45, `excl` (`F_WRLCK`) 55; §6.4, §6.4 | `SMB2_LOCKFLAG_SHARED_LOCK` or `EXCLUSIVE_LOCK`, always with `FAIL_IMMEDIATELY` | — (a refusal versus `0` is the test; the client answers a conflict with `EACCES`) |
| `unlock(slot, off, len)` | `fcntl(F_OFD_SETLK, F_UNLCK)`; §6.4, §6.4 | `SMB2_LOCKFLAG_UNLOCK` | — |
| `testlock(slot, kind, off, len)` | `fcntl(F_OFD_GETLK)` | a lock and unlock pair, or none, as the client implements it | the returned type and range |
| `flock(slot, how)` | `LOCK_SH\|LOCK_NB` 40, `LOCK_EX\|LOCK_NB` 40, `LOCK_UN` 20 | a whole-file lock | — |

Length `0` in `fcntl` means "to the end of the file, whatever it becomes"; the client sends
it as the largest SMB length, so lengths are drawn from §6.4 with `0` included on purpose.

What plans reach, through these operations and §5.4's overlap bias:

- **conflicts between users and between handles:** shared against shared (allowed), shared
  against exclusive and exclusive against either (refused), across users and across two slots
  of one user — settled by the client, as above, so both sides agree whatever the servers do;
- **stacking:** the same open locking the same range twice, shared over shared, exclusive
  over its own shared; SMB keeps each lock separately, and each needs its own unlock;
- **unlocking:** a range not exactly matching any lock (SMB requires an exact match and
  answers `STATUS_RANGE_NOT_LOCKED`, where POSIX would split the lock; what the client makes
  of the difference is the same for both servers, what the server answers is the test), an
  unlock with nothing locked, a double unlock — the last two answered by the client;
- **edges:** a lock past the end of the file, a zero-length lock, a lock to the largest
  length, overlapping locks that touch at one byte (`off+len` against `off`);
- **enforcement:** `pread`, `pwrite`, `ftruncate`, `fallocate` and `copy_file_range` through
  another open, over a locked range or across its edge;
- **release:** `close` releases a slot's locks; `unlink` and `rename` of a locked file.

The blocking forms (`F_OFD_SETLKW`, `flock` without `LOCK_NB`) are never drawn: in a
sequential plan they wait forever. Oplocks, leases and share modes cannot be reached this way
— the mount turns leases off (§3.4) and the client always asks for every share access — and
are left to a wire driver (§13).

### 7.6 Security descriptors

Read and written as the client's extended attributes, so the client passes the binary
self-relative descriptor straight to `QUERY_INFO`/`SET_INFO`:

| Operation | Parameters | Compared |
|---|---|---|
| `getacl(path, part)` | §6.3; part `dacl` (`system.cifs_acl`) 30, `ntsd` (`system.cifs_ntsd`, owner+group+DACL) 50, `ntsd_full` (`system.cifs_ntsd_full`, + SACL) 20 | the descriptor, normalised (§9.3) |
| `setacl(path, part, sd)` | §6.3; part as above; `sd` from §8 | — |
| `setowner(path, owner, group)` | §6.3; owner and group from §6.2 — a set of `system.smb3_ntsd_owner`, which the client sends as owner and group together | — |
| `aceadd(path, part, index, ace)` | §6.3; part `dacl` 50, `ntsd` 30, `ntsd_full` 20; index `0` 30, `1` 20, `last` 20, `end` 30; `ace` from §8.1, an audit entry going to the SACL | the descriptor as read before the edit |
| `acedel(path, part, index)` | as above; index `0` 40, `1` 25, `last` 35 | as above |
| `aceedit(path, part, index, mask, aceflags)` | as `acedel`; mask and flags from §8.1, written over the entry's, its type and trustee kept | as above |

**Editing one entry.** `setacl` writes a whole descriptor drawn from scratch; tools that grant
or revoke one entry (`icacls /grant`, Explorer) instead read the descriptor, insert, remove or
change one entry, and write the whole list back, including the entries the server returned as
inherited. The three edit operations do that, each on its own side from what its own server
returned: `index` is the position (`last` the last entry, `end` one past it), and a position
past the list, or a DACL that is null or absent for a removal or a change, is refused by the
worker with `ENOENT`, alike on both sides when the two lists agree. Adding to a null DACL
creates a list with that one entry. What was read is compared even when the write then fails.

The client has no owner-only set, so owner and group always travel together.
`setowner` is how the `sd` profile changes ownership: setting the owner to oneself, to a group
one holds, to another user, to `ghost`, as admin and as not, must fail or succeed the same way
on both servers.

A **SACL** (system ACL) is the second list a descriptor may carry, beside the DACL. Its
entries grant nothing: they say which accesses, by whom, are to be **audited** — logged when
they succeed or fail — and Windows also keeps the file's mandatory integrity label there.
Reading or writing it needs the system-security right, which only a caller with the security
privilege gets. Both are tested: `getacl` and `setacl` of part `ntsd_full` are drawn for every
user, so admin must succeed and alice, bob and carol must be refused alike, and the SACL a set
carries comes from §8.4. The client writes owner, group, DACL and SACL together for an
`ntsd_full` set, so a set whose descriptor has no SACL clears it.

The privilege is granted the same way on both servers: on the reference with
`net sam rights grant "DITTOFS\Domain Admins" SeSecurityPrivilege`, on DittoFS through the
admin flag [RFC 19 §3.5](../../docs/internals/rfcs/rfc-19-authorization.md#3.5%20Privilege%20and%20the%20system-security%20right)
gives Domain Admins. DittoFS today grants the right to no caller, so the first rounds that
reach a SACL will diverge for admin; that is a finding against RFC 19, not a known divergence.

The SACL is only stored and read back: no audit event is compared, because Samba writes none
and DittoFS's audit stream is [RFC 25](../../docs/internals/rfcs/rfc-25-observability.md)'s.

### 7.7 Default weights

| Group | Weight | Operations within it |
|---|---|---|
| open/close | 18 | `open` 3, `close` 1 |
| data | 20 | `pread` 5, `pwrite` 6, `write` 2, `ftruncate` 2, `truncate` 1, `fallocate` 3, `lseek` 1, `fsync` 0.5, `fdatasync` 0.5, `copy_file_range` 0.7, `ficlone_range` 0.3, `setsparse` 0.5 (admin only; another user's draw is redrawn as admin) |
| namespace | 14 | `mkdir` 3, `rmdir` 1, `unlink` 2, `rename` 3, `link` 1, `readdir` 2 |
| attributes | 12 | `stat` 4, `lstat` 1, `fstat` 2, `utimensat` 1, `getdosattr` 1.5, `setdosattr` 2, `getbtime` 0.5, `setbtime` 0.5, xattr ops 0.5 each, `statfs` 0.5 |
| locks | 14 | `lock` 5, `unlock` 3, `testlock` 1, `flock` 1 |
| security descriptors | 22 | `getacl` 4, `setacl` 3, `setowner` 1.5, `aceadd` 2, `acedel` 1.5, `aceedit` 1.5 |

In the `posix` profile `chmod` and `chown` take 2 and 1 within the attribute group.

## 8. The ACL generator

### 8.1 What an ACE is drawn from

- **trustee:** §6.2, weights: the four users 10 each, the three domain groups 6 each, `WD`
  8, `AU` 4, `CO` 5, `CG` 3, `OW` 4, `DU` and `DA` 3 each, `BA` and `BU` 2 each, `SY` 1,
  `ghost` 2, `unix-1000` 1;
- **type:** `ACCESS_ALLOWED` 60, `ACCESS_DENIED` 40;
- **mask:** a non-empty subset of the 14 rights of §8.2, each bit with p = 0.35, plus, with
  p = 0.1, one of `GENERIC_ALL`, `GENERIC_READ`, `GENERIC_WRITE`, `GENERIC_EXECUTE` (servers map
  or keep generic bits differently on inheritable entries), plus, with p = 0.03, a bit outside
  the defined set (`0x00000200`), which a server either strips, keeps or refuses;
- **flags:** each of `OBJECT_INHERIT`, `CONTAINER_INHERIT` with p = 0.3, `NO_PROPAGATE_INHERIT`
  and `INHERIT_ONLY` with p = 0.15, `INHERITED` with p = 0.1. Meaningless combinations
  (`INHERIT_ONLY` without either inherit flag) are drawn anyway: what each server does with
  them is the test.

**Count of ACEs:** one descriptor in ten has 100–1000 entries, one in ten 6–100, the rest 1–6.
Within each band the count is drawn from a finite list: `1 2 3 4 5 6`, `8 12 16 32 64 100`,
`128 256 512 1000`. Plus two edge cases at p = 0.03 each: an **empty** DACL (no access for
anyone but the owner's implicit rights) and a **null** DACL (no DACL at all: full access for
everyone).

**Control flags:** `SE_DACL_PROTECTED` p = 0.3, `SE_DACL_AUTO_INHERITED` p = 0.3,
`SE_DACL_AUTO_INHERIT_REQ` p = 0.1.

**Owner and group** in an `ntsd`/`ntsd_full` set: absent (unchanged) p = 0.5, otherwise from
§6.2.

Entries are **not** put in canonical order (denies before allows, explicit before inherited).
Both servers receive the same bytes, and the comparison shows whether either reorders.

### 8.2 Rights

| Right | Mask | On a directory |
|---|---|---|
| `FILE_READ_DATA` | `0x00000001` | `FILE_LIST_DIRECTORY` |
| `FILE_WRITE_DATA` | `0x00000002` | `FILE_ADD_FILE` |
| `FILE_APPEND_DATA` | `0x00000004` | `FILE_ADD_SUBDIRECTORY` |
| `FILE_READ_EA` | `0x00000008` | |
| `FILE_WRITE_EA` | `0x00000010` | |
| `FILE_EXECUTE` | `0x00000020` | `FILE_TRAVERSE` |
| `FILE_DELETE_CHILD` | `0x00000040` | |
| `FILE_READ_ATTRIBUTES` | `0x00000080` | |
| `FILE_WRITE_ATTRIBUTES` | `0x00000100` | |
| `DELETE` | `0x00010000` | |
| `READ_CONTROL` | `0x00020000` | |
| `WRITE_DAC` | `0x00040000` | |
| `WRITE_OWNER` | `0x00080000` | |
| `SYNCHRONIZE` | `0x00100000` | |

| ACE flag | Value |
|---|---|
| `OBJECT_INHERIT_ACE` | `0x01` |
| `CONTAINER_INHERIT_ACE` | `0x02` |
| `NO_PROPAGATE_INHERIT_ACE` | `0x04` |
| `INHERIT_ONLY_ACE` | `0x08` |
| `INHERITED_ACE` | `0x10` |
| `SUCCESSFUL_ACCESS_ACE_FLAG`, `FAILED_ACCESS_ACE_FLAG` | `0x40`, `0x80` — SACL entries only (§8.4) |

### 8.3 How a descriptor is written in a plan

SDDL, with trustees by their §6.2 names instead of SIDs (`(A;OICI;FR;;;alice)`), and masks as
SDDL's right letters where one exists and hex otherwise. The plan stays readable and does not
depend on the domain SID, which changes every time the fixture provisions a new domain. A SACL
is SDDL's `S:` part (`S:AI(AU;SAFA;FW;;;bob)(ML;;NW;;;ME)`).

### 8.4 The SACL

Drawn for every `setacl` of part `ntsd_full`; absent (cleared) with p = 0.3.

- **entries:** a system-audit entry (`SYSTEM_AUDIT_ACE`, type `0x02`) with weight 85, a
  mandatory-label entry (`SYSTEM_MANDATORY_LABEL_ACE`, type `0x11`) with weight 15. A second
  label in one SACL is drawn anyway, like every meaningless combination.
- **an audit entry** takes its trustee, mask and inheritance flags as a DACL entry does
  (§8.1), and each of `SUCCESSFUL_ACCESS` and `FAILED_ACCESS` with p = 0.6, so an audit entry
  that audits nothing is reachable too.
- **a label entry** names one of Low `S-1-16-4096`, Medium `S-1-16-8192`, High `S-1-16-12288`
  or System `S-1-16-16384`, and a mask that is a subset of `NO_WRITE_UP` `0x1`, `NO_READ_UP`
  `0x2`, `NO_EXECUTE_UP` `0x4`. Neither server enforces integrity levels; whether each stores
  the label, refuses it or drops it is the test.
- **count:** from `0 1 2 3 4 8 16` (0 is an empty SACL, distinct from none).
- **control flags:** `SE_SACL_PROTECTED` p = 0.3, `SE_SACL_AUTO_INHERITED` p = 0.3.

## 9. Comparison

### 9.1 Lockstep, and the first divergence ends the round

Each step runs on `ref`, then on `dut`, and is compared before the next step starts. The
first new divergence ends the round: after it the two trees differ, and every later
comparison would report the difference again in another form.

**Every descriptor write is read back.** After a `setacl`, `setowner` or ACE edit that
succeeds on both sides, admin reads the descriptor at once — owner, group and DACL, plus the
SACL when the write was of the full descriptor — and it is compared entry by entry in order
(§9.3). A server that reorders, merges, canonicalises or drops entries diverges at the write
that did it, signed `<op>:readback:<part>` (`aceadd:readback:dacl`), and the step shows in a
trace as `NNR`.

A **known** divergence (§12.2) does not end the round. It is recorded and the round goes on,
because a known difference that leaves the trees alike — a descriptor reported with another
owner name — would otherwise hide every step after it. A round that met only known
divergences is graded "known", not passed.

A round with no divergence ends with a **sweep**: the admin worker walks the round directory
on both sides — `readdir`, `stat`, DOS attributes, the content of every file (§9.3) and its
`ntsd` — and compares them as if they were steps. The sweep catches state no step happened to
observe, including whatever a rename moved out of the pool. It looks at the wide directory's
fillers only through the directory's listing. It is not one of the 12 steps, and a divergence in it is reported as a step numbered
`S`.

### 9.2 Errors

The errno of every step **MUST** be equal. No two errnos are treated as equivalent: an
equivalence would hide exactly the difference between "denied" and "not found" a client
script relies on. A divergence that is acceptable is a known divergence (§12.2), with a
reason, and names its signature exactly.

What the client returns is its translation of an NT status; two different statuses can map
to one errno and be invisible here (§14).

### 9.3 Outputs

| Output | Compared as |
|---|---|
| bytes read | the count, and the SHA-256 of the bytes; on a mismatch the first differing offset and the two bytes there are reported |
| bytes written, copied, resulting offset | exact |
| listings | the set of (name, type), sorted bytewise; order is the server's and never compared |
| xattr values and lists | exact |
| DOS attributes | exact, every bit of §6.9 |
| security descriptor | parsed, then compared field by field: owner, group, DACL present/null, each ACE in order (type, flags, mask, trustee), SACL likewise; control flags `SE_DACL_PROTECTED`, `SE_DACL_AUTO_INHERITED`, `SE_SACL_PROTECTED`, `SE_SACL_AUTO_INHERITED` only (`SE_SELF_RELATIVE` and the "defaulted" flags are not compared) |
| lock query | type, start, length |

**Trustees in descriptors are normalised to §6.2's names**, through the domain SID the fixture
learnt. A SID that is neither a domain SID nor a well-known one — a server's own local SID,
a `S-1-22-*` uid SID that is not `unix-1000` — is normalised to `local:<class>-<rid>`, so a
server's local accounts compare by role, not by machine.

### 9.4 Attributes

| Field | Compared |
|---|---|
| file type | exact |
| size | exact |
| nlink | exact |
| inode | never as a number, which is each server's own; as a relation only: two paths of the round that have equal inode numbers on one side must have equal ones on the other. A removed name is forgotten with every name below it, so a number one server reuses is never related to a name gone |
| uid, gid, permission bits | `posix` profile only, exact |
| mtime, btime | the first look at a time compares the two values, which may be up to 1 s apart (each step runs on one server and then the other); every later look is **relative**: per path, each side remembers the last value it observed, and the comparison is "unchanged", "advanced" or "went back". A time the plan sets — `utimensat`, `setbtime`, the tree's setup — becomes the remembered value on both sides, so "unchanged" after it means both kept exactly what was set, truncated to 100 ns. A rename carries what is remembered of the source, and of every name below it, to the target: a later look at a moved object compares against what it was before the move |
| atime | not compared (the reference's mount options decide when it moves) |
| ctime | not compared: Samba reports a file's ChangeTime as its modification time, DittoFS as the time of its last change, metadata included, as Windows does, so every ACL, attribute or rename step would diverge on semantics alone |
| blocks, `SEEK_DATA`, `SEEK_HOLE` | **allocation-dependent**: compared exactly; when they differ, the comparator checks each side's answer against the logical content both sides agree on (a reported hole must read as zeros, a reported data offset must not be past the size). If both are admissible, the difference is recorded as a note, not a divergence |

The allocation rule exists because ext4 allocates in 4 KiB blocks and DittoFS in its own
units, so the same file may honestly report holes at different offsets. A note that is never a
divergence is still in the report, so a pattern in notes is visible.

### 9.5 Crashes and hangs

A step that times out on one side and not the other, a worker killed by a signal, or a mount
that returns `EIO`/`EHOSTDOWN` on one side is a divergence with signature
`<op>:timeout:<side>` or `<op>:errno:…`. After any of them the fixture checks the DittoFS
process: if it exited, its exit status and the tail of its log are attached to the report, and
the fixture restarts it before the next round.

## 10. Rounds, isolation and cleanup

- Each round runs in a new directory named `r<N>-a<attempt>` at the share's root, created by
  admin on both sides, so a replay (§11) never sees a previous attempt's leftovers.
- The round directory's descriptor is set by admin before step 1, from a short list of round
  roots the generator draws first (§5.2): `open` (Everyone full control, inheritable),
  `owner-only` (CREATOR OWNER full, inheritable; Domain Users add-file and traverse),
  `protected` (alice full, protected), `empty` (no entry but the one below). Every root but
  `open` also grants Domain Admins full control, inheritable, so cleanup (below) does not
  have to take ownership of every round. Without it, each
  server's default descriptor for a new directory would decide every inheritance test, and the
  two defaults differ by design.
- Then admin builds the plan's tree (§6.3) on both sides, one object at a time, the reference
  first: each directory, each file with its content, the wide directory's fillers in steps of
  100. Then it sets each object's access and modification time to `1500000000.1234567 + 1000·k`
  seconds and its creation time to `1400000000 + 1000·k`, the `k`-th object first from the
  deepest, so no later creation moves a time already set. These are steps like the plan's,
  numbered `T` in a trace, and a divergence among them ends the round as any other does. The
  objects inherit the round root's descriptor.
- **After the round**, its directory is removed:
  - on the reference, out of band: `rm -rf` of the directory on the server's ext4, which no
    ACL under test can block;
  - on DittoFS, over SMB as admin: every entry is removed, deepest first; where a removal is
    refused, admin sets itself as owner and sets a DACL granting it full control, then removes
    it again; then the round directory goes, and the **share root's owner and descriptor are
    reset** to the fixture's.
  - If anything is left on DittoFS after that, the share is removed and created again at
    once.
- **Every 100 rounds** the DittoFS share is removed and created again regardless, so state that
  survives deletion (a leaked handle, a lock nobody can see, a stale entry) cannot accumulate
  across a long run.
- Rounds run one at a time per fixture. Parallelism is several fixtures, never two rounds on
  one share, because a lock or a share-mode conflict between rounds would make a round depend
  on its neighbour.

## 11. Minimisation

### 11.1 Replay before shrinking

A failing round is replayed twice more, each in a fresh directory. If all three runs diverge
with the same signature, the failure is **stable** and is minimised. Otherwise it is
**unstable**: it is reported with the signatures each run produced, and not minimised,
because a shrinking step cannot tell "the removed step mattered" from "it did not fail this
time".

### 11.2 Removing steps

The minimiser removes one step at a time:

```
plan := failing plan; sig := its signature
repeat
    changed := false
    for i := last step down to first:
        cand := plan without step i
        if replay(cand) diverges with signature sig:
            plan := cand; changed := true
until not changed
```

The result is **1-minimal**: no single step can be removed and still produce the signature.
At 12 steps that is at most 78 replays. Going from the last step down keeps the steps a
removal would orphan (an `open` whose slot later steps use) alive longest, so fewer replays
are wasted.

Each candidate must reproduce the **same signature**, not merely some divergence: removing the
`open` of step 2 typically turns the failure into `pread:errno:EBADF/…` or into a different
real bug, and a minimiser that accepted that would converge on the easiest divergence to hit,
not on the one it was given. A candidate that diverges differently is discarded, and its
signature is recorded in the report as a lead.

### 11.3 Shrinking parameters

After step removal, each remaining parameter is tried at its domain's simplest value — offset
and length the smallest point that keeps the signature, flags and DOS attributes with one
removed at a time, an ACE removed at a time from each descriptor, masks with one bit removed at
a time, paths replaced by `f0`/`d0`. Same acceptance rule. A shrink that would orphan a
reference (a path renamed in one step but not in the step that uses it) is rejected by the
replay, not by special cases.

The tree shrinks after step removal and before the parameters: each object is removed with
what is below it, deepest first,
then the wide directory's `fill` goes to 0 and each file's length is tried at simpler sizes,
under the same rule.

### 11.4 What a report carries

- the original plan file and the minimised one, both runnable with `smbfuzz run --plan`;
- for the minimised plan, both sides' trace: every step's request, errno and output;
- the summary line: the round number, the generator version, the commit, the profile, the
  authentication and the client's kernel version
  (`round 4242, gen 1, commit 1a2b3c4, profile sd, auth ntlm, kernel 6.8.0`), then the
  signature;
- the leads of §11.2;
- for a crash or hang, the DittoFS log tail (§9.5).

## 12. Running it

### 12.1 The command

```
smbfuzz fixture up [--auth ntlm|krb5] [--profile sd|posix]   # §3, state in /var/tmp/smbfuzz
smbfuzz run   --from 1 --count 500 [--gen 1] [--minimise=false] [--stop]
smbfuzz run   --plan FILE              # one plan, e.g. a minimised one or a corpus entry
smbfuzz plan  --round 4242 [--gen 1]   # print, don't run
smbfuzz fixture down
```

`run` reads the authentication and profile back from the fixture, runs §3.6's probes, then
the rounds. It exits 0 when every round passed or diverged only as
known, 1 on a new divergence, 2 when it ran fewer rounds than asked (a fixture probe failed,
the fixture died and could not be restarted).

### 12.2 Known divergences and the corpus

- `test/smbfuzz/known.yaml` lists accepted divergences, one entry per signature, each with a
  reason and, unless the reason is "DittoFS differs by design" citing its rule, an issue.
  An entry excuses that signature only, wherever it occurs, per RFC 29's grading rule that a
  known-failure entry names one check, not a class of outcomes.
- `test/smbfuzz/corpus/*.plan` holds minimised plans. A plan enters the corpus when the bug it
  found is fixed, in the same change as the fix, and is replayed by every run from then on. A
  corpus plan that diverges is a regression, never a known divergence.

### 12.3 Under `dt`

`smb-fuzz` is a suite of [RFC 29](../../docs/internals/rfcs/rfc-29-test-harness.md)'s registry, with needs
`[linux, root, docker, smb-client]` and a fixture that brings up §3's DC, reference and DittoFS and
mounts both shares. It reports one CTRF test per round (`smb-fuzz/round-<N>`) and one per
corpus plan (`smb-fuzz/corpus/<file>`), graded by `dt` like every other suite; a divergence
attaches §11.4's report. Rounds the run was meant to run and did not are reported as not run
(RFC 29's R10), so a fixture that dies at round 40 of 300 is not green.

Which tiers run the suite, how many rounds each runs and from which round, is decided once the
suite exists and its cost per round is measured.

### 12.4 Opening an issue for a divergence

An issue opened for a divergence the fuzzer found carries:

- the report's **summary line** (§11.4): round, generator version, commit, profile,
  authentication, kernel;
- the **minimised plan** (`minimized.plan`), pasted whole.

The plan is the reproducer. It is self-contained — the tree, the round root and every step
with its arguments are in it — and runs with `smbfuzz run --plan` on any fixture with the
issue's profile and authentication, without regenerating anything. The round number and the
generator version name the plan the run drew (`smbfuzz plan --round N --gen G` prints the
original again); the commit fixes the comparator, the fixture and the DittoFS that gave the
verdict. When the bug is fixed, the same plan enters the corpus (§12.2).

## 13. Alternatives considered

**Shell commands instead of system calls.** A `bash` step per operation and `su` for users is
quick to write. Rejected: a shell command bundles several system calls with its own flags
(`echo >` is `O_WRONLY|O_CREAT|O_TRUNC`, always), so the flags under test are not parameters;
and `su` per step opens a new SMB session per step, which tests session setup 12 times per
plan and nothing else more often.

**A user-space SMB client (go-smb2) instead of the kernel's.** It would compare NT statuses
instead of errnos, and reach share modes, create dispositions, alternate data streams,
oplocks, leases and delete-on-close that the kernel client never sends, with no root and no
second machine. Rejected for generator version 1 because the requirement is system calls, and
because the kernel client is what Linux users actually run; kept as the obvious **second
driver**: §7's operations are defined so that a wire driver can implement them with the same
plans, and add its own SMB-only operations.

**Windows Server as the reference.** The real specification of Windows ACL semantics; where
Samba differs from Windows, a Windows reference would be right. Wanted, but no Windows server
is available to the project. When one is, it is a second `ref` that passes §3.6's probes, and
nothing else in this design changes.

**RFC 19's model as the oracle instead of a second server.** Exact where the model is
complete, but it evaluates DittoFS against DittoFS's own reading of the specification, so it
cannot catch a shared misunderstanding. Samba is an independent reading. RFC 19's model tests
remain where they are.

**A seed per campaign, beside the round number.** It would let two runs explore different
plans from the same round numbers. Rejected: "test N" would then need a second number to mean
anything, and the round number alone already gives unlimited fresh plans by running further.

**Random values instead of finite domains.** Rejected by requirement R9, and for a reason
beyond file size: a divergence found with offset 1 048 579 is one nobody recognises as
"1 MiB plus three", and minimisation cannot shrink a value it has no order on.

## 14. Risks

- **The client hides differences.** errno is a lossy image of the NT status; the client also
  answers some calls itself, retries others (a sharing violation on `unlink` becomes a rename
  and a delete-on-close), and its behaviour changes with the kernel. Mitigation: every report
  carries the kernel version; a wire driver (§13) is the eventual cure.
- **Samba is wrong.** Every Samba bug becomes a known divergence to triage. Mitigation: the
  known list carries reasons, so a "Samba is wrong" entry has to say why; repeated Samba bugs
  in one area are reported upstream.
- **Allocation noise.** §9.4's admissibility rule exists for blocks and holes; if notes turn
  out to hide real bugs, the rule narrows.
- **Fixture cost.** Four parts, a second machine, a domain join: the suite is the most
  expensive in the registry to bring up. Mitigation: one fixture runs thousands of rounds, and
  rounds are cheap once it is up.
- **Boundaries move.** DittoFS's chunk and block sizes are still settling (the code and RFC 2
  disagree today, which is why both are points). Each change is a new generator version, so
  old reports stay reproducible, but plans of the old version stop testing the new
  boundaries.

## 15. Plan

Each step ships on its own and is useful alone.

1. **Fixture.** DC with carol, fuzzers, per-user keytabs and the servers' DNS names; Samba
   member reference with §3.2's configuration on a loop ext4 and the security privilege
   granted; DittoFS joined; the client host's mounts and credentials under NTLM and Kerberos;
   §3.6's probes.
2. **Workers and the core operations.** The worker protocol, handle slots, `open`, `close`,
   `pread`, `pwrite`, `stat`, `fstat`, `mkdir`, `rmdir`, `unlink`, `readdir`; lockstep
   execution and comparison; the plan file format and `smbfuzz run --plan`. Hand-written plans
   only: this step is useful as a differential test runner before any fuzzing.
3. **Generator version 1.** Round-number seeding (§5.1), domains (§6), shape and bias (§5.2,
   §5.4), `smbfuzz plan`.
4. **Security descriptors.** The descriptor codec and normaliser, `getacl`, `setacl`,
   `setowner`, the ACL and SACL generators (§8), round roots and cleanup (§10).
5. **Locks and DOS attributes.** §7.5 and §7.4's DOS and creation-time operations, with the
   overlap bias.
6. **The rest of §7.** Truncate, fallocate, lseek, copy, clone, rename, link, xattrs, times;
   the `posix` profile with `chmod` and `chown`.
7. **Minimisation.** Replay, step removal, parameter shrinking, reports.
8. **Harness.** The `smb-fuzz` suite, `known.yaml`, the corpus, CTRF results. Tiers come after.

Generator version 1 is released at the end of step 6, once every operation it draws exists;
until then the generator is unreleased and may change freely.

## 16. Open questions

1. **The cost of a round** with the larger boundaries of §6.4, measured before deciding the
   rounds per run (§12.3).
2. **Which combinations of profile and authentication** a scheduled run covers, once tiers are
   decided (§12.3); every one of the four is runnable by hand.
