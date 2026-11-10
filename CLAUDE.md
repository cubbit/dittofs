# CLAUDE.md

Guidance for Claude Code working in this repository. Project details live in `README.md` and
`docs/` — read those for depth. This file captures what *won't* be found there: invariants,
conventions, and playbooks.

## Project

DittoFS is an experimental modular virtual filesystem in Go. NFSv3/v4.0/v4.1 + SMB2/3 servers
in userspace (no FUSE), with pluggable metadata and block stores; on this branch `dfs` serves
no file protocol (rule 2 below). **Not production ready.**

Two binaries: `dfs` (server, `cmd/dfs/`) and `dfsctl` (REST client, `cmd/dfsctl/`). Both use Cobra.

## Where to look

- `README.md` — install, quick start, feature matrix
- `docs/internals/architecture.md` — full design, diagrams, directory map
- `docs/guide/configuration.md` — config file + env vars (`DITTOFS_*`)
- `docs/guide/cli.md` — full `dfs` / `dfsctl` reference (**generated** — see below)
- `docs/guide/nfs.md`, `docs/guide/smb.md` — user mounting guides (protocol internals in `docs/internals/`)
- `docs/guide/smb-acl-fidelity.md` — SMB Windows-ACL/SD interop matrix (Works/Partial/Unsupported)
- `docs/internals/implementing-stores.md` — metadata/block store contracts
- `docs/internals/debugging.md` — SMB/NFS pcap-diff interop playbook
- `docs/guide/faq.md` — known limitations (ETXTBSY, POSIX gaps, single-node)
- `docs/internals/contributing.md` — dev workflow

## Finding your way around the code

There is a graphify code graph at `graphify-out/`. It is one tool among several,
not a gate you must pass through — route by the *shape* of the question, because
the wrong tool costs either tokens or a round trip, and there is no prize for
using the graph on a question it cannot answer.

| The question | Use |
| --- | --- |
| Who calls `X`? What breaks if I change it? How do `A` and `B` connect? | `graphify query` / `path` / `explain` |
| Where is the identifier `X` — a symbol, error string, flag, `DITTOFS_*` key? | `rg` directly |
| Something spanning many files whose names you don't know yet | an `Explore` / `Agent` subagent |
| Exact line numbers, or verbatim source | `rg` / `Read` — the graph stores no text |

The graph indexes Go source, shell scripts, and `.md` files (the latter as
*document* nodes, so `README.md`, `CLAUDE.md`, `docs/**` and `.planning/**` are
reachable through it). What is **not** in it: `.pi/` and `.claude/`, excluded via
`.graphifyignore` because they are agent tooling rather than project content;
`.tokensave/` and `graphify-out/`, which are already in `.gitignore` (graphify
merges that file, so they need no `.graphifyignore` entry).

**The graph is AST-only.** `graphify update .` extracts symbols and edges; it
runs no model and infers no intent. So a question phrased the way you'd ask a
colleague — "why do cold reads return zeros after a restart" — is matched as the
bare keywords *cold*, *read*, *restart* and comes back as noise. Name a symbol,
a type, or a package, or don't use it. If a query returns junk, that is the
answer: switch to `rg`, don't re-word it three times.

Greps that stay cheap: scope to the package directory rather than the repo root,
`rg -l` first when you only need the file set, `-g '!*_test.go'` when tests are
drowning the signal, and `-B2 -A2` instead of reading the whole file afterwards.

Rebuilding is opt-in per machine: run `graphify update .` after changing code.
`graphify hook install` can automate it with a `post-commit` and a `post-checkout`
hook, but those are untracked local state, so a fresh clone has neither.

`.pi/` and `.claude/` are excluded in `.graphifyignore`; `.tokensave/` is already
in `.gitignore`, which graphify merges.

## Frequent commands

```bash
# Build
go build -o dfs    cmd/dfs/main.go
go build -o dfsctl cmd/dfsctl/main.go

# Unit/integration
go test ./...
go test -race ./...

# E2E (requires sudo + kernel NFS client)
cd test/e2e && sudo ./run-e2e.sh           # all configs
sudo ./run-e2e.sh --s3                      # include S3 via Localstack
sudo ./run-e2e.sh --test TestNFSv4BasicOperations

# Lint
go fmt ./...
go vet ./...

# Regenerate docs/guide/cli.md after changing any command/flag
go run ./cmd/gendocs

# Run server with debug logging
DITTOFS_LOGGING_LEVEL=DEBUG ./dfs start
```

