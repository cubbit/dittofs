package engine_test

import (
	"context"
	"errors"
	"fmt"
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
}

// acceptedWrite counts the calls of one write's Done.
type acceptedWrite struct {
	step int
	done atomic.Int32

	// orphaned is set when a crash leaves the write to recovery, which commits
	// it with no Done to call.
	orphaned bool
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
			if got := write.done.Load(); !write.orphaned && got != 1 {
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

// doneAtMostOnce is the one invariant: no accepted write is reported finished
// twice.
func doneAtMostOnce(_ *fuzz.Env, w *engineWorld) error {
	for _, write := range w.writes {
		if got := write.done.Load(); got > 1 {
			return fmt.Errorf("the Done of the write of step #%02d ran %d times", write.step, got)
		}
	}

	return nil
}

// contents reads every file whole through the engine.
func (w *engineWorld) contents(ctx context.Context) ([fuzzFiles]string, error) {
	var out [fuzzFiles]string
	for i, file := range w.files {
		// One byte more than any program writes, so a file too long shows.
		buf := make([]byte, fuzzMaxFile+1)

		n, err := w.fix.E.Read(ctx, w.fix.Share.ID, file, 0, buf)
		if err != nil {
			return out, fmt.Errorf("read file %d: %w", i, err)
		}

		out[i] = string(buf[:n])
	}

	return out, nil
}

// leftToReplay counts, per file, what the journal holds above what the store
// has committed: what a replay would still have to apply.
func (w *engineWorld) leftToReplay(ctx context.Context) ([fuzzFiles]int, error) {
	var left [fuzzFiles]int
	for i, file := range w.files {
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

// restartedAnswer is what the files hold after a new process started, the
// size the store has committed for each, and, after a clean stop, what that
// stop had left for a replay.
type restartedAnswer struct {
	err    error
	files  [fuzzFiles]string
	sizes  [fuzzFiles]int64
	replay [fuzzFiles]int
}

func (a restartedAnswer) String() string {
	if a.err != nil {
		return a.err.Error()
	}

	return fmt.Sprintf("%s, committed sizes %v, left to replay %v", describeFiles(a.files), a.sizes, a.replay)
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
		sizes[i] = w.fix.Committed(h).Size
	}

	return sizes
}

func engineWrite(env *fuzz.Env, w *engineWorld, a fuzz.Args) fuzz.Result {
	off := int64(a.Int("off"))
	data := make([]byte, a.Int("len"))
	for i := range data {
		data[i] = fuzzByte(env.Step, off+int64(i))
	}

	write := &acceptedWrite{step: env.Step}
	verifier, err := w.fix.E.Write(env.T.Context(), w.fix.Share.ID, w.files[a.Int("file")], off, data, engine.WriteOpts{
		Done: func() {
			write.done.Add(1)
		},
	})
	if err == nil {
		w.writes = append(w.writes, write)
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

// engineCrash is a power loss with no variant: the simulated device keeps no
// write that was not synced, and a new process starts on what is left.
func engineCrash(env *fuzz.Env, w *engineWorld, _ fuzz.Args) fuzz.Result {
	w.fix.Crash()
	for _, write := range w.writes {
		if write.done.Load() == 0 {
			write.orphaned = true
		}
	}

	files, err := w.contents(env.T.Context())

	return restartedAnswer{
		err:   err,
		files: files,
		sizes: w.committedSizes(),
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

	files, err := w.contents(env.T.Context())

	return restartedAnswer{
		err:    errors.Join(replayErr, err),
		files:  files,
		sizes:  w.committedSizes(),
		replay: replay,
	}
}

// engineModel is what the contract lets the files hold. It follows the real
// side's answers: it checks each against the contract and goes on from what the
// engine chose.
type engineModel struct {
	files [fuzzFiles]modelFile

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

type unsyncedWrite struct {
	step   int
	off    int64
	length int
}

// apply is content with the writes made over it, a gap reading as zeros. Over a
// file's synced content, its unsynced writes give what a read sees.
func apply(content string, writes []unsyncedWrite) string {
	out := []byte(content)
	for _, w := range writes {
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

// modelWrite allows success alone: no operation of the target makes the store
// fail, the journal read only or a file the destination of a clone.
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

func modelCrash(m engineModel, _ fuzz.Args, got fuzz.Result) ([]engineModel, error) {
	answer := got.(restartedAnswer)
	if answer.err != nil {
		return nil, fmt.Errorf("after the crash: %w", answer.err)
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

	m.newProcess = true

	return []engineModel{m}, nil
}

// compareFiles reads every file after each step, so a wrong byte fails at the
// step that made it wrong.
func compareFiles(_ fuzz.Step, _ fuzz.Result, w *engineWorld, m engineModel) error {
	got, err := w.contents(w.ctx)
	if err != nil {
		return err
	}

	for i, file := range m.files {
		if view := apply(file.synced, file.unsynced); got[i] != view {
			return fmt.Errorf("file %d reads %s, the model holds %s", i, describe(0, got[i]), describe(0, view))
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

	return fuzz.Target[*engineWorld, engineModel]{
		Name:     "engine",
		MaxSteps: fuzzMaxSteps,
		Ops: []fuzz.Op[*engineWorld, engineModel]{
			{
				Name:   "write",
				Weight: 8,
				Params: []fuzz.Param{file, off, length},
				Real:   engineWrite,
				Model:  modelWrite,
			},
			{
				Name:   "commit",
				Weight: 4,
				Params: []fuzz.Param{file, off, commitLength},
				Real:   engineCommit,
				Model:  modelCommit,
			},
			{
				Name:   "read",
				Weight: 4,
				Params: []fuzz.Param{file, off, length},
				Real:   engineRead,
				Model:  modelRead,
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
				Real:   engineRestart,
				Model:  modelRestart,
			},
		},
		NewReal:    newEngineWorld,
		NewModel:   newEngineModel,
		Compare:    compareFiles,
		Key:        engineKey,
		Invariants: []fuzz.Invariant[*engineWorld]{doneAtMostOnce},
	}
}

// engineSeeds are the programs every plain go test runs.
var engineSeeds = []struct {
	name  string
	calls []string
}{
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
				synced:   base,
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

func TestEngineSeedsRunTheSameTwice(t *testing.T) {
	target := engineTarget()
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
