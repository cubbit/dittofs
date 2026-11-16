# Learning decks

Slide decks that explain DittoFS RFCs in plain language, with clickable terms, a
glossary, and small animations where a picture moves better than it reads. They
are served as static HTML under `/learn-dittofs/`.

This file is the guideline: how the tree is laid out, how a deck is built, and
the rules that keep a simplified explanation from becoming a wrong one. A skill
that generates decks will be distilled from it once the first deck exists.

> Status: the shared runtime, a home page, an introduction and seven sections,
> ordered from the client down to the disk: the filesystem service (RFC 17),
> adapters (RFC 20, then RFC 22 for SMB), open state and locks (RFC 14), the
> metadata store (RFC 16), the engine (RFC 8), the journal (RFC 1) and the test
> harness (RFC 29), in English, Italian and Brazilian Portuguese.

## Serving

```bash
docs/guide/learning/serve.sh            # http://127.0.0.1:8765/learn-dittofs/
PORT=9000 docs/guide/learning/serve.sh
```

It is `python3 -m http.server` rooted at `docs/guide/learning/`, bound to
`127.0.0.1`, so only this machine can open the decks. To serve them to other
machines, opt in with `BIND=0.0.0.0 docs/guide/learning/serve.sh`. A server is
required rather than opening the file: decks load their strings and glossary
with `fetch`, which browsers refuse over `file://`.

## Layout

```text
docs/guide/learning/
├── README.md                  this file
├── serve.sh                   local HTTP server
└── learn-dittofs/             served at /learn-dittofs/
    ├── index.html             home page: one card per section
    ├── decks.json             the sections it lists, in order: id, deck path, RFC
    ├── shared/                the runtime every deck loads
    │   ├── deck.css           theme, slide canvas, term styling
    │   ├── deck.js            boot: reveal, language, glossary, overflow check
    │   ├── i18n.js            string loading, [[term]] expansion, language switch
    │   ├── glossary.js        term popovers and the glossary slide
    │   └── scene.js           animation kit: nodes, links, packets, steps
    ├── third-party/           pinned third-party libraries, committed
    │   ├── reveal.js-<ver>/
    │   └── anime-<ver>/
    ├── i18n/                  UI chrome strings (buttons, labels)
    │   ├── en.json
    │   ├── it.json
    │   └── pt-BR.json
    ├── glossary/
    │   ├── terms.json         language-neutral: source, code anchors, RFCs
    │   ├── en.json            per language: name, short, long
    │   ├── it.json
    │   └── pt-BR.json
    ├── intro/                 the introduction: a deck built on RFC 0 and architecture.md
    └── rfcs/
        └── rfc-<n>-<slug>/    one deck per RFC
            ├── index.html     slide structure, no prose
            ├── deck.json      metadata: RFC path, the RFC commit it was built from
            ├── i18n/{en,it,pt-BR}.json
            └── scenes/index.js  every scene the deck animates
```

A **deck** explains one RFC. A **course** (later) is an ordered list of decks
for one audience, under `courses/`; decks stay audience-neutral until then.

