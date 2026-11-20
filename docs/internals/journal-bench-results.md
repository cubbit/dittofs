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
own 4 KiB write-and-fsync p50 and p99, then the directory, sync bound, segment cap and writers)
and the raw twins `BenchmarkRawSequential`, `BenchmarkRawRandom` and `BenchmarkRawScaling`, which
write the same bytes through `os` with the same block size, writers and sync pattern as the row
they twin (16 writers for J1's twins, n files for `RawScaling`'s J2). A raw file is given its
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

Free space: a 16-stream journal makes about 4.5 GiB of spares when it opens (16 content, one
headroom and one reserve, each a 256 MiB segment), and J2 and J3 open one per count, so each
needs that much free on the device besides the bytes it writes.

Notes on what a row measures:

- `p99-write-µs` is the 99th percentile of one `WriteAt` call, sync excluded. The call includes
  the copy of the payload, which the journal cannot report apart, so the figure is an upper
  bound for RFC 1's "excluding sync and copy". It drops attempts refused for want of a spare,
  so it can understate when a roll happens inside the burst.
- `BenchmarkJ3_PointLookup` times a block of 100,000 `ReadAt` calls of one 64-byte extent each
  and reports `ns/lookup`. It is an upper bound: it includes the pread syscall and the copy
  into the caller's buffer, and the public API cannot time the index alone. RFC 1's
  "point lookup <= 1 µs at 10^6" is therefore not judged from this row, and nothing is filled in
  as a measured target from it.
- Sizes are the suite's fixed bursts: 128 MiB over all writers without a sync after each write,
  16 MiB with one; J2 spreads 128 MiB over its files; the mixed case streams one 128 MiB file.
  Each is under one segment, so a one-iteration run never rolls.
- A journal is opened with its content spares, headroom and repack reserve at full size before
  the timer starts.

Do not quote a figure of this suite as the device's speed: its bursts are short and run on a
fresh journal, so until the open rows below exist a run reports a warm-cache rate.

To add a result: copy the box's field table (commit, CPU and memory, device and its raw figures,
operating system and kernel, filesystem and mount options, sync bound) from the `bench.txt` box
lines into the box's heading below, then each row's figures beside its raw twin, as RFC 1 §9.6
asks ("each result also records the device's raw figures, the filesystem and mount options").
The first run on the dedicated host, and the open rows, are tracked by issue #5182.

## What the suite does not yet measure (open)

RFC 1 §9.6's method asks for more than these rows give. Each item below is OPEN, owned by issue
#5182 and the deferral list of the epic 12 plan, and comes back when a reference-box run exists.
That run is the precondition for any keep-or-change decision below:

- Write-cache exhaustion: reporting the rate after the device's write cache is full. Bursts are
  short, so a run reports the burst's rate with a warm cache.
- More data than the journal holds, so rolls and refusals are timed into the latency.
- Submission and completion latency reported apart.
- Create and overwrite reported apart. The random rows overwrite within a burst and the
  sequential rows create.
- The 256 MiB mixed stream. The mixed case streams 128 MiB, so the one stream never rolls beside
  the small writer.

The keep-or-change decisions for the segment cap, the sync bound and the idle seal wait for rows
that exercise them. The rows here write less than a segment and never wait out the idle-seal age,
so no figure of this suite bears on any of the three, and the tables below stay empty on those
rows.

Today's values, from `pkg/block/journal/internal/core/limits.go` and `config.go`:

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
| J2 write scaling, 1, 4, 16, 64 files, each synced at its end | 1 to 64 | `RawScaling` (same files, 1 MiB blocks, sync at end) | | within 5% of the filesystem, flat to 64 | |
| J3 `ReadAt` of one 64-byte extent, 10^3 and 10^6 extents (upper bound, includes pread and copy) | 1 | | | ≤ 1 µs at 10^6 (index lookup; not judged from this row) | none: not filled in from this row |
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
