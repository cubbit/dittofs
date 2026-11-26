# Decision record: Codecov for a tests-first, rewrite-heavy workflow

**Issue:** cubbit/dittofs#5184 — *test: evaluate whether Codecov is useful for a
tests-first, rewrite-heavy workflow*

**Status:** recommendation, not yet trialled. The verdict is **reshape**, on one
condition the evidence makes unavoidable: the uploads must first reach a Codecov
repo that indexes them (§2). The reshape config is in §8; the trial that can
overturn or confirm it is in §9.

**Provenance:** measurements taken on 2026-11-26 against `origin/develop`
(`e0b3ecd1f`), the Codecov API, and the CI run of `develop@d219266ac`
(`tests.yml` run 45796112142). Package numbers come from that run's unit
`coverage.txt` artifact, not from Codecov.

---

## 1. The question, and the answer in one paragraph

Codecov is wired into CI but silent: `codecov.yml` turns off the project and
patch statuses, the PR comment and the annotations, because they fired on every
PR and nobody read them. The unit cell still uploads its profile. The question is
whether any of it earns its place in a workflow where tests land before their
code, components are rewritten next to their predecessors, and PRs measure only
the packages they changed.

**Reshape, don't drop and don't replace yet.** Codecov's only defensible job here
is a **per-component coverage trend, not a gate**: it is the cheapest way to
answer "is the v2 component as covered as the v1 it will delete", a question this
repository asks repeatedly (`feat/storage-v2` is live, and RFC 17 §9 migrates the
adapters onto the VFS service). Everything else the issue lists either does not
survive the workflow (project/patch statuses as gates), is already answered
better in-repo (packages with no tests), or is not a Codecov feature at all
(duplicate-test detection). One finding decides the sequence: **the dashboard is
not merely silent, it is empty** — today's uploads are accepted and never
indexed.

## 2. Finding 1: the upload succeeds, the coverage never arrives

This is the finding the issue did not anticipate, and it changes what "keep the
history" means.

The step logs `Upload queued for processing complete`, but the URL it prints is

```
https://app.codecov.io/github/marmos91/dittofs/commit/d219266ac9aa614d
```

not `cubbit/dittofs`. The Codecov API confirms the split:

| Query | Result on 2026-11-26 |
| --- | --- |
| `cubbit/dittofs` | `activated: false`, `active: false`, 0 commits, 0 flags, 0 pulls |
| `marmos91/dittofs` | active, 65 commits on `develop`, **one** flag (`unittests`), last commit **2026-11-04** |
| today's commit (`d219266ac`) on `marmos91/dittofs` | `No Commit matches the given query` |
| branches on `marmos91/dittofs` | includes `chore/move-to-cubbit-org`, last updated 2026-11-04 |

The repository moved to the `cubbit` organisation on 2026-11-04. Since then the
action still authenticates against the old repo slug, the upload is accepted
asynchronously, and no commit is indexed. The only data that exists is a
**22-day-old, pre-migration snapshot** whose history covers nine days
(2026-10-27 to 2026-11-04): 1,127 files, 110,976 lines, **54.34%**, one flag, no
components. Every develop run since has uploaded 2.4 MB into a void.

Consequences for the issue:

- "The history keeps growing" is false. There is no post-migration history to
  keep.
- Any trial must first make the data land (§8.2), then starts from zero; the stale
  snapshot is only useful as a sanity check on aggregation.
- The upload itself is healthy: 4.3 s for a 2.4 MB profile, `continue-on-error:
  true`, on a 20.9-minute unit cell — 0.3% of that cell.
- The `decision:` header in `codecov.yml` ("the coverage history is there") is
  false today and stays false under any verdict; the config change that fixes
  ingestion, or the drop, corrects it in the same PR.

**Ruling:** the recommendation cannot be "keep as-is", because as-is delivers
nothing. The choice is fix-and-reshape, drop, or replace with `dt`.

## 3. Finding 2: which decisions the data would inform

The issue names three. Measured against the repository:

**(a) Is v2's coverage as good as v1's before v1 is deleted? — yes, this is a
live decision.** `feat/storage-v2` replaces `pkg/block`, `pkg/metadata` and the
storage engine with `pkg/engine`, `pkg/journal`, `pkg/vfs`, `pkg/openstate` and
friends. The paths differ, so a path-based component map tells v1 from v2
without any per-branch bookkeeping. This is the one decision worth a dashboard,
and it recurs with every component cutover.

