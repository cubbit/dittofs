# How we review PRs

This file is the contract for every reviewer of this repository — human or
agent, whatever workflow you drive (Copilot, a review council, ad-hoc reads).
Your own workflow decides *how* you review; this file decides *what may block
a merge and what may not*. If a review round deviates from it, the deviation
is the problem, not the PR.

## 1. What a review is checked against

A review is a diff read against the repository's own record, not against any
one reviewer's memory of it. Before judging a point, open the sources:

- **`docs/internals/rfcs/*.md`** — the RFCs are the design authority. When a
  PR changes behavior or a ruling, the relevant RFC sections are in scope of
  the review, even if the PR itself doesn't touch them.
- **`.planning/*.md`** — the implementation plans and epic breakdowns. A point
  that "the plan says X" must name which plan file and line; a plan in your
  head is not a plan.

The PR diff alone is not the review's only input, but neither is the wider
repo an invitation to re-review it: the sources above are what a point may
be *blocking* against. If two documents disagree, that disagreement is the
finding — cite both, and the fix is the smallest edit (§6), not a preference
between them.

## 2. What may block

A point blocks only when it shows one of the two, *with the evidence quoted*:

1. **A contradiction** — the PR says something the RFC, the plan or the code
   does not. Name the contradicting passage (RFC or `.planning/` file and
   line, or the code path) beside your point.
2. **A break** — lost data, a crash, a security hole, a red test. Name the
   failing path.

Everything else a reviewer notices — wording, structure, a nicer shape, a
missing test or metric someone wishes existed, a subtask someone would have
done differently — is a **suggestion**. Suggestions go in the review body and
into a follow-up issue. They never block, and they never gate a merge.

A reviewer who can't quote the contradicting line or name the failing path
doesn't have a blocking point yet.

## 3. Rounds: review the delta, not the PR again

- An approval is a checkpoint. The round after it covers **only what changed
  since that approval** — diff against the approving SHA. A new point on text
  that the approval already covered is out of scope for that review; file it
  as a follow-up issue unless it is blocking under §2.
- Exceptions are honest and rare: if you approved text and now believe it was
  wrong, say so explicitly in those words; the team then treats the approval
  as withdrawn and the point as blocking under §2 for the *next* round.
- A round that brings no new blocking point means the review is done.
- Cap: two full review rounds per PR. A third round needs a standup decision,
  not another pass.

## 4. Reviews never add scope to an open PR

A requirement, test, metric or subtask raised mid-review — even a good one —
is not part of the PR under review. It becomes a follow-up issue, labeled
(`tech-debt`, or the epic's label), and is prioritized in standup like all
other work. Plan estimates stay on their agreed baseline (9 Nov 2026) unless
standup explicitly moves them.

If the new point is big enough to change the plan, it is by definition not
review feedback: give it its own planning PR.

## 5. Silence belongs to the author

Where the RFCs (§1) and the plans are silent, the author's choice stands. A
reviewer who thinks the choice hides a real gap shows the gap; the gap then
gets **its own document PR** (a ruling). It is not folded into the open PR,
and it does not block that PR.

## 6. Doc drift blocks, cheaply

In this repository most PRs amend the RFCs and plans alongside the code, so
"the text says something the code does not" is a contradiction under §2 — the
most common blocking finding we have. Its fix is the **smallest edit** that
aligns the sentence with the code or the sibling plan, never a redesign.

When a PR writes a new ruling into prose, the same commit touches every
sibling that cites the affected passage — the epics, the program plan's
ledger rows, a replaced rule's citations — so the next reviewer does not have
to find the drift for you. Drift found later is a follow-up issue, not
endless review rounds.

## 7. Follow-ups need an owner

Every follow-up issue gets a quick triage in standup: keep, drop, or schedule.
The backlog is reviewed weekly; anything alive there for three sprints without
an owner is closed as wontfix rather than kept as ambient guilt.
