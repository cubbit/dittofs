# Journal v2, epic-8 leftovers: one PR for #3951 #3952 (#3950 audited, not built; verify-and-close #3953 #3990)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the open issues of epic 8 ([#3378](https://github.com/cubbit/dittofs/issues/3378)) with one PR on `feat/storage-v2`: one still-live bug (#3951, the share tally undercounting records that land outside a grant), one regression test for a bug #4214 already fixed (#3952), the audit of the deliberate-ceiling turn-on (#3950), which found it wider than one write path and left it open, and evidence-based closure of two issues whose work already landed but never closed (#3953, #3990).

**Architecture:** No structural change. The share tally gains `Capacity.Landed`, which every tagged writer outside `Grant.Wrote` calls so a retirement refunds only bytes the tally holds: durability's re-appends and loss records through a `durability.Config.Landed` hook, the removal and forget records directly, a repack pass's copies through the copy ledger and its carried records through the pass's count. A tag mid-seed takes such bytes into a pending figure the seed adds in, so the figure only ever errs high. #3950 was audited and not built (see Task 3). A datapath regression test proves a stream-sealed-but-unrolled segment retires (#3952). Everything rides the existing rigs: the capacity rig on real collaborators (PR #3992) and the datapath core fixture.

**Tech Stack:** Go, the journal v2 packages under `pkg/block/journal/`, the `dt` harness (`test/harness/bin/dt`), Grafana-style `decision:` markers.

**Spec:** the four issues themselves, plus `docs/internals/rfcs/rfc-1-*.md` §6 (version contract on a refused write) read from `origin/develop`.

## Global Constraints

- Tests per `CLAUDE.md`: no mocks/stubs; fault wrappers around real implementations only; a changed test must be shown to fail on its assertion by reverting the fix locally.
- Waits: bounded wait as the assertion with a comment naming the backstop; never `time.Sleep` for synchronisation.
- Comments: behaviour only, no external IDs in code; `decision:` marker wherever a gate is deliberately narrower than its name.
- One commit per issue in the PR; `test():`/`fix():` prefixes; no AI attribution anywhere.
- Toolchain: `GOTOOLCHAIN=go1.26.10` for local lint and commit hooks; run pinned `golangci-lint` per the CI definition, not the local default; pre-push hook on this branch skips storage-v2 (`DITTOFS_SKIP_PREPUSH=1` if it fires).
- Suites as CI runs them: `test/harness/bin/dt run unit` (do not hand-run two suites at once).
- PR: base `feat/storage-v2`, title as opened (the plan's first title named the refund budget and the refused write's version, which were not built), `--assignee didimelli`. `Closes #N` never fires on storage-v2: every issue closes by hand after merge.
- Worktree `.worktrees/epic8-leftovers`, branch `fix/journal-epic8-leftovers` off an updated `origin/feat/storage-v2`.

## Review Focus

1. **Refund starvation.** The refund budget must not eat legitimate refunds: bytes admitted through `Grant.Wrote` that later retire must still lower the tally (Pinned by Task 2, case 2: without it the tag would slowly refuse all writes).
2. **Seed race.** A retirement racing `seedFor` must refund exactly once — the existing pair arithmetic must keep its one-read property (Task 2, case 3, mirrored from the `Retired` comment race).
3. **Version-on-error misuse.** A caller given `(v, err)` with `err != nil` might apply the version as success; the audit step in Task 3 exists to prove `Handle.WriteAt` and its engine consumer record a partial write, not assume success.
4. **Stale-comment drift.** #4214 fixed behaviour silently and left comments asserting the old world; grep for the same pattern repo-wide in Task 1 (the `conformance/rfc28_c_test.go:1279` "park on that held call until the roll" wording is checked and left if it describes a different mechanism).
5. **Silent fix without landing evidence.** #3990 and #3953 must close only with named, verifiable evidence (commit + test name per item), never "looks done".

---

### Task 1: #3952 — the sealed-unrolled retire regression test and the stale comments it exposed

**Files:**
- Modify: `pkg/block/journal/internal/datapath/retire_copies_test.go` (comments at ~lines 22–26 and ~48–53; new test beside `TestRetirement_ThroughExecutorLeavesNoCopyEntry`, which it mirrors minus the roll)
- Reference: `pkg/block/journal/internal/segment/stream.go` seal success tail (`sealedRef.Release()`), the #4214 fix being pinned.

