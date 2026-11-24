# Epic 10: NFS adapter v2 delegations and the back channel

**Sources:** [RFC 21](../docs/internals/rfcs/rfc-21-nfs.md) §3.1's back-channel slot count,
§3.4's callbacks, security and path down, §5.1's administrator's revoke, §5.2's rule 3 for a
delegation's `seqid` and rule 6, §5.3, §11.4's grant, `AckRecall` and `Reach.Down` rows and its
paragraph on the `OPEN4_SHARE_ACCESS_WANT_*` flags, §11.5's `ErrState` (`NoRecall`) row and "What
runs while draining" for `DELEGRETURN`, §11.6's golden vectors for the delegation types, its single
codec suite for the callback program and its drain test, §12's N6 and N14 on one node and N24's
revoke clause, §13's "§3.4 path down", "§5.3 no v4.0 delegations", "§5.3 revoked delegation *(1
node)*", "§5.2 delegation stateid", "§5.2 stateids *(1 node)*"'s recall-timeout step and "§5.1
administrator's revoke *(1 node)*", and Appendix D's `DELEGRETURN`, `WANT_DELEGATION` and
`GET_DIR_DELEGATION` rows;
[RFC 20](../docs/internals/rfcs/rfc-20-adapters.md) §3.3's NFS callbacks, §4.7's `Conn.Send`,
`Conn.Keep` and `RoleRelease` as `DELEGRETURN` and the drain use them;
[RFC 17](../docs/internals/rfcs/rfc-17-vfs.md) §3.1's `ChannelDown` and §3.2's back-channel and
callback rules, with their §6 row, for the service's half of amendment A2;
[RFC 14](../docs/internals/rfcs/rfc-14-open-state.md) §5.1, §5.4 and §5.5 for what a delegation is
and when one is offered or asked back;
[RFC 24](../docs/internals/rfcs/rfc-24-resources.md) §5.1's "callbacks to clients" row and §6.1 for
a pending callback. Every citation is `develop`'s text, which holds amendment A2 and reaches
`feat/storage-v2` with the cherry-pick of RFC changes.
Cited, not owned: RFC 8881 §20 for the callback program's bytes, §18.16 for `open_delegation4`,
§18.39 for `GET_DIR_DELEGATION`; RFC 5531 for the RPC reply; RFC 20 §2.5's layout of `ops/cb`.
**Track:** A.
**Epic 10 estimate: 9 subtasks, 25.75 man-days.**
**Depends on:** **S2**, NFS's test foundations in CI: E1's request state and members' table (T1.1,
T1.2), E2's `nfsharness` (T2.2) and knfsd's `nfs41/` delegation capture with its `CB_RECALL` (T2.3).
**S4**, an NFSv4.1 session over loopback, for every loopback row. **S2b**, met: amendment A2 is in
RFC 17. Beyond them, pieces of other epics, each named by id in the subtask that takes it and
settled in §9: **E3**'s scripted client with its call encoder and reply decoder (T3.2, its D11) and
its drain rows (T3.4); **E8**'s callbacks value and `v4.Handlers` field (T8.1), its session
registry with each session's `BackChannel`, which records the callback program, credential, slot
count and bound connections (T8.4) and which `BIND_CONN_TO_SESSION` and `BACKCHANNEL_CTL` rebind
(T8.9), and its renewal step's `SEQUENCE` flags from `ClientStatus` (T8.6); **E9**'s stateid codec
and the `ErrState` (`Revoked`) row (T9.3), `OPEN` with D8's want value (T9.4), `CLOSE` (T9.6),
`memory.Service.Clock` (T9.7), `LOCK` and `LOCKT` (T9.8), I/O under a delegation stateid (T9.9),
`TEST_STATEID` and `FREE_STATEID` (T9.10); **E5**'s not-supported handler and `ErrDelay` row
(T5.8).
**Blocks:** E16, whose suites, end-to-end rows and the "§5.3 lock recall" and "§5.3 handle-cached
deny mode" rows run over this epic's delegations.

---

## 1. Goal

Epic 10 makes the server call the client. It builds the NFSv4.1 back channel: `CB_COMPOUND` sent on a
connection the client bound for it, `CB_SEQUENCE`'s slots, the replies the client sends back on the
fore connection. Over it, the callbacks value's grant methods recall delegations and ask for
some back. It offers read and write delegations with `OPEN`, takes them back through `DELEGRETURN`,
answers what was revoked, marks the back channel down when its connection goes, with the service's
half of amendment A2 that clears it again, and runs the drain test against Linux's compounds once
every operation it names exists.

**At the end of the epic:**

- over loopback, an NFSv4.1 client that opens a file alone is offered a read or write delegation as
  its `OPEN4_SHARE_ACCESS_WANT_*` flags allow; a second client's conflicting open is answered
  `NFS4ERR_DELAY` and the first receives `CB_RECALL` on its back channel; its `DELEGRETURN` lets the
  second's retry through;
- an NFSv4.0 client is offered none, and nothing dials its callback address;
- a recall left unanswered past its deadline is `NFS4ERR_DELEG_REVOKED` on the delegation's
  stateid, flagged on `SEQUENCE` until freed; an administrator's revoke keeps the client's session;
- when the back channel's last connection closes, the next `SEQUENCE` carries
  `SEQ4_STATUS_CB_PATH_DOWN` and no delegation is offered, until the client binds a back channel
  again;
- a node stopping answers Linux's compounds as RFC 21 §11.6 says: releases and context run whole,
  everything else no reply, and a compound begun before the drain runs whole.

### 1.1 In scope

`ops/cb`, the callback program's generated structs; the call encoder and reply decoder in `wire`,
moved there from `nfsclient`; the slot table, `Send` and `Reply` of E8's `BackChannel`, and the
router's hand-off of a reply to it;
the callbacks value's `RecallGrant`, `RecallAny`, `Notify` and `LockWaitOver` for an NFSv4 client and
`Revoked` for a grant and an administrator's revoke; `OPEN`'s delegation half, `DELEGRETURN`,
`GET_DIR_DELEGATION` and `WANT_DELEGATION`'s rows; I/O under a delegation stateid; `ChannelDown` in
`pkg/vfs`, `internal/vfs` and `vfstest`, and `Connect` clearing `Reach.Down`; `SEQUENCE`'s
`SEQ4_STATUS_CB_PATH_DOWN`; an administrator's revoke on `memory.New` for tests; the
loopback and handler rows named in §3, and the drain test.

### 1.2 Out of scope

- `CREATE_SESSION`'s and `BIND_CONN_TO_SESSION`'s binding of a back channel, its security choice
  among `csa_sec_parms` and its slot count, `BACKCHANNEL_CTL`, and the re-registration through
  `Connect`: E8 (T8.4, T8.9). E8's T8.4 also builds the session's `BackChannel` with
  `NewBackChannel`, `Bind` and `Unbind`; this epic adds its slot table, `Send` and `Reply`, and
  sends on what E8 records.
- `OPEN` itself, its claim types (`CLAIM_DELEGATE_CUR` and `CLAIM_DELEG_CUR_FH` among them), the
  stateid codec and rules 1 to 5, `TEST_STATEID`, `FREE_STATEID`, `FreeState` on an NFSv4.0 client's
  behalf, the `ErrState` (`Revoked`) row, the `READ`, `WRITE` and `SETATTR` arm for a delegation
  stateid, and the per-request drain roles of `WRITE`, `LOCK` and `OPEN`: E9 (T9.3, T9.4, T9.9,
  T9.10). `SEQUENCE`'s revoked-state flags from `ClientStatus`: E8's T8.6. `memory.New`'s clock:
  E9's T9.7.
- The fence after a missed recall, the grant budget's count and when a grant is offered: the
  service's (RFC 14 §5.3 to §5.5), held by open state's own suite.
- §13's "§5.3 lock recall" with NLM and SMB, "§5.3 handle-cached deny mode", and "§5.2 delegation
  stateid" under Kerberos at `e2e/kerberos`: E16. This epic runs the last at the handler with two
  proven principals (T10.4).
- "§3.4 callback routing", `Rebind` on another node, and a drain naming another gateway, whose
  `WRITE` gets `NFS4ERR_MOVED`: the RFC 21 implementation plan's deferral ledger, row "RFC 21's
  (cluster) parts". A single node's `MoveTarget` names no gateway.
- `CB_NOTIFY_LOCK` (D10): the deferral ledger's row for it.
- `nfs_cb_path_down_total`: this epic is its producer, but rule 3 records nothing in NFS code while
  who records the series only NFS code can count is open with the RFC's author. When that settles,
  this epic adds it, about 0.5 man-days inside its figure, as the plan's open point says.
- A production caller of an administrator's revoke: no RFC 23 endpoint exists; the plan's deferral
  ledger row for the `grace` and `nfs_clients` API endpoints carries it (§9, gap 1).

### 1.3 Names used here

- **E1**, epic 1, the NFS surface: `protocol.State` and `ops/v4`'s members' table.
- **E2**, epic 2, test foundations: `adaptertest/nfsharness` and knfsd's captures cut into golden
  files.
- **E3**, the RPC layer: the scripted client `adaptertest/nfsclient` and `core`'s refusals in each
  table, `ErrDraining` and `ErrMoved` among them.
- **E5**, the NFSv4 namespace, attributes and data path: `READ`, `WRITE`, `SETATTR` and the one
  not-supported handler.
- **E7**, identity's NFS side, which resolves an `AUTH_SYS` caller.
- **E8**, clients and sessions: the callbacks value, the session registry, `CREATE_SESSION`,
  `BIND_CONN_TO_SESSION` and `SEQUENCE`.
- **E9**, opens, stateids and locks: `OPEN`, the stateid codec and rules, `LOCK`, `TEST_STATEID`,
  `FREE_STATEID`.
- **E10**, this epic.
- **E11**, NLM, NSM and the datagram path, which sends NLM's callbacks through the same call encoder.
- **E13**, RPCSEC_GSS, which a Kerberos run of a row needs.
- **E16**, E2E green: the suites, the totality test and the end-to-end rows.
- **S0**, the shared test pieces the RFC 22 implementation plan's epic 2 builds, `vfstest.Mutations`
  among them.
