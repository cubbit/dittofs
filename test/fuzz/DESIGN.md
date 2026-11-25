# fuzz — a generic harness for operation-sequence fuzzing

**Audience:** anyone writing a fuzz target for a DittoFS component (the journal and the engine
first), and anyone triaging a failure one of those targets reports.

This document designs the shared harness of the v1.0 fuzz-testing roadmap: the Go package that
turns the byte vector the Go fuzzer supplies into a sequence of operations, prints it, runs it
against the component under test and a reference model side by side, and checks the result
after every step. The per-component targets build on it; they are not designed here.

The rules every target follows ("How every fuzz test here is written" and "The oracle" in the
roadmap epic) are restated in §1 as requirements on the harness. Where this document and those
rules differ, the rules win and this document is wrong. §6.2 states the one exception.

This harness is for operation sequences. A target of a decoder of untrusted bytes (a wire codec,
a token parser, a key file), which checks how the decoder refuses what it must refuse, is outside
it: it is a plain `testing.F` target under the repository's rule for decoders (CLAUDE.md, "Fuzz").

## Summary

- **The test declares, the harness decodes.** A target declares a table of operations: a name,
  a few parameters each with a small finite domain, and two functions that run the operation
  against the real component and against the model. The harness owns nothing about any
  component.
- **Fixed stride.** Every operation occupies the same number of bytes, whatever its opcode, so
  a mutated byte changes one operation or one of its parameters and never re-frames the rest
  of the input.
- **Every input is valid.** Bytes wrap into domains with a modulo; a short tail reads as zeros;
  extra bytes past the step cap are ignored. The decoder has no error path.
- **One trace, three uses.** The decoded program prints as
  `write(file=2, off=4096, len=512, pattern=0x3a)`. The same text is the failure message, the
  seed format (`f.Add(fuzz.Seed(target, "…"))`) and the replay format, and it round-trips.
- **The model is a set of states.** Without faults the set has one member and the comparison is
  exact. The model takes the real component's result and continues from the state it allows;
  a read-back after a fault narrows any choice the contract left open. An empty set is the
  bug. A model keeps durable and volatile state apart only once a fault needs it; until then a
  single state is enough (§6.2).
- **Faults are operations.** Arming a torn write, failing a sync, crashing and restarting, a
  remote error or a delay is a row of the same table, picked by the same opcode byte.
- **Coverage is checked, not promised.** The target names the interfaces it implements and
  the fault sources of its collaborators; a reflection check fails the target when a method or
  a fault has neither an operation nor a written reason for its absence.
- **Generic invariants are the harness's.** No panic, no step past its bound, no goroutine left
  at the end. Component invariants are hooks the target adds.

## 1. Requirements

From the issue and the roadmap epic, as they bind the harness:

| # | Requirement | Where it is met |
|---|---|---|
| R1 | Operations and their parameter domains are declared by the test, not the harness. | §3 |
| R2 | Any `[]byte` decodes to a legal sequence; nothing is rejected. | §4.2 |
| R3 | A mutated byte changes at most one operation or one parameter. | §4.1 |
| R4 | Decoding is deterministic, and so is the run. | §4.4, §6 |
| R5 | The decoded sequence prints as a human-readable trace that goes into the failure message. | §5 |
| R6 | Each step runs against the component and the model; after every step the model checks the component's result and one read-back hook supplied by the test checks observable state. | §6 |
| R7 | With faults, the model checks that the real component's outcome is one the contract allows; a read-back after the fault narrows the set to the component's state, and candidates persist only where no read-back can tell them apart. | §6.2 |
| R8 | The table covers every method of every interface the component implements and every fault injector of its collaborators; an omission is listed with why. | §7 |
| R9 | The harness checks no panic, no deadlock (bounded step), no goroutine leak; the test adds its own invariants. | §8 |
| R10 | Seeds are written as readable operation lists and encoded by a helper, so they survive a change of encoding. | §5.2 |
| R11 | No mocks or stubs; faults only as wrappers around real implementations; the model is a verified in-memory implementation. | §9 |
| R12 | The package has its own unit tests and a toy example that fails on a planted bug with a readable trace. | §10 |

## 2. Where it lives

