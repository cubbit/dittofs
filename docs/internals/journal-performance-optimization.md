# Journal performance — the optimization ledger

This document is the journal's performance-optimization log: what each round of profiling
discovered, what was implemented because of it, what was measured before and after, and what
was deliberately left unbuilt with the trigger that might reopen it. It exists so the next
optimizer — human or agent — starts from the previous round's knowledge instead of re-deriving
or re-disproving it. One **iteration** section per optimization round; the older rounds stay as
written, with a line at the top of each noting what a later round overturned.

Where the numbers live: reference benchmark runs and keep-or-change decisions live in
[`journal-bench-results.md`](journal-bench-results.md); a developer machine's figures never feed
those and this ledger says so wherever one of its own figures came from one. The 2026-11-26
round's raw artifacts (pprof profiles, bench outputs, per-finding reports) were kept on the
author's machine and in the git-excluded `docs/superpowers/plans/2026-11-26-journal-perf-*.md`;
the *conclusions* those artifacts supported are all repeated here, so this file stands alone.

## How to run a round (the measurement rules)

These came out of the 2026-11-26 round the hard way; they apply to every future round.

- **Same-hour pairs.** A change's before and after run on the same box, same hour, same bench
  directory, `-count=10 -benchtime=1x`, small scale (`DITTOFS_BENCH_SCALE=small`), no profilers.
  A day-old baseline is not a baseline: the raw twin's own medians moved ~2× between days on the
  same MacBook.
- **The raw twin is the validity gate, not just the yardstick.** A pair counts only if the twin's
  own median stays within ±25% of the same pair's before run. The twin is bimodal on some boxes
  (~1.6 vs ~4.3 GB/s within one run); when it jumps, the pair is void, not interesting.
- **Gate a row at ≥1.25× median** for normal rows, ≥1.5× for sync rows (±40% noise even
  same-hour), and expect allocs-based gates, not MB/s, where the fix's gain is churn (a pool).
