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
round's raw artifacts (pprof profiles, bench outputs) are not in the repository; the
*conclusions* they supported are all repeated here, so this file stands alone.

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

*Later rounds:* iteration 3 built the empty-`Release` `c.mu` skip listed under "Not built", for
its contention value rather than a measured gain, and removed four of the six production
allocations per write counted below.

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
- **Two unreproduced reds**: `TestSealRefusesAnAppendResumedFromTheRollGate` and
  `TestSoakRemovalChurnDrains` each went red once in an unprofiled parallel full pass and never
  again; neither sits on a path this round changed.

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
   +15% over P1a alone. The stacked large row still sits at ≈ 55% of the raw twin (732 vs
   ~1300–1500 MB/s), unprofiled; the inline seal drains, unmasked by P1a, are the likeliest share.

### Not built, with the evidence that would reopen it

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

## Iteration 2 — 2026-11-27: the roll pays two device flushes per segment

*Later rounds:* iteration 3 corrected dead end 9's mechanism (the deferral never moved the
make). Its test of recommendation 1 did not run as intended (see iteration 3), so recommendation
1 is unsettled, neither confirmed nor withdrawn.

**Context.** Measured at the merged stack, `33e01b49d` (the read-ahead hint #6472, the record
buffer pool #6473 and the opening spare #6511 all in), on the same MacBook (APFS,
`F_FULLFSYNC`), in the morning, load average 2.2–3.4. Iteration 1 had left the large row at
about 55% of its twin, unprofiled. Its two open hypotheses were (a) the two inline
`F_FULLFSYNC`s in each roll's seal, now unmasked, and (b) whatever kernel `pwrite` cost was
left. This round measured the residual and recommends what to build next. It built nothing.

**Baseline** (`bench-data.sh j1`, small scale, 10 × 1x, profilers on):

| Row | MB/s median | p99-write µs | Note |
| --- | --- | --- | --- |
| J1_LargeSequential | 724.8 | 103.6 | spare-refusals 0 in 9 of 10 runs, 1 in one |
| RawLargeSequential | 3992 | 21.1 | the journal is at **18% of its twin** |
| J1_WriteSequential/64k-nosync | 1185 | 5042 | twin 8278 |
| J1_WriteSequential/64k-sync | 5.72 | 1994 | twin 51.4 |

Isolated large-row runs in the same hour gave 678–700 MB/s (twin 3830–4070), and 595–930 later
in the morning as the machine moved. The twin is now 3–4 GB/s on this Mac, against 1.3–1.5 GB/s
yesterday. Iteration 1's "55% of the twin" is therefore yesterday's twin. Measured against
today's, the gap is larger.

### Discovered

- **Hypothesis (a) holds: two thirds of the large row's wall time is the roll, and the roll is
  two device flushes.** Scratch timers around each roll's steps, never committed, ran over 2 × 10
  iterations, with a median wall of ~767 ms per 512 MiB:

  | Where the time goes (per iteration) | ms | Share |
  | --- | --- | --- |
  | 3 rolls (2 full seals plus the first roll) | ~507 | **66%** |
  | — seal step 1, `F_FULLFSYNC` of ~256 MiB of records, ×2 | ~250 | 33% |
  | — seal step 2, `F_FULLFSYNC` of the marker, footer and trailer, ×2 | ~250 | 33% |
  | — take, drain, marker writes, successor header | ~1 | — |
  | `pwrite` syscalls (8,194 of them) | ~95 | 12% |
  | the rest of each write call (encode, CRC, locks, capacity, catalog, chasing the first hint) | ~145 | 19% |
  | the closing `Handle.Sync` | ~20 | 3% |

- **Seal step 2 costs about 4 ms on its own. In the journal it waits on the Maker.** A
  stand-alone replay of the segment and its seal (preallocated, hinted, 4,085 records of
  65,640 B, step 1, marker, footer, trailer, step 2) gives step 1 115–125 ms and step 2 3.8 ms.
  That holds whether the file is zero-filled or preallocated, and whether the writes are aligned
  or not. In the journal, step 2 takes 100–125 ms. An event timeline of the roll, the Maker's
  `make` and the hint worker shows why. The roll's take kicks the refill, the Maker's
  `F_FULLFSYNC` of the fresh spare starts within 0.1 ms of seal step 1, and **step 2 returns
  within 3–6 ms of that make's sync returning, in every roll logged**. On this volume
  `F_FULLFSYNC`s queue behind one another, so the seal pays the spare's flush.