```text
test/fuzz/
  DESIGN.md            this document
  fuzz.go              Target, Op, Param, Args, Exec, Fuzz
  domain.go            the parameter domains (§3.2)
  decode.go            bytes → Program (§4)
  trace.go             Program → text, text → Program, Seed (§5)
  oracle.go            the step loop and the state set (§6)
  coverage.go          the interface and fault coverage check (§7)
  invariant.go         panic, step bound, goroutine leak (§8)
  clock.go             the deterministic clock (§6.4)
  fault.go             Switch: the armed-fault table fault wrappers consult (§6.3)
  *_test.go            the package's own tests (§10.1)
  example/             a toy component, its model and its target (§10.2)
```

The import path is `github.com/cubbit/dittofs/test/fuzz` and the package name is `fuzz`. It is
imported only from `_test.go` files: production code never depends on it. It carries no build
tag, so `go vet` and the linter see it and lint-go's tag list does not change.

The targets themselves live next to the component they fuzz, in the component's package, so
they can reach `export_test.go` hooks: `pkg/journal/fuzz_test.go` and `pkg/engine/fuzz_test.go` on
`feat/storage-v2`, where both are written (`develop` has the packages under `pkg/block/`). Their corpora
live where Go puts them, `testdata/fuzz/<FuzzName>/`.

### 2.1 The journal's model harness

`feat/storage-v2` already has a model-based harness for the journal,
`pkg/journal/journaltest/model`: a step decoder for the fuzzer's bytes, a readable corpus text
(`corpus.go`), shrinking to a short corpus file (`shrink.go`), crash steps, and `Compare` after
every step. `FuzzModel` (`journaltest/model/fuzz_test.go`) and `FuzzModelDataPath`,
`FuzzModelRepack` and `FuzzModelForget` (`pkg/journal/conformance`, with their own step decoder)
run on it. Its model is driven by
the journal's result (§6.2), which is the form this harness takes.

The journal target keeps that model and drives it through this harness: its operations become a
`Target`'s table, its corpus files become text seeds (§5.2), and its own step decoder and corpus
format are retired once the journal target runs here. Its shrinking moves into the harness
(§5.3). So the journal has one harness and one seed format, and the model is not written twice.

## 3. Declaring a target

### 3.1 The shape

A target is parameterised by the type of the real component `R` and the type of the model `M`:

```go
type Target[R, M any] struct {
    Name     string
    Ops      []Op[R, M]

    // NewReal builds the component under test with its real collaborators, wrapped in
    // every fault injector it has, all reading env.Clock and env.Faults. Cleanup goes
    // through env.Cleanup, and runs before the goroutine-leak check.
    NewReal  func(env *Env) (R, error)
    NewModel func() M

    // Compare is the one read-back hook: after a step, does the real component's
    // observable state match this candidate model state? (§6)
    Compare  func(step Step, got Result, real R, model M) error

    // Key identifies a model state, so equal candidates collapse (§6.2).
    Key      func(M) string

    Invariants []Invariant[R]    // component-specific, checked after every step (§8)

    Covers     []Surface         // interfaces implemented and fault sources wired (§7)
    Uncovered  []Omission        // what is deliberately left out, and why (§7)

    MaxSteps   int               // default 64
    StepBound  time.Duration     // default 10s; a deadlock backstop, not a timing (§8)
}

type Op[R, M any] struct {
    Name   string
    Weight int                   // relative frequency; default 1 (§4.3)
    Covers []string              // "journal.LocalStore.WriteAt", "fault:fsync.fail"
    Params []Param

    Real  func(env *Env, r R, a Args) Result
    // Model takes the real component's result and returns the model states the
    // contract allows after it, or an error when the contract does not allow that
    // result. One state when the result pins it; several only when it does not (§6.2).
    // A fault step's Real reads back what the fault may have changed and returns it
    // in its Result, so that result pins the state.
    Model func(m M, a Args, got Result) ([]M, error)
}
```

`Result` is a small comparable value: the operation's return values reduced to what the
contract defines (bytes read, a size, an error *class* such as `ErrNoEntity`, never an error
string), produced by the target's own `Real`. For a fault step it also carries what `Real` read
back of the state the fault may have changed (§6.2), reduced to what the model needs to pick
one state.

`Env` is what the harness hands the real side: the deterministic `Clock` (§6.4), the fault
`Switch` (§6.3), a `TempDir` for components that need a real filesystem, `Cleanup`, and the
current step number for messages. `Env` is built fresh for every input.

### 3.2 Parameter domains

