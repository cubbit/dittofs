# jdemo — presenter's screenplay

A speaker-support script for the twelve-beat story. Every line in **bold** is a
thing to say out loud; everything else is a stage direction for you. Keystrokes:
`⏎` runs the beat; view keys open a look; an empty `⏎` moves on.

## Setup (before the room fills)

- `go run ./cmd/jdemo --dir talk-journal` — named dir, so it survives a rehearsal
- a named dir is recovered on the next run; rm it for a clean basic story
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
- Now open `i` — **THE MOMENT**: "here is the placement index itself, the
  in-memory structure every read walks." The ruler on top is the file's byte
  range: ONE held piece, `├── v1 ──┤`, offsets 0 to 11. Underneath, the
  index's own shape — held, retained, markers, three separate sets — and the
  row `0 ─ 11  v1  record 4@48`: "backed by the record in segment 4 at offset
  48 — the same place the data dump will take you to." Open `s` too:
  `seg 4 … held 11 … active` — the segment that holds it.
- Then `f` — **the room sees this:**
  - **"Five segments on disk before we wrote anything — the open made them,
    and not one is waste. Walk the numbers:"** (the dump says `spare, never
    taken` for every one of them)
    - **"Segments 1 and 2 — the HEADROOM spares, one per stream. The class
      records that RETURN space (forget, release) must always have somewhere
      to roll, even with the journal full."**
    - **"Segment 3 — the REPACK RESERVE. Repack's copying needs its own
      room, reserved the same way."**
    - **"Segments 4 and 5 — the CONTENT spares. Two streams, one roll each,
      without waiting on a filesystem."**
    - **"Content fills LAST, deliberately: if it filled first, a full journal
      could write until every spare is gone with no class left that accepts
      the records that would free space again."**
  - The new row marked `+` after the write is a SIXTH file, first
    `00…06.spare.tmp` (spooling), a moment later `00…06.seg`:
    **"The write took segment 4 — the first content spare. The maker
    refilled the content class straight away, so the NEXT roll already has a
    spare waiting. That's the point."**
- Open `d`:
  - **"Right here: segment 4 keeps `hello world`. Forty-eight bytes of
    segment header, then the record — its mtime, then the payload,
    aligned."**
  - **"The others say `spare, never taken` — the honest decode of a segment
    nobody has written to."**
- Stats (`+`) after the write: `segments 1`, `spares 2 → 1`, `used` 1.25 →
  1.5 GiB. Every segment — written or spare — is counted at the 256 MiB cap,
  so each spare the pool makes raises `used` by one segment.

## STEP 4 — overwrite half of it

- **"Now overwrite `XY` over bytes 2–4."**
- `⏎`, then `i` straight away — the ruler splits in three, and a second lane
  appears: `retained  ├─ v1 by v2 ─┤`, underneath the new piece:
  - **"The old entry split around the overwrite: v1 0–2, the new v2 2–4, v1
    4–11. The superseded v1 2–4 is NOT destroyed — it sits in its own set,
    RETAINED, never served to a read."**
  - **"It stays until the new write is synced. If a crash loses the new
    bytes, the old ones are still there."**
- The journal syncs in the background shortly after, so the retained row
  leaves on its own a moment later (it is gone by the next beat). Open `d`
  for the second record: `record 4@168`.
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
  - **"Sync returns a MARK, one per stream that has bytes: which segment,
    through which offset, at which sequence number. That is durability's
    receipt."** (One stream wrote, so one mark.)
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
  - **"Notice the overwrite's mark on it: v1 survives as two pieces, 0–2 and
    4–11, around the v2 that replaced the middle."**

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
  - **"The marker is gone, and Since no longer reports the truncate — only
    what is still held, the three small writes. The bookkeeping footprint of
    a settled removal is exactly nothing."**
  - Stats: `removal markers 0`, but `unreclaimed` still reads about 300 MB —
    **"the space comes back when repack rewrites the segments, not when
    metadata settles."**

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
- **"Is `Inspect` real, or a stub?"** — Real. `Inspect().File` and
  `Inspect().Segments` read the live index and the live segment accounting —
  the `i` and `s` looks are exactly those calls. `Stats()` is the totals, the
  same internals summed. Only `Inspect().Verify` (the index checked against
  the segments) still answers `not implemented`.
- **"Why does `used` say 1.25 GB?"** — Segments are accounted at the 256 MB
  cap they can reach, not the bytes they hold.
- **"What does the index cost in memory?"** — One entry per disjoint piece of
  one record: a few dozen bytes each. The ruler's `cells` come from those
  entries, not from the file's size — a 300 MB write is still one piece.
- **"Is a write durable when `WriteAt` returns?"** — No: it is in the journal
  and readable. Durable is what `Sync` (STEP 6) promises, or a truncate's own
  fsync (STEP 9). Basic shows both on purpose.
- **"Can the journal reopen right now?"** — Yes: the crash story's last beat
  does it live — close, `Open` with the marks, same four entries, same bytes.

---

# SCENARIO 2 — crash (--story crash)

*A seven-beat story about durability: what survives, in what order, and a
real restart. Runs after a basic run if the room wants the safety half. The
whole story is about ONE sentence: **nothing is lost that a synced version
might still name.** Say it at the start and again at the end.*

Setup: a fresh `--dir` (`go run ./cmd/jdemo --story crash --dir /tmp/crash`).
If the dir already has data the story still runs — the open recovers it — but
the numbers below assume empty.

## STEP 1–2 — online, attach (same as basic)

- Skip fast: **"Same opening as before — a real journal, one handle for the
  share, tag 1."** Do not re-explain the spare pool; this room has seen it, or
  will on the `f` look in beat 3.

## STEP 3 — write, then name its durability

- `⏎`. Two calls print: the `WriteAt` (`v1`) and the `Sync`.
- Read the mark OUT LOUD, field by field:
  `{Stream:0 Segment:4 Through:168 Offset:168 Sequence:1}`
  - **"Stream 0, segment 4 — the file the record is in."**
  - **"Through 168 — durable up to byte 168 of that segment. That is 48 bytes
    of header, the record, its padding: exactly the end of `hello world`."**
  - **"Sequence 1 — the first sync of this stream. Sequences only go up."**
- **"This is the receipt. The engine writes it down somewhere that survives a
  crash — and hands it back at the next Open. Keep it in mind: beat 7 uses
  this very mark."**
- Checks that print: index +1 entry, dirty +11 B, versions +1. *Dirty* means
  "not yet handed to the remote tier" — it stays dirty here; offload is a
  stub.
- Open `i`: the ruler is one piece, `├────v1────┤`, offsets 0 to 11.

## STEP 4 — overwrite: retention is the crash story

- **"Now overwrite bytes 2–4 with `XY`. Watch what the journal does NOT do:
  it does not destroy the old bytes."**
- `⏎`, then `i`. The ruler splits into three, and a second lane appears:
  `held  ├v1┤├v2┤├──v1──┤` and `retained  ├ v1 by v2 ┤` under the middle.
  - **"Held is what a read serves: v1 0–2, the new v2 2–4, v1 4–11. Retained is
    the superseded v1 2–4 — kept, never served."**
  - **"Why keep it? The new write is NOT synced yet. If we crash right now and
    the XY never reached the disk, the old two bytes are still the truth."**
- Point at the check lines: `index +3 entries` — the one entry became three
  held pieces and a retained one, minus the original.
- If asked "when does retained go?": **"When a sync covers the overwrite. The
  journal syncs in the background, so it may vanish by itself while you talk —
  that is not a bug, that is the lifting."**
- Open `d` if the room likes bytes: the second record, `record 4@168`, holds
  `XY`; the first is untouched. Append-only: an overwrite is a NEW record.

## STEP 5 — truncate: durable the moment it returns

- **"Drop everything past 32. Look at what we're truncating: the file is 11
  bytes long. A truncate past the end still counts — the marker is the
  record that says 'the size is now 32'."**
- `⏎`: `v3`, and `✓ removal markers +1`.
- **"Truncate is a record, fsynced BEFORE it answers. Crash one nanosecond
  later and the truncate still happened."**
- **"And that fsync is the same fsync that makes the XY durable — so the
  retained lane just vanished. Open `i`: the marker row says
  `32 ─ end  v3`, an arrow off the right edge of the ruler: 'to the end of
  the file, whatever that is'."** Say it: *one fsync, two promises.*

## STEP 6 — the feed a recovery replays

- `⏎`. Four lines, in version order:
  `write off=0 len=2 v1`, `write off=4 len=7 v1`, `write off=2 len=2 v2`,
  `truncate off=32 len=9223372036854775775 v3`.
- **"This is what metadata reads after a crash to catch up. Three things to
  notice:"**
  - **"The first write is two lines, not one — the overwrite cut it. The feed
    shows what is HELD, not what was once written."**
  - **"The truncate is last, v3 — still unsettled, so it is still reported. If
    metadata had settled it, it would be gone."**
  - **"The retained bytes are NOT here. A feed is the truth, and the retained
    range is only insurance."**
- The `len=9223372036854775775` is "to the end of the file" (max int64 minus
  the offset); the index view draws it as `end`.

## STEP 7 — close, then restart: recovery rebuilds it

- **"Now the proof. I close the journal. The process keeps only three
  things: the directory, the identity beside it, and that mark from beat
  3."**
- `⏎`. Four lines print:
  - `// → journal closed; only the directory, the identity and the marks remain`
  - `journal.Open(ctx, dir, floor, id, marks, opts...)` — **"Open scans every
    segment, replays the records, rebuilds the index, and checks the tails it
    found against the mark. A tail shorter than the mark would be a
    reported loss."**
  - `j.SetVersionLimit(limit)` — **"A reopened journal has no version window
    until the caller raises it. Without it, a write has no version to draw —
    check `+` on this beat and `versions left` reads 4.29 billion, not zero."**
  - `reopened: 4 index entries before the close, 4 after` and
    `"heXYo world"`.
