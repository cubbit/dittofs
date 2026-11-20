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
box lines the run prints (commit, directory, filesystem, sync bound, segment cap, writers) and
the raw twins `BenchmarkRawSequential`, `BenchmarkRawRandom4k` and `BenchmarkRawFsync4k`, which
write the same bytes through `os` and `Sync` with the same block size, 16 writers and sync
pattern as J1; read each J row as a fraction of its twin. Run it twice, then
`benchstat first/bench.txt second/bench.txt`.

The suite refuses a directory on a RAM-backed filesystem (`tmpfs`, `ramfs`) and exits non-zero
before measuring. Record by hand what the run cannot read: the device, the mount options
(`findmnt -no OPTIONS <dir>` on Linux) and the device's own sync figure. A RAM disk formatted
with a disk filesystem passes the check, so name the real device.

Notes on what a row measures:

- `p99-write-µs` is the 99th percentile of one `WriteAt` call, sync excluded. The call includes
  the copy of the payload, which the journal cannot report apart, so the figure is an upper
  bound for RFC 1's "excluding sync and copy".
- `BenchmarkJ3_PointLookup` times a block of 100,000 `ReadAt` calls of one 64-byte extent each
  and reports `ns/lookup`. A lookup carries the read of its bytes, so it is an upper bound for
  the index's own lookup.
- Sizes are the suite's fixed bursts: 256 MiB over all writers without a sync after each write,
  16 MiB with one; J2 spreads 256 MiB over its files; the mixed case streams one 256 MiB file.

## Constants these rows exercise

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
| J1 sequential, 4 KiB to 1 MiB | 16 | `RawSequential` | | ≥ 95% of the filesystem at ≥ 256 KiB | |
| J1 random 4 KiB, no sync | 16 | `RawRandom4k` | | none | |
| J1 random 4 KiB, sync per write | 16 | `RawFsync4k` | | ≥ 90% of its IOPS | |
| J1 `WriteAt` p99 | 16 | | | ≤ 20 µs | |
| J1 mixed durability | 2 (one stream) | | | none | |
| J2 write scaling, 1, 4, 16, 64 files | 1 to 64 | `RawSequential` | | within 5% of the filesystem, flat to 64 | |
| J3 point lookup, 10^3 and 10^6 extents | 1 | | | ≤ 1 µs at 10^6 | |
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