A parameter is a name, a domain and a format. Domains are finite and small on purpose: the
epic asks that two writes to the same block, or a read across a flush boundary, be likely.

| Constructor | Values | Bytes |
|---|---|---|
| `Enum("file", 0, 1, 2)` | the listed values | 1 |
| `Range("len", lo, hi, step)` | `lo, lo+step, …, hi` | 1 if ≤ 256 values, else 2 |
| `Choice("off", 0, 4095, 4096, 4097, 1<<20-1, 1<<20)` | listed values: boundaries and their neighbours | 1 if ≤ 256 values, else 2 |
| `Bool("sync")` | false, true | 1 |
| `Pattern("pattern")` | 0x00–0xff, a payload generator seed (§3.3) | 1 |
| `Named("fault", "torn", "fail", "delay")` | the listed names, printed as names | 1 |

A domain with more than 65 536 values is refused when the target is declared: such a domain
is a sign the test is not choosing its values. A domain is fixed at declaration; a parameter
whose legal values depend on state (a handle that exists only after an open) takes a small
index into the state the operation interprets — `fd=1` is "the second open handle, or a closed
one if fewer are open" — never a value that could be illegal.

### 3.3 Payloads

Input bytes never become file data directly: a 4 KiB write would need 4 KiB of input and would
move the stride. A write takes a `Pattern` byte and a length, and the data is generated:

```text
data[i] = mix(pattern, absoluteOffset + i)
```

where `mix` is a fixed, cheap hash. Because each byte depends on its absolute offset, data
written to the wrong place, shifted by a block, or read back from another write never compares
equal by accident. The model stores the pattern and offset of each extent, not the bytes.

## 4. Decoding

### 4.1 Layout: fixed stride

```text
 step 0                  step 1                  step 2
 ┌──┬──┬──┬──┬──┬──┐    ┌──┬──┬──┬──┬──┬──┐    ┌──┬──┬──┬──┬──┬──┐
 │op│p0│p1│p2│p3│ ·│    │op│p0│p1│p2│ ·│ ·│    │op│p0│ ·│ ·│ ·│ ·│ …
 └──┴──┴──┴──┴──┴──┘    └──┴──┴──┴──┴──┴──┘    └──┴──┴──┴──┴──┴──┘
   stride = 1 + max over ops of (sum of its parameter widths)
```

Every step consumes exactly `stride` bytes: one opcode byte, then the operation's parameters in
declaration order, then padding that the operation ignores. Because the stride does not depend
on the opcode, changing the opcode byte of step *k* changes step *k* only; changing a parameter
byte changes that parameter only. There are no length prefixes and no variable-width fields.

The Go mutator also inserts and deletes bytes, which shifts every later step by a partial
stride. That still decodes to a legal program (R2); it is simply a non-local mutation, the
mutator's choice rather than the encoding's.

### 4.2 Every input is valid

- **Opcode:** `opTable[b]`, a 256-entry table built from the weights (§4.3), so every byte
  value names an operation.
- **Parameter:** `domain[v % len(domain)]`, with `v` read little-endian over the parameter's
  width.
- **Short tail:** a final partial step is padded with zero bytes. An empty input is the empty
  program, which runs `NewReal`, the invariants and the teardown, and is a test in itself.
- **Long input:** steps past `MaxSteps` are ignored. The fuzzer gains nothing from inputs
  longer than `MaxSteps × stride`, and the step cap keeps a single input's cost bounded.

### 4.3 Weights

Faults should be rarer than writes, and a crash every other step makes deep states
unreachable. An operation's `Weight` sets how many of the 256 opcode values map to it,
apportioned by largest remainder, every operation getting at least one. That is impossible past
256 operations, so a target declaring more is refused when it is built, the same way §3.2 refuses
an oversized domain; weights may sum past 256, since apportioning scales them. The table is computed
once per target and is a pure function of the declaration, so it is part of the encoding:
changing a weight changes which bytes mean which operation (§5.2 covers what that does to the
corpus).

### 4.4 Determinism

`Decode(target, data)` is a pure function. Running the program is deterministic as long as the
target follows §6.4: the harness runs every step on the calling goroutine, in order, with an
injected clock, and the component's background work is driven by operations rather than by
timers. `fuzz.Deterministic(t, target, data)` runs an input twice and fails if the two traces
of results differ; the seed replay uses it (§10.1).

