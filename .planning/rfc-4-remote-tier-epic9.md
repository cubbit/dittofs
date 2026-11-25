# Epic 9: remote tier v2 E2E green

**Sources:** [RFC 4](../docs/internals/rfcs/rfc-4-remote-tier.md) §7.4's production row and its
paragraph on rows the local emulator cannot hold; §7.2's rule that the fault transport never
answers in the service's place nor pretends a setting; the rows of §7.1 that only a real bucket
can show (§4.7's prefix-revoke half, §4.11's archive-class and archive-tiering open halves, Appendix
C.3's encryption clause on a bucket LocalStack cannot hold); §7.5's benchmarks and its suites
paragraph; §4.10's idle-pool `SHOULD`, which §7.5's steady-load row holds at load; Appendix B, whose
Cubbit DS3 and Scaleway columns this epic measures again and extends. The [RFC index](../docs/internals/rfcs/rfc-index.md), "Test
tiers": "What must not stand in", "A check that did not run is a failure", "Recording results"
and "A proposed target is replaced once, by measurement". [RFC 25](../docs/internals/rfcs/rfc-25-observability.md)
§5.4 (O14), for what the production row's results may not hold. Cited, not owned:
[RFC 29](../docs/internals/rfcs/rfc-29-test-harness.md) §4.3 (tiers and blocking), §4.5 (needs) and
§4.10 (benchmarks); the RFC 2 plan's epic 2, D6, for the shared `fuzz` suite's form. Every citation
is `develop`'s text. The code lands on `feat/storage-v2`, and the paths below are paths there.
**Track:** the team.
**Epic 9 estimate: 7 subtasks, 14.5 man-days.**
**Depends on:** **R4-S3**, the store and codec complete: E3's `s3fault` (`New`, `Inject`,
`StripHeaders`, `FailBatchKey`, `Requests`, `LocalStack`, `FreshBucket` and its record of which
settings LocalStack 4.13.1 holds, D7's signature pin); E4's client (`ClientConfig`, `NewClient`,
`Open`, the connection counters its `export_test.go` reads), its `*_integration_test.go` rows and
`FuzzContentRange`; E5's check inside `Open`, `Health`, and the rows it sends here (its R37, R38
and R42); E6's `codec.Store`, `FuzzHeader`, `FuzzBlock` and `FuzzBody`; E2's `remotetest`. **E7**,
ordered before this epic: nothing of its surface is used here (§9, point 1). **The RFC
2 plan's epic 2**, T2.4: the nightly `fuzz` suite T9.7 adds lines to.
**Blocks:** no other epic of RFC 4's. It feeds RFC 4 Appendix B the production columns that decide
which services are supported (§7.4), and the RFC 3 plan's pool-sizing tool reads the put curve
T9.4 measures (RFC 3 §2.11).

---

## 1. Goal

Epic 9 takes the remote tier past `pr`. The same §7.1 rows that run against LocalStack on every pull
request run, by hand, against Cubbit DS3 and Scaleway; the rows only a real bucket can show run
there too; §7.5's benchmarks run after every merge; and the decoders' fuzz targets run nightly.

**At the end of the epic:**

- `dt run remote-service`, with one service's `DITTOFS_TEST_S3_*` variables set, runs the
  capability check and every §7.1 row v1.1 holds against that service, in a disposable namespace
  of a bucket the runner names, and a fresh bucket per row that needs a bucket setting; without
  the variables it refuses to start, naming them.
- A header-rewriting fault reaches a service that validates SigV4, because the transport signs the
  rewritten request again.
- The prefix-revoke row, the archive-tiering row, the archive-class row on a service found to
  ignore the class header, and the default-encryption row of Appendix C.3 run against a real
  bucket, or record that the service cannot hold the setting.
- Appendix B's Cubbit DS3 and Scaleway columns are measured again from one run each and gain the
  rows this epic measures; neither is marked supported until the nightly row runs.
- `dt run remote-bench` runs §7.5's benchmarks against LocalStack after every merge, each beside
  its raw-client twin, naming service, region, block size and pool size in every result line, and
  fails only on a count: the steady-load connection bound, one request per ranged chunk, and one
  page held by a listing.
- `dt run fuzz` fuzzes `FuzzContentRange`, `FuzzHeader`, `FuzzBlock` and `FuzzBody`.

### 1.1 In scope

The production target, its fresh buckets and the sweep in `s3fault`; re-signing; the switch of E2's, E4's,
E5's and E6's service-agnostic §7.1 rows onto the target; the `remote-service` cell and the
`remote` group; the real-bucket rows; the runs against Cubbit DS3 and Scaleway and their Appendix B
columns; §7.5's benchmarks, the `remote-bench` cell and the steady-load row; the four fuzz targets'
lines in the `fuzz` suite; RFC 4 §7.5's suites paragraph.

### 1.2 Out of scope

- **Running the production row and §7.5's daily run in CI:** the RFC 4 plan's §6.1 ledger row on
  the nightly service row, owned `unowned`, triggered by a credential per service allowed to create
  and delete buckets, or a pool of fresh buckets, existing as CI secrets. This epic leaves both at
  `tier: manual` (R2) and names in the cell what CI would need (T9.3).
- **The rows on settings neither LocalStack nor a measured service holds,** in CI: the §6.1 ledger
  row on §7.1's T rows on a setting LocalStack cannot hold, owned `unowned`.
- **Every `Recheck` row and half:** the §6.1 ledger row owned by RFC 9.
- **A backup location's rows, the object-lock lab rows of §7.4 and their release check:** the
  §6.1 ledger row owned by RFC 26, which this epic's pull request extends to name the lab rows and
  the release check. No v1.1 release offers an immutable location.
