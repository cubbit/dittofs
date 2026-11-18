# Design: finding where CI time went (`dt history diff`)

**Status:** draft, awaiting approval. Size: a small change (one subcommand, one pure function, one refactor).

## Why

CI times on `feat/storage-v2` keep growing and a cell now times out (journal-conformance hit the 30 min
`go test` limit). Some growth is honest (new code, new tests); some is not. Nobody can currently say
*which tests* got slower between two points in time, so the offenders stay hidden until a cell fails.

## What already exists

- `tests.yml` runs on every push to `develop`, `main` and `feat/storage-v2`, plus a nightly cron. Every cell
  uploads `dt-run-<job>-<attempt>` with `summary.json`; each test in it has `duration` in milliseconds
  (10 ms resolution from Go, so under-10 ms tests read 0). Cell time (`Took`) is whole seconds.
- Artifacts live 90 days.
- `dt history --ci` (`test/harness/history`, `cmd/dt/history.go`) downloads those artifacts and aggregates
  p50/p90 per suite/test, and `dt history SUITE/TEST` already lists one test's time run by run.

So the data and the fetcher exist. **No new workflow and no stored timing file.**

## Gap

1. `history.CI` lists only `?branch=develop` runs, so `feat/storage-v2` is invisible.
2. Nothing compares two runs: no "what got slower between A and B".
3. A trend over a window is only per test, one at a time.

## Goals

- `dt history diff` ranks cells and tests by time change between two runs of the same tests workflow.
- Works on any branch and any event (a PR run included), because runs are named by workflow run ID.
- A windowed form for the trend question: oldest vs newest run of the last N on a branch.

## Non-goals

- No nightly job, no committed timings file, no history past artifact retention (90 days).
- Not a fix for hangs: a test that never finishes has no duration, so a diff shows it only as missing.
  A per-test timeout or panic summary in the report is a separate issue.
- No charts, no regression gate in CI.

## Approaches considered

1. **Extend `dt history` (chosen).** Reuses the fetcher, report parsing and table output. Smallest.
2. **Nightly job writing a big YAML.** Duplicates what artifacts already hold, adds a file to keep in sync,
   and needs somewhere to commit it. Worth it only to outlive the 90-day retention.
3. **Separate script outside `dt`.** Re-implements artifact download and `summary.json` parsing.

## Design

**Naming a run.** `diff` takes workflow run IDs, which are branch-independent, so it needs no branch flag.

```
dt history diff A B            # two workflow run IDs, any branch, any event
dt history diff --last N       # oldest vs newest of the last N runs on --branch
```

**Branch (my call, open to change).** The windowed form and `dt history --ci` get `--branch`, default
`develop`. Nothing about it is tied to `feat/storage-v2`: if that branch is retired the default is already
right and the flag just sits unused. Alternative if you'd rather not have it: derive the branch from the
checkout's current branch. I did not pick that because it makes the command's answer depend on where you run it.

**Fetch.** Split `history.CI` into "list workflow runs" and "read one run's artifacts" so `diff A B` can read
a run by ID. The listing keeps skipping `pull_request` runs (fork-branch caveat in its comment); reading
by ID does not.

**Diff.** A pure function over two `[]report.Run`, no I/O:

- A test is keyed by (cell name, suite path, test name). Delta = B − A.
- Rows: tests present in both with `|delta| >= floor`; tests only in B (new, listed with their time);
  tests only in A (gone). Sorted by delta, largest increase first.
- A cell row for each cell, with `Took` from both runs.
- `--floor` defaults to 100 ms, so Go's 10 ms resolution and run-to-run noise don't dominate.
- `--top N` limits test rows (default 30).

A run holds a cell's first attempt only (existing rule), so a retry doesn't double-count.

## Tests

Per the repo rules: no mocks. `Diff` is a function of real `report.Run` values, table-driven (grew, shrank,
new, gone, below floor, same test in two cells). Fetch-by-ID extends the existing `TestCI` setup. That test
uses an `httptest` server that answers canned JSON, which the test rules ban for new tests. Since the
change touches that function, bring it in line or say why not in the PR.

## Open points

- Whether `--branch` is the right shape (above).
- Whether 100 ms is the right default floor; settle by running the command on real artifacts once built.
- Flag rename of `docs/guide/cli.md` is generated: run `go run ./cmd/gendocs`.

## Deferred

| Deferred | Why | Owner | Trigger |
| --- | --- | --- | --- |
| Nightly job committing a timings file | Artifacts hold 90 days, enough for the question asked | unowned | a regression older than 90 days needs tracing |
| Per-test timeout / panic summary in the report | Different problem (hangs), different code | unowned | the next cell that times out with no per-test time |
| A CI gate failing on a time regression | Needs a baseline policy first | unowned | the diff has been used by hand for a few weeks |
