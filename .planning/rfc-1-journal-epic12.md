# Epic 12: the suite green

**Sources:** [RFC 1](../docs/internals/rfcs/rfc-1-journal.md) §9.1, §9.3, §9.4, §9.6 and §9.7;
[RFC 29](../docs/internals/rfcs/rfc-29-test-harness.md) §4.7.
**Track:** the team.
**Epic 12 estimate: 8 subtasks, 29 man-days.**

Epic 12 takes the journal's suite from listed to passing, for RFC 1 without 7b and 9b.

State of the known-failure files on `feat/storage-v2` at `9efa0702f`, after 7a's T7.12.

`test/journal/known.yaml`: 185 entries, by the message each fails on (the counts add to 185: 109 + 5 + 8 + 2 + 4 + 4 + 53):

- 109 fail on a 9b call: `stub: MarkOffloaded` (87), `Offload` (16), `Release` (2), `Stamp` (2), `Hold` (2).
- 5 fail on a 7b call: `stub: Restamp` (2), `Inspect.Verify` (1), and a recovery message ending `names no record` (2).
  - With the 9b rows these are the 114 that stay listed, each with a recorded decision.
- 8 fail on a 9a call: `stub: Forget` (2), `Repack` (2), `repack_copy` (1), and `stub: the repack` (3).
  - The three read as the call `the` under the pattern below, so T12.1 tightens them to `stub: Repack`.
- 2 are 7a's Open refusals, `segments true, want `: a generic got/want fragment, which T12.1 tightens to the full text.
- 4 are the race-detector skips, kept.
- 4 are bare: `stub:` (3) and `stub: ` with a trailing space (1).
- 53 are other: seal, idle-seal, corrupt-counter, model and figure rows.
  - The reopen no longer masks them: 7a's reopen landed, and no row fails on `holds segments` any more.

`test/engine/known.yaml`: 26 entries. `test/engine/soak-known.yaml`: 0.

- 11 wait on the journal's `Repack` or `Forget` (9a).
- The rest are RFC 8's own.
- Their messages are the engine's, and a `reason` is free text that drifts when it is reworded.
- decision: T12.1 does not classify this file. RFC 8's T10.3 owns re-pointing it. The ceiling: a row that waits on a journal call is then tagged by its engine owner. Overturned if RFC 8's T10.3 does not land, and then T12.1 adds the file by test name.

What that means:

- Retagging is by message, never by issue, and it is redone when 9a lands.
  - A row that stopped at a stub runs further and fails on the next message.
  - T12.1 starts from the files as 7a left them. 7a's and 9a's remaining PRs re-run the classification by hand until T12.1's test exists, and use it after.
- What T12.2 may inherit, by message: the 53 other rows and the 4 bare rows once tightened, so 57.
  - 7a's open subtasks (#4044: 6 rows) and 9a clear some of them in their own PRs.
  - About 45 rows at 0.3 md is 13.5 md; the placeholder is 14 md.
- The system rows that need `vfs.New` and the offload engine are RFC 8's second part.

## Requirements

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R1 | Every conformance check passes on a real filesystem on every supported platform, except the rows 7b and 9b own and the four race-detector skips | MUST | RFC 1 §9.1, §9.3, §9.4 | T12.2 |
| R2 | A known failure names an open owner: the epic whose missing call it fails on, or epic 12 (T12.2) for a defect in code that has landed; none names an epic that has landed | MUST | RFC 29 §4.7 | T12.1 |
| R3 | The model-based test compares a reference model after every step of random sequences; a failing sequence is shrunk and kept as a regression case | MUST | RFC 1 §9.1 | T12.5 |
| R4 | The soak runs for hours against a capacity smaller than the data written, and the counters, open descriptors and footprint stay flat | MUST | RFC 1 §9.1 | T12.6 |
| R5 | Benchmarks run in real time on a real device, each result recording the device's raw figures, the filesystem and mount options and the sync bound | MUST | RFC 1 §9.6 | T12.7 |
| R6 | The conformance suite runs per change; the model, the soak and the benchmarks never run per change (benchmarks are registered `manual` until a reference runner exists, then daily) | MUST | RFC 1 §9.1, §9.7 | T12.4 |
| R7 | Allocation counts are checked where the race detector is off, because it changes them (the job `unit.options.journal-conformance-norace` already does) | MUST | RFC 1 §9.7 | T12.3 |
| R8 | Each test's resource use is held to a baseline calibrated for that test | MUST | RFC 1 §9.7 | T12.8 |

