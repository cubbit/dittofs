# Terminal demos

This directory holds a small player for live terminal demos, and the demo of the test harness
built on it.

- **`player.sh`** plays a demo step by step. It types each command out, waits for Enter, runs it,
  and checks the output against what the step says to look for. It records every output, so a
  step that breaks on stage can show its saved output instead, and a whole demo can be replayed
  with no network. It is one file of bash (3.2 or later) and perl, with no other dependency, so it
  can be copied into any project.
- **`dt-demo.sh`** is the ten-step demo of `dt`, the test harness that
  [`docs/internals/testing.md`](../../../docs/internals/testing.md) describes.

## Run the dt demo

It's written for macOS: steps 3 to 6 mount a share with the Mac's own SMB client. It needs Go
1.26, Docker, `jq`, an IPv4 network address, and a terminal at least 120 columns wide.

```bash
test/harness/demo/dt-demo.sh setup      # before the talk: worktree, warm builds, dtc, Localstack, checks
test/harness/demo/dt-demo.sh            # the demo, live
test/harness/demo/dt-demo.sh replay     # the demo from recordings/: nothing builds, mounts or needs a network
test/harness/demo/dt-demo.sh record     # run every step unattended and save its outputs (about 2 minutes)
test/harness/demo/dt-demo.sh reset      # revert the demo's edits after a run that was cut short
test/harness/demo/dt-demo.sh cleanup    # reset, then remove the worktree
```

`setup` adds a detached worktree at `DEMO_REF` (default `f02e02620`, the commit the recordings
were made at) beside this checkout, named `dittofs-demo-dt`; `DEMO_WT` moves it. Every command
runs there, and every edit a step makes is reverted when the demo exits. `setup --quick` skips the
warm-up runs. Nothing leaves the machine: no push, no posting, no ssh.

Between steps:

| Key | Does |
|---|---|
| Enter | next step |
| `r` | run the step again (steps 4 and 5 undo their own edit first) |
| `f` | show the step's saved output, then go on |
| `b` | back one step |
| `1` to `9` | jump to that step |
| `q` | quit |

When a step's output misses something it should show, the prompt says so, and `f` shows the
saved output instead. `DEMO_HINTS=1` lists each step's "look for" items before it runs.

| Step | Shows | Runs |
|---|---|---|
| 1 | What runs here: the `pr` tier's suites, and whether this machine can run each | `dt list --tier pr` |
| 2 | The diff decides what runs: a docs change plans 0 cells, an SMB change adds the e2e cells | [`plan-compare.sh`](plan-compare.sh): three throwaway commits, `dt plan` for each |
| 3 | A real cell on a laptop: dt builds dfs from the checkout, starts it, and mounts it | `dt run clients/macos` |
| 4 | A regression fails the run, named in the output and the report | a broken expectation in `test/clients/files.sh`, then `dt report --failures` |
| 5 | A known failure passes, kept on record with its reason | [`known-demo.yaml`](known-demo.yaml) added to the registry |
| 6 | A stale known entry fails the run: the fix landed, the entry stayed | the expectation restored |
| 7 | One test's history across those runs | `dt history clients/test/clients/files.sh` |
| 8 | A cell this Mac can't run: not run on the host; `--in auto` moves it into dtc; `--parallel 3` runs three NFS versions at once (17 s) | `dt run pjdfstest/...`, `dtc shell` |
| 9 | What the dedicated host runs, and a merge run it graded | `dt plan --tier nightly`, `dt report` on [`nightly-run/`](nightly-run) |
| 10 | Where to start: which needs this machine meets | `dt doctor` |

`recordings/` holds one file per command, `stepS-K.txt` for step S's K-th command. Its first line
is `# command | time | commit` and the rest is the output. `record` rewrites them.

On screen and in recordings, the player masks the machine's private IPv4 addresses
(`<lan-ip>`), the worktree's path (`<worktree>`) and the home directory (`~`). `DEMO_MASK=0`
shows them.

## Write a demo of your own

Copy `player.sh` next to your demo script. A demo sets `DEMO_DIR`, `DEMO_STEPS` and `DEMO_WT`,
sources the player, defines `step1` to `stepN`, and calls `demo_main "$@"`:

```bash
#!/usr/bin/env bash
# hello-demo.sh: two steps, to show the player.
#
#   hello-demo.sh           the demo, live
#   hello-demo.sh record    run both steps and save their outputs in recordings/
#   hello-demo.sh replay    play the saved outputs back
set -u
DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export DEMO_DIR DEMO_STEPS=2 DEMO_WT="$DEMO_DIR"
. "$DEMO_DIR/player.sh"

step1() {
  title "Step 1: where this runs" "Show: the kernel, and who runs the demo"
  look '^(Linux|Darwin) ' "the kernel's name"
  cmd 10 <<'EOF'
uname -sr; id -un
EOF
}

step2() {
  title "Step 2: what is here" "Show: the demo's own files"
  look 'player\.sh' "the player, next to the demo"
  cmd <<'EOF'
ls
EOF
}

demo_main "$@"
```

`hello-demo.sh help` prints the script's opening comment, so that comment is the demo's usage.

A step calls:

| Call | Does |
|---|---|
| `title TITLE SUBTITLE` | Starts the step: clears the screen and shows its number, title and subtitle |
| `look REGEX TEXT` | Adds an item to check: after the step, the player marks TEXT with a tick when an extended regex matches any line the step printed, and a cross when none does |
| `cmd [SECONDS] <<'EOF'` | Types the command on stdin, waits for Enter, then runs it with `bash -c` in `DEMO_WT`, stopping it after SECONDS (default `DEMO_TIMEOUT`, 180). Output and errors both show and are checked |

Quote the heredoc (`<<'EOF'`) to keep `$` for the command itself. An unquoted one (`<<EOF`) expands
the demo's own variables first, such as `$DEMO_DIR`, which the screen shows as the directory's
path in its repository.

Settings, from the demo or the environment:

| Setting | Default | What it sets |
|---|---|---|
| `DEMO_DIR` | required | The demo's directory |
| `DEMO_STEPS` | required | How many steps there are |
| `DEMO_WT` | required, except for `replay` | Where every command runs |
| `DEMO_COUNTED` | `DEMO_STEPS` | How many steps are numbered on screen; later ones are titled CLOSE |
| `DEMO_SAVED` | `$DEMO_DIR/recordings` | Where `record` writes and `replay` reads |
| `DEMO_TYPE_SPEED` | `0.02` | Seconds per typed character; `0` prints at once |
| `DEMO_TIMEOUT` | `180` | Seconds a command may run |
| `DEMO_HINTS` | `0` | `1` lists a step's "look for" items before it runs |
| `DEMO_MASK` | `1` | `0` shows addresses and paths unmasked |

Hooks the demo may define, each optional:

| Hook | Runs |
|---|---|
| `demo_setup [ARG]` | On `setup`, with its argument: prepare and check the machine |
| `demo_reset [quiet]` | On exit from a live run or `record`, and on `reset` and `cleanup`: revert what the steps changed |
| `demo_undo STEP` | On `r`, before the step runs again: undo that step's own edit |
| `demo_after_record` | After `record` |
| `demo_cleanup` | On `cleanup`, after `demo_reset` |

[`dt-demo.sh`](dt-demo.sh) uses every one of them.