**(b) Which packages have no tests? — already answered, and already gated.** The
newest unit profile names 183 packages; `go list ./...` minus those leaves
**seven**, and none is an untested package with coverable statements:
`internal/bench`, `pkg/auth` and `pkg/metadata/store/metabench` have tests but
only type or benchmark files, so `go test` reports "[no statements]";
`bench/workloads` is an embed, `portmap/types` constants, `api/dto` structs, and
`scripts` a build-time command. `test/new-package-tests.sh` fails a PR that adds
a package without a test, and `test/required-tests.sh` pins named authorisation
and encryption gates. Codecov adds nothing here.

**(c) Did a deletion take its tests with it? — partially covered.** The two
scripts above catch a new package with no test and a deleted named gate test, but
not "this rewrite removed a test file along with the code it guarded". A
per-component line that steps down is a hint, not a proof, and it only exists if
the component is mapped and the data lands. Keep this as a secondary use, not a
reason on its own.

## 4. Finding 3: unit coverage undercounts exactly what the adapters need

From the same run's profile (unit cell, `-race`, without `-short`: this is the
**merge** run with the full package set; the PR tier adds `-short`):

| Measurement group (package prefix) | Unit coverage | Statements |
| --- | --- | --- |
| `pkg/block` (v1, being replaced) | 79.68% | 5,921 / 7,431 |
| `pkg/metadata` (v1) | 51.49% | 8,361 / 16,237 |
| `pkg/controlplane` | 53.29% | 3,794 / 7,120 |
| `internal/adapter/nfs` | 64.05% | 9,815 / 15,325 |
| `internal/adapter/smb` | 69.65% | 10,169 / 14,601 |
| `pkg/adapter` | 54.87% | 1,385 / 2,524 |
| `cmd` | 36.73% | 2,286 / 6,223 |
| **whole unit profile** | **59.71%** | **52,172 / 87,374** |

The adapter averages hide the shape that matters. In unit, `nlm` is **0%**
(client bookkeeping no unit test reaches), and the top-level `nfs/v3` dispatch is
**1.3%** (2/151) — but its handler package is 51.1%. Beside it:
`nlm/handlers` 54.3%, `mount/handlers` 33.9%, `v41/handlers` 13.7%, against SMB
handlers at 71.3% and NFSv4 state at 88.1%. The **handler** packages are the
protocol paths a kernel client drives, and unit leaves the weaker ones half
covered or worse; the e2e suite, which exercises them, uploads nothing. A
unit-only number therefore says little about the code a rewrite of the adapters
risks, which is the issue's own point, now measured. (These groups are the
package prefixes measured; §8's components split `pkg/adapter/nfs|smb` to follow
the same boundary, so the two lists are not one-to-one.)

## 5. Finding 4: Test Analytics is redundant here

Codecov Test Analytics ingests JUnit XML and reports per-test duration, failure
rate, failed tests with stack traces, and flaky-test detection. `dt` already
provides all of it from its own results: `dt report` (text/md/json) gives the
run's tests and grades, `dt history` gives pass/fail/skip/flaky counts with p50
and p90 durations per suite and per test, and `quarantine.yaml` + `dt doctor`
make a flake an owned, expiring entry rather than a statistic. Uploading a second
copy adds a credential, a schema conversion in `dt`, and a second place where
"flaky" means something different from RFC 29 §4.7. **Verdict: not adopted.**

## 6. Finding 5: duplicate tests are not detectable from what we upload

Go's `-coverprofile` is aggregate: every test's hits are unioned into one
profile per run, with no test-level attribution. Codecov receives that union, so
no Codecov feature can say which test covered which line, and "Test B adds
nothing" cannot be computed there. A `dt`-side experiment could: run a suite, run
it again with one test skipped, diff the profiles — but that is O(tests) suite
runs, far outside a tier budget, and it detects *coverage* redundancy, not
*assertion* redundancy. **Verdict: not a Codecov question; not in this issue's
scope.** Recorded as deferred work in §12.

## 7. Cost

| Item | Measured / estimated | Gate |
| --- | --- | --- |
| Unit upload | 4.3 s, 2.4 MB, on a 20.9 min cell | passes; keep |
| Plan limits | public repo, free Codecov tier; no seat or private-repo cost | passes |
| Integration `-coverprofile` | integration cell 3.4 min baseline; profiling re-instruments and recompiles the packages under test (separate build-cache keys), so cost is **estimated** at seconds to ~1 min, not the baseline | measure with the flag in the trial |
| e2e `go build -cover` | instrumented build **14.9 s** vs 0.74 s cached normal build, this host; binary 78.3 MB vs 76.3 MB | passes alone |
| e2e runtime instrumentation | e2e cell **13.1 min**; rule-of-thumb coverage overhead is 5–15% CPU (no benchmark source), and e2e is dominated by mount/setup/IO, so **≈0.7–2.0 min estimated** | borderline: measure in the trial; if > 2–3 min, nightly-only or drop |
| `GOCOVERDIR` collection | mechanism verified: built `dfs -cover`, ran a command, `go tool covdata percent` read the data; 581 KB meta per run | passes |