**Interfaces:**
- Consumes: the file's existing fixture (`newTestCore`, `mustShare`, `testFile`, `syncFiles`, `sameStreamFile`) and the executor's retire entry the existing test uses.
- Produces: `TestRetirement_TakesASegmentTheStreamSealedButNeverRolledAwayFrom` — the guard that the fix in base already passes.

- [ ] **Step 1: Write the regression test, modelled on `TestRetirement_ThroughExecutorLeavesNoCopyEntry` minus the successor-roll write:** write a record, `Seal(false)`, do **not** write again, retire the sealed segment through the executor path the existing test takes. The retire runs in a goroutine with `select` on a done channel against one generous bounded backstop (30 s), and a comment naming it as the assertion — the fix-less code parks here forever. Assert the retirement completed (`durableCollector`-equivalent record for this file's fixture) and the segment is out of the set.
- [ ] **Step 2: Prove it guards the bug.** Stash the change, comment out the `sealedRef.Release()` in the seal success tail of `internal/segment/stream.go` (the `ours == true` release after `WriteSeal`), run the test: it must FAIL on the bounded-wait assertion, not hang past the backstop. Restore the release and re-run: PASS.
- [ ] **Step 3: Rewrite the two stale comments in this file.** Both claim "a seal leaves the segment's descriptor to the stream until its next roll"; now the seal releases the write ref itself (#4214). The roll in the two existing tests stays — it gives the successor its predecessor name and exercises the normal path; only the justification is wrong. Then grep the journal packages for other "until its next roll"-style claims about the seal's ref and fix any that describe this mechanism.
- [ ] **Step 4: Run the packages.** `go test ./pkg/block/journal/internal/datapath/ ./pkg/block/journal/internal/segment/` — PASS, then `test/harness/bin/dt run unit`.
- [ ] **Step 5: Commit.** `test(journal): the retirement of a segment the stream sealed but never rolled from [#3952]`

### Task 2: #3951 — the share tally's refund budget

> **Superseded while executing — see "Outcome" at the end.** The refund-bucket design below is a no-op: every raiser of `shareTally` raises the bucket by the same amount, so it always equals the tally and the clamp changes nothing. What was built is `Capacity.Landed`, fed by every tagged landing outside `Grant.Wrote`.

**Files:**
- Modify: `pkg/block/journal/internal/capacity/take.go` — `Retired` (~:918), `seedFor` (~:854), `Grant.Wrote` (~:983), and every other raiser of `c.shareTally` in the file (enumerate with `rg 'shareTally\['`); the `Capacity` struct's counters block.
- Test: `pkg/block/journal/internal/capacity/take_test.go` (the #3992 rig of real collaborators).

**Interfaces:**
- Consumes: `c.mu` (capacity domain), `ShareFigures`, the rig's fault wrappers.
- Produces: a per-tag `refundable map[core.ShareTag]int64` counter under `c.mu` — no public signature changes. Invariant: `Retired` never subtracts more than the tally's own raisers ever put in, so bytes that entered the set through durability's unadmitted appends (re-append `appendAgain`, loss `appendRecord`) — which `Grant.Wrote` never counted — refund nothing.

- [ ] **Step 1: Enumerate the tally raisers.** `rg 'shareTally\[' pkg/block/journal/internal/capacity` — every site that raises the figure (seed baseline, `Wrote`, any copy hand-over) raises `refundable` by the same amount in the same lock region; every retirement subtraction lowers it by what it actually subtracted.
- [ ] **Step 2: Write the failing tests in the rig, three cases:**
  1. *Unadmitted bytes refund nothing* — land a record on tag1 through the set without a grant (what durability does: `set.AddRecord` under the tag, no `Wrote`), retire that segment so the set's per-tag `retired` rises, call `Capacity.Retired(tag1)`: the admission's share figure must be unchanged (a later `Take` at the old limit must still be refused).
  2. *Admitted bytes still refund* — an ordinary admitted write, then retire: the refund MUST lower the tally (this case pins Review Focus 1 and must pass both before and after the fix — before the fix it proves the mechanism works; that it also passes after is the guard).
  3. *Seed race* — the retirement racing `seedFor` refunds exactly once, mirrored from the existing `Retired` race comment, with the unadmitted bytes in play.
- [ ] **Step 3: Run to verify case 1 fails on current code** (the tally is wrongly lowered today) — the assertion is `got %v, want %v` on the share figure.
- [ ] **Step 4: Implement the clamp.** In `Retired`: `sub := min(retired-delta, c.refundable[tag]); tally -= sub; refundable -= sub` (keeping the existing ≥0 clamp). Add the `decision:` marker at the clamp: the set's retired figure includes bytes that never passed this admission — durability re-appends and loss records — so the refund is bounded to bytes the tally itself took in; the ceiling is a fail-closed overcount of at most those bytes; overturned by routing durability's appends through the admission. Note in the PR body that this departs from the issue's Set-side `added` counter suggestion: one domain, one lock, no `ShareBytesAndRetired` API churn, and the Capacity already knows its own intake.
- [ ] **Step 5: Re-run; revert-verify.** All three cases PASS; comment out the clamp (not the counter maintenance) and case 1 must fail again. Restore.
- [ ] **Step 6: Run the packages and the suite.** `go test ./pkg/block/journal/internal/capacity/ ./pkg/block/journal/internal/segment/` and `dt run unit`.
- [ ] **Step 7: Commit.** `fix(journal): the share tally refunds no bytes its admission never counted [#3951]`

### Task 3: #3950 — a refused unreserved write returns the version it drew

> **Superseded while executing — see "Outcome" at the end.** The audit in Step 1 found the contract wider than one write path, so nothing below Step 1 was built. The only consumer of `Handle.WriteAt`'s version discards it and records nothing; recording a partial write needs the published extent surfaced through `Handle.WriteAt`, `Reservation.WriteAt`, the engine interface, the model and the fakes. #3950 stays open as the tracker, and the `decision:` marker in `datapath/write.go` says why.

**Files:**
- Read first (audit): `pkg/block/journal/internal/datapath/write.go:66-235` (the refusal return and its `decision:` marker), `pkg/block/journal/handle.go:210` (`Handle.WriteAt`), its engine-side caller, and RFC 1 §6 from `origin/develop`.
- Modify: `pkg/block/journal/internal/datapath/write.go` (the error return(s) after the version was drawn); the `decision:` marker's overturn clause.
- Test: `pkg/block/journal/internal/datapath/write_room_test.go` if it exists, else the file the existing room-only-grant tests live in — `rg -l 'Roll\{\}|pool.Take\(Content\)' pkg/block/journal/internal/datapath/*_test.go` finds it.

**Interfaces:**
- Consumes: `Core.WriteAt`'s `(core.Version, error)` — no signature change on this fix; `Core.WriteAt` keeps returning `(v, done, err)` with `v` now populated on the refusal paths where a version was drawn.
- Produces: a version-on-error contract for `Handle.WriteAt`'s consumers.

- [ ] **Step 1: The audit (abort-able task).** Read RFC 1 §6 and every consumer of `Handle.WriteAt`'s version. If any consumer applies the version as success on `err != nil` and would corrupt its state with a partial write — record the finding, skip to Task 4, and reopen #3950 as a follow-up issue instead of changing the contract. If RFC 1 §6 forbids a version on a refused write outright, same abort.
- [ ] **Step 2: Write the failing test.** In the datapath rig, spend the room a grant covers only by its stream's room (idle sealer, a re-append, or a same-stream writer holding spares — use whichever the existing room tests already stage), exhaust the pool's spares for the roll (a `holdFault` wrapper), and send the multi-piece write: assert `WriteAt` returns the pool's named refusal AND a non-zero version AND the pieces already published.
- [ ] **Step 3: Run it red** — today the version comes back zero on this path.
- [ ] **Step 4: Implement.** On the refusal paths in `Core.WriteAt` where the version `v` was already drawn, return `v, done, err` instead of the zero version. Update the `decision:` marker: the overturn clause ("returning the version with the error") is now applied — reword the rest of the marker to describe only what remains open (an ENOSPC'd spare the pool cannot make, per the marker's own second ceiling).
- [ ] **Step 5: Re-run; revert-verify.** Test PASSes; revert the return change, it fails again. Also run the journaltest model/conformance around `WriteAt` errors (`rg -l 'WriteAt' pkg/block/journal/journaltest/`) for a contract assertion on error+version.
- [ ] **Step 6: Packages + suite.** `go test ./pkg/block/journal/...` and `dt run unit`.
- [ ] **Step 7: Commit.** `fix(journal): a refused unreserved write returns the version it drew [#3950]`

### Task 4: evidence-based closure of #3953 and #3990 (and the issue-closing pass)

**Files:**
- No code, unless verification finds a gap (then the smallest fix rides this PR as its own `fix(journal): … [#N]` commit).

- [ ] **Step 1: Verify #3990 completely.** Diff `git show a32d5c147 -- pkg/block/journal/internal/capacity/take_test.go` against today's file: every pre-#3992 rig test (the issue counts ~20) has a surviving counterpart, same assertion, now against real collaborators with fault wrappers. Spot-check `setHoldErr`-shaped cases became `holdFault` cases. Gaps → add the missing test as a `test(journal): the rig's missing <case> [#3990]` commit.
- [ ] **Step 2: Verify #3953 item by item.** Entry reservation (`index/bound.go` `writeAdds`/`checkWrite`), version-race counting (`heldVersions`, `reservation_refused` on `LimitVersion`), MaxInt64 overflow-safe sums (take.go:743, 1331), `checkWrite` duplication gone. Name the covering test or commit for each item, verifiable from the issue's four bullets.
- [ ] **Step 3: Draft the closing comments** (posted only after merge): #3990 cites PR #3992 commit `a32d5c147` with the surviving-assertions evidence; #3953 cites per-item evidence (commits `0fcf352d8`/`563f7f0ea`/`33cc23cf0` and successors); #3950/#3951/#3952 close by hand on the merged PR with one line each.
- [ ] **Step 4: PR and land.** Push the branch, `gh pr create` per `pr-opening` (base `feat/storage-v2`, the title in Global Constraints, assignee `didimelli`, stacked on nothing); the PR body lists each issue, the red/green evidence per test, and the design departure note from Task 2. Then the standard pipeline: reviews requested, replies then re-request, squash-merge, then hand-close all five issues with `state_reason: completed` and their drafted comments.

---

## Outcome

What was built, against the plan above. Each row is one commit on the branch.

| Task | Result |
|---|---|
| 1 — #3952 | Built as planned: the seal-without-roll retire test with a bounded-wait assertion, revert-proved against the seal's own release, and the two stale comments corrected. |
| 2 — #3951 | **Redesigned.** The refund bucket is a no-op (see the note under Task 2). Built instead: `Capacity.Landed(tag, n)`, which raises the tally without refusing, fed by durability's re-append and loss record through a `durability.Config.Landed` hook and called directly by the removal appender; the forget record is counted by a direct call like the removal record, and `Forget`'s settle of the whole tag to zero stays correct because it retires the record with the rest. A datapath test with a row per writer asserts the share figure equals the set's settled bytes minus the held repack copies, while the segment stands and after it retires. The invariant: the tally equals the set's per-tag settled bytes minus the held copies, apart from a lag that only overcounts. |
| 2a — #3951 review (added) | Review found two more undercounts, both fixed: a `Landed` or copy transfer running while the tag's seed reads the set was lost (the seed now marks the tag as seeding and adds what such calls took, never counting a record zero times), and a carried or deferred-loss record whose index note failed after it landed was missing from the pass's count. |
| 2b — #5100 (added) | Review of Task 2 found a fifth writer: a repack pass that fails after appending copies discarded them uncounted. The pass now counts a failed unit's partial copies before it abandons them. Tests cover the Carry-error and Copy-error branches, each asserting after the abandoned copies retire. |
| 3 — #3950 | **Not built; the audit said no.** RFC 1 does not forbid a version on a refused write, but the only consumer (`pkg/block/engine/write.go`) discards it and records nothing, and recording a partial write needs the published extent surfaced through `Handle.WriteAt` and `Reservation.WriteAt`, the engine interface, the model and the fakes: wider than one write path. The `decision:` marker in `datapath/write.go` now says so. #3950 stays open as the tracker. |
| 4 — #3953, #3990 | Both verified against the code. #3990 had landed whole in an earlier change. #3953 items 1 to 3 had landed but the exact scenarios had no test, so two were added; item 4 (duplicated entry prediction and refusal construction) is now one helper each in the index bound. |
| 5 — #5101 (added) | A flake the unit runs surfaced, fixed at the root: a test that cancelled a run on a fixed 300 ms timer. The other flake they surfaced, #5078 (a test that never closed its journals before its temp directory was removed), was fixed here too, then left out: the pool change in its own open pull request holds the same fix to that test, and carrying it twice would conflict. |

### Deferred

| Deferred | Why | Owner | Trigger |
|---|---|---|---|
| A refused unreserved write returns its drawn version and published extent (#3950) | Needs `WriteAt` to report the extent through `Handle` and `Reservation`, an engine way to record a partial write, and a model rule for a refused write that publishes pieces; wider than this change | unowned | An engine consumer that can record a partially published write |
