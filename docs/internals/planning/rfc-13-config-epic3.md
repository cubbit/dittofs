# Epic 3: configuration v2, settings

**Sources:** [RFC 13](../rfcs/rfc-13-configuration.md) §2.1, §2.3, §3, §4.2, §5, §5.1 and §6;
§5.2 as RFC 13's plan's D9 refusal; §5.4's refusal to advance, on one node; §8's three settings
series. [RFC 16](../rfcs/rfc-16-metadata-store.md) §2.3's `Setting` record and §3's settings view.
[RFC 23](../rfcs/rfc-23-control-plane.md) §5.2's `admin`-only settings.
**Track:** B.
**Epic 3 estimate: 9 subtasks, 18.5 man-days.**
**Depends on:** S1, the configuration surface frozen by E1: `pkg/settings`' types with stub bodies,
the kvstore view's `DeleteSetting` and `AllSettings` with their transaction forms. S2, a `Secret`
sealed by E4, for T3.7 only.
**Blocks:** E5, which builds the node's table from `Of`, reconciles through `Read` and `Diff`, and
calls `Check` from the API's settings routes; the RFC 18 plan's epic 6, partly, whose NetBIOS
cross-check is a tracked read through `Check`.

---

## 1. Goal

Epic 3 makes a component's settings a section the node reads through one call, checks every write
by the component's own rules before it commits, and classifies each change for reconcile.

**At the end of the epic:**

- a component declares its section once, from its typed settings, its defaults and `Validate`,
  with a class on every field, its scope form, its write role, its required keys and its secret
  references; a typo in any of them fails the table's first test;
- `Read` builds a snapshot of every section from one scan, each section decoded strictly on its
  own, so one bad row stops only its section, and `Get` hands a component its value;
- `Check` refuses, inside the write's transaction, an unknown key, a wrong scope, a newer schema, a
  caller below the section's write role, an invalid value, a bound change while content exists, a
  store location's change, a reference to another kind's or owner's secret, and a block format
  version past this node's range;
- `Diff` tells reconcile which keys changed and with which class, and `Keep` holds a refused
  section at its last snapshot;
- the three settings series exist, and a benchmark measures the reconcile scan RFC 13's plan marks
  as knowingly simple.

### 1.1 In scope

The kvstore settings view's delete, cross-scope read and schema refusal; `Of`, `Table` and their
options; the strict decode and the size grammar; `Read`, `Get`, `Diff` and `Keep`; `Check` and
every gate RFC 13's plan gives it; the three series; the reconcile scan benchmark; the `decision:`
and `ponytail:` markers RFC 13's plan places in this epic.

### 1.2 Out of scope

- The node's table, reconcile, the API's settings routes and `HoldsContent`'s implementation: E5.
- The test that parses Appendix B against the table (RFC 13's plan's D3): E5.
- RFC 13 §6's port rule across the three adapter ports: E5. The general rule across two
  components' settings is a `ponytail:` marker here (RFC 13's plan, §4.3).
