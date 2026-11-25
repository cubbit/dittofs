package engine_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

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

const fuzzFiles = 3

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

// restartedAnswer is what the files hold after a new process started, and,
// after a clean stop, what that stop had left for a replay.
type restartedAnswer struct {
	err    error
	files  [fuzzFiles]string
	replay [fuzzFiles]int
}

func (a restartedAnswer) String() string {
	if a.err != nil {
		return a.err.Error()
	}

	return fmt.Sprintf("%s, left to replay %v", describeFiles(a.files), a.replay)
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

	m.newProcess = true

	return []engineModel{m}, nil
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
