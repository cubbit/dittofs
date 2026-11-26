package engine_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cubbit/dittofs/pkg/engine"
	"github.com/cubbit/dittofs/pkg/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
	"github.com/cubbit/dittofs/test/fuzz"
)

// fuzzByte is the byte the write of step puts at the absolute offset off. It is
// never zero, so a lost write never reads as a hole, and two steps less than
// 255 apart never put the same byte at one offset, so a byte read back names
// the write it came from.
func fuzzByte(step int, off int64) byte {
	return byte(1 + (int64(step)+off)%255)
}

const (
	fuzzFiles = 3

	// fuzzMaxSteps and fuzzMaxWrite are the longest program and the longest
	// write.
	fuzzMaxSteps = 64
	fuzzMaxWrite = 8192

	// fuzzMaxFile is the largest size a program can give a file: the highest
	// offset and the longest write.
	fuzzMaxFile = 12288 + fuzzMaxWrite

	// fuzzSegmentCap keeps each input's segments small: at the format's size an
	// input spends seconds allocating them. checkOneSegment holds that no
	// program fills one.
	fuzzSegmentCap = 4 << 20

	// fuzzMinJournal is the smallest configured journal maximum the target runs
	// over: with less, a write could be refused for room, or old segments
	// repacked, and the model allows neither.
	fuzzMinJournal = 2 << 30

	// variantRefused ends what a trace prints for a crash variant the disk
	// refused, so the crash kept no unsynced write.
	variantRefused = " refused, nothing unsynced kept"
)

// fuzzFileIDs are fixed, because the journal picks a file's append stream by a
// hash of its ID, and a sync of one file makes the whole stream durable: with
// drawn IDs, what a crash keeps would differ between two runs of one input.
// Over the fixture's two streams these put the first two files together and the
// third apart, so a program reaches both.
var fuzzFileIDs = [fuzzFiles]entity.FileID{{15: 1}, {15: 3}, {15: 2}}

// engineWorld is the real side of one input: the engine over the real journal
// and store, and what the operations record from their own calls.
type engineWorld struct {
	fix     *enginetest.Fixture
	handles [fuzzFiles]entity.Handle
	files   [fuzzFiles]entity.FileID

	// ctx is the input's, for the reads made after each step.
	ctx context.Context

	// writes are the writes the engine accepted.
	writes []*acceptedWrite

	// faults is what the operations know from their own answers, which decides
	// the steps that call nothing.
	faults faultState
}

// acceptedWrite counts the calls of one write's Done.
type acceptedWrite struct {
	step int
	file int
	done atomic.Int32

	// orphaned is set when a crash leaves the write to recovery, which commits
	// it with no Done to call.
	orphaned bool
}

// faultState is what decides a step with no effect: a call that would wait for
// its context, which no step ends, or one the filesystem service never makes.
// The real side and the model each keep one, from the same answers, and the
// model refuses a step the real side skipped or ran against the model's own.
type faultState struct {
	// armed is that every update of the store fails.
	armed bool

	// owed marks a file with a removal whose first transaction has not
	// committed: the engine holds the file's guard until it does. With the
	// engine's clock never advanced, it does only after a crash.
	owed [fuzzFiles]bool

	// gone marks a released file.
	gone [fuzzFiles]bool

	// size is each file's size, and recorded is a floor under the size the
	// store has committed for it: set only by a removal that committed, a
	// crash and a restart. A commit over committed content records a size too
	// and is not counted, so the guard may skip more than it has to.
	size     [fuzzFiles]int
	recorded [fuzzFiles]int
}

// truncated is an accepted truncate to size. One that takes bytes away records
// the size, unless its transaction is owed.
func (s *faultState) truncated(file, size int, owed bool) {
	shrunk := size < s.size[file]
	s.size[file] = size

	if shrunk && !owed {
		s.recorded[file] = size
	}
}

func (s *faultState) released(file int) {
	s.size[file] = 0
	s.recorded[file] = 0
}

// started takes what a new process serves, whose sizes its start committed.
func (s *faultState) started(files [fuzzFiles]string) {
	for i, content := range files {
		s.size[i] = len(content)
		s.recorded[i] = len(content)
	}
}

func (s faultState) anyOwed() bool {
	return s.owed != [fuzzFiles]bool{}
}

// noEffect is whether op on file calls nothing. A restart names no file.
//
// While the store fails, every commit is skipped, though only one that needs
// the store would wait: telling the two apart takes the file's content. A
// removal is skipped then only once one is owed, so one can be owed at all.
func (s faultState) noEffect(op string, file int) bool {
	switch op {
	case "restart":
		return s.armed || s.anyOwed()
	case "write":
		// A released file is written by no one: the filesystem service
		// refuses its handle, and the engine would never commit the write.
		return s.gone[file]
	case "read":
		// A release not yet committed is refused above the engine too.
		return s.gone[file] && s.owed[file]
	case "commit":
		return s.armed || s.gone[file] || s.owed[file]
	}

	return s.gone[file] || s.owed[file] || (s.armed && s.anyOwed())
}

// crashed is a new process: the store works, and recovery commits every
// removal that was owed.
func (s *faultState) crashed() {
	s.armed = false
	s.owed = [fuzzFiles]bool{}
}

// tailOverUnrecordedSize is whether a deallocate of length bytes at off takes
// the last byte of a file of size bytes while the store holds less than size.
// With the store failing, a crash after such a step leaves the file shorter:
// the journal no longer holds where it ended. The target skips the step then.
// With the store working the removal commits before the step ends, so it runs.
func tailOverUnrecordedSize(size, recorded, off, length int) bool {
	// A range that starts past the end takes no byte of the file.
	if off >= size || off+length < size {
		return false
	}

	return recorded < size
}

// skips is the answer of a step that calls nothing, and whether op is one.
func (s faultState) skips(op string, a fuzz.Args) (skippedAnswer, bool) {
	if s.noEffect(op, fileOf(op, a)) {
		return skippedAnswer{}, true
	}

	if op != "deallocate" || !s.armed {
		return skippedAnswer{}, false
	}

	file := a.Int("file")
	tail := tailOverUnrecordedSize(s.size[file], s.recorded[file], a.Int("off"), a.Int("len"))

	return skippedAnswer{unrecordedTail: tail}, tail
}

// fileOf is the file op names, or -1 for one that names none.
func fileOf(op string, a fuzz.Args) int {
	if op == "restart" {
		return -1
	}

	return a.Int("file")
}

// skippedAnswer is what a step that calls nothing answers. unrecordedTail
// marks the deallocate the target leaves out.
type skippedAnswer struct {
	unrecordedTail bool
}

func (a skippedAnswer) String() string {
	if a.unrecordedTail {
		return "skipped: a deallocate of a tail the store has not recorded"
	}

	return "no effect"
}

type realFunc = func(*fuzz.Env, *engineWorld, fuzz.Args) fuzz.Result

type modelFunc = func(engineModel, fuzz.Args, fuzz.Result) ([]engineModel, error)

func realUnless(op string, call realFunc) realFunc {
	return func(env *fuzz.Env, w *engineWorld, a fuzz.Args) fuzz.Result {
		if answer, skipped := w.faults.skips(op, a); skipped {
			return answer
		}

		return call(env, w, a)
	}
}

func modelUnless(op string, allows modelFunc) modelFunc {
	return func(m engineModel, a fuzz.Args, got fuzz.Result) ([]engineModel, error) {
		answer, skipped := got.(skippedAnswer)
		wantAnswer, want := m.faults.skips(op, a)

		if skipped != want || (skipped && answer != wantAnswer) {
			return nil, fmt.Errorf("the step answered %s, and the model's state says it calls nothing: %v, answering %s",
				got, want, wantAnswer)
		}

		if skipped {
			return []engineModel{m}, nil
		}

		return allows(m, a, got)
	}
}