Default NFS port is `12049` (not 2049). Server config lives at `~/.config/dittofs/config.yaml`
(`dfsctl` credentials at `~/.config/dfsctl/config.json`).

`docs/guide/cli.md` is generated from the Cobra command trees by `cmd/gendocs`. Never hand-edit it —
change the command definitions and rerun `go run ./cmd/gendocs`.

## Architecture invariants

The Runtime (`pkg/controlplane/runtime/`) is the single entrypoint for all operations — a
composition layer over its sub-services: `blockstoreprobe/`, `lifecycle/`, `shares/`,
`snapshotsched/`, `stores/`, `trash/`. API handlers go through it.

**Key rules you must preserve when editing:**

1. **Protocol handlers handle only protocol concerns.** XDR/SMB wire encoding, framing,
   dispatch, type conversion. Permission checks, file ops, and directory traversal belong in
   `pkg/metadata/` and the store implementations. Don't inline business logic into handlers.

2. **On this branch the v1 adapters and their share gates are gone.** The NFS and SMB
   adapters, their `AuthContext` builders, and the runtime's share-access, netgroup and
   squash gates were deleted with them; nothing serves a file protocol. Admission is
   `internal/vfs/admit` (RFC 18 and RFC 19), and RFC 20's v2 adapters reach the
   filesystem only through `vfs.Service`. `develop` keeps the v1 description of this rule.

3. **File handles are opaque.** Generated by the metadata store; they encode share identity so
   the Runtime can route. Handlers never parse or interpret them. They must stay stable across
   restarts for persistent backends.

4. **The data path is cut until the cutover.** No share holds a content store: every
   operation that reads or writes file content answers `block.ErrNoContentStore`
   (`pkg/block/errors.go`), which each protocol maps to its generic I/O status, while
   metadata operations and size changes run against the metadata service. The engine is
   `pkg/block/engine` (formerly `enginev2`) over `pkg/block/journal`; after the cutover the
   adapters reach it through `vfs.Service`, never directly. Each share still binds one block
   store config, and its remote store is ref-counted per config across the shares that use it;
   its state directory hangs off `blockstore.journal.path`. Lifecycle is tied to `AddShare` /
   `RemoveShare`.

5. **WRITE validates before it answers:** `metaSvc.PrepareWrite` (permission check, type,
   read-only share) runs first, so a refused write keeps its own status; a zero-count write
   succeeds, and any other answers `ErrNoContentStore` before it breaks a lease or changes
   metadata. On this branch no WRITE handler exists, since the v1 adapters are gone;
   `develop` keeps this rule for v1.

6. **Error codes:** return `metadata.ExportError` values (`ErrNotDirectory`, `ErrNoEntity`,
   `ErrAccess`, `ErrExist`, `ErrNotEmpty`, …). Log expected errors at `Debug`, unexpected at `Error`.