A step that hangs on the calling goroutine cannot be interrupted, and a watchdog goroutine
cannot fail the test, since `FailNow` must run on the test's goroutine. So a step past its bound
is not reported like a panic: the watchdog dumps every goroutine's stack with the trace and ends
the process (§8). The Go fuzzer then records the input as a crasher, as it does for a panic on a
goroutine the component started.

## 5. The trace

### 5.1 Format

A program prints one step per line:

```text
#00 write(file=2, off=4096, len=512, pattern=0x3a)
#01 fault(op=fsync, kind=fail)
#02 sync(file=2)                                       -> ErrIO
#03 crash()
#04 read(file=2, off=4096, len=512)                    -> 512 bytes, pattern 0x3a@4096
```

The left side is the decoded call, built from the operation name and each parameter's value in
its declared format (decimal, hex for patterns, names for `Named`). The right side, present
once the step has run, is the real component's `Result` in its `String` form. Parameter
padding is never printed.

On a failure the message carries the whole trace up to and including the failing step, then
what failed:

```text
fuzz: FuzzJournal: step #04 read: no allowed state matches
  got:  512 bytes, pattern 0x00@4096
  closest candidate: model: read of a durable write returned other bytes (write #00)
  candidate states before the step: 1
  seed: fuzz.Seed(target, `write(file=2, off=4096, len=512, pattern=0x3a)
fault(op=fsync, kind=fail)
…`)
```

### 5.2 Seeds

The trace's left side is also the seed language. `fuzz.Seed(target, text) []byte` parses a
program and encodes it, choosing for each opcode and parameter the smallest byte value that
decodes to it; `fuzz.Parse` returns the program instead. A value outside its domain is an
error: the encoder is strict where the decoder is lenient, because a seed with an impossible
value is a mistake in the test.

Seeds are written as text in the target:

```go
func FuzzJournal(f *testing.F) {
    for _, s := range []string{
        "write(file=0, off=0, len=4096, pattern=0x01)\nsync(file=0)\ncrash()\nread(file=0, off=0, len=4096)",
        "write(file=1, off=4095, len=2, pattern=0x02)\nflush()\nevict()\nread(file=1, off=4094, len=4)",
    } {
        f.Add(fuzz.Seed(target, s))
    }
    fuzz.Fuzz(f, target)
}
```

so they keep their meaning when the table is reordered, a weight changes or a parameter is
added. Crashers the fuzzer finds are bytes in `testdata/fuzz/<FuzzName>/` and do not survive
such a change. Two rules contain that:

- When a crasher is committed, its trace is added as a text seed too, so the case outlives
  its bytes.
- A change to a target's table re-checks the committed crashers: `go test -run FuzzXxx` prints
  the trace of every corpus entry when `DITTOFS_FUZZ_TRACE=1` is set, and a corpus file whose
  trace no longer reproduces its case is replaced by its text seed.

The round-trip `Parse(Print(Decode(b)))` equals `Decode(b)` for every `b`, and
`Decode(Seed(text))` prints as `text` (after normalising whitespace) for every valid `text`.
Both are tested (§10.1).

### 5.3 Shrinking

The Go fuzzer minimises bytes and knows nothing of steps, so a failing program can stay
`MaxSteps` long. On a failure the harness shrinks the program by steps: it drops chunks of
steps, halving the chunk size down to one and starting again while any drop still fails the same
way, then moves each kept parameter toward its domain's first value the same way. It stops after
a fixed number of attempts, never a wall-clock deadline, so the same failure shrinks to the same
program on every run (R4). The message prints the shrunk trace and its text seed beside the
original. `journaltest/model/shrink.go` drops chunks the same way and then halves each step's
length (§2.1); it also stops at a 5 minute deadline, which the harness does not keep.

Shrinking runs only for a failure the harness reports in its own process. A hang (§4.4) and a
panic on a goroutine the component started (§8) end the process first, so their crashers arrive
unshrunk. One is shrunk by hand, or by replaying each candidate program in a subprocess per
attempt and keeping the ones that still end the process the same way.

## 6. The oracle

### 6.1 The step loop