func newEngineWorld(env *fuzz.Env) (*engineWorld, error) {
	if err := checkOneSegment(fuzzMaxSteps, fuzzMaxWrite, fuzzSegmentCap); err != nil {
		return nil, err
	}

	w := &engineWorld{ctx: env.T.Context()}

	// Registered before the fixture, so it runs after the fixture has closed
	// the engine, which calls the Done of what its last group commit takes.
	env.Cleanup(func() {
		for _, write := range w.writes {
			if got := write.done.Load(); !write.orphaned && !w.faults.owed[write.file] && got != 1 {
				env.T.Errorf("the Done of the write of step #%02d ran %d times by the end, want once", write.step, got)
			}
		}
	})

	w.fix = enginetest.New(env.T, enginetest.WithSegmentCap(fuzzSegmentCap))
	for i := range fuzzFiles {
		nf := kvstore.Regular(0o644)
		nf.ID = fuzzFileIDs[i]

		h, file := w.fix.FS.Create(env.T, w.fix.FS.Admin, w.fix.FS.Root, fmt.Sprintf("f%d", i), nf)
		w.handles[i], w.files[i] = h, file.ID
	}

	// Registered after the fixture, so it runs before the fixture closes the
	// engine: with the store still failing, the close would not finish.
	env.Cleanup(func() {
		w.fix.FailUpdatesAfter(-1)
	})

	for _, j := range w.fix.E.Stats().Journals {
		if j.Stats == nil {
			return nil, errors.New("the journal answers no figures")
		}

		// Zero is the default, a share of the filesystem. That filesystem has
		// room, or the fixture's journal would not have got its spare
		// segments.
		if configured := j.Stats.MaxBytes; configured != 0 && configured < fuzzMinJournal {
			return nil, fmt.Errorf("the journal is configured with a maximum of %d bytes, under %d",
				configured, int64(fuzzMinJournal))
		}
	}

	return w, nil
}

// checkOneSegment holds that no program fills a segment, so no write is split
// over two and each one is one record, which a crash keeps whole or not at all.
// The model counts on it. The bytes a program writes may take a quarter of a
// segment: the rest is room for what the journal adds around them.
func checkOneSegment(steps, write int, segmentCap int64) error {
	if payload := int64(steps) * int64(write); payload > segmentCap/4 {
		return fmt.Errorf("a program of %d writes of %d bytes holds %d bytes, over a quarter of a segment of %d",
			steps, write, payload, segmentCap)
	}

	return nil
}

// doneAtMostOnce holds that no accepted write is reported finished twice.
func doneAtMostOnce(_ *fuzz.Env, w *engineWorld) error {
	for _, write := range w.writes {
		if got := write.done.Load(); got > 1 {
			return fmt.Errorf("the Done of the write of step #%02d ran %d times", write.step, got)
		}
	}

	return nil
}

// owedRemovalsStay holds every removal that answered retry later to staying in
// the journal, above what the store has applied, until its first transaction
// commits: a settle above it would lose it.
func owedRemovalsStay(env *fuzz.Env, w *engineWorld) error {
	for i, owed := range w.faults.owed {
		if !owed {
			continue
		}

		applied := journal.VersionFromBytes(w.fix.Committed(w.handles[i]).Applied)
		found := false
		for c, err := range w.fix.Handle().Since(env.T.Context(), journal.NewFileID(w.files[i]), applied) {
			if err != nil {
				return fmt.Errorf("since, file %d: %w", i, err)
			}

			found = found || (c.Kind != journal.ChangeWrite && c.Kind != journal.ChangeLoss)
		}

		if !found {
			return fmt.Errorf("a removal of file %d is owed, and the journal yields none above what the store has applied", i)
		}
	}

	return nil
}

// awaitRemovalLoop waits until the removal loop has run everything that is due
// and waits again. The loop runs on its own goroutine, so a step that went on
// earlier would leave it to the scheduler whether its first run sees the next
// step's change of the store. It waits on the loop's own state; a loop that
// never gets there ends the run at the harness's step bound.
func (w *engineWorld) awaitRemovalLoop() {
	for !w.fix.E.LoopIdle(w.fix.Share.ID) || !w.fix.E.LoopParked(w.fix.Share.ID) {
		runtime.Gosched()
	}
}

// contents reads every file whole through the engine, and marks the released
// ones.
func (w *engineWorld) contents(ctx context.Context) (files [fuzzFiles]string, gone [fuzzFiles]bool, err error) {
	for i, file := range w.files {
		// A release not yet committed is refused above the engine, and what
		// the engine answers for it then is outside its contract.
		if w.faults.gone[i] && w.faults.owed[i] {
			continue
		}

		// One byte more than any program writes, so a file too long shows.
		buf := make([]byte, fuzzMaxFile+1)

		n, err := w.fix.E.Read(ctx, w.fix.Share.ID, file, 0, buf)
		if errors.Is(err, vfserr.ErrStale) {
			gone[i] = true

			continue
		}

		if err != nil {
			return files, gone, fmt.Errorf("read file %d: %w", i, err)
		}

		files[i] = string(buf[:n])
	}

	return files, gone, nil
}

// leftToReplay counts, per file, what the journal holds above what the store
// has committed: what a replay would still have to apply.
func (w *engineWorld) leftToReplay(ctx context.Context) ([fuzzFiles]int, error) {
	var left [fuzzFiles]int
	for i, file := range w.files {
		if w.faults.gone[i] {
			continue
		}

		applied := w.fix.Committed(w.handles[i]).Applied
		from := journal.VersionFromBytes(applied)

		for _, err := range w.fix.Handle().Since(ctx, journal.NewFileID(file), from) {
			if err != nil {
				return left, fmt.Errorf("since, file %d: %w", i, err)
			}

			left[i]++
		}
	}

	return left, nil
}

// errorClass is what the contract tells one error from another by, and all a
// trace prints of one.
func errorClass(err error) string {
	var delay *vfserr.Delay
	switch {
	case errors.As(err, &delay):
		return "retry later"
	case errors.Is(err, vfserr.ErrReadOnly):
		return "read only"
	case errors.Is(err, vfserr.ErrLost):
		return "lost"
	case errors.Is(err, vfserr.ErrStale):
		return "no entity"
	case errors.Is(err, vfserr.ErrInvalid):
		return "invalid"
	case errors.Is(err, vfserr.ErrUnavailable):
		return "unavailable"
	}

	return "another error"
}

// describe prints file content as runs naming the step that wrote each, from
// the absolute offset at. A trace and a failure say which writes are there,
// and never print the bytes.
func describe(at int64, content string) string {
	if content == "" {
		return "empty"
	}

	owner := func(i int) string {
		b := content[i]
		if b == 0 {
			return "hole"
		}

		step := (int64(b) - 1 - (at + int64(i))) % 255

		return fmt.Sprintf("#%02d", (step+255)%255)
	}

	var runs []string
	start := 0
	for i := 1; i <= len(content); i++ {
		if i < len(content) && owner(i) == owner(start) {
			continue
		}

		runs = append(runs, fmt.Sprintf("[%d,%d)=%s", at+int64(start), at+int64(i), owner(start)))
		start = i
	}

	return strings.Join(runs, " ")
}

func describeFiles(contents [fuzzFiles]string) string {
	parts := make([]string, len(contents))
	for i, content := range contents {
		parts[i] = fmt.Sprintf("f%d: %s", i, describe(0, content))
	}

	return strings.Join(parts, " | ")
}

// writeAnswer is what a write answered.
type writeAnswer struct {
	step     int
	err      error
	verifier vfserr.Verifier
}

func (a writeAnswer) String() string {
	if a.err != nil {
		return errorClass(a.err)
	}

	return "ok"
}

// commitAnswer is what a commit answered.
type commitAnswer struct {
	err      error
	verifier vfserr.Verifier
	lossSeq  uint64
}

func (a commitAnswer) String() string {
	if a.err != nil {
		return errorClass(a.err)
	}

	return fmt.Sprintf("ok loss=%d", a.lossSeq)
}

// readAnswer is what a read answered, its bytes starting at off.
type readAnswer struct {
	off  int64
	err  error
	data string
}

func (a readAnswer) String() string {
	if a.err != nil {
		return errorClass(a.err)
	}

	return describe(a.off, a.data)
}

// removalAnswer is what a truncate, a deallocate or a delete answered. pending
// is a delete that answered a retry channel.
type removalAnswer struct {
	err     error
	pending bool
}

