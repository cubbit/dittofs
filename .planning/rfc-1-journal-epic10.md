# Epic 10: the start check

**Sources:** [RFC 0](../docs/internals/rfcs/rfc-0-data-lifecycle.md) §1.4 and §10; [RFC 28](../docs/internals/rfcs/rfc-28-journal-format.md)
§3.4; [RFC 8](../docs/internals/rfcs/rfc-8-engine.md) §2.5 step 1; [RFC 16](../docs/internals/rfcs/rfc-16-metadata-store.md) §3.2.
**Track:** A.
**Epic 10 estimate: 4 subtasks, 7.5 man-days.**

What this epic owns:

- A start over an absent store creates nothing.
- A refused start says what it found.
- A start over a pre-journal (v1) local layout refuses.

What it does not own:

- The initialisation step is the RFC 23 plan's epic 5, T5.4.
  - It runs the store's `Init` first.
  - So no device ever names an installation without a store.
  - The old "wipe of devices left by an interrupted step" is retired.
- `dfs start` is the RFC 23 plan's epic 3, T3.9.
  - Its R3 refuses a v1 `controlplane.db`, and stays the only check for it.
  - Nothing there refuses a v1 journal layout: R10 covers it here.

## Requirements

| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R1 | A start never creates a metadata store: finding none, it refuses to serve naming the store | MUST NOT | RFC 0 §10, §1.4 | T10.2 |
| R2 | A start never creates a journal: `Open` of a directory with no journal fails and changes nothing | MUST NOT | RFC 28 §3.4; RFC 8 §2.5 | T10.2 |
| R3 | A refused start writes nothing: the store directory and every device are as before | invariant | RFC 28 §3.4; RFC 0 §10 | T10.2, T10.4 |
| R4 | A journal naming an installation with no store beside it is the metadata store lost, never a first start | MUST | RFC 0 §10, §1.4 | T10.1 |
| R5 | A journal naming another installation than the store's is refused, naming the journal identity and installation ID its `format` holds (`*IdentityError` already carries both; the test is what is missing) | MUST | RFC 28 §3.4 | T10.4 |
| R6 | A start over a store never initialised refuses and says so, never "lost" | MUST | RFC 16 §3.2; RFC 0 §1.4 | T10.1 |
| R7 | A directory holding a segment but no `format` file is refused as unidentifiable, never opened as empty | MUST NOT | RFC 28 §3.4 | T10.1 |
| R8 | A node with no journal device is refused before anything opens | MUST | RFC 0 §1.4 | T10.3 |
| R9 | A device whose `format` cannot be read or does not verify is refused naming the error, never read as a journal that is absent | MUST NOT | RFC 28 §3.4; RFC 8 §2.5 | T10.1, T10.2 |
| R10 | A start that finds a pre-journal (v1) local layout under a configured device, beside the store directory or under v1's default root refuses before anything opens, naming the path | MUST | RFC 23 plan §1.4; journal plan, "No upgrade from v1" | T10.1, T10.2 |
| R11 | `kvstore.Init` never writes v2 keys into a key-value store that holds a key outside v2's key kinds and no format record (a v1 Badger store): it refuses, and its transaction writes nothing. The clock ceiling `badgerkv.Open` writes is v2's own key | MUST NOT | RFC 16 §3.2; journal plan, "No upgrade from v1" | T10.2 |

Not this epic's: a store with no journal, and a journal not recorded for this device (the engine's step 1).

## 6. Subtasks

### T10.1 The verdict

**Subtask T10.1 estimate: 1.25 man-days.**

- **Depends on:** none.
- **Covers requirements:** R4, R6, R7, R9, R10.
- **Deliverables:**
  - `decide(facts) error` in `internal/node`, unexported.
  - Input: the pre-journal layouts found, if any. The verdict is "v1 state, naming the path", ahead of every other.
  - Input: the store directory's state: present, absent, or unidentifiable.
  - Input: the device count.
  - Input per device: the result of `journal.Open` with a zero identity, and whether it holds a segment.
    - `ErrNoJournal`: the device holds no journal.
    - `*IdentityError`: the installation its `format` names.
    - Any other error (`ErrCorrupt`, `ErrUnknownFormat`, a platform or permission error):
      the device is unidentifiable, naming the device and the error.
  - Output: v1 state, no device, not initialised, store lost, devices disagree, or unidentifiable.
    - Devices naming different installations are "devices disagree": they cannot all be this store's.
    - An unidentifiable store: named, and `dfs init` is not suggested.
  - Several verdicts at once give the most serious: v1 state, unidentifiable, devices disagree, lost, not initialised.
  - The not-initialised and store-lost messages say to check the mounts: an unmounted device or store volume is an empty directory.
    - RFC 0 §10's remedy for a lost store is a restore from backup, which loses every write since.
