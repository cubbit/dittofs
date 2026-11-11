# Epic breakdown: GitHub issues

Step 6 of the `epic-breakdown` skill (`SKILL.md`, beside this file).

## Step 6 — GitHub issues: only when asked, as pointers to the documents

**Do not create issues unasked.** The user asks for this step when the team is ready to
assign work.

This step is the **one description** of the RFC's issues: roadmap epics, epic issues and
subtask issues. `rfc-to-epic-plan` sends its issue requests here. Issues never copy the
specification: a body is a pointer to a section of the plan or of the epic document, with
a line or two of context, so revising a document never leaves an issue saying something
else.

Three requests reach this step:

- **a roadmap epic** for an RFC and a release (6.2);
- **epic issues for a merged plan**, before the epic documents exist (6.3). E2 needs them:
  each red test's `known.yaml` entry names the issue of the epic that turns it green, so
  every epic of the plan has its issue before E2's pull request lands;
- **an epic's issues once its document has merged**: the epic issue (6.3) and one issue per
  subtask (6.4).

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
   carries `roadmap` and the RFC's label, never `epic` or an epic's label. Check with
   `gh label list -R "$R" --search rfc-<n>` and create what is missing:
   ```bash
   gh label create -R "$R" rfc-<n>-<slug> --color 5319E7 --description "RFC <n> <title>"
   gh label create -R "$R" rfc-<n>-epic-<k> --color C5DEF5 --description "RFC <n> <component> v2, epic <k>: <title>"
   ```
5. **Every link resolves.** Link a document on a branch that holds it, which is not always
   `develop` (a programme branch such as `feat/storage-v2` holds documents `develop` does not
   have yet). Check each file before writing its link:
   `gh api "repos/$R/contents/<path>?ref=<branch>" --silent`. A 404 means linking another
   branch, never writing a dead link. Issues pointing at an epic document are created only
   **once its pull request has merged**, never during the breakdown.

### 6.2 The roadmap epic: the only place a release is chosen

A **roadmap epic** says what of one RFC ships in one milestone. It collects the RFC's epic
issues for that release as sub-issues, and it is the milestone's default for the RFC: a unit
of the RFC with no epic issue of its own ships with it, and the burnup reads it that way. An
RFC normally has one; an RFC split across releases has one per release. **It exists before
any epic issue of the RFC**, because epic issues are created under it. Epic issues never
choose a milestone: they copy their roadmap epic's, and their subtask issues copy the same
one. No document names a release.

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

  The RFC's epic issues for this release are sub-issues of this one, and each carries this milestone. A unit of the RFC with no epic issue of its own ships with this roadmap epic. The plan and the epic documents say what to build; the milestone is the only place that says when.
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
**Depends on:** epics <j> and <k> (from the plan's §3.3). **Blocks:** epics <m>, ….
```

**Once the epic document has merged:**
```markdown
Epic <k> of <component> v2 (RFC <n>): <§1's goal, in one sentence>.

**Specification:** [Epic <k>: <title>](https://github.com/<R>/blob/<branch>/docs/internals/planning/<file>.md)
**Sources:** RFC <n> §…; **Track:** <A|B|C>.
**Depends on:** epics <j> and <k> (or a sync point, as `S2`, or "every earlier epic").

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

Then attach it under its roadmap epic as a sub-issue (6.5).

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
withdrawn subtask, or a split subtask's old issue), and then it is closed, as 6.6 says. Sub-issues that did not
come from the epic document (bugs, follow-ups) are not its subject: they are the team's.
A matched older issue keeps its title form;
if its body copies the specification (`Covers:`, `Done when`, a checklist), it is rewritten
to the pointer form below.

Title `<component> v2, subtask T<e>.<n>: <title>`. Body:
```markdown
Subtask T<e>.<n> of epic <k> (#<epic-number>): <the subtask's purpose, in one sentence>.

**Specification:** [T<e>.<n> <title>](<document link>#<heading anchor>)
**Depends on:** #<subtask-number>, … (or the sync point, as `S2`).
```
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
  "DittoFS" project). Only subtask issues get `Estimate`, their man-days from §6:
  a figure on the epic or the roadmap epic too would count it twice in the project's sums.
  In a new shell, set `R`, `owner`, `num`, `project` and `field` again as 6.1 does.
  ```bash
  item=$(gh project item-add "$num" --owner "$owner" --url <issue-url> --format json --jq .id)
  gh project item-edit --project-id "$project" --id "$item" --field-id "$field" --number <man-days>
  ```

### 6.6 When a document is revised, or an epic changes release

- The issues follow a revision **after its pull request has merged**, on request, never in
  the same pass: until then the links still show the old document. The sync covers a renamed
  subtask's title, a changed estimate's field, a new subtask's issue, a withdrawn subtask's
  issue closed as not planned, a split subtask (T4.3 into T4.3a and T4.3b: new issues, the
  old one closed), and a `**Depends on:**` line the revision changed. That line is the one
  part of a body that copies the documents, kept because release tracking reads it: an epic
  whose milestone is due before that of an epic it depends on is a conflict to report.
  Otherwise bodies need no change, because they say nothing the document can contradict.
- An epic changes release only by moving under the other release's roadmap epic (the user
  creates it if the RFC has none there yet); then update the milestone copy on the epic
  issue and its open sub-issues. Closed ones keep the release they shipped in.

### 6.7 Report

The issue links, each epic's roadmap epic, the subtask numbers, any body left as it was,
and any epic placed in an earlier release than an epic it depends on. Check the result with
`gh api --paginate "repos/$R/issues/<epic-number>/sub_issues" --jq '.[].title'`.

Before reporting, check each rule of 6.1–6.5 against what was created.

### 6.8 Where a subtask id is cited

Before renumbering anything (`SKILL.md`, "A cited id is fixed"), look for citations of both
forms, with the repository named as 6.1 says:

- pull requests, since they cite ids in their descriptions too, discarding the hits that only
  cite an RFC's §<e>.<n>: `gh pr list -R "$R" --state all --search '"T<e>.<n>" in:title,body'`
  and `'"<e>.<n>" in:title,body'`;
- the sub-issues of every part's epic issue (6.4);
- the other planning documents, leaving out this document and the plan:
  `rg -n 'T<e>\.<n>\b' docs/internals/planning -g '!<plan>' -g '!<this-doc>'`.