- **§7.1's two §4.10 rows:** E8 builds them; T9.3 moves them onto the target once they exist.
- **The syncer's re-get of a corrupt ranged chunk:** the §6.1 ledger row owned by the RFC 3 plan's
  epic 6.
- **Replacing §7.5's proposed targets** with measured values: the §6.1 ledger row on it, owned
  `unowned`, triggered by the first run on the reference box (RFC index, "A proposed target is
  replaced once"), which no cell here runs on.
- **Measuring AWS S3 and MinIO:** their Appendix B columns exist, marked not measured; the RFC 4
  plan's §6.1 ledger row on them, owned `unowned`. The target takes any endpoint, so each is a run,
  not code (§9, point 3).
- **A row on a setting no measured service holds:** the §6.1 ledger row on such rows, owned
  `unowned` (D7).

### 1.3 Names used here

- **E2**, epic 2, conformance suite and test foundations: `remotetest` and the in-memory store.
- **E3**, epic 3, fault transport and LocalStack: `s3fault` and LocalStack over HTTPS in
  `integration`.
- **E4**, epic 4, S3 data path, errors and client.
- **E5**, epic 5, capability check and health.
- **E6**, epic 6, block codec.
- **E7**, epic 7, integration: keys and observability.
- **E8**, epic 8, integration: the client limit, and its two §4.10 rows.
- **E9**, this epic.
- **R4-S3**, store and codec complete: epics 3 to 6 done, every §7.1 and §7.3 row they own green.
- **S3** names the storage service (Amazon S3 and the services compatible with it) everywhere in
  this document, except inside **R4-S3**.

## 2. How the general rules apply

The rules are those of [the RFC 4 implementation plan's §1.5](rfc-4-remote-tier-plan.md#15-general-rules-for-each-epic),
cited by number and title and not copied.

| Rule | Subtasks | How |
| --- | --- | --- |
| rule 1, independent unit tests ([`CLAUDE.md`, "Writing tests"](../CLAUDE.md#writing-tests)) | T9.1, T9.2 | the target's variables are read by a function of a lookup function, table-tested with a map, and the sweep's age by the injected clock (inputs, not doubles); re-signing is a function of a request and credentials, tested on its output. Everything else runs against LocalStack or a real service |
| rule 2, fuzz tests where relevant ([`CLAUDE.md`, "By kind"](../CLAUDE.md#by-kind)) | T9.7 | this epic writes no decoder; it joins E4's and E6's four targets to the nightly `fuzz` suite |
| rule 3, observability follows RFC 25 | T9.3 | no metric, log or span is added. The production row handles real credentials, so its cell greps everything it wrote for the access key, the secret and a signature and fails on a hit (R8), and a real wrong-secret open's error and log are searched the same way (R28) |
| rule 4, profiling | T9.3 to T9.5 | the rows keep E2's to E6's `internal/prof` checks on every target; every benchmark calls `b.ReportAllocs()`; the list benchmark asserts its peak heap |
| rule 5, public interface and internals ([`CLAUDE.md`, "Verifying a change"](../CLAUDE.md#verifying-a-change)) | T9.2, T9.4, T9.5 | re-signing is judged where it is consumed, by a validating service (T9.3's rows on Scaleway); the real-bucket rows assert at `Open` and `Health`, the calls the node and the syncer make, each verdict against the property measured on the same bucket; the steady-load bound is read from the dialer's own counters, not from the requests sent |
| rule 6, known failures shrink | none | no suite used here keeps a `known.yaml`; a row that fails on a real service is a defect to fix, not a listed failure |
| rule 7, v2 ignores v1, and imports only what RFC 0 §1.2 allows | T9.1, T9.2 | `s3fault` keeps importing `aws-sdk-go-v2` alone, its `aws/signer/v4` for re-signing; nothing from `develop`'s `pkg/block`; tests import what they need |
| rule 8, real dependencies for properties of those dependencies ([`CLAUDE.md`, "Writing tests"](../CLAUDE.md#writing-tests)) | T9.3 to T9.6 | this epic is the rule's last clause: the real services. Every setting applied for real on a fresh bucket of the service; a setting the service refuses is recorded as not held, never pretended (D7) |
| rule 9, code comment conventions ([`CLAUDE.md`, "Code comments"](../CLAUDE.md#code-comments)) | T9.2 to T9.5 | `decision:` markers at `sign` (D3, with E3's marker at `StripHeaders` pointed at it), at the `remote-service` entry's tier (D4), at `remote-bench`'s blocking (D6), and at a row skipped because the service does not hold its setting (D7) |
| rule 10, a test must be able to fail ([`CLAUDE.md`, "Writing tests"](../CLAUDE.md#writing-tests)) | all | each test names the break it catches in §6; the steady-load bound is shown failing on a planted idle pool on every run (T9.4), the cell's secret grep on a planted string (T9.3), the counts on a planted second request and page (T9.4) |
| rule 11, a symbol is exported only if the RFC's interface uses it | T9.1, T9.2 | `s3fault` exports `Target`, `Service`, `Target.Production` and `Target.New`, which the rows of E4, E5, E6 and E8 call from their own packages (rule 11's fault-transport clause); the variables' reader, the sweep and `sign` stay unexported |
| rule 12, each subtask creates only what it declares | all | `Target` lands with `FreshBucket` as its first consumer, `Target.New` with re-signing, the `remote_service` tag with its first rows, each suite entry with the tests it runs |

## 3. Requirements and invariants

Every obligation RFC 4, the RFC index and RFC 25 place on this epic, numbered so that subtasks and
tests can cite them.

**The production row**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R1 | Each production service Appendix B measures passes the capability check and §7.1, as far as v1.1 holds it, in a disposable namespace, with a fresh bucket for each row that needs a bucket setting | test row | RFC 4 §7.4 | T9.1, T9.2, T9.3, T9.6 |
| R2 | Until the nightly run holds credentials for a service, the production row runs by hand, so it guards no change meanwhile; the remote tier is under test against a local emulator and at least one real service, the schedule half waiting for those credentials | invariant | RFC 4 §7.4; RFC index, "What must not stand in" | T9.3 |
| R3 | A §7.1 row on a bucket setting the local emulator cannot hold runs against a fresh bucket of a production service, with the setting applied for real | MUST | RFC 4 §7.2, §7.4 | T9.1, T9.5 |
| R4 | A real-bucket row never pretends a setting or a storage class: a setting the service does not hold is recorded and its row skips, and re-signing a rewritten request changes only what the service authenticates, never an answer in its place | MUST NOT | RFC 4 §7.2 | T9.2, T9.5 |
| R5 | A service is supported only once it has a column in Appendix B's service semantics table and passes the nightly row; the row's results go into Appendix B. A run by hand records results and supports no service | MUST | RFC 4 §7.4, Appendix B | T9.6 |
| R6 | A check that did not run is a failure: a skip because a service was absent is reported with its reason, and a check skipped in every tier is moved to a tier that runs it | invariant | RFC index, "A check that did not run is a failure" | T9.1, T9.3, T9.5 |
| R7 | A harness that constructs a component differently from production is never the only path under test: a row reaches a production service through the client the store is built with, as it reaches the emulator | invariant | RFC index, "What must not stand in" | T9.1, T9.3 |
| R8 | No result, log or error the production row leaves behind carries the access key, the secret or a request's signature | MUST NOT | RFC 25 §5.4 (O14); the RFC 4 plan's rule 3 | T9.3 |
| R28 | A wrong-secret open against a production service fails `ErrDenied` of cause `access`, and its error and every log record hold no part of the access key, the secret or the signature the service echoes | MUST NOT | RFC 25 §5.4 (O14); the RFC 4 plan's rule 3; E5's wrong-secret row (its T5.4) | T9.3 |

**Rows only a real bucket shows**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R9 | §4.7: revoke access to the store's own prefix only: both probes of that direction fail although a neighbouring prefix stays accessible | test row | RFC 4 §7.1 | T9.5 |
| R10 | §4.11: a bucket holding a tiering rule with an archive tier whose filter could match the prefix: open fails naming it, on a service that holds such a rule (T); the `Recheck` half is RFC 9's | test row | RFC 4 §7.1, Appendix C.1 | T9.5 |
| R11 | §4.11: every object is written in the class the profile names, never the service's default, and no archive class; a bucket that stores what the check wrote in an archive storage class: open fails naming it (T). It runs where LocalStack holds no archive class: on a service with one, the store configured for it, and on any service found to ignore the class header; each service's reported class is recorded | test row | RFC 4 §7.1, Appendix C.1 | T9.5, T9.6 |
| R12 | Appendix C.3: a bucket default encryption under which the ETag is not the content MD5, set before open on a store whose check chose the ETag: open fails naming it, on a real bucket where LocalStack holds no such default (T); the drift half is RFC 9's | test row | RFC 4 §7.1, Appendix C.1 | T9.5 |

**Benchmarks**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R13 | §7.5's benchmarks run against the local emulator after each merge, and against a real service daily; the daily run is by hand until CI holds credentials | invariant | RFC 4 §7.5, §7.4 | T9.4, T9.6 |
| R14 | Every result records the service, region, block size and pool sizes | MUST | RFC 4 §7.5 | T9.4, T9.6 |
| R15 | Every result is recorded in absolute numbers with the commit, the box, the operating system and each row's concurrency; results from another box are compared only with themselves; a proposed target is replaced only by the first run on the reference box | invariant | RFC index, "Recording results", "A proposed target is replaced once, by measurement" | T9.4, T9.6 |
| R16 | No timed check runs per change: the benchmarks' cell fails on a count, never on a throughput or latency figure | invariant | RFC index, "Test tiers" | T9.4 |
| R17 | Codec encode and decode with no transform: MB/s per core, reported beside plaintext hashing on the same box; the proposed 10% target is read by hand, never asserted (R16) | bound | RFC 4 §7.5 | T9.4 |
| R18 | Put of one full block at 1 to N workers: MB/s at each step, reported beside the same curve measured with raw client calls; the proposed 10% at the saturation point is read by hand, never asserted | bound | RFC 4 §7.5 | T9.4 |
| R19 | Ranged get of one chunk, 4 KiB to 1 MiB: one request per chunk, asserted; p50 and p99 latency reported beside a raw client call of the same size, the proposed 10% read by hand | bound | RFC 4 §7.5 | T9.4 |
| R20 | Whole-block get: MB/s reported beside a raw client call; the proposed 10% read by hand | bound | RFC 4 §7.5 | T9.4 |
| R21 | Put of one block with and without a measuring pass: MB/s of each reported, the gap read by hand | bound | RFC 4 §7.5 | T9.4 |
| R22 | Health probe: exactly one request, reported here and asserted by E5's `Health` row | bound | RFC 4 §7.5; E5's T5.5 | T9.4 |
| R23 | List of 100,000 blocks: one request per page of the caller's `n` (1,000, and 500 through `Small`), reported here and asserted by E4's listing rows; at most one page held, asserted on the peak heap; peak memory reported | bound | RFC 4 §7.5; E4's R28 and R64 | T9.4 |
| R24 | Steady load at a fixed pool size for 10 minutes: connections opened no more than the pool size, plus one per connection the service closed, counted from the transport's close events | bound | RFC 4 §7.5 | T9.4 |
| R25 | Idle connections kept for reuse are sized as the limit, so a connection one request releases is reused by the next rather than reopened; adopted, and held at load by R24's row failing on a smaller idle pool | SHOULD | RFC 4 §4.10 | T9.4 |

**Fuzzing and suites**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R26 | The codec's fuzz targets, and the store's `Content-Range` reader's, fuzz past their seed corpus in the shared nightly fuzz suite; their seeds keep running in `unit` | invariant | RFC 4 §7.5 ("Suites"), §7.1 (its §4.4 rows), Appendix C.3; the RFC 4 plan's rule 2 | T9.7 |
| R27 | §7.5's suites paragraph names the suite that runs each part: the production row, the benchmarks and the fuzzing | invariant | RFC 4 §7.5 ("Suites") | T9.7 |

## 4. Decisions this epic takes

- **D1. One production service per run, named by six variables.** `DITTOFS_TEST_S3_SERVICE` (the
  name recorded with results, `cubbit-ds3`, `scaleway-fr-par`), `_ENDPOINT`, `_REGION`,
  `_ACCESS_KEY_ID`, `_SECRET_ACCESS_KEY` and `_BUCKET`, the bucket whose fresh prefixes are the
  disposable namespaces. With `_ENDPOINT` unset the target is LocalStack; with it set and another
  missing, `Service` fails the test naming the missing one, since a half-set run is a mistake, not
  an absent service. Each service is one run. The credential must be allowed to create and delete
  buckets, for the fresh buckets: the runner's own, by hand, in a project holding nothing else.
  Prior art: the harness's `DITTOFS_TEST_KMIP_*` variables (`test/harness/needs/needs.go`,
  checked); rclone's integration tests take one remote per run from its configuration (from
  memory).
- **D2. Rows choose their service through `s3fault.Service`.** E4's, E5's and E6's §7.1 rows and
  E4's run of E2's suite call `Service(tb)` and build their client from its fields, so one body
  runs on the emulator in `integration` and on a production service in `remote-service` (R1, R7).
  `FreshBucket` creates its bucket on the same target, and its cleanup deletes every object
  version and delete marker before the bucket, since a versioning row leaves versions behind on a
  real service; a cleanup that fails is `tb.Errorf` naming the bucket, so a leak is seen. Rows
  that pin a LocalStack fact keep `LocalStack(tb)`: E3's certificate measurement, its record of
  held settings and D7's signature pin, and E5's rows asserting LocalStack's step 1 and step 3
  choices, whose production equivalents are recorded in Appendix B (T9.6). Rows that only a real
  bucket can run sit behind the `remote_service` build tag, so `integration` never carries a row
  that skips on every run (R6). Prior art: the repository's `engine_conformance` tag for a suite
  with its own cell (checked).
- **D3. A rewritten request is signed again, so the production row runs every row.** E3's
  `StripHeaders` and `FailBatchKey` change headers after SigV4 signing, which a validating service
  refuses as `SignatureDoesNotMatch` (E3's D7), and RFC 4 §7.4 makes no exception for them.
  `Target.New` builds E3's transport with the target's credentials; after an effect rewrites a
  header it signs the request again with `aws/signer/v4`, already in the allowed SDK, at the
  request's own `X-Amz-Date` and over `UNSIGNED-PAYLOAD` (Appendix C.3), so the time does not move
  the signature. It signs on LocalStack too, so `integration` runs the path, and `New` itself
  stays unsigned for D7's pin. A `decision:` marker at `sign` says re-signing changes what the
  service authenticates, never what it decides, so R4 holds; withdraw it if an effect ever
  rewrites a header the service judges the request by. E3's marker at `StripHeaders`, which leaves
  re-signing to this epic, is updated to point at it. Rejected: keeping those rows on the
  emulator, which narrows §7.4 without an amendment and leaves a checksum-validating service's
  refusal of a stripped put unmet. Prior art: `awslabs/aws-sigv4-proxy`, which re-signs forwarded
  requests (from memory).
- **D4. The production row is a manual cell that refuses to start without its variables.**
  `remote-service`, `tier: manual`, `format: exit`, `needs: [docker, "service:localstack"]` so the
  emulator-only rows in the same packages run rather than skip. One `sh` command, as `journal-bench`
  has, since a `go-json` cell rewrites every command as `go test`: it exits 2 naming
  `DITTOFS_TEST_S3_ENDPOINT` when it is unset; sweeps D10's leftovers; runs `go test
  -tags=integration,remote_service -p 1 -timeout=…` over `./pkg/remote/...` with its output
  written to `{out}/remote-service.txt` and printed; then `grep -rlF` over `{out}` for the access
  key, the secret, `Signature=` and `SignatureProvided`, printing only the file names, and fails on a hit (R8). Every
  failure message comes from that `go test` output, so it is in `{out}`. No `cloud:` need, since
  `dt` checks none yet (`needs.go`, checked) and one would make the cell unrunnable by hand; and no
  `service:` entry, which RFC 29 §4.5 keeps for what `dt services` starts. A `decision:` comment on
  the entry names the ledger row that moves it to `nightly`. Prior art: `journal-bench`'s guard
  and its `sh` form (checked).
- **D5. A result names its conditions.** What varies inside a run is a key of the sub-benchmark's
  name, `block=<size>/workers=<n>/impl=<store|raw>`; what is fixed for a run is a configuration
  line Go's benchmark format reads: `go test`'s own `goos`, `goarch` and `cpu`, and `service:`,
  `region:`, `limit:` and `mem:` printed once by the benchmark (R14). `dt` records the commit, the
  operating system, the kernel and the host (R15); the device's own raw figures do not apply to a
  network store. `benchstat -col /impl` reads a row's 10% target from one run. Prior art: Go's
  benchmark data format, its configuration lines and name keys (from memory); `dt`'s `go-bench`
  reader (`test/harness/run/gobench.go`, checked).
- **D6. `remote-bench` blocks after a merge, and fails only on counts.** The RFC 4 plan's outline of
  this epic calls the benchmark cell non-blocking; this epic makes it blocking, because RFC 29 §4.3 says a
  non-blocking cell's failure after a merge posts nowhere, which would leave the steady-load bound
  and the ranged-get count guarding nothing. A count a benchmark breaks is a `b.Fatalf`;
  throughput and latency go to `dt`'s `go-bench` history and never fail the cell (R16). `tier:
  merge`, `format: exit`, `metrics: go-bench`, one test per benchmark group, as
  `journal-bench-data`. Each test's command is `go test -tags=integration -run=^$ -bench=…
  -timeout=…` writing `{out}/{test}.txt`, then fails with exit 2 when no `Benchmark` line was
  written, as `journal-bench` does, so a run where every benchmark skipped is not green (R6); the
  cell's `timeout` sits above the `-timeout`, which `go test` would otherwise hold at ten minutes.
  A `decision:` comment says so; a count that proves noisy on shared runners is a defect in the
  count. Prior art: `engine-bench`'s rule that a count a benchmark breaks fails the cell
  (checked).
- **D7. A setting the service refuses is recorded, and its row skips with the reason.** A real
  bucket row first applies its setting; the service's refusal of the configuration call (`501`,
  `NotImplemented`, an invalid class) is recorded for Appendix B, and the row skips naming the
  service and the setting, never passing (R4, R6). A setting the service accepts is read back, and
  the row records which verdict it reached (refused, or opened because the property was measured
  absent), so a service that accepts and ignores a setting is seen. A row that no measured
  service holds goes to the RFC 4 plan's §6.1 ledger row on rows no measured service holds, which
  this epic's pull request adds and T9.6 names the rows in; it is not left skipping silently. A `decision:` marker says so.
  Prior art: none found.
- **D8. Prefix revoke is a bucket policy on a fresh bucket.** Two stores on neighbouring prefixes of
  one fresh bucket; a policy that allows the test's principal everything on the bucket and denies
  `s3:PutObject`, then `s3:GetObject`, on the first prefix's keys; both probes of that direction
  fail on the first store and pass on the second; the policy deleted, the next probe passes. The
  explicit allow keeps a service that denies whatever a policy does not allow from refusing the
  neighbour and the cleanup; the deny names object keys only, so the credential still deletes the
  policy and the bucket. The row first puts the policy on an empty fresh bucket and checks the
  neighbour still reads, else it skips under D7. Prior art: AWS's prefix-scoped bucket-policy
  examples (from memory).
- **D9. The steady-load row is a benchmark run once, with a planted twin.** `BenchmarkSteadyLoad`
  runs ten minutes at `-benchtime=1x`, `P` workers each putting and getting one block in a loop on
  a client of limit `P`, then asserts E4's dialer counts: opened ≤ `P` + service-started closes
  (R24). The ten minutes is §7.5's measurement window, not a wait for an event, and a comment says
  so. Its twin, `idle=planted`, runs one minute with the client's idle pool cut to `P/2` through
  `export_test.go` and fails unless the bound is broken, so the row shows it can fail on every run
  (R25). The surplus connections the twin's client closes itself are client closes, which E4's
  dialer never counts as the service's, so the twin's breach does not depend on how often the
  service closes. Prior art: Appendix B's connection-pool table, which measured the same
  reopenings (checked).
- **D10. A killed run's leftovers are swept at the next start.** A timeout or a kill skips
  `t.Cleanup`, which on a production service leaves `dittofs-test-*` buckets and namespace prefixes
  in `_BUCKET` behind, billed. The cell's command first deletes `dittofs-test-*` buckets and
  `_BUCKET` prefixes older than the cell's timeout, through an unexported sweep `Service` runs once
  per test binary. Prior art: none found.

## 5. Target tree

```text
pkg/remote/s3/s3fault/
  service.go            T9.1  Target, Service, Production, the variables (D1), the sweep (D10)
  bucket.go             T9.1  FreshBucket on Service's target, versions emptied (E3's file)
  service_test.go       T9.1  the variables' table, FreshBucket and the sweep on the target
  sign.go               T9.2  Target.New, sign (D3)
  sign_test.go          T9.2  the signed-header list after a rewrite
  effect.go             T9.2  StripHeaders' decision: marker pointed at sign (E3's file)
pkg/remote/s3/
  *_integration_test.go T9.3  §7.1 rows on Service(tb) and Target.New (D2)
  secret_test.go        T9.3  the wrong-secret open and the planted string, behind remote_service
  bench_test.go         T9.4  data-path benchmarks, raw twins and the listing, behind integration
  steady_test.go        T9.4  BenchmarkSteadyLoad and its planted twin, behind integration
  export_test.go        T9.4  the raw SDK client on the store's transport, and the idle cap
  realbucket_test.go    T9.5  the rows only a real bucket shows, behind remote_service
pkg/remote/codec/
  bench_test.go         T9.4  encode, decode and plaintext hash
  bench_s3_test.go      T9.4  put with and without a measuring pass, behind integration
test/suites.yaml        T9.3  remote-service, the remote group, remote_service in lint-go's vet
                        T9.4  remote-bench; T9.7 four lines and the timeouts of fuzz
docs/internals/rfcs/rfc-4-remote-tier.md
                        T9.6  Appendix B's columns measured again, and one service's benchmark run
                        T9.7  §7.5's suites paragraph
docs/internals/testing.md  T9.7  regenerated by dt docs
```

## 6. Subtasks

### T9.1 The production target, its fresh buckets and the sweep

**Subtask T9.1 estimate: 1.75 man-days.**

- **Track:** the team. **Depends on:** R4-S3.
- **Covers:** R1, R3, R6, R7.
- **Deliverables:** `service.go` (D1):

  ```go
  // Target is the S3 service a row runs against: a production service named by the
  // DITTOFS_TEST_S3_* variables, or LocalStack when DITTOFS_TEST_S3_ENDPOINT is unset.
  type Target struct {
      Service, Endpoint, Region    string
      AccessKeyID, SecretAccessKey string
      Roots  *x509.CertPool // nil: the system's roots
      Bucket string         // disposable namespaces are fresh prefixes of it
      Admin  *s3.Client     // reaches the service without the fault transport
  }
  func Service(tb testing.TB) Target
  func (t Target) Production() bool // false on LocalStack
  ```

  `FreshBucket` creates its bucket on `Service(tb)`, named `dittofs-test-<random>` since a
  production service's names are global, and empties versions and delete markers at cleanup (D2).
  D10's sweep, unexported, run once per test binary by `Service`. Consumed by T9.2 (the
  credentials), T9.3's rows, T9.4's benchmarks and T9.5's rows.
- **Acceptance criteria:**
  - With `_ENDPOINT` unset, `Service` is LocalStack's endpoint, roots and the bucket E4's rows use.
  - With `_ENDPOINT` set and any other variable empty, the test fails naming that variable.
  - Two `FreshBucket` calls give two buckets on the target; a bucket with versioning on and two
    versions of one key is gone after cleanup.
  - A `dittofs-test-*` bucket older than the cell's timeout is gone after the next `Service` call;
    a younger one, another run's, is kept.
- **Tests:** the variables read by an unexported function of a lookup function, table-tested with
  a map (catches a missing variable taken for LocalStack, R6); `FreshBucket` and the sweep in
  `integration` on LocalStack, the sweep's age taken from the injected clock (kind 1), and in
  `remote-service` on the service (catch a bucket left behind by its versions, one created on the
  wrong endpoint, R3, or a sweep deleting a run still going). No test double.

### T9.2 Re-signing a rewritten request

**Subtask T9.2 estimate: 1 man-days.**

- **Track:** the team. **Depends on:** T9.1.
- **Covers:** R1, R4.
- **Deliverables:** `sign.go` (D3): `func (t Target) New(next http.RoundTripper, fault Fault)
  *Transport`, which is E3's `New` signing again every request an effect rewrote; the unexported
  `sign`, with D3's `decision:` marker; E3's marker at `StripHeaders` pointed at it. Consumed by
  T9.3's rows and T9.5's.
- **Acceptance criteria:**
  - A put signed by the SDK, stripped of `x-amz-checksum-crc32c` and signed again carries an
    `Authorization` whose signed headers omit the stripped one, under the original `X-Amz-Date`.
  - A request no effect rewrote reaches the service with its original signature.
  - On LocalStack, every E3 alteration row passes through `Target.New` as through `New`.
- **Tests:** at the request the next transport receives: the signed-header list after a strip
  (catches a re-sign that keeps the old list) and after `FailBatchKey` with its recomputed
  `Content-MD5`; the untouched request byte for byte. The consumer's half, a validating service
  judging the rewritten request, is T9.3's rows run on Scaleway, which validates signatures.

### T9.3 The §7.1 rows on any service, and the `remote-service` cell

**Subtask T9.3 estimate: 2 man-days.**

- **Track:** the team. **Depends on:** T9.2.
- **Covers:** R1, R2, R6, R7, R8, R28.
- **Deliverables:**
  - E4's, E5's and E6's `*_integration_test.go` rows, and E4's run of E2's suite, on `Service(tb)`
    and `Target.New` (D2, D3), and E8's two §4.10 rows when E8 has landed (otherwise E8 builds
    them on `Service(tb)`); the rows that pin a LocalStack fact keep `LocalStack(tb)`, listed in
    the pull request.
  - A `remote_service` row opening the target with a wrong secret: `ErrDenied` of cause `access`,
    and the access key, the secret and the `SignatureProvided` the service echoes absent from the
    error and from every record of a test `slog` handler (R28), the real-service half of E5's
    scripted row.
  - `test/suites.yaml`: `remote-service` (D4), whose `decision:` comment names the §6.1 ledger row
    that would move it to `nightly` and what CI needs for it (one credential per service allowed
    to create and delete buckets, or a pool of fresh buckets); the `remote` group,
    `[remote-service]`, which T9.4 extends; `remote_service` in lint-go's `go vet -tags=` list.
  Consumed by T9.5, whose rows run in the cell, and T9.6.
- **Acceptance criteria:**
  - `dt run integration` runs the same rows as before against LocalStack, none skipped.
  - `dt run remote-service` without the variables exits 2 naming `DITTOFS_TEST_S3_ENDPOINT`.
  - With a service's variables it runs the switched rows against it, and fails when the access
    key, the secret or `Signature=` appears in any file under `{out}`, naming the file only.
- **Tests:** the switched rows themselves on both targets. The grep is checked by a
  `remote_service` test that logs a planted string through the same `t.Log` path a failing row
  uses, and the cell is run once with that string set as the secret, failing; the run is attached
  to the pull request (catches a grep that misses `go test`'s output, R8). The guard is a command
  of the cell, checked by running it.

### T9.4 §7.5's benchmarks, the steady-load row and the `remote-bench` cell

**Subtask T9.4 estimate: 4.75 man-days.**

- **Track:** the team. **Depends on:** T9.1, T9.3.
- **Covers:** R13 to R25.
- **Deliverables:**
  - `pkg/remote/codec/bench_test.go`: encode and decode under the empty chain beside the plaintext
    hash of the same bytes (R17); `bench_s3_test.go`, behind `integration`: a `codec.Store` put
    with and without a measuring pass (R21).
  - `pkg/remote/s3/bench_test.go`, behind `integration`: put at 1, 2, 4, 8 and 16 workers (R18),
    ranged get of one chunk at 4 KiB, 64 KiB and 1 MiB with p50 and p99 reported (R19),
    whole-block get (R20), each beside its `impl=raw` twin through the SDK client on the store's
    own transport, which `export_test.go` exposes; the health probe (R22). Requests are counted
    from `s3fault`'s record, cleared per op, and reported; the ranged get's one request per chunk,
    which no earlier epic counts, is a `b.Fatalf`, while the probe's and the listing's are already
    E5's and E4's tests in `integration` (E5's `Health` row, E4's R28 and R64).
  - The list of 100,000 blocks (R23): seeded once per package through `Target.Admin`, outside the
    transport, so neither the seeding nor its record inflates the heap; at `n` 1,000 and 500, the
    peak heap during the walk reported and a `b.Fatalf` when it exceeds two pages' worth of
    entries, so "at most one page held" is asserted, not only reported.
  - `steady_test.go` (D9), and `export_test.go`'s idle-pool cap for the planted twin.
  - D5's names and configuration lines, on `Service(tb)`'s target; `test/suites.yaml`'s
    `remote-bench` (D6), `needs: [docker, "service:localstack"]`, tests `codec`, `put`, `get`,
    `list` and `steady`, each at `-count=10` but `steady` at `-benchtime=1x`, writing
    `{out}/{test}.txt` for `dt`'s `go-bench` reader; `remote-bench` joins the `remote` group.
    Consumed by T9.6's run against one service and T9.7's suites paragraph.
- **Acceptance criteria:**
  - `dt run remote-bench` reports every row of §7.5, each result under the service, region, block
    size and workers it ran at (R14).
  - A ranged get sending two requests, or a listing holding more than two pages, fails the cell;
    no throughput or latency figure does (R16, R19, R23); a test whose benchmarks all skipped fails
    with exit 2 (D6).
  - Against LocalStack, ten minutes at `P` workers open at most `P` connections plus the closes
    the service started (R24); the planted twin reports the bound broken, and a twin that sees no
    breach fails the cell (R25).
- **Tests:** the benchmarks; the ranged get's and the listing's `b.Fatalf` seen to fire once on a
  planted second request and a planted retained page before the pull request opens (catches a
  count that reads nothing). The steady-load row and its twin read E4's dialer counters, which
  count what the transport opened, not what the store sent (catch an idle pool smaller than the
  limit, R25, and a counter that drops the service's closes, which would make the twin's breach
  vanish). Under `internal/prof`'s leak checks (rule 4).

### T9.5 The rows only a real bucket shows

**Subtask T9.5 estimate: 2.25 man-days.**

- **Track:** the team. **Depends on:** T9.3.
- **Covers:** R3, R4, R6, R9, R10, R11, R12.
- **Deliverables:** `realbucket_test.go`, behind `remote_service`, each row on a `FreshBucket` and
  failing at once when `Target.Production()` is false, since LocalStack cannot hold its settings:
  - prefix revoke (D8), per direction;
  - an intelligent-tiering configuration with an archive tier over the prefix, then `Open`;
  - one put in the profile's class and the class a get reports, recorded; `Open` on a store
    configured for the service's archive class where it has one; `Open` on a service that reports
    another class than the one put;
  - a default encryption of each kind the service holds (SSE-S3, SSE-KMS), the ETag compared with
    the content MD5, then `Open` where the check chose the ETag.

  A row whose setting the service refuses follows D7, with its `decision:` marker. The rows E3's
  T3.5 found LocalStack holds are not repeated here. Consumed by T9.6.
- **Acceptance criteria:**
  - Both probes of the revoked direction fail with the neighbouring store's passing; deleting the
    policy restores them (R9).
  - Where the service holds the setting, `Open` fails naming the archive tier, the class or the
    encryption, and succeeds when the ETag proves to be the MD5 under that encryption (R10 to R12).
  - A setting the service refuses skips its row naming service and setting; a setting it accepts
    records which verdict the row reached; nothing reports a setting the bucket does not hold (R4,
    R6).
- **Tests:** these rows, at `Open` and `Health`, the calls the node and the syncer make; each
  verdict compared with the property measured on the same bucket, never with a fixed answer
  (catches a judgement right on LocalStack and wrong on the service). No test double.

### T9.6 The runs against Cubbit DS3 and Scaleway, into Appendix B

**Subtask T9.6 estimate: 2.25 man-days.**

- **Track:** the team. **Depends on:** T9.4, T9.5.
- **Covers:** R1, R5, R11, R13, R14, R15.
- **Deliverables:** one `dt run remote-service` per service, in a disposable bucket deleted after,
  and one `dt run remote-bench` against one of them, recorded with service, region, block size,
  pool sizes, commit and box, and replacing no proposed target (R15). RFC 4 Appendix B gains, per
  service, the run's date, SDK version and commit, and rows for what the check chose (integrity
  mechanism and recorded rejection, batch delete), path-style addressing accepted, signatures
  validated, the class a get reports, each setting held, refused or accepted and ignored, and the
  production row passed by hand. No service is marked supported: §7.4 supports one once it passes
  the nightly row, which waits for CI credentials (§9, point 4). A row that no measured service
  holds is named in the §6.1 ledger row on settings LocalStack cannot hold (D7). A row that fails
  on a service is a defect in E4 or E5, fixed through its own issue before the column says it
  passed; that fixing is not in this estimate.
- **Acceptance criteria:**
  - Both services' columns are written from a run whose cell passed, or name the rows that failed
    and the issue that fixes each.
  - Neither column says supported (R5).
- **Tests:** the runs themselves; the RFC edit is checked by review against the run's output,
  which the pull request attaches once the cell's grep has passed on it (R8).

### T9.7 The fuzz targets nightly, and §7.5's suites paragraph

**Subtask T9.7 estimate: 0.5 man-days.**

- **Track:** the team. **Depends on:** T9.3, T9.4; the RFC 2 plan's epic 2, T2.4.
- **Covers:** R26, R27.
- **Deliverables:** four lines of the `fuzz` suite, `go test -run=^$ -fuzz=^<Name>$ -fuzztime=10m`
  for `FuzzContentRange` in `./pkg/remote/s3/` and `FuzzHeader`, `FuzzBlock` and `FuzzBody` in
  `./pkg/remote/codec/`, each preceded by a check that `go test -list` finds its target, so a
  renamed target fails rather than fuzzing nothing, and all placed before the suite's last
  command, which copies crashers into `{out}`; `timeout.test` and `timeout.cell` raised by 40
  minutes each (the RFC 2 plan's D6); RFC 4 §7.5's suites paragraph naming `remote-service` (by
  hand), `remote-bench` (after each merge) and `fuzz` (nightly) in place of "no suite runs yet";
  `dt docs`.
- **Acceptance criteria:**
  - `dt run fuzz` fuzzes each of the four for ten minutes, a crasher landing in `{out}`.
  - A target renamed in a scratch branch fails its line.
  - `dt docs --check` passes.
- **Tests:** none of its own: the targets are E4's and E6's, their seeds already in `unit`;
  `dt docs --check` catches a suite the paragraph names and the registry lacks.

## 7. Order of work

```mermaid
flowchart TD
  RS3(["R4-S3 store and codec complete"]):::sync --> T9_1["T9.1 Production target, sweep"]
  T9_1 --> T9_2["T9.2 Re-signing"] --> T9_3["T9.3 Rows on any service, remote-service"]
  T9_3 --> C1(["after T9.3: the production row runs by hand"]):::sync
  T9_3 --> T9_4["T9.4 Benchmarks, steady load, remote-bench"]
  T9_4 --> C2(["after T9.4: benchmarks after every merge"]):::sync
  T9_3 --> T9_5["T9.5 Real-bucket rows"]
  T9_4 & T9_5 --> T9_6["T9.6 Cubbit DS3 and Scaleway into Appendix B"]
  R2E2(["RFC 2 plan epic 2: the fuzz suite"]):::sync --> T9_7["T9.7 Fuzz lines, suites paragraph"]
  T9_3 & T9_4 --> T9_7
  classDef sync stroke:#2c5aa0,stroke-width:3px,stroke-dasharray:4 2
```

**Checkpoints:**

- After T9.3, the production row runs by hand against any service.
- After T9.4, §7.5's benchmarks run after every merge.

## 8. Suggested pull requests

| PR | Subtasks | Man-days | After | Why together |
| --- | --- | --- | --- | --- |
| 1 | T9.1, T9.2, T9.3 | 4.75 | — | the production target, end to end: every row runs against a real service by hand |
| 2 | T9.4, T9.7 | 5.25 | PR 1 | the tiers past `pr`: benchmarks after every merge, the steady-load bound, fuzzing nightly |
| 3 | T9.5, T9.6 | 4.5 | PR 1 for T9.5, PR 2 for T9.6 | the rows only a real bucket shows, and the Appendix B columns measured from their runs |

A suggestion only: one pull request per subtask works as well. PRs 2 and 3 can be open at once;
PR 3 merges after PR 2, since T9.6 records a `remote-bench` run.

## 9. Open points

1. **Epic 7 was not yet broken down when this was written.** Nothing here reads E7's settings rows
   or its `Config`: the target builds E4's `ClientConfig` from its own variables, which a test may
   do. If E7's document makes `ClientConfig` reachable only through the content wiring, T9.1 builds
   it the way E7's tests do instead.
2. **Which settings each service holds is unmeasured.** From memory, Scaleway offers a `GLACIER`
   class and lifecycle transitions but no intelligent-tiering call, and neither service is known
   to hold a KMS default encryption. R12 runs only where the check chooses the ETag, which
   Appendix B shows only on Cubbit DS3, so it needs Cubbit to hold a default that breaks the ETag.
   A row neither service holds goes to the ledger row D7 names, and its only remaining run is
   against AWS S3 (point 3).
3. **AWS S3 and MinIO.** Appendix B names them the first to measure. The target takes any endpoint,
   so either is one more run with its own variables; MinIO also runs in a container, with no
   credential to hold. Whether to measure them is the user's call: the ledger row this epic's pull
   request adds.
4. **No service is supported in v1.1 without CI credentials.** RFC 4 §7.4 supports a service once
   it passes the nightly row, and the nightly row waits for the ledger row on CI credentials, whose
   owner is `unowned`. Until someone provides one credential per service allowed to create and
   delete buckets, or a pool of fresh buckets, as CI secrets, T9.6's columns record runs by hand
   and mark nothing supported. Either the credentials are provided, or §7.4 is amended to let a
   recorded run by hand support a service for v1.1; this epic does neither, and the pull request
   asks.
5. **LocalStack's keep-alive behaviour under steady load is unmeasured.** If LocalStack closes a
   connection after every reply, the main row passes by the service's closes alone; the planted
   twin still breaches, since its surplus closes are the client's (D9). T9.4 records the service's
   close count with the result.
