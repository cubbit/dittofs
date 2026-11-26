# Epic 22: engine offload E2E green

**Sources:** [RFC 8](../docs/internals/rfcs/rfc-8-engine.md) §15's opening paragraph (every check
against the engine as production composes it), the two MUST NOTs under §15.3 and §15's Suites
paragraph; the rows of §15.1 to §15.3 that epic 12 catalogues, as their burn-down; §15.4 (B1 to B4,
B6 and B8; B5, B7 and B9 again with offload composed; its `decision:`, its Method paragraph); §11.4's
clock sentence; §14's `engine_stage_busy_ratio`; §16's questions 1, 4 and question 2's read-ahead cap,
with §6.2, §6.3, §7.3 and §7.4, which state them. [RFC 3](../docs/internals/rfcs/rfc-3-syncer.md)
§8.1's Soak row and §8.6's sentence placing it. [RFC 13](../docs/internals/rfcs/rfc-13-configuration.md)
Appendix B's rows for `offload.max_age`, `offload.retry_min`, `offload.retry_max`,
`offload.retry_max_pressure`, `readahead.max_blocks`, `readahead.max_bytes` and the fill low-water mark,
their "settled by" column. [RFC 14](../docs/internals/rfcs/rfc-14-open-state.md) §12's L9 and
[RFC 17](../docs/internals/rfcs/rfc-17-vfs.md) §6's V20 (its offload half), rows of epic 12's catalogue.
The [RFC index](../docs/internals/rfcs/rfc-index.md), "Test tiers": what must not stand in, a check
that did not run is a failure, recording results, a proposed target replaced once by measurement.
[RFC 29](../docs/internals/rfcs/rfc-29-test-harness.md) §4.3 (tiers), §4.7 (known failures and the
unexpected pass) and §4.10 (benchmarks and long runs). Every citation is `develop`'s text. The code
lands on `feat/storage-v2`, and the paths below are paths there.
**Track:** the team.
**Epic 22 estimate: 8 subtasks, 25.25 man-days.**
**Depends on:** **S7**, node composition built (epic 21): `internal/node/content` builds a share's
S3 store, `codec.Store` over it and the syncer from a store configuration, and `syncertest`'s Group A
suite runs against LocalStack behind a fault proxy, so a composition outside the node can build the
same stores. **S6**, journal offload half built: every row, the soak and every benchmark run over the
real journal. **S4**, offload complete in the fixture (epics 13 to 20), with **S5**'s two halves.
**Epic 12**: the rows in `engine-conformance` and `test/engine/known.yaml`, and `enginetest`'s
`WithCarver`, `WithChain`, `WithSyncer` and `WithRefs` and `(*Fixture).FaultRemote`, which epic 16
makes real. **The RFC 4 plan's epic 3**: `pkg/remote/s3/s3fault` (the transport under the S3 client,
its `Inject` rules and its record of the requests that reached it, `FreshBucket`), the `localstack`
needs entry and `LOCALSTACK_HTTPS_ENDPOINT`. **The first part's epic 10**: `engine-soak` with its oracle
and fault log, `engine-bench` with `composeBench`, B5, B7 and B9, and its T10.5 baseline of those
three, which T22.8 compares against.
**Blocks:** the release: the offload programme (#5488) closes on this epic. The RFC 3 plan's ledger
row for the soak (RFC 3 §8.1) closes with T22.7. Later changes of the settled defaults, and the first
real-service run, are read against the series this epic starts on the reference box.

---

## 1. Goal

Epic 22 closes the offload part. It turns every red row of the part green over the real journal and
the composed components, runs the crash-and-restart soak with offload composed and the remote store
failing on cue, runs the hours-long soak against LocalStack that RFC 3 hands to the engine, and runs
the nine §15.4 benchmarks against LocalStack with a recorded baseline. It writes little engine code:
the fixes the real journal shows, the soak's remote faults, the benchmark composition with offload,
six benchmarks and the settling sweeps.

**At the end of the epic:**

- `test/engine/known.yaml` holds no entry naming an issue of epics 11 to 22 or #5757, and
  `test/engine/soak-known.yaml` holds none; `engine-conformance` is green on every pull request
  (T22.4, T22.5);
- `engine-soak` replays its seeds with offload, eviction and remote reads composed, faults at every
  pipeline step that calls a dependency and partitions to the remote store and to the metadata store,
  on every pull request (T22.6);
- `engine-soak`'s `localstack` option runs for hours, nightly, against LocalStack with outages on a
  cycle, and after each outage tasks, open connections, `inflight_bytes` and queue depth return to
  idle (T22.7);
- `engine-bench` runs B1 to B9 over one composition with offload against LocalStack; every count
  that follows from a rule fails the benchmark when broken, every proposed figure is reported; the
  baseline of all nine is recorded on the reference box, and B5, B7 and B9 are compared with the
  first part's (T22.1, T22.2, T22.8);
- RFC 13 Appendix B's rows for the fill low-water mark, `offload.max_age` and the `offload.retry_*`
  keys carry their settled values; RFC 8 §16's questions 1 and 4 are answered, and question 2's
  read-ahead cap has its LocalStack figures beside it; RFC 8 §15's Suites paragraph names what runs
  where (T22.3, T22.8).

### 1.1 In scope

The burn-down of `test/engine/known.yaml` and `soak-known.yaml` for this part, the re-pointing of
entries whose epic closed, and the coverage table of epic 12's catalogue; engine fixes the real
journal or the composed components show, each with its unit test; the soak's remote faults and
partitions, and its idle checks; `enginetest`'s store option and syncer figures for the LocalStack
run; `engine-soak`'s `localstack` option; `composeBench` with offload, the raw-put and raw-get
baselines; B1, B2, B3, B4, B6 and B8; B5, B7 and B9 rerun; the settling sweeps; `engine-bench`'s
guard and needs; the baseline; the RFC 8 §15 and §16 and RFC 13 Appendix B edits that record the
results; `tests.yml`'s select and `dt docs`.

### 1.2 Out of scope

- Turning each row green in the first place: the middle epics (13 to 20) whose issue its entry
  names. This epic takes only what is still red when they close, or what S5 and S6 hold red past them.
- #5757's four entries: epic 18 turns them green and closes #5757; this epic only checks none is left
  (R11).
- The first part's #5261 entries (the journal's lost-tail refusal): the journal's, not this part's.
- The benchmarks run per change against an emulator: the plan's §6.1 row "The benchmarks run per
  change against an emulator" (unowned), whose trigger is this epic's recorded baseline.
- §15.4's daily runs against each real service, and the baselines that replace its proposed targets:
  the plan's §6.1 row "§15.4's daily runs against each real service" (unowned).
