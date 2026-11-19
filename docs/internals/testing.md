# Testing DittoFS

Every DittoFS test suite, from `go vet` to the SMB conformance suites, is registered in one file,
[`test/suites.yaml`](../../test/suites.yaml), and run by one command, `test/harness/bin/dt`. The
command runs the same way on a laptop and in CI: it builds DittoFS from your checkout, starts what
the suite needs, runs it, grades the results against the suite's list of known failures, and
writes one report. This page is everything a contributor needs to run, read and add tests. The
design and its reasons are in [RFC 29](rfcs/rfc-29-test-harness.md).

## Contents

- [Quick start](#quick-start)
- [How the harness is organized](#how-the-harness-is-organized)
- [Setting up a machine](#setting-up-a-machine)
- [Running tests](#running-tests)
- [Results and reports](#results-and-reports)
- [Known failures, expected skips and quarantine](#known-failures-expected-skips-and-quarantine)
- [When a test fails](#when-a-test-fails)
- [CI](#ci)
- [Git hooks](#git-hooks)
- [Writing tests](#writing-tests)
- [Adding or changing a suite](#adding-or-changing-a-suite)
- [The Go suites: unit, integration, e2e, operator](#the-go-suites-unit-integration-e2e-operator)
- [Conformance test suites](#conformance-test-suites)
- [Performance and benchmarks](#performance-and-benchmarks)
- [Running DittoFS by hand](#running-dittofs-by-hand)
- [Real NFSv3 NLM lock testing (network-namespace isolation)](#real-nfsv3-nlm-lock-testing-network-namespace-isolation)
- [Command reference](#command-reference)
- [Troubleshooting](#troubleshooting)
- [Further reading](#further-reading)

## Quick start

You need Git, Go 1.26 (see [Go version](#go-version)) and, for most suites, Docker. From the
repository root:

```bash
test/harness/bin/dt doctor               # which needs this machine meets, and how to meet the rest
test/harness/bin/dt list --tier pr       # the suites a pull request runs; HERE says if this machine can run each
test/harness/bin/dt run lint-go          # gofmt, go vet, golangci-lint and the other Go checks
test/harness/bin/dt run unit --changed   # the unit tests of the packages your branch changed
test/harness/bin/dt report               # the last run again, with its new failures
```

To complete `dt`'s arguments with TAB, alias it once from the repository root, then load the
script. This is zsh's; bash's is `dt completion bash`, which needs the bash-completion package, and
cobra also writes one for fish and for powershell:

```bash
alias dt="$PWD/test/harness/bin/dt"
source <(dt completion zsh)
```

A suite that needs Linux runs from a Mac in dtc, the harness's Linux container:

```bash
test/harness/bin/dt services up localstack                     # the S3 endpoint the badger-s3 profile writes to
test/harness/bin/dt run pjdfstest/badger-s3/4.1 chmod/00.t     # dt moves the run into dtc by itself
```

To see what CI will run for your branch, `test/harness/bin/dt plan --tier pr --base origin/develop`.

## How the harness is organized

- **Suite:** one entry in `test/suites.yaml`: the command, the format its results come in, its
  tier, what it needs and the fixture it runs against. The table below lists each one.
- **Option:** a named variant of a suite, such as `unit/nonrace` or `smbtorture/kerberos`. Each
  option is also a flag of `dt run`: `dt run smbtorture --kerberos`.
- **Matrix:** the axes a suite runs across, such as the storage profile and the NFS version.
- **Shard:** a suite's tests dealt over N cells, so a long suite fits a runner's time budget.
- **Cell:** the unit dt runs, grades and reports. Its name is
  `SUITE[/OPTION][/matrix values][/shard-N]`, such as `pjdfstest/badger-s3/4.1` or
  `smbtorture/badger-s3/shard-2`.
- **Fixture:** what dt starts for a cell: a `dfs` built from your checkout with a storage profile
  (`memory`, `badger` or `badger-s3`, the last against Localstack), its shares and users, NFS and
  SMB mounts, a Kerberos KDC.
- **Tier:** when a suite runs: `pr` on every pull request, `merge` after a merge, `nightly` each
  night, `manual` only when asked. Each tier includes the ones before it.
- **Group:** a name for several suites: `lint`, `go`, `conformance` and `perf`.
- **Needs:** what a cell requires of the machine, such as `linux`, `root`, `nfs-client` or
  `service:localstack`. `dt doctor` checks each one and says how to meet it.
- **Place:** where CI runs a cell: a hosted `ubuntu`, `windows` or `macos` runner, or the
  `dedicated` host for what a hosted runner can't hold.
- **Known file:** a suite's list of known failures and expected skips, each with its reason.

### The suites

| Suite | What it tests | Tier | Runs on |
|---|---|---|---|
| `lint-go` | `go mod verify`, `gofmt -s`, `go vet` with every build tag, spec citations, the pinned tests, golangci-lint (the "Go Checks" job) | pr | any machine |
| `lint-repo` | `dt docs --check`, `dt doctor`, the harness's own tests, the commit checks (the "Repo Checks" job) | pr | any machine |
| `shellcheck` | every tracked shell script, at `--severity=warning` (the "ShellCheck" job) | pr | any machine |
| `unit` | `go test -race` over every package; `-short` on pull requests | pr | any machine; `unit/windows` on Windows, `unit/nonrace` nightly |
| `integration` | `-tags=integration` tests against real services | pr | Docker with Postgres and KMIP up; `integration/portmap` needs a Linux host with rpcbind |
| `operator` | the Kubernetes operator: `make test`, `make lint`, manifests, kustomize | pr | any machine with `make` and `kubectl` |
| `e2e` | `-tags=e2e` tests through real kernel NFS and SMB mounts, as root | merge | Linux, or dtc |
| `pjdfstest` | POSIX semantics over an NFS mount, on NFSv3, v4 and v4.1 | pr | Linux, or dtc |
| `pynfs` | NFSv4.0 and v4.1 protocol conformance | pr | Linux, or dtc |
| `wpts` | the SMB2 build verification tests of Microsoft's Windows Protocol Test Suites | pr | Linux with Docker, or dtc |
| `smbtorture` | SMB2 protocol torture tests, in two shards | pr | Linux with Docker, or dtc |
| `cthon04` | Connectathon over the kernel NFS client, once per mount-option set | pr | a Linux host only |
| `nfstest` | nfstest modules over the kernel NFS client, with two clients | nightly | a Linux host only |
| `clients` | file, rename and S3 checks over an SMB mount from real clients | merge | Linux; `clients/macos` on a Mac, `clients/windows` on Windows |
| `fio-verify` | fio jobs with data verification over an NFS or SMB mount | nightly | Linux, or dtc |
| `bench-smoke` | a short run of the benchmark tool, non-blocking | pr | any machine with `fio` |
| `system` | whole-system scenarios: DittoFS in a container, real clients, injected faults | merge | the dedicated host |
| `perf-night` | the nightly performance run | nightly | the dedicated host |
| `smb-fuzz` | an SMB request fuzzer that compares error codes with a reference server | manual | Linux with Docker |

`dt list` prints the same registry with each suite's format, needs, and a HERE column that says
whether this machine can run it, or why not.

## Setting up a machine

### Go version

`go.mod` pins the toolchain to `go1.26.10`, and CI, the dtc image and the harness's own tests all
follow it. Go's toolchain switching only ever moves up, so a newer local Go (1.27 from a package
manager, say) keeps running as 1.27. Install a Go 1.26 release, or run with
`GOTOOLCHAIN=go1.26.10`. A different release formats some code differently, and `gofmt` then fails
lint-go.

`test/harness/bin/dt` builds `dt` from your checkout with that Go each time you call it (a few
seconds when cold, instant after), so even a run that goes to dtc needs Go on the host.

### Docker

dtc, the services and the container-based suites (`wpts`, `smbtorture`, the Kerberos KDC) need a
local Docker: Docker Desktop on a Mac, Docker Engine on Linux. A remote Docker daemon is refused,
and rootless Docker can't make the mounts the suites need. If `dt list` or `dt doctor` takes about
30 seconds, Docker Desktop is stuck (each probe gives up after 30 s): restart it.

### On a Mac

Lint, unit, integration, operator and `clients/macos` run natively. Everything that needs Linux
runs in dtc (below), which `dt run` uses by itself. `clients/macos` mounts with the Mac's own SMB
client and needs an IPv4 address on an interface that is up; with none (offline, or on some VPNs)
the cell doesn't run.

### On Linux

Everything but `system` and `perf-night` can run on the host. Suites that mount need root through
passwordless `sudo` (dt calls `sudo -n`), plus the client packages `dt doctor` names: the NFS
client, `cifs-utils`, `smbclient`, `nfs4-acl-tools`, and a running `rpcbind` for
`integration/portmap`, `cthon04` and `nfstest`. On the host, dt takes pjdfstest, pynfs, cthon04 and
nfstest from the repository's Nix flake (`nix build .#NAME`), so install Nix, or run those suites
in dtc, whose image carries them.

`cthon04` and `nfstest` run only on a Linux host: they mount from a client in its own network
namespace, as an ordinary user with passwordless `sudo`, and dtc runs everything as root, so it
refuses them. They also use `ip`, `iptables`, `tc`, `unshare`, `rpc.statd` and `sshd`.

### On Windows

`unit/windows` and `clients/windows` run on Windows. The repository pins LF line endings for the
files tests compare byte for byte (`.gitattributes`), so keep Git's defaults for this checkout.

### Services

The suites that need a backend find it on localhost. `dt run` never starts one, so start them
first:

```bash
test/harness/bin/dt services up localstack           # S3 for the badger-s3 profile and e2e
test/harness/bin/dt services up postgres kmip        # for integration
test/harness/bin/dt services status                  # all three
test/harness/bin/dt services down
```

| Service | Address | Used by |
|---|---|---|
| `localstack` | `127.0.0.1:4566`, S3 only | `e2e`, and every `badger-s3` cell: pjdfstest, pynfs, wpts, smbtorture, cthon04, nfstest, clients, fio-verify |
| `postgres` | `127.0.0.1:15432`, database `dittofs_test` | `integration` |
| `kmip` | `127.0.0.1:5696`, with throwaway certificates under `test/harness/state/` | `integration` |

To use services of your own instead, set `LOCALSTACK_ENDPOINT`, `DITTOFS_TEST_POSTGRES_DSN` or
the `DITTOFS_TEST_KMIP*` variables.

### dtc, the Linux container

dtc runs `dt` inside a Linux container on any machine with Docker. The container is privileged,
runs as root, and has the NFS and SMB clients, Kerberos, the kernel NFS server, fio and the
conformance tools built in.

```bash
test/harness/bin/dtc run pynfs/badger-s3/1        # any dt command, inside the container
test/harness/bin/dtc shell                        # a shell in it
test/harness/bin/dtc shell -c 'uname -sr; id -un' # one command: Linux, root
test/harness/bin/dtc build                        # rebuild the image now
test/harness/bin/dtc clean                        # remove the image and its cache volumes
```

- **Image:** `dittofs-dtc`, from `test/harness/docker/dev.Dockerfile`, built for the Docker host's
  own architecture on first use (about a minute), and rebuilt whenever the Dockerfile changes.
  The first build clones two test suites over `git://`, so it needs TCP port 9418 out.
- **Mounts:** your checkout at the same path, so paths in output match; the Docker socket, so a
  suite can start containers beside it; and three volumes that keep the Go module, build and lint
  caches between runs.
- **Network:** on Docker Desktop the container reaches the services you started on the host
  through forwarders for ports 4566, 15432 and 5696. On a Linux engine it uses the host network.
- **Mounts it makes** stay inside the container: a program on the host can't see them.
- **Limits:** Docker Desktop's kernel has no `rpcsec_gss_krb5` and no `dm-flakey`, and dtc refuses
  `cthon04` and `nfstest`.

### Disk space

Each run writes its own `dfs` and `dfsctl` build and its outputs under `test/harness/results/`,
which is ignored by Git and never pruned. Delete old runs by hand. `dt cleanup` stops a stack or
services left running and lists leftover `dittofs-`, `dt-` and `dtc-` containers;
`dt cleanup --volumes` also removes the stack's volumes, and `dtc clean` drops the dtc image and
caches. Keep about 30 GB free: the Go build cache with `-race`, Docker's disk and fio's outputs
grow quickly.

## Running tests

### What to run

```bash
test/harness/bin/dt run SUITE|GROUP[/CELL] [--OPTION] [TEST...]
```

```bash
test/harness/bin/dt run unit                                   # one suite, every cell
test/harness/bin/dt run unit --nonrace                         # an option, as a flag
test/harness/bin/dt run pjdfstest/badger-s3/4.1                # one cell
test/harness/bin/dt run pynfs/badger-s3                        # every cell under a name: both NFSv4 minor versions
test/harness/bin/dt run smbtorture/badger-s3/shard-1           # one shard
test/harness/bin/dt run smbtorture smb2.lock                   # one test of a suite
test/harness/bin/dt run e2e TestNFSv4BasicOperations           # one Go test
test/harness/bin/dt run e2e 'TestStoreMatrixOperations/memory/memory'   # one Go subtest
test/harness/bin/dt run conformance                            # a group
test/harness/bin/dt run conformance --skip wpts,smbtorture     # a group, less two suites
test/harness/bin/dt unit                                       # unit, integration and e2e have short forms
```

- **Go tests** are named as `go test -run` patterns, anchored, and dt always adds `-count=1`, so a
  result never comes from Go's test cache. Name a subtest on its own, one per run.
- **Cell names** match by whole parts: `pynfs/badger-s3` picks both of its cells, `pynfs/badger`
  matches nothing.
- **Other suites** take a test's full name or its file name. A name that matches nothing is an
  error. pynfs and the lint suites have no tests to pick from.
- **Tiers:** `dt run` runs at the `pr` tier unless told otherwise, or at a suite's own tier when
  that is wider: `dt run e2e` runs at `merge`, `dt run nfstest` at `nightly`. The tier sets the
  suite's arguments, environment and test selection; `--tier` picks another.

### `dt run` flags

Every flag of `dt run`, each suite's options among them (`dt run smbtorture --kerberos`), is in
the [command reference](#command-reference), as `dt run --help` prints it. Where a run happens and
how many cells run at once have their own sections next.

### Where a run happens: `--in`

- **`host`** runs on this machine. A cell whose needs this machine doesn't meet is skipped, or,
  with `--ci`, not run.
- **`container`** runs the whole command in dtc.
- **`auto`**, the default outside CI, moves the run into dtc when a cell needs something the host
  lacks and dtc gives (Linux, root, the NFS and SMB clients, Kerberos, the kernel NFS server, fio),
  and Docker answers. dt prints `[dt] in dtc: dt run ... --in=host` and runs the same command
  there. Inside dtc a run always stays put.

A need that only the host can meet keeps the run on the host. A `service:` need counts as met in
dtc only when the service is up on the host, so with Localstack down, `--in auto` stays on the
host and the cell doesn't run. A fixed port works the same way: `wpts` and `smbtorture` need port
445, which is free on a Mac, so `--in auto` keeps them there, where they're skipped. Run those
with `--in container`.

Here is what that looks like on a Mac, as dt prints it (checkout path shortened):

```text
$ test/harness/bin/dt run pjdfstest/badger-s3/4.1 --in host --ci
[dt] pjdfstest/badger-s3/4.1: not run dt mounts NFS only on Linux yet
::error title=dt pjdfstest/badger-s3/4.1::not run: dt mounts NFS only on Linux yet
CELL                     RESULT   TIME  NOTE
pjdfstest/badger-s3/4.1  not run  0s    dt mounts NFS only on Linux yet
results: ~/dittofs/test/harness/results/20261119T092223Z-2d7db986
cells: 1 not run, 1 total in 0s
(exit 2)

$ test/harness/bin/dt run pjdfstest/badger-s3/4.1 chmod/00.t
[dt] in dtc: dt run pjdfstest/badger-s3/4.1 chmod/00.t --in=host
[dt] $ go build -o ~/dittofs/test/harness/results/20261119T092525Z-2d7db986/bin/ ./cmd/dfs ./cmd/dfsctl
[dt] fixture: blocks in the bucket dittofs-dt at http://127.0.0.1:4566, under dt-1bda5eab77bc3f40/
...
CELL                     RESULT  TIME  NOTE
pjdfstest/badger-s3/4.1  passed  19s
results: ~/dittofs/test/harness/results/20261119T092525Z-2d7db986
cells: 1 passed, 1 total in 20s
(exit 0, after 25 s)
```

### Several cells at once: `--parallel`

`--parallel N` starts up to N cells together. Only suites whose registry entry says
`parallel: true` share a run: today `lint-go`, `lint-repo`, `shellcheck`, `unit` and `pjdfstest`.
Two cells still run one after the other when either suite lacks `parallel: true`, either needs a
disk of its own, both need the host's rpcbind or kernel NFS server, or both need the same fixed
port. With N above 1, each output line starts with its cell's name, and each cell keeps its own
log, fixture, port and mount point.

```bash
test/harness/bin/dt run pjdfstest chmod/00.t --in container --parallel 3   # NFSv3, v4 and v4.1 side by side
```

On a Mac each of the three cells takes 13 s, and the run ends with `cells: 3 passed, 3 total in
16s`, about what one takes alone.
[`testing-parallel.md`](testing-parallel.md) says when a suite may say `parallel: true` and how to
check it.

### Testing only what changed

`dt run unit --changed` tests the packages with a change committed between the base and `HEAD`.
It tests every package when `go.mod`, `go.sum` or `go.work` changed, or when a changed file is in
no package. It doesn't test the packages that import a changed one. It sees only committed changes,
and it needs the base fetched (`git fetch origin develop`). On a branch based on another branch,
pass `--base origin/<that branch>`; the commit checks in `lint-repo` use the same base.

### Exit codes

| Code | Means |
|---|---|
| 0 | Every cell passed or was skipped, and at least one passed |
| 1 | A cell failed: a new failure, or a known failure that now passes |
| 2 | A cell didn't run, or nothing ran: a need not met under `--ci`, a fixture that failed to start, a skip no known file explains |
| 3 | A usage or registry error, or (from `dt doctor`) an expired quarantine entry |

Skipped cells don't count. Without `--ci`, a cell whose needs aren't met is skipped quietly, so a
run can exit 0 having skipped most of what you asked for. Read the table it prints, or pass `--ci`.

### Environment variables

| Variable | Effect |
|---|---|
| `LOCALSTACK_ENDPOINT`, `DITTOFS_TEST_POSTGRES_DSN`, `DITTOFS_TEST_KMIP*` | Use these services instead of the local ones |
| `GITHUB_TOKEN` | Lets `dt history --ci`, `dt plan --estimate --ci` and `dt known prune --ci` read CI's results |
| `DITTOFS_REPO` | Points `dtc` and the environment commands at another checkout |
| `DITTOFS_E2E_*`, `DT_*` | Passed through into dtc |
| `NO_COLOR` | Set to anything but empty, turns `dt run`'s colors off, as `--no-color` does; passed through into dtc |
| `DITTOFS_SKIP_PREPUSH`, `DITTOFS_SKIP_*`, `DITTOFS_PREPUSH_BASE` | Git hook switches (see [Git hooks](#git-hooks)) |

Suites that run as root (e2e, pjdfstest and the other mounting suites) go through `sudo`, which
resets the environment, so variables you set don't reach those tests. The `dfs` a fixture starts
gets no `DITTOFS_*` variable from your shell either.

## Results and reports

Each run writes to `test/harness/results/<UTC time>-<commit>/`:

| Path | Holds |
|---|---|
| `summary.json` | The whole run in [CTRF](https://ctrf.io) 0.1.0: every test's status, and dt's grade, reason and issue under `extra` |
| `report.md` | The report as Markdown, as CI shows it |
| `junit/<cell>.xml` | JUnit, one file per cell |
| `logs/<cell>.log` | Everything the cell printed |
| `<cell>/` | The cell's own outputs: `dfs.log`, the tool's result files, `coverage.txt` for unit, a failed scenario's logs under `failed/` |
| `bin/`, `tools/` | The `dfs` and `dfsctl` built for the run, and the test tools it used |

A run ends with its results directory and a line counting its cells by result, with the run's time:
`cells: 1 failed, 2 passed, 1 skipped, 4 total in 1m12s`. On a terminal the results are colored:
passed green, failed red, not run yellow. `--no-color`, or `NO_COLOR` set, turns that off.

```bash
test/harness/bin/dt run SUITE --json > run.json # the run as JSON on stdout, the rest on stderr
test/harness/bin/dt report                      # the last run: each cell's result and time, then its new failures
test/harness/bin/dt report --failures           # only the new failures
test/harness/bin/dt report RUN_DIR              # another run, such as one downloaded from CI
test/harness/bin/dt report --format md|json     # as Markdown or JSON
test/harness/bin/dt history SUITE[/TEST]        # pass, fail, skip and flaky counts over the last 10 local runs, with p50 and p90 times
test/harness/bin/dt history --ci SUITE/TEST     # the same over develop's CI runs (needs GITHUB_TOKEN)
test/harness/bin/dt history diff [A B]          # which cells and tests took longer or shorter between two CI runs; without run IDs, the last 10's halves
```

## Known failures, expected skips and quarantine

A suite with a `known:` file in the registry is graded against it. Each entry names a test that
fails, or skips, for a reason someone has written down:

```yaml
- test: smb2.lock.cancel-tdis      # a name, or a glob where * crosses /
  status: fail                     # fail or skip
  fix: planned                     # planned or wontfix
  category: bug                    # bug, feature, env, proto or permanent
  issue: "#1234"                   # required when fix is planned
  reason: why it fails, specific enough to act on
  where: {nfs: ["3"]}              # optional: only on these cells (axis, tier, option or os)
  message: substring of the failure # optional: only a failure with this text matches
  intermittent: true               # optional: a pass is not unexpected
```

When several entries match a test, an exact name wins over a pattern, and otherwise the first in
the file.

| What happened | Grade | Effect on the cell |
|---|---|---|
| A failure no entry covers (or whose message doesn't match) | `new` | fails it |
| A failure an entry covers | `known` | none |
| A pass where an entry expects a failure | `unexpected_pass` | fails it, unless the entry says `intermittent` |
| A skip an entry expects | `expected_skip` | none |
| A skip no entry expects, in a suite with a known file | `unexpected_skip` | not run (exit 2) |
| A skip in a suite without a known file | `ungraded` | none |
| A Go test that crashes, or any test that hangs past its limit | `new` | fails it; no entry can excuse one |
| Output that stops mid-test (tap, subunit, nfstest) | `incomplete` | not run (exit 2) |

So the change that fixes a known failure removes its entry in the same pull request, or the run
fails.

**Quarantine.** A flaky test that can't be fixed yet can go in
[`test/harness/quarantine.yaml`](../../test/harness/quarantine.yaml): its suite, its exact name
(no globs), an issue, and an expiry date at most 90 days away. While the entry lasts, the test's
failures and passes don't affect the cell. Once it expires it excuses nothing, and `dt doctor`,
which Repo Checks runs, fails until someone fixes the test or renews the entry.

**Retries.** `--retry N` runs a new failure again, on the same fixture, within the cell's time
limit. A test that passes on a retry is graded flaky: the cell passes with a note, and
`dt history` counts it. CI never retries on pull requests or in the merge queue, and retries once
after a merge and nightly, keeping the first attempt's record.

**Tools for known files:**

```bash
test/harness/bin/dt known prune SUITE            # drop entries that matched nothing in the last runs (edits the file)
test/harness/bin/dt known baseline smbtorture/badger-s3/shard-1   # rewrite smbtorture's baseline-results.md
test/harness/bin/dt known import LIST.md KNOWN.yaml    # convert an old Markdown list, once
```

## When a test fails

1. **Read the report.** Locally, `dt report`. In CI, the `gate` job's summary collects every
   blocking cell's report, and each cell's job has its own; the annotations point at the failing
   tests.
2. **Check develop.** If the same test fails on develop, it isn't yours: the
   [`develop is red`](https://github.com/cubbit/dittofs/issues?q=label%3Adevelop-red) issue lists
   what fails there, and `dt history --ci SUITE/TEST` shows the test's record.
3. **Check the cause.** A failure in a download, a package mirror, the runner's Docker or its disk
   is the infrastructure: re-run the job. The gate reads each cell's latest attempt.
4. **A new failure your change caused:** fix the code. A test written before the code it tests
   lands red with a known entry (`fix: planned`, its issue, a specific `message:`).
5. **"Passed unexpectedly":** your change fixed a known failure; remove its entry. If it only
   passes by chance, mark the entry `intermittent: true` and say how often it passes.
6. **"Unexpected skip" (exit 2):** a test skipped that the known file doesn't expect. Either
   something in the environment is missing, or the skip is real and needs an entry with its reason.
7. **A flaky test:** fix it. Don't add a retry, a sleep or a longer timeout. If the fix has to
   wait, quarantine it with an issue.

To run what CI ran, download the cell's artifact (`dt-run-<cell>-<attempt>`) and run
`test/harness/bin/dt report <dir>`, or run the cell yourself with `--ci`.

## CI

Every test workflow runs through `dt`, in [`.github/workflows/tests.yml`](../../.github/workflows/tests.yml).
Its `plan` job asks `dt plan` which cells the event's tier runs, then runs each cell on a runner
of its own with `dt run CELL --ci`. A new suite in the registry reaches CI with no workflow change.

| Event | Tier | Retries | Notes |
|---|---|---|---|
| Pull request | `pr` | none | Plans only what the diff touches (below) |
| Merge queue | `pr` | none | The whole `pr` tier, on the merged result |
| Push to `develop`, `main`, `feat/storage-v2` | `merge` | 1 | Posts failures on develop (below) |
| Nightly (22:13 UTC, develop) | `nightly` | 1 | Posts failures on develop |
| Manual dispatch | chosen | none | Maintainers: `gh workflow run tests.yml -f tier=nightly -f suite=nfstest --ref BRANCH` |

**What a pull request runs.**

- **The `pr` tier**, minus what the diff can't affect. A change to documentation alone (`.md` and
  image files, anything under `docs/`) plans no cells. The suites with `paths` (`e2e/kerberos`
  and `e2e/cross-protocol`) run only when the change touches their files. The `unit` cell tests
  only the packages the change touched.
- **Always:** Go Checks, Repo Checks, ShellCheck, the secret scan (gitleaks) and CodeQL.
- **Blocking or not:** `e2e`'s options, `bench-smoke` and `unit/windows` don't block a pull
  request. Everything else does.

**Required to merge** into `develop` or `feat/storage-v2`:

- **Checks:** `gate` (green when every blocking cell passed), Go Checks, Repo Checks, ShellCheck,
  gitleaks and `Analyze (go)`.
- **Review and history:** one approving review, signed commits, and a branch up to date with its
  base. Merges are squash merges, through a merge queue.

The queue runs the whole `pr` tier again on the merged result. A check that fails there drops the
pull request from the queue and turns its auto-merge off.

**Commit messages.** The subject is a conventional commit, `type(scope): subject`, at most 100
characters. A `docs:` or `chore(release):` commit may change only documentation, since reviewers
skip its diff (Repo Checks' `commit-type-matches-diff.sh`).
Commits and pull request descriptions carry no AI attribution trailers (`no-ai-attribution.sh`).

**Where the results are.**

- **The `gate` job's summary:** one page with every blocking cell's report.
- **Each cell's job:** its summary is its report, and its artifact `dt-run-<cell>-<attempt>`
  (`dt-non-blocking-...` for a non-blocking cell) holds its whole results directory except `bin/`
  and `tools/`. Artifacts last 90 days.
- **Annotations:** new failures, unexpected passes and cells that didn't run show on the pull
  request. Go Checks also annotates vet and golangci-lint findings.

**From a fork.**

- **Approval:** workflows on a pull request from a fork may wait for a maintainer's approval
  before they start.
- **Secrets:** no job on a pull request needs a secret, so a fork's pull request runs the whole
  `pr` tier.
- **Not on any pull request:** nightly cells, the `merge`-tier `e2e` cell, and the dedicated
  host's suites.

**Develop's health.**

- **The `develop is red` issue:** after each merge and nightly run on develop, dt keeps this one
  issue (label `develop-red`) up to date. It is open while anything fails and closed when nothing
  does. A cell that didn't run counts on its second run in a row.
- **Per-test issues:** a test that fails anew twice in a row gets an issue of its own, titled
  `CELL TEST fails on develop`.
- **The CI Health comment:** pull requests from branches of this repository get a comment while
  develop is red.

**The dedicated host.** Some suites need more than a hosted runner gives: the system scenarios,
some of which need 110 GB of disk, and the nightly performance run. They run on a dedicated Linux
server outside GitHub Actions, which runs the `merge` tier on develop's new commits and the
`nightly` tier each night, sets the `system (dedicated)` commit status and files issues like the
hosted runs. No pull request code runs there. `dt plan --tier nightly` lists its cells under
place `dedicated`.

**How long it takes.** In November 2026 a merge-queue run took about 15 minutes (25 at the 90th
percentile), and a run after a merge about 23 minutes (30). The slowest `pr` cells are `unit`,
`pynfs` and the `wpts` and `smbtorture` shards, each 10 to 20 minutes.

**Coverage.** The `unit` cell's coverage profile goes to Codecov after a merge. Pull requests get
no coverage status.

## Git hooks

```bash
make setup-hooks                                 # or: test/harness/bin/dt hooks install
test/harness/bin/dt hooks install --container    # the same hooks, run in dtc: no Go needed on the host
test/harness/bin/dt hooks uninstall              # back to .githooks
test/harness/bin/dt hooks status
```

| Hook | Checks | Skip with |
|---|---|---|
| `pre-commit` | conflict markers, staged files over 1 MiB, `gofmt -s` on staged Go files, `go vet`, `go build`, golangci-lint (if installed), `go mod tidy -diff`, shellcheck on staged scripts | `DITTOFS_SKIP_CONFLICT`, `_BIGFILE`, `_GOFMT`, `_VET`, `_BUILD`, `_LINT`, `_TIDY`, `_SHELLCHECK` (each `=1`) |
| `commit-msg` | a conventional subject of at most 100 characters; no AI attribution trailers | `DITTOFS_SKIP_COMMITMSG=1` |
| `pre-push` | builds the changed packages, and runs `dt run unit --packages` on those with tests | `DITTOFS_SKIP_PREPUSH=1` |

`git commit --no-verify` and `git push --no-verify` skip them all. The pre-push hook compares
with `DITTOFS_PREPUSH_BASE` (default `origin/develop`); on a branch based on another branch, set
it to that branch.

## Writing tests

These rules apply to every new or changed test. They are the ones
[`CLAUDE.md`](../../CLAUDE.md#writing-tests) gives every contributor, agents included.

**A test must be able to fail.** Break the code it guards in a realistic way that still compiles,
and watch the test fail on its assertion. A test that has never failed is unverified. Tests that
can't fail are rejected: a type-shape assertion, a constant compared with its own literal, a check
the compiler already makes, a copy of the implementation's own table, a test that exercises only a
fake.

**No mocks, no stubs.** Needing one means the code is coupled to something it should take as an
argument. Put the logic in a function of its inputs and keep I/O in a thin layer at the edge, or
take the dependency as a small interface and test against a real implementation of it: the memory
metadata or block store, `t.TempDir()`, a listener on `127.0.0.1:0`, an in-memory SQLite store.
Three kinds of double are allowed ([RFC 29 §4.11](rfcs/rfc-29-test-harness.md#411-test-doubles) has
the reasoning):

1. An injected clock or seeded random source.
2. A verified in-memory implementation that lives with the interface's package and passes the same
   conformance suite as the production one.
3. Fault injection: a wrapper around a real implementation, or around kind 2, that fails, delays,
   tears or crashes on cue.

Banned: a canned stub written for one test, a recording or counting fake used to check which calls
were made, a double no conformance suite holds to the real implementation, and swapping a
package-level variable to replace behavior. A sink that only keeps what the code delivered is
fine, and so is a wrapper that records each call and passes it through, when that call is itself
the requirement.

**Synchronize on events, never on time.** Wait on a channel, a `sync.WaitGroup` or the condition
itself, with one generous timeout as a backstop. A duration the code waits on is advanced on an
injected clock, not slept.

**No fixed ports, paths or global state.** Bind to `127.0.0.1:0`, write under `t.TempDir()`, clean
up with `t.Cleanup`.

**Fix a flake, never retry it.** A flaky test is a race in the code or in the test. Quarantine
one you can't fix yet, with an issue.

**Assertions name what they check:** `got %v, want %v`, naming the field or operation.
Table-driven cases suit several inputs to one rule.

**By kind:**

- **Unit:** untagged and fast; `-short` on pull requests. Badger under `t.TempDir()` and
  in-memory SQLite are real implementations, so their tests run untagged.
- **Integration** (`-tags=integration`, `-p 1`): for a backend the host must provide, such as a
  KMIP server. External systems (a KDC, a domain controller, S3) are tested for real here; their
  parsing and logic are pure functions with unit tests. A missing backend is a skip that names the
  variable to set.
- **E2E** (`-tags=e2e`, root, kernel clients): only for what a real mount shows: client caching,
  the mount handshake, interoperation. A rule a unit or integration test can check belongs there.
- **Fuzz** every decoder of bytes from the wire or the disk. Assert an invariant (no panic, reads
  stay inside the buffer, a round trip decodes to the same value), and commit each crasher under
  `testdata/fuzz/<FuzzName>/`, where plain `go test` replays it.
- **Build tags:** a new tag goes into lint-go's `go vet -tags=` list in `test/suites.yaml`, or
  vet and the linter never see the file. Don't tag a file only to keep it out of `go test ./...`
  where `testing.Short()` would do.
- **New packages** come with a test file; Repo Checks' `new-package-tests.sh` fails a new package
  without one.

**Test the consumer.** When a fix carries a value from one layer to another, test where it's
used, not where it's produced: the value can be dropped again in between. Revert the fix and
watch the test fail on its assertion.

## Adding or changing a suite

A suite is an entry in [`test/suites.yaml`](../../test/suites.yaml). Every `dt` command checks the
whole registry when it loads, so a mistake fails fast with exit 3. The fields:

| Field | Meaning |
|---|---|
| `cmd` | The command: a string, `{sh: '...'}` for a shell line, or a list (every command runs even after one fails). Placeholders such as `{test}`, `{mount}`, `{bin}/dfs`, `{out}`, `{packages}` and `{base}` are filled in per cell |
| `format` | How results come back: `exit`, `go-json`, `junit`, `tap`, `subunit`, `trx` or `nfstest` |
| `tier` | `pr`, `merge`, `nightly` or `manual` (default `merge`) |
| `blocking` | Whether a failure blocks, overall or per tier (default true) |
| `needs` | What the machine must give: `linux`, `macos`, `windows`, `root`, `docker`, `podman`, `rpcbind`, `knfsd`, `nfs-client`, `smb-client`, `smbclient`, `nfs4-acl-tools`, `kerberos`, `dm-flakey`, `fio`, `host:dedicated`, `disk:N`, `port:N`, `service:NAME` |
| `needs_by_test` | Extra needs for the tests a glob matches, which then get a cell of their own |
| `fixture` | What dt starts: `server` (dfs or knfsd), `profile`, `nfs`, `smb`, `shares`, `users`, `kerberos`, `clients`, `reset: each` and more |
| `image`, `volumes`, `templates` | A container image to run the command in, its mounts, and config templates rendered into `{out}/config` |
| `tests`, `exclude`, `select` | The tests: a glob, a list or a `.txt` file of names with per-test limits and arguments; globs to leave out; per-tier selection |
| `matrix` | Axes and their values, each combination a cell |
| `options` | Named variants; each inherits the suite's fields and replaces the ones it sets |
| `shards` | Deal the tests over N cells |
| `timeout` | `test` and `cell` limits (defaults 30 and 60 minutes) |
| `known` | The known file |
| `packages` | For Go suites: a build `tag`, or package `patterns` |
| `env`, `args` | Environment and arguments, overall or per tier |
| `paths` | On pull requests, run only when the change touches these globs |
| `parallel` | May share a `--parallel` run (see [testing-parallel.md](testing-parallel.md)) |
| `job` | A fixed GitHub job name, for the three lint suites |

The checks that keep a suite honest:

- **Variants:** a variant is an option of its suite, not a new suite.
- **New needs:** a new need needs CI to install it. Under `--ci` an unmet need is "not run", and
  that turns the cell red.
- **Known files:** a suite with a `known` file appears in the table under
  [Conformance test suites](#conformance-test-suites). Run `test/harness/bin/dt docs` to rewrite
  that table, and commit the result. Repo Checks runs `dt docs --check`.
- **RFCs:** each RFC under `docs/internals/rfcs/` has one `**Suites.**` paragraph whose code spans
  name real suites, groups or options. The same check enforces it.
- **`parallel: true`:** set it only after the checks in [testing-parallel.md](testing-parallel.md).
- **Performance:** a performance suite tracks and doesn't block. Hosted runners are too noisy to
  gate on.

## The Go suites: unit, integration, e2e, operator

**Unit.**

```bash
test/harness/bin/dt run unit --tier merge         # every package, -race, no -short (as after a merge)
test/harness/bin/dt run unit --changed            # what a pull request runs
test/harness/bin/dt run unit --packages ./pkg/metadata/...,./internal/adapter/nfs/...
go test ./pkg/metadata/...                        # plain go test works for a quick loop
```

`unit/nonrace` runs two performance gate tests without the race detector, which would distort
what they measure. It runs nightly.

**Integration.**

```bash
test/harness/bin/dt services up postgres kmip
test/harness/bin/dt run integration
test/harness/bin/dt run integration --kerberos    # against a real KDC, in Docker
test/harness/bin/dt run integration --portmap     # Linux, with rpcbind running
test/harness/bin/dt run integration --ad-dc       # against a domain controller, in Docker (merge tier)
```

The stores are Badger, and FoundationDB for a cluster: add no new Postgres test. The suite runs
with `-p 1`, one package at a time.

**E2E.** It needs Linux, root, the kernel NFS and SMB clients, `nfs4-acl-tools` and Localstack. On a
Mac, `dt run e2e` goes to dtc once Localstack is up.

```bash
test/harness/bin/dt services up localstack
test/harness/bin/dt run e2e                                  # every test
test/harness/bin/dt run e2e TestNFSv4BasicOperations
test/harness/bin/dt run e2e --kerberos                       # Kerberos over NFS and SMB
test/harness/bin/dt run e2e --cross-protocol                 # NFS and SMB on the same files
test/harness/bin/dt run e2e --stress                         # nightly
```

The plain `e2e` cell runs after a merge and blocks there. Its `kerberos` and `cross-protocol`
options run on pull requests that touch their files, without blocking. The suite sets
`DITTOFS_E2E_REQUIRE_NLM=1` and `DITTOFS_E2E_REQUIRE_KERBEROS=1`, so a missing prerequisite fails
a test instead of skipping it. E2E has no known file: a failing e2e test is fixed, not listed.

**Operator.** `dt run operator` runs the Kubernetes operator's `make test`, `make lint`,
`make manifests-verify` and a `kubectl kustomize` render, in `k8s/dittofs-operator/`. `make test`
downloads `setup-envtest` and the Kubernetes test binaries on first use.

## Conformance test suites

DittoFS is validated against established conformance suites for SMB and NFS. Each runs through
`dt` against a `dfs` built from your checkout, usually on the `badger-s3` profile, which needs
Localstack. The table comes from the registry:

<!-- conformance-suites:begin -->
<!-- Generated from test/suites.yaml by test/harness/bin/dt docs. Do not edit by hand. -->

| Suite | Protocol | Profiles | Variants | Tier | Known failures and skips |
|---|---|---|---|---|---|
| `pjdfstest` | NFS | `badger-s3` | nfs: `3`, `4`, `4.1` | `pr` | [`test/posix/known.yaml`](../../test/posix/known.yaml) |
| `pynfs` | NFS | `badger-s3` | minor: `0`, `1` | `pr` | [`test/nfs-conformance/pynfs/known.yaml`](../../test/nfs-conformance/pynfs/known.yaml) |
| `pynfs/knfsd` | NFS | none | minor: `0`, `1` | `manual` | [`test/nfs-conformance/pynfs/known.yaml`](../../test/nfs-conformance/pynfs/known.yaml) |
| `wpts` | SMB | `badger-s3` | none | `pr` | [`test/smb-conformance/known.yaml`](../../test/smb-conformance/known.yaml) |
| `smbtorture` | SMB | `badger-s3` | none | `pr` | [`test/smb-conformance/smbtorture/known.yaml`](../../test/smb-conformance/smbtorture/known.yaml) |
| `smbtorture/kerberos` | SMB | `badger-s3` | none | `merge` | [`test/smb-conformance/smbtorture/known-kerberos.yaml`](../../test/smb-conformance/smbtorture/known-kerberos.yaml) |
| `cthon04` | NFS | `badger-s3` | mount-options: `default`, `v3`, `v4.0`, `v4.1`, `v4.1-noac`, `v4.1-smallio` | `pr` | [`test/nfs-mount/cthon04/known.yaml`](../../test/nfs-mount/cthon04/known.yaml) |
| `nfstest` | NFS | `badger-s3` | mount-options: `default`, `v3`, `v4.0`, `v4.1`, `v4.1-noac`, `v4.1-smallio` | `nightly` | [`test/nfs-mount/nfstest/known.yaml`](../../test/nfs-mount/nfstest/known.yaml) |
| `system` | none | none | none | `merge` | [`test/scenarios/known.yaml`](../../test/scenarios/known.yaml) |

From [`test/suites.yaml`](../../test/suites.yaml): its suites graded against a known file, and their options.
Each runs in its tier and the wider ones, `pr` then `merge` then `nightly`; a `manual` one only when asked.
<!-- conformance-suites:end -->

Each suite starts its own `dfs`, Localstack bucket prefix and mounts, and the container suites use
fixed container names and port 445. Run one at a time unless `--parallel` allows more: two `dt`
processes started by hand collide.

### pjdfstest (POSIX over NFS)

pjdfstest's `.t` files, run with `prove` over an NFS mount with client caching off
(`noac,sync,lookupcache=none`, and `nolock` on NFSv3), on NFSv3, v4 and v4.1.

```bash
test/harness/bin/dt run pjdfstest/badger-s3/3
test/harness/bin/dt run pjdfstest/badger-s3/4 chmod/00.t       # one test file
test/harness/bin/dt run pjdfstest chmod/00.t --parallel 3       # all three versions at once
```

Known failures: [`test/posix/known.yaml`](../../test/posix/known.yaml). Manual setup and the test
categories: [`test/posix/README.md`](../../test/posix/README.md).

### pynfs (NFSv4 protocol)

The NFSv4 suite that exercises the protocol itself rather than filesystem semantics: state,
sessions, owners, locking and wire error codes. It is its own NFSv4 client, so it needs no kernel
mount and no root, and it sends what a kernel client never would.

```bash
test/harness/bin/dt run pynfs/badger-s3/0      # NFSv4.0
test/harness/bin/dt run pynfs/badger-s3/1      # NFSv4.1
test/harness/bin/dt run pynfs --knfsd          # the same tests against the Linux kernel's NFS server (manual)
```

The `--knfsd` option tells an assertion no server satisfies from one DittoFS fails alone; a known
entry with `category: proto` and `fix: wontfix` needs that evidence. pynfs can't run single tests
through dt. Known failures: [`test/nfs-conformance/pynfs/known.yaml`](../../test/nfs-conformance/pynfs/known.yaml);
test codes and grading: [`test/nfs-conformance/pynfs/README.md`](../../test/nfs-conformance/pynfs/README.md).

### WPTS (Windows Protocol Test Suites)

The MS-SMB2 build verification tests from Microsoft's Windows Protocol Test Suites, run in their
container image against `dfs`.

```bash
test/harness/bin/dt run wpts/badger-s3
test/harness/bin/dt run wpts --in container    # from a Mac: port 445 keeps --in auto on the host
```

Known failures and expected skips: [`test/smb-conformance/known.yaml`](../../test/smb-conformance/known.yaml);
more in [`test/smb-conformance/README.md`](../../test/smb-conformance/README.md).

### smbtorture

The SMB2 tests listed in [`tests.txt`](../../test/smb-conformance/smbtorture/tests.txt), run in a
container, split over two shards, five minutes per test, with the share reset before each test.
Its `kerberos` option runs a few tests with Kerberos sign-in against a KDC dt starts.

```bash
test/harness/bin/dt run smbtorture/badger-s3                # both shards
test/harness/bin/dt run smbtorture/badger-s3/shard-1        # one
test/harness/bin/dt run smbtorture smb2.lock                # one test
test/harness/bin/dt run smbtorture --kerberos               # merge tier
```

Known failures: [`known.yaml`](../../test/smb-conformance/smbtorture/known.yaml) and
[`known-kerberos.yaml`](../../test/smb-conformance/smbtorture/known-kerberos.yaml).
`dt known baseline` keeps [`baseline-results.md`](../../test/smb-conformance/smbtorture/baseline-results.md)
current.

### cthon04 and nfstest (NFS mount options)

The other NFS suites mount with client caching and locking off, or don't mount at all. These two
mount the way a user would, through the kernel client, once per named option set: `default` (no
options, so NFSv4.2), `v3`, `v4.0`, `v4.1`, `v4.1-noac` and `v4.1-smallio`. A failure points at
the options that caused it.

- **cthon04** (Connectathon): `basic`, `general`, `special` and `lock`, on every set, one minute
  per test. Every pull request.
- **nfstest:** `posix` and `dio` on every set, `cache` on `v3`, `lock` on `v3`, `v4.0` and `v4.1`,
  and `delegation` on the v4 sets, with a second client; 20 minutes per test. Nightly.

```bash
test/harness/bin/dt run cthon04/badger-s3/v4.1     # a Linux host, as an ordinary user with passwordless sudo
test/harness/bin/dt run nfstest/badger-s3/v3
```

Known failures: [`cthon04/known.yaml`](../../test/nfs-mount/cthon04/known.yaml) and
[`nfstest/known.yaml`](../../test/nfs-mount/nfstest/known.yaml); an entry's `where` names the sets
it holds on. The client setup, and how to compare with the Linux kernel's server
(`test/nfs-mount/nfstest/baseline-knfsd.sh`), are in [`test/nfs-mount/README.md`](../../test/nfs-mount/README.md).

### SMB clients

`clients` mounts a share with a real SMB client and runs `files.sh`, `rename.sh` and `s3.sh` from
[`test/clients/`](../../test/clients) on it: the Linux kernel client after a merge, and nightly SMB
2.1 (`--smb21`), macOS's client (`--macos`) and Windows' (`--windows`).

```bash
test/harness/bin/dt run clients/macos          # on a Mac, against the memory profile
```

### smb-fuzz

A manual suite that sends generated SMB requests to a reference server and to DittoFS and compares
their error codes. `dt run smb-fuzz`; see [`test/smbfuzz/README.md`](../../test/smbfuzz/README.md).

### Spec citations

Code comments that cite a protocol specification section are checked against the specifications'
section lists by `go run ./test/spec-citations`, part of lint-go. The rules are in
[`test/spec-citations/README.md`](../../test/spec-citations/README.md).

### System scenarios

Each scenario in [`test/scenarios/`](../../test/scenarios/README.md) runs a DittoFS server in a
throwaway podman container, with SMB and NFS clients and faults such as an S3 outage or a crash.
Names follow `NN-what-size.sh`, the size bounding its run time (`xs` under 10 s up to `xxl`).

- **Where it runs:** the `system` suite needs the dedicated host. It runs the `0x` to `8x`
  scenarios after every merge to develop, and every scenario, the long `9x` ones included, each
  night. It sets the `system (dedicated)` commit status.
- **A failed scenario's logs:** under the run's `system/failed/<scenario>/`, the last 4 MiB of each
  file.
- **Known failures:** [`test/scenarios/known.yaml`](../../test/scenarios/known.yaml).
- **To run one yourself,** on Linux with rootless podman 5.6 or later and bash 5:
  ```bash
  test/scenarios/setup.sh 01-*.sh
  ```
  The scenarios README shows how to do the same from Docker Desktop, in podman's container image.

## Performance and benchmarks

Performance is tracked, not gated: hosted runners are too noisy for a threshold.

- **`bench-smoke`:** a short run of the benchmark tool on every pull request, non-blocking.
- **`fio-verify`:** fio jobs over an NFS 4.1 mount (`fio-verify/smb` over SMB) with data
  verification, so a verify error fails the test. Nightly.
- **`perf-night`:** the full nightly performance run on the dedicated host.
- **Go benchmarks:** `go test -bench=. -benchmem -run=^$ ./pkg/block/engine/`, and
  `make bench-blockstore`.

Writing a benchmark: measure only the operation. Do setup before `b.ResetTimer()`, call
`b.ReportAllocs()` on hot paths, and compare with `benchstat` over `-count=10` runs on the same
machine, never against a number from another machine or from inside Docker Desktop. The benchmark
tool itself, `dfsbench`, is documented in [`bench/README.md`](../../bench/README.md).

## Running DittoFS by hand

`dt stack` runs DittoFS and Localstack from the repository's `docker-compose.yml`, for poking at a
server by hand:

```bash
test/harness/bin/dt stack up            # dfs on badger-s3 with Localstack; --memory for the memory profile
test/harness/bin/dt stack mount         # NFSv3 at ~/mnt/dittofs-nfs and SMB at ~/mnt/dittofs-smb
test/harness/bin/dt stack status
test/harness/bin/dt stack logs
test/harness/bin/dt stack umount
test/harness/bin/dt stack down          # -v also removes its volumes
```

The stack publishes the API on `127.0.0.1:8080`, NFS on `12049` and SMB on `12445`, and creates a
development user for SMB. To mount a `dfs` you started yourself:

```bash
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049 localhost:/export /mnt/test            # Linux
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049,resvport localhost:/export /mnt/test   # macOS
```

## Real NFSv3 NLM lock testing (network-namespace isolation)

DittoFS coordinates byte-range locks across NFSv3 NLM, NFSv4 and SMB through a single
cross-protocol lock manager (`pkg/metadata/lock/manager.go`). Proving this end to end with a
**real kernel NFSv3 client** (not the `nolock` mount option, which makes the kernel resolve locks
locally and never contact the server) takes a specific harness. This section explains why, and how
the e2e suite does it.

### Why a single host is not enough

A kernel NFSv3 client discovers the server's NLM (lockd) port by asking rpcbind on port 111, a
location fixed by RFC 1833 with no client-side override. But the kernel's own lockd also registers
program 100021 with the local rpcbind. The moment an NFSv3 mount with locking activates on the same
host as DittoFS, the kernel lockd takes the 100021 mapping back, so the client sends its NLM
requests to the local kernel lockd instead of to DittoFS. A userspace NLM server and a kernel NFSv3
client's lockd can't share one rpcbind. Any userspace NFS server meets this; the usual answer, and
the one DittoFS uses, is to **isolate the client in a separate Linux network namespace**.

### Harness layout

```
  server netns (sns)                client netns (cns)
  ┌────────────────────────┐        ┌────────────────────────┐
  │ dfs (NFSv3/v4 + SMB)    │        │ kernel NFSv3 client     │
  │ rpcbind :111 (own)      │◀──────▶│ rpcbind :111 (own)      │
  │   NLM 100021 → dfs port │  veth  │ rpc.statd (NSM)         │
  │                         │        │ mount -o nfsvers=3      │
  └────────────────────────┘        │   (NO nolock)           │
                                     └────────────────────────┘
```

The server and the client each run in their own network namespace, with their own rpcbind and
their own lockd and NSM port space, so the two 100021 registrations never collide. The server's
rpcbind is made private by starting dfs under `unshare --mount` with a fresh tmpfs on `/run`;
otherwise it would share the host's rpcbind socket. The client's `GETPORT` for NLM asks the
**server** namespace's rpcbind across the veth and resolves to the DittoFS port; `SM_NOTIFY` and
`GRANTED` callbacks come back over the same veth.

Each of these is a real failure mode:

- **`rpc.statd` must run in the client namespace.** Without a reachable NSM, the first lock fails
  with `SM_MON status 1`, then `NLM4_DENIED_NOLOCKS`. The harness takes a warm-up lock first, so an
  NSM that isn't ready is never mistaken for a conflict.
- **Give each side its own rpcbind.** "failed to contact remote rpcbind" usually means a shared or
  misbound rpcbind.
- **Use IP addresses, not hostnames,** for the mount target and NSM: the two namespaces don't share
  name resolution.
- **DittoFS must register with the system rpcbind, with UDP on** (below); UDP carries NSM and
  `SM_NOTIFY`.

### Enabling server-side registration

Registration is off by default. The e2e harness turns it on at run time:

```
PATCH /api/v1/adapters/nfs/settings
{ "portmapper_register_with_system": true, "udp_enabled": true }
```

From the command line, `dfsctl adapter settings nfs update --portmapper-register-with-system
--udp-enabled`, then restart the adapter.

### What it covers

- **NLM and SMB:** an NFSv3 NLM byte-range lock conflicts with an SMB byte-range lock on the same
  file, in both directions, resolved by the server's one lock manager.
- **NLM and NFSv4:** an NFSv3 NLM lock conflicts with an NFSv4 `LOCK`, in both directions.
- **Registration:** `rpcinfo -p` shows DittoFS's NLM (100021) mapping.

### Running it

The tests need root, `rpcbind`, `rpc.statd` and `iproute2`, so they run on Linux. Under `dt` the
e2e suite sets `DITTOFS_E2E_REQUIRE_NLM=1`, so a missing prerequisite fails the test rather than
skipping it. CI's e2e cells install `rpcbind`, and dt runs the tests as root.

```bash
test/harness/bin/dt run e2e TestNLMAxisInterop
```

The framework is in `test/e2e/framework/netns.go` and the tests in
`test/e2e/nlm_axis_interop_test.go`. The single-host registration check, which can't take a real
lock for the reason above, is `TestNLMSystemRpcbindRegistration` in `test/e2e/nlm_locking_test.go`.

## Command reference

Every `dt` command with its usage line and flags, which `dt COMMAND --help` also prints. `dt docs`
writes this section from dt's commands, and `dt docs --check`, in `lint-repo`, fails when it is out
of date.

<!-- dt-commands:begin -->
<!-- Generated from dt's commands by test/harness/bin/dt docs. Do not edit by hand. -->

### `dt cleanup`

Stop everything the harness can leave running

```text
dt cleanup [ARGS]
```

### `dt completion`

Generate the autocompletion script for the specified shell

```text
dt completion
```

### `dt completion bash`

Generate the autocompletion script for bash

```text
dt completion bash
```

Flags:

```text
      --no-descriptions[=true|false]   disable completion descriptions
```

### `dt completion fish`

Generate the autocompletion script for fish

```text
dt completion fish [flags]
```

Flags:

```text
      --no-descriptions[=true|false]   disable completion descriptions
```

### `dt completion powershell`

Generate the autocompletion script for powershell

```text
dt completion powershell [flags]
```

Flags:

```text
      --no-descriptions[=true|false]   disable completion descriptions
```

### `dt completion zsh`

Generate the autocompletion script for zsh

```text
dt completion zsh [flags]
```

Flags:

```text
      --no-descriptions[=true|false]   disable completion descriptions
```

### `dt docs`

Write the testing guide's suite table from the registry and its command reference from dt's commands, or check them; check the RFCs' suites paragraphs; exit 1 on a difference

```text
dt docs [--check] [flags]
```

Flags:

```text
      --check[=true|false]   only check the suite table, the command reference and the RFCs' suites paragraphs, and exit 1 on a difference
```

### `dt doctor`

Which needs this machine meets, and how to meet the rest

```text
dt doctor
```

### `dt e2e`

dt run e2e

```text
dt e2e [ARGS...]
```

### `dt history`

Pass, fail, skip and flaky counts over past runs, with p50 and p90 durations: per suite, per test of SUITE, and each result of TEST

```text
dt history [SUITE[/TEST]] [flags]
```

Flags:

```text
      --branch string     with --ci, the branch whose runs of tests.yml are read (default "develop")
      --ci[=true|false]   read the last N runs of tests.yml on --branch, from their artifacts through the GitHub API, with the token in GITHUB_TOKEN, instead of this checkout's runs
      --last int          read the last N runs (default 10)
```

### `dt history diff`

Which cells and tests took longer or shorter between two runs of tests.yml, by workflow run ID, or between the older and the newer half of the last N on --branch, each test's median

```text
dt history diff [A B] [flags]
```

Flags:

```text
      --branch string    without run IDs, the branch whose runs of tests.yml are compared (default "develop")
      --floor duration   leave out tests whose time changed by less than this (default 100ms)
      --last int         without run IDs, compare the median of the older half of the last N runs with the newer half's (default 10)
      --top int          show at most N tests, the ones that took longer first; 0 shows all (default 30)
```

### `dt hooks`

Run git's hooks through the harness, on this machine or in the dtc container

```text
dt hooks [ARGS]
```

### `dt integration`

dt run integration

```text
dt integration [ARGS...]
```

### `dt known`

Work on the suites' known files

```text
dt known
```

### `dt known baseline`

Write a cell's results, the latest run's by default, as its baseline file (smbtorture's)

```text
dt known baseline SUITE/CELL [RUN]
```

### `dt known import`

Convert a Markdown known-failure list into a known file, once

```text
dt known import FILE.md KNOWN.yaml [flags]
```

Flags:

```text
      --where string   AXIS=V,...: where the list's entries apply
```

### `dt known prune`

Remove the known entries that matched no failing or skipping test in the runs dt history reads

```text
dt known prune SUITE [flags]
```

Flags:

```text
      --branch string     with --ci, the branch whose runs of tests.yml are read (default "develop")
      --ci[=true|false]   read the last N runs of tests.yml on --branch, from their artifacts through the GitHub API, with the token in GITHUB_TOKEN, instead of this checkout's runs
      --last int          read the last N runs (default 10)
```

### `dt list`

Suites, options and groups, with their tier, format and needs, and whether they can run here

```text
dt list [flags]
```

Flags:

```text
      --json[=true|false]   print JSON
      --tier string         only the suites tier T runs
```

### `dt plan`

The cells a tier runs, each with its place; with --estimate, the tier's runner-minutes from dt history

```text
dt plan [flags]
```

Flags:

```text
      --base string             compare the branch with REF: a docs-only change plans nothing, and suites with paths run only when touched
      --branch string           with --ci, the branch whose runs of tests.yml are read (default "develop")
      --ci[=true|false]         read the last N runs of tests.yml on --branch, from their artifacts through the GitHub API, with the token in GITHUB_TOKEN, instead of this checkout's runs
      --estimate[=true|false]   give each cell its p50 over the runs dt history reads, and the tier's runner-minutes, their sum
      --json[=true|false]       print JSON
      --last int                read the last N runs (default 10)
      --shard string            only shard I of N of the cells, as I/N
      --tier string             the tier to plan: pr, merge, nightly or manual (required)
```

### `dt report`

Show a run, the latest by default: how each cell went, and its new failures; with --post, post them, and with --commit-status, set its commit's status

```text
dt report [RUN...] [flags]
```

Flags:

```text
      --commit-status string    set the commit status NAME on a run of develop's merge or nightly tier: success, failure or error from its exit status, with its test counts, with the token in GITHUB_TOKEN
      --failures[=true|false]   only the new failures
      --format string           text, md or json (default "text")
      --post strings            post the new failures of a tier run of develop, whose planned cells are CELL,..., to the develop-status issue, and give a test failing 2 runs in a row an issue of its own, with the token in GITHUB_TOKEN; a planned cell with no run didn't run
      --results string          where the runs are, relative to the checkout's root (default test/harness/results)
```

### `dt run`

Run suites and write their results and logs; exit 0 when all passed, 1 when one failed, 2 when one didn't run

```text
dt run SUITE|GROUP[/CELL] [--OPTION] [TEST...] | --place P [flags]
```

Flags:

```text
      --ad-dc[=true|false]            run the suite's ad-dc option
      --base string                   compare the branch with REF wherever a command uses {base}, instead of the base dt finds: origin/develop, or in CI the event's
      --changed[=true|false]          where a suite's packages gives patterns, test only the packages with a change committed since the base: every one without a base, when go.mod, go.sum or go.work changed, or when a changed file is in no package
      --ci[=true|false]               an unmet need fails the run instead of skipping the cell, and new failures are annotated
      --cross-protocol[=true|false]   run the suite's cross-protocol option
      --in string                     where the run happens: host, container (dtc) or auto, which moves it to dtc when a cell needs what only the container gives (default auto, host with --ci)
      --json[=true|false]             print the run's results on stdout as dt report --format json does, once the run ends, and the rest on stderr
      --kerberos[=true|false]         run the suite's kerberos option
      --knfsd[=true|false]            run the suite's knfsd option
      --macos[=true|false]            run the suite's macos option
      --no-color[=true|false]         don't color the results, as NO_COLOR set does; without either, they're colored on a terminal
      --nonrace[=true|false]          run the suite's nonrace option
      --only strings                  run only these suites of the group
      --packages strings              test only these packages, where a suite's commands take {packages}: they replace the patterns and --changed's selection
      --parallel int                  run up to N cells at once; cells that conflict, as two on one fixed port, still run one at a time (default 1)
      --place string                  run the cells of the tier that dt plan gives place P, as the dedicated host does; that place's host need is met
      --portmap[=true|false]          run the suite's portmap option
      --results string                where results go, relative to the checkout's root (default test/harness/results)
      --retry int                     run each test that failed new again, up to N times: a pass on a retry is flaky
      --skip strings                  skip these suites of the group
      --smb[=true|false]              run the suite's smb option
      --smb21[=true|false]            run the suite's smb21 option
      --stress[=true|false]           run the suite's stress option
      --tier string                   run at tier T, or at a suite's own tier when wider: it sets args, env and select (default "pr")
      --windows[=true|false]          run the suite's windows option
```

### `dt services`

Start, stop or show the services the suites use: localstack, postgres, kmip

```text
dt services [ARGS]
```

### `dt stack`

Start, stop, show or mount the compose stack: a server with its S3 on Localstack

```text
dt stack [ARGS]
```

### `dt unit`

dt run unit

```text
dt unit [ARGS...]
```

<!-- dt-commands:end -->

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `dt list` or `dt doctor` takes about 30 s | Docker Desktop is stuck. Restart it |
| A cell is skipped, or "not run", on your machine | A need isn't met. `dt doctor` and `dt list` say which and how to meet it; for a Linux need on a Mac, start Docker so `--in auto` can use dtc |
| `--in auto` stays on the host and the cell doesn't run | A service it needs is down on the host (`dt services up localstack`), or it needs a fixed port (`wpts`, `smbtorture`): use `--in container` |
| `clients/macos` doesn't run | No IPv4 address on an interface that is up: connect to a network |
| "nix isn't installed" | On a Linux host the conformance tools come from Nix. Install it, or run in dtc |
| `cthon04` or `nfstest` refused in dtc | They run only on a Linux host, as an ordinary user with passwordless sudo |
| A cell "didn't start" or isn't run after about 5 minutes | Fixture setup (building dfs, starting it, pulling an image, building the KDC) has 5 minutes. Pull the suite's images and warm the Go cache first, then run again |
| A mount is left behind | dt prints the `sudo umount -f -l /tmp/dt...` to run |
| `--changed` tests nothing | It sees committed changes only, against `origin/develop`: commit, and `git fetch origin develop` |
| "passed unexpectedly" | A known failure passes: remove its entry (see [When a test fails](#when-a-test-fails)) |
| "unexpected skip", exit 2 | A test skipped that the known file doesn't expect: fix the environment, or add an expected skip with its reason |
| Repo Checks fails on `dt doctor` | A quarantine entry has expired: fix the test, or renew the entry with its issue |
| Repo Checks fails on `dt docs --check` | The suite table or the command reference is out of date: run `test/harness/bin/dt docs` and commit |
| gofmt differences you didn't make | A Go release other than 1.26: set `GOTOOLCHAIN=go1.26.10` |
| The disk fills up | `test/harness/results/` is never pruned: delete old runs; `dt cleanup --volumes`; `dtc clean` |
| CI fails in a download, the package mirror, the runner's Docker or its disk | Infrastructure, not your change: re-run the job |
| Your pull request left the merge queue | A check failed on the merged result, and auto-merge is off. Find out why, then queue it again |

## Further reading

- [RFC 29: the test harness](rfcs/rfc-29-test-harness.md), the design and its reasons.
- [`testing-windows.md`](testing-windows.md): the manual checklists for real Windows applications, over SMB and NFS.
- [`test/crash/`](../../test/crash/README.md): two rigs, run by hand as root on a Linux VM, that check data lost under DittoFS reads back as an error.
- [`testing-parallel.md`](testing-parallel.md): when a suite may run beside others.
- [`test/suites.yaml`](../../test/suites.yaml): the registry itself.
- [`test/harness/demo/`](../../test/harness/demo/README.md): a ten-step terminal demo of `dt`,
  and the player it runs on.
- Per-suite details: [`test/posix/`](../../test/posix/README.md),
  [`test/nfs-conformance/pynfs/`](../../test/nfs-conformance/pynfs/README.md),
  [`test/nfs-mount/`](../../test/nfs-mount/README.md),
  [`test/smb-conformance/`](../../test/smb-conformance/README.md),
  [`test/scenarios/`](../../test/scenarios/README.md),
  [`test/smbfuzz/`](../../test/smbfuzz/README.md),
  [`test/pcap-corpus/`](../../test/pcap-corpus/README.md), [`bench/`](../../bench/README.md).
