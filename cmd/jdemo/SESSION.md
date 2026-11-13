# jdemo session sheet

The beat-by-beat command/pane sequence for a live journal demo. All commands
assume the daemon is up and the socket `--socket` points at it; viewer panes
re-run their command after each beat.

Setup: `jdemo start --dir journal --socket /tmp/jdemo.sock` in the driving
pane (it prints the journal dir and the offload dir). A fresh empty directory
is initialised; a stopped journal is refused until recovery lands (`start`
says so itself — the honest answer, verbatim).

| # | Beat | Command pane | What a viewer pane shows after |
| --- | --- | --- | --- |
| 1 | init + open | `jdemo start --dir journal …` | `ls journal`: the format file plus the spare segments the open made |
| 2 | attach a share | `jdemo share --tag 1` | `jdemo stats`: share 1's row appears, all zeros |
| 3 | a small write | `jdemo write-at --tag 1 FILE 0 --str hello` | `jdemo stats`: held/dirty +5; `jdemo index --file FILE` (once T7.6 lands): one held entry, v1 |
| 4 | over-write part of it | `jdemo write-at --tag 1 FILE 2 --str XYZ` | `index`: the old entry narrowed, the new one at v2; the superseded range retained until settle |
| 5 | a big write (roll!) | `jdemo write-at --tag 1 FILE 4096 --size 300000000` | `jdemo stats`: segments 2 (1 sealed) — the 256 MiB cap rolled a segment |
| 6 | read with a gap | `jdemo read-at --tag 1 FILE 0 16` | the pane itself: mixed `held` extents at different versions, `missing 5–8` for the unwritten gap |
| 7 | changes feed | `jdemo since --tag 1 FILE 0` | one line per change, version order |
| 8 | truncate | `jdemo truncate --tag 1 FILE 32` | `since`: the write narrowed, a `truncate` marker beyond 32 |
| 9 | settle it | `jdemo settle --tag 1 FILE v3` | `since` from v0: the marker gone |
| 10 | reserved write | `jdemo reserve --tag 1 4096 1`, `jdemo reserve-write …` | versions keep flowing; the reservation is exempt from limits |
| 11 | offload (once it lands) | `jdemo offload --tag 1 FILE v999` | `ls journal-offload/<file hex>/`: payload files named by range; `jdemo index`: entries' offloaded bits turn |
| 12 | verify (once E9a lands) | `jdemo verify` | counts, zero on a clean journal |

## What answers "journal: not implemented" today, and why

`sync`, `index`'s `File`/`All` read-outs, `segments`, `verify`, the offload
family, `hold`/`unhold`/`stamp` and `repack` are declared but not built on
this base of `feat/storage-v2`; the CLI prints the journal's own answer
verbatim, so the tool never lies about what the layer beneath can do. Each
lands with its subtask (T7.6 wires `Inspect().File`/`All`, E9a ` Segments`,
epic 7a/9a the rest); rebase `demo/journal-cli` onto fresh `feat/storage-v2`
and re-run the beat.

## Numbers worth narrating

- `used` counts each segment at the format's 256 MiB cap — so five segments
  read as 1.25 GiB whatever the payload.
- `versions left` falls one per write; the daemon raises the limit by 2³²
  per start, mirroring an engine recording its window durably.
- `read-at` prints `n=…` even with gaps: missing bytes are reported as
  extents, never zeros.
