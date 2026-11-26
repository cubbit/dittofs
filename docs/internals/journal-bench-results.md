# Journal benchmark results (RFC 1 §9.6, J1 to J3)

**Read this first.** `benchstat` compares only two runs of one box, never across boxes. Only the
dedicated host's figures feed the keep-or-change decisions below; a developer machine's run
compares only against that same machine's earlier figures, and a Mac's absolute values are
`F_FULLFSYNC` figures that feed no decision (RFC 29 §4.10, and the RFC index's "Recording
results" rule). No figure here was measured yet: every table is empty until its box's first run,
and none is invented.

## How a run is made

```
DITTOFS_BENCH_DIR=<a directory on the device under test> \
  JOURNAL_BENCH_COUNT=10 test/harness/bin/dt run journal-bench
```

Run it by hand, outside the dedicated host's nightly work. The suite's `bench.txt` carries the
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
The first run on the dedicated host, and the open rows, are tracked by issue #5182.

## What the suite does not yet measure (open)

RFC 1 §9.6's method asks for more than these rows give. Each item below is OPEN, owned by issue
#5182 and the deferral list of the epic 12 plan, and comes back when a reference-box run exists.
That run is the precondition for any keep-or-change decision below:

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

The keep-or-change decisions for the segment cap, the sync bound and the idle seal wait for rows
that exercise them. Only the large-file row makes the journal roll under a sustained stream, so
only its figures can bear on the segment cap; the sync bound and the idle seal wait for their own
rows, and the tables below stay empty until the reference run exists.

Today's values, from `pkg/journal/internal/core/limits.go` and `config.go`:

| Constant | Today's value | Where |
| --- | --- | --- |
| Segment cap | 256 MiB (`SegmentCap`) | `limits.go`; the fixture overrides it per test |
| Sync bound | 1 s (`SyncBound`) | `limits.go` |
| Idle seal | 30 s age (`IdleSealAge`) once a segment holds 16 MiB (`IdleSealBytes` default); the fixture's convention is `IdleSealBytes = SegmentCap/16` | `limits.go`, `config.go`, `open.go` |

## The dedicated host

Reference box. Not yet run: the first run lands in the first window outside its nightly work.

| Field | Value |
| --- | --- |
| Commit | |
| CPU, memory | |
| Device and its raw figures | |
| Operating system, kernel | |
| Filesystem and mount options | |
| Sync bound | |

| Row | Queue depth | Raw twin | Result | RFC 1's proposed target | Measured target |
| --- | --- | --- | --- | --- | --- |
| J1 sequential, 4 KiB to 1 MiB, each sync pattern | 16 | `RawSequential`, same name | | ≥ 95% of the filesystem at ≥ 256 KiB | |
| J1 random, 4 KiB to 1 MiB, each sync pattern | 16 | `RawRandom`, same name | | ≥ 90% of its IOPS at 4 KiB with a sync per write | |
| J1 `WriteAt` p99 | 16 | | | ≤ 20 µs | |
| J1 mixed durability | 2 (one stream) | | | none | |
| J1 overwrite, sequential and random, 4 KiB to 1 MiB, each sync pattern (create pass synced untimed) | 16 | `RawOverwriteSequential` (streamed) / `RawOverwriteRandom`; the sequential sync rows twin `RawSequential`'s | | none named: reported apart | |
| J1 large sequential, one writer, one file 30 GiB (`JOURNAL_BENCH_LARGE_BYTES`), one sync at its end | 1 | `RawLargeSequential` (same span, preallocated; see the Notes' caveat on its cache state) | | none: reported by the metrics the Notes' large-file bullet names, no fraction judged | |
| J2 write scaling, 1, 4, 16, 64 files, each synced at its end | 1 to 64 | `RawScaling` (same files, 1 MiB blocks, sync at end) | | within 5% of the filesystem, flat to 64 | |
| J3 `ReadAt` of one 64-byte extent, a point per decade 10^3 to 10^6 at the full sizes (upper bound, includes pread and copy) | 1 | | | ≤ 1 µs at 10^6 (index lookup; not judged from this row) | none: not filled in from this row |
| J3 warm 1 MiB read | 1 | | | ≥ 2 GB/s | |

Keep-or-change decision for each constant, with the evidence:

| Constant | Today | Decision | Evidence |
| --- | --- | --- | --- |
| Segment cap | 256 MiB | | |
| Sync bound | 1 s | | |
| Idle seal | 30 s, 16 MiB | | |

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