7. **Store contracts** live in `pkg/metadata/storetest/` — any new metadata backend must pass
   that conformance suite. Block stores have exactly one suite,
   `blockstoretest.RemoteBlockStoreConformance`, and it covers only the block-keyed
   `remote.RemoteBlockStore` surface. There is **no hash-keyed suite**: the CAS `block.Store`
   interface and its `BlockStoreConformance` were deleted. The journal and the engine carry their
   own conformance suites (`journal_conformance`, `engine_conformance` tags).

   **`pkg/block` declares no whole-block-store interface.** The tier contracts live with their
   tiers (`remote.RemoteBlockStore`, the journal's own API); what stays in
   `pkg/block/blockstore.go` is what both sides speak — `Meta`, `DurabilityReporter` and
   `IsDurable`. So a method reachable only from tests has nothing holding its signature up: say
   so with a `decision:` marker at the method rather than reintroducing an interface to pin it.

## Writing tests

These rules apply to every new or changed test. Older tests that break them are brought in line
when you touch them, not in a sweep.

**No mocks, no stubs.** Needing one is a design smell: the code under test is coupled to
something it should take as an argument. Restructure the code so the test runs against real
collaborators:

- Put the logic in a function of its inputs, and test that function. Keep the I/O at the edge,
  in a thin layer.
- Take a dependency as a small interface the consumer declares, and test against a **real**
  implementation of it: the memory metadata or block store, `t.TempDir()`, a listener on
  `127.0.0.1:0`, an in-memory SQLite control-plane store.

Only three kinds of test double are allowed:

1. **An injected clock or seeded random source.** These are inputs to the code, not
   collaborators.
2. **A verified in-memory implementation.** It lives with the package that owns the interface,
   and it passes the **same conformance suite** as the production implementation. It may record
   its own outputs, and tests assert on those outputs or on resulting state. A dependency that
   isn't built yet gets its conformance suite first, then the smallest in-memory implementation
   that passes it. The real implementation must pass the same suite when it lands.
3. **Fault injection:** a wrapper around a real implementation, or around kind 2, that fails,
   delays, tears or crashes on cue. A scripted error answer is a fault, so it comes from such a
   wrapper. It may count calls to decide when to fire.

Deterministic simulation is built only from these three, so it is allowed.

**Outputs are not interactions.** A sink that only keeps what the code under test delivered
needs no conformance suite: a `Sink` that stores the events it was given is fine, and asserting
that `Close` was called twice is not. A count that *is* the requirement, such as one batched
read per batch, comes from a metric or from a counter the in-memory implementation exports and
its conformance suite covers, never from a counting fake that answers in the dependency's place;
the recording fake banned below is one that no suite holds to the real implementation. A per-OS
table is data, so test its contents. A tunable such as a sampling rate is an input, passed in
through the constructor.

When the call reaching a dependency is itself the requirement (a sync reaching storage, one
mutating call per request), a wrapper that records each call and passes it through, never
answering in its place, keeps a record a test may assert on. A hook in the code under test
that pauses it, or delivers what it reached, is set per instance from `export_test.go`. A null
implementation that answers every call at once, to measure the caller's own cost, is a load,
not a double: nothing asserts on its answers. RFC 29 §4.11 has the whole rule.

Banned:

- a canned stub written for one test;
- a recording or counting fake used to check which calls were made;
- a double that no conformance suite holds to the real implementation;
- swapping a package-level `var` that replaces behaviour (`serveRuntime = func…`); to capture
  output, inject an `io.Writer` or use `cmd.SetOut` rather than reassigning `os.Stdout`.

These test a program that production never runs. When the double drifts from the real
implementation, the test stays green while guarding nothing.

External systems (a KDC, a domain controller, an LDAP directory, S3) are tested for real at the
integration tier. Their parsing and logic are pure functions with unit tests.

**A test must be able to fail.** Break the code it guards and watch it fail **on its
assertion**. See "Verifying a change" below. A test that has never failed is unverified.

**Synchronise on events, never on time.** Don't use `time.Sleep` to wait for a goroutine,
callback or state change. Wait on a channel, a `sync.WaitGroup` or the condition itself, with
one generous timeout as a backstop. Polling the real condition with a short pause between polls
counts as waiting on it; sleeping a fixed time and then checking does not. A duration the code
under test waits on is advanced on the injected clock, not slept; to show something does not
happen, advance the clock past its deadline and assert. Where no clock can be injected, a bounded
wait is the assertion, and a comment says so. On a loaded CI box, a sleep is long enough until
the day it isn't.

**No fixed ports, paths or global state.** Bind to `127.0.0.1:0`, write under `t.TempDir()`, and clean
up with `t.Cleanup`, so tests run in parallel and on any host.

**Fix a flake, never retry it.** A flaky test is either a race in the code or a race in the
test, and both are bugs. Don't add retries, raise timeouts or skip it to get green. The
harness's own policy (RFC 29 §4.7): no retries on pull requests; on develop and nightly a failure
that passes on retry lets the cell through as flaky, recorded in `dt history` but not alerted.
A flake you can't fix now is quarantined in
`test/harness/quarantine.yaml` with an issue and an expiry date, and the issue fixes it.

**Assertions name what they check.** A failure message says which value was wrong and what was
expected (`got %v, want %v`, naming the field or the operation). Prefer table-driven cases when
several inputs exercise one rule.

### By kind

`test/suites.yaml` defines how CI runs each suite (tags, tiers, flags), and RFC 29 §4.3 says
why; `test/harness/bin/dt` runs one locally. What follows is only what those don't say.
Don't start two suites by hand at once: they collide on ports and mounts. `dt` runs cells in
parallel only where the registry says each one is isolated (`parallel: true`).

**Integration** (`-tags=integration`, `-p 1`) is for a backend the host must provide: a KMIP
server for the key provider, and a real S3-compatible endpoint. Postgres is gone: the stores are
Badger, and FoundationDB for a cluster, so don't add a Postgres test. CI starts no S3 endpoint
yet, so a test that needs one skips in every run and guards nothing until one is wired in; don't
count it as coverage. Badger under `t.TempDir()` and in-memory SQLite are real implementations, and their
tests run untagged. Durability and ordering changes need a test on a real filesystem with
the crash simulated beneath the code under test (RFC 1 §1.3), because a memory backend proves
nothing about fsync or a crash. A backend the host lacks is a skip that names the environment
variable to set; it is reported, never counted as a pass. Each test builds its own state and
leaves nothing behind.

**E2E** (`-tags=e2e`, sudo, a kernel client) is for what only a real mount shows: client
caching, the mount handshake, interop. A rule that a unit or integration test can check belongs
there instead. E2E has no `known` file yet (RFC 29 §4.7), so until it has one a failing e2e test
is fixed, not listed; once it does, a known failure goes there only after CI shows it, with the
fields §4.7 requires.

**Build tags hide a whole file from `go build`, `go vet` and the linter.** A tagged test must
compile under `go vet -tags=…`: a new tag goes into lint-go's list in `test/suites.yaml` in the
same change. Never tag a file only to keep a test out of `go test ./...` when `testing.Short()`
would do.

**Fuzz** every decoder that reads bytes off the wire or the disk: XDR, SMB2 PDUs, on-disk
records. The target asserts an **invariant**: no panic; any length the decoder reads stays inside
the buffer; and what it decodes either matches an independent decoder or re-encodes to bytes
that decode to the same value (padding and reserved fields may differ). A target that only checks that
encode and decode agree passes when both share a bug. Commit every crasher to
`testdata/fuzz/<FuzzName>/`, where it becomes a regression seed that plain `go test` runs.

**Benchmarks** run in the `manual` tier, with only `bench-smoke` on pull requests, non-blocking
(RFC 29 §4.3). They measure only the operation. Do setup before `b.ResetTimer()`, or between
`b.StopTimer()` and `b.StartTimer()`, but remember that Go sizes `b.N` from the timed region
alone, so an expensive untimed setup inside the loop multiplies the run's wall time. Call
`b.ReportAllocs()` on hot paths. Compare with `benchstat` over `-count=10` runs on the same
machine, never against a number from another host or from Docker Desktop, which inflates
database round trips.

## Verifying a change

Two failure modes this codebase has actually hit. Both are invisible to a passing test suite,
so they are checks to run deliberately rather than rules a test will catch for you.

**Test the layer that consumes the data, not the layer that produces it.** A regression test
placed next to the function you changed can pass green while the bug is untouched, because the
data may be dropped again downstream. When a fix carries a field from A to B, the test belongs
at B — the consumer. A fix that set `Identity.SID` where the identity was *built* proved nothing:
the RPC layer copied only UID/GID/GIDs into the handler contexts, and both auth-context builders
then built a fresh identity from those three fields, so the SID was read by nobody. Verify a
regression test by reverting the fix and watching it fail **on the assertion** — a build error
from an unused import is not proof.

**When a change starts populating a field that was always empty, audit the cache keys.** Keys,
dedup keys and equality checks written against the old field set are invisible to `rg` for the
field name, because the bug is in the key rather than the field. Carrying SID/GroupSIDs into
`AuthContext.Identity` silently made the NFSv3 handlers' `authCacheKey` incomplete (in the v1
NFS adapter, on `develop`): it keyed on share + flavor + uid + gid + gids, and two Kerberos
principals can share that triple because a user record with no UID defaulted to 1000. The second principal would have been served the
first's cached identity and inherited its SID-keyed ACE grants — failing **open**, unlike the
missing-SID bug the change set out to fix.

