# Contributing to DittoFS

DittoFS is in active development and welcomes contributions! This guide will help you get started with development.

## Table of Contents

- [Getting Started](#getting-started)
- [Development Workflow](#development-workflow)
- [Testing](#testing)
- [Benchmarking](#benchmarking)
- [Common Development Tasks](#common-development-tasks)
- [Areas Needing Attention](#areas-needing-attention)

## Getting Started

### Using Nix (Recommended)

The easiest way to get a complete development environment is using [Nix](https://nixos.org/):

```bash
# Clone repository
git clone https://github.com/cubbit/dittofs.git
cd dittofs

# Enter development shell (installs all dependencies automatically)
nix develop

# Or with direnv (auto-activates when entering directory)
direnv allow

# Set up pre-commit hooks (recommended)
make setup-hooks

# Build and run
go build -o dfs cmd/dfs/main.go
./dfs start
```

The Nix flake provides:
- Go 1.26 with gopls, delve debugger
- golangci-lint for code quality
- NFS utilities for E2E testing (Linux)
- ACL libraries for POSIX compliance testing
- pjdfstest for POSIX compliance testing (Linux)
- pynfs for NFSv4 protocol conformance, as `pynfs-4.0` and `pynfs-4.1`

`nix flake check` builds both binaries.

The binaries also run straight from the flake, without a clone:

```bash
nix run github:cubbit/dittofs#dfs -- start
nix run github:cubbit/dittofs#dfsctl -- --help
```

### Manual Setup (Alternative)

If you prefer not to use Nix, install dependencies manually:

#### Prerequisites

- Go 1.26.10; format with its gofmt, since another release indents some code differently and the lint checks fail
- NFS client tools (for E2E testing)
  - Linux: `nfs-common` package
  - macOS: Built-in NFS client
- Git
- FoundationDB client library, to build with the `foundationdb` tag and to run
  `test/harness/bin/dt run lint-go`, on every OS

#### FoundationDB client

The FoundationDB metadata backend links `libfdb_c`, so it builds only with the
`foundationdb` tag; `go build ./...` and `go test ./...` leave it out and need
nothing. With the tag, `go build -tags=foundationdb ./...` fails with
`foundationdb/fdb_c.h: No such file or directory` until the client is
installed. `dt run lint-go` vets and lints with the tag, so it needs the
client on macOS too, where the Nix dev shell does not provide it: install
the macOS package from FoundationDB's releases.

On Debian or Ubuntu, install the same release CI uses, checked against the
digest CI pins (on ARM, use `aarch64` and the arm64 digest from
`.github/actions/fdb-client/action.yml`):

```bash
v=7.3.77
curl -fLO https://github.com/apple/foundationdb/releases/download/$v/foundationdb-clients_${v}-1_amd64.deb
echo "b6c263723009bddbc30b6ae8cf9854340bd35cef14152abb0da8fb5af2b91254  foundationdb-clients_${v}-1_amd64.deb" | sha256sum -c -
sudo dpkg -i foundationdb-clients_${v}-1_amd64.deb
```

On other x86_64 distributions, `test/fdb/cluster.sh client` downloads the library and
headers under `~/.cache/dittofs-fdb/` without installing anything, and
`test/fdb/cluster.sh test -tags=foundationdb ./...` runs `go test` with them on the cgo paths.
That script also starts a local cluster for the backend's tests
(`test/fdb/cluster.sh up`, then `down`); without one they skip.

### Clone and Setup

```bash
# Clone repository
git clone https://github.com/cubbit/dittofs.git
cd dittofs

# Set up pre-commit hooks (recommended)
make setup-hooks

# Install dependencies
go mod download

# Build
go build -o dfs cmd/dfs/main.go

# Run with development settings
DITTOFS_LOGGING_LEVEL=DEBUG ./dfs start
```

## Development Workflow

### Building

```bash
# Build the main binary
go build -o dfs cmd/dfs/main.go

# Install dependencies
go mod download
```

### Running

```bash
# Run server with defaults (NFS port 12049, INFO logging)
./dfs start

# Run with debug logging and custom settings
DITTOFS_LOGGING_LEVEL=DEBUG ./dfs start

# Use environment variables for quick config overrides
DITTOFS_LOGGING_LEVEL=DEBUG DITTOFS_SHUTDOWN_TIMEOUT=60s ./dfs start
```

This build serves no file protocol: `dfs start` runs only the control-plane
REST API and metrics. Adapter settings already stored in the control-plane
database are kept; `DITTOFS_ADAPTERS_*` variables do not configure adapters.

### Pre-commit Hooks

DittoFS includes pre-commit hooks that automatically check formatting and run static analysis before each commit. Set them up once after cloning:

```bash
make setup-hooks
```

The hook checks `gofmt -s` on staged Go files, then runs `go vet ./...` and `golangci-lint run` (if installed) across the full repository. Use `git commit --no-verify` to skip when needed (e.g., WIP commits).

`test/harness/bin/dt hooks install --container` runs the same hooks in the dtc Linux container instead, so the machine needs no Go toolchain, and `dt hooks uninstall` goes back to `.githooks`. Both set `core.hooksPath` without keeping a custom value you had, and `make check-hooks` accepts only `.githooks`, so it fails in container mode.

On a branch stacked on another branch, set `DITTOFS_PREPUSH_BASE=origin/<epic>` for the pre-push hook and pass `dt run --base origin/<epic>`.

### Linting and Formatting

```bash
# Format code
make fmt          # or: gofmt -s -w .

# Static analysis
make vet          # or: go vet ./...

# Run linters (if golangci-lint is installed)
make lint         # or: golangci-lint run

# Check MS-FSCC / MS-FSA / MS-SMB2 citations in comments against the spec
# section maps vendored under test/spec-citations/
go run ./test/spec-citations
```

See `test/spec-citations/README.md` for what that check does and does not decide, and for how
the section maps are refreshed when a spec revision is bumped.

### Vulnerability checks

Run `make vulncheck` from the repository root to scan both Go modules with a
pinned version of [govulncheck](https://go.dev/doc/security/vuln/). Both scans
run even if one fails; the command returns nonzero for findings or scan errors.
Use `make vulncheck-main` or `make vulncheck-operator` to scan one module.

This is an opt-in check and does not change CI or Git hooks. It needs network
access to download the scanner and query the Go vulnerability database, and
can use several GiB of memory. Results depend on the active Go toolchain,
platform, build settings, and current database; tests are not included in the
scan. Check `go version` when a report identifies standard-library issues.
Both modules' `toolchain` line selects Go 1.26.10, so a report of
standard-library issues usually means an older toolchain is active.

## Testing

[docs/internals/testing.md](testing.md) is the guide: the harness, what each suite needs, how to run
and read a run, CI, and how to write a test. Before you write or change one, read its "Writing tests"
section and "Writing tests" in [CLAUDE.md](../../CLAUDE.md).

## Benchmarking

On storage-v2, `tests.yml` drops `bench-smoke` and `fio-verify`, as it does every suite that
hasn't moved to storage-v2 yet. Go's `-bench`, `dfsbench`'s full runs and the `manual` tier's
`engine-bench`, `reconcile-bench` and `version-limit-probe-bench` are run by hand (RFC 29 §4.3).
[`bench/README.md`](../../bench/README.md) covers how to run `dfsbench` and the components'
`go test -bench`.

To compare a change, run the same benchmark with `-count=10` before and after it on one machine
and compare the two with `benchstat`; never against a number from another host. Call
`b.ReportAllocs()` on hot paths.

## Common Development Tasks

### Adding a New NFS Procedure

1. Add handler in `internal/adapter/nfs/v3/handlers/` or `internal/adapter/nfs/mount/handlers/`
2. Implement XDR request/response parsing
3. Build the auth context (NFSv3 handlers call `h.GetCachedAuthContext(ctx)`, which applies share permission and squashing)
4. Delegate business logic to repository methods
5. Add the procedure to its dispatch table (for NFSv3, `NfsDispatchTable` in `internal/adapter/nfs/v3/procedures.go`)
6. Add test coverage

For a complete example, follow READLINK: the handler is `ReadLink` in
`internal/adapter/nfs/v3/handlers/readlink.go`, its XDR codec is in `readlink_codec.go`
next to it, and `handleNFSReadLink` in `internal/adapter/nfs/v3/procedures.go` decodes
the request, calls the handler and encodes the reply.

### Adding a New Store Backend

Business logic (permission checking, locking, quotas) lives in the filesystem service (`internal/vfs`) and the engine (`pkg/block/engine`), which the adapters reach through `vfs.Service` once its file operations land (see [Data path](architecture.md#data-path)).

**Metadata Store:**

The metadata store (`pkg/metadata/kvstore`) is written once, over `kv.KV`
(`pkg/metadata/kv/kv.go`). A new backend implements `kv.KV` and passes
`pkg/metadata/kv/kvtest`'s suite, and the store over it
`pkg/metadata/kvstore/kvstoretest`'s. See
[IMPLEMENTING_STORES.md](implementing-stores.md#implementing-metadata-stores).

**Journal (local tier):**

The local tier is `pkg/block/journal`, one journal per device with a handle per
share; it is not a pluggable backend. Its conformance checks run under the
`journal_conformance` tag (`pkg/block/journal/conformance/`).

**Block Store (Remote):**

1. Implement `pkg/block/remote.RemoteStore` interface. It embeds `RemoteBlockStore`,
   `ChunkReader` and `ChunkSealer`, and adds `HealthCheck`, `Healthcheck`, and
   `Close` — see `pkg/block/remote/remote.go`.
2. Remote stores are shared across shares via ref counting
3. Test with the `RemoteBlockStoreConformance` suite in `pkg/block/blockstoretest/`

See [IMPLEMENTING_STORES.md](implementing-stores.md) for detailed implementation guide.

### Adding a new metadata error code

To add a store error code, declare it in `pkg/metadata/errors/errors.go`, before
`errCodeSentinel` and with a `String` arm, and give it a row in `storeCodes`
(`internal/vfs/errors.go`), which maps it to one of the `pkg/vfs/vfserr` sentinels. An unmapped
code answers `ErrUnavailable`, a retry. The node API answers store codes from its own table in
`internal/node/api/problems.go`; `TestProblemTableIsTotal` fails on a code without a row there.

### Adding a New Protocol Adapter

Adapters receive a runtime reference and **interact with services, not stores directly**.

1. Create new package in `pkg/adapter/`
2. Implement `Adapter` interface:
   - `Serve(ctx)`: Start protocol server
   - `Stop(ctx)`: Graceful shutdown
   - `SetRuntime(rt any)`: Receive the runtime reference (provides access to services)
   - `Protocol()`: Return name
   - `Port()`: Return listen port
   - `Healthcheck(ctx)`: Report health from cheap, already-tracked signals
3. Reach file content only through `vfs.Service`, whose Read, Write and Commit
   reach the engine
4. Register in `cmd/dfs/main.go`
5. Update README with usage instructions

Embed `*adapter.BaseAdapter` for the shared TCP lifecycle (listener bind, accept loop
with backoff, connection tracking, graceful drain) instead of reimplementing it.

`SetRuntime` takes `any` rather than `*runtime.Runtime` on purpose: the adapter
interface must not import the runtime package, which imports the adapters. The
implementation type-asserts:

Example:
```go
// pkg/adapter/smb/adapter.go
type Adapter struct {
    *adapter.BaseAdapter // holds Registry *runtime.Runtime, set by SetRuntime

    config  Config
    handler *handlers.Handler
}

// SetRuntime takes any, not *runtime.Runtime: the Adapter interface lives in
// pkg/adapter, which must not import the runtime package (the runtime imports
// the adapters). BaseAdapter.SetRuntime type-asserts and panics otherwise.
func (a *Adapter) SetRuntime(rtAny any) {
    a.BaseAdapter.SetRuntime(rtAny) // sets a.Registry
    rt := rtAny.(*runtime.Runtime)

    // Protocol-specific wiring: hand the runtime to the components that need it.
    a.handler.Registry = rt
}
```

Adapters therefore reach the runtime through `a.Registry` (or a component they wired in
`SetRuntime`), not through a `rt *runtime.Runtime` field of their own — storing it directly would
reintroduce the import cycle the `any` parameter exists to avoid.

## Areas Needing Attention

### High Priority

- Additional repository backend implementations (Redis, custom)
- Performance optimization and profiling
- Test coverage expansion (especially NFSv4 test coverage)
- Protocol compliance testing
- Kerberos authentication testing

### Medium Priority

- Documentation improvements
- Example applications and tutorials
- Monitoring and observability
- A distributed metadata backend for multi-node deployments (TiKV preferred)

### Future Work

- Advanced caching strategies
- Multi-region replication

## Code Guidelines

### Separation of Concerns

**Protocol handlers should ONLY handle protocol-level concerns:**
- XDR encoding/decoding
- RPC message framing
- Procedure dispatch
- Converting between wire types and internal types

**Business logic belongs in repository implementations:**
- Permission checks (`CheckAccess`)
- File creation/deletion
- Directory traversal
- Metadata updates

Example:
```go
// GOOD: Handler delegates to repository
func HandleLookup(ctx *AuthContext, dirHandle, name string) {
    // Parse XDR request
    // Call repo.Lookup(ctx, dirHandle, name)
    // Encode XDR response
}

// BAD: Handler implements permission checks
func HandleLookup(ctx *AuthContext, dirHandle, name string) {
    attr := getFile(dirHandle)
    if attr.UID != ctx.UID { /* check permissions */ }  // ❌ Wrong layer
}
```

### Error Handling

The store returns `*StoreError` values from `pkg/metadata/errors` (codes `ErrNotFound`,
`ErrNotDirectory`, `ErrAccessDenied`, …). The filesystem service maps them to the
`pkg/vfs/vfserr` sentinels (`ErrNotFound`, `ErrNotDir`, `ErrAccess`, `ErrExist`, `ErrNotEmpty`, …)
in `storeErr` (`internal/vfs/errors.go`), and each adapter maps those to its own status codes.

Log appropriately:
- `logger.Debug()`: Expected/normal errors (permission denied, file not found)
- `logger.Error()`: Unexpected errors (I/O errors, invariant violations)

## CI Workflows

The suites of [`test/suites.yaml`](../../test/suites.yaml) run in
[`tests.yml`](../../.github/workflows/tests.yml), driven by `dt`: which cells each event's tier
runs, its retries, the merge queue and the checks a merge requires are
[docs/internals/testing.md](testing.md#ci)'s.

### Combined-tree checks

`tests.yml` tests one tree per pull request: that head merged into
`develop` as `develop` stood when the matrix ran. The tree that reaches
`develop` after two such pull requests land in sequence is tested by none of
them, and `mergeable` does not cover the gap -- it reports whether the texts
conflict, so two disjoint file lists inside one Go package merge cleanly and
can still fail to compile.

`combined-tree.yml` runs on a schedule and builds those trees. It pairs open
pull requests against `develop` whose Go packages intersect, merges them into a
detached tree, and vets and tests only the shared packages. Nothing is pushed
and no pull request is touched; the result is a job summary. A pair that does
not merge cleanly is reported and skipped rather than failing the run.

The filter is package intersection, never file overlap: file overlap is exactly
the filter that waves through the class of breakage that reaches `develop`.
The exact selection rules, and the ceilings they carry, are documented at the
top of [`.github/scripts/combined-tree.sh`](../../.github/scripts/combined-tree.sh)
-- the one place they can drift out of agreement with the code that applies
them is a second prose copy here.

## Submitting Changes

1. Fork the repository
2. Set up pre-commit hooks (`make setup-hooks`)
3. Create a feature branch (`git checkout -b feature/amazing-feature`)
4. Make your changes
5. Run tests (`go test ./...`)
6. Commit your changes — pre-commit hooks check formatting and lint automatically
7. Push to the branch (`git push origin feature/amazing-feature`)
8. Open a Pull Request

## Getting Help

- Open an issue on GitHub for bugs or feature requests
- Check existing issues for similar problems
- Review the [Architecture](architecture.md) and [FAQ](../guide/faq.md) documentation

## License

By contributing to DittoFS, you agree that your contributions will be licensed under the MIT License.