- **"Same four entries, same bytes — including the XY, which was never
  explicitly synced by a write. The truncate's fsync did it."**
- Close: **"Nothing synced was lost. That is the whole contract."**
- If asked "what about the marker?" — it survived too: open `i` and the
  `32 ─ end v3` row is back. Markers are records; recovery rebuilds them.

### Crash-story Q&A

- **"What if the crash lands between the write and the sync?"** — The write
  may or may not be on disk. Recovery keeps what is whole: a record whose
  checksum verifies, and discards a torn tail. The mark tells it how far
  durability reached; bytes past the mark are unguaranteed, not lost.
- **"Why a mark per stream?"** — Streams fsync independently. Each reports
  where its own durability reached.

# SCENARIO 3 — limits (--story limits)

*An eight-beat story about space: the share budget, the reserve that must
come first, and what truncate does not give back. The throughline: **a full
journal must still be able to make room.***

Setup: fresh `--dir`. This story writes about 440 MiB in total and is the
slowest: expect a couple of seconds on the fill beat, and keep the terminal
tall enough for the stats look.

## STEP 1 — journal online (note the 2 GiB world)

- Point at `MaxBytes`: **"This journal is capped at two gibibytes in total.
  That cap covers everything it holds: the data, AND the spare files it
  keeps ready."**