func (a removalAnswer) String() string {
	switch {
	case a.err != nil:
		return errorClass(a.err)
	case a.pending:
		return "pending"
	}

	return "ok"
}

// faultAnswer is what arming or clearing a fault answers.
type faultAnswer struct{}

func (faultAnswer) String() string {
	return "ok"
}

// restartedAnswer is what the files hold after a new process started, the
// size the store has committed for each, and, after a clean stop, what that
// stop had left for a replay.
type restartedAnswer struct {
	// variant is empty for a plain crash and for a restart.
	variant string
	err     error
	files   [fuzzFiles]string
	gone    [fuzzFiles]bool
	sizes   [fuzzFiles]int64
	replay  [fuzzFiles]int
}

func (a restartedAnswer) String() string {
	if a.err != nil {
		return a.err.Error()
	}

	answer := fmt.Sprintf("%s, released %v, committed sizes %v, left to replay %v",
		describeFiles(a.files), a.gone, a.sizes, a.replay)
	if a.variant != "" {
		return a.variant + ": " + answer
	}

	return answer
}

// checkSizes holds the new process to having committed the existence of what
// it serves. A read alone does not show it: the journal answers the bytes it
// holds whether or not the store knows of them.
func (a restartedAnswer) checkSizes() error {
	for i, content := range a.files {
		if a.sizes[i] != int64(len(content)) {
			return fmt.Errorf("file %d holds %d bytes after the start, and the store has committed a size of %d",
				i, len(content), a.sizes[i])
		}
	}

	return nil
}

func (w *engineWorld) committedSizes() [fuzzFiles]int64 {
	var sizes [fuzzFiles]int64
	for i, h := range w.handles {
		// A released file has no record left to read.
		if !w.faults.gone[i] {
			sizes[i] = w.fix.Committed(h).Size
		}
	}

	return sizes
}

func engineWrite(env *fuzz.Env, w *engineWorld, a fuzz.Args) fuzz.Result {
	off := int64(a.Int("off"))
	data := make([]byte, a.Int("len"))
	for i := range data {
		data[i] = fuzzByte(env.Step, off+int64(i))
	}

	write := &acceptedWrite{
		step: env.Step,
		file: a.Int("file"),
	}
	verifier, err := w.fix.E.Write(env.T.Context(), w.fix.Share.ID, w.files[a.Int("file")], off, data, engine.WriteOpts{
		Done: func() {
			write.done.Add(1)
		},
	})
	if err == nil {
		w.writes = append(w.writes, write)
		w.faults.size[write.file] = max(w.faults.size[write.file], int(off)+len(data))
	}

	return writeAnswer{
		step:     env.Step,
		err:      err,
		verifier: verifier,
	}
}

func engineCommit(env *fuzz.Env, w *engineWorld, a fuzz.Args) fuzz.Result {
	verifier, lossSeq, err := w.fix.E.Commit(
		env.T.Context(),
		w.fix.Share.ID,
		w.files[a.Int("file")],
		int64(a.Int("off")),
		int64(a.Int("len")),
	)

	return commitAnswer{
		err:      err,
		verifier: verifier,
		lossSeq:  lossSeq,
	}
}

func engineRead(env *fuzz.Env, w *engineWorld, a fuzz.Args) fuzz.Result {
	off := int64(a.Int("off"))
	buf := make([]byte, a.Int("len"))

	n, err := w.fix.E.Read(env.T.Context(), w.fix.Share.ID, w.files[a.Int("file")], off, buf)

	return readAnswer{
		off:  off,
		err:  err,
		data: string(buf[:n]),
	}
}

func engineTruncate(env *fuzz.Env, w *engineWorld, a fuzz.Args) fuzz.Result {
	file := a.Int("file")
	err := w.fix.E.Truncate(env.T.Context(), w.fix.Share.ID, w.files[file], int64(a.Int("size")))
	owed := err != nil && errorClass(err) == "retry later"

	if err == nil || owed {
		w.faults.truncated(file, a.Int("size"), owed)
	}

	return w.removed(file, err)
}

func engineDeallocate(env *fuzz.Env, w *engineWorld, a fuzz.Args) fuzz.Result {
	file := a.Int("file")
	err := w.fix.E.Deallocate(env.T.Context(), w.fix.Share.ID, w.files[file], int64(a.Int("off")), int64(a.Int("len")))
	if err == nil {
		w.faults.recorded[file] = w.faults.size[file]
	}

	return w.removed(file, err)
}

// removed records what a truncate or a deallocate answered.
func (w *engineWorld) removed(file int, err error) fuzz.Result {
	if err != nil && errorClass(err) == "retry later" {
		w.faults.owed[file] = true
	}

	w.awaitRemovalLoop()

	return removalAnswer{err: err}
}

// engineDelete releases a file as the filesystem service does: its only name
// goes first, or the release finds a holder and is aborted. The store works
// for that, and fails again for the release if it did before.
func engineDelete(env *fuzz.Env, w *engineWorld, a fuzz.Args) fuzz.Result {
	file := a.Int("file")

	if w.faults.armed {
		w.fix.FailUpdatesAfter(-1)
	}

	name := fmt.Appendf(nil, "f%d", file)
	_, err := w.fix.FS.Store.Namespace().Unlink(env.T.Context(), w.fix.FS.Admin, w.fix.FS.Root, name)

	if w.faults.armed {
		w.fix.FailUpdatesAfter(0)
	}

	if err != nil {
		return removalAnswer{err: fmt.Errorf("unlink: %w", err)}
	}

	retry, err := w.fix.E.Delete(env.T.Context(), w.fix.Share.ID, w.files[file])
	if err == nil {
		w.faults.released(file)
		w.faults.gone[file] = true
		w.faults.owed[file] = retry != nil
	}

	w.awaitRemovalLoop()

	return removalAnswer{
		err:     err,
		pending: retry != nil,
	}
}

func engineFailUpdates(_ *fuzz.Env, w *engineWorld, a fuzz.Args) fuzz.Result {
	n := a.Int("n")
	w.fix.FailUpdatesAfter(n)
	w.faults.armed = n == 0

	return faultAnswer{}
}

// engineCrash is a power loss with no variant: the simulated device keeps no
// write that was not synced, and a new process starts on what is left.
//
// The crash operations are host losses that differ in which of the writes no
// sync covered survive: engineCrash none, the others those the disk kit's
// option of the same name keeps. A restart, a process death, has no variant:
// it keeps every accepted write.
func engineCrash(env *fuzz.Env, w *engineWorld, _ fuzz.Args) fuzz.Result {
	w.fix.Crash()

	return w.afterCrash(env, "")
}

func engineCrashKeeping(env *fuzz.Env, w *engineWorld, a fuzz.Args) fuzz.Result {
	mask := a.Int("mask")

	return w.crashVariant(env, fmt.Sprintf("keeping %#b", mask), enginetest.Keeping(uint64(mask)))
}

func engineCrashTorn(env *fuzz.Env, w *engineWorld, a fuzz.Args) fuzz.Result {
	n := int(a.Int("n"))

	return w.crashVariant(env, fmt.Sprintf("torn after %d bytes", n), enginetest.Torn(n))
}

func engineCrashReordered(env *fuzz.Env, w *engineWorld, a fuzz.Args) fuzz.Result {
	seed := a.Int("seed")

	return w.crashVariant(env, fmt.Sprintf("reordered by seed %d", seed), enginetest.Reordered(uint64(seed)))
}

// crashVariant crashes with opt, or plain when the disk refuses it, which the
// answer then says.
func (w *engineWorld) crashVariant(env *fuzz.Env, variant string, opt enginetest.CrashOption) fuzz.Result {
	if refused := w.fix.CrashOrPlain(opt); refused != nil {
		variant += variantRefused
	}

	return w.afterCrash(env, variant)
}

// afterCrash is what the new process of a crash serves.
func (w *engineWorld) afterCrash(env *fuzz.Env, variant string) fuzz.Result {
	for _, write := range w.writes {
		if write.done.Load() == 0 {
			write.orphaned = true
		}
	}

	w.faults.crashed()
	files, gone, err := w.contents(env.T.Context())
	w.faults.started(files)

	return restartedAnswer{
		variant: variant,
		err:     err,
		files:   files,
		gone:    gone,
		sizes:   w.committedSizes(),
	}
}

