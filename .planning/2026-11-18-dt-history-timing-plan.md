# `dt history diff` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `dt history diff` ranks cells and tests by time change between two CI workflow runs, on any branch.

**Architecture:** `history.Diff` is a pure function over two `[]report.Run`. `history.CI` is split so one workflow run can be read by ID, and gains a branch argument. `cmd/dt/history.go` gets a `diff` subcommand and a `--branch` flag.

**Tech Stack:** Go, Cobra, the GitHub REST API through the existing fetcher in `test/harness/history`.

**Spec:** `.planning/2026-11-18-dt-history-timing-design.md`

## Global Constraints

- No new workflow, no committed timings file (spec: Non-goals).
- `--branch` default is `develop`; `diff A B` takes workflow run IDs and needs no branch.
- `--floor` default 100 ms; `--top` default 30.
- A test is keyed by (cell name, `report.Test.Title()`); delta is B − A.
- Only each job's first attempt is read (existing rule, unchanged).
- Tests follow CLAUDE.md "Writing tests": no new canned stubs; synchronise on events, never time; every assertion names the value and the expectation.
- `docs/guide/cli.md` is generated: `go run ./cmd/gendocs`, never hand-edited.
- Commits: no AI attribution, signed (`git commit -S`), concise. Lint and hooks locally need `GOTOOLCHAIN=go1.26.10`.
- Comments describe behaviour only: no issue, PR or plan references.

## Review Focus

- The same test name in two cells of one run: each is its own row, not merged.
- A test with several results in one cell (one `Test` per result): its durations are summed per key.
- A test under the floor in both runs but present in only one: dropped, not listed as new or gone.
- A cell skipped for unmet needs in one run (`report.Skipped`): not compared, as `history.Cells` already excludes it.
- A window with fewer than two distinct workflow runs: an error naming how many it found, not an empty diff.
- An expired or cut-short artifact in one of the two runs: skipped with a warning on stderr, the diff still prints.

---

### Task 1: `history.Diff`

**Files:**
- Create: `test/harness/history/diff.go`
- Test: `test/harness/history/diff_test.go`

**Interfaces:**
- Consumes: `report.Run`, `report.Cell`, `report.Test` (`Title()`, `Duration`), `report.Skipped`.
- Produces:
  ```go
  // Change is one cell's or test's time in two runs. Test is "" for a cell.
  type Change struct {
      Cell, Test string
      A, B       time.Duration
      InA, InB   bool
  }
  func (c Change) Delta() time.Duration // B - A; a side the change is not in counts 0

  type Delta struct{ Cells, Tests []Change }

  // Diff compares what a and b ran. Tests: those whose time moved by at least
  // floor, or that are in one side only and took at least floor there, by Delta
  // descending, at most top (top <= 0 keeps all). Cells: every cell of either
  // side, by Delta descending.
  func Diff(a, b []report.Run, floor time.Duration, top int) Delta
  ```

- [ ] **Step 1: Write the failing tests** in `diff_test.go`, table-driven over real `report.Run` values. Cases and their assertions:
  - `grew`: test 1 s → 3 s, floor 100 ms ⇒ one row, `Delta()` 2 s, `InA && InB`.
  - `shrank`: 3 s → 1 s ⇒ row with `Delta()` -2 s, ordered after a grown row.
  - `new` / `gone`: one side only, 5 s ⇒ row with `InA` or `InB` false and the time in `B` or `A`.
  - `below floor`: 40 ms → 90 ms ⇒ no row; one side only at 50 ms ⇒ no row.
  - `two cells, one name`: `TestX` in `unit/a` 1 s → 2 s and in `unit/b` 5 s → 5 s ⇒ one row, Cell `unit/a`.
  - `several results in one cell`: two `Test` entries named `TestY`, 1 s and 2 s in A, 4 s in B ⇒ A is 3 s.
  - `skipped cell`: a cell with `Result: report.Skipped` in B only ⇒ no cell row.
  - `top`: 5 grown tests, top 2 ⇒ the two largest, `Tests` length 2.
  - `cells`: `Took` 60 s → 90 s ⇒ cell row `Delta()` 30 s.
  - `same run twice` ⇒ `len(Tests) == 0`.
  Failure messages: `got %v, want %v` naming the field and the case.

