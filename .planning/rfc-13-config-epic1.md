# Epic 1: configuration v2, the configuration surface

**Sources:** [RFC 13](../docs/internals/rfcs/rfc-13-configuration.md) §2.2 ("A host holds only its bootstrap",
but its scratch paths), §5's classes, §6's refusal as a user reads it, §7's provider and Appendix
B's value conventions, as declarations and as the user surface; [RFC
16](../docs/internals/rfcs/rfc-16-metadata-store.md) §2.3's `Setting`, `Secret` and `SecretOwner` and §3's settings
and secrets methods, as signatures.
**Track:** the team.
**Epic 1 estimate: 7 subtasks, 8.75 man-days.**
**Depends on:** S0, reached: RFC 23's epic 1 has stripped `pkg/config` to the bootstrap loader and
renamed its `Config` to `Bootstrap` (#4334). RFC 23's T2.4, which declares `ControlPlaneTx` for
T1.6, has landed (#4362, D2).
**Blocks:** E2, E3 and E4, which fill the bodies frozen here (S1); the RFC 18 plan's epic 2, which
adds the frozen `pkg/secrets` to identity's `Deps`; RFC 23's T3.10 and T5.4, which call the node
identity's entry points, and its T5.3 and T5.4, which seal through `pkg/secrets`.

---

## 1. Goal

Epic 1 settles how configuration looks to the people who use it, then freezes the code surface
every later epic of configuration v2 fills in. Configuration is the part of DittoFS an operator
touches first: the bootstrap file, the refusals they read and the values they type are agreed and
written into RFC 13 before any of it is built. Nothing the epic declares runs yet: every new body
is a stub, behind a signature that is final.

**At the end of the epic:**

- RFC 13 names every bootstrap key an operator writes, with its type, its default or "required"
  and its environment form, states the one shape every configuration refusal takes, and fixes how
  a duration, a size and a key are written (T1.1);
- `pkg/config` is gone and `pkg/bootstrap` builds in its place, with every importer (T1.2);
- `Bootstrap` holds RFC 13 §2.2's facts but its scratch paths, under the keys T1.1 names, and
  `pkg/bootstrap` exports the node identity's three entry points (T1.3);
- `pkg/secrets` exports the provider of RFC 13 §7, and `pkg/metadata/entity` the `Secret` record
  and its kind and owner (T1.4);
- kvstore's control-plane view has the settings delete, the cross-scope read and the secrets
  methods (T1.5);
- `pkg/settings` exports the classes, the section and its options, the table, the snapshot,
  `Read`, `Get`, `Diff`, `Keep`, `Check` with its `Put` and `Delete` writes, and their errors
  (T1.6);
- each of the three packages' exports is pinned by a test, and every symbol is matched to its
  first caller (T1.7): S1.

### 1.1 In scope

The user surface of configuration, as RFC 13 text; the rename of `pkg/config`; the declarations of
`pkg/bootstrap`, `pkg/settings`, `pkg/secrets` and the kvstore view's settings and secrets methods;
the move of `keyprovider` (D3); the export pins.

### 1.2 Out of scope

- Every body behind a stub: the loader's refusals, the node identity's reads and writes and the
  version gate (E2); sections, the snapshot, `Check` and the diff (E3); sealing, versions and
  pruning (E4).
- The kvstore view's `expect` on writes that have none today: RFC 23's T2.3.
- `dfs config`'s behaviour on the bootstrap and the configuration guide: E2, which implements what
  T1.1 settles.
- The management API's settings and secrets routes and `dfsctl`'s commands: RFC 23's epics 4 and
  6. T1.1 fixes the conventions they inherit (the key, the value, the refusal), not their routes.
- The scratch paths of RFC 13 §2.2 (`backups.spill_path`, `salvage.path`, `events.path`): they
  land with their first reader, RFC 26, RFC 1 and RFC 25.
- The initialise-if-empty option: E6, with its first caller.
- The Kubernetes operator's `internal/controller/config`, its own module, whose comments name
  `pkg/config`: RFC 23's O4 moves it.

### 1.3 Names used here

- **E1**, this epic.
- **E2**, epic 2, bootstrap, node identity and versions: the loader, the refused v1 keys and
  environment names, `dfs config` on the bootstrap, the node identity, the single-node version
  gate. Track A.
- **E3**, epic 3, settings: sections and the table, the effective value, the write check, classes
  and the diff. Track B.
- **E4**, epic 4, secrets: `Secret` records, sealing, versions, rotation and pruning. Track C.
- **E5**, epic 5, **the integration epic**: RFC 23's node and API on this library.
- **E6**, epic 6, E2E green: RFC 13 §9's single-node rows through the API.
- **S0**, RFC 23's epic 1 has stripped `pkg/config`. Reached with #4334.
- **S1**, the configuration surface frozen: this epic has merged.
- **S2**, secrets sealed: reached partway through E4, for the RFC 18 plan.
- **S3**, RFC 23's node reconciles and its write path exists, outside this epic.
- **S4**, RFC 23's `dfs init` exists, outside this epic.
- **RFC 23's T2.4**, `Write`, `ControlPlaneTx` and the audit record, in RFC 23's epic 2.
- **D1** to **D11** are this epic's decisions (§4). A decision of RFC 13's plan is always written
  with the plan named ("RFC 13's plan, D8").

## 2. How the general rules apply

The rules are those of
[RFC 13's plan, §1.5](rfc-13-config-plan.md#15-general-rules-for-each-epic), cited by number and
title and not copied.

| Rule | Subtasks | How |
| --- | --- | --- |
| rule 1, independent unit tests ([`CLAUDE.md`, "Writing tests"](../CLAUDE.md#writing-tests)) | T1.2 to T1.7 | each test runs on real values: the RFC's own appendices parsed from `docs/internals/rfcs/`, real `slog` and `fmt` output, the package's exported set read by `go/types`. No test double, and no internal interface for one to stand in for |
| rule 2, fuzz every decoder of untrusted bytes | none | this epic adds no decoder: the bootstrap file's, the `node-id` file's and a setting value's fuzz targets are E2's and E3's, the `Secret` codec's and the envelope's E4's |
| rule 3, observability follows RFC 25 | none | no series: the stubs serve nothing. Each of RFC 13 §8's series is its owning epic's |
| rule 4, profiling | none | no test here starts a goroutine or opens a store, so the leak check has nothing to find |
| rule 5, public interface and internals ([`CLAUDE.md`, "Verifying a change"](../CLAUDE.md#verifying-a-change)) | T1.3, T1.4, T1.7 | the bootstrap key test reads the keys a user writes, not the Go fields; the redaction test reads what a log line and `%v` print; the export pins read the packages as an importer sees them |
| rule 6, known failures shrink | none | no test lands red, and no suite with a `known.yaml` is touched |
| rule 7, v2 ignores v1 | T1.2, T1.3 | `pkg/config`'s loader and path search are kept and renamed; the only lines `Load`'s body changes are the wiring of a decode hook for `node.id` and the `DITTOFS_JOURNALS` form into its `Unmarshal`, composed with hooks equal to viper's own (today `Load` has no decode hook; E2 replaces the whole decoder with one strict value codec), and `Load`'s `environ` argument, whose body ignores it until E2; `keyprovider` moves unchanged (D3) |
| rule 8, real dependencies for properties of those dependencies | none | no property of a store, a key file or a journal directory is tested here; E2 and E4 test them on real ones |
| rule 9, code comment conventions ([`CLAUDE.md`, "Code comments"](../CLAUDE.md#code-comments)) | T1.4, T1.6 | RFC 13 §7's `decision:` note (two wrapping keys) at `secrets.Role`, the site that declares the two roles; RFC 13's plan's D9 `decision:` note at `secrets.ErrUnproven` and `settings.ErrUnproven`; RFC 13's plan's D11 note is E3's, at `Check`'s body, and T1.6's own note on a secret's rotation sits at `Check`'s declaration |
| rule 10, a test must be able to fail | T1.2 to T1.7 | each test fails on a planted break: a bootstrap key missing from RFC 13's table, a role swapped in the kind table, a `Value` printed by `%v`, an extra export, a symbol with no caller listed |
| rule 11, a symbol is exported only if RFC 13's interfaces use it, or a component declares through it | T1.3 to T1.7 | the surface frozen here is R5 to R18; T1.7 pins each package's exports and lists each symbol's first caller, `ObserveLag` (E3's D5, first caller E5's reconcile) the one export added after the freeze |
| rule 12, each subtask creates only what it declares | all | each package lands with the subtask that declares its whole surface; a stub is declared only where T1.7 names its caller in a later epic or plan (D4); no test helper lands without the test that uses it |

## 3. Requirements and invariants

Every obligation RFC 13 and RFC 16 place on this epic's declarations, and the ones RFC 13's plan
sets for it, numbered so that subtasks and tests can cite them. Where the behaviour behind a
declaration is another epic's, the row says which.

**The user surface**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R1 | Every bootstrap fact an operator sets has one key name, a type, a default or "required", and one environment form, stated in RFC 13 before it is built | invariant | RFC 13 §2.2; RFC 13 plan, E1 | T1.1, T1.3 |
| R2 | A refusal of configuration names the field and the rule, and its source (the file, the variable or the record and its scope). It never prints three kinds of value: an environment variable's, a credential key's, and the value of a key it does not declare or no longer reads (`controlplane.jwt.secret`, a misspelled `pasword:`), since each may be a secret. A credential key is one of Appendix C's credential rows, whose value can be material typed where a path belongs (a PEM pasted into an `api.tls.key_file` fails the path check, and its refusal prints the field, never the PEM), so its value is never printed even where the rule it fails is not about secrets. T1.1 marks in Appendix C the keys whose value no refusal ever prints. Any other value it refuses it prints (`got 70000`), since a setting never holds a secret. E2 and E3 produce the refusals, each with a canary test for each of the three kinds, E2's credential canary a PEM in a `key_file` | MUST NOT | RFC 13 §6 ("A refusal says where, what and why, in one shape", as #4486 amends it), §7 ("Read never returns a secret") | T1.1 |
| R3 | A duration, a size and a setting's key are written one way in every section, so that a value valid in one is valid in all. E3 decodes them | invariant | RFC 13 §6 ("Unknown fields are refused"), Appendix B | T1.1 |

**The bootstrap**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R4 | `pkg/config` is gone and `pkg/bootstrap` builds in its place, with every importer, `dfs config`'s commands among them | invariant | RFC 13 plan, D8 | T1.2 |
| R5 | `Bootstrap` holds RFC 13 §2.2's facts but its scratch paths and the memory limit, each under Appendix C's key (#4486): the node's identity, the metadata store's backend, location and credential files, the journal devices and their paths, the adapters', the API's and the metrics listener's addresses, the API's TLS certificate and key, the log's destination, format, rotation and level, the roles, each role's wrapping key, and the API's local socket path `api.socket` (`dfs.sock` in the runtime directory by default, #4486). No key names the runtime directory itself, which R22's rules give | MUST | RFC 13 §2.2 (its table), Appendix C | T1.3 |
| R6 | A bootstrap fact is not a record field, and a record field is not overridable from the host: `Bootstrap` declares no field a setting also holds. E2 refuses the host's other names | MUST NOT | RFC 13 §2.2 | T1.3, T1.7 |
| R7 | The roles flag names `protocol` and `storage`; both is the only accepted value, which E2 enforces | MUST | RFC 13 §2.2 (roles row); RFC 15 §2.1, §2.2 | T1.3 |
| R8 | Each role names a sealing key and its retired keys (`local.file` and `retired_files`, or `kmip.key_uid` and `retired_key_uids`). `keys.<role>.kind` is required for both roles, and the two roles name two different keys, different as `keyprovider` reports their IDs once loaded, not as paths compare, since a copied key file or a symbolic link to one is the same key (RFC 13's plan, epic 2); one key for both voids the split of RFC 13 §7. E2 refuses a file that breaks either, and `dfs config validate` with it, since it refuses what `Load` refuses and so loads the keys to compare the IDs | MUST | RFC 13 §2.2 (wrapping-key row), §7 ("Two wrapping keys, one per role", "A wrapping key rotates the same way"), Appendix C; RFC 13 plan, epic 2 | T1.3 |
| R9 | The node identity has three entry points: `NodeID`, which reads and compares every copy before the store opens and writes none; `InitNodeID`, for the initialisation step and a recovery import; `WriteMissingNodeIDs`, which writes a missing copy only beside a journal the store records for this node and device. E2 fills them | MUST | RFC 13 §2.2 (node identity row) | T1.3 |
| R21 | An `api.listen` or `metrics.listen` address that is not loopback, without `api.tls.cert_file` and `api.tls.key_file`, refuses the start, whether it comes from the file or the environment. Loopback is `127.0.0.0/8` and `::1` only: `localhost`, `0.0.0.0` and `::` do not count. E2 enforces it, and `dfs config validate` with it | MUST | RFC 13 Appendix C (#4486); RFC 23 §5.3 | T1.1, E2 |
| R22 | The runtime directory that holds `dfs.sock` is `$RUNTIME_DIRECTORY`, else `$XDG_RUNTIME_DIR/dittofs`, else a directory under the state directory that only the process's user owns (mode `0700`), and a caller on that socket is exempt from the sign-in throttle's source count. E2 makes the fallback directory with that mode, and the exemption is RFC 23's | MUST | RFC 13 Appendix C (#4486); RFC 23 §5.3 | T1.1, E2 |

**Secrets**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R10 | `pkg/secrets` exports RFC 13 §7's provider: `Resolve`, `Seal` returning the `Secret` unwritten for its caller's `Write`, `Versions`, `Prune` and `Delete`, each with the kind and owner it checks, `Seal` with its `Meta`, and `Meta` and `Version` | invariant | RFC 13 §7 ("One interface, two providers") | T1.4 |
| R11 | Two roles, `protocol` and `storage`; each secret kind is sealed under the key of the role §7's table gives it, so a kind's role is fixed data, not a caller's choice | MUST | RFC 13 §7 ("Two wrapping keys, one per role") | T1.4 |
| R12 | No log line, metric, `%v` or `dfs config show` carries a secret: an opened `Version` never prints its value, and a `Bootstrap` field that holds a credential is a type that redacts itself | MUST NOT | RFC 13 §7 ("Read never returns a secret") | T1.3, T1.4 |
| R13 | `entity.Secret`, `SecretKind` and `SecretOwner` are RFC 16 §2.3's: ID, kind, version, key ID, owner, sealed value, `KeyVersion` and `HandedOver`; an owner compared whole | MUST | RFC 16 §2.3 | T1.4 |
| R14 | Configuration holds no secret value, only a reference: a section declares each field that holds one, with the kind it must name, and a reference names a secret of that kind and that owner. T1.6 only declares the fields and the option, T1.5 the tracked read of a secret, and E3 checks the reference | MUST NOT | RFC 13 §7 (first paragraph); RFC 13 plan, D10 | T1.5, T1.6 |

**The views**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R15 | kvstore's control-plane view has `DeleteSetting(ctx, scope, key, expect)`, `Secret(ctx, id, version)`, `PutSecret(ctx, s, expect)` and `PruneSecret(ctx, id, below)`, as RFC 16 §3 gives them | MUST | RFC 16 §3 | T1.5 |
| R16 | The view reads every scope's settings in one scan, for reconcile | invariant | RFC 13 plan, §1.1 and the reconcile marker of §4.3 | T1.5 |

**Settings**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R17 | Four binding classes, declared with each setting: live, restart, next write and bound | MUST | RFC 13 §5 (its table) | T1.6 |
| R18 | A section is declared from its component's typed settings, its defaults and its validator, with its scope in one of RFC 13 §3's three forms, its required keys, its write role, its secret references and the tracked check its component hands `Of` (`CheckTx`, D9); `Check` takes the caller's role, a put or a delete, and whether the scope holds content, read by its caller in the write's transaction; `pkg/settings` declares no store interface | invariant | RFC 13 §3 ("How a scope is written"), §4.2, §5.1; RFC 13 plan, §1.3 and D2, and its §4.4 code sketch | T1.6 |

**The freeze**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R19 | `pkg/bootstrap`, `pkg/settings` and `pkg/secrets` export only what RFC 13's interfaces use or a component declares through | invariant | RFC 13 plan, §1.5 rule 11 | T1.7 |
| R20 | Every symbol frozen at S1 is matched to its first caller, in a later epic of RFC 13's plan or in RFC 23's or RFC 18's | invariant | RFC 13 plan, E1 ("The freeze review") | T1.7 |

Key words in the owned sections that this epic does not carry: RFC 13 §2.2's "initialise if
empty **MUST NOT** be on by default" is E6's, with its first caller; §5's five-second bound is E5's;
§5's "**(cluster)**" rule waits for RFC 15; §7's lower-case "must" in its export and key-table
bullets describe RFC 26's and RFC 5's records.

## 4. Decisions this epic takes

- **D1, the user surface is settled in RFC 13, the developer surface here.** What an operator
  writes and reads (the bootstrap keys, the refusal's shape, the value conventions) is RFC text,
  since RFC 13 §2.2 names three bootstrap keys only and every later plan inherits the rest. T1.1
  amends RFC 13 on `develop`, reviewed before any of it is built. How a component declares a
  section (D9) is a Go signature, frozen by T1.6, and recorded here. No marker.
- **D2, `Check` takes RFC 23's `ControlPlaneTx`, which has landed.** `Check` runs inside the
  write's transaction and takes `entity.ControlPlaneTx`, which RFC 23's T2.4 declared on
  `feat/storage-v2` (#4362) without the settings delete and the secret writes. So T1.6 waits on
  nothing outside the epic, and T1.5 adds those writes' transaction forms to `ControlPlaneTx`, as
  RFC 23's epic 2 says in its D4 for whichever lands second. No marker.
- **D3, `keyprovider` moves here, not in E4.** `Bootstrap` names each role's wrapping key, so
  the type must be importable from `pkg/bootstrap` without the block
  middleware tree, which is why it moves at all. The move is unchanged, with no tag added:
  `keyprovider.Config` is flat (`kind`, then `file` and `retired_files`, or `endpoint` and
  `timeout_ms`, all directly under the struct), so a `mapstructure` tag on each field cannot
  carry Appendix C's keys, which nest by provider (`keys.<role>.local.file`,
  `keys.<role>.kmip.endpoint`, the KMIP `timeout` a duration), and the struct's `json` shape
  is the stored per-remote policy's, which `encryption.ParsePolicy` decodes with a plain
  `json.Unmarshal`, so renaming a field would drop a stored value without an error (D10).
  `Bootstrap` declares its own per-role struct instead, which reads the nested keys through
  `mapstructure` tags (viper decodes by the `mapstructure` tag, and would not match
  `retired_files` to `RetiredFiles` without one) and converts:
  ```go
  type WrappingKey struct {
    Kind  keyprovider.Kind `mapstructure:"kind"`
    Local struct {
      File         string   `mapstructure:"file"`
      RetiredFiles []string `mapstructure:"retired_files"`
    } `mapstructure:"local"`
    KMIP struct {
      Endpoint       string        `mapstructure:"endpoint"`
      ServerCA       string        `mapstructure:"server_ca"`
      ClientCert     string        `mapstructure:"client_cert"`
      ClientKey      string        `mapstructure:"client_key"`
      KeyUID         string        `mapstructure:"key_uid"`
      Timeout        time.Duration `mapstructure:"timeout"`
      RetiredKeyUIDs []string      `mapstructure:"retired_key_uids"`
    } `mapstructure:"kmip"`
  }

  // Provider converts to the keyprovider.Config NewProvider takes, with
  // Timeout as TimeoutMS milliseconds.
  func (k WrappingKey) Provider() keyprovider.Config
  ```
  The importers that move
  with it, RFC 13's plan's D5 list: `pkg/block/middleware/encryption/decorator.go`,
  `decorator_test.go`, `policy.go` and `README.md` (its relative `keyprovider/` link),
  `pkg/block/middleware/ordering_test.go`, `docs/guide/encryption.md`,
  `docs/internals/encryption-design.md`, `test/scenarios/12-smb-encryption-roundtrip-s.sh`, and
  `test/harness/run/gojson_test.go`, which holds the path as a string, so no compiler catches it.
  No marker.
- **D4, a stub fails loudly and names its epic.** A stub that returns an error returns one wrapping
  `errors.ErrUnsupported` and naming the epic that fills it, as RFC 23's epic 1 did; one that
  returns no error (`Of`, `Diff`, `Keep`) panics with the same text. A zero value would read as
  "no change" or "an empty section". Nothing calls a stub before its epic lands: T1.7's caller list
  says who calls it first. No marker.
- **D5, the memory limit is not a `Bootstrap` field.** RFC 13 §2.2 lists it as a host fact, but its
  sources are the runtime's soft limit, the container's and the host's memory, in RFC 24 §2.1's
  order, none of them a key of the file. A field would be a fourth source. No marker.
- **D6, `entity.ResourceRef` exists, and T1.4 uses it.** It is declared with `ResourceKind` in
  `pkg/metadata/entity/api.go` on `feat/storage-v2` (#4494), and `ControlPlaneTx.RunningOperation`
  already takes it. RFC 16 §2.3 uses it in `SecretOwner` and `Operation` as "the record's kind and
  ID". T1.4 declares no second one. No marker.
- **D7, `secrets.New` is part of the freeze.** The node and `dfs init` build a provider from the
  view and the two roles' keys; without a frozen constructor each would reach for the unexported
  struct. `bootstrap.Load` exists already; T1.3 only adds its environment argument. No marker.
- **D8, the secret kinds are RFC 16's list, and a kind with no role is not declared.**
  `SecretKind` declares RFC 16 §2.3's kinds whose role RFC 13 §7's table gives. The node credential
  (RFC 15 §4.4) has no role there and waits with the cluster; the directory's bind credential and
  the OpenID Connect client secret have no kind in RFC 16 (§9), and take `protocol` and `storage`
  once they do (RFC 13 §7, RFC 23 §5.5). Adding a constant later breaks no
  caller. No marker.
- **D9, the declaration API: a class is a struct tag.** A component's settings struct carries each
  field's class beside its `json` tag (`class:"restart"`), and `Of` panics on a field without one,
  so a class cannot drift from the field it describes and a renamed field takes its class with it.
  The scope, the override and the options stay arguments of `Of`:
  ```go
  func Of[T interface{ Validate() error }](prefix string, scope Scope, def T, opts ...Option) Section
  // Scope is one of RFC 13 §3's three forms: At(entity.ScopeInstallation),
  // PerThing(entity.ScopeNode, "journal"), or Override(entity.ScopeInstallation, entity.ScopeNode).
  ```
  The options are `WriteRole`, `Required`, `CheckTx` and `SecretRef(field, kind)`.
  `CheckTx[T](func(entity.ControlPlaneTx, Write, T) error)` is a check the declaring component
  hands `Of`, which reads store state beyond settings, tracked, inside the write's transaction
  ([RFC 13's plan, §4.4](rfc-13-config-plan.md)'s code sketch; the RFC 18 plan's epic 2 is its
  first caller, an `identity.id_range`). `Of` panics on a `Required`
  or `SecretRef` field name that `T` does not have, on a class missing from a field, on a field
  with no explicit `json` name (its key is the field's Go name, which is not the lower-case key
  the conventions T1.1 settles allow; refusing a mixed-case one is the decode's, which E3 matches
  exactly as declared, so the unnamed field never reaches a record), on a class that is not one of the four (`class:"Restart"`, `class:"restar"`), and on two fields that share a name after the flattening
  (`encoding/json` resolves a duplicate by depth, so the drop is not always of both; the panic is
  right either way), so a security setting with a misspelled class never gets an unspecified binding. A field
  tagged `json:"-"` (the adapters' `Addrs`) is not a setting and needs no class, and an embedded
  struct (the adapters' `Adapter`) is read as its own fields, flattened as `json` does. Against a map of classes
  beside the struct, as RFC 13's plan sketched: the map repeats every field name as a string, which
  only a panic at start ties back to the struct. T1.1's review may revise this decision through a
  pull request before T1.6 freezes it. No marker.
- **D10, a duration is a string with a unit everywhere an operator writes one, as T1.1 settles
  it (#4486, provisional until it merges, §9).** RFC 13 Appendix B, as #4486 writes it: "a duration is a number and a unit, `ms`,
  `s`, `m` or `h`", and "a bare number is refused". Two types on `feat/storage-v2` hold a duration
  another way. `pkg/adapter/settings` declares `time.Duration` fields with plain `json` tags, which
  `encoding/json` writes as integer nanoseconds (`30000000000`): E3 gives those structs a string
  codec (`"30s"`), with a `Schema` raise by RFC 13's plan's D11. `keyprovider.Config` holds
  `TimeoutMS int` (`timeout_ms`), and keeps it: `encryption.ParsePolicy` decodes that struct
  out of the stored per-remote policy with a plain `json.Unmarshal`, which drops an unknown
  field without an error, so renaming it would leave a stored `"timeout_ms": 5000` behind and
  KMIP would run on its default timeout. The bootstrap names it the way Appendix C does
  instead, `timeout`, a duration, on the per-role struct D3 declares, whose conversion maps
  it into `TimeoutMS`. The stored JSON, the moved type and the guides' `timeout_ms` are
  untouched. The string form of the adapters' durations is E3's codec test, since
  the codec is E3's. No marker.
- **D11, today's bootstrap keys each have a decision, in #4486's Appendix C (provisional until it
  merges, §9).** `Bootstrap`'s
  four untagged fields (`mapstructure:"-"`, under one `ponytail:` marker, `pkg/config/config.go`)
  are retagged, and the marker goes with them: `Node` to `node.id`, `MetricsAddr` to
  `metrics.listen` (one `ip:port`, or `off`), and `APIAddr` to `api.listen`, which Appendix C
  holds as a list of `ip:port`, more than `APIAddr string` carries, so the field widens to a
  slice of addresses. `keys_test` decodes a file with two of them. `Devices []string` cannot carry Appendix C's
  `journals`, a list of entries each with a `path`, so it is replaced: `Bootstrap` holds one
  small entry struct per device, and the environment form `DITTOFS_JOURNALS=/a,/b`, which sets
  the paths only, reaches it through a decode hook beside the `node.id` one.
  `blockstore.journal.path` becomes
  `journals`, one entry per device. `logging.format` and `logging.rotation.*` stay, the rotation
  size as a size with its unit and the age as a duration. Appendix C's table "From the files of
  today" gives each of the others its fate: `controlplane.*` and `metrics.*` are renamed
  (`api.listen`, `api.tls`, `metrics.listen`), `shutdown_timeout` becomes the setting
  `adapters.<protocol>.drain_timeout`, `database.*` is dropped, and the keys that became records
  or settings are refused. `renamed_keys.go` already exists (`checkRenamedKeys` and its table), and
  its refusal runs as soon as an entry is added. So T1.3 adds no entry and leaves the table
  alone: E2 extends it and turns the refusal on in the same change that moves the launchers off the
  refused names (`docker/config.yaml`, the bench scripts, the operator's rendered file), and
  `Blockstore.Journal.Path` stays, with `cmd/dfs/commands/config/validate.go:68` that prints it,
  until E2 retires it. No marker.

## 5. Target tree

```text
docs/internals/rfcs/rfc-13-configuration.md    T1.1  Appendix C, the bootstrap file; §6, the refusal's shape;
                                                     Appendix B's value conventions
pkg/bootstrap/                                  T1.2  renamed from pkg/config, with every importer
pkg/bootstrap/bootstrap.go                      T1.3  Bootstrap's fields, under Appendix C's keys
pkg/bootstrap/nodeid.go                         T1.3  NodeID, InitNodeID, WriteMissingNodeIDs (stubs)
pkg/bootstrap/keys_test.go                      T1.3  Bootstrap's keys against Appendix C
pkg/keyprovider/                                T1.3  moved from pkg/block/middleware/encryption/keyprovider (D3)
pkg/metadata/entity/secret.go                   T1.4  Secret, SecretKind, SecretOwner (ResourceRef exists, D6)
pkg/secrets/                                    T1.4  Role, Meta, Version, Provider, New, its methods and errors
pkg/secrets/redact_test.go                      T1.4  a Version never prints its value
pkg/secrets/roles_test.go                       T1.4  the kind table against RFC 13 §7
pkg/metadata/kvstore/settings.go                T1.5  DeleteSetting, AllSettings (stubs)
pkg/metadata/kvstore/secrets.go                 T1.5  Secret, PutSecret, PruneSecret (stubs)
pkg/settings/                                   T1.6  Class, Scope, Section, Of and its options, Table, Snapshot,
                                                      Read, Get, Diff, Change, Keep, Write, Put, Delete,
                                                      HoldsContent, Check, errors
pkg/{bootstrap,settings,secrets}/arch_test.go   T1.7  each package's exports pinned
```

## 6. Subtasks

### T1.1 The configuration's user surface, settled in RFC 13

**Subtask T1.1 estimate: 2 man-days.**

- **Track:** the team. **Depends on:** nothing.
- **Covers:** R1, R2, R3, R21, R22.
- **Deliverables:** a docs pull request on `develop` amending RFC 13, and reviewed before any code
  of E1 to E3 builds on it (D1):
  - **Appendix C, the bootstrap file.** One row per §2.2 fact but the scratch paths: its key in the
    file, its type, its default or "required", its environment form, and an example. It answers:
    the file's format and the order of its sources (the file, then the environment, then the
    defaults); how a nested key maps to a variable (`api.listen` to `DITTOFS_API_LISTEN`); how a
    list is written in the environment (the journal devices); which facts are paths, and whether a
    relative path is read against the file's directory; which facts are secrets or point at one
    (the store's credential, the wrapping keys), and how they are given without the secret in the
    file (a path to read, or a value, which T1.3 types so that it redacts itself; #4486 gives files); how a role names its sealing key and its retired keys.
  - **Today's keys (D11).** For each key `pkg/config` has today, and `Node`, `Devices`,
    `APIAddr` and `MetricsAddr`: kept,
    renamed (with a `renamed_keys.go` entry) or dropped, in Appendix C (#4486).
  - **§7's kind column.** RFC 13 §7's wrapping-key table says what each role seals in prose
    ("references to master keys"), not the kind names RFC 16 §2.3 spells, so a test comparing the
    kinds with it needs a hand-written prose map. T1.1 adds to the table a kind column naming each
    kind as `SecretKind` spells it, and T1.4's `roles_test` reads the kinds straight from it.
  - **§6, the refusal's shape.** One shape for every refusal of configuration, the file's, the
    environment's and a record's: what was refused (the key, the variable, or the record and its
    scope), the rule, and, for a v1 key, what replaced it; never the value of an environment
    variable, of a credential key, or of a key the refusal does not declare (R2). Appendix C
    marks the keys whose value no refusal ever prints. Two or more refusals in
    one file are reported together, so a user fixes the file in one pass. Examples for a v1 key, an
    unknown key, a value out of range and a bound setting refused while content exists, and
    E2's credential canary a PEM typed into an `api.tls.key_file`, refused without it.
  - **Appendix B's value conventions.** A key is lower case with dots and underscores, no two
    keys share an environment name, and an unknown `DITTOFS_*` variable or a mixed-case key is
    refused, with an allowlist of names read elsewhere (today an unknown file key only warns); a
    duration is a string with a unit (`"30s"`, D10), and a bare number refused; a size is a number
    and a unit (`KB` to `TB`, `KiB` to `TiB`, `B`), an integer of bytes only in a record and refused
    in the file, so `internal/bytesize`'s `Gi`, `Mi`, `m` and `g` are not Appendix B's and E3's
    codec refuses them; a list and a map are JSON's in a record and YAML's in the file.
  - **The two host rules (R21, R22)**, a non-loopback listener without TLS and the runtime
    directory, stated in Appendix C for E2 to enforce.
  - **`dfs config`'s three commands on the bootstrap.** What `show` prints (the effective
    bootstrap, each value's source, secrets as references only), what `validate` refuses (what a
    start refuses), and what `schema` describes (Appendix C).
  - **The developer surface.** D9's declaration of a section, shown on the adapters' section from
    `pkg/adapter/settings`, in the pull request's description; a change to D9 revises this
    document through the same review.
- **Acceptance criteria:**
  - Every key `pkg/config` has today has a decision (D11), and the duration convention names what
    `pkg/adapter/settings` and `keyprovider.Config` do (D10).
  - Every §2.2 fact but the scratch paths and the memory limit (D5) has one Appendix C row; no key
    there names a setting of Appendix B (R6).
  - The refusal's shape is stated once, in §6, and its examples print a value only where it is
    none of R2's three kinds.
  - Appendix C carries R21 and R22.
  - The plans that edit `docs/guide/configuration.md` and the API's settings routes can cite the
    conventions rather than restate them.
- **Tests:** none in Go: the pull request is text. Appendix C is parsed by T1.3's test, and the
  value conventions by E3's decode tests, so a later drift fails there.

### T1.2 `pkg/config` renamed to `pkg/bootstrap`

**Subtask T1.2 estimate: 0.75 man-days.**

- **Track:** the team. **Depends on:** nothing (S0 is reached).
- **Covers:** R4.
- **Deliverables:** `git mv pkg/config pkg/bootstrap`, the package clause renamed, and every
  importer on `feat/storage-v2` with it: `cmd/dfs/commands/config`'s `edit.go`, `schema.go`,
  `show.go`, `validate.go` and `roundtrip_test.go`, `cmd/dfs/commands/start.go`,
  `internal/node/node.go`, `internal/node/node_test.go` and `internal/node/nodetest/harness.go`. The paths held as strings, which no compiler catches, change too:
  `test/harness/plan/plan_test.go`'s two `pkg/config/config.go` and `test/suites.yaml`'s
  `pkg/config/**` glob; and `pkg/metadata/types.go`'s comment naming `pkg/config.ServerConfig`,
  which no longer exists. Nothing else changes: no field, no function, no behaviour.
- **Acceptance criteria:**
  - No Go file on `feat/storage-v2` imports `pkg/config`, and the directory is gone: a `git grep`
    for `pkg/config` in `*.go` returns nothing outside `k8s/dittofs-operator` (O4), and `go vet ./internal/node/...` passes.
  - `test/suites.yaml`'s glob still selects the files it selected (now under `pkg/bootstrap`).
  - The build, `go vet` with every `lint-go` tag, and `dt`'s registry test pass.
- **Tests:** `plan_test.go`'s two cases, now naming `pkg/bootstrap/config.go`: the Kerberos case
  is the guard, since it must plan `e2e/kerberos` and fails if the `pkg/config/**` glob in
  `test/suites.yaml` is left on the old path. The other, an absence of `e2e/cross-protocol`,
  passes either way and checks only that the rename names it. The rest is checked by the build.

### T1.3 `Bootstrap`'s facts and the node identity's entry points

**Subtask T1.3 estimate: 1.75 man-days.**

- **Track:** the team. **Depends on:** T1.1 (the keys), T1.2.
- **Covers:** R1, R5, R6, R7, R8, R9, R12.
- **Deliverables:**
  - `Bootstrap`'s fields, each tagged with Appendix C's key, one field for each of its rows: the
    node's identity (optional), the metadata store's backend, path or cluster file, and its TLS
    certificate, key and CA files, the journal devices and their paths, the adapters', the API's
    and the metrics listener's addresses, the API's TLS certificate and key, the log's
    destination, format, rotation and level (kept), the roles, and each role's wrapping key as the
    nested per-role struct D3 declares, which converts to the `keyprovider.Config`
    `keyprovider.NewProvider` takes (D3). The API's local socket keeps Appendix C's `api.socket`
    row (#4486), a path whose default is under the runtime directory R22 gives. No field for the memory limit (D5), the scratch paths or initialise-if-empty.
  - `entity.NodeID` is a `[16]byte` with no `UnmarshalText`, and `pkg/config` has no decode hook, so
    viper cannot read `node.id` in the form Appendix C gives (a UUID): with a nested tag it fails
    on `node.id[0]`, with a dotted one it returns no error and leaves the identity zero. `Load`'s
    decoder gains a `DecodeHookFunc` that parses a UUID string into `entity.NodeID`, composed
    with a duration hook and a string-to-slice hook equal to viper's own, which `viper.DecodeHook`
    replaces: a bare hook would drop them, and D3's `WrappingKey.KMIP.Timeout` would stop
    reading `timeout: "5s"` (`mapstructure.ComposeDecodeHookFunc` rebuilds the list). So the
    file's
    `node.id` reaches `Bootstrap.Node`.
  - Appendix C's `journals` is a list of entries each with a `path`, which `Devices []string`
    cannot decode (D11): `Bootstrap` holds one small entry struct per device, and `Load`'s
    `DecodeHookFunc` also turns the environment form `DITTOFS_JOURNALS=/a,/b`, which sets the
    paths only, into entries, beside the `node.id` case.
  - #4486 gives the store's credential as files (`metadata.foundationdb.tls.*`), so `Bootstrap`
    holds their paths only. If its review turns the credential into a value, a type for it whose `String`, `Format`,
    `LogValue`, `MarshalYAML` and `MarshalJSON` redact. Today `pkg/config` has no redaction hook,
    and `dfs config show` prints `yaml.Marshal(cfg)`, so without the type `show` would print the
    credential in clear from PR 3 until E2.
  - `keyprovider` moved to `pkg/keyprovider`, unchanged, with its importers (D3).
    Its local provider still reads `DITTOFS_ENCRYPTION_PASSPHRASE` with `os.Getenv`, past `Load`'s
    `environ`, so both roles share one passphrase and a test that seals sets it with `t.Setenv`:
    the plan's `ponytail:` note on that travels with the moved code, and E2 moves the read onto
    `environ`.
  - `renamed_keys.go` is not touched, and `Blockstore.Journal.Path` stays until E2 (D11).
  - `Load` and `MustLoad` take the environment, `Load(path string, environ []string)`, and every
    caller passes `os.Environ()`, so epic 2's loader and its tests read no process state (epic 2's
    D2). The body ignores the argument until epic 2, which feeds it to the key binding and the
    scan of names alike, and drops viper's `AutomaticEnv`, the process-reading path of today.
  - The node identity's entry points, with stub bodies (D4), shaped for their callers, RFC 23's
    T3.10 and T5.4:
    ```go
    // NodeID reads every journal directory's node-id copy and compares them, with b's
    // identity if it names one. Before the store opens; it writes nothing.
    func NodeID(b *Bootstrap) (entity.NodeID, error)
    // InitNodeID adopts or generates the identity and writes it into one journal
    // directory, between that journal's Init and RecordJournal, for the initialisation
    // step and a recovery import. Called once per directory.
    func InitNodeID(b *Bootstrap, installation [16]byte, dir string) (entity.NodeID, error)
    // WriteMissingNodeIDs writes a missing copy only beside a journal the store records
    // for this node and device, logging each write.
    func WriteMissingNodeIDs(b *Bootstrap, id entity.NodeID, recorded func(dir string) (bool, error)) error
    ```
- **Acceptance criteria:**
  - Every key of Appendix C is a `Bootstrap` key, and the other way round, and no two keys share
    an environment name.
  - A canary credential does not appear in `dfs config show`, in either format, when #4486
    turns the store's credential into a value; with files, its current text, show prints paths
    only, which hold no secret.
  - `dfs config schema` lists the new keys, and `dfs config show` and `validate` still pass on
    today's default file (their bodies are E2's).
  - The three entry points match their callers' calls in RFC 23's epic documents, or §9 records the
    difference.
- **Tests:** `keys_test.go` parses RFC 13's Appendix C from `docs/internals/rfcs/` and compares its
  keys with `Bootstrap`'s `mapstructure` paths, both ways (catches a field added without its RFC
  row, a row with no field, and a misspelled tag), but for `blockstore.journal.path`, the one
  named exception E2 removes (D11). Appendix C writes some key cells in shorthand (`.key_file`,
  `.ca_file`, `keys.<role>.kmip.endpoint`, `.server_ca`): the test expands a leading dot into the
  row's first key's prefix and `<role>` into both roles, and fails on any key cell it cannot
  parse whole, so a skipped cell never passes the zero-rows guard. A case decodes a file that
  sets `node.id` into a non-zero `Bootstrap.Node`. A case decodes `kmip.timeout: "5s"` into
  `WrappingKey.KMIP.Timeout` (catches a hook that replaces viper's defaults instead of composing
  with them). A case decodes the file's `journals` entries, and one the environment's
  `DITTOFS_JOURNALS=/a,/b`, into the same entries (catches a `Devices []string` left in place of
  D11's entry struct). A case decodes `api.listen` with two addresses (catches the field left a
  single string). A case decodes a file naming
  `keys.<role>.local.retired_files` into the per-role struct (catches a `mapstructure` tag the
  declaration needs being left out). The test fails if
  Appendix C yields no rows (a renamed heading), and asserts that each key has its own environment
  name (`api.tls_cert` and `api.tls.cert` both map to `DITTOFS_API_TLS_CERT`). A canary test,
  when #4486 turns the credential into a value, runs it through `show` in YAML and JSON.

### T1.4 `pkg/secrets`' provider and the `Secret` record's types

**Subtask T1.4 estimate: 1.5 man-days.**

- **Track:** the team. **Depends on:** T1.3 (`keyprovider`'s new path).
- **Covers:** R10, R11, R12, R13.
- **Deliverables:**
  - In `pkg/metadata/entity`: `Secret`, `SecretKind` with D8's constants, `SecretOwner`,
    over the existing `ResourceRef` (D6). No codec: E4's.
  - In `pkg/secrets`: `Role` (`Protocol`, `Storage`) with RFC 13 §7's `decision:` note; the kind
    table giving each kind its role; `Meta`; `Version`, whose `String`, `Format`, `LogValue`, `MarshalJSON` and `MarshalYAML`
    print its number and `Meta` and never its value (`Value` is an exported `[]byte`, which
    `json.Marshal` and `yaml.Marshal` would otherwise print base64-encoded); `Provider`, `New` (D7), and RFC 13 §7's five
    methods with stub bodies (D4), `Delete` provisional, as `AllSettings` is, since it gains a
    transaction when E4 fills it; the errors `ErrWrongRole`, `ErrWrongKind`, `ErrKindChanged` and
    `ErrUnproven` with RFC 13's plan's D9 `decision:` note.
    ```go
    func New(cp kvstore.ControlPlane, keys map[Role]keyprovider.KeyProvider) (*Provider, error)
    func (p *Provider) Seal(ctx context.Context, role Role, kind entity.SecretKind, owner entity.SecretOwner, ref entity.SecretRef, value []byte, m Meta) (entity.Secret, error)
    func (p *Provider) Resolve(ctx context.Context, role Role, kind entity.SecretKind, owner entity.SecretOwner, ref entity.SecretRef) ([]byte, error)
    func (p *Provider) Versions(ctx context.Context, role Role, kind entity.SecretKind, owner entity.SecretOwner, ref entity.SecretRef) ([]Version, error)
    func (p *Provider) Prune(ctx context.Context, kind entity.SecretKind, owner entity.SecretOwner, ref entity.SecretRef) error
    func (p *Provider) Delete(ctx context.Context, kind entity.SecretKind, owner entity.SecretOwner, ref entity.SecretRef) error
    ```
- **Acceptance criteria:**
  - The surface is RFC 13 §7's, with `Seal` returning the `Secret` unwritten.
  - Every declared kind has exactly one role, the one RFC 13 §7's table gives.
  - `pkg/secrets` and `pkg/settings` import neither each other.
- **Tests:** `redact_test.go` prints a `Version` holding a canary through `%v`, `%+v`, `%#v`,
  `fmt.Sprint`, `json.Marshal`, `yaml.Marshal` and an `slog` JSON handler, and fails if the canary appears (R12). `roles_test.go`
  compares the kind table with RFC 13 §7's table, read from the kind column T1.1 adds and
  failing on zero rows (catches a kind sealed under
  the other role, which would let a protocol-only node open a storage secret).

### T1.5 The kvstore view's settings delete, cross-scope read and secrets

**Subtask T1.5 estimate: 0.5 man-days.**

- **Track:** the team. **Depends on:** T1.4 (`entity.Secret`).
- **Covers:** R14, R15, R16.
- **Deliverables:** on kvstore's `ControlPlane`, with stub bodies (D4):
  `DeleteSetting(ctx, scope, key, expect)`, `AllSettings(ctx) iter.Seq2[entity.Setting, error]`
  (§9: the name waits on RFC 16), `Secret(ctx, id, version)`, `PutSecret(ctx, s, expect)` and
  `PruneSecret(ctx, id, below)`, each with its transaction form on `ControlPlaneTx` (D2), among them a tracked
  `Secret(id, version)` read, so that `Check` verifies a reference's kind and owner inside the
  write's transaction. `AllSettings` is provisional until E3's §4.3 benchmark.
- **Acceptance criteria:**
  - The four RFC 16 §3 methods have RFC 16's signatures.
  - `ControlPlaneTx` has a tracked read of a secret version, which `Check` calls.
  - E3 and E4 fill them without changing a signature, `AllSettings` aside.
- **Tests:** none of its own: the signatures are pinned by T1.7, and E3's and E4's tests run them.

### T1.6 `pkg/settings`' types

**Subtask T1.6 estimate: 1.5 man-days.**

- **Track:** the team. **Depends on:** T1.1 (the review of D9), T1.4 (`entity.SecretKind`), T1.5
  (`ControlPlaneTx`'s tracked `Secret` read, which `Check`'s signature takes through its transaction).
- **Covers:** R14, R17, R18.
- **Deliverables:** with stub bodies (D4):
  - `Class` (`Live`, `Restart`, `NextWrite`, `Bound`) and the `class` struct tag (D9), and the tags
    themselves on every field of `pkg/adapter/settings`' NFS and SMB structs, so the example test
    below declares real sections;
  - `Scope` and its three constructors, `At`, `PerThing` and `Override`;
  - `Section`, `Of` and its options `WriteRole`, `Required`, `CheckTx` and `SecretRef(field, kind)`
  (D9);
  - `Table`, `Snapshot`, `Read(ctx, cp, table, node)` returning the snapshot and the sections it
    refused, `Change`, and
    `Get[T any](snap *Snapshot, section string, node entity.NodeID) (T, error)` (an error for a
    section the snapshot holds no value of, such as one held at start),
    `Diff(prev, cur *Snapshot) []Change` (a nil `prev` makes every key a change) and
    `(*Snapshot).Keep(section string, prev *Snapshot) *Snapshot` (the section's values from `prev`,
    the rest from the receiver);
  - a `Table` built from two sections that declare one key panics and names both sections, so
    keys stay unique across sections and the table's first test fails on a clash;
  - `Write`, `Put(s)`, `Delete(scope, key)`, `HoldsContent` (`func(scope entity.SettingScope) (bool, error)`, so that a write that is not
    bound never probes), and
    `Check(tx entity.ControlPlaneTx, caller entity.APIRole, w Write, holds HoldsContent) error`,
    which refuses the write when `holds` returns an error, so a bound setting never fails open;
  - the errors `ErrUnknownKey`, `ErrWrongScope`, `ErrNewerSchema`, `ErrForbidden`, `ErrRequired`,
    `ErrInvalid`, `ErrBound`, `ErrSecretRef`, `ErrUnproven` (with RFC 13's plan's D9 note) and
    `ErrFormatVersion`.
- **Acceptance criteria:**
  - `pkg/settings` imports no store package and declares no store interface; `Check` reads only
    through the `ControlPlaneTx` it is given. Its refusal of a reference to a secret of another kind
    or another owner (R14) needs a real body and the tracked read of T1.5, so it is E3's criterion.
  - A section can be declared for `pkg/adapter/settings`' NFS and SMB structs, in a test, with the
    adapters' scope form (installation, node override).
- **Decision note:** a secret's rotation never reaches `Check`, since a reference names the
  secret, not a version: the `decision:` note goes at `Check`'s declaration.
- **Tests:** an `Example` function with no `// Output:` comment declares the adapters' section with
  D9's tags: `go test` compiles it and never runs it, so it fails to compile if `Of`'s signature or
  an option changes, which is the freeze's point, and the `Of` stub's panic (D4) is not reached.
  The bodies are E3's: its first `Table` test builds two sections sharing a key and asserts the
  panic, and the check of the adapters' string form (D10) is its codec's test. Its decode test
  refuses `{"Port": 1}` on a section whose field names only its `json` tag, matched exactly as
  declared where the standard library matches case-insensitively (D9).

### T1.7 The freeze review and the export pins

**Subtask T1.7 estimate: 0.75 man-days.**

- **Track:** the team. **Depends on:** T1.3, T1.4, T1.5, T1.6.
- **Covers:** R6, R19, R20.
- **Deliverables:**
  - The freeze review of `pkg/bootstrap`'s inherited exports. `pkg/config` exports 15 symbols
    today on `feat/storage-v2`, and the review keeps those with a caller outside the package:
    `Bootstrap` (`cmd/dfs/commands/config`, `internal/node` and its tests), `Load`
    (`cmd/dfs/commands/config`), `MustLoad` (`cmd/dfs/commands/config` and `start.go`),
    `GetDefaultConfigPath` (`edit.go`, `validate.go`), `GetDefaultConfig` (`roundtrip_test.go`) and
    `InitLogger` (`start.go`). The nine others have none, and are
    unexported unless a kept field's type needs them: `LoggingConfig` and `LogRotationConfig`
    (the `Logging` field), `BlockstoreConfig` and `BlockstoreJournalConfig` (until E2 retires the
    field, D11), `DefaultConfigExists`, `GetStateDir`, `GetDefaultLogPath`, `ApplyDefaults` and
    `Validate`.
  - One `arch_test.go` per package, `pkg/bootstrap`, `pkg/settings` and `pkg/secrets`, pinning its
    exported set as `internal/node`'s does: an extra export, an exported field added to a
    frozen struct, or a changed signature fails it.
  - The caller list, in the pull request's description: each frozen symbol with its first caller
    (an E2 to E5 subtask, RFC 23's T3.10, T5.3 or T5.4, or the RFC 18 plan's epic 2), and each
    symbol of T1.3 to T1.6's deliverables checked present. `Delete`, pinned here without a transaction, is
    provisional like `AllSettings` and listed as gaining one when E4 fills it, and `ObserveLag`,
    which E3's D5 adds to `pkg/settings` after the freeze, is listed with E5's reconcile as its
    first caller.
- **Acceptance criteria:**
  - Every symbol T1.3 to T1.6 deliver is exported (they are RFC 13's plan's S1 list), and nothing
    else is but D7's `New` and the `pkg/bootstrap` exports kept above, each with its caller.
  - No `Bootstrap` key names a setting of RFC 13's Appendix B.
- **Tests:** each pin runs over a planted extra export in `testdata` and fails on it (catches a pin
  that compares nothing). A case parses Appendix B's setting keys and fails if one is a
  `Bootstrap` key (R6).

## 7. Order of work

```mermaid
flowchart TD
  T1_1["T1.1 User surface, in RFC 13"] --> T1_3["T1.3 Bootstrap's facts, node identity"]
  T1_2["T1.2 pkg/config renamed"] --> T1_3
  T1_3 --> T1_4["T1.4 pkg/secrets, Secret types"]
  T1_4 --> T1_5["T1.5 kvstore views"]
  T1_1 --> T1_6["T1.6 pkg/settings' types"]
  T1_4 --> T1_6
  T1_5 --> T1_6
  T1_3 & T1_5 & T1_6 --> T1_7["T1.7 Freeze review, export pins"]
  T1_7 --> S1(["after T1.7: S1, the surface frozen"]):::sync
  T1_2 --> C1(["after T1.2: pkg/bootstrap builds"]):::sync
  classDef sync stroke:#2c5aa0,stroke-width:3px,stroke-dasharray:4 2
```

T1.1 and T1.2 start at once and in parallel. Checkpoint after T1.1: the user surface is agreed,
the review the epic exists for. Checkpoint after T1.2: `pkg/config` is gone. No subtask waits
outside the epic (D2).

## 8. Suggested pull requests

| PR | Subtasks | Man-days | After | Why together |
| --- | --- | --- | --- | --- |
| 1 | T1.2 | 0.75 | none | the rename alone, mechanical and easy to review, on `feat/storage-v2` |
| 2 | T1.1 | 2 | none | the RFC 13 amendment, on `develop`, the review of the user surface; parallel with PR 1 |
| 3 | T1.3, T1.4, T1.5 | 3.75 | PR 1, PR 2 and its cherry-pick to `feat/storage-v2` | the bootstrap's facts, the secrets surface and the views |
| 4 | T1.6, T1.7 | 2.25 | PR 3 | the settings surface and the pins that freeze all three packages: S1 |

PR 3's `keys_test.go` reads Appendix C from `docs/internals/rfcs/`, and RFC text reaches
`feat/storage-v2` from `develop` (a merge or a cherry-pick), where RFC 13 ends at Appendix B today,
so PR 3 starts once PR 2 is there. A suggestion only: one pull request per subtask works as well.

## 9. Open points

- **The OpenID Connect client secret and the directory's bind credential have no secret kind.**
  RFC 16 §2.3's `Secret.Kind` list has neither. RFC 13 §7's table gives the bind credential the
  `protocol` role and leaves the client secret out, which RFC 23 §5.5 seals under `storage`. Until
  RFC 13 §7 and RFC 16 §2.3 are amended, D8 declares neither kind. The bind credential's kind is
  proposed in RFC 18's epic 6 breakdown (#4341, its D3). Pending #4472.
- **The cross-scope read has no RFC 16 signature.** RFC 16 §3 has `Settings(ctx, scope)` only;
  T1.5 names the one-scan read `AllSettings` until RFC 16 §3 gains it. Pending #4473.
- **D10 and D11 quote #4486 as amended, and it is not merged.** Appendix C, §6's shape and
  Appendix B's conventions are read from its branch. This breakdown merges after #4486, or each
  quote is re-read against the merged text. Pending #4486.
- **`WriteMissingNodeIDs` differs from its caller.** RFC 23's epic 3 (T3.10) calls it with the
  store's `JournalID`, while T1.3's stub takes `recorded func(dir string) (bool, error)`, a
  callback the caller builds from that read. The stub keeps the callback until T3.10 and E2 agree
  on one form.
- **`kmip_fake_test.go` moves with `keyprovider`.** It is a canned-response KMIP server, which
  `CLAUDE.md`'s "Writing tests" bans for a new or changed test. The move changes no line of it, so
  it is not a touch that brings it under the rule, and its replacement by a fake that passes a
  conformance suite is a follow-up, not part of E1. It is a row of RFC 13's plan's §6.1 deferral
  ledger, owner `unowned`, so the fake's first change brings it back.
- **Moved from E4.** `keyprovider`'s move (D3), which RFC 13's plan gave E4 until this change
  takes it into E1; E4's own breakdown, 12 man-days, already leaves the move out (its estimate
  note).
- **The estimate.** 8.75 man-days and 7 subtasks against the 6 and 4 first estimated: T1.1, the
  review of the user surface, adds 2, and D3's move and the per-role `WrappingKey` struct and its
  conversion add 0.75, which is new plan money, since epic 4's breakdown, 12 man-days, already
  excludes the move cost.