- **Acceptance criteria:**
  - Each verdict has its own error.
  - Store lost names the directory and the installation, and says to check the store volume's mount first.
  - Not initialised names neither, and points at the mounts.
  - Only `ErrNoJournal` reads as "holds no journal"; every other error is unidentifiable.
- **Tests:**
  - By table: every row.
  - A mixed device set in two orders.
  - One row per error class above, including a corrupt `format` beside an absent store.
  - A corrupt `format` must never read as "not initialised" (the operator would run `dfs init` over a lost store).
  - Two devices naming different installations give "devices disagree", not one "store lost".
  - The store-lost message names the mounts.

### T10.2 `node.Preflight`

**Subtask T10.2 estimate: 3.25 man-days.**

- **Depends on:** T10.1; the RFC 23 owner's agreement to change the node's export pin (below).
- **Covers requirements:** R1, R2, R3, R9, R10, R11.
- **Deliverables:**
  - `Preflight(ctx, storeDir, devices, clock) error` in `internal/node`.
    - `clock` is the node's clock, of the type `node.Config` already uses: no option type joins the exports.
    - A nil clock is refused first, as a wiring error. It is not a device verdict: `journal.Open`'s `ErrNoClock` would otherwise make every device unidentifiable.
  - Called by `dfs start` before `badgerkv.Open`.
  - Why before: `badgerkv.Open` creates a missing directory and writes a clock ceiling.
  - It builds no component, so it fits T3.9's R2 (`start.go` imports only `internal/node`, `pkg/bootstrap`, `badgerkv`, `pkg/metrics`).
  - `internal/node/arch_test.go`: `Preflight` added to the exports.
    - That test allows only `New`, `Node`, `Config`, `Node.Start` and `Node.Stop`: it is RFC 23 epic 1's R9, a MUST.
    - The change is agreed with the RFC 23 owner (open points).
  - The v1 probe, run first, from what v2 knows. No v1 root is configured: the RFC 13 plan replaces `blockstore.journal.path` with `journals`.
    - Each configured device, the store directory's parent, and the parent's `blocks/`.
      - `blocks/` is v1's default root, `<state dir>/blocks`: v1 sets it when `blockstore.journal.path` is unset.
      - v2's store defaults to `<state dir>/metadata`, so a default install is one level below the parent.
    - Probed for `shares/<name>/` holding a populated `journal`, `blobs` or `logs` directory.
    - The rule is broader than `hasLegacyLocalLayout` on `develop`, which returns false once `journal/` holds a data segment.
    - Found: refuse naming the path.
    - `controlplane.db` stays T3.9's R3.
  - The store probe:
    - Stats the store directory for Badger's `MANIFEST`; present means return nil.
    - Only `fs.ErrNotExist` or an empty directory means absent.
    - Any other directory without a `MANIFEST` (tables, `*.vlog` or `KEYREGISTRY` left by a partial restore) is an unidentifiable store: refuse naming it.
    - Any other stat error refuses, naming the path and the error.
  - Absent: read each device through `journal.Open` with a zero identity, which opens nothing, then apply `decide`.
  - `kvstore.Init` refuses a key-value store that holds a key outside v2's key kinds and no format record (R11).
    - The `MANIFEST` probe cannot tell a v1 Badger store from a v2 one: v1's guides put it at `/var/lib/dittofs/metadata`.
    - Without this, `Init` commits v2 keys beside v1's.
    - `badgerkv.Open` writes the node's clock ceiling before it returns, so a fresh store is never empty: that key is exempt.
    - v1's keys are ASCII prefixes (`f:`, `p:`, `c:`, `lock:`), none starting with a v2 key kind.
    - The refusal is its own error, and `Init`'s transaction writes nothing.
  - A `ponytail:` at the probe:
    - A `MANIFEST` with no format record is left to `kvstore.Open` and `Init` (R11).
    - Upgrade: a read-only open, once Badger tolerates one after an unclean close.
