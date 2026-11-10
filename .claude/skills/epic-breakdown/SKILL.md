---
name: epic-breakdown
description: Expand one epic of a DittoFS implementation plan (docs/internals/planning/rfc-*-plan.md) into its standalone epic document — every MUST and MUST NOT the RFC places on the epic numbered as R-requirements, ordered subtasks with acceptance criteria, tests and a man-day estimate each, a grep-able total, and a suggested, optional grouping into pull requests — written to docs/internals/planning/. Also says how to create the epic and its subtasks as GitHub issues that point at the document, in the repo's project board with their estimates and under the RFC's roadmap epic, done only when explicitly asked and once the document has merged. Use whenever an epic is to be broken down, detailed, expanded into subtasks or made ready to start ("break down epic 5 of the journal plan", "expand rfc-1 epic 7", "/epic-breakdown 1 8"). Not for planning a whole RFC (rfc-to-epic-plan).
---

# Epic breakdown

Adapted from Addy Osmani's `planning-and-task-breakdown` skill (agent-skills, MIT licence)
for DittoFS's RFCs, plans and epics. It applies "Design principles" in
[`.claude/skills/rfc-to-epic-plan/SKILL.md`](../rfc-to-epic-plan/SKILL.md#design-principles),
the only copy, including how tests run through the repo's test runner and land red.

## Overview

Expand one epic of a plan into an epic document a single owner can work from, without
opening the plan or the RFC to find out what to build. The document has three parts that
the plan does not:

1. **The requirements.** Every MUST and MUST NOT that the RFC places on the epic, numbered
   R1…Rn, so that subtasks and tests can cite them and nothing is silently dropped.
2. **The subtasks.** Small, ordered and verifiable, each with acceptance criteria, its tests,
   the requirements it covers and a man-day estimate.
3. **The pull requests.** An optional suggestion of how the subtasks group into pull
   requests; one pull request per subtask works as well.

The reference result is [`example-epic.md`](example-epic.md), beside this file: an epic of
an invented RFC that follows every rule below, with its requirement tables, estimate lines
and suggested pull requests. Read it once for shape and depth. **Do not copy its content.**

**Do not write code while breaking down.** Read the RFC and the code you need to understand,
and write the document. That is the whole output.

## Terminology: epic and subtask, nothing else

The vocabulary is the one under "Terminology: epic and subtask, nothing else" in
`.claude/skills/rfc-to-epic-plan/SKILL.md`: read it there. The epic document adds `D` for its
decisions, and its subtask headings (`### T4.3 …`) and estimate lines
(`**Subtask T4.3 estimate: …**`) always carry the `T`, because the checks parse them.
The placeholders `<k>` (the epic as written, `7a`) and `<e>` (its number without the
letter, `7`) and the meaning of a *cited* id are defined there too: subtask ids are always
`T<e>.<n>`, and an epic split into parts (`7a`, `7b`) keeps the epic's subtask ids,
numbered once across the parts.

Two things decide the numbers: the order of work, and whether an id has been cited.

- **Number in order of work.** The ids read as a path, `T4.1 → T4.2 → T4.3 → …`, in the
  order of §6, which follows the rules under "Order" in Step 3. The plan's expected content
  for the epic is an outline with no ids; an older plan that numbers it gives ids nobody
  has cited, so number from the order of work, not from the plan.
- **A cited id is fixed.** Progress is tracked by mapping pull requests to subtask ids, and
  other epic documents name this epic's subtasks, so a cited id keeps its number for the
  same work. When the order of work changes, **move the subtask in §6 and §7 and keep its
  number**, even where that leaves the numbers out of order. Before renumbering anything,
  look for citations as `issues.md` 6.8 says.
- **Split with a letter suffix, never by renumbering.** A subtask split after its id was
  cited becomes `T4.3a`, `T4.3b`; work mapped to `T4.3` then covers both parts.
- **Never reuse or drop a cited id.** A subtask added once ids are cited takes the next free
  number and its place in the order of work. If cited work moved, say where in §9.

## When to use

- An epic of an existing plan is about to start and needs its detailed breakdown.
- An epic's subtasks are too vague to estimate or to hand to someone.
- The owner wants to know which RFC obligations the epic is answerable for.

**When not to use:** there is no plan yet (use `rfc-to-epic-plan`); the epic is "planned
after Sx" and Sx has not been reached (say so, and stop unless the user insists); the
epic's document already exists and the user did not ask to replace it.

## Step 0 — Inputs

| # | Input | How to fill it |
| --- | --- | --- |
| 1 | Plan path and epic number | **required**, from the request |
| 2 | Output path | default `docs/internals/planning/rfc-<n>-<slug>-epic<k>.md`. **If the file exists, stop and ask** whether to replace it, revise it in place, or write elsewhere. Never overwrite it on your own: someone may be working from it |
| 3 | Track | A, B, C or the team, from the plan's §2 (Step 1 has the fallback for an older plan); never ask for names |
| 4 | Existing epic issue | `gh issue list -R "$R" --state all --label epic --label rfc-<n>-epic-<k>`, with `-R` as `issues.md` 6.1 says (both labels must match; a text search for "epic <k>" also returns other epics). `<k>` keeps its letter: for epic 7a the label is `rfc-<n>-epic-7a`, and the unsplit epic 7's issue is not this part's. If one exists, note its number; when issues are created (Step 6), there must not be a second one |

Ask for whatever is missing with `AskUserQuestion`, in one batch, before reading further.
If you cannot ask (for example when running as a subagent), stop and report the missing
inputs rather than guess.

## Step 1 — Read

- The plan: the epic's row in §2, its section in §5 (its **Owns:** and **Done when:**
  lines), its blockers and synchronisation points in §3, the general rules in §1.5, the
  §4.2 decisions and §4.3 simplifications that name the epic (they become entries of the
  epic document's §4), and every §7 coverage row that names the epic. That
  gives the **owned sections**: the RFC sections, and the related-RFC sections, the epic is
  answerable for.
- **A plan written before these conventions** may lack some of that. Fall back, and say in
  the report which fallback was used:
  - no §7 coverage table → the sections cited in the epic's §5 section and its §2 row are
    only a starting point: a plan without §7 cites a section's parent and leaves out the
    sections it names in passing. Widen the list with the sections those cite, the RFC's
    requirement and test rows that name the epic's area, the RFC headings (`rg -n '^#'
    <rfc>`) matching the §5 bullets, and the **Sources** header of an existing document for
    this epic. Show the widened list to the user and have it confirmed before Step 2; it
    is then the owned sections for every later step.
    When nobody can confirm it, keep the widened list and name it in the report as an
    assumption;
  - no `**Epic <k> estimate:**` line → report "no plan figure" instead of a comparison;
  - no Track column → the track named for the epic in §3.4 or in the §2 row (a letter
    only), else "the team".
  - no **Owns:** / **Done when:** lines, or no §4.2–§4.3 → the epic's §5 paragraph and §7
    rows give the owned sections, and its decisions are whatever the plan's text settles
    for the epic. The plan is not converted as part of the breakdown.
  - a §1.5 that says to copy the rules word for word, or rules with no number or title →
    §2 cites them anyway, by number, or by a short title the document gives each.
- The RFC: the owned sections in full, and the sections they cite. Large RFCs exceed one
  `Read`; list headings with `rg -n '^#' <rfc>` and read by line range.
- The epics this one depends on: their documents if they exist, for the interfaces this epic
  builds on. And the epics it blocks, for what they expect from it.
- Code only as far as the plan's rules allow. If the plan says v2 ignores v1, do not read v1
  to design v2.
- For each choice §4 will record, how one or two comparable projects or the state of the art
  do it, named, with a link or marked "from memory"; §4 cites it.

## Step 2 — Extract the requirements

This is the step the plan does not do, and the one that matters most. The goal is that
**every obligation the RFC places on this epic appears exactly once, numbered**, and that a
reviewer can check the list against the RFC without rereading it.

**2.1 Read each owned section for its obligations.** Read the owned sections in full and
list every statement that binds this epic's code. The markers below are what the RFC's
author uses to flag a candidate, not proof of an obligation: decide each one from what the
sentence says (a key word can sit in a heading, an example or a negation such as "is not
required", and a rule can carry no key word at all). Look for:

- the RFC 2119 key words (MUST, MUST NOT, SHALL, REQUIRED, SHOULD, SHOULD NOT), in capitals
  or in lower case: some RFCs write them only in lower case;
- **bold lead sentences**, which the RFCs use for rules even with no key word
  ("**Records, once written, are immutable.**"); each one is a rule;
- invariants stated in prose (`never`, `always`, `at most`, `at least`, `only`), when they
  constrain the code rather than describe it (`append-only` and "the only copy" usually
  describe);
- the numbers the sections put on the epic: cost bounds (`O(log n)`), limits, sizes and
  performance targets in tables, and the RFC's test rows (a §9-style table) that name this
  area.

Note each candidate with its RFC line number in a scratch file, not in the document.

**2.2 Turn the candidates into requirements.**

- One requirement per obligation. Merge sentences that state the same obligation in two
  places, and cite both sections. Split a sentence that carries two independent ones.
- Restate each one as a checkable sentence in the RFC's terms, and keep the polarity: a MUST
  NOT stays a prohibition ("never…", "is refused…").
- **Name only what the sources name.** Every identifier a row gives (a type, an error, a
  function, a config key, a metric) must appear in a section its `Source` cites. A name
  taken from somewhere else (another RFC, an earlier epic's document, the plan) adds that
  place to `Source`; a name found in none of them is replaced by the RFC's own words, since
  a requirement must not invent the API it holds the code to.
- A prose `never` / `always` becomes a requirement only if it constrains this epic's code. A
  sentence describing another component, or giving background, does not.
- A SHOULD becomes a requirement only if the epic adopts it. Otherwise record why not in §4
  (decisions).
- An obligation the RFC leaves open (it states the rule but not one case of it) stays a
  requirement, and names the subtask that decides it and records a `decision:` marker. When
  a later subtask of this epic or a later epic pins the case, §4 decides it, with its marker,
  rather than the code.

- An obligation in an owned section that another epic carries out: write the part that is
  this epic's, and name the epic that owns the rest ("the index offers the predicate; epic 9
  owns the hold rule"). Do not list obligations that belong wholly to another epic.

Not adopting a SHOULD, or deciding an open case, that a later subtask or epic pins is a
question for the user (Design principles).

**2.3 Number and tabulate.** In a new document, number them R1…Rn in table order, grouped
under short bold headings by theme (structure, queries and cost, concurrency, limits, recovery, …). One table
per group:

```markdown
| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R1 | A file's held extents are disjoint. A superseding write splits or narrows, and never leaves two extents covering one offset | MUST | RFC 1 §4 | T4.2 |
| R13 | Covering extent of an offset, or its absence, in O(log n) | bound | §4.1 | T4.1, T4.2 |
```

`Kind` is one of `MUST`, `MUST NOT`, `SHOULD` (adopted), `invariant` (prose `never` /
`always`), `bound` (cost, size or performance target) or `test row` (a row of the RFC's own
test tables). `Source` cites the section, with the RFC named when the epic has more than
one source. Once the document exists, numbers are stable: a requirement added later takes
the next free number and goes in its group's table, so a table may read `R12, R47, R13`.
A number is never reused or renumbered, and a dropped requirement keeps its row, marked
"withdrawn" with the reason.

**2.4 Check coverage, both ways.**

- Every candidate noted in 2.1 maps to an R-number, to another epic, or to "not an
  obligation" (background, an example, a negation). Go through the scratch list one by one;
  an unmapped candidate is a gap.
- Every R-number has a subtask, and either a test that fails on its assertion when the
  requirement is broken, or what already enforces it when this epic lands, named: the
  compiler, a check, E2's red test, or a test of an epic this one depends on, by id. A
  requirement nothing in this epic can break goes to the epic that can.
- Every subtask covers at least one R-number, or names, by id, the subtask in this epic that
  consumes it. A contract a later epic pins is allowed only with a §4 decision saying why it
  cannot wait. No test asserts a stub's answer.

## Step 3 — Carve the subtasks

**Slice vertically where the epic allows it.** Build one operation or one path end to end,
with its tests, rather than all the types, then all the methods, then all the tests:

```
Bad:  T4.1 all types   T4.2 all insert paths   T4.3 all queries   T4.4 the tests
Good: T4.1 B-tree container with its property test
      T4.2 held-extent insert and point lookup, tested at 10⁶ extents
      T4.3 span query with exact gaps, tested against an oracle
```

Each subtask leaves the tree building and its tests green, lands its own tests, and creates
only what it or an earlier subtask consumes.

**Size.** One engineer, under the subtask ceiling (rfc-to-epic-plan, "Subtasks are not pull
requests" in §1.5's instruction), and there is no target number of subtasks. Cut by what makes sense: one logical change, readable on its
own, following the dependencies, delivering something that can be named. A subtask over the
ceiling says why it cannot be split without leaving a half-built
state. Signs that a subtask should be split: its acceptance criteria take many bullets, it
touches two independent subsystems, or its title needs an "and" between two unrelated
things.

**Order.** Settle the order before the numbers, and number from it ("Number in order of
work" above). Draw the dependencies, then order the subtasks by these rules, **applied in
turn: each later rule only breaks the ties the earlier ones leave.** This is the one copy of
them.

1. No subtask depends on one below it in §6. The check enforces this.
2. A blocker moves as early as its own dependencies allow. A blocker is a subtask two or
   more others build on, or one another epic waits for; this is a class, not a ranking,
   so the blockers' own order is left to the next rules.
3. One path at a time: the next subtask builds on the one just above where it can, and a
   branch is finished before another starts rather than alternating. With A1→A2, B1→B2 and
   B1 blocking another epic, rule 2 puts B1 first and rule 3 then gives B1, B2, A1, A2.
4. Between subtasks still tied, the high-risk one goes first, so it fails fast.

Mark checkpoints (§7 below) wherever something larger than one subtask can be shown to
work. Checkpoints are the natural places to cut a pull request.

## Step 4 — Estimate each subtask in man-days

Estimate each subtask with the scale, the weighting and the number rules under "Estimating
in man-days" in `.claude/skills/rfc-to-epic-plan/SKILL.md`. That section is the only copy:
read it there rather than restating it here, so the plan's figures and these stay on one
scale.

Each subtask carries exactly one estimate line, on its own line, in this form:

```markdown
**Subtask T4.2 estimate: 2.5 man-days.**
```

The epic total goes in the document header, in the same form the plan uses:

```markdown
**Epic 4 estimate: 12 subtasks, 21.5 man-days.**
```

Numbers follow "Estimating in man-days" in `rfc-to-epic-plan/SKILL.md`. Compute the total
with the check beside this file, never by hand:

```bash
awk -f .claude/skills/epic-breakdown/epic-check.awk <epic-doc>
```

Only the `TOTAL` line may print; write it into the header. Anything else is a defect to
fix. The comment at the top of `epic-check.awk` is the one list of what it reports and of
the format it reads: the estimate lines, the subtask headings and the `**Depends on:**`
field, which names ids one by one (`T7.2, T7.3`), never as a range. Fixtures for each case
live in `testdata/`, beside it; `testdata/run.sh` runs them.

`grep -E '^\*\*(Epic [0-9]+[a-z]?|Subtask T[0-9]+\.[0-9]+[a-z]?) estimate:' <epic-doc>` lists the whole estimate.

The plan is edited only when the user asks, through a pull request.

## Step 5 — Write the document

The document follows the instruction that opens the plan's §1.5: **it stands alone for what
to build** (requirements, subtasks, decisions: it never refers to the plan for them or says
"see the plan"), and §2 cites the plan's general rules without copying them: each row
names its rule by number and bold title, and §2 links the plan's §1.5 with a full link (the
plan's path and the section's anchor, as
`rfc-<n>-<slug>-plan.md#15-general-rules-for-each-epic`, beside the epic document, or
its GitHub URL) and the `CLAUDE.md` sections that hold a rule, so
the document reads alone and each rule's text is one click away; and **it records
decisions, not progress** (no status, no checkboxes, no bare "TBD": an answer still pending
is an open point linked as "Ask, don't guess" in the Design principles says; nothing to
keep up to date as the work goes on; it is revised through a pull request when a decision changes). Acceptance criteria
are plain bullets, not checkboxes.

