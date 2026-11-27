---
name: bench-host
description: Access DittoFS's dedicated Linux benchmark host (ditto) and run, collect, and probe benchmark workloads on it. Use when fetching reference artifacts from the box, scheduling hand runs around its cron and lock, or running device probes (fio, /proc sampling) against its storage.
---

# The DittoFS benchmark host (ditto)

The team's quiet Linux reference box: two NVMe members in md-raid1 (`md2`, ext4 on `/`),
used to measure DittoFS away from developer-machine noise — the journal benches, the
engine benches, and any future suite that needs a quiet device. All box time is CET.
Lucian (llucian-iws on GitHub) owns the host and its schedule; the shared account exists
for read access to artifacts and for hand-run measurements in the quiet windows. For an
agent: exactly that access — nothing extra gets asked, and anything beyond goes through
Lucian.

## Access

The address, port and account are **not in this file** — they come from the environment,
and the owner hands them out with the account. Set both before any command in this skill
(the commands fail loudly until they are):

```
export DITTOFS_BENCH_HOST=<user@host>   # the account the owner issued
export DITTOFS_BENCH_PORT=<ssh port>
```

```
ssh -o BatchMode=yes -o ConnectTimeout=15 -p "${DITTOFS_BENCH_PORT:?}" "${DITTOFS_BENCH_HOST:?}" '<command>'
rsync -az -e "ssh -p ${DITTOFS_BENCH_PORT:?}" "${DITTOFS_BENCH_HOST:?}":<remote-path> <local-path>
```

Keys are in place on the issued account; BatchMode works. The box reaches GitHub over
HTTPS (anonymous fetch of this repo works; ssh to github.com fails host-key verification).
Keep the access values in your local environment or an untracked file, never in the repo.

## Hard etiquette

