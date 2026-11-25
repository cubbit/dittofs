# Epic 6: transforms v2, namespace keys and the provider

**Sources:** [RFC 5](../docs/internals/rfcs/rfc-5-transforms.md) §2.5 (material held by
namespace, kind and ID; fingerprints and their refusal; the kinds the provider holds that no
transform consumes); §2.7's material errors as the provider returns them, and its rule that a store
whose material cannot load does not open; §3.1's `keys.Provider` (`Current`, `Get`) and
`ErrNotEscrowed`; Appendix B.1's key wrap, its associated data and the key file's input of it, the
2³² wrap bound and its count, the stored wrap, the lengths of keys and the plain-terms rule that
master keys never leave the provider; Appendix B.2's key table, its salvage key, "every namespace has
a chunk-ID key and an export key from its creation" with its `decision:` note, the escrow rule, "no
storage-key fallback", key records, export key IDs and "the key records keep every key ID";
Appendix B.4's rows at the provider and its conditions; Appendix B.6's two gauges; §7's T7, T12,
T14 and T18 as the provider meets them; §8.1's §2.5, T18, T22 (the key file), wrap-count and
golden-vector (fingerprint) rows; §8.2's T7, T14 and T18 rows, the provider's halves.
[RFC 16](../docs/internals/rfcs/rfc-16-metadata-store.md) §4.2's rows `NS‖ns‖key‖kind‖keyID`,
`MKW‖masterKeyID`, §4.2.1's encoding of key IDs and master key IDs, and §4.3's codecs. [RFC 13](../docs/internals/rfcs/rfc-13-configuration.md)
§4.2 (a key file's verified escrow), §7 (the key rules) and §9's rows "§7 namespace keys", "§7
chunk-ID key" and "§7 key table" (its non-encrypting-namespace and escrow clauses), and Appendix C
(the key file's passphrase). [RFC 0](../docs/internals/rfcs/rfc-0-data-lifecycle.md) §1.2 (component
imports, as amended). Every citation is the RFCs' text on `develop`; the code lands on
`feat/storage-v2`, and the paths below are paths there.
**Track:** C.
**Epic 6 estimate: 8 subtasks, 18 man-days.**
**Depends on:** **S2**: from E2, `keystest.Run` with its `Harness` (`Give`, `Replace`, `Destroy`,
`Calls`) and its salvage row, and `transformtest.LoadVectors` with the fingerprint, key wrap AAD and
key-file stored-wrap vectors. From E1, `keys.MaterialID`, `keys.Provider`, the three kinds,
`keys.SalvageWrapper`, the three material errors, and the import test with its allowlist, which
names `internal/keyfile`, `golang.org/x/crypto/...` and `github.com/google/uuid`. Nothing from E5:
the key-file wrap calls the standard library's AES-256-GCM itself. On `feat/storage-v2`,
`pkg/keyprovider`'s key-file format, `pkg/metadata/kv` and its `keys` and `kvtest` packages,
`pkg/metadata/kvstore`, `badgerkv` and `fdbkvtest`.
**Blocks:**
- E7, which adds key-service custody behind the custody seam of T6.3 and passes its suite.
- E8, which builds rotation, the bucket's `keys` object with the `keyseq` key lock, the destroyed
  marking and the notice on the key records, their codec, the view's guarded `Update` and the key
  creation of this epic.
- E9, which calls `CreateKeys` from namespace creation, starts `VerifyEscrow` from the
  `verify-escrow` action, records its result in the store configuration, maps `ErrNotEscrowed`, and
  defines the gauges and conditions over `Status`.
- Outside RFC 5's epics, the RFC 8 offload plan: its epic 20 calls `WrapSalvageKey`, and its epic
  21's content wiring builds the provider with `kvstore`'s view, the `storage` key's ID and
  fingerprint, and turns `Status` into collectors and a health source.

---

## 1. Goal

Epic 6 builds the material provider of one store configuration: the namespace keys of RFC 5
Appendix B.2, each stored as a key record in the metadata store and wrapped under a master key read
from a key file, served by namespace, kind and ID with their fingerprints checked, and created with a
namespace as the key table says.

**At the end of the epic:**

- `pkg/transform/keys/provider` implements `keys.Provider` and `keys.SalvageWrapper`, and passes E2's
  `keystest.Run` with the same call count E2's in-memory provider exports (T6.6);
- every namespace key is a key record under `NS‖ns‖key‖kind‖keyID`, wrapped under the key-file
  master key with associated data naming its namespace, kind and ID, in Appendix B.1's stored-wrap
  layout, except a non-encrypting namespace's chunk-ID key, held in the clear (T6.3, T6.5, T6.8);
- every wrap first raises the master key's one server-wide count, `MKW‖masterKeyID`, and the 2³²nd
  wrap is the last (T6.3);
- `Get` refuses material whose fingerprint no longer matches, tells a key it was never given from
  one it cannot reach and from one destroyed, and reports the first two by ID for RFC 25's
  conditions (T6.5, T6.6);
- a namespace gets its keys at creation per Appendix B.2's table, once, under concurrent and torn
  creations (T6.8); an encrypting one is refused `ErrNotEscrowed` until the key file's escrow check
  has passed (T6.7, T6.8); a master key that is the `storage` key is refused (T6.3);
- a salvage key is drawn and wrapped under the configuration's master key, bound to its export
  (T6.4);
- the key-file format lives in `internal/keyfile`, which `pkg/keyprovider` and the provider both
  import (T6.1).

### 1.1 In scope

`internal/keyfile`; the `MKW` kind in `pkg/metadata/kv/keys`; `kvstore`'s key-record view; `pkg/transform/keys/provider`: the stored wrap, the key-file custody with its
wrap count and its suite, the key record and its codec, `Current` and `Get` with their cache and
failures, `Status`, the escrow check, namespace key creation, the salvage-key wrap; `ErrNotEscrowed`
in `pkg/transform/keys`; the fuzz targets, the allocation test and the keystest run.

### 1.2 Out of scope

- Key-service custody over KMIP, the version-qualified IDs a service reports, `internal/kmip`, and
  §8.1's key-management and T22 key-service rows: E7.
- A key record reaching the bucket's `keys` object before it becomes current, at creation as at
  every rotation; the object itself with its sequence and MAC, and the namespace's key lock,
  `NS‖ns‖keyseq‖target`, with its subtag, which every writer of a `keys` object takes;
  the non-encrypting namespace's chunk-ID key in that object; data-key and master-key rotation,
  the key-file escrow check at rotation, retired master-key files; the primary-version and
  key-state read; marking `destroyed` the records a destroyed master key wraps; the
  destroyed-material notice, and dropping a destroyed key from the cache: E8.
- The settings rows, the store configuration's record of a passed escrow check (D6), the
  `verify-escrow`, `rotate-*` operations and their `dfsctl` commands, calling key creation from
  namespace creation, `ErrNotEscrowed` through the API, the series specs, conditions and alert
  rules: E9.
- A namespace that does not open because its key was refused, and §8.2 T7 through the engine: the
  RFC 8 offload plan and E11.
- `Provider.Remove`, its fence, `ErrMaterialInUse`, and erasing a destroyed key's wrapped bytes:
  the RFC 5 plan's §6.1 deferral ledger, its `Provider.Remove` row (RFC 9).
- The escrow refusal at backup-policy creation, the export key's next generation and the `retiring`
  state's pending set: the ledger's export-key row (RFC 26). The recovery import and "every chunk
  verifies from the bucket alone": the ledger's recovery-import row (RFC 26).
- A key-file master key current in one installation only, which a move's importer enforces: the
  ledger's re-home and move row (RFC 27).
- Building the provider in the node, the `storage` key's fingerprint, the collectors and the health
  source: the RFC 8 offload plan's epic 21.

### 1.3 Names used here

- **E1**, epic 1, interfaces: `keys.Provider`, `keys.MaterialID`, the kinds, `keys.SalvageWrapper`,
  the material errors, the import test and its allowlist.
- **E2**, epic 2, test foundations: `keystest.Run`, `keystest.Harness`, `keystest.Memory`, and
  `transformtest.LoadVectors` with RFC 5's golden vectors.
- **E3**, epic 3, the chain, whose `unit` option `transform-allocs` runs `TestAllocs…` tests.
- **E4**, epic 4, compression, whose first fuzz target added RFC 5's suites line naming the fuzz
  targets.
- **E5**, epic 5, encryption, which consumes the data key through `Get`.
- **E6**, this epic.
- **E7**, epic 7, key-service custody over KMIP.
- **E8**, epic 8, rotation and the destroyed-material notice.
- **E9**, epic 9, the integration epic: settings, the key operations, error mapping, series specs,
  conditions and alert rules.
- **E11**, epic 11, E2E green: RFC 5's rows through the codec and the engine.
- **S2**, E2's conformance suites and vectors are in.
- **The content wiring**, `internal/node/content`, which builds the provider; and **the RFC 8
  offload plan's epics 20 and 21**: the salvage export, and the node's composition of this
  component.

## 2. How the general rules apply

The rules are those of
[RFC 5's plan, §1.5](rfc-5-transforms-plan.md#15-general-rules-for-each-epic), cited by number and
title and not copied.

| Rule | Subtasks | How |
| --- | --- | --- |
| rule 1, **independent unit tests** ([`CLAUDE.md`, "Writing tests"](../CLAUDE.md#writing-tests)) | all | the provider's tests run on `kvstore` over `badgerkv` in memory, a real implementation (D2); randomness is an injected `io.Reader` and time an injected clock (kind 1); a torn commit is `kvtest.FaultKV` beneath `kvstore` (kind 3); the provider passes E2's `keystest.Run`. No in-memory view stands in for `kvstore` |
| rule 2, **fuzz every decoder of bytes this component did not just write** ([`CLAUDE.md`, "Writing tests"](../CLAUDE.md#writing-tests)) | T6.3, T6.5 | `FuzzStoredWrap` (T6.3) and `FuzzKeyRecord` (T6.5): no panic, every length read inside the buffer, and what decodes re-encodes to bytes that decode to the same value. Both join the nightly `fuzz` suite in the form E4's target did, and RFC 5 §8.3's suites line names them |
| rule 3, **observability follows RFC 25** | T6.5, T6.6 | the provider imports no `pkg/metrics` or `pkg/health`: `Status` returns, as plain values, the material it cannot reach, the material it was never given, and each namespace's current data key's start, which the content wiring turns into `material_unavailable`, `material_unknown` and the two gauges (D8). A planted key's bytes are searched for in every log line and error (T6.6, R43) |
| rule 4, **profiling** | T6.5 | every test under `internal/prof`'s `Watch`; `TestAllocsProviderGet` asserts no allocation for `Get` on a cached key and joins `transform-allocs` by name |
| rule 5, **public interface and internals** ([`CLAUDE.md`, "Verifying a change"](../CLAUDE.md#verifying-a-change)) | T6.5, T6.6, T6.8 | each rule is tested where its value is consumed: a record copied to another namespace is refused by `Get`, not only by the unwrap; a created namespace's keys are read back through `Current` and `Get`; concurrency runs under `-race` |
| rule 6, **known failures shrink** | none | `unit` and `integration` keep no `known.yaml` for this component; every test lands green |
| rule 7, **v2 ignores v1, and imports only what RFC 0 §1.2 allows** | T6.1, T6.2, T6.3 | the provider imports `keys`, `entity`, `internal/keyfile`, `golang.org/x/crypto`, `github.com/google/uuid` and the standard library; never `kvstore`, `pkg/keyprovider`, `pkg/settings`, `pkg/metrics` or `pkg/health`. It declares the key-record view in plain types, which `kvstore` satisfies without importing it (D1). `internal/keyfile` imports no module-internal package (D14). E1's import test holds both |
| rule 8, **real dependencies for properties of those dependencies** ([`CLAUDE.md`, "Writing tests"](../CLAUDE.md#writing-tests)) | T6.2, T6.8 | `kvstore`'s view runs over Badger in memory and under `t.TempDir()`, and over FoundationDB where `fdbkvtest` runs; key creation's ordering claim has its crash-seam test with `kvtest.FaultKV` tearing the commit beneath `kvstore` |
| rule 9, **code comment conventions** ([`CLAUDE.md`, "Code comments"](../CLAUDE.md#code-comments)) | T6.2, T6.3, T6.5, T6.7, T6.8 | RFC 5's `decision:` note on the clear chunk-ID key at key creation (R27); D1's at the view's `Update`, D5's at the wrap count, D6's at `VerifyEscrow`, D11's at the corrupt-record mapping, D13's at `WrapSalvageKey`; D10's `ponytail:` at the cache |
| rule 10, **a test must be able to fail** ([`CLAUDE.md`, "Writing tests"](../CLAUDE.md#writing-tests)) | all | each test in §6 names the bug it catches; the fingerprint and wrap vectors are E2's, computed by an independent implementation, never by this code |
| rule 11, **a symbol is exported only if RFC 5's interface or a consumer outside the package uses it** | T6.3, T6.5, T6.6, T6.7, T6.8 | each export names its consumer in §6: `New`, `Config`, `Deps`, `Row`, `KeyRecords` for the content wiring and `kvstore`; `Calls` for `keystest.Harness`; `Status` for the content wiring; `CreateKeys`, `VerifyEscrow`, `Escrow` and `ErrNotEscrowed` for E9. They are frozen when E9 first calls them |
| rule 12, **each subtask creates only what it declares** | all | the view lands with its first consumer, the custody (T6.3); the custody seam has two implementations, the key file here and the key service in E7, so it is an interface with a suite; the `keyseq` subtag is not added: this epic writes no `keys` object, and its writers conflict through a range guard (D1), so the key lock lands with E8's first writer of one |

## 3. Requirements and invariants

Every obligation the sources place on this epic, numbered so that subtasks and tests can cite them.
`Source` is RFC 5 unless it names another source.

**Imports and the moved format**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R1 | The provider imports another component's contract, never its implementation: not `pkg/metadata/kvstore`, not `pkg/keyprovider`, not `pkg/settings`, `pkg/metrics` or `pkg/health`; it declares what it needs of the metadata store as a view of its own, which the store satisfies without importing the provider | MUST NOT | RFC 0 §1.2; RFC 5 plan §1.5 rule 7, D7 | T6.2, T6.3 |
| R2 | The key-file format moves to `internal/keyfile`, which `pkg/keyprovider` and the provider both import; `pkg/keyprovider`'s exported surface and its tests do not change, and `internal/keyfile` imports neither `pkg/keyprovider` nor any other implementation | MUST | RFC 5 plan D5, §1.5 rule 7; RFC 0 §1.2 | T6.1 |

**Material, fingerprints and failures**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R3 | Material is held by namespace, kind and ID, and every call names its namespace: `Current` of one namespace never returns another's material, and `Get` of an ID under a namespace that does not hold it is `ErrUnknownMaterial` | MUST, invariant | §2.5, §3.1, §7 T14, §8.2 T14 | T6.5, T6.6 |
| R4 | Each piece of material carries a fingerprint, `HMAC-SHA256(material, "dittofs key check v1")`, 32 bytes, recorded with its ID; it is checked after unwrapping | MUST | §2.5; Appendix B.1 (labels, lengths); Appendix B.2 | T6.5 |
| R5 | The provider refuses to serve an ID whose material no longer matches its recorded fingerprint, as `ErrMaterialUnavailable`, and reports it for the `material_unavailable` condition; nothing is decoded or written with it | MUST | §2.5; §7 T12; §8.1 §2.5 row; Appendix B.4 | T6.5, T6.6 |
| R6 | `Get` returns `ErrUnknownMaterial` for an ID the provider was never given, `ErrMaterialUnavailable` for one it cannot reach now or whose content no longer matches its fingerprint, and `ErrMaterialDestroyed` for one whose record is `destroyed` | MUST | §3.1 (`Provider.Get`); §2.7; Appendix B.2 ("the provider can tell a key it never had…") | T6.5 |
| R7 | `Current` is the material new bodies of a kind use in a namespace; for a kind the namespace was never given it is `ErrUnknownMaterial` | MUST | §3.1 (`Provider.Current`); E2 D3 | T6.5, T6.6 |
| R8 | An `ErrUnknownMaterial` answer reports the ID for the `material_unknown` condition, until the key is loaded and a read of it succeeds | MUST | §2.7; Appendix B.4; §8.2 T7; RFC 25 §3.9 | T6.6 |
| R9 | `encryption_keys_unavailable` is the count of keys the provider cannot currently return with a matching fingerprint, and the `material_unavailable` condition names them | MUST | Appendix B.6; Appendix B.4; RFC 5 plan §1.5 rule 3 | T6.6 |
| R10 | `encryption_current_key_age_seconds`, labelled `namespace`, is how long the namespace's current data key has been current | MUST | Appendix B.6 | T6.5, T6.6 |
| R11 | Each call of `Current` or `Get`, a failing one included, raises an exported count, so a test can assert that the provider records no call | test row | §8.2 T12, T14; RFC 5 plan §1.4; E2 R19 | T6.6 |
| R12 | The provider passes E2's provider suite, `keystest.Run` | test row | RFC 5 plan §1.4; E2 T2.1, T2.2 | T6.6 |
| R41 | A master key that cannot be read or unwrapped at start fails the provider's construction, so the store does not open and nothing is written without it | MUST | §2.7 (a chain that cannot be built); Appendix B.4 (unreachable at start) | T6.3 |
| R42 | Material that cannot be reached when read is `ErrMaterialUnavailable`, never zeros, and a later read succeeds once it can be reached | MUST | Appendix B.4; §7 T7 | T6.5 |

**Key records**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R13 | Each namespace key is a key record under `NS‖ns‖key‖kind‖keyID`: its kind, ID, fingerprint, the ID of the master key that wraps it, the wrapped bytes (the key itself for a non-encrypting namespace's chunk-ID key, with no master key ID), and its state, `current`, `retired` or `destroyed`; a destroyed key keeps only ID, fingerprint and state | MUST | Appendix B.2 ("Where wrapped keys live"); RFC 16 §4.2 | T6.2, T6.5 |
| R14 | A record whose master key ID differs from the one inside its wrapped bytes is refused as corrupt | MUST | RFC 16 §4.2 (`NS‖ns‖key‖kind‖keyID`) | T6.5 |
| R15 | Each persisted value is a version byte and a fixed field order; an unknown version is refused | MUST | RFC 16 §4.3 | T6.2, T6.5 |
| R16 | The key records keep every key ID the namespace has ever issued: no write of this epic deletes one | invariant | Appendix B.2 | T6.2 |
| R18 | A key record holds no secret: the master key is never written to the metadata store | invariant | Appendix B.2; RFC 13 §7 | T6.5 |

**The key wrap and its count**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R19 | A namespace key is stored only wrapped: `nonce ‖ AES-256-GCM(master key, nonce, key bytes, AAD)`, the nonce 12 bytes from a cryptographic random source, the tag 16 bytes; the AAD is namespace ID ‖ kind ‖ key ID, each with a 4-byte big-endian length, and the key file passes those bytes to AES-256-GCM | MUST | Appendix B.1 ("Key wrap", "Lengths and encodings", the key wrap AAD row, "The AAD reaches each provider's input") | T6.3 |
| R20 | Every wrapped key is stored as provider kind (1 byte, `0x01` for the key file) ‖ master-key ID length (2 bytes, big-endian) ‖ master-key ID ‖ wrap length (4 bytes, big-endian) ‖ wrap bytes; for the key file the master-key ID is a minted ID's bytes | MUST | Appendix B.1 ("stored wrap") | T6.3 |
| R21 | A wrapped record whose namespace, kind or key ID is changed in the AAD, or which is copied into another namespace's key records, fails to unwrap | MUST, test row | Appendix B.1; §7 T18; §8.1 T18 | T6.3, T6.5 |
| R22 | A master key makes at most 2³² wraps: every wrap first raises that master key's one server-wide count, `MKW‖masterKeyID`, by a conditional write before the wrap's bytes are used, and the wrap at a count of 2³² is refused with an error asking for a master-key rotation; every wrap counts, a key record's, a salvage key's and the escrow check's test record's | MUST | Appendix B.1 ("A master key makes at most 2³² wraps"); RFC 16 §4.2 (`MKW‖masterKeyID`); §8.1's wrap-count row | T6.2, T6.3, T6.4, T6.7 |
| R23 | The key-file provider holds its master key in its own process memory; a master key never leaves the provider, and a namespace key is stored only wrapped | invariant | Appendix B.1 (plain terms) | T6.3 |
| R24 | Every master, data, chunking, chunk-ID and export key is 32 bytes from a cryptographic random source, drawn when created, never derived | MUST | Appendix B.1 ("Lengths and encodings") | T6.4, T6.8 |
| R45 | The provider's fingerprint, key wrap AAD and key-file stored wrap equal RFC 5's golden vectors, computed by an independent implementation | test row | §8.1's Appendix B.1 golden-vector row (the fingerprint), §8.1 T22 (the key file's stored wrap); E2 T2.5 | T6.3, T6.5 |

**The key table and key creation**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R26 | Every namespace has a chunk-ID key and an export key from its creation; an encrypting one also gets its first data key and chunking key then; a non-encrypting one has no chunking key | MUST | Appendix B.2 (table; "Every namespace has a chunk-ID key…"); §7 T18; §8.2 T18 | T6.8 |
| R27 | A non-encrypting namespace's chunk-ID key is held in the clear in its key record, with no master key ID; every other namespace key is wrapped under a master key of its store configuration | MUST | Appendix B.2 (table and its `decision:` note); RFC 13 §7; RFC 13 §9 "§7 key table" | T6.8 |
| R28 | Of each kind a namespace has, one key is current at a time: two writers of one namespace's key records conflict, so concurrent creations of its keys leave one set | invariant | Appendix B.2 (table: "one current at a time") | T6.2, T6.8 |
| R29 | An export key's ID is its generation, 8 bytes big-endian, 1 for the namespace's first, followed by a minted ID's bytes | MUST | Appendix B.2 ("An export key's ID carries its generation"); RFC 16 §4.2 | T6.8 |
| R31 | A namespace key ID is at most 255 bytes, the bound of RFC 4's header census and of the metadata store's key encoding | bound | RFC 16 §4.2.1; RFC 4 §3.2 | T6.8 |
| R32 | A crash torn beneath key creation leaves no wrap in use without its committed record: a record is committed before its key is served, and a repeated creation after a crash finds the committed set or makes a fresh one | invariant, test row | RFC 5 plan §1.5 rule 8; Appendix B.1 ("a crash can only overcount") | T6.8 |
| R46 | Two non-encrypting namespaces get different chunk-ID keys, so one file's chunk IDs differ between them and from the unkeyed hash of its bytes | test row | RFC 13 §9 "§7 chunk-ID key"; Appendix B.2 | T6.8 |

**Escrow and the `storage` key**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R34 | A key-file master key's off-host escrow is verified by handing the copy back once: the provider checks that the copy unwraps a test record and that its fingerprint matches | MUST | Appendix B.2 ("Where the master key lives"); RFC 13 §4.2 | T6.7 |
| R35 | Until its current key-file master key's escrow check has passed, creating an encrypting namespace on a store configuration is refused with `ErrNotEscrowed`, naming the key; once it has passed, creation succeeds | MUST | Appendix B.2; §3.1 (`ErrNotEscrowed`); §8.2 T18; RFC 13 §4.2; RFC 13 §9 "§7 key table" | T6.7, T6.8 |
| R37 | A configuration none of whose namespaces encrypts needs no escrow: a non-encrypting namespace is created without it | MUST | Appendix B.2 ("The check is per key…") | T6.8 |
| R38 | The `storage` wrapping key never wraps a namespace key: the provider, given that key's ID and fingerprint and never the key, refuses a master key whose ID or fingerprint matches, so a master-key file holding the `storage` key's bytes under another ID is refused too | MUST NOT | Appendix B.2 ("No storage-key fallback"); §8.2 T18; RFC 13 §7; RFC 5 plan D5 | T6.3 |

**The salvage key**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R39 | A salvage key is a fresh 32-byte key for one salvage export, wrapped under the configuration's current master key with associated data naming the export, returned with its wrap in the stored-wrap layout; it belongs to no namespace, so no key record is written, and its wrap raises the master key's count | MUST | Appendix B.2 (table's `salvage-key` row; "A salvage key seals one salvage export"); Appendix B.1; RFC 5 plan D16; E2 R20 | T6.4 |
| R40 | A salvage key unwraps only under its master key and its export's associated data | test row | Appendix B.2; RFC 5 plan §5 (epic 6, "Done when") | T6.4 |

**Concurrency, logs, cost and decoders**

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R43 | No key byte, unwrapped material, master key or passphrase reaches a log line, an error string or a reported value: IDs and fingerprints only | MUST NOT | RFC 5 plan §1.5 rule 3 | T6.6 |
| R44 | `Get` on a cached key allocates nothing | bound | RFC 5 plan §1.5 rule 4 | T6.5 |
| R47 | A namespace's chunking key replaced under the same ID is refused by fingerprint | test row | RFC 13 §9 "§7 namespace keys" (the provider's half); §2.5 | T6.5 |
| R48 | The stored wrap's and the key record's decoders never panic and never read past their buffer, and what they decode re-encodes to bytes that decode to the same value | MUST | RFC 5 plan §1.5 rule 2; `CLAUDE.md` "Writing tests", "By kind" | T6.3, T6.5 |
| R49 | The provider is safe for concurrent use: `Current`, `Get`, `CreateKeys` and the wraps from many goroutines under the race detector give the results of sequential calls | invariant | RFC 5 plan §1.5 rule 5 | T6.6, T6.8 |

`R17`, `R25`, `R30`, `R33` and `R36` are not used: candidates numbered while extracting were
merged into R28 (writers of one namespace's records conflict, and one current key of each kind),
R22, the bucket gate (E8, §1.2) and R35.

## 4. Decisions this epic takes

- **D1, the key-record view is three calls in plain types, and every write is guarded.**
  The provider declares, in `pkg/transform/keys/provider`:

  ```go
  // Row is one key record as the metadata store holds it: its kind, its ID and its bytes in the
  // provider's codec. It is an alias of an unnamed struct, so kvstore satisfies KeyRecords
  // without importing this package.
  type Row = struct {
  	Kind, ID string
  	Record   []byte
  }

  type KeyRecords interface {
  	// Load returns every key record of ns.
  	Load(ctx context.Context, ns entity.NamespaceID) ([]Row, error)
  	// Update runs fn on ns's key records in one transaction that conflicts with any other write
  	// of them, and writes the rows fn returns, each replacing the record of its kind and ID. It
  	// deletes nothing. fn may run more than once and must have no other effect.
  	Update(ctx context.Context, ns entity.NamespaceID, fn func(rows []Row) ([]Row, error)) error
  	// RaiseWrapCount adds n to masterKeyID's wrap count if the sum stays at most limit, and
  	// reports whether it did.
  	RaiseWrapCount(ctx context.Context, masterKeyID []byte, n, limit uint64) (bool, error)
  }
  ```

  Go's type identity makes an unnamed struct with the same exported fields one type in every
  package, so `kvstore`'s methods, written with the literal struct, satisfy `KeyRecords` and import
  nothing of `pkg/transform` (R1). A named `Row` would force the import. `Update` calls
  `GuardRange(NS‖ns‖key‖)` before it scans, because a scan is never tracked (`kv.Reader`): without
  the guard, two creators of one namespace each see no record, write different ones under different
  IDs, and both commit. With it, the second commit aborts, `kv.KV.Update` retries `fn`, and the
  retry sees the first's records (R28). The key lock, `NS‖ns‖keyseq‖target`, is not needed for
  that: it orders writers of a `keys` object, and lands with E8's first one. A `decision:` marker at
  `Update` says that every write of `NS‖ns‖key‖…` goes through it, so the guard covers every writer.
  Prior art: `kv.Txn.GuardRange`'s own doc, "how a check over a range (the directory is empty) is
  made safe against a concurrent insert" (checked); RFC 14's `OpenRecords`, the other
  consumer-declared view; the Go specification's type identity rule for struct types (checked).

- **D2, one implementation of the view, tested where it lives; no in-memory view and no keystest
  suite for it.** RFC 5's plan put a key-record suite and a verified in-memory view in `keystest`, run
  over `kvstore` from the content wiring's tests. Nothing needs the in-memory view: the provider's
  tests may import `kvstore` (amended RFC 0 §1.2 binds production code only) and run on the real
  one over `badgerkv` in memory. With one implementation, its contract is its own tests:
  `kvstore`'s tests of `Load`, `Update` and `RaiseWrapCount` run over Badger in memory, under
  `t.TempDir()`, and over FoundationDB where `fdbkvtest` runs, in plain types, importing nothing of
  `pkg/transform`. The provider's external tests assign `kvstore`'s view to a `KeyRecords`, so the
  compiler holds the two signatures together. The RFC 8 offload plan's epic 21 then has no
  key-record suite to run (§9). Prior art: `CLAUDE.md`'s "Writing tests", real collaborators first.

- **D3, the custody seam is unexported, and its suite is a helper in the provider's own tests.**
  ```go
  type custody interface {
  	kind() byte // the stored wrap's provider kind
  	wrap(ctx context.Context, aad, key []byte) (masterKeyID, wrapped []byte, err error)
  	unwrap(ctx context.Context, masterKeyID, wrapped, aad []byte) ([]byte, error)
  }
  ```
  Both implementations live in package `provider`, the key file here and E7's `kmip.go`, so the
  interface needs no export (rule 11). Its suite, `runCustody(t, newCustody func(t *testing.T)
  custody)`, is an unexported helper in `custody_test.go` (package `provider`), which E7's
  `integration` test calls for the key service; RFC 5's plan put it in `keystest`, which would then
  import the provider for a suite only the provider's own tests run. The primary-version read is
  E7's and E8's addition to the interface. Prior art: HashiCorp Vault's seal wrappers, one interface
  per custody (from memory).

- **D4, key IDs are minted IDs' bytes.** A namespace key's ID is 16 bytes of a random UUID with its
  version bits set (`uuid.NewRandomFromReader` over the injected reader), held as the `ID` string's
  raw bytes; an export key's is its generation's 8 bytes and then such 16 bytes. So no ID exceeds
  24 bytes, far under R31's 255. The key file's master key ID, recorded in the file as a UUID's
  text, is parsed to its 16 bytes for the stored wrap and the `MKW` row; a file whose ID is not a
  UUID is refused as corrupt. Logs and errors print IDs in hex. Prior art: RFC 16's minted IDs
  (`entity.NewFileID`), checked.

- **D5, the wrap count is a conditional raise by `n` under a limit the caller passes.** The
  key-file custody calls `RaiseWrapCount(ctx, masterKeyID, 1, 1<<32)` before it draws the nonce, and
  refuses the wrap when it reports no raise, with an error naming the master key and asking for a
  rotation. Passing the limit keeps the bound in RFC 5's code and lets `kvstore`'s own test use a
  limit of 3; passing `n` lets the provider's test reach a count of 2³² − 1 through the same call
  on the real store, `RaiseWrapCount(ctx, id, 1<<32-1, 1<<32)`, with nothing seeded beneath it. A
  `decision:` marker at the raise says that a crash between the raise and the wrap overcounts by
  design. Prior art: RFC 16 §4.4's counters raised by conditional write (checked).

- **D6, a passed escrow check is recorded in the store configuration, not by the provider.** RFC 5
  says when the check must have passed but not where a pass is kept across restarts, and RFC 16
  §4.2 has no row for it; RFC 13 §4.2 lists a key file's verified escrow among the store
  configuration's values. So `VerifyEscrow` returns the master key's ID and fingerprint on a pass,
  E9's `verify-escrow` action records them in the configuration, and the provider's `Config`
  carries the passes back at every start. The provider also remembers a pass it made, so creation
  succeeds without a restart. At start, a pass whose ID matches the loaded master key but whose
  fingerprint does not is the key file replaced under its ID: construction fails (R5, R41). A
  `decision:` marker at `VerifyEscrow` names this split, to be withdrawn if RFC 16 gains a row for
  it. Prior art: none found.

- **D7, key creation is idempotent and wraps before it writes.** `CreateKeys` checks the escrow
  gate, draws and wraps every key (each wrap raising the count, D5), then calls `Update`; if the
  namespace already holds key records, `fn` returns nothing and the existing keys stand, so a
  chunk-ID key is never replaced. A conflict, a lost race or a crash after the wraps therefore only
  overcounts, which Appendix B.1 allows, and no wrap is in use without its committed record (R32).
  Its records are written `current` directly: E8 inserts the bucket's `keys` put before they become
  current. Prior art: `CREATE … IF NOT EXISTS` (from memory).

- **D8, gauges and conditions are read, not recorded.** RFC 5's plan (its D2) gives the provider a recorder
  for its gauges. Both gauges are levels, read at scrape: `Status()` returns the IDs the provider
  cannot reach, the IDs it was asked for and never given, and each loaded namespace's current data
  key's start, as plain values, and the content wiring builds the two gauges and the two conditions
  from it (R8 to R10). No recorder, no import. Prior art: the Prometheus client's `GaugeFunc` and
  collectors that read state at scrape (from memory).

- **D9, a key record carries the time it entered its state.** `encryption_current_key_age_seconds`
  needs when the current data key became current, which RFC 16 §4.2's row does not list. The codec's
  version 1 holds it as 8 bytes of Unix nanoseconds after the state, from the injected clock. A
  field added later would be a version 2 every reader keeps, so it is settled now (§9 proposes the
  row's wording). The `retiring` state's pending set lands with the export key's rotation (RFC 26),
  as a codec version 2: RFC 16 §4.3's per-value version byte reads both. Prior art: RFC 16 §4.3's
  lazy upgrade (checked).

- **D10, the cache never evicts**, as RFC 5's plan's §4.3 knowingly-simple entry says: every unwrapped key stays until stop, and
  so does each namespace's set of records, loaded on first use and replaced after the provider's own
  `Update`. The provider is the only writer of its records on one node. `Get` returns the cached
  slice, which callers must not modify, so a cached `Get` allocates nothing (R44). A `ponytail:` at
  the cache names its ceiling, about 32 bytes and an ID per key, and the upgrade, a bounded cache
  that re-reads on a miss when one node serves about 10⁶ namespaces, or a second writer (RFC 11)
  appears. Prior art: none found.

- **D11, an unreadable record is unavailable material.** A record that fails its codec, whose two
  master key IDs differ (R14), whose wrap names a master key the provider does not hold, or whose
  unwrap fails, is `ErrMaterialUnavailable`, reported in `Status` and logged once at `Error`, never
  `ErrMaterialDestroyed`: the right record or key may come back, and only a `destroyed` state is
  loss for good (Appendix B.4). A `decision:` marker says so. Prior art: none found.

- **D12, the salvage and escrow wraps use the key wrap's AAD layout with an empty namespace.**
  A salvage key's AAD is namespace ID (length 0) ‖ `salvage-key` ‖ the export bytes `WrapSalvageKey`
  is given; the escrow check's test record's is namespace ID (length 0) ‖ `escrow-check` ‖ a fresh
  minted ID. Every namespace ID is 16 bytes, so neither AAD can equal a namespace key's, and each is
  bound to its export or its check. The kind strings, with `export-key`, `master-key` and
  `salvage-key`, stay unexported, as E1's D5 left them. Prior art: none found.

- **D13, a salvage key is wrapped under the provider's own configuration's current master key.**
  Appendix B.2 says "the installation's current master key", but master keys belong to a store
  configuration and an installation may have several. The provider serves one configuration, so
  `WrapSalvageKey` wraps under that configuration's current master key, and which provider the
  engine is handed is the content wiring's choice. A `decision:` marker at `WrapSalvageKey` names
  the reading, pending the RFC author's answer (§9). Prior art: none found.

- **D14, `internal/keyfile` takes the whole format, not the file's publication.** It holds the
  encoding and decoding of the key file: `Generate(passphrase string, time, memoryKiB uint32)` and
  `Open(raw []byte, passphrase string) (id string, key []byte, err error)`, with the Argon2 bounds and
  the format's errors. `CreateKeyFile`, which publishes a file with a hard link and syncs its
  directory through `pkg/internal/dirsync`, stays in `pkg/keyprovider`, so `internal/keyfile`
  imports no module-internal package and needs no allowlist entry. `pkg/keyprovider` keeps its
  exported functions and error values, now delegating, and the unexported names its tests call, as
  thin wrappers. Prior art: Go's own `internal/` packages shared by two public ones (checked).

- **D15, the `storage` key is refused at construction.** `Deps` carries the `storage` key's ID, as
  the key file records it, and its fingerprint; `New` compares both with the loaded master key's
  and fails on either match, naming the master key ID (R38). A fingerprint is one-way, so the
  provider still holds no `storage` key. Prior art: none found.

## 5. Target tree

```text
internal/keyfile/
  keyfile.go            T6.1  Generate, Open, the Argon2 bounds, ErrCorrupt, ErrUnwrap, ErrPassphrase
  keyfile_test.go       T6.1  round trip, corrupt and out-of-bounds files, wrong passphrase
pkg/keyprovider/
  local.go, keyfile.go  T6.1  delegate to internal/keyfile; CreateKeyFile stays
pkg/metadata/kv/keys/
  keys.go, format.go    T6.2  KindMKW 0x4D, its name
  keys_test.go          T6.2  MKW in the table's list
pkg/metadata/kvstore/
  keyrecords.go         T6.2  KeyRecords: Load, the guarded Update, RaiseWrapCount; the MKW codec
  keyrecords_test.go    T6.2  over Badger in memory and under t.TempDir(), and FoundationDB where it runs
pkg/transform/keys/
  keys.go               T6.7  ErrNotEscrowed
pkg/transform/keys/provider/
  doc.go                T6.3  package doc
  wrap.go               T6.3  the key wrap AAD, the stored wrap's encode and parse
  keyfile.go            T6.3  the key-file custody, its count, custody (unexported)
  provider.go           T6.3  Row, KeyRecords, Config, Escrow, Deps, Provider, New, the storage-key refusal
                        T6.5  Current, Get, the cache, Calls
                        T6.6  Status
  salvage.go            T6.4  WrapSalvageKey
  record.go             T6.5  the key record and its codec
  escrow.go             T6.7  VerifyEscrow, the escrow gate
  create.go             T6.8  CreateKeys
  custody_test.go       T6.3  runCustody, the custody suite, run on the key file
  wrap_test.go          T6.3  vectors, the AAD rows, FuzzStoredWrap
  count_test.go         T6.3  the wrap count at 2³² − 1, across two namespaces
  salvage_test.go       T6.4  the salvage row and its unwrap
  record_test.go        T6.5  FuzzKeyRecord, the codec's refusals
  provider_test.go      T6.5  the read path's rows, TestAllocsProviderGet
  keystest_test.go      T6.6  the Harness adapter and keystest.Run; Status; the planted key
  escrow_test.go        T6.7  the escrow rows
  create_test.go        T6.8  the key table, concurrency, the crash seam, the chunk-ID key row
  testdata/fuzz/        T6.3, T6.5  crashers, when found
```

## 6. Subtasks

### T6.1 The key-file format in `internal/keyfile`

**Subtask T6.1 estimate: 1 man-days.**

- **Track:** C. **Depends on:** none.
- **Covers:** R2.
- **Deliverables:**
  - `internal/keyfile` (D14), consumed by T6.3 and T6.7, which open the master key and an escrow
    copy, and by `pkg/keyprovider`:

    ```go
    func Generate(passphrase string, time, memoryKiB uint32) ([]byte, error)
    func Open(raw []byte, passphrase string) (id string, key []byte, err error)

    const MinTime, MinMemoryKiB = 1, 8 * 1024
    var ErrCorrupt, ErrUnwrap, ErrPassphrase error
    ```

  - `pkg/keyprovider`'s `GenerateKeyFile`, `GenerateWeakKeyFile`, its key-file read and its error
    values delegating to it; `CreateKeyFile` unchanged.
- **Acceptance criteria:**
  - `pkg/keyprovider`'s tests pass unchanged, `kmip_integration_test.go` included.
  - `internal/keyfile` imports only the standard library, `golang.org/x/crypto/argon2` and
    `github.com/google/uuid`.
  - A file `pkg/keyprovider` wrote before the move opens through `Open`, and one `Generate` writes
    opens through `pkg/keyprovider`.
- **Tests:** `Generate` then `Open` returns the ID and key (catches a format split between the two
  packages); the wrong passphrase is `ErrUnwrap`, a truncated PEM, a bad version and an out-of-range
  Argon2 parameter each `ErrCorrupt` (catch the bounds lost in the move); an empty passphrase is
  `ErrPassphrase` before any work. `pkg/keyprovider`'s unchanged tests catch a changed surface.
- **Rules:** rule 7: no module-internal import, so E1's import test needs no new entry.

### T6.2 The wrap count and `kvstore`'s view

**Subtask T6.2 estimate: 3 man-days.**

- **Track:** C. **Depends on:** none.
- **Covers:** R13, R15, R16, R22 (the conditional raise), R28 (the conflict).
- **Deliverables:**
  - `pkg/metadata/kv/keys`: `KindMKW byte = 0x4D`, its name in `format.go`, and the kind in
    `keys_test.go`'s list, as RFC 16 §4.2.1's table gives it. Consumed by the view below.
  - `pkg/metadata/kvstore`'s view (D1), consumed by the provider from T6.3, by E8's writers and by
    the content wiring:

    ```go
    func (s *Store) KeyRecords() *KeyRecords
    func (r *KeyRecords) Load(ctx context.Context, ns entity.NamespaceID) ([]struct{ Kind, ID string; Record []byte }, error)
    func (r *KeyRecords) Update(ctx context.Context, ns entity.NamespaceID, fn func(rows []struct{ Kind, ID string; Record []byte }) ([]struct{ Kind, ID string; Record []byte }, error)) error
    func (r *KeyRecords) RaiseWrapCount(ctx context.Context, masterKeyID []byte, n, limit uint64) (bool, error)
    ```

    Rows live at `NS‖ns‖key‖kind‖keyID`, the kind and ID as byte strings (RFC 16 §4.2.1), the
    record as given. `MKW‖masterKeyID`'s value is a version byte and the count as 8 bytes.
- **Acceptance criteria:**
  - `Update` guards the namespace's key range before its scan, writes the rows `fn` returns and
    deletes nothing.
  - `RaiseWrapCount` adds `n` only while the sum stays at most `limit`, by a tracked read and a
    write in one transaction.
  - An unknown `MKW` version is refused.
- **Tests** (`kvstore`'s own, in plain types, over Badger in memory and under `t.TempDir()`, and
  FoundationDB where `fdbkvtest` runs):
  - Two `Update`s of one namespace started together, each writing a row only when it sees none:
    exactly one row results (catches a missing guard, a scan relied on, R28).
  - Two namespaces' `Update`s run concurrently without conflict, and `Load` of one never returns the
    other's rows (catches a guard or prefix shared across namespaces).
  - A row written replaces the one of its kind and ID; one not returned stays (catches a delete,
    R16).
  - `RaiseWrapCount` by 1 with limit 3 from 8 goroutines: exactly three report a raise; by 2 from a
    count of 2 under limit 3: refused, and the count stays 2 (catch a raise lost to a race, an
    unconditional write, or a partial raise, R22).
  - Under `t.TempDir()`, close and reopen: the rows and the count survive.
- **Rules:** rule 7: `kvstore` imports nothing of `pkg/transform`; its import test's list is not
  widened. rule 8: the real backends. rule 9: D1's `decision:` at `Update`.

### T6.3 The stored wrap, the key-file custody and its count

**Subtask T6.3 estimate: 3 man-days.**

- **Track:** C. **Depends on:** T6.1, T6.2.
- **Covers:** R1, R19, R20, R21, R22, R23, R38, R41, R45 (key wrap AAD and stored wrap), R48 (the
  stored wrap).
- **Deliverables:**
  - The package and its constructor, consumed by the content wiring (the RFC 8 offload plan's epic
    21) and by every later subtask:

    ```go
    type Row = struct {
    	Kind, ID string
    	Record   []byte
    }

    type KeyRecords interface { /* D1 */ }

    type Config struct {
    	KeyFile    string   // the master key's file
    	Passphrase string   // RFC 13 Appendix C's
    	Escrowed   []Escrow // passes recorded in the store configuration (D6)
    }

    // Escrow is a passed escrow check: what VerifyEscrow (T6.7) returns and the store
    // configuration records.
    type Escrow struct {
    	MasterKeyID string   // as the key file records it
    	Fingerprint [32]byte
    }

    type Deps struct {
    	Records               KeyRecords
    	StorageKeyID          string   // as the storage key's file records it (D15)
    	StorageKeyFingerprint [32]byte
    	Logger                *slog.Logger
    	Now                   func() time.Time // injected clock, kind 1
    	Rand                  io.Reader        // keys, nonces and IDs; crypto/rand.Reader in production
    }

    type Provider struct{ /* unexported */ }

    func New(ctx context.Context, cfg Config, deps Deps) (*Provider, error)
    ```

  - `wrap.go`: the key wrap AAD (R19) and the stored wrap's encode and parse (R20), unexported.
  - `keyfile.go`: the key-file custody behind D3's `custody`: `wrap` raises the count (D5), draws a
    12-byte nonce from `Deps.Rand` and seals; `unwrap` checks the master key ID and opens.
  - `New`: reads the key file through `internal/keyfile`, refuses a non-UUID ID, the `storage` key
    (D15), and a fingerprint that a recorded pass with the same ID contradicts (D6); fails if the
    file cannot be read or unwrapped (R41).
  - `custody_test.go`'s `runCustody`, consumed by E7.
  - `FuzzStoredWrap`, joined to the nightly `fuzz` suite and named in RFC 5 §8.3's suites line.
- **Acceptance criteria:**
  - The custody suite passes on the key file: a wrap unwraps under its AAD; a changed namespace,
    kind or key ID in the AAD fails; a flipped byte of the wrap fails; the master key ID reported is
    the one in the stored wrap.
  - At a seeded count of 2³² − 1, one wrap succeeds and the next is refused naming the master key,
    the count reached through `RaiseWrapCount(…, 1<<32-1, 1<<32)` (D5), and the wraps made with the
    AADs of two namespaces (R22).
  - `New` fails for a missing file, a wrong passphrase, the `storage` key's ID, the `storage` key's
    bytes under another ID, and a recorded pass whose fingerprint differs.
- **Tests:**
  - E2's key wrap AAD and stored-wrap vectors: the AAD from the vector's fields, and the stored wrap
    from the vector's master key, nonce (served by `Deps.Rand`) and key, byte for byte; and the
    vector's stored wrap unwraps to its key (catch a field order, a length width or an endianness
    apart from RFC 5's, R19, R20, R45).
  - The custody suite's AAD rows (catch a wrap that binds nothing, R21).
  - The count row above, through `kvstore` over `badgerkv` in memory, and a wrap whose raise is
    refused draws no nonce from `Deps.Rand` (catch a bound off by one, a count per namespace, or
    bytes made before the count, R22).
  - The `storage`-key refusals, by ID and by bytes under a new ID (catch a check of the ID alone,
    R38).
  - `FuzzStoredWrap`'s seeds: the vector, a zero-length ID, lengths past the end (catch a parse that
    trusts a length, R48).
- **Rules:** rule 9: D5's `decision:` at the raise. rule 11: `Row`, `KeyRecords`, `Config`, `Deps`,
  `Provider`, `Escrow` and `New` for the content wiring, `kvstore` and E9.

### T6.4 The salvage-key wrap

**Subtask T6.4 estimate: 1 man-days.**

- **Track:** C. **Depends on:** T6.3.
- **Covers:** R22 (the salvage wrap counts), R24, R39, R40.
- **Deliverables:** `(*Provider).WrapSalvageKey(ctx context.Context, export []byte) (key, wrap
  []byte, err error)`, E1's `keys.SalvageWrapper`, consumed by the RFC 8 offload plan's epic 20:
  a 32-byte key from `Deps.Rand`, wrapped by the key-file custody under D12's AAD, in the stored-wrap
  layout, no key record written (D13).
- **Acceptance criteria:**
  - Two calls return different keys and different wraps; the count rises by one per call.
  - No namespace's records change.
- **Tests:** the wrap unwraps under the configuration's master key and the export's AAD, and fails
  under another export's bytes, under a namespace key's AAD, and under another master key (catch a
  salvage wrap that binds nothing, R40); the count before and after (catches an uncounted wrap,
  R22); `Load` of a namespace before and after (catches a salvage key written as material, R39). E2's
  salvage row runs with the rest of `keystest.Run` in T6.6.
- **Rules:** rule 9: D13's `decision:`.

### T6.5 Key records, `Current` and `Get`

**Subtask T6.5 estimate: 3.5 man-days.**

- **Track:** C. **Depends on:** T6.3.
- **Covers:** R3, R4, R5, R6, R7, R10 (the start time recorded), R13, R14, R15, R18, R21 (copied
  record), R42, R44, R45 (fingerprint), R47, R48 (the key record).
- **Deliverables:**
  - `record.go`: the key record, version 1: kind, ID, fingerprint, state, the time it entered the
    state (D9), master key ID, wrapped or clear bytes, each length-prefixed; a `destroyed` record
    keeps ID, fingerprint and state. Encode and decode, unexported. Consumed by T6.6, T6.8 and E8.
  - `(*Provider).Current` and `(*Provider).Get`, `keys.Provider`, consumed by E3's chain, E5's
    transform and the engine: the namespace's records loaded once and cached; `Get` unwraps through
    the custody named by the stored wrap, compares the fingerprint, caches the key (D10), and maps
    every failure per R6 and D11.
  - `FuzzKeyRecord`, joined to the nightly `fuzz` suite and named in RFC 5 §8.3's suites line.
  - `TestAllocsProviderGet`, in `transform-allocs` by name.
- **Acceptance criteria:**
  - `Current` returns the `current` record of the kind, with its fingerprint; a kind with none is
    `ErrUnknownMaterial`.
  - `Get` returns the key for a matching record; `ErrUnknownMaterial` for an ID absent from the
    namespace; `ErrMaterialDestroyed` for a `destroyed` record; `ErrMaterialUnavailable` for a
    fingerprint mismatch, a record whose two master key IDs differ, a failed unwrap and an unknown
    codec version.
  - A cached `Get` allocates nothing.
- **Tests:**
  - Each `Get` answer above from records the test writes through the view (catch errors folded
    into one, a destroyed key reported as unavailable, R6, R42).
  - One namespace's wrapped data key copied, as a row, into another namespace's records: `Get` there
    is `ErrMaterialUnavailable` and returns no bytes (catches an AAD checked at wrap time only, R21).
  - A chunking key's record rewritten with another key's wrap under the same ID and the old
    fingerprint, on a fresh provider: `ErrMaterialUnavailable` (catches a fingerprint read from the
    unwrapped bytes rather than the record, R5, R47).
  - The fingerprint of E2's vector material equals the vector (catches a label or HMAC slip, R4,
    R45).
  - A record's two master key IDs made to differ: refused (catches a lookup that trusts the copy,
    R14).
  - The bytes of every record written hold no master key, raw, in hex or in base64 (catches a
    master key persisted, R18).
  - `FuzzKeyRecord`'s seeds: every state, a clear record, a `destroyed` one, an unknown version,
    lengths past the end (catch a decoder that panics or trusts a length, R15, R48).
  - `TestAllocsProviderGet`, skipping under `-race` (catches a copy or a map key built per call,
    R44).
- **Rules:** rule 9: D10's `ponytail:` at the cache, D11's `decision:` at the mapping.

### T6.6 `Status`, the provider suite and the planted key

**Subtask T6.6 estimate: 2 man-days.**

- **Track:** C. **Depends on:** T6.4, T6.5.
- **Covers:** R3, R5, R7, R8, R9, R10, R11, R12, R43, R49.
- **Deliverables:**
  - `(*Provider).Calls() uint64`, consumed by `keystest.Harness` and E3's "no call" rows run on the
    provider.
  - `(*Provider).Status() Status`, consumed by the content wiring (D8):

    ```go
    type Status struct {
    	Unavailable  []keys.MaterialID // material_unavailable; encryption_keys_unavailable is its length
    	Unknown      []keys.MaterialID // material_unknown
    	CurrentSince map[entity.NamespaceID]time.Time // encryption_current_key_age_seconds
    }
    ```

    An ID leaves `Unavailable` or `Unknown` at its next successful `Get`.
  - `keystest_test.go`: the `Harness` adapter, in package `provider`. `Give` wraps the material
    under the key file and writes its record through `Update`, retiring the kind's previous current
    record; `Replace` rewrites the record with the new material's wrap and the old fingerprint, then
    opens a fresh provider over the same store, since the cache holds what was unwrapped (D10);
    `Destroy` marks the record `destroyed`; `Calls` sums across the providers it opened.
- **Acceptance criteria:**
  - `keystest.Run` passes on the provider under `-race`, its salvage row included (R12).
  - After a failed `Get` of an ID never given, `Status().Unknown` names it; once it is given and read,
    it is gone (R8).
- **Tests:**
  - `keystest.Run` (catches every row E2 holds the in-memory provider to, R3, R5, R7, R11, R12).
  - A replaced key: `Get` refused, `Status().Unavailable` holds it, so the gauge is 1, and a body
    encoded through E3's chain with E5's transform over this provider fails rather than writing
    (catch a refusal that is not reported, or a key served after it, R5, R9).
  - `CurrentSince` equals the injected clock's time at the record's write (catches an age measured
    from process start, R10).
  - 64 goroutines of `Current` and `Get` over two namespaces under `-race` (catches an unguarded
    cache, R49).
  - The planted key: a known data key, chunking key, master key and passphrase, then the captured
    log and every error and `Status` value of the rows above searched for their bytes, raw, in hex
    and in base64 (catches key material formatted into a message, R43).
- **Rules:** rule 1: the adapter is test code over the real provider, not a double. rule 3: the
  planted key.

### T6.7 The escrow check

**Subtask T6.7 estimate: 1.5 man-days.**

- **Track:** C. **Depends on:** T6.3.
- **Covers:** R22 (the test record counts), R34, R35 (the gate).
- **Deliverables:**
  - `ErrNotEscrowed` in `pkg/transform/keys`, as RFC 5 §3.1 declares it, consumed by T6.8 and E9.
  - `escrow.go`, consumed by E9's `verify-escrow` action and T6.8:

    ```go
    func (p *Provider) VerifyEscrow(ctx context.Context, copy []byte) (Escrow, error)
    ```

    It opens the copy with the configured passphrase, wraps a fresh test key under the master key
    with D12's AAD (raising the count), unwraps it with the copy's key, compares the copy's ID and
    fingerprint with the master key's, and on a pass returns them and remembers the pass (D6).
  - The gate, unexported: a key-file master key is escrowed when a pass for its ID and fingerprint
    is in `Config.Escrowed` or was made by this provider.
- **Acceptance criteria:**
  - A copy of the master key file passes and returns the key's ID and fingerprint.
  - A copy of another key file, a copy under the same ID holding other bytes, and an unreadable copy
    each fail with `ErrNotEscrowed`, naming the key, and leave the gate shut.
- **Tests:** each row above (catch a check of the ID alone, a check that never unwraps, R34); the
  count rises by one per check (catches an uncounted test record, R22); a pass made, then a fresh
  provider with that `Escrow` in `Config.Escrowed` has the gate open, and one without it shut
  (catches a pass kept only in memory, R35, D6).
- **Rules:** rule 9: D6's `decision:`.

### T6.8 Namespace key creation

**Subtask T6.8 estimate: 3 man-days.**

- **Track:** C. **Depends on:** T6.5, T6.7.
- **Covers:** R24, R26, R27, R28, R29, R31, R32, R35, R37, R46, R49.
- **Deliverables:** `(*Provider).CreateKeys(ctx context.Context, ns entity.NamespaceID, encrypts
  bool) error`, consumed by E9's namespace creation and E8: Appendix B.2's table (D7): a chunk-ID key,
  wrapped when `encrypts` and in the clear otherwise; an export key of generation 1; when
  `encrypts`, a data key and a chunking key; each 32 bytes from `Deps.Rand`, each ID minted (D4).
  `ErrNotEscrowed` before any wrap when `encrypts` and the gate is shut.
- **Acceptance criteria:**
  - An encrypting namespace has four `current` records, each served by `Current` and `Get`; a
    non-encrypting one has two, its chunk-ID key in the clear with no master key ID, and `Current`
    of `chunking-key` is `ErrUnknownMaterial`.
  - A second `CreateKeys` of a namespace changes nothing and makes no record.
  - With the gate shut, creating an encrypting namespace is `ErrNotEscrowed` naming the key, and
    nothing is written or counted; a non-encrypting one succeeds.
- **Tests:**
  - The two key sets above, read back through `Current` and `Get` (catch a missing or extra kind, a
    wrapped clear key or a clear wrapped one, R26, R27).
  - The export key's ID: its first 8 bytes are 1, big-endian, and every ID is at most 255 bytes
    (catch a generation laid out otherwise, an ID too long for the census, R29, R31).
  - Eight `CreateKeys` of one namespace at once under `-race`: one set of records, and every caller
    then reads the same IDs (catches creation outside the lock, R28, R49).
  - The crash seam: `kvtest.FaultKV` beneath `kvstore` tears the `Update`'s commit; `Current` on a
    fresh provider over the reopened store is `ErrUnknownMaterial`, the count shows the wraps made,
    and the next `CreateKeys` makes a full set; torn after the commit, the next `CreateKeys` keeps the
    committed set (catches a key served without its record, or a set replaced on retry, R32).
  - The escrow gate: refused, then `VerifyEscrow`, then accepted; and a non-encrypting namespace
    with the gate shut (catch a gate on the wrong kind of namespace, R35, R37).
  - RFC 13's "§7 chunk-ID key" row: two non-encrypting namespaces; BLAKE3's keyed mode over one
    file's bytes under each chunk-ID key gives two IDs, both unlike the unkeyed hash (catches a fixed
    or shared chunk-ID key, R46).
- **Rules:** rule 8: the crash seam beneath `kvstore`. rule 9: RFC 5's `decision:` note on the clear
  chunk-ID key at the branch that writes it.

## 7. Order of work

```mermaid
flowchart TD
  S2(["S2 suites and vectors in"]):::sync --> T6_3
  T6_1["T6.1 internal/keyfile"] --> T6_3["T6.3 Stored wrap, key-file custody, count"]
  T6_2["T6.2 Key lock, wrap count, kvstore's view"] --> T6_3
  T6_3 --> C1(["after T6.3: E7 can build its custody"]):::sync
  T6_3 --> T6_4["T6.4 Salvage-key wrap"]
  T6_4 --> C2(["after T6.4: the RFC 8 offload plan's epic 20 can seal salvage exports"]):::sync
  T6_3 --> T6_5["T6.5 Key records, Current, Get"]
  T6_4 & T6_5 --> T6_6["T6.6 Status, keystest, planted key"]
  T6_3 --> T6_7["T6.7 Escrow check"]
  T6_5 & T6_7 --> T6_8["T6.8 Namespace key creation"]
  T6_6 & T6_8 --> C3(["after T6.8: E8 and E9 can start on the provider"]):::sync
  classDef sync stroke:#2c5aa0,stroke-width:3px,stroke-dasharray:4 2
```

T6.1 and T6.2 run in parallel. After T6.3, T6.4, T6.5 and T6.7 can run in parallel; T6.6 and T6.8
close the epic.

## 8. Suggested pull requests

| PR | Subtasks | Man-days | After | Why together |
| --- | --- | --- | --- | --- |
| 1 | T6.1 | 1 | — | the moved format alone, a refactor `pkg/keyprovider`'s tests check |
| 2 | T6.2 | 3 | — | the metadata store's half: the two key kinds and the view, tested on its backends; open beside PR 1 |
| 3 | T6.3, T6.4, T6.7 | 5.5 | PR 1, PR 2 | the provider's constructor and everything that wraps: the custody and its count, the salvage key and the escrow check, which `Config` and `New` name |
| 4 | T6.5, T6.6 | 5.5 | PR 3 | the read path and the suite that holds it: records, `Current`, `Get`, `Status`, `keystest.Run` |
| 5 | T6.8 | 3 | PR 4 | key creation, the provider's last writer, with its crash seam |

A suggestion only: one pull request per subtask works as well.

## 9. Open points

- **Which master key wraps a salvage key, for RFC 5's author.** Appendix B.2 says "the
  installation's current master key", but a master key belongs to a store configuration (B.2's
  table, RFC 13 §4.2) and an installation may have several. The recommendation, which D13 builds:
  the current master key of the store configuration of the stranded tag's share, so the salvage key
  is held off the host exactly where that share's data keys are; when the node cannot establish the
  share's configuration at all, no provider exists to wrap under, which RFC 1 §3 and the RFC 8 offload
  plan's epic 20 decide. The wording to propose: "wrapped under the current master key of its share's
  store configuration". E6's code does not change with the answer: only which provider the content
  wiring hands the engine does.
- **Where a passed escrow check is kept (D6).** RFC 16 has no row for it and RFC 13 §4.2 lists it
  among the store configuration's values without naming a setting. E9 needs a setting, per store
  configuration, listing each passed master key's ID and fingerprint, written by `verify-escrow` and
  read into `Config.Escrowed`.
- **RFC 16 §4.2's key-record row lacks the state's start time (D9).** The row should read "and
  state, with the time it entered it". Proposed for the next RFC 16 revision; nothing waits on it.
- **RFC 5 §8.1's §2.5 row says "availability gauge 0".** Appendix B.6 defines no availability
  gauge: `encryption_keys_unavailable` counts unavailable keys, so a refused key makes it 1. T6.6
  asserts B.6's count; the row's wording is for RFC 5's author.
- **The `storage` key's fingerprint.** The content wiring must pass it (D15). For a key-file
  `storage` key it can read it through `internal/keyfile.Open`; `pkg/keyprovider`'s surface, which
  does not change, offers no bytes for a KMIP `storage` key. For the RFC 8 offload plan's epic 21 and
  RFC 13's plan.
- **The RFC 8 offload plan's epic 21 runs no key-record suite.** Its plan says the content wiring's
  tests run "RFC 5's key-record suite" against `kvstore`; with D2, `kvstore`'s own tests hold the
  view over every backend, and nothing is left for epic 21 but passing `kvstore`'s view to `New`.
- **Departures from RFC 5's plan's layout.** No key-record suite or in-memory view in `keystest` (D2);
  the custody suite in the provider's own tests (D3); no recorder for the gauges (D8). Each needs
  nothing of another epic but E7's call of `runCustody` and E9's gauges over `Status`.
- **Needs on E8.** The `keyseq` subtag (`0x07` under `NS‖ns`) and the key lock land with E8's
  first writer of a `keys` object, which takes it in the transaction of its `Update` beside D1's
  range guard; dropping a destroyed key from the cache, and the bucket put before creation's records
  become current (D7), are E8's too.
- **Estimate.** 8 subtasks and 18 man-days, against RFC 5's plan's 8 and 19: the in-memory view and its
  suite are not built (D2). The least certain estimates are T6.5's, where every failure mapping and
  the codec meet, and T6.8's crash seam.
