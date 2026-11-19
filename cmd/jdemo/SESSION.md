# jdemo — the interactive session

One binary, one journal, one story at a time:

```sh
go run ./cmd/jdemo                            # a fresh journal under $TMPDIR, the basic story
go run ./cmd/jdemo --story crash             # the durability story: retention, the mark, a real restart
go run ./cmd/jdemo --story limits            # the space story: the share budget, the reserve-first escape
go run ./cmd/jdemo --dir talk-journal        # name the directory (a used one is recovered on open)
```

## Driving

- Each beat fills the screen with its step banner — `· STEP 3 OF 13 ·` — its
  title and caption, and a **look for** line telling the room what to watch.
- **`Enter`** runs the beat: the public API call, source-style, and what it
  returned — then each watched fact confirmed with a **✓** and the delta the
  journal's own stats measured. An unchanged watch says so, never a lie.
- Between beats the chooser offers the views, as often as you like:
  `(i)` index, `(s)` segments, `(f)` filesystem, `(d)` data dump, `(+)` stats.
  An empty line moves to the next beat; `q` ends the story.
- A view's rows are marked against how the view stood just before the beat
  that ran — `+` new, `~` changed, `- removed` — so a view you skipped for a
  beat shows only the latest beat's changes, and looking twice in one beat
  shows the same diff. Colourised on a real terminal, glyphed on a piped one.
  (The data dump `(d)` lists every record and is not diffed.)

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
6. **make it durable** — Sync's mark, the receipt the caller records and hands to the next open
7. **read across the gap** — served extents, `missing` for what is not held
8. **the change feed** — `Since`'s changelog, version by version
9. **truncate** — the removal marker, durable on return
10. **settle it** — metadata recorded, the marker goes
11. **a reserved write** — bytes held up front, no limit refuses it
12. **offload** — the remote tier's beat
13. **verify** — the index checked, reported, never repaired

## The other stories

- **crash** (7 beats) — durability's half: a write with its sync mark, an
  overwrite whose retained range the index look shows, a truncate durable
  on return, the Since feed a recovery replays, and a real restart: the
  journal closes, `Open` rebuilds the index from the segments and the
  marks, and the bytes read back.
- **limits** (7 beats) — space's half: a 2 GiB world and a 512 MiB share
  budget, a reservation taken before the burst, a filler that walks 40 MB
  writes until the share's own refusal, the reserved write served through
  it, the headroom math, and truncate's unchanged `used`.

## What answers "not implemented" today, and why

The offload family, holds/stamps, `repack` and `Inspect().Verify`
answer the journal's own `journal: not implemented` verbatim, and the
story walks on — the tool never claims a layer beneath the built ones.
`(i)` and `(s)` show the journal's real index and segment rows, and a used
`--dir` is recovered on open. Rebase this branch onto fresh
`feat/storage-v2` as the rest lands and the same story fills in.

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