// engineRestart is a clean stop and a new start. The new process is held where
// it raises the shards' incarnations, which is before it replays anything, and
// what the clean stop left for the replay is counted there.
func engineRestart(env *fuzz.Env, w *engineWorld, _ fuzz.Args) fuzz.Result {
	entered, release := w.fix.HoldNextStart()
	restart := fuzz.Begin(env, func() bool {
		w.fix.Restart()

		return true
	})

	if !restart.WaitParked(entered) {
		restart.Finish()

		return restartedAnswer{err: errors.New("the restart ended before its start raised the incarnations")}
	}

	replay, replayErr := w.leftToReplay(env.T.Context())
	release()
	restart.Finish()

	files, gone, err := w.contents(env.T.Context())
	w.faults.started(files)

	return restartedAnswer{
		err:    errors.Join(replayErr, err),
		files:  files,
		gone:   gone,
		sizes:  w.committedSizes(),
		replay: replay,
	}
}

// engineModel is what the contract lets the files hold. It follows the real
// side's answers: it checks each against the contract and goes on from what the
// engine chose.
type engineModel struct {
	files [fuzzFiles]modelFile

	faults faultState

	// verifier is the last one answered, and answered that one was. newProcess
	// says a new process started since, so the next must differ.
	verifier   vfserr.Verifier
	answered   bool
	newProcess bool
}

// modelFile is one file: the content that survives a crash, and over it the
// writes not yet synced, in the order they were made. A model is copied by
// value, so neither is changed in place.
type modelFile struct {
	synced   string
	unsynced []unsyncedWrite
}

// unsyncedWrite is a write not yet synced, or, with grow set, a truncate that
// made the file longer, to off bytes. A removal is durable once it returns, so
// every crash keeps a grow. A removal that takes bytes away needs no entry: its
// first transaction syncs the file before it, so what is left is all synced.
type unsyncedWrite struct {
	grow   bool
	step   int
	off    int64
	length int
}

// apply is content with the writes made over it, a gap reading as zeros. Over a
// file's synced content, its unsynced writes give what a read sees.
func apply(content string, writes []unsyncedWrite) string {
	out := []byte(content)
	for _, w := range writes {
		if w.grow {
			if int(w.off) > len(out) {
				out = append(out, make([]byte, int(w.off)-len(out))...)
			}

			continue
		}

		if end := int(w.off) + w.length; end > len(out) {
			out = append(out, make([]byte, end-len(out))...)
		}

		for off := w.off; off < w.off+int64(w.length); off++ {
			out[off] = fuzzByte(w.step, off)
		}
	}

	return string(out)
}

// afterCrash is the file as found after a crash, or an error when no choice of
// unsynced writes explains it. A crash keeps every synced write and any subset
// of the others, each whole or not at all.
func (f modelFile) afterCrash(found string) (modelFile, error) {
	// A byte names the write it came from, so the kept writes are those with a
	// byte of their own in what was found. One kept with none is hidden under
	// later kept writes, and changes nothing.
	var kept []unsyncedWrite
	for _, w := range f.unsynced {
		if w.grow {
			kept = append(kept, w)

			continue
		}

		for off := w.off; off < w.off+int64(w.length) && off < int64(len(found)); off++ {
			if found[off] == fuzzByte(w.step, off) {
				kept = append(kept, w)

				break
			}
		}
	}

	if want := apply(f.synced, kept); found != want {
		return modelFile{}, fmt.Errorf("found %s, which no choice of the %d unsynced writes gives: synced alone is %s, the writes seen give %s",
			describe(0, found), len(f.unsynced), describe(0, f.synced), describe(0, want))
	}

	return modelFile{synced: found}, nil
}

func newEngineModel() engineModel {
	return engineModel{}
}

// sawVerifier checks a verifier: one value for as long as a process runs with
// no loss, and another in a new one. Nothing these operations do is a loss.
func (m engineModel) sawVerifier(got vfserr.Verifier) (engineModel, error) {
	switch {
	case !m.answered:
	case m.newProcess && got == m.verifier:
		return m, fmt.Errorf("the verifier is still %x after a new process started", got)
	case !m.newProcess && got != m.verifier:
		return m, fmt.Errorf("the verifier went from %x to %x with no loss and no new process", m.verifier, got)
	}

	m.verifier, m.answered, m.newProcess = got, true, false

	return m, nil
}

// one is the single state a model step allows, or why it allows none.
func one(m engineModel, err error) ([]engineModel, error) {
	if err != nil {
		return nil, err
	}

	return []engineModel{m}, nil
}

// modelWrite allows success alone: a failing store does not delay a write, and
// no operation of the target makes the journal read only or a file the
// destination of a clone.
func modelWrite(m engineModel, a fuzz.Args, got fuzz.Result) ([]engineModel, error) {
	answer := got.(writeAnswer)
	if answer.err != nil {
		return nil, fmt.Errorf("the write failed, and nothing allows it: %w", answer.err)
	}

	file := &m.files[a.Int("file")]
	file.unsynced = append(slices.Clip(file.unsynced), unsyncedWrite{
		step:   answer.step,
		off:    int64(a.Int("off")),
		length: a.Int("len"),
	})
	size := &m.faults.size[a.Int("file")]
	*size = max(*size, a.Int("off")+a.Int("len"))

	return one(m.sawVerifier(answer.verifier))
}

// modelCommit makes every write of the file synced, whatever the extent: the
// journal's sync takes the file whole.
func modelCommit(m engineModel, a fuzz.Args, got fuzz.Result) ([]engineModel, error) {
	answer := got.(commitAnswer)
	if answer.err != nil {
		return nil, fmt.Errorf("the commit failed, and nothing allows it: %w", answer.err)
	}

	if answer.lossSeq != 0 {
		return nil, fmt.Errorf("the loss sequence is %d, and no operation loses a write", answer.lossSeq)
	}

	file := &m.files[a.Int("file")]
	*file = modelFile{synced: apply(file.synced, file.unsynced)}

	return one(m.sawVerifier(answer.verifier))
}

func modelRead(m engineModel, a fuzz.Args, got fuzz.Result) ([]engineModel, error) {
	answer := got.(readAnswer)
	if m.faults.gone[a.Int("file")] {
		if answer.err == nil || errorClass(answer.err) != "no entity" {
			return nil, fmt.Errorf("a read of a released file answered %s, want no entity", answer)
		}

		return []engineModel{m}, nil
	}

	if answer.err != nil {
		return nil, fmt.Errorf("the read failed, and nothing allows it: %w", answer.err)
	}

	file := m.files[a.Int("file")]
	view := apply(file.synced, file.unsynced)
	start := min(a.Int("off"), len(view))
	end := min(a.Int("off")+a.Int("len"), len(view))

	if want := view[start:end]; answer.data != want {
		return nil, fmt.Errorf("read %s, the model holds %s", answer, describe(answer.off, want))
	}

	return []engineModel{m}, nil
}

// reachedFirstTransaction is a removal that takes something away from file,
// leaving after. With the store failing it must answer retry later and is
// owed, and otherwise it must succeed. Either way the file is synced first, as
// before every existence commit.
func (m engineModel) reachedFirstTransaction(file int, answer removalAnswer, after string) ([]engineModel, error) {
	switch {
	case m.faults.armed && (answer.err == nil || errorClass(answer.err) != "retry later"):
		return nil, fmt.Errorf("the removal answered %s with every store update failing, want retry later", answer)
	case !m.faults.armed && answer.err != nil:
		return nil, fmt.Errorf("the removal failed, and nothing allows it: %w", answer.err)
	}

	m.files[file] = modelFile{synced: after}
	m.faults.owed[file] = m.faults.armed
	m.faults.size[file] = len(after)

	if !m.faults.armed {
		m.faults.recorded[file] = len(after)
	}

	return []engineModel{m}, nil
}