Use this structure. Section numbers are fixed so epic documents can be compared.

```
# Epic <k>: <component> v2 <title>
Header, one field per line: **Sources:** the RFC sections owned, with links.
        **Track:** A, B, C or the team.
        **Epic <k> estimate: <n> subtasks, <d> man-days.**   ← alone on its line, so a
                                                               grep anchored on ^ finds it
        **Depends on:** synchronisation points and epics, naming what each provides.
        **Blocks:** epics, and what they need.

## 1. Goal                     — what exists at the end; "at the end of the epic" bullets
### 1.1 In scope
### 1.2 Out of scope           — and which epic owns each item
### 1.3 Names used here        — a legend, one line each, of the plan's names the document
                                 and its §2 table use: every `E<n>` and `S<n>`, and
                                 role names such as "the integration epic", with what each
                                 is (from the plan's §2 and §3.2)
## 2. How the general rules apply — table: rule | subtasks | how, one row per numbered
                                    §1.5 rule of the plan, cited as "rule N, <title>" and
                                    never copied, with a full link to the plan's §1.5
                                    (Step 5's opening); where CLAUDE.md holds the rule (code
                                    comments and markers, writing tests, verifying a
                                    change), the row links that section. A rule that does
                                    not apply is named, with the reason
## 3. Requirements and invariants — the R-tables from Step 2
## 4. Decisions this epic takes   — what the RFC leaves open, the choice and why; each one
                                    names the `decision:` or `ponytail:` marker it leaves
## 5. Target tree               — `text` block, each entry tagged with its subtask
## 6. Subtasks                  — one `### T<e>.<n> <title>` per subtask (below)
## 7. Order of work             — Mermaid graph of subtask dependencies, and checkpoints
## 8. Suggested pull requests   — Step 5.1
## 9. Open points
```

Each subtask in §6:

```markdown
### T4.2 Held-extent set for one file