- **Profiles mislead in three known ways.**
  1. A Go mutex profile attributes contention at the *holder's* `Unlock`, and `runtime.unlock`
     (the scheduler's own lock) appears as a top "mutex" frame: the 2026-11-26 round initially
     read 89% of lock delay as the capacity ledger when the true figure was 9.3% of total delay
     and 2–4% of writer time. Always `-peek` the holder and separate the scheduler's share.
  2. Time blocked inside a syscall (a flush, an I/O) is invisible to the CPU *and* the block
     profile: absence of samples is not absence of cost. Cross-check with wall-clock arithmetic
     over the row's own operation count before believing a profile.
  3. At 16–64 goroutines on ~11 cores, even a µs-scale critical section shows large "delay" from
     scheduler convoy alone. Confirm the flatness exists at **1 writer** before blaming a lock —
     a row flat at 1 writer (where nothing contends) cannot be a lock story.
- **The counted-allocation test is the allocation gate.** `pkg/journal/conformance/`
  `counted_alloc_test.go` pins the exact per-call allocations (`maxWriteAllocs`); when a change
  removes a counted allocation, the pin tightens in the same change or it guards nothing.
- **`-bench` patterns containing `/` skip top-level benchmarks** that lack sub-benches: run
  row families as separate invocations. Median tools that pool every file given to them
  misgrade: parse per row, per file.

## Iteration 1 — 2026-11-26: page faults, seals, locks, spares

**Context.** RFC 1's J1–J3 conformance benches (tags `journal_conformance,journal_bench`) had
just gained the large sequential row; a profiling pass on a MacBook (APFS, `F_FULLFSYNC`) put
the row at ~250 MB/s against its raw twin's ~1300–1500, J2 writer-scaling flat at ~38 MB/s from
1 to 64 writers, and J2's 16-writer p99 write at 131–172 ms.

### Discovered

- **The dominant write-path cost was the fresh-segment first write** (`H-align` in the round's
  notes): content records are 65,640 B (68 B framing + 64 KiB) written at stride 48+k·65,640, so
  **no record starts or ends on a page boundary**, and each write's tail half-page falls in
  segment space that macOS preallocated (`F_PREALLOCATE`) but never wrote — the kernel faults
  it in on the write. Measured: 526 MB/s unaligned-into-preallocated vs 9745 MB/s the same bytes
  into a zero-filled file (alignment itself is free); journal rows cross-confirm — an untimed
  pre-read of each segment lifted the large row 298 → 1757 MB/s, p99 1388 → 97 µs, and lifting
  the spare pre-roll lifted J2_1 8.0× and J2_16 6.1× (p99 167 ms worst → 9 ms). **One cause,
  three rows** (large row, J2 flat floor, 16-writer p99). On a Mac every freshly made segment is
  in this state; on Linux the maker already zero-fills spares (RFC 28 Appendix A) so the
  mechanism should not exist there — measured only on macOS, Linux is an open item below.
- **Seals are inline in the timed path but bounded at ≤20%**: 3 rolls per 512 MiB iteration, 4
  inline `F_FULLFSYNC`s, ~0.4 s of a 2.1 s iteration. The 1 s sync-bound worker never blocks the
  writer (it releases `appendMu` across its fsync; writer blocked 13.8 ms in `WriteAt` across 10
  iterations). SegmentCap shows no dose-response (512 MiB: 1.16×; 1 GiB: 1.06×) — segment size
  is not the lever.
- **The capacity ledger is not a ceiling**: `c.mu` crossed 4×/write costs 2–4% of writer time;
  the critical section is map lookups and atomics (~1 µs). Two droppable crossings exist
  (below) but none clear a gate today.
- **Spare-pool waits never happen at stock**: 16 ready spares cover every take; no block profile
  shows a pool park. J2's flat floor is the **maker's flush** (one 256 MiB make ≈ 165–205 ms)
  landing inside the timed window when a stream's first roll kicks it, with the writers' closing
  `Sync` queueing behind it (~200 ms/iteration floor; this also explains J1 64k-sync's bimodal
  17/90–192 ms).
- **The per-append record buffer was 94.6% of all allocation** (the whole-record `make` at the
  stream's encode site), but only 1 of ~9.6 allocations per write — exactly 6 of the 9.6 are
  production (buffer, `leaveDrain` defer, the capacity `Grant`, `Share.enter`'s method value,
  the file lock's entry and closure), the rest are harness.

### Implemented

Each change lands on its own pull request, P2 stacked on P1a; each was measured in an isolated
worktree first and then stacked, with the narrow test cells, `-race` on segment/platform/journal
and tagged `go vet` green.

1. **P1a — segment read-ahead hint (#6472).** A windowed `F_RDADVISE` (via the platform call
   table, args pinned with `runtime.Pinner`): the stream asks for `ReadAheadWindow` (256 MiB)
   past its append position, first at the roll from the header, and again each time the
   position passes half of the last window, and a journal-owned worker (not a per-roll
   goroutine) makes the call outside every lock. At the default 256 MiB cap that is one
   whole-segment hint per roll. Linux/Windows are decisions-marked no-ops (Linux's zero-fill
   makes the pages resident). Prototype pairs: **large row 293 → 603 MB/s (2.06×), p99 1388 →
   120 µs; the 64k-nosync control *rose* 5.4×** (284 → 1545 MB/s) — the inline form regressed
   that control and the async form fixed it: `F_RDADVISE` over never-written space is
   *synchronous* (34–60 ms for 256 MiB), so it must not sit on any writer's path. The production
   form's own pairs, in #6472's description: 1.98–2.01×, p99 111–126 µs. A whole-segment hint
   **does nothing at a 1 GiB cap** (only ~60% stays resident, pages-first evicted): hence the
   window.
2. **P2 — content-record buffer pool (#6473, stacked on #6472).** A journal-wide `sync.Pool`, 9
   classes of `1<<k + 128`, content records only — **header-only records' buffers stay
   un-pooled** (the durability ledger retains them and re-decodes for re-append; a pooled buffer
   there would corrupt recovery). One inline recycle point after the payload CRC, before
   `appendMu` is retaken. The counted-allocation pin `maxWriteAllocs` goes 10 → 9 in the same
   change. **B/op −97.3% (619 → 16.8 MB), allocs exactly −8,236 with GC off** (the overshoot in
   profiled runs was per-GC-cycle allocations no longer built), MB/s +1.5% alone.
3. **The pair, stacked, is super-additive** (measured on the prototype stack of P1a and P2; it
   describes the tree once both have merged): P1a removes the page-fault stall, then the write loop is
   CPU-and-GC bound and P2's churn reduction counts — **large row 292 → 732 MB/s (2.50×), p99
   92–96 µs, control 5.9–8.1×**; ~24% above P1a alone (P2's isolated gain is the honest +1.5%).
   Quote P2's stacked gain only when stacked; #6473's own production pair is one same-hour pair,
   +15% over P1a alone.

### Parked, with triggers

- **Empty-`Release` `c.mu` skip** (no-op holds were 21.5% of J1's lock delay) and **fold
  Claimed+Release into one settle**: both fail closed, ~0.5 and 1.5–3.5 half-days. Trigger: the
  **re-profile after P1a** (now unmasked: seals ≤20%, locks 2–4% of a 2–2.5× faster path ≈ 2–3×
  relative share) or a real >=128-writer server workload on the reference box.
- **ContentBand (P1b)** — a refill band above the RFC target so a burst of rolls kicks no make:
  J2-floor evidence (150–200 ms/iteration) justifies it; on Linux each make zeroes 256 MiB of
  real bytes (up to half the device's write bandwidth), so it is a *stronger* candidate there.
  Default 0 until the RFC 1 §6 footprint ruling. **Trigger: at P1a's throughput the stream needs
  ~5.6 rolls/s against a ~5 spares/s maker — the pool becomes the next wall exactly then.**
  More refill workers are *not* an alternative: makes are flush-bound (Mac) / bandwidth-bound
  (Linux), and more in flight makes it worse.
- **Linux `posix_fadvise(WILLNEED)` half of P1a**: mechanism probably absent (spares are
  zero-filled); measure on the reference box before believing either way.
- **Parked test-flake watch**: `TestSealRefusesAnAppendResumedFromTheRollGate` and
  `TestSoakRemovalChurnDrains` each went red once in an unprofiled parallel full pass, never
  reproduced; not on any prototype path. File issues if they recur rather than quarantining
  unseen.

### Dead ends — checked, do not retry without new evidence

1. **Lock-free capacity admission** (per-counter CAS): can refuse two racing writes both at the
   limit; `TestTake_AtomicNoReadThenDecide` pins that as a bug, and an RFC ruling would be
   needed regardless.
2. **Sharded `shareTally`**: the "map insert" is an update of an existing key worth ~20 ns;
   sharding breaks the exact-share-limit tests (`TestTake_ShareFigureIsExactUnderLandings`,
   the stale-figure and landing suites).
3. **Reserve-batching to cut `c.mu` crossings**: `Draw` still takes `c.mu` per write, and a
   reservation holds spares — the thing a stressed pool lacks.
4. **Pipelined sealing**: RFC 28 §2.2 requires the marker durable before a successor names the
   segment — forbidden as written; the drafted question for the RFC owner caps the payoff at
   the seal's ≤20% anyway.
5. **Syncless spare make**: RFC 28 §2.2's make ordering (allocate, sync, rename, dir-sync) is
   load-bearing — recovery re-adopting a spare checks only its size, so an unsynced spare would
   pass recovery unnoticed. RFC-owner question, drafted, not a candidate without the ruling.
6. **Whole-segment hints at large caps**: measured no-op at 1 GiB (cache evicts pages-first).
   The windowed form is the ceiling; do not "simplify" back to whole-segment.
7. **Sync contentions at the bound**: the 1 s sync-bound worker alongside aligned-rate flushing
   measured 0.95× (noise). `SyncBound` and `SegmentCap` should not be touched on this
   evidence — RFC 13's "J1 at 1 s and a longer bound" method stays available if a *new* story
   appears.
8. **Unwritten-extent write penalty**: rejected by aligned micro-benchmark, then superseded by
   the page-fault finding — the two are distinct; the fault story, not the extent story, is
   what the journal has.

## Open levers, ranked (as of iteration 1)

1. Post-P1a+P2 **residual ≈ 55% of the twin** (the stacked 732 vs ~1300–1500 MB/s, unprofiled):
   the now-unmasked inline seal drains are the likeliest share; the next round opens with a
   block-scoped profile of the post-P1a+P2 stack and an honest read of item 4 above.
2. **ContentBand**, once throughput makes the pool binding (see trigger above) — reference box.
3. Lock micro-costs, gated on the post-P1a re-profile or a >=128-writer Linux workload.
4. The J2 floor's other half: writer `Sync` queueing behind the make's flush — addressed by
   ContentBand's roll-kick change, re-measure with it.

**Figures caveat.** Every number in this iteration came from a MacBook/APFS developer box under
daytime load; its absolute values mean nothing outside it (the runs' own twin gate held each
pair). The mechanisms were confirmed three independent ways (micro-benchmarks, in-journal
pre-read, pre-roll diagnostic), which is what makes them transferable; the *amounts* still need
the dedicated reference host at full scale (`JOURNAL_BENCH_LARGE_BYTES=30g`) before any
constant anywhere is changed on their word.

**Where the PRs from this round said things**: #6472 and #6473 list their measured pairs in
their descriptions; the J1 rows they measure come from the bench rows that landed with the
large-sequential-write line, so a future round running after merge gets its before numbers
from `journal-bench-results.md`, not from these developer-box figures.