- **A make's flush is as expensive as flushing the segment's data.** The Maker's sync of a
  freshly preallocated, never-written 256 MiB file costs **150–205 ms idle**. Measured by
  variant: `F_PREALLOCATE` 256 MiB then `F_FULLFSYNC` 154 ms; 32 MiB 24 ms; plain `fsync` 30 ms;
  a sparse file (size only) 4 ms. The cost scales with the preallocated size, at about 1.7 GB/s.
  So on macOS each 256 MiB segment costs the device two flushes of about its own size: one when
  it is made and one when it is sealed. The stream rolls every ~0.35 s, and the Maker is busy
  for most of that.
- **Moving the make off the seal removes the wait, but the flush lands on another sync.** With
  the stock deepened to two content spares (`SpareTarget: 2`, a setting), step 2 drops to 7–9 ms
  in every run and the roll from 139–148 to 65–117 ms. The large row's valid pairs (twin within
  ±25%) read **1.20×, 1.48× and 1.01×**, and a fourth pair was void (twin −33%). In the 1.01×
  pair, step 1 grew to 167 ms: the make's flush landed on step 1 instead of step 2. Whether a
  make overlaps a seal, the end sync or plain writing is a matter of timing. A deeper stock lets
  it miss the seal, but nothing makes it miss reliably. The median gain is about 1.2×, under the
  1.25× gate, and spare-refusals stay 0 at both stocks.
- **The profiles cannot see any of this, as predicted** (mislead 2). The isolated row's block
  profile shows the writer blocked only in the closing `Sync` (19 ms/iteration). The ~500 ms of
  roll per iteration is syscall time and appears in neither CPU nor block profile. The mutex
  profile puts 86 ms/iteration at the writer's file-lock `Unlock`. That is the durability worker
  waiting on a lock the writer holds across the seal's syncs (mislead 1), and it costs the
  worker, not the writer. The writer's sampled CPU is almost all `pwrite`. Journal Go code (CRC,
  capacity, locks, catalog) is ≤2% of the row's samples.
- **The lock micro-costs did not move into range.** At ≤2% of the writer's samples, the empty-
  `Release` skip and the settle fold cannot reach 1.25× on the large row. They stay parked on
  their old trigger, a >=128-writer workload on the dedicated Linux benchmark host.
- **The 64k-sync row cannot corroborate the seal at small scale.** Its synced span is 1 MiB over
  16 writers, so each writer makes one write and one sync, inside a single segment with no seal.
  The row measures the first sync after open. It is the worst ratio to its twin in J1 (0.11×),
  but nothing in this round attributes that gap. It needs the full scale's 16 MiB span before it
  can say anything about steady-state syncs.

### Recommended (not built in this round)

1. **Ask the RFC 28 Appendix A owner about the macOS make's sync** (an RFC question, not a
   prototype). The make's `F_FULLFSYNC` over a preallocated extent is the biggest single cost
   this round found: about 150 ms of device flush per 256 MiB, about as much as the segment's
   own data costs to flush. This is not dead end 5 (syncless make). The question is whether the
   make's preallocation needs a *full* flush before the rename, or whether `fsync` (30 ms)
   followed by the directory sync meets RFC 28 §2.2's order. Evidence: the variant table above.
   Expected gain if it is allowed: about half of the roll's flush bill, so roughly 1.3–1.5× on
   the large row on macOS. That is an estimate from the timeline and has not been measured.