- Each component's section and its Appendix B rows: the component's own plan (RFC 13's plan, O4).
- `Resolve`, `Seal` and the secret records: E4. The secrets pass and the `NodeStatus` write of the
  reconcile benchmark: E5 (D3).
- The proof of a store location's change (RFC 4 §4.11's check, RFC 27's claim), RFC 13 §5's
  two-step restart value and §5.4 across nodes: RFC 13's plan, O1.
- A share's rebind with content refused (RFC 13 §5.1, "§5.1 share binding"): RFC 23's plan, the
  share's own write.
- A node's refusal to start below a namespace's block format version (RFC 13 §5.4): E2.

### 1.3 Names used here

- **E1**, epic 1, the configuration surface: the frozen declarations of `pkg/bootstrap`,
  `pkg/settings` and `pkg/secrets`.
- **E2**, epic 2, bootstrap, node identity and versions. Track A.
- **E3**, this epic. Track B.
- **E4**, epic 4, secrets. Track C.
- **E5**, epic 5, **the integration epic**: RFC 23's node and API on this library.
- **E6**, epic 6, E2E green: RFC 13 §9's single-node rows through the API.
- **S1**, the configuration surface frozen: E1 has merged.
- **S2**, secrets sealed: a `Secret` seals, resolves and versions under each role's key, partway
  through E4.
- **A1**, RFC 13's plan's amendment making `identity.directory`, the whole record, `admin`-only.
- **D9**, **D10** and **D11**, RFC 13's plan's decisions on a store location's change, on a
  reference's kind and owner, and on a section's schema; **D1** to **D6** without a plan named are
  this epic's own (§4).

## 2. How the general rules apply

The rules are those of
[RFC 13's plan, §1.5](rfc-13-config-plan.md#15-general-rules-for-each-epic), cited by number and
title and not copied.

| Rule | Subtasks | How |
| --- | --- | --- |
| rule 1, independent unit tests ([`CLAUDE.md`, "Writing tests"](../../../CLAUDE.md#writing-tests)) | all | sections declared in the test, over small typed structs; the store is Badger under `t.TempDir()` through kvstore, never a double; `HoldsContent` is a function the test passes |
| rule 2, fuzz every decoder of untrusted bytes | T3.3 | `FuzzDecode` over a row's value, seeded with every row of T3.3's table, asserting no panic and, for a decode that succeeds, a re-encode that decodes to the same value |
| rule 3, observability follows RFC 25 | T3.9 | `config_refused_total{component,reason}`, `config_bound_refusals_total` and `config_generation_lag_seconds{kind}`; no label carries a record, a key's value or a secret |
| rule 4, profiling | all | every test runs under the leak check; T3.9's benchmark reports allocations |
| rule 5, public interface and internals ([`CLAUDE.md`, "Verifying a change"](../../../CLAUDE.md#verifying-a-change)) | T3.4, T3.5, T3.6, T3.7 | refusals are asserted through `Check` and `Read`, on the error a caller sees, never on a helper |
| rule 6, known failures shrink | none | no suite of this epic has a `known.yaml` |
| rule 7, v2 ignores v1 | T3.1 | the view's existing `Settings` and `PutSetting` are kept; nothing reads v1's GORM settings store |
| rule 8, real dependencies for properties of those dependencies | T3.1, T3.9 | the generation raise, the scan and the conflict of a tracked read on Badger under `t.TempDir()` |
| rule 9, code comment conventions ([`CLAUDE.md`, "Code comments"](../../../CLAUDE.md#code-comments)) | T3.1, T3.5, T3.6 | D9's and D11's `decision:` notes at `Check`; the `ponytail:` markers of the one-scan reconcile, the one `CFGGEN` key and the rule across components |
| rule 10, a test must be able to fail | all | each gate has a test feeding the refused write and one feeding the write that passes beside it |
| rule 11, a symbol is exported only if RFC 13's interfaces use it | all | nothing beyond E1's frozen surface is exported but the lag tracker E5 feeds (D5) |
| rule 12, each subtask creates only what it declares | all | the size grammar lands with the decode, the series with T3.9, the gates each with its subtask |

## 3. Requirements and invariants

Every obligation RFC 13, RFC 16 and RFC 23 place on this epic, and those RFC 13's plan sets for it.

**Records and scopes**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R1 | A setting is a `Setting` record in the metadata store; a component gets its value from `Read` and `Get`, never from a file or a source of its own | invariant | RFC 13 §2.1 | T3.4 |
| R2 | A setting has exactly one scope, in one of three forms: one value across the scope, one per thing inside it, or a wider default a narrower record may replace | MUST | RFC 13 §3 | T3.2 |
| R3 | The effective value is the narrower override if one exists, else the wider value, else the default, per thing where the form is per thing | invariant | RFC 13 §3 | T3.4 |
| R4 | A record with a schema version the reader does not know is refused whole, never read for the fields it recognises | MUST | RFC 13 §2.3; RFC 16 §2.3 (`Setting.Schema`) | T3.1, T3.4, T3.5 |
| R5 | A delete removes one row, so the wider value is effective, and raises the settings generation in its transaction as a put does | MUST | RFC 16 §3 (`DeleteSetting`) | T3.1 |
| R6 | Reconcile reads every scope's rows in one prefix scan | invariant | RFC 13's plan, §4.3; pending #4473 | T3.1 |

**Declaring a section**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R7 | A setting has a default or is required; a snapshot or a write that leaves a required key unset is refused | MUST | RFC 13 §4.2 | T3.2, T3.4, T3.5 |
| R8 | Every field of a section declares its class: live, restart, next write or bound; one left unclassified panics at `Of` | MUST | RFC 13 §5 | T3.2 |
| R9 | The sections Appendix B gives to `admin` alone (`identity.oidc`, `api.oidc_role_map`, `api.password_sign_in`, `identity.directory`, `audit.retention`, `api.session_lifetime`, `api.token_max_lifetime`) are written by `admin` only, first value included; roles compare by an explicit rank, an empty or unknown one ranking lowest | MUST | RFC 13 Appendix B; RFC 23 §5.2; RFC 13's plan, A1 | T3.2, T3.5 |
| R10 | `Of` panics on a class, required key, per thing or secret reference that names no field; `Table` panics on two sections whose prefixes overlap ambiguously; a key belongs to the longest section prefix on a dot boundary | invariant | RFC 13's plan, epic 3 | T3.2 |

**Decoding a value**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R11 | A field the section does not know is refused | MUST | RFC 13 §6 ("Unknown fields are refused") | T3.3 |
| R12 | Keys match exactly as declared; `adapters.smb.Port` is refused | invariant | RFC 13's plan, epic 3; RFC 13 Appendix B's conventions (#4486) | T3.3 |
| R13 | A duration is a string (`"30s"`), a bare number refused; a size is a string or an integer of bytes; `null` into a non-pointer field, a duplicate key and invalid UTF-8 are refused | invariant | RFC 13's plan, epic 3; RFC 13 Appendix B's conventions (#4486) | T3.3 |
| R14 | A size keeps `internal/bytesize`'s grammar, and a product past `uint64` is refused on both of its paths | invariant | RFC 13's plan, epic 3 | T3.3 |

**Validation**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R15 | A value out of range, of the wrong type or naming something absent is refused, naming the field and the rule, in RFC 13 §6's refusal shape | MUST | RFC 13 §6 ("Refused, never replaced") | T3.3, T3.5 |
| R16 | A value is never clamped, rounded or replaced by a default | MUST NOT | RFC 13 §6 | T3.4, T3.5 |
| R17 | The write check calls the component's own `Validate` and has no rule of its own to drift | invariant | RFC 13 §6 ("Checked twice") | T3.5 |
| R18 | A section that does not decode or validate stops only itself: `Read` returns it refused and decodes the rest | invariant | RFC 13 §6 ("stops only what it configures") | T3.4 |
| R19 | A write related to another setting by a rule is validated against that setting's current value, read tracked in the write's transaction | MUST | RFC 13 §6 ("A rule across settings") | T3.5 |

**The write check**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R20 | `Check` refuses an unknown key, a wrong scope, a newer schema and a caller below the section's write role | MUST | RFC 13 §2.3, §3, §6; RFC 23 §5.2 | T3.5 |
| R21 | A delete is checked as the change it is: against the effective value it leaves | invariant | RFC 13's plan, D9 | T3.5 |
| R22 | A change to a bound setting is refused while its scope holds content, by a put or a delete; a put of the same value is no change | MUST | RFC 13 §5.1 | T3.6 |
| R23 | A change to a remote store's endpoint, bucket, prefix or credential reference is refused until it can be proven, naming RFC 13 §5.2; its first value passes | MUST | RFC 13 §5.2; RFC 13's plan, D9 | T3.6 |
| R24 | A namespace's block format version is not advanced past the oldest version a node that may read the namespace supports; on one node, this node's range | MUST | RFC 13 §5.4 | T3.6 |
| R25 | A secret reference names a secret of the kind its field declares, owned by the record or key that names it | MUST | RFC 13's plan, D10; RFC 23 §5.2 | T3.7 |
| R26 | A section's newer schema is written only once the installation's active version allows it | invariant | RFC 13's plan, D11; RFC 13 §1 | T3.6 |

**Classes and change**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R27 | `Diff` lists each changed key with its class; with no previous snapshot, every key is a change | invariant | RFC 13 §5 | T3.8 |
| R28 | `Keep` gives a refused section its previous snapshot's value, so a component never runs a default in place of a refused record | invariant | RFC 13 §6 | T3.8 |

**Observability**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R29 | `config_refused_total` by component and reason, `config_bound_refusals_total`, and `config_generation_lag_seconds` per kind (`settings`, `shares`, `principals`); no label names a record or a secret | MUST | RFC 13 §8 | T3.9 |
| R30 | The reconcile scan and `Diff` are measured at 10⁴ shares' settings, against a ceiling of 100 ms | invariant | RFC 13's plan, §4.3 | T3.9 |

Key words in the owned sections this epic does not carry: §5's "a node **MUST** run a live change
within 5 s" is reconcile's, E5 and RFC 23's; §5's settings nodes "**MUST** agree" are a class each
component declares, checked against Appendix B by E5; §5's two-step value is the cluster's (O1);
§5.1's share binding "**MUST NOT** change" is the share's write, RFC 23's; §5.2's capability check
"**MUST** find" the claim waits in O1, D9's refusal standing in for it here; §5.4's node "**MUST**
refuse to join" is E2's; §4.2's required keys are each component's section, which `Required`
declares, and its `ErrNotEscrowed` is RFC 5's plan's.

## 4. Decisions this epic takes

- **D1, one settings schema version.** Every section's rows carry one schema version, the
  settings schema format's, which E2's D6 declares beside the other formats. A field added,
  removed or renamed in any section raises it, as RFC 13's plan's D11 says for a section. One
  number serves while no format has a second version, and needs no option on E1's frozen `Of`.
  No marker.
- **D2, the view refuses a newer schema on write, not on read.** `PutSetting` refuses a row whose
  `Schema` is newer than the binary's. `AllSettings` returns such a row as it is, and `Read`
  refuses its section only. A refusal in the scan would stop every section on one row, which RFC
  13 §6's "stops only what it configures" forbids. No marker.
- **D3, the benchmark measures the scan and `Diff`; E5 adds the rest.** RFC 13's plan lists the
  secrets pass and the `NodeStatus` write in this benchmark. Neither exists before E4's `Resolve`
  and RFC 23's `NodeStatus`, so E5, which builds the reconcile they belong to, adds them and judges
  the 100 ms ceiling on the full figure. No marker.
- **D4, a refusal is a typed error in RFC 13 §6's shape.** Each of `Check`'s and `Read`'s refusals
  matches its frozen error with `errors.Is` and prints as `<scope>: <key>: <rule>[; <what to
  do>]`, the scope written as `installation`, `node <id>`, `share <id>`, and so on. The API and
  the component print the same line, which RFC 13 §9's "§6 one validator" row asserts. Pending
  #4486's review of the shape. No marker.
- **D5, the lag tracker is exported for E5.** `config_generation_lag_seconds` measures time since
  a node's reconciled position fell behind the store's counter. The position is RFC 23's node's,
  so this epic exports a small tracker, `ObserveLag(kind, counter, reconciled)`, and E5 calls it at
  each poll. No marker.
- **D6, the block format version's key is declared in the test.** RFC 13 Appendix B's "block format
  version to write" row names no key, and no RFC 4 plan declares one yet, so `Check`'s gate takes
  the key from a section option and T3.6's test declares it. No marker.

## 5. Target tree

```text
pkg/metadata/kvstore/settings.go        T3.1  DeleteSetting, AllSettings, the schema refusal
pkg/settings/section.go                 T3.2  Of, its options, the class tags, the rank
pkg/settings/table.go                   T3.2  Table, lookup on a dot boundary
pkg/settings/decode.go                  T3.3  the strict decode, durations, sizes
pkg/settings/decode_fuzz_test.go        T3.3  FuzzDecode
internal/bytesize/bytesize.go           T3.3  the product past uint64 refused
pkg/settings/read.go                    T3.4  Snapshot, Read, Get
pkg/settings/check.go                   T3.5  Check, the effective value after a write
pkg/settings/gates.go                   T3.6  bound, store location, format version
pkg/settings/secretref.go               T3.7  the reference's kind and owner
pkg/settings/diff.go                    T3.8  Diff, Keep
pkg/settings/metrics.go                 T3.9  the three series, ObserveLag
pkg/settings/read_bench_test.go         T3.9  the reconcile scan benchmark
```

## 6. Subtasks

### T3.1 The kvstore settings view

**Subtask T3.1 estimate: 1.5 man-days.**

- **Track:** B. **Depends on:** S1.
- **Covers:** R4, R5, R6.
- **Deliverables:** `DeleteSetting` and its transaction form, deleting the row its `expect` names
  and raising `CFGGEN` in the same transaction; `AllSettings`, one prefix scan of `CFG` across
  every scope; `PutSetting` refusing a newer `Schema` (D2); the `ponytail:` marker of the one
  `CFGGEN` key at its raise (RFC 13's plan, §4.3).
- **Acceptance criteria:**
  - A delete with a stale `expect` returns `ErrGenerationMismatch` and deletes nothing.
  - `AllSettings` returns the rows of all five scopes in one scan, a newer-schema row among them.
- **Tests:** on Badger under `t.TempDir()`: a delete raises the settings generation by one; a
  delete of a share's row while the share is being removed is refused as `PutSetting`'s is; a scan
  over rows at every scope returns each once; a put with `Schema` one past the binary's is refused
  and the row unchanged (catches a check on read only).

### T3.2 `Of`, the table and their options

**Subtask T3.2 estimate: 2.5 man-days.**

- **Track:** B. **Depends on:** S1.
- **Covers:** R2, R7, R8, R9, R10.
- **Deliverables:** `Of`'s body: the section from `T`'s fields and their `class` tags (E1's D9),
  its scope form, `WriteRole`, `Required`, `Per` and `SecretRef(field, kind)`; panics on an
  unclassified field, an option naming no field and an unknown write role; the explicit role rank;
  `Table`'s body, panicking on two prefixes where neither is a dot-boundary prefix of the other
  yet one key could match both, and `lookup`'s longest dot-boundary match.
- **Acceptance criteria:**
  - A section declared for `pkg/adapter/settings`' SMB struct builds, and one of its fields left
    untagged panics, naming the field.
  - `identity.oidc.issuer` resolves to `identity.oidc` beside an `identity` section.
- **Tests:** one row per panic; the rank compares `viewer` below `operator` below `admin`, and an
  empty role below `viewer` (catches a comparison by name); `identity.oidcx` does not resolve to
  `identity.oidc` (catches a prefix match off a dot boundary).

### T3.3 The strict decode

**Subtask T3.3 estimate: 2.5 man-days.**

- **Track:** B. **Depends on:** T3.2.
- **Covers:** R11, R12, R13, R14, R15.
- **Deliverables:** a section's value decoded over its defaults, refusing an unknown field, a key
  in another case than declared (Go's JSON decode matches names without case, so the decode
  checks each key itself), a bare number for a duration, `null` into a non-pointer field, a
  duplicate key and invalid UTF-8; sizes through `internal/bytesize`, its product past `uint64`
  refused on the integer path, which wraps today, and on the fraction path, whose float
  conversion is undefined past it; `FuzzDecode`.
- **Acceptance criteria:** each refusal names the field and the rule.
- **Tests:** a table over `internal/bytesize`'s grammar, one row per form: decimal `K`/`KB` to
  `T`/`TB`, binary `Ki`/`KiB` to `Ti`/`TiB`, a bare `B` or no unit, units without case, `1.5GiB`,
  `1.5` as 1 and `0.1KiB` as 102, whitespace around and inside; `16EiB` and `18446744073709551616`
  refused (catches the wrap); `30` for a duration refused and `"30s"` accepted; `{"port":1,"port":2}`
  refused; `adapters.smb.Port` refused; `FuzzDecode` seeded with every row.

### T3.4 The snapshot, `Read` and `Get`

**Subtask T3.4 estimate: 2 man-days.**

- **Track:** B. **Depends on:** T3.1, T3.3.
- **Covers:** R1, R3, R4, R7, R16, R18.
- **Deliverables:** `Read(ctx, cp, table, node)`: one `AllSettings` scan, each section decoded on
  its own from its installation value, then the narrower override, per thing where the form is per
  thing, then its required keys, then `Validate`; a newer-schema row refusing its section; the
  sections it could not decode returned as refused, with D4's errors. `Get`, a lookup of a value
  `Read` decoded, with no decode. The `ponytail:` marker of the one-scan reconcile.
- **Acceptance criteria:**
  - One bad row refuses its section and no other.
  - `Get` on a refused section returns its error, never the default.
- **Tests:** a node override wins over the installation's value, and a delete of the override
  brings it back; a per-journal value read for two journals; a required key unset refuses the
  section (RFC 13 §9's "§6 refused" at the library's layer); an invalid value refuses and no
  default is returned for it; a newer-schema row refuses its section (RFC 13 §9's "§2.3 schema").

### T3.5 `Check`

**Subtask T3.5 estimate: 2.5 man-days.**

- **Track:** B. **Depends on:** T3.2, T3.3.
- **Covers:** R4, R7, R9, R15, R16, R17, R19, R20, R21.
- **Deliverables:** `Check(tx, caller, w, holds)`'s first gates, in RFC 13's plan's order: `lookup`
  for the unknown key and the wrong scope, the newer schema, the write role, then the effective
  value after the write, a put's row or what a delete leaves, decoded and validated through the
  section's own `Validate`; every related row read through `tx`, so it is tracked; the `ponytail:`
  marker of the rule across components (RFC 13's plan, §4.3).
- **Acceptance criteria:**
  - `Check` and `Read` return the same error for the same invalid row (RFC 13 §9's "§6 one
    validator").
  - `pkg/settings` imports no store package.
- **Tests:** on Badger under `t.TempDir()`, through `Write`: RFC 13 §9's "§6 unknown field" row,
  one misspelled field per declared section; a delete that leaves a required key unset refused; an
  `operator`'s first put to an `admin`-only section refused as its later change is, and an
  `admin`'s passing; an `operator`'s put to a default section passing; an `operator`'s write to
  `identity.oidc.audience` refused beside an `identity` section an `operator` may write; a
  concurrent write of a row `Check` read makes the transaction retry (catches an untracked read).

### T3.6 `Check`'s bound, store location and format version gates

**Subtask T3.6 estimate: 2.5 man-days.**

- **Track:** B. **Depends on:** T3.5.
- **Covers:** R22, R23, R24, R26.
- **Deliverables:** the bound refusal on `holds`, for a put and for a delete, a put of the same
  value passing; D9's refusal of a store location's change, its first value passing, with its
  `decision:` note; the block format version past this node's range refused (D6); D11's
  `decision:` note.
- **Acceptance criteria:** a store error from `holds` is returned as itself, never counted as a
  bound refusal.
- **Tests:** a bound key changed with content refused, then with none accepted, for a put and for
  a delete; the same value put again with content accepted; a bucket's first value accepted, its
  change refused naming RFC 13 §5.2 even with no content; a block format version one past the
  range refused and the newest one accepted, on a key the test declares.

### T3.7 Secret references

**Subtask T3.7 estimate: 1.5 man-days.**

- **Track:** B. **Depends on:** T3.5, S2.
- **Covers:** R25.
- **Deliverables:** `Check` reading each `SecretRef` field's `Secret` through `tx` and refusing a
  reference to another kind or another owner.
- **Acceptance criteria:** the refusal names the field, never the secret's value.
- **Tests:** with secrets sealed by E4: a bind credential reference swapped to the OpenID Connect
  client secret refused, and to another record's bind credential refused; D9's credential
  reference repointed at a freshly sealed `remote credential` refused, and its first value
  accepted.

### T3.8 `Diff`, `Keep` and the classes

**Subtask T3.8 estimate: 1.5 man-days.**

- **Track:** B. **Depends on:** T3.4.
- **Covers:** R27, R28.
- **Deliverables:** `Diff(prev, cur)`, listing each changed key with its section, scope and class,
  every key with no previous snapshot; `Keep(section, prev)`.
- **Acceptance criteria:** a key whose value is unchanged is never listed.
- **Tests:** one change per class lands with that class; a per-thing value changed for one journal
  lists that journal only; `Keep` returns the previous value after a refused read.

### T3.9 The series and the reconcile benchmark

**Subtask T3.9 estimate: 2 man-days.**

- **Track:** B. **Depends on:** T3.4, T3.5, T3.8.
- **Covers:** R29, R30.
- **Deliverables:** the three series, `config_refused_total` raised by `Check` and `Read` by
  component and reason, `config_bound_refusals_total` by T3.6's refusal, and `ObserveLag` (D5);
  `BenchmarkReconcileScan`, `AllSettings` and `Diff` at 10⁴ shares' settings on Badger under
  `t.TempDir()`, in the `manual` tier with `ReportAllocs` (D3).
- **Acceptance criteria:** no label holds a record's ID, a key's value or a secret.
- **Tests:** each series read from the registry after the refusal that raises it; the lag at zero
  while caught up, and growing while behind; the benchmark runs once in the unit tier with a
  small count, so it cannot rot.

## 7. Order of work

```mermaid
flowchart TD
  S1(["S1: configuration surface frozen"]):::sync --> T3_1["T3.1 kvstore settings view"]
  S1 --> T3_2["T3.2 Of, table, options"]
  T3_2 --> T3_3["T3.3 Strict decode"]
  T3_1 & T3_3 --> T3_4["T3.4 Snapshot, Read, Get"]
  T3_2 & T3_3 --> T3_5["T3.5 Check"]
  T3_5 --> T3_6["T3.6 Bound, location, format gates"]
  S2(["S2: secrets sealed"]):::sync --> T3_7["T3.7 Secret references"]
  T3_5 --> T3_7
  T3_4 --> T3_8["T3.8 Diff, Keep, classes"]
  T3_4 & T3_5 & T3_8 --> T3_9["T3.9 Series, benchmark"]
  classDef sync stroke:#2c5aa0,stroke-width:3px,stroke-dasharray:4 2
```

T3.1 and T3.2 start at once. Checkpoint after T3.4: a section reads end to end, enough for E5 to
build the node's table. T3.7 is the one subtask that waits outside the epic, on E4's S2.

## 8. Suggested pull requests

| PR | Subtasks | Man-days | After | Why together |
| --- | --- | --- | --- | --- |
| 1 | T3.1, T3.2, T3.3 | 6.5 | S1 | the view, the declarations and the decode, each reviewable alone |
| 2 | T3.4, T3.8 | 3.5 | PR 1 | the read side, what reconcile calls |
| 3 | T3.5, T3.6 | 5 | PR 1 | the write check and its gates, the epic's least certain estimate |
| 4 | T3.7, T3.9 | 3.5 | PR 2, PR 3, S2 | the secret references once E4 seals, and the series and benchmark over the finished code |

A suggestion only: one pull request per subtask works as well.

## 9. Open points

- **The refusal shape (D4)** follows #4486's §6. Its review may change the line, not the gates.
- **The one-scan read's name.** RFC 16 §3 has no read of every scope's settings; T3.1 builds it as
  E1 names it, `AllSettings`. Pending #4473.
- **The block format version's key (D6).** RFC 13 Appendix B's row names none; its component's
  plan declares it.
- **The secret kinds of the bind credential and the client secret** come from E1's D8, pending
  #4472; T3.7's test uses them.
- **The estimate.** 18.5 man-days, as RFC 13's plan first estimated, in 9 subtasks rather than 8:
  the secret references wait for S2, so they are a subtask of their own.
