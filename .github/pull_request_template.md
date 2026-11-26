<!--
Title: conventional commit, `type(scope): what changes`, lowercase, under ~70 characters.
  Good: fix(smb): refuse TREE_CONNECT when the share permission cannot be resolved
  Bad:  SMB improvements

The description should fit on one screen without scrolling. Delete any section that
would be empty or obvious. If the PR cannot be explained in one screen, it is probably
too big: say so and suggest how to split it.

How to open a PR (base, assignee, auto-merge, stacking):
.github/instructions/pr-opening.instructions.md
Writing rules for review comments and replies:
.github/instructions/pr-commenting.instructions.md
-->

# Description

<!-- One to three sentences: what changes in behavior, and why. Link the issue / RFC
instead of re-explaining it. For each subtask this PR finishes, one line closing its
issue: Closes #<subtask issue>. For subtasks with no issue, one Closes: line, as
pr-opening.instructions.md "Subtasks with no issue" describes. These lines credit the PR
on the burn-up; without them it has no estimate. -->

## Decision needed

<!-- What you need from the reviewer: "approve as is", "pick A or B", "check the
locking in X". One line. -->

## Type of change

<!-- Please delete options that are not relevant. -->

- [ ] Bug fix (non-breaking change which fixes an issue)
- [ ] New feature (non-breaking change which adds functionality)
- [ ] Breaking change (fix or feature that would cause existing functionality to not work as expected)
- [ ] Documentation update
- [ ] Performance improvement
- [ ] Code refactoring
- [ ] Test coverage improvement

## Changes Made

<!-- The changes a reviewer needs to know about, not every file or function: the diff
already shows those. Say where to start, which files are mechanical (renames, moves,
generated code) and which part needs careful eyes. -->

-
-
-

## Testing

<!-- Please describe the tests that you ran to verify your changes. Mention new tests
by the behavior they protect. Do not paste test output or logs. -->

- [ ] Unit tests pass (`go test ./...`)
- [ ] Manual testing performed
- [ ] Integration testing performed

## Testing Instructions

<!-- Describe how reviewers can test your changes: -->

1.
2.
3.

## Checklist

- [ ] My code follows the style guidelines of this project
- [ ] I have performed a self-review of my own code
- [ ] I have made corresponding changes to the documentation
- [ ] New and existing unit tests pass locally with my changes
- [ ] Any dependent changes have been merged and published

## Screenshots (if applicable)

Add screenshots to help explain your changes.

## Out of scope

<!-- What a reviewer would reasonably have expected this PR to do, and it does not: one
row per item, with the reason. Only things that were in reach of the original ask, the
issue or the subtask, not everything the PR does not touch. Delete the section when
nothing was left out. See pr-opening.instructions.md "Out of scope". -->

| Out of scope | Reason |
| --- | --- |
|  |  |

## Additional Notes

<!-- Risks, known limitations, other follow-ups. Things deliberately left out go under
Out of scope. -->
