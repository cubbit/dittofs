# Epic 5: adapters v2 dispatch

**Sources:** [RFC 20](../rfcs/rfc-20-adapters.md) §3.1's drain filter, §4.1 (one pipeline), §4.2
(the operation table), §4.3's compound runner and reply budget, §4.5 (error tables: the type and
its mapping), §4.6 (held requests), §4.7's pipeline, runner, middleware, `Park` and `HoldCodec`,
§4.8's table selection, §6.3 (the cap of 64, the reply reservation), §6.4's request deadline,
§6.5's client cancel, §6.6, §6.9's request rows, §6.10's pipeline row, §10, and §9's A4, A6,
A7, A15, A16 and A17; [RFC 24](../rfcs/rfc-24-resources.md) §3.3, §3.4, §4.3 and §6.1's rule on held
requests; [RFC 17](../rfcs/rfc-17-vfs.md) §5.9's `WithHold`. Cited, not owned: RFC 20 §4.4's
expired-credential check, §5.3, §5.5's note on the reader and §6.8 (who holds a buffer), §6.1;
RFC 17 §4.3 (errors and their kinds); [RFC 25](../rfcs/rfc-25-observability.md) §8.2 (the
registry's calls); [RFC 22](../rfcs/rfc-22-smb.md) §6.3 (the re-run backoff).
**Track:** A.
**Epic 5 estimate: 17 subtasks, 30.5 man-days.**
**Depends on:** **S2, `testproto` and the import test in CI**: E1's frozen surface of `core`,
E2's `testproto` with its stateful half, `adaptertest.CheckLeaks`, the `adapter-conformance`
suite and its end-to-end specification, the `unit/adapter-allocs` cell. **E3**: the bodies of
`pkg/resources`, which every subtask that reserves needs. **E4**: `Server`, accept, the reader
and the writer of `Conn`, for the two subtasks that need a socket, T5.1c and T5.10; every other
subtask is tested at the pipeline's seam (D2, §7).
**Blocks:** E4's writer, which must pass the suite of `out` that T5.1a lands (D1); E4's drain,
close and cancellation tests, which run this pipeline; S3, which for this epic is T5.1c, and
through it E6's `New`; RFC 17's clone wait, which reads `vfs.WithHold`.

---

## 1. Goal

Epic 5 gives `core` its request path: one pipeline that takes a framed record from a connection
and hands the writer a reply, the operation and error tables it reads, the compound runner, the
requests it holds and parks, the middleware around every handler, and the metrics, spans and
logs of all of it.

**At the end of the epic:**

- `NewErrorTable`, `ErrorTable.Map`, `NewOpTable`, `Request.Park` and `Conn.Send` have their
  bodies, and `core` has its pipeline, runner, hold table and middleware chain, all unexported;
- with E4, a `testproto` request over loopback is framed, checked, run against `memory.New` and
  answered, and `TestConformanceEchoEndToEnd` is gone from `test/adapter/known.yaml`, which is
  then empty of this epic's entries;
- every chained, compound, held, parked, cancelled and draining case `testproto`'s stateful
  half scripts is asserted at the pipeline's seam, and what a socket decides over loopback,
  each test ending with `adaptertest.CheckLeaks`;
- `pkg/metrics` resolves a series once, `core` records RFC 20 §10.1's series the request path
  produces, and the pipeline's allocations per request are gated in `unit/adapter-allocs`;
- `pkg/vfs` has `WithHold`, and `*resources.Buf` can move its charge to the hold budget.

### 1.1 In scope

The bodies of `NewErrorTable`, `Map`, `NewOpTable`, `Park` and `Conn.Send`; `pipeline.go`,
`compound.go`, `middleware.go` and `metrics.go` in `internal/adapter/core`, and the hold table
in `hold.go`; `Stages.Errors`, `OpTable.Protocol`, `Op.MaxMembers`, `ErrDecode`,
`ErrCancelled` and `ErrReplyTooBig`, with their RFC amendments; the three registry calls in
`pkg/metrics`; `Buf.Recharge` in `pkg/resources`; `WithHold` and its reader in `pkg/vfs`; the
RFC sentences §4's decisions amend; the suite of `out` and the recorder that passes it (D2);
the record walk's fuzz target; the pipeline benchmark and its gate; the edits to the
program's deferral ledger and coverage table that §9 lists.

Eleven changes to `testproto`, each made by the subtask whose tests need it:

1. `Stages.Errors` set on both stage sets, and `Route` returning its table with its refusal
   where it has chosen one (T5.1a, D6).
2. `Decode` of a header returning the header with its error when its fixed fields decoded and
   its `next` is bad (T5.1a, D6).
3. Each table's `Protocol` set to `testproto` (T5.9, D18).
4. The compound row's `MaxMembers` set to 8 (T5.4, D10).
5. `Create`'s row given a `resolve` that refuses with `ErrNoSession` once its session has
   ended (T5.5b).
6. The hold row's bound read from a value a test sets on the `Protocol`, `HoldFor` its default
   (T5.5b).
7. `Park`'s reply carrying its name, its encoder refusing a name that does not fit the buffer
   it is handed with an error a new row maps (T5.11, D26).
8. A row for `ErrDecode`, with a status of its own, in every table (T5.7, D7); the row that
   names `context.Canceled` naming `ErrCancelled` instead (T5.2a, D17); a row for
   `ErrReplyTooBig`, with a status of its own (T5.4, D11).
9. `Unwrap` returning its header with its error on a bad checksum, so the refusal is answered
   and the connection kept (a related member with none before it still returns none, which
   closes the connection); the reply to it encoded under no session, and the client accepting
   it unsealed (T5.1a, D6).
10. A router built by `NewConn(nil)`, as `testproto`'s own tests build it, negotiating up to
    version 2 without calling `Conn.Settings`, whose body is E4's: the seam has no `Conn`
    (T5.1a, D2).
11. `Session` calling `Conn.Established`, and `Conn.Keep` on `Msg.Keep`, only on a non-nil
    `r.Conn`, as change 10 does for `Settings`: both bodies are E4's, and at the seam every
    `Request.Conn` is nil (T5.2b, D2).

### 1.2 Out of scope

- `Server`, listeners, accept and its limits, the reader, the writer and its queue, the read,
  write, idle and handshake deadlines, `Conn`'s fields, `Established`, `Keep` and `Settings`,
  the drain's deadline, asking the service for a draining request's move target: E4. D1 is the
  seam between the two epics.
- **`Conn.Close`'s body: E4.** It orders the reader's stop, the context's cancelling, the
  pipeline's close, `OnClose` and the writer's stop, and all but one of those are E4's. The
  pipeline's close that it calls is T5.1a's, which T5.5b and T5.11 extend to holds and parks
  (§9).
- `adapter_connections`, `adapter_connection_closes_total` and `adapter_bytes_total`: E4, whose
  accept, reader and writer count them; the ledger's row
  "`dittofs_client_connections_active{protocol}`", retargeted, and the row beside it (§9).
- Each protocol's error-table rows, its totality test and the helper that test calls (D5): the
  ledger's row "each protocol's error-table rows and its totality test (RFC 20 §7.3), with the
  helper it calls".
- An NFSv4.1 session's reply budget (`ca_maxresponsesize`, RFC 20 §4.3) and the bound by a
  quarter of the slots on calls waiting under `WithHold` (D11, D16): the ledger's row
  "RPCSEC_GSS context table and window; NFSv4.1 slots and reply cache", which now names both.
- `vfs.WithRequestID`: the ledger's row "`vfs.WithRequestID` (RFC 17 §4.7), set from the
  protocol's retry identity". A single node sets no request ID.
- `adapter_translation_refused_total`, `adapter_record_decode_failures_total` and
  `adapter_record_replays_total`: the ledger's new row of that name, owners the RFC 21 and
  RFC 22 plans. The conditions and alert rules of RFC 20 §10.1: E7.
- Checking a `Spec`'s name and labels against RFC 25 §2: the ledger's row "`metrics.Registry`
  an alias of `pkg/metrics`' `Metrics`".
- The one-call rule read per handler and handler tests: the ledger's row "handler tests on
  `memory.New` and `vfstest.Faults` (§7.2)". Codecs: the row "operation codecs, their golden
  vectors". Turns at the reply budget: the row "RFC 24's turns, `Reclaim`, `ShareCap`,
  `Detect`, `Split`".
- Fuzzing the codecs: `testproto`'s frame and header are E2's, the frame cut is E4's, each
  protocol's codecs are its plan's (the ledger's row "operation codecs, their golden
  vectors"). The record walk `core` does itself, by each header's `Next` and each member's
  byte counts, is fuzzed here (T5.4, R93).

### 1.3 Names used here

- **E1**, epic 1, delete v1 and fix the surface: `core`'s exported types with stub bodies.
- **E2**, epic 2, test foundations: `testproto`, the import test, the leak check, the
  `adapter-conformance` suite and the `unit/adapter-allocs` cell.
- **E3**, epic 3, budgets and buffers: the bodies of `pkg/resources`.
- **E4**, epic 4, connections and lifecycle: `Server`, accept, `Conn`'s reader and writer,
  drain, `Reconfigure`. Track B. It has no breakdown document yet.
- **E5**, this epic.
- **E6**, epic 6, protocol scaffolds: `wire/`, `protocol/` and `New` for NFS and SMB.
- **E7**, epic 7, integration with the node.
- **S1**, surface frozen: after E1, `core`'s exported surface changes only with an amendment
  of RFC 20 §4.7.
- **S2**, `testproto` and the import test in CI: after E2.
- **S3**, `core` serves `testproto` end to end: after E4 and this epic's T5.1c, the one
  subtask this document means wherever it says S3. T5.10's gate comes after it.
- **Track A, B, C**: the three lines of work through the middle epics. This epic is track
  A's. The program's split lets track C, while it waits for S3, take three of this epic's
  subtasks that need only E1's types and `testproto`: T5.6, T5.7 and T5.9. Each says so.
- **The deferral ledger**: the list of everything RFC 20 or develop's v1 provides that the
  branch does not serve yet, each row naming the owner that brings it back.
- **The RFC 21 plan, the RFC 22 plan**: the implementation plans of the NFS and SMB protocol
  RFCs, which own the rows the ledger gives them.
- **Seam test**: a test in `package core_test` that builds a pipeline through `hooks_test.go`
  with a `link` whose `out` is the recorder (D1, D2), hands records to `serve` as the reader
  would, reads the replies with `testproto.Client`, and ends with `CheckLeaks`.
- **Loopback test**: a test in `package core_test` that starts a `core` server on `testproto`
  over `127.0.0.1:0`, drives it with `testproto.Client`, and ends with `CheckLeaks`.
- **Needs E4**: a field each subtask of §6 carries beside **Depends on**, since E4 has no
  subtask ids to name yet: what of E4 must have landed before the subtask can.

## 2. How the general rules apply

Each of the program's ten general rules for an epic
([`rfc-20-adapters-plan.md` §1.5](rfc-20-adapters-plan.md#15-general-rules-for-each-epic)),
cited by number and title, and how it applies here or why it does not.

| Rule | Subtasks | How |
| --- | --- | --- |
| rule 1, independent unit tests, under `-race` ([`CLAUDE.md`, Writing tests](../../../CLAUDE.md#writing-tests); [Verifying a change](../../../CLAUDE.md#verifying-a-change)) | all | every test is in `package core_test` and imports no protocol but `testproto`. Each part is tested once, at the pipeline's seam: a record handed to `serve`, its replies read from a recorder in `out`'s place that passes the suite E4's writer must pass too, a verified implementation and not a stand-in (D1, D2). What a socket decides is asserted by T5.1c's loopback tests, on E4's real `Conn`. Errors, delays and holds come from `vfstest.Faults` over `memory.New`. Time is an input (D24): no test sleeps |
| rule 2, fuzz tests where relevant | T5.4 | the record walk `core` does itself reads a peer's bytes: each chain member's offset is the `Next` of the header before it, and each compound member's the byte counts `ReadOp` and `Decode` returned. `FuzzRecordWalk` holds R93 over arbitrary records. The frame and the codecs are E2's and the protocols' (§1.2) |
| rule 3, observability follows RFC 25 and RFC 20 §10 | T5.9, T5.6, T5.1b, T5.5b | T5.9 resolves every series once per server and table; each later subtask records its own through it and reads it back in its tests; spans are T5.1b's (request) and T5.6's (operation), a held request's attempts T5.5b's; `testproto` records nothing |
| rule 4, leaks and allocations | all seam and loopback tests, T5.10 | every seam and loopback test ends with `adaptertest.CheckLeaks` over all four budgets; T5.10 checks in the pipeline's allocations per request and registers the gate in `unit/adapter-allocs` |
| rule 5, v2 ignores v1 | all | nothing is lifted: RFC 20 Appendix B marks none of v1's dispatch keep |
| rule 6, nothing deferred is lost | T5.7, T5.4, T5.5b, T5.9 | the ledger rows §1.2 cites, each as §9's list of edits to the ledger leaves it |
| rule 7, code comment conventions ([`CLAUDE.md`, Code comments](../../../CLAUDE.md#code-comments)) | all | each decision of §4 leaves the marker it names at its code site; RFC 20's notes go at theirs. As `decision:`: parked requests off the hold floor, `Pends` asked only for an error the service returned, a call in the service pending past the ceiling and the floor, context and attribute reads served while draining, a drain's refusal answered by an interim with no final, the expired credential running only releases. As `ponytail:`: the cancel record's 5 s, `unwrap` on the reader (in `serve`, D8), the cap of 64, one goroutine per request, one reply reservation of a compound's summed bounds (D11) |
| rule 8, a test must be able to fail ([`CLAUDE.md`, Writing tests](../../../CLAUDE.md#writing-tests)) | all | each test in §6 names the change that fails it; the end-to-end specification fails today and passes only on E4's and this epic's bodies |
| rule 9, a symbol is exported only if RFC 20's interface uses it | T5.7, T5.2a, T5.3, T5.1a, T5.4, T5.5a, T5.9 | `core` gains three exported fields, `Stages.Errors` (D6), `OpTable.Protocol` (D18) and `Op.MaxMembers` (D10), and three errors, `ErrDecode` (D7), `ErrCancelled` (D17) and `ErrReplyTooBig` (D11); `pkg/resources` gains one method, `Buf.Recharge`, each with its RFC amendment and its golden file in `testdata/surface`; everything else in `core` is unexported. `pkg/vfs` and `pkg/metrics` are outside RFC 20's surface: their additions are RFC 17 §5.9's and RFC 25 §8.2's |
| rule 10, each subtask creates only what it declares | all | no totality helper (D5), no session reply budget (D11), no request ID, no series this epic does not record; a part built before the pipeline names the subtask that wires it |

## 3. Requirements and invariants

`Source` cites RFC 20 unless it names another RFC.

**The pipeline**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R1 | Every request passes the stages in §4.1's order: decode header, unwrap, admit, replay check, decode args, resolve IDs, the call, map error, encode, wrap, send. There is one implementation, parameterized per protocol by its stages; no protocol writes its own | MUST NOT | §4.1, §4.7, §9 A4 | T5.1a |
| R2 | A request refused before the call reaches no service operation; where its arguments decoded, its `Encode` still runs, handed no reply, and encodes the status alone | invariant | §4.1, §4.7 | T5.1a, T5.2b |
| R3 | A header or arguments that do not decode are answered with the protocol's own answer for them, and the request does not run (D6 says what a header must have decoded to be answered, D7 how the answer is forced to exist) | MUST | §4.1 | T5.7, T5.1a |
| R4 | A message `Unwrap` refuses is answered as the connection's table maps the refusal, a `NoReply` row included, and the connection is kept; where the protocol closes it instead (a message that fails decryption), it is closed through `Conn.Close` and answered nothing (D6 says how `Unwrap` says so) | MUST | §4.1, §4.7 | T5.1a |
| R5 | A reply `Lookup` returns is wrapped and sent, and nothing runs; an error it returns is mapped by the table; `nil, nil` runs the request. The encoded reply of a row marked `Cacheable` is handed to `Store` before `Wrap` | MUST | §4.1, §4.7 | T5.1a, T5.4 |
| R6 | A nil stage takes its default, filled once at accept so the pipeline never checks for nil: `Security` passes messages unchanged, `Replay` checks nothing, `Flow` is the fixed cap of 64 | MUST | §4.7 | T5.1a, T5.3 |
| R7 | `Hold` is required where a table holds and nil elsewhere: a nil one cancels nothing, and a table that holds is never run without it (D25) | MUST | §4.7 | T5.1a |
| R8 | The table is chosen by `Route` from the decoded header, before any argument is decoded; a version or dialect selects a table or a row, never a branch in a handler | invariant | §4.8, §4.7, §9 A17 | T5.1a, T5.2b |
| R9 | Requests decoded on one connection run concurrently, and their replies are sent as each completes, in any order; nothing serializes them beyond the members of one compound | MUST NOT | §6.1, §6.6 | T5.4 |
| R10 | Each admitted request runs on a goroutine of its own, with no worker pool; the reader blocks in `Admit` | invariant | §6.9, §6.6 | T5.1a |
| R11 | A request's goroutine never waits on a client: it hands its reply to the connection's writer and ends | MUST NOT | §6.1 | T5.1c |
| R12 | No lock of the adapter is held across the service call: the table the pipeline keeps per connection is locked for the lookup only | invariant | §6.6, §6.9 | T5.5b |
| R13 | The pipeline reaches the service only through the `Deps.VFS` its server was built with; nothing looks the service up while serving | invariant | §9 A16, §4.7 | T5.4 |
| R14 | Every pipeline test ends with every budget's reserved bytes at zero | test row | RFC 24 §9 (R3); §6.8 | T5.1a, T5.2b, T5.4, T5.8, T5.5b, T5.5c, T5.11, T5.1c |

**Tables and row checks**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R15 | `NewOpTable` builds a table and checks it: codes unique, ranges well formed, and no `ErrorRow.Hold` unless the table holds | MUST | §4.7 | T5.2a |
| R16 | An operation the table lacks is answered `ErrNotSupported` by the pipeline, before argument decoding | MUST | §4.2, §4.7 | T5.2b |
| R17 | An operation the operator switched off by name is answered as one the table lacks; no handler checks the setting | MUST | §4.2 | T5.2b |
| R18 | An operation outside its row's `Versions` is answered `ErrVersion`; the handler does not run | MUST | §4.2, §4.7 | T5.2b |
| R19 | A row's `Needs` is checked against the request's `Level` before the handler runs: the ordered precondition answers `ErrNotNegotiated`, `ErrNoSession` or `ErrNoTree`, a missing current filehandle `ErrNoFH`. No handler checks a precondition its row carries | MUST | §4.2, §4.7, §9 A15 | T5.2b |
| R20 | A request whose `Evidence` has expired is refused `ErrExpired` after `Resolve`, unless its row is marked `Op.Expired` or its role is `RoleRelease`. `RoleFlush` does not exempt a request from the expiry check, although the drain filter serves it | MUST | §4.4, §4.7 | T5.2b |
| R21 | `Invocation.Role` is called only while draining or on an expired credential, after `Resolve`; where it is nil `Op.Role` decides | MUST | §4.7 | T5.2b, T5.1a |

**Admission and the reply reservation**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R22 | Where a protocol supplies no `Flow`, a connection has at most 64 requests in flight, counted at admit; at the cap it is not read until a request completes | bound | §6.3, §6.9; RFC 24 §3.4 | T5.3, T5.1a, T5.1c |
| R23 | Once its arguments are decoded, a request reserves its reply bound from the reply pool before the service is called | MUST | RFC 24 §3.3; §4.2, §6.3 | T5.3, T5.1a |
| R24 | A reply reservation's wait ends at the call's deadline with an `ErrDelay` naming `DelayResources`, which the adapter builds itself and the table answers at once | MUST | RFC 24 §3.3; §6.3, §4.1 | T5.3 |
| R25 | A reply's reservation is released on every path: with the reply once the writer has sent or discarded it, and at once where no reply goes out | MUST | RFC 24 §3.3; §4.7 | T5.1a |

**Error tables**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R26 | `Map` takes the most specific row that matches, in a total order: a row whose `Err` is a typed value before one whose `Err` is a sentinel; then a row naming `Ops` and `Versions`, `Ops` alone, `Versions` alone, neither | MUST | §4.7, §4.5 | T5.7 |
| R27 | A row's `Err` is matched with `errors.Is`: a sentinel matches its whole class, a typed value its own kind only | MUST | §4.5, §4.7; RFC 17 §4.3 | T5.7 |
| R28 | `NewErrorTable` refuses two rows equal in that order that can match the same error, operation and version | MUST | §4.7 | T5.7 |
| R29 | An error with no row is answered the protocol's server-fault status, logged at error and counted in `adapter_unmapped_errors_total` | MUST | §4.5, §9 A7, §10.1 | T5.7, T5.1b |
| R30 | A `NoReply` row sends no final reply and frees the reply reservation; in a table that holds, an interim grants the request's credits, holds nothing and counts toward neither the pending ceiling nor the hold floor | MUST | §4.7, §4.6, §3.1 | T5.7, T5.1a |
| R31 | Expected refusals are logged at debug, unexpected errors at error | MUST | §4.5, §10.3 | T5.1b |

**The drain filter**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R32 | While draining, what a request does to state decides whether it runs: `RoleContext`, `RoleFlush` and `RoleRelease` are served, `RoleNone` gets the drain's error, `ErrMoved` or `ErrDraining`, decided per request (which of the two: E4, D23) | MUST | §3.1, §4.7 | T5.1a |
| R33 | A request already handed to the service when the drain begins runs to its answer, and its reply is sent | MUST | §3.1 | T5.4 |
| R34 | A request whose context was cancelled by a closed connection or the drain's deadline is never answered: its reply is dropped, and no error is encoded for it | MUST NOT | §3.1, §6.5 | T5.8 |

**The compound runner**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R35 | A compound runs as a sequence of handlers, one per operation, each its own service call; the runner keeps nothing between them but the compound state in the `Request`, and holds no lock across two | invariant | §4.3, §9 A6 | T5.4 |
| R36 | A compound past its protocol's operation cap is refused `ErrTooLong` before any of its operations is decoded or run | MUST | §4.3, §4.7 | T5.4 |
| R37 | The runner decodes exactly `Count` members, each's code read with `ReadOp` and its arguments with that row's `Decode`, moving on by the bytes each returned; bytes left after the last are the compound's decode error, before any member runs | MUST | §4.7 | T5.4 |
| R38 | Outside a drain, a member that does not decode, or names a code its table lacks, is answered with that error once the members before it have run; the runner stops at the first failure, and the reply ends there | MUST | §4.3, §4.7 | T5.4 |
| R39 | Each operation's reply bound is reserved before it runs, not after (a session's own reply budget: the RFC 21 plan, D11) | MUST | §4.3; RFC 24 §3.3 | T5.4 |
| R40 | A chain's members come from one record, each with its header at its own offset; `Decode`, `Unwrap`, `Admit` and `Lookup` run per member, and so does everything from the row checks to the encoding. Each member's admission is released once its reply is encoded, before the next member is admitted (D8) | MUST | §4.7 | T5.4 |
| R41 | `Unwrap` and `Route` are handed the member before a related one, so the IDs it inherits are resolved before its check and its handles carry over | MUST | §4.7 | T5.4 |
| R42 | A related member after one that failed is answered with the same status and reaches no service call | MUST | §4.3 | T5.4 |
| R43 | A chain's replies are joined with `HeaderCodec.Chain`, and `Wrap` is called once on the joined record; a member `Unwrap` refused is not in it, and goes out in a record of its own (D6) | MUST | §4.7 | T5.4 |
| R44 | A chain member whose row is `NoReply` stops the chain: the members after it do not run; those before it stay applied and are answered, chained with the refused member's interim, and no final reply follows | MUST | §4.7 | T5.4 |
| R45 | That interim is handed the headers of the members after it, the only part of them decoded, in a slice taken from a `sync.Pool`; a later header that does not decode ends the list there | MUST | §4.7 | T5.4 |
| R46 | While draining, a single-reply compound is judged whole before any operation runs: each operation's role is `Op.Role`, or `Invocation.Role` over its decoded arguments, called before `Resolve`; an operation that does not decode counts as refused | MUST | §4.7, §3.1 | T5.4 |
| R47 | If any operation of it would be refused with a `NoReply` row, the compound runs nothing, answers nothing, and nothing is handed to `Store`, so a retransmission runs | MUST | §4.7 | T5.4 |
| R48 | The runner reads whether it is draining once per single-reply compound, before its first operation; a compound that began while serving runs whole | MUST | §4.7 | T5.4 |
| R49 | The drain error of a compound's refused members is decided once, from every filehandle its members' `Invocation.Handle` name, handed over together (the asking of `MoveTarget`: E4, D23) | MUST | §4.7, §3.1 | T5.4 |
| R93 | Every step of a record's walk moves strictly forward and stays inside the record: a chain member's `Next` lies past the offset of the header that names it and inside the record, a compound member's code takes at least one byte, and its code and arguments take no more than the bytes left. A step that does not is refused as a decode error (R3), and nothing past it is read | MUST | §5.2, §4.7 | T5.4 |

**Held requests**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R50 | A held request is a record and a timer: it holds no worker and no goroutine between attempts | MUST NOT | §4.6, §6.9; RFC 24 §6.1 | T5.5b |
| R51 | A held request keeps its receive buffer from frame until its final answer, across every re-drive (D12) | invariant | §5.3, §6.8, §4.7 | T5.5b |
| R52 | Every call carries `vfs.WithHold`. In a table that holds, it is the deadline of the row that maps `ErrDelay` of `DelayClone`, evaluated at arrival with the hold's start at now, and none where no row maps it; elsewhere it is the request's remaining deadline (D16) | MUST | §4.6; RFC 17 §5.9 | T5.5d, T5.5b |
| R53 | A request still unanswered a short interval after it reached the service, or answered an error whose row has a `Hold`, gets its interim from the adapter's own timer | MUST | §4.6 | T5.5b |
| R54 | The interim of a hold is handed no headers; only a `NoReply` row's interim is | MUST | §4.7 | T5.5b |
| R55 | Each attempt re-runs the held `Invocation`'s `Resolve` and `Call` with the same decoded arguments, under a fresh 30 s attempt deadline inside the hold's bound | MUST | §4.6, §6.4 | T5.5b |
| R56 | How long a request is held is its row's `Hold`, evaluated on each attempt's error from the hold's start: a deadline at or before the attempt's time answers `Status` now, and a row without `Hold` is answered at once. The closure is called afresh on each attempt, so a held request follows the live settings | MUST | §4.6, §4.7 | T5.5b |
| R57 | Held requests are counted per connection against a floor of 256 requests and 32 MiB, a parked request not among them; past the floor a hold reserves the call's whole receive charge from the hold pool and only then releases the receive reservation | MUST | §4.6; RFC 24 §4.3 | T5.5a, T5.5b |
| R58 | A request neither the floor nor the hold pool can take, whose hold starts on an error the service returned, is answered at once, holds nothing and is counted in `adapter_hold_refusals_total` | MUST | §4.6, §6.3, §10.1 | T5.5b |
| R59 | Every request pending on a connection, held past its interim or parked, counts toward `Stages.MaxPending`; it is checked when a hold starts on an error the service returned and in `Park`, and past it the request is answered `ErrTooManyPending` at once | MUST | §4.6, §4.7 | T5.5b, T5.11 |
| R60 | A call the interim timer finds still in the service is never refused: it goes pending past the ceiling, and past the floor with the hold pool full keeping its receive reservation; its interim then withholds its credits and its final reply grants the rest | MUST | §4.6, §4.7; RFC 24 §4.3 | T5.5b |
| R61 | Every request of a table that holds is keyed by `HoldCodec.Key`; its interim passes `Security.Wrap`, and its final reply goes out under `Final`'s header | MUST | §4.7 | T5.5b |
| R62 | A held call holds no reply reservation between attempts; each re-run takes one as a new call would | MUST | RFC 24 §4.3 | T5.5b |
| R63 | `Cancels` is asked after `Unwrap`, so an unverified cancel ends no hold; a cancel gets no reply | MUST | §4.7, §6.5 | T5.5b |
| R64 | A client's cancel ends a hold between attempts, and the request is answered: the connection is live, so a final response is always sent. A running attempt is not cancelled: it finishes and answers with its own result | MUST | §6.5, §4.6 | T5.5b |
| R65 | A cancel that finds no hold or park with its key is remembered, and a request with that key that holds or parks meanwhile ends as cancelled | MUST | §4.7 | T5.5b |
| R66 | A cancel is remembered for at most 5 s, and at most `MaxPending` of them per connection, the oldest dropped first | bound | §4.7 | T5.5b |
| R67 | A held request keeps its request span across its re-drives, with one child per attempt | MUST | §10.2 | T5.5b |
| R68 | A chain member before the last, answered an error whose row holds, is answered as `Pends` says: pending, the replies before it and its interim going out as one chain and the members after it running once it has answered; or at once, the runner replacing the error with `ErrNotLast` before the table maps it, and the chain goes on. `Pends` is asked only for an error the service returned | MUST | §4.7 | T5.5c |
| R69 | The credits of chain members answered before a pending one ride on its interim and are granted in full | MUST | §4.6, §4.7 | T5.5c |

**Parked requests**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R70 | `Park` refuses a member before the last of a chain with `ErrNotLast`, a connection at `Stages.MaxPending` with `ErrTooManyPending`, a draining connection with the drain's error, and a reply pool that cannot take the small-class reservation with `ErrDelay` of `DelayResources`; nothing is sent | MUST | §4.7; RFC 24 §3.3 | T5.11 |
| R71 | Otherwise `Park` sends the interim at once, releases the receive reservation and keeps a small-class reply reservation, and no more, while the request waits: it holds no worker and no write data | MUST | §4.7, §4.6; RFC 24 §3.3 | T5.11 |
| R72 | A parked request's reply that would not fit that reservation is answered as its protocol answers an overflow (D26) | MUST | §4.7 | T5.11 |
| R73 | A parked request has no request deadline: it waits until `done`, a cancel, a drain or a close | MUST | §6.4, §4.7 | T5.11 |
| R74 | A request already pending past its interim gets no second one when it parks: its slot moves from held to parked | MUST | §4.7 | T5.11 |
| R75 | On `done` the reply is encoded, wrapped and sent under `Final`'s header | MUST | §4.7 | T5.11 |
| R76 | A client's cancel calls `cancel` first, and `done`'s error goes out as the protocol's cancel status; a `done(nil)` is answered success. A mutation applied after the cancel is never reported as not applied | MUST NOT | §4.7, §6.5 | T5.11 |
| R77 | The start of a drain calls `cancel` on every parked request, so none waits for the deadline; `done`'s error is replaced by the drain's and answered as its table maps it, and a `done(nil)` is answered success | MUST | §3.1, §4.7 | T5.11 |
| R78 | A connection that is closed or closes, `Close` included, ends every hold and park: `cancel` is called, the adapter stops waiting, and the reply is dropped | MUST | §4.7, §6.5, §4.6 | T5.5b, T5.11, T5.1c |

**Middleware and the request context**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R79 | The cross-cutting work is one chain, built once by `core` in one fixed order: recovery, trace span, metrics, deadline; a test pins the order | MUST | §4.7, §4.2 | T5.6 |
| R80 | A handler that panics is answered the protocol's server-fault status, logged at error with the operation and the stack, counted in `adapter_handler_panics_total`, and does not take down the connection's other requests | MUST | §4.2, §10.1 | T5.6, T5.1a |
| R81 | Each attempt of every service call carries a deadline of a fixed 30 s from arrival, set by the pipeline in the context | MUST | §6.4; RFC 17 §4.3 | T5.6, T5.8 |
| R82 | A request has one context, derived from its connection's, so closing the connection cancels it | MUST | §6.9, §6.5 | T5.8 |
| R83 | The service's answer at the deadline is mapped as any other | MUST | §6.4 | T5.8 |

**Observability**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R84 | Every metric, span and log key of the request path is recorded by `core`; a protocol supplies only names, through its tables | MUST | §10 | T5.9 |
| R85 | The request path produces `adapter_requests_in_flight`, `adapter_refusals_total` by stage, `adapter_replay_hits_total`, `adapter_wire_seconds` by stage in the `request` bucket set, `adapter_handler_panics_total` by operation, `adapter_unmapped_errors_total`, `adapter_status_mapped_total` by error, `adapter_held_requests`, `adapter_held_bytes`, `adapter_hold_pool_bytes` and `adapter_hold_refusals_total`, each labeled by `protocol` but the hold pool's | MUST | §10.1 | T5.9, T5.1b, T5.6, T5.5b |
| R86 | No series carries a client, principal, share or path label | MUST NOT | §10.1 | T5.9 |
| R87 | Every series a request can touch is resolved once, per protocol and per operation, so recording on the request path is an atomic add or a histogram observe, with no label lookup and no allocation (D18 says when) | bound | §10.1, §6.9 | T5.9, T5.10 |
| R88 | The request span is opened when a record is framed and ended when its reply is handed to the writer, or when it is dropped; the sampling decision is made there, once | MUST | §10.2 | T5.1b |
| R89 | A compound has one child span per operation, opened by the trace middleware; a single-operation request has none, and a handler opens no span | MUST | §10.2, §4.2 | T5.6, T5.4 |
| R90 | A span's attributes are the closed-set fields: protocol, version or dialect, operation, status, the neutral result, request and reply bytes, the stage that refused; never a path, a name, a principal or a client address | MUST NOT | §10.2 | T5.1b, T5.6 |
| R91 | Every request-scoped log line carries the request's trace ID, sampled or not; a misbehaving client is logged once per connection, not once per request | MUST | §10.3 | T5.1b |
| R92 | The whole pipeline over loopback against a null service has its allocations per request asserted with `testing.AllocsPerRun` at or under a checked-in number that may only fall, in a non-race cell; with a request unsampled the span calls allocate nothing and read no clock beyond the trace ID | bound | §6.10, §10.2, §6.9 | T5.10 |

What already holds, and is not numbered again here: `Bind` returns the row's `Decode` (E1's
test); a row sees the error, the operation and the version and nothing else (`Map`'s
signature); `Errors()` lists `core`'s refusals (E1). What other epics own of these sections:
the frame stage, the receive reservation and the writer's send (E4); which drain error a
refused request gets, the drain's deadline and the cancelling at it (E4); the connection,
close and byte series (E4); every protocol row, its statuses, which errors hold and for how
long, and that an `ErrDelay` is mapped by its cause (the RFC 21 and RFC 22 plans); that every
service error has a row (those plans, D5); the one-call rule (§1.2).

## 4. Decisions this epic takes

- **D1, the seam with E4.** RFC 20 §4.7 puts no interface between a connection and the
  pipeline, and names neither side. A `Conn` holds one `*pipeline`, and the pipeline is handed
  everything it reads of its connection, so it reads no field of `Conn`. `decision:` on
  `pipeline`. What changing it costs once both epics build on it: signatures in one package
  both own. No prior art sought: it is the split of one package between two owners. The
  architect agreed it on #4184, with two conditions: the points below go into epic 4's issue,
  and `close()` never blocks, with a test (T5.1a's `TestCloseNeverBlocks`, and T5.1c's on E4's
  `Conn`).

  **This epic owns:**
  - `withDefaults(st Stages) (Stages, error)`: R6's and D25's defaults, called at accept on
    what `NewConn` returned; the error refuses stages that lack `Header`, `Router` or
    `Errors` (D6), and accept then closes the connection before it reads.
  - `newShared(d adapter.Deps, reply *resources.BufPool) *shared`, once per `Server`: the
    series by table (D18), the reply pool, the hold budget, the clock (D24).
  - `newPipeline(sh *shared, st Stages, l link) *pipeline`, once per connection at accept.
    `link` is what the pipeline reads of its connection: `ctx`, the connection's context;
    `remote netip.Addr`, the budgets' key; `set settings.Settings`, the snapshot loaded at
    accept (D22); `out func(msg *resources.Buf, key, before uint64) error`, the writer's
    enqueue; `close func(err error)`, `Conn.Close`.
  - `(p *pipeline) serve(rec *resources.Buf)`, called by the reader only, one record at a
    time.
  - `(p *pipeline) drain()`, `(p *pipeline) wait(ctx context.Context) error`, which returns
    nil once nothing runs, is held or is parked, and `(p *pipeline) close()`.
  - The bodies of `Conn.Send`, which wraps the message and hands it to `out` with its
    `before` (T5.1c), and of `Request.Park`.
  - The suite `out` must pass, and the recorder that passes it (T5.1a, D2).

  **E4 owns:**
  - `Server`, accept, the reader, the writer, and every field of `Conn`.
  - The bodies of `Conn.Close`, `Established`, `Keep` and `Settings`.
  - The calls: `withDefaults`, `newShared`, `newPipeline` with the connection's `link`, the
    writer's enqueue as its `out` and `Conn.Close` as its `close`, `serve`, `drain`, `wait`,
    `close`.
  - A writer that passes the suite of `out`.

  **The points**, fixed in code by the suite of `out` T5.1a lands. The architect agreed the
  record, `out`, the close order, `close()`, `OnClose`, `drain()` and the reply pool (#4184,
  13 Nov). The link, the queue's bound and `Conn.Settings` and `Conn.Established` needed only
  from T5.1c came after his answer, for the seam tests D2 allows, and wait for his
  confirmation (§9):
  - **The record.** `serve` owns `rec`'s one reference and releases it exactly once, on every
    path; it blocks only in `Admit`.
  - **The link.** What the pipeline reads of its connection is handed to `newPipeline`, never
    read from `Conn`: the context every request's context derives from, the remote address as
    a `netip.Addr`, the settings snapshot, `out`, and `Conn.Close`, which the pipeline calls
    where D6, D18 and D25 close the connection.
  - **`out` never blocks its caller.** It takes `msg`'s reference whatever it returns, `msg`
    already wrapped, and fails only once the connection is closed. `key` is the request's
    `HoldCodec.Key` in a table that holds, else 0; `before` is `Conn.Send`'s, else 0; a message
    handed with `before` goes out ahead of the reply of that key.
  - **The queue behind `out` is bounded** (RFC 20 §6.2), by messages and by bytes, and the
    bound counts every message `out` takes: replies, interims and `Conn.Send`'s alike. At the
    bound the reader stops reading the connection until the queue drains below it (RFC 20
    §6.1). `out` still takes what it is handed past the bound: the replies and interims of
    requests already admitted or pending, at most one of each per request, and `Conn.Send`'s
    callbacks, which recall caching grants the open-state pool bounds (RFC 24 §2.3, §5.1),
    until the write deadline closes the connection (RFC 20 §6.4).
  - **Close order**, idempotent: the reader stops, the connection's context is cancelled,
    `close()` is called, E4 calls `wait()`, then `OnClose`, then stops the writer.
  - **`close()` never blocks**: it starts the teardown (every hold and park ended, their
    replies dropped) and returns. So `Conn.Close` may be entered from a handler's own
    goroutine, and from the reader when `Unwrap`, which runs there (D8), says close (D6);
    neither waits on itself. What waits is E4's teardown, off both goroutines.
  - **`OnClose`** is called by E4 only after `wait()` has returned, so it never drops state a
    running handler still uses.
  - **`drain()`** is called by E4 on every connection's pipeline as draining begins, and on a
    pipeline built after that before its first `serve`; it sets that pipeline's draining
    state and cancels its parks. At the drain's deadline E4 closes each connection; there is
    no separate call to abandon.
  - **`Conn.Settings()` and `Conn.Established()`** have their bodies before T5.1c:
    `testproto`'s router calls the first on a `Negotiate`, and its `Session` the second, over a
    real `Conn`. At the seam neither is called (§1.1's changes 10 and 11). `Keep` is called only
    on `Msg.Keep`, which no test of this epic sets.
  - **The reply pool**: `Server` builds one `BufPool` over the reply budget (RFC 20 §6.8) and
    hands it to `newShared`.
- **D2, each part is tested once, at the pipeline's seam; what a socket decides is tested on
  E4's `Conn`.** With D1's `link`, a test builds a pipeline through `hooks_test.go`, hands
  `serve` the records a reader would, and reads the replies from a recorder in `out`'s place,
  with the clock injected (D24) and no socket. The recorder is a verified implementation, not
  a stand-in: T5.1a lands the suite of `out` first (it never blocks, also past its bound; it
  takes the message's reference whatever it returns; it fails only once closed; a message
  handed with `before` goes out ahead of the reply of that key), the recorder passes it, and
  E4's writer must pass it too, which fixes D1 in code. The recorder queues what it takes and
  writes it, in order, to one end of a `net.Pipe`, from which `testproto.Client` reads, so a
  test reads statuses, checksums and balances as a client does. The four behaviors only a
  socket decides are T5.1c's, on E4's real `Conn`: admission pausing reads, `Send`'s `before`
  against a queued reply, a slow or closed client, and the end-to-end specification; the
  pipeline benchmark, which RFC 20 §6.10 runs over loopback, is T5.10's. `decision:` at the
  head of `hooks_test.go`. Prior art: the standard library's `export_test.go` files; the name
  differs because `exports_test.go` here is the surface test.
- **D3, a table that fails its checks panics at build.** `NewOpTable` and `NewErrorTable`
  return no error, and a table is program text. The panic names the row. `decision:` at each.
  Prior art: `http.ServeMux.Handle` panics on a conflicting pattern and
  `regexp.MustCompile` on a bad expression.
- **D4, a typed row is one whose `Err` has an `Is` method.** RFC 17's typed values implement
  `Is(error) bool` to match their sentinel and their kind; a sentinel is a plain value. Two
  rows can match the same error when either's `Err` matches the other's under `errors.Is`.
  Rows are sorted once at build and `Map` scans them: errors are the slow path. A nil `op`
  matches only rows that name no operation. `decision:` at the sort. No prior art found for
  ranking error matchers.
- **D5, no totality helper here.** A helper that checks a table against `vfs.Errors()` and
  `core.Errors()` has no caller in this epic or in E6: `testproto`'s tables are not total by
  E2's decision, and E6's scaffolds answer every operation not supported, their other rows
  being the protocols'. **It is built with the first protocol table that must be total**, as
  the architect agreed. What this epic settles for it: an unmapped error is observable from
  outside `core`, since a table built over the same rows with a fault status no row uses
  answers that status from `Map`. No marker: nothing is written. No prior art sought: the
  question is which epic has the first caller.
- **D6, an error raised before a table is chosen is mapped by `Stages.Errors`, and `Unwrap`
  can say close.** `Unwrap`, `Admit` and `Lookup` can fail before `Route` runs, `Route` can
  refuse without a table, and `Map` needs a table. Four conventions on bodies and one
  additive field, no signature changed:
  - **`Route` returns its table together with its error whenever it has chosen one**, and
    the pipeline maps the error through that table, with no operation.
  - **`HeaderCodec.Decode` returns a non-nil `Header` with its error when enough decoded to
    address a reply.** With a nil `Header` nothing can be answered: the connection is closed
    and the close counted.
  - **`Security.Unwrap` says close the same way, with a nil `Header` and its error.** The
    pipeline answers nothing and closes the connection through `Conn.Close` with that error.
    That is a message that fails decryption: MS-SMB2 3.3.5.2.1.1 disconnects, and RFC 20 §4.1
    and RFC 22 §4.1 already close the connection for it, and an encrypted message whose inner
    header did not decrypt has no message ID to answer on. With a `Header` and its error the
    refusal is answered and the connection kept: SMB's bad signature, `STATUS_ACCESS_DENIED`
    (MS-SMB2 3.3.5.2.4); RPCSEC_GSS's failed verifier, `RPCSEC_GSS_CREDPROBLEM` (RFC 2203
    §5.3.3.4.2), and failed integrity check (§5.3.3.4.2) or privacy check (§5.3.3.4.3),
    `GARBAGE_ARGS`, in RFC 5531's reply formats; a sequence number below the window or already
    seen, dropped through a `NoReply` row (RFC 2203 §5.3.3.1, RFC 21 §10).
  - **`Stages.Errors *ErrorTable` answers `Unwrap`, `Admit`, `Lookup`, a header returned with
    its error, and a `Route` with no table.** A `NoReply` row there drops the message. **The
    field is required, as `Header` and `Router` are**: `withDefaults` refuses stages without
    it, before the connection reads (D1). Of the two ways to keep R4, this is the simpler: no
    second amendment of §4.1, whose refusal table already says which `Unwrap` refusals close.
  - **`Route` is never called after an earlier stage refused**, so no unverified header
    reaches it.
  - **The reply to a header that failed `Decode` or `Unwrap` is not passed to `Wrap`.** It
    goes out as encoded, never signed or sealed under a session the header merely names.
    RFC 20 §4.7 gives `Wrap` "sign or encrypt, in place" under `h`, and §5.5 keys each
    transform by the session or context; neither says what `h` is when `Unwrap` returned
    none, so this is an addition to both, listed in §9. A refusal after `Unwrap` succeeded
    (`Admit`, `Lookup`, `Route`) is wrapped under the header `Unwrap` returned.
  - **In a chain, a member that fails `Unwrap` splits its replies.** The replies of the
    members before it are joined and wrapped once (R43) and go out first; its refusal goes out
    after them in a record of its own, not wrapped, with the replies of the related members
    after it, each answered its status with no call (R42); the next unrelated member starts a
    new record, joined and wrapped once with those after it, and the chain runs on. Every
    member is answered, so its credits are granted and no message ID waits out the client's
    timeout (RFC 22 §4.1). MS-SMB2 3.3.5.2.7.1 has each unrelated member handled on its own
    and lets their replies go out separately; 3.3.5.2.7.2 compounds a related chain's
    replies as a SHOULD. A single `Wrap` of a record that held the refusal would sign or seal
    it under the session the refused member names, which may be another than the first
    member's: the oracle this decision exists to close. With a nil `Header` the connection
    closes and nothing more is sent.

  It amends RFC 20 §4.7 (the `Stages` struct, the comments on `Decode`, `Unwrap`, `Route`
  and `Wrap`, the pipeline sketch, and the chain's single `Wrap`), §4.1's refusal table (a
  header with nothing to address closes the connection), §5.5 (the unwrapped reply, and
  `Unwrap`'s nil header closing the connection), and `core.golden`. `decision:` in `serve`,
  and in the runner for the chain. No prior art sought: the gap is in this surface's own
  order of stages.
- **D7, a decode error is wrapped in `ErrDecode`, and every table must map it.** RFC 20 §4.1
  answers it "the protocol's garbage-args or invalid-parameter answer", and a row nothing
  demands would let any client's garbage be answered as a fault and raise
  `adapter_unmapped_errors_total`, which RFC 20 §10.1 alerts on as a bug. **The pipeline wraps
  the error of `HeaderCodec.Decode`, of a row's `Decode`, of a compound member's and of a
  step R93 refuses in `ErrDecode`, a new member of `Errors()`; `NewErrorTable` refuses, at
  build (D3), a table with no row that maps it for every operation and version: a row naming
  neither `Ops` nor `Versions`.** A header's decode error is mapped with no operation (D6),
  which a row naming `Ops` never matches, and a row naming `Versions` misses outside its range,
  so either would still let a client reach the fault status. A table cannot be built without
  the row, so no totality helper is needed for it, and the protocols' totality tests demand
  it as well. It lives in `core`'s `errors.go` because the pipeline is what wraps and every
  table of every protocol must name it; a protocol's own value could not be demanded by
  `core`. A protocol that wants two answers names its own value in a typed row, which wins
  (R26). It amends RFC 20 §4.7 (`Errors()`, `NewErrorTable`) and `core.golden`. `decision:` at
  the check. No prior art sought: the choice is between a row nothing demands and one a build
  demands.
- **D8, the reader runs a record's first message as far as `Admit`.** `serve` decodes the
  first header, unwraps it, asks `Cancels` and blocks in `Admit` on the reader, so a
  connection at its bound is not read; everything after runs on the request's goroutine, the
  later members of a chain included, each admitted there. One record is one goroutine: a
  chain's members are serialized by the protocol. **Each member's admission is released once
  its reply is encoded, before the next member is admitted**, not at the join: a chain holds
  one place under `Flow` at a time, so a chain longer than the cap of 64 (nothing caps a
  chain's length but its record's) never waits on its own members under a context with no
  deadline. It amends RFC 20 §4.7's chain paragraph. `decision:` in `serve`, beside RFC 20
  §5.5's `ponytail:` for `unwrap` on the reader. Prior art: the standard library's `net/http`
  server reads on the connection's goroutine and runs HTTP/2 handlers on their own (from
  memory).
- **D9, every reply buffer is taken from the reply pool, the small class included.** RFC 24
  §3.3 charges a small reply to the receive reservation's overhead, and `BufPool` hands out no
  buffer without a reservation. **The pipeline calls `Get` on the reply pool for every reply,
  of the bound plus the header.** A small request is then charged 12 KiB twice while it runs.
  `ponytail:` at the call: at 64 requests in flight a connection over-reserves 768 KiB; give
  `BufPool` an unreserved small buffer when the reply budget is shown to bind before the
  receive budget does. RFC 24 is amended in three places that rely on a small reply riding
  the receive charge: §3.3's paragraph on the small class, §3.3's paragraph on `Park` ("which
  a small reply was charged to until then"), and §4.2's "needs only a small reply (§3.3), so
  no reply wait can refuse it". E3's second pull request also edits §3.3's charging text, so
  this amendment lands after it. No prior art sought: the question
  is which budget one buffer is charged to.
- **D10, the compound cap is a column of the compound row.** RFC 20 §4.7 has the runner
  refuse a count past the cap, and no field of the frozen surface carries the cap.
  **`Op` gains `MaxMembers int`, read by the runner, which refuses a `Count` past it with
  `ErrTooLong` before it decodes any member, as §4.7 words it; `NewOpTable` refuses a row
  with `Members` and no `MaxMembers`.** A cap checked in each compound row's `Decode` is a
  check a protocol can forget, the drift R1 forbids. It amends RFC 20 §4.7 (`Op`) and
  `core.golden`. `decision:` at the field. No prior art sought: the cap joins the row's
  other columns.
- **D11, the runner reserves the reply; it does not know a session's reply budget.** Every
  member of a single-reply compound is decoded before any runs (R37), so every bound is known:
  **the runner takes one reply buffer of the bounds' sum before the first member runs**, which
  meets "before each operation, not after". **A sum the reply pool can never hold is
  answered `ErrReplyTooBig`, a new member of `Errors()`, at once**, the compound refused whole
  before any member runs, as a count past its cap is. `WaitBudget.Reserve` refuses a charge
  past its ceiling at once, under any context, with an error that matches no context error
  (its `decision:` on the branch), so the sum never waits out its deadline; `reserveReply`
  answers that refusal `ErrReplyTooBig` (T5.3), rather than leave it to the fault status and
  the unmapped-error alert. Not `ErrTooLong`: RFC 20 §4.7 defines it as the compound cap, and
  RFC 21 §11.5 maps it to `NFS4ERR_TOO_MANY_OPS`, which would answer a compound within its
  operation cap. The ceiling is the reply pool's, a sixteenth of the node budget and 16 MiB
  at the floor (RFC 24 §2.3), which a compound at its operation cap of 1 MiB `READ` bounds
  passes. Comparing a bound with what is left of an NFSv4.1
  session's `ca_maxresponsesize` needs a limit set by a `SEQUENCE` that has run and an error
  for it, and the frozen surface carries neither: it comes with the slots (§1.2). RFC 20
  §4.3's sentence on `ca_maxresponsesize` and `NFS4ERR_REP_TOO_BIG` is not amended: it is
  deferred, under the ledger's row for NFSv4.1's slots; the refusal of a sum past the reply
  pool's ceiling is added to §4.3 (T5.4), and `ErrReplyTooBig` to §4.7's `Errors()` and
  `core.golden` (T5.3). Which status each protocol answers it with is its table's row, which
  its totality test demands (D5); for RFC 21 §11.5, `NFS4ERR_REP_TOO_BIG` for NFSv4.1 and
  `NFS4ERR_RESOURCE` for NFSv4.0 (§9). `ponytail:` in the runner: one reservation of the
  summed bounds, refused past the reply pool's ceiling and otherwise held whole while it
  runs; reserve per member as it runs, as RFC 20 §4.3 words it, or check what is left of the
  session's `ca_maxresponsesize`, when the slots bring that limit. No prior art sought: the
  limit is one protocol's.
- **D12, every held request keeps its record; only a park releases it.** RFC 20 §4.6 has a held
  request without write data copy its arguments out and release its receive reservation, and
  §4.7 declines the release hook that would let `core` do so: `core` cannot tell which
  arguments point into the record. **A request held between attempts keeps its record's `Buf`
  until its final answer; a parked request releases it in `Park`, whose handler has copied what
  it needs.** RFC 20 §4.6's first bullet is amended. `decision:` on the hold record. No prior
  art sought: the two RFC sentences conflict, and one must give.
- **D13, a held request counts its record's length toward the floor's bytes, at least
  64 KiB.** RFC 24 §4.3 counts "the write's bytes", which `core` does not read. The floor's two
  figures are fields of `shared`, 256 and 32 MiB, which a test lowers per server through
  `hooks_test.go`: on `testproto` the pending ceiling of 8 is reached long before either.
  `decision:` at the count. No prior art sought: the record's length is the only size `core`
  has.
- **D14, the interim interval is 1 s, fixed.** RFC 20 §4.6 says "a short interval". The timer
  is armed only in a table that holds, one per call. `ponytail:` at the constant: one timer per
  call; a sweep per connection if T5.10's figure shows it, and RFC 22's figure when it sets
  one. No prior art checked: what other servers use was not looked up.
- **D15, a held request is re-driven on a backoff of 10 ms doubling to 1 s**, RFC 22 §6.3's,
  never past its hold's deadline, each attempt on the timer's goroutine. `ponytail:` at the
  backoff: the service signals nothing when a delay's cause ends; re-drive on that signal when
  it has one. Prior art: RFC 22 §6.3.
- **D16, what `WithHold` carries, and who reads it.**
  - A table that holds passes the deadline its clone-wait row's `Hold` returns from now, and
    nothing where no row maps the clone wait.
  - A table that does not hold passes the request's remaining deadline, RFC 20 §4.6's rule
    for NFSv3, NFSv4.0 and NLM. NFSv4.1's bound by a quarter of its slots comes with the
    slots (§1.2); until then an NFSv4.1 call waits its whole deadline.
  - T5.5d adds `vfs.Hold(ctx) (max time.Duration, ok bool)` beside `WithHold`, the reader
    RFC 17 §5.9 does not name, with that line in RFC 17.

  The branch's service does not wait behind a clone yet, so the hold is asserted on what
  reaches the service, read by a pass-through wrapper in the test (RFC 29 §4.11's record of a
  call that is itself the requirement). The test at the consumer is RFC 17's V30, a write
  into a cloning extent held under `WithHold(10s)` until the clone ends and answered
  `DelayClone` at once under `WithHold(0)`. No piece of the RFC 17 plan passes it yet: piece
  11, which lists it, merged (#4157) answering `DelayClone` at once with no held wait, which
  `pkg/block/engine/clone.go`'s `decision:` leaves to the filesystem service. A ledger row
  points there (§9). `decision:` at the call. Prior art:
  `context.WithDeadline` and `Deadline`.
- **D17, the hold table's two refusals: one reuses a mapped class, one gets a named error.**
  A request the floor and the hold pool cannot take is answered `ErrDelay` of
  `DelayResources`, which RFC 20 §6.3 calls one class with the reply reservation's refusal
  and `vfs.Errors()` already lists. A hold or a park ended by its client's cancel is
  answered **`ErrCancelled`, a new member of `Errors()`**, which RFC 20 §6.5's cancelled
  status maps from; **`NewOpTable` refuses a table that holds whose error table has no row
  for it naming neither `Ops` nor `Versions`** (D3), for the reasons D7 gives: a cancel ends a
  hold of any operation and version. It lives in `core`'s `errors.go` beside `ErrParked`. It
  amends RFC 20 §4.7 (`Errors()`, `NewOpTable`) and `core.golden`. `decision:` at each. No
  prior art sought: both are choices among this surface's own errors.
- **D18, the `protocol` label is a field of the table, and a server resolves a table's
  series once.** RFC 20 §10.1 labels every series `nfs3`, `nfs4` or `smb`, resolved "when the
  table is built"; `NewOpTable` is given neither a name nor a registry, and `core` first
  sees a table when `Route` returns it. **`OpTable` gains `Protocol string`, set after
  `NewOpTable` as `ReadOp` is. A server resolves a table's series once, at the first `Route`
  that returns it**: `shared` keeps a copy-on-write `atomic.Pointer[map[*OpTable]*series]`,
  and every request loads it and indexes it by its table, with no lock and no label lookup; a
  table it lacks is resolved under `shared`'s mutex, which reads the map again first, so two
  first `Route`s at once register each series once. The once is per server and table, not a
  `sync.Once` in the table: two servers can share a table (one `testproto.Protocol`, or it
  and its `Bare`, handed to both), and each registers into its own registry. A table whose
  `Protocol` is empty is refused there, the first place `core` sees it, since `NewOpTable` runs
  before the field is set: the connection is closed with an error naming the table, as D25
  does. Nothing is written into the table. A compound row's `Members` take their parent's
  label. It amends RFC 20 §4.7 (`OpTable`), §10.1's "when the table is built" and §6.9's "once,
  at table build", and `core.golden`. `decision:` at the field and at the map. No prior art
  sought: the constraint is this surface's.
- **D19, `pkg/metrics` gains RFC 25 §8.2's three calls and no more.** `Spec`, `Buckets` with
  `None` and `Request`, and `Counter`, `Gauge` and `Histogram` on the registry, returning the
  client library's vectors as RFC 25 §8.2 has them. `core` may not import the client library,
  so it holds each resolved child through a one-method interface of its own (`Add`, `Set`,
  `Observe`). A nil `Deps.Metrics` resolves children that record nothing. `ponytail:` at
  `Spec`: unchecked; RFC 25's registry refuses a bad name. Prior art: RFC 25 §8.2.
- **D20, error level is for what no row maps and for a panic.** RFC 20 §4.5 logs expected
  refusals at debug, naming `ErrNotFound`, `ErrExist`, `ErrAccess` "and the like", and
  unexpected ones at error, with no closed set of either. A mapped error is logged at debug;
  the service logs its own faults. `decision:` at the log. No prior art sought: the RFC
  gives examples, not a set to compare with.
- **D21, a recovered panic is answered the fault status without `Map`.** The recovery
  middleware returns an unexported error; the pipeline answers it with the table's fault
  status directly, so a panic is not also counted as an unmapped error. `decision:` at the
  branch. Prior art: `net/http`'s server recovers a handler's panic and logs the stack.
- **D22, a switched-off operation is matched by `Op.Name` in the connection's snapshot.** The
  names are read once per connection, from the snapshot `link` carries (D1), into a set.
  `decision:` at the set. No prior art sought: the setting's shape is E1's.
- **D23, which drain error is E4's, behind one function.** The pipeline decides that a request
  is refused; `drainErr(ctx, handles)` says with what. This epic writes it answering
  `ErrDraining`, the service's answer while no topology names a target; E4 makes it ask.
  `decision:` at the function. No prior art sought: it is a split of work.
- **D24, time is an input.** The pipeline reads the time and arms every timer (interim,
  re-drive, hold deadline, cancel record, attempt deadline) through one unexported clock in
  `shared`, set per server from `hooks_test.go`. The attempt deadline is a context of
  `core`'s own, whose `Deadline`, `Done` and `Err` follow that clock: a `context.WithDeadline`
  would fire on the runtime's time, which a seam test cannot move. That context
  implements `AfterFunc(func()) func() bool`, the method the standard library looks for on a
  parent that is not its own: without it every `context.WithCancel` or `WithTimeout` the
  service derives from a request starts a goroutine to watch the parent. A seam or loopback
  test advances the clock once an event it can wait on has happened (a reply received, a
  `Hold` reached); a part with no pipeline around it runs in a `testing/synctest` bubble.
  `decision:` at the clock.
  Prior art: RFC 29 §4.11's first kind of double.
- **D25, a nil `Hold` is a stage that cancels nothing, and a table that holds needs a real
  one.** RFC 20 §4.7's sketch asks `Cancels` of every message, and has `Hold` "required where
  a table Holds". `withDefaults` fills a nil `Hold` with one whose `Cancels` answers no. A
  table that holds, returned by `Route` on a connection whose `Hold` was nil, is a fault of
  the protocol's wiring: the connection is closed with an error naming it. `decision:` at the
  check. No prior art sought: the rule is the surface's own.
- **D26, a parked reply that does not fit is refused by its encoder.** RFC 20 §4.7 answers it
  "as its protocol answers an overflow". `Encode` is handed the small-class buffer; an
  encoder with more to write returns its protocol's error, which the table maps, and the
  status goes out alone. A reply that outgrew the buffer with no error is the fault status,
  logged at error. `decision:` where `done` encodes. No prior art sought: the answer is the
  protocol's, by the RFC's own words.
- **D27, a cancel finds its target by the protocol's key alone, within its connection.** The
  hold table is per connection and keyed by `HoldCodec.Key`, and `Cancels` names a key; both
  are the protocol's. `Cancels` is asked after `Unwrap` (R63), so a cancel is verified under
  the session it names, but `core` does not compare that session with its target's: on one
  connection, a request of session B can cancel a request of session A when the protocol's
  key lets it. A protocol that must stop that puts the verified session into `Key` and into
  what `Cancels` returns. SMB's `CANCEL` names its target by message ID or async ID on its
  connection (MS-SMB2 3.3.5.16), which RFC 22 keys. `decision:` at the hold table's lookup.
  No prior art sought: the key is the protocol's by RFC 20 §4.7.

## 5. Target tree

```text
internal/adapter/core/
  errors.go              T5.7: NewErrorTable, Map, ErrDecode; T5.2a: ErrCancelled;
                         T5.3: ErrReplyTooBig
  dispatch.go            T5.2a: NewOpTable; T5.4: Op.MaxMembers; T5.9: OpTable.Protocol;
                         T5.11: Request.Park
  protocol.go            T5.1a: Stages.Errors
  metrics.go             T5.9: the series, resolved once per server and table
  middleware.go          T5.6: the chain and its four, the clock's context
  pipeline.go            T5.3: the cap, the reply reservation; T5.1a: link, shared, pipeline,
                         serve; T5.1b: the request's span, logs and counts; T5.2b: the row
                         checks; T5.8: the request context
  compound.go            T5.4: decoding, judging, the walk's checks and the runner;
                         T5.5c: pending members
  hold.go                T5.5b: the hold table, timers, cancel and its records; T5.11: parks
  conn.go                E4's, but for T5.1c: Conn.Send's body
  hooks_test.go          T5.9 on: package core; the hooks of D2 and D24
  out_test.go            T5.1a: the suite of out and the recorder that passes it (D2)
  errors_test.go  dispatch_test.go  metrics_test.go  middleware_test.go
  pipeline_test.go  compound_test.go  hold_test.go  conn_test.go  bench_test.go
  testdata/fuzz/FuzzRecordWalk/       T5.4: the crashers, kept as seeds
  testdata/surface/core.golden        T5.7: ErrDecode; T5.2a: ErrCancelled; T5.3: ErrReplyTooBig;
                                      T5.4: Op.MaxMembers; T5.1a: Stages.Errors;
                                      T5.9: OpTable.Protocol
  testdata/surface/resources.golden   T5.5a: Buf.Recharge
  testdata/allocs                     T5.10: the checked-in number
pkg/metrics/registry.go               T5.9: Spec, Buckets, Counter, Gauge, Histogram
pkg/resources/buf.go                  T5.5a: Buf.Recharge
pkg/vfs/hold.go                       T5.5d: WithHold, Hold
internal/adapter/adaptertest/testproto/   the eleven changes of §1.1, each by its subtask
test/adapter/known.yaml  T5.1c: the end-to-end entry removed
test/suites.yaml         T5.10: ./internal/adapter/core/ added to unit/adapter-allocs
docs/internals/rfcs/rfc-20-adapters.md   T5.7: §4.7 (D7); T5.2a: §4.7 (D17); T5.3: §4.7 (D11);
                                         T5.1a: §4.7, §4.1, §5.5 (D6); T5.9: §4.7, §10.1,
                                         §6.9 (D18); T5.4: §4.7 (D6's chain, D8, D10),
                                         §4.3 (D11); T5.5b: §4.6 (D12)
docs/internals/rfcs/rfc-24-resources.md  T5.5a: §4.3 Recharge; T5.1a: §3.3, §4.2 (D9)
docs/internals/rfcs/rfc-17-vfs.md        T5.5d: §5.9 Hold (D16)
```

Exported symbols added: `Stages.Errors *ErrorTable`; `OpTable.Protocol string`;
`Op.MaxMembers int`; `ErrDecode`, `ErrCancelled` and `ErrReplyTooBig`, each in `Errors()`;
`func (b *Buf) Recharge(to *Budget) error`;
`vfs.WithHold(ctx context.Context, max time.Duration) context.Context` and
`vfs.Hold(ctx context.Context) (max time.Duration, ok bool)`; in `pkg/metrics`, `Spec`,
`Buckets`, `None`, `Request`, and `Counter(Spec)`, `Gauge(Spec)`, `Histogram(Spec)` on the
registry. Nothing else is exported.

## 6. Subtasks

### T5.7 Error tables

**Subtask T5.7 estimate: 2 man-days.**

- **Track:** A; one of the three the program's split lets track C take. **Depends on:** S2.
- **Needs E4:** nothing.
- **Covers:** R26, R27, R28, the mapping half of R29 and R30, and what forces R3's answer
  to exist (D7).
- **Deliverables:** in `errors.go`, the bodies of `NewErrorTable` and `Map`: rows ranked and
  sorted at build (D4), equal rows that overlap refused (D3), `Map` answering the first match,
  `reply` false on a `NoReply` row, the fault status where none matches. An unexported
  `mapped(err, op, v) (Status, bool, bool)` beside `Map`, its third result false where no row
  matched, which T5.1b calls to count and log. `ErrDecode`, added to `Errors()`, and
  `NewErrorTable` refusing a table with no row for it that names neither `Ops` nor `Versions`
  (D7), with RFC 20 §4.7's amendment and `core.golden`; `testproto`'s row for it, with a
  status of its own (§1.1's change 8); the tables `errors_test.go` and `dispatch_test.go`
  already build gain that row, or they panic.
- **Landed in part:** #4194 gave `NewErrorTable`, `Map` and `mapped` their bodies, with the
  tests of the first five criteria below. `ErrDecode`, its check, its row and its amendment
  are what this subtask still adds.
- **Consumers:** T5.2a (the check on `Hold`), T5.1a, T5.1b, `testproto`'s tables.
- **Acceptance criteria:**
  - A row naming `&vfs.Delay{Cause: vfs.DelayRecall}` wins over a row naming `vfs.ErrDelay`,
    in either order of declaration, and does not match a `Delay` of another cause.
  - Among rows of one `Err`, the one naming the operation and the version wins, then the
    operation, then the version, then neither; with a nil operation only rows naming none
    match.
  - A wrapped error is matched as its cause is.
  - Two rows of one rank that can both match one error, operation and version panic at build,
    naming both; two typed rows of different kinds do not.
  - An error no row matches answers the fault status with `reply` true.
  - A table with no row for `ErrDecode`, and one whose only `ErrDecode` row names an
    operation or a range of versions, each panic at build, naming it; a wrapped `ErrDecode`
    maps to its row.
  - `ErrDecode` is in `Errors()`, so `TestErrorsListsEverySentinelOnce` in `errors_test.go`,
    which counts `core`'s declared refusals against `Errors()`, still passes.
- **Tests:** in `errors_test.go`, through the exported surface. The typed and sentinel rows in
  both orders (catches a `Map` that takes the first declared). The four ranks on one error
  (catches a rank compared the wrong way). The nil operation against a row naming operations
  (catches an operation row answering a refusal that has no operation). A wrapped `Delay`
  (catches `==` in place of `errors.Is`). A `Delay` of another cause against a typed row
  (catches a typed row matching its class). The overlapping rows, and the pair that does not
  overlap by version (catches a build that accepts an ambiguity, and one that refuses
  disjoint ranges). The unmapped error (catches a zero status in place of the fault). The table
  without `ErrDecode`'s row, and the tables whose only `ErrDecode` row names an operation or
  versions (catch a build that lets a client's garbage reach the fault status: a header's
  decode error has no operation, and a row naming one never matches it).
  `TestErrorsListsEverySentinelOnce` (catches `ErrDecode` declared and left out of
  `Errors()`). No test double.
- **Rules:** rule 9: `mapped` is unexported; `ErrDecode`'s amendment precedes it.

### T5.2a `NewOpTable`

**Subtask T5.2a estimate: 0.75 man-days.**

- **Track:** A. **Depends on:** T5.7.
- **Needs E4:** nothing.
- **Covers:** R15.
- **Deliverables:** in `dispatch.go`, `NewOpTable`'s body: the map built, and a duplicate
  code, a range whose `First` is past its `Last`, and a row of `errs` with a `Hold` in a
  table that does not hold, each a panic naming the row (D3); a table that holds whose
  `errs` has no row for `ErrCancelled` naming neither `Ops` nor `Versions` refused the same
  way (D17). `ErrCancelled`, added to `Errors()`, with RFC 20 §4.7's amendment and
  `core.golden`; `testproto`'s cancel row renamed to it (§1.1's change 8); the holding table
  `dispatch_test.go` already builds gains that row, or it panics.
- **Landed in part:** #4194 gave `NewOpTable` its body and its first three checks, and moved
  the end-to-end entry's message in `test/adapter/known.yaml` to `core: new server`, where the
  specification now stops, with `TestNewOpTableChecksItsRows` and `testproto`'s
  `TestTablesBuild`. `ErrCancelled`, its check, its row and its amendment are what this
  subtask still adds.
- **Consumers:** T5.9, T5.4, T5.1a; T5.5b and T5.11, which answer `ErrCancelled`;
  `testproto`, whose tables build from here on.
- **Acceptance criteria:** each of the four faults panics with the row's name, a holding
  table whose only `ErrCancelled` row names an operation or versions included; `testproto`'s
  two tables and its members' table build; a `Hold` row in a holding table is accepted;
  `ErrCancelled` is in `Errors()`.
- **Tests:** in `dispatch_test.go`. One case per fault (catch a check dropped: two rows under
  one code would otherwise shadow each other silently), and the `ErrCancelled` rows that name
  an operation or a range of versions (catch a check that any row satisfies, which leaves a
  cancel of another operation or version to the fault status). `testproto`'s `TestTablesBuild`,
  which builds both versions' tables directly (catches a check that refuses a valid table).
  `TestErrorsListsEverySentinelOnce` (catches `ErrCancelled` left out of `Errors()`). The
  `adapter-conformance` specification, known by its message (a run that still stops at the
  tables fails as unexpected).

### T5.9 Series and the registry's calls

**Subtask T5.9 estimate: 2.25 man-days.**

- **Track:** A; one of the three the program's split lets track C take.
  **Depends on:** T5.2a.
- **Needs E4:** nothing.
- **Covers:** R84, R85 (resolution), R86, R87.
- **Deliverables:** in `pkg/metrics/registry.go`, D19's types and three calls. In `metrics.go`,
  the series of R85 as one struct per table, its per-operation and per-error parts indexed,
  each child held through `core`'s own one-method interface, a nil registry giving children
  that record nothing; `shared`'s copy-on-write map from table to that struct, loaded with no
  lock and filled under `shared`'s mutex, once per server and table, under its `Protocol`
  (D18), with an
  empty `Protocol` refused with an error naming the table, which T5.1a answers by closing the
  connection; the record methods the later subtasks call, each an atomic add or an observe.
  `OpTable.Protocol`, with RFC 20's three amendments (D18) and `core.golden`; `testproto`'s
  tables set it to `testproto`.
- **Consumers:** T5.6 (panics, wire seconds), T5.1b (in flight, refusals, replay hits, unmapped
  errors, statuses mapped), T5.5b (held requests, held bytes, hold pool, hold refusals), T5.10
  (the gate).
- **Acceptance criteria:**
  - Resolving one table twice on one server registers each series once, and so do two first
    resolutions of it at once; two tables under one label share their per-protocol series;
    one table on two servers records into each server's own registry.
  - A table whose `Protocol` is empty is refused, naming it, and registers nothing.
  - Every series carries only the labels R85 gives it.
  - A record method on a resolved series allocates nothing, with a registry and with none.
- **Tests:** in `pkg/metrics`, each call registers a vector a scrape of the registry's handler
  shows, with the `request` buckets on the histogram (catches a vector built and not
  registered, and default buckets). In `metrics_test.go`, through `hooks_test.go`, on the
  tables `testproto`'s router returns: each record method scraped (catches a method wired to
  another series); the label names of every series compared with R85's (catches a client,
  share or operation label added where it does not belong); the double resolution (catches a
  duplicate registration, which panics in the client library); two goroutines resolving one
  new table at once on one server, under the race detector (catches a map written without
  the mutex, and a resolution not re-checked under it, which registers twice and panics); the
  empty `Protocol` (catches series registered with an empty label); one table on two
  registries (catches series kept in the table, which sends the second server's counts to the
  first);
  the allocation count of each record method at 0, skipped under the race detector (catches a
  label lookup per call).
- **Rules:** rule 3: nothing outside `core` records; rule 9: the amendment precedes the field.

### T5.6 The middleware chain

**Subtask T5.6 estimate: 1.5 man-days.**

- **Track:** A; one of the three the program's split lets track C take.
  **Depends on:** T5.9.
- **Needs E4:** nothing. That a panic leaves the connection's other requests unharmed is
  T5.1a's seam test.
- **Covers:** R79, R80 (recovery, log, count), R81 (the deadline set), R85 (panics, wire
  seconds), R89, R90.
- **Deliverables:** in `middleware.go`, the chain, built once per row from one ordered list
  of four `Middleware`: recovery (the panic logged at error with the operation and the stack,
  counted, returned as D21's error), trace span (a child per operation of a compound, none
  for a single operation, its attributes R90's, ended in a deferred call), metrics (the stage
  clock of `adapter_wire_seconds` around the call, observed in a deferred call), deadline
  (D24's context, 30 s from the clock).
- **Consumers:** T5.1a, which runs every `Invocation.Call` through it; T5.5b, whose every
  attempt passes it again.
- **Acceptance criteria:**
  - A `Call` that panics returns D21's error; one Error record names the operation and holds
    the stack; the counter of that operation rises by one.
  - The `Call` sees a context whose `Deadline` is 30 s from the clock, whose `Err` turns
    to `context.DeadlineExceeded` when the clock passes it, and from which a
    `context.WithCancel` child is derived without a goroutine, and inside a compound a recording
    span of its own; outside one, the request's span.
  - On a panic the span is ended with the fault recorded and the stage is observed.
- **Tests:** in `middleware_test.go`, through `hooks_test.go`, on a probe `Call`, a recording
  tracer the test supplies as `Deps.Tracer` and a `slog` handler it supplies as `Deps.Logger`.
  The panic (catches a chain without recovery, a log without the stack, a count on another
  operation). The order: the probe panics, and the span is found ended and the stage observed
  (catches a span or a clock closed without a deferred call, and recovery placed inside
  either, which then never sees the panic pass); the probe's context is done at 30 s on the
  clock while the span is still open (catches the deadline placed outside the span). The
  clock's context in a `synctest` bubble (catches a deadline on the runtime's time); a child
  derived from it there, with the goroutines counted before and after (catches a context
  without `AfterFunc`). The
  span's attribute keys against R90's set (catches an attribute outside it). The recording
  tracer and handler are sinks, not doubles.

### T5.5d `vfs.WithHold`

**Subtask T5.5d estimate: 0.5 man-days.**

- **Track:** A. **Depends on:** S2.
- **Needs E4:** nothing; nor E3, which is why it is apart from T5.5a.
- **Covers:** the call R52 is built on.
- **Deliverables:** in `pkg/vfs/hold.go`, `WithHold` and `Hold` (D16), with RFC 17 §5.9's
  line.
- **Consumers:** T5.5b; RFC 17's clone wait (`Hold`), which can start once this has landed.
- **Acceptance criteria:** `Hold` returns what `WithHold` set, the innermost one, and `false`
  on a bare context.
- **Tests:** in `pkg/vfs`: the round trip, nesting and the bare context (catches a reader of
  another key, and an outer hold winning).

### T5.3 Admission: the cap of 64 and the reply reservation

**Subtask T5.3 estimate: 1.25 man-days.**

- **Track:** A. **Depends on:** S2 and E3.
- **Needs E4:** nothing. That the cap is wired is T5.1a's seam test, and that a connection at
  it is not read is T5.1c's loopback test.
- **Covers:** R22 (the count), R23 (the reservation), R24, and `Flow`'s default in R6; D11's
  ceiling.
- **Deliverables:** in `pipeline.go`, the default `FlowControl`, a counting semaphore of 64
  whose `Admit` waits under its context and whose release is idempotent, with RFC 20 §6.3's
  `ponytail:`; `reserveReply(ctx, key, bound) (*resources.Buf, error)`, a `Get` on the reply
  pool (D9) that turns the wait's end at the context's deadline into
  `&vfs.Delay{Cause: vfs.DelayResources}`, leaves a cancellation as it is, and answers
  `ErrReplyTooBig` for the refusal that matches neither context error: `WaitBudget.Reserve`
  refuses at once, under any context, a charge past the reply pool's ceiling (D11).
  `ErrReplyTooBig`, added to `Errors()`, with RFC 20 §4.7's amendment and `core.golden`.
- **Consumers:** T5.1a (`withDefaults`, the call before every handler), T5.4, T5.5b (each
  re-run), T5.11.
- **Acceptance criteria:**
  - 64 `Admit` calls return; the 65th returns only after a release; a second call of one
    release frees no second place.
  - With the reply budget full, `reserveReply` returns the `Delay` when its context's deadline
    passes, with nothing reserved; when the context is cancelled it returns the context's
    error; when room frees first it returns the buffer.
  - A bound past the reply pool's ceiling returns `ErrReplyTooBig` at once, under a context
    with no deadline, with nothing reserved.
  - `ErrReplyTooBig` is in `Errors()`.
- **Tests:** in `pipeline_test.go`, through `hooks_test.go`, each in a `synctest` bubble. The
  65th `Admit` still blocked after `synctest.Wait`, then returning on a release (catches a cap
  of 65 and a semaphore that never blocks). The double release (catches a counter that
  drifts). The three ends of a waiting reservation, the budget read after each (catches a
  deadline surfaced as the context's error, which no table maps, and a reservation kept on the
  error path). The bound past the ceiling, `synctest.Wait` showing `reserveReply` returned
  (catches the refusal passed on as it is, which no table maps and the unmapped-error alert
  then counts, and a sum that waits on a budget that can never grant it).
  `TestErrorsListsEverySentinelOnce` (catches `ErrReplyTooBig` left out of `Errors()`). No
  test double.

### T5.5a `Buf.Recharge`

**Subtask T5.5a estimate: 0.5 man-days.**

- **Track:** A. **Depends on:** E3.
- **Needs E4:** nothing.
- **Covers:** the call R57 is built on.
- **Deliverables:** in `pkg/resources/buf.go`, `Recharge(to *Budget) error`: reserve the
  buffer's whole charge from `to`, and only on success release its reservation and keep the
  new one; on refusal nothing changes. RFC 24 §4.3 gains the method, and `resources.golden`
  the line.
- **Consumers:** T5.5b.
- **Acceptance criteria:**
  - After a `Recharge` that fits, the receive budget is lower and the hold budget higher by
    the buffer's whole charge, class and overhead, and the last `Release` returns it to the
    hold budget.
  - A `Recharge` that does not fit returns an error and both budgets read as before.
  - At no point between the two reads is the charge in neither budget.
- **Tests:** in `pkg/resources`: the two outcomes, both budgets read after each, then the
  release (catches a charge of the length in place of the class, a release to the old budget,
  and a refusal that drops the reservation); the order, read by a `Recharge` into a full hold
  budget that must leave the receive charge in place (catches release before reserve).

### T5.1a The pipeline, at its seam

**Subtask T5.1a estimate: 2.75 man-days.**

- **Track:** A. **Depends on:** T5.2a, T5.9, T5.6, T5.3.
- **Needs E4:** nothing: every test is a seam test (D2). The drain cases call `drain` through
  `hooks_test.go`, as E4's drain will.
- **Covers:** R1, R2, R3, R4, R5 (the miss), R6, R7, R8, R10, R14, R21, R22, R23, R25, R30,
  R32, R80.
- **Deliverables:** first, in `out_test.go`, the suite of `out` (D2): a function that takes a
  constructor of a `link`'s `out` and `close` and checks that `out` never blocks, also past
  its queue's bound, takes the message's reference whatever it returns, fails only once
  closed, and sends a message handed with `before` ahead of the reply of that key; and the
  recorder that passes it, which E4's tests reach through `hooks_test.go` to run the suite on
  E4's writer. Then, in `pipeline.go`, `link`, `shared`, `pipeline` and D1's functions:
  `withDefaults` (R6, D6, D25), `serve` as D8 splits it, one request from `Route` to `out` in
  RFC 20 §4.7's order, D6's paths (an error before a table, its unwrapped reply, `Unwrap`'s
  nil header closing the connection), decode errors wrapped in `ErrDecode` (D7), a table's
  series resolved at its first `Route` and an empty `Protocol` closing the connection (D18),
  the role asked only while draining or expired, the drain filter for a request outside a
  compound with D23's `drainErr`, `mapped`'s status sent (D21), the `NoReply` branch with its
  interim where the table holds, `Lookup` and `Store`, `wait`, and `close`, which never
  blocks, for a pipeline that holds nothing yet. `Stages.Errors`, with RFC 20's amendments
  (D6) and `core.golden`; `testproto`'s changes 1, 2, 9 and 10 of §1.1. RFC 24's amendment
  (D9), after E3's second pull request.
- **Consumers:** every later subtask; E4, whose writer must pass the suite of `out`, and
  whose accept and reader call D1's functions.
- **Acceptance criteria:**
  - The recorder passes the suite of `out`.
  - An `Echo` handed to `serve` over `Bare` and over the full protocol is answered its
    payload.
  - An `Echo` with no role byte and a header whose `next` is bad are answered `testproto`'s
    status for `ErrDecode`, on their own message ID, the connection kept, and
    `adapter_unmapped_errors_total` does not move.
  - A message with a bad checksum, one that overdraws its credits and a version the router
    refuses are each answered a status on their own message ID, the connection kept; the
    reply to the bad checksum is unsealed. A record too short for a header closes the
    connection.
  - A message whose `Unwrap` returns no header with its error closes the connection with that
    error, and nothing reaches `out`.
  - Stages without `Errors` are refused by `withDefaults`, with an error naming the field.
  - While draining, an `Echo` whose argument says none gets nothing over version 1 and an
    interim granting its credits over version 2; one that says context, flush or release is
    answered.
  - A `Panic` is answered the fault status, and an `Echo` after it on the same connection is
    answered.
  - On `Bare` with the reply budget full, of 65 `Echo` handed to `serve`, 64 are admitted and
    the 65th `serve` returns only after one has completed; all 65 are answered once room is
    freed.
  - Version 2 on stages with no `Hold` closes the connection; `Bare`, whose `Hold` is nil,
    serves version 1. A table whose `Protocol` is empty closes the connection, naming it.
  - `close()` never blocks: with an `Echo` waiting for its reply reservation, it returns at
    once when called from the test, from a handler's own call and from `Unwrap` on the
    reader, and `wait()` returns only once that `Echo` has ended (`TestCloseNeverBlocks`).
- **Tests:** seam tests in `pipeline_test.go`. The suite on the recorder (catches a recorder
  that would hide a writer's fault: an `out` that blocks, keeps a reference, fails before
  the close, or ignores `before`). The echo on both stage sets (catches a nil stage called,
  and a default left out). The order of stages, read from `testproto`'s own effects: a bad
  checksum is refused before credits are charged and before the replay cache is asked
  (catches two stages swapped). The two decode errors (catch a garbage request answered as a
  fault, the unmapped counter raised by a client, and a header with its error dropped
  unanswered). The three other refusals and the short record (catch a refusal that closes, a
  reply sealed under a session its request did not prove, and a record with no header
  answered). `testproto`'s `TestUnwrapRefusesAChangedByte` also reads the header returned
  with the refusal (catches a bad checksum that closes the connection, as D6's nil header
  does). `Unwrap`'s nil header, through a protocol declared in the test that returns
  `testproto`'s stages with an `Unwrap` that answers none, an input and not a double (catches
  a refusal answered that the protocol must not answer, as SMB's failed decryption, and a
  connection kept after it). The stages without `Errors`, through that test protocol (catches
  a connection served with nothing to answer its refusals). The drain cases, by role (catch
  a filter on `Op.Role` alone); with `testproto`'s tables wrapped by the test protocol so
  each `Invocation.Role` passes through a record of its calls, no call while serving
  (catches a role asked of every request). The panic, then an echo (catches a recovered
  goroutine that closes the connection). The 65 requests, read from one ordered stream of
  admissions and completions that a hook set per server delivers: the 65th admission comes
  after the first completion (catches `withDefaults` leaving `Flow` without the cap). The
  stripped `Hold` and the empty `Protocol`, through the test protocol (catch a holding table
  run with nothing to key it, and a table resolved under no label). `TestCloseNeverBlocks`,
  in a `synctest` bubble, the handler's call and `Unwrap` reaching `close()` through
  `hooks_test.go` (catches a `close` that waits for the requests it ends, which deadlocks the
  bubble when called from a handler or from the reader, and a `wait` that returns while a
  request still runs). Each test ends with `CheckLeaks` (catches a reply reservation kept on
  a `NoReply` path). What needs a service call (out of order, the replay hit, a request in
  the service as the drain begins, two servers) is T5.4's: `testproto` reaches the service
  only from a chain or a compound, since `Create` needs the filehandle a `PutFH` before it
  set.
- **Rules:** rule 1: no in-package test of `serve`; the recorder is a verified implementation
  of `out`, held to the suite E4's writer passes (D2).

### T5.1b The request's span, logs and counts

**Subtask T5.1b estimate: 1.25 man-days.**

- **Track:** A. **Depends on:** T5.1a.
- **Needs E4:** nothing.
- **Covers:** R29 (answered, logged, counted), R31, R85 (in flight, refusals, replay hits,
  unmapped errors, statuses mapped), R88, R90, R91.
- **Deliverables:** in `pipeline.go`: the request span, opened as `serve` takes the record
  and ended as the reply goes to `out` or is dropped, its attributes R90's; `mapped`'s
  counts and its log at D20's levels; the in-flight gauge, the refusals by stage, the replay
  hits, the statuses mapped; each request's log lines with its trace ID, and the one line
  per connection for a misbehaving client.
- **Consumers:** T5.4 (the request span its children hang from), T5.5b, T5.10.
- **Acceptance criteria:**
  - A message that overdraws its credits, whose error no row maps, is answered the fault
    status, logged once at error, and counted in `adapter_unmapped_errors_total`.
  - Each refusal of T5.1a raises `adapter_refusals_total` under its own stage, and each
    mapped error `adapter_status_mapped_total` under its error.
  - `adapter_requests_in_flight` reads 64 when the 64th of T5.1a's 65 requests has been
    admitted, and zero when all are answered.
  - Each request has one span, ended, with R90's attributes and no other; a refused one
    names the stage that refused.
  - Each line logged for a request carries its trace ID, sampled or not; a second refused
    checksum on one connection logs nothing more; a mapped refusal logs at debug.
- **Tests:** seam tests. The overdraw (catches an unmapped error answered silently). The
  counters after each refusal (catch a stage label on the wrong refusal, and a mapped status
  not counted). The gauge on T5.1a's stream of admissions (catches a gauge not wired, and one
  never lowered). The spans through a recording tracer (catch a span left open on a refusal,
  and an attribute outside the set). The log lines through a handler the test installs
  (catch a line without its trace ID, one line per request, and a client's refusal at
  error). The tracer and handler are sinks, not doubles.

### T5.2b Row checks and the expired credential

**Subtask T5.2b estimate: 1 man-days.**

- **Track:** A. **Depends on:** T5.1a.
- **Needs E4:** nothing.
- **Covers:** R2, R8, R14, R16, R17, R18, R19, R20, R21.
- **Deliverables:** in `pipeline.go`, the checks between `Route` and `Decode`, in this order:
  the row absent or switched off (D22), `Versions`, `Needs` against `Level` from the lowest
  precondition up, then the filehandle; after `Resolve`, the expired-credential check.
  `testproto`'s change 11 of §1.1, without which `Session` panics at the seam and no session
  or tree can be set up before E4.
- **Consumers:** T5.4, which runs the same checks per member.
- **Acceptance criteria:**
  - An unknown code, and `Echo` on a pipeline whose `link` carries settings that switch it off
    by name, are answered not supported, their arguments never decoded; `Negotiate` on that
    pipeline runs.
  - `Park` in version 1 is answered the version status.
  - `Tree` before a session, `PutFH` before a tree and `Create` with a tree and no filehandle
    are answered the session, tree and filehandle statuses.
  - On an expired session `Tree` is answered the expired status, `Logoff` and `Session` run,
    and an `Echo` runs only when its argument says release.
- **Tests:** seam tests. Each check alone (catches a check dropped). The unknown code
  with arguments that do not decode, still answered not supported (catches decoding before
  the check). `Create` with no session answered the session status, not the filehandle's
  (catches the order). The switched-off `Echo` beside a served `Negotiate` (catches a setting
  that is not read, and one that switches every operation off). The expired cases, with an
  `Echo` that says flush refused (catches the drain's wider set used for expiry) and `Tree`
  refused (catches no check at all); `Session` answered success (catches change 11 left out:
  the handler then panics on the nil `Conn` and is answered the fault status).

### T5.4 The compound runner

**Subtask T5.4 estimate: 4.25 man-days.**

- **Track:** A. **Depends on:** T5.1b, T5.2b.
- **Needs E4:** nothing.
- **Covers:** R5 (the hit), R9, R13, R14, R33, R35, R36, R37, R38, R39, R40, R41, R42, R43,
  R44, R45, R46, R47, R48, R49, R89, R93; D11's ceiling.
- **Deliverables:** in `compound.go`, the walk, the judgment and the runner.
  `members(row *Op, inv Invocation, rest []byte, v Version)` refuses a `Count` past the row's
  `MaxMembers` with `ErrTooLong` before it reads a member (D10), decodes up to `Count`
  members with their rows, returns those that decoded with the index and error of the first
  that did not or has no row, and returns none, with the compound's decode error, when bytes
  are left after the last; a chain's walk takes each header at the `Next` of the one before;
  every step of both is checked as R93 says. `judge(ms, r)` takes each member's role from
  `Op.Role` or its `Invocation.Role`, counts a member that did not decode as refused,
  collects every `Invocation.Handle`, and answers whether the compound runs whole. The runner,
  for a compound row: the members decoded, one reply buffer of the bounds' sum through
  `reserveReply`, answered `ErrReplyTooBig` past the reply pool's ceiling (D11), the members run
  in turn through T5.2b's checks and the chain, the `Request` carried between them, the first
  failure ending the reply; while draining, read once, `judge` first. For a chain: each member
  decoded, unwrapped, admitted and looked up at its own offset, its admission released once
  its reply is encoded (D8), `prev` handed to `Unwrap` and `Route`, a related member after a
  failed one answered the failed member's status without a call (R42), a member `Unwrap`
  refuses splitting the chain's replies as D6 says, the replies joined with `Chain` and
  wrapped once per record; at a
  `NoReply` member, the later members' headers decoded into a pooled slice and handed to
  `Interim`. `Op.MaxMembers`, with RFC 20 §4.7's amendment and `core.golden`, and
  `NewOpTable` refusing a row with `Members` and no `MaxMembers`; `testproto`'s compound row
  sets it to 8 (§1.1's change 4), and its tables gain the row for `ErrReplyTooBig` (change 8).
  RFC 20's amendments for D6's chain rule and D8's release (§4.7) and D11's ceiling (§4.3).
  `FuzzRecordWalk`, its crashers kept under `testdata/fuzz/FuzzRecordWalk/`.
- **Consumers:** T5.8, T5.5b, T5.5c, T5.11, whose tests reach the service through a chain;
  E4, whose `drainErr` reads the handles `judge` collects (D23).
- **Acceptance criteria:**
  - A compound of three members yields three, in order; one byte more after the last yields
    none and the compound's decode error; a count of three over the bytes of two yields the
    two, with index 2 and the third's decode error; a second member with a code the table
    lacks is reported at index 1 with `ErrNotSupported`, the first returned decoded.
  - A version 1 compound of `PutFH` and two `Create` makes two files; with the first `Create`
    failed by the service, the second does not run and the reply ends at the failure. One
    whose third member does not decode makes the first two files and ends at the third's
    error.
  - A compound of 9 members is answered the too-long status with no member read and no file
    made; one of 8 is decoded. A table whose compound row has no `MaxMembers` panics at
    build.
  - A compound whose bounds' sum passes the reply pool's ceiling is answered
    `ErrReplyTooBig`'s status at once, under a context with no deadline, with no member run
    and nothing reserved. With the reply budget full, a compound makes no call until room is
    freed.
  - `judge` serves a compound of a `PutFH` and an `Echo` whose argument says release, and
    refuses one whose `Echo` says none, or whose last member does not decode; it returns the
    `PutFH`'s handle in both. While draining, a compound with one refused member gets no
    reply, makes no file, and its retransmission after the drain runs; a compound sent whole
    before the drain began runs whole.
  - A version 2 chain of a `PutFH` and a related `Create` makes the file under the handle the
    first set; with the `PutFH` refused, the related `Create` is answered the `PutFH`'s
    status and makes none.
  - A chain of a member under session A, an unrelated member naming session B whose checksum
    fails, and an unrelated third member under A sends three records: the first's reply
    sealed under A, the second's refusal unsealed, and the third's reply sealed under A; the
    client's balance then holds every member's credits. A related member whose checksum fails
    is refused the same way, and a related member after it is answered that status, with no
    call, in the refusal's record.
  - A chain's replies arrive as one record whose checksum verifies.
  - A chain of 65 `Echo` on `Bare` is answered whole.
  - A chain member whose `Next` points at its own header, behind it or past the record, and
    a compound member whose code takes no byte or whose arguments claim more bytes than are
    left, are answered `ErrDecode`'s status, and nothing past them is read.
  - While draining, a chain whose second of four members is refused answers the first, sends
    one interim that grants the credits of the last three, and the client's balance says so.
  - Two chains on one connection, the first's `Create` held in the service, are answered
    second first; a chain in the service when the drain begins is answered.
  - A sequenced chain sent twice is answered twice and its `Create` runs once.
  - Two servers over two services each apply their own `Create`.
- **Tests:** seam tests in `compound_test.go`, on `testproto`'s compound row and chained
  frames, over `vfstest.Faults` on `memory.New` with `vfstest.RecordMutations`; the walk and
  `judge` alone through `hooks_test.go`, on bytes built with `testproto.Append`. The exact
  count and the trailing byte (catch a loop that stops at the bytes' end and accepts what is
  left). The short compound and the unknown code at index 1 (catch a compound refused whole
  for one member, which outside a drain must still run the members before it). The cap at 9
  and at 8, with `testproto`'s `ReadOp` wrapped by a pass-through that records its calls,
  the record of a call that is itself the requirement and not a double (catches a cap off by
  one, and a member read before the refusal). The row without its cap (catches a compound no
  cap bounds). The stop at the first failure (catches a runner that runs on). The
  undecodable third member (catches a compound refused whole outside a drain). The sum past
  the ceiling, on a server whose reply budget the test sets below it, `synctest.Wait` showing
  the reply sent (catches a sum that waits on a budget that can never grant it, a refusal
  answered as the fault, and one answered `ErrTooLong`'s status, which an NFSv4.1 table maps
  to too many operations). The compound sent against a full reply budget, `RecordMutations`
  empty until the test frees room, which it does once the stream of admissions shows the
  compound admitted (catches a reply reserved after the first member ran). The four
  judgments (catch a role read from the row where the arguments decide, an undecodable
  member served, and a handle dropped). The drained compound and its retransmission (catches
  a judgment per member, which applies the `PutFH`, and a reply cached for a compound that
  did not run). The compound begun before the drain, held in its first member while the
  drain starts (catches the drain read per member). The related pair and its failure
  (catches a related member routed without `prev`, one that calls after a failure, and one
  answered by its own row in place of the failed member's status). The member naming session
  B, the client's balance read after the third record (catches a refusal joined into the
  record and signed under the session it names, a later member left unanswered, and its
  credits not granted); the related member's bad checksum and the related member after it
  (catch `Unwrap` handed no `prev`, which then closes the connection, and a related member
  run after the refusal). The joined record (catches `Wrap` per member, which breaks the
  record's checksum). The 65-member chain on `Bare`, in a `synctest` bubble (catches admissions
  released at the join, which leaves the 65th waiting on its own chain until the bubble
  deadlocks). The walk's bad steps, through a header codec and a members table declared in the
  test that take each `Next` and each count as the bytes give them, inputs that stand for a lax
  protocol and not doubles (catch a `Next` that loops on its own header, appending replies for
  ever, and a slice past the record, which panics outside the recovery middleware).
  `FuzzRecordWalk`, over arbitrary records through the same codec and table: no panic, the walk
  ends, every offset it reads lies inside the record and past the one before, and a step that
  does not is `ErrDecode` (catches any bound R93 leaves out). The balance after a refused
  second member (catches credits of later members not granted, and a later member run). Out of
  order, with `Faults.Hold` and its `Reached` (catches requests run one after another). The
  chain in the service at the drain (catches a drain that drops what already runs). The replay,
  with the record at one and `adapter_replay_hits_total` at one (catches `Lookup`'s reply
  ignored, and `Store` after `Wrap`: the second reply then fails its checksum). Two servers
  (catches a service kept in a package variable). A service error no row maps, answered the
  fault status (catches a service error dropped). One child span per member and none for a
  single request (catches a span per request only). Doubles: `vfstest.Faults` over
  `memory.New`, kind 3 over kind 2; `vfstest.RecordMutations`, the pass-through record.
- **Rules:** rule 2: `FuzzRecordWalk` (§2).

### T5.8 The request context

**Subtask T5.8 estimate: 1 man-days.**

- **Track:** A. **Depends on:** T5.4.
- **Needs E4:** nothing. A connection's close is the `link`'s context cancelled and `close()`
  called, through `hooks_test.go`, as E4's `Conn.Close` does.
- **Covers:** R14, R34, R81 (end to end), R82, R83.
- **Deliverables:** in `pipeline.go`, the request's context derived from the connection's; the
  reply of a request whose context is cancelled dropped, with its reservations released and its
  span ended as dropped, and nothing encoded.
- **Consumers:** T5.5b, whose attempts each take a fresh deadline under this context; E4's
  drain deadline, which cancels through it.
- **Acceptance criteria:**
  - The context a `Create` reaches the service with has its deadline 30 s after the request's
    arrival on the clock.
  - A `Create` held in the service is answered once the clock passes that deadline, and an
    error the service answers then is mapped by the table.
  - A `Create` held in the service when its connection closes sees its context cancelled; no
    reply is encoded, its span ends as dropped, and every budget returns to zero.
- **Tests:** seam tests, each request a chain of `PutFH` and `Create`. The deadline read
  by a pass-through wrapper declared in the test, which records the context each call
  arrives with (catches a context with no deadline, and one counted from the wrong instant).
  The held call, with `Faults.Hold`, answered only after the clock is advanced past 30 s
  (catches a deadline that never fires: the reply never comes). `Faults.Fail` with a wrapped
  `context.DeadlineExceeded`, answered the fault status `testproto` gives an error it has no
  row for (catches a deadline error answered outside the table). The close, read from
  `CheckLeaks` and the recording tracer (catches a request that outlives its connection, a
  reply reservation leaked with a dropped reply, and a span left open).

### T5.5b Held requests

**Subtask T5.5b estimate: 4.5 man-days.**

- **Track:** A. **Depends on:** T5.5a, T5.5d, T5.8.
- **Needs E4:** nothing.
- **Covers:** R12, R14, R50, R51, R52, R53, R54, R55, R56, R57, R58, R59, R60, R61, R62, R63, R64,
  R65, R66, R67, R78 (holds), R85 (the held series).
- **Deliverables:** in `hold.go`, the per-connection table of pending requests under one
  mutex, held for the lookup only: a request's record (its `Invocation`, its retained record,
  its key, its start, its timer); the interim timer (D14), armed only in a table that holds;
  the hold begun on an error whose row has a `Hold`, the floor and the hold pool
  (`Recharge`, D13), the ceiling, D17's refusal; the re-drive (D15) from `Resolve`, each under
  a fresh attempt deadline and a fresh reply reservation; the interim, handed no headers, and
  the final header with their `grant` flags; `WithHold` on every call (D16); the cancel after
  `Unwrap`, matched by the protocol's key alone (D27), a cancelled hold answered `ErrCancelled`
  (D17), the cancel remembered for 5 s and at
  most `MaxPending` of them, with RFC 20's
  `ponytail:`; `close` ending every hold; the gauges and the refusal counter; one child span
  per attempt. RFC 20 §4.6's amendment (D12). `testproto`'s changes 5 and 6 of §1.1.
- **Consumers:** T5.5c, T5.11.
- **Acceptance criteria:**
  - In version 2, a `Create` the service answers `ErrDelay` twice gets one interim, runs a
    third time on the backoff and is answered success; in version 1 it is answered the delay
    status at once.
  - Left failing, it is answered the delay status when the clock passes the hold's bound, and
    not before; with the bound the test sets on the protocol shortened between attempts, it
    ends at the new bound.
  - Between attempts the reply budget reads zero, and each attempt reaches the service with a
    deadline 30 s from its own start.
  - A session logged off between attempts ends the hold with the no-session status.
  - A `Create` still in the service after the interval gets an interim, and its final reply
    carries the key.
  - Past `MaxPending`, a hold starting on a service error is answered the too-many-pending
    status; a call found in the service goes pending, its interim leaves the client's balance
    at one, and its final reply grants the remaining credits.
  - With the floor lowered to two requests, the third held raises the hold budget by its
    record's whole charge and lowers the receive budget by it. With the hold budget full, a
    hold starting on a service error is answered D17's refusal and counted, and a call found
    in the service goes pending and keeps its receive reservation.
  - A cancel between attempts is answered the cancel status and gets no reply of its own; a
    cancel sent while an attempt is in the service lets it finish and answer its own result;
    a cancel sent before its target holds ends it as cancelled when it does; an unsealed
    cancel under a session ends nothing.
  - A cancel sent before its target is forgotten after 5 s on the clock, and the ninth such
    cancel drops the first.
  - Closing the connection with a request held leaves every budget at zero and no timer;
    `close()` returns at once, from the test and from a handler's own call, while the request
    is held between attempts, and `wait()` returns once the hold has ended.
  - A cancel sealed under another session of the same connection, naming a held request's
    key, ends that hold (D27).
  - Each call reaches the service with the hold D16 gives it: in version 2, the deadline
    the delay row's `Hold` returns from the request's arrival; in version 1, the request's
    remaining deadline; and none in a table that holds and has no row for the clone wait.
- **Tests:** seam tests in `hold_test.go`, version 2, each request a chain of `PutFH` and
  a related `Create`, the clock advanced by the test. The two failures then success, with
  `Faults.Fail` (catches no re-drive, and a second interim). The logoff between attempts
  (catches a re-drive that skips `Resolve`). The bound and its change (catch a deadline cached
  at the hold's start). The reply budget and each attempt's deadline, the second read by the
  pass-through wrapper (catch a reply reservation held across the wait, and one deadline
  shared by every attempt). The interim for a call in the service, with `Faults.Hold`
  (catches an interim only on an error). The interim of a held last member after an answered
  first, read on `Client.Balance` (catches a hold's interim handed headers, which
  `testproto` would grant credits for). The ceiling, both ways, on `Client.Balance` (catches
  a call in the service refused, and credits granted in full). The floor, lowered (D13),
  reading both budgets as a third is held (catches a charge of the record's length, and a
  receive reservation released before the hold budget took it). The full hold budget, both
  ways (catches a request held with no memory accounted, and a call in the service refused
  for memory). The four cancels (catch a cancel that answers itself, a running attempt cut
  short, a remembered cancel lost, an unverified cancel honored). The cancel under the second
  session (catches a session check in `core` that the protocol's key did not ask for, against
  D27). The record's expiry and bound (catch a record kept for ever, and an unbounded list).
  The close, `close()` called from the test and from the held request's own re-drive (catches
  a timer that fires on a closed connection, and a `close` that waits for the hold it
  ends). Eight requests held between attempts: `RecordMutations`
  shows no call until the clock moves, and no goroutine's stack, listed once the eight
  interims have arrived, is inside the hold table's code (catches a goroutine that waits per
  held request). The held gauges and the refusal counter read after each case (catch a gauge
  not lowered when a hold ends). One child span per attempt under one request span (catches a
  span per attempt with no parent, and none). The hold handed to the service in versions 2 and 1,
  read by the
  pass-through wrapper (catches a call without `WithHold`, and one bound for both); the third
  case on a table built in the test, since `testproto`'s delay row maps every cause, through
  the test protocol of T5.1a. A second chain answered while the
  first is in the service (catches the table's mutex held across the call).

### T5.5c Chain members that hold

**Subtask T5.5c estimate: 1.5 man-days.**

- **Track:** A. **Depends on:** T5.5b.
- **Needs E4:** nothing.
- **Covers:** R14, R68, R69.
- **Deliverables:** in `compound.go`, a member before the last answered an error whose row
  holds: `Pends` asked; pending, the replies so far and its interim sent as one chain, the
  members after it run on its final answer and chained after it; or `ErrNotLast` in the
  error's place. A member before the last still in the service at the interval pends without
  asking (R60).
- **Consumers:** T5.11; it completes T5.4's chains.
- **Acceptance criteria:**
  - A chain whose second of three members has the pend flag and is delayed by the service
    sends one record with the first member's reply and the interim, then one with the second
    member's final reply and the third's; the client's balance after the first record holds
    the first member's credits in full.
  - Without the flag the second member is answered the not-last status and the third runs.
  - A member held in the service past the interval pends, flag or not.
- **Tests:** seam tests, with `Msg.Pend`. The pending member (catches the later members
  run before it answered). The balance after the first record (catches the credits of an
  answered member withheld with the pending one's). The answered-at-once member (catches the
  hold row's status sent for a hold that did not happen). The member in the service without
  the flag (catches `Pends` asked for a call that may still apply).

### T5.11 Parked requests

**Subtask T5.11 estimate: 3 man-days.**

- **Track:** A. **Depends on:** T5.5b, T5.5c. T5.5c only so that, when this one lands, every
  case of `testproto`'s stateful half runs through `core` at the seam.
- **Needs E4:** nothing. `Conn.Close`'s body is E4's and calls the pipeline's `close`, which
  this subtask extends to parks.
- **Covers:** R14, R59, R70, R71, R72, R73, R74, R75, R76, R77, R78 (parks).
- **Deliverables:** in `dispatch.go`, `Park`'s body; in `hold.go`, the parked state of a
  pending record: the four refusals, the interim (none for a request already pending), the
  record released and a small-class reply reservation taken under a context already past its
  deadline, no deadline armed, `done` encoding into that buffer under `Final`'s header
  (D26), the client's cancel answered `ErrCancelled` (D17), `drain` cancelling every park and
  replacing its error, `close`
  cancelling and dropping. `testproto`'s change 7 of §1.1.
- **Consumers:** T5.1c, with which S3 is reached; E4's `Conn.Close`; the RFC 22 plan's waiting
  locks and change notifications.
- **Acceptance criteria:**
  - A parked request gets its interim at once, the receive budget returns to zero while it
    waits and the reply budget holds one small-class charge; released, it is answered under
    its key, its name in the reply.
  - Left parked while the clock passes 30 s and the hold's bound, it is still parked, and is
    answered when released.
  - A park whose name does not fit the small class is answered the overflow status.
  - A park before the last member, a ninth park, a park while draining and a park with the
    reply budget full each make no call, send no interim, and are answered their status.
  - Cancelled by its client it is answered the cancel status and no file exists; cancelled
    as its call completes it is answered success and the file exists.
  - A drain started with a request parked calls `cancel`, and nothing more is sent for it; a
    park whose call completes as the drain cancels it is answered success. A close calls
    `cancel` and leaves every budget at zero, and `close()` returns while the park waits.
  - A request held, then parked on its re-drive, has received one interim.
- **Tests:** seam tests, version 2, each a chain of `PutFH` and a related `Park` over a
  `Faults.Hold`. The budgets while parked (catch a record kept, and a reply reserved at its
  bound). The clock moved past both bounds (catches a park ended by the request deadline or
  by a hold's). The long name (catches a reply encoded outside its reservation). The four
  refusals, with `RecordMutations` empty (catch a refusal after the interim). The two
  cancels, the second by releasing the hold before the cancel is sent (catches a granted call
  reported cancelled). The drain, its success case by releasing the hold first, and the close
  (catch a park that waits for the drain's deadline, a granted call unanswered, a
  reservation kept by a dropped reply, and a `close` that waits for the park it ends). The
  single interim (catches a second one). The held gauge unchanged by eight parks (catches a
  park counted as held).

### T5.1c The pipeline on E4's connection (S3)

**Subtask T5.1c estimate: 1.25 man-days.**

- **Track:** A. **Depends on:** T5.11, so that S3 is one subtask: when this one lands, every
  case of `testproto`'s stateful half runs through `core`, and the end-to-end specification
  over a socket.
- **Needs E4:** `Server` started on a listener, accept calling `withDefaults`, `newShared`
  and `newPipeline` with the connection's `link`, the reader calling `serve`, the writer
  behind `out` passing its suite, `Conn.Settings` (a `Negotiate`), `Conn.Established` (a
  `Session`), and `Conn.Close` with D1's close order. Nothing here can land before them.
- **Covers:** R11, R14, R22 (the connection not read), R78 (a close from a handler).
- **Deliverables:** in `conn.go`, `Conn.Send`'s body: the message wrapped by the
  connection's `Security` under `h`, then handed to `out` with its `before`. Loopback tests,
  in `conn_test.go`, of the four behaviors only a socket decides (D2) and of `Conn.Close`
  entered from a handler and from the reader. The end-to-end entry removed from
  `test/adapter/known.yaml` once CI shows it passing.
- **Consumers:** E6's `New`, which waits for S3; E4's drain, close and cancellation tests,
  which run on this pipeline; T5.10.
- **Acceptance criteria:**
  - `TestConformanceEchoEndToEnd` passes: an `Echo` over `Bare` and over the full protocol
    is answered, and the server leaks nothing when it stops; its entry is gone from
    `known.yaml`.
  - On a `Bare` connection with the reply budget full and 65 `Echo` sent, 64 are admitted and
    the connection is not read until one has completed; all 65 are answered once room is
    freed.
  - With a client that sends 16 `Echo` and reads nothing, all 16 requests complete; closing
    that client's connection drops their replies and leaves every budget at zero.
  - A `Notify` sends its message through `Conn.Send`: the client verifies its checksum, and
    receives it ahead of the reply of the request it names, that reply queued while the
    client did not read.
  - `Violate`, which calls `Conn.Close` from its handler, returns, and the connection closes;
    so does a connection whose `Unwrap` says close on the reader (D6). A chain of `Violate`
    and a related `Park` has `cancel` called at once, makes no file and leaves every budget
    at zero.
- **Tests:** loopback tests in `conn_test.go`, each ending with `CheckLeaks`. The
  specification (catches a stage left unwired between E4's reader and writer). The 65
  requests, read from the stream of admissions and completions: the 65th admission comes
  after the first completion (catches a reader that reads on past a blocked `Admit`). The
  client that reads nothing, on the same stream (catches a request goroutine that writes to
  the socket itself). The notify (catches a `Send` that skips `Wrap`, which the client then
  refuses for its checksum, and a `before` the writer ignores). `Violate`, `Unwrap`'s close
  and the chain of `Violate` and a related `Park`, each under a backstop deadline (catch a
  `Conn.Close` that waits for the goroutine that called it, so that the test never ends, and
  a park registered on a closed connection, which nothing would ever end).

### T5.10 The pipeline benchmark and its gate

**Subtask T5.10 estimate: 1.25 man-days.**

- **Track:** A. **Depends on:** T5.1c.
- **Needs E4:** as T5.1c.
- **Covers:** R87, R92.
- **Deliverables:** in `bench_test.go`, `BenchmarkPipeline`, which calls `b.ReportAllocs()`,
  over loopback against `vfstest.Null`, with a registry, a tracer that samples nothing and a
  discarding logger, in three cases: an `Echo` over `Bare`, which reaches no service; a
  version 1 compound of `PutFH` and `Create` over the full protocol, its `Negotiate`,
  `Session` and `Tree` sent before `b.ResetTimer()`, the service reached through a
  pass-through declared in the test that derives one `context.WithCancel` child per call, as
  a real service does; and the same compound with its `Create` answered `vfs.ErrDelay` by
  that pass-through, which version 1 answers at once through a row late in `Map`'s scan,
  past every row of `core`'s refusals. Its runs are compared with `benchstat` over
  `-count=10`. `TestPipelineAllocs`,
  which reads `testing.AllocsPerRun` over the same three, skips under the race detector as
  `testproto`'s gate does, and fails above the numbers in `testdata/allocs` or when one could
  be lowered; `./internal/adapter/core/` added to `unit/adapter-allocs`' command in
  `test/suites.yaml`.
- **Consumers:** every later change to the connection, pipeline or pool code, which runs it and
  reports its allocations and its mutex and block profiles (RFC 20 §6.9).
- **Acceptance criteria:** the gate passes at the checked-in numbers; one allocation added per
  request fails it, in each case; a number checked in above the measurement fails it; with
  the request unsampled, the test's clock is read as often as with no tracer at all.
- **Tests:** the gate is the test. It catches an allocation added to the request path, a
  series resolved per request, a span call that allocates or reads the clock when unsampled,
  a request context without `AfterFunc`, whose derived child in the compound case then costs
  a goroutine and its allocations, and a service error mapped through a path that allocates
  or a scan that does.
  `Null` is a load, not a double.
- **Rules:** rule 4: this is the pipeline's gate; it lands last so the numbers cover holds and
  parks.

## 7. Order of work

```mermaid
flowchart TD
  S2(["S2 testproto and the import test in CI"]):::sync --> T5_7["T5.7 error tables"]
  T5_7 --> T5_2a["T5.2a NewOpTable"]
  T5_2a --> T5_9["T5.9 series"]
  T5_9 --> T5_6["T5.6 middleware chain"]
  S2 --> T5_5d["T5.5d WithHold"]
  S2 --> T5_3["T5.3 cap and reply reservation"]
  E3(["E3 bodies of pkg/resources"]):::sync --> T5_3
  E3 --> T5_5a["T5.5a Recharge"]
  T5_2a --> T5_1a["T5.1a the pipeline, at its seam"]
  T5_9 --> T5_1a
  T5_6 --> T5_1a
  T5_3 --> T5_1a
  T5_1a --> T5_1b["T5.1b span, logs and counts"]
  T5_1a --> T5_2b["T5.2b row checks"]
  T5_1b --> T5_4["T5.4 compound runner"]
  T5_2b --> T5_4
  T5_4 --> C1(["after T5.4: testproto reaches the service at the seam"]):::sync
  T5_4 --> T5_8["T5.8 request context"]
  T5_5a --> T5_5b["T5.5b held requests"]
  T5_5d --> T5_5b
  T5_8 --> T5_5b
  T5_5b --> T5_5c["T5.5c chain members that hold"]
  T5_5b --> T5_11["T5.11 parked requests"]
  T5_5c --> T5_11
  T5_11 --> C2(["after T5.11: every stateful case runs through core at the seam"]):::sync
  E4(["E4 Server, accept, reader, writer"]):::sync --> T5_1c["T5.1c the pipeline on E4's connection"]
  T5_11 --> T5_1c
  T5_1c --> C3(["S3: core serves testproto end to end"]):::sync
  T5_1c --> T5_10["T5.10 benchmark and gate"]
  classDef sync stroke:#2c5aa0,stroke-width:3px,stroke-dasharray:4 2
```

Fifteen subtasks, 28 man-days, need no socket:

| When | Subtasks | Man-days |
| --- | --- | --- |
| at S2, now | T5.7, then T5.2a, then T5.9, then T5.6; T5.5d at any time | 7 |
| once E3's two pull requests have merged | T5.3 and T5.5a, then T5.1a, T5.1b and T5.2b, T5.4, T5.8, T5.5b, T5.5c, T5.11 | 21 |
| once E4's `Server`, accept, reader and writer have landed, its writer passing the suite of `out`, with `Conn.Settings`, `Conn.Established` and `Conn.Close` | T5.1c, T5.10 | 2.5 |

**The critical path** is T5.7, T5.2a, T5.9, T5.6, T5.1a, T5.1b, T5.4, T5.8, T5.5b, T5.5c,
T5.11, T5.1c and T5.10: 27.25 of the 30.5 man-days, all serial, with S3 reached at T5.1c
after 26. Only T5.5d, T5.3, T5.5a and T5.2b are off it, and T5.3 joins it if E3 merges less
than 1.25 man-days, T5.3's own figure, before T5.6 lands. Only T5.1c and T5.10 wait for E4, 2.5
man-days after it lands. The least certain subtasks, T5.4, T5.5b and T5.1a, are on the path.
Track C taking T5.7, T5.9 and T5.6 shortens it only while track A has something else to build;
the error tables go first because two subtasks and `testproto`'s tables build on them.

## 8. Suggested pull requests

| PR | Subtasks | Man-days | After | Why together |
| --- | --- | --- | --- | --- |
| 1 | T5.7, T5.2a | 2.75 | none | the two rows every table must have, `ErrDecode` and `ErrCancelled`, with their amendments; #4194 delivered the rest of both subtasks |
| 2 | T5.9 | 2.25 | PR 1 | the registry's calls and `core`'s series, with D18's amendments |
| 3 | T5.6 | 1.5 | PR 2 | the chain |
| 4 | T5.3, T5.5a | 1.75 | E3 | what reserves: the cap, the reply reservation and its ceiling, `Recharge`; open beside PR 1 to PR 3 |
| 5 | T5.5d | 0.5 | none | `WithHold` and its reader, which RFC 17's clone wait builds on; open at once |
| 6 | T5.1a, T5.1b, T5.2b | 5 | PR 3, PR 4 | the suite of `out` first, then one request at the seam with its observability and its row checks: D6's amendment |
| 7 | T5.4 | 4.25 | PR 6 | the runner, its walk and its fuzz target, with the amendments of D6's chain rule, D8, D10 and D11 |
| 8 | T5.8, T5.5b, T5.5c | 7 | PR 4, PR 5, PR 7 | the request's context and the holds built on it |
| 9 | T5.11 | 3 | PR 8 | parks, on the pending table |
| 10 | T5.1c | 1.25 | PR 9, E4 | the pipeline on E4's `Conn`: the end-to-end specification green, S3 |
| 11 | T5.10 | 1.25 | PR 10 | the gate, measured on the finished path |

A suggestion only: one pull request per subtask works as well.

## 9. Open points

- **The architect's answers** (#4184, 13 Nov) are folded into §4 as he gave them: D1's split
  with its two conditions, D2, D5, D6's `Stages.Errors` with `Unwrap`'s close, D7, D9, D10,
  D11's deferral of the session reply budget, D12, D16, D17 and D18's field. Epic 5 goes
  ahead before epic 4's breakdown, D1 being its input and not renegotiated there.
- **Added after the architect's answers, waiting for his confirmation:** in D1, the `link` in
  place of the address and context read from `Conn`, the queue's bound, and `Conn.Settings`
  and `Conn.Established` needed only from T5.1c (§1.1's changes 10 and 11), all for the seam
  tests D2 allows; D6's chain rule; D8's release of each member's admission; D11's refusal
  past the reply pool's ceiling, with its own error; D18's once per server and table, through
  a map every request indexes with no lock, in place of the `sync.Once` per table he asked
  for, since two servers can share a table and each registers into its own registry. Also
  his to say before T5.2a lands: `ErrCancelled` follows RFC 20's spelling and `testproto`'s
  `StatusCancelled`, while Go's is `context.Canceled`.
- **Rules this revision settles that belong in RFC 20**, each an amendment the subtask named
  lands with its code, as rule 9 has it: the reply to a header that failed `Decode` or
  `Unwrap` going out unwrapped, and `Unwrap`'s nil header closing the connection (§4.7, §4.1,
  §5.5; T5.1a, D6); a chain member `Unwrap` refuses splitting the chain's replies, and a
  member's admission released before the next is admitted (§4.7; T5.4, D6, D8); a compound
  whose summed bounds pass the reply pool's ceiling refused `ErrReplyTooBig` (§4.3, §4.7;
  T5.3, T5.4, D11). D27's cancel key, and the queue behind `out` counting every message (D1),
  need none: the first is the protocol's key by §4.7, the second RFC 20 §6.1 and §6.2 already
  say.
- **Findings for other documents, not edited here.** RFC 21 §11.5 lacks a row for
  `ErrReplyTooBig`: `NFS4ERR_REP_TOO_BIG` for NFSv4.1 and `NFS4ERR_RESOURCE` for NFSv4.0, a
  finding for an RFC 21 pull request, its rows the RFC 21 plan's (D11). The RFC 17 plan's
  piece 11 still lists V30 among what it must pass, though it merged (#4157) without the held
  wait V30 tests: a finding for that plan (D16).
- **Changes from the epic's first description in who owns what.** `Conn.Close`'s body, which
  that description listed under T5.11, is E4's (§1.2): this epic keeps the pipeline's
  `close`, which T5.1a builds. `Conn.Send`'s body, which it left to E4's writer, is T5.1c's:
  it wraps and calls `out`, and the order against a queued reply stays the writer's. The
  connection, close and byte series are E4's, and so are D1's calls into the pipeline.
- **Against the epic's first description.** That description had eleven subtasks at 26.5
  man-days. The eleven ids stand for the same work. T5.2 is split by order: T5.2a, which
  #4194 delivered in part, builds the table before the pipeline, and T5.2b checks rows in
  it. T5.1 is split by concern: the pipeline at its seam, its observability, and what a
  socket decides, which with E4 turns the end-to-end specification green (T5.1c). T5.5 is
  split in four: it would pass the 5 man-day ceiling whole, T5.5a needs E3 and no
  connection, and T5.5d needs neither. T5.4 is whole: its earlier split by what needs a
  connection went with D2. The total is 30.5 man-days: 26.5, plus about 1 for the amendments
  of D6, D7, D10, D12, D17 and D18 with their golden files and eleven changes to `testproto`,
  plus 0.5 for the suite of `out` and its recorder (D2), plus about 2 for what the reviews
  and the architect's answers added (R93's walk and `FuzzRecordWalk`, `Unwrap`'s close and the
  chain member it refuses, the close that never blocks and its test, the reply reservation's
  ceiling and its error, the series map, T5.10's compound and refused cases), plus about 1.5
  for the pipeline past the 4 man-days this document's first revision gave T5.1 (1 for
  `Send`, D6's paths and the request's observability, which its second revision found too
  much for 4, and 0.5 for T5.1c's loopback tests of what a socket decides), less about 1 for
  what is dropped: the totality helper (D5), the session reply budget (D11) and the three
  connection series. Against the second revision, at 17 subtasks and 30 man-days: D2 tests
  each part once (about 2.25 less), the suite of `out` adds 0.5, the third review and the
  answers about 2, and the admission test on a socket 0.25. `ReplayCache`'s own copy, listed
  under T5.7, was E2's and E3's and is done.
- **Edits to the program's implementation plan and to the epic 1 and epic 2 documents**,
  made in this document's pull request:
  - the ledger's row for each protocol's error-table rows and totality test now names the
    helper it calls, and the coverage rows for RFC 20 §4.5 and §7.3 send the helper there
    (D5);
  - the row for NFSv4.1's slots now names the session's reply budget and the bound by a
    quarter of the slots on calls waiting under `vfs.WithHold`, and the coverage row for
    RFC 20 §4.3 says what of the reply budget this epic builds (D11, D16);
  - the row "`dittofs_client_connections_active{protocol}`" is E4's, a new row beside it
    gives E4 `adapter_connection_closes_total` and `adapter_bytes_total`, and the coverage
    row for RFC 20 §10 names them;
  - a new row gives the RFC 21 and RFC 22 plans `adapter_translation_refused_total`,
    `adapter_record_decode_failures_total` and `adapter_record_replays_total`;
  - a new row points the test of `vfs.WithHold` at its reader to RFC 17's V30, owned by the
    RFC 17 plan with no piece of it building the held wait yet: piece 11, which lists V30,
    merged (#4157) without it (D16);
  - the coverage row for RFC 20 §4.7 gives E4 `Conn.Close`'s body;
  - the rows for `vfs.Grace{Until}` and `vfs.Shares.MoveTarget` name RFC 17's plan as their
    owner, as the epic 1 document's notes do, with no piece of it building either yet. The
    first's trigger is the RFC 22 plan's first row that bounds a hold by a grace's end:
    `core` hands the error to `ErrorRow.Hold` and reads no field of it, so T5.5b does not
    wait for the type. The second's is E4's T4.5, which calls it (D23);
  - the row "`metrics.Registry` an alias of `pkg/metrics`' `Metrics`" says what T5.9 adds to
    it and what is still RFC 25's (D19);
  - the program's own descriptions of epic 5 and epic 4 agree with those rows: epic 5's
    loses the totality check, the session reply budget and `Conn.Close`'s body and names its
    split subtasks; epic 4's gains the connection, close and byte series, `Conn.Close`'s
    body, D1's calls with the `link`, and a writer that passes the suite of `out`;
  - epic 5's estimate line and its row in the program's §2 carry this document's figure,
    17 subtasks and 30.5 man-days, and the program's total follows; the architect accepted
    the earlier figure against 26.5 (he quoted 29), and 30.5 is this revision's; epic 4's
    breakdown prices what it gained;
  - the program's blockers and tracks say that the pipeline's tests run at its seam, and
    only those that need a socket on E4's `Conn` (D2); its sketch of a connection calls
    `serve`;
  - the epic 2 document's citations of T5.1, T5.2 and T5.5 name the part each means, and
    each names the subtask whose test uses what it cites.
- **What the first description names that the branch does not have.** `vfs.WithHold`: T5.5d
  adds it. The service's wait behind a clone that reads it: not on the branch, so T5.5b
  asserts on what reaches the service (D16). `vfs.Grace{Until}`: not on the branch, and not
  needed here. `vfs.Shares.MoveTarget`: not on the branch; its owner is RFC 17's plan, and
  E4 calls it (D23). `Buf.Recharge`: T5.5a adds it. The bodies of `pkg/resources`: E3's
  first pull request (#4177, the budgets) has merged and its second (#4178, `BufPool`) is
  open; T5.3 and everything after it need both. `vfstest.Null`, `memory.New`,
  `vfstest.Faults`, `vfstest.RecordMutations` and `adaptertest.CheckLeaks` are on the branch
  as named. #4194 delivered T5.7 and T5.2a but for `ErrDecode` and `ErrCancelled`.
- **Not taken from the third review, with the reason.** Checking D22's switched-off names
  against the tables when settings load: `settings.Adapter`'s check already refuses an empty
  or repeated name, and a name no table has is the protocol's to refuse in its shape's
  `Validate`, which knows its operations; `core` first sees a table at `Route`, after the
  settings loaded. A counter of dropped replies: RFC 20 §10.1 names no such series, and a
  dropped reply already ends its request span as dropped (R88, T5.8); a series would need an
  amendment of §10.1 first.
- **Not confirmed.** What `memory.New` answers a call whose context has ended: T5.8 asserts
  that the reply comes and when, not its status.
- **Least certain:** T5.5b, for the number of paths through the pending table (interim racing
  the answer, cancel racing a re-drive, close racing both); T5.4, the largest subtask, for a
  chain's stop at a `NoReply` member, the least exercised path of the surface, and for what
  `FuzzRecordWalk` finds; T5.1a, whose seam harness (the recorder over a `net.Pipe`, the
  stream of admissions) every later subtask reuses; T5.10's figure, unknown until the path
  exists. T5.4, T5.5b and T5.1a are on the critical path (§7).
