# How we review PRs

This file is the contract for every reviewer of this repository, human or
agent, whatever workflow you drive (Copilot, a review council, ad-hoc reads).
Your own workflow decides *how* you review; this file decides *what may block
a merge and what may not*. How to write a comment, its tags and the review
body, is in `.github/instructions/pr-commenting.instructions.md`.

## 1. What a review is checked against

A point may block only against the repository's own record, read on the PR's
base branch. A passage the PR itself edits is under review, not authority for
it. Cite the file and line:

- **`docs/internals/rfcs/*.md`**: the RFCs are the design authority. When a
  PR changes behavior or a ruling, an RFC section it makes false is in
  scope, even if the PR does not edit it.
- **`.planning/*.md`**: the implementation plans and epic breakdowns, and
  once a plan's issues exist, those issues. "The plan says X" names the file
  and line; a plan in your head is not a plan.
- **`CLAUDE.md`, `pr-opening.instructions.md` and
  `pr-commenting.instructions.md`**: the repo's conventions. `CLAUDE.md`'s
  "Writing tests" and "Code comments" sections are rules, not preferences.
  A checklist that guides a review, such as `pr-commenting`'s "What to
  check", is not a rule. `copilot.instructions.md` guides Copilot and is not
  a source.
- **The protocol specifications** the code implements (RFC 1813, RFC 7530,
  RFC 8881, RFC 7862, MS-SMB2, MS-FSCC and the like): a reply that differs
  from the section it implements is a contradiction. Quote the section.
- **The code** on the PR's base branch. For a stacked PR, that is the branch
  below it (`pr-opening.instructions.md`, "Stack or not").

If the PR makes two of these disagree, that disagreement is the finding:
cite both. The fix is the smallest edit (§6), not a preference between them.
A disagreement already on the base is a follow-up issue (§4).

## 2. What may block

A point blocks only when it is tagged `issue (blocking)` and shows one of
these two, *with the evidence quoted*:

1. **A contradiction**: the PR says or does something a source in §1 does
   not. Quote the passage, or name the code path, beside your point.
2. **A break**: lost data, a crash, a security hole, a race, or a red test
   that is not a registered known failure (`CLAUDE.md`, "Writing tests").
   Name the failing path. A test the reviewer wrote is a break when it fails
   on the PR's head, checks what a §1 source or the PR itself claims, and
   either passes on the base or checks a fix the PR claims to make.
   A security hole, or a way to take a node down or exhaust its resources,
   names only its class in a public thread. Its path and evidence go to a
   maintainer privately, as `docs/guide/security.md` asks, never into a public
   issue. It still blocks.

Only `issue (blocking)` blocks. Other tags in use, such as `must:`,
`should:` or `[important]`, do not. A reviewer who means to block uses
`issue (blocking)`.

Mark how you know, as `pr-commenting.instructions.md` asks: "ran it", "read
the code" or "I think". A point that rests on "I think" or "not measured" is
a question, not a blocking point, until someone runs it. A bot that posts
directly, such as Copilot code review, is named by its account; a review a
person posts is that person's (`pr-commenting.instructions.md`, "A human owns
the output"). Either review's blocking points are judged like anyone's, by
this section.

Everything else is a **suggestion**: wording, structure, a nicer shape, a
test or metric that no rule in §1 requires, a subtask someone would split
differently. Post it as `pr-commenting.instructions.md` says. The author
decides: apply it in the PR, or leave it. A suggestion the author leaves
and the reviewer still wants becomes a follow-up issue (§4). A suggestion
never blocks a merge.

A file the PR only moves or renames is reviewed as a move, not as new text.
Check with `git diff -M --color-moved` that its content did not change, and
that every reference to the old path follows it.

## 3. Rounds: review the delta, not the PR again

- An approval is a checkpoint. The approved SHA is the commit your Approve
  review names on GitHub. Your next round covers only what changed since:
  `git diff <approved-sha>..<pr-head>`. If the PR was rebased since, use
  `git range-diff <approved-base>..<approved-sha> <base>..<pr-head>`, where
  `<approved-base>` is `git merge-base <base> <approved-sha>`. The base's own
  commits then stay out of the delta. Outside the delta, two things may
  still block: a break (§2.2), and a contradiction the delta creates,
  wherever its other side lives. Any other point outside the delta is a
  follow-up issue.
- The branch protection requires an approval of the last push and resolved
  conversations. A push after an approval needs a new approval, and an open
  thread holds the merge, even with auto-merge armed. The delta round
  therefore happens before the merge, not after it.
- If you approved text and now find a break in it, say so in those words:
  "my approval at `<sha>` missed this". The break blocks; nothing else
  reopens.
- When no blocking point is open, old or new, and none of your questions is
  open, the review is done: approve. If a question is still open, comment
  instead, as `pr-commenting.instructions.md` says under "Review body".
- Cap: two reviewed heads per reviewer. After that, the author and that
  reviewer settle what is left in a quick live review, as
  `pr-commenting.instructions.md` proposes. What the live review does not
  settle goes to standup. The same reviewer reviews the head that carries
  what they settled, as a delta limited to the settled points, and approves
  it.

## 4. Reviews never add scope to an open PR

A requirement, test, metric or subtask raised in review, even a good one,
cannot be required of the PR under review unless a rule in §1 already
requires it. It becomes a follow-up issue labelled `follow-up` and with the
RFC's or epic's label (for example `rfc-20-epic-6`). The reviewer who raised
the point files it before approving and links it in the thread. An agent
reviewer drafts it for the person who runs it, who files it. Standup
prioritizes it like all other work. If the author folds such a point in, the
PR reports it on its own line. A plan's estimate stays as merged, and only
standup moves it.

If the new point is big enough to change the plan, it is not review
feedback: give it its own planning PR.

## 5. Silence belongs to the author

Where the sources in §1 are silent, the author's choice stands, unless it is
a break (§2.2). A break blocks whether or not a source covers it. A reviewer
who thinks the choice hides a real gap shows the gap. The gap then gets
**its own document PR** (a ruling). It is not folded into the open PR, and it
does not block that PR.

## 6. Doc drift blocks, cheaply

In this repository most PRs amend the RFCs and plans alongside the code, so
"the text says something the code does not" is a contradiction under §2, and
the most common blocking finding we have. Its fix is the **smallest edit**
that aligns the sentence with the code or the sibling plan, never a
redesign. After an approval, §3 limits it to the delta.

Authors: when a PR writes a new ruling into prose, update in the same commit
every sibling that cites the affected passage, such as the epics, the
program plan's ledger rows and a replaced rule's citations. This saves the
next round. A reviewer blocks on a sibling the PR makes false, edited or
not. Wording that was already stale on the base is a follow-up issue.

An author's fix answers the finding and adds no new claims. Edit only what
the thread asks, and cite a source instead of restating it. A new claim goes
to a new PR, or is reviewed as new scope.

## 7. Follow-ups need an owner

Standup triages every follow-up issue: keep, drop or schedule. The backlog
is reviewed weekly. An issue leaves only by a recorded decision to drop it,
or through the PR that does the work, as `CLAUDE.md` says for deferral
ledgers; nothing is closed by age alone.