```text
real    := NewReal(env)
states  := { NewModel() }
for each step in Decode(data):
    got     := op.Real(env, real, args)          // bounded, panics recovered (§8)
    next    := {}
    for m in states:                             // in Key order
        cands, err := op.Model(m, args, got)     // the states allowed after got
        if err != nil: keep err as this candidate's failure; continue
        for c in cands:
            if Compare(step, got, real, c) == nil:
                next[Key(c)] = c
    if next is empty: fail with trace and the closest candidate's error
    states  := next
    check Invariants(real)
teardown; check goroutines (§8)
```

Candidates are kept and visited in `Key` order, never in map order, so the closest candidate's
error and the order of `Compare`'s read-backs are the same on every run (R4).

`Model` checks the step's result; `Compare` is the target's only read-back hook. It compares
whatever observable state the target chooses to read back (data in the touched range, sizes, listings).
Reading back must use operations the contract defines as side-effect free; if a read-back can
change state (a read that hydrates a cache), it is a step of its own instead, so the model sees
it.

### 6.2 Faults and the state set

Without faults `Model` returns one state, `states` stays a singleton, and the comparison is
exact, as the epic requires.

**The model is driven by the observation.** `Model` takes what the real component returned,
checks that the contract allows it, and continues from the state that result implies. Where the
contract leaves a choice to the component (which unsynced writes survive a crash, whether a
failed operation took effect), the model accepts any allowed outcome and continues from the one
the component took. A model that enumerated every allowed outcome up front would keep every
subset of the volatile changes after a crash: 2^n candidates, so 7 unsynced writes before a
`crash()`, an ordinary program, would pass the cap. Enumeration stays available for an
operation whose result does not pin the state.

**A fault step reads back what it left unknown.** The `Real` of a fault step reads back the
state the fault may have changed and returns it in its `Result`, as `journaltest/model` compares
every share after `Reopen` before any other call on a file the crash left unknown. `Model` then
sees which allowed outcome the component took, checks it, and returns that one state, so the set
narrows at the fault step itself, as the epic's oracle says, without enumerating the subsets.

**The exception to the epic.** The epic says the model "narrows its state to the one the real
component chose, so the comparison is exact again from the next operation on". Where no
read-back can tell two candidates apart (a state the contract keeps unobservable until a later
step), both stay until a later step separates them. This is the one place this document departs
from the epic's oracle sentence. Driving the model by the observation, rather than having it
compute every outcome up front, follows the same oracle in another order and is not a second
exception. Two limits keep it tractable:

- `Key` collapses equal candidates, so the set holds distinct states only.
- The set is capped (default 64). With the observation driving the model, the cap bounds only
  what is truly ambiguous, not every crash. A step that would exceed it fails the run with the
  trace, as a target bug: the fix is an earlier read-back or smaller domains, not a larger cap.
  `journaltest/model` has a bound of the same kind: 16 undecided releases, marks and settled
  markers per file after a crash (`maxUncertain`, `crash.go`).

An outcome outside the set — durable data lost, a write half applied where the contract
forbids it, data that was never written — leaves `next` empty and is reported.

**The durable/volatile split is not required up front.** A model needs more than one truth
only where its component can legitimately end up in more than one state. A target whose
component has no crash or sync fault yet, or none whose outcome is ambiguous, keeps a single
state, its `Model` returns one state, and the set stays a singleton. The harness is the same
either way. A model gains the split when a fault that needs it is added to the target, not
before.

### 6.3 Fault operations and the fault switch

A fault is an operation like any other: `fault(op=fsync, kind=fail)` arms a fault,
`crash()` crashes and restarts, `delay(ms=500)` advances the clock. Arming is separate from
firing, so the input decides both what fails and when.

The harness provides the one piece every fault wrapper needs and no more: `fuzz.Switch`, a
table of armed faults keyed by injection point (`"fsync"`, `"remote.put"`). A wrapper asks
`env.Faults.Take("fsync")` at its injection point; the switch answers the armed fault, if any,
and disarms it. A fault can be armed for the next call or the *n*th call (n from a small
domain). The switch also keeps the list of points that were armed but never reached, which
the trace prints, so an armed fault that the program never exercises is visible.

The fault wrappers themselves — a filesystem that tears and loses unsynced writes on crash, a
remote store that errors or delays — belong to the packages whose interfaces they wrap
(kind 3 in RFC 29 §4.11). They are built by the component tasks, not here. Each wrapper
exports the list of its injection points and fault kinds, which is what §7 checks against.

### 6.4 Time and background work