- [ ] **Step 2: Run to verify it fails**
  Run: `GOTOOLCHAIN=go1.26.10 go test ./test/harness/history/ -run TestDiff -count=1`
  Expected: FAIL, `undefined: Diff`.

- [ ] **Step 3: Implement `Diff` in `diff.go`.** Sum durations per (cell, title) key on each side, take the union of keys, apply the floor rule above, `slices.SortStableFunc` by `Delta()` descending with the key as tie-break so output is deterministic.

- [ ] **Step 4: Run to verify it passes** (same command). Expected: PASS.

- [ ] **Step 5: Break it on purpose.** Make the floor check ignore one-sided rows, re-run, and watch `below floor` fail on its assertion; revert. Then drop the per-key summing and watch `several results in one cell` fail; revert.

- [ ] **Step 6: Commit**
  ```bash
  git add test/harness/history/diff.go test/harness/history/diff_test.go
  git commit -S -m "feat(dt): history.Diff ranks time change between two sets of runs"
  ```

---

### Task 2: Read one workflow run by ID, and pick the branch

**Files:**
- Modify: `test/harness/history/history.go` (the `CI` function, about lines 55-195)
- Modify: `test/harness/history/history_test.go` (`TestCI`, about line 86)
- Modify: `cmd/dt/history.go` (`pastRuns`, `historyFlags`; callers of `history.CI`)

**Interfaces:**
- Consumes: nothing new.
- Produces:
  ```go
  // CI: new branch parameter, "" meaning develop.
  func CI(ctx context.Context, suite string, n int, branch string, warn io.Writer) ([]report.Run, error)

  // Workflow reads the cells of one run of the tests workflow by its ID, any
  // branch and any event, named as CI names a run. A run that does not exist is
  // an error; an artifact that cannot be read is skipped and warn says so.
  func Workflow(ctx context.Context, suite string, id int64, warn io.Writer) ([]report.Run, error)
  ```

- [ ] **Step 1: Extend `TestCI`** with two cases against its existing HTTP fixture: a `branch` argument of `"feat/x"` makes the listing request carry `branch=feat%2Fx` (assert on the request the fixture saw, as the test already records `fetched`); and a new `TestWorkflow` reading run `3` returns the same cells `CI` returns for that run, including when its event is `pull_request`. Also a `Workflow` call for an unknown ID returns an error containing the status text.
- [ ] **Step 2: Run to verify it fails**
  Run: `GOTOOLCHAIN=go1.26.10 go test ./test/harness/history/ -run 'TestCI|TestWorkflow' -count=1`
  Expected: FAIL (compile error on the new signature).
- [ ] **Step 3: Refactor `CI`.** Pull the HTTP helpers (`get`, `getJSON`, token and API base) into one small unexported value, and the per-run artifact loop (the `for _, w := range slices.Backward(found)` body) into `readRun(api, w workflowRun, prefix, warn)`. `CI` lists runs and calls `readRun`; `Workflow` fetches `/actions/runs/{id}` and calls `readRun`. Listing keeps skipping `pull_request` and cancelled runs. The `branch=` query uses `url.QueryEscape(cmp.Or(branch, "develop"))`.
- [ ] **Step 3b: Bring `TestCI` in line with the test rules or say why not.** Its fixture answers canned JSON in GitHub's place. If it cannot be rebuilt on a real implementation, leave a one-line `decision:` comment at the fixture stating that and what would overturn it.
- [ ] **Step 4: In `cmd/dt/history.go`** add `branch *string` to `historyFlags` (`--branch`, default `develop`, help: the branch whose runs `--ci` reads), thread it through `pastRuns` to `history.CI`, and update the other caller of `pastRuns` (`known prune`).
- [ ] **Step 5: Run to verify it passes**, plus `go vet ./cmd/dt/ ./test/harness/...`. Expected: PASS, clean.
- [ ] **Step 6: Commit**
  ```bash
  git add test/harness/history cmd/dt/history.go
  git commit -S -m "feat(dt): history reads one workflow run by ID and takes --branch"
  ```

