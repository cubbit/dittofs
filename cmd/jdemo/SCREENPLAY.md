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
- Now open `i` — **THE MOMENT**: "the honesty period ends here — here is
  the placement index itself, the in-memory structure every read walks:
  ONE held entry, v1, backed by the record at segment 3, offset 48." Let
  the room read the row; point at `record 3@48` — "the same place the
  data dump will take you to."
- Then `f` — **the room sees this:**
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

## STEP 6 — make it durable

- **"The write is on the disk — now make it a promise."**
- `⏎`:
  - **"Sync returns a MARK, one per stream: which segment, through which
    offset, at which sequence number. That is durability's receipt."**
  - Point at the mark — **"The engine records this mark, and hands it to
    the next Open — recovery checks the scanned tails against it. THIS is
    the sync mark you have heard named all week."**

## STEP 7 — read across the gap

- **"Read the first 16 bytes. Hello is there, the pattern starts at 4096 —
  everything between is a hole."**
- `⏎`:
  - **"What came back: `n=11`. Served extents at v1 and v2 —"** point at them
    — **"and `missing 11–16`. NEVER zeros. A hole is reported as an empty
    range, not faked data."**

## STEP 8 — the change feed

- `⏎`:
  - **"Everything a file owes, version by version — this is what metadata
    reads after a crash to catch up."**
  - **"Notice both states of the same range: the write that got superseded
    shows beside the one that replaced it."**

## STEP 9 — truncate

- **"Drop everything past 32."**
- `⏎`. Watch line: **"a removal marker appears — the held bytes past 32 go,
  and the marker stays UNSETTLED."**
- **"The truncate is durable the moment it returned — that's the
  write-ahead promise — but the journal keeps the bookkeeping record until
  the caller confirms metadata has it."**

## STEP 10 — settle the removal

- **"Settle is that confirmation."**
- `⏎`:
  - **"The marker is gone; Since has nothing more to say about this file.
    The bookkeeping footprint of a settled change is exactly nothing."**

## STEP 11 — a reserved write

- **"The engine doesn't write blind: it reserves BEFORE a burst — bytes,
  entries, versions and SPARES held up front."**
- `⏎`:
  - **"So a reserved write is exempt — no limit refuses it, and the rolls
    it needs are already held before it starts."**

## STEP 12 — offload (the honest stub)

- **"Offload is where the journal hands dirty bytes to the remote tier and
  marks them offloaded in the index."**
- `⏎`:
  - **"And here is the journal telling you the truth: `not implemented` —
    verbatim — no fake success. It lands with its subtask, and this exact
    beat turns green."**
- **"The tool never lies about the layer beneath it — the same honesty as
  the engine calling the same API."**

## STEP 13 — verify

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
  everything else is rebuilt from it on open. The mark from STEP 6 is what
  recovery checks the scanned tails against; the version window means no
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

---

# SCENARIO 2 — crash (--story crash)

*A six-beat story about durability: what survives, in what order, and the
honest seam. Runs after a basic run if the room wants the safety half.*

## the beats

1. journal online (same opening)
2. **write + sync mark — THE receipt beat.** Run, then read the mark out
   loud: stream, segment, offset, sequence. *"Durability has a name. The
   engine records THIS and hands it to the next Open."*
3. **overwrite, retained.** Open `i` right after. The index shows the
   narrowed old entry AND the retained range: *"the old bytes are still
   findable — that is the crash story: nothing synced is lost."*
4. **truncate, durable on return.** Watch the marker arrive unsettled.
5. **the Since feed.** *"Everything a recovering caller replays — this is
   the journal's side of the contract."*
6. **close, restart refused.** The journal closes, `journal.Open` runs
   against the state file's identity — and answers the roadmap verbatim:
   *"recovering them is not built. The identity lives on disk; the pass
   that reads it back is the next merge."* Don't apologize — point at it.

# SCENARIO 3 — limits (--story limits)

*A seven-beat story about space: the share budget, the reserve that must
come first, and what truncate does not give back.*

## the beats

1. journal online — note the **2 GiB world** in the banner.
2. **attach with a 512 MiB budget.** Basic passed zero for this same
   call — the share IS the budget here.
3. **reserve BEFORE the burst.** *"The reservation is charged the budget
   too — you take it while budget remains or the hatch is gone."*
4. **fill until the share says no.** 40 MiB at a time; the refusal is
   the SHARE's, with the bytes it's missing. Read it verbatim.
5. **the reserved write walks through it.** v-late, served — the escape
   hatch in the only order that works.
6. **headroom math.** Stats look: what's used vs the 2 GiB — the pool's
   own quarter is overhead the budget lives under.
7. **truncate gives back nothing — yet.** held falls, used doesn't:
   *"reclaimed space is repack's to free."* The honesty ledger ends it.

---

# Caption glossary (the plain-language pass — adopt, adapt, or ignore)

The room's test: "held", "drawn from the window", "spare pool" are words
the journal's author uses, not words a generalist met yet. When the
on-screen caption loses the room, the spoken line below carries the
same beat in plain nouns. Read the caption OR the gloss — not both.

- **STEP 1 (online):** "One directory, one file that names the journal.
  Before anything writes, the journal sets aside a chunk of version
  numbers so a crash can never re-issue one."
- **STEP 2 (attach):** "Everything I write goes through one open
  connection, tagged with this share's number — like a tenant ID on
  every box."
- **STEP 3 (a first write):** "Watch: one write becomes one record —
  never half of one. The 11 lands, and the journal hands back v1: the
  first version of this file it has never seen before."
- **STEP 4 (overwrite):** "I overwrite two bytes. The old bytes are NOT
  deleted — kept where a restart can find them, until the layer above
  confirms it has noted the change. Delete-later is the whole game."
- **STEP 5 (the roll):** "Now 300 MB. Watch the segment count: the
  current file filled up, and the journal switched to a spare it had
  already prepared — that switch is the only cost of a full disk file."
- **STEP 6 (make it durable):** "This is the receipt. Sync tells me:
  everything up to THIS point is physically on the disk. I record the
  receipt, and after a crash I hand it back — recovery starts there."
- **STEP 7 (gap read):** "I ask for 16 bytes. The journal has 11, a hole,
  then more. It returns what it has and TELLS me the hole — never
  silent zeros."
- **STEP 8 (change feed):** "The journal keeps a diary of every change.
  This is what the layer above reads to catch up: what exists, in
  order."
- **STEP 9 (truncate):** "Everything past 32 goes. Note: already
  durable — the removal is a record too."
- **STEP 10 (settle):** "The layer above says: noted. The journal drops
  the bookkeeping. Cleaned-up means GONE, not hidden."
- **STEP 11 (reserved write):** "The engine reserves capacity before a
  burst — pays up front, so no limit can interrupt the burst."
- **STEP 12/13 (stub honesty):** "These two are not built yet — the
  journal says so itself, in its own words. That is the point."
