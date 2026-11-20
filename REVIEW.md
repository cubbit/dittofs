# How we review PRs

This file is the contract for every reviewer of this repository, human or
agent, whatever workflow you drive (Copilot, a review council, ad-hoc reads).
Your own workflow decides *how* you review; this file decides *what may block
a merge and what may not*. How to write a comment, its tags and the review
body, is in `.github/instructions/pr-commenting.instructions.md`.

## 1. What a review is checked against

A point may block only against the repository's own record. Cite the file and
line:

- **`docs/internals/rfcs/*.md`**: the RFCs are the design authority. When a
  PR changes behavior or a ruling, the RFC sections it touches are in scope,
  even if the PR does not edit them.
- **`.planning/*.md`**: the implementation plans and epic breakdowns, and
  once a plan's issues exist, those issues. "The plan says X" names the file
  and line; a plan in your head is not a plan.
- **`CLAUDE.md` and `.github/instructions/*.md`**: the repo's conventions.
  The test rules (no mocks, no sleeps, no fixed ports, a test that can fail,
  fuzzed decoders), the comment rules and the `decision:` and `ponytail:`
  markers are rules, not preferences.
- **The code** on the PR's base branch.

If two of these disagree, that disagreement is the finding: cite both. The
fix is the smallest edit (§6), not a preference between them.

## 2. What may block

A point blocks only when it is tagged `issue (blocking)` and shows one of
these two, *with the evidence quoted*:

1. **A contradiction**: the PR says or does something a source in §1 does
   not. Quote the passage, or name the code path, beside your point.
2. **A break**: lost data, a crash, a security hole, a race, or a red test.
   Name the failing path. A test the reviewer wrote that fails on the PR's
   head is a break, and it blocks.

Mark how you know, as `pr-commenting.instructions.md` asks: "ran it", "read
the code" or "I think". A point that rests on "I think" or "not measured" is
a question, not a blocking point, until someone runs it.

Everything else is a **suggestion**: wording, structure, a nicer shape, a
test or metric that no rule in §1 requires, a subtask someone would split
differently. Suggestions go in the review body and into a follow-up issue
(§4). They never block a merge.

## 3. Rounds: review the delta, not the PR again

- An approval is a checkpoint. The same reviewer's next round covers only
  `git diff <approved-sha>..HEAD`. A point on text outside that diff is a
  follow-up issue, even when it is a contradiction. Only a break (§2.2) found
  outside the delta may block.
- If you approved text and now find a break in it, say so in those words:
  "my approval at `<sha>` missed this". The break blocks; nothing else
  reopens.
- A round that brings no new blocking point means the review is done:
  approve.
- Cap: two reviewed heads per reviewer before that reviewer approves or the
  PR goes to standup. A third round needs a standup decision, not another
  pass.

## 4. Reviews never add scope to an open PR

A requirement, test, metric or subtask raised in review, even a good one, is
not part of the PR under review unless a rule in §1 already requires it. It
becomes a follow-up issue labelled `deferred` and with the RFC's or epic's
label (for example `rfc-20-epic-6`), and standup prioritizes it like all
other work. A plan's estimate stays as merged; scope a review adds is
reported on its own line, and only standup moves the estimate.

If the new point is big enough to change the plan, it is not review
feedback: give it its own planning PR.

## 5. Silence belongs to the author

Where the sources in §1 are silent, the author's choice stands. A reviewer
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
next round; a reviewer does not block on a sibling the PR does not touch.
That drift is a follow-up issue.

## 7. Follow-ups need an owner

Standup triages every follow-up issue: keep, drop or schedule. The backlog
is reviewed weekly. An issue leaves only by a recorded decision to drop it,
or through the PR that does the work, as `CLAUDE.md` says for deferral
ledgers; nothing is closed by age alone.
