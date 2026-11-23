# RFC 2 — the carver: implementation plan

**Scope:** all of [RFC 2, the carver](../docs/internals/rfcs/rfc-2-carver.md): the chunker,
the carver, chunk and block identity, and the block assembler. Its status is `draft`, not
`deferred`, and the whole RFC is planned. Related RFCs, all **constraints only**:
- [RFC 0](../docs/internals/rfcs/rfc-0-data-lifecycle.md) (§1.2, §2.1, §2.2, §5.2) and
  [RFC 1](../docs/internals/rfcs/rfc-1-journal.md) (§3.2, §3.3: holes and offers), which shape
  the carver's inputs; [RFC 28](../docs/internals/rfcs/rfc-28-journal-format.md) is not touched.
- [RFC 3](../docs/internals/rfcs/rfc-3-syncer.md), [RFC 4](../docs/internals/rfcs/rfc-4-remote-tier.md)
  and [RFC 5](../docs/internals/rfcs/rfc-5-transforms.md), the other three components of the
  offload programme, each with its own plan: the seams this plan assumes with them are in §3.2
  and §4.2.
- [RFC 8](../docs/internals/rfcs/rfc-8-engine.md), whose offload part composes the carver and the
  assembler (§2.1, §2.4, §2.6, §6.2 to §6.7, §7.8); that composition is
  [`rfc-8-offload-engine-plan.md`](rfc-8-offload-engine-plan.md)'s, being written alongside this one.
