---
name: bench-host
description: Access DittoFS's dedicated Linux benchmark host (ditto) and run, collect, and probe journal benchmarks on it. Use when fetching reference-box bench artifacts, placing hand-run journal-bench small/full runs, scheduling around the host's cron, reading /proc device stats or running fio probes against the raid.
---

# The DittoFS benchmark host (ditto)

The quiet Linux reference box for the journal performance work: two NVMe members in
md-raid1 (`md2`, ext4 on `/`), where the scheduled full-scale J1–J3 runs live. All box time
is CET. Lucian (llucian-iws on GitHub) owns the host, its schedule and the scenario infra,
and hands a shell account to whoever on the team runs measurements. Read-only access to
the shared artifacts and manual bench runs in the host's quiet windows are the standing
arrangement — anything beyond that goes through him. For an agent: assume exactly that
access, ask for nothing less freely given, and nothing more.

## Access

The address, port and account are **not in this file** — they come from the environment,
and the owner hands them out with the account. Set both before any command in this skill
(the skill's commands fail loudly until they are):

```
export DITTOFS_BENCH_HOST=<user@host>   # the account the owner issued
export DITTOFS_BENCH_PORT=<ssh port>
```

```
ssh -o BatchMode=yes -o ConnectTimeout=15 -p "${DITTOFS_BENCH_PORT:?}" "${DITTOFS_BENCH_HOST:?}" '<command>'
rsync -az -e "ssh -p ${DITTOFS_BENCH_PORT:?}" "${DITTOFS_BENCH_HOST:?}":<remote-path> <local-path>
```

Keys are in place on the issued account; BatchMode works. Keep the values in your local
environment or an untracked file, never in the repo or its docs. The box reaches GitHub
over HTTPS (anonymous fetch of this repo works; ssh to github.com fails host-key
verification).

## Hard etiquette

- **Lucian's paths are read-only for the shared account**: `/srv/cubbit/journal-bench`
  (his checkout), `/var/tmp/dittofs-perf/journal` (his run folders),
  `/var/tmp/dittofs-journal-bench` (his bench data dir), `/run/lock/dittofs-run.lock`
  (his run lock — do not flock it). Never write, move, or prune anything there. His
  artifacts ARE world-readable: fetch and read them freely.
- **`BENCH_EXPORT_DEST` stays unset**: the scheduled runs export nothing and hand runs must
  keep it that way — results stay on the box and in your local archive, nowhere else.
- **Check what's running first**: `pgrep -a -f "fio|bench-run|conformance"` and `uptime`
  before anything that writes. A scheduled run in flight means waiting it out.
- **Clean up your own probe files** (fio/`dd` scratch under the account's home) but never
  the run folders — run folders are the record.

## The host's schedule (what to schedule around)

`cat /etc/cron.d/dittofs-dt /etc/cron.d/dittofs-journal-bench` on the box confirms; as of
2026-11-27:

| When (CET) | What | Duration |
| --- | --- | --- |
| daily 12:50 | prune of old run folders | seconds |
| Mon–Fri 13:00 | scheduled small run | ~20–25 min |
| Tue+Fri 08:30 | scheduled full run (30 GiB rows) | ~1 h 45 m |
| daily 18:45 | dt scenario night (18:00 canary first) | hours |

Quiet windows run roughly 12:10–12:50, 13:30–17:30, and after ~21:00. Nothing that writes
should overlap the 18:45 night; a full must start early enough to end before the 18:00
canary.

## Hand runs (the issued account's own runner, no Lucian involvement)

Under the account's home on the box (recreate per account if absent):

- `~/journal-bench` — a checkout of feat/storage-v2 with
  `origin = https://github.com/cubbit/dittofs.git`.
- `~/journal-runs` — `BENCH_RUN_ROOT`, mirroring his layout: `small|full/<UTC>-<sha>/…`
  with `commit.txt`, per-group `j1/j2/j3.txt` + `.cpu/.mutex/.block/.heap` profiles, a
  `done` marker when finished, and the full run's `bench.txt`/`dt.log` (benchstat against
  the previous run of the same kind in the same root).
- `~/journal-bench-data` — `DITTOFS_BENCH_DIR` (on md2, like his).
- Launcher shape, per `test/journal/bench-run.sh`'s own hand-run mode:

```sh
export BENCH_CHECKOUT=$HOME/journal-bench
export BENCH_RUN_ROOT=$HOME/journal-runs
export DITTOFS_BENCH_DIR=$HOME/journal-bench-data
export BENCH_NOT_AFTER=$(date -d "today 17:00" +%s)   # refuses to start after this
nohup sh $HOME/journal-bench/test/journal/bench-run.sh full > $HOME/full-run.log 2>&1 &
```

`BENCH_LOCK` unset means no lock — the hand run is its own collision manager; the script's
quiet-load gate (waits for load < 4) still helps. The script fetches the branch tip and
detaches at it (a run always measures the merged tip, never a stale tree), builds with the
repo's pinned toolchain (auto-downloads on first use), and prunes only within its own
root. Small ≈ 20–25 min; full ≈ 1 h 45 m.

## Collecting and reading results

Raw artifacts out, parse locally — remote one-liners over ssh invite quoting failures
(timed in this repo, twice):

```sh
rsync -az -e "ssh -p ${DITTOFS_BENCH_PORT:?}" "${DITTOFS_BENCH_HOST:?}":journal-runs/<kind>/ <local archive>/
```

Parse benches with benchstat (strip any `== ` wrapper lines; benchstat config-keys every
differing header line, so feed it only `goos/goarch/pkg/cpu` + `Benchmark` lines), or a
local per-row script. The measurement and comparison rules — same-hour pairs, the raw
twin's ±25% validity gate, the pprof misleads — live in
`docs/internals/journal-performance-optimization.md`; read them before quoting any number.

## Ad-hoc device probes (fio, /proc sampling)

For attributing device factors (flush tax, writeback throttling, jbd2 coupling). fio is
installed (3.41); job files need **one option per line** (comma-lists on one line abort
every phase); write scratch under the account's home; get per-job bandwidth via
`--output-format=json` and parse the json **after fetching it locally** —
`d["jobs"][i]["jobname"]` names the job, its bandwidth is `d["jobs"][i]["write"]["bw"]`
in KiB/s. Sample once per second during a probe: `/proc/diskstats` (watch `md2` +
`nvme0n1p4`/`nvme1n1p4`), `/proc/vmstat` (`nr_dirty`/`nr_writeback`), and
`/proc/fs/jbd2/*/info` (the md2 ext4 journal). Delete probe files when done. Known box
facts: `md2` reports `write_zeroes_max_bytes=0` (`FALLOC_FL_WRITE_ZEROES` gains nothing
there); twin-class sequential writes reach ~850–890 MB/s; both members are NVMe.
