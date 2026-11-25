# Epic 8: control plane v2 — E2E green

**Sources:** [RFC 23](../docs/internals/rfcs/rfc-23-control-plane.md) §12 (Conformance): the rows
that mount a share ("§3.2 create admits nobody", "§5.1 roles grant nothing", "§3.5 grants"' admission
half), "§2.2 order"'s adapter clause, "§7 CLI"'s second-user form, and the suites paragraph; §3.2's
create, §3.5's grants, §5.1, §7 and §10's CP9 and CP14 for what those rows hold; RFC 23's plan §1.5
rule 8, D9 and §5 epic 8; epic 7's D4 (the second-user form handed here).
**Track:** the team.
**Epic 8 estimate: 8 subtasks, 15.5 man-days.**
**Depends on:** E4 (the client, `pkg/node/apiv1/client`), E7 (the server fixture, its T7.1, landed;
the CLI's child-safe rows, its T7.4) for T8.1 to T8.4, which can start now; S3 for T8.5 to T8.8.
S3 has not been reached (§9): RFC 20's plan's epic 7 starts the adapters inside the node, but the
NFS and SMB handlers on the filesystem service, RFC 21's and RFC 22's plans, are at their first two
epics, so no client can mount yet. Outside RFC 23's plan as well: X3 for the settings the fixture
seeds; the RFC 18 plan's epic 5 (Kerberos and the PAC, closed) and epic 9 (the e2e Kerberos
helper mended) for T8.7; the RFC 13 plan's epic 6 for `dt stack` in T8.4; RFC 8's second part for
the `badger-s3` profile T8.8's suites run on.
**Blocks:** nothing in RFC 23's plan. RFC 21's plan's epic 16, RFC 22's plan's epic 16 and the RFC 18
plan's epic 9 write their e2e tests on T8.1's helpers.

---

## 1. Goal

The rows only a real mount shows: a share admits nobody until granted, an API role grants nothing
on files, a `none` grant blocks while a group's grant admits once it is gone, and the CLI works as
an OS user who cannot read the host. Around them, the e2e driver epic 1 deleted is rebuilt on the
new client, and the cells the `feat/storage-v2` filter switched off come back on as the data path
they need exists.

**At the end of the epic:**

- `test/e2e` drives the server through epic 7's fixture and the API through epic 4's client, with
  helpers the protocol plans' e2e epics build on;
- `§7 CLI` passes as a second OS user, and the `e2e` cell runs on `feat/storage-v2`;
- the three mount rows pass in `e2e` over NFS and SMB, and their Kerberos flavours in
  `e2e/kerberos`;
- the setup scripts that drove v1's CLI drive the new one;
- the filter entries this epic owns are gone, and the cells they skipped run against the new
  server.

### 1.1 In scope

The e2e helpers; `§7 CLI`'s second-user form; the mount rows of RFC 23 §12 over NFS, SMB and
Kerberos; `test/suites.yaml`'s e2e paths and the planner's test; the setup scripts D9 names that
still exist; the filter entries D2 gives this epic.

### 1.2 Out of scope

- v1's deleted e2e tests as such (D1): the CLI-only ones are covered at `unit` by E6's `clisuite`
  and E7's T7.4; the protocol ones go to RFC 21's plan's epic 16, RFC 22's plan's epic 16 and the
  RFC 18 plan's epic 9, a row of
  [the deferral ledger](rfc-23-control-plane-plan.md#61-deferral-ledger) this document's pull
  request adds.
- The filter entries of the protocol suites (D2): `pynfs`, `cthon04` and `nfstest` are RFC 21's
  plan's epic 15; `smbtorture`, `wpts` and `clients/windows` RFC 22's plan's epic 15; `system` RFC
  22's plan's T16.5; `e2e/cross-protocol` whichever protocol plan lands its first cross-protocol
  test. `unit/windows` and `integration/kerberos` go with the filter itself at cutover (plan D9).
- The SMB-conformance shares file's per-share options (`encrypt-data`, `access-based-enumeration`,
  …) applied as share settings: they bootstrap `smbtorture` and `wpts`, RFC 22's plan's epic 15,
  and their keys wait on X3; a ledger row the same pull request adds.