2. **ContentBand / a two-spare stock for a fast stream: measure it on the dedicated Linux benchmark
   host, don't build it here.** On this Mac it is a 1.0–1.5× coin flip. On Linux the Maker zero-fills the
   256 MiB with real writes, so the device bill per segment is a write of the segment plus a
   flush, which may compete harder. The trigger iteration 1 named for it ("the pool becomes the
   next wall") has fired in a different form. The pool keeps up (refusals 0), but its makes'
   flushes land on the writer's seal. Footprint +1 cap per pool, and the RFC 1 §6 ruling is
   still owed.
3. **The drafted pipelined-sealing question gains its evidence and a ceiling.** Step 1 (the
   seal's own data flush) is 33% of the wall, larger than iteration 1's bounded 20%. Pipelining
   it would only move that flush, and this volume serializes flushes, so the payoff is capped by
   the same device bill as item 1. It stays forbidden as written (dead end 4). The question
   should now carry this round's table.

### Dead ends added

9. **Deferring the refill kick from the take to after the seal**, as a stand-alone change: the
   content take is not the only kick (the capacity hold, retirement and the pool's own settle
   kick too). In the diagnostic the make still started with step 1, and with two spares in stock
   the deferral made the row worse (652 MB/s). Retry it only as part of a ContentBand design that
   owns every kick.
10. **Fixing step 2 itself**: on its own it is a 4 ms sync. Its cost is the queue it joins, not
    its bytes.

**Artifacts**: the round's profiles, event logs, stand-alone programs and probe tree are not in
the repository; the conclusions above are all they supported.

**Not checked**: the dedicated Linux benchmark host. Its pre- and post-merge runs were not read
in this round (iteration 2b reads them). The iteration-1 validation and every Linux statement
above waited on it. Untested: several streams, full scale, and J2/J3 at the
merged tip.

## Iteration 2b — 2026-11-27: on Linux at full scale the large row runs at the Maker's rate

**Context.** This round read the dedicated Linux benchmark host (a Linux ext4 volume)
instead of the MacBook. It used three runs: the small pre-merge run with
pprof at `8922beb5` (`j1`/`j2`/`j3`), the full pre-merge run at `e5e487cb`, and the full
post-merge run at `7e3505d0e`, which has the read-ahead hint, the record buffer pool and the
opening spare. Both full runs use a 30 GiB large row, 1 MiB blocks and 10 × 1x. The round
looked only at structural changes in the journal's own Go code. The macOS make-sync question
from iteration 2 stays parked. Nothing was built.

### Full scale, before and after the merged stack (benchstat, n=10 each)

| Row | pre `e5e487cb` | post `7e3505d0e` | |
| --- | --- | --- | --- |
| J1_LargeSequential MiB/s | 204 ± 11% | 188 ± 8% | ~ (p=0.19): **no change** |
| — spare-refusals/iter | 104.5 | 107 | ~ (p=0.93) |
| — p99-write-waited µs | 840 | 367 | −56% |
| RawLargeSequential MiB/s (twin) | 825 | 853 | ~: the journal is at **22% of its twin** |
| J2_WriteScaling 1 / 4 / 16 / 64 MiB/s | 143 / 224 / 174 / 146 | 430 / 340 / 262 / 280 | **+201 / +52 / +51 / +92%** |
| J1_WriteSequential 64k / 256k / 1m-nosync | 4.06 / 5.70 / 4.85 GiB/s | 6.25 / 7.39 / 5.83 | +54 / +30 / +20% |
| J1_WriteSequential/64k-sync MiB/s | 31.1 | 18.8 | −40% (p=0.002), twin flat: **flag, below** |

On Linux the merged stack's wins carry over to every row that does not roll: J2 and the
unsynced rows. On the one row that rolls through many segments, they carry over to the p99 but
not to throughput. The −8% an earlier reading took from the large row is noise (p=0.19).

### Discovered

- **The large row is spare-bound, and the refusal waits are its wall time, not sub-millisecond
  pauses.** At full scale a block is 1 MiB, so one iteration is 30,720 write calls. The p99
  with waits, 367 µs, means at least 99% of calls take ≤ 367 µs each, ≤ 11 s in total. An
  iteration takes ~165 s. So **≥ 150 s (≥ 91%) sits in at most 1% of calls**: the ~107 refused
  rolls and the closing sync. That is ~1.4 s per refusal, about one make. The p99 cannot see
  107 calls out of 30,720 (0.35%), so "p99-waited ≈ p99" does not mean the waits are free.
  The arithmetic also matches as a rate: 120 segments in ~165 s is 0.73 spares/s × 256 MiB =
  **~186 MiB/s, which is the row's own rate**. On this host the stream writes each segment in
  ~0.1 s and then waits for the Maker. (Bench gap: the row reports no total wait time, so this
  comes from the bound, not from a direct reading. See candidate 3.)
- **Each fresh segment costs the device two segments of writes.** In the small pre-merge
  profiles the Maker is **59% (j1), 72% (j2) and 71% (j3) of all CPU samples**. 98.7% of
  `allocate` is the zero `pwrite`s and 1.2% is `fallocate`, and the make's own `Sync` adds
  another 16–20%. The writer's `WriteAt` is 10% of samples, including its `pwrite`s;
  `AppendRecord` is 2.6%, `memmove` 1.7%, and lock delay is 1.1 s over a 545 s profile. RFC 28
  Appendix A states the ceiling in its own decision box: *"Zero-filling every new segment would
  write each segment twice, so above half the device's bandwidth spares would run out and the
  fill would land on the write path."* At full scale that has happened. The device moves
  ~512 MiB per segment (fill plus data) in ~1.37 s, about 375 MiB/s against the twin's
  ~850. So two factors stack: **2× write amplification from the fill**, and a further ~2.3×
  from making and sealing at the same time on one device. The second factor is not attributed.
  Candidates are the fill's unwritten-extent conversions committing at the make's `fsync`, the
  seal's two syncs queueing with the make's, and two interleaved sequential streams on one volume.
- **The Linux production ceiling for sustained ingest, on the host's own numbers: about one
  Maker, ~190 MiB/s journal-wide, whatever the stream count**, for as long as the journal is
  growing (no retirement, so no recycle). J2 does not show this because a J2 iteration writes
  128 MiB, which the opening stock absorbs without a roll. Its +50–200% is real but measures
  bursts. A stock of 16 × 256 MiB absorbs about 4 GiB of burst, and anything sustained past
  that runs at the Maker's rate. Iteration 1's trigger ("the pool becomes the next wall") **has
  fired on Linux at full scale**, and "spare-pool waits never happen at stock" holds only at
  small scale and on macOS.
- **Lever 2 as framed (re-use retired segments as spares) is already built, and it writes no
  zeros.** `Retirer.recycle` (`segment/retire.go`) renames a full-sealed segment to a fresh id,
  syncs the directory, zeroes only its header slot and its 32-byte trailer, and syncs (RFC 28
  §2.2, R40/R41). Old records never verify under the new name (§2.2: "A recycled segment's old
  life never verifies"; `format/scan.go`). Ids are one-shot by design (the `Maker.last` decision),
  which is why a recycle draws a fresh id instead of re-adopting the old one. **Punching holes to
  blank a recycled segment would be a regression**: it brings back the unwritten extents that
  the Linux fill exists to avoid. The real gap is that recycling never runs here. A segment
  retires only when its content is released, and nothing releases content in the J1 rows or on
  a write-only node today, because no remote tier is wired. The bench that would exercise it,
  J8 (RFC 1 §9.6; RFC 28 Appendix A names it as the gate for the fill), does not exist.
- **Direct placement (the staging copy) is worth ≤ 2%.** Records have no footer. The layout is a
  96 B header with the payload CRC at [84,88) and the header CRC over [0,92), then for a write an
  8 B mtime and the payload, then padding to 8. The header therefore needs the payload's CRC
  before it is written. That is a read pass over the caller's buffer, which costs the same either
  way. A laptop micro-benchmark wrote 65,640 B records into resident pages: the copy alone takes 0.6 µs, copy plus `pwrite` 6.5–9.0 µs,
  and `pwritev` [header+mtime, payload] 5.2–7.0 µs, so **~1.3 µs per record**. That is ~11 ms
  of the laptop's ~767 ms iteration (~1.4%). On the host the row is spare-bound, so the gain is
  ~0. Readers need the record to be contiguous on disk, not written in one span: two iovecs
  that tear leave the same torn tail that a single non-atomic `pwrite` can leave. `unix.Pwritev`
  exists for darwin and linux in x/sys v0.50. Windows has no gather write for buffered files
  and keeps the copy. The code already marks this ceiling (`segment/stream.go`, the ponytail at
  the encode site: "only a vectored write would avoid").
- **Which iteration-2 findings hold on Linux.** *Hold:* the writer's Go code is a small share
  (≤ 3%), locks are irrelevant, and the make and the stream compete for one device. *macOS
  only:* the `F_FULLFSYNC` queueing numbers, the claim that a make's flush costs about the
  segment's own flush (on Linux a make is a full 256 MiB *write* plus its flush, which is
  worse), and the read-ahead hint's gain (a no-op on Linux by design; the Linux gains come from
  the pool and the opening spare).
- **Flag, not attributed: J1_WriteSequential/64k-sync −40% (p=0.002) with its twin flat.**
  The neighbouring sync rows are either noisy (256k-sync ±89–101%) or flat. The device's own
  4 KiB flush probe read p50 3.08 ms before and 1.39 ms after, so the host differed between the
  runs. This needs a same-hour pair on the host before anyone calls it a merged-stack cost.

### Candidates for iteration 3 (ranked)

1. **Make the large row report its spare wait (bench only, ~0.5 day).** Add the total of each
   call's waited-minus-attempt time per iteration as a metric, for example `spare-wait-ms/iter`.
   The finding above is a bound from p99 and wall time, and someone has already misread the p99
   once. The metric makes the attribution direct and gives every later candidate its gate.
   No RFC dependency.
2. **Pay the Linux fill without moving the bytes: `fallocate(FALLOC_FL_WRITE_ZEROES)` where the
   kernel offers it, falling back to today's zero writes on `EOPNOTSUPP`.** RFC 28 Appendix A
   already allows it: "or one zeroing allocation where the kernel offers one that leaves the
   extents written". The constant is in x/sys v0.50, and the host's kernel has it. The change is
   confined to `platform/alloc_linux.go`. `Allocated()`'s FIEMAP walk already checks that no
   extent is left unwritten, so it is the conformance check. Cost: ~1–1.5 days with tests.
   **Expected at full scale:** the fill's device bytes go away if the host's volume and the devices
   under it advertise write-zeroes. The row's ceiling then moves from a 512 MiB/segment cycle toward a
   256 MiB/segment one, an estimated **1.8–3× (≈ 350–550 MiB/s)**. That is an estimate from the
   arithmetic above, not a measurement. **Confirm first**, read-only: the volume's and its devices'
   `queue/write_zeroes_max_bytes` in sysfs. If it is 0, the kernel falls back or refuses and this
   candidate is dead on that host. This is our code's call
   choice, but its gain depends on the device. Say so when proposing it.
3. **J8, the steady-state row (~2–3 days).** J1 against a capacity well below the data written,
   with content released continuously, so segments retire and recycle (RFC 1 §9.6 J8; RFC 28
   Appendix A's gate). This measures the production steady state, where recycling is supposed
   to make the fill disappear, and it is what settles both the fill and the spare target. The
   full-scale large row today only measures the growth phase. Expected: refusals near 0 and
   the row near the twin, *if* recycling keeps up. If it does not, the RFC already says what
   happens next (raise the target). Dependency: the bench needs a driver that releases content
   (RFC 1 §7.1).
4. **The RFC's own gate on the fill:** J1's `Sync`-per-write row on the host with and without
   the fill. This is only worth running if candidate 2 is unavailable on the device. It
   overturns the fill only if unwritten-extent conversions cost less than the zero writes.
5. **Direct placement with `pwritev` (~2–3 days):** a simplification, not a speed lever. It
   deletes the content-record pool (P2, #6473) and its multi-MiB slabs, and gains ≤ 2% of
   wall time. Build it only if the pool's memory or code becomes a problem. If it is built, the
   counted-allocation pin stays where it is and the recovery copy path (`recovery/copy.go`)
   keeps `AppendRecord`.

Parked: ContentBand and refill pacing. They move makes out of bursts, and J2 no longer rolls
inside its window. They cannot lift a bandwidth-bound sustained stream.

**Artifacts**: the three runs' outputs, the benchstat table's inputs and the `pwritev`
micro-benchmark are not in the repository. The table was computed from the runs' raw outputs.

## Iteration 3 — 2026-11-27: the large row's budget, flush-behind, and the writer path

**Context.** Three studies on the same MacBook (APFS, `F_FULLFSYNC`) at the merged tip, which
by then also carried iteration 2b's candidate 1 (the large row's `spare-wait-ms/iter`). Scratch
timers and knobs, never committed, did the attribution. The first study closed the large row's
budget, the second prototyped flush-behind against it, and the third ranked the writer's own
per-write costs at 4 KiB and 64 KiB. Only the third built anything: the writer-path pull request
that carries this section.

### The large row's budget closes, and the roll is paying the Maker

Pooled over 47 iterations of seven same-session bases (small scale, 512 MiB per iteration,
712–725 MB/s). The timers partition the writer's wall, and the per-component medians sum to
754.9 ms against a wall of 755.3 ms.

| Component (per 512 MiB iteration) | ms | Share |
| --- | --- | --- |
| Rolls (2 seals and the opening roll) | 486 | **64%** |
| — seal step 1, `F_FULLFSYNC` of the records | 231 | 31% |
| — seal step 2, marker, footer, trailer and `F_FULLFSYNC` | 250 | 33% |
| `pwrite` of the records (8,194) | 104 | 14% |
| encode: `format.AppendRecord`, the copy plus the payload CRC32C | 73 | 10% |
| above the stream: `Handle.WriteAt` less `AppendRoll` (capacity, index, file locks) | 58 | 8% |
| the rest of `Stream.AppendRoll` | 13 | 2% |
| the closing `Handle.Sync` | 22 | 3% |
| spare parks | 0 | 0 refusals in 45 of 47 |

A 2 GiB steady state (8 seals per iteration) has the same shape, with a 216 ms spare park in 3
of 8 iterations: at volume the Maker starts to fall behind even on the Mac.

- **The roll is the Maker's preallocation commit.** An event timeline of 30 rolls: the take
  kicks the make, the make's `F_FULLFSYNC` starts 0.2 ms later and runs 232–248 ms, seal step 1
  ends at ~119 ms, and step 2 returns 3.6–4.1 ms after the make's sync does, in all 30. With the
  make's bill gone, step 2's own barrier is 8–9 ms. So each seal is ~116 ms of its own data
  flush and ~117 ms of waiting on the make.
- **Whoever syncs next pays the make's bill.** Dropping every make sync gave 1.33× at small
  scale and 1.04× at steady state, with the bill landing on the seals: the cost follows the
  preallocated extent, not the call that flushes it. A sparse spare (size only, no
  `F_PREALLOCATE`) was the only make whose bill disappeared. **The probes of a cheaper sync
  primitive did not run:** the segment files sit behind the durability layer's file wrapper,
  which did not pass the probe's sync choice through, so the plain-`fsync`, `F_BARRIERFSYNC` and
  directory-sync-only arms all ran the normal `F_FULLFSYNC` (found by a later round on the Linux
  host, where the same wrapper hid a writeback hint). Their figures (1.01×, 0.89×, 0.96×) are
  measurements of the unchanged code, and recommendation 1 stays open until a run whose
  pass-through is verified.
- **Dead end 9's mechanism, corrected.** Iteration 2's deferral suppressed only
  `Pool.Take(Content)`'s kick, and the large row never calls that: its roll's content comes
  through a capacity hold, and the kicks of `Pool.Hold` and `Hold.Take` fire within 0.1 ms of the
  take. The deferral moved nothing, and its 652 MB/s was noise (re-run: 0.99×). A real deferral,
  every make held until no roll is sealing, measured 1.28× at small scale with two spares but
  **1.08× at steady state**: a make (~230–250 ms) is longer than a segment's write phase
  (~115–130 ms), so it lands on the next seal instead.
- **No sync-bound flush lands mid-roll.** None in ~120 rolls: each seal's step 1 makes every
  record durable within ~0.35 s, so no record reaches its 1 s bound.
- **Step 1 is not size-proportional.** Its median falls as the cap grows (95, 78, 54 ms at
  256 MiB, 512 MiB, 1 GiB caps): a longer write phase gives kernel writeback time to clean the
  segment before the barrier.

Levers, against bracketing same-session bases (the twin was bimodal all session, so its ±25%
gate was unusable):

| Lever | small | 2 GiB steady | Owner |
| --- | --- | --- | --- |
| Sparse spare (`ftruncate` only) | 1.87× | **2.03×** | RFC 28 Appendix A: the make's space guarantee; ENOSPC would arrive at a record write |
| SegmentCap 1 GiB / 512 MiB | 1.40× / 1.35× | **1.98× / 1.65×** | RFC 28 §5 Q2 and RFC 1 §6 (the stock's footprint doubles) |
| Make held out of the seal window, two spares | 1.28× | 1.08× | in-code, below the gate |
| Merge the seal's two flushes | 1.00× | — | RFC 28 §2.5; do not ask |
| Seal step 1 as `F_BARRIERFSYNC` | not run: the probe's choice never reached the file | — | open |
| Deeper stock alone (`SpareTarget: 2`) | 0.96–1.02× | 0.91× | a setting |
| Faster CRC32C (interleaved arm64) | not run; ≤ 4% | | in-code, macOS only (amd64 is already interleaved) |

Every lever that clears 1.25× at steady state is RFC-owned and macOS-only. On Linux a make
zero-fills the whole cap, so neither the cap nor the sparse spare is expected to carry over; the
Linux analogue is iteration 2b's candidate 2.

### Flush-behind: refuted, and the reason transfers

The prototype: once a segment's unsynced tail passes a threshold, the sink's `Track` asks the
durability worker for an ordinary active-segment sync, at most one in flight, never blocking the
appender. It does shorten step 1: from ~109 to ~52 ms when a flush landed shortly before the
roll. But step 2 still ends 3–4 ms after the make's sync in every variant, so the roll lasts as
long as the make whatever step 1 does, and the time step 1 saves is absorbed by step 2. Each
flush-behind sync is one more `F_FULLFSYNC` (device busy time +12% to +73%), and a seal that
starts while one is in flight waits it out in the drain.

| | Result | Gate |
| --- | --- | --- |
| Large row, 512 MiB | 1.00–1.03× | ≥ 1.15× |
| Large row, 2 GiB steady | 0.94–1.04× at 32 and 128 MiB; 0.81–0.89× at 64 MiB | ≥ 1.15× |
| J1 4k-nosync, full scale (the knob fires on small records) | **0.86×, 0.83×** (two pairs, opposite order) | ≥ 0.9× |
| J1 sync rows | 0.99–1.13× (a waiter always asks first) | ≥ 0.9× |

Two further costs: RFC 1 §5.2 states the bound adds "at most one [flush] per stream per second",
and flush-behind breaks that (~60 per second at 2 GB/s with 32 MiB); and a 4 KiB threshold turned
16 tests red and hung one, each assuming that only a waiter or the bound issues a sync. If the
make ever stops being the wall and the seal becomes it, the form to try is a writeback hint, not
a durability sync: `sync_file_range(SYNC_FILE_RANGE_WRITE)` on Linux, which claims nothing
durable and leaves the §5.2 budget and its tests standing. macOS has no such primitive.

### The writer path: a quadratic scan, and what shipped

The macOS CPU profile could not attribute the writer: SIGPROF lands on syscall-heavy threads,
so a 64 KiB run put all of the writer in `pwrite` and showed no CRC32C, which costs ~5 µs per
record. Attribution came from wall-clock accumulators on the write path read by a scratch
micro-benchmark: one writer appending one file in order, no syncs, no roll.

| Slot (ns per write, probe medians) | 4k before | 4k after | 64k before | 64k after |
| --- | --- | --- | --- | --- |
| **`Durability.Durable` in the publish** | **18,270** | **63** | 1,278 | 57 |
| `pwrite` of the record | 4,300 | 3,618 | 30,570 | 29,778 |
| encode (copy and CRC32C) | 644 | 550 | 8,846 | 8,561 |
| `appendMu`, `Track`, the worker's wake, complete | 855 | 817 | 3,429 | 2,806 |
| index publish | 746 | 666 | 892 | 847 |
| `Capacity.Take` and the second tag check | 326 | 258 | 349 | 350 |
| deferred: grant release, unpin, domain unlock, exit | 314 | 224 | 312 | 279 |
| file append domain `Lock` | 119 | 59 | 364 | 60 |
| `beginCall` and the first tag check | 110 | 63 | 187 | 65 |
| **Total** | **26,162** | **6,742** | 46,801 | 43,386 |

- **`Durable` scanned the stream's ledger oldest first** for the record the write had just
  appended at its tail. The ledger holds every unsynced record, so a nosync burst made the scan
  quadratic: ~18 µs of a ~26 µs 4 KiB write. Scanning newest first gives the same answer, since
  a location names one record, and takes it to ~60 ns. Other streams' ledgers are still scanned
  whole, so a several-stream journal pays O(streams × ledger) per write; the code's `ponytail:`
  names the upgrade.
- **The pull request ships that scan and four small trims**: an empty grant release skips the
  capacity mutex (the iteration 1 item); a share binds its exit once; the drain's leave is
  deferred outside the append loop, so its closure is not heap-allocated; file-lock entries and
  their release are reused from a per-shard free list of four. `maxWriteAllocs` goes 9 → 5 and
  `maxReadAllocs` 7 → 4.
- **Measured on the Mac**, same-hour pairs: J1 4k-nosync at full scale (32,768 writes)
  **210 → 759 MB/s (3.6×)**, the scan alone 738 MB/s; the micro-benchmark's 4 KiB write
  24.2 → 6.0 µs. The gain grows with the burst (1.11× at small scale's 2,048 writes) and is nil
  on the large row (705 → 706 MB/s), which seals every 4,096 writes and is device-bound. The
  trims together save ~150–200 ns and 4 allocations per write, under every row's spread: they
  are justified by allocation rate, not MB/s. The pull request's description carries its own
  benchstat pair.
- **Overwriting a small hot file is pathological.** A micro-benchmark that overwrote a 1 MiB
  file in place spent 38% of all CPU in `index.(*retainedSet).edit` under the durability
  worker's `LiftSuperseded`, a quadratic retained set. Background work, not the writer's path,
  and not pursued; worth a look for J1's overwrite rows.
- **On macOS, `pwrite` into a segment costs 3–4× a raw one** (3.0–4.4 µs for a 4 KiB record
  against 2.0 µs into an `ftruncate`d file). That is APFS preallocation and partial pages: the
  macOS track, not in-code.

Once the scan is gone, the writer's CPU above `pwrite` is ~3 µs at 4 KiB and ~13.6 µs at
64 KiB, of which the CRC is 8.6 µs and the worker's wake 1.6 µs.

### Not built, with the evidence that would reopen it

- **No wake while the bound timer is armed (B2): rejected, and it needs the RFC 1 owner.** It
  fails `TestRearmKeepsTheBoundFromTheRecordsOwnStamp` and `TestOneTimerIsOutstandingPerStream`
  (`durability/spec_test.go`): those pins say each record's bound runs from its own stamp, and
  B2 would stamp a record at the next fire, so a record in a later segment than the fire's sync
  could wait up to two bounds. The exact version stamps the record with its arrival time, which
  the stream already reads before `appendMu` and could carry in `Written`. That changes the
  durability layer's stamp semantics, so it is RFC 1's call. Worth asking only if the wake
  (~1.6 µs, ~5% of a trimmed 64 KiB write) matters.
- **The grant's allocation and the Claimed-and-Release fold** were not prototyped: ~7 ns of mutex
  and one ~20 ns allocation, and both touch the "Claimed before Release" order in `write.go`.

### Dead ends added

11. **Waking the durability worker only for the first unstamped record (B).** Correct, and saves
    nothing: whenever the worker keeps up every record is the first unstamped one. Track's slot
    went 707 → 1,012 ns at 4 KiB.
12. **Flush-behind as a durability sync**, above: the roll waits on the make, not on step 1.
    Retry only as a writeback hint, and only once the make stops being the wall.
13. **Merging the seal's two flushes**: 1.00× at stock; the second flush's own cost is ~8 ms per
    roll. Do not send the RFC question. (Step 1 as `F_BARRIERFSYNC` was meant to be tested beside
    it, but that probe never reached the file, so it is not a dead end.)

**Not checked**: the dedicated Linux benchmark host. There `pwrite` is cheaper and the CRC is
already interleaved, so the scan's share of a write should be larger still, but no run backs it.
Several streams (J2), where the scan fix helps only the record's own stream. A CPU profile of
flush-behind's 4k-nosync regression, and three of its 4 KiB reds, which were not root-caused.

**Artifacts**: the probes, event logs and rejected diffs are not in the repository; the
conclusions above are all they supported.
