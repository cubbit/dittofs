# Epic 13: the journal's exports

**Sources:** [RFC 1](../rfcs/rfc-1-journal.md) §3.7 and §3.9; [RFC 25](../rfcs/rfc-25-observability.md) §2.5 and
§2.8; [RFC 8](../rfcs/rfc-8-engine.md) §2.6 (`Deps.Tracer`).
**Track:** B.
**Epic 13 estimate: 4 subtasks, 14.75 man-days.**

This is not the old epic 11 ("the engine on the RFC 16 store"), which is superseded.

- The engine, composition root, version fence and store contracts are the RFC 8 journal-only plan's.
- The offload wiring and salvage are RFC 8's second part, which waits on 9b.
- What nothing else owns, and RFC 1 §3.9 requires:
  - the export of the journal's own `Stats`.
  - RFC 8's epic 7 (T7.3) carries only a subset of it into `engine.Stats`.
  - The node serves no `journal_` series today.

## Requirements

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R1 | Everything an operator needs to tell a wedged journal from a slow one is readable from one `Stats` call | MUST | RFC 1 §3.7 | T13.1 |
| R2 | The engine exports every `Stats` field of §3.7 and the §3.9 counters, each as a series or with a written reason: a counter as `journal_<field>_total`, a gauge as `journal_<field>`, labelled with the journal | MUST | RFC 1 §3.9 | T13.2 |
| R3 | Per-share fields are exported as worst-n gauges: labelled `share` for the `metrics.top_shares` shares worst by that gauge (selection recomputed at every export), and, without a `share` label, as the sum over every share and, for an age or a headroom, the worst value | MUST | RFC 1 §3.9; RFC 25 §2.5 | T13.2 |
| R4 | `corrupt_extents` is labelled `outcome` (`data_loss` for an extent whose offloaded bit is unset, `repairable` for one whose bit is set); `descriptor_overruns` and `descriptor_open_failures` are labelled `kind` (`read` or `write`), once the journal maintains them | MUST | RFC 1 §3.9 | T13.2 |
| R5 | Only what the journal alone knows is a `journal_` metric: `WriteAt` timing and read hits and misses are the caller's | MUST NOT | RFC 1 §3.9 | T13.2 |
| R6 | The export is a read-through collector over a fresh `Stats` snapshot; it takes no lock a request path needs beyond what `Stats` takes, and its cost does not grow with work in flight | MUST | RFC 25 §2.8 | T13.2 |
| R7 | A component the export cannot read within 5 s is omitted and counted in `health_export_partial_total`, never waited for | MUST | RFC 25 §2.8 | T13.2 |
| R8 | An export carries no file name, no user content and no secret | MUST NOT | RFC 25 §2.8 | T13.2 |
| R9 | The journal's spans reach the engine's tracer, `Deps.Tracer` | MUST | RFC 8 §2.6 | T13.3 |
| R10 | `Stats` is served from maintained counters, with no walk and no lock a write needs, including the locks below | MUST, MUST NOT | RFC 1 §3.7; epic 8 R96, R97 | T13.4 |

`Stats` itself breaks RFC 1 §3.7 today, so R10 has its own subtask (T13.4).

- `Core.Stats` (`datapath/stats.go`) takes twelve locks that writes take:
  - `dirtyFiles.mu`: `Account` runs on every publish, and `oldestDirty` walks `byTime` under it;
  - `cap.mu`, through `ReservedBytes`, `ReservedByTag` and `RefusedCount`: `Take` holds it on every write and fill;
  - `set.mu`, through `Counters` and `TagFigures`: `AddRecord` holds it;
  - `pool.mu`, through `Counters`;
  - `retirer.mu`, through `Counters`;
  - `shares.mu`, through `shareFigures`: `Account` holds it on every publish;
  - `Durability.mu`, through `snapshot()`: `Sink` and `WaitDurable` hold it too;
  - `Durability.lmu`: the loss path holds it while it raises and publishes a loss;
  - `conflictSet.mu`, through `TagConflicts`: `conflictDiff` holds it during an index update;
  - `Headroom.mu`, through `headroom.Draws()`: `Take` reads `BelowHalf` under it on every content write;
  - `Descriptors.mu`, through `cache.Stats()`: `Acquire` takes it on a segment roll;
  - `Durability.dmu`, through `damagedReport`, which also walks `d.damaged`.
