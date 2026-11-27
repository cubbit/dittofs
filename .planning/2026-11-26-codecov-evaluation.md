# Decision record: Codecov for a tests-first, rewrite-heavy workflow

**Issue:** cubbit/dittofs#5184 — *test: evaluate whether Codecov is useful for a
tests-first, rewrite-heavy workflow*

**Status:** recommendation, not yet trialled. The verdict is **reshape**, on one
condition the evidence makes unavoidable: the uploads must first reach a Codecov
repo that indexes them (§2). The reshape config is in §8; the trial that can
overturn or confirm it is in §9.

**Provenance:** measurements taken on 2026-11-26 against the CI run of
`develop@d219266ac` (`tests.yml` run 45796112142; its unit artifact supplies the
coverage profile, and every tree scan in this document runs at that same commit,
per the appendix — not at the current develop tip), the Codecov API, and a peek
at `origin/feat/storage-v2` (`b6affb589`) for the v2 package layout.

---

## 1. The question, and the answer in one paragraph

Codecov is wired into CI but silent: `codecov.yml` turns off the project and
patch statuses, the PR comment and the annotations, because they fired on every
PR and nobody read them. The unit cell still uploads its profile. The question is
whether any of it earns its place in a workflow where tests land before their
code, components are rewritten next to their predecessors, and PRs measure only
the packages they changed.

**Reshape, don't drop and don't replace yet.** Codecov keeps two jobs, neither
a gate:

1. **A per-component team metric.** A weekly scheduled run of the whole suite
   with coverage (unit, integration, the tagged suites, e2e and conformance via
   a `-cover` `dfs`) is the **one** data source behind the component cards, so
   the trend compares components with each other and cannot jump between full
   and partial runs. The v1-vs-v2 cutover comparison of §3(a) stays available as
   one reading of those cards, but it is no longer the load-bearing reason: v1
   and v2 differ too much for one percentage to settle a cutover by itself.
2. **An informational patch status on every non-docs PR**: which changed lines
   no test executes. It gets its data from PR-side uploads of the changed
   packages **plus the tagged suites** — without the tagged suites a family
   whose coverage comes from `engine-conformance` would read as uncovered when
   its code lands, which is exactly the noise #3483 silenced.

Everything else the issue lists either does not survive the workflow (project
status as a gate, PR comments), is already answered better in-repo (packages
with no tests), or is not a Codecov feature at all (duplicate-test detection).
One finding decides the sequence: **the dashboard is not merely silent, it is
empty** — today's uploads are accepted and never indexed.

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
components. Every develop run since has uploaded a 4.6 MB profile (2.4 MB
compressed) into a void.

Consequences for the issue:

- "The history keeps growing" is false. There is no post-migration history to
  keep.
- Any trial must first make the data land (§8.2), then starts from zero; the stale
  snapshot is only useful as a sanity check on aggregation.
- The upload itself is healthy: 4.3 s for a 4.6 MB profile (2.4 MB compressed
  on the wire), `continue-on-error: true`, on a 20.9-minute unit cell — 0.3% of
  that cell.
- The `decision:` header in `codecov.yml` ("the coverage history is there") is
  false today and stays false under any verdict; the config change that fixes
  ingestion, or the drop, corrects it in the same PR.

**Ruling:** the recommendation cannot be "keep as-is", because as-is delivers
nothing. The choice is fix-and-reshape, drop, or replace with `dt`.

## 3. Finding 2: which decisions the data would inform

The issue names three. Measured against the repository:

**(a) Is v2's coverage as good as v1's before v1 is deleted? — a live question,
but a secondary read, not the verdict's load-bearing reason.** v1 and v2 never
share a branch, so (a) is always a develop line against a `feat/storage-v2`
line, and the two versions differ too much for one percentage to settle a
cutover by itself. Where a component moved, the paths tell
it apart: v2 `pkg/engine`, `pkg/journal`, `pkg/remote`, `pkg/syncer`,
`pkg/carver`, `pkg/transform`, `pkg/workqueue` replace v1's `pkg/block` tree, and
the control plane moved the same way (`pkg/controlplane`/`internal/controlplane`
against `pkg/node`/`pkg/settings`/`internal/node`). `pkg/metadata` is the one
rewritten in place, so its v1 and v2 halves split by subdirectory
(`store`/`lock`/`acl` plus the root service package against `entity`/`kv`/
`kvstore`). §8.1 encodes that split; on the dashboard the two branches' cards
are read side by side as one input among several to a cutover, next to how each
component compares with its peers — the primary use the cards exist for.

