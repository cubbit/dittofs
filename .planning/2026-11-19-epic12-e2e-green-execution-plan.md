# RFC 1 Journal v2, Epic 12 — E2E Green — Execution Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement
> this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Take the journal's conformance suite from listed to passing for RFC 1 without epics 7b and
9b (issue [#3395](https://github.com/cubbit/dittofs/issues/3395)), and register the model, soak and
benchmarks at their tiers.

**Architecture:** Five stacked pull requests over the epic-9a chain. PR1 makes `test/journal/known.yaml`
honest (a classifier test beside `trace_test.go`) and pins the non-race leg; PR2 registers the new suites
in `test/suites.yaml` and calibrates the profiler baselines; PR3 fills the model (many seeds, alphabet
without offload/release) and the soak; PR4 adds J1–J3 as a manual benchmark suite on a real device;
PR5 burns down every row that is a defect in landed code, once 9a has merged.

**Tech stack:** Go 1.26 (pinned toolchain), the `dt` harness (`test/harness/bin/dt`),
`test/suites.yaml` registry, `golangci-lint` v2.15.0.

**Spec:** [`.planning/rfc-1-journal-epic12.md`](https://github.com/cubbit/dittofs/pull/4762/files)
(PR #4762 branch `docs/rfc1-journal-epics-10-12-breakdown`, not merged) for what to build;
**RFC 1** (`docs/internals/rfcs/rfc-1-journal.md` §9.1, §9.3, §9.4, §9.6, §9.7) and
**RFC 29** (`docs/internals/rfcs/rfc-29-test-harness.md` §4.7, §4.10) on the working branch for
normative detail. Where the planning document and the RFCs disagree, the RFCs win and the
disagreement is raised to the user, not silently resolved.

## Global Constraints

- **Branch:** all work stacks on `origin/feat/journal-t9-10-forget` (PR #4993, which itself sits on
  PR #4846). PR1 branches from it; each later PR branches from the previous. When #4993 merges,
  retarget PR1's base to `feat/storage-v2` (GitHub restacks the rest while the branches live).
  Rebase, never merge, when the base moves. The user has said #4846/#4993 should not drift much —
  if a review there lands large changes, stop and rebase the whole stack rather than diverging.
- **PR conventions:** title `test(journal): … (T12.x[+T12.y]) [#3395]` — task in parens, issue in
  brackets, at the end. `--assignee didimelli`. No AI attribution anywhere (repo rule: no Claude
  footers, no Co-Authored-By for AI). Base exceptions: only `docs:` commits may touch only
  documentation; each commit type must match its diff (CI checks per commit).
- **Commits:** concise, signed if possible, `Co-Authored-By` omitted per repo rule.
- **Local toolchain:** `GOTOOLCHAIN=go1.26.10` for fmt/vet/hook parity with CI; format with the
  pinned toolchain only, no drive-by format fixes. Lint exactly as CI does: golangci-lint
  v2.15.0, whole repo. A fresh worktree needs `graphify hook install && graphify update .`
  (opt-in per machine).
- **Known-failure rules (RFC 29 §4.7):** every entry has `status`, `fix`, `category`, `reason`;
  `fix: planned` requires `issue: "#N"`; `dt` fails a run whose listed test passes; a `message:`
  makes the entry match only failures whose output contains it (`strings.Contains`). Quarantine
  matches any message; `dt` grades known before quarantine.
- **Test rules (CLAUDE.md):** no mocks/stubs/recording fakes; real filesystem under `t.TempDir()`;
  no fixed ports/paths; synchronise on events, not time; a changed step's regression test must be
  shown to fail on its assertion (revert the fix, watch it fail). Comments carry no external IDs;
  a knowingly-narrow gate gets a `decision:` marker, a knowingly-simple implementation a
  `ponytail:` marker, both at the code site.
- **Conformance runs:** as CI does — `test/harness/bin/dt run unit` (the journal suites are its
  options) and `dt run journal` (the `journal-conformance` suite), with
  `IT_CHECK_CMD` set to include `-tags=journal_conformance`; judge any red test against **the
  current RFC text**, not the entry list. Unit default: one `-race -count=1`; a red `-race` run
  is a real bug, never retried away. A new build tag must join lint-go's `go vet -tags=` list in
  the same change (this epic adds `journal_soak` and `journal_bench`).
- **`known.yaml` grading caution:** a row's `message` is matched with `Contains`, so a bare
  `stub:` matches any stub failure — the classifier test (Task 1) exists to make that impossible.

## Review protocol (the user's ask, every PR)

1. Between subtasks: the usual superpowers review (superpowers:requesting-code-review) — a fresh
   reviewer on the completed subtask before the next starts.
2. Before opening each PR: one final review round on the **whole PR diff**, for correctness **and
   conformance with RFC 1 §9 and RFC 29 §4.7/§4.10** (the five input classes below are the
   checklist seed).
3. Then `/llm-council` on the PR's core decision (what it registers/gates and why), then open the
   PR via the pr-opening skill. After any review replies: re-request review from the original
   reviewers (replies alone do not notify).
4. PR5 additionally re-measures T12.2 before opening (it is a placeholder estimate).

## File map (what exists at the stack tip, `origin/feat/journal-t9-10-forget`)

- `test/journal/known.yaml` — 156 rows today (185 at `9efa0702f`, before 9a's last PRs trimmed it).
- `test/journal/trace.yaml` + `test/journal/trace_test.go` — the RFC-row ↔ test ↔ owner map; runs
  inside `lint-repo` as `go test ./test/journal/` (no build tag).
- `test/suites.yaml` — the registry. Relevant today: `unit.options.journal-conformance-norace`
  (tier `pr`, runs `TestCounted_AllocationsPerCall|TestRFC1D_StatsCostIsFlat|TestStatsAfterOpenWalksNoLock`
  without `-race`), `journal-conformance` (tier `pr`, `known: test/journal/known.yaml`,
  `-race -tags=journal_conformance`), `engine-bench` (tier `manual`, the pattern to copy for
  benchmarks), `lint-go`'s `go vet -tags=` list, `dt docs --check` in `lint-repo`.
- `pkg/block/journal/conformance/` — the suite: `rfc1_*_test.go`, `model_test.go`, `norace_test.go`,
  `race_test.go`, `counted_*_test.go`, `damage_test.go`, `stats_cost_test.go`, corpus under
  `testdata/model`.
- `pkg/block/journal/journaltest/model/` — the reference model. `Generate(seed, n)` draws the full
  weighted alphabet; **`GenerateWithin(seed, n, ops []Op)` already exists** (subset alphabet,
  same weights renormalised); `shrink.go` shrinks failing sequences; `DefaultSeed`,
  `DefaultSteps = 200`. `model_test.go` takes `DITTOFS_SEED` from the env and writes shrunk
  sequences to the artifact directory.
- `internal/prof/` — `prof.Watch(tb, Options{HeapSlack})` fails a test that leaks goroutines,
  descriptors or heap; overlapped watches are refused; `prof.Descriptors()` exports the open count;
  its own tests already plant a leaked goroutine (`TestWatchFailsOnLeakedGoroutine`).
- Journal constants J1–J3 exercise: `SegmentCap` and `IdleSealBytes`
  (`pkg/block/journal/internal/core/config.go`, `pkg/block/journal/internal/capacity/`), the sync
  bound (`pkg/block/journal/handle.go`); the test fixture uses
  `FixtureIdleSealBytes = FixtureSegmentCap/16`.
- `docs/internals/testing.md` — the suite table `dt docs --check` verifies; regenerate with
  `test/harness/bin/dt docs`.

## Deferral ledger (owned elsewhere, do not build)

- The `journal_` metrics exports, tracer/logger wiring, lock-free `Stats` →
  **epic 13 of the same plan** (PR #4762).
- Offload wiring, salvage, the system e2e rows, `vfs.New` and the offload engine →
  **RFC 8's part 2**. J4, J5, J8 and the model/soak `Offload`+`Release` steps →
  **epic 9b**. The `test/engine/known.yaml` re-point → **RFC 8's T10.3**.
- The daily benchmark run → a deferral row already in PR #4762's plan §6 ("a reference runner
  exists" is the trigger); verify it is on the base at execution, add nothing if it isn't merged yet.
- Tightening the heap watch's 16 MiB slack, or per-test thresholds where they land → the T12.8
  calibration (above, Task 4) found that default ≈15× above the worst measured per-test delta,
  so it stays a coarse detector that catches large leaks only; **the harness track** owns it,
  when a watch must catch a smaller leak or a calibration on new hardware shows different deltas.

---

## PR 1 — T12.1 re-triage the lists, and T12.3 hold the non-race leg

**Branch:** `journal/epic12-retag` (from `origin/feat/journal-t9-10-forget`)
**Title:** `test(journal): re-triage known.yaml to its open owners and hold the non-race leg (T12.1, T12.3) [#3395]`

### Task 1 (T12.1): the classifier test, and the retag it green-runs on

**Files:**
- Create: `test/journal/known_test.go` (no build tag; runs in `lint-repo` via `go test ./test/journal/`)
- Modify: `test/journal/known.yaml` (every row re-owned; four bare messages tightened)
- Modify: `test/journal/trace.yaml` (three `owner: epic 11` rows)
- Modify: `pkg/block/journal/conformance/damage_test.go` (only if `waitAtHold(t, p, "the repack")` still
  exists — rename the label to `Repack`. As built: the label existed and was renamed; the three
  `stub: the repack` rows 9a's last PRs had already removed no longer existed at this base, so no
  row was tightened against it)

**Interfaces:**
- Consumes: the `known:` files list parsed from `test/suites.yaml` (parse it with `dt`'s own
  registry loader under `test/harness/`, the same code that parses it in production — do not
  hand-roll a second YAML schema); `trace_test.go`'s file-locating convention for the repo root.
- Produces: `TestKnownFilesClassified` (or similarly named) — the contract later tasks and 7b/9b
  PRs rerun when they touch `known.yaml`.

- [ ] **Step 1: write the failing classifier test**, in `test/journal/known_test.go`. It must:

  1. Read every `known:` file `test/suites.yaml` lists (journal, engine, engine-soak, adapter,
     posix, pynfs, smb, smbtorture) and reject a bare message in **any** of them: a message that
     equals `stub:` after trimming whitespace, or equals `journal: not implemented`. Equality,
     not `Contains` — `…journal: not implemented: vfs: unavailable` is a specific row and passes.
     Name the suite and file in the failure (`got %q in %s, want a message that names its failure`).
  2. Classify `test/journal/known.yaml` only (decision marker at the test: engine's file is RFC 8's
     T10.3, overturned if its rows are still red when those issues close — then it joins by name).
     Every row gets exactly one owner and the test fails on a row that has none, naming the test
     and its message:
     - **Message class** — an anchored prefix `stub: <Call>`: the identifier after `stub: ` must
       match `[A-Za-z_.]+` and the class equals the call before any other character. So
       `a Release on a read-only journal returned …` is *not* a class, and `stub: the repack` is a
       class named `the` (which no owner table holds, so it fails — the fix is the label rename in
       `damage_test.go`; as built, no `the` row existed at this base, the rename landed anyway, and
       a future failure there yields the class `Repack`). Map classes to owners:
       `MarkOffloaded|Offload|Release|Hold|Stamp` → 9b (`#3379`);
       `Restamp|Inspect.Verify` → 7b (`#3596`), plus one suffix rule: a message ending
       `names no record` → 7b (`#3596`);
       `Forget|Repack|repack_copy` → 9a at the documented branch tip, which is landed code in this
       stack — so on this branch such a row, still red, is a **defect in landed code** and its
       owner is epic 12 itself (`#3395`). (At the epic-12 branch point 9a's PRs are the base, so
       no row may carry a 9a issue as its owner. If, contrary to the stack, a 9a row is still in
       the file, re-run the classification after confirming with the user.)
     - **Test-name table** — for rows whose message names no call (the seal, idle-seal,
       corrupt-counter, model and figure rows, and the spanning message
       `segment 3 is not among the journal`), a table in the test maps the test name to its owner.
       The owner is the one the row's `reason` names per the epic's counts: 9b rows → `#3379`,
       7b rows → `#3596`, everything else on landed code (epic 8's `#3378` rows, the closed
       T6.6 `#3748` rows) → `#3395`. The tightened bare rows do not join this table: their tightened
       messages are stub classes in their own right (three 9b — `MarkOffloaded` twice, `Offload` —
       and two of the four also move from `#3378` to `#3379`), so the class rule routes them.
       decision: one message spans owners, so the message cannot route them; the table's ceiling
       is that a new row has no entry until someone adds it, and the test fails on it.
     - **Open-owner set** — every `fix: planned` row's `issue` must be one of
       `{"#3596": "epic 7b", "#3379": "epic 9b", "#3395": "epic 12 (T12.2)"}`. No row names a
       landed epic or a closed subtask (today that kills, alongside the 9a/7a tags, the four
       race-detector skip rows' `#3764` instances — a closed subtask:
       re-issue those to `#3395`, see Task 2). `fix: wontfix` entries carry no owner and skip this.
  3. Fail on the three `owner: epic 11` rows in `trace.yaml`: they are re-pointed to their owners —
     `rfc1 / 9.4 / C / "stranded tag salvaged after serving"` → `RFC 8's second part`;
     `rfc0 / 1.4 / "clock bound"` → `RFC 8's second part`;
     `rfc8 / 15 / "one clock"` keeps `TestClock_WallJumpMovesNoTimer` and its owner line also
     becomes `RFC 8's second part` (the other half of that row is the node's, run there). The
     reason strings stay; `trace_test.go` requires an owner to carry a reason. If assertion of
     owners-in-trace is not what `trace_test.go` parses, extend the new test to read `trace.yaml`'s
     owner field and reject `epic 11` by name — the value is free text today.

- [ ] **Step 2: run it red, six ways.** `go test ./test/journal/ -run TestKnownFilesClassified -v`
  after each deliberate edit, then revert: (a) retag one entry to a wrong owner; (b) add a journal
  entry with a message class no table holds; (c) leave a bare `stub:` in the journal file;
  (d) leave a bare `journal: not implemented` in the engine file; (e) tag a `names no record`
  row to 7a; (f) give the read-only-`Release` row's message to the 9b class. Each fails **on its
  own assertion**. This is the "test must be able to fail" evidence — keep the transcript.

- [ ] **Step 3: retag `known.yaml` green.** Every row re-owned per the tables; the four bare rows
  (`stub:`, `stub: ` with trailing space, and the `journal: not implemented` one if present)
  tightened to their real messages by re-running the failing tests and copying the actual output,
  then classified by their strengthened messages (by message class, mostly 9b — not `#3395`, which
  no bare row turned out to hit);
  `segments true, want ` rows tightened to the full got/want text (down to the per-run identity
  hash, which is random per journal); the `the repack` label renamed
  in `damage_test.go` (as built, its three rows no longer existed at this base, so nothing was
  tightened against it); the four race skips
  re-issued (Task 2 Step 1 covers the reason edit, do them together).

- [ ] **Step 4: grade the retag the way CI will.**
  `IT_CHECK_CMD="test/harness/bin/dt run journal" test/harness/bin/dt plan`-style dry runs aside,
  run: `test/harness/bin/dt run journal` and `test/harness/bin/dt run unit` (known-failure
  grading is exercised there), and confirm no row that now has a wrong message grades "new".
  Expected: journal-conformance red only on rows that are still red, each graded against its
  entry; `go test ./test/journal/` green, including `trace_test.go` (rows ↔ tests still match).

- [ ] **Step 5: commit** — `test(journal): known.yaml names only open owners, checked by a test`.

### Task 2 (T12.3): the non-race leg names its enforcer

**Files:**
- Modify: `test/journal/known.yaml` (the four race-detector skip rows' `reason`)
- Modify: `test/suites.yaml` — only if a check finds a gap (see Step 1); otherwise no diff.
- Modify: `.github/workflows/tests.yml` — only if the norace job step there no longer matches the
  option that enforces R7 (PR1's `ci:` commit added the step; check it, do not assume it).

**Interfaces:**
- Consumes: `unit.options.journal-conformance-norace` (tier `pr`): today
  `go test -timeout=5m -tags=journal_conformance -run=^(TestCounted_AllocationsPerCall|TestRFC1D_StatsCostIsFlat|TestStatsAfterOpenWalksNoLock) ./pkg/block/journal/conformance/`.
- Produces: nothing new; R7's enforcement is verified, stated where the rows name it.

- [ ] **Step 1: verify coverage.** The four race-skip rows (message
  `the race detector changes allocation counts`, two carrying `#3395`, two `#3764`) each name an
  allocation test that skips under `-race`. List each row's test, and check each appears in the
  norace job's `-run` pattern (`TestCounted_AllocationsPerCall…` is an unanchored suffix, so
  `TestCounted_AllocationsPerCallFailsWithOneAdded` matches). Any missing test joins the pattern —
  that is the only `suites.yaml` diff this task is allowed.
- [ ] **Step 2: name the enforcer.** Each of the four rows' `reason` ends with the same sentence it
  has, plus R7's authority explicitly: "the job `unit.options.journal-conformance-norace` is R7's
  enforcer (RFC 1 §9.7)". Keep the rows `status: skip`, `fix: wontfix` — they are deliberate
  `-race`-leg skips, and with Task 1 they now carry `#3395`.
- [ ] **Step 3: run both legs.** `test/harness/bin/dt run unit` (the norace option included) and
  `dt run journal` (the `-race` leg) — expected: norace option green on every named allocation
  test; the `-race` leg green with the four skips expected. Then plant one allocation on a hot
  path the tests count and re-run the norace leg — it must fail (existing mutation tests), revert.
- [ ] **Step 4: commit** — `test(journal): the non-race job is named as the allocation checks' enforcer`.

**PR 1 close-out:** the two commits above (plus one `docs:` commit adding this execution plan to
`.planning/`, committing a copy is expected; never edit `.planning` files owned by PR #4762 — the
execution plan is a new file). Then: whole-PR review round → `/llm-council` → open the PR
(base `feat/journal-t9-10-forget`, assignee `didimelli`).

---

## PR 2 — T12.4 register the tiers, and T12.8 profiling baselines

**Branch:** `journal/epic12-tiers` (from `journal/epic12-retag`)
**Title:** `test(journal): the model, the soak and the benchmarks at their tiers, baselines held (T12.4, T12.8) [#3395]`

### Task 3 (T12.4): tier registration

**Files:**
- Modify: `test/suites.yaml` (three new suites; lint-go's vet tag list gains `journal_soak,journal_bench`)
- Modify: `docs/internals/testing.md` (regenerate with `test/harness/bin/dt docs`)

**Interfaces:**
- Consumes: the registry format (RFC 29 §4.3) and the existing tier patterns — `journal-conformance`
  (pr), `engine-soak` (pr today), `engine-bench`/`reconcile-bench` (manual, `DITTOFS_BENCH_DIR` guard).
- Produces: suite names later tasks implement against, exact and stable:
  - `journal-model` — `format: go-json`, `tier: nightly`,
    `cmd: go test -tags=journal_conformance,journal_model -run ^TestModelManySeeds$ -timeout=110m -artifacts -outputdir={out} ./pkg/block/journal/conformance/`,
    `env: { DITTOFS_MODEL_SEEDS: "100" }`, no `known:` (a red seed must fail the run, not be listed),
    `timeout: { test: 110m, cell: 120m }`, no `parallel: true`.
  - `journal-soak` — `format: go-json`, `tier: nightly`,
    `cmd: go test -tags=journal_conformance,journal_soak -run ^TestJournalSoak -timeout=150m -artifacts -outputdir={out} ./pkg/block/journal/conformance/`,
    `env: { DITTOFS_SOAK_DURATION: "2h" }`, no `known:`,
    `timeout: { test: 150m, cell: 160m }` — **mandatory**: the registry's default cell timeout
    (60m) would kill a 2-hour soak; the run itself is 2h with the cell backstop above it, inside
    nightly's 4-hour budget. The duration knob starts the hours, `DITTOFS_SOAK_DURATION` is
    the interface (Task 6).
  - `journal-bench` — tier `manual`, copy `engine-bench`'s guard shape verbatim (fail with exit 2
    when `DITTOFS_BENCH_DIR` is unset; count knob `JOURNAL_BENCH_COUNT`,
    `-count=10 -benchtime=1x -tags=journal_conformance,journal_bench -run ^$ -bench '^BenchmarkJ[123]_'`,
    output to `{out}/bench.txt`, `format: exit` — `go test -json` sends no pass event for a
    passing benchmark and a go-json cell would grade it failed).

- [ ] **Step 1: name the empty tags.** Add `journal_model`, `journal_soak`, `journal_bench` to
  lint-go's `go vet -tags=…` list (they cover tests PR3/PR4 land; an unused tag vets as nothing,
  a tag added later than its files breaks vet in CI the other way). Commit with the suite entries.
- [ ] **Step 2: write the registry entries** with the Interfaces block's exact names and tiers
  (R6: conformance stays per-change — already is; the model and soak never run per change;
  benchmarks `manual` until a reference runner exists). decision: no `known:` files on the three
  new suites — a red model seed, soak drift or a benchmark that refuses to run is a finding to
  fix, not a list to grow.
- [ ] **Step 3: reach CI.** A cell `dt plan` lists runs nowhere until CI picks it up, and a cell
  reaches CI only through a `job:` field of its own or the jq `select` in `tests.yml`'s
  "Plan the cells" step — the whitelist `journal-conformance` itself is named in, cell by cell.
  The `journal-conformance-norace` option fell through exactly this trap: `dt plan` listed it,
  no CI job ran it, and PR1's `ci:` commit bolted it on as a job step. So the change that
  registers the suites also names `journal-model` and `journal-soak` (tier `nightly`) in that jq
  `select` or gives each a `job:` — never leaving a listed suite that no CI job runs; `journal-bench`
  stays `manual` and needs no CI entry, since no dispatch or tier names it.
  Verify the mechanism the way `journal-conformance` reaches CI: run `dt list` and
  `dt plan --json` (the cells exist), then grep `tests.yml` for each new suite's name or its
  `job:` (the jq whitelist that runs it) — `dt docs --check` will not notice a suite CI never
  runs.

- [ ] **Step 4: verify registration.** `test/harness/bin/dt run journal-model` and
  `dt run journal-soak` list and (with the tests not yet written) pass green on zero matches —
  confirm grade behaviour is "no tests matched" and not a grading exit; regenerate
  `docs/internals/testing.md` via `dt docs` and pass `dt docs --check` in `lint-repo`;
  `go vet -tags=journal_conformance,journal_model,journal_soak,journal_bench ./...` compiles.
  Also verify the deferral-ledger row for the daily benchmark run is already in the base's
  `.planning/rfc-1-journal-plan.md` §6 (it ships with PR #4762; if that PR is still unmerged on
  your branch base, note it in the PR description and add nothing).
- [ ] **Step 5: duration baseline** — `dt history`'s per-suite duration is read **after PR5**
  (the epic gates it on the 10⁶-extent checks passing); leave a TODO line in the PR description,
  not in code.
- [ ] **Step 6: commit** — `test(harness): journal model, soak and bench suites at nightly/nightly/manual`.

### Task 4 (T12.8): profiling baselines

**Files:**
- Modify: `pkg/block/journal/conformance/*_test.go` where `prof.Watch` is called with slack that
  the calibration shows is either wasteful or flaking
- Modify: `internal/prof/` only if calibration exposes a real defect (a leaked-descriptor
  platform quirk, e.g. on macOS dev machines) — expect no diff

**Interfaces:**
- Consumes: `prof.Watch(tb, prof.Options{HeapSlack})` (fails on leaked goroutines, leaked
  descriptors, heap grown over its slack; two watches may not overlap in one process);
  `prof.Descriptors()`.
- Produces: per-test `HeapSlack` values, set only where the default (16 MiB) is shown wrong in
  both directions.

- [ ] **Step 1: calibrate.** Run the suite ten times on one machine:
  `GOTOOLCHAIN=go1.26.10 go test -race -tags=journal_conformance -count=10 -timeout=6h ./pkg/block/journal/conformance/`.
  For each watched test record the heap delta and descriptor / goroutine high-water marks (the
  watch already prints them when it fails; instrument a one-off local run if numbers are needed,
  do not commit a metrics harness). Any test that flakes once in ten is a bug to root-cause
  (superpowers:systematic-debugging), not a threshold to raise; raising a threshold requires the
  run's evidence named in the commit message.
- [ ] **Step 2: hold the baselines.** Set explicit `HeapSlack` only on tests whose measured
  live-heap delta justifies a different bound; every such value carries one comment line naming
  the calibration (`heap grows ~X MiB here (b.N-capped waiter map), slack Y`). Tests with no
  watch on the hot paths get one (the soak test in PR3 will rely on this infrastructure being
  trustworthy) — audit, don't blanket-wrap.
- [ ] **Step 3: the planted-leak check.** RFC 1's R8 test is "a planted goroutine leak fails the
  one test it lives in": `internal/prof` already proves it (`TestWatchFailsOnLeakedGoroutine`).
  Extend it by one case only if the suite's watch scoping is not already covered: a leak planted
  *while another test's watch is not active* must not fail the other test (the `overlapped`
  machinery). Run `go test ./internal/prof/`.
- [ ] **Step 4: commit** — `test(journal): per-test heap baselines calibrated from ten runs`.

**T12.8 outcome (calibrated 2026-11-19, one darwin/arm64 macOS 27.0.1 host, GOTOOLCHAIN=go1.26.10):**
zero-diff — the 16 MiB default `HeapSlack` was shown wrong in neither direction, so no explicit
value landed. Race leg: the six watch-bearing tests ×10 (3:26 wall) plus the full suite twice
(3:58, 4:32), identical failure sets, zero prof failures; the only red is `TestModel`'s
pre-existing known failure, stable in all ten. Norace heap leg ×10: worst per-test live-heap
delta +1,126,656 B (~1.07 MiB, `TestRFC1D_StatsCostIsFlat`, the 10⁵-extent rig; the norace
trio's others stay at ~578 KiB and ~51 KiB). Reproduce with
`go test -race -tags=journal_conformance -count=10 -timeout=6h ./pkg/block/journal/conformance/`
and a one-off per-watch delta log in `prof`'s cleanup (instrument locally, never commit it).
`TestModel` itself dies at corpus step 1 on the `Offload` stub, so its delta must be
re-measured under later PRs' data path before treating the default as generous there.
The default stays ≈15× above that worst delta — a coarse detector for large leaks only;
the tightening row is in the deferral ledger.

**PR 2 close-out:** whole-PR review → `/llm-council` → open the PR.

---

## PR 3 — T12.5 the model on many seeds, and T12.6 the soak

**Branch:** `journal/epic12-model-soak` (from `journal/epic12-tiers`)
**Title:** `test(journal): the model on many seeds and the hours-long soak, without offload (T12.5, T12.6) [#3395]`

### Task 5 (T12.5): `TestModelManySeeds`

**Files:**
- Create: `pkg/block/journal/conformance/model_many_test.go` (tag `//go:build journal_conformance,journal_model`)
- Create: `pkg/block/journal/journaltest/model/plant_truncate_test.go` or equivalent (see Step 4)

**Interfaces:**
- Consumes: `model.GenerateWithin(seed, n, ops)` and the `Op` set from `journaltest/model/gen.go`;
  `model.Check(name, steps, open, note)` (compares after every step, shrinks a failing sequence to
  the artifact directory); `journaltest.ModelTargets(t)`; `modelSeed()`'s `DITTOFS_SEED`
  convention (`model_test.go`).
- Produces: `TestModelManySeeds`, the test the `journal-model` suite runs (PR2's exact name);
  the planted-truncate leg the `unit.options.journal-plant` option gains.

- [ ] **Step 1: write the test red.** `TestModelManySeeds` reads `DITTOFS_MODEL_SEEDS`
  (default `1` so the file also runs usefully under plain `go test -tags=journal_conformance`),
  and for each of seeds `0..n-1` runs `model.Check(t, fmt.Sprintf("seed-%d", s),
  model.GenerateWithin(uint64(s), model.DefaultSteps, modelOpsWithoutOffload), open, …)`.
  `modelOpsWithoutOffload` is every `Op` the weights table in `gen.go` names **except
  `OpOffload` and `OpRelease`** (epic: out of scope until 9b; the model and the soak run without
  them, making reclaimable space with overwrites, deletes and truncates) — read the full
  weights table and list the ops explicitly at the definition, with one decision line naming 9b
  as the owner that removes the exclusion. The rig keeps RFC 1 §9.3's shape the existing
  `TestModel` already has (files across streams and shares on one journal). A failing seed's
  name and the `DITTOFS_SEED=<s>` reproduction line must be in the failure, on the assertion.
  First run it green uses one seed and passes; then verify red as in Step 2.
- [ ] **Step 2: show it fails on a defect.** Plant a bug — the journal's truncate handling, via a
  fault at the seam the crash/damage tests already use (a wrong-byte or dropped-removal variants
  of an existing seam fault; no new mock): with the plant, the many-seed run must fail **and**
  the shrunk sequence printed; revert. (If the only economical plant is in the reference model's
  truncate — test code — that is acceptable for a self-check of the harness's detection+shrink,
  but the journal-seam plant is preferred; take whichever is smaller and say which in the commit.)
- [ ] **Step 3: nightly scale.** With `DITTOFS_MODEL_SEEDS=100` locally, the run finishes well
  inside the suite's budget (measure; if a step count or seed count must drop to fit nightly,
  change the number in PR2's suite entry here, not the RFC's intent). Record in the PR
  description: seeds run, wall time, shrinker evidence. A failing-seed corpus file lands under
  `testdata/model/` if any seed found a real defect (then the defect is fixed in this PR too,
  or the seed is printed in the PR and the defect routed to the user — the epic wants a real
  defect fixed, not listed).
- [ ] **Step 4: the planted-truncate leg (R3's acceptance again, structured).** Add to the
  `unit.options.journal-plant` option's command list one run of
  `-run ^TestModelManySeedsFindsPlantedTruncate$` under the plant tag pair used by the existing
  planted-leg (`journal_plant_split` shows the pattern: tag the planted variant, run only that
  test). The planted bug is in `Truncate`; the assertion: the run fails, and the shrunk
  sequence is shorter than the drawn one. Update `test/suites.yaml` and lint-go's tag list if a
  new tag is introduced.
- [ ] **Step 5: commit** — `test(journal): the model runs many nightly seeds without offload`.

### Task 6 (T12.6): `TestJournalSoak`

**Files:**
- Create: `pkg/block/journal/conformance/soak_test.go` (tag `//go:build journal_conformance,journal_soak`)

**Interfaces:**
- Consumes: the conformance rig's fixture (real filesystem under `t.TempDir()`, virtual clock),
  `prof.Watch`/`prof.Descriptors`, the journal's `Stats` counters, the repack/reopen machinery
  the suite already exercises.
- Produces: `TestJournalSoak` (+ exactly how its duration is set: `DITTOFS_SOAK_DURATION`
  environment, `0` meaning a ~5-minute calibration default so the suite is runnable by hand;
  the nightly hours come from PR2's suite entry, which already sets
  `DITTOFS_SOAK_DURATION: "2h"` — change that entry's number if calibration demands, not the
  default here).

- [ ] **Step 1: write the soak red-checked.** Structure, in order (RFC 1 §9.1 soak row):
  - A journal whose `MaxBytes` is well below the data the run writes (capacity pressure with no
    offload/release: overwrites, deletes and truncates make the reclaimable space).
  - A mixed ops loop (write, overwrite, delete, truncate, sync, repack — the two-file,
    two-stream, two-share floor of §9.3 held) driven for the duration; a crash+reopen on a
    slow period (the rig's seam crash, not process death); syncs forced at step boundaries.
  - Periodic snapshots of: `Stats().ExtentCount`, live segment count, `Spares`,
    `prof.Descriptors()`, and the process heap (`runtime.ReadMemStats` is the honest floor —
    `prof.Watch` bounds the test's own leak). Snapshots synchronise on loop edges, never sleeps.
  - The R4 assertions in the test body, after the last window: index counters and open
    descriptors flat across the run (end value within the calibrated threshold of the windowed
    minimum-maximum band — calibrate the band in Step 2); footprint flat; failure messages name
    the counter, the first and last samples (`got %v..%v, want flat`).
- [ ] **Step 2: calibrate and plant.** Run the short default twice: once clean (records the band
  this machine observes; a threshold in the test may then carry one comment line naming the
  calibration evidence), and once with a **planted descriptor leak** — the test itself drops an
  `os.Open` handle mid-run, never closing it — asserting the descriptor assertion **fails on
  the descriptor exactly**. The plant is fault injection on the measurement, not a test double.
  The planted variant is a short, negative subtest of the same file (no, and never an entry in
  any `known:` list).
- [ ] **Step 3: run it as CI will.** `dt run journal-soak` (nightly entry, 2 h) once — expected
  green with counters flat; `dt run journal-model` again green. Confirm `dt run unit` if the
  option list changed (it should not have).
- [ ] **Step 4: commit** — `test(journal): an hours-long soak holding counters, descriptors and footprint flat`.

**PR 3 close-out:** whole-PR review → `/llm-council` → open the PR.

---

## PR 4 — T12.7 J1, J2 and J3

**Branch:** `journal/epic12-bench` (from `journal/epic12-model-soak`)
**Title:** `test(journal): J1 to J3 on a real device, with the method and recorded baselines (T12.7) [#3395]`

### Task 7 (T12.7): the benchmark suite

**Files:**
- Create: `pkg/block/journal/conformance/bench_test.go` (tag `//go:build journal_conformance,journal_bench`)
- Create: `docs/internals/journal-bench-results.md` (the committed results file; `docs:` commit)
- Modify: `test/journal/known.yaml` — **not expected**: benchmarks have no known list; nothing
  runs them per change.

**Interfaces:**
- Consumes: `DITTOFS_BENCH_DIR` (a directory on the device under test; the suite refuses to run —
  exit 2 — without it, exactly as `engine-bench` does), the `journal-bench` suite (PR2), the
  journal's public `WriteAt`/`ReadAt`/`Sync` handles over shares/streams, the raw-figure
  baseline benchmark below.
- Produces: benchmarks named to match the suite's `-bench '^BenchmarkJ[123]_'`, and the raw
  reference on the same filesystem.

- [ ] **Step 1: the RAM-backed refusal (RFC 1 §9.5's test).** The suite must refuse a
  RAM-backed benchmark device *before* running: in the suite's sh guard (PR2's entry), after the
  `DITTOFS_BENCH_DIR` check, refuse when the directory's filesystem is one the journal's own
  platform probe (`Init`'s unsuitable-storage check, RFC 28 Appendix A — reuse its predicate)
  rejects as RAM-backed/tmpfs for durability reasons, or by mount-type name where the probe
  cannot apply away from `Init`. Keep it one shell line if a probe helper exists; otherwise an
  internal/prof-style helper. A test asserts the refusal (point the guard at a tmpfs-shaped
  mount on a host that has one, or assert the helper's classification on a table of
  (filesystem string → verdict) rows — the per-OS table is data, test the table).
- [ ] **Step 2: the raw baseline, on the same filesystem.** `BenchmarkRawSequential`,
  `BenchmarkRawRandom4k`, `BenchmarkRawFsync4k` writing directly through `os` + `Sync` on
  `DITTOFS_BENCH_DIR`, with the same block size, queue depth and sync pattern J1–J3 use (RFC 1
  §9.6). Each J-target's result is only interpretable as a fraction of these.
- [ ] **Step 3: J1 write path.** `BenchmarkJ1_WriteSequential{4k|64k|256k|1m}` and
  `_WriteRandom{4k…}` with and without a `Sync` after each write, 16 writers on one append
  stream; `BenchmarkJ1_MixedDurability` (one writer syncing every 4 KiB beside one streaming
  256 MiB files synced at their end, one stream). Method (each in `b.ReportAllocs()`;
  setup before `b.ResetTimer()`, or between `b.StopTimer/StartTimer`; state built once, reused,
  knowing `b.N` sizing applies to the timed region). `p99 ≤ 20 µs WriteAt`-excluding-sync-and-copy:
  measure per-op times in the benchmark body and `b.ReportMetric` the p99, with the collection
  itself outside the timed region.
- [ ] **Step 4: J2 and J3.** `BenchmarkJ2_WriteScaling{1|4|16|64}` concurrent files; J3:
  `BenchmarkJ3_PointLookup` with the index at 10³ and 10⁶ extents (the suite's index-rig exists
  for the counted checks — reuse), `BenchmarkJ3_WarmRead1m`.
- [ ] **Step 5: run on the real device.** With `DITTOFS_BENCH_DIR` on the reference device the
  team names (the user is the team here — **ask which machine before running**; if none is
  available, land the suite and the empty results file, and the run happens when one is), run
  `JOURNAL_BENCH_COUNT=10` and `benchstat` over the two runs' `bench.txt` outputs on the same
  machine (never cross-machine). Record in `docs/internals/journal-bench-results.md`, per
  RFC 29 §4.10 and the index's "Recording results" rule: the commit it was measured on, the box,
  each row's queue depth, the device's raw figures, filesystem, mount options and the sync bound —
  and the keep-or-change decision for each of the three proposed constants J1–J3 exercise
  (**segment cap, sync bound, idle seal** — note today's values from
  `pkg/block/journal/internal/core/config.go` and `handle.go`, and the fixture's
  `IdleSealBytes = SegmentCap/16` convention, alongside the measured evidence for keeping or
  changing each). Replace the RFC's proposed targets in the file's table with the measured ones
  alongside, not instead of the RFC. A `docs:` commit; hand the file path to the user afterwards.
- [ ] **Step 6: tests of the suite itself.** `dt run journal-bench` with the variable unset
  exits 2 naming the variable; the RAM refusal's classification test is green; on no real device
  available, mark Step 5's half **open in the PR description**, do not fake numbers.
- [ ] **Step 7: commit(s)** — `test(journal): J1 to J3 with the raw baseline and the refusal`,
  then the `docs:` results commit.

**PR 4 close-out:** whole-PR review → `/llm-council` → open the PR.

---

## PR 5 — T12.2 burn down what 7a and 9a leave

**Branch:** `journal/epic12-burndown` (from `journal/epic12-bench`, itself rebased by then)
**Title:** `test(journal): the suite green for RFC 1 without 7b and 9b (T12.2) [#3395]`
**Timing:** only after #4846 and #4993 have merged and PR1–PR4 have been retargeted and merged.

### Task 8 (T12.2): the burn-down

**Files:**
- Modify: `test/journal/known.yaml` (rows leave in the changes that turn them green)
- Modify: `pkg/block/journal/**` and `test/journal/known_test.go`'s name table, per finding
- Modify: `test/suites.yaml` only if the duration evidence moves heavy checks to `merge`/`nightly`

**Interfaces:**
- Consumes: the classifier test from PR1 (R1's honest list), `dt history` for duration (Task 3's
  deferred read).
- Produces: the epic's end state.

- [ ] **Step 1: re-measure first.** The estimate (8 md, ~27 rows at 0.3 md) is a placeholder.
  Run the full suite on every platform the matrix lists (`-race`), classify fresh with the
  classifier test, and report the actual row set to the user **before** burning down — the
  count may be very different; approval to proceed assumes the measured count, and the epic
  re-measures on the real rows.
- [ ] **Step 2: burn down.** For each row still red against landed code (the `#3395` set: the
  classifier test names them precisely): root-cause with
  superpowers:systematic-debugging; fix in `pkg/block/journal/**` at the layer that owns the
  behaviour (the consumer-layer test rule: the regression test goes where the wrong data is
  *read*, not where the patch is); the row leaves in the same change that turns it green,
  and the known-failure entries leave only with `dt run journal` showing them green.
  A row not fixed on purpose becomes a `decision:` in `known.yaml`'s reason naming the
  ceiling and the owner (7b/9b rows keep their issues), never a silent stay.
- [ ] **Step 3: end-state acceptance** — R1: everything passes except the 7b/9b rows and the
  four race-detector skips. R1's platform clause: run the suite on every platform the matrix
  lists (the classifier and `dt` handle Windows legs as CI does). Confirm by
  `grep -c '^- test' test/journal/known.yaml`: remaining rows = 7b+9b-owned + four skips + the
  skips' wontfix rows. `dt history`'s duration read from Task 3 closes here: if the `pr` budget
  needs it, move the heavy checks to `merge`/`nightly` with expected-skip entries — otherwise
  record the duration in the PR and move nothing.
- [ ] **Step 4: commit(s)** — one per defect or per small batch that its own suite run shows.

**PR 5 close-out:** whole-PR review → `/llm-council` → open the PR.

---

## Open points (raise with the user, never decide alone)

- The reference box for J1–J3: who runs the manual benchmarks and on which machine (epic's own
  open point). Asked at task 3/7 execution time.
- T12.2's real size at PR5 start (epic's placeholder).
- Rows on the stack tip that carry a 9a issue as owner: re-run the classifier fresh at Task 1;
  if 9a is still not merged when PR1 is made, confirm with the user that PR1 rides on the 9a
  branch before its names enter the owner set.
- The three-engine-file rows: RFC 8's T10.3 owns them; do not touch them. (`test/engine/known.yaml`
  has 11 rows on the journal's `Repack`/`Forget`; those rows will grade green once 9a's branch
  is under them — check but do not own.)

## Review Focus (the five input classes the RFCs imply, beyond each task's own tests)

1. **A bare or drifting `message:`** (R2/R29 §4.7): a row whose message matches any failure
   (`stub:`, `journal: not implemented`) or whose text reworded as the code changed — Task 1's
   bare-rejection runs over *every* `known:` file; the whole-PR round greps reworded tests for
   their row's updated message.
2. **A row left against landed code**, hiding a real defect behind an issue number that has
   shipped — Task 1's open-owner set and Task 8's burn-down.
3. **A benchmark measuring on the wrong device or against no raw figure** (RFC 1 §9.5, §9.6):
   RAM-backed `DITTOFS_BENCH_DIR`, missing mount-option/sync-bound fields, cross-machine
   `benchstat` — Task 7's refusal and results file.
4. **A suite registered at the wrong tier** (R6): the model, soak or a benchmark running per
   change, or a conformance check pushed to nightly to dodge red — Task 3's `dt list` read and
   the review of every tier field against R6.
5. **A threshold calibrated to the machine that flaked** (R8): a raised `HeapSlack` or widened
   flat-band that silences a leak rather than bounding measured behaviour — Task 4's and Task 6's
   calibration, each named value carrying its run's evidence.
</content>
