# Journal benchmark results (RFC 1 §9.6, J1 to J3)

**Read this first.** `benchstat` compares only two runs of one box, never across boxes. Only the
dedicated host's figures feed the keep-or-change decisions below; a developer machine's run
compares only against that same machine's earlier figures, and a Mac's absolute values are
`F_FULLFSYNC` figures that feed no decision (RFC 29 §4.10, and the RFC index's "Recording
results" rule). The dedicated host's table holds its first recorded run, a baseline before
`#6472` and `#6511`; its scheduled runs from `7e3505d0e` on are the reference the trend compares
against. Every figure is the median of the run's ten counts; none is invented.

## How a run is made

```
DITTOFS_BENCH_DIR=<a directory on the device under test> \
  JOURNAL_BENCH_COUNT=10 test/harness/bin/dt run journal-bench
```

Run it by hand, or read the scheduled runs' folders: the host runs small on working days at
13:00 and full Tuesdays and Fridays at 08:30, so a hand run waits for the lock beside them.
The suite's `bench.txt` carries the
box lines the run prints (commit, memory, device, filesystem, mount options, kernel, the device's
own figure, the p50 and p99 of 64 overwrites of 4 KiB in a prefilled, synced span, each flushed as
the twins are, so a floor beside them and with 64 samples a p99 that is the maximum, then the
directory, sync bound, segment cap and writers)
and the raw twins `BenchmarkRawSequential`, `BenchmarkRawRandom`, `BenchmarkRawScaling`,
`BenchmarkRawLargeSequential`, `BenchmarkRawOverwriteSequential` and `BenchmarkRawOverwriteRandom`,
which
write the same bytes through `os` with the same block size, writers and sync pattern as the row
they twin (16 writers for J1's twins, n files for `RawScaling`'s J2, one writer and one sync at the
file's end for `RawLargeSequential`'s row; an overwrite twin takes
the row's halved streamed span and, for the random one, its with-replacement draw). A raw file is given its
full span, zero-filled and synced, before the timer starts, as the journal's segments are
preallocated, and is flushed with `fdatasync` on Linux and `F_FULLFSYNC` on macOS, as the
journal flushes. Read each J row as a fraction of its twin of the same name. Run it twice, then
`benchstat first/bench.txt second/bench.txt`. Two runs from two commits differ in the `commit:`
line, which benchstat may treat as a configuration split; `-ignore commit` (or `-col commit`) may
pair them. That is unverified: benchstat was not available where this was written.

The suite refuses a directory on a RAM-backed filesystem (`tmpfs`, `ramfs`) and exits non-zero
before measuring: each row fails with exit 1, where an unset `DITTOFS_BENCH_DIR` stops the suite
with exit 2. The refusal fails closed, a directory whose filesystem cannot be read is refused too,
and it was checked against a real tmpfs on Linux. A RAM disk formatted with a disk filesystem
passes the check, so name the real device. The mount options are empty on macOS.

Free space: a 16-stream journal makes about 8.25 GiB of spares when it opens (16 content, 16
headroom, one per stream, and one reserve, each a 256 MiB segment), and J2 and J3 open one per
count, so each needs that much free on the device besides the bytes it writes. The large-file
row needs its span besides its own journal's three spares (one content spare, one headroom
segment, one reserve, about 768 MiB at one stream), and its twin prepares the span again
zero-filled. The bench journal carries no capacity maximum: it names only the streams, the
settings' `MaxBytes` default — 80% of the filesystem — is not derived for a journal opened
without one, and zero passes through unbounded, so the real ceiling on every row is free disk,
not a journal limit.

The large-file row streams 30 GiB at the full sizes, 512 MiB at the small ones. `JOURNAL_BENCH_LARGE_BYTES`
sizes it for a bigger customer-shaped file instead — `100g` for a 100 GiB one; the value is a
multiple of 1 MiB and a typo fails the run rather than running a size nobody asked for. Each
count writes the span through the journal and then prepares and writes it again zero-filled for
the twin, three times the span per count: about 900 GiB of I/O over the default ten counts for
the 30 GiB row, about 3 TiB for a `100g` override — past the suite's 290-minute timeout on a
slow device, so run overrides with `JOURNAL_BENCH_COUNT=1`.

Notes on what a row measures:

- `p99-write-µs` is the 99th percentile of one `WriteAt` call, sync excluded. The call includes
  the copy of the payload, which the journal cannot report apart, so the figure is an upper
  bound for RFC 1's "excluding sync and copy". It drops attempts refused for want of a spare,
  so it can understate when a roll happens inside the burst. The large-file row reports the
  complement beside it: `p99-write-waited-µs`, the whole call from first attempt to success, so
  every spare refusal's wait is timed in, and `spare-refusals/iter` counts the refusals. Its MB/s
  is the average over the whole stream, the cache-absorbed start included; the rate past the
  device's write cache is a separate figure no row reports yet.
- `BenchmarkJ1_LargeSequential` is the customer-shaped write: one writer, one file tens of
  gigabytes long (30 GiB at the full sizes), 1 MiB blocks, one sync at the file's end (its report
  is this list's rows above: MB/s, `p99-write-µs`, `p99-write-waited-µs`, `spare-refusals/iter`).
  It is the only row over more than a couple of segment caps, so it is where rolls and spare
  refusals show. Its twin is flattering beside it: the twin's prepare spends the device's write
  cache zero-filling the full file right before its timed stream, while the journal row starts
  after its spares' own writes, and the twin overwrites a file it owns where the journal appends
  new segments — read the fraction with that.
- `BenchmarkJ1_OverwriteSequential` and `BenchmarkJ1_OverwriteRandom` are the rewrite rows of
  RFC 1 §9.6's "create and overwrite reported apart": a create pass writes and syncs each
  writer's span untimed, then the timed pass rewrites the same blocks, so the first write's
  cost — index insertion, extent creation — belongs to the sequential and random create rows
  and the rewrite's (index updated, prior version superseded) to these. Their streamed span is
  the create rows' halved, so create pass plus rewrite plus record headers stay inside one
  segment and the row times an overwrite, not a roll; the synced span halves nothing, since it
  sits far below one segment already. They hold their steady state at the
  harnesses' one iteration per count — in the nosync pattern a longer run's second iteration
  rewrites the first's extents while they are still unsynced, which one iteration never does
  (nothing in a run ever drains extents; only offload, release and repack do, and none runs
  here). Their raw twins are their own rows, `RawOverwriteSequential` and `RawOverwriteRandom`,
  at the same halved span and the same draws, so the like-for-like rule of RFC 1 §9.6 holds;
  `RawOverwriteSequential` runs the streamed pattern alone — its synced span never halves and a
  raw file's offsets are in order, so `RawSequential`'s sync rows are the same figure and the
  sequential overwrite sync rows read against them; the create rows' twins stay the create
  rows'.
- The create rows' random draws are a per-writer permutation, each block exactly once, so the
  random create row creates where it used to draw with replacement — about 37% of its writes
  landed on a block already written, about a quarter of its blocks a second time; the random
  overwrite rows keep drawing with replacement, any
  block many times over.
- `BenchmarkJ3_PointLookup` times a block of 100,000 `ReadAt` calls of one 64-byte extent each
  and reports `ns/lookup`, at 10^3 and 10^4 extents at the small sizes, and at one point per
  decade between them (10^3, 10^4, 10^5, 10^6) at the full sizes. It is an upper bound: it
  includes the pread syscall and the copy into the caller's buffer, and the public API cannot
  time the index alone. RFC 1's "point lookup <= 1 µs at 10^6" is therefore not judged from
  this row, and nothing is filled in as a measured target from it.
- Sizes are the suite's fixed bursts: 128 MiB over all writers without a sync after each write,
  16 MiB with one; J2 spreads 128 MiB over its files; every burst stays under one segment, so a
  one-iteration run never rolls — except the overwrite rows and their raw twins, which stream
  the create rows' span halved, 64 MiB, so their create pass and rewrite together stay inside
  one segment (the Notes' overwrite bullet). The mixed case streams one 256 MiB file, as RFC 1
  §9.6's J1 sets it: the stream's payload alone fills one segment, so at the full sizes it rolls
  beside the small writer mid-run and the roll's seal sync lands inside the small writer's
  window. The large-file row rolls throughout.
- The mixed-durability row has no raw twin and no target: it is judged on its own, by its
  `p99-sync-write-µs` against the same box's earlier runs.
- A journal is opened with its content spares, headroom and repack reserve at full size before
  the timer starts.

Do not quote a figure of this suite as the device's speed: its bursts are short and run on a
fresh journal, so they report a warm-cache rate. The large-file row's stream is long enough to
pass the cache, but its MB/s still averages the whole stream, cache-absorbed start included.

To add a result: copy the box's field table (commit, CPU and memory, device and its raw figures,
operating system and kernel, filesystem and mount options, sync bound) from the `bench.txt` box
lines into the box's heading below, then each row's figures beside its raw twin, as RFC 1 §9.6
asks ("each result also records the device's raw figures, the filesystem and mount options").
The first run on the dedicated host is recorded below; the open rows remain tracked by
issue #5182.

## What the suite does not yet measure (open)

RFC 1 §9.6's method asks for more than these rows give. Each item below is OPEN, owned by issue
#5182 and the deferral list of the epic 12 plan, and turns on the recorded run's figures or its
trend:

- Write-cache exhaustion, and the rate after it. The large-file row's span is far past any write
  cache, but its MB/s averages the whole stream, cache-absorbed start included, so the rate with
  the cache exhausted is still unreported.
- More data than the journal holds. No row reaches it: the bench journal sets no capacity
  maximum, so no write is ever refused for the journal's own limit, and nothing in a row
  offloads, releases or repacks, so the data one run writes simply grows. What the large-file
  row does time in is the spare refusals of its rolls, `p99-write-waited-µs` and
  `spare-refusals/iter`; a run past a capacity needs a journal opened with one, which no row
  here is.
- Submission and completion latency reported apart.

Three method items are carried by built rows, and wait on nothing but the reference run's
figures: create and overwrite are reported apart by the overwrite rows, with the create rows
drawing each block once (the Notes above); the index sizes between 10^3 and 10^6 by J3's
point per decade at the full sizes; and the 256 MiB mixed stream of RFC 1 §9.6's J1 by the
mixed case, which streams it as the RFC sets it, rolls beside the small writer included
(the Notes' sizes bullet).

The keep-or-change decisions for the segment cap, the sync bound and the idle seal now have
their rows and a first recorded run. One baseline names each question — the cap under the
large-file row's sustained rolls, the idle-seal age nothing yet waits out — and
the scheduled twice-weekly fulls accumulate the trend that decides them.

Today's values, from `pkg/journal/internal/core/limits.go` and `config.go`:

| Constant | Today's value | Where |
| --- | --- | --- |
| Segment cap | 256 MiB (`SegmentCap`) | `limits.go`; the fixture overrides it per test |
| Sync bound | 1 s (`SyncBound`) | `limits.go` |
| Idle seal | 30 s age (`IdleSealAge`) once a segment holds 16 MiB (`IdleSealBytes` default); the fixture's convention is `IdleSealBytes = SegmentCap/16` | `limits.go`, `config.go`, `open.go` |

## The dedicated host

Reference box. The first recorded run is a by-hand full run of 2026-11-26 23:21 to 01:08 CET
(`full/20261126T222154Z-e5e487cbe9e5`, ten counts, medians below), a **baseline that predates
the two journal speed-ups** (`#6472`, `#6511`): the scheduled **full runs — Tuesdays and
Fridays at 08:30, from `7e3505d0e`, carrying both — are the reference the trend compares
against**, and the pair is the first same-box benchstat of those two changes. The small runs
(working days at 13:00) run other sizes under the same names and compare only with other small
runs. Both kinds run under the host's lock; folders under `/var/tmp/dittofs-perf/journal/`.

| Field | Value |
| --- | --- |
| Commit | `v0.36.0-1166-ge5e487cbe` (before `#6472` and `#6511`; the 2026-11-27 08:30 run at `7e3505d0e` is the reference going forward) |
| CPU, memory | AMD Ryzen 7 PRO 8700GE, 64 GB |
| Device and its raw figures | `/dev/md2` (ext4 on RAID), 4 KiB overwrite-flush p50 3.08 ms, p99 3.64 ms |
| Operating system, kernel | Linux 7.0.0-44-generic, amd64 |
| Filesystem and mount options | ext4, `rw,relatime` |
| Sync bound | 1 s |

The baseline run, `e5e487cbe` — medians of ten counts, journal against its raw twin (MB/s;
the fraction beside):

| Row | Queue depth | Raw twin | Result | RFC 1's proposed target | Measured target |
| --- | --- | --- | --- | --- | --- |
| J1 sequential, nosync, 4 KiB to 1 MiB | 16 | `RawSequential`, same name | 166 / 19010 (0.9%), 4363 / 25854 (17%), 6123 / 22358 (27%), 5209 / 22352 (23%) | ≥ 95% of the filesystem at ≥ 256 KiB | not met on one run: the trend decides whether the target or the design changes |
| J1 sequential, sync each, 4 KiB to 1 MiB | 16 | `RawSequential`, same name | 8.7 / 2.65 (3.3×), 32.7 / 54.2 (60%), 65.7 / 131.5 (50%), 75.2 / 495.1 (15%) | ≥ 95% of the filesystem at ≥ 256 KiB (the target covers both patterns) | not met on one run: 60% at 64 KiB falling to 15% at 1 MiB |
| J1 random, nosync, 4 KiB to 1 MiB | 16 | `RawRandom`, same name | 164 / 17653 (0.9%), 4630 / 23311 (20%), 5972 / 23700 (25%), 5079 / 21285 (24%) | | |
| J1 random, sync each, 4 KiB to 1 MiB | 16 | `RawRandom`, same name | 8.8 / 2.8 (3.2×), 34.5 / 49.6 (70%), 74.7 / 175.5 (43%), 409 / 696 (59%) | ≥ 90% of its IOPS at 4 KiB with a sync per write | met past the proposal: 8.8 against 2.8 MB/s at 4 KiB — the group commit serves 3.2× the raw fsync rate |
| J1 `WriteAt` p99 (median of p99-write-µs, sync rows and nosync rows apart) | 16 | | nosync: 1245 to 7553 µs; sync: 188 to 3933 µs (all J1 create rows) | ≤ 20 µs | not met on one run: the target excludes sync and copy only, so the writers' shared-stream wait counts against it; what to judge from it is the trend's first question |
| J1 mixed durability (p99 of the small writer's write and sync together) | 2 (one stream) | | 522 ms p99, beside the stream's 324 MB/s | none | none: the roll's seal sync lands in the small writer's window, as the RFC's 256 MiB stream sizes it |
| J1 overwrite sequential (halved span) | 16 | `RawOverwriteSequential` (nosync); the sync rows twin `RawSequential`'s | 276 / 16811 (1.6%), 5593 / 25460 (22%), 7592 / 23203 (33%), 5967 / 19795 (30%); sync rows: 4k 10.9 / 2.65 (4.1×), 64k 147 / 54.2 (2.7×), 256k 368 / 131.5 (2.8×), 1m 462 / 495.1 (93%) | none named: reported apart | none |
| J1 overwrite random | 16 | `RawOverwriteRandom` | nosync: 268 / 16390 (1.6%), 5492 / 31224 (18%), 7608 / 28626 (27%), 6151 / 27878 (22%); sync: 10.8 / 2.7 (4×), 141 / 56.7 (2.5×), 351 / 172.1 (2×), 582 / 686.8 (85%) | none named: reported apart | none |
| J1 large sequential, 30 GiB, one sync at its end | 1 | `RawLargeSequential` | 214 / 865 MB/s (25%; the twin flattering per the Notes), p99-write 837 µs, p99-write-waited 840 µs, 104.5 spare-refusals/iter — about one per roll of the ~120 a 30 GiB stream makes | none: the Notes' metrics | none |
| J2 write scaling, each synced at its end | 1 to 64 | `RawScaling` | 150 / 912 (16%), 235 / 970 (24%), 182 / 990 (18%), 152.7 / 811 (19%) for 1, 4, 16, 64 files | within 5% of the filesystem, flat to 64 | not met on one run: 16 to 24% through 1 to 64 files, the peak at 4 files |
| J3 `ReadAt` of one 64-byte extent, point per decade | 1 | | 2.32, 2.35, 2.46, 2.72 µs at 10^3 to 10^6 extents | ≤ 1 µs at 10^6 (index lookup) | none: not judged from this row (the figure includes the pread and the copy) |
| J3 warm 1 MiB read | 1 | | 12.0 GB/s | ≥ 2 GB/s | met, 6× over |

The first reference full — 2026-11-27 08:30, `7e3505d0e`, carrying `#6472` and `#6511`,
1h38m, same box, medians of its counts, journal against its raw twin:

| Row | Result (journal against twin) |
| --- | --- |
| J1 sequential, nosync | 171 / 18778 (0.9%), 6707 / 26337 (25%), 7935 / 23783 (33%), 6258 / 19300 (32%) |
| J1 sequential, sync each | 7.8 / 2.6 (3.0×), 19.7 / 52.2 (38%), 35.8 / 170 (21%), 376 / 656 (57%) |
| J1 random, nosync | 169 / 17300 (1.0%), 6676 / 24579 (27%), 8046 / 23318 (35%), 6306 / 20933 (30%) |
| J1 random, sync each | 8.0 / 2.8 (2.9×), 22.9 / 52.2 (44%), 33.3 / 186 (18%), 462 / 452 (102%) |
| J1 `WriteAt` p99 | nosync 773 to 7760 µs; sync 176 to 3082 µs (all J1 create rows) |
| J1 mixed durability | 518 ms p99, beside the stream's 349 MB/s |
| J1 overwrite sequential | nosync: 288 / 16525 (1.7%), 7194 / 21757 (33%), 9296 / 21681 (43%), 7659 / 21120 (36%); sync: 10.9 / 2.6 (4.2×), 141 / 52.2 (2.7×), 336 / 170 (2.0×), 547 / 656 (83%) |
| J1 overwrite random | nosync: 281 / 16349 (1.7%), 7180 / 28108 (26%), 9001 / 23697 (38%), 7368 / 24784 (30%); sync: 10.7 / 2.7 (3.9×), 141 / 53.8 (2.6×), 338 / 179 (1.9×), 499 / 692 (72%) |
| J1 large sequential, 30 GiB | 197 / 894 MB/s (22%), p99-write 365 µs, p99-write-waited 367 µs, 107 spare-refusals/iter — the refusals flat against the baseline's 104.5, the waits as short (2 µs at the p99) |
| J2 write scaling, 1, 4, 16, 64 files | 451 / 554 (81%), 357 / 650 (55%), 275 / 884 (31%), 294 / 897 (33%) |
| J3 point lookups, 10^3 to 10^6 | 2.23, 2.28, 2.47, 2.69 µs |
| J3 warm 1 MiB read | 12.9 GB/s |

Read the two tables with the device's own state in each: the raw probe moved from p50 3.08 ms
(baseline) to 1.39 ms (reference), so a pair of runs cannot name a cause. What survives the
device moving under them: the large-file row's refusals stay ~one per roll with waits short in
both runs; the group-commit rows stay above 3× their twins; the warm read stays 6× its target;
and J2's 1-file fraction went from 16 to 81% while the other J2 rows moved less — the shape
`#6511` (an opening spare held until the first stream rolls) predicts, but the trend over more
fulls still decides. The synced rows swing most between runs and are the last to read.

Keep-or-change decision for each constant, with the evidence. One baseline run cannot
decide any of the three; what it does is name the questions the trend now answers, and the
first same-box benchstat — the baseline against the reference fulls at `7e3505d0e` and later,
which carry `#6472` and `#6511` — is the trend's first entry:

| Constant | Today | Decision | Evidence |
| --- | --- | --- | --- |
| Segment cap | 256 MiB | open: the trend decides | the large-file row: 104.5 refusals over the ~120 rolls of a 30 GiB stream, each wait short (the waited p99 within 3 µs of the plain), at 25% of a flattering twin (the Notes); whether a smaller cap (cheaper spares, more rolls) or a bigger one trades better is what the twice-weekly fulls accumulate |
| Sync bound | 1 s | open: not exercised | the bound times records that wait by their own accord, when no Sync names their file (limits.go); the mixed row's 522 ms is an explicit Sync's latency, which the bound does not govern, and no row measures the timer's own cost yet |
| Idle seal | 30 s, 16 MiB | open: not exercised | no row waits out a 30 s idle segment; nothing in this run bears on it |

## Davide's MacBook

A desktop run, comparable only with this machine's earlier figures. Not yet recorded.

| Field | Value |
| --- | --- |
| Commit | |
| Chip, memory | |
| Filesystem (APFS), volume | |
| Sync bound | |

| Row | Queue depth | Raw twin | Result |
| --- | --- | --- | --- |
| J1 to J3 | | | |