The e2e number is an estimate by design: the issue's cost question is answered
for unit and integration, and e2e is the only unknown. The trial measures it
with/without on the same runner tier before the e2e flag is made permanent.

## 8. The reshape, concretely

Two changes: make the data land, and make the data mean something per component.

### 8.1 `codecov.yml` (proposed)

```yaml
# decision: a per-component trend dashboard, not a gate. Tests land before the
# code they guard and components are replaced whole, so a PR-side percentage is
# frequently a known failure becoming a test, not a regression. Overturn the
# no-gate half if a stabilised release needs an enforced component floor; see
# .planning/2026-11-26-codecov-evaluation.md §11.
coverage:
  precision: 2
  round: down
  range: "50...80"

  status:
    project: off                # repo-wide % averages v1 next to v2: no signal
    patch:
      default:
        informational: true     # visible if a PR ever uploads; never blocking
        target: auto
        threshold: 1%

flags:
  unittests:
    carryforward: true          # reuses the last flag upload when a run skips it
  integration:
    carryforward: true          # e.g. the flag is absent from a PR-tier run
  e2e:
    carryforward: true

component_management:
  individual_components:
    # v2 components, by path, so a rewrite is measured next to its predecessor
    - component_id: engine
      name: engine (v2)
      paths: [pkg/engine/**, pkg/journal/**]
    - component_id: vfs
      name: vfs (v2)
      paths: [pkg/vfs/**, internal/vfs/**]
    - component_id: openstate
      name: open state (v2)
      paths: [pkg/openstate/**]
    # shared and v1 components, so their lines are not averaged into v2's
    - component_id: metadata
      name: metadata
      paths: [pkg/metadata/**]
    - component_id: block-v1
      name: block (v1, to be deleted)
      paths: [pkg/block/**]
    - component_id: adapters-nfs
      name: adapters/nfs
      paths: [internal/adapter/nfs/**, pkg/adapter/nfs/**]
    - component_id: adapters-smb
      name: adapters/smb
      paths: [internal/adapter/smb/**, pkg/adapter/smb/**]
    - component_id: controlplane
      name: control plane
      paths: [pkg/controlplane/**, pkg/node/**, pkg/settings/**]

comment: false                  # no PR upload exists to comment on; see §9

github_checks:
  annotations: false

ignore:
  - "cmd/**"                    # CLI wiring; the commands' tests are the gates
  - "test/**"
  - "docs/**"
  - "bench/**"
  - "scripts/**"
  - "k8s/**"
  - "**/*.pb.go"
  - "**/zz_generated*"
```

Deliberate omissions:

- **No per-component status now.** A component's floor applies only once it is
  *done* — known failures gone — which is not a Codecov concept. The card is
  informational until then; a done component gets an enforced rule in a follow-up
  change, recorded in the issue, not silently in this file.
- **`ignore` for v1 is `block-v1` as its own component, not an ignore entry.**
  Its line falling to zero on deletion is evidence the deletion happened; hiding
  it would discard the second use in §3(c).
- **`develop`, `main` and `feat/storage-v2` stay in the same flags.** Components
  are path-based, so the v2 branch's numbers land on the v2 components regardless
  of branch; per-branch trend filtering is a dashboard query, not a config split.

### 8.2 CI changes

1. **Fix ingestion (prerequisite).** Activate `cubbit/dittofs` on Codecov and
   update `CODECOV_TOKEN`; confirm the upload log prints the `cubbit` slug and the
   commit appears in the API. Without this the rest is unobservable.
2. **Integration upload.** `test/suites.yaml` integration `cmd` gains
   `-coverprofile={out}/coverage.txt`; a Codecov step uploads it with
   `flags: integration` on non-PR runs. Its retry guard must name the integration
   cell — `hashFiles('test/harness/results/*/integration/go-test-*-retry-*.json') == ''`
   — not copy the unit step's `unit` path; the matrix name there is `integration`
   and its options (`ad-dc`, `kerberos`, `portmap`) write their own cell
   directories.
3. **e2e upload (conditional).** `dt` builds the fixture's `dfs`; the e2e cell
   builds it with `-cover` and sets `GOCOVERDIR` for the server, then
   `go tool covdata textfmt` writes `{out}/coverage.txt`, uploaded with
   `flags: e2e`. Landed only if the trial's measurement keeps the cell within
   ~2–3 min of its 13.1 min baseline; otherwise the flag uploads from nightly
   alone. This is the only `dt` code in the reshape.
