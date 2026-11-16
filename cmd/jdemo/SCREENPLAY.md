# jdemo — presenter's screenplay

A speaker-support script for the twelve-beat story. Every line in **bold** is a
thing to say out loud; everything else is a stage direction for you. Keystrokes:
`⏎` runs the beat; view keys open a look; an empty `⏎` moves on.

## Setup (before the room fills)

- `go run ./cmd/jdemo --dir talk-journal` — named dir, so it survives a rehearsal
- rm the dir between rehearsals: the journal cannot reopen until recovery lands
- Terminal: dark theme, reasonably tall; the colours are picked for one
- Have `SESSION.md` next to this — it is the audience-facing contract

---

## STEP 1 — journal online

- Wait for the screen; let the room read the banner.
- **"This is the journal — the write-ahead heart of the storage engine. Not a
  diagram: this process owns a real one, on the disk behind that path, and
  everything you'll see is the journal's own answer."**
- Point at the two blue lines:
  - **"Open first — never Init over data we can't recognise. The identity
    beside the directory is what lets the next start know it's the same
    journal."**
  - **"And the version window is raised before anything writes — the same
    handshake an engine recording its window durably performs."**

## STEP 2 — attach the share

- **"The engine speaks to the journal through one handle per share."**
- **"Every record this handle writes carries the share's tag — a file belongs
  to its tag for as long as any segment holds one of its records."**
- `⏎` to run.

## STEP 3 — a first write

- **look-for is on screen — use it.** "The room should watch for: one held
  entry, the bytes going dirty, the file appearing, one version drawn."
- `⏎`. Point at the result line:
- **"Eleven bytes became ONE record — that's the unit: never a partial write."**
- **"v1 is not a timestamp — it's a drawn version, from the window we raised.
    Every byte range answers to one."**
- Now open `f` — **the room sees this:**
  - **"Four segments already on disk, before we wrote anything. This is the
    open's work, and it isn't waste — walk the numbers:"**
    - **"Segment 1 — the HEADROOM spare. The class records that RETURN
      space (forget, release) must always have somewhere to roll, even with
      the journal full."**
    - **"Segment 2 — the REPACK RESERVE. Repack's copying needs its own
      room, reserved the same way."**
    - **"Segments 3 and 4 — the CONTENT spares. Two streams, one roll
      each, without waiting on a filesystem."**
    - **"Content fills LAST, deliberately: if it filled first, a full journal
      could write until every spare is gone with no class left that accepts
      the records that would free space again."**
- Open `d`:
  - **"Right here: segment 3 keeps `hello world`. Forty-eight bytes of
    segment header, then the record — its mtime, then the payload, aligned."**
  - **"The other segments say `spare, never taken` — the honest decode of a
    segment nobody has written to."**
- `(f)` again if asked why a 5th appeared after an 11-byte write:
  - **"The take left the content class short of its target for a moment —
    the maker refills to stay ahead. The NEXT roll already has a spare
    waiting. That's the point."**

## STEP 4 — overwrite half of it

- **"Now overwrite `XY` over bytes 2–4."**
- `⏎`. Then `d`:
  - **"A second record. The old range was NOT destroyed —"**
  - **"the old entry narrows, the superseded bytes stay RETAINED until
    metadata says it has recorded the change."**
- **"This is the crash story: nothing is ever lost that a synced version
  might still name."**

## STEP 5 — a big write, the roll

- Look-for line does the tease: "watch the segment count."
- `⏎`. Then `+`:
  - **"Two segments, one sealed. The 300 MB crossed the segment cap and the
    stream ROLLED — onto a spare that already existed. No write waited on
    the filesystem; taking one cost a pop off a list."**
- **"That's what the spare pool buys: a roll is a pointer move, not a
  `fsync`'d creation."**

## STEP 6 — read across the gap

- **"Read the first 16 bytes. Hello is there, the pattern starts at 4096 —
  everything between is a hole."**
- `⏎`:
  - **"What came back: `n=11`. Served extents at v1 and v2 —"** point at them
    — **"and `missing 11–16`. NEVER zeros. A hole is reported as an empty
    range, not faked data."**

## STEP 7 — the change feed

- `⏎`:
  - **"Everything a file owes, version by version — this is what metadata
    reads after a crash to catch up."**
  - **"Notice both states of the same range: the write that got superseded
    shows beside the one that replaced it."**

## STEP 8 — truncate

- **"Drop everything past 32."**
- `⏎`. Watch line: **"a removal marker appears — the held bytes past 32 go,
  and the marker stays UNSETTLED."**
- **"The truncate is durable the moment it returned — that's the
  write-ahead promise — but the journal keeps the bookkeeping record until
  the caller confirms metadata has it."**

## STEP 9 — settle the removal

- **"Settle is that confirmation."**
- `⏎`:
  - **"The marker is gone; Since has nothing more to say about this file.
    The bookkeeping footprint of a settled change is exactly nothing."**

## STEP 10 — a reserved write

- **"The engine doesn't write blind: it reserves BEFORE a burst — bytes,
  entries, versions and SPARES held up front."**
- `⏎`:
  - **"So a reserved write is exempt — no limit refuses it, and the rolls
    it needs are already held before it starts."**

## STEP 11 — offload (the honest stub)

- **"Offload is where the journal hands dirty bytes to the remote tier and
  marks them offloaded in the index."**
- `⏎`:
  - **"And here is the journal telling you the truth: `not implemented` —
    verbatim — no fake success. It lands with its subtask, and this exact
    beat turns green."**
- **"The tool never lies about the layer beneath it — the same honesty as
  the engine calling the same API."**

## STEP 12 — verify

- **"Last: the index checked against the segments — the report, never the
  repair."**
- `⏎` — same honesty line. `q` to quit:
- **"Close is the last demo beat too: in-flight work stops, reservations
  are returned, descriptors close. A journal you can always shut cleanly
  is a journal you can crash safely."**

---

## If the room asks — likely Q&A

- **"Why not SQLite/a database?"**  — Because the pattern is the product:
  append-only records, in-memory index, one fsync per sync. A database gives
  none of that story.
- **"What if the process dies right now?"** — Everything synced is on disk;
  everything else is rebuilt from it on open. The version window means no
  version is ever drawn twice.
- **"Where does `Stats()` come from if the index says 'not implemented'?"** —
  `Stats` is measured by the journal as it runs; the `Inspect` read-outs are
  the same internals behind a public window that lands with a later subtask.
  The numbers are the journal's own, the *view* is what's pending.
- **"Why does `used` say 1.25 GB?"** — Segments are accounted at the 256 MB
  cap they can reach, not the bytes they hold.
- **"Can the journal reopen right now?"** — No: `recovering them is not
  built` is a live subtask's honest absence — the same rule the CLI enforces,
  surfacing at the exit-code the process would feel.
