# RFC 16 store cutover: implementation plan (stage 4 of #3119)

Stage 4 of wiring the RFC 16 store into the server (#3119, route B). Route B was decided on 16 Oct:
new installations only. The new store replaces Badger, and a start on v1 data is refused with
"upgrading from v1 is not supported". The stage also covers the FoundationDB release packaging that
#3119's step 3 left waiting for it.

**Status:** draft for review. Owner rif, since 23 Nov.

**Terms.** Every file:line below is on `feat/storage-v2` at 6aaaefe25, where the code lives, and
not on `develop`, where this plan lives. "v1 store" means the old Badger metadata store and
everything that only it used. "The new store" is `pkg/metadata/kvstore` over `kv.KV`. Tn.m is
subtask m of epic n.

## 1. What already exists

The issue's wording, "the new store replaces Badger", is older than most of the code. Much of it is
already done, and what remains is smaller and different:

- **The node already runs on the new store.** `dfs init` and `dfs start` open only `badgerkv`
  (`cmd/dfs/commands/kv.go:39-41`) and hand it to `install.Run` (`init.go:85-99`) and
  `node.New` (`start.go:126-141`). `node.New` opens `kvstore.Open` (`internal/node/node.go:248`)
  and composes the engine (`node.go:270-283`). The harness seeds the same store
  (`test/harness/fixture/server.go:107,148`). Nothing on the branch opens the v1 store.
- **The v1 store is dead code: 340 files and 103,114 lines, in seven directory trees holding 13
  Go packages.** The trees are:
  - `pkg/metadata` (its root package, `MetadataService`): 119 files, 31,339 lines;
  - `pkg/metadata/store/{badger,basestore,internal,metabench}`: 83 files, 18,234 lines;
  - `pkg/metadata/storetest`: 33 files, 13,873 lines;
  - `pkg/metadata/lock`: 57 files, 24,555 lines;
  - `pkg/metadata/acl`: 22 files, 6,700 lines;
  - `pkg/metadata/backup`: 2 files, 339 lines;
  - `pkg/block/gc`, v1's GC over the v1 store: 24 files, 8,074 lines.

  An eighth tree, `internal/sysinfo` (6 files, 262 lines), is imported only by
  `pkg/metadata/store/badger/cache.go`, so it is dead once the store goes.

  `go list` finds no importer outside this set, test files and the `integration`, `e2e` and `fdb`
  tags included. As a trial, all seven trees were deleted in a scratch copy. The result: `go mod tidy`
  drops only `github.com/oklog/ulid/v2`; `CGO_ENABLED=0 go build ./...` and `go vet ./...` are
  clean, with no tag and with each of those tags; and the architecture tests of `internal/vfs`,
  `internal/node`, `pkg/settings`, `adaptertest` and `pkg/block/engine` pass. Those tests name
  `pkg/metadata` only as a path prefix, or in testdata fixtures that they parse without
  type-checking. The precondition from 3 Nov, that v1 metadata waits for the adapters on
  `vfs.Service`, is met: no adapter imports it.
- **v1 data is refused in three places, and none of them says "upgrading from v1 is not
  supported":**
  1. **v1's control-plane database**, beside the bootstrap or at `/data/{controlplane,state}/controlplane.db`.
     `refuseV1` refuses it (`cmd/dfs/commands/preflight.go:15-17,27-32`), and it runs first
     (`start.go:86`).
  2. **v1's pre-journal block layout** (`shares/<name>/{journal,blobs,logs}`).
     `probeV1State` detects it (`internal/node/preflight.go:80-94`), and the start check reports
     `errV1State` (`preflight.go:53,225`).
  3. **A v1 Badger metadata directory at `metadata.badger.path` is not recognised as v1.** It holds
     a `MANIFEST`, so the start check passes it as a present store (`preflight.go:136-137,228-235`).
     `badgerkv.Open` then writes its clock ceiling into it, by a recorded decision
     (`badgerkv.go:71-78`), and `kvstore.Open` answers `ErrNotInitialised`
     (`kvstore/store.go:138-141`). So `dfs start` fails with "store not initialised", which sends
     the operator to `dfs init`. `dfs init` then refuses with `ErrForeignStore`
     (`store.go:99-115`, tested by `TestInitRefusesAV1Store`, `store_test.go:205`). Nothing is
     destroyed, but the operator is told the wrong thing first.

  A v1 bootstrap file is refused earlier still, by the loader's unknown-key rule
  (RFC 13 Appendix C, its v1-key table), before any of the three checks run (`start.go:77-85`).
- **FoundationDB is configured but cannot be selected.** The bootstrap declares
  `metadata.backend: badger|foundationdb`, `metadata.foundationdb.cluster_file` and the three
  `tls.*` files (`pkg/bootstrap/keyspec.go:86-93`, `load.go:47-60`), as RFC 13 Appendix C has
  them. `checkKV` refuses every backend but `badger` (`kv.go:17-19`), and a test pins that
  refusal (`start_test.go:419-425`). Three more gaps:
  - `fdbkv.Options` has no TLS fields (`pkg/metadata/kv/fdbkv/fdbkv.go:28-37`);
  - the loader does not enforce RFC 13's "required with `foundationdb`" for `cluster_file`
    (`keyspec.go:90`, default `""`), and an empty value makes the client fall back to its own
    default cluster file;
  - `node.Preflight` refuses an empty store directory (`internal/node/preflight.go:172-173`),
    although RFC 23 §2.1 says a cluster node's store has none
    ([rfc-23 line 475](../docs/internals/rfcs/rfc-23-control-plane.md#2.1%20The%20node%20builds%20its%20components)).
- **`fdbkv` is gated by `linux && cgo` alone** (`fdbkv.go:1`), and it includes `fdb_c.h` and links
  `-lfdb_c`. So any Linux build of `./...` with cgo needs the FoundationDB client, and every such
  CI job installs it: `tests.yml:388-391`, `codeql.yml:78`, and `foundationdb.yml:56-58` through
  `.github/actions/fdb-client` (7.3.77, amd64 and arm64, digests pinned). On this laptop,
  `go build ./...` outside the nix shell fails on `foundationdb/fdb_c.h: No such file or directory`.
  The client is 7.3 (`apiVersion = 730`, `fdbkv.go:25`; `test/fdb/cluster.sh:20`).
- **The release ships only static builds without cgo.** GoReleaser builds `dfs` and `dfsctl` with
  `CGO_ENABLED=0` for Linux, macOS and Windows, on amd64 and arm64 (`.goreleaser.yml:13-60`). The
  image is Alpine 3.21 (`Dockerfile.goreleaser:4`, `.goreleaser.yml:177-200`), and so is the
  from-source `Dockerfile` (`Dockerfile:28,34`). Packages are deb, rpm, Arch, Homebrew and Scoop.
  The before-hook runs `go test -short ./...` (`.goreleaser.yml:11`) on a runner that installs no
  FoundationDB client (`release.yml:50-63`). Since `ubuntu-latest` has gcc, cgo is on, so the hook
  compiles `fdbkv`'s tests and fails. **The first tag cut from a tree that holds `fdbkv` fails in
  its release job before anything is built.** The flake builds `cmd/dfs` and `cmd/dfsctl`
  (`flake.nix:425-455`); `fdbkv` is in the dev shell only (`flake.nix:299-302`).
- **The docs still describe v1's store:**
  - `docs/guide/configuration.md:775-850`: v1's Badger cache keys, which RFC 13 Appendix C drops;
  - `docs/guide/metadata-store-migration.md`: SQL to Badger;
  - `docs/internals/architecture.md:143-147` and `docs/internals/testing.md:906`;
  - `docs/internals/implementing-stores.md`'s metadata section;
  - `CLAUDE.md:148`, rule 7: "any new metadata backend must pass `pkg/metadata/storetest/`".

## 2. Epics at a glance

| # | Epic | Content | Subtasks | Man-days | Blocks |
| --- | --- | --- | --- | --- | --- |
| 1 | **v1 refused in one sentence** | a v1 Badger store recognised at start and at init; one refusal wording for all three v1 finds | 2 | 2 | — |
| 2 | **Delete the v1 store** | the seven trees and `internal/sysinfo`, `go.mod`, the internal docs, CLAUDE.md and agent skills; the user docs' metadata sections; the v1 block tier (D8) | 4 | 4.75 | — |
| 3 | **FoundationDB selectable in `dfs`** | a `foundationdb` build tag; bootstrap to `fdbkv` options with TLS; the start check without a store directory; init and start on a cluster of their own | 4 | 7 | E4 |
| 4 | **FoundationDB release packaging** | the release hook; an image and two Linux tarballs built with FoundationDB; a smoke test of the image; install docs | 4 | 5 | — |
| | **Total** | | **14** | **18.75** | |

**Plan estimate: 14 subtasks, 18.75 man-days.**

## 3. Dependencies, blockers and the split

- E2's T2.1 is being built now, ahead of the rest of stage 4: nothing in E1, E3 or E4 frees it or
  needs it.
- E1 and E2 are independent, and each can start now. E1's tests build the v1 store from raw keys,
  as `TestInitRefusesAV1Store` does, never from v1's code, so E2 can delete that code in either
  order.
- E3 T3.3 waits for Marco's answer to D4, because it changes `node.Preflight`, RFC 23 §2.1's
  surface. The RFC edit lands on `develop` first and is then cherry-picked.
- E3 T3.3 also needs E1 T1.1. Its row "a key that is not the installation's answers
  unidentifiable" needs T1.1's `Open`: today `Open` answers `ErrNotInitialised` whenever the
  format record is missing (`store.go:139-140`). E1 and E3 may go to different people (below), so
  T1.1 merges first.
- E3 T3.2 touches `pkg/bootstrap`, RFC 13 epic 2's code (Victor), to require `cluster_file`.
  It is agreed with him, or lands as an epic 2 follow-up of his.
- E4 needs E3 T3.1, because the image is built with the tag. The release workflow, GoReleaser, the
  Docker Hub repository and the signing are Marco's (CLAUDE.md, "Releases"). E4 is reviewed by him
  and merges before the first tag cut from a tree holding `fdbkv`, since that tag's release job
  fails today (plan §1).
- If D1 is answered "not in release 1", E3 and E4 move to the cluster stage, and stage 4 is E1 and
  E2 alone, 6.75 man-days.
- **D7, settled in review: an installation owns its whole FoundationDB cluster.** The store opens
  `fdbkv` with no key prefix, so `Init`'s scan (`store.go:108-115`) and T3.3's probe read the whole
  cluster, and any key another user wrote there is foreign. The bootstrap gains no prefix key.
  Why: less surface. A prefix key would need an RFC 13 Appendix C edit first, a loader rule and
  a test row, all for a sharing case no release-1 user has. In exchange, CI's cluster cannot serve
  as an installation: it holds the `kv` and `kvstore` suites' keys under their own prefixes. So
  T3.3's and T3.4's cluster tests run in a job of their own in `foundationdb.yml`, with its own
  `test/fdb/cluster.sh up 1`. It is a separate job, on its own runner, because `cluster.sh` fixes
  the container name `dittofs-fdb1` and port 4500 (`cluster.sh:55-60`), so two clusters cannot
  share a host. A prefix key is a deferral-ledger row.
- The split: E1 and E2 are one person's (rif's). E3 then E4 can be a second person's.

## 4. Decisions for Marco

1. **D1. Does release 1 ship FoundationDB at all?** RFC 0 §1.4 makes the first release "one node, an
   embedded metadata store and no replicas". FoundationDB is the replicated backend of the cluster
   (RFC 15). It is not forbidden on one node: RFC 15 §2.2 refuses only an embedded store with more
   than one node.
   **Recommended: ship it as an opt-in build** (E3, E4). The default stays Badger, and RFC 29's
   conformance runs stay on `badger-s3`. The backend is merged and passes kvstore's conformance
   suite on a real cluster (`foundationdb.yml:66-72`), so the only work left is wiring and
   packaging. Deferring it would leave the bootstrap accepting a value no build opens.
2. **D2. A build tag, or cgo alone?** **Recommended: a `foundationdb` tag** on `fdbkv`, on
   `fdbkvtest`, on `pkg/metadata/kvstore/suite_fdb_test.go` (today `linux && cgo`, importing
   both), and on the file of `cmd/dfs/commands` that opens it. Tagging the packages but not that
   test file would leave `go vet ./...` with cgo failing on "build constraints exclude all Go
   files". Default builds stay static
   with `CGO_ENABLED=0`, as every release artefact is today, and a contributor's `go build ./...`
   no longer needs libfdb_c. The CI jobs that compile `fdbkv` pass `-tags=foundationdb`: the
   FoundationDB workflow (both its `fdbkv` and its `kvstore` runs), `lint-go`'s `go vet -tags=`
   list in `test/suites.yaml`, and CodeQL. The
   unit job's FoundationDB client step goes. A build without the tag refuses
   `metadata.backend: foundationdb` and names the build that opens it.
3. **D3. Which artefacts carry FoundationDB?** **Recommended:**
   - an image, `marmos91c/dittofs:<tag>-foundationdb`, multi-arch, on `debian:bookworm-slim` with
     `libfdb_c` 7.3.77 from the same pinned `.deb` the CI action installs. Apple builds that
     library for glibc only, so Alpine cannot carry it;
   - `dfs-foundationdb_<version>_Linux_{x86_64,arm64}.tar.gz`, checksummed and signed like the
     other archives;
   - no deb, rpm, Homebrew or Scoop package. Apple publishes `foundationdb-clients` only on its
     GitHub releases, so a package could not declare that dependency (see the deferral ledger);
   - macOS and Windows keep only the static build.
4. **D4. How does the start check work without a store directory?** **Recommended:** `Preflight`
   takes the store as one of two things, and `internal/node` exports nothing new. RFC 23 §2.1's
   `decision:` (:515-521) allows `New`, `Start`, `Stop` and `Preflight` only.

   ```go
   // Exactly one of dir and store is set: the Badger store's directory, or the
   // FoundationDB store's opened KV. The other parameters are unchanged.
   func Preflight(ctx context.Context, dir string, store kv.KV, v1Root string, devices []string, clock engine.Clock, opts ...journal.Option) error
   ```

   - **With `dir`**, `Preflight` probes the directory as today (`probeStore`,
     `internal/node/preflight.go:111`).
   - **With `store`**, it calls `kvstore.Open` on it and classifies the answer itself: none is
     present, `ErrNotInitialised` is absent, and `ErrForeignStore` or `ErrFormat` is
     unidentifiable. Any other error refuses the start: `kvstore.Open` also returns transport errors
     and `keyBudget`'s (`store.go:166`). The classification stays in `internal/node`; `cmd/dfs`
     classifies nothing and reads no key.
   - The FoundationDB host opens `fdbkv` once, with the one `fdbkv.Options` T3.2 builds from the
     bootstrap. It hands that KV to `Preflight` and then the same KV to `node.New`. `Preflight`
     never closes it. The host closes it on every return before `node.New` takes it over: a
     refusal, a check cancelled by a signal, and a failure between the check and `New`, such as
     the tracing provider's.
   - On the installation's own cluster (D7), the clock ceiling that open writes
     (`fdbkv.go:101-108`) lands in that installation's keyspace, and it is the one key `Init`
     tolerates. On a foreign cluster, it lands in someone else's keyspace before the check refuses.
     The RFC 23 edit below states this exception, so the code follows the RFC.
   - A `decision:` marker records it where the write happens: the host's open in
     `kv_foundationdb.go`, as Badger's sits on `badgerkv.Open` (`badgerkv.go:71-78`). `Preflight`'s
     KV branch only receives a KV that was already written to. The deferral ledger holds the row
     that would retire the write.
   - `decide` is unchanged.
   - The RFCs are edited in four places, on `develop` before T3.3 and then cherry-picked:
     - §2.1's signature, and the whole `Preflight` description above it (:473-479). That includes
       "before it opens the metadata store's KV", "creates nothing", and the "**(cluster)**: a
       cluster node's store has no directory" sentence, which described the rejected alternative.
       It is rewritten along these lines:

       > Preflight is the start check. Over the store, which is a directory or, on a store with
       > no directory, the KV the host has opened, and the journal devices the bootstrap names,
       > and a pre-journal (v1) layout on a device or at the configured pre-journal root, it
       > refuses naming what it found. It creates nothing, except that opening a store with no
       > directory writes the node's clock ceiling first, also into a foreign cluster it then
       > refuses. A first start meets two of its refusals most often: a store never initialised,
       > and a bootstrap with no journal device.

       The last sentence is kept as it stands.
     - RFC 23 §2 step 2 (:289-293), "before anything opens … and nothing was opened". This is
       false for a host whose store has no directory, which opens the KV first.
     - RFC 23 §2's `run` sketch (:303-309). It calls `node.Preflight(...)` with "nothing was opened:
       the start check refused" (:305-306) and opens the KV only after it, `kv.Open(boot.Store)`
       (:308). For a store with no directory, the sketch moves the open before the check, passes
       the KV to `Preflight`, and closes it on the refusal.
     - RFC 13 §2.2's verification row (`rfc-13-configuration.md:1000` on `develop`, :994 on
       `feat/storage-v2`). It reads "Start a node against an empty metadata store and empty journal
       directories: assert it refuses, creating nothing." Under D7, an empty FoundationDB cluster
       is that empty store, and RFC 16 §4.1 makes the ceiling write necessary, since FoundationDB's
       read version is not a timestamp. The row becomes: "…assert it refuses, creating nothing but,
       on a store with no directory, the node's clock ceiling."

   The alternative is to skip the store probe when there is no directory and let `node.New`'s
   `kvstore.Open` answer. It is rejected: it loses "metadata store lost", which RFC 13 §2.2
   requires when the journals name an installation and the store holds none.
5. **D5. One refusal sentence for v1.** **Recommended:** each of the three v1 finds (plan §1) refuses
   with `refusing to start: <what was found> at <path> is from v1; upgrading from v1 is not
   supported: start a new installation with dfs init on empty storage`. Over a Badger store,
   `dfs init` maps `ErrForeignStore` to the same sentence. Over FoundationDB, v1 never ran, so a key
   with no format record belongs to someone else, and the error keeps `ErrForeignStore`'s own
   wording rather than telling the operator to clear a cluster. A v1 bootstrap keeps RFC 13's
   unknown-key refusal, which names each key's replacement.
6. **D6. Plain names.** RFC 16 §3.2 calls the store `metadata.Init` and `metadata.Open`. The code says
   `kvstore`, because `pkg/metadata` was v1's. **Recommended: not in stage 4.** Renaming
   `pkg/metadata/kvstore` to `pkg/metadata` touches 209 importing files, while RFC 18, 20, 21 and
   23 PRs are open against them. Do it at the cutover, as one mechanical PR once those lanes drain
   (deferral ledger).
8. **D8. Does the v1 block tier go in stage 4 too?** These packages are as dead as the store, and the
   RFC 8 plan's rule 7 calls them v1:
   - `pkg/block` (its root package), `remote`, `remote/memory`, `remote/s3`, `middleware`,
     `middleware/compression`, `middleware/encryption`, `blockcodec`, `syncer`, `carver`, `chunker`
     and `blockstoretest`: 82 files, 11,592 lines;
   - `pkg/netutil` (2 files) and `pkg/schedule` (2 files), which have no importer.

   Outside the block tier itself and the v1 store, nothing on `feat/storage-v2` imports any of
   them. Deleting them also drops `github.com/pierrec/lz4/v4`, which only
   `middleware/compression` imports. The conflict: RFC 8 plan rule 7 ends "v1 is not touched: it is
   deleted at the cutover" (`.planning/rfc-8-journal-only-engine-plan.md:357`), while the 3 Nov
   decision is to delete v1 "as soon as nothing live depends on it". **Answered yes** (Marco,
   2026-11-23): the block tier goes in stage 4, as T2.4 (1.5 md).

## 5. The epics in detail

"Done when" means every named test passes and was seen to fail on its assertion with the fix
reverted (CLAUDE.md, "Verifying a change"). No test uses a double. FoundationDB is tested on a real
one-process cluster (`test/fdb/cluster.sh up 1`), and a test that finds none skips naming
`DITTOFS_FDB_CLUSTER_FILE`, as `fdbkvtest` does.

### Epic 1 — v1 refused in one sentence

**Epic 1 estimate: 2 subtasks, 2 man-days.**

RFC 16 §4.6 (a store is never upgraded by being opened), RFC 13 §2.2 (a start opens what exists
and creates neither), #3119's route B.

- **T1.1 — `kvstore.Open` tells a foreign store from an empty one (1 md).**
  - With no format record, `Open` looks for the first key that is not a clock ceiling. It uses
    one helper, shared with `Init`'s check (`store.go:108-115`). If it finds one, it answers
    `ErrForeignStore`, naming the key, as `Init` does. That is one sentinel, with no v1-specific
    error.
  - A store holding only ceilings still answers `ErrNotInitialised`.
  - Two `decision:` markers become false, because `Open` now refuses where they say it answers
    `ErrNotInitialised`. Rewrite both: `badgerkv.go:71-78` (the ceiling written into a v1 store;
    the write stays, the reasoning changes) and `store.go:99-107` (`Init`'s refusal, now shared
    with `Open`).

  Tests in `kvstore`:
  - `TestOpenRefusesAForeignStore`: for each of `f:1`, `p:x`, `c:y` and `lock:z`, `Open`
    answers `ErrForeignStore`, not `ErrNotInitialised`, and commits nothing;
  - `TestOpenOverCeilingsOnlyIsNotInitialised`.
- **T1.2 — one sentence at start and at init (1 md).**
  - D5's wording goes into `refuseV1` and into `decide`'s `errV1State` branch.
  - On the Badger host only, `dfs start` and `dfs init` map `ErrForeignStore` to it. The
    FoundationDB host keeps `ErrForeignStore`'s wording (D5).

  Tests in `cmd/dfs/commands`:
  - `TestStartRefusesAV1MetadataStore`: a Badger directory holding v1 keys, written as in T1.1,
    at `metadata.badger.path`. `dfs start` fails with the sentence, naming the directory, and the
    v1 keys are unchanged;
  - `TestInitRefusesAV1MetadataStore`: the same for `dfs init`;
  - `TestStartRefusesV1Database` and `TestRefuseV1` (`start_test.go:360,456`), with the new
    wording.

  T3.4 adds the FoundationDB side.

### Epic 2 — Delete the v1 store

**Epic 2 estimate: 4 subtasks, 4.75 man-days.**

RFC 16 §6.1: the store's two suites, `kvtest` for a backend and `kvstoretest` for the store, are
the contract that replaces `storetest`.

- **T2.1 — delete the packages (1 md).** Delete the seven trees of plan §1, holding 13 Go
  packages, and `internal/sysinfo`, whose only importer is the v1 Badger store. Run
  `go mod tidy`, which drops `oklog/ulid/v2`. The `ponytail:` markers inside them go with them,
  since their shortcuts are gone. Done when `dt run unit` and `lint-go` are green, which the trial
  in plan §1 predicts, and the FoundationDB, CodeQL and `combined-tree` workflows pass.
- **T2.2 — internal references (1 md).**
  - `CLAUDE.md`:
    - rule 1 (`:116`) stops sending permission checks to `pkg/metadata/`, naming the filesystem
      service and the store instead;
    - rule 6 (`:145`) names v2's errors in place of `metadata.ExportError`, which no package on
      the branch declares any more;
    - rule 7 (`:148`) says a new backend passes `kvtest`'s and `kvstoretest`'s suites;
    - the `ponytail:` paragraph (`:354`) stops naming `pkg/metadata/store`;
    - `:174` and `:272` stop recommending an "in-memory SQLite control-plane store", since SQLite
      is no longer in `go.mod`.
  - `pkg/settings/imports_test.go:21` drops `pkg/metadata/store` from its store packages.
  - `pkg/metadata/errors/errors.go:6`, the package comment's import graph "errors <- lock <-
    metadata <- store implementations", names the packages that import it now.
  - Agent skills: `.claude/skills/fix-issue/SKILL.md:77` and `.pi/skills/fix-issue/SKILL.md:77` say
    "errors are `metadata.ExportError` values".
  - `test/suites.yaml:346` drops `internal/auth/**`, a v1 path RFC 23's epic 1 already deleted.
  - Docs: `docs/internals/architecture.md`'s metadata section, `implementing-stores.md`'s
    metadata half, `testing.md:906`, `acl-design.md`, `kv-layer-guide.md`, `smb-protocol.md`,
    `contributing.md`, `docs/guide/smb-acl-fidelity.md`, `docs/guide/access-control.md` and
    `.github/instructions/copilot.instructions.md`.
  - `feat/storage-v2`'s own plans, `docs/internals/planning/rfc-14-open-state-plan.md` and
    `rfc-17-vfs-plan.md`.
  - The RFC 23 plan's D13 (`.planning/rfc-23-control-plane-plan.md:712`) says `config show` keeps
    `internal/sysinfo` and `pkg/block`. That holds on `develop`, where `show.go` still imports
    both, and not on `feat/storage-v2`, so D13 is edited in the PR that lands the branch on
    `develop`.
  - The `pkg/metadata/**` paths in `test/suites.yaml:345,370` stay, since they now match the
    new store.

  Done when `dt docs --check` passes and `rg --hidden -g '!.git' 'metadata/(store|storetest|lock|acl|backup)|block/gc'`
  finds nothing outside `.planning/` and the RFCs. `docs/internals/planning/` is not excluded.
- **T2.3 — the user docs' metadata sections (1.25 md).**
  - `configuration.md`'s metadata section becomes Appendix C's four keys, with v1's cache keys
    gone.
  - `metadata-store-migration.md` is replaced by a short "Upgrading from v1" page: not
    supported; what a start says; v1 releases stay available.
  - The metadata parts of `choosing-stores.md`, `faq.md`, `install.md` and `sizing.md` are
    rewritten.
  - The adapter and user-guide parts stay the cutover's, as RFC 20's ledger has them on #3119.
  - `scripts/setup-dittofs-demo.sh` is deleted: it drives only v1's `dfsctl store …` commands,
    which no longer exist.
  - `docker/config.yaml` is rewritten in v2 keys, so `dfs config validate --config
    docker/config.yaml` passes; it now fails on `controlplane.host` and `database.sqlite.path`.
    The compose file's v1 bootstrap (`docker/bootstrap.sh`) stays: see the ledger.
- **T2.4 — delete the v1 block tier (1.5 md).**
  - Delete D8's packages, `pkg/netutil` and `pkg/schedule`, and run `go mod tidy`, which drops
    `pierrec/lz4/v4`.
  - The `unit` suite's `nonrace` option (`test/suites.yaml:90-92`) runs two tests in `./pkg/block/`
    and `./pkg/block/middleware/compression/`. Both go with their packages, so the option goes,
    and RFC 29 §4.1's registry example drops it too.
  - `test/harness/demo/plan-compare.sh:25` names `pkg/block/blockstore.go`; it moves to a path that
    still exists.
  - `pkg/block/engine/imports_test.go:126-129` keeps its refused-import rows only for packages
    that still exist.
  - CLAUDE.md rule 7's block half (`:149-159`): `blockstoretest.RemoteBlockStoreConformance`,
    `remote.RemoteBlockStore`, and `pkg/block/blockstore.go`'s `Meta`, `DurabilityReporter` and
    `IsDurable`.
  - The block-tier references in `docs/guide/faq.md`, `docs/guide/configuration.md`,
    `bench/README.md`, `docs/internals/architecture.md:590`, `docs/internals/contributing.md:250-254`
    and `implementing-stores.md`'s block half.

  Done when `dt run unit` and `lint-go` are green and
  `rg --hidden -g '!.git' 'pkg/block/(remote|middleware|blockcodec|syncer|carver|chunker|blockstoretest)'` finds
  nothing outside `.planning/`, `docs/internals/planning/` and the RFCs: storage-v2's plans
  there, the RFC 8 plan among them, name these packages as what they delete.

### Epic 3 — FoundationDB selectable in `dfs`

**Epic 3 estimate: 4 subtasks, 7 man-days.**

RFC 13 Appendix C (`metadata.backend`, `metadata.foundationdb.*`), RFC 16 §4.1 (the backend
contract) and §3.2 (`Init`, `Open`), RFC 23 §2.1 (`Preflight`, `Config.KV`), RFC 29 §4.1 and §4.8.

Every cluster test of this epic runs in `foundationdb.yml`, named in an anchored `-run` list.
- **The existing suites job** keeps its shared cluster, where each suite works under its own
  prefix.
- **A new installation job** has a cluster of its own (D7). T3.1 creates it; T3.3 and T3.4 add
  to it. Its tests run with `-tags=foundationdb -p 1`, and each clears the whole keyspace in
  `t.Cleanup` with T3.3's `fdbkvtest.ClearAll`, since each test is an installation that owns the
  cluster.

A test that finds no cluster skips, naming `DITTOFS_FDB_CLUSTER_FILE`. None of this epic's tests
lives only in a job that has none.

- **T3.1 — the build tag, the backend switch and the installation job (2 md).**
  - D2's tag goes on `fdbkv`, `fdbkvtest` and `pkg/metadata/kvstore/suite_fdb_test.go`.
  - `openKV` returns `kv.KV` plus `Close`, in place of `*badgerkv.Store`, and dispatches on
    `metadata.backend`; FoundationDB's case lives in `kv_foundationdb.go` under the tag.
  - `checkKV`'s refusal of an empty `metadata.badger.path` (`kv.go:23`) applies to Badger only.
    Today it refuses every FoundationDB bootstrap before `openKV` dispatches.
  - CI: `-tags=foundationdb` on both of `foundationdb.yml`'s package runs, `fdbkv` and `kvstore`,
    in `lint-go`'s vet list and in CodeQL; the unit job's FoundationDB client step goes.
  - CI: the installation job in `foundationdb.yml`, with its own `test/fdb/cluster.sh up 1` and an
    anchored `-run` list over `./cmd/dfs/commands/`.
  - CI: the installation job gets its own copy of the path filter, since the filter is a step
    inside each job (`foundationdb.yml:33-47`). It holds the suites job's paths plus
    `cmd/dfs/commands/**`, `internal/node/**` and `pkg/bootstrap/**`. Those are the host
    (`kv_foundationdb.go`), the start check and `New` that T3.3 and T3.4 drive, and the loader
    T3.2 changes. Without them, a pull request that changes only one of those runs none of the
    job's tests.

  Tests:
  - `TestStartRefusesTheStore`'s "backend not badger" row now expects a build without the tag to
    name the FoundationDB build. Its "no store path" row (`start_test.go:426`) stays, for Badger;
  - `TestCheckKVFoundationDBNeedsNoBadgerPath`, tagged, the first entry in the installation
    job's list;
  - a CI step in the unit job, with no FoundationDB client installed: `CGO_ENABLED=1 go build ./...`
    and `go vet ./...` succeed. It fails today, as plan §1 shows.
- **T3.2 — bootstrap to `fdbkv` options (1.5 md).**
  - `cluster_file` is required with `foundationdb`, refused in the loader's single report, as
    RFC 13 §6 does for every key (with Victor, plan §3).
  - One function in `cmd/dfs/commands` builds the `fdbkv.Options` from the bootstrap:
    `ClusterFile`, the three TLS paths, `NodeID` and `Clock`, the last two as `badgerkv` gets them.
    D4's start-check open and the KV `node.New` gets are the same open, so this one option set
    serves both.
  - `fdbkv.Open` checks the three TLS paths with `os.Stat` before it starts the client's network,
    and refuses a missing one, naming it. Only then does it set them as network options, so the
    check does not depend on which open in the process came first.
  - No key prefix (D7).

  Tests:
  - `pkg/bootstrap`: `foundationdb` with no `cluster_file` is refused naming the key;
  - `fdbkv`: `TestOpenRefusesAMissingTLSFile`, one row per path, needs no cluster: the refusal
    comes before the network starts. It is tagged, since it compiles against libfdb_c, and is
    added to the suites job's anchored list.

  A TLS-enabled cluster is deferred.
- **T3.3 — the start check without a store directory (2 md, after D4).**
  - This implements D4: `Preflight` takes a directory or an opened KV, and classifies the KV's
    `kvstore.Open` answer itself. `internal/node` exports nothing new.
  - The Badger host passes the directory. The FoundationDB host passes its one open KV.
  - The `decision:` marker at the host's open in `kv_foundationdb.go`, not in `Preflight`: over a
    foreign cluster, the open writes this node's clock ceiling before the check refuses, as the
    RFC 23 edit states (D4).
  - The host closes the FoundationDB KV on every return before `node.New`: refusal, a cancelled
    check, and a failure between the check and `New`. `Preflight` never closes it.
    `start.go:104-116`'s comments, "nothing is open yet" and "node.New owns the KV from openKV
    on", are rewritten to match. Tests wrap the KV through the existing `kvKey` hook
    (`start.go:131`), which T3.3 applies right after the open so that `Preflight` gets the wrapped
    KV too.
  - `fdbkvtest.ClearAll` clears the whole cluster's keyspace. `Open` swaps a nil prefix for a
    fresh one, and `clearPrefix` cannot clear an empty prefix (`fdbkvtest.go:28,52`).
  - It adds `./internal/node/` and its names to the installation job's run list.

  Tests in `internal/node`:
  - `preflight_internal_test.go`'s `decide` rows unchanged;
  - `Preflight` rows refusing a call with both or neither of `dir` and `store` set;
  - `TestStartRefusalClosesTheStoreOnFoundationDB`, in `cmd/dfs/commands`, in the installation
    job's list. Over an empty cluster, `dfs start` refuses "not initialised", and a pass-through
    wrapper set through `kvKey` records exactly one `Close` before the command returns. CLAUDE.md
    allows such a wrapper, since the call reaching the store is the requirement here;
  - `TestPreflightOnFoundationDB`, in the installation job's list, with four rows:
    - an empty cluster answers "not initialised";
    - a journal naming an installation over an empty cluster answers "metadata store lost";
    - a key that is not the installation's answers unidentifiable;
    - an initialised cluster passes.
- **T3.4 — init, start and stop on FoundationDB (1.5 md).** In `cmd/dfs/commands`, tagged, added
  to the installation job's list with `./cmd/dfs/commands/`:
  - `TestInitStartStopOnFoundationDB`:
    - `dfs init`;
    - `dfs start` until the node is ready, then SIGTERM, exit zero;
    - a second `dfs init`, refused as finished.
  - `TestStartAndInitRefuseAForeignClusterOnFoundationDB`: a key the installation did not write
    makes both `dfs start` and `dfs init` refuse with `ErrForeignStore`'s wording, never D5's v1
    sentence. Afterwards the cluster's keys are the foreign key plus this node's clock ceiling,
    and nothing else (D4's `decision:`).

  The protocol suites stay on `badger-s3` (D1).

### Epic 4 — FoundationDB release packaging

**Epic 4 estimate: 4 subtasks, 5 man-days.**

RFC 29 §4.8 (CI), CLAUDE.md "Releases". No RFC specifies the release artefacts. D3 is the decision
this epic implements: one image and two Linux tarballs.

- **T4.1 — the release hook and the image (2 md).**
  - The before-hook stays `go test -short ./...`, which passes once T3.1 tags `fdbkv` and
    `suite_fdb_test.go`. Today it fails (plan §1).
  - `Dockerfile.foundationdb`: `golang:1.26-bookworm` with the pinned `foundationdb-clients`
    `.deb` builds `dfs` with `-tags foundationdb`; the runtime is `debian:bookworm-slim` with
    `libfdb_c.so` from the same `.deb`, the non-root user, volumes and health check of
    `Dockerfile.goreleaser`, and `/etc/foundationdb/fdb.cluster` named as a mount.
  - Built by `release.yml` for `linux/amd64` and `linux/arm64`, on native runners
    (`ubuntu-latest`, `ubuntu-24.04-arm`) rather than QEMU, and pushed as `<tag>-foundationdb`.
- **T4.2 — Linux tarballs (1.5 md).**
  - Two jobs in `release.yml`, one per native runner, build `dfs-foundationdb` with cgo and the
    tag. Each tarball holds the binary, README and LICENSE, and a note that libfdb_c 7.3 is
    required.
  - The GoReleaser job `needs:` both jobs and downloads their tarballs before it runs.
    `.goreleaser.yml` names them in `checksum.extra_files` and `release.extra_files`, so the one
    `checksums.txt` GoReleaser writes and cosign signs (`.goreleaser.yml:136-149`) covers them,
    and the release publishes them.

  Done when a dry run on a pre-release tag (`vX.Y.Z-rc.N`, exempt from the changelog check)
  publishes both tarballs, `cosign verify-blob` passes on `checksums.txt`, and `sha256sum -c`
  passes on the tarballs.
- **T4.3 — smoke test of the image (1 md).** A job in `foundationdb.yml` with its own cluster
  (D7), run on PRs that touch `Dockerfile.foundationdb`, `release.yml`, `.goreleaser.yml` or
  `fdbkv`:
  - build the image;
  - start `cluster.sh up 1`;
  - run the image's `dfs init` and `dfs start` against it, as T3.4 does in-process.

  This proves that the image's `libfdb_c` loads and speaks to a 7.3 server. The release job runs
  the same test before it pushes.
- **T4.4 — install and configuration docs (0.5 md).** `install.md` gets the FoundationDB image and
  tarballs, the client requirement (7.3), mounting the cluster file, and that an installation owns
  its whole cluster (D7). `configuration.md` gets the `foundationdb` block from T2.3.
  `CHANGELOG.md` gets its entry when the release is cut, as CLAUDE.md's release steps say.

## 6. Risks and open points

- **The release job fails on the next tag from a tree holding `fdbkv`** (plan §1), until T3.1 or
  T4.1 lands. If a release is cut from `feat/storage-v2` before then, installing the client in
  `release.yml` is a one-step stopgap.
- **Other plans name "stage 4" in its old sense** ("`type: kv`, off by default"):
  - `.planning/rfc-8-engine-epic9.md:80,283` and `rfc-8-engine-epic10.md:80` say "`node.New`:
    stage 4", which RFC 23's node now does (`internal/node/node.go:270-283`);
  - `rfc-8-journal-only-engine-plan.md:1234`'s "every share has a remote" row gives RFC 23's plan
    as owner, with trigger "stage 4";
  - `rfc-14-open-state-plan.md:94`.

  This plan builds none of them. Their owners re-point them, RFC 8's and RFC 23's plans for the
  first two. This plan's PR only lists them.
- **The FoundationDB client is pinned to 7.3** (`apiVersion = 730`). A 7.3 client reaches only a
  7.3 cluster, unless the multi-version client is set up (deferral ledger).

### 6.1 Deferral ledger

| Deferred | Why | Owner | Trigger |
| --- | --- | --- | --- |
| Renaming `pkg/metadata/kvstore` to `pkg/metadata` (RFC 16 §3.2's names), D6 | 209 importing files while four lanes have open PRs against them | stage 7, the cutover (#3119) | the RFC 18, 20 and 23 lanes have no open PR against `feat/storage-v2` |
| A converter from v1 data | route B: new installations only (#3119, 16 Oct) | unowned (its own RFC, none yet), as the RFC 1 and RFC 17 plans' ledgers hold it | demand for one |
| deb, rpm, Homebrew and Scoop packages of the FoundationDB build (D3) | Apple publishes `foundationdb-clients` only on its GitHub releases, so a package cannot depend on it | unowned | `foundationdb-clients` reaches a distribution repository, or a user asks for a package |
| A FoundationDB build for macOS or Windows (D3) | no release-1 user; the Go binding links Apple's per-OS client, which CI does not install | the cluster stage | a cluster node on either OS |
| FoundationDB's multi-version client (`external_client_directory`), to reach a cluster on another minor version | release 1 pins 7.3 end to end | the cluster stage | a cluster on a version other than 7.3 |
| A FoundationDB cluster with TLS in CI (T3.2 tests the options, not a TLS handshake) | `cluster.sh` starts no TLS cluster | RFC 29 harness | `cluster.sh` gains a TLS mode |
| Protocol suites (pjdfstest, pynfs, smbtorture) on a FoundationDB profile (D1) | release 1's conformance profile is `badger-s3` (RFC 29 §4.3) | the cluster stage | a cluster profile is added to `test/suites.yaml` |
| The FoundationDB image in the Kubernetes operator | the operator is RFC 30's, and it drives v1's API today | RFC 30's plan | that plan |
| `metadata.foundationdb.prefix`, so several installations share one cluster (D7) | an installation owns its whole cluster: less surface, and no release-1 user shares one; the key needs an RFC 13 Appendix C edit before any code | RFC 13 (rif) | an operator needing a shared cluster |
| `packages.dfs-foundationdb` in the flake: `buildGoModule` with `tags = [ "foundationdb" ]` and nixpkgs' `foundationdb`, Linux only (D3) | D3 ships one image and two tarballs; nix users build from the dev shell, which already holds the client (`flake.nix:299-302`) | unowned | a nix user asks for the package |
| A `latest-foundationdb` image tag beside `<tag>-foundationdb` (D3) | D3 names one tag per release; a moving tag is a second promise to keep | unowned | a user asks to track the newest FoundationDB image |
| The start check over a FoundationDB cluster without opening it through `fdbkv.Open`, which would retire the clock ceiling it writes into a foreign cluster before refusing it (`fdbkv.go:101-108`, D4) | `Open` raises the ceiling before it returns, so no `Now` precedes a durable ceiling; a read-only open would need its own path through `fdbkv` | unowned | an operator reports the stray key, or the `metadata.foundationdb.prefix` row lands and a cluster can hold other installations |
| The start check reading a Badger store without opening it, which would retire `badgerkv`'s ceiling write over a v1 store (`badgerkv.go:71-78`) | Badger cannot open read-only after an unclean close | unowned | Badger opens read-only after an unclean close |
| The compose file's v1 bootstrap, `docker/bootstrap.sh` and its `docker-compose.yml` service | it drives v1's API; the v2 image needs `dfs init` as an explicit step first | RFC 23's plan, its D13 | the v2 image or compose work |
| Dismantling `pkg/block` completely: `pkg/block/engine` (with `enginetest` and `enginewire`) to `pkg/engine`, and `pkg/block/journal` (with its `internal/` and `journaltest/` trees) to `pkg/journal`, so no `pkg/block` directory remains; the same PR updates every file naming the path (about 177 outside `pkg/block`: Go imports, CLAUDE.md, `test/suites.yaml`, the RFCs and the plans) | once T2.4 lands the level adds nothing, but the move touches every engine and journal import, so it is its own mechanical PR | #3119, a separate PR | T2.4 merged |

## 7. RFC coverage

| Section | Epic |
| --- | --- |
| RFC 16 §3.2, `Init` and `Open` | E1, E3 |
| RFC 16 §4.1, the backend contract | E3 |
| RFC 16 §4.6, the store format and a store never upgraded by opening | E1 |
| RFC 16 §6.1, two suites | E2 |
| RFC 13 §2.2, a start opens what exists | E1, E3 |
| RFC 13 Appendix C, `metadata.*` and the v1-key table | E2, E3 |
| RFC 23 §2.1, `Preflight` and `Config.KV` | E3 |
| RFC 0 §1.4 and RFC 15 §2.2, the single-node profile | E3, E4 (D1) |
| RFC 29 §4.1, §4.3 and §4.8, the registry, tiers and CI | E3, E4 |