- Epic 8 owns the rule as R96 and R97 and has landed, so the fix is owned here.

Deferred, with owners in the plan's ledger:

- Alert rules (RFC 1 §3.9: any `data_loss` is an alert): RFC 25's plan.
  - They read a series T13.2 ships.
- API and `dfsctl` journal stats: RFC 23's API.
  - RFC 1 does not require them.
- Salvage and the offload wiring: RFC 8's second part.

## 6. Subtasks

### T13.1 Carry the journal's snapshot to the node

**Subtask T13.1 estimate: 1.25 man-days.**

- **Depends on:** none. The RFC 8 owner agreed the change on the PR: T7.3 has landed (#3940), and he asks for one copy of each figure.
- **Covers requirements:** R1.
- **Deliverables:**
  - Replace the six figures `engine.JournalStats` copies today with one field holding the journal's `Stats`.
    - The six: `MaxBytes`, `UsedBytes`, `DirtyBytes`, `UnreclaimedBytes`, `NextVersion`, `VersionLimit`.
  - The copy is in `engine.go`'s `Stats` assembly: it becomes one assignment per `engine.Stats` call.
  - Move the readers of the six: `engine/stats_test.go` reads `UsedBytes`, `NextVersion` and `VersionLimit`.
  - `Shares` (keyed by share id) and `Conditions` stay.
  - The share table's tag to share id map travels beside the snapshot: only the engine knows which share holds a tag.
  - One copy per figure, per share too:
    - The four figures `JournalShareStats` carries are exported from it.
    - The collector reads only the other per-tag fields from the carried `Stats`, and finds each one's share through the map.
- **Acceptance criteria:**
  - The carried snapshot equals the journal's `Stats` at the moment of the call.
  - `engine.JournalStats` declares none of the six fields.
  - The map names every attached share's tag, and no other.
- **Tests:**
  - At `engine.Stats`, over a real journal under `t.TempDir()`.
  - After a write, a sync and a refused reservation, the snapshot equals the journal's `Stats`.
  - The moved `stats_test.go` assertions read the same values from the new field.
  - Catches a field dropped on the way: a value set where it is produced and read by nobody.

### T13.2 The `journal_` collector

**Subtask T13.2 estimate: 5 man-days.**

- **Depends on:** T13.1.
- **Covers requirements:** R2, R3, R4, R5, R6, R7, R8.
- **Deliverables:**
  - One read-through collector in the node's `stats.go`, beside the engine series.
  - Names per R2; `outcome` and `kind` labels per R4.
  - Every `Stats` field has a series or a written reason, as the table below says, including the five an operator needs most:
    - `OldestDirty`: an age gauge in seconds, from the injected clock.
    - `LastSyncError`: a 0 or 1 flag. The error text is never a label (R8).
    - `ReadOnly`: a 0 or 1 flag.
    - `Recovery`: the recovery duration and the catalog and scanned segment counts, as gauges.
    - `SyncDuration`: a histogram.
  - Per-share gauges per R3, as RFC 25 §2.5 words it:
    - labelled `share`, for the `metrics.top_shares` worst shares (default 20), selection recomputed at every export;
    - without a `share` label, the sum over every share and, for an age or a headroom, the worst value.
    - The churn of a changing worst set is the RFC's own choice, so no `rank` label.
    - If the registry has no `metrics.top_shares` setting yet, the constant 20 with a `ponytail:` naming the move.
  - The `journal` label's value is the journal identity (hex of its `JournalID`), never the device path.
  - One single-flight, deadlined `engine.Stats` snapshot for the whole node:
    - `engineSeries`, the `journal_` collector and `engineConditions` all read it.
    - Today each scrape runs `Stats` twice and the health evaluator a third time, with no deadline.
    - Prometheus `Collect` has no context, so the 5 s cut-off runs in a goroutine; single-flight keeps a wedged journal from leaking one per scrape.
    - The deadline runs on the injected clock, not wall time.
    - On the deadline: omit the series and count it in `health_export_partial_total` (R7).
  - A closed journal exports no series: its `Stats` is the zero value, which would read as healthy.
  - Labels come from an allow-list of names: `journal`, `outcome`, `kind`, `share`, `stream` (R8).
  - A tag no attached share holds (a detached share's records) has no `share` label, because nothing names it. The sum without a `share` label still covers it.
  - The field-to-series table, which the reflection test enforces:
    - `Counters`: one series per counter, named after its field.
    - `Recovery`: its three gauges (duration, catalog segments, scanned segments).
    - `Losses`, `TagConflicts`: counts only (`LossGeneration`, and the length of `TagConflicts`). Events and files are lists, and a series per element needs a file label, which R8 forbids.
    - `ReadOnly`: the 0 or 1 flag. Any text it carries is excluded, because R8 forbids it as a label.
    - `Shares`: per-share gauges, per R3.
    - `SyncMarks`: per append stream, under a `stream` label added to the allow-list (`stats.go` carries it since T7.14).
    - `RejectedSegments`: a gauge.
    - `SuspectSegments`: carried but never set (`stats.go:238`), so excluded by name in the reflection test with a `decision:`: a zero would claim nothing is suspect. It gets its series when its source lands.
    - `HeadroomSparesShort` and the descriptor counters: excluded, because `journal.Stats` does not carry them yet (`stats.go` says nothing maintains a source). Each gets its series when its source lands. The reflection test fails when the field is added to `Stats` with no series.
- **Acceptance criteria:**
  - Every `Stats` field and §3.9 counter has a series under its name, or a written reason in the table.
  - Per-share gauges carry at most `metrics.top_shares` `share` values, and an unlabelled total beside them.
  - A scrape of a wedged journal returns at the injected deadline. The whole snapshot is omitted, `engineSeries` too, because `engine.Stats` calls each journal's `Stats` in turn.
  - The partial counter rises.
  - Repeated scrapes of a wedged journal keep the goroutine count flat.
  - No label name outside the allow-list appears.
  - A closed journal's series are absent, not zero.
- **Tests:**
  - At the node's metrics listener, over a started node.
  - Drive each condition RFC 1 §9.4 Group D names that needs no 9b call:
    - a sync failure;
    - a corrupt extent of each outcome;
    - a descriptor overrun of each kind;
    - a refused reservation;
    - a fill;
    - a rejected segment;
    - a torn tail.
  - Assert each named series moved by the amount driven.
    - This is RFC 1's "every metric is reachable" check.
    - Its `known.yaml` entry leaves with it, for the metrics that need no 9b call.
  - A reflection test: every field of `journal.Stats` maps to the table above, exactly one series or one written reason.
    - It fails when a field is added to `Stats` with no series.
    - It fails when a figure has two sources (the four per-share figures are the named case).
  - More shares than `metrics.top_shares`: only the worst appear with a `share` label, the total covers all.
  - Two scrapes with a different share worst in between: the `share` labels follow it and the old one goes.
    - Catches a collector that picks the worst shares once and keeps them.
  - A tag no attached share holds: it has no `share` label and is in the total.
  - A wedged journal held by a test hook, the injected clock advanced past 5 s.
    - The scrape returns, the engine series omitted with the rest, and the goroutine count is the same after 100 scrapes.
  - A closed journal: its series are absent after the close.
  - The label names of every series are checked against the allow-list.

### T13.3 The tracer and logger wiring

**Subtask T13.3 estimate: 3 man-days.**

- **Depends on:** the RFC 23 owner's agreement to change the node's export pin (open points). It adds the two things it relies on:
  - The node has no tracer: `internal/node/node.go` says "Config names no tracer".
  - The journal declares no repack span: `core/trace.go` has `sync`, `reserve_refused`, `fill` and the `open` spans only.
- **Covers requirements:** R9.
- **Deliverables:**
  - `node.Config` gains a `Tracer`, the node's OpenTelemetry tracer.
    - `internal/node/arch_test.go`: `Tracer` added to `configFields` (RFC 23 epic 1's R9, agreed with its owner).
    - The API's own tracer stays RFC 23's.
  - The journal gains a repack span (`SpanRepack`), emitted by `Repack`.
  - A function adapting `Deps.Tracer` to the journal's stdlib `Tracer` option.
    - The `journal` attribute is built once, when the adapter is built.
  - `enginewire.Params` gains a `Tracer` field.
    - Today `enginewire.go` hardcodes `noTrace` and `Params` has no tracer.
  - With no tracer configured, `enginewire` passes nil to the journal's `WithTracer`.
    - The journal skips all span work when its tracer is nil (`worker.go`, `fill.go`); a non-nil no-op would pay for it.
    - `noTrace` stays for the engine's own `Deps.Tracer`.
  - `enginewire` also passes `journal.WithLogger(log.With("journal", id))`.
    - Today it passes no logger.
  - Spans: open, sync, repack, fill, refused reservation.
  - The offload pass span waits on 9b.
- **Acceptance criteria:**
  - Each listed operation produces one span.
  - Each span carries the journal as an attribute.
  - With tracing off, the journal receives a nil tracer and no call blocks.
  - A journal log line carries the `journal` attribute.
- **Tests:**
  - At the node, with OpenTelemetry's in-memory span recorder (a real implementation, not a double).
  - A sync, a repack and a refused reservation each produce one span with the journal attribute.
  - Tracing off produces none, and the journal's tracer is nil.
  - The logger's output, captured through an injected `slog.Handler`, names the journal.

### T13.4 `Stats` without a walk or a write lock

**Subtask T13.4 estimate: 5.5 man-days.**

- **Depends on:** none.
- **Covers requirements:** R10.
- **Deliverables:**
  - The oldest dirty write kept as a maintained value, updated where files enter and leave the dirty set.
    - `oldestDirty` no longer walks `byTime` under `dirtyFiles.mu`.
  - `Stats` takes none of the twelve locks listed under R10, or reads its figure as an atomic.
    - Each lock is either taken out of `Stats`' path or kept with a `decision:` naming why holding it cannot stall a write, and listed by name in the test's allow-list.
  - `Account` keeps the minimum, so the next-oldest scan moves onto the clean path and off `Stats`.
  - A `ponytail:` at the maintained value if it keeps one-second resolution, as `oldestDirty` does today.
- **Acceptance criteria:**
  - `Stats` costs the same at 10^3 and 10^5 dirty files.
  - `Stats` returns while a writer holds each of the twelve locks, one at a time, except the locks on the named allow-list.
  - The inequalities `Stats` keeps (RFC 1 §3.7) still hold under concurrent writes.
- **Tests:**
  - At `Journal.Stats`, the public call.
  - A benchmark at 10^3 and 10^5 dirty files with `b.ReportAllocs()` and setup before `b.ResetTimer()`: flat.
    - `byTime` is keyed by the second, so the files are written over distinct seconds on the injected clock.
    - Ten thousand writes in a tight loop make a handful of keys, and the benchmark cannot fail.
    - A second benchmark over `Account`, at the same sizes. It passes when flat, with each iteration cleaning the current oldest file and dirtying a new newest one: otherwise the set drains once `b.N` exceeds its size, and the benchmark reads flat for the wrong reason.
  - One test per lock, with a writer holding it through an `export_test.go` hook: `Stats` returns.
    - `internal/lockorder`'s `Recorder` cannot replace them: a rank of 0 is never recorded, and most of these mutexes are plain `sync.Mutex`.
    - The ranked ones (`cap.mu`, `set.mu`, `pool.mu`) all share the `Capacity` rank, so a rank cannot tell them apart.
    - The test lists the locks `Stats` reaches: a lock added to its path without a test fails it.
  - The existing Group D check "`Stats` is cheap and keeps its inequalities" still passes.
  - A `-race` run with writers and a `Stats` loop.

## Order and pull requests

- Order: T13.1, then T13.2. T13.3 and T13.4 are independent.
- Suggested PR 1: T13.1 and T13.2 (6.25 md).
- Suggested PR 2: T13.3 (3 md).
- Suggested PR 3: T13.4 (5.5 md).
- One PR per subtask works too.

## Open points

- The RFC 23 plan's owner agrees `node.Config` gains a `Tracer`, and that `internal/node/arch_test.go` pins it.
  - The node's `decision:` leaves the tracer for later.
  - The same owner agrees `Preflight` joins the exports (epic 10, T10.2).
- The `journal_` prefix and the worst-n helper are RFC 25's.
  - If its registry is not built, T13.2 uses the node's existing collector style.
  - A `ponytail:` names the move to the registry.