func modelTruncate(m engineModel, a fuzz.Args, got fuzz.Result) ([]engineModel, error) {
	answer := got.(removalAnswer)
	file := &m.files[a.Int("file")]
	view := apply(file.synced, file.unsynced)
	size := a.Int("size")
	if size < len(view) {
		return m.reachedFirstTransaction(a.Int("file"), answer, view[:size])
	}

	// A grow takes no transaction, so a failing store does not delay it.
	if answer.err != nil {
		return nil, fmt.Errorf("a truncate that takes nothing away failed: %w", answer.err)
	}

	file.unsynced = append(slices.Clip(file.unsynced), unsyncedWrite{
		grow: true,
		off:  int64(size),
	})
	m.faults.truncated(a.Int("file"), size, false)

	return []engineModel{m}, nil
}

func modelDeallocate(m engineModel, a fuzz.Args, got fuzz.Result) ([]engineModel, error) {
	file := m.files[a.Int("file")]
	after := []byte(apply(file.synced, file.unsynced))
	start := min(a.Int("off"), len(after))
	clear(after[start:min(a.Int("off")+a.Int("len"), len(after))])

	return m.reachedFirstTransaction(a.Int("file"), got.(removalAnswer), string(after))
}

func modelDelete(m engineModel, a fuzz.Args, got fuzz.Result) ([]engineModel, error) {
	answer := got.(removalAnswer)
	if answer.err != nil {
		return nil, fmt.Errorf("the delete failed, and nothing allows it: %w", answer.err)
	}

	if answer.pending != m.faults.armed {
		return nil, fmt.Errorf("the delete answered %s, and every store update failing is %v", answer, m.faults.armed)
	}

	file := a.Int("file")
	m.files[file] = modelFile{}
	m.faults.released(file)
	m.faults.gone[file] = true
	m.faults.owed[file] = answer.pending

	return []engineModel{m}, nil
}

func modelFailUpdates(m engineModel, a fuzz.Args, _ fuzz.Result) ([]engineModel, error) {
	m.faults.armed = a.Int("n") == 0

	return []engineModel{m}, nil
}

func modelCrash(m engineModel, _ fuzz.Args, got fuzz.Result) ([]engineModel, error) {
	answer := got.(restartedAnswer)
	if answer.err != nil {
		return nil, fmt.Errorf("after the crash: %w", answer.err)
	}

	m.faults.crashed()

	if answer.gone != m.faults.gone {
		return nil, fmt.Errorf("after the crash the released files are %v, want %v", answer.gone, m.faults.gone)
	}

	for i, file := range m.files {
		after, err := file.afterCrash(answer.files[i])
		if err != nil {
			return nil, fmt.Errorf("file %d after the crash: %w", i, err)
		}

		m.files[i] = after
	}

	if err := answer.checkSizes(); err != nil {
		return nil, err
	}

	m.faults.started(answer.files)
	m.newProcess = true

	return []engineModel{m}, nil
}

// modelRestart allows no loss at all: with no power cut, the next open keeps
// even a write that was never synced, and a clean stop commits every file.
func modelRestart(m engineModel, _ fuzz.Args, got fuzz.Result) ([]engineModel, error) {
	answer := got.(restartedAnswer)
	if answer.err != nil {
		return nil, fmt.Errorf("after the restart: %w", answer.err)
	}

	if answer.gone != m.faults.gone {
		return nil, fmt.Errorf("after the restart the released files are %v, want %v", answer.gone, m.faults.gone)
	}

	for i, file := range m.files {
		if answer.replay[i] != 0 {
			return nil, fmt.Errorf("the clean stop left %d changes of file %d for the next start to replay", answer.replay[i], i)
		}

		view := apply(file.synced, file.unsynced)
		if answer.files[i] != view {
			return nil, fmt.Errorf("file %d after the restart holds %s, want %s",
				i, describe(0, answer.files[i]), describe(0, view))
		}

		m.files[i] = modelFile{synced: view}
	}

	if err := answer.checkSizes(); err != nil {
		return nil, err
	}

	m.faults.started(answer.files)
	m.newProcess = true

	return []engineModel{m}, nil
}

// compareFiles reads every file after each step, so a wrong byte fails at the
// step that made it wrong.
func compareFiles(_ fuzz.Step, _ fuzz.Result, w *engineWorld, m engineModel) error {
	got, gone, err := w.contents(w.ctx)
	if err != nil {
		return err
	}

	for i, file := range m.files {
		// A release not yet committed is not read.
		if m.faults.gone[i] && m.faults.owed[i] {
			continue
		}

		if gone[i] != m.faults.gone[i] {
			return fmt.Errorf("file %d reads released: %v, the model holds %v", i, gone[i], m.faults.gone[i])
		}

		view := apply(file.synced, file.unsynced)
		if got[i] != view {
			return fmt.Errorf("file %d reads %s, the model holds %s", i, describe(0, got[i]), describe(0, view))
		}

		if m.faults.size[i] != len(view) {
			return fmt.Errorf("file %d holds %d bytes in the model, and the size its skips are decided from is %d",
				i, len(view), m.faults.size[i])
		}
	}

	return nil
}

// engineKey names a state. Every step allows one, so it only has to tell a
// state from itself.
func engineKey(m engineModel) string {
	return fmt.Sprint(m)
}

func engineTarget() fuzz.Target[*engineWorld, engineModel] {
	file := fuzz.Enum("file", 0, 1, 2)
	off := fuzz.Choice("off", 0, 1, 4095, 4096, 8192, 12288)
	length := fuzz.Choice("len", 1, 2, 512, 4096, fuzzMaxWrite)
	// Zero is the whole file.
	commitLength := fuzz.Choice("len", 0, 1, 2, 512, 4096, fuzzMaxWrite)

	size := fuzz.Choice("size", 0, 1, 4096, 8192)
	// 0 fails every update of the store, -1 clears that.
	updates := fuzz.Choice("n", -1, 0)
	mask := fuzz.Choice("mask", 0b1, 0b10, 0b11, 0b101)
	torn := fuzz.Choice("n", 1, 8, 64)
	seed := fuzz.Choice("seed", 1, 2, 3)

	return fuzz.Target[*engineWorld, engineModel]{
		Name:     "engine",
		MaxSteps: fuzzMaxSteps,
		Ops: []fuzz.Op[*engineWorld, engineModel]{
			{
				Name:   "write",
				Weight: 8,
				Params: []fuzz.Param{file, off, length},
				Real:   realUnless("write", engineWrite),
				Model:  modelUnless("write", modelWrite),
			},
			{
				Name:   "commit",
				Weight: 4,
				Params: []fuzz.Param{file, off, commitLength},
				Real:   realUnless("commit", engineCommit),
				Model:  modelUnless("commit", modelCommit),
			},
			{
				Name:   "read",
				Weight: 4,
				Params: []fuzz.Param{file, off, length},
				Real:   realUnless("read", engineRead),
				Model:  modelUnless("read", modelRead),
			},
			{
				Name:   "crash",
				Weight: 1,
				Real:   engineCrash,
				Model:  modelCrash,
			},
			{
				Name:   "restart",
				Weight: 1,
				Real:   realUnless("restart", engineRestart),
				Model:  modelUnless("restart", modelRestart),
			},
			{
				Name:   "truncate",
				Weight: 2,
				Params: []fuzz.Param{file, size},
				Real:   realUnless("truncate", engineTruncate),
				Model:  modelUnless("truncate", modelTruncate),
			},
			{
				Name:   "deallocate",
				Weight: 2,
				Params: []fuzz.Param{file, off, length},
				Real:   realUnless("deallocate", engineDeallocate),
				Model:  modelUnless("deallocate", modelDeallocate),
			},
			{
				Name:   "delete",
				Weight: 1,
				Params: []fuzz.Param{file},
				Real:   realUnless("delete", engineDelete),
				Model:  modelUnless("delete", modelDelete),
			},
			{
				Name:   "fail_updates_after",
				Weight: 2,
				Params: []fuzz.Param{updates},
				Real:   engineFailUpdates,
				Model:  modelFailUpdates,
			},
			// Each bit of the mask is one write to the disk that no sync covers,
			// in the order issued, not one engine write: the first write of a new
			// segment counts as two, the segment's header and then the record.
			{
				Name:   "crash_keeping",
				Weight: 1,
				Params: []fuzz.Param{mask},
				Real:   engineCrashKeeping,
				Model:  modelCrash,
			},
			{
				Name:   "crash_torn",
				Weight: 1,
				Params: []fuzz.Param{torn},
				Real:   engineCrashTorn,
				Model:  modelCrash,
			},
			// The parameter is a seed, not a mask: from it the disk draws whether
			// each of those same disk writes is kept, so a new segment's header
			// and its first record are drawn apart. It may keep none.
			{
				Name:   "crash_reordered",
				Weight: 1,
				Params: []fuzz.Param{seed},
				Real:   engineCrashReordered,
				Model:  modelCrash,
			},
		},
		// The engine's methods are not listed here yet: this holds only what
		// the target leaves out on purpose.
		Uncovered: []fuzz.Omission{
			fuzz.Skipped(
				"deallocate_of_an_unrecorded_tail",
				"while the store fails, a deallocate that takes a file's last byte is skipped unless the store is "+
					"known to hold the file's size: a crash after it leaves the file shorter",
			),
		},
		NewReal:    newEngineWorld,
		NewModel:   newEngineModel,
		Compare:    compareFiles,
		Key:        engineKey,
		Invariants: []fuzz.Invariant[*engineWorld]{doneAtMostOnce, owedRemovalsStay},
	}
}