- **The scheduled infra is read-only**: `/srv/cubbit/journal-bench` and `/srv/cubbit/dt`
  (the owner's checkouts), `/var/tmp/dittofs-perf/**` (run folders) and the other
  scheduled-run data dirs are not yours to write, move or prune. The artifacts under them
  ARE world-readable: fetch and read them freely. The shared EXCEPTION:
  `/run/lock/dittofs-run.lock` is there to be TAKEN (below).
- **Announce, then lock, then quiet window.** That is the order. Say what you are about
  to run in the team channel first.
- **Every run takes the lock**: wrap the whole run in
  `flock -w 3600 /run/lock/dittofs-run.lock <command>`. The scheduled runs hold it, yours
  wait for theirs and theirs for yours; the kernel releases it the moment the run ends,
  even on a crash, so it is never left stuck. A scheduled job that gives up waiting is
  skipped and logged, never retried. A run started WITHOUT the lock can overlap a
  scheduled one and skew both results — that is the failure this whole section exists to
  prevent.
- **Check what's running first**: `pgrep -a -f "fio|bench-run|dt"` and `uptime` before
  anything that writes, even with the lock held honestly.
- **Clean up your own scratch** (probe files, ad-hoc benchmarks under the account's home)
  but never a run folder — run folders are the record.

## The host's schedule (what to schedule around)

The owner's authoritative schedule lives in the infra repo (cubbit/dittofs-test-infra,
docs/schedule.md); `cat /etc/cron.d/dittofs-dt /etc/cron.d/dittofs-journal-bench` on the
box confirms the local half. As of 2026-11-27 (all CET):

| When | What | Holds the lock for |
| --- | --- | --- |
| daily 06:00 and 18:00 | scenario canary | ~30 min (waits up to 1 h) |
| daily 18:45 | the nightly (perf night, then system scenarios); ends ~22:10 lately | hours |
| Tue+Fri 08:30 | journal bench full | up to 3.5 h (~1 h 45 m typical) |
| Mon–Fri 13:00 | journal bench small | a few minutes |
| any time a develop/main head lands | its check | ~25 min (skips, retries in 10 min) |
| daily 12:50 | prune of old run folders | seconds |

Quiet windows: about **22:15–05:45**, **07:00–12:45** (minus 08:30–~11:30 on Tue and
Fri), and **13:15–17:45**. The list is illustrative, not exhaustive — trust `pgrep` and
`uptime` over it. A hand run still holding the lock at **19:45 costs that night's
nightly**: the nightly waits up to 1 h and then runs anyway, so plan hand runs to be
done before the evening's work begins.

## Running your own benchmarks

Whatever the suite — journal, engine, a one-off `dt` cell, an ad-hoc measurement — the
method is the same:

1. **Your own checkout**, never the scheduled one: the box fetches this repo anonymously
   over HTTPS, so `git clone https://github.com/cubbit/dittofs.git` under the account's
   home; `git fetch origin && git checkout -f --detach <sha>` to measure the commit you
   mean, in a tree nobody else moves.
2. **Your own run root and data dir**, on the same device as what you measure
   (everything on the box sits on md2, so any directory under the account's home
   measures the same device the scheduled runs do). Keep artifacts: dated folders with a
   `commit.txt` and a `done` marker are the house pattern.
3. **Announce, then wrap the run in the lock**
   (`flock -w 3600 /run/lock/dittofs-run.lock …`).
4. If the suite has a not-later-than guard, use it. The journal's `bench-run.sh`
   computes `BENCH_NOT_AFTER` as the 18:00 canary minus the worst-case run — a
   convention that transfers to any suite: a ~3.5 h worst-case run starts no later than
   14:30; anything shorter scales up from 17:40.
5. Run the suite the way CI does — `test/harness/bin/dt` and the tags each suite defines —
   rather than bare `go test -bench` invocations that miss tags and known failures.

**A worked precedent** (the journal's own runner: `~/journal-bench` +
`~/journal-runs` + `~/journal-bench-data`, driven by
`test/journal/bench-run.sh`'s hand-run mode): its layout — one checkout whose origin is
the HTTPS remote, per-kind run folders stamped `<UTC>-<sha>` holding group outputs and
profiles — is a good shape to copy for a new suite. One subtlety from it that transfers
to any script with its OWN lock env: if you wrap a command in `flock` yourself, do NOT
also point the script's internal lock variable at the same file — the script's inner
flock then waits behind your outer one *from the same process* and deadlocks on
yourself. Wrap or pass a lock env, never both.

## Collecting and reading results

Raw artifacts out, parse locally — remote one-liners over ssh invite quoting failures:

```
rsync -az -e "ssh -p ${DITTOFS_BENCH_PORT:?}" "${DITTOFS_BENCH_HOST:?}":<run-root>/<kind>/ <local archive>/
```

Read numbers by the repo's own rules (CLAUDE.md's benchmarks section): `benchstat` over
`-count=10` runs on the same machine only; never compare across hosts or against a
Docker Desktop figure; each suite carries its own validity checks (the journal's bench
rows pair every figure with a raw twin for this). Benchstat needs cleaned inputs: strip
any `== ` wrapper lines and feed it only `goos/goarch/pkg/cpu` + `Benchmark` lines, or
its config keys split the runs into incomparable groups.

## Ad-hoc device probes (fio, /proc sampling)

For attributing device factors under any benchmark (flush tax, writeback throttling,
filesystem-journal coupling). fio is installed (3.41); job files need **one option per
line** (comma-lists on one line abort every phase); write scratch under the account's
home; get per-job bandwidth via `--output-format=json` and parse the json **after
fetching it locally** — `d["jobs"][i]["jobname"]` names the job, its bandwidth is
`d["jobs"][i]["write"]["bw"]` in KiB/s. Sample once per second while a probe runs:
`/proc/diskstats` (watch `md2` + `nvme0n1p4`/`nvme1n1p4`), `/proc/vmstat`
(`nr_dirty`/`nr_writeback`), and `/proc/fs/jbd2/*/info` (the md2 ext4 journal). Take
the lock for probes too — they write.

Known box facts: `md2` reports `write_zeroes_max_bytes=0` (`FALLOC_FL_WRITE_ZEROES` gains
nothing there); twin-class sequential writes reach ~850–890 MB/s; flush latency ~3 ms;
both members are NVMe. The box's own runs under `/var/tmp/dittofs-perf/` (read-only for
the shared account) are the public record — check what already exists before
commissioning a new measurement.