**(b) Which packages have no tests? — measurable, but not by subtracting.** At the
profile's commit, **32 directories under `pkg/`, `internal/` and `cmd/` hold Go
files and no `_test.go`**: cmd wiring (17: 15 `cmd/dfsctl` directories
including the root command, plus `cmd/dfs` and `cmd/bench`; several `dfsctl`
directories do carry tests), the
adapters' wire-format and type helpers (`nlm`'s dispatch, `nlm/types`,
`nsm/types`, `nsm/xdr`, `portmap/sysreg`, `portmap/types`, `xdr/core`,
`v3/handlers/testing`), `internal/cli/prompt`, `internal/cli/timeutil`,
`internal/dfsbench/config`, `pkg/block/blockstoretest`, `pkg/block/remote`,
`pkg/controlplane/api/dto` and `pkg/metadata/store/internal/sharecache`. The
subtraction `go list` minus profile is the wrong tool: since Go 1.22 the profile
lists test-less packages at 0% (`nlm` shows up that way), so the difference only
ever finds packages with no coverable statements. `test/new-package-tests.sh`
guards only directories **new against the base**, so these predate the guard and
nothing catches them. A one-line `git ls-tree` scan answers the question
(appendix), so Codecov still adds nothing here — but the guard's gap is recorded
in §12.

**(c) Did a deletion take its tests with it? — partially covered.**
`test/new-package-tests.sh` catches a new package with no test and
`test/required-tests.sh` catches the deletion of a named gate test, but neither
catches "this rewrite removed a test file along with the code it guarded". A
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
(that package is `dispatch.go` alone, with no test file), and the top-level
`nfs/v3` dispatch is **1.3%** (2/151) — but its handler package is 51.1%. Beside
it: `nlm/handlers` 54.3%, `mount/handlers` 33.9%, `v41/handlers` 13.7%, against
SMB handlers at 71.3% and NFSv4 state at 88.1%. The **handler** packages are the
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
make a flake an owned, expiring entry rather than a statistic. The upload itself
is nearly free — `dt report` already writes a JUnit XML per cell, and the same
`CODECOV_TOKEN` carries it — so the cost is not plumbing but a second place where
"flaky" means something different from RFC 29 §4.7, with no question a developer
asks that `dt history` and the quarantine do not already answer. **Verdict: not
adopted.**

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
| Unit upload | 4.3 s, 2.4 MB sent (4.6 MB profile), on a 20.9 min cell | passes; keep |
| Plan limits | public repo, free Codecov tier; no seat or private-repo cost | passes |
| Integration `-coverprofile` | integration cell 3.4 min baseline; profiling re-instruments and recompiles the packages under test (separate build-cache keys), so cost is **estimated** at seconds to ~1 min, not the baseline; the flag is tier-scoped (§8.2), so the profile is only written on merge and nightly runs — measure those | write the flag at all, then measure in the trial |
| e2e `go build -cover` | binary 78.3 MB vs 76.3 MB; build cost measured like for like on this host: warm **+0.06 s** (0.34 vs 0.28), cold **+2.9 s** (70.5 vs 67.6), separate GOCACHEs | passes: ~3 s of the build, once per cell |
| e2e runtime instrumentation | e2e cell **13.1 min**; rule-of-thumb coverage overhead is 5–15% CPU (no benchmark source), and e2e is dominated by mount/setup/IO, so **≈0.7–2.0 min estimated** | borderline: measure in the trial; if > 2–3 min, nightly-only or drop |
| `GOCOVERDIR` collection | mechanism verified: built `dfs -cover`, ran a command, `go tool covdata percent` read the data; 581 KB meta per run | passes |

The e2e number is an estimate by design: the issue's cost question is answered
for unit alone; integration and e2e are estimates the trial measures on the same
runner tier before any of it is made permanent.

## 8. The reshape, concretely

Two changes: make the data land, and make the data mean something per component.

### 8.1 `codecov.yml` (proposed)