Out of scope until 9b (the plan's epic 9 owns them, run once it lands):

- J4, J5 and J8.
- The model's `Offload` and `Release` steps.
- The soak's offload and release load.
  - The soak and model run without them.
  - They make reclaimable space with overwrites, deletes and truncates.
- The system rows that need `vfs.New` and the offload engine are RFC 8's second part.

## 6. Subtasks

### T12.1 Re-triage the lists

**Subtask T12.1 estimate: 2.5 man-days.**

- **Depends on:** none. It starts from the files as 7a left them.
- **Covers requirements:** R2.
- **Deliverables:**
  - Every entry of `test/journal/known.yaml` retagged to the owner of its message.
    - `stub: MarkOffloaded`, `stub: Offload`, `stub: Release`, `stub: Hold`, `stub: Stamp`: 9b.
    - `stub: Restamp`, `stub: Inspect.Verify`, and a message ending `names no record`: 7b.
    - `stub: Forget`, `stub: Repack`, `stub: repack_copy`: 9a.
    - `stub: Open`, `stub: open_seal_after_truncation`: 7a.
    - Seal, corrupt-counter and figure rows: epic 12 (T12.2), because epic 8, which built the code, has landed.
  - A `stub:` class is an anchored prefix, `stub: <Call>`: the identifier after `stub: ` matches `[A-Za-z_.]+` and equals the call.
    - So `a Release on a read-only journal returned …` is not a 9b row.
    - `stub: the repack` (three rows) is tightened to `stub: Repack`, and `segments true, want ` (two rows) to the full got/want text.
  - A bare message, in every `known:` file, is rejected.
    - Bare: a message that equals `stub:` after trimming, or equals `journal: not implemented`.
    - Equality, not `Contains`: `…journal: not implemented: vfs: unavailable` is a specific row.
    - The harness matches a message with `strings.Contains`, so a bare one matches any stub failure.
    - Each bare row on `feat/storage-v2` is tightened to its call.
  - The test reads every `known:` file `test/suites.yaml` lists, so the next one is not missed.
    - It classifies `test/journal/known.yaml` only. `test/engine/known.yaml` is RFC 8's (decision above).
  - `test/journal/trace.yaml`: the three `owner: epic 11` rows are re-pointed to their owners.
  - A test beside `trace_test.go`: message class to owning epic issue.
  - A sentence in 9a's known-failures rule: a row that now fails further is retagged in the same change.
- **Acceptance criteria:**
  - No entry names a landed epic or a closed subtask.
  - In `test/journal/known.yaml`, every message matches exactly one class, or the test fails.
  - The test fails on a bare message in any `known:` file.
  - The test is green on the files as they stand after T12.1's retag.
- **Tests:**
  - The new test, shown to fail six ways:
    - retag one entry;
    - add a journal entry with an unknown message;
    - leave a bare `stub:` entry;
    - leave a bare `journal: not implemented` entry in the engine file;
    - leave a `names no record` row tagged to 7a;
    - give `a Release on a read-only journal …` to 9b.
  - `dt`'s grading over every edited list.

### T12.2 Burn down what 7a and 9a leave

**Subtask T12.2 estimate: 14 man-days.**

- **Depends on:** T12.1, and 7a and 9a landed.
- **Covers requirements:** R1.
- **Deliverables:**
  - Every entry still red against 7a or 9a is fixed or recorded as a decision.
  - This includes the rows 7a's reopen unmasks, once classified again.
  - Each entry is removed in the change that turns it green.
  - A placeholder: about 45 rows at 0.3 md, less what 7a and 9a clear in their own PRs.
- **Acceptance criteria:**
  - The lists hold only entries owned by 7b or 9b, each with a recorded decision.
  - The four race-detector skips stay.
- **Tests:**
  - The suite itself, under `-race`, on every platform the matrix lists.
  - `dt` fails when a listed test passes.

### T12.3 Hold the non-race leg

**Subtask T12.3 estimate: 0.5 man-days.**

- **Depends on:** none.
- **Covers requirements:** R7.
- **Deliverables:**
  - Confirm `unit.options.journal-conformance-norace` (tier `pr`) runs both allocation tests.
  - Keep the four race-detector entries: they are deliberate skips for the `-race` leg.
  - Name the job as R7's enforcer in the entries' `reason`.
- **Acceptance criteria:**
  - The `-race` leg stays green with the four skips.
  - The non-race job runs both allocation tests green.
- **Tests:**
  - Adding an allocation to a hot path fails the non-race job (existing).

### T12.4 Register the tiers

**Subtask T12.4 estimate: 2 man-days.**

- **Depends on:** none.
- **Covers requirements:** R6.
- **Deliverables:**
  - In `test/suites.yaml`:
    - conformance per change;
    - the model on many seeds, nightly;
    - the soak, nightly;
    - the benchmarks as `tier: manual`, like `engine-bench`, needing `DITTOFS_BENCH_DIR` on the device under test.
  - Each new build tag added to lint-go's `go vet -tags=` list.
  - A ledger row: the daily benchmark run waits for a reference runner (RFC 29 §4.10).
  - The suite's duration, measured with `dt history` once the checks at 10⁶ extents pass.
    - If the `pr` budget needs it, the heavy checks move to `merge` or `nightly` with expected-skip entries.
- **Acceptance criteria:**
  - Each suite runs at its tier.
  - No benchmark runs per change.
- **Tests:**
  - `dt run` lists each suite at its tier.
  - `go vet` over the tag list compiles the tagged files.

### T12.5 The model on many seeds

**Subtask T12.5 estimate: 2 man-days.**

- **Depends on:** T12.4.
- **Covers requirements:** R3.
- **Deliverables:**
  - A nightly run over many seeds.
  - Alphabet: write, fill, truncate, deallocate, delete, settle, repack, crash, reopen.
  - Two files, two streams, two shares.
  - A failing seed is printed, shrunk and committed to the corpus.
- **Acceptance criteria:**
  - A failing seed reproduces from its printed value.
  - It shrinks to a short sequence.
- **Tests:**
  - A planted bug in the model's `Truncate` makes the run fail and shrink.

### T12.6 The soak without offload

**Subtask T12.6 estimate: 3 man-days.**

- **Depends on:** T12.4.
- **Covers requirements:** R4.
- **Deliverables:**
  - A suite that runs for hours against a capacity below the data written.
  - Operations: write, overwrite, delete, truncate, sync, repack, reopen.
  - Runs under the profiler.
- **Acceptance criteria:**
  - Index and descriptor counters are flat across the run.
  - The footprint is flat across the run.
- **Tests:**
  - A planted descriptor leak in a short run of the same suite.
  - It must fail on the descriptor assertion.

### T12.7 J1, J2 and J3

**Subtask T12.7 estimate: 3.5 man-days.**

- **Depends on:** T12.4.
- **Covers requirements:** R5.
- **Deliverables:**
  - J1, J2 and J3 run on the reference box's real device, via `DITTOFS_BENCH_DIR`.
  - Method:
    - `-count=10`, compared with `benchstat` on the same machine;
    - `b.ReportAllocs()` on the hot paths;
    - setup before `b.ResetTimer()`;
    - the §9.6 method and recorded fields.
  - Results replace the proposed targets under the index's rule.
  - Measured values recorded for the proposed constants J1 to J3 exercise:
    - segment cap;
    - sync bound;
    - idle seal.
- **Acceptance criteria:**
  - Each result records the raw-I/O figure on the same filesystem.
  - Each records the mount options and the sync bound.
  - Each constant has a measured value and a keep-or-change decision.
- **Tests:**
  - The harness refuses a RAM-backed device.
  - The benchmark output is compared with the raw figure.

### T12.8 Profiling baselines

**Subtask T12.8 estimate: 1.5 man-days.**

- **Depends on:** none.
- **Covers requirements:** R8.
- **Deliverables:**
  - Each test's goroutine, descriptor and heap threshold, with slack.
  - Calibrated from a run of the suite.
- **Acceptance criteria:**
  - The suite does not flake over ten runs on the same machine.
- **Tests:**
  - A planted goroutine leak fails the one test it lives in.

## Order and pull requests

- Order:
  - T12.1 first, so the list is honest.
  - T12.3, T12.4 and T12.8 any time.
  - T12.5 to T12.7 after T12.4.
  - T12.2 after 7a and 9a land.
- Suggested PR 1: T12.1 and T12.3 (3 md).
- Suggested PR 2: T12.4 and T12.8 (3.5 md).
- Suggested PR 3: T12.5 and T12.6 (5 md).
- Suggested PR 4: T12.7 (3.5 md).
- Suggested PR 5: T12.2 (14 md).

## Open points

- T12.2 is a placeholder: re-measure it once 7a and 9a have landed and the lists are classified again.
- Which machine is the reference box, and who runs the manual benchmarks, is for the team to name.
- The daily benchmark run (RFC 1 §9.1) waits for a reference runner.