- The scenarios' commands with no v2 command (`system drain-uploads`, `share snapshot`, `trash`,
  `stats`): the syncer's and the snapshot resource's rows of
  [the deferral ledger](rfc-23-control-plane-plan.md#61-deferral-ledger), and a row for `trash`
  and `stats` the same pull request adds.
- `docker compose up` on a v2 node, which `dt stack` needs: the ledger's row, the RFC 13 plan's
  epic 6.
- The multi-node rows and CP15: the ledger's cluster row.

### 1.3 Names used here

- **E1**, epic 1: v1 deleted, among it `test/e2e`'s helpers and the tests on them.
- **E3**, epic 3, the node: `node.New`, start and stop, `dfs start`.
- **E4**, epic 4, the management API, and its client `pkg/node/apiv1/client`.
- **E5**, epic 5, accounts and sign-in: roles, the bootstrap administrator, `dfs init`.
- **E6**, epic 6, the CLI: `dfsctl` and its shared table `clisuite`.
- **E7**, epic 7, the integration epic: the server fixture (T7.1), the CLI from a child (T7.4).
- **E8**, this epic.
- **S1**, epic 1 merged: v1 gone and the node's surface frozen.
- **S2**, the node starts and reconciles (E3).
- **S3**, adapters serve through the node, outside RFC 23's plan: RFC 20's plan's epic 7 and RFC
  21's and RFC 22's handlers on the filesystem service. Not the S3 storage service.
- **S7**, in RFC 21's and RFC 22's plans, their sync point "the node serves NFS" (or SMB): the part
  of S3 each protocol brings.
- **X3**, the RFC 13 plan's epic 5, which wires `Check`, `Diff` and `Get`.
- **CP3, CP9, CP14**, RFC 23 §10's invariants of those numbers.
- **plan D9**, RFC 23's plan's decision on the `feat/storage-v2` filter (#3714, `tests.yml`'s `V2`)
  and who turns each cell back on.

## 2. How the general rules apply

The rules are those of
[RFC 23's plan, §1.5](rfc-23-control-plane-plan.md#15-general-rules-for-each-epic), cited by number
and title and not copied.

| Rule | Subtasks | How |
| --- | --- | --- |
| rule 1, independent unit tests ([`CLAUDE.md`, "Writing tests"](../CLAUDE.md#writing-tests)) | T8.1 | the helpers run against a real server started by E7's fixture; no test double: a mount is a kernel client, the server a `dfs start` child over `t.TempDir()` |
| rule 2, fuzz every decoder of untrusted bytes ([`CLAUDE.md`, "Writing tests"](../CLAUDE.md#writing-tests)) | none | this epic adds no decoder |
| rule 3, observability follows RFC 25 | none | no series and no condition; the rows read the API's answers and the client's mount result |
| rule 4, profiling | none | the server is a child process; E7's fixture checks it exits 0 at stop, and the in-process leak check has nothing to watch |
| rule 5, public interface and internals ([`CLAUDE.md`, "Verifying a change"](../CLAUDE.md#verifying-a-change)) | T8.2, T8.5–T8.7 | each row asserts at the consumer: the kernel client's mount and the CLI process's exit, never a record |
| rule 6, known failures shrink ([`CLAUDE.md`, "Writing tests"](../CLAUDE.md#writing-tests)) | T8.2, T8.5–T8.8 | `e2e` keeps no `known.yaml`, so each e2e row lands green or not at all (D1); `pjdfstest`'s `test/posix/known.yaml` is rewritten for v2 in T8.8, each entry naming its owner's issue |
| rule 7, v2 ignores v1 | T8.1, T8.4 | the helpers are written against epic 4's client, not lifted from E1's deleted package; the scripts call the new CLI's nouns |
| rule 8, real dependencies for properties of those dependencies ([`CLAUDE.md`, "Writing tests"](../CLAUDE.md#writing-tests)) | T8.5–T8.7 | every refusal and admission is a Linux kernel client's mount (`mount -t nfs`, `mount -t cifs`), Kerberos against a real KDC |
| rule 9, code comment conventions ([`CLAUDE.md`, "Code comments"](../CLAUDE.md#code-comments)) | T8.2, T8.5 | `tests.yml`'s filter keeps its `decision:` marker, narrowed to the cells still skipped (D2); D6's choice of where an NFS refusal shows carries a `decision:` at the row |
| rule 10, a test must be able to fail ([`CLAUDE.md`, "Writing tests"](../CLAUDE.md#writing-tests)) | all | each row is broken once on purpose (a grant written at create, a role read as a grant, the CLI reading the bootstrap) and fails on its assertion |
| rule 11, a symbol is exported only if RFC 23's interface uses it | T8.1 | the helpers' exports are `test/e2e`'s own surface; nothing under `internal/node` or `pkg/node` gains one |
| rule 12, each subtask creates only what it declares | T8.1 | the helpers land with their first consumer, T8.2's row; each helper a later subtask needs lands with that subtask |

## 3. Requirements and invariants

Every obligation RFC 23 and its plan place on this epic. A `§12` row is written as RFC 23 names
it, in the part this epic owns.

**The driver and the suites**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R1 | The e2e helpers are rebuilt on epic 4's client, and the server they drive is started through epic 7's fixture on `dfs init` and `dfs start` | MUST | RFC 23 plan §5 epic 8, §3.3 (E7's row) | T8.1 |
| R2 | The rows that mount shares over NFS and SMB run in `e2e`, their Kerberos flavours in `e2e/kerberos` | MUST | RFC 23 §12 (Suites) | T8.5, T8.6, T8.7 |
| R3 | A mount refusal is checked with a kernel client, at the e2e tier | MUST | RFC 23 plan §1.5 rule 8 | T8.5, T8.6, T8.7 |
| R4 | `test/suites.yaml`'s `paths:` for `e2e/kerberos` and `e2e/cross-protocol`, and `test/harness/plan/plan_test.go`'s list, name the v2 packages that replaced the trees epic 1 deleted, so a change to them plans those cells | MUST | RFC 23 plan §5 epic 8 | T8.3 |

**The CLI**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R5 | `§7 CLI`, its second-user form: the CLI's suite run as an OS user to whom the host's bootstrap and the store are unreadable passes, every command working; the CLI never opens the metadata store, a journal or a remote store, nor reads the bootstrap | test row | RFC 23 §12, §7, §10 (CP14); epic 7's D4 | T8.2 |

**Admission**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R6 | `§3.2 create admits nobody`: a share created through the API, alice's mount over NFS and over SMB is refused; once alice is granted, her mount is admitted. A create writes no grant, and a new share is reachable by nobody until an operator grants it | test row | RFC 23 §12, §3.2 | T8.5, T8.6, T8.7 |
| R7 | `§3.5 grants`, its admission half: `Finance` granted read and alice, a member, granted `none`, alice's mount is refused; alice's grant deleted, she is admitted read through `Finance`. A `none` grant is stored and blocks the principal, and a deleted grant lets the principal fall through to its groups' grants | test row | RFC 23 §12, §3.5 | T8.5, T8.6 |
| R8 | `§5.1 roles grant nothing`: with the bootstrap administrator and an account of every role, every mount of every share over every flavour is refused, and the administrator's record holds no uid, gid, SID, Kerberos name or grant. No role makes an account privileged or granted on any share; file access comes only from share grants | test row | RFC 23 §12, §5.1, §10 (CP9) | T8.5, T8.6, T8.7 |

**Start order**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R9 | `§2.2 order`, its adapter clause: no adapter listened before the first reconcile completed | test row | RFC 23 §12, §2.2 (step 9) | — : held by `TestAnAdapterListensOnlyFromTheAdaptersStep` in `internal/node`, which RFC 20's plan's epic 7 landed, in `unit` (D4) |

**The cells back on**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R10 | The setup that drives v1's `dfsctl` and configuration is rebuilt on the new CLI: `test/harness/env.sh`'s `stack_dev_user` and `test/scenarios`. Of the rest D9 names, `test/posix/setup-posix.sh` no longer exists (`pjdfstest` starts its server through epic 7's fixture), and the SMB-conformance bootstrap is RFC 22's plan's epic 15's (§1.2) | MUST | RFC 23 plan D9, §5 epic 8 | T8.4 |
| R11 | The filter's entries this epic owns are removed on `feat/storage-v2`, each cell running against the new server: `e2e`, `e2e/kerberos`, `pjdfstest`, `fio-verify`, `bench-smoke` and `clients` but `clients/windows` | MUST | RFC 23 plan D9, §5 epic 8; D2 | T8.2, T8.7, T8.8 |

## 4. Decisions this epic takes

- **D1, the deleted tests are not brought back as such.** E1 deleted 52 e2e tests, about 24,000
  lines. `e2e` keeps no `known.yaml`, so a test lands only green, and most of them check a
  protocol's own behaviour (sessions, delegations, locks, leases, Kerberos, squash, NLM, the port
  mapper), which needs RFC 21's and RFC 22's handlers to pass. So:
  - the tests that only drive the CLI (users, groups, shares, stores, adapters, contexts, the
    server's lifecycle) are not brought back: E6's `clisuite` and E7's T7.4 check the same at
    `unit`, as `CLAUDE.md`'s E2E paragraph wants;
  - the tests whose check an RFC 23 §12 mount row states (`permission_enforcement_test.go`'s
    read-only and no-access cases) are replaced by that row;
  - the protocol tests go to the plan that owns the protocol: RFC 21's plan's epic 16 for NFS, RFC
    22's plan's epic 16 for SMB and the cross-protocol ones, the RFC 18 plan's epic 9 for squash and
    the Kerberos identity rows, each written on T8.1's helpers. A ledger row records it.

  This replaces the plan's "the tests epic 1 deleted brought back from history", which this
  document's pull request amends. Prior art: Kubernetes' e2e tests are owned per area, each area's
  team keeping its own (from memory). No marker.
- **D2, each filter entry leaves with the plan whose cell it skips.** The filter skips by suite,
  and RFC 21's and RFC 22's plans (their epics 15, and RFC 22's T16.5) already claim `pynfs`,
  `cthon04`, `nfstest`, `smbtorture`, `wpts`, `clients/windows` and `system`. This epic keeps what
  no protocol plan claims: `e2e`, its `kerberos` option, `pjdfstest`, `fio-verify`, `bench-smoke`
  and `clients`' other cells. A cell whose run finds no test is graded "not run", so `e2e`'s
  `kerberos` and `cross-protocol` options stay skipped by name until their first test lands
  (T8.2), and `clients` is skipped by name while `clients/windows` waits. Plan D9 is amended to
  this list in the same pull request. The filter's `decision:` marker stays, naming the cells still
  skipped. Prior art: none found.
- **D3, the helpers start the server through epic 7's fixture.** `fixture.Start` already runs `dfs
  init` and `dfs start`, reads the API address from the log, waits on `/health/ready`, signs in and
  stops by signal. A second launcher in `test/e2e` would drift from it. The helpers add only what a
  row needs beyond the fixture's users and shares: accounts of each role with a password, groups and
  members, shares created with no grant, grants put and deleted, and a mount wrapper over
  `test/e2e/framework`. No marker.
- **D4, `§2.2 order`'s adapter clause stays in `unit`.** RFC 23 §12's suites paragraph runs in `e2e`
  only the rows that mount, and the clause is held in-process by
  `TestAnAdapterListensOnlyFromTheAdaptersStep`, which dials each adapter's port after every start
  step. An e2e copy would only repeat it slower. No marker.
- **D5, the second OS user is `nobody`.** `e2e` runs as root (`dt` runs it so), so the row starts the
  `dfsctl` child with `SysProcAttr.Credential` set to uid and gid 65534, its `HOME` and `XDG_*`
  directories its own, the server's data directory and bootstrap left `0700` root. No user is
  created on the host. Prior art: Go's `os/exec` tests run children under another credential the
  same way (from memory). No marker.
- **D6, each account presents the flavours it can.** `§5.1`'s "every flavour" is, per account, what
  its record lets it present. Each role account is created with a uid, a gid and a password and no
  grant, so a refusal can come only from admission, never from a missing spelling: it mounts over
  `AUTH_SYS` with its uid, over SMB with NTLM, and, in `e2e/kerberos`, with its Kerberos name. The
  bootstrap administrator has no spelling, so it presents only SMB with NTLM by its name and
  password; that its record holds none is read back through the API. Where an NFS refusal shows,
  at the mount or at the first access to the share, is RFC 21's; the row asserts that no file of the
  share is reachable, with a `decision:` marker naming RFC 21's choice once it is made (§9). No other
  marker.

## 5. Target tree

```text
test/e2e/helpers/                         T8.1  server through fixture.Start, signed-in client,
  server.go accounts.go shares.go               accounts, groups, members, shares, grants, mounts
  mount.go
test/e2e/main_test.go                     T8.1  one server per package run, stopped at exit
test/e2e/cli_test.go                      T8.2  §7 CLI as uid 65534
.github/workflows/tests.yml               T8.2, T8.7, T8.8  the filter's entries removed (D2)
test/suites.yaml                          T8.3  e2e paths on the v2 packages
test/harness/plan/plan_test.go            T8.3
test/harness/env.sh                       T8.4  stack_dev_user on the new CLI
test/scenarios/setup.sh, lib/tools/       T8.4
test/scenarios/*.sh                       T8.4  calls with a v2 command rewritten
test/e2e/admission_nfs_test.go            T8.5  §3.2, §3.5, §5.1 over NFS
test/e2e/admission_smb_test.go            T8.6  §3.2, §3.5, §5.1 over SMB
test/e2e/admission_kerberos_test.go       T8.7  §3.2, §5.1 Kerberos flavours
test/posix/known.yaml                     T8.8  rewritten for v2
```

## 6. Subtasks

### T8.1 The e2e helpers on epic 4's client

**Subtask T8.1 estimate: 2.5 man-days.**

- **Track:** the team. **Depends on:** none. E4's client and E7's T7.1 have landed.
- **Covers:** R1.
- **Deliverables:** `test/e2e/helpers` (D3): `Start(t)` over `fixture.Start`, returning the server's
  addresses and a client signed in as the administrator; `Account(t, role, uid, gid)` creating an
  account with a password and no grant; `Group`, `Member`; `Share(t, name)` creating a share whose
  export policy states its flavours and which holds no grant; `Grant(t, share, principal, access)`
  through `Put` with `If-None-Match: *` and `Ungrant` through `Delete`; `MountNFS` and `MountSMB`
  over `test/e2e/framework`'s mounts, returning the client's error rather than failing; each
  helper's cleanup on `t.Cleanup`. `main_test.go` starts one server per package run and stops it at
  exit. Consumed by T8.2 (start, client), T8.5 to T8.7 (the rest), and by RFC 21's and RFC 22's
  plans' epics 16 and the RFC 18 plan's epic 9. Each helper beyond `Start` lands with the first
  subtask that calls it (rule 12).
- **Acceptance criteria:**
  - `go vet -tags=e2e ./test/e2e/...` passes, and nothing under `test/e2e` imports `internal/node`.
  - `dt run e2e` on a Linux host with root starts the server once and stops it with exit 0.
- **Tests:** T8.2's row is the helpers' first consumer and their test; no test of the helpers
  alone.

### T8.2 `§7 CLI` as a second OS user, and the `e2e` cell back on

**Subtask T8.2 estimate: 1 man-days.**

- **Track:** the team. **Depends on:** T8.1; E7's T7.4 (the child-safe rows run from a `dfsctl`
  child).
- **Covers:** R5, R11 (`e2e`).
- **Deliverables:** `cli_test.go`: a `dfsctl` built from the module, run as uid and gid 65534 (D5)
  through E7's runner of E6's child-safe `clisuite` rows, against T8.1's server; the server's data
  directory and bootstrap checked unreadable to that user first. `tests.yml`'s filter: `e2e` out of
  the suite list, `e2e/kerberos` and `e2e/cross-protocol` skipped by name (D2), the `decision:`
  marker narrowed to them.
- **Acceptance criteria:**
  - Every child-safe row passes as uid 65534, and the `e2e` cell is green on `feat/storage-v2`.
- **Tests:** the row, run once with the bootstrap made readable and a command changed to read it
  (catches a command that bypasses the API, R5).

### T8.3 The e2e cells' paths on the v2 packages

**Subtask T8.3 estimate: 0.5 man-days.**

- **Track:** the team. **Depends on:** none.
- **Covers:** R4.
- **Deliverables:** `test/suites.yaml`: `e2e`'s `kerberos` option gains `internal/node/**`,
  `pkg/node/**` (the sign-in, the keytab and the share's export policy now reach the mount through
  them) and the restored Kerberos test files' glob; `cross-protocol` gains `internal/node/**` and
  `pkg/node/**` (the grants and the export policy both adapters admit by). `plan_test.go`: a change
  to `internal/node/reconcile.go` and to `pkg/node/apiv1/share.go` plans both cells.
- **Acceptance criteria:**
  - `dt`'s planner tests pass with the new cases.
- **Tests:** the planner cases, run once with the new paths removed (catches a v2 change that no
  longer plans the cells).

### T8.4 The setup scripts on the new CLI

**Subtask T8.4 estimate: 2 man-days.**

- **Track:** the team. **Depends on:** none. The RFC 13 plan's epic 6 for running `dt stack`.
- **Covers:** R10.
- **Deliverables:** `env.sh`'s `stack_dev_user` on `dfsctl login`, `user create` with its uid and
  password, and `grant set`; `test/scenarios/setup.sh` on `dfs init`, the new `store`, `namespace`,
  `share create` (with its export policy, no default permission) and `grant set` nouns, and
  `adapter` settings; `lib/tools/dfs-server`'s login on the new client; each scenario's call that
  has a v2 command rewritten (`store`, `share`, `grant`, `user`, `adapter`, `login`). A scenario that
  still calls a command with no v2 command (`system drain-uploads`, `share snapshot`, `trash`,
  `stats`) keeps it, listed in the ledger row §1.2 names.
- **Acceptance criteria:**
  - `shellcheck` passes over every changed script, and no script calls a `dfsctl` noun the new CLI
    does not have but the listed ones.
  - Once the RFC 13 plan's epic 6 serves `docker compose up` on a v2 node, `dt stack up` creates its
    user and grant.
- **Tests:** the `shellcheck` cell; the scripts' behaviour is the `system` suite's, which RFC 22's
  plan's T16.5 turns on. A `rg` over `test/scenarios` and `env.sh` for v1's nouns (`share
  permission`, `store metadata`, `store block`, `adapter enable`) returns only the listed calls.

### T8.5 The admission rows over NFS

**Subtask T8.5 estimate: 2.5 man-days.**

- **Track:** the team. **Depends on:** T8.1. S3 for NFS: the node serving NFS to a Linux kernel
  client over `AUTH_SYS`, with share admission read from the grants (RFC 21's plan's epics 4, 5 and
  7, and its sync point S7).
- **Covers:** R2, R3, R6, R7, R8 (their NFS `AUTH_SYS` part).
- **Deliverables:** `admission_nfs_test.go`: `§3.2`, alice's NFSv3 and NFSv4.1 mounts refused on a
  new share, admitted once granted; `§3.5`, `Finance` read and alice `none`, refused, then alice's
  grant deleted and her mount admitted read, a write refused; `§5.1`, an account of each role
  (D6) mounting every share, each refused, and the administrator's record read back with no
  spelling. D6's `decision:` marker on where the refusal shows.
- **Acceptance criteria:**
  - Each row passes on the `e2e` cell with a kernel client.
- **Tests:** the rows, each broken once: a create that writes a grant for its caller (R6), `none`
  stored as no grant (R7), a role read as an `admin` grant (R8).

### T8.6 The admission rows over SMB

**Subtask T8.6 estimate: 2 man-days.**

- **Track:** the team. **Depends on:** T8.5. S3 for SMB: the node serving SMB to a Linux kernel
  client with NTLM, `TREE_CONNECT` admitted from the grants (RFC 22's plan's epics 4, 5 and 6, and
  its sync point S7).
- **Covers:** R2, R3, R6, R7, R8 (their SMB NTLM part).
- **Deliverables:** `admission_smb_test.go`: the same three rows as T8.5 over `mount -t cifs` with
  NTLM, sharing T8.5's table of accounts and expectations; the bootstrap administrator's own SMB
  mount by name and password, refused on every share.
- **Acceptance criteria:**
  - Each row passes on the `e2e` cell with a kernel client.
- **Tests:** the rows, broken as T8.5's are; the administrator's mount admitted once by planting a
  grant for it (catches an exemption for the bootstrap administrator, R8).

### T8.7 The Kerberos flavours, and `e2e/kerberos` back on

**Subtask T8.7 estimate: 2.5 man-days.**

- **Track:** the team. **Depends on:** T8.5, T8.6. S3 for Kerberos: RPCSEC_GSS `krb5`, `krb5i` and
  `krb5p` (RFC 21's plan's epic 13) and SMB Kerberos sessions (RFC 22's plan's epics 4 and 5), on the
  RFC 18 plan's epic 5; the e2e Kerberos helper mended by the RFC 18 plan's epic 9.
- **Covers:** R2, R3, R6, R8 (their Kerberos part); R11 (`e2e/kerberos`).
- **Deliverables:** `admission_kerberos_test.go`: `§3.2` and `§5.1` with each account's Kerberos
  name, NFS over `krb5`, `krb5i` and `krb5p` and SMB with Kerberos, against the real KDC
  `test/e2e/framework` starts; the test names matching the option's `-run Kerberos`. `tests.yml`:
  `e2e/kerberos` out of the filter.
- **Acceptance criteria:**
  - Each flavour's rows pass on the `e2e/kerberos` cell, and the cell is green on
    `feat/storage-v2`.
- **Tests:** the rows, broken as T8.5's are, once per mechanism (catches an admission check one
  flavour skips).

### T8.8 The data-path cells back on

**Subtask T8.8 estimate: 2.5 man-days.**

- **Track:** the team. **Depends on:** T8.2. S3 for both protocols, and the data path the suites
  exercise; RFC 8's second part for the `badger-s3` profile `pjdfstest` and `fio-verify` run on.
- **Covers:** R11 (`pjdfstest`, `fio-verify`, `bench-smoke`, `clients` but `clients/windows`).
- **Deliverables:** `tests.yml`: each entry out of the filter once its cell passes against the new
  server; `test/posix/known.yaml` rewritten for v2, each entry naming the issue of the epic that
  fixes it in RFC 17's, RFC 21's or RFC 22's plan; `clients`' Linux cells run through epic 7's
  fixture.
- **Acceptance criteria:**
  - Each cell is green on `feat/storage-v2`, `pjdfstest` at its rewritten known failures, and
    `clients/windows` is the only `clients` cell still skipped.
- **Tests:** the suites themselves; a known failure that passes fails the run (RFC 29 §4.7), so the
  file cannot keep a stale entry.

## 7. Order of work

```mermaid
flowchart TD
  T8_1["T8.1 E2E helpers"] --> T8_2["T8.2 §7 CLI as a second user, e2e on"]
  T8_1 --> C1(["after T8.1: the protocol plans can write e2e tests"])
  T8_3["T8.3 E2E paths"]
  T8_4["T8.4 Setup scripts"]
  S3N(["S3 for NFS"]) --> T8_5["T8.5 Admission over NFS"]
  T8_1 --> T8_5
  S3S(["S3 for SMB"]) --> T8_6["T8.6 Admission over SMB"]
  T8_5 --> T8_6
  T8_6 --> T8_7["T8.7 Kerberos flavours, e2e/kerberos on"]
  S3K(["S3 for Kerberos"]) --> T8_7
  T8_2 --> T8_8["T8.8 Data-path cells on"]
  S3N & S3S --> T8_8
  classDef sync stroke:#2c5aa0,stroke-width:3px,stroke-dasharray:4 2
  class S3N,S3S,S3K sync
```

T8.1 goes first: every row and the protocol plans' e2e tests build on it. T8.1 to T8.4 can be done
now; T8.3 and T8.4 depend on nothing and can run beside the others. T8.5 to T8.8 wait for S3, each
for the protocol it mounts; if SMB serves before NFS, T8.6 can go first, taking the shared table of
accounts with it.

## 8. Suggested pull requests

| PR | Subtasks | Man-days | After | Why together |
| --- | --- | --- | --- | --- |
| 1 | T8.1, T8.2 | 3.5 | — | the helpers with their first consumer, and the `e2e` cell on |
| 2 | T8.3, T8.4 | 2.5 | — | the suites' paths and the scripts: no Go, no mount |
| 3 | T8.5, T8.6 | 4.5 | PR 1, S3 for NFS and SMB | the mount rows on the two protocols, one table of accounts |
| 4 | T8.7 | 2.5 | PR 3, S3 for Kerberos | the Kerberos flavours and their cell |
| 5 | T8.8 | 2.5 | PR 1, S3 | the data-path cells, burned down as their data path lands |

PRs 1 and 2 can be open together, now. A suggestion only: one pull request per subtask works as well.

## 9. Open points

- **S3 has not been reached (2026-11-25).** On `feat/storage-v2`, RFC 20's plan's epic 7 starts the
  adapters after the first full reconcile and drains them at stop (`internal/node/adapters.go`),
  but `internal/adapter/nfs` holds only the NFSv4 `COMPOUND` scaffold and `internal/adapter/smb` no
  handler: RFC 21's plan has its epics 1 and 2 open (#5637, #5641) and RFC 22's plan none issued, and
  each plan's own sync point S7 ("the node serves" its protocol) is far off. T8.1 to T8.4 (6 man-days)
  can start now; T8.5 to T8.8 (9.5 man-days) wait. What each needs from S3 is named at the
  subtask, by the RFC 21 and RFC 22 plans' epics; their interfaces are not assumed here.
- Where an NFS refusal shows, at `MOUNT` for NFSv3 and at `PUTFH` or a junction `LOOKUP` for NFSv4,
  or at the first access, is RFC 21's to settle (D6). T8.5's assertion is written once it is.
- How a member of `Finance` is resolved at admission over `AUTH_SYS`, from the store's membership or
  from the client's gids, is RFC 18's and RFC 21's (RFC 18 §6.4); R7's row needs only that the
  membership the API wrote admits alice.
- T8.8's burn-down is sized blind: `pjdfstest`'s v2 failures are unknown until the data path serves.
  The estimate covers the rewrite of the known file and the filter change, not fixing what it
  lists.
- Whether `fio-verify` and `bench-smoke` can run on the journal-only engine before RFC 8's second
  part, on a profile with no remote store, is open; if they can, T8.8 drops that dependency.
- The proposals to RFC 23's plan this document's pull request makes: §5 epic 8's estimate and
  expected content (D1, D2, the second-user form); D9's list of who removes each filter entry (D2);
  the deferral ledger's rows for the protocol e2e tests, the shares file's options, and the
  scenarios' `trash` and `stats` commands.