`fuzz.Clock` implements the `Now()` clock interfaces the components already inject
(`journal.Clock` is one) and moves only when an operation advances it. A delay fault advances
it; nothing sleeps. A component's background work (flush, sync, eviction, GC) runs only when
an operation in the program asks for one step of it, through the component's own step method
or an `export_test.go` hook, set per instance. A component that starts a goroutine the program
does not drive is not yet fuzzable by this harness; making it steppable is part of that
component's task.

Interleaving is fuzzed the same way: an operation can start a call that parks at a hook in the
component, and a later operation releases it, so the input chooses the interleaving rather
than the scheduler. The harness supplies the park-and-release primitive; which hooks exist is
the component's business. This is the one place a step leaves a goroutine behind, and the
parked call must be released or abandoned by teardown or §8's leak check reports it.

## 7. Coverage of interfaces and faults

The issue requires the table to expose every method of every interface the component
implements and every fault injector its collaborators have, and anything left out to be listed
with why. The harness makes that mechanical:

```go
Covers: []fuzz.Surface{
    fuzz.Interface[journal.LocalStore](),
    fuzz.Faults("fs", faultfs.Points()),
    fuzz.Faults("remote", faultremote.Points()),
},
Uncovered: []fuzz.Omission{
    {Name: "journal.LocalStore.Stats", Why: "metrics only; no observable state the model owns"},
},
```

`fuzz.CheckCoverage(t, target)` enumerates the methods of each interface by reflection and the
points of each fault source, and fails when one is named by no operation's `Covers` and by no
`Omission`. It also fails on the reverse: an `Omission` or a `Covers` entry naming something
that does not exist. `fuzz.Fuzz` runs the check before `f.Fuzz`, so it runs on every plain
`go test`. A method added to an interface, or a fault point added to a wrapper, then breaks the
target until someone decides whether to fuzz it, which is the point.

## 8. Generic invariants

The harness checks these itself, for every input:

- **No panic.** Each step runs under `recover`; a panic becomes a failure with the trace and
  the stack. A panic on a goroutine the component started cannot be recovered and kills the
  process; the Go fuzzer still records the input as a crasher.
- **No deadlock.** Each step runs with `StepBound` as a backstop. The clock is the injected one,
  so the bound measures nothing about the component's timing: it exists only to turn a hang
  into a failure with a trace. Where the bound is hit, a watchdog goroutine prints the trace and
  every goroutine's stack and ends the process (§4.4); the fuzzer records the input as a crasher.
  This is the one wall-clock wait in the harness, and the code says so at the site.
- **No goroutine leak.** After teardown, the goroutines that did not exist before `NewReal`
  must be gone. The check snapshots goroutine stacks before and after, ignores the runtime's and
  the testing package's own, and polls the condition until it holds or the bound expires —
  waiting on the condition, not sleeping a fixed time.

Component invariants are `Invariant[R]` hooks (`func(env *Env, r R) error`) checked after every
step: durability after a sync, bounded growth, internal consistency the component exposes
through its own check method.

## 9. The test rules this follows

- **No mocks or stubs.** The real side is the real component with real collaborators: a real
  filesystem under `env.TempDir`, the memory remote store, the memory metadata store.
- **Faults are wrappers** around those real implementations (§6.3), firing only when armed.
- **The model is a verified in-memory implementation** of the component's interface. A
  target's model passes the interface's conformance suite, where the interface has one, in its
  fault-free mode: that is what makes it a reference rather than a second guess. Where the
  interface has no suite yet, the component task writes one first (CLAUDE.md, "Writing tests").
- **The clock and the input are inputs**, not doubles.
- **Nothing global.** Every input builds its own `Env`, temp dir, clock and switch; no package
  variable changes.

## 10. Acceptance

### 10.1 The package's own tests