// engineSeeds are the programs every plain go test runs.
var engineSeeds = []struct {
	name  string
	calls []string
}{
	{
		// The truncate's mark in the journal is all that says the bytes are
		// gone when the power goes.
		name: "a truncate owed its transaction at a crash",
		calls: []string{
			"write(file=0, off=0, len=8192)",
			"restart()",
			"fail_updates_after(n=0)",
			"truncate(file=0, size=0)",
			"crash()",
			"read(file=0, off=0, len=8192)",
		},
	},
	{
		name: "removals that commit, then a crash",
		calls: []string{
			"write(file=0, off=0, len=8192)",
			"deallocate(file=0, off=1, len=4096)",
			"truncate(file=0, size=8192)",
			"write(file=1, off=0, len=512)",
			"truncate(file=1, size=4096)",
			"truncate(file=0, size=4096)",
			"delete(file=1)",
			"read(file=1, off=0, len=1)",
			"crash()",
			"read(file=0, off=0, len=8192)",
		},
	},
	{
		name: "what calls nothing around an owed removal",
		calls: []string{
			"write(file=0, off=0, len=8192)",
			"write(file=1, off=0, len=8192)",
			"fail_updates_after(n=0)",
			"deallocate(file=0, off=0, len=4096)",
			"truncate(file=1, size=0)",
			"commit(file=2, off=0, len=0)",
			"restart()",
			"fail_updates_after(n=-1)",
			"commit(file=0, off=0, len=0)",
			"delete(file=0)",
			"write(file=0, off=8192, len=2)",
			"write(file=1, off=0, len=1)",
			"commit(file=1, off=0, len=0)",
			"truncate(file=1, size=1)",
			"crash()",
			"read(file=0, off=0, len=8192)",
		},
	},
	{
		name: "a release owed at a crash",
		calls: []string{
			"write(file=2, off=0, len=512)",
			"fail_updates_after(n=0)",
			"delete(file=2)",
			"read(file=2, off=0, len=1)",
			"write(file=2, off=0, len=1)",
			"crash()",
			"read(file=2, off=0, len=1)",
			"restart()",
		},
	},
	{
		name: "a program that ends with the store failing",
		calls: []string{
			"write(file=0, off=0, len=512)",
			"write(file=1, off=0, len=512)",
			"fail_updates_after(n=0)",
			"truncate(file=0, size=1)",
		},
	},
	{
		name: "a committed append survives a crash",
		calls: []string{
			"write(file=0, off=0, len=8192)",
			"commit(file=0, off=0, len=0)",
			"crash()",
			"read(file=0, off=0, len=8192)",
		},
	},
	{
		name: "an unsynced write at a crash",
		calls: []string{
			"write(file=0, off=0, len=8192)",
			"crash()",
			"read(file=0, off=0, len=8192)",
		},
	},
	{
		name: "a committed overwrite and an unsynced one over it",
		calls: []string{
			"write(file=0, off=0, len=8192)",
			"commit(file=0, off=0, len=0)",
			"write(file=0, off=4095, len=4096)",
			"commit(file=0, off=4095, len=2)",
			"write(file=0, off=0, len=512)",
			"crash()",
			"read(file=0, off=4095, len=4096)",
		},
	},
	{
		name: "a commit of one file and a write of another",
		calls: []string{
			"write(file=0, off=4096, len=512)",
			"write(file=1, off=0, len=512)",
			"commit(file=0, off=0, len=1)",
			"crash()",
			"read(file=0, off=0, len=8192)",
			"read(file=1, off=0, len=512)",
		},
	},
	{
		// The third file is on the other stream, which the commit leaves alone.
		name: "a commit and an unsynced write on the other stream",
		calls: []string{
			"write(file=0, off=0, len=512)",
			"write(file=2, off=0, len=512)",
			"commit(file=0, off=0, len=0)",
			"crash()",
			"read(file=2, off=0, len=512)",
		},
	},
	{
		name: "a clean restart keeps writes never synced",
		calls: []string{
			"write(file=0, off=0, len=512)",
			"write(file=2, off=12288, len=2)",
			"restart()",
			"read(file=2, off=12288, len=8192)",
		},
	},
	{
		// A stable write: a write, and a commit over exactly what it wrote.
		name: "a stable write survives a crash",
		calls: []string{
			"write(file=0, off=0, len=4096)",
			"write(file=0, off=4096, len=512)",
			"commit(file=0, off=4096, len=512)",
			"crash()",
			"read(file=0, off=4096, len=512)",
		},
	},
	{
		name: "the verifier across restarts",
		calls: []string{
			"write(file=0, off=0, len=1)",
			"write(file=0, off=1, len=1)",
			"restart()",
			"write(file=0, off=4095, len=1)",
			"crash()",
			"commit(file=0, off=0, len=0)",
		},
	},
	{
		// The commit of an append leaves the size to the journal alone, so with
		// the store failing the deallocate of the tail is the step that is
		// skipped.
		name:  "a deallocate of a tail the store has not recorded is skipped",
		calls: tailDeallocate,
	},
	{
		name: "a deallocate of a tail the store has not recorded runs while the store works",
		calls: []string{
			"write(file=0, off=0, len=8192)",
			"commit(file=0, off=0, len=0)",
			"deallocate(file=0, off=4096, len=4096)",
			"crash()",
			"read(file=0, off=0, len=8192)",
		},
	},
	{
		name: "a crash that keeps the first unsynced write",
		calls: []string{
			"write(file=0, off=0, len=512)",
			"commit(file=0, off=0, len=0)",
			"write(file=0, off=4096, len=512)",
			"write(file=0, off=8192, len=512)",
			"crash_keeping(mask=1)",
			"read(file=0, off=0, len=8192)",
			"read(file=0, off=8192, len=512)",
		},
	},
	{
		name: "a crash that tears the last unsynced write",
		calls: []string{
			"write(file=0, off=0, len=512)",
			"commit(file=0, off=0, len=0)",
			"write(file=0, off=4096, len=512)",
			"write(file=0, off=8192, len=512)",
			"crash_torn(n=8)",
			"read(file=0, off=0, len=8192)",
			"read(file=0, off=8192, len=512)",
		},
	},
	{
		name: "a crash that keeps a later unsynced write alone",
		calls: []string{
			"write(file=0, off=0, len=512)",
			"commit(file=0, off=0, len=0)",
			"write(file=0, off=4096, len=512)",
			"write(file=0, off=8192, len=512)",
			"crash_reordered(seed=1)",
			"read(file=0, off=0, len=8192)",
			"read(file=0, off=8192, len=512)",
		},
	},
	{
		name: "a removal not yet stored, then a crash that keeps a later write",
		calls: []string{
			"write(file=0, off=0, len=512)",
			"restart()",
			"fail_updates_after(n=0)",
			"truncate(file=0, size=0)",
			"write(file=1, off=0, len=512)",
			"crash_keeping(mask=1)",
			"read(file=0, off=0, len=512)",
			"read(file=1, off=0, len=512)",
		},
	},
}