When a gate or check is deliberately narrower than its name suggests, say so at the code site
with a `decision:` marker (below) rather than leaving the next reader to infer the ceiling.

## Code comments

- Comments describe the code's **behaviour** — what it does and why, in terms of
  the code itself. Never reference things external to the code: no issue/PR
  numbers, no CI/runner/OS names, no phase/plan/decision IDs. Those belong in
  commit messages, PR descriptions, or `.planning/`, not in source.

### `ponytail:` markers

A `ponytail:` comment marks a **knowingly-simple implementation**, naming its ceiling and
the upgrade path that would justify replacing it:

```go
// ponytail: a covered read scans O(n) keys once; a hole or nested overlap adds
// a second full scan, O(n log n) sorting and at most O(n) row loads. Upgrade to
// a numeric offset index, which costs migrating the existing decimal keys, when
// a payload's row count makes those per-hole row loads show up in a profile.
func (s *BadgerMetadataStore) GetFileChunkAtOffset(...)
```

They are a **debt ledger, not TODOs**: each one records a decision that was correct at the
time along with the evidence that would overturn it. They appear throughout the codebase,
mostly in `pkg/block/engine`, `pkg/block/journal` and `pkg/metadata/store`.

This is the one sanctioned exception to the "no external references" rule above — the
prefix is what makes the set harvestable as a group. Keep the marker when editing nearby
code; drop it only when the shortcut is actually replaced, not when the comment is merely
reworded.