**Subtask T4.2 estimate: 2.5 man-days.**

- **Track:** B. **Depends on:** T4.1.
- **Covers:** R1–R6, R13–R16.
- **Deliverables:** the types, functions and files it adds, with signatures where they are
  a contract.
- **Acceptance criteria:** specific, checkable conditions, in terms of the requirements.
- **Tests:** each test, the bug it catches, the layer it runs at (the consumer's, CLAUDE.md
  "Verifying a change"), and each test double by its kind in CLAUDE.md "Writing tests". A
  test that nothing plausible would make fail is not listed.
- **Rules:** how it meets each general rule that applies, unless §2 already says it.
```

Graphs are Mermaid, never ASCII art, styled with stroke only and quoted labels, as the
plan skill describes. Checkpoints are stadium nodes (`(["after T4.3: index API usable"])`).

### 5.1 Suggested pull requests (§8)

Suggest a grouping of the subtasks into pull requests, following "Subtasks and pull
requests" in `.claude/skills/rfc-to-epic-plan/SKILL.md`. It is a suggestion, not an
obligation: the owner may follow it, regroup, or open one pull request per subtask, and the
subtasks stay the unit that is estimated and tracked. Say so in one line under the table.

- **Size:** under the pull-request ceiling in the same instruction. The number of pull
  requests follows from that and from the groupings below.
- **What goes together:** group by logic, readability and dependencies, so that each pull
  request delivers a piece of functionality that can be named. Take consecutive subtasks in
  the order of work. Each pull request builds, passes its tests and leaves nothing
  half-wired; a requirement's test lands in the same pull request as the code that meets
  it. Cut at a checkpoint where you can.
- **Parallel tracks:** two pull requests with no dependency between them can be open at
  once, and the table says so.

```markdown
| PR | Subtasks | Man-days | After | Why together |
| --- | --- | --- | --- | --- |
| 1 | T4.1, T4.2, T4.4 | 7 | — | the container and counters every later subtask builds on |
| 2 | T4.3, T4.5–T4.7 | 8.5 | PR 1 | the file map, markers and floor buckets share the per-file lock |
| 3 | T4.8–T4.12 | 6 | PR 2 | bounds, accounting and rebuild, which read everything above |