var tailDeallocate = []string{
	"write(file=0, off=0, len=8192)",
	"commit(file=0, off=0, len=0)",
	"fail_updates_after(n=0)",
	"deallocate(file=0, off=4096, len=4096)",
	"crash()",
	"read(file=0, off=0, len=8192)",
}

// TestTheGuardTakesOnlyATailTheStoreHasNotRecorded holds the guard to a
// deallocate that takes the file's last byte while the store holds a smaller
// size than the file has.
func TestTheGuardTakesOnlyATailTheStoreHasNotRecorded(t *testing.T) {
	cases := []struct {
		name     string
		size     int
		recorded int
		off      int
		length   int
		skipped  bool
	}{
		{
			name:    "the tail, nothing recorded",
			size:    8192,
			off:     4096,
			length:  4096,
			skipped: true,
		},
		{
			name:    "past the end",
			size:    8192,
			off:     4096,
			length:  8192,
			skipped: true,
		},
		{
			name:    "the whole file",
			size:    8192,
			length:  8192,
			skipped: true,
		},
		{
			name:     "the tail, part of the size recorded",
			size:     8192,
			recorded: 4096,
			off:      4096,
			length:   4096,
			skipped:  true,
		},
		{
			name:     "the tail, the size recorded",
			size:     8192,
			recorded: 8192,
			off:      4096,
			length:   4096,
		},
		{
			name:   "one byte short of the end",
			size:   8192,
			off:    4096,
			length: 4095,
		},
		{
			name:   "wholly past the end",
			size:   8192,
			off:    8192,
			length: 4096,
		},
		{
			name:   "an empty file",
			length: 4096,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tailOverUnrecordedSize(c.size, c.recorded, c.off, c.length); got != c.skipped {
				t.Fatalf("tailOverUnrecordedSize(size=%d, recorded=%d, off=%d, len=%d) = %v, want %v",
					c.size, c.recorded, c.off, c.length, got, c.skipped)
			}
		})
	}
}

// TestTheGuardSkipsOnlyWhileTheStoreFails holds the guard to the failing
// store: with the store working, a removal has committed by the time its step
// ends, and a crash after it leaves the size alone.
func TestTheGuardSkipsOnlyWhileTheStoreFails(t *testing.T) {
	cases := []struct {
		name    string
		armed   bool
		skipped bool
	}{
		{
			name:    "the store failing",
			armed:   true,
			skipped: true,
		},
		{
			name: "the store working",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			faults := faultState{armed: c.armed}
			faults.size[0] = 8192

			_, got := faults.skips("deallocate", parsed(t, "deallocate(file=0, off=4096, len=4096)"))
			if got != c.skipped {
				t.Fatalf("a deallocate of an unrecorded tail with the store armed=%v is skipped: %v, want %v",
					c.armed, got, c.skipped)
			}
		})
	}
}

// TestTheSkippedDeallocateShowsInTheTraceAndTheCoverage holds the guard to
// being seen: the step says why it called nothing, and the target's coverage
// lists what it leaves out.
func TestTheSkippedDeallocateShowsInTheTraceAndTheCoverage(t *testing.T) {
	target := engineTarget()

	program, err := fuzz.Parse(target, strings.Join(tailDeallocate[:4], "\n"))
	if err != nil {
		t.Fatal(err)
	}

	env := &fuzz.Env{
		T:       t,
		Cleanup: t.Cleanup,
	}
	w, err := newEngineWorld(env)
	if err != nil {
		t.Fatal(err)
	}

	var answer string
	for i, step := range program {
		env.Step = i
		answer = target.Ops[step.Op].Real(env, w, step.Args).String()
		t.Logf("#%02d %s -> %s", i, step, answer)
	}

	if want := "skipped: a deallocate of a tail the store has not recorded"; answer != want {
		t.Errorf("the deallocate answered %q, want %q", answer, want)
	}

	listed := slices.ContainsFunc(target.Uncovered, func(o fuzz.Omission) bool {
		return o.Name == "skip:deallocate_of_an_unrecorded_tail" && o.Why != ""
	})

	if !listed {
		t.Errorf("the target's coverage does not list the skipped deallocate and why: %v", target.Uncovered)
	}
}

// TestModelAllowsWhatACrashMayLeave holds the model to the contract: a crash
// keeps every synced write and any subset of the others, each whole or not at
// all.
func TestModelAllowsWhatACrashMayLeave(t *testing.T) {
	synced := unsyncedWrite{
		step:   0,
		off:    0,
		length: 8,
	}
	first := unsyncedWrite{
		step:   1,
		off:    2,
		length: 4,
	}
	second := unsyncedWrite{
		step:   2,
		off:    4,
		length: 8,
	}
	cover := unsyncedWrite{
		step:   3,
		off:    0,
		length: 16,
	}

	base := apply("", []unsyncedWrite{synced})
	with := func(writes ...unsyncedWrite) string {
		return apply(base, writes)
	}

	cases := []struct {
		name     string
		unsynced []unsyncedWrite
		found    string
		allowed  bool
		// removed is how many bytes a synced removal cut off the end.
		removed int
	}{
		{
			name:     "none kept",
			unsynced: []unsyncedWrite{first, second},
			found:    base,
			allowed:  true,
		},
		{
			name:     "all kept",
			unsynced: []unsyncedWrite{first, second},
			found:    with(first, second),
			allowed:  true,
		},
		{
			name:     "the first alone",
			unsynced: []unsyncedWrite{first, second},
			found:    with(first),
			allowed:  true,
		},
		{
			name:     "the later one without the earlier",
			unsynced: []unsyncedWrite{first, second},
			found:    with(second),
			allowed:  true,
		},
		{
			name:     "one hidden under a later one",
			unsynced: []unsyncedWrite{first, cover},
			found:    with(cover),
			allowed:  true,
		},
		{
			name:     "a write kept in part",
			unsynced: []unsyncedWrite{second},
			found:    with(second)[:10],
		},
		{
			name:    "a removal that came back",
			removed: 4,
			found:   base,
		},
		{
			name:     "a synced write lost",
			unsynced: []unsyncedWrite{first},
			found:    "",
		},
		{
			name:     "the earlier write over the later one",
			unsynced: []unsyncedWrite{first, second},
			found:    with(second, first),
		},
		{
			name:     "a size no write gives",
			unsynced: []unsyncedWrite{first},
			found:    base + "\x00",
		},
		{
			name:     "a byte no write made",
			unsynced: []unsyncedWrite{first},
			found:    base[:7] + "\xff",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			file := modelFile{
				synced:   base[:len(base)-c.removed],
				unsynced: c.unsynced,
			}

			after, err := file.afterCrash(c.found)

			switch {
			case c.allowed && err != nil:
				t.Fatalf("a legal state is refused: %v", err)
			case !c.allowed && err == nil:
				t.Fatalf("the illegal state %s is allowed", describe(0, c.found))
			case c.allowed && (after.synced != c.found || len(after.unsynced) != 0):
				t.Fatalf("the file after the crash holds %s and %d unsynced writes, want %s and none",
					describe(0, after.synced), len(after.unsynced), describe(0, c.found))
			}
		})
	}
}

// TestModelRefusesAStartThatLeftExistenceUncommitted holds both starts to the
// committed size: what a read serves alone does not show a replay that was
// missed.
func TestModelRefusesAStartThatLeftExistenceUncommitted(t *testing.T) {
	starts := []struct {
		name  string
		model func(engineModel, fuzz.Args, fuzz.Result) ([]engineModel, error)
	}{
		{
			name:  "crash",
			model: modelCrash,
		},
		{
			name:  "restart",
			model: modelRestart,
		},
	}

	for _, start := range starts {
		t.Run(start.name, func(t *testing.T) {
			content := apply("", []unsyncedWrite{{length: 3}})
			before := newEngineModel()
			before.files[0] = modelFile{synced: content}
			answer := restartedAnswer{
				files: [fuzzFiles]string{content},
				sizes: [fuzzFiles]int64{3},
			}

			if _, err := start.model(before, fuzz.Args{}, answer); err != nil {
				t.Fatalf("the committed size matches what is read, and the model refuses: %v", err)
			}

			answer.sizes[0] = 0
			if _, err := start.model(before, fuzz.Args{}, answer); err == nil {
				t.Fatal("a committed size of 0 under 3 bytes read is allowed")
			}
		})
	}
}