| Test | What it proves |
|---|---|
| `TestDecodeAnyBytes` | A sweep of inputs from a seeded random source, plus the edge cases (empty, one byte, a stride minus one, a stride plus one, longer than `MaxSteps × stride`, all 0x00, all 0xff), decodes without error to a program of the expected length. |
| `TestDecodeDeterministic` | Decoding the same bytes twice gives the same program; so does decoding on a fresh target built from the same declaration. |
| `TestMutationLocality` | For sampled inputs, every byte position and several values, changing that byte changes at most one step, and only the opcode or one parameter of it. |
| `TestTraceRoundTrip` | `Parse(Print(Decode(b))) == Decode(b)` for sampled `b`, and `Decode(Seed(text))` prints as `text` for a set of programs. |
| `TestSeedRejectsOutOfDomain` | A seed with a value outside its domain, an unknown operation or an unknown parameter is an error naming it. |
| `TestWeights` | The opcode table gives every operation at least one byte and is proportional to the weights within one; a declaration of more than 256 operations is refused. |
| `TestStateSetNarrows` | With a model that returns two states for a result, a read-back matching one narrows the set to it, one matching both keeps both, one matching neither fails; a result the model refuses fails; candidates are visited in `Key` order. |
| `TestInvariantsCatch` | A step that panics and a step that leaves a goroutine each fail with the trace. A step that blocks past its bound, run in a subprocess, ends that process with the trace and every goroutine's stack on its output. |
| `TestShrink` | A failing program of `MaxSteps` steps with one step that matters shrinks to that step, and the shrunk text seed fails the same way. |
| `TestCoverageCheck` | A target missing an interface method, missing a fault point, or naming a non-existent one fails the check with the name. |

Each of these is verified by breaking what it guards (CLAUDE.md, "Verifying a change").

### 10.2 The example target

`test/fuzz/example` holds a toy component, its model and its target:

- **The component:** a small key–value log that appends records to a file, keeps an index in
  memory, and has `Put`, `Get`, `Delete`, `Sync`, `Compact`, and `Close`/`Open` for restart.
  It writes through a filesystem wrapper with two fault points, `write` (torn) and `fsync`
  (fail), and a crash that drops unsynced bytes.
- **The model:** a map with each key's value marked durable or volatile, taking the component's
  result on `crash()` and on a failed `fsync` and continuing from the state it allows.
- **The planted bug:** a constructor option, not a package variable, that makes recovery stop
  one record early. `TestExampleFindsPlantedBug` runs `fuzz.Exec` over inputs from a seeded
  random source and asserts that one fails within a fixed budget, and that its trace names the
  lost record. The same target without the option passes the same budget.
- **`FuzzExample`** is the template a component target copies: seeds as text, `fuzz.Fuzz`, a
  coverage declaration with one `Omission`.

## 11. Running

- **Replay**, on every `go test` in the unit suite: each target's seeds and committed crashers
  run as subtests of `FuzzXxx`. No tag, no registry change.
- **Fuzzing**, by hand: `go test -run '^$' -fuzz '^FuzzJournal$' -fuzztime 10m ./pkg/journal` (the
  `feat/storage-v2` path).
  A crasher lands in `testdata/fuzz/FuzzJournal/`, is committed with its text seed (§5.2), and
  becomes a regression test.
- **`DITTOFS_FUZZ_TRACE=1`** prints the trace of every input replayed, for reading a corpus.

## 12. Open questions

- **Comparison cost.** Reading back all observable state after every step is exact but slow
  for a large component. The likely answer is that `Compare` reads back what the step touched
  and a periodic full read-back runs as an operation; the journal target will tell.
- **The state-set cap.** 64 is a guess. The journal's crash model, with several volatile
  writes per file, is the first that can approach it.
- **A scheduled campaign.** A `fuzz` suite that runs each target for a fixed time, a short run on
  the packages a pull request changes and a longer one at night with the corpus kept between
  runs, is proposed as its own task of the roadmap (§13). `feat/storage-v2` already has 70 native
  targets, decoders included, that no suite runs past their seeds. The repository is public, so that
  task also decides where a crasher of a decoder a network peer reaches is run and kept: never in a
  public log until its fix is public.

## 13. Deferred

| Deferred | Why | Owner | Trigger |
|---|---|---|---|
| Fault wrappers for the filesystem and the remote store | They wrap interfaces the harness does not own; each lives with its package | fuzz: journal, fuzz: engine | The component target is started |
| Making background work steppable in the journal and engine | Component change, not harness | fuzz: journal, fuzz: engine | The component target is started |
| A scheduled fuzzing suite in `test/suites.yaml` | Needs targets to run and a measured cost per run | fuzz: running the targets (a task of the roadmap, proposed) | Now on `feat/storage-v2`, whose 70 targets already exist; on `develop`, two component targets merged |
| A conversion tool from crasher bytes to text seeds across table changes | `DITTOFS_FUZZ_TRACE` and hand conversion suffice while the corpora are small | unowned | A table change invalidates more than a handful of crashers |