- **Acceptance criteria:**
  - Absent store directory and empty devices: it refuses, and the directory still does not exist.
  - Directory holding a `MANIFEST`: it returns nil without opening it.
  - A directory holding only `*.sst` files is refused as an unidentifiable store, never as not initialised.
  - A device that is an old v1 root refuses naming it, never "not initialised".
  - No verdict creates, changes or removes a file.
- **Tests:**
  - At `Preflight`, over `t.TempDir()` and the real journal `Open`.
  - Every verdict, each followed by a hash of the store directory's parent and of every device.
  - The hash must equal the one taken before.
  - Catches a probe that opens Badger.
  - Catches a journal `Open` that writes.
  - An installed store returns nil.
  - A store directory that cannot be read (permissions) refuses naming the error.
  - A nil clock refuses as a wiring error, never as unidentifiable devices.
  - At `kvstore.Init`, over `badgerkv.Open` on a fresh `t.TempDir()`:
    - a store holding only the clock ceiling initialises;
    - the same with a v1-style key present refuses, and only the ceiling changed.
  - v1 rows:
    - a device that is an old v1 root (`shares/<n>/journal/x.seg`);
    - a store directory's parent holding `shares/<n>/blobs/x`;
    - `<parent>/blocks/shares/<n>/blobs/x`, with an absent store: refuses naming that path, never "not initialised";
    - an empty `shares/<n>/blobs/` does not refuse.

### T10.3 No journal device

**Subtask T10.3 estimate: 1.5 man-days.**

- **Depends on:** T10.2; the RFC 23 plan's T3.9 (`dfs start`) and the RFC 13 plan's epic 1 (`pkg/bootstrap`).
  - `start.go` reads `pkg/config` today, so the call has no bootstrap to take its paths from before those land.
- **Covers requirements:** R8.
- **Deliverables:**
  - Refuse an empty device list in `Preflight`, before the probes.
  - Refuse it in `node.New` too.
  - Add the `Preflight` call to `runStart`, unconditionally, agreed with T3.9's owner.
- **Acceptance criteria:**
  - `Preflight` and `node.New` refuse no device.
  - `dfs start` over a bootstrap with no device exits non-zero and names it.
  - `dfs start` creates no store.
- **Tests:**
  - At `Preflight`.
  - At `dfs start` run as a child process.
  - No device, with and without a store directory present.
  - Catches a refusal that waits for the store probe.

### T10.4 Refusal rows at the node

**Subtask T10.4 estimate: 1.5 man-days.**

- **Depends on:** T10.2; the RFC 23 plan's T5.4 (`install.Run`) for the harness.
  - Until it lands, `nodetest`'s own `kvstore.Init` and `journal.Init` initialise.
- **Covers requirements:** R3, R5.
- **Deliverables:**
  - Rows in `lifecycle_test.go`:
    - a normal start;
    - the refusals;
    - a journal of another installation (no test covers it today);
    - a refused start leaves everything byte-identical.
  - The matching `trace.yaml` rows.
  - Each `known.yaml` entry that turns green is removed.
- **Acceptance criteria:**
  - Each refusal row fails on its assertion when its refusal is removed.
  - The other-installation row asserts the journal identity and installation ID in the error.
- **Tests:**
  - At `Node.Start`, over a harness initialised through the real step.
  - Remove each refusal in turn and watch its row fail on the assertion.

## Order and pull requests

- Order: T10.1, then T10.2, then T10.3 and T10.4.
- Suggested PR 1: T10.1 to T10.3 (6 md).
- Suggested PR 2: T10.4 (1.5 md).
- One PR per subtask works too.

## Open points

- The RFC 23 owner agrees two changes to `internal/node/arch_test.go` (RFC 23 epic 1's R9):
  - `Preflight` joins the exports (this epic).
  - `Tracer` joins `configFields` (epic 13's T13.3).
  - The same pin is worded in RFC 23's own documents, which change with it, or its T1.3 fails T10.2 and T13.3:
    - `rfc-23-control-plane-epic1.md` (R9, "nothing else", and T1.3's "exactly" test);
    - RFC 23 §2.1's `decision:`;
    - the `Config` list in `rfc-23-control-plane-plan.md`.
- RFC 23's T3.9 owner confirms the `Preflight` call.
  - T3.9's own v1-database test could also assert R1.
- T10.2 confirms the `MANIFEST` name against the pinned Badger version.
- A first `Init` killed before Badger writes its `MANIFEST` leaves an unidentifiable store: T5.4's re-run is the way out, and its owner confirms it.