The **introduction** (`learn-dittofs/intro/`, the home page's first section) is
the one deck not tied to a single RFC. It is an overview told from the user's
side — who mounts DittoFS, what they see, the parts inside one node, one write
followed to the bucket and back — and it ends with a map of every section after
it. No single RFC covers that span, so it is built on RFC 0 (the terms, the
layers, the write path, the failure model) and on
[architecture.md](../../internals/architecture.md) (the code on `develop`
today). Its `deck.json` records RFC 0 as its RFC and lists the other sources. It
keeps a cover, a "what you'll learn" slide, its quizzes and the glossary, but drops
the RFC-specific opening (two sentences, known, new and clashing terms).

## Stack

- **Slides: [reveal.js](https://revealjs.com/) (MIT).** It brings a fixed
  1280×720 canvas scaled to the window, keyboard navigation, `#/7` links to a
  slide, fragments, speaker notes (`S`) and PDF export (`?print-pdf`).
  `shared/deck.js` adds a top-right bar: back to the first slide, back to the
  home page, the animation mode and the language switch.
- **Animations: [anime.js](https://animejs.com/) (MIT)** driving inline SVG.
  Only where a step-by-step picture beats a static Mermaid-style diagram.
- **Pinned, committed, no CDN.** Libraries live in `third-party/` at an exact
  version, so the decks work offline and do not change under us.
- No build step. Plain HTML, CSS, ES modules and JSON.

### Animations

- **Stepped.** Each scene is a list of steps driven by reveal fragments:
  → advances one step, ← rewinds. Every step highlights the sentence it
  illustrates, so picture and text move together. Nothing advances on its
  own, the last step included.
- **The reader picks how much moves**, with the three-way switch in the
  top-right bar, beside the language switch:
  - **loop** replays the step on screen after a pause, for as long as its
    slide is shown, and only that step;
  - **once** plays the step on screen once, then holds its end state;
  - **off** moves nothing: each step is drawn at its end state at once.

  The choice applies at once to the slide on screen and to every deck, and is
  kept in `localStorage` (`learn-dittofs.motion`). Without one, the mode is
  **once**, since a picture that never stops moving distracts from the text
  beside it (WCAG 2.2.2), or **off** under `prefers-reduced-motion`. PDF export
  is always still and shows each scene's last step.
- **Read aloud, not looked at.** The drawing is hidden from screen readers:
  the captions carry what it shows, and the caption of the step that becomes
  current is announced (the captions list is a polite live region).
- **Declared, not drawn by hand.** A scene declares nodes, links and steps
  in the deck's `scenes/index.js`; `scene.js` renders and animates them. The
  full shape is documented at the top of `shared/scene.js`: nodes have a
  `kind` (`client`, `adapter`, `service`, `store`, `group`), links may route
  through `via` points, and each step can `focus`, `show`, `dim` or `tone`
  nodes, move `packets` along links (`reverse`, `bounce`, `with` to run in
  parallel) and add `marks` (badges) once its packets arrive. Labels are string
  keys; a label starting with `=` is a literal (wire names, stage names, code)
  and is not translated. A deck needing a new primitive adds it to `scene.js`
  rather than to the deck, so every deck looks the same.
- An animation must also make sense frozen: each step at its end state, as
  **off** and `prefers-reduced-motion` show it, and the whole scene at its
  last step, as PDF export prints it.

## Languages

English (`en`), Italian (`it`) and Brazilian Portuguese (`pt-BR`).

- **English is the source.** Write it first. The translations follow it and
  do not add, drop or soften claims (see [Correctness](#correctness)).
- **Prose lives only in `i18n/*.json`.** `index.html` holds structure and
  keys (`data-i18n="problem.title"`), never sentences. A missing key falls back
  to English and is outlined in the overflow check so it is visible.
- **The switch** is a button in the corner. It changes language in place,
  keeping the current slide and step, and stays in the URL (`?lang=it`) and in
  `localStorage`, so a link opens in the sender's language.
- **Translate the meaning, not the sentence.** A translation reads as if it
  had been written in that language: its own word order, its own way of
  chaining clauses, not English colons and semicolons carried over. Read each
  string aloud; if it limps, rewrite it rather than polish it.
- **Introduce before you use.** A word, an image or a name appears in a slide
  only after the deck has introduced it. An analogy's limit cannot talk about
  "a box" the analogy never mentioned. This holds in English first; a
  translation inherits the gap.
- **Never carry over a pointer.** An RFC sentence that points elsewhere
  ("costs what §4.2 states", "see below", "the table in §5") does not go
  into a slide as it stands, not even with the section number reworded as
  "the RFC's table of keys". Say what it points to, in words the deck has
  already introduced, or move the claim after the slide that introduces it.
  The citation goes in the speaker notes.
- **Write full sentences that stand on their own. This is mandatory.** A
  learner who has only the slide in front of them, without a speaker, must
  be able to follow every caption and list item. Concision is not a goal;
  being understood is. So:
  - name what is acted on: "the other 960 KiB", not "the rest"; "the first
    64 KiB as they were before alice-pc rewrote them", not "C1's old copy";
  - say who does what, and why when it is not obvious: "N1 asks the metadata
    store where they are", not "metadata store, asked about the rest only";
  - give a size, a state or a term its meaning where it is first leaned on:
    "block K1, about 4 MiB", "Remote, so its only copy is in the bucket";
  - no telegraphic clauses chained with colons, no noun-phrase captions, no
    passive fragments that read as machine translation ("each chunk is
    verified whole first");
  - no asides that only matter to the RFC's authors ("RFC 8's proposal",
    "§7.8 default"): whether a detail is proposed, tunable or open goes in
    the speaker notes.

  When full sentences do not fit, change the layout (`scene one`, or split
  the slide in two), never the sentences. Both reviews check this rule, and
  a caption that breaks it is a defect, not a style choice.
- **Component and code names stay in English** in every language: journal,
  syncer, carver, `WriteAt`, share. Their definitions are translated.
- **Markup allowed in strings:** `[[term-id]]` and `[[term-id|surface text]]`
  for glossary terms, `**bold**`, `` `code` ``. No raw HTML, so a translator
  cannot break the layout.

### Text budget per slide

Italian and Portuguese run 20–30 % longer than English, so the budget is set
for the longest language and English is written well under it.

| Element | Limit (English) |
| --- | --- |
| Title | 50 characters |
| Bullets | 5 per slide, 80 characters each |
| Body text | 350 characters per slide, all bullets included |
| Text beside a diagram | 3 bullets, 60 characters each |
| Quiz | question 120 characters, each option 80, each explanation 160 |
| Speaker notes | no limit: they carry the detail and the citations |

The budget is a guide; the check is the rendered slide. `?check=1` makes
`deck.js` outline every slide whose content overflows the canvas and every
string that fell back to English, and list them in a box in the corner; the
result is also on `window.deckCheckResult`, so a headless browser can collect
it (load `…/?lang=it&check=1`, wait for `deckCheckResult`). Run it in **all
three languages** before a deck is done. A slide that overflows is split, not
shrunk: no font-size overrides. Scene labels are the one exception: a label
longer than its box is shrunk to fit, since a picture cannot reflow.

## Glossary

Every RFC introduces many terms; the glossary is shared across decks so a term
is defined once.

- **`glossary/terms.json`** holds what does not depend on language:

  ```json
  "adapter": {
    "source": "docs/internals/rfcs/rfc-0-data-lifecycle.md#glossary",
    "sourceLabel": "RFC 0 · Glossary",
    "rfcs": ["rfc-0", "rfc-17", "rfc-20"],
    "code": ["pkg/adapter", "internal/adapter"],
    "target": ["internal/adapter/nfs", "internal/adapter/smb"]
  }
  ```

  `source` is a repository path with a GitHub-style anchor; the popover links
  it on GitHub. `code` is where the term lives **today**; `target` is where the
  RFC puts it when that does not exist yet. Check both with `ls` or `rg`.
  Only `code` is shown; `target` stays data, since a planned location is
  progress, not design.
- **`glossary/<lang>.json`** holds `name`, `short` (one sentence, shown in the
  popover and on the "new terms" slides), `long` (shown on the glossary slides)
  and, for a term that clashes, `clash` (shown on the "terms that clash" slides
  and in the popover). Entries may link other terms with `[[id]]`. A deck's
  glossary slides include the terms linked from a `short` or `clash` text (what
  a popover shows), not those linked only from a `long` text: following those
  pulled another RFC's whole vocabulary into a deck.
- **It simplifies, it does not redefine.** RFC 0's
  [Glossary](../../internals/rfcs/rfc-0-data-lifecycle.md#Glossary) is the
  definition of record and every other RFC uses words in its sense;
  [docs/guide/glossary.md](../glossary.md) covers protocol and security terms.
  A glossary entry here is a plain restatement of that row and cites it in
  `source`. Where the two disagree, the entry is wrong.
- **`code`** points to the package or type that implements the term, so a
  reader can go from the slide to the source. Find it with `rg` or graphify;
  leave it empty rather than guess.
- **Clickable everywhere.** Every `[[term]]` renders as a term that opens a
  popover with `short` and links to the glossary slide. Only the first use
  on a slide is marked, to keep slides readable; in a title it keeps the
  title's colour and is only underlined. A term is reachable with Tab and
  opens with Enter or Space; focus moves into the popover and Esc returns it
  to the term. "Open in the glossary" leaves a history entry behind, so the
  browser's Back returns to the slide the reader left.
- **Link on every slide, not once per deck.** A reader jumps around, so a
  technical term is linked on each slide it appears on, titles, tables and
  scene captions included. Link it only where it carries its glossary sense:
  "share" the verb, "report" meaning "say", or a "block store" are not the
  terms `share`, `report` and `block`. SVG labels and badges in a scene render
  as plain text, so the link goes in the caption.
- **Conflicts** are a `clash` text on the term: the other senses of the same
  word — in another RFC, in NFS/SMB, in the kernel (ext4's journal is not
  DittoFS's journal). A deck lists which clashes it shows in `deck.json`.

## Writing a deck's HTML

`index.html` is structure only. Every text node is filled by the runtime:

| Attribute | Filled with |
| --- | --- |
| `data-i18n="key"` | one string |
| `data-i18n-list="key"` | an array of strings as `<li>`; add `data-fragments` to reveal them one by one |
| `data-i18n-table="key"` | an array of rows; the first row is the header |
| `<aside class="notes" data-i18n="….notes">` | the speaker notes, with the citations |
| `<section data-term-list="known\|new\|conflicts">` | generated slides from `deck.json`'s `terms`; `data-per-slide` pages them, `data-style="chips"` shows names only |
| `<section data-glossary data-per-slide="4">` | the glossary slides: every term the deck's strings mention |
| `<div class="scene" data-scene="name">` | an animated scene from `scenes/index.js`, with `<ol class="captions" data-i18n-list="…">` inside, one caption per step |
| `<div class="scene side">`, `<div class="scene side half">` | captions beside the drawing instead of under it, for a scene with many steps; `half` gives the captions the wider half, for a tall, narrow drawing |
| `<div class="scene one">` | captions under the drawing, only the current step's visible, all sized to the tallest; for captions written as full sentences that would not fit as a list |
| `<section data-quiz="key" data-answer="a\|b\|c">` | a quiz slide: kicker, question, three options and their explanations, built from the strings under `key` (see [Quizzes](#quizzes)); only the notes `<aside>` goes inside |

Layout helpers in `shared/deck.css`: `.kicker`, `.lead`, `.cols`
(`.wide-left`), `.cols3`, `.card` (`.good`, `.bad`, `.accent`,
`.warn-card`), `.stats`/`.stat`, `.analogy`,
`ol.layers` (two columns), `pre.code-sm`. Code samples sit in the HTML, not in
the strings: they are not translated.

### Quizzes

A quiz is a slide with one question and three possible answers. The reader
may click one, or skip the slide with → like any other. A click is final: it
marks the right answer green and the picked one red if it was wrong, shows
under **every** option why it is right or wrong, and names the right letter.
The pick survives a language switch. The kicker ("Check yourself"), the hint
and the verdict are UI strings in `i18n/<lang>.json` under `quiz`; the deck
writes only the question, the options and the explanations.

```html
<section id="quiz-ack" data-quiz="quizAck" data-answer="b">
  <aside class="notes" data-i18n="quizAck.notes"></aside>
</section>
```

```json
"quizAck": {
  "question": "A client's WRITE has been acknowledged. Where are its bytes now?",
  "options": [
    "In the bucket: the acknowledgement waits for the upload.",
    "In the [[journal]] on the node's local disk.",
    "Only in the client's memory, until the next COMMIT."
  ],
  "why": [
    "The upload runs later, in the background, so the client never waits for the bucket.",
    "The [[journal]] is where a write lands first, and the bytes stay there until the [[syncer]] has uploaded them.",
    "The node already holds the bytes when it answers, so they are no longer only on the client."
  ],
  "notes": "RFC 1 §2, §4.1."
}
```

- **How many and where.** One to three per deck, by length: one for a short
  deck, three for a long one. Spread them through the body, each right after
  the section it checks, so it tests what the reader has just seen while it
  is still fresh. One at the end, before the glossary, is allowed but not
  required. Never two in a row, never in the opening.
- **The right letter is in the HTML** (`data-answer`), not in the strings, so
  no translation can move it. Options keep their order in every language.
  Vary the right letter across a deck's quizzes.
- **Test understanding, not recall.** Ask what happens in a case — a crash, a
  second client, a full disk — or why a rule exists, not which section
  number or which name. The answer must follow from the slides before the
  quiz, never from the speaker notes or a later slide.
- **Wrong options are plausible.** Each is a mistake a reader could really
  make: the misconception the deck sets out to correct, a term from a clash
  slide in its other sense, the before-the-RFC behaviour. No joke options,
  no "all of the above", no option wrong only by a technicality or a word.
  The three are about the same length, so the longest is not a giveaway.
- **Every option has an explanation**, one or two full sentences that say
  *why* — the mechanism or the rule — not just "correct" or "wrong": the
  colours already say that. A wrong option's explanation names what is
  really true. The explanations follow the same correctness rules as any
  slide: their claims are cited in the quiz's notes.
- **Options are plain text.** A `[[term]]` in an option renders without a
  link, since a link inside a button would nest one control in another. Link
  the term in the question or in an explanation instead.
- **`?check=1` measures a quiz answered**, with every explanation shown, and
  flags one without a question, three options, three explanations and a
  valid `data-answer`. PDF export prints it answered too.

`deck.json` holds the RFC's number, path, status and the commit the deck was
built from, and the term ids for the three opening slides.

## Deck structure

### Opening (fixed, every deck)

1. **The RFC in two sentences.** What it is, and why it exists. If it does
   not fit in two sentences, that is a finding about the RFC worth noting.
2. **Terms you already know.** Glossary terms from earlier RFCs this one
   builds on, each a link, no redefinition.
3. **New terms.** The terms this RFC introduces, with their `short` text.
4. **Terms that clash.** Words this RFC uses in a sense that differs from
   another RFC, the protocols or common usage. Skip the slide only if there
   are none, and say so on slide 3.

### Body (a guideline, adapted per RFC)

The order gives the decks a consistent shape; an RFC that does not need a
section skips it rather than padding it.

The slides before "How it works" — the opening, the problem, what the RFC
promises — speak in outcomes a newcomer already understands: what is fast,
what survives a crash, what is refused. Mechanisms such as keys, codecs or
folds wait for the slides that explain them. An RFC's "What this RFC
promises" list is rewritten this way, not translated.

1. **The problem.** What breaks or costs too much without this RFC, with a
   concrete case.
2. **One running example.** A single operation followed through the whole
   deck — "a 4 KiB WRITE from an NFS client" — before and after the RFC.
3. **Concrete numbers.** Sizes, counts and latencies from the RFC, not
   abstractions.
4. **How it works.** The mechanism, animated where data moves.
5. **When things fail.** Crash, restart, full disk, lost network: what the
   RFC guarantees in each, animated step by step. Often the most important
   section.
6. **What changes for whom.** Users and operators, developers, store authors.
7. **Rules and non-goals.** The RFC's MUST and MUST NOT in plain words, and
   what it explicitly does not do.
8. **Open questions.** The points the RFC leaves open.
9. **Glossary.** Every term the deck used, with `long` text.

Between these sections sit one to three [quizzes](#quizzes), each after the
section it checks; where exactly is the author's call.

## Correctness

A simplification that is false is worse than no deck. These rules are what
stand between the two.

- **The RFC is the source of truth; the deck is derived.** Nothing in a deck
  goes beyond what the RFC says. Where the RFC is ambiguous or silent, the deck
  says so instead of resolving it.
- **Every claim cites its section.** Each slide's speaker notes list the RFC
  sections (`§4.2`) its content comes from. A slide with nothing to cite is
  either the deck's own framing (say so) or invented (remove it).
- **Analogies are labelled as analogies** and say where they stop holding.
  "Like a queue at the post office — except that entries can be rewritten
  before they are served."
- **Record the RFC version.** `deck.json` stores the RFC's path and the commit
  the deck was built against. When the RFC changes after that commit, the
  deck is stale until reviewed against the diff.
- **Explain the design, not its progress.** Decks teach how the system
  works. They do not call an RFC a draft, and they do not track the work:
  no branch names, no plans, epics, estimates or man-days, no "pieces 1 to 7
  merged". That changes every week and teaches nothing about the system.
  Where today's code behaves differently in a way a reader would notice,
  a deck may say so once, as behaviour ("today a cold read fetches a byte
  range of the whole block"), never as a branch or a schedule. `deck.json`
  keeps the RFC's status as data only.
- **Review adversarially before calling a deck done.** A separate reviewer —
  a person, or an agent given only the RFC and the deck — looks for
  simplifications that became false, citations that do not support their
  slide, and claims the RFC does not make. Not the author re-reading.
- **Read it cold.** A reviewer who has never read the RFC — a person, or an
  agent given only the deck's English strings in slide order, without the
  RFC and without the speaker notes — reads the slides front to back and
  lists every sentence they cannot follow from the slides before it: a
  term, a name, a table or a mechanism not yet shown, or a pointer to the
  RFC. Neither the adversarial review nor the language review catches
  this, because both reviewers know the RFC: a promise slide that said
  "costs what the RFC's table of keys states", six slides before the keys
  appear, passed both. Fix each finding in English first.
- **Translations keep the meaning.** A translation is checked against the
  English for dropped qualifiers ("usually", "at most", "on a single node")
  — the words a translator most often loses and the ones that carry the rule.
- **Review the language last, separately from the facts.** Once the
  content is final, a copy editor fluent in each language reads every
  string — a person, or an agent briefed as a native-level editor, one per
  deck plus one for the glossary so no two touch the same file. The
  correctness review does not catch this: in the first nine decks it passed
  hundreds of strings that were right and unreadable. What it looks for:
  - calques of English verbs ("report" as *riportare*, "hold" as *tenere*,
    "name" as *nominare*, "supersede" as *superare*) and false friends
    (*evidenza*, *allarmare*), plus words that do not exist (*ritentativo*);
  - English clause chains — colons and semicolons — carried over instead of
    rewritten as sentences of that language;
  - a word, image or name used before the deck introduced it; that is fixed in
    English first, and the English is part of what this pass may change;
  - one gender and one translation per term across decks and the glossary
    (*l'RFC* / *o RFC*, *un open*, *il lease*), matching glossary `name`s;
  - a bare `[[id]]` in a sentence renders the glossary `name`, which may not
    fit the grammar around it ("I nodo gateway"): give it surface text;
  - meaning drift: a number, a qualifier or a MUST lost in the rewrite.
  - a caption that breaks "Write full sentences that stand on their own":
    telegraphic, a noun phrase, or leaning on a word the slide never
    explained. The fix is a fuller sentence, in English first.

  Rewrites change lengths, so `?check=1` runs again in every language after
  it. A native speaker's read stays worth having on top: an agent pass
  catches the pattern, not every sentence.

## Making a deck

1. Read the RFC in full, RFC 0's glossary and the RFCs it depends on (the
   [index](../../internals/rfcs/rfc-index.md) has the dependency graph).
2. List the terms: known, new, clashing. Add missing ones to `glossary/`
   with their `source` and `code`.
3. Write the English outline against the structure above, with the citations
   in the notes.
4. Pick the one or two mechanisms that need animation and write their scenes,
   and the one to three points worth a quiz.
5. Build the slides, then run `?check=1` in English.
6. Translate to `it` and `pt-BR`; run `?check=1` in each.
7. Adversarial review (see [Correctness](#correctness)); fix; record the RFC
   commit in `deck.json`.
8. Cold read (see [Correctness](#correctness)); fix, in every language.
9. Language review, last, once the content no longer changes (see
   [Correctness](#correctness)); then `?check=1` again in every language.
10. Add the deck to `learn-dittofs/decks.json` under its section, and give a
   new section a `name` and a one-sentence `blurb` under `sections.<id>` in
   `learn-dittofs/i18n/<lang>.json`. A section may hold several decks, shown
   as numbered parts; `sections.<id>.parts` names them (adapters: the model,
   then SMB). A later part links the earlier one's terms instead of
   re-explaining them. A section is named for what it explains
   ("The journal"), not for its RFC; the RFC number is only the card's
   footnote.