A suggestion only: one pull request per subtask works as well.
```

Compute each pull request's man-days from the subtask lines, for example
`grep -E '^\*\*Subtask T4\.(1|2|4)[a-z]? estimate:' <epic-doc> | awk '{d+=$4} END{print d}'`
(the `[a-z]?` keeps split subtasks such as `T4.2a` and `T4.2b` in the sum). The
pull requests' man-days add up to the epic total. Each pull request's description names the
RFC and every subtask it delivers, one id per mention (`RFC 1, T4.1, T4.2`),
and says when it finishes a subtask only in part, so that it can be mapped to the subtasks
it delivers.

## Step 6 — GitHub issues: only when asked

In [`issues.md`](issues.md), beside this file.

## Common rationalizations

| Rationalization | Reality |
| --- | --- |
| "The plan already lists the subtasks" | It lists candidates sized for a plan. Without the requirements next to them, nobody can tell which RFC obligation fell through. |
| "I'll pick out the important MUSTs" | The unimportant-looking ones are the ones nobody tests. Read every owned section in full, note every candidate, and map each one. |
| "This MUST is obvious, it needs no test" | Then the test is cheap. A requirement without a test is a claim, and an enforcer named in its place must exist when this epic lands: read the code. |
| "The estimate is close enough to the plan's" | Then the script will say so. Compute it; don't copy the plan's figure. |

## Verification

Before handing over, check:

- Every candidate of 2.1 (key words, bold lead sentences, invariants, bounds) is mapped,
  and the R-tables carry `Kind`, `Source` and `Subtasks` on every row.
- Every R-number has a subtask and a test that fails on its assertion, or names what already
  enforces it when this epic lands (2.4); every subtask covers an R-number or names the
  subtask that consumes it.
- Design principles: every deliverable names its consumer; each §4 decision cites prior art
  or says none was found. They were applied: the architect proposal was reconciled, the
  three review passes ran and their findings are fixed (or, without agents, ran in sequence
  and are listed as to-dos), the surface was put to the user for approval, and the design questions were
  asked, deferred to the open points of the epic they belong to, or returned as pending.
- Every identifier in an R row is read back in the sections its `Source` cites: list the
  row's backticked names, and `rg -n -w '<name>'` each one in the cited documents, keeping
  only the hits inside the cited sections' line ranges. A name with no hit there is a
  defect to fix before handing over, not to report: add the place it really comes from to
  `Source`, or rewrite the row in the RFC's words. This holds for rows this run did not
  write, too, when revising an existing document.
- No subtask is over the ceiling without a reason, and none was cut to reach a count.
- §6 lists the subtasks in order of work, blockers first, and their numbers ascend in that
  order except for ids already cited, which keep theirs; the order-of-work graph in §7
  matches §6's `**Depends on:**` lines.
- `epic-check.awk` prints only its `TOTAL` line, and the header's `**Epic <k> estimate: …**`
  line says the same; the plan is touched only if the user asked for it.
- §8's suggested pull requests cover every subtask once, their man-days add up to the
  total, each names what it delivers and leaves nothing half-wired, and the table says the
  grouping is only a suggestion.
- The epic's units are called epics and subtasks: no other word (task, story, phase,
  stage, …) names them. Read the document for it; a word search only suggests where to look,
  since phase, stage and plan are also ordinary words and terms of some RFCs.
- The document never refers to the plan for what to build, has no checkboxes or status, and
  its graphs are Mermaid. Its one link to the plan is §2's, to §1.5. `rg -n -i "\b(this|the) plan\b|\bplan's\b" <epic-doc>` shows where to look; read
  each hit, since some RFCs use "plan" as a term of their own.
- §2 has one row per numbered rule of the plan's §1.5, cited by number and title, none
  copied, and its full link to the plan's §1.5 resolves.
- §1.3 explains every `E<n>` and `S<n>` the document uses, §2 included. This
  prints the ones missing from it, and must print nothing. A token that names something
  else, such as the S3 storage service, gets its own line in §1.3 saying so:

  ```bash
  rg -N -o -w 'E[0-9]+[a-z]?|S[0-9]+[a-z]?' <epic-doc> | sort -u |
    comm -23 - <(awk '/^### 1\.3 /{f=1; next} /^#/{f=0} f' <epic-doc> |
                 rg -N -o -w 'E[0-9]+[a-z]?|S[0-9]+[a-z]?' | sort -u)
  ```
- No GitHub issue was created or synced unless the user asked (`issues.md` 6.7 checks the
  rest when they did).

Then report the output path, the requirement count, the subtask count and the man-day total
(and the plan's figure for comparison, or "no plan figure"), any Step 1 fallback used,
the suggested pull requests, the subtasks whose
estimate is least certain, and the open points, in a few lines; then, at whatever length
they take, the target tree and every exported symbol signature (Design principles), the
review passes still to run independently, if any, and the questions pending the user's
answer. End with one line
reminding the user to create the GitHub issues with Step 6 (`issues.md`) once the epic
document is approved and its pull request has merged; for a revision of an epic that already has
issues, the line reminds them instead that the issues need syncing once the revision
merges. The line names Step 6 as the next step for the user to ask for; it does not offer
to run it now.
