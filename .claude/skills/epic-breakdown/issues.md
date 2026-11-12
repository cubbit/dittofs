# Epic breakdown: GitHub issues

Step 6 of the `epic-breakdown` skill (`SKILL.md`, beside this file).

## Step 6 — GitHub issues: at each merge, as pointers to the documents

Issues come at fixed points of the planning lifecycle (the wiki's "From RFC to plan to
epics"): the RFC's roadmap epic before its plan, the epic issues once the plan merges, and
an epic's subtask issues once its epic document merges. Create them when the user asks at
one of those points, never while the document is still in review.

**Once an issue exists, it holds the figures.** Its `Estimate` and its blocked-by links are
the current estimate and dependencies; the documents keep the first ones and are never
revised to follow the issues. The burn-up reads them in that order: a subtask issue, then
its epic issue, then the epic document, then the plan. An issue with no blocked-by links
defers to its document: absent links mean "not stated here", never "depends on nothing".

**The documents' dependencies stay required, and must be right.** The plan's §2 Blocks
column and §3, the epic document's header **Depends on:** line and each subtask's in §6 are
what the blocked-by links are created from, and what the burn-up reads for an issue that has
none. An issue's body does not copy them; the links carry them.

This step is the **one description** of the RFC's issues: roadmap epics, epic issues,
subtask issues and deferral-ledger issues. `rfc-to-epic-plan` sends its issue requests here.
Issues never copy the specification: a body is a pointer to a section of the plan or of the
epic document, with a line or two of context, so revising a document never leaves an issue
saying something else.

Four requests reach this step:

- **a roadmap epic** for an RFC and a release (6.2);
- **epic issues for a merged plan**, before the epic documents exist (6.3). E2 needs them:
  each red test's `known.yaml` entry names the issue of the epic that turns it green, so
  every epic of the plan has its issue before E2's pull request lands;
- **an epic's issues once its document has merged**: the epic issue (6.3) and one issue per
  subtask (6.4);
- **a merged plan's deferral-ledger issues**, one per row of the plan's §6 ledger (6.6).

### 6.1 Before creating anything

Do all of these first. Each one stops the step rather than letting it create half the issues.

1. **Name the repository on every call.** `gh issue list`, `gh pr list --search` and
   `gh label list` build their query from the local remote, and a clone whose `origin` still
   names an old repository gets `[]` back with no error. Resolve it once and pass it
   everywhere:
   ```bash
   R=$(gh repo view --json nameWithOwner -q .nameWithOwner)
   gh issue list -R "$R" --state all --label roadmap --json number,title,milestone
   ```
   `gh api` calls use `repos/$R/…`. **An empty lookup is not evidence**: that last command
   must list the roadmap epics the programme already has. If it prints `[]`, stop and report
   it rather than conclude that nothing exists.
2. **Check the token's `project` scope, and find the board.** Run `gh auth status`; if
   `project` is missing, stop and ask the user to run `gh auth refresh -s project`. Then look
   up the repo's project board (for example the cubbit "DittoFS" project) by title rather
   than by number, and its `Estimate` field, never hard-coding an id:
   ```bash
   owner=${R%%/*}
   num=$(gh project list --owner "$owner" --format json \
     --jq '.projects[] | select(.title == "<project title>") | .number')
   project=$(gh project view "$num" --owner "$owner" --format json --jq .id)
   field=$(gh project field-list "$num" --owner "$owner" --format json \
     --jq '.fields[] | select(.name == "Estimate") | .id')
   ```
   An account outside the org gets an empty list with no error. Stop and ask if `num` is not
   exactly one number (no board, or more than one, matches) or `field` is empty.
3. **Find what exists, before deciding what to create:** the RFC's roadmap epics (6.2), the
   epic issue (`gh issue list -R "$R" --state all --label epic --label rfc-<n>-epic-<k>`;
   both labels must match, since a text search for "epic <k>" returns other epics), and its
   sub-issues (6.4).
4. **Labels.** The epic issue carries `epic`, the RFC's label (`rfc-<n>-<slug>`, the slug
   being the RFC file's, `rfc-17-vfs` for `rfc-17-vfs.md`) and the epic's label
   (`rfc-<n>-epic-<k>`, with `<k>` keeping its letter: `rfc-1-epic-7a`, never the
   unsplit epic's `rfc-1-epic-7`). Subtask issues carry the last two, **not** `epic`. A roadmap epic
   carries `roadmap` and the RFC's label, never `epic` or an epic's label. A deferral
   ledger row's issue carries `deferred` and the RFC's label. Check with
   `gh label list -R "$R" --search rfc-<n>` (and `--search deferred`) and create what is missing:
   ```bash
   gh label create -R "$R" rfc-<n>-<slug> --color 5319E7 --description "RFC <n> <title>"
   gh label create -R "$R" rfc-<n>-epic-<k> --color C5DEF5 --description "RFC <n> <component> v2, epic <k>: <title>"
   gh label create -R "$R" deferred --color D4C5F9 --description "Deferred by a plan's deferral ledger"
   ```
5. **Every link resolves.** Link a document on a branch that holds it, which is not always
   `develop` (a programme branch such as `feat/storage-v2` holds documents `develop` does not
   have yet). Check each file before writing its link:
   `gh api "repos/$R/contents/<path>?ref=<branch>" --silent`. A 404 means linking another
   branch, never writing a dead link. Issues pointing at an epic document are created only
   **once its pull request has merged**, never during the breakdown.

### 6.2 The roadmap epic: the only place a release is chosen

A **roadmap epic** says what of one RFC ships in one milestone. It collects the RFC's epic
issues for that release as sub-issues, with the deferral-ledger issues due in it (6.6), and it
is the milestone's default for the RFC: a unit of the RFC with no epic issue of its own ships
with it, and the burnup reads it that way. An RFC normally has one; an RFC split across
releases has one per release. **It exists before any epic issue of the RFC**, because epic
issues are created under it. Epic issues never choose a milestone: they copy their roadmap
epic's, and their subtask issues copy the same one. No document names a release, except a
deferral-ledger row that says `post-v1` (6.6).

**Finding it**, for an epic issue: `gh issue list -R "$R" --state all --label roadmap --label
rfc-<n>-<slug> --json number,title,state,milestone`. One open → use it, without asking.
Several (an RFC split across releases) → **ask the user** which, since that is the decision
of when the epic ships. None → **stop** and ask the user for one first; do not pick a
milestone instead.

**Creating it**, only when the user asks for a roadmap epic. Look first with the same
command: if one exists for that milestone, use it and create nothing, and if it is closed,
ask before reopening it. A second one for one RFC and milestone would make the RFC read as
split across releases.

- Title `roadmap <milestone>: RFC <n> — <RFC title>`, the title as the RFC's `#` heading
  writes it (`roadmap v1.0: RFC 17 — the filesystem service (VFS)`). One RFC per roadmap
  epic.
- The milestone: ask the user, never guess.
- Body, and nothing more:
  ```markdown
  Roadmap epic: what of RFC <n> — <title> ships in <milestone>.

  **In this release:** the whole RFC. | the whole RFC except epics <k>, …, which ship in <later milestone> (#<roadmap>). | epics <k> (<title>), …; the rest of the RFC ships in <milestone> (#<roadmap>).
  **RFC:** [`rfc-<n>-<slug>.md`](https://github.com/<R>/blob/<branch>/docs/internals/rfcs/rfc-<n>-<slug>.md)
  **Plan:** [`<plan>.md`](https://github.com/<R>/blob/<branch>/docs/internals/planning/<plan>.md) | none yet.

  The RFC's epic issues for this release, and its deferred items due in it, are sub-issues of this one, and each carries this milestone. A unit of the RFC with no epic issue of its own ships with this roadmap epic. The plan and the epic documents say what to build; the milestone is the only place that says when.
  ```
- Add it to the project (6.5) with no `Estimate`.

### 6.3 The epic issue

One per epic, never two: if 6.1 found one, update it rather than create. Title
`<component> v2, epic <k>: <title>` (as in `journal v2, epic 4: placement index and
versions`). Body, and nothing more, in one of two forms.

**Before the epic document exists** (a merged plan, for every epic that has no issue):
```markdown
Epic <k> of <component> v2 (RFC <n>): <the epic's goal, in one sentence>.

**Spec:** [Epic <k> — <title>](<plan link>#<heading anchor>). No epic document yet.
```

**Once the epic document has merged:**
```markdown
Epic <k> of <component> v2 (RFC <n>): <§1's goal, in one sentence>.

**Specification:** [Epic <k>: <title>](https://github.com/<R>/blob/<branch>/docs/internals/planning/<file>.md)
**Sources:** RFC <n> §…; **Track:** <A|B|C>.

The document is the specification: requirements, subtasks, acceptance criteria and
tests live there, not here. Each subtask is a sub-issue.
```

An existing epic issue whose body is still the first form, or holds no more than a line or
two of context, is rewritten to the second form when its document lands. Lines that place
it in the programme (`Part of #3119, stage 3.`, `Plan: #3546.`) are kept. A body someone
has written into is left as it is, and reported.

```bash
gh issue create -R "$R" --title "<component> v2, epic <k>: <title>" \
  --label epic,rfc-<n>-<slug>,rfc-<n>-epic-<k> --milestone <roadmap's milestone> --body-file <body.md>
```

Then attach it under its roadmap epic as a sub-issue, and give it its `Estimate` and its
blocked-by links (6.5). From the plan, it is blocked by every epic whose cell in §2's
Blocks column names it: that column lists the epics a row blocks, so read it in reverse
(epic 3's row saying `4, 8` makes epics 4 and 8 blocked by epic 3, never epic 3 blocked by
them), and `everything` or `all` names every later epic; §3 gives the reasons. Once its
document has merged, it is blocked by the epics the document's header **Depends on:** line
names, where "every earlier epic" is a link to each of them.

### 6.4 One issue per subtask

**Match the existing sub-issues first**, of this epic's issue and, for a split epic, of
every other part's, since a subtask can move between parts (`--paginate`, since the endpoint returns 30 per page):
```bash
gh api --paginate "repos/$R/issues/<epic-number>/sub_issues" --jq '.[] | "\(.number) \(.state) \(.title)"'
```
A sub-issue matches a subtask by its id, in any of the three title forms in use, each with
its boundary: `subtask T6.1:` (this skill's), `epic 6.1:` (issues from before subtask ids
carried the `T`, as `journal v2, epic 4.1: …`) and `(T6.1)` (as `engine v2, epic 6: the
store's lifecycle view (T6.1)`):
```text
(subtask T6\.1:|epic 6\.1:|\(T6\.1\))
```
The boundary matters: a bare `epic 4.1` also matches 4.10 to 4.19. **If the epic has
sub-issues but none matches any subtask, stop and report the titles**: a fourth title form is
more likely than an epic whose every issue is missing. Create issues only for the subtasks
with no match. A match under another part's epic issue is moved under this part's, never
duplicated. A subtask issue is obsolete only when no part's document has its id (a
withdrawn subtask, or a split subtask's old issue), and then it is closed, as 6.7 says. Sub-issues that did not
come from the epic document (bugs, follow-ups) are not its subject: they are the team's.
A matched older issue keeps its title form;
if its body copies the specification (`Covers:`, `Done when`, a checklist), it is rewritten
to the pointer form below.

Title `<component> v2, subtask T<e>.<n>: <title>`. Body:
```markdown
Subtask T<e>.<n> of epic <k> (#<epic-number>): <the subtask's purpose, in one sentence>.

**Specification:** [T<e>.<n> <title>](<document link>#<heading anchor>)
```
Its dependencies are blocked-by links (6.5), from the subtask's **Depends on:** line in §6:
a subtask names that subtask's issue, a whole epic its epic issue, and a sync point the
issues of the subtasks it lists.
The anchor is GitHub's for the `### T<e>.<n> <title>` heading: lower case, dots and
punctuation other than hyphens dropped, spaces as hyphens (`### T3.1 Usage table` →
`#t31-usage-table`). Open one link to check it. Requirements, acceptance criteria, tests and
the estimate are not copied: the document holds the first three, the project's field the
last. Create it with the milestone of the roadmap epic the epic sits under, also when the
subtasks come long after the epic issue.

### 6.5 Sub-issues and the project

- **Sub-issue links**, not checklists in a body: a subtask under its epic, an epic under its
  roadmap epic. The API wants the child's numeric `id`, not its number:
  ```bash
  id=$(gh api "repos/$R/issues/<child-number>" --jq .id)
  gh api -X POST "repos/$R/issues/<parent-number>/sub_issues" -F sub_issue_id="$id"
  ```
  Moving an issue that already has a parent (6.4's match under another part's epic issue)
  needs `-F replace_parent=true` on the same call; without it the API refuses the child.
- **Every issue joins the repo's project board** that 6.1 found (for example the cubbit
  "DittoFS" project). Epic and subtask issues get `Estimate`: a subtask its man-days from
  §6; an epic the plan's figure (none while it is "planned after Sx"), replaced by the
  epic document's total, the sum of its subtasks, when that document merges. The roadmap
  epic gets none. The project's sums then count an epic's work at both levels, so sum one
  level at a time.
  In a new shell, set `R`, `owner`, `num`, `project` and `field` again as 6.1 does.
  ```bash
  item=$(gh project item-add "$num" --owner "$owner" --url <issue-url> --format json --jq .id)
  gh project item-edit --project-id "$project" --id "$item" --field-id "$field" --number <man-days>
  ```
- **Blocked-by links**, created from the documents' dependencies (6.3, 6.4) and never
  written as a **Depends on:** line in the issue's body: an epic issue is blocked by other
  epic issues, a subtask issue by subtask issues, or by an epic issue when it waits on a
  whole epic. The API also wants the blocker's numeric `id`:
  ```bash
  id=$(gh api "repos/$R/issues/<blocker-number>" --jq .id)
  gh api -X POST "repos/$R/issues/<blocked-number>/dependencies/blocked_by" -F issue_id="$id"
  ```
  Two epics the plan has blocking each other (`9a` blocks `7a` for T9.1 alone) are never
  linked both ways: link the subtask that waits, once its issue exists.

### 6.6 Deferral-ledger issues

Asked for a merged plan's deferral ledger (§6 of the plan), one issue per row that has
none yet (step 1), never before the plan has merged. Only the ledger is read: a deferral that
lives only in an epic document gets no issue until it has a row (`SKILL.md`, §1.2).

Ledger text is never pasted inline into a shell command: its cells carry apostrophes and
backticks, which a shell quote ends or runs.

1. **Search first.** List the `deferred` issues once and match the rows against the file:
   ```bash
   gh issue list -R "$R" --state all --label deferred --limit 1000 --json number,title,body > deferred.json
   ```
   A row has an issue when a `deferred` issue's body links it. One filed by another plan's row
   for the same item gains a second **Row:** line, not a second issue.
   For a row with no match, look for one filed before the ledger with a search on a few plain
   words of the item, typed rather than copied (`--search 'read-only journal in:title,body'`).
2. **Title and body.** The title is the Deferred cell, shortened. The body is a link to the
   row and one sentence on what waits, nothing more; Why, Owner and Trigger stay in the row,
   so a revised row leaves no issue saying something else. The plan never lists the issue
   numbers: the issue links the row, not the row the issue:
   ```markdown
   Deferred by the RFC <n> plan: <what waits, in one sentence>.

   **Row:** [<plan>.md §6, deferral ledger](<plan link>#<ledger heading anchor>)
   ```
   The title goes through a quoted heredoc and the body through a file:
   ```bash
   title=$(cat <<'EOF'
   <the title>
   EOF
   )
   gh issue create -R "$R" --title "$title" --label deferred,rfc-<n>-<slug> \
     --milestone <roadmap's milestone> --body-file <body.md>
   ```
3. **Labels** `deferred` and the deferring RFC's label (6.1 point 4); never `epic`.
4. **Parent and release.** A sub-issue (6.5) of the deferring RFC's roadmap epic, whose
   milestone it copies: a deferred item is due in the deferring RFC's release unless the row
   says otherwise. A later release only when the row's Trigger says so (`post-v1`) or the user
   decides, never on the agent's judgement; the issue then goes under the deferring RFC's
   roadmap epic for that release. Finding it is 6.2's: several open, or none, → stop and ask.
   Add it to the project (6.5) with no `Estimate`.
5. **Closing.** An issue closes when the work it names merges, through `Closes #<number>` in
   the pull request that finishes it, never by an agent unasked, and not when the owner's plan
   merges. A row a revision drops, by a recorded decision, closes its issue as not planned,
   once the revision has merged.

### 6.7 When a document is revised, or an epic changes release

- **During the work, changes are made on the issues alone**, with no pull request: a new
  subtask is a new sub-issue with its `Estimate` (titled `subtask T<e>.<n>:` with the next
  free id), a split one gets new issues (`T4.3a`, `T4.3b`) and its old one is closed as not
  planned, a withdrawn one is closed as not planned, and a changed estimate or dependency is
  a changed `Estimate` or blocked-by link. The epic's own `Estimate` then drifts from the sum
  of its subtasks, and the documents from both: expected, and not corrected.
- A document revision that does merge is synced to the issues **after its pull request has
  merged**, on request, never in the same pass: until then the links still show the old
  document. The sync covers a renamed subtask's title, a changed estimate's field, a new
  subtask's issue, a withdrawn subtask's issue closed as not planned, a split subtask, and
  the blocked-by links of a dependency the revision changed. An epic whose milestone is due
  before that of an epic it depends on is a conflict to report. Bodies need no change,
  because they say nothing the document can contradict. An older body's `**Depends on:**`
  line is read only while its issue has no blocked-by links.
- An epic changes release only by moving under the other release's roadmap epic (the user
  creates it if the RFC has none there yet); then update the milestone copy on the epic
  issue and its open sub-issues. Closed ones keep the release they shipped in.
- A deferral-ledger issue sits under the roadmap epic, not under an epic, so an epic's move
  leaves it where it is. It changes release only as 6.6 point 4 says: it moves under the other
  release's roadmap epic and copies its milestone.

### 6.8 Report

The issue links, each epic's roadmap epic, the subtask numbers, any body left as it was,
and any epic placed in an earlier release than an epic it depends on. Check the result with
`gh api --paginate "repos/$R/issues/<epic-number>/sub_issues" --jq '.[].title'`.

Before reporting, check each rule of 6.1–6.6 against what was created.

### 6.9 Where a subtask id is cited

Before renumbering anything (`SKILL.md`, "A cited id is fixed"), look for citations of both
forms, with the repository named as 6.1 says:

- pull requests, since they cite ids in their descriptions too, discarding the hits that only
  cite an RFC's §<e>.<n>: `gh pr list -R "$R" --state all --search '"T<e>.<n>" in:title,body'`
  and `'"<e>.<n>" in:title,body'`;
- the sub-issues of every part's epic issue (6.4);
- the other planning documents, leaving out this document and the plan:
  `rg -n 'T<e>\.<n>\b' docs/internals/planning -g '!<plan>' -g '!<this-doc>'`.