- [RFC 6](../docs/internals/rfcs/rfc-6-block-metadata.md) (§3.5 zero refs, §4.1 one commit per block),
  [RFC 9](../docs/internals/rfcs/rfc-9-gc.md) (relocation mints a new name),
  [RFC 12](../docs/internals/rfcs/rfc-12-snapshots.md), [RFC 13](../docs/internals/rfcs/rfc-13-configuration.md)
  (Appendix B: `Target`, the block target, the keys), [RFC 16](../docs/internals/rfcs/rfc-16-metadata-store.md)
  (§4.2.1, the namespace ID's raw bytes), [RFC 26](../docs/internals/rfcs/rfc-26-catalog-backups.md),
  [RFC 27](../docs/internals/rfcs/rfc-27-namespace-migration.md) (§2.4, §2.7: imports and re-homes).
- [RFC 25](../docs/internals/rfcs/rfc-25-observability.md) and [RFC 29](../docs/internals/rfcs/rfc-29-test-harness.md):
  the rules every epic follows (§1.5 rules 3 and 6).

**Tracks:** A, B and C (§3.4).
**Programme:** [#5488](https://github.com/cubbit/dittofs/issues/5488), "offload: RFC 2–5 and the
engine's offload part", release v1.1; roadmap epic [#3618](https://github.com/cubbit/dittofs/issues/3618).
It builds on the journal-only engine, #3119's stage 3
([`rfc-8-journal-only-engine-plan.md`](rfc-8-journal-only-engine-plan.md)), and on the journal
([`rfc-1-journal-plan.md`](rfc-1-journal-plan.md)). Composing the carver into the engine and the
node is the RFC 8 offload plan's, so this plan's integration keeps only what is the carver's own,
and what waits on that composition is planned after a synchronisation point named for it (§3.2).
GC (RFC 9) is not in the programme.
**Naming:** **v1** is `pkg/block/carver` and `pkg/block/chunker`, with the rest of the v1 block
tier, deleted by #3119's stage 4 (D8 of [`rfc-16-store-cutover-stage4-plan.md`](rfc-16-store-cutover-stage4-plan.md),
PR #5484). **v2** is the carver RFC 2 describes, built here.
**Transition:** new installations only, as #3119 decided. v1 and v2 never run in one binary, so no
name carries a version, and v2 neither reads nor migrates anything v1 wrote. No epic touches v1
code.
**Inputs settled with the user (Step 0):** the RFC path; v1 and its deletion; the related RFCs as
constraints only; the branch, `develop`; the programme #5488; no dashboards; the output path; the
whole RFC planned. **Defaults used:** none beyond those. Epic issues are not created before this
plan merges; epic 2 needs none (§1.4).

---

## 1. Introduction

### 1.1 What we are building

The carver and the block assembler of RFC 2's "Start here" and §1: pure functions from bytes to
chunk boundaries and IDs, and from chunk streams to block plans, with the block name of §4.2. They
read no disk, call no network and hold no state (RFC 2 §1.1, §1.3), so every epic tests them with a
byte slice and a closure.

| v1 | v2 |
| --- | --- |
| `pkg/block/chunker` | `pkg/block/carver/internal/chunker` (D1, D2) |
| `pkg/block/carver`, block assembly included (RFC 2 Appendix A, last row) | `pkg/block/carver` and `pkg/block/carver/assembler` (D1) |
| the chunk profile per share, from the host file (RFC 13 Appendix A, row C1) | `Target` per namespace, a bound setting (RFC 13 Appendix B), declared by epic 3 |
| `blockstore.journal.chunk_size`, `.chunk_max` | dropped (RFC 13 Appendix C's dropped keys) |
| on-disk content cut by v1 | none: new installations only |

**Dependency limits.** RFC 2 §1.1 allows the standard library and a BLAKE3 implementation, and
an import test enforces it from epic 1. The repository already depends on
`lukechampine.com/blake3`, which has the key-derivation and keyed modes and the extendable output
RFC 2 §3.2 and §4.2 use; epic 2 checks each against an independent vector.

### 1.2 How the plan is organised

Epics 1 and 2 are the team's and run first: the frozen surface, and the test components every
later epic checks against (a reference chunker written straight from RFC 2 §3.2, golden vectors
computed by an independent implementation, seeded inputs and the corpus). Three middle epics
follow RFC 2's seams, one per track: the chunker (§3, §6), the carver with chunk and block
identity (§2, §4, §7), and the block assembler (§5). Epics 6 and 7 integrate, both planned after
points the RFC 8 offload plan delivers (S3, S4). Epic 8 runs what no per-change suite runs: the
benchmarks, fuzzing past the seed corpus, the second architecture.

### 1.3 What epic 1 must deliver

The exported surface of D1's packages, frozen at S1, with stub bodies:

- `carver`: the settings value built from a namespace's `Target`, its encryption flag, its
  chunking key and its chunk-ID key (RFC 2 §3.2, §3.7, §4.1, §6; D4); `Cut` (§1.2, §2.1, §2.2,
  §2.4); `Chunk` (§1.2); the chunk-ID function RFC 4's codec verifies reads with (§4.1, RFC 4
  §3.4); the block name (§4.2), which the RFC 8 pipeline and RFC 4's whole-block read call (D9);
  the error set of §1.2, §3.7, §7 and §10.2.
- `assembler`: the concrete fold of §5.1 and its constructor, which refuses a block target out
  of §5.2's range (D2, D3).
- RFC 2 §1.1's import test over every package of the tree, test packages included in its
  allowlist (rule 7), following `pkg/block/foreign_imports_test.go`'s `go/build` walk.

The chunker's boundary search stays internal (D2): no consumer outside the carver calls it.
Nothing in epic 1 declares an interface: the engine declares its own over these concrete types
(RFC 8 §2.1).

### 1.4 What epic 2 must deliver

**No red suite, and no epic issue needed for it.** RFC 2's suites paragraph (§10.6) puts every
check in `unit`, and the allocation counts in `unit/nonrace`. Neither keeps a `known.yaml`, so no
test of this plan lands red: each lands green with the epic that makes it pass, and epic 2
registers no suite. §1.5's instruction says epic issues exist before epic 2's pull request
because red tests name them; here no test is red, so that clause has nothing to apply to.

**The scenario catalogue, by source.** Each check lands in the epic named:
- RFC 2 §9's rows: the chunker rows in epic 3; the carver and name rows in epic 4; the consumer
  rows in the plans of their consumers (§6.1).
- RFC 2 §5.7's check table, and A1 to A7: epic 5.
- RFC 2 §10.1's golden, property, differential and fault kinds, §10.2's edge cases and §10.3's
  fault sweep: epic 3 for the chunker's, epic 4 for the carver's, sized for `unit` as D8 says.
- RFC 2 §10.4's per-change counts: epics 3 and 4, as §6 point 3's amendment settles them; §9.1's
  K1 to K5 and §10.5's targets: epic 8.
- RFC 2 "Start here"'s five scenarios: each is one of the rows above, run as written.
- From v1's history, as regression checks the rows above already make: RFC 2 Appendix A.1 (masks
  that ignore `Target`), A.2 (warm-up over the whole chunk), and the "not yet" answer of
  Appendix A's sixth row.

**The test components**, in `pkg/block/carver/internal/carvetest`:
- **The reference chunker**: RFC 2 §3.2's derivation written literally, one byte at a time, by an
  engineer other than epic 3's. It is the independent implementation §9's derivation row and
  §10.1's differential kind ask for, and computes the golden boundaries (D6).
- **The golden vectors** of D6, and the check of the BLAKE3 library's three modes against them.
- **Seeded inputs** of RFC 2 §10.2's content kinds.
- **The corpus** for K4, K5 and §10.4's edit stability (D7).

The awkward readers of RFC 2 §10.2 are `testing/iotest`'s and an `io.MultiReader` over an error
reader; the one `iotest` lacks, zero bytes with no error, is a few lines in epic 4's tests.

### 1.5 General rules for each epic

> **Instruction for whoever expands an epic into its detailed description and subtasks,
> human or AI:**
>
> - **Creating an epic from this plan: use the `epic-breakdown` skill.** Whenever the user
>   asks to create an epic from this plan, or to expand, detail or break one down, run the
>   `epic-breakdown` skill (`.claude/skills/epic-breakdown/SKILL.md`) on this plan and that
>   epic, rather than writing the epic document freehand. It extracts the RFC's MUST and
>   MUST NOT into numbered requirements, estimates each subtask in man-days and suggests
>   the pull requests. A human doing it by hand follows the same skill.
> - **Epic and subtask, nothing else.** The units of work are the epic and its subtasks,
>   with subtask ids `<e>.<n>` (`subtask 4.3`, or `T4.3` for short), assigned by the epic
>   document. No other word names
>   them: not task, ticket, story, step or phase, in headings, tables, ids or prose.
> - **The epic stands alone for what to build.** Its requirements, subtasks and decisions
>   are in the epic document itself, which never refers to this plan for them or says "see
>   the plan": the RFC sections it implements, the interfaces it builds on, the
>   subtasks, and a short legend of the names it borrows from this plan (E1, E2, S1, the
>   integration epic, …). It cites the numbered rules below, without copying them, in a
>   table of how each applies: each row names its rule by number and bold title, which
>   says what the rule requires, and the table links this §1.5 with a full link (the
>   plan's path and the section's anchor) and the `CLAUDE.md` sections that hold a rule,
>   so the epic reads alone and its reader can still open each rule's text in one click.
> - **The epic document records decisions, not progress, like this plan.** It has no
>   status fields, no checkboxes to tick and no sections to keep up to date as the work
>   goes on. It is revised through a pull request only when a decision changes.
> - **Subtasks are not pull requests.** A subtask is ideally up to 5 man-days. The owner
>   ships subtasks as they see fit, one pull request each or several together; the epic
>   document only suggests a grouping, each suggested pull request ideally up to 15
>   man-days. Cut and group by logic, readability, dependencies and a deliverable that can
>   be named, not to hit a number.
> - **Every subtask states how it meets each rule that applies to it.** A rule that does
>   not apply is named, with the reason.
> - **In the issue tracker, once each document merges,** following `epic-breakdown`
>   Step 6, their one description. Issues point at the documents and copy nothing; once
>   created, their `Estimate` and blocked-by links are the current estimate and
>   dependencies, and the documents are not revised to follow them. The
>   epic issues exist before E2's pull request lands, because each red test's
>   `known.yaml` entry names one; subtask issues come once their epic document has
>   merged. The release an epic ships in is set by the RFC's roadmap epic that its epic
>   issue sits under, never in a document and never on the epic issue on its own: the epic
>   issue and its subtask issues copy that roadmap epic's milestone, and an epic changes
>   release only by moving under the roadmap epic of the other release.

1. **Independent unit tests.** Each epic carries its own unit tests, over epic 2's components
   and the packages of the epics it depends on once landed. The carver needs no test double at
   all (RFC 2 §1.3); the assembler's lookup is a function over a map (RFC 2 §5.7). E1 declares no
   internal interface for a test to stand in for (rule 12).
2. **Fuzz tests where relevant** (`CLAUDE.md`, "Writing tests", "By kind"). The carver parses
   nothing off a disk or a wire, so its fuzz targets are RFC 2 §10.1's properties: random bytes and
   random valid settings into the chunker and `Cut`, asserting §8's C2 to C4 and C7 and agreement
   with the reference chunker; random settings into the settings constructor, asserting §3.7's
   refusals; random chunk streams into the assembler, asserting §5.7's A1, A2 and A4 to A7.
3. **Observability follows RFC 25, with RFC 2's exception.** RFC 2 §1.1 limits the carver's
   dependencies, and §2.3 with RFC 25 §2.8 says it keeps no counters: its caller derives the
   `carver_` series. So no epic here takes a logger, a registry or a tracer, and the bridge to RFC
   25 is the engine's, not this plan's integration epics' (§6.1).
4. **Profiling.** No package starts a goroutine or opens a descriptor, so leak checks do not
   apply. The hot paths carry allocation assertions: the boundary search and `Cut` (RFC 2 §10.4),
   and the assembler's memory against chunk size (RFC 2 §5.5). Where they run is §6 point 3.
5. **Public interface and internals.** Tests cover the internal chunker and the exported surface.
   A value carried to a consumer outside this plan (a chunk ID into RFC 4's header, a plan into
   the RFC 8 pipeline) is tested at that consumer (`CLAUDE.md`, "Verifying a change"; RFC 2
   §10.1's last paragraph). No concurrency test: RFC 2 §10.1 says there is nothing two calls share.
6. **Known failures shrink.** No suite here keeps a `known.yaml` (§1.4), so no test lands red; a
   test written ahead of its code waits in its epic's branch.
7. **v2 ignores v1.** No import of, copy from, or behavioural dependence on v1's
   `pkg/block/carver` or `pkg/block/chunker`, its code and its measurements' code included.
   Epic 1's import test enforces RFC 2 §1.1, which is stricter: every package of the tree, its
   allowlist naming each exception per package, test packages included. v1 is deleted by #5484.
8. **Real dependencies for properties of those dependencies** (`CLAUDE.md`, "Writing tests").
   Three checks need the real thing: the golden vectors on a second CPU architecture (RFC 2 §10.1
   "Architecture", B2), the speed measures on the reference box (RFC 2 §9.1 K1 to K3), and the
   default `Target` measured through the real offload path (RFC 2 Appendix C). Everything else is
   a pure function of bytes and settings.
9. **Code comment conventions** (`CLAUDE.md`, "Code comments"). RFC 2 §6's `ponytail:` goes at
   the keyed gear table's construction, and §5.2's `decision:` at the block-target check.
10. **A test must be able to fail** (`CLAUDE.md`, "Writing tests"), and nothing else would catch
    the change it guards. The golden vectors guard the derivation; agreement with the reference
    chunker guards nothing the goldens do not unless it runs over random input and settings,
    which is why it is a property.
11. **A symbol is exported only if the RFC's interface uses it.** The consumers are the RFC 8
    pipeline and RFC 4's codec (§1.3). Everything exported is frozen at S1; the chunker is
    internal.
12. **Each subtask creates only what it declares.** No empty package and no stub without a
    caller: `internal/chunker` lands with epic 3, `internal/carvetest` with epic 2, each with its
    first consumer.

## 2. Epics at a glance

| # | Epic | Content | Subtasks | Man-days | Track | Blocks |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | **Interfaces** ⛔ | the exported surface of `carver` and `assembler`, stubbed; the error set; the §1.1 import test | 4 | 3 | team | **everything** |
| 2 | **Test foundations** ⛔ | the reference chunker, independent golden vectors and the BLAKE3 modes check, seeded inputs, the corpus | 4 | 7.5 | team | 3, 4, 5, 8 |
| 3 | **The chunker** ⛔ | settings derivation and refusal, `Target` in RFC 13's settings table, the gear tables, the boundary search | 7 | 12 | A | 4, 8 |
| 4 | **The carver and identity** | `Cut`, artificial ends, errors, chunk IDs, block names | 7 | 13.5 | B | 6, 8 |
| 5 | **The block assembler** | the fold, P1 to P6, the short last block, the block-target range | 6 | 10 | C | 6, 8 |
| 6 | **Integration with the offload pipeline** 🔀 | the value `Deps.Carver` takes, the assembler's additions S3 agrees | planned after S3 | planned after S3 | team | 7 |
| 7 | **Integration: the default `Target` on the real path** 🔀 | RFC 2 Appendix C's measurement plan, and §11's default frozen | planned after S4 | planned after S4 | team | 8 |
| 8 | **E2E green** | K1 to K5 on the reference box, nightly fuzzing, the second architecture | 5 | 8 | team | — |
| | **Total** | | **33** | **54** | | |

**Plan estimate: 33 subtasks, 54 man-days, not counting epics 6 and 7.**

RFC 2 is 1,582 lines, which gives N = 4. Its cohesive areas are three: the chunker and the
carver, RFC 2 §1.2's two layers, and the block assembler, which §1 and §5 make a separate layer.
The block name joins the carver, since §1.2 puts all of §4 there, rather than make a fourth epic
of about two man-days.

⛔ = blocker: other epics cannot finish until this one has delivered.
🔀 = synchronisation point: the team converges, or the epic waits on what another plan delivers.

## 3. Synchronisation points, blockers and the split

### 3.1 Dependency graph

```mermaid
flowchart TD
  X1(["#5484 deletes v1"]):::sync --> E1["E1 Interfaces ⛔"]:::blocker --> S1(["S1 surface frozen"]):::sync
  S1 --> E2["E2 Test foundations ⛔"]:::blocker --> S2(["S2 reference and goldens in place"]):::sync
  subgraph A["Track A"]
    E3["E3 The chunker ⛔"]:::blocker
  end
  subgraph B["Track B"]
    E4["E4 The carver and identity"]
  end
  subgraph C["Track C"]
    E5["E5 The block assembler"]
  end
  S2 --> E3 & E4 & E5
  X2(["RFC 13 plan epic 5: the node's settings table"]):::sync -. "Target's row" .-> E3
  E3 -. "the boundary search, for Cut" .-> E4
  S3(["S3 RFC 8 offload plan: the carver's surface in the pipeline agreed"]):::sync --> E6["E6 Integration with the pipeline 🔀"]:::sync
  E4 & E5 --> E6
  S4(["S4 RFC 8 offload plan: a pass runs end to end"]):::sync --> E7["E7 The default Target on the real path 🔀"]:::sync
  E6 --> E7
  E3 & E4 & E5 --> E8["E8 E2E green"]
  E7 -. "the default frozen closes it" .-> E8
  classDef blocker stroke:#c0392b,stroke-width:3px
  classDef sync stroke:#2c5aa0,stroke-width:3px,stroke-dasharray:4 2
```

### 3.2 Synchronisation points

- **S1 🔀 Surface frozen** (end of epic 1). The team reviews every exported signature, with the
  RFC 8 offload plan's and RFC 4 plan's authors, since their code calls it (§4.2's seams).
- **S2 🔀 Reference and goldens in place** (end of epic 2). The reference chunker agrees with the
  golden tables and boundaries; the BLAKE3 modes match the independent vectors; the inputs and
  corpus exist. Epics 3, 4 and 5 start.
- **S3 🔀 RFC 8 offload plan: the carver's surface in the pipeline agreed** (external). What
  that plan agrees with this one: the method set of `Deps.Carver`, and how a namespace's settings
  value reaches the Carve state, since the settings are per namespace and `Deps` holds one
  `Carver` (RFC 8 §2.6); whether the assembler takes RFC 8 §6.3's key budget as an input, against
  RFC 2 P2 and A7 (§6 point 4); the lookup while deduplication is off (D3); and who computes a
  chunk's all-zero flag (D3). Epic 6 starts here.
- **S4 🔀 RFC 8 offload plan: a pass runs end to end** (external). A workload written through the
  journal is offloaded through the carver, the assembler and the syncer to a remote store, with
  offers bounded and widened as RFC 8 §6.7 says. Epic 7 starts here.

### 3.3 Blockers

| Epic | Blocks | Why |
| --- | --- | --- |
| ⛔ E1 | every epic | the frozen surface every test calls |
| ⛔ E2 | E3, E4, E5, E8 | the reference chunker, goldens, inputs and corpus |
| ⛔ E3 | E4 (`Cut`, dotted), E8 | `Cut` asks the chunker for every boundary; E8 benchmarks it |
| E4 | E6, E8 | the pipeline calls `Cut` and the name; E8 benchmarks `Cut` |
| E5 | E6, E8 | the pipeline drives the fold; E8 fuzzes it nightly |
| E6 | E7 | the sweep runs the composed pipeline |
| E7 | E8 (dotted) | E8 closes once the default is frozen; nothing else in it waits |
| S3, S4 (external) | E6, E7 | the RFC 8 offload plan's agreement and pipeline |
| #5484 (external) | E1 | v1 holds `pkg/block/carver` until it merges (D1, §6 point 1) |
| RFC 13 plan, epic 5 (external) | E3 (dotted) | the node's settings table `Target` is declared in |

### 3.4 Splitting the work

- **Track A — the chunker.** Epic 3, from S2. It lands the boundary search first, since track B's
  `Cut` needs it, then the keyed tables, `Target`'s settings row and the conformance rows. After
  it, epic 8's K1 benchmark and the second-architecture cell.
- **Track B — the carver and identity.** Epic 4, from S2. Until epic 3's boundary search lands it
  builds what needs no chunker: the chunk-ID function, the block name and its golden vector, the
  fault-sweep harness. Then `Cut`, its edge cases and faults. After it, epic 8's nightly fuzz
  suite, until S3 opens epic 6.
- **Track C — the assembler.** Epic 5, from S2; it needs nothing of tracks A and B, since its
  tests fold synthetic chunk streams. After it, epic 8's K4 and K5 corpus reports, then epic 7
  at S4.
- **Where the team converges.** Epics 1 and 2 at the start; epics 6 and 7 at the RFC 8 offload
  plan's points. Once epic 8's pieces are done, a track whose next epic waits on S3 or S4 is idle
  for this plan, and goes to the RFC 8 offload plan's Carve and Assemble work, which S3 needs.

## 4. Implementation decisions

### 4.1 Directory layout

```text
pkg/block/carver/                    E1 → E4: the settings value, Cut, Chunk, chunk ID, block name, errors
pkg/block/carver/imports_test.go     E1: RFC 2 §1.1's import rule over the tree
pkg/block/carver/internal/chunker/   E3: gear tables, masks, the boundary search
pkg/block/carver/internal/carvetest/ E2: reference chunker, seeded inputs, corpus fetch
pkg/block/carver/assembler/          E1 → E5: the fold of RFC 2 §5
pkg/block/carver/*/testdata/         E2: golden vectors; E3 to E5: fuzz seeds
```

### 4.2 Decisions

- **D1. v2 takes v1's path, `pkg/block/carver`, once #5484 deletes v1** — pending the user's
  answer (§6 point 1). The tree groups the layers under one root, so the import test covers them
  all and RFC 8 and RFC 4 import from one place. Epic 1 waits on #5484. The other option is a
  fresh root now (`pkg/block/carverv2`, renamed later), which starts without waiting and costs a
  rename of every import once the RFC 8 and RFC 4 code calls it. Prior art: the journal-only
  engine plan built under `enginev2` and renamed after the delete (#3645). Applied by epic 1.
- **D2. The chunker is an internal package; the assembler is a concrete type.** RFC 2 §1.2's
  shapes are indicative. No consumer outside the carver calls the boundary search, so it is not
  exported; the package boundary still keeps RFC 2 §1.2's two layers apart, since the carver
  sees no fingerprint. RFC 2 §5.1 shows `Assembler` as an interface; the engine declares its own
  interface over the concrete type, so a method S3 adds later is additive and costs no
  migration. Prior art: restic's `chunker` package exports one chunker type and keeps its tables
  internal (from memory); Go's "return concrete types" convention. Applied by epics 1, 3 and 5.
- **D3. While deduplication is off, the pipeline passes a lookup that finds nothing**, so every
  chunk is carried (RFC 8 §6.3's Assemble, RFC 8 V1) with no nil case in the fold. A lookup error
  is handled as RFC 2 §5.1 and §5.7's "lookup error" row say, so the fold returns no error of its
  own (§6 point 3). A chunk's all-zero flag is set by the caller, which holds the bytes in `emit`;
  the assembler never sees bytes (RFC 2 §5.5). Both are confirmed at S3. Prior art: none found.
  Applied by epic 5.
- **D4. The settings constructor takes the encryption flag beside the keys**, since the carver
  cannot otherwise know it for RFC 2 §3.2's refusal of a keyed table. It refuses a chunking key
  without encryption and encryption without a key, and builds the table itself, so no
  inconsistent pair can be built (C10's cutter half). Prior art: none found. Applied by epics 1
  and 3.
- **D5. A reader that makes no progress 100 times in a row fails `Cut` with `io.ErrNoProgress`**
  (RFC 2 §10.2 leaves the bound open). Prior art: `bufio.Reader`, the same bound and error (from
  memory). Applied by epic 4.
- **D6. The goldens come from implementations the code under test does not share.** The gear
  tables, a chunk ID and a block name are computed with BLAKE3's reference implementation (its
  `b3sum` tool or its official Python binding, named once with the commands beside the vectors);
  the name vector's chain ID is laid out by hand from RFC 5 §2.8's table. Boundaries come from
  epic 2's reference chunker. Prior art: BLAKE3's own `test_vectors.json`, produced by its
  reference implementation (from memory). Applied by epic 2.
- **D7. The corpus is public tarballs pinned by SHA-256, fetched once and cached outside the
  repository** — pending the user's answer (§6 point 2). K4, K5 and §10.4's edit stability need
  "real files of several kinds" (RFC 2 §9.1), which no repository should hold; RFC 2 Appendix C
  used a compiler toolchain tar. The other options: a seeded synthetic corpus in the repository
  (no real structure, so K5 measures nothing real), or a committed corpus (repository growth).
  How the per-change check reaches it is §6 point 3. Prior art: `golang.org/x/benchmarks` fetches
  its inputs (from memory). Applied by epics 2 and 8.
- **D8. Under `-short`, the heavy rows run a reduced size**, and their full size runs nightly in
  epic 8's suite. RFC 2 §10.3's sweep at every byte offset is quadratic in the stretch, and §10.4's
  256 MiB per profile and §9's 64 MiB of zeros at every `Target` add up past what `unit` can
  spend. Under `-short` the sweep covers every chunk end and buffer refill, one byte either side,
  and a seeded sample of other offsets, at the smallest valid `Target`. Prior art: Go's own
  `testing.Short()` convention in the standard library's tests. Applied by epics 3 and 4.
- **D9. The block name takes plain byte arrays** for the scope, the nonce, the chain ID and the
  chunk IDs (RFC 2 §4.2). It imports nothing of RFC 5's or RFC 16's packages (RFC 2 §1.1), and it
  draws no nonce: the pipeline does (RFC 8 §6.6). Prior art: none found. Applied by epic 4.

### 4.3 Knowingly simple implementations

- **The keyed gear table** (RFC 2 §6's `ponytail:`): its ceiling and upgrade are the RFC's,
  carried at the table's construction (rule 9). Epic 3. The upgrade is a ledger row (§6.1).

## 5. The epics in detail

### Epic 1 — Interfaces ⛔

**Epic 1 estimate: 4 subtasks, 3 man-days.**

The surface §1.3 lists, with stub bodies, reviewed at S1. **Owns:** RFC 2 §1.1 (the import rule),
§1.2's split into packages. **Done when:** the import test passes over the tree, and the surface
is reviewed with the RFC 8 offload plan's and RFC 4 plan's authors.

**Expected content:**
- `carver`: the settings value and constructor, `Cut`, `Chunk`, the chunk-ID and name functions,
  the errors.
- `assembler`: the fold's type, constructor and plan types (D2, D3).
- The import test.

### Epic 2 — Test foundations ⛔

**Epic 2 estimate: 4 subtasks, 7.5 man-days.**

The components of §1.4; no suite registered and no red test (§1.4). **Owns:** RFC 2 §1.3 (checks
need no fixture), §10.1's golden kind's inputs. **Done when:** the reference chunker matches the
tables of D6; the BLAKE3 library's three modes match D6's vectors; the inputs have their tests.

**Expected content:**
- The reference chunker, literal from RFC 2 §3.2, by an engineer other than epic 3's.
- The golden vectors of D6 and the check of the BLAKE3 modes against them.
- Seeded inputs of RFC 2 §10.2's content kinds.
- The corpus of D7: the pinned list, the fetch and cache.

### Epic 3 — The chunker ⛔

**Epic 3 estimate: 7 subtasks, 12 man-days.**

The boundary function and its settings. **Owns:** RFC 2 §3.1 to §3.5, §3.7, §3.8, §6 (the tables
and the cutter's refusal); `Target`'s row of RFC 13 Appendix B. **Done when:** RFC 2 §9's chunker
rows; B1 to B5 (§3.1); C2, C3, C4 and C10's cutter half (§8); §10.4's counts for the chunker, as
§6 point 3 settles them; §10.2's settings edge cases; the RFC 13 plan's Appendix B check (its D3)
passing with `Target` declared.

**Expected content:**
- The settings derivation and refusal, and the mask derived from `Target` with its start-up check.
- `Target` declared in the node's settings table, namespace-scoped and bound, validated by the
  settings constructor (§6 point 5).
- The unkeyed and keyed gear tables, with §6's `ponytail:` at the keyed one.
- The boundary search with normalisation and the 64-byte warm-up, landed first for track B.
- §9's chunker rows, repetitive input among them, sized by D8.
- Property fuzzing against the reference chunker; the allocation count.
- K1's benchmark function, run by epic 8.

### Epic 4 — The carver and identity

**Epic 4 estimate: 7 subtasks, 13.5 man-days.**

`Cut` over one stretch, and the names of §4. **Owns:** RFC 2 §2.1, §2.2, §2.4, §4.1 to §4.3, §7.
**Done when:** RFC 2 §9's carver and name rows; C1, C4, C7 (§8); §10.2's stretch-shape,
buffer-edge, reader and content cases; §10.3's fault sweep, sized by D8, and its two-buffer-size
determinism; §10.4's counts for `Cut` and the name vector, as §6 point 3 settles them.

**Expected content:**
- The chunk-ID function and the block name with its golden vector, first, while epic 3 lands.
- `Cut`: the refill loop, borrowed bytes, `consumed`, the buffer-size refusal.
- The artificial end, checked as offers re-offered from `consumed` against one long stretch.
- Errors: a reader or `emit` failure, no short chunk, D5's bound.
- The edge cases of RFC 2 §10.2.
- §10.3's sweep; property fuzzing of C1, C4, C7.
- K2 and K3's benchmark functions, and the allocation count.

### Epic 5 — The block assembler

**Epic 5 estimate: 6 subtasks, 10 man-days.**

The fold of RFC 2 §5. **Owns:** RFC 2 §5.1 to §5.7. **Done when:** RFC 2 §5.7's A1 to A7 and every
row of its check table; §10.2's block-target edge cases.

**Expected content:**
- The constructor and the block-target range, with §5.2's `decision:` at the check.
- The fold: whole chunks, the target in carried bytes, the count cap (P1, P2, P4, P5).
- Dispositions: zero refs and the lookup, finding nothing and failing (P3, D3).
- Repeats within a block (P6).
- Packing across files, contiguous per file; the short last block and its age (§5.3, §5.4).
- The plan holds no bytes (§5.5), with the memory check; fuzzing of A1, A2, A4 to A7.

### Epic 6 — Integration with the offload pipeline 🔀

**Epic 6 estimate: planned after S3.**

The carver's own part of the composition S3 agrees with the RFC 8 offload plan. The carver has no
state, no configuration file and no observability surface of its own (rule 3), so this is all
the integration it has. **Owns:** RFC 2 §5.1's inputs as the pipeline supplies them. **Done
when:** the RFC 8 offload plan's Carve and Assemble rows pass over the real carver and assembler.

**Expected content:**
- The value the node passes as `Deps.Carver`, and the per-namespace settings it carries.
- The assembler's input for RFC 8 §6.3's key budget, if S3 agrees one (§6 point 4).

### Epic 7 — Integration: the default `Target` on the real path 🔀

**Epic 7 estimate: planned after S4.**

RFC 2 §11's one open question. **Owns:** RFC 2 §11, Appendix C's measurement plan, §9.1's rule
for full-stack benchmarks. **Done when:** Appendix C's plan has run on the K5 corpus and the
default `Target` is frozen in RFC 2 and RFC 13 Appendix B, before the release writes content.

**Expected content:**
- The sweep of Appendix C's measurement plan, through the composed pipeline, with its report.
- The default frozen, or changed, in both RFCs.

### Epic 8 — E2E green

**Epic 8 estimate: 5 subtasks, 8 man-days.**

What no per-change suite runs (RFC 2 §10.6's last sentence). **Owns:** RFC 2 §9.1, §10.5, §10.6.
**Done when:** K1 to K5 recorded on the reference box against §10.5's targets, as §10.6 records
them; the golden vectors and allocation counts green on a second architecture; fuzzing past the
seed corpus and D8's full sizes run nightly; RFC 2's suites paragraph names each new suite.

**Expected content:**
- A `manual` benchmark suite for K1 to K3 (RFC 29 §4.10), with §10.6's SIMD and toolchain fields.
- K4 and K5 over D7's corpus, reported.
- A nightly suite of fuzz targets, one `go test -fuzz` per target, and D8's full sizes.
- A cell running the carver's tests on a second architecture (§6 point 6).
- RFC 2's suites paragraph and `docs/internals/testing.md` regenerated with `dt docs`.

## 6. Risks and open points

1. **Where v2 lives, and #5484** (D1) — pending the user's answer. Recommended: `pkg/block/carver`
   once #5484 merges; epic 1 waits on it.
2. **The corpus** (D7) — pending the user's answer. Recommended: pinned public tarballs, fetched
   and cached outside the repository.
3. **RFC 2 amendments** — pending the user's answer, proposed to RFC 2's author; RFC 2 is `draft`.
   The plan follows RFC 2 as written meanwhile:
   - §10.4 against §10.6: §10.4 lists per change the golden vectors on two architectures, the
     allocation counts and edit stability on the fixed corpus; §10.6 puts the allocation counts in
     `unit/nonrace`, which runs nightly, and says no suite runs the golden vectors on a second
     architecture; the corpus needs a network fetch, which RFC 2 §1.3 rules out for a check.
     Proposed: per change, the golden vectors on one architecture, the allocation counts in `unit`
     behind a race-detector guard, and edit stability on a small pinned file held in the
     repository; nightly, the second architecture and K4 on the 1 GiB corpus file of §10.5.
   - §2.3: `carver_chunk_size_bytes`'s buckets become RFC 25 §2.6's `size` set, as RFC 25 §11's
     audit asks.
   - §5.1: the Go block reads "indicative, as §1.2's"; the fold is a concrete type and `Add`
     returns no error (D2, D3); the plan's shape names refs apart from carried entries, which P6
     needs.
   - §3.2: say whether "while the chunk is shorter than `Target`" counts the byte being tested;
     the golden boundaries depend on it. Epic 2's reference chunker takes the reading in which the
     length counts the bytes before the tested one, until the RFC says.
   - Appendix A: once #5484 deletes v1, its table describes nothing; proposed: keep A.1 and A.2 as
     the measurements behind §3.2 and §3.4, and drop the table.
4. **The key budget against P2 and A7** — pending the user's answer, and S3. RFC 8 §6.3's
   Assemble closes a block early when its commit would pass RFC 6 §5.2's key budget, which RFC 2
   P2 and A7 do not allow. Recommended: amend RFC 2 P2 and A7 to allow that close, with the budget
   as an input of the fold; the other option, the pipeline cutting a plan after the fold,
   duplicates P1's chunk boundaries in the engine. Epic 6 builds whichever S3 agrees.
5. **`Target` has no owner in RFC 13's plan**, which gives the block target to RFC 4 and the keys
   to RFC 5. This plan takes it, in epic 3; if the RFC 8 offload plan or RFC 23's namespace
   creation takes it instead, epic 3 sheds half a day.
6. **Burn-down whose size cannot be known**: epic 7's sweep depends on what the pipeline costs to
   run per `Target`; epic 8's second-architecture cell depends on the hosts the harness has. Its
   cells run on Linux, Windows and macOS (`test/suites.yaml`), so the arm64 cell is macOS unless a
   Linux arm64 host is added.

### 6.1 Deferral ledger

| Deferred | Why | Owner | Trigger |
| --- | --- | --- | --- |
| Deriving and exporting the `carver_` series of RFC 2 §2.3 (§1.5 rule 3) | the carver keeps no counters; its caller derives them (RFC 25 §2.8) | RFC 8 offload plan | the Carve state calls `Cut` |
| RFC 2 §2.4's two exceptions, the age and widening back, and RFC 8 §6.7's run widening (§1.4) | they are the caller's decisions | RFC 8 offload plan | the pipeline offers runs |
| RFC 2 §5.3's "a block never mixes two flows or two namespaces", and §10.1's checks at the consumer, whole chunks and copies made before `emit` returns (§1.5 rule 5) | the pipeline decides what one assembler folds and what `emit` keeps | RFC 8 offload plan | its Assemble state lands |
| Recording which settings produced a namespace's content and reporting a change as a migration (RFC 2 §3.6, RFC 8 §2.4, RFC 13 §5.3) (§1.4) | the engine's validator row, as RFC 13's plan assigns it | RFC 8 offload plan | the engine applies settings |
| Re-cutting or refusing an imported chunk longer than `Max` (RFC 2 §3.6, C9, §9's consumer row) (§1.4) | the import and re-home paths are RFC 27's | RFC 27 | a plan for RFC 27 is written |
| The namespace keys: the chunk-ID key drawn for every namespace and never rotated (RFC 2 §4.1), the chunking key drawn only for an encrypting one, refused by fingerprint and never rotated with the data keys (RFC 2 §6, C10's namespace half) (§1.3, D4) | the key table and namespace keys are RFC 5's; the carver takes keys as inputs | RFC 5 | its namespace keys land |
| C8 and C11: the nonce drawn from a cryptographic source, the name minted once with its put intent, a plan refused under another namespace's key (RFC 2 §4.2, §4.3; §9's consumer row) (D9) | the intent step is the pipeline's (RFC 8 §6.6) | RFC 8 offload plan | its Intent state lands |
| The block-target key in RFC 13's table (RFC 2 §5.2; epic 5 provides the check) (§6 point 5) | RFC 13's plan gives it to RFC 4 | RFC 4 | its remote-store settings land |
| The dedup lookup in use, and §4.3's matching rules (D3) | deduplication is off in the first release (RFC 8 §6.5) | RFC 8 | deduplication is re-added, post-v1 |
| RFC 2 §6's keyed PRF upgrade of the keyed gear table (§4.3) | the RFC's `ponytail:` | unowned | a deployment must hide which files it holds from a bucket reader |
| Randomised assembly (RFC 2 §5.6) (epic 5) | sequential assembly is the RFC's proposal; changing it needs no migration | unowned | a measured need to blur the chunk-size fingerprint, post-v1 |

## 7. RFC coverage

| RFC section | Epic(s) |
| --- | --- |
| RFC 2 "Start here", "In short", "Background" | explanatory; the scenarios run as §1.4 says |
| RFC 2 §1, §1.1 | E1 |
| RFC 2 §1.2 | E1 (packages), E3 (chunker), E4 (carver) |
| RFC 2 §1.3 | E2 |
| RFC 2 §2.1, §2.2, §2.4 | E4; ledger (the age and widening) |
| RFC 2 §2.3 | ledger (RFC 8 offload plan); §6 point 3 |
| RFC 2 §3.1 to §3.5, §3.7, §3.8 | E3 |
| RFC 2 §3.6 | E3 (`Target` declared bound); ledger (recording, imports) |
| RFC 2 §4.1, §4.2 | E4; ledger (the keys, the nonce, the intent) |
| RFC 2 §4.3 | E4 (the scope in the name); ledger (matching, C11) |
| RFC 2 §5.1 to §5.7 | E5; E6 (inputs from the pipeline); ledger (§5.3's per-share rule, §5.6) |
| RFC 2 §6 | E3; ledger (the keys, the PRF upgrade) |
| RFC 2 §7 | E4 |
| RFC 2 §8 | E3 (C2, C3, C4, C10's cutter half), E4 (C1, C4, C7); ledger (C8, C9, C10's namespace half, C11) |
| RFC 2 §9 | E3 (chunker rows), E4 (carver and name rows); ledger (consumer rows) |
| RFC 2 §9.1 | E3, E4 (benchmark functions); E7 (full-stack rule); E8 (runs) |
| RFC 2 §10.1 to §10.3 | E2 (components), E3, E4; ledger (§10.1's consumer checks) |
| RFC 2 §10.4 | E2 (corpus), E3, E4; §6 point 3 |
| RFC 2 §10.5, §10.6 | E8 |
| RFC 2 §11, Appendix C | E7 |
| RFC 2 Appendix A | E2 (regression checks); §6 point 3 (rewrite) |
| RFC 0 §1.2 | E2 (no fixture), rule 1 |
| RFC 0 §2.1, §2.2 | E4, E5 |
| RFC 0 §5.2 | constraint on `Cut`'s callers; ledger |
| RFC 1 §3.2, §3.3 | constraint on `Cut`'s callers; E4 |
| RFC 4 §3.2, §3.4 | E1, E4 (the chunk-ID and name functions its codec calls) |
| RFC 5 §2.8, Appendix B.2 | E2 (the chain ID by hand), E3, E4 (keys as inputs); ledger |
| RFC 6 §3.5, §4.1 | E5 |
| RFC 8 §2.1, §2.6, §6.3, §6.5 | E5, E6 |
| RFC 8 §2.4, §6.2, §6.6, §6.7, §7.8 | ledger (RFC 8 offload plan) |
| RFC 13 Appendix A, Appendix B | E3 |
| RFC 16 §4.2.1 | E4 |
| RFC 25 §2.6, §2.8 | rule 3; §6 point 3 |
| RFC 27 §2.4, §2.7 | ledger |
| RFC 29 §4.10 | E8 |
