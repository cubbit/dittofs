# jdemo — the interactive session

One binary, one journal, one story at a time:

```sh
go run ./cmd/jdemo                       # a fresh journal under $TMPDIR, the basic story
go run ./cmd/jdemo --dir talk-journal    # name the directory (a used one opens or says why it cannot)
```

## Driving

- **`Enter`** runs the beat the screen shows; every beat prints its public API
  call source-style and what it returned — the version drawn, the bytes read,
  or the journal's own answer verbatim.
- Between beats the chooser offers the views, as often as you like:
  `(i)` index, `(s)` segments, `(f)` filesystem, `(d)` data dump, `(+)` stats.
  An empty line moves to the next beat; `q` ends the story.
- Every mutating beat refreshes the diff snapshots: the next look at a view
  marks its rows — `+` new, `~` changed, `- removed` — colourised on a real
  terminal, glyphed on a piped one.

## The views

| key | what it shows |
| --- | --- |
| `i` | the story file's placement entries, straight from `Inspect()` |
| `s` | per-segment accounting (`Inspect().Segments()`) |
| `f` | the journal directory: `format`, the 20-digit segments, spooling spares, human sizes |
| `d` | the on-disk truth: every record decoded and hexdumped with an ASCII lane — headers' columns (kind, file, extent, version, tag, seq) and the payload bytes right where they sit |
| `+` | the journal's totals and per-share rows (`Stats()`) |

## The basic story

1. **journal online** — init/open/SetVersionLimit, the state file, the spare pool
2. **attach the share** — one handle, tag 1
3. **a first write** — `hello world` at v1
4. **overwrite half of it** — the superseded range goes retained
5. **a 300 MB write, the roll** — the segment crosses its cap, a spare becomes a segment
6. **read across the gap** — served extents, `missing` for what is not held
7. **the change feed** — `Since`'s changelog, version by version
8. **truncate** — the removal marker, durable on return
9. **settle it** — metadata recorded, the marker goes
10. **a reserved write** — bytes held up front, no limit refuses it
11. **offload** — the remote tier's beat
12. **verify** — the index checked, reported, never repaired

## What answers "not implemented" today, and why

`sync`, the offload family, holds/stamps, `repack`, `Inspect().File`,
`Inspect().Verify` answer the journal's own `journal: not implemented`
verbatim, and the story walks on — the tool never claims a layer beneath
the built ones. The `index` all-entries view and the per-segment
accounting answer empty (no error to print): the view says so in one
line. Rebase this branch onto fresh `feat/storage-v2` as those subtasks
land and the same story fills in — recovery's assembly (T7.6) brings the
reopen and the index rows, epic 9a the segment accounting and the
offload flow.

## Numbers worth narrating

- `used` counts each segment at the format's 256 MiB cap: the pool holds
  six of them (content, spares, headroom, reserve), so a fresh journal
  reads about 1.5 GiB committed and the roll beat does not raise it — a
  spare simply becomes the next content segment.
- Every write draws a version: hello is v1, the overwrite v2, the roll v3,
  the truncate v4 — `versions left` falls one each.
- The `(d)` tail lines tell three stories apart: "untaken" is an
  unwritten tail of zeros (or a spare never taken), "sealed here" is the
  DJS1 marker a seal leaves, and "torn" is bytes the walk cannot step
  over — a torn write, or a record whose segment or own offset names
  somewhere else, which is how a recycled segment's old bytes stay
  honestly unreadable.