// parsed is the arguments of one call written as a trace prints it.
func parsed(t *testing.T, call string) fuzz.Args {
	t.Helper()

	program, err := fuzz.Parse(engineTarget(), call)
	if err != nil {
		t.Fatal(err)
	}

	return program[0].Args
}

// TestARemovalStepLeavesTheLoopNothingToRun holds a removal step to ending only
// once the removal loop has made its first try. On one processor, a step that
// did not wait would leave that try to run after the store works again, and the
// truncate would commit.
func TestARemovalStepLeavesTheLoopNothingToRun(t *testing.T) {
	procs := runtime.GOMAXPROCS(1)
	t.Cleanup(func() {
		runtime.GOMAXPROCS(procs)
	})

	env := &fuzz.Env{
		T:       t,
		Cleanup: t.Cleanup,
	}
	w, err := newEngineWorld(env)
	if err != nil {
		t.Fatal(err)
	}

	engineWrite(env, w, parsed(t, "write(file=0, off=0, len=8192)"))
	w.fix.Restart()
	engineFailUpdates(env, w, parsed(t, "fail_updates_after(n=0)"))

	if got := engineTruncate(env, w, parsed(t, "truncate(file=0, size=0)")).String(); got != "retry later" {
		t.Fatalf("the truncate answered %s, want retry later", got)
	}

	engineFailUpdates(env, w, parsed(t, "fail_updates_after(n=-1)"))
	w.awaitRemovalLoop()

	if size := w.fix.Committed(w.handles[0]).Size; size != 8192 {
		t.Fatalf("the committed size is %d, want 8192: the first try ran after the store worked again", size)
	}
}

func TestAnOwedRemovalMustStayInTheJournal(t *testing.T) {
	env := &fuzz.Env{
		T:       t,
		Cleanup: t.Cleanup,
	}
	w, err := newEngineWorld(env)
	if err != nil {
		t.Fatal(err)
	}

	engineWrite(env, w, parsed(t, "write(file=0, off=0, len=8192)"))
	w.faults.owed[0] = true

	if err := owedRemovalsStay(env, w); err == nil {
		t.Fatal("an owed removal the journal does not hold is allowed")
	}

	w.faults.owed[0] = false
}

func TestNoProgramFillsASegment(t *testing.T) {
	cases := []struct {
		name       string
		steps      int
		segmentCap int64
		fits       bool
	}{
		{
			name:       "the target's own limits",
			steps:      fuzzMaxSteps,
			segmentCap: fuzzSegmentCap,
			fits:       true,
		},
		{
			name:       "programs eight times as long",
			steps:      8 * fuzzMaxSteps,
			segmentCap: fuzzSegmentCap,
		},
		{
			name:       "a segment of one MiB",
			steps:      fuzzMaxSteps,
			segmentCap: 1 << 20,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkOneSegment(c.steps, fuzzMaxWrite, c.segmentCap)

			if got := err == nil; got != c.fits {
				t.Fatalf("checkOneSegment(%d, %d, %d) = %v, want fits %v", c.steps, fuzzMaxWrite, c.segmentCap, err, c.fits)
			}
		})
	}
}

func TestACrashVariantKeepsTheUnsyncedWritesItNames(t *testing.T) {
	// Two writes after the commit: neither is synced at the crash.
	writes := []string{
		"write(file=0, off=0, len=512)",
		"commit(file=0, off=0, len=0)",
		"write(file=0, off=4096, len=512)",
		"write(file=0, off=8192, len=512)",
	}

	cases := []struct {
		crash  string
		first  bool
		second bool
	}{
		{
			crash: "crash()",
		},
		{
			crash: "crash_keeping(mask=1)",
			first: true,
		},
		{
			crash:  "crash_keeping(mask=2)",
			second: true,
		},
		{
			crash:  "crash_keeping(mask=3)",
			first:  true,
			second: true,
		},
		{
			// The mask names a third write, which there is not.
			crash: "crash_keeping(mask=5)",
		},
		{
			crash: "crash_torn(n=64)",
		},
		{
			// This seed drops the earlier write and keeps the later one.
			crash:  "crash_reordered(seed=1)",
			second: true,
		},
	}

	for _, c := range cases {
		t.Run(c.crash, func(t *testing.T) {
			target := engineTarget()
			program, err := fuzz.Parse(target, strings.Join(append(slices.Clone(writes), c.crash), "\n"))
			if err != nil {
				t.Fatal(err)
			}

			env := &fuzz.Env{
				T:       t,
				Cleanup: t.Cleanup,
			}
			w, err := newEngineWorld(env)
			if err != nil {
				t.Fatal(err)
			}

			var answer fuzz.Result
			for i, step := range program {
				env.Step = i
				answer = target.Ops[step.Op].Real(env, w, step.Args)
				t.Logf("#%02d %s -> %s", i, step, answer)
			}

			found := answer.(restartedAnswer).files[0]
			var holds [3]bool
			for i, off := range []int{0, 4096, 8192} {
				holds[i] = len(found) >= off+512 && found[off] != 0
			}

			if !holds[0] {
				t.Errorf("the synced write at 0 is gone: %s", answer)
			}

			if holds[1] != c.first {
				t.Errorf("the first unsynced write survived: %v, want %v", holds[1], c.first)
			}

			if holds[2] != c.second {
				t.Errorf("the second unsynced write survived: %v, want %v", holds[2], c.second)
			}
		})
	}
}

// FuzzEngine runs random programs of engine calls against a model. To fuzz
// locally:
//
//	TMPDIR=/dev/shm go test -run '^$' -fuzz '^FuzzEngine$' -fuzzminimizetime=0 ./pkg/engine/
//
// -fuzzminimizetime=0 turns off the fuzzer's minimization, which took most of
// a run. The harness shrinks a failing program itself. TMPDIR on tmpfs changes
// nothing the target checks, since the simulated disk keeps its sync in memory.
// On ext4 some inputs take seconds. The probable cause is the flush at the close
// of a file that was truncated and rewritten, as the simulated crash does; it
// was not verified in the kernel.
func FuzzEngine(f *testing.F) {
	target := engineTarget()
	for _, seed := range engineSeeds {
		f.Add(fuzz.Seed(target, strings.Join(seed.calls, "\n")))
	}

	fuzz.Fuzz(f, target)
}

// TestEngineSeedsRunTheSameTwice holds every seed to the same trace in two
// runs, and every crash variant in a seed to being applied: a refused one runs
// as a plain crash, so the seed would no longer reach what it is there for.
func TestEngineSeedsRunTheSameTwice(t *testing.T) {
	target := engineTarget()
	for i := range target.Ops {
		call := target.Ops[i].Real
		target.Ops[i].Real = func(env *fuzz.Env, w *engineWorld, a fuzz.Args) fuzz.Result {
			answer := call(env, w, a)
			if strings.Contains(answer.String(), variantRefused) {
				env.T.Errorf("step #%02d of a seed: the crash variant was refused: %s", env.Step, answer)
			}

			return answer
		}
	}

	for _, seed := range engineSeeds {
		t.Run(seed.name, func(t *testing.T) {
			fuzz.Deterministic(t, target, fuzz.Seed(target, strings.Join(seed.calls, "\n")))
		})
	}
}

func TestFuzzByteNamesTheStepThatWroteIt(t *testing.T) {
	offsets := []int64{0, 1, 254, 255, 256, 4095, 4096, 8192, 12288, 20479}

	for _, off := range offsets {
		for step := range 600 {
			got := fuzzByte(step, off)
			if got == 0 {
				t.Fatalf("fuzzByte(%d, %d) = 0, which reads as a hole", step, off)
			}

			for other := step + 1; other < step+255; other++ {
				if fuzzByte(other, off) == got {
					t.Fatalf("steps %d and %d both put %#02x at offset %d", step, other, got, off)
				}
			}
		}
	}
}
