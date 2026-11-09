# CLAUDE.md

Guidance for Claude Code working in this repository. Project details live in `README.md` and
`docs/` — read those for depth. This file captures what *won't* be found there: invariants,
conventions, and playbooks.

## Project

DittoFS is an experimental modular virtual filesystem in Go. NFSv3/v4.0/v4.1 + SMB2/3 servers
in userspace (no FUSE), with pluggable metadata and block stores. **Not production ready.**

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
composition layer over six sub-services: `adapters/`, `stores/`, `shares/`, `mounts/`,
`lifecycle/`, `identity/`. API handlers and protocol adapters both go through it.

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
   metadata.

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
`AuthContext.Identity` silently made `authCacheKey`
(`internal/adapter/nfs/v3/handlers/doc.go`) incomplete: it keyed on share + flavor + uid + gid +
gids, and two Kerberos principals can share that triple because a user record with no UID
defaults to 1000 (`pkg/adapter/identity.go`). The second principal would have been served the
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

- PR descriptions, review comments and replies follow
  `.github/instructions/pr-writing.instructions.md`.
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