---

### Task 3: `dt history diff`

**Files:**
- Modify: `cmd/dt/history.go` (new `newHistoryDiff() *cobra.Command`, added in `newHistory`)
- Test: `cmd/dt/history_test.go`
- Regenerate: `docs/guide/cli.md`

**Interfaces:**
- Consumes: `history.Diff`, `history.Workflow`, `history.CI`, `table`, `duration` (existing in `cmd/dt`).
- Produces:
  ```go
  // ends splits runs, oldest first, into the first and the last workflow run
  // by Run.Name; it is an error when there are fewer than two distinct names.
  func ends(runs []report.Run) (a, b []report.Run, err error)
  ```
  Usage: `dt history diff [A B]` (two workflow run IDs) or `dt history diff --last N [--branch B]`; flags `--floor` (duration, 100ms), `--top` (int, 30), `--ci`-style token from `GITHUB_TOKEN`. Two args and `--last` together is an error.

- [ ] **Step 1: Write the failing tests**
  - `TestEnds`: runs named `r1,r1,r2,r3` ⇒ a is both `r1` runs, b is `r3`; one name only ⇒ error containing `found 1`; empty ⇒ error containing `found 0`.
  - `TestDiffOutput`: render a `history.Delta` through the command's formatter (factor the rendering into `writeDiff(out io.Writer, d history.Delta, top int) error` so it takes a plain value) and assert the header `TEST CELL A B DELTA`, a `+2s`-style delta on a grown row, `new`/`gone` markers, and the cell section.
  - Argument check: one positional arg ⇒ error; two args plus `--last` ⇒ error.
- [ ] **Step 2: Run to verify it fails**
  Run: `GOTOOLCHAIN=go1.26.10 go test ./cmd/dt/ -run 'TestEnds|TestDiffOutput' -count=1`
  Expected: FAIL, undefined.
- [ ] **Step 3: Implement `newHistoryDiff`.** Resolve A and B (IDs via `history.Workflow`, or `ends` over `history.CI(ctx, "", last, branch, warn)`), call `history.Diff`, print cells then tests with `table`. Print a one-line header naming both runs and their commits.
- [ ] **Step 4: Run to verify it passes**, then `go run ./cmd/gendocs` and check `git diff docs/guide/cli.md` shows only the new command and flags.
- [ ] **Step 5: Run it for real.** `GITHUB_TOKEN=$(gh auth token) go run ./cmd/dt history diff --last 10 --branch feat/storage-v2`. Compare its top rows with `dt history --ci` for one suite to confirm they agree, and record in the PR whether 100 ms is a good floor.
- [ ] **Step 6: Commit**
  ```bash
  git add cmd/dt docs/guide/cli.md
  git commit -S -m "feat(dt): history diff ranks what got slower between two runs"
  ```

---

## Deferred

| Deferred | Why | Owner | Trigger |
| --- | --- | --- | --- |
| Nightly job committing a timings file | Artifacts hold 90 days, enough for the question asked | unowned | a regression older than 90 days needs tracing |
| Per-test timeout / panic summary in the report | Different problem (hangs), different code | unowned | the next cell that times out with no per-test time |
| A CI gate failing on a time regression | Needs a baseline policy first | unowned | the diff has been used by hand for a few weeks |
| Suite filter on `diff` | Two runs read whole are small enough | unowned | a diff is too slow to read for the artifacts it downloads |