## STEP 2 — attach, with a budget

- **"Basic passed zero here. Now the second argument is real: 512 MiB. That
  is this SHARE's budget for record bytes, and it is checked before anything
  else."**
- `⏎`: `h, err := j.Share(1, 536870912, journal.Version{})`, handle attached.

## STEP 3 — reserve BEFORE the burst

- **"Before we write anything, we reserve 80 MiB: two fills' worth. Why
  80 and not 40? Each write costs a little more than its payload — entries,
  a footer — so a reservation of exactly the payload cannot hold the write
  it was made for."**
- `⏎`: `res, err := h.Reserve(83886080, 1)`.
- **"The reservation is charged against the 512. Take it while there is
  budget — the escape hatch has to be built BEFORE the room is full. Reserve
  after, and it is refused with everything else."**
- Open `+`: `reserved` is now 80 MiB. This is the number to point at later.

## STEP 4 — fill the budget, until the share says no

- **"Forty MiB at a time, same file, next offset. I will not narrate ten
  writes — watch the screen."** `⏎` (a few seconds).
- The last line is the point, read it verbatim:
  `write 11: journal: no space: refused by the share limit, 8389856 bytes
  missing`.
  - **"Ten fit. The eleventh asked for 40 MiB and was refused by the SHARE —
    not by the disk, not by the journal. 512 minus 80 held for the
    reservation is 432 MiB; ten fills are 400; the eleventh needs 40 more
    than the 32 left. It even tells us how many bytes it was short."**