- B9's snapshot-cut run: the plan's §6.1 row "Snapshot-hold ordering in the pipeline, … the cut's
  `PrepareCut` and `CommitCut`" (RFC 12).
- RFC 8 §16 question 2's yield mechanism, and every pre-warm leg: the plan's §6.1 row "`PreWarm` and
  the speculator's pre-warm" (unowned).
- The engine-cost benchmark of §15.4's `ponytail:`: the first part's ledger row (unowned).
- Bucket-setting drift: the plan's §6.1 row "Detecting bucket-setting drift" (RFC 9).
- The rows that need a second node or a shard move: the plan's §6.1 row "The cluster rules".
- RFC 13 rows whose settling measurement names a §15.4 benchmark but which RFC 8 §16 does not ask
  this epic to answer: the speculation budget, `eviction.repack_threshold` and the eviction occupancy
  target. No epic of the part owns them; §9 point 6.

### 1.3 Names used here

- **E12**, epic 12, red rows and test foundations: the catalogue of 50 rows in `engine-conformance`,
  their `known.yaml` entries, and `enginetest`'s component options and controls.
- **E13** to **E20**, the middle epics: the scheduler, the work queue, the pipeline, passes, remote
  reads, local space, restart and the reconcile, and health, drain and salvage. Each turns its rows
  green and removes their entries.
- **E14**, epic 14, the work queue, whose D5 registers the offload and removal sources' limits as
  constants this epic's benchmarks measure; **E16**, epic 16, which makes `FaultRemote` real and
  builds the gates' switch the §3.2 row reruns under; **E17**, epic 17, whose fill bound and read
  table size B4 measures; **E18**, epic 18, which closes #5757.
- **E21**, epic 21, the integration epic: the node composes offload.
- **S4**, offload complete in the fixture: epics 13 to 20 done.
- **S5**, offload components built: the RFC 2 to RFC 5 plans' implementation epics, in two halves.
- **S6**, journal offload half built: the journal plan's epics 9b and 7b.
- **S7**, node composition built: the end of epic 21.
- **S3** in this document is the storage service's protocol (the S3 store), never the plan's
  synchronisation point of that name, which is not used here.
- **E1, E4, E5, E6, E7, E8** in RFC 13 Appendix B's "settled by" column are RFC 8 §15.4's B1, B4,
  B5, B6, B7 and B8, under an older numbering that collides with RFC 8 §13's invariants E1 to E42
  (§9 point 5).
- **The first part**: the journal-only engine, epics 1 to 10 of RFC 8's first plan; its epic 10
  built the soak, `engine-bench` and B5, B7 and B9.
- **The reference box**: the harness's dedicated host, which a cell reaches with
  `needs: [linux, host:dedicated]`, and where the first part's baselines were taken.
- **#5757**: the issue the first part's four share-limit and clone entries name. **#5261**: the
  issue its two lost-tail entries name.
- **D1** to **D12** are this epic's decisions (§4); another epic's are written with it named.

## 2. How the general rules apply

