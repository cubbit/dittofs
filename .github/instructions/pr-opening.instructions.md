---
applyTo: "**"
excludeAgent: "code-review"
---

# Opening a pull request

These rules apply when a PR is opened, and when its title or description is edited later.

## Target and branch

PRs target an integration branch: `develop`, or `feat/storage-v2` for storage-v2 work, which
has the same rules and its own merge queue. A stacked PR targets the PR below it (below).
Branch from a fresh `origin/<target>` and rebase onto it before pushing; never merge the
target in. When the target is not `develop`, set `DITTOFS_PREPUSH_BASE=origin/<target>` and
pass `dt run --base origin/<target>`.

## Title and description

Follow [`.github/pull_request_template.md`](../pull_request_template.md), including the
guidance in its comments, and the ground rules of
[`pr-commenting.instructions.md`](pr-commenting.instructions.md). If the PR fixes an issue,
the body carries `Closes #<N>`. That keyword fires only on `develop`, the default branch: on
any other target, close the issue by hand after the PR merges. A PR that finishes a subtask
of an epic closes that subtask's issue the same way, one `Closes #<N>` line per subtask, on
any target: that line is what credits the PR to the subtask on the burn-up. A subtask
finished only in part is named, not closed. On every target, also link the PR to each issue
it closes from the issue's or the PR's sidebar ("Development"), so the issue shows the PR:
on `develop` the keyword creates that link itself, so check it is there; on any other target
the sidebar or the API creates it. From a script or an agent:

```bash
pr=$(gh pr view <PR> -R cubbit/dittofs --json id -q .id)
issue=$(gh issue view <N> -R cubbit/dittofs --json id -q .id)
gh api graphql -f query="mutation{addCloseIssueReferences(input:{issueId:\"$issue\",pullRequestIds:[\"$pr\"]}){clientMutationId}}"
```

`removeCloseIssueReferences`, with the same input, removes a link added this way; a link the
`Closes` keyword created goes only by editing the PR body. `closingIssuesReferences` on the
PR and `closedByPullRequestsReferences` on the issue read the links.

Never mention Claude Code or
AI tooling, and add no `Co-Authored-By` lines for AI.

### Subtasks with no issue

Many subtasks have no GitHub issue yet. A PR that finishes one names it instead, by RFC and
subtask id, on one line of its own in the body, so the burn-up can credit it and give the PR
an estimate. A PR that does not name the subtasks it works on counts as work with no
estimate.

```text
Closes: RFC 8 T3.4, T3.5, RFC 23 T3.6
Part of: RFC 1 T7.2
```

- `Closes:`, with the colon, at the start of the line: the subtasks this PR finishes.
  `Part of:` the same way names a subtask it finishes only in part. Each keyword appears
  at most once.
- `RFC <n>` opens a group; every id after it, up to the next `RFC`, belongs to that RFC.
- An id is `T<epic>.<n>` exactly as its document writes it: `T3.4`, a split row
  `T4.2a`, or `T7.2` for epic 7a, since an epic split into parts numbers its
  subtasks once across them. Never the bare `3.4` or `E3-4`.
- If an RFC ever has more than one plan, the plan's slug, from its file name
  `rfc-<n>-<slug>-plan.md`, follows the number: `RFC 8 journal-only-engine T3.4`. With one
  plan, as every RFC has today, leave it out.
- One PR may carry both `Closes #<N>` lines and one `Closes:` line.
- A subtask with an issue that this PR finishes only in part is named `Part of #<N>`, on a
  line of its own, never on the `Part of:` line.
- No keyword fires on these lines and nothing links them: GitHub ignores them, only the
  burn-up reads them.

## Stack or not

Stack a PR on another one only when they must merge in order: this PR builds on code that
is still in another open PR. PRs that merely touch the same area are not stacked; each
targets the integration branch.

Before opening, look at what is already open:

```bash
gh pr list --state open --json number,title,author,headRefName,baseRefName
git log --oneline origin/<target>..HEAD   # commits from another PR's branch here = stacked already
```

If a stack looks warranted, **ask the user to confirm it** before creating the PR, naming
the PR it would sit on and the merge order.

A stacked PR targets the branch below it (`--base <branch-below>`) and says
`Stacked on #<M>; merge after it.` in its description. For its pre-push hook, set
`DITTOFS_PREPUSH_BASE=origin/<branch-below>`.

Prefer adding it to a GitHub stack on the PR below: when that one merges, GitHub retargets
the upper PR onto the integration branch and rebases it, but auto-merge does not carry over,
so enable it again then. A plain branch-on-branch PR gets no auto-merge until it targets the
integration branch, since nothing gates a merge into the branch below, and its retarget is
manual:

```bash
git fetch origin
git rebase -S --onto origin/<target> origin/<branch-below>
git push --force-with-lease
gh pr edit <PR> --base <target>
gh pr merge <PR> --auto
```

## Commands

```bash
git fetch origin && git rebase -S origin/<target>
git push -u origin <branch>
gh pr create --base <target> --assignee @me --title "..." --body-file <file>
gh pr merge <PR> --auto
```

Always assign the PR to yourself, and always enable auto-merge. Both integration branches
have a squash merge queue that also requires an approving review, green checks and signed
commits, so `--auto` queues the PR once those are in place; it does not skip the review.
