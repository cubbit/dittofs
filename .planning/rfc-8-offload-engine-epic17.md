# Epic 17: engine offload remote reads, fill and speculation

**Sources:** [RFC 8](../docs/internals/rfcs/rfc-8-engine.md) §1.1 (cold-read isolation, no copy
of the bytes kept), §3.1 (`fill` and the speculator as a module), §3.3 (readahead frontiers are
memory), §7.1 (the carved row), §7.2, §7.3, §7.4 (read-ahead; pre-warm deferred), §7.5, §7.7, §7.8,
§9.1 (adoption, the copies narrowed to what no ref covers, the destination's fill refused, the
missing-chunk backstop), §11.2's remote-unavailable row for reads, §13's E9, E10, E14, E25 and E34,
§14's read, fetch, fill, speculation and re-resolution series and the `engine.fetch` span, and the
§15.1 and §15.2 rows in §3's last table; [RFC 6](../docs/internals/rfcs/rfc-6-block-metadata.md)
§8.1 (the covering lookup, as this epic's pull request amends it to name a carved span's chunk
location), §2.2, §2.5, §6.6 and §7.2; [RFC 3](../docs/internals/rfcs/rfc-3-syncer.md) §1.3
(`Fetch`, `Prefetch`, the error table, borrowed bytes), §4.1 to §4.5;
[RFC 1](../docs/internals/rfcs/rfc-1-journal.md) §3.4 (`Fill`) and §3.7 (`Extents`);
[RFC 4](../docs/internals/rfcs/rfc-4-remote-tier.md) §4.1 (`Name`, `ChunkRange`, `ErrNotFound`);
[RFC 5](../docs/internals/rfcs/rfc-5-transforms.md) §2.7 (the material errors).
**Track:** C.
**Epic 17 estimate: 9 subtasks, 23.5 man-days.**
**Depends on:** sync point **S2, red rows in CI**: epic 12's rows and the fixture's component
options; **epic 11's frozen surface**: `Settings.FillLowWater`, `ShareSettings`' read-ahead caps and
speculation budget, the `Stats` fields `Fetches`, `FetchBytes`, `Fills`, `SpeculationBytes`,
`SpeculationUsedBytes`, `Reresolves`, and the recorded declarations `Handle.Fill` and the flow's
`Fetch` and `Prefetch`; **epic 16's T16.1**: the binding's `flow`, opened at attach as a demand flow, and
every engine the fixture and the in-package tests build composing RFC 3's syncer over two in-memory
stores, with `FaultRemote` real (its D9, D10); **epic 16's D6**, the gates' switch `Deps.Gates`,
which `fill` reads (pending the user's answer in epic 16's document);
**S5's first half, partly**: RFC 3's syncer (`pkg/syncer/pool`) over RFC 4's in-memory store through
`codec.Store`, and RFC 5's material errors in the `pkg/transform` root (epic 15 admits that root into
the engine's import walk). The conformance rows also wait on epics 15 and 16 (the carver, chain and
syncer options made real) and on **S6** for the rows that evict through the real journal's
`Release`.
**Blocks:** epic 18, which calls T17.7's cancellation of queued speculation from `admit` and the
pacing path; epic 19, through track C's order only.

---

## 1. Goal

Epic 17 makes a read reach past the journal. A carved run is fetched through the share's flow,
chunk by chunk, each verified before a byte of it reaches the caller; an absent object is
re-resolved while its location moves; a cold read asks for chunks or for the whole block; the
fetched bytes may fill the journal beside the reply, never in its way; a sequential reader is read
ahead within its share's budget; and a clone adopts carved refs instead of copying what a ref
already holds.

**At the end of the epic:**

- a read of a carved extent returns its verified bytes in file order, fails as **Lost**, corrupt or
  unavailable distinguishably, and never answers from a chunk that did not verify;
- the first part's `ErrUnavailable` for a carved run, its `decision:` in `read.go` and the tests
  that pinned it are gone;
- a get that finds its object absent re-runs the covering lookup while each answer names a new
  location, and fails as **Lost** when one repeats or a chunk has no record;
- a fill never delays or fails a read, and loses to any write that landed meanwhile;
- read-ahead never delays a demand read, never refuses a write, never fetches what the journal
  holds, and stays within the share's speculation budget;
- a clone copies only the source runs no live chunk covers at or above their version, and adopts
  the rest.

### 1.1 In scope

The covering lookup's chunk location (RFC 6 §8.1 as amended); the carved row of §7.1 with §7.2's
reply, §7.5's unavailable answer and the material errors; §7.7's re-resolution; §7.8's choice of
chunks or block; `fill`, the journal's `Fill` beside the reply, and the in-memory journal's `Fill`
and `Extents`; the speculator's read-ahead with its budget, windows and cancellations; §9.1's
narrowed copies, the destination fill exclusion and the missing-chunk backstop's log; the §14
series and span of these paths; the fixture controls epic 12 proposed for these rows, made real.

### 1.2 Out of scope