```yaml
# decision: a per-component trend dashboard, not a gate. Tests land before the
# code they guard and components are replaced whole, so a percentage judged
# against a commit is frequently a known failure becoming a test, not a
# regression. Enforce a component floor only once a stabilised release needs
# one, and only on a component whose known failures are gone.
coverage:
  precision: 2
  round: down
  range: "50...80"

  status:
    project: off                # a repo-wide % hides per-component moves, and a
                                # test landing before its code reads as a drop
    patch:
      default:
        informational: true     # changed lines no test executes, per PR;
                                # shown, never blocking

flags:
  full:                         # the weekly scheduled run of the whole suite
    carryforward: true          # set — the cards' one data source, so
                                # per-push partial runs cannot make the trend
                                # jump between full and partial
  pull:                         # a PR's changed packages + tagged suites, for
    carryforward: true          # patch coverage alone

component_management:
  individual_components:
    # v1 cards, populated by develop's runs
    - component_id: block-v1
      name: block (v1)
      paths: [pkg/block/**]
    # the fine-grained v1 cards overlap block-v1 and mirror the v2 cards'
    # grouping, so decision (a) is one number against one number
    - component_id: block-engine-v1
      name: engine + journal (v1)
      paths: [pkg/block/engine/**, pkg/block/journal/**]
    - component_id: block-remote-v1
      name: remote tier + syncer (v1)
      paths: [pkg/block/remote/**, pkg/block/syncer/**]
    - component_id: block-carver-v1
      name: carver (v1)
      paths: [pkg/block/carver/**]
    - component_id: metadata-v1
      name: metadata (v1)
      paths: [pkg/metadata/*.go, pkg/metadata/store/**, pkg/metadata/lock/**,
              pkg/metadata/acl/**, pkg/metadata/backup/**,
              pkg/metadata/storetest/**, pkg/metadata/errors/**]
    - component_id: controlplane-v1
      name: control plane (v1)
      paths: [pkg/controlplane/**, internal/controlplane/**]
    # v2 cards, populated by feat/storage-v2's runs
    - component_id: engine-v2
      name: engine + journal (v2)
      paths: [pkg/engine/**, pkg/journal/**]
    - component_id: remote-v2
      name: remote tier + syncer (v2)
      paths: [pkg/remote/**, pkg/syncer/**]
    - component_id: carver-v2
      name: carver (v2)
      paths: [pkg/carver/**]
    - component_id: transform-v2
      name: transform + workqueue (v2)
      paths: [pkg/transform/**, pkg/workqueue/**]
    - component_id: vfs-v2
      name: vfs (v2)
      paths: [pkg/vfs/**, internal/vfs/**]
    - component_id: openstate-v2
      name: open state (v2)
      paths: [pkg/openstate/**]
    - component_id: metadata-v2
      name: metadata (v2)
      paths: [pkg/metadata/entity/**, pkg/metadata/kv/**, pkg/metadata/kvstore/**,
              pkg/metadata/errors/**]   # errors is the shared leaf: on both cards
    - component_id: controlplane-v2
      name: control plane (v2)
      paths: [pkg/node/**, pkg/settings/**, internal/node/**]
    - component_id: setup-v2
      name: bootstrap + keys (v2)
      paths: [pkg/bootstrap/**, pkg/secrets/**, pkg/keyprovider/**]
    # shared paths while the migration runs: pkg/identity and the adapters
    # exist on both branches; internal/identity and internal/adapter/core
    # are v2-only and live on the card below. Shared cards are read per branch.
    - component_id: identity
      name: identity (shared)
      paths: [pkg/identity/**]
    - component_id: identity-v2
      name: identity + adapter core (v2)
      paths: [internal/identity/**, internal/adapter/core/**]
    - component_id: adapters-nfs
      name: adapters/nfs
      paths: [internal/adapter/nfs/**, pkg/adapter/nfs/**]
    - component_id: adapters-smb
      name: adapters/smb
      paths: [internal/adapter/smb/**, pkg/adapter/smb/**]
    # deliberate overlap with the two protocol cards: this card carries the
    # adapter tree neither of them names (v1 sidecar, v2 settings, gss,
    # adaptertest, common, pool, testruntime, internal/sidecar)
    - component_id: adapters-plumbing
      name: adapters + plumbing
      paths: [internal/adapter/**, pkg/adapter/**, internal/sidecar/**]

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
- **v1 stays a component rather than an ignore entry, so its deletion is
  visible.** Note the rendering: once `pkg/block/**` is gone no files match, and
  a component with no files shows as empty or no-data rather than 0%, while
  `carryforward` can keep stale figures on a card. The deletion is read as the
  card emptying (and from the commit that removes the package), not as a step to
  zero; verify the rendering during the trial before relying on it.
- **`develop` and `feat/storage-v2` write to different cards, one set of
  flags.** The v1 cards are populated by develop's weekly run and the v2 cards
  by `feat/storage-v2`'s, because v1 and v2 never share a branch; the shared cards
  (adapters, identity, `pkg/metadata/errors`) carry both branches' paths and are
  read per branch. Decision (a) is one card against the other, with per-branch
  filtering a dashboard query, not a config split.
- **Every card reads the same `full` flag.** The weekly run is the cards' one
  data source on both branches, so no card ever merges a partial or one-sided
  flag set — the asymmetry that would have made a v1 card read higher than its
  v2 counterpart cannot arise by configuration.

### 8.2 CI changes

1. **Fix ingestion (prerequisite), on both branches.** Activate
   `cubbit/dittofs` on Codecov and update `CODECOV_TOKEN`; confirm the upload log
   prints the `cubbit` slug and the commit appears in the API. `feat/storage-v2`
   carries its **own** `codecov.yml` and its own upload step, and the v2 half of
   decision (a) comes only from that branch's runs — so the same activation,
   token and reshaped config must land there too, or the trial has nothing v2 to
   compare against. That branch's upload step lives in a dedicated `unit:` job
   (no matrix, unlike develop's), so its condition needs no `matrix.name` guard
   and reads `env.TIER != 'pr' && hashFiles('…/unit/go-test-*-retry-*.json') == ''
   && github.ref == 'refs/heads/feat/storage-v2'`. Without this the rest is
   unobservable.
2. **The weekly full-coverage run is the cards' one upload.** A new scheduled
   workflow (weekly, `develop` and `feat/storage-v2`) runs the whole set with
   coverage — unit, integration with tier-scoped
   `args: { merge: "-coverprofile={out}/coverage.txt", nightly: same }`
   (the mechanism unit uses for `-short`), the tagged suites, e2e per item 3,
   and conformance against the `-cover` `dfs` — merges the profiles, and
   uploads once, `flags: full`. Per-push unit uploads stop feeding the cards:
   a changed-packages report between two full ones is exactly the jump the
   single data source exists to prevent. frequency and budget are the weekly
   run's own, not the merge path's, so the CI-time gate applies to it alone.
3. **e2e coverage runs in that weekly job, unconditionally on the critical
   path.** The plumbing goes through `dt`, not the workflow:
   - `buildBin` skips the build whenever `{bin}/dfs` already exists ("an earlier
     cell of this run built them"), so the `-cover` build is **keyed by name**:
     coverage builds write `dfs` to `{bin}/cover/dfs` (or `dfs.cover`). Anything
     else lets cell order decide which build every later cell runs, and a
     cross-cell run hands a `-cover` dfs to cells that never set `GOCOVERDIR`. A
     stamp, or the separate directory alone, is the check.
   - The suite's env cannot carry `GOCOVERDIR`: `Registry.Units` merges each
     option over the suite (`merge(base, o)`), so `e2e/kerberos` and
     `e2e/stress` inherit `Env.ByTier` too, and tiers are cumulative
     (`plan.RunsIn`) — a merge or nightly run would instrument the kerberos and
     stress cells for data no gate uploads. Instead the cover build, the
     `GOCOVERDIR` variable and the covdata step are **keyed to the base e2e
     cell alone** (the unit without an option); dt passes nothing coverage-
     related to the options. `GOCOVERDIR` points at a directory dt creates
     under `{out}` — the suite runs as root through the `sudo -n env …`
     wrapper, so a workflow-level variable never reaches the server, and a
     missing directory means the runtime writes nothing.
   - `go tool covdata textfmt` runs **as `dt` work after the base cell's
     `go test` commands** — the registry rejects a go-json suite command that
     does not begin with `go test` — writing `{out}/coverage.txt` into the
     weekly run's merged upload.
   - The fixture's teardown is the first thing the trial fixes: `ForceKill`
     waits **2 s** after SIGTERM before SIGKILL, and `StopGracefully` waits
     only **10 s** — both shorter than `dfs`'s default `shutdown_timeout`
     (30 s), which `StopGracefully` does not even enforce (it returns an
     error, and a later cleanup kills). So the trial's wait is raised to at
     least `shutdown_timeout` whenever `GOCOVERDIR` is set, or the e2e
     fixture's config sets a `shutdown_timeout` shorter than the wait — a
     `-cover` binary flushes its counters only on a clean exit, so a normal
     teardown losing the race loses the coverage, and an empty-looking
     `GOCOVERDIR` would be a teardown artifact, not a test result.
   Off the merge path, the weekly job's cost ceiling is its own runner budget;
   the trial still measures e2e with and without `-cover`, because a runaway
   cost makes even a weekly job unaffordable.
4. **PR-side uploads feed the patch status only.** On `pull_request`, upload
   the changed-packages profiles the unit cell already writes **plus the
   tagged suites that run on that PR** (a PR whose paths filter in
   `engine-conformance` carries its profile too — without it, code whose
   coverage comes from a tagged suite reads as uncovered when it lands, the
   noise #3483 silenced), `flags: pull`. Patch coverage reads only the changed
   lines, so a changed-packages report is sufficient data there; `carryforward`
   keeps the rest from reading as a drop. Nothing else comes to the PR: no
   comment (`comment: false`), no project status, one informational check.
   The push-side unit upload step is **replaced by the weekly run** — that
   retires the tier/ref-gate argument for the card feed entirely, though
   `feat/storage-v2`'s own upload config still follows its dedicated `unit:`
   job shape (no matrix, so no `matrix.name` guard).

## 9. Trial (what makes this a recommendation rather than a guess)

Run for **one week after ingestion is fixed**: two weekly full-coverage runs on
each branch, and every PR that merges in that week.

- Data lands for `cubbit/dittofs`: the weekly run appears in the API and the
  component cards populate once per week on each branch — one stable trend line,
  not a per-push zig-zag.
- Use the component dashboard at least once for a real decision: a cutover
  reads the v1 card against the v2 card alongside how each component compares
  with its peers. Record whether the view changed the decision.
- Tally the PR **patch** statuses, which now do fire (informational, from
  `flags: pull`): actionable (an uncovered changed line its author then covered,
  or that blocked review) versus noise (a known failure becoming a test whose profile the upload
  missed, v1 drift, a family the tagged suites should have carried). Comments and annotations still cannot fire.
- Measure the e2e portion of the weekly run with and without `-cover`, and
  confirm `GOCOVERDIR` holds data after a normal e2e teardown (§8.2, item 3,
  including the lengthened shutdown wait).

**Pass:** the weekly run lands on both branches; the component view is used
in at least one real decision; the patch statuses produce at least one
actionable signal and no more than two judged noise. **Fail any of those →
drop**, with the `dt` floors in §10 as the alternative if a coverage number is
still wanted. The weekly job's own cost is a budget question, not a
pass/fail: it replaces per-push uploads, so the CI-time gate applies to the
job's runner budget (§11's third bullet names the cut).

The PR-side upload is partial on purpose — patch coverage reads only the
changed lines, and `carryforward` fills the rest — but it feeds the patch
status alone. The `comment: false` in §8.1 keeps the heavier PR surface off:
one informational check, no comment thread, no annotations.

## 10. The alternatives, compared

| | Drop | Reshape (chosen) | Replace with `dt` |
| --- | --- | --- | --- |
| Work | delete step + `codecov.yml`, document the `dt` recipe | fix token/slug, config on both branches, a weekly full-coverage workflow, PR-side uploads, the dt e2e plumbing | per-package floors in `test/suites.yaml`, a `dt` coverage check over its own profile, trend in `dt history`; profile-diff for duplicates |
| Cost | ~zero | ~1 day config + 1–2 days e2e collection **in `dt`** (the name-keyed `-cover` build, the base-cell-only `GOCOVERDIR`, the teardown wait raised to `shutdown_timeout`, and the post-cell `covdata textfmt` step) — estimates | new Go in the harness (Go 1.26), floors that must not fire while a component is half-built, and a trend view that does not exist in `dt history` today; an estimate of 1–2 weeks |
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
- **The view is not used, or the patch statuses are noise**: no real
  decision consults the component view in a week, or the patch statuses produce
  no actionable signal and more than two noise ones. Either way Codecov has
  become again what #3483 silenced; drop and revisit `dt`.
- **The weekly run does not fit its budget**: keep the reshape for the
  components the cheaper profiles cover, and drop the e2e portion, noting that
  adapter numbers stay a lower bound; if even that is unaffordable, the trial's
  fail rule applies.
- **A release needs an enforced floor**: move the floor to `dt` (§10), leaving
  Codecov as trend only — the "replace" option becomes the gate and Codecov the
  history.

## 12. Deferred

| Deferred | Why | Owner | Trigger |
| --- | --- | --- | --- |
| Per-package coverage floors in `test/suites.yaml`, checked by `dt` | the reshape keeps Codecov as a trend; a floor is a gate, and a gate during a rewrite fires on known failures | unowned | the trial fails, or a release needs an enforced floor |
| Duplicate-test detection via a `dt` profile-diff scan | O(tests) suite runs; detects coverage redundancy, not assertion redundancy; Codecov cannot do it | unowned | someone asks "which tests are redundant" with a budget for the runs |
| Per-component enforced status once a v2 component is done | a Codecov component has no "done" state; flipping it early recreates the noise the issue is about | unowned | the component's known failures are gone and the trial passed |
| PR comments and annotations | one informational patch check is the whole PR surface; a comment or annotation is the noise #3483 silenced | unowned | reviewers ask for a per-PR summary the check cannot give |
| A `new-package-tests.sh` guard for pre-existing packages | the script checks only directories new against the base; 32 untested directories predate it (§3b) | unowned | a rewrite touches one of the 32, or a cleanup pass adds their tests |

Test Analytics is declined, not deferred: §5 records why it adds nothing over
`dt history` and quarantine.

## Appendix: reproducing the measurements

```bash
# Unit profile from the merge run cited in §4 (artifact dt-run-unit-1).
gh run download 45796112142 -R cubbit/dittofs -n dt-run-unit-1
# -> <timestamp>-<sha>/unit/coverage.txt
```

Per-package aggregation, with the prefix rollup and percentages that build the
§4 table (run the tree scan at the profile's own commit, `d219266ac`, not at the
current tip — the provenance line above names both):

```python
import collections, glob, os, sys
path = (sys.argv[1:] or glob.glob("*-*/unit/coverage.txt"))[0]
cov = collections.defaultdict(lambda: [0, 0])   # statements, hit
for line in open(path):
    if line.startswith("mode:") or not line.strip():
        continue
    f, rest = line.rsplit(":", 1)
    path = f.removeprefix("github.com/cubbit/dittofs/")   # trim the module prefix
    stmts, count = rest.split()[-2:]
    cov[os.path.dirname(path)][0] += int(stmts)
    if int(count) > 0:
        cov[os.path.dirname(path)][1] += int(stmts)