- **S2**, NFS's test foundations in CI, after E1, E2 and S0.
- **S2b**, amendments A1 to A3 agreed, met: they are in RFC 20 and RFC 17.
- **S4**, an NFSv4.1 session over loopback, after E3, E5 and E8.
- **S6b**, Kerberos sign-in, the RFC 18 implementation plan's epic 5, behind `SignIn`.
- **A2**, the amendment to RFC 17 §3.1 and §3.2: `ChannelDown`, and `Connect` clearing a downed back
  channel.
- **E12**, the NFSv4.2 operations, whose hold budget runs in the same sessions.

## 2. How the general rules apply

The rules are those of [the RFC 21 implementation plan's §1.5](rfc-21-nfs-plan.md#15-general-rules-for-each-epic),
cited by number and title and not copied.

| Rule | Subtasks | How |
| --- | --- | --- |
| rule 1, independent unit tests, under `-race` ([`CLAUDE.md`, "Writing tests"](../CLAUDE.md#writing-tests)) | all | handlers on `nfsharness` over `memory.New` (kind 2) with `vfstest.Faults` (kind 3); the back channel over loopback through `core`, the client's side played by E3's scripted client; `memory.New`'s test clock advanced past a recall's deadline (kind 1, T10.7); the service's half against `vfstest`'s conformance suite; no other double |
| rule 2, fuzz tests where relevant | T10.1 | `FuzzParseReply`, the RPC reply decoder that reads the client's answer to a callback, against a second decoder written by hand from RFC 5531; the callback program's structs join `wire`'s `FuzzCodec` and its golden suite (R10). The want flags are a bit set mapped by a total function, held by a property test rather than a fuzz target |
| rule 3, observability follows RFC 25 and RFC 20 §10 | T10.2, T10.6 | no recorder in NFS code; `nfs_cb_path_down_total` waits as §1.2 says; no log line or attribute added here carries a client address, a file handle or a principal (RFC 25 §5.4) |
| rule 4, profiling: leaks, allocations and contention | T10.2, T10.3, T10.9 | every loopback test starts with `adaptertest.CheckLeaks`; a back channel's slot table is shared by every recall to its client, so T10.2 reports its mutex profile under 16 concurrent sends; no codec gate applies, since the callback program carries no `READ`, `WRITE` or `GETATTR` |
| rule 5, public interface and internals, at the layer that consumes a value ([`CLAUDE.md`, "Verifying a change"](../CLAUDE.md#verifying-a-change)) | all | a recall asserted on the `CB_COMPOUND` bytes the client's connection receives; a delegation on the `OPEN` reply the client decodes; path down on the `SEQUENCE` reply; `ChannelDown` on the next `OPEN`'s answer, not only on the record |
| rule 6, known failures shrink | none | no `pynfs`, `cthon04` or `nfstest` test runs before the cutover; pynfs's `DELEG*` tests under NFSv4.0 and its `GET_DIR_DELEGATION` tests stay on RFC 21 §13's known-divergence list |
| rule 7, v2 ignores v1 | all | nothing from `develop`'s delegation or callback code; the back channel follows RFC 8881 §2.10 and §20 and RFC 21 §3.4 |
| rule 8, real dependencies for properties of those dependencies | T10.1, T10.2, T10.4 | `CB_COMPOUND` golden files cut from knfsd's capture, or written by hand from RFC 8881 §20, never vectors the encoder under test made; real TCP over loopback for the back channel |
| rule 9, code comment conventions ([`CLAUDE.md`, "Code comments"](../CLAUDE.md#code-comments)) | T10.2, T10.3, T10.4, T10.5, T10.6, T10.8 | RFC 21 §5.3's `decision:` notes, no delegations for NFSv4.0 and no directory delegations, at `OPEN`'s want mapping and `GET_DIR_DELEGATION`'s handler; the `decision:` markers §4 names; `internal/vfs/clients.go`'s `decision:` rewritten (T10.5) |
| rule 10, a test must be able to fail | all | each test names the break it catches; none asserts a stub's answer |
| rule 11, a symbol is exported only if an interface uses it | T10.1, T10.2, T10.5 | `ops/cb`'s structs for `protocol` and `nfs`; `wire.AppendCall` and `ParseReply` for `nfsclient`, `protocol` and E11's `rpcclient`; `Send` and `Reply` on E8's `protocol.BackChannel` for `nfs`; `vfs.Clients.ChannelDown` and `OpChannelDown` through A2, already in RFC 17 §3.1; under Q1, `RecallGrant`'s handle through an RFC 17 pull request first |
| rule 12, each subtask creates only what it declares | all | `ops/cb` holds only the operations sent; the call codec moves into `wire` with its first server-side sender; no callbacks method is filled before the subtask that sends it |
| rule 13, translate, never decide | T10.3, T10.4, T10.6 | the want flags, `open_delegation4` and a delegation stateid's I/O are pure translations; whether a grant is offered, recalled, revoked or fenced is the service's; `DELEGRETURN` makes one mutating call, `AckRecall`; `ChannelDown` is made outside any handler, on a connection's close |
| rule 14, nothing deferred is lost | all | §1.2 names the owner of each part of an obligation this epic leaves out; §9 the gaps |

## 3. Requirements and invariants

Every MUST and MUST NOT that RFC 21, RFC 20, RFC 17, RFC 14 and RFC 24 place on this epic, numbered
so that subtasks and tests can cite them.

**The back channel**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R1 | A callback to an NFSv4.1 client is sent on a connection the client opened for its back channel, through `core`'s writer (`Conn.Send`); the server opens no connection for it | MUST | RFC 21 §3.4; RFC 20 §3.3, §4.7 | T10.2, T10.3 |
| R2 | A client's latest back-channel registration receives every later callback: after `BIND_CONN_TO_SESSION`, or a `CREATE_SESSION` with `CREATE_SESSION4_FLAG_CONN_BACK_CHAN`, callbacks go there | invariant | RFC 21 §3.4, §12 (N6, its single-node half) | T10.3, T10.6 |
| R3 | A callback with no live back-channel connection fails, with an error the service takes as the client not answering | MUST | RFC 21 §3.4; RFC 20 §3.3; RFC 17 §3.2 | T10.3 |
| R4 | A callback returns once its message is queued, never after the client's answer, and within its context's deadline whether or not the client answers | MUST | RFC 20 §3.3; RFC 17 §3.2 | T10.2, T10.3 |
| R5 | A pending callback has no goroutine of its own and waits in no queue of the adapter's: only the writer's queue, bounded by the callback deadline | MUST NOT | RFC 24 §5.1 ("callbacks to clients"), §6.1 | T10.2 |
| R6 | The back channel's credential is `AUTH_NONE` or `AUTH_SYS`, as the client asked in `csa_sec_parms`; Kerberos (`RPCSEC_GSS`) is never sent on it | MUST | RFC 21 §3.4 | T10.2 |
| R7 | No more callbacks are in flight on a session's back channel than the slots `CREATE_SESSION` granted it, at most 16 | bound | RFC 21 §3.1 | T10.2 |
| R8 | A connection carrying a back channel holds `Conn.Keep` while it does, so the idle timeout passes it over | MUST | RFC 20 §4.7 (`Keep`) | T10.2 |
| R9 | The per-client callbacks value knows the client's connections and nothing about files | invariant | RFC 20 §3.3 | T10.3 |
| R10 | The callback program's procedures join the one golden-vector suite and the one fuzz target every program has, and every decoder of bytes the client sends back is fuzzed | MUST | RFC 21 §11.6 | T10.1 |

**Delegations**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R11 | Read and write delegations are offered to NFSv4.1 and NFSv4.2 clients under RFC 14 §5.4, and the client's `OPEN4_SHARE_ACCESS_WANT_*` flags are honoured | MUST | RFC 21 §5.3 | T10.4 |
| R12 | Every NFSv4.0 `OPEN` returns `OPEN_DELEGATE_NONE`, and the server makes no connection to the client's callback address | MUST | RFC 21 §5.3, §13 ("§5.3 no v4.0 delegations") | T10.4 |
| R13 | A read delegation is a read grant and a write delegation a read and write grant; neither is ever a handle grant | invariant | RFC 21 §5.3, §11.4, §12 (N14); RFC 14 §5.1 | T10.4 |
| R14 | Withdrawn: the `OPEN4_SHARE_ACCESS_WANT_*` flags kept out of `Access` and `Deny` is E9's R17, met by T9.4's `share_access` translation, which hands this epic the want value alone | MUST | RFC 21 §11.4 (access and deny); E9's R17 | — |
| R15 | `CB_RECALL` is the recall: `RecallGrant` with no session and no openers sends it for the delegation on the client's back channel | MUST | RFC 21 §5.3; RFC 17 §3.2; RFC 20 §3.3 | T10.3, T10.4 |
| R16 | `DELEGRETURN` is `AckRecall` to `GrantNone`; an unasked one is a return, and `ErrState` (`NoRecall`), never met, is answered the fault by its own row | MUST | RFC 21 §11.4, §11.5, App. D | T10.4 |
| R17 | `DELEGRETURN` is a release, served while draining | MUST | RFC 21 §11.5 (what runs while draining); RFC 20 §4.7 | T10.4 |
| R18 | I/O under a delegation stateid belongs to the client: it is refused `NFS4ERR_BAD_STATEID` unless the delegation is held, unrevoked, by the call's client, and is then authorised afresh as anonymous I/O | MUST | RFC 21 §5.2 (rule 6), §13 ("§5.2 delegation stateid") | T10.4 |
| R19 | A delegation is never changed in place, so its stateid keeps the `seqid` it was granted with | invariant | RFC 21 §5.2 (rule 3) | T10.4 |
| R20 | `CB_RECALL_ANY` is sent when a client reaches its grant budget at one primary, naming 2048 to keep; for an NFSv4.0 client `RecallAny` answers nil and sends nothing | MUST | RFC 21 §5.3; RFC 14 §5.5; RFC 17 §3.2; RFC 20 §3.3 | T10.3, T10.8 |
| R21 | `GET_DIR_DELEGATION` returns `GDD4_UNAVAIL` with `will_signal_deleg_avail` false | MUST | RFC 21 §5.3 | T10.8 |
| R22 | `WANT_DELEGATION` is not supported | MUST | RFC 21 App. D | T10.8 |
| R23 | No `CB_NOTIFY` is sent: no directory delegation exists for it to ride on | invariant | RFC 21 §5.3; RFC 20 §3.3 | T10.3 |

**Recalls and revocations**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R24 | A `LOCK` or `LOCKT` from another client on a file under a delegation recalls it before the lock is decided; while the recall runs it is `NFS4ERR_DELAY` and leaves no waiter, never `NFS4ERR_DENIED` | MUST | RFC 21 §5.3, §12 (N14, its NFSv4 half) | T10.8 |
| R25 | The adapter keeps no recall deadline of its own: it learns of a revocation only from the service, through `Revoked` with `RevokedGrant`, `Renew`'s `ClientStatus` and `ErrState` (`Revoked`) | invariant | RFC 21 §5.3 | T10.3, T10.7 |
| R26 | A delegation revoked at its recall deadline is `NFS4ERR_DELEG_REVOKED`, never `NFS4ERR_BAD_STATEID`, for a `WRITE` under it and for its `TEST_STATEID`; `SEQ4_STATUS_RECALLABLE_STATE_REVOKED` is set until it is freed, and its `FREE_STATEID` succeeds | test row | RFC 21 §13 ("§5.3 revoked delegation *(1 node)*", "§5.2 stateids *(1 node)*"'s recall step) | T10.7 |
| R27 | An administrator's revoke keeps the client and its sessions: the next `SEQUENCE` succeeds on the same session with `SEQ4_STATUS_ADMIN_STATE_REVOKED`, and each revoked stateid, a delegation's included, is `NFS4ERR_ADMIN_REVOKED`, never `NFS4ERR_DELEG_REVOKED`, until freed | MUST | RFC 21 §5.1, §12 (N24), §13 ("§5.1 administrator's revoke *(1 node)*") | T10.3, T10.7 |

**The path down**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R28 | When a client is left with no working back channel on a node, for any reason, that node's adapter calls `ChannelDown`: its last bound back-channel connection closed, the session holding it destroyed, the connection re-bound for the fore channel only, or the client silent, every slot of its back channel outstanding past the callback deadline | MUST | RFC 17 §3.2; RFC 21 §3.4; RFC 14 §5.4 (no grant without a working callback path) | T10.6 |
| R29 | `ChannelDown` sets the record's `Reach.Down` only while `Reach.Callback` names the calling node, and changes nothing otherwise; it is `StateError` `StaleClient` once the record is released | MUST | RFC 17 §3.1, §3.2 | T10.5 |
| R30 | A `Connect` that re-registers a back channel clears `Reach.Down` through `Rebind`, on the node that marked it down as on any other, a single node included | MUST | RFC 17 §3.2; RFC 21 §3.4 | T10.5 |
| R31 | While `Reach.Down` is set, the client's next `SEQUENCE` reply sets `SEQ4_STATUS_CB_PATH_DOWN` | MUST | RFC 21 §3.4, §11.4 | T10.6 |
| R32 | While the path is down no delegation is offered to the client; once a re-bind clears it, one is offered again | MUST NOT | RFC 21 §3.4, §13 ("§3.4 path down"); RFC 14 §5.4 | T10.6 |
| R33 | A lost back channel is offered no grant until re-registered, on one node too: after `ChannelDown`, `Record` shows `Reach.Down` and the next open is offered no delegation; the next `Connect` clears it; a `ChannelDown` from a node the record no longer names changes nothing | test row | RFC 17 §6 (§3.2's lost back channel row) | T10.5 |
| R35 | A closing connection's back-channel cleanup, its release of the back channel and any `ChannelDown` it makes, completes before a back channel of the same client is bound again on this node, so a late `ChannelDown` never marks down the path a rebind has just re-registered | MUST | RFC 20 §3.2 ("Cleanup comes before reconnect"); RFC 17 §3.2 | T10.2, T10.6 |

**Draining**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R34 | The drain against Linux's compounds, on the only node stopping: `SEQUENCE`, `PUTFH`, `GETATTR`, `CLOSE`, and the same with `DELEGRETURN`, run whole; NFSv4.0's `PUTFH`, `GETATTR` of `fsid` and `fs_locations`, `RENEW` is answered in full; `EXCHANGE_ID` gets no reply; `SEQUENCE` under NFSv4.0 is `NFS4ERR_OP_ILLEGAL`; `SEQUENCE`, `PUTFH`, `WRITE` gets no reply, not a results array cut short, and its slot keeps its previous sequence ID and reply, so its retransmission is refused again, never `NFS4ERR_RETRY_UNCACHED_REP`; `SEQUENCE`, `PUTFH`, `CLOSE`, `PUTFH`, `READ` gets no reply and runs nothing, the first file stays open, and its retransmission is refused the same way, never `NFS4ERR_BAD_STATEID`; a compound whose `CLOSE` ran before the drain began runs whole and is answered | test row | RFC 21 §11.6, §7.4; RFC 20 §4.7 | T10.9 |

## 4. Decisions this epic takes

- **D1. How `CB_RECALL` names its file. Pending the user's answer** (Q1). `CB_RECALL` carries the
  file's handle (RFC 8881 §20.2), but `RecallGrant` hands the adapter a `CachingGrant`, whose `File`
  is a `FileID`, and handles are the service's to mint (RFC 7 §6.1). Recommended: RFC 17 §3.2's
  `RecallGrant` gains the grant's file handle, `RecallGrant(ctx, s, openers, g, h Handle, to)`,
  which `internal/vfs`'s sink mints with `entity.NewHandle`. The sink makes no such call today, and
  every `NewHandle` in `internal/vfs` takes the admitted share's ID, which `CachingGrant` does not
  carry: minting `h` needs the share, either `CachingGrant` gaining the share's ID from the open
  that made it (an open-state change, no lookup) or a FileID→share lookup per recall; the first is
  recommended. Inserting `h` moves `to` to `Args[4]`, so `internal/vfs/locks_test.go`'s
  `Args[3].(vfs.GrantKind)` and about seven call sites in `vfstest`'s callback tests change with it.
  The other implementations today are `vfstest.Callbacks`, `vfstest.CallbackFaults` and one test
  helper in `internal/vfs`; no adapter implements it yet. The alternative, a map from `GrantID` to handle
  kept on the callbacks value from `OPEN` to `DELEGRETURN`, breaks R9 and misses every delegation
  granted through another node once the back channel moves (cluster). Prior art: knfsd keeps the
  handle in the delegation's file state and encodes it from there (from memory). A `decision:`
  marker at `callbacks.go`'s `RecallGrant` only under the alternative.
- **D2. The connection a callback goes on.** The callbacks value holds the client's sessions on this
  node, and each session's `BackChannel` its connections in the order they were bound. A callback
  goes on the most recently bound connection still live, of the session most recently bound, so
  N6's latest registration wins on one node. `ChannelDown` is called when the client has no working
  back channel left on this node, for any of R28's causes, not when each connection goes. Prior
  art: knfsd picks the callback connection from the client's most recent session with a back
  channel (from memory). A `decision:` marker at `BackChannel.Unbind`, overturned if a client is
  seen expecting a callback on a connection other than its latest.
  Under `SP4_NONE`, whoever knows a session's ID can bind a connection to its back channel and
  so receive its recalls: E8 checks the owner on `BIND_CONN_TO_SESSION` only under `SP4_MACH_CRED`
  (its R15). That leaks the recalled stateids and handles, and a recall answered by no one gets the
  real holder's delegation revoked at its deadline, but grants no data: I/O under a delegation
  stateid is authorised afresh (R18). A `decision:` marker at `Bind` names that ceiling, overturned
  if a deployment is seen relying on `SP4_NONE` sessions over a network where session IDs can be
  read, when the bind would check the session's principal whatever the state protection.
- **D3. `CB_SEQUENCE` as the requester.** `csa_cachethis` false and `csa_referring_call_lists`
  empty, as knfsd sends (from memory). On a slot, an unchanged sequence ID means a retry of the same
  request (RFC 8881 §2.10.6.1), and a `CB_SEQUENCE` error leaves the client's slot unchanged
  (§2.10.6.1.2), so:
  - a slot's sequence ID advances only on a `CB_SEQUENCE` result of `NFS4_OK`, or on
    `NFS4ERR_RETRY_UNCACHED_REP` to a resent call, which proves the client ran it;
  - any other `CB_SEQUENCE` error, and an RPC-level refusal (`PROG_UNAVAIL`, `MSG_DENIED` and the
    like) where `CB_SEQUENCE` never ran, frees the slot with its sequence ID unchanged and fails
    that callback;
  - a call unanswered when its connection closes keeps its slot and its encoded bytes, and is resent
    unchanged, the same sequence ID, as the first thing sent on the back channel's next bound
    connection (§2.10.6.2 lets a requester re-send only after a disconnect); its slot is free only
    once that resend is answered. knfsd resends the callback the same way (from memory).
  The slot record holds the bytes, so the bound stays the slot count (R5, R7). A `decision:` marker
  at the slot table on the empty referring-call lists, overturned if a client is seen misordering a
  recall against the `OPEN` reply that granted it.
- **D4. No slot free is a callback not sent, which the service sends again.** RFC 24 §5.1 bounds a
  callback by the writer's queue and its deadline and gives the adapter no queue of its own (R5), so
  a `Send` that finds every slot busy answers `ErrNoSlot` at once and queues nothing. `RecallGrant`
  answers that `sent` false with a nil error: `internal/vfs/sink.go` resends a recall answered so
  every `grantRetry` (10 s) until the callback deadline, while one answered with an error is
  resent only by another route (another registration or session). A recall burst larger than the
  slot count, which a client granting one back-channel slot meets at two recalls, is so delivered as
  slots free. `ErrNoBackChannel` and the silent client below stay errors: there is nowhere to send
  until the client binds again, which re-registers through `Connect` and so is another route. A
  `decision:` marker at `BackChannel.Send`, overturned if a delegation is seen revoked at its
  deadline while its recall waited for a slot. A client that never answers on a live connection
  keeps its slots busy (D3); once every slot has been outstanding past the 35 s callback deadline
  the back channel is silent, its callbacks fail and R28's `ChannelDown` runs (T10.6), so recalls
  are revoked at their deadline, the outcome RFC 21 §3.4 gives a callback with nowhere to go, and no
  delegation is offered until a re-bind.
- **D5. Where `SEQUENCE` reads `Reach.Down`. Pending the user's answer** (Q2). `ClientStatus` has no
  field for it, so `Renew` does not report it. Recommended: `ClientStatus` gains `ChannelDown`, which
  `Renew` reports from what open state already holds in memory (`known[c].down`, set by `setReach`,
  the same value the grant offer reads), so it costs no read. E8's renewal step, which calls `Renew`
  at most once per renewal interval, keeps it with the cached `ClientStatus`; this node's own
  `ChannelDown` and its re-registration through `Connect` drop the cache, and a renewal that began
  before the drop does not store its result (a generation the drop raises), so on one node the next
  `SEQUENCE` is exact. Another node's report reaches the client up to one renewal interval late,
  the staleness RFC 14 §5.3 already accepts for revoked-state flags. Prior art: knfsd sets the flag
  from its in-memory callback state on every `SEQUENCE` (from memory). A `decision:` marker at the
  renewal step naming the interval.
- **D6. The want flags.** `OPEN4_SHARE_ACCESS_WANT_NO_PREFERENCE`, `WANT_ANY_DELEG` and
  `WANT_WRITE_DELEG` ask for read and write, `WANT_READ_DELEG` for read, `WANT_NO_DELEG` and
  `WANT_CANCEL` for none; the push and signal flags are hints and never answered, since nothing
  pushes a delegation or signals one available. E9's T9.4 has already refused a want code RFC 8881
  does not define, and any want bit under NFSv4.0, `NFS4ERR_INVAL` (its D8 and R17), so the mapping
  sees only defined codes; under minor version 0 it sees none, and the want is none. An `OPEN` that asked for a delegation by name and
  got none is answered `OPEN_DELEGATE_NONE_EXT`, `WND4_NOT_WANTED` for `WANT_NO_DELEG`,
  `WND4_CANCELLED` for `WANT_CANCEL` (RFC 8881 §18.16.3) and `WND4_CONTENTION` otherwise, with
  neither push nor signal promised; one that stated no preference, `OPEN_DELEGATE_NONE`. The
  service's `GrantNone` carries no reason, so a refusal at the grant budget, which `WND4_RESOURCE`
  would name, is answered `WND4_CONTENTION` too. Prior art: knfsd answers `WND4_CONTENTION` or
  `WND4_RESOURCE` in `OPEN_DELEGATE_NONE_EXT` (from memory). A `decision:` marker beside RFC 21
  §5.3's on NFSv4.0, at the mapping.
- **D7. What a delegation carries.** Its stateid is the grant's `StateRef` through E9's codec with
  `seqid` 1, the value it keeps (R19); a delegation stateid quoted with `seqid` 0 means current, any
  other than 1 is `NFS4ERR_BAD_STATEID`. `recall` is false; a write delegation's
  `nfs_space_limit4` is `NFS_LIMIT_SIZE` at the largest size; the `permissions` of a read and of a
  write delegation alike are an empty `ACE4_ACCESS_ALLOWED_ACE_TYPE` entry, which tells the client to
  check access with the server, asserted in T10.4's golden file for both types.
  `CB_RECALL`'s `truncate` is false. Prior art: knfsd sends `seqid` 1, no space limit and an empty
  ACE (from memory). No marker: RFC 8881 gives no default for these fields, and each value is the
  one that promises the client nothing (no limit to honour, no access decided locally), which the
  `delegation` function's doc comment says.
- **D8. `WANT_DELEGATION` is `NFS4ERR_NOTSUPP`. Pending the user's answer** (Q3). Appendix D has it
  "answered as this RFC says of each", but RFC 21 says nothing of it. Its row points at E5's one
  not-supported handler (T5.8). Prior art: RFC 8881 makes the operation optional, and knfsd does not
  serve it (from memory). The document's pull request proposes the sentence for RFC 21 §5.3.
- **D9. `GET_DIR_DELEGATION` makes no service call.** It needs a current filehandle
  (`NFS4ERR_NOFILEHANDLE` otherwise, E3's row) and answers `GDD4_UNAVAIL` whatever the handle names,
  a file included, rather than reading it for `NFS4ERR_NOTDIR`. A `decision:` marker at the handler,
  overturned if a client is seen relying on `NFS4ERR_NOTDIR` there. Prior art: none checked.
- **D10. `CB_NOTIFY_LOCK` is not sent.** RFC 21 §5.5 makes it optional and the poll decides, so
  `LockWaitOver` for an NFSv4 client sends nothing and answers neither refused nor an error. A
  `decision:` marker at `LockWaitOver`, overturned by the deferral ledger's trigger for it: a
  supported client shown to wait on it rather than poll. Prior art: knfsd sends it, Linux clients
  poll regardless (from memory).
- **D11. `Revoked` ends nothing an NFS client still uses.** NFS holds no request pending on a grant
  or on state an administrator revoked: a recall's slot waits for its own answer, and the session,
  its back channel and the client's slots stay (R27). For `RevokedGrant` and `RevokedByAdmin` the
  callbacks value therefore does nothing; E8's `Renew` carries the flags and E9's rows the errors. A
  `decision:` marker at `Revoked`, overturned if NFS ever holds a request on a grant.
- **D12. The call encoder and reply decoder move into `wire/call.go`** from `nfsclient`, as E3's D11
  foresees, when the back channel sends its first call; `nfsclient` keeps its surface and calls them.
  T10.1 moves them: it is this epic's first subtask, done before E11's T11.2, which only uses them.
  No marker.
- **D13. `memory.New` exposes an administrator's revoke.** `memory.Service.Revoke(ctx, cl)` calls
  open state's `Revoke`, the real path an administrator's revoke takes, which no `vfs.Service`
  method reaches yet (§9, gap 1). `Service` keeps no open-state table today (`New` builds it and
  drops it), so T10.7 adds an unexported field holding the table `New` already makes. A test advances E9's `memory.Service.Clock` (T9.7), the test clock
  open state already reads (kind 1), past a recall's deadline. No marker: the method's doc comment
  says what it drives.

- **D14. The connection's close is `Stages.OnClose`, and a path-down is ordered before the next
  bind.** E8 records the bound connections but learns of no close; RFC 20 §4.7 gives every
  connection the hook, `Stages.OnClose`, which NFS's `Protocol.NewConn` builds per connection, and
  says a connection's `OnClose` drops its reference to each session it carried. T10.2 sets it to a
  closure that calls `Unbind` for each `BackChannel` the connection is bound to; no `core` change and
  no RFC change. When an `Unbind` leaves none of the client's sessions on this node with a bound
  connection, T10.6's `ChannelDown` runs inside the same `OnClose`, under its 30 s context, while
  the closure holds a per-client back-channel lock that T10.2's `Bind` also takes; a
  `CREATE_SESSION`, `BIND_CONN_TO_SESSION` or `BACKCHANNEL_CTL` binds before it calls `Connect`, so
  its `Rebind` lands after the `ChannelDown` and clears it (R35). RFC 20 §3.2 keys that wait by the
  session; it is per client here because `ChannelDown` is the client's. A `ChannelDown` still failing
  when the context ends is dropped, not handed to `Deps.Retry`: the path stays up in the record,
  recalls fail at once on the dead channel and are revoked at their deadline, as RFC 21 §3.4 has a
  callback with nowhere to go; the drop is logged at `Warn` with the client ID and no address
  (rule 3). A connection the server closes itself, in `Stop`, makes no `ChannelDown`: a stopping
  node offers no grant (`OPEN` is refused while draining) and RFC 21 §3.4 counts the node's loss as
  the path down, so one service write per connection inside the 30 s grace would only make stop
  time grow with the clients; a `decision:` marker at the `OnClose` says so, overturned if a
  stopped node's clients are seen offered delegations by another node before they re-bind. A
  `decision:` marker at `Bind` on the lock held across a service call, which blocks only that
  client's binds; overturned if a bind is seen waiting on a slow `ChannelDown` in a profile. Prior art: knfsd marks the callback path down from its transport's
  close callback (`nfsd4_conn_lost`, from memory).

## 5. Target tree

```text
pkg/vfs/
  callbacks.go                     T10.3  RecallGrant's handle, under Q1
  service.go                       T10.5  Clients.ChannelDown, OpChannelDown
  vfstest/mutations.go, null.go, faults.go   T10.5  ChannelDown
  vfstest/callbacks.go             T10.3  RecallGrant's handle, under Q1
  vfstest/suite/clients.go         T10.5  RFC 17 §6's lost back channel row
  vfstest/memory/memory.go         T10.7  Revoke (Clock is E9's T9.7)
pkg/openstate/
  clients.go                       T10.5  Rebind writing only a changed reach; T10.6  ClientStatus.ChannelDown, under Q2
  grants.go                        T10.3  CachingGrant's share ID, under Q1
internal/vfs/
  clients.go                       T10.5  ChannelDown, Connect's Rebind, the decision rewritten
  sink.go                          T10.3  RecallGrant's handle, under Q1
internal/adapter/nfs/
  callbacks.go                     T10.3  RecallGrant, RecallAny, Notify, LockWaitOver, Revoked for grants and an administrator's revoke; T10.6  ChannelDown when no working back channel is left
  delegation_test.go               T10.4, T10.6, T10.7, T10.8  loopback rows
  drain_test.go                    T10.9  the drain against Linux's compounds
  wire/
    call.go                        T10.1  AppendCall, ParseReply, moved from nfsclient
    call_fuzz_test.go              T10.1  FuzzParseReply
    testdata/golden/               T10.1, T10.4  knfsd-nfs41-cb-*.hex, rfc-cb-recall-any-*.hex, delegation types
  protocol/
    backchannel.go                 T10.2  E8's BackChannel: slots, CB_SEQUENCE, Send, Reply, Keep and the client's lock in Bind
    protocol.go                    T10.2  NewConn's Stages.OnClose unbinding the connection (D14)
    stages.go                      T10.2  Route hands a reply to its connection's back channel
  ops/
    cb/types.go                    T10.1  generated CB_COMPOUND, CB_SEQUENCE, CB_RECALL, CB_RECALL_ANY
    v4/open_deleg.go               T10.4  the want flags, open_delegation4
    v4/delegreturn.go              T10.4  DELEGRETURN
    v4/errors.go                   T10.4  ErrState (NoRecall) at the fault
    v4/getdirdeleg.go              T10.8  GET_DIR_DELEGATION
    v4/handlers.go                 T10.8  WANT_DELEGATION's row to the not-supported handler
    v4/sequence.go                 T10.6  SEQ4_STATUS_CB_PATH_DOWN (E8's file)
internal/adapter/adaptertest/nfsclient/
  client.go                        T10.1  the codec from wire
```

## 6. Subtasks

### T10.1 The callback program's bytes

**Subtask T10.1 estimate: 2 man-days.**

- **Track:** A. **Depends on:** S2, E3's T3.2.
- **Covers:** R10.
- **Deliverables:**
  - `ops/cb/types.go`, generated by `wire/gen` from RFC 8881 §20's XDR: `CB_COMPOUND`'s arguments
    and results, and the operations this epic sends, `CB_SEQUENCE`, `CB_RECALL` and
    `CB_RECALL_ANY`, with `nfs_cb_argop4` and `nfs_cb_resop4` over those three and `CB_ILLEGAL`.
    No `Handlers` and no table: the server serves none of it. Consumed by T10.2 (`CB_SEQUENCE`) and
    T10.3 (`CB_RECALL`, `CB_RECALL_ANY`).
  - `wire/call.go`, moved from `nfsclient` (D12):
    ```go
    type Auth struct {
    	Flavour uint32
    	Body    []byte
    }
    type Reply struct {
    	XID       uint32
    	Denied    bool
    	Stat      uint32
    	Verf      Auth
    	Low, High uint32
    	Body      []byte
    }
    func AppendCall(dst []byte, xid, prog, vers, proc uint32, cred, verf Auth) []byte // the header; the arguments follow
    func ParseReply(rec []byte) (Reply, error)                                     // one record's reply; ErrNotReply for a call
    ```
    `nfsclient`'s `Auth` and `Reply` become aliases of these, its surface unchanged. Consumed by
    `nfsclient`, T10.2 and E11's `rpcclient` (T11.2).
  - `wire/call_fuzz_test.go`: `FuzzParseReply` against a second decoder written in the test from
    RFC 5531 §9: no panic, every length inside the record, and the two decoders agreeing.
  - Golden files: knfsd's `CB_COMPOUND` call and reply cut from E2's `nfs41/` delegation capture
    (T2.3), a `CB_RECALL_ANY` call and reply written by hand from RFC 8881 §20.6, each added to the
    golden suite and as a seed of `FuzzCodec`.
- **Acceptance criteria:**
  - Each golden `CB_COMPOUND` decodes into the generated structs and encodes back to its bytes.
  - `ParseReply` reads every reply golden file `nfsclient`'s tests already read, to the same fields.
- **Tests:** the golden cases at `ops/cb`'s structs, which T10.2 and T10.3 encode with (catch a field
  out of place); `FuzzCodec` with the new seeds and `FuzzParseReply` (catch a decoder that reads past
  its record or disagrees with RFC 5531, R10); E3's `nfsclient` tests, unchanged, over the moved codec
  (catch a move that changes the bytes). No test double.
- **Rules:** rule 11's exports above; rule 12: `ops/cb` holds only the operations sent.

### T10.2 The back channel

**Subtask T10.2 estimate: 4.5 man-days.**

- **Track:** A. **Depends on:** T10.1, S4, T8.4, T8.9.
- **Covers:** R1, R4, R5, R6, R7, R8, R35.
- **Deliverables:** `protocol/backchannel.go`:
  ```go
  // Send and Reply extend E8's BackChannel (T8.4), whose NewBackChannel, Bind and
  // Unbind keep the program, credential, granted slots and bound connections.
  func (b *BackChannel) Send(ctx context.Context, ops []cb.NfsCbArgop4) error // CB_SEQUENCE first, on the latest live connection, once queued
  func (b *BackChannel) Reply(c *core.Conn, r wire.Reply) bool // an answer to one of b's calls: frees its slot
  ```
  the slot table; T8.4's `Bind` taking `Keep` on the connection and `Unbind` releasing it;
  `ErrNoBackChannel`, `ErrNoSlot` and `ErrChannelSilent` for D4's answers. E8's `CREATE_SESSION` with
  `CREATE_SESSION4_FLAG_CONN_BACK_CHAN` (T8.4), and its `BIND_CONN_TO_SESSION` and `BACKCHANNEL_CTL`
  (T8.9), call `NewBackChannel` and `Bind` with what they accepted; this subtask calls `Unbind` when a
  bound connection closes, from the `Stages.OnClose` that `protocol.NewConn` builds for it (D14),
  freeing the slots of the calls sent on that connection, and makes `Bind` take the client's
  back-channel lock (R35). A call's XID comes from one allocator per connection, since one
  connection can carry several back channels (D14); `stages.go`'s `Route` matches a reply by
  (connection, XID) to the `BackChannel` that sent it and hands it to its `Reply`, and drops any
  other reply, as it drops any message that is not a call. `Reply` frees the slot only when the
  `CB_SEQUENCE` result names that call's session, slot and sequence ID, and drops a reply that does
  not; either drop is logged at `Debug`, with no address (rule 3). A slot advances, keeps its
  sequence ID, or keeps its call for a resend as D3 says, and the resends go first on the next
  `Bind`. An outstanding call is a slot record, not a goroutine. Consumed by T10.3 and T10.6.
- **Acceptance criteria:**
  - Over loopback, a session created with a back channel and slot count 2 receives, on its
    connection, a `CB_COMPOUND` to the session's callback program whose `CB_SEQUENCE` names its
    session, slot 0 and sequence ID 1, under `AUTH_SYS` when the session asked for it and `AUTH_NONE`
    when it asked for that.
  - `Send` returns before the client answers; the client's answer on the fore connection frees the
    slot, and the next call on that slot carries sequence ID 2.
  - With both slots unanswered, a third `Send` fails `ErrNoSlot` at once; with the connection
    closed, `ErrNoBackChannel`.
  - A bound connection outlives the idle timeout on the server's injected clock, and closes after it
    once unbound.
- **Tests:** over loopback through `core` on `memory.New`, the client's side played by E3's
  scripted client reading the call with `Read` and answering with `Send` (the layer that receives a
  callback): the call's bytes against T10.1's golden encoding with this session's fields (catch a
  credential other than the one asked, R6, a call on another connection, R1); `Send` returning with
  the reply unread (catches a wait for the answer, R4); the sequence ID after an answer and after a
  close (catch D3's rules broken); the third `Send` (catches a queue, R5, or a slot past the grant,
  R7); the client closing its only bound connection, then a `Send` failing `ErrNoBackChannel`
  (catches no `OnClose` reaching `Unbind`, D14); `runtime.NumGoroutine` unchanged across 16 outstanding calls (catches a goroutine per call,
  R5); the idle timeout (catches a back channel left without `Keep`, R8); a reply whose XID matches
  no call, dropped with no answer and the connection still serving (catches a reply routed as a
  call). D3's rules, each at the bytes the scripted client reads: a call whose reply is lost, the
  connection closed, a new one bound: the same call is resent first with the same sequence ID, the
  client answers it, and the next recall on that slot carries the next ID and succeeds (catches a
  different call sent on a reused ID, answered `NFS4ERR_RETRY_UNCACHED_REP`); one call answered
  `PROG_UNAVAIL`, then the next call on that slot carries the same sequence ID and succeeds
  (catches a slot advanced past a call the client never ran, which never converges). Reply routing:
  two sessions on one connection, each with a call outstanding, each answer freeing its own
  session's slot, and a reply with a matching XID but another session's ID in its `CB_SEQUENCE`
  result dropped with the slot still busy (catches a stray reply freeing another session's slot).
  No test double.
- **Rules:** rule 4, the slot table's mutex profile under 16 concurrent `Send`s; rule 9, D3's and
  D4's `decision:` markers.

### T10.3 The callbacks value's grant methods

**Subtask T10.3 estimate: 3 man-days.**

- **Track:** A. **Depends on:** T10.2, T8.1.
- **Covers:** R1, R2, R3, R4, R9, R15, R20, R23, R25, R27.
- **Deliverables:** in `nfs/callbacks.go`, on T8.1's per-client value, which holds the client's
  sessions on this node:
  - `RecallGrant`: `CB_RECALL` of the delegation's stateid and file handle (D1, D7) through the
    latest bound session's `BackChannel` (D2), answering `sent` once queued; `sent` false and a nil
    error when no slot is free, so the service resends it (D4); the error when no back channel is
    live or it is silent (R3, D4), the silent case after T10.6's `ChannelDown`.
  - `RecallAny`: `CB_RECALL_ANY` with `craa_objects_to_keep` the service's `keep` and the read and
    write delegation types in its mask; nil and nothing sent for an NFSv4.0 client.
  - `Notify`: nil and nothing sent (R23). `LockWaitOver` for an NFSv4 client: nothing sent, neither
    refused nor an error (D10).
  - `Revoked` with `RevokedGrant` or `RevokedByAdmin`: nothing ended (D11). E9's T9.7 adds the
    state reasons; E11's T11.3 NLM's.
  - Under Q1's recommended answer, the RFC 17 §3.2 pull request first, then `RecallGrant`'s handle in
    `pkg/vfs/callbacks.go`, minted in `internal/vfs/sink.go` from the share's ID `CachingGrant` gains
    in open state (D1), and carried by `vfstest.Callbacks`, `vfstest.CallbackFaults` and
    `internal/vfs`'s test helper, with `locks_test.go`'s `Args` index and `vfstest`'s callback tests
    moved with the new argument.
  Consumed by the service through E8's `Connect`, T10.4's recall row and T10.6.
- **Acceptance criteria:**
  - A `RecallGrant` with a zero session and no openers puts one `CB_COMPOUND`, `CB_SEQUENCE` then
    `CB_RECALL` naming the delegation's stateid and handle, on the client's latest back channel.
  - After a `BIND_CONN_TO_SESSION` of a second connection for the back channel, the next recall
    arrives there and not on the first.
  - With no back channel live, `RecallGrant` answers an error and sends nothing.
  - A session granted one back-channel slot, two delegations recalled at once through the service:
    both `CB_RECALL`s arrive, the second once the first is answered and the service's clock is
    advanced past `grantRetry`.
  - `RecallAny(2048)` sends `CB_RECALL_ANY` keeping 2048; for an NFSv4.0 client it answers nil.
  - `Revoked` of either reason leaves the client's sessions, back channel and slots as they were.
- **Tests:** over loopback after S4, on `memory.New`, each method called through the `vfs.Callbacks`
  E8's `Connect` registered with the service, the call read by the scripted client (the layer the
  callback reaches): the recall's bytes (catch a recall of the wrong stateid or file, R15); the
  second connection (catches callbacks pinned to the first registration, R2); the closed back
  channel (catches a callback reported sent with nowhere to go, R3); the one-slot session, through
  `memory.New`'s service and its clock (kind 1), at the second `CB_RECALL` the scripted client
  reads (catches `ErrNoSlot` answered as an error, which the service never resends, D4);
  `RecallAny` for both minor
  versions (catch a wrong keep count, or a call to an NFSv4.0 client, R20); `Revoked` of a grant and
  of the client by administrator, followed by a `SEQUENCE` on the same session that succeeds
  (catches a session torn down, R27); every method returning with the client silent, under a
  context of 1 s (catches a wait for the answer, R4); `Notify` called on the value with a session
  bound, answering nil with nothing on the scripted client's connection (catches a `CB_NOTIFY`
  sent, R23). R9 has no test: it is held by the value's fields, which name sessions and no file,
  checked in review. No test double.
- **Rules:** rule 9, D10's and D11's `decision:` markers; rule 11, under Q1, the RFC 17 pull request
  before `pkg/vfs`'s signature changes.

### T10.4 Delegations offered, recalled and returned

**Subtask T10.4 estimate: 3 man-days.**

- **Track:** A. **Depends on:** T10.3, T9.3, T9.4, T9.9.
- **Covers:** R11, R12, R13, R15, R16, R17, R18, R19.
- **Deliverables:**
  - `ops/v4/open_deleg.go`: `func want(minor uint32, w uint32) vfs.GrantKind`, a pure, total
    function over the want value T9.4 split from `share_access` and already checked (D6), never
    giving a handle grant; `func
    delegation(g *vfs.CachingGrant, wanted bool, w vfs.GrantKind) openDelegation4` encoding
    `OPEN_DELEGATE_READ`, `OPEN_DELEGATE_WRITE`, `OPEN_DELEGATE_NONE` or `OPEN_DELEGATE_NONE_EXT`
    (D6, D7). E9's `OPEN` passes `want`'s kind as `OpenRequest.Want` and encodes `delegation`'s answer
    in its reply.
  - `ops/v4/delegreturn.go`: `DELEGRETURN`, one `AckRecall` to `GrantNone` with the stateid's
    `GrantID`, `Role` `RoleRelease`, a delegation `seqid` other than 0 and 1 refused
    `NFS4ERR_BAD_STATEID` before the call (R19).
  - "§5.2 delegation stateid" over a delegation `OPEN` offered: the `READ`, `WRITE` and `SETATTR`
    arm for a stateid of the grant type is E9's T9.9, `vfs.Delegated(g, fh)` with the current
    filehandle and no other check in the adapter (R18); this subtask runs the row over it.
  - `ops/v4/errors.go`: `ErrState` (`NoRecall`) at `NFS4ERR_SERVERFAULT`, by its own row.
  - The golden file of §11.6's delegation types: each want code and its `GrantKind`, each grant and
    its `open_delegation4`, the empty ACE on the read and the write type alike (D7), NFS → neutral →
    NFS.
  Consumed by T10.6, T10.7, T10.8, T10.9 and E16.
- **Acceptance criteria:**
  - Over loopback, an NFSv4.1 `OPEN` for write of a file no other client holds, with no want flag,
    is offered a write delegation; an `OPEN` for read with `WANT_READ_DELEG` a read one; with
    `WANT_NO_DELEG` none,
    `OPEN_DELEGATE_NONE_EXT` with `WND4_NOT_WANTED`.
  - A second client's `OPEN` for write is `NFS4ERR_DELAY`; the first receives `CB_RECALL`; after its
    `DELEGRETURN`, the second's retry is granted.
  - An NFSv4.0 client with a callback address the test listens on opens the file read-only, then for
    write: `OPEN_DELEGATE_NONE` both times, and nothing connects to the address.
  - Every want code T9.4 passes, under each minor version, maps to a kind, never a handle grant.
- **Tests:** at `want` and `delegation`, which E9's `OPEN` calls: the golden file (catches a kind
  mapped differently, R11, R13), and a loop over every defined want code under minor versions 0, 1
  and 2 (catches a handle grant, R13, or a delegation wanted under NFSv4.0, R12). On `nfsharness` over `memory.New` with two proven principals of one client: alice's
  `OPEN` of a 0644 file she owns with a read delegation, then bob's `READ` under its stateid served
  and his `WRITE` `NFS4ERR_ACCESS`, and another client's `READ` under it `NFS4ERR_BAD_STATEID` (catch
  delegation I/O not reauthorised, or served for another client, R18); `DELEGRETURN` with `seqid` 2
  `NFS4ERR_BAD_STATEID` and with 1 accepted (catch R19); `DELEGRETURN`'s one `Mutations` entry,
  `AckRecall` (catches a second call, R16); the `NoRecall` row asserted as a matched row, with
  `adapter_unmapped_errors_total` at 0 (R16). Over loopback after S4, the recall and the NFSv4.0
  cases above, the listener's accept count 0 (catch a delegation to NFSv4.0, R12, a recall missing,
  R15). R17 is held by `DELEGRETURN`'s row, which T10.9's drain test runs.
- **Rules:** rule 9, RFC 21 §5.3's `decision:` on NFSv4.0 and D6's at `want`; rule 13, `want` and
  `delegation` pure and total.

### T10.5 `ChannelDown` in the service, and `Connect` clearing it

**Subtask T10.5 estimate: 2.25 man-days.**

- **Track:** A. **Depends on:** S2b.
- **Covers:** R29, R30, R33.
- **Deliverables:**
  - `pkg/vfs/service.go`: `Clients.ChannelDown(ctx context.Context, c Call, cl ClientID) error`, as
    RFC 17 §3.1 states it, and `OpChannelDown` with its name.
  - `internal/vfs/clients.go`: `ChannelDown` over open state's `ChannelDown` when the record's
    `Reach.Callback` names this node, the zero node ID on one node, and nothing otherwise;
    `StaleClient` once released. `Connect` of an NFSv4 client already recorded calls open state's
    `Rebind` with this node after `Connect` succeeds.
  - `pkg/openstate`'s `Rebind` writes only when the record's `Reach` changes: a reconnect whose
    reach already names this node and is up reads the record under `cstep` and commits nothing, so
    the reconnects after a restart do not queue a commit each behind the lock every client shares. The file's `decision:` keeps its zero node ID
    and loses its sentence that nothing calls `ChannelDown` or `Rebind`.
  - `vfstest.Mutations` recording it, `vfstest.Null` and `vfstest.Faults` passing it through.
  - `vfstest/suite`: RFC 17 §6's lost back channel row.
  Consumed by T10.6 and, through `memory.New`, every row after it.
- **Acceptance criteria:**
  - RFC 17 §6's row passes against `internal/vfs` over the memory store.
  - A `ChannelDown` from a record naming another node changes nothing.
- **Tests:** the suite row, at the service every adapter calls: after `ChannelDown`, `Record` shows
  `Reach.Down` and an `Open` asking for a read grant gets none; after `Connect`, `Record` shows it
  clear and the next `Open` gets one (catch a `ChannelDown` that sets nothing, R29, or a `Connect`
  that leaves it set, R30, R33); a record whose `Reach.Callback` names another node, written through
  the memory store, unchanged by `ChannelDown` (catches a report racing a rebind, R29); a
  `ChannelDown` after `Destroy` is `StaleClient` (R29); in open state's tests, a `Rebind` with the reach
  unchanged succeeding while a fault wrapper around the records store fails every client `Put`
  (kind 3), and one that changes it failing (catches a write per reconnect).
  No test double: the memory store is kind 2.
- **Rules:** rule 9, `internal/vfs/clients.go`'s `decision:` rewritten, and a `decision:` at the
  node check, which reads `Record` before open state's `ChannelDown` and so is not atomic with it,
  safe on one node only (§9, gap 5); rule 11, `ChannelDown` is RFC 17 §3.1's.

### T10.6 The path down on the wire

**Subtask T10.6 estimate: 3.5 man-days.**

- **Track:** A. **Depends on:** T10.3, T10.4, T10.5, T8.6.
- **Covers:** R2, R28, R31, R32, R35.
- **Deliverables:**
  - `callbacks.go`: when the client is left with no working back channel on this node (R28),
    `ChannelDown` for the client under the client's back-channel lock (D14), and the cached
    `ClientStatus` dropped (D5). The causes, each reaching one check after `Unbind`: the last bound
    connection's close, from its `OnClose`; E8's `DESTROY_SESSION` of the session holding it, whose
    session end this subtask makes `Unbind` each of the session's back-channel connections; E8's
    `BIND_CONN_TO_SESSION` re-binding a connection for the fore channel only, which `Unbind`s it
    from the back channel; and a silent back channel, which `Send` reports with `ErrChannelSilent`
    once every slot has been outstanding past the 35 s callback deadline on the server's clock, and
    `RecallGrant` or `RecallAny` then unbinds the session's connections from the back channel
    (without closing them, and releasing their `Keep`) and makes the `ChannelDown` before it
    returns the error, inside its own context's deadline (R4). A connection the server closes in
    `Stop` makes no `ChannelDown` (D14).
  - Under Q2's recommended answer, `ClientStatus.ChannelDown` in `pkg/openstate`, reported by
    `Renew` from `known[c].down`, and T8.6's renewal step keeping it with the cached `ClientStatus`
    behind the drop's generation (D5), and
    `ops/v4/sequence.go` setting `SEQ4_STATUS_CB_PATH_DOWN` while it is set.
  - The loopback row "§3.4 path down", with a delegation offered again after the re-bind.
  Consumed by E16.
- **Acceptance criteria:**
  - With the client's only back-channel connection closed, its next `SEQUENCE` on another connection
    carries `SEQ4_STATUS_CB_PATH_DOWN`, and an `OPEN` that would have been offered a delegation is
    offered none.
  - After `BIND_CONN_TO_SESSION` of a new connection for the back channel, the next `SEQUENCE` clears
    the flag, the next `OPEN` is offered a delegation, and its recall arrives on the new connection.
  - With two back-channel connections, closing one changes nothing.
  - The same flag and no delegation after each other cause of R28: `DESTROY_SESSION` of the only
    session with a back channel, `BIND_CONN_TO_SESSION` re-binding the only back-channel connection
    as fore channel only, and a client that leaves every recall on its live connection unanswered
    past the callback deadline.
- **Tests:** over loopback after S4 on `memory.New`, the scripted client holding two connections of
  one session (the layer that reads the flag and the offer): the close and the next `SEQUENCE`
  (catches `ChannelDown` never called, R28, or the flag not set, R31); the `OPEN` while down (catches
  a delegation offered with no path, R32); the re-bind, the `OPEN` after it and its recall (catch a
  path never cleared, R32, or a recall on the old connection, R2); the second connection kept
  (catches a path marked down while one connection lives, D2); the only back-channel connection
  closed with its `ChannelDown` held in the service by `vfstest.Faults`' delay (kind 3), a
  `BIND_CONN_TO_SESSION` sent on a new connection meanwhile, then the delay released: the next
  `SEQUENCE` carries no `SEQ4_STATUS_CB_PATH_DOWN` and the next `OPEN` is offered a delegation
  (catches a late `ChannelDown` landing after the rebind, R35). `vfstest.Mutations` shows one
  `ChannelDown` per close. R28's other causes, each asserted on the next `SEQUENCE`'s
  `SEQ4_STATUS_CB_PATH_DOWN` and the next `OPEN` offered no delegation: the `DESTROY_SESSION`, the
  fore-only re-bind, and the silent client, its clock advanced past the callback deadline with its
  slots unanswered (catch a path left up with no working back channel, R28, RFC 14 §5.4). `Stop`
  with a back channel bound: `vfstest.Mutations` shows no `ChannelDown` (catches a service write per
  connection on a planned stop, D14). No test double.
- **Rules:** rule 3, `nfs_cb_path_down_total` as §1.2 says; rule 9, D2's, D5's and D14's `decision:`
  markers.

### T10.7 Revocations

**Subtask T10.7 estimate: 3 man-days.**

- **Track:** A. **Depends on:** T10.4, T9.3, T9.7, T9.10, T8.6.
- **Covers:** R25, R26, R27.
- **Deliverables:**
  - `pkg/vfs/vfstest/memory`: `Service.Revoke(ctx context.Context, cl vfs.ClientID) error` over open
    state's `Revoke` (D13); the clock is T9.7's `Service.Clock`.
  - The loopback rows "§5.3 revoked delegation *(1 node)*", "§5.2 stateids *(1 node)*"'s
    recall-timeout step and "§5.1 administrator's revoke *(1 node)*", its NFSv4.0 steps over T9.9's
    `FreeState` on the client's behalf.
  Consumed by E16.
- **Acceptance criteria:**
  - A delegation whose recall the client never answers, the clock advanced past one lease period
    while the client renews: its stateid's `WRITE` and `TEST_STATEID` are `NFS4ERR_DELEG_REVOKED`, the
    next `SEQUENCE` carries `SEQ4_STATUS_RECALLABLE_STATE_REVOKED`, and after its `FREE_STATEID`
    neither.
  - An NFSv4.1 client holding an open, a lock and a delegation, revoked by administrator: its next
    `SEQUENCE` succeeds on the same session with `SEQ4_STATUS_ADMIN_STATE_REVOKED` and without
    `SEQ4_STATUS_RECALLABLE_STATE_REVOKED`; `READ` under the open's stateid and `TEST_STATEID` of the
    lock's and the delegation's are each `NFS4ERR_ADMIN_REVOKED`; after `FREE_STATEID` of each the flag
    clears and a new `OPEN` under the same client ID succeeds, with no `NFS4ERR_STALE_CLIENTID` and
    no retry loop: the scripted client sends each step once, and the server answers no step with an
    error that would make a client repeat it.
  - Over NFSv4.0, a `READ` under the revoked open's stateid is `NFS4ERR_ADMIN_REVOKED` once, then
    `NFS4ERR_BAD_STATEID`, and the client's reopen of the file under the same client ID succeeds.
  - Before the recall deadline, the clock advanced to just short of it: a `WRITE` under the
    delegation is served and its `TEST_STATEID` is `NFS4_OK`.
- **Tests:** the rows above over loopback after S4 on `memory.New`, the clock advanced through
  `Service.Clock` (kind 1) and the revoke through `Service.Revoke`, each asserted on the replies the
  scripted client reads: the revoked delegation (catches `NFS4ERR_BAD_STATEID` for it, or a flag
  never set, R26); the step short of the deadline (catches the adapter answering a revocation the
  service has not made, from a deadline or a recall count of its own, R25); the administrator's
  revoke (catches a session torn down, a delegation answered as a missed recall, a reopen refused
  `NFS4ERR_STALE_CLIENTID`, R27, or a v4.0 reopen refused). No test double.
- **Rules:** rule 14, the missing production caller of an administrator's revoke named in §9.

### T10.8 Locks during a recall, the grant budget and the operations not served

**Subtask T10.8 estimate: 2 man-days.**

- **Track:** A. **Depends on:** T10.4, T9.8, T5.8.
- **Covers:** R20, R21, R22, R24.
- **Deliverables:**
  - `ops/v4/getdirdeleg.go`: `GET_DIR_DELEGATION` answering `GDD4_UNAVAIL`, `will_signal_deleg_avail`
    false, with no service call (D9), and RFC 21 §5.3's `decision:` on directory delegations.
  - `WANT_DELEGATION`'s row in `ops/v4`'s members' table, pointing at E5's not-supported handler
    (D8, under Q3); both rows `Versions` 1 and 2, so NFSv4.0 meets `NFS4ERR_OP_ILLEGAL`.
  - The handler row of an NFSv4 lock met during a recall, and the loopback row of the grant budget.
  Consumed by E16.
- **Acceptance criteria:**
  - On `memory.New`, client A holding a write delegation: client B's `LOCK` and `LOCKT` over a range
    are each `NFS4ERR_DELAY`, A is sent `CB_RECALL`, and no waiter is left (B's `LOCK` after A's
    `DELEGRETURN` is granted at once, without a poll's reserved place).
  - A client opening 4096 files alone, each offered a delegation, receives one `CB_RECALL_ANY`
    keeping 2048, and its next `OPEN` is offered none.
  - `GET_DIR_DELEGATION` with a current filehandle answers `GDD4_UNAVAIL`, without one
    `NFS4ERR_NOFILEHANDLE`; `WANT_DELEGATION` answers `NFS4ERR_NOTSUPP`; neither leaves a
    `Mutations` entry.
- **Tests:** on `nfsharness` over `memory.New`, the lock cases at the `LOCK` and `LOCKT` replies
  (catch `NFS4ERR_DENIED` during a recall, or a waiter left, R24); over loopback after S4, the budget
  at the `CB_RECALL_ANY` the scripted client reads (catches a request never sent or a wrong count,
  R20); the two operations' replies against hand-written golden files (catch R21, R22). No test
  double.
- **Rules:** rule 9, D9's marker; rule 12, `WANT_DELEGATION` adds a row, not a handler.

### T10.9 The drain against Linux's compounds

**Subtask T10.9 estimate: 2.5 man-days.**

- **Track:** A. **Depends on:** T10.4, T3.4, T9.6, T9.9.
- **Covers:** R17, R34.
- **Deliverables:** `nfs/drain_test.go`: RFC 21 §11.6's drain test over loopback through `core` on
  `memory.New`, the node drained by `core.Server`'s `Stop` under a context that keeps it draining,
  with every single-node step of R34. Consumed by E16, whose totality test and suites run beside it.
- **Acceptance criteria:**
  - Every step of R34 holds, each compound sent by the scripted client as Linux sends it.
  - The compound begun before the drain, as RFC 21 §11.6 places it: `SEQUENCE`, `PUTFH`, `CLOSE`,
    `PUTFH`, `READ`, its `CLOSE` run and its `READ` held in the service by `vfstest.Faults`' delay,
    `Stop` begun, then released: the whole compound runs and is answered.
- **Tests:** the steps above at the replies the scripted client reads, and at `vfstest.Mutations`
  (catch a release refused while draining, R17, a results array cut short under `NFS4_OK`, a slot
  advanced by a compound that ran nothing, a `CLOSE` applied with its reply unsent, or a
  retransmission of the `CLOSE`, `READ` compound answered `NFS4ERR_BAD_STATEID`, R34);
  `vfstest.Faults`' delay is kind 3. The step with `NFS4ERR_MOVED` needs a gateway named by
  `MoveTarget`, which one node never has: the deferral ledger's row for the cluster's parts.
- **Rules:** rule 4, `adaptertest.CheckLeaks` after `Stop`; rule 14, the cluster step named.

## 7. Order of work

```mermaid
flowchart TD
  S2(["S2 NFS test foundations in CI"]):::sync --> T10_1["T10.1 Callback program's bytes"]
  T10_1 --> T10_2["T10.2 Back channel"]
  S4(["S4 NFSv4.1 session over loopback"]):::sync --> T10_2
  T10_2 --> T10_3["T10.3 Callbacks value's grant methods"]
  T10_3 --> T10_4["T10.4 Delegations offered, recalled, returned"]
  T10_4 --> C1(["after T10.4: delegations end to end"]):::sync
  S2b(["S2b amendments agreed"]):::sync --> T10_5["T10.5 ChannelDown in the service"]
  T10_3 & T10_4 & T10_5 --> T10_6["T10.6 Path down on the wire"]
  T10_4 --> T10_7["T10.7 Revocations"]
  T10_4 --> T10_8["T10.8 Locks in a recall, budget, unserved operations"]
  T10_4 --> T10_9["T10.9 Drain test"]
  classDef sync stroke:#2c5aa0,stroke-width:3px,stroke-dasharray:4 2
```

T10.1 to T10.4 are one path, each sending what the one above it built: the bytes, the channel, the
callbacks, the delegations they recall. T10.4 is the blocker of the rest. T10.5 waits for nothing
but S2b, which is met, and can run beside any of them; it sits before T10.6, its one consumer. T10.7
to T10.9 are tests over what T10.4 offers and can run in any order.

## 8. Suggested pull requests

| PR | Subtasks | Man-days | After | Why together |
| --- | --- | --- | --- | --- |
| 1 | T10.1 | 2 | — | the callback program's bytes and the call codec in `wire`, which E11's T11.2 waits on and which needs no session |
| 2 | T10.2, T10.3 | 7.5 | PR 1 | the server calling the client, from the back channel to the callbacks the service makes |
| 3 | T10.4 | 3 | PR 2 | delegations offered, recalled and returned end to end |
| 4 | T10.5, T10.6 | 5.75 | PR 3 | the path down, the service's half and the wire's; T10.5 can open beside PR 1 |
| 5 | T10.7, T10.8, T10.9 | 7.5 | PR 3 | the rows over delegations: revocations, locks in a recall, the budget, the drain |

A suggestion only: one pull request per subtask works as well; PR 4 and PR 5 can be open at once.

## 9. Open points

Questions pending the user's answer. Each recommended option is written in place above.

- **Q1. How `CB_RECALL` names its file** (D1). RFC 17 §3.2's `RecallGrant` hands a `CachingGrant`
  whose `File` is a `FileID`, and the adapter cannot mint a handle.
  - (a) **Recommended:** RFC 17 §3.2 amended, `RecallGrant(ctx context.Context, s SessionRef,
    openers []Principal, g CachingGrant, h Handle, to GrantKind) (sent bool, err error)`, `h` the
    grant's file, minted by the filesystem's sink with `entity.NewHandle`; SMB ignores it. The sink
    needs the grant's share for that, which `CachingGrant` gains from the open that made it (a
    FileID→share lookup per recall otherwise). Today it changes `pkg/openstate`'s `CachingGrant`,
    `pkg/vfs`, `internal/vfs/sink.go`, `vfstest.Callbacks`, `vfstest.CallbackFaults`, one test
    helper, `locks_test.go`'s `Args` index and about seven `vfstest` callback tests, 0.5 man-days
    of T10.3's figure; once the SMB adapter implements `Callbacks`, every change
    costs both adapters. knfsd encodes the handle from the delegation's file state (from memory).
  - (b) The callbacks value keeps a map from `GrantID` to the handle `OPEN` resolved, dropped at
    `DELEGRETURN`, revocation and the client's end. No RFC change, but it breaks RFC 20 §3.3's value
    that knows nothing about files (R9), and a back channel moved to another node finds no handle
    (cluster).
- **Q2. Where `SEQUENCE` reads `Reach.Down`** (D5). `ClientStatus` has no field for it.
  - (a) E8's renewal step reads `Record` with `Renew`, once per renewal interval, and this node's
    `ChannelDown` and re-registration drop the cache. One more store read per client per interval;
    another node's report up to 10 s late; and a renewal that read `Record` before this node's
    `ChannelDown` and stores after the drop hides the flag for an interval unless the drop raises a
    generation the store checks.
  - (b) **Recommended:** RFC 14 §11.3's `ClientStatus` gains `ChannelDown bool`, reported by `Renew`
    from open state's in-memory `known[c].down`, which `setReach` keeps and the grant offer already
    reads (`pkg/openstate/clients.go`, `grants.go`), with RFC 17 §3.2's `ClientStatus` paragraph
    amended. No extra I/O, exact on a node once the cache drop carries the same generation; a change
    to open state and two RFCs.
  - Under (a) or (b) `SEQUENCE` answers from a cached copy, and RFC 21 §11.4 ends "the adapter reads
    the record through the service on every decision that needs it and never answers from a copy it
    holds", so either needs a sentence in RFC 21 §3.4 allowing the flag one renewal interval stale
    from another node, in the same RFC 21 pull request as Q3's.
  - (c) `SEQUENCE` reads `Record` every time: exact and within RFC 21 §11.4 as written, one store
    read per compound.
- **Q3. `WANT_DELEGATION`'s answer** (D8). RFC 21 Appendix D says "answered as this RFC says", and
  RFC 21 does not say.
  - (a) **Recommended:** `NFS4ERR_NOTSUPP` through E5's one handler, with the sentence "`WANT_DELEGATION`
    is not supported: a client asks for a delegation with `OPEN`'s want flags" proposed for RFC 21
    §5.3 in batch two's one RFC 21 pull request, opened with E8's document pull request. RFC 8881
    makes the operation optional; knfsd does not
    serve it (from memory).
  - (b) Serve it as a want on an open the client already holds, through a new RFC 17 call, since
    `Open` is the only way a grant is offered; more service surface for an operation Linux does not
    send.

Gaps, not questions:

1. **An administrator's revoke has no production caller.** Open state's `Revoke` is reached by no
   `vfs.Service` method and no RFC 23 endpoint; T10.7 drives it through `memory.Service.Revoke`
   (D13). This pull request widens the deferral ledger's row "The `grace` and `nfs_clients` API
   endpoints" to an administrator's revoke of a client, with the same owner and trigger.
2. **"§5.2 delegation stateid" under Kerberos.** T10.4 runs it at the handler with two proven
   principals; the row under Kerberos over the wire is E16's T16.3, on the composed node with E13's
   `RPCSEC_GSS` client.
3. **The drain's `NFS4ERR_MOVED` step** needs a gateway `MoveTarget` names, which a single node never
   has: the deferral ledger's row "RFC 21's (cluster) parts", `fs_locations` naming another gateway.

4. **Only the client's own principal moves its path** (RFC 17 §3.2): T10.5's `Connect` calls open
   state's `Rebind`, which refuses a stranger only on a record with `Protect` set (`ErrNotYours`),
   not RFC 17's `ClientInUse` for a caller who neither owns the record nor holds a session under it.
   On one node `Rebind` never moves the path, which always names the zero node, so nothing is
   exposed; the refusal binds where a `Connect` moves the path to another node, under the deferral
   ledger's row "RFC 21's (cluster) parts", which this pull request widens to name it.
5. **T10.5's node check is not atomic with the write.** `internal/vfs` reads `Record`, then calls
   open state's `ChannelDown`, which takes no node (RFC 14's signature); a rebind through another
   node between the two would be marked down. On one node `Reach.Callback` is always the zero node,
   so it cannot happen; the same cluster row, widened in this pull request to name it, owns moving
   the check under open state's client step.
   A `decision:` marker at T10.5's check says so.

What this epic takes from other epics, settled with their documents:

- **E8:** T8.1's per-client callbacks value in `nfs/callbacks.go`, registered at `Connect`, holding
  the client's sessions on this node, its methods answering "not built" until their epic; T8.4's
  `BackChannel` on each session, recording the callback program, the credential D11 picks from
  `csa_sec_parms` (`AUTH_NONE` or `AUTH_SYS` and its body), the granted back-channel slot count and
  the bound connections, with `NewBackChannel`, `Bind` and `Unbind`, which T8.4's `CREATE_SESSION`
  and T8.9's `BIND_CONN_TO_SESSION` and `BACKCHANNEL_CTL` call; one type, which T10.2 extends rather
  than a second. Those three handlers call `Bind` before their `Connect`, which D14's ordering of
  a path-down before the rebind needs; E8 learns of no connection's close, which T10.2 takes from
  `Stages.OnClose` (D14). T8.9's `DESTROY_SESSION` ending a session and a `BIND_CONN_TO_SESSION`
  that re-binds a connection for the fore channel only reach `Unbind` through T10.6, which adds the
  call where E8's session end does not make it (R28). T8.6's renewal step caching `ClientStatus`
  once per renewal interval, which T10.6 extends under Q2.
- **E9:** T9.4's `OPEN` passing `OpenRequest.Want` and encoding T10.4's `open_delegation4`; T9.3's
  stateid codec giving a grant-typed stateid's `GrantID` and its `ErrState` (`Revoked`) row by
  cause; T9.9's I/O under a delegation stateid and its `FreeState` on an NFSv4.0 client's behalf;
  T9.10's `TEST_STATEID` and `FREE_STATEID`; T9.8's `LOCK` and `LOCKT`; T9.6's `CLOSE`; T9.7's
  `memory.Service.Clock`; the per-request drain roles of `WRITE`, `LOCK` and `OPEN`.
- **E11:** T11.2 uses `wire.AppendCall` and `ParseReply` from T10.1 (D12) and moves nothing.

Which pull request carries which RFC amendment: Q1's RFC 17 §3.2 `RecallGrant` goes in batch two's
one RFC 17 pull request, with E9's D1, E11's Q1 and E12's D1, opened before E9's PR 1; T10.3's
`pkg/vfs` change waits for it (rule 11). Q3's RFC 21 §5.3 sentence goes in batch two's one RFC 21
pull request, with Q2's sentence on the flag's staleness; Q2's recommended option (b) adds RFC 14
§11.3 and RFC 17 §3.2 to those.