The word is `ponytail:`, even where a tool or plugin asks for another (the ponytail plugin's
5.x releases write `shortcut:`): one word keeps the ledger harvestable with one search.

### `decision:` markers

The same ledger, one axis over. A `ponytail:` marker says the *implementation* is simpler
than it could be; a `decision:` marker says the *behaviour* is deliberate — most often that
a gate is knowingly not applied:

```go
// decision: a null or guest session is exempt from encryption enforcement
// because it holds no session key and cannot encrypt at all (MS-SMB2 3.3.5.2.9).
// The exemption is worth only what such a session can reach — withdraw it if one
// can ever reach a share holding data.
if sess.IsNull || sess.IsGuest {
```

Write one whenever a check is deliberately skipped, narrower than its name suggests, or
fails open. Name the rule, why it holds, and what evidence would overturn it — an exemption
with no stated ceiling is indistinguishable from a bug, both to a reviewer and to whoever
audits the gate later.

The decision is recorded **at the code site**, not in the issue that asked for it. #2518
asked for exactly the exemption above; #2523 shipped it without stating the reasoning
anywhere, and nobody noticed, because an issue thread is not a thing anyone reads while
changing the line.

## Commits & PRs

- Opening a PR (base, title and description, assignee, auto-merge, stacking) follows
  `.github/instructions/pr-opening.instructions.md`; review comments and replies follow
  `.github/instructions/pr-commenting.instructions.md`.
- Never mention Claude Code, AI tools, or add `Co-Authored-By` lines for AI.
- Keep commit messages concise.
- Sign commits (`git commit -S`) when possible.

## Releases

**Tags live on `main`.** `main` is a linear, strict ancestor of `develop`; releases flow
develop → main. v0.18.0 / v0.19.0 broke this by tagging develop-only and never advancing
`main` (it stuck at v0.17.0 for 152 commits) — don't repeat that.

Steps (X.Y.Z):

1. **Changelog first, as its own PR:** `docs(changelog): describe X.Y.Z` adds a
   `## [X.Y.Z] — YYYY-MM-DD` section to `CHANGELOG.md`, covering user-visible changes
   since the last tag (`git log vPREV..origin/develop`) in the file's existing style.
   Leave out test harness, CI, docs and dependency bumps. `release.yml` puts this section
   at the top of the GitHub release notes (`scripts/release-notes.py`). Without it, the
   notes quietly fall back to the commit list, which is how the file lapsed after 0.13.0.
2. On an up-to-date `develop`, run `scripts/release.sh X.Y.Z`. It refuses unless the tree
   is clean and synced, `main` is an ancestor, the tag is new and the changelog section
   exists (rc versions are exempt from that last check), and it lists any CI run on the
   commit that isn't green. Then it bumps `flake.nix` `version =` (the single source of
   truth; the commit touches flake only), makes the signed `chore(release): vX.Y.Z`
   commit and signed tag, and asks before pushing develop, **fast-forwarding `main`**
   (the step that used to be forgotten) and pushing the tag. The tag push triggers
   `release.yml`; non-rc tags push docker `:latest`.

Notes:
- SSH signing failing with `Couldn't get agent socket` → use the key explicitly, through
  git's env config so the script picks it up:
  `GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=user.signingkey GIT_CONFIG_VALUE_0=$HOME/.ssh/id_rsa.pub scripts/release.sh X.Y.Z`.
- `develop` and `main` are protected; admin bypass lets release pushes through (old unsigned
  commits in the catch-up range show as harmless "violations").
- A flaky SMB-conformance run does not block cutting a tag.