4. Unit step unchanged except its condition: keep skipping PRs and retries. Patch
   coverage is informational and PR uploads stay out, so no partial profile is
   ever read as a drop.

## 9. Trial (what makes this a recommendation rather than a guess)

Run for **one week of `develop` pushes and at least five PRs** after ingestion is
fixed. PR-bound signals are deliberately out of scope: the upload step excludes
`pull_request` (and a changed-packages-only profile would misread as a drop), so
Codecov has no PR upload from which to produce a patch status or a comment. The
trial measures what the reshape can actually deliver:

- Data lands for `cubbit/dittofs`: today's commit appears in the API, and the
  `unittests` flag updates on each develop push.
- Use the component dashboard at least once for decision (a): compare a v2
  component against the v1 path it replaces and record whether the comparison
  changed a cutover decision.
- Record any signal on a non-PR run (merge queue), and whether it was actionable
  or noise; zero is an acceptable count, since none is configured to fire.
- Measure the e2e cell with and without `-cover` on the same runner tier.

**Pass:** data lands; the component view answers (a) without manual profile
aggregation and is used in at least one decision; e2e overhead ≤ 3 min (else the
e2e flag moves to nightly). **Fail any of these → drop**, with the `dt` floors in
§10 as the alternative if decision (a) still needs a number.

PR-side coverage signals return only if full, non-partial PR uploads become
affordable; that is deferred in §12, not assumed here. The `comment: false` in
§8.1 matches this: there is no PR upload to comment on, and a comment on a
merge-queue run would be noise.

## 10. The alternatives, compared

| | Drop | Reshape (chosen) | Replace with `dt` |
| --- | --- | --- | --- |
| Work | delete step + `codecov.yml`, document the `dt` recipe | fix token/slug, config, integration upload, conditional e2e | per-package floors in `test/suites.yaml`, a `dt` coverage check over its own profile, trend in `dt history`; profile-diff for duplicates |
| Cost | ~zero | ~1 day config + 1–2 days e2e collection (estimates) | new Go in the harness (Go 1.26), floors that must not fire while a component is half-built, and a trend view that does not exist in `dt history` today; an estimate of 1–2 weeks |
| Keeps | nothing | per-component trend, third party, no repo code | everything in-repo, one tool, one registry (RFC 29's shape) |
| Loses | the only cheap answer to (a) | a dependency and a second place coverage is defined | the cheapest route to (a) now; duplicates still need the O(tests) scan |
| When it wins | the trial fails | decision (a) stays live through the rewrites | the team wants coverage defined beside the suites and is willing to build it |

`dt` is the right long-term home if coverage becomes a *gate*: RFC 29 already
owns suites, tiers and results, and a floor declared next to a suite is easier to
review than YAML in a third-party dashboard. It is the wrong first move here
because building a trend view and a failure-aware floor is weeks of harness work
for a question a config change can answer this month — and the trial is what
tells us whether the question is still being asked.

## 11. What would overturn this verdict

- **Ingestion cannot be fixed** (token/org constraints): drop, because a
  dashboard that receives nothing is not a dashboard.
- **The trial produces only noise**: more than two non-actionable PR signals, or
  no cutover decision uses the component view in a week. Drop and revisit `dt`.
- **e2e overhead exceeds ~3 minutes**: keep the reshape for unit+integration but
  make e2e nightly-only; if even nightly is too slow, drop the e2e flag and note
  that adapter numbers stay a lower bound.
- **A release needs an enforced floor**: move the floor to `dt` (§10), leaving
  Codecov as trend only — the "replace" option becomes the gate and Codecov the
  history.

## 12. Deferred

| Deferred | Why | Owner | Trigger |
| --- | --- | --- | --- |
| Per-package coverage floors in `test/suites.yaml`, checked by `dt` | the reshape keeps Codecov as a trend; a floor is a gate, and a gate during a rewrite fires on known failures | unowned | the trial fails, or a release needs an enforced floor |
| Duplicate-test detection via a `dt` profile-diff scan | O(tests) suite runs; detects coverage redundancy, not assertion redundancy; Codecov cannot do it | unowned | someone asks "which tests are redundant" with a budget for the runs |
| Per-component enforced status once a v2 component is done | a Codecov component has no "done" state; flipping it early recreates the noise the issue is about | unowned | the component's known failures are gone and the trial passed |
| PR-side coverage signals (patch status, comment) | the upload step excludes pull requests, and a changed-packages-only profile would read as a drop | unowned | full, non-partial PR uploads become affordable |

Test Analytics is declined, not deferred: §5 records why it adds nothing over
`dt history` and quarantine.