The rules are those of
[the offload plan's §1.5](rfc-8-offload-engine-plan.md#15-general-rules-for-each-epic), cited by
number and title and not copied.

| Rule | Subtasks | How |
| --- | --- | --- |
| rule 1, **Independent unit tests** ([`CLAUDE.md`, "Writing tests"](../CLAUDE.md#writing-tests)) | T22.5, T22.6 | each engine fix the burn-down makes lands with a unit test over the first part's in-memory journal and the components' real implementations; the soak's oracle keeps its unit test over hand-made histories, extended for remote faults |
| rule 2, **Fuzz the key and ref decoders the queue's rebuild reads** | none | no decoder is written here; the decoders are epics 14 to 16's, with their targets |
| rule 3, **Observability follows RFC 25** | T22.1, T22.2, T22.6, T22.7 | the benchmarks and the soak read RFC 8 §14's and RFC 3's series only through `Stats` and the syncer's figures, by the names the RFCs give them (`engine_stage_busy_ratio`, `inflight_bytes`, the queue depths); nothing new is exported |
| rule 4, **Profiling** | T22.6, T22.7, T22.1 | the soak runs under the fixture's `prof.Watch`, and its idle check reads goroutines and descriptors from `internal/prof`; the benchmark composition keeps the first part's heap slack. The allocation gate on the two hot paths is epic 15's `unit/engine-allocs` |
| rule 5, **Public interface and internals** ([`CLAUDE.md`, "Verifying a change"](../CLAUDE.md#verifying-a-change)) | all | the benchmarks measure through `vfs.Service`; a cold read's fetch is proved at the store, from `s3fault`'s record, not from an engine counter (D7); the soak's oracle reads through the filesystem service |
| rule 6, **Known failures shrink** | T22.4, T22.5 | every entry that turns green leaves in that change; an entry whose epic closed moves to this epic's issue (D2) |
| rule 7, **v2 ignores v1, and the engine imports only what RFC 0 §1.2 allows** | T22.1, T22.6, T22.7 | only test files change outside engine fixes: `internal/vfs/integration` and `enginetest` import the content wiring and `s3fault`, which tests may; no production import changes, and v1 is not read |
| rule 8, **Real dependencies for properties of those dependencies** ([`CLAUDE.md`, "Writing tests"](../CLAUDE.md#writing-tests)) | all | every row, the soak and the benchmarks run over the real journal on a real directory; the benchmarks and the `localstack` soak over the S3 store against LocalStack; the `pr` soak over RFC 4's in-memory store, which RFC 4's suite holds |
| rule 9, **Code comment conventions** ([`CLAUDE.md`, "Code comments"](../CLAUDE.md#code-comments)) | T22.1, T22.2, T22.5, T22.7 | `decision:` markers at D5's reported targets, D7's cold-read check, D4's outage driver and wherever the real journal forces a behaviour the RFC leaves open; no RFC 8 marker is this epic's to place |
| rule 10, **A test must be able to fail** ([`CLAUDE.md`, "Writing tests"](../CLAUDE.md#writing-tests)) | all | each asserted count is seen to fail on a planted break before its pull request opens (named in each subtask's Tests); a reported figure fails nothing, and §4 says why |
| rule 11, **A symbol is exported only if the RFC's interface uses it** | none | nothing production is exported; the fixture's option and syncer figures are test API in `enginetest` |
| rule 12, **Each subtask creates only what it declares** | all | each helper lands with the benchmark or soak run that calls it; the fixture's store option lands with the `localstack` run, its first caller |

## 3. Requirements and invariants

Every obligation the sources place on this epic. Where the check is another epic's, the row says so.

**The burn-down**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R1 | Every Group A row epic 12 catalogues passes over the real journal and the composed components, its entry removed: §3.2 no gate is safety, §6.8 per-block reporting, §4.1 verifier inputs (the offloaded loss), §6.4 guard across a removal, §8.3 transfer survives a removal, §2.5 restart re-offer, journal gone and attach after a device loss (the acknowledgement), metadata reconcile, `Bind` takes over the reconcile's handle, unsuitable journal at start, unread range at start, reseed gates offer and eviction, §6.6 minted names and intents, §6.7 widening and stretch ends, §9.1 clone keeps writes outside its extent (offload half) and no pre-clone fill (a demand read's leg), §6.4 guard is not a fence, §7.1 stale ref and the order, §7.2 streaming, verified, fill cannot fail a read and fill loses to a write, §7.7 re-resolution, §5.1 durable commits and overwrite waits for existence (offload half), §7.6 zero ref under an overwrite, §9.1 Lost source run carried and one source state, §2.5 clone resumed at a full limit (remote half), §10.2 retry later, §10.4 destroyed material | test row | RFC 8 §15.1; plan §1.4 | T22.5 |
| R2 | Every Group B row epic 12 catalogues passes the same way: §11.1 health per direction, offload health and `stranded_export_failed`, §10 occupancy triggers eviction, §10.2 refusal loop, §10.3 repack reserve, §10.2.1 pacing law, §2.3 removal during a reconcile, §2.5 join, §8.1 removal wedge (the undecodable ref), §11.4 drain names its blocker, §1.1 no cross-share slowdown (cold reads), §6.1.1 a stuck run | test row | RFC 8 §15.2; plan §1.4 | T22.5 |
| R3 | The two Group C rows pass: §2.5 reseed, and §2.4 settings in the form epic 12 writes it | test row | RFC 8 §15.3; plan §1.4 | T22.5 |
| R4 | The other plans' rows pass: L9 (an open and a lock never stop eviction or reclamation) and V20's offload half | test row | RFC 14 §12; RFC 17 §6; plan §1.4 | T22.5 |
| R5 | "§3.2 no gate is safety" passes whole: every other Group A row passes with suspension, pins and health gating off and eviction as eager as possible | test row | RFC 8 §15.1 | T22.5 |
| R6 | The four extensions pass in their offload halves: `TestRFC8A_CloneResumedAtAFullLimit`, `TestRFC8A_RetryLaterAtTheShareLimit`, `TestRFC8C_Settings` and `TestV20` | test row | plan §1.4 | T22.5 |
| R7 | A sink that always succeeds never stands in for Group A or the pipeline's checks, and a single-file rig never stands in for §8.3 | MUST NOT | RFC 8 §15 (under §15.3) | T22.5, T22.6 |
| R8 | Every check runs against the engine as production composes it: the rows, the soak and the benchmarks build the components through the node's content wiring | invariant | RFC 8 §15 (opening paragraph); RFC index, "What must not stand in" | T22.1, T22.5, T22.6, T22.7 |
| R9 | A known failure that passes fails the run, so an entry leaves `known.yaml` in the change that turns its test green, a journal or component change included | invariant | RFC 29 §4.7; plan rule 6 | T22.4, T22.5 |
| R10 | Every planned entry names an open issue of the epic that turns it green; an entry whose epic closed names this epic's | invariant | RFC 29 §4.7 | T22.4 |
| R11 | When the epic is done, `test/engine/known.yaml` holds no entry naming an issue of epics 11 to 22 or #5757, and `test/engine/soak-known.yaml` holds none | invariant | plan, epic 22 ("Done when") | T22.5 |

How R7, R9 and R11 are enforced once this epic lands: R7 by epic 12's fixture, whose rows reach a
component only through the content wiring and never through a double (E12's D1), and by the rows of
R1 that run two passes at once; R9 by the harness, which fails a run whose known failure passes; R11
by T22.5's acceptance check, since a file that still holds a red row's entry passes the harness.

**The soak**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R12 | Under crashes, restarts and fault schedules, with offload, eviction and remote reads composed, every acknowledged synced write reads back its bytes or reads **Lost**, in the three cases the first part's oracle defines; a read that a remote fault reached answers an error, never wrong bytes | invariant | RFC 8 §5.1, §7.1, §7.2 | T22.6 |
| R13 | The soak's faults reach every pipeline step that calls a dependency (the intent and commit transactions, the put, the mark, the fetch), and partitions cut the remote store and the metadata store for spans of calls | invariant | plan, epic 22; RFC 8 §6.3 | T22.6 |
| R14 | Nightly, for hours, against a local emulator of the remote service with outages injected on a cycle, the soak asserts that tasks, open connections, `inflight_bytes` and queue depth return to idle after each outage | test row | RFC 3 §8.1 (Soak) | T22.6, T22.7 |
| R15 | RFC 3's soak runs in the engine's soak suite, on the engine as the node composes it | invariant | RFC 3 §8.1, §8.6 | T22.7 |
| R16 | A check that did not run is a failure: a run that needs LocalStack fails, naming `LOCALSTACK_HTTPS_ENDPOINT`, when it is unset, and never passes having measured nothing | invariant | RFC index, "A check that did not run is a failure"; plan rule 8 (the variable) | T22.1, T22.7 |
| R17 | The `pr` soak fits its tier's budget, shared with `engine-conformance`, and every soak cell is selected by the workflow that runs the branch, or the branch never runs it | invariant | RFC 29 §4.3 | T22.6, T22.7 |

**The benchmarks**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R18 | B1: sustained writes of non-deduplicating data larger than the journal; offloaded MiB/s reported as a fraction of raw puts of the same block size on the same store at the same pool sizes, measured in runs interleaved with it; target 80 % | bound (reported) | RFC 8 §15.4 (B1) | T22.1 |
| R19 | B2: 10⁴ and 10⁵ files of 64 KiB created, written and closed; files/s within 20 % across directory sizes (reported); puts per file at most 64 KiB over the block target, rounded up (asserted) | bound | RFC 8 §15.4 (B2) | T22.2 |
| R20 | B3: B1 below and above the soft threshold; p99 submission latency ≤ 50 ms, max ≤ 500 ms, offloaded throughput within 10 % of B1 | bound (reported) | RFC 8 §15.4 (B3), §10.2.1 | T22.1 |
| R21 | B4: random 4 KiB reads of evicted content take one request per read (asserted) with p99 ≤ 2 store round trips (reported); sequential reads fetch at most 1.1 bytes per byte read (reported) | bound | RFC 8 §15.4 (B4) | T22.2 |
| R22 | B6: one file written sequentially to 20 × the journal's capacity; offloaded MiB/s over the last tenth within 10 % of the first tenth (reported); its extents made offloaded while it is still written, block by block, never only at the end of a pass (asserted) | bound | RFC 8 §15.4 (B6), §6.8 | T22.1 |
| R23 | B8: cold sequential 8 MiB reads, their chunks fetched at once; p99 ≤ the slowest chunk's fetch and verification plus 10 % | bound (reported) | RFC 8 §15.4 (B8), §7.2 | T22.2 |
| R24 | B5, B7 and B9 run again over the composition with offload: B5's and B7's counts asserted as the first part asserts them, their latencies and B9's figures reported, each compared with the first part's baseline | bound | RFC 8 §15.4 (B5, B7, B9); plan, epic 22 | T22.8 |
| R25 | The method: the raw link, raw object storage and the full stack measured on the same hosts at the same time, each reported as a fraction of the one below; data that does not deduplicate; every stage's occupancy (`engine_stage_busy_ratio`) beside B1; the network distance to the store recorded; runs long enough to exhaust the local device's write cache | invariant | RFC 8 §15.4 (Method), §14 | T22.1 |
| R26 | A benchmark of the write path stops its clock at `WaitOffloaded`, never at the last acknowledgement | MUST | RFC 8 §11.4 | T22.1, T22.2, T22.8 |
| R27 | §15.4's figures (80 %, 20 %, 50 ms, 500 ms, 10 %, 2 round trips, 1.1, 2×) are release bars proposed, not measured; the first baseline run against each service replaces them, a figure that run misses is a design finding, never relaxed; a LocalStack baseline replaces none of them | invariant | RFC 8 §15.4 (`decision:`); RFC index, "A proposed target is replaced once"; plan, epic 22 | T22.1, T22.2, T22.8 |
| R28 | Every result is recorded in absolute numbers with the commit, the box (CPU, memory, device and its raw figures), the operating system and filesystem with its mount options, and each row's concurrency; one reference box carries the series, and results from another box are compared only with themselves | invariant | RFC index, "Recording results" | T22.1, T22.8 |
| R29 | Performance is tracked, not gated: the benchmarks run in the `manual` tier as Go benchmark output compared with `benchstat`; a baseline is replaced only through a pull request, with the commit and box it was measured on beside it | invariant | RFC 29 §4.3, §4.10 | T22.8 |
| R30 | A cold read proves it reached the store: no cache between the check and the store answers it, and the check asserts the fetch happened | invariant | RFC index, "What must not stand in" | T22.2 |
| R31 | The remote tier under test is a local emulator of the remote service; the scheduled real-service half is deferred (§1.2) | invariant | RFC index, "What must not stand in"; RFC 8 §15.4 | T22.1, T22.7 |

**The open questions and the settings they settle**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R32 | Question 1: fill-always, the fill rule and fill-never compared on a large-file workload by hit rate and write refusals; the fill low-water mark takes the value that comparison selects | invariant | RFC 8 §16 (1), §7.3; RFC 13 Appendix B (fill low-water mark) | T22.3 |
| R33 | Question 4: `offload.max_age` takes the value a streaming workload (B1, B6) selects, by whether the age bound rather than the byte bound fragments blocks; `offload.retry_min`, `offload.retry_max` and `offload.retry_max_pressure` take the values failing-store and recovered-store runs select, the cap neither hammering a failing store nor leaving a recovered one idle | invariant | RFC 8 §16 (4), §6.2, §6.3; RFC 13 Appendix B | T22.3 |
| R34 | Question 2's read-ahead cap: `readahead.max_blocks` and `readahead.max_bytes` measured across caps, by B4's sequential bytes fetched per byte read and B8's first-byte latency | invariant | RFC 8 §16 (2), §7.4; RFC 13 Appendix B | T22.3 |
| R35 | A setting's default takes the value its settling measurement selects, with no headroom and no miss; the RFC 13 row records it | invariant | RFC index, "A proposed target is replaced once" | T22.3 |
| R36 | RFC 8 §15's Suites paragraph says which suites run §15.4's benchmarks and the soak, and which still run nowhere | invariant | RFC 8 §15 (Suites); RFC 29 §4.9 | T22.8 |

The candidates of the owned sections that bind other epics are theirs: each row's behaviour (the
middle epics, through E12's catalogue); §15.3's architecture rows (the first part); §15.4's
`ponytail:` (unowned, §1.2); the per-change and daily runs (§1.2). §15.4's sentence "These measure
the pipeline, against a local emulator per change and each real service daily" is met here only for
the emulator, by hand, which §1.2 records.

## 4. Decisions this epic takes

- **D1, the burn-down ends at a file with no entry of this part, not at a row count.** Epic 12
  counts 50 rows, and the middle epics split rows into subtests with entries of their own (E12's D4
  and D6, epic 16's rerun of every Group A row under the gates' switch). A count would let a row
  without a test pass unseen. So T22.4 tables every row of E12's catalogue against its tests, and
  T22.5 ends when R11 holds. Prior art: the first part's epic 10, D1 (checked).
- **D2, entries whose epic closed move to this epic's issue as soon as it exists.** The middle
  epics' documents remove an entry "whichever of this epic and S6 lands second" (epic 17's open
  points, checked), so an epic can close with its rows red over the real journal, and its entries
  then name a closed issue. T22.4 moves them in one change, keeping each `message`, and runs again
  whenever an epic closes before S6. #5757's and #5261's entries are not moved. Prior art: the first
  part's epic 10, D4 (checked).
- **D3, the `pr` soak's remote faults are rules on the fixture's own fault points, and its oracle
  accepts an error only where the fault log puts one.** The soak gains `FaultRemote` rules (a put
  failed or cut, a get failed or corrupted, a delete refused), `FaultStore` rules on the intent and
  commit transactions, and the journal seam's existing rules for the mark; partitions are rules with
  `From` and `Count` over every op, so a span of calls fails whole. All of it goes through the seed's
  `sim.NewSchedule`, so a flag replays it as today, and none delays (the first part's rule: a delay
  waits on the test clock only the writer moves). The journal is sized so the eviction loop evicts
  during a seed, and the restart's verification reads run with the remote rules paused, so they
  fetch and must read exact. During the loop, bytes a read returns must be exact; an error is
  accepted only in a call the fault log shows a remote or store fault fired in. After each partition
  the loop calls `WaitOffloaded`, moving the test clock as it waits, then asserts the syncer's
  `inflight_bytes`, waiters and queue depth, the engine's queue depths and the goroutine count are
  back at their values before the partition. A `decision:` at the acceptance names the rule and that
  a read answering an error with no fault in its call would overturn it. Prior art: the first part's
  fault log (D5 of its epic 10, checked).
- **D4, the hours-long soak is an option of `engine-soak` against LocalStack, its outages driven by
  calls and by the store's refusals, never by the wall clock.** RFC 3 §8.1 puts it "in the engine's
  soak on the composed node". The node's `node.New` has no crash seam, so the option runs the same
  loop and oracle over the fixture with the stores the content wiring builds for the node (E21),
  through a fixture option naming the S3 store at `LOCALSTACK_HTTPS_ENDPOINT` behind `s3fault`, in a
  bucket from `s3fault.FreshBucket`. An outage opens at the loop's every Nth client call, as an
  `s3fault.Inject` rule dropping every request, and closes once `s3fault`'s record shows at least K
  requests refused under it, so it is never shorter than the store's retries reaching it; then the
  idle check of D3 runs, with open connections read as the process's descriptors (`prof.Descriptors`)
  back within the first part's tolerance and `remote_connections_opened_total` growing by no more
  than the client's limit per outage. The run lasts `DITTOFS_SOAK_DURATION`, as `journal-soak` does.
  A wait that never sees K refusals fails at a backstop, printing the idle figures. An option rather
  than a new suite: one loop, one oracle, one group. Prior art: `journal-soak`'s duration and
  timeouts (checked); RFC 4 epic 3's `Inject` and request record (checked).
- **D5, asserted counts and reported figures.** What follows from a rule is asserted with `b.Fatalf`,
  as the first part does for B5 and B7: B2's puts per file, B4's one request per random read, B6's
  marks during the write, and every cold read reaching the store (D7). Every figure §15.4's
  `decision:` lists as proposed is reported with `b.ReportMetric` and fails nothing (R27): a LocalStack
  run replaces none of them, so failing on one would gate a release bar no service has measured. A
  `decision:` at the reported targets says so and names the first real-service run as what replaces
  them. Prior art: the first part's epic 10, D6 (checked).
- **D6, one benchmark composition, with offload.** `composeBench` gains the carver, RFC 5's chain over
  its in-memory `keys.Provider`, the syncer and the S3 store at `LOCALSTACK_HTTPS_ENDPOINT`, each
  built by the content wiring as the node builds it (E21), with `s3fault` under the client for its
  request record, a fresh bucket per benchmark, a journal capacity each benchmark sets (B6 needs a
  small one, B1 one smaller than its data), and seeded random data so nothing deduplicates. B5, B7
  and B9 then run over it unchanged, which is what "again with offload composed" asks, and the first
  part's figures for them are its T10.5 baseline. `engine-bench` adds `needs: [localstack]` and
  refuses an unset `LOCALSTACK_HTTPS_ENDPOINT` before `go test` runs, exiting 2, as it refuses an
  unset `DITTOFS_BENCH_DIR`. Prior art: the first part's epic 10, D12 (checked).
- **D7, a cold read is proved at the store.** B4 and B8 read content the engine's own eviction loop
  evicted, waiting until the share's `Stats` shows the file's extent no longer held, and count, per
  read, the gets `s3fault`'s record saw: a read that reached no get fails the benchmark, whatever it
  answered. The journal is the only cache between the read and the store (a read of held content is
  served there), so the check before the read is the extent's absence, and the check after is the
  get. B4's random reads also assert that no speculative fetch ran (`Stats`' speculation bytes
  unchanged), so the one request is the read's own. Prior art: the RFC index's cold-read rule, which
  this applies.
- **D8, B1's three layers are measured inside one iteration.** Each iteration runs, on fresh data:
  the link (the TCP round trip to the endpoint, and one raw `PutObject` stream of the bytes through the
  S3 client with the codec bypassed), raw object storage (`codec.Store` put attempts of the block
  target, at `upload_workers` concurrency, called directly as RFC 3's B5 does) and the full stack,
  alternating their order between iterations, so a drifting host moves all three. Each is reported
  as MiB/s and as a fraction of the one below; the network distance is the round trip. Prior art:
  RFC 3 §7.4's B5 (raw calls at the same pool size, checked).
- **D9, every write-path clock stops at `WaitOffloaded`.** B1, B2, B3, B6, and B5, B7 and B9 once they
  run with offload, end each iteration with `WaitOffloaded` inside the timed region; the per-call
  latencies they report are unchanged. A read benchmark does not wait. Prior art: none found; RFC 8
  §11.4's MUST.
- **D10, a default is settled where its selecting measurement does not depend on the store's
  latency, and only recorded where it does.** The RFC index settles a default "wherever measurement
  runs", but against LocalStack on loopback a round trip is a fraction of a millisecond. The fill
  low-water mark (hit rate and write refusals), `offload.max_age` (whether the age bound fragments
  blocks) and the `offload.retry_*` caps (requests to a failing store, idle time after it recovers)
  are measured by counts that do not move with latency, so T22.3 settles them. The read-ahead cap's
  B8 half is a first-byte latency, so T22.3 records the LocalStack figures beside the proposal and
  question 2's cap stays open until the first real-service run (§1.2). Fill-never is the low-water
  mark at 100 % of the maximum footprint and fill-always at 0 % with reads that never form a scan
  past the window, so rule 2 of §7.3 is not compared separately; a `decision:` on the sweep says
  so. Prior art: the RFC index's settling rule, which this applies.
- **D11, other epics' constants are measured, and changed only where a run shows one binding.** E14's
  D5 (the offload and removal sources' `Batch`, `Concurrency` and `RunTimeout`) and E17's fill bound
  and read table size are proposals those epics hand to these benchmarks. T22.3 reports each beside
  the run that reaches it, and changes one only when a sweep point shows it limiting throughput or
  causing re-fetches, in the same pull request as the figure. Prior art: none found.
- **D12, the baseline is one file, replaced through a pull request.** The first part keeps
  `baseline.txt` beside the benchmarks (its epic 10, T10.5); T22.8 replaces its B5, B7 and B9 lines
  with the offload composition's and adds the other six, from a run of ten on the reference box, and
  the pull request shows `benchstat` of the old file against the new one, which is the comparison
  R24 asks for. Prior art: RFC 29 §4.10's stored baseline (checked).

## 5. Target tree

```text
test/engine/
  known.yaml                         T22.4, T22.5  entries re-pointed (D2), then removed (R11)
  soak-known.yaml                    T22.6          none kept; a flaky seed is quarantined
test/suites.yaml                     T22.1, T22.7   engine-bench: needs localstack, the endpoint guard (D6);
                                                    engine-soak: the localstack option (D4)
.github/workflows/tests.yml          T22.7          the plan job's select names engine-soak/localstack
pkg/engine/                          T22.5          fixes the real journal shows, each with its unit test
pkg/engine/enginetest/
  remote.go                          T22.7          WithStore, the S3 store the content wiring builds (D4)
  fixture.go                         T22.6          SyncerStats, the syncer's figures for the idle check
internal/vfs/integration/
  soakmodel_test.go                  T22.6          the oracle's remote-fault acceptance (D3) and its unit test
  soak_test.go                       T22.6, T22.7   remote and store rules, partitions, the idle check;
                                                    the localstack outage cycle (D4)
  benchcompose_test.go               T22.1          composeBench with offload and LocalStack (D6); the
                                                    three layers (D8); the network distance in printBox
  bench_b1_test.go                   T22.1          B1, B3 and B6
  bench_b2_test.go                   T22.2          B2
  bench_b4_test.go                   T22.2          B4 and B8, the cold-read proof (D7)
  bench_sweep_test.go                T22.3          the settling sweeps (D10, D11)
  baseline.txt                       T22.8          all nine, from the reference box (D12)
docs/internals/rfcs/rfc-8-engine.md  T22.3, T22.8   §16's answers; §15's Suites paragraph
docs/internals/rfcs/rfc-13-configuration.md  T22.3  the settled rows; B for the old E ids
docs/internals/testing.md            T22.1, T22.7   regenerated by dt docs
```

## 6. Subtasks

### T22.1 The benchmark composition with offload, B1, B3 and B6

**Subtask T22.1 estimate: 4.5 man-days.**

- **Track:** the team. **Depends on:** none. It needs S7 (E21's store composition), S6 and the RFC 4
  plan's epic 3.
- **Covers:** R8, R16, R18, R20, R22, R25 to R28, R31.
- **Deliverables:**
  - `composeBench` with offload (D6): the content wiring's carver, chain, syncer and S3 store at
    `LOCALSTACK_HTTPS_ENDPOINT` behind `s3fault`, a bucket from `FreshBucket`, the journal capacity
    as a parameter, seeded random data. Its endpoint check fails the benchmark naming the variable
    when unset (R16). Consumed by every benchmark of this epic and by B5, B7 and B9.
  - The three layers of D8 and `printBox` extended with the round trip, the LocalStack version and
    the pool sizes. Consumed by B1 and T22.8's record.
  - `bench_b1_test.go`: `BenchmarkB1` (data twice the journal; full stack, raw object storage and link
    per iteration, each as MiB/s and fraction; `engine_stage_busy_ratio` per stage as metrics),
    `BenchmarkB3` (B1 below and above the soft threshold; p99 and max submission latency, throughput
    against B1), `BenchmarkB6` (one file to 20 × the journal; last tenth against first tenth; `Stats`
    sampled during the write, failing when the share's offloaded bytes rise only in samples where
    `OffloadPasses` rose too). Each clock stops at `WaitOffloaded` (D9).
  - `engine-bench` in `test/suites.yaml`: `needs: [localstack]` and the endpoint guard before
    `go test` (D6); `dt docs`.
- **Acceptance criteria:**
  - with LocalStack up and `DITTOFS_BENCH_DIR` set, `dt run engine-bench` reports B1's three layers and
    fractions, the four stages' busy ratios, B3's latencies and B6's two tenths;
  - with `LOCALSTACK_HTTPS_ENDPOINT` unset the cell exits 2 naming it, and no `go test` runs;
  - every reported figure carries the unit `benchstat` reads, and the header carries R28's fields with
    the round trip.
- **Tests:** B6's assertion seen to fail with the pipeline's per-block report held until the pass
  ends (a planted break of RFC 8 §6.8); the endpoint guard seen to refuse. The reported figures fail
  nothing (D5). No test double: LocalStack and the real journal.
- **Rules:** rule 3, the stage series through `Stats`; rule 9, D5's `decision:`. Others as §2 says.

### T22.2 B2, B4 and B8

**Subtask T22.2 estimate: 3.5 man-days.**

- **Track:** the team. **Depends on:** T22.1.
- **Covers:** R19, R21, R23, R26, R27, R30.
- **Deliverables:**
  - `bench_b2_test.go`: `BenchmarkB2/<10000|100000>`, files/s per directory size, and puts per file
    from `s3fault`'s record, failing above 64 KiB over the block target, rounded up.
  - `bench_b4_test.go`: the eviction wait and per-read get count of D7; `BenchmarkB4/random`, one get
    per 4 KiB read asserted, the speculation bytes asserted unchanged, p99 reported against the p99 of
    a raw ranged get measured in the same iteration; `BenchmarkB4/sequential`, bytes fetched per byte
    read reported; `BenchmarkB8`, 8 MiB cold sequential reads, p99 reported against each read's
    slowest chunk fetch and verification plus 10 %, taken from the `engine.fetch` spans a recording
    tracer keeps (an output sink, not a double).
- **Acceptance criteria:**
  - each benchmark fails naming the read when a read reached no get (R30);
  - B2 fails when a file takes more puts than the bound; B4's random case fails on a second get or a
    speculative fetch;
  - B2's clock stops at `WaitOffloaded`; B4's and B8's do not wait.
- **Tests:** planted breaks, each seen to fail before the pull request opens: a read served from a
  copy kept after eviction (D7's proof), a block closed per file below its target (B2's bound), a
  fetch of the whole block for a 4 KiB read (B4's one request). No test double.
- **Rules:** rule 5, the proof at the store; rule 9, D7's `decision:`. Others as §2 says.

### T22.3 The settling sweeps

**Subtask T22.3 estimate: 2.5 man-days.**

- **Track:** the team. **Depends on:** T22.2.
- **Covers:** R32 to R35.
- **Deliverables:**
  - `bench_sweep_test.go`: the fill policies of D10 on a large-file workload with writes beside its
    reads, reporting hit rate and write refusals; `offload.max_age` across values under B6's stream,
    reporting blocks closed below target per file; the `offload.retry_*` caps across values under an
    `s3fault` outage, reporting requests to the failing store and idle seconds after it recovers;
    `readahead.max_blocks` and `readahead.max_bytes` across caps under B4's sequential reads and B8.
    E14's and E17's constants reported where a run reaches them (D11).
  - RFC 13 Appendix B: the fill low-water mark, `offload.max_age` and the `offload.retry_*` rows
    settled to the selected values (R35), with the run's commit and box; the read-ahead row with the
    LocalStack figures beside the proposal (D10); the "settled by" column's E ids written as B
    (§9 point 5). RFC 8 §16's questions 1 and 4 answered, question 2's cap with its figures.
- **Acceptance criteria:**
  - each settled default is the value its sweep selected, by the measure the RFC 13 row names;
  - a constant of E14 or E17 changes only with the sweep point that shows it binding (D11).
- **Tests:** none of its own: a sweep reports and fails nothing. The settled values are checked by
  RFC 13's Appendix B check (`TestAppendixBMatchesTheTable`), which fails when a default and its row
  disagree.
- **Rules:** rule 9, D10's `decision:` on the fill policies. Others as §2 says.

### T22.4 Re-point and cover

**Subtask T22.4 estimate: 1.25 man-days.**

- **Track:** the team. **Depends on:** none. It needs only this epic's issue, and runs again each
  time a middle epic closes with entries left.
- **Covers:** R9, R10.
- **Deliverables:** one change to `test/engine/known.yaml` moving every entry that names a closed
  issue to this epic's issue, keeping its `message` and naming in its `reason` what it waits for
  (D2); and a table, in that change's description, of every row of E12's catalogue against the test
  and subtests that check it, and of every test of `pkg/engine/conformance` against its row. A row
  with no test gets one, red, naming this epic's issue.
- **Acceptance criteria:**
  - no entry names a closed issue; #5757's and #5261's are untouched;
  - every row of R1 to R6 names its test in the table.
- **Tests:** none of its own: the suite grades the file, and an entry whose message no longer
  matches turns the run red (RFC 29 §4.7).
- **Rules:** rule 6, its first half. Others as §2 says.

### T22.5 The burn-down

**Subtask T22.5 estimate: 5 man-days.**

- **Track:** the team. **Depends on:** T22.4. Each slice waits on what it names.
- **Covers:** R1 to R11.
- **Deliverables:** for each entry left after the middle epics, the change that turns its test green
  and removes the entry, in slices by what it waits on:

  | Slice | Waits on | Man-days |
  | --- | --- | --- |
  | a | S6's first half: `Offload`, `OffloadMany`, `MarkOffloaded`, `Unmark`, `Release` on the real journal | 2 |
  | b | S6's second half: `DropOlder`, `Reconciled`, `AcknowledgeUnreadable` | 1.25 |
  | c | S5's halves that landed after their epic: the destroyed-material notice, the key-record view, the salvage-key wrap | 1 |
  | d | the last Group A row: "§3.2 no gate is safety" whole, and R11's check | 0.75 |

  Where the real journal or a composed component shows the engine wrong, the fix with a unit test
  over the in-memory journal and the row that found it. A run over the in-memory journal a middle
  epic kept beside a red real-journal row is dropped once the real row is green (rule 8).
- **Acceptance criteria:**
  - `test/engine/known.yaml` holds no entry naming an issue of epics 11 to 22 or #5757, and
    `soak-known.yaml` none (R11);
  - `dt run engine-conformance` passes under `-race` with no entry but #5261's, if the journal's
    lost-tail fix has not landed;
  - every row of R1 to R6 runs over the real journal and the components the content wiring builds.
- **Tests:** the rows, run as CI runs them. For each engine fix, a unit test that fails when the fix
  is reverted, read where the value is consumed.
- **Rules:** rule 8, the in-memory runs dropped; rule 9, a `decision:` where the real journal forces
  a behaviour the RFC leaves open. Others as §2 says.

### T22.6 The soak with offload and remote faults

**Subtask T22.6 estimate: 3.5 man-days.**

- **Track:** the team. **Depends on:** none. It needs S4, S6 and E16's `FaultRemote`.
- **Covers:** R7, R8, R12 to R14, R17.
- **Deliverables:**
  - `soak_test.go`: the fixture composed with `WithCarver`, `WithChain`, `WithSyncer` and `WithRefs`,
    a journal small enough that each seed evicts, the remote, store and partition rules of D3 in the
    seed's schedule, the paused remote rules for the verification reads, and the idle check after
    each partition.
  - `soakmodel_test.go`: the acceptance of an error only in a call the fault log marks (D3), with its
    `decision:`.
  - `enginetest`: `(*Fixture).SyncerStats() syncer.Stats`, the composed syncer's figures (consumed by
    the idle check here and by T22.7).
  - The seed count set so `engine-soak` stays within the `pr` budget, from
    `dt plan --tier pr --estimate` (R17).
- **Acceptance criteria:**
  - every seed passes over the real journal, with at least one eviction, one remote read and one
    partition of each kind in the fault log of each seed;
  - after each partition the idle figures equal their values before it;
  - `soak-known.yaml` stays empty.
- **Tests:** the oracle's unit test gains three cases, each failing when its rule is loosened: an
  error accepted in a call with no fault; wrong bytes accepted after an injected corruption; an error
  rejected in a call whose fault fired. Mutants run by hand once the soak is green: a failed put
  reported offloaded (the pipeline marking an extent whose put failed), which the soak must fail on a
  read after eviction; a fetch leaking its buffer past a cancelled read, which the idle check must
  fail. The faults are kind 3, wrappers on real implementations.
- **Rules:** rule 4, the idle check's goroutines; rule 9, D3's `decision:`. Others as §2 says.

### T22.7 The hours-long soak against LocalStack

**Subtask T22.7 estimate: 3 man-days.**

- **Track:** the team. **Depends on:** T22.6. It needs S7 and the RFC 4 plan's epic 3.
- **Covers:** R8, R14 to R17, R31.
- **Deliverables:**
  - `enginetest`'s `WithStore`, the S3 store the content wiring builds from a store configuration,
    with a transport hook for `s3fault` (its signature follows E21's store configuration; §9 point
    3). Consumed by the `localstack` run.
  - `soak_test.go`: the outage cycle of D4, by calls and by `s3fault`'s refusals, the run bound by
    `DITTOFS_SOAK_DURATION`, the idle check with open connections, its backstop printing the idle
    figures; D4's `decision:` at the driver.
  - `engine-soak`'s `localstack` option: `tier: nightly`, `needs: [localstack]`, the endpoint guard,
    `DITTOFS_SOAK_DURATION: "2h"`, `timeout: { test: 150m, cell: 160m }`, as `journal-soak`'s; its
    name in `tests.yml`'s select; `dt docs`.
  - RFC 3 §8.6's sentence and RFC 8 §15's Suites paragraph name the option.
- **Acceptance criteria:**
  - a `workflow_dispatch` of `tests.yml` at the nightly tier on `feat/storage-v2` runs the option for
    its full duration and passes, with every outage closed by K refusals;
  - with `LOCALSTACK_HTTPS_ENDPOINT` unset the option fails naming it (R16).
- **Tests:** mutants run by hand: a client that never closes an idle connection, which the
  descriptor check must fail; a retry loop that ignores its budget, which the outage's refusal count
  and the idle check must show. The outage is `s3fault`'s, kind 3.
- **Rules:** rule 8, LocalStack; rule 9, D4's `decision:`. Others as §2 says.

### T22.8 The baseline and the record

**Subtask T22.8 estimate: 2 man-days.**

- **Track:** the team. **Depends on:** T22.3, T22.5.
- **Covers:** R24, R26 to R29, R36.
- **Deliverables:**
  - B5, B7 and B9 run over the offload composition (D6), each clock stopping at `WaitOffloaded` (D9),
    B5's and B7's counts still asserted.
  - `baseline.txt` (D12): all nine from a run of ten on the reference box, its header carrying R28's
    fields; the pull request shows `benchstat` of the first part's file against it.
  - RFC 8 §15's Suites paragraph: `engine-bench` runs §15.4's benchmarks by hand against LocalStack on
    the reference box, `engine-soak` and its `localstack` option the soak; the per-change and daily
    runs still nowhere. `docs/internals/testing.md` regenerated.
- **Acceptance criteria:**
  - `benchstat` reads the old and new files and gives B5's and B9's fsync and write p99 against the
    first part's; a figure worse than the first part's beyond the noise band is recorded as a finding
    in the pull request;
  - each §15.4 figure the run misses is recorded as a finding against RFC 8, not loosened (R27).
- **Tests:** none of its own; B5's and B7's counts are the first part's checks, seen to fail on its
  mutants again over the offload composition.
- **Rules:** rule 4, the heap slack kept. Others as §2 says.

## 7. Order of work

```mermaid
flowchart TD
  S7(["S7 node composition built"]):::sync --> T22_1["T22.1 composition with offload, B1, B3, B6"]
  S6(["S6 journal offload half built"]):::sync --> T22_1
  T22_1 --> T22_2["T22.2 B2, B4, B8"]
  T22_2 --> T22_3["T22.3 settling sweeps"]
  T22_4["T22.4 re-point and cover"] --> T22_5["T22.5 burn-down, in slices"]
  S6 -.-> T22_5
  S4(["S4 offload complete in the fixture"]):::sync --> T22_6["T22.6 soak with remote faults"]
  S6 --> T22_6
  T22_6 --> T22_7["T22.7 LocalStack soak"]
  S7 --> T22_7
  T22_3 & T22_5 --> T22_8["T22.8 baseline and record"]
  T22_2 --> CP1(["after T22.2: B1 to B4, B6, B8 run against LocalStack"]):::sync
  T22_5 --> CP2(["after T22.5: known.yaml holds nothing of this part"]):::sync
  T22_8 & T22_7 --> done(["epic 22: green, soaked, baselined"]):::sync
  classDef sync stroke:#2c5aa0,stroke-width:3px,stroke-dasharray:4 2
```

T22.1 is the blocker: the composition the other benchmarks, the sweeps and the baseline run over.
Its path runs to T22.3. T22.4 needs only this epic's issue and starts long before S6; the burn-down
goes in slices as S6 and S5's late halves land. T22.6 and T22.7 are a path of their own, open beside
the others. T22.8 goes last, so the baseline is taken with the settled defaults and the burn-down's
fixes in.

## 8. Suggested pull requests

| PR | Subtasks | Man-days | After | Why together |
| --- | --- | --- | --- | --- |
| 1 | T22.1, T22.2 | 8 | — | the composition with offload and the six new benchmarks against LocalStack |
| 2 | T22.3 | 2.5 | PR 1 | the sweeps, the settled RFC 13 rows and §16's answers |
| 3 | T22.4 | 1.25 | — | the re-pointing and the coverage table; open beside every other |
| 4a to 4d | T22.5 | 2, 1.25, 1, 0.75 | PR 3; each after what its slice waits on | the burn-down, one slice each |
| 5 | T22.6, T22.7 | 6.5 | — | the soak with remote faults and its LocalStack option, one loop and one oracle |
| 6 | T22.8 | 2 | PRs 2 and 4d | the baseline, the comparison and the suites paragraph |

A suggestion only: one pull request per subtask works as well. PRs 1, 3 and 5 are independent and can
be open at once.

## 9. Open points

1. **Estimate against the plan's figure.** The plan says 7 subtasks and 21.5 man-days; this document
   says 8 and 25.25. Added: the settling sweeps (T22.3, 2.5), which the plan's "answered from those
   runs" leaves inside the benchmarks, and the re-pointing (T22.4, 1.25), which D2 needs because the
   middle epics can close before S6. The burn-down's 5 days is a guess (plan §6 point 2): its size is
   what S5 and S6 leave red, unknown until they land, and it is the least certain figure here, with
   T22.7, whose LocalStack run has not been timed.
2. **No nightly runs on `feat/storage-v2`.** The schedule runs on `develop` only, so the `localstack`
   option starts its nightly series when the branch reaches `develop`; until then it is accepted by a
   `workflow_dispatch` run (T22.7), and the benchmarks by the reference box's owner from a checkout.
3. **The fixture's store option follows epic 21's store configuration.** Epic 21's document is not
   written yet; `WithStore`'s argument is whatever type its content wiring takes to build an S3 store,
   with a hook for the transport, which S7's own `syncertest` run behind a fault proxy already needs.
   If epic 21 exposes no such hook, T22.1 and T22.7 add it there, about half a day.
4. **The first part's baseline may not exist yet.** T22.8 compares against the first part's T10.5
   `baseline.txt`, which is not on `feat/storage-v2` today. If epic 10 has not recorded it by then,
   T22.8 runs B5, B7 and B9 once more over the composition without offload, on the same box and week,
   as the comparison's other side.
5. **RFC 13's settling column cites RFC 8's benchmarks as E1 to E8.** Those ids are RFC 8 §13's
   invariants; the column means B1 to B8. T22.3 rewrites the ids it touches, and the others in the
   same change, since a reader following E4 to RFC 8 lands on an unrelated invariant.
6. **Three RFC 13 rows name these benchmarks and no epic settles them.** The speculation budget (B4
   and B8 at 1, 4 and 16 readers against write refusals), `eviction.repack_threshold` and the eviction
   occupancy target (the profile workload across values) are outside the questions RFC 8 §16 asks
   here, and no epic of the part owns their settling. The sweeps of T22.3 could carry them at about a
   day more; the plan's owner decides, or they become ledger rows.
7. **B9 runs through `vfs.Service`, not SMB,** as the first part adapted it (its epic 10, D8). With
   SMB in v1.1, the SMB run is nobody's yet; the adaptation is kept, and the gap is noted here.