GROUPS = ["pkg/block", "pkg/metadata", "pkg/controlplane",
          "pkg/adapter", "internal/adapter/nfs", "internal/adapter/smb",
          "cmd"]
for g in GROUPS:
    t = h = 0
    for d, (st, hit) in cov.items():
        if d.startswith(g):
            t, h = t + st, h + hit
    if t == 0:                       # the group does not exist on this commit
        continue
    print(f"{100*h/t:6.2f}%  {h:6}/{t:<6} {g}")
t = sum(v[0] for v in cov.values()); h = sum(v[1] for v in cov.values())
print(f"TOTAL {100*h/t:.2f}% ({h}/{t}) over {len(cov)} packages")
```

The §3(b) count (directories with Go files and no `_test.go`, excluding
`k8s/`, `scripts/` and `bench/`; run at the same commit):

```bash
git ls-tree -r --name-only d219266ac | grep '\.go$' | grep -v '_test\.go$' \
  | sed 's![^/]*\.go$!!' | sort -u > /tmp/dirs
git ls-tree -r --name-only d219266ac | grep '_test\.go$' \
  | sed 's![^/]*_test\.go$!!' | sort -u > /tmp/testdirs
comm -23 /tmp/dirs /tmp/testdirs | grep '^\(pkg\|internal\|cmd\)/'
```


The dashboard state in §2:

```bash
curl -s https://api.codecov.io/api/v2/github/cubbit/repos/dittofs/
curl -s 'https://api.codecov.io/api/v2/github/marmos91/repos/dittofs/commits?branch=develop&page_size=100'
curl -s https://api.codecov.io/api/v2/github/marmos91/repos/dittofs/flags
```

The e2e cost in §7:

```bash
go build -cover -o /tmp/dfs-cover ./cmd/dfs
mkdir -p /tmp/cov && GOCOVERDIR=/tmp/cov /tmp/dfs-cover version
go tool covdata percent -i=/tmp/cov | head
```

The unit upload's own cost is in the unit job log: the Codecov step reports the
profile size and the time between its start and its `end-action` line.