- Open `i`: the ruler lines the ten pieces up, 0 to 400 MiB. **"Ten held
  pieces, one record each — not 400 million of anything."** Open `s`: two
  segments, the first sealed, the second active: the stream rolled once.

## STEP 5 — the reserved write walks through the refusal

- **"Same share, same file — a refused write a second ago. Now the same bytes
  through the RESERVATION."** `⏎`.
- `res.WriteAt(ctx, FILE, 536870912, fill40MiB, 0)` answers `v31`, served.
- **"Its bytes were set aside up front. No limit refuses it. That is the
  hatch: the engine uses it for the records that FREE space — forget,
  release. If it had to ask for room first, a full journal could never
  recover."**
- The reservation closes on return; its unused bytes go back.

## STEP 6 — the headroom math

- `⏎`. Three lines: `max 2 GiB`, `used 2 GiB (2 segments written, 3 spares…)`,
  `share budget 512 MiB (held 440 MiB)`.
- **"Held is 440 MiB: ten fills plus the reserved write. The share has not
  hit its 512 — but `used` HAS hit the world's 2 GiB."**
- **"`used` counts every segment, written or spare, at the 256 MiB cap. The
  pool's spares are the journal's own overhead — headroom, the repack
  reserve, the content spares. The world has to hold them AND the budget."**
- Open `+`: the full picture. `used` read 1.25 GiB when the story began (five
  segments, all spares) and has climbed to 2 GiB: **"every time a content
  spare is taken, the pool makes a replacement, and the replacement is
  counted too. Three more spares, three more segments, and the journal is
  at its ceiling — with 440 MiB of actual data."** Compare with `reserved`:
  it was 80 MiB after beat 3 and is 0 now; the reservation was spent and
  closed.

## STEP 7 — truncate gives back nothing, yet

- **"Drop everything past 32 bytes."** `⏎`.
- Checks: `held -439 MiB`, `removal markers +1`. Line:
  `used before 2 GiB, after 2 GiB — unchanged; repack frees`.
- **"The data is logically gone. The segments are not. Segment files are only
  freed when REPACK rewrites them — and repack is not wired in this journal
  yet. So `used` does not move."**
- **"That is why space needs a reservation and not just a delete button: the
  space returns on repack's schedule, not the caller's."**
- Open `+`: `held` is 32 B now, `removal markers 1`, and `unreclaimed`
  jumped from 0 to about 440 MiB — the gauge for what the truncate left
  behind for repack. `used` is still 2 GiB.

## STEP 8 — the honesty ledger

- **"What is NOT here: the drain. Offload would hand the dirty bytes to the
  remote tier; repack would reclaim the segments. This journal says so
  itself."** `⏎`: `journal: not implemented`.
- **"Live wiring: the share limit, the reservation, the refusal. Pending:
  the drain. The tool never claims more."**

### Limits-story Q&A

- **"Why 512 MiB and a 2 GiB world — what is the rest?"** — The pool: spare
  segments at the cap are permanently charged, so the world holds roughly a
  quarter of itself as overhead before a byte of content.
- **"What is the difference between the share limit and the world?"** — The
  share's budget is per-share, in record bytes, checked first. The world is
  the journal's whole local storage. A share refusal is polite; a world
  refusal means the journal is full.
- **"Could the reservation run out?"** — Yes: it is exactly what you asked
  for. Beyond it, the write is an ordinary one again and meets the limits.

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
  deleted — kept where a restart can find them, until the new bytes are
  safely on the disk. Delete-later is the whole game."
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
