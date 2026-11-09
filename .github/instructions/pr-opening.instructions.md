---
applyTo: "**"
excludeAgent: "code-review"
---

# Opening a pull request

## Base and branch

PRs target `develop`, or the PR they are stacked on (below). Branch from a fresh
`origin/develop`, and rebase onto it before pushing — never merge `develop` in.

## Title and description

Follow [`.github/pull_request_template.md`](../pull_request_template.md), including the
guidance in its comments, and the ground rules of
[`pr-commenting.instructions.md`](pr-commenting.instructions.md). If the PR fixes an issue,
the body carries `Closes #<N>`. Never mention Claude Code or AI tooling, and add no
`Co-Authored-By` lines for AI.

## Stack or not

Stack a PR on another one only when they must merge in order: this PR builds on code that
is still in another open PR. PRs that merely touch the same area are not stacked; each
targets `develop`.

Before opening, look at what is already open:

```bash
gh pr list --state open --json number,title,author,headRefName,baseRefName
git log --oneline origin/develop..HEAD   # commits from another PR's branch here = stacked already
```

If a stack looks warranted, **ask the user to confirm it** before creating the PR, naming
the PR it would sit on and the merge order.

A stacked PR targets the branch below it (`--base <branch-below>`), says
`Stacked on #<M>; merge after it.` in its description, and gets no auto-merge until it
targets `develop`. For its pre-push hook, set `DITTOFS_PREPUSH_BASE=origin/<branch-below>`.
`develop` squash-merges and does not delete merged branches, so nothing retargets a
stacked PR when the one below lands. Do it by hand:

```bash
git fetch origin
git rebase --onto origin/develop origin/<branch-below>
git push --force-with-lease
gh pr edit <PR> --base develop
gh pr merge <PR> --auto
```

## Commands

```bash
git fetch origin && git rebase origin/develop
git push -u origin <branch>
gh pr create --base develop --assignee @me --title "..." --body-file <file>
gh pr merge <PR> --auto
```

Always assign the PR to yourself, and always enable auto-merge. `develop` has a squash
merge queue that also requires an approving review and green checks, so `--auto` queues
the PR once both are in place; it does not skip the review.