- `PreWarm`, the speculator's pre-warm, `fill`'s rule 3 (a pre-warm asked to yield) and the
  pre-warm legs of §7.4's checks and of §15.1's "§9.1 no pre-clone fill": deferred, unowned until a
  pre-warm action is specified (the offload plan's §6.1 row on `PreWarm`).
- Posting capacity pressure from `admit` and the pacing path, and so cancelling speculation there:
  epic 18, which calls T17.7's entry point.
- Opening a binding's flow at attach and closing it at `Drop`, composing the syncer in every
  fixture, and the gates' switch itself: epic 16 (its T16.1, D6, D9).
- Relocating a chunk (RFC 6 §7.3): RFC 9's, with GC; the relocation leg of "§7.7 re-resolution"
  waits on it (the offload plan's §6.1 row this pull request adds; §9's first point).
- A read's span linking to a joined fetch's span (RFC 8 §14): unowned until RFC 3 reports a join
  (D9; the §6.1 row this pull request adds).
- Reading a carved chunk of an older namespace generation on another store: RFC 27, with the re-home
  (D4; the offload plan's §6.1 row on the re-home).
- The clone's admission, registration, journal step, phases and refusals, and `kvstore`'s carrying
  of a gone chunk's ref as **Lost**: the first part's, kept.
- Turning green the rows that need a pass end to end or the real journal's `Release`: they turn
  green in whichever change lands last of this epic, S3 and S6 (T17.9). Until then this epic's unit
  tests are the green coverage of what they check.

### 1.3 Names used here

- **S2**, red rows in CI: the end of epic 12; epics 13, 15 and 17 start there.
- **S3**, a pass runs end to end: the end of epic 16, over S5's carver and syncer and S6's `Offload`.
- **S5**, offload components built: the RFC 2 to RFC 5 plans' implementation epics; its first half
  is the carver, RFC 3's syncer, RFC 4's in-memory store with `codec.Store` and RFC 5's chain.
- **S6**, journal offload half built: the journal plan's epics 9b and 7b (`Offload`, `Release`,
  `Unmark`, …).
- **§7.4 S1, S2, S3, S4, S5** are RFC 8 §7.4's five speculator rules, never synchronisation points;
  this document always writes them with the section ("§7.4 S3").
- **E3, E9, E10, E14, E25, E34** are RFC 8 §13's invariants of those numbers, not epics.
- **The first part**: the journal-only engine, epics 1 to 10 of RFC 8's first plan, whose read and
  clone code this epic extends.
- **The integration epic**, epic 21: the node's content wiring composes offload.

## 2. How the general rules apply

The rules are those of [the offload plan's §1.5](rfc-8-offload-engine-plan.md#15-general-rules-for-each-epic),
cited by number and title and not copied.

| Rule | Subtasks | How |
| --- | --- | --- |
| rule 1, independent unit tests ([`CLAUDE.md`, "Writing tests"](../CLAUDE.md#writing-tests)) | all | tests run over the first part's in-memory journal and `kvstore` over the in-memory `kv.KV`, refs and blocks written through `kvstore`'s `Intend` and `Commit` and `codec.Store`'s put into RFC 4's in-memory store (kind 2, RFC 4's suite), fetched through RFC 3's real syncer, faults from RFC 4's fault wrapper (kind 3); a carved extent is one the journal never held, so no pass and no `Release` is needed; no engine-side double is written |
| rule 2, fuzz the key and ref decoders the queue's rebuild reads | none | no decoder: the chunk location arrives decoded by `kvstore`, and the block body is RFC 4's codec's |
| rule 3, observability follows RFC 25 | T17.2, T17.3, T17.4, T17.5, T17.7 | `engine_reads_total`'s `remote` class, `engine_fetches_total` by `shape`, `engine_fetch_bytes_total`, `engine_fills_total` by `result`, `engine_speculation_bytes_total`, `engine_speculation_used_bytes_total` and `engine_reresolves_total`, as `Stats` fields with no share label; the `engine.fetch` span |
| rule 4, profiling | T17.2, T17.5, T17.6 | every test runs under `prof.Watch`; a read's fetch goroutines are joined before it returns, and the fills and prefetch consumers at `Close`, so a leak fails the run; the first part's allocation assertion on a read of held content stays green (no path here runs for held bytes) |
| rule 5, public interface and internals ([`CLAUDE.md`, "Verifying a change"](../CLAUDE.md#verifying-a-change)) | all | asserted at `Read`'s reply and error, which the filesystem service consumes; a fill at the journal's next `ReadAt`; a location at the engine's re-resolution, not at the store; `-race` on every concurrent leg |
| rule 6, known failures shrink | T17.2, T17.9 | each row of §15.1 and §15.2 this epic owns leaves `known.yaml` in the change that turns it green, or has its `message:` rewritten in the change that moves its first failing check |
| rule 7, v2 ignores v1, and the engine imports only what RFC 0 §1.2 allows | T17.2 | the read path calls the flow through the `pkg/syncer` root and reads RFC 4's errors from the `pkg/remote` root and the material errors from the `pkg/transform` root, all contracts; tests may import `pkg/syncer/pool`, `pkg/remote/memory` and `pkg/remote/codec` |
| rule 8, real dependencies for properties of those dependencies | T17.1, T17.5, T17.9 | the location join is tested in `kvstore` over each backend RFC 6's suite runs; the in-memory journal's `Fill` and `Extents` are held to the real journal by the fixture's agreement tests; T17.9's rows run over the real journal |
| rule 9, code comment conventions ([`CLAUDE.md`, "Code comments"](../CLAUDE.md#code-comments)) | T17.2, T17.4, T17.8 | `read.go`'s carved-run `decision:` deleted (T17.2); §7.8's quarter `decision:` at the choice (T17.4); §9.1's clone-copies `decision:` kept and reworded for the narrowed copies (T17.8); each §4 decision leaves the marker it names |
| rule 10, a test must be able to fail | all | each test names the break it catches; none asserts a stub's answer |
| rule 11, a symbol is exported only if the RFC's interface uses it | T17.1, T17.5, T17.6, T17.8, T17.9 | the only exported additions are RFC 6's `Location` and `Span.Loc`, `kvstore`'s `Content.WithLogger`, the in-memory journal's `Fill`, `Extents` and `FailNextFill`, and epic 12's proposed fixture controls; `fill` and the speculator are unexported |
| rule 12, each subtask creates only what it declares | all | `Handle.Fill` joins `Deps` with T17.5 and `Handle.Extents` with T17.6, each with its first caller; each fixture control lands with its first test; the read state lands with T17.4, its first reader |

## 3. Requirements and invariants

Every obligation RFC 8, RFC 6, RFC 3 and RFC 1 place on this epic, numbered so subtasks and tests
can cite them. Rows of §15 the epic turns green are listed last.

**Resolution of a carved run**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R1 | The journal is asked before block metadata, and metadata only about what the journal does not hold, on the carved path too | MUST | RFC 8 §7.1 | T17.2 |
| R2 | The resolution is computed per request and never kept, fetched locations included | MUST NOT | RFC 8 §7.1 | T17.2, T17.3 |
| R3 | Zeros are returned only for a hole or a zero ref, never for a carved run whose fetch failed | MUST NOT | RFC 8 §7.1, E9 | T17.2 |
| R4 | A fetch per missing extent, never per window: an extent over *n* blocks costs one fetch per block, whatever the window | invariant | RFC 8 §7.1; RFC 3 §1.3 | T17.2 |
| R5 | A carved span names its chunk's location, the block and the body's position and length with the block's encoded size, read in the same transaction in the namespace the ref's generation names; a span whose chunk record is gone, names a deleted block or names a block with no record names none | MUST | RFC 6 §8.1 (as amended here), §2.1, §2.2; RFC 8 §7.1 | T17.1 |
| R6 | The covering lookup still costs O(log *n* + results) with the locations joined | bound | RFC 6 §8.1 | T17.1 |
| R7 | A ref under an overwrite at or below `asOf` is never fetched: the run reads **Lost** | MUST NOT | RFC 8 §7.1, E9 | T17.2 |
| R8 | A **Remote** extent whose fetch cannot complete, the remote unreachable or the demand deadline expired, fails the read with an error distinct from **Lost** | MUST | RFC 8 §7.5, §11.2, E9 | T17.2 |
| R9 | A material unavailable or unknown fails the read as the remote being unavailable; a material destroyed makes the chunk **Lost**, never corrupt | MUST | RFC 3 §1.3; RFC 5 §2.7 | T17.2 |
| R10 | A chunk that fails verification fails the read as corrupt | invariant | RFC 8 §7.7, E9 | T17.2 |

**The reply**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R11 | `dst` is filled in file order, held bytes, zeros and remote chunks, and `dst[:n]` holds verified bytes only | MUST | RFC 8 §7.2 | T17.2 |
| R12 | No byte of a chunk is copied into `dst` before the whole chunk has verified | MUST NOT | RFC 8 §7.2, E10 | T17.2 |
| R13 | The reply is taken from the verified bytes the fetch returned, never by re-reading the journal after a fill | MUST NOT | RFC 8 §7.2 | T17.2, T17.5 |
| R14 | A read that fails part-way returns `n`, the bytes filled in file order before the first failing part, and the error | invariant | RFC 8 §7.2 | T17.2 |
| R15 | A whole-block fetch is verified the same way, each chunk as its last byte arrives, and only the read's chunks are copied | invariant | RFC 8 §7.2; RFC 3 §1.3 | T17.2, T17.4 |
| R16 | A fetched chunk's `Bytes` are copied out before the next iteration, since they are borrowed until then | MUST | RFC 3 §1.3 | T17.2, T17.5 |
| R17 | The fetches of one read run at once, so its latency is the slowest chunk's, not the sum | invariant | RFC 8 §7.2 | T17.2 |
| R18 | No payload is kept in engine state: bytes pass through to `dst` and to the journal, and a fill's copy is released when its `Fill` returns | invariant | RFC 8 §1.1 | T17.2, T17.5 |

**Re-resolution**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R19 | A get that finds its object absent runs the covering lookup for the extent again and acts on the new answer | MUST | RFC 8 §7.7, E14 | T17.3 |
| R20 | It keeps doing so while each answer names a different location, bounded by the read's deadline | MUST | RFC 8 §7.7, E14 | T17.3 |
| R21 | A new answer of a hole or past end of file is answered as such; an uncarved run of a file whose `applied` or whose run's overwrite version is above `asOf` asks the journal again | MUST | RFC 8 §7.7, §7.1 | T17.3 |
| R22 | The read fails as **Lost** when an answer names a location that already missed, or a chunk with no chunk record | MUST | RFC 8 §7.7, E14 | T17.3 |
| R23 | A verification failure, a transport error or a timeout never triggers re-resolution | MUST NOT | RFC 8 §7.7 | T17.3 |

**Chunks or the block**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R24 | A read asks for only its chunks when its missing bytes are at most a quarter of the block and it is not sequential (it neither starts at the block's beginning nor continues where the file's previous read ended); otherwise for the whole block | SHOULD | RFC 8 §7.8 | T17.4 |
| R25 | A fetch's ranges are whole chunks, each by its hash and recorded range | invariant | RFC 3 §1.3 | T17.4 |

**Fill**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R26 | `fill` decides per demanded extent: fill unless the journal's free capacity is below the fill low-water mark, or the read is part of a sequential scan longer than the read-ahead window | SHOULD | RFC 8 §7.3, §3.1 | T17.5 |
| R27 | `fill` takes plain inputs, does no I/O, holds no component, and calls no other policy function | MUST NOT | RFC 8 §3.1 | T17.5 |
| R63 | `fill` is a gate, never what keeps a read correct: with the gates' switch off it fills every demanded extent, and the Group A rows still pass | MUST NOT | RFC 8 §3.2, E3; epic 16, D6 | T17.5 |
| R28 | `Fill` is passed exactly the verified bytes fetched for that extent, with step 1's `asOf` and the ref's version | MUST | RFC 8 §7.2; RFC 1 §3.4 | T17.5 |
| R29 | A refused fill is not an error, and a fill never delays or fails the reply | MUST NOT | RFC 8 §7.2, E10; RFC 3 §4.2 | T17.5 |

**The speculator**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R30 | The speculator is an unexported struct: it takes each read's `(file, off, len, time)` and yields hints labelled speculation, and cancellations, to the flow's `Prefetch` | invariant | RFC 8 §7.4, §3.1 | T17.6 |
| R31 | §7.4 S1: every hint is issued through `Prefetch`, and the engine sets no other priority | MUST | RFC 8 §7.4, E25; RFC 3 §4.4 | T17.6 |
| R32 | §7.4 S2: speculative fills stop while free capacity is below the low-water mark, and queued hints are cancelled when a write meets capacity pressure | invariant | RFC 8 §7.4, E25 | T17.7 |
| R33 | §7.4 S3: speculative bytes in flight per share stay within its budget, and read-ahead of one reader within its window | MUST | RFC 8 §7.4 | T17.6, T17.7 |
| R34 | §7.4 S4: speculation is memory only; a frontier lost to a restart is relearnt from the next reads | MUST | RFC 8 §7.4, §3.3 | T17.4, T17.6 |
| R35 | §7.4 S5: no speculative fetch for a block the journal holds whole; for a block held in part, only the missing chunks unless the whole block costs fewer round trips | MUST NOT | RFC 8 §7.4, §7.8, E25 | T17.6 |
| R36 | The window opens at one block on the first read that continues the last, doubles on each further one up to the smaller of `readahead.max_blocks` and `readahead.max_bytes`, and a read that does not continue resets it and cancels the file's queued hints | invariant | RFC 8 §7.4 | T17.6 |
| R37 | A cancelled or abandoned speculative fetch fails no read | MUST | RFC 3 §4.4, §4.5 | T17.7 |

**Clone**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R38 | A clone copies only the source runs the journal holds at `asOf` newer than any ref over them, or under no ref; carved refs are adopted by the metadata-only batches, and the clone never waits on the remote tier | MUST | RFC 8 §9.1, E34 | T17.8 |
| R39 | A source ref whose chunk record is gone, or names a deleted block, is carried into the destination as uncarved, reading **Lost**, counted and logged at `Error` | MUST | RFC 8 §9.1, E34; RFC 6 §6.6, §7.2 | T17.8 |
| R40 | A fill of the destination extent, by a demand read or by speculation, whose fetch resolved before the clone's journal step never installs | MUST NOT | RFC 8 §9.1 | T17.8 |

**Isolation and observability**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R41 | No lock, queue or other serialisation point shared by more than one file or share is held across a fetch, a fill or a speculative hint | MUST NOT | RFC 8 §1.1 | T17.2, T17.5, T17.7 |
| R42 | `engine_reads_total` counts a read served from the remote under `remote`; `engine_fetches_total` by `shape` `chunks` or `block`; `engine_fetch_bytes_total`; `engine_fills_total` by `result` `done`, `declined` or `refused`; `engine_speculation_bytes_total` and `engine_speculation_used_bytes_total`; `engine_reresolves_total`; none share-labelled | MUST | RFC 8 §14 | T17.2, T17.3, T17.4, T17.5, T17.7 |
| R43 | A content call opens an `engine.fetch` child span per fetch it waits on, carrying the share, the file, the bytes and the outcome | MUST | RFC 8 §14 | T17.2 |
| R44 | A **Lost** or corrupt read logs the file and extent at `Error`, once per extent, on the remote path as on the journal's | MUST | RFC 8 §14 | T17.2, T17.3 |

**Module checks (unit tests, green with this epic)**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R45 | §7.4 S1: with the fetcher saturated by read-ahead for one file, a demand read of another is not held behind it | test row | RFC 8 §7.4 Checks | T17.6 |
| R46 | §7.4 S3: 10³ files read sequentially at once never take speculative bytes in flight past the share's budget | test row | RFC 8 §7.4 Checks | T17.7 |
| R47 | §7.4 S3, read-ahead window: the window opens only for a sequential replay, not a random or strided one, and a random read cancels queued hints | test row | RFC 8 §7.4 Checks | T17.6 |
| R48 | §7.4 S4, read-ahead half: after a restart during a read-ahead no speculative state was persisted and the window reopens from the next reads | test row | RFC 8 §7.4 Checks | T17.6 |
| R49 | §7.4 S5, read leg: a held file read sequentially twice costs the remote zero requests; with one block not held, exactly that block's chunks are fetched | test row | RFC 8 §7.4 Checks | T17.6 |

**Conformance rows this epic turns green**

Each is green in whichever change lands last of this epic, S3 and, where the row evicts, S6
(§1.2); none is the only test of a requirement above.

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R50 | "§7.1 stale ref", `TestRFC8A_StaleRef` | test row | RFC 8 §15.1; epic 12, T12.3 | T17.9 |
| R51 | "§7.1 the order", `TestRFC8A_TheOrderOfAReadsSteps`, and its swapped-steps build failing | test row | RFC 8 §15.1; epic 12, T12.3 | T17.9 |
| R52 | "§7.2 streaming, verified", `TestRFC8A_StreamingVerified` | test row | RFC 8 §15.1; epic 12, T12.3 | T17.9 |
| R53 | "§7.2 fill cannot fail a read", `TestRFC8A_FillCannotFailARead` | test row | RFC 8 §15.1; epic 12, T12.3 | T17.9 |
| R54 | "§7.2 fill loses to a write", `TestRFC8A_FillLosesToAWrite` | test row | RFC 8 §15.1; epic 12, T12.3 | T17.9 |
| R55 | "§7.7 re-resolution", `TestRFC8A_Reresolution`, but its relocation leg (§9) | test row | RFC 8 §15.1; epic 12, T12.3 | T17.9 |
| R56 | "§7.6 zero ref under an overwrite", `TestRFC8A_ZeroRefUnderAnOverwrite` | test row | RFC 8 §15.1; epic 12, T12.3 | T17.9 |
| R57 | "§9.1 clone keeps writes outside its extent", offload half, `TestRFC8A_CloneKeepsWritesOutsideItsExtentOffloaded` | test row | RFC 8 §15.1; epic 12, T12.3 | T17.9 |
| R58 | "§9.1 no pre-clone fill", the demand read's leg, `TestRFC8A_NoPreCloneFill` | test row | RFC 8 §15.1; epic 12, T12.3 | T17.9 |
| R59 | "§9.1 Lost source run carried", `TestRFC8A_LostSourceRunCarried` | test row | RFC 8 §15.1; epic 12, T12.3 | T17.9 |
| R60 | "§9.1 one source state", `TestRFC8A_OneSourceState` | test row | RFC 8 §15.1; epic 12, T12.3 | T17.9 |
| R61 | "§1.1 no cross-share slowdown", cold reads, `TestRFC8B_NoCrossShareSlowdownColdReads` | test row | RFC 8 §15.2; epic 12, T12.7 | T17.9 |
| R62 | "§7.1 the join", its carved and stale rows, which the first part's `TestRFC8A_TheJoin` leaves to this part | test row | RFC 8 §15.1 | T17.9 |

## 4. Decisions this epic takes

- **D1, the covering lookup names a carved span's location.** RFC 8 §7.1 resolves a carved ref's
  chunk and §7.7 has the reader "run the covering lookup again" while "each answer names a different
  location", but RFC 6 §8.1 returned the ref alone and no engine view read a chunk record. This
  epic's pull request amends RFC 6 §8.1 and its types: `Location` (block, position, length and the
  block's encoded size, never the refcount, so two answers compare by where the chunk is) and
  `Span.Loc`, set on a carved span in `Content.Covering`'s own transaction and zero when the chunk
  record is gone, names a deleted block or names a block with no record. The join is in
  `Content.Covering` only: a clone batch's `adopt` and `Allocation` share the lookup and pay
  nothing for it. A second call for the location would let a relocation slip between the two reads.
  The ref still names only its hash (RFC 6 §2.5). Changing it later costs a field and the join,
  nothing persisted. A `ponytail:` at the join says the block size decodes the whole block record,
  whose carried list grows with its chunks, and names the upgrade, the size kept on the chunk
  record, if lookups over many-chunk blocks show in a profile. Prior art: none found.
- **D2, re-resolution is a pure function of the locations seen.** The loop of RFC 8 §7.7 keeps the
  locations that missed and calls `reresolve(missed []entity.Location, next entity.Span, asOf
  entity.JournalVersion) step`, answering fetch, re-ask the journal, zeros, past end of file or
  **Lost**. It is table-tested on its own (`CLAUDE.md`, "Put the logic in a function of its
  inputs"), so a location moving twice is tested here although nothing in v1.1 moves a chunk (§9).
  Prior art: none found.
- **D3, a fill copies its bytes, bounded per share.** Once a chunk is in `dst` the reply needs the
  fetch no longer, but `dst` is the caller's once `Read` returns, so a fill copies the extent's
  verified bytes and runs in its own goroutine, beside the reply. RFC 8 §1.1 forbids a payload
  copied through engine state: the copy is a buffer in flight to the journal, as a write's `p` is,
  kept by nothing and released when `Fill` returns (R18). Each binding holds a byte semaphore of 4
  MiB of fills in flight, taken with a try-acquire; a fill that does not fit is declined, counted
  `declined`, so no fill waits and no share's fills hold another's (R41). A `decision:` at the
  hand-off names both rules and the ceiling, fills declined under a burst of cold reads, withdrawn if
  B4 shows declined fills costing re-fetches. Prior art: Linux's page cache inserts read pages
  asynchronously and drops readahead under memory pressure (from memory).
- **D4, one flow per share serves every carved read of the share.** A ref of an older namespace
  generation is fetched through the share's one flow, on the share's store. In v1.1 every
  generation of a share lives on one store: only a re-home (RFC 27) moves a share between
  namespaces, and `Bind` refuses a new store or write namespace for a served share (epic 16's D5),
  so a binding's flow never changes under a read. A `decision:` at the fetch names the ceiling and RFC 27 as what withdraws it. Prior
  art: none found.
- **D5, a file's read state lives in the speculator's own table.** The first part's file entry
  exists only while it holds pending work, so a file that is only read has none. The speculator
  keeps, per file, the end of the last read, the length of the sequential scan it continues, the
  read-ahead window, the cancel function of its queued hints and its speculatively filled extents
  not yet read, in a table sharded by file like the file table, each shard's lock held for the
  update only, never across I/O. The table holds at most 10⁴ files; the least recently read is
  dropped first. That meets §1.1 (keyed by file) and §3.3 (memory). A `ponytail:` names the ceiling,
  a file dropped from the table relearns its window, and the upgrade, a cap from RFC 24's memory
  budget once it sets one. Prior art: Linux keeps readahead state per open file (`struct
  file_ra_state`, from memory).
- **D6, §7.4 S5's held check is the journal's `Extents`.** The handle already answers `Extents`
  (RFC 1 §3.7) on `feat/storage-v2`; the engine's `Handle` gains it with T17.6, its first caller,
  and the in-memory journal with it. No marker. Prior art: none found.
- **D7, `fill`'s third rule and pre-warm are not built.** They need a pre-warm, which is deferred
  with `PreWarm` (the offload plan's §6.1 row); the function takes no pre-warm input, and adding one
  later changes an unexported signature only. Rule 2 compares the scan's length with the window's
  cap, `readahead.max_blocks` and `readahead.max_bytes`, the most the window can be. No marker: the
  ledger row records it.
- **D8, the missing-chunk backstop is logged by `kvstore`.** The batch that finds a source ref with
  no chunk record, or one naming a deleted block, runs inside `kvstore`'s `Resume`, which already
  carries it as **Lost** and which the engine does not see into. `Content` gains `WithLogger`, as
  `Lifecycle` has, and the batch logs the destination file, the extent and a running count at
  `Error`. RFC 6 §10.2 names no series for it, so the count lives in the log line; a `decision:` at
  the log says so, withdrawn when RFC 6 names a series. Reporting it through `Resume`'s result
  instead would change RFC 6's surface and epic 14's removal source for a case a correct store never
  reaches. Prior art: none found.
- **D9, a read joining a fetch does not link to its span.** RFC 8 §14 asks for the link, but the
  syncer joins fetches internally and its `Fetch` reports no join. Each read opens its own
  `engine.fetch` span; a `decision:` at the span says so, and the offload plan's §6.1 gains the row
  in this pull request. Prior art: none found.
- **D10, pressure cancels through one entry point.** `(*speculator).cancelQueued(j)` cancels every
  queued hint of journal `j`'s shares. This epic calls it from the first part's refusal path in
  `capacity.go`, the one place a write meets capacity pressure today; epic 18 adds the call from
  `admit` when it paces. Prior art: none found.
- **D11, used speculative bytes are counted at the next demand read.** A speculative fill records
  its extent in the file's read state (D5); a demand read answered from held bytes overlapping it
  adds the overlap to `engine_speculation_used_bytes_total` and forgets it. A `ponytail:` says an
  extent dropped with its file's state, or evicted unread, is counted neither way, at most D5's 10⁴
  files' worth, and names the upgrade, a count from the journal's eviction report, if B4 needs the
  figure exact. Prior art: none found.
- **D12, §7.8's quarter compares plaintext with the encoded block.** The missing bytes are
  plaintext and `Location`'s size is encoded, so a compressible block takes the whole-block shape
  sooner. The quarter's `decision:` (rule 9) gains that sentence. Prior art: none found.

## 5. Target tree

On `feat/storage-v2`.

```text
pkg/metadata/entity/content.go      T17.1  Location, Span.Loc
pkg/metadata/kvstore/covering.go    T17.1  the location join in Content.Covering
pkg/metadata/kvstore/clone.go       T17.8  the backstop's log; Content.WithLogger
pkg/engine/
  read.go                           T17.2  the carved row's reply in file order, the remote class
  fetch.go                          T17.2  the fetch per carved part, its errors; T17.3 reresolve and its loop; T17.4 blockShape
  readstate.go                      T17.4  the speculator's per-file read table (D5)
  fill.go                           T17.5  the per-share fill hand-off beside the reply
  policy.go                         T17.5  fill (epic 15's file, or created here if this lands first)
  speculate.go                      T17.6  the speculator, windows, hints; T17.7 budget, cancellation, counters
  deps.go                           T17.5  Handle.Fill; T17.6 Handle.Extents
  capacity.go                       T17.7  the refusal path's cancelQueued
  clone.go                          T17.8  copies narrowed to runs no live chunk covers
  types.go, health.go               T17.2 to T17.7  the Stats fields filled
  read_test.go                      T17.2  the stub-backed "carved after a hole" row rewritten over kvstore
pkg/engine/enginetest/
  fake.go                           T17.5  the in-memory journal's Fill and FailNextFill; T17.6 its Extents
  journal_test.go                   T17.5, T17.6  Fill and Extents agreement with the real journal
  offload.go                        T17.9  FailNextFill, HoldBetweenReadSteps, HoldNextRelease, Evict
pkg/engine/conformance/
  rfc8_cover_test.go                T17.2  TestRFC8A_CarvedRunFailsUnavailable becomes the carved row's fetch test
  rfc8_remote_test.go               T17.9  "§7.1 the join"'s carved and stale rows
test/engine/known.yaml              T17.9  entries removed or their messages rewritten
```

## 6. Subtasks

### T17.1 The covering lookup names a carved span's chunk location

**Subtask T17.1 estimate: 2 man-days.**

- **Track:** C. **Depends on:** S2.
- **Covers:** R5, R6.
- **Deliverables:** in `entity`, as RFC 6 §8.1 and §2 now say:
  ```go
  // Location is where a chunk's body is: its block, its position and length there, and the
  // block's encoded size (RFC 6 §8.1).
  type Location struct {
  	Block                       BlockName
  	Position, Length, BlockSize int64
  }
  // Span gains:
  	Loc Location // when Carved, but a zero ref: where the chunk is; zero when its record is gone,
  	             // names a deleted block or names a block with no record
  ```
  `kvstore`'s `Content.Covering` fills `Loc` inside its one `View`: one point read of the chunk
  record per distinct hash the results name, in the namespace `Ref.NSGen` names, and one of the
  block record per distinct block; `adopt` and `Allocation`, which share `covering`, do not. D1's
  `ponytail:`. Consumed by T17.2 (the fetch), T17.3 (the location compare), T17.4 (the quarter) and
  T17.8 (the narrowed copies).
- **Acceptance criteria:**
  - A carved span over a committed block names that block and the body's position and length the
    commit recorded; a zero ref's span names none; a span whose chunk record was pruned, or whose
    block is deleted, names none.
  - Two spans of one chunk cost one chunk read; the lookup's reads grow with results, not with the
    file's refs.
- **Tests:** in `kvstore`, over the memory KV, Badger under `t.TempDir()` and FoundationDB where it
  runs: a block committed with two chunks, a file's refs over both and a zero ref, its covering
  lookup asserting each span's `Loc` against the commit's `ChunkAt`s (catches a location read from
  the wrong namespace generation or position, R5); the block marked deleted through `Blocks`, and
  then pruned, asserting no `Loc` each time (catches a location of a deleted block, which would be
  fetched, R5); `kv`'s counting wrapper over a file of 10⁴ refs reading a 4-ref extent, asserting the
  reads stay within a constant of the results (catches a scan of the file's refs or of chunk
  records, R6). No test double.
- **Rules:** rule 8: run over each backend RFC 6's suite names.

### T17.2 The carved row: fetch, verified reply, distinguishable failures

**Subtask T17.2 estimate: 5 man-days.**

- **Track:** C. **Depends on:** T17.1.
- **Covers:** R1 to R4, R7 to R18, R41 to R44.
- **Deliverables:** in `read.go` and `fetch.go`, `resCarved` fetches: each carved part of a missing
  extent, one per block, is one `Fetch(ctx, name, want)` on the binding's `syncer.Flow`, `name` the
  span's block as RFC 4's `remote.Name`, `want` its chunks as `remote.ChunkRange`s from `Loc`; the
  parts of one read run at once, one goroutine per part joined before `Read` returns; each yielded
  chunk is copied into `dst` at its file offsets (the ref's `Skip` and length) before the next
  iteration; the first failing part sets `n` and the error. `remote.ErrNotFound` hands the part to
  T17.3; `remote.ErrCorrupt` and `ErrMalformed` fail it as `vfserr.ErrCorrupt`;
  `ErrMaterialDestroyed` as `vfserr.ErrLost`; `ErrMaterialUnavailable`, `ErrUnknownMaterial`,
  `remote.ErrInvalid`, every transient, throttled or denied error and the context's deadline as
  `vfserr.ErrUnavailable`. The `decision:` that a carved run fails unavailable is deleted (rule 9);
  D4's `decision:` at the fetch. The tests that pinned it go with it: the conformance test
  `TestRFC8A_CarvedRunFailsUnavailable` becomes the carved row's own test, fetching what it carves,
  and `read_test.go`'s "carved after a hole" row, which runs over a stub covering lookup, is
  rewritten over `kvstore` (`CLAUDE.md`: a touched test is brought in line). `readClass` gains
  `classRemote`; `Stats.Fetches` and `FetchBytes` filled; an `engine.fetch` child span per fetch
  with share, file, bytes and outcome (D9's `decision:`). The fetch runs on the binding's `flow`
  epic 16's T16.1 opens; a hook between the journal step and the metadata step is set per instance
  from `export_test.go`. A test helper, `carved(t, ...)`, writes a file's
  existence and refs through `kvstore`'s `Commit` and `Intend` and the block through
  `codec.Store`'s put into RFC 4's in-memory store, so the journal never held the bytes. Consumed by
  `Read`'s callers and T17.3 to T17.9.
- **Acceptance criteria:**
  - A cold read of a carved extent returns its bytes; a read spanning held, hole and carved parts
    returns each in file order.
  - With the fifth of eight chunks corrupt, `n` covers the first four exactly, `dst` past them holds
    no byte of the fifth, and the error is `ErrCorrupt`.
  - A stale ref under a committed overwrite reads **Lost** and no fetch is counted.
  - With the store unreachable, the read fails `ErrUnavailable`, never `ErrLost`; past the read's
    deadline likewise; a destroyed material reads `ErrLost`, an unavailable or unknown one
    `ErrUnavailable`.
- **Tests:** at `Read`, which the filesystem service calls, over the in-memory journal, `kvstore` and
  RFC 3's syncer over RFC 4's in-memory store: the mixed read (catches a part written out of order,
  or zeros for a carved part, R3, R11); the fifth-chunk corruption through RFC 4's fault wrapper,
  kind 3, asserting `n`, the untouched tail of `dst` and the error (catches a copy before
  verification or a wrong `n`, R12, R14); the stale ref with `Stats.Fetches` 0 (catches a fetch of a
  superseded chunk, R7); the unreachable store and the expired deadline on the injected clock
  (catch `Lost` reported for an outage, R8); the material errors raised by RFC 5's in-memory
  `keys.Provider` (catch a destroyed material read as corrupt, R9); a cold read over two blocks with
  every get held at the fault wrapper until both blocks' gets have arrived, then released (catches
  parts fetched one after another, which never brings the second while the first is held, R4, R17);
  a whole-block fetch whose chunks, after the next iteration, still read right in `dst` (catches a
  borrowed buffer kept instead of copied, R16); the read order: the hook holding a read between its
  journal step and its metadata step while a ref is committed over the extent, released, asserting
  the bytes and no **Lost** (catches metadata asked first or a resolution kept, R1, R2); share A's
  cold read held at the fault wrapper while share B's cold read returns, the generous timeout only a
  backstop (catches a lock or queue shared across shares, R41); the span through the fixture's
  `Trace` (R43); the loss log once per extent (R44); `prof.Watch` after a read that failed part-way
  (catches a fetch goroutine left running and a buffer held past the read, R18).
- **Rules:** rule 4: the per-part goroutines are joined before `Read` returns; rule 6: the pinning
  test's entry, if any, follows it; rule 7: imports through contracts only.

### T17.3 Re-resolution of an absent object

**Subtask T17.3 estimate: 2.5 man-days.**

- **Track:** C. **Depends on:** T17.2.
- **Covers:** R2, R19 to R23, R42, R44.
- **Deliverables:** in `fetch.go`, `reresolve(missed []entity.Location, next entity.Span, asOf
  entity.JournalVersion) step`, pure (D2), answering fetch, re-ask, zeros, past end of file or lost,
  and its loop on `remote.ErrNotFound`: covering lookup again for the part, the step applied, the
  journal re-asked for an uncarved run above `asOf`, bounded by the read's deadline; no
  re-resolution on corrupt, transport or timeout errors. `Stats.Reresolves` filled. Consumed by
  T17.2's fetch path and T17.9's "§7.7 re-resolution" row.
- **Acceptance criteria:**
  - A location that misses twice reads **Lost**; one whose second answer differs is fetched there.
  - A truncate between resolution and get, whose block is then gone, answers past end of file, not
    **Lost**.
  - A corrupt get never re-runs the lookup.
- **Tests:** `reresolve` by table: new location, repeated location, no location, hole, past end of
  file, uncarved above `asOf` by `applied`, by overwrite version, and at or below both (each row
  catches one wrong step, R20 to R22). At `Read`: the block deleted from RFC 4's in-memory store
  with its chunk record kept, asserting **Lost** after exactly one re-resolution (catches an
  unbounded loop, R22); the file truncated in `kvstore` and its block deleted from the store while
  the get is held at the fault wrapper, asserting a short read (catches **Lost** for a removal,
  R21); a corrupt chunk, asserting `Reresolves` 0 (catches re-resolution on corruption, R23); a
  fresh read after a re-resolution resolving anew (catches a remembered location, R2). No test
  double beyond the fault wrapper.

### T17.4 Chunks or the whole block

**Subtask T17.4 estimate: 1.5 man-days.**

- **Track:** C. **Depends on:** T17.3.
- **Covers:** R15, R24, R25, R34, R42.
- **Deliverables:** `readstate.go`, the speculator's per-file read table (D5, its `ponytail:`), its
  first fields the end of the last read and the length of the scan it continues;
  `blockShape(missing, blockSize int64, startsBlock, continues bool) shape`, pure, with RFC 8
  §7.8's quarter `decision:` at it, D12's sentence included; a whole-block fetch passes an empty
  `want` and copies only the read's chunks. `Stats.Fetches` by shape. Consumed by T17.2's fetch,
  T17.5's `fill` and T17.6, which adds its fields.
- **Acceptance criteria:**
  - A 4 KiB random read of a 4 MiB block asks for its chunk; a read starting the block, or
    continuing the last, asks for the block; a read missing more than a quarter asks for the block.
  - A file only ever read, with no pending work, keeps its read state between reads.
- **Tests:** `blockShape` by table at the quarter's edge, one byte either side (catches an
  off-by-one or a swapped condition, R24); at `Read`, the shape counters for a random and a
  sequential pair of reads of a file the journal holds nothing of (catches state kept on the file
  entry, which such a file lacks, R24); a whole-block fetch whose `dst` holds only the read's bytes
  (catches neighbouring chunks copied, R15); `want` ranges equal to the spans' locations (catches a
  range splitting a chunk, R25); the table past 10⁴ files dropping the least recently read (catches
  an unbounded table, R34).

### T17.5 Fill beside the reply

**Subtask T17.5 estimate: 2.5 man-days.**

- **Track:** C. **Depends on:** T17.4.
- **Covers:** R13, R16, R18, R26 to R29, R41, R42, R63.
- **Deliverables:** `fill(r readInfo, s journalStats) bool` in `policy.go`, pure (R27), rules 1 and
  2 (D7), answering fill whatever its inputs when `Deps.Gates` is ungated (epic 16's D6), the
  low-water mark from `Settings.FillLowWater` and the scan length from T17.4's state;
  `Handle.Fill` joins `Deps`, its first caller here; `fill.go`'s hand-off (D3): a copy of each
  demanded extent's verified bytes, with step 1's `asOf` and the ref's `Newest`, filled in its own
  goroutine under the binding's 4 MiB byte semaphore, joined at `Close`; a `journal.ErrStaleFill`
  or a capacity refusal counted `refused`, never returned; `Stats.Fills` by result; D3's
  `decision:`. The in-memory journal (`enginetest/fake.go`) gains `Fill` and, as its kind 3 fault,
  `FailNextFill`; `enginetest/journal_test.go` holds its `Fill` to the real journal's beside
  `TestShareAgreesWithTheJournal`: the stale refusal, never overwriting held bytes, the offloaded
  bit set, the capacity refusal. Consumed by `Read`, T17.6's speculative fills and T17.9's
  `FailNextFill`.
- **Acceptance criteria:**
  - A cold read with `Fill` failing returns the fetched bytes and no error.
  - A write landing between the fetch and its fill survives: the next read returns the write.
  - A fill makes the next read of the extent local (`Stats.Fetches` unchanged by it).
  - Below the low-water mark, or in a sequential scan past the window's cap, the fill is `declined`.
- **Tests:** `fill` by table over free capacity at and either side of the mark and scan lengths
  either side of the cap (each row catches one inverted rule, R26); the same table ungated, every row filling (catches
  a gate that ignores the switch, R63); at `Read`: `FailNextFill`,
  asserting the bytes and no error (catches a reply that waits on or fails with the fill, R29); a
  fill held before its `Fill` call while the extent is written, then released, asserting the write
  reads back and the fill counted `refused` (catches a fill that overwrites, R28, R29); a second
  read after a done fill with `Fetches` unchanged (catches a fill of the wrong extent or version,
  R28); the reply's bytes asserted while the fill is still held (catches a reply answered by
  re-reading the journal, R13); a share's semaphore filled by held fills, a further fill of that
  share declined while another share's fill is done (catches an unbounded hand-off or one shared
  across shares, R18, R41); the agreement test (catches the in-memory `Fill` drifting from the
  real one).
- **Rules:** rule 4: fills are joined at `Close`; rule 8: the agreement test.

### T17.6 Read-ahead windows and hints

**Subtask T17.6 estimate: 2.5 man-days.**

- **Track:** C. **Depends on:** T17.5.
- **Covers:** R30, R31, R33 to R36, R45, R47 to R49.
- **Deliverables:** `speculate.go`: the unexported `speculator`, one per engine, over T17.4's table:
  each read's `(file, off, len, time)` updates the window as R36 says; each hint is a `(file,
  block)` resolved through the covering lookup, checked against the journal's `Extents` (D6), and
  issued as `Prefetch(ctx, name, want)` with `want` empty or the missing chunks by T17.4's rule;
  its stream is consumed by a bounded set of consumers that hand each chunk to `fill`; a read that
  does not continue cancels the file's queued hints. `Handle.Extents` joins `Deps`, its first
  caller here; the in-memory journal gains `Extents`, held to the real journal's by the agreement
  test. Consumed by `Read`, and by T17.7.
- **Acceptance criteria:**
  - The window follows R36's example step by step.
  - A held file read sequentially twice issues no prefetch; with one block never written to the
    journal, only that block's chunks are prefetched.
  - After a restart the window starts at zero and opens on the first read that continues the last.
- **Tests:** RFC 8 §7.4's checks, `unit`, over RFC 3's syncer and RFC 4's in-memory store: §7.4 S3's
  window row, a sequential, a random and a strided replay with the hints asserted per read (catches
  a window opened for a stride or not reset, R36, R47); §7.4 S5's read leg over a file whose
  blocks the journal holds but one, which is only carved, with the fetch and speculation counters 0
  on the held reads, then exactly that block's chunks (catches a whole-block refetch of a held file,
  R35, R49); §7.4 S4's read-ahead half, the engine closed and reopened over the same journal and
  store, asserting `kvstore`'s key count unchanged by the reads and the window reopening (R34, R48);
  §7.4 S1, every prefetch get held at the fault wrapper, which records arrival order and passes each
  call through, while a demand read of another file is issued, asserting its get reaches the store
  before any held prefetch is released (catches a hint issued through `Fetch`, R31, R45); the
  agreement test for `Extents`. The replays use the injected clock for `time`.

### T17.7 Speculation budget, cancellation and its counters

**Subtask T17.7 estimate: 2.5 man-days.**

- **Track:** C. **Depends on:** T17.6.
- **Covers:** R32, R33, R37, R41, R42, R46.
- **Deliverables:** a per-binding atomic of speculative bytes in flight, a hint issued only while it
  fits `ShareSettings.SpeculationBudget`, released as its stream ends; speculative fills skipped
  below the low-water mark; `cancelQueued(j)` (D10), called from `capacity.go`'s refusal path;
  `Stats.SpeculationBytes` and `SpeculationUsedBytes` (D11, its `ponytail:`). Consumed by epic 18,
  which calls `cancelQueued` from `admit`.
- **Acceptance criteria:**
  - Speculative bytes in flight for a share never exceed its budget.
  - A write refused for capacity cancels every queued hint of its journal's shares, and no read
    fails for it.
  - A speculatively filled extent read later counts its bytes as used.
- **Tests:** §7.4 S3's budget row, 10³ files read sequentially at once with every prefetch held at
  the fault wrapper, asserting the sum of the held prefetches' requested bytes never passes the
  budget (catches a check after the issue or a release before the stream ends, R33, R46); a refusal
  through the first part's capacity fixture while hints are held, asserting each cancelled and every
  concurrent demand read answered (catches cancellation that fails a read or misses a share, R32,
  R37); free capacity below the mark with a hint's chunks arriving, asserting no fill (R32); a
  used-bytes count after a demand read of a speculated extent (R42); two shares speculating with one
  share's prefetches held, the other's still issued (catches a lock shared across shares, R41).

### T17.8 Clone: narrowed copies and the backstop's log

**Subtask T17.8 estimate: 2.5 man-days.**

- **Track:** C. **Depends on:** T17.5.
- **Covers:** R38 to R40.
- **Deliverables:** in `clone.go`, `size` keeps a held run unless a carved span over it, from the
  source's covering lookup at `asOf`, has a `Loc` and a ref `Newest` at or above the run's version,
  so `copyRuns` copies only those and the adoption takes the rest; a held run over a ref whose chunk
  is gone is still copied, never left to the backstop. §9.1's clone-copies `decision:` reworded to
  say so. In `kvstore`, `Content.WithLogger(log *slog.Logger) Content` and the clone batch's log of
  each source ref it carries as **Lost** with a running count (D8's `decision:`); the carrying itself
  is the first part's. The fixture passes its logger. Consumed by the first part's clone path and
  T17.9's clone rows; `WithLogger` by the integration epic.
- **Acceptance criteria:**
  - Cloning a source held whole and offloaded whole copies nothing and reads the source's bytes
    from the adopted refs; one held newer than its ref copies exactly that run.
  - A held source run over a ref whose chunk record is gone is copied, and reads its bytes.
  - A source ref, not held, whose chunk record is gone or whose block is deleted reads **Lost** in
    the destination and logs once at `Error`.
  - A demand read's fill of the destination, held across the clone's journal step and released,
    installs nothing, and the extent reads the source's bytes.
- **Tests:** at `Clone` and the destination's `Read`, over the in-memory journal and `kvstore`: the
  two copy cases, asserting the reservation's byte count (catches every held run still copied, or a
  newer run adopted from a stale ref, R38); the held run over a gone chunk (catches a narrowing that
  drops it to the backstop, reading **Lost** for bytes the journal has, R38); the two backstop legs,
  record gone and block deleted, asserting **Lost** and the captured log line, the carry a
  regression check of the first part's (catches a silent carry, R39); the held fill, with
  `Stats.Fills` counting it `refused` (catches a fill installed over the removal, R40); the remote
  unavailable throughout the first case, asserting the clone completes (catches a clone that
  fetches, R38).

### T17.9 The conformance rows

**Subtask T17.9 estimate: 2.5 man-days.**

- **Track:** C. **Depends on:** T17.2, T17.3, T17.5, T17.7, T17.8.
- **Covers:** R50 to R62.
- **Deliverables:** epic 12's proposed controls made real in `enginetest/offload.go`:
  `FailNextFill(err error)` at the journal's `Fill` (the fake's, or a fault seam on the real one);
  `HoldBetweenReadSteps() (entered <-chan struct{}, release func())`, the per-instance hook T17.2
  added; `HoldNextRelease() (entered <-chan struct{}, release func())` on a clone's
  `CloneHold.Release`; `Evict(id entity.FileID)` through the real journal's `Release`, refused by the
  journal until S6. `Relocate` stays refused by name (§9). "§7.1 the join"'s carved and stale rows
  as a test of their own beside `TestRFC8A_TheJoin`, with refs written as
  `TestRFC8A_CarvedRunFailsUnavailable` writes them. Every row of R50 to R62 runs past its controls;
  each that passes leaves `test/engine/known.yaml`, and each still red has its `message:` rewritten
  to the check it now meets: the carver's refusal until epic 15, `RunPass`'s until epic 16's
  T16.3,
  `Evict`'s until S6 (rule 6). The unit tests of T17.2 to T17.8 that check what a row checks stay:
  they are the green coverage until the row turns green.
- **Acceptance criteria:**
  - No row's entry names a check the row no longer meets first.
  - Each row that turns green here was seen failing on its own assertion with this epic's change to
    its path reverted, as epic 12's §9 asks.
- **Tests:** the thirteen rows, in `engine-conformance`. Doubles: RFC 4's in-memory store, kind 2,
  under its fault wrapper, kind 3; the fixture's fault `kv.KV`, kind 3. "§7.1 the order" is also run
  once over a build with the two steps swapped, which must fail it (R51).
- **Rules:** rule 6, the entries; rule 8, the rows run over the real journal on a real directory.

## 7. Order of work

```mermaid
flowchart TD
  S2(["S2 red rows in CI"]):::sync --> T17_1["T17.1 Location in the covering lookup"]
  T17_1 --> T17_2["T17.2 The carved row"] --> T17_3["T17.3 Re-resolution"] --> T17_4["T17.4 Chunks or block"]
  T17_4 --> T17_5["T17.5 Fill"] --> T17_6["T17.6 Read-ahead"] --> T17_7["T17.7 Budget and cancellation"]
  T17_5 --> T17_8["T17.8 Clone"]
  T17_2 & T17_3 & T17_5 & T17_7 & T17_8 --> T17_9["T17.9 Conformance rows"]
  T17_3 --> C1(["after T17.3: carved reads correct end to end"]):::sync
  T17_7 --> C2(["after T17.7: epic 18 can cancel speculation on pressure"]):::sync
  classDef sync stroke:#2c5aa0,stroke-width:3px,stroke-dasharray:4 2
```

Checkpoint after T17.3: a carved read is correct, whatever it fetches. Checkpoint after T17.7: the
entry point epic 18 calls exists. T17.8 needs only T17.5 and can run beside T17.6 and T17.7.

## 8. Suggested pull requests

| PR | Subtasks | Man-days | After | Why together |
| --- | --- | --- | --- | --- |
| 1 | T17.1, T17.2, T17.3 | 9.5 | — | carved reads end to end: the location, the fetch and its failures, re-resolution |
| 2 | T17.4, T17.5, T17.6, T17.7 | 9 | PR 1 | what a read fetches and keeps: the shape, the fill and the speculator, which reads both |
| 3 | T17.8, T17.9 | 5 | PR 2 | the clone's adoption and the rows that check the whole epic |

A suggestion only: one pull request per subtask works as well. PR 3's T17.8 may open beside PR 2
once T17.5 is in.

## 9. Open points

- **The relocation leg of "§7.7 re-resolution" (pending the user's answer).** The row relocates a
  chunk twice between resolution and get, but nothing in the repository moves a chunk while its
  refs are live: RFC 6 §7.3's relocation is GC's (RFC 9), and `kvstore` has no `Relocate`. Written
  in place (recommended): D2 tests the rule as a pure function, the row's relocation leg becomes a
  subtest of its own whose `known.yaml` entry waits on RFC 9's relocation, and the offload plan's
  §6.1 gains that row in this pull request. The other option builds `kvstore`'s `Blocks.Relocate`
  here, as RFC 6 §7.3 specifies it with its checks, as a T17.10 of about 3 man-days, ahead of GC; it
  lands a GC mechanism whose only caller is a test.
- **Rows that need the real journal's `Release`** ("§7.1 the order", "§7.2 streaming, verified",
  the fill rows, "§9.1 no pre-clone fill", the cold-read row) stay red after this epic until S6;
  whichever of this epic and the journal plan's epic 9b lands second removes their entries.
- **The gates' switch** (epic 16's D6) is pending the user's answer there; `fill` reads whatever
  form it settles on, and R63's test follows it.
- **The fill bound** (4 MiB per share, D3) and **the read table's size** (10⁴ files, D5) are
  guesses; B4 in epic 22 measures declined fills, re-fetches and windows relearnt.
