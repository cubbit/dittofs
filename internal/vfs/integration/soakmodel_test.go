package integration

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/vfs"
)

// opKind is what one call of the soak did to a file's content.
type opKind uint8

const (
	opWrite opKind = iota + 1
	opTruncate
	opDeallocate
)

// soakOp is one write, truncate or deallocate the soak issued on a file, in
// the order it issued them.
type soakOp struct {
	kind opKind
	off  int64  // a write's or a deallocate's offset; a truncate's new size
	data []byte // a write's bytes
	n    int64  // a deallocate's length

	proc     int          // the process the call ran in
	done     bool         // the call answered success
	verifier vfs.Verifier // an unstable write's verifier, which a Commit must match to acknowledge it
	// grow marks a truncate that sets the size at or above the one the file's
	// operations leave. The engine answers it before any sync, and its group
	// commit commits it lazily (pkg/block/engine/remove.go, Truncate), so it
	// is acknowledged by a later Commit, as an unstable write is.
	grow bool
	// acked is set once the client was told the op is durable: a stable
	// write, a shrinking truncate or a deallocate answered success, or an
	// unstable write or a growing truncate a later Commit of the same process
	// answered.
	acked bool
	// faulted is set when the schedule lost the op's journal record: in the
	// op's own call, or credited to it as written since its file's last sync.
	faulted bool
	mtime   time.Time // the file's modify time the service answered after the call
}

// covers reports whether o's effect reaches offset at, and the byte it
// leaves there.
func (o *soakOp) covers(at int64) (byte, bool) {
	switch o.kind {
	case opWrite:
		if at >= o.off && at < o.off+int64(len(o.data)) {
			return o.data[at-o.off], true
		}
	case opTruncate:
		return 0, at >= o.off
	case opDeallocate:
		return 0, at >= o.off && at < o.off+o.n
	}

	return 0, false
}

// size is the file's size once o applies over size.
func (o *soakOp) size(size int64) int64 {
	switch o.kind {
	case opWrite:
		return max(size, o.off+int64(len(o.data)))
	case opTruncate:
		return o.off
	}

	return size
}

// leaves is the size f has once every op whose call answered success applies:
// the size the engine's own operations leave, which decides a truncate's grow.
func (f *fileModel) leaves() int64 {
	var size int64
	for _, o := range f.ops {
		if o.done {
			size = o.size(size)
		}
	}

	return size
}

// fileModel is what the soak knows of one file: every op on it since it was
// created empty, and the modify time it was created with.
type fileModel struct {
	name    string
	created time.Time
	ops     []*soakOp
	losses  []lossEvent // the losses found on the file while a process served it
	// owes is set while the file's next Commit may fail once for a loss: a
	// failed sync window that held its unacknowledged writes, or a loss a
	// restart found, which the next Commit's drain may raise.
	owes bool
}

// lossEvent is a loss found on a file while process proc served it, at a
// point of the oracle's own order.
type lossEvent struct {
	proc, at int
}

// smbOpen is an SMB open of a file in process proc, at the point of the
// oracle's order where it last sampled the file's loss sequence: when it was
// taken, then each time its flush reported a loss.
type smbOpen struct {
	f        *fileModel
	proc, at int
}

// soakModel is the oracle: the files' histories, the verifiers each process
// answered, and the losses that fired with no client call in flight.
type soakModel struct {
	files        []*fileModel
	verifiers    map[vfs.Verifier]int // a verifier, to the first process that answered it
	unattributed int
	now          int // the oracle's order of opens taken and losses found
}

func newSoakModel() *soakModel {
	return &soakModel{verifiers: map[vfs.Verifier]int{}}
}

// file adds a file created empty at created.
func (m *soakModel) file(name string, created time.Time) *fileModel {
	f := &fileModel{name: name, created: created}
	m.files = append(m.files, f)

	return f
}

// add records op on f, acknowledged at once when the call answered a stable
// write, a shrinking truncate or a deallocate with success.
func (f *fileModel) add(op *soakOp, stable bool) *soakOp {
	op.acked = op.done && (stable || op.kind == opDeallocate || op.kind == opTruncate && !op.grow)
	f.ops = append(f.ops, op)

	return op
}

// commit records a Commit of [off, off+length) of f in process proc, length
// 0 meaning to the end of the file. A Commit answered success acknowledges
// every unstable write of the same process in its range that carries the
// verifier it answered, and every growing truncate of the same process. A Commit that failed, or answered another verifier,
// reported a loss and acknowledges nothing.
func (f *fileModel) commit(proc int, off, length int64, v vfs.Verifier, err error) {
	if err != nil {
		return
	}

	for _, o := range f.ops {
		if o.kind == opTruncate && o.grow && o.done && o.proc == proc {
			o.acked = true
		}

		end := o.off + int64(len(o.data))
		inRange := end > off && (length == 0 || o.off < off+length)
		if o.kind == opWrite && o.done && !o.acked && o.proc == proc && o.verifier == v && inRange {
			o.acked = true
		}
	}
}

// credit marks faulted every write of every file not yet acknowledged: a
// loss that fired in a call other than a write's own append lands on a
// record written since its file's last sync, and the soak cannot tell which.
func (m *soakModel) credit() {
	for _, f := range m.files {
		for _, o := range f.ops {
			if o.kind == opWrite && !o.acked {
				o.faulted = true
			}
		}
	}
}

// syncFailed records a failed sync window: every file with an unacknowledged
// write may have had it in the window, so each one's next Commit may fail
// once, as a failed timer's sync fails the file's next Commit once.
func (m *soakModel) syncFailed() {
	for _, f := range m.files {
		for _, o := range f.ops {
			if o.kind == opWrite && !o.acked {
				f.owes = true
			}
		}
	}
}

// owed reports whether f's Commit now may fail once for a loss, and clears
// it: whatever that Commit answers, the one after owes nothing.
func (f *fileModel) owed() bool {
	owes := f.owes
	f.owes = false

	return owes
}

// answered reports whether a write whose call answered success covers
// offset at: a loss event raises the file's loss sequence only for such a
// write.
func (f *fileModel) answered(at int64) bool {
	for _, o := range f.ops {
		if _, ok := o.covers(at); ok && o.kind == opWrite && o.done {
			return true
		}
	}

	return false
}

// open records an SMB open of f taken in process proc.
func (m *soakModel) open(f *fileModel, proc int) *smbOpen {
	m.now++

	return &smbOpen{f: f, proc: proc, at: m.now}
}

// lose records a loss found on f while process proc serves it: a read that
// dropped a record as corrupt.
func (m *soakModel) lose(f *fileModel, proc int) {
	m.now++
	f.losses = append(f.losses, lossEvent{proc: proc, at: m.now})
}

// flush judges a Commit through the SMB open o, then records it as commit
// does. RFC 8 §5.1 reports a loss through the file's loss sequence, which the
// process keeps in memory, and an SMB flush through an open taken before the
// loss answers ErrLost from it once. So a flush that answers success while a
// loss found after o's last sample is unreported fails; ErrLost moves the
// sample; another error leaves the report owed.
func (m *soakModel) flush(o *smbOpen, v vfs.Verifier, err error) error {
	o.f.commit(o.proc, 0, 0, v, err)

	if errors.Is(err, vfs.ErrLost) {
		m.now++
		o.at = m.now

		return nil
	}

	if err != nil {
		return nil
	}

	for _, l := range o.f.losses {
		if l.proc == o.proc && l.at > o.at {
			return fmt.Errorf(
				"%s: a loss found after its SMB open sampled the loss sequence, and the open's flush answered success, not ErrLost",
				o.f.name,
			)
		}
	}

	return nil
}

// observe judges a verifier process proc answered: one an earlier process
// answered fails, since every restart must change it.
func (m *soakModel) observe(proc int, v vfs.Verifier) error {
	first, seen := m.verifiers[v]
	if !seen {
		m.verifiers[v] = proc

		return nil
	}

	if first != proc {
		return fmt.Errorf(
			"process %d answered verifier %x, which process %d answered before it",
			proc,
			v,
			first,
		)
	}

	return nil
}

// candidates is what a read of f at offset at may answer: the bytes, and
// whether Lost is one of them.
//
// decision: Lost is accepted only where a write whose journal record the
// schedule lost covers the offset, and it is the newest acknowledged op there
// or comes after it. RFC 8 §5.1 has such a synced record read Lost, never
// zeros or the superseded chunk. A write no client Commit acknowledged may
// still have been synced by the timer's group commit, since the soak moves
// the clock, so it may read Lost too. An unsynced write that was dropped
// reads as the file did before it, which is the bytes of the newest
// acknowledged op, or zero where there is none.
// It holds because RFC 8 §15.1's "sync before existence" says a committed
// size is never lost to a host crash. A journal that loses a synced record no
// fault touched would overturn it, and this oracle reports that as a failure.
func (f *fileModel) candidates(at int64) (vals []byte, lost bool, floor int) {
	floor = -1
	for i := len(f.ops) - 1; i >= 0; i-- {
		if _, ok := f.ops[i].covers(at); ok && f.ops[i].acked {
			floor = i

			break
		}
	}

	vals = []byte{0}
	if floor >= 0 {
		v, _ := f.ops[floor].covers(at)
		vals = []byte{v}
	}

	for _, o := range f.ops[max(floor, 0):] {
		if _, ok := o.covers(at); ok && o.kind == opWrite && o.faulted {
			lost = true
		}
	}

	// Every op after the newest acknowledged one may or may not have landed,
	// whole or torn, so each of its bytes is one the read may answer.
	for _, o := range f.ops[floor+1:] {
		if v, ok := o.covers(at); ok && !slices.Contains(vals, v) {
			vals = append(vals, v)
		}
	}

	return vals, lost, floor
}

// judge checks what a read of [off, off+len(got)) of f answered, after a
// restart: lost reports that the read failed as Lost. A failure names the
// first offset it found wrong and why.
func (m *soakModel) judge(f *fileModel, off int64, got []byte, lost bool) error {
	if lost {
		if _, ok, _ := f.candidates(off); !ok {
			return fmt.Errorf("%s [%d, %d) read Lost with no fault on its acknowledged write%s",
				f.name, off, off+int64(len(got)), m.unattributedNote())
		}

		return nil
	}

	for i, b := range got {
		at := off + int64(i)
		vals, _, floor := f.candidates(at)
		if slices.Contains(vals, b) {
			continue
		}

		for _, o := range f.ops[:max(floor, 0)] {
			if v, ok := o.covers(at); ok && v == b {
				return fmt.Errorf(
					"%s at %d read %#x, older content served as newer: an acknowledged op wrote %#x",
					f.name,
					at,
					b,
					vals[0],
				)
			}
		}

		if b == 0 {
			return fmt.Errorf(
				"%s at %d read zero where the acknowledged content is %#x",
				f.name,
				at,
				vals[0],
			)
		}

		return fmt.Errorf(
			"%s at %d read %#x, a byte no write put there (want one of %#x)",
			f.name,
			at,
			b,
			vals,
		)
	}

	return nil
}

func (m *soakModel) unattributedNote() string {
	if m.unattributed == 0 {
		return ""
	}

	return fmt.Sprintf(
		"; %d losses fired with no client call in flight, credited to no range",
		m.unattributed,
	)
}

// sizes is every size f may have after a restart: each acknowledged op
// applied, and each other op applied or not. A write whose record the
// schedule lost is still applied: RFC 8 §5.1 commits an acknowledged append's
// existence from its loss record, so it keeps its size and reads Lost.
func (f *fileModel) sizes() []int64 {
	sizes := []int64{0}
	for _, o := range f.ops {
		next := make([]int64, 0, 2*len(sizes))
		for _, s := range sizes {
			next = append(next, o.size(s))
			if !o.acked {
				next = append(next, s)
			}
		}

		slices.Sort(next)
		sizes = slices.Compact(next)
	}

	return sizes
}

// mtimes is every modify time f may have after a restart: the one after the
// newest acknowledged op or after any op past it, or the creation's when no
// op was acknowledged.
func (f *fileModel) mtimes() []time.Time {
	from := -1
	for i, o := range f.ops {
		if o.acked {
			from = i
		}
	}

	var ts []time.Time
	if from < 0 {
		ts = append(ts, f.created)
		from = 0
	}

	for _, o := range f.ops[from:] {
		ts = append(ts, o.mtime)
	}

	return ts
}

// stat checks the size and modify time a restart served for f, before any
// read: a pending existence the restart did not re-apply shows here.
func (f *fileModel) stat(size int64, mtime time.Time) error {
	if !slices.Contains(f.sizes(), size) {
		return fmt.Errorf(
			"%s served size %d after the restart, want one of %v",
			f.name,
			size,
			f.sizes(),
		)
	}

	if !slices.ContainsFunc(f.mtimes(), mtime.Equal) {
		return fmt.Errorf(
			"%s served modify time %v after the restart, want one of %v",
			f.name,
			mtime,
			f.mtimes(),
		)
	}

	return nil
}

// TestOracle holds the oracle to hand-made histories, each read as a restart
// might serve it: a correct engine's answer is accepted, a broken one's
// rejected with the reason.
func TestOracle(t *testing.T) {
	t0 := time.Unix(1000, 0)
	vA, vB := vfs.Verifier{1}, vfs.Verifier{2}
	bytesOf := func(c byte, n int) []byte {
		return bytes.Repeat([]byte{c}, n)
	}
	// write records a write of n bytes of c at off in process 1 answered
	// success with verifier vA, stable or not.
	write := func(f *fileModel, off int64, c byte, n int, stable bool) *soakOp {
		return f.add(&soakOp{
			kind:     opWrite,
			off:      off,
			data:     bytesOf(c, n),
			proc:     1,
			done:     true,
			verifier: vA,
			mtime:    t0.Add(time.Duration(len(f.ops)+1) * time.Second),
		}, stable)
	}
	// read judges a read of f at off answering got, or Lost.
	read := func(m *soakModel, f *fileModel, off int64, got []byte, lost bool) error {
		return m.judge(f, off, got, lost)
	}

	cases := []struct {
		name string
		run  func(m *soakModel, f *fileModel) error
		want string // "" accepts; otherwise a part of the rejection
	}{
		{"O1 a stable write reads its own bytes", func(m *soakModel, f *fileModel) error {
			write(f, 0, 'a', 4, true)
			return read(m, f, 0, bytesOf('a', 4), false)
		}, ""},
		{"O2 a stable write no fault touched reads Lost", func(m *soakModel, f *fileModel) error {
			write(f, 0, 'a', 4, true)
			return read(m, f, 0, bytesOf('a', 4), true)
		}, "Lost with no fault"},
		{
			"O3 a stable write whose record was lost reads Lost",
			func(m *soakModel, f *fileModel) error {
				write(f, 0, 'a', 4, true).faulted = true
				return read(m, f, 0, bytesOf('a', 4), true)
			},
			"",
		},
		{"O4 a lost stable overwrite reads zeros", func(m *soakModel, f *fileModel) error {
			write(f, 0, 'a', 4, true)
			write(f, 0, 'b', 4, true).faulted = true
			return read(m, f, 0, bytesOf(0, 4), false)
		}, "read zero where"},
		{
			"O5 a lost stable overwrite reads the superseded bytes",
			func(m *soakModel, f *fileModel) error {
				write(f, 0, 'a', 4, true)
				write(f, 0, 'b', 4, true).faulted = true
				return read(m, f, 0, bytesOf('a', 4), false)
			},
			"older content served as newer",
		},
		{
			"O6 an unstable write a Commit reported lost reads as before it",
			func(m *soakModel, f *fileModel) error {
				write(f, 0, 'a', 4, true)
				write(f, 0, 'b', 4, false)
				f.commit(1, 0, 0, vB, nil) // the verifier changed: the loss is reported
				return read(m, f, 0, bytesOf('a', 4), false)
			},
			"",
		},
		{"O7 an unstable write a crash dropped reads Lost", func(m *soakModel, f *fileModel) error {
			write(f, 0, 'a', 4, false)
			return read(m, f, 0, bytesOf('a', 4), true)
		}, "Lost with no fault"},
		{
			"O8 a loss with no call in flight credits no range",
			func(m *soakModel, f *fileModel) error {
				write(f, 0, 'a', 4, true)
				m.unattributed = 1
				return read(m, f, 0, bytesOf('a', 4), true)
			},
			"credited to no range",
		},
		{"O9 a byte no write put there", func(m *soakModel, f *fileModel) error {
			write(f, 0, 'a', 4, false)
			return read(m, f, 0, []byte("aaxa"), false)
		}, "a byte no write put there"},
		{"O10 an unacknowledged write torn at its tail", func(m *soakModel, f *fileModel) error {
			write(f, 0, 'a', 4, true)
			write(f, 0, 'b', 4, false)
			return read(m, f, 0, []byte("bbaa"), false)
		}, ""},
		{"O11 a deallocate reads zeros", func(m *soakModel, f *fileModel) error {
			write(f, 0, 'a', 8, true)
			f.add(&soakOp{kind: opDeallocate, off: 2, n: 4, proc: 1, done: true}, false)
			return read(m, f, 0, []byte("aa\x00\x00\x00\x00aa"), false)
		}, ""},
		{
			"O12 a truncate down, then a write past it, reads zeros between",
			func(m *soakModel, f *fileModel) error {
				write(f, 0, 'a', 8, true)
				f.add(&soakOp{kind: opTruncate, off: 2, proc: 1, done: true}, false)
				write(f, 6, 'b', 2, true)
				if !slices.Contains(f.sizes(), 8) || len(f.sizes()) != 1 {
					return fmt.Errorf("sizes %v, want [8]", f.sizes())
				}
				return read(m, f, 0, []byte("aa\x00\x00\x00\x00bb"), false)
			},
			"",
		},
		{
			"O13 a lost unstable write a Commit acknowledged reads Lost",
			func(m *soakModel, f *fileModel) error {
				write(f, 0, 'a', 4, false).faulted = true
				f.commit(1, 0, 0, vA, nil)
				return read(m, f, 0, bytesOf('a', 4), true)
			},
			"",
		},
		{
			"O14 a Commit-acknowledged overwrite reads the superseded bytes",
			func(m *soakModel, f *fileModel) error {
				write(f, 0, 'a', 4, true)
				write(f, 0, 'b', 4, false)
				f.commit(1, 0, 0, vA, nil) // the verifier held: no loss reported
				return read(m, f, 0, bytesOf('a', 4), false)
			},
			"older content served as newer",
		},
		{
			"O15 a flush through an open answering ErrLost leaves the write unacknowledged",
			func(m *soakModel, f *fileModel) error {
				write(f, 0, 'a', 4, true)
				write(f, 0, 'b', 4, false)
				f.commit(1, 0, 0, vA, vfs.ErrLost)
				return read(m, f, 0, bytesOf('a', 4), false)
			},
			"",
		},
		{"O16 a verifier repeated across a restart", func(m *soakModel, f *fileModel) error {
			if err := m.observe(1, vA); err != nil {
				return err
			}
			if err := m.observe(1, vA); err != nil {
				return fmt.Errorf("the same process: %w", err)
			}
			return m.observe(2, vA)
		}, "which process 1 answered before it"},
		{"O17 a pending existence served at the old size", func(m *soakModel, f *fileModel) error {
			write(f, 0, 'a', 8, true)
			if err := f.stat(8, t0.Add(time.Second)); err != nil {
				return fmt.Errorf("the written size and mtime: %w", err)
			}
			if err := f.stat(8, t0); err == nil {
				return errors.New("the creation's mtime accepted after an acknowledged write")
			}
			return f.stat(0, t0.Add(time.Second))
		}, "served size 0"},
		{"O18 a committed size reads Lost", func(m *soakModel, f *fileModel) error {
			write(f, 0, 'a', 4, false)
			f.commit(1, 0, 0, vA, nil)
			return read(m, f, 0, bytesOf('a', 4), true)
		}, "Lost with no fault"},
		{"O19 a credited range reads its own bytes", func(m *soakModel, f *fileModel) error {
			write(f, 0, 'a', 4, false)
			m.credit()
			f.commit(1, 0, 0, vA, nil)
			return read(m, f, 0, bytesOf('a', 4), false)
		}, ""},
		{
			"O20 a credited range reads zeros over acknowledged bytes",
			func(m *soakModel, f *fileModel) error {
				write(f, 0, 'a', 4, true)
				write(f, 0, 'b', 4, false)
				m.credit()
				f.commit(1, 0, 0, vA, nil)
				return read(m, f, 0, bytesOf(0, 4), false)
			},
			"read zero where",
		},
		{
			"O21 a dropped unacknowledged write over a hole reads zeros",
			func(m *soakModel, f *fileModel) error {
				write(f, 0, 'a', 2, true)
				write(f, 4, 'b', 4, false)
				return read(m, f, 4, bytesOf(0, 4), false)
			},
			"",
		},
		{
			"O22 a dropped unacknowledged write over acknowledged bytes reads zeros",
			func(m *soakModel, f *fileModel) error {
				write(f, 0, 'a', 4, true)
				write(f, 0, 'b', 4, false)
				return read(m, f, 0, bytesOf(0, 4), false)
			},
			"read zero where",
		},
		{"O23 a truncate's size", func(m *soakModel, f *fileModel) error {
			write(f, 0, 'a', 8, true)
			cut := f.add(&soakOp{kind: opTruncate, off: 2, proc: 1, done: true}, false)
			cut.mtime = t0.Add(time.Minute)
			if err := f.stat(2, cut.mtime); err != nil {
				return fmt.Errorf("the truncated size: %w", err)
			}
			return f.stat(8, cut.mtime)
		}, "served size 8"},
		{
			"O24 a credited write a Commit acknowledged reads Lost",
			func(m *soakModel, f *fileModel) error {
				write(f, 0, 'a', 4, false)
				m.credit()
				f.commit(1, 0, 0, vA, nil)
				return read(m, f, 0, bytesOf('a', 4), true)
			},
			"",
		},
		{"O25 a credit leaves an acknowledged write alone", func(m *soakModel, f *fileModel) error {
			write(f, 0, 'a', 4, true)
			m.credit()
			return read(m, f, 0, bytesOf('a', 4), true)
		}, "Lost with no fault"},
		{
			"O26 an unacknowledged extending write a crash dropped leaves the size",
			func(m *soakModel, f *fileModel) error {
				write(f, 0, 'a', 8, true)
				write(f, 8, 'b', 8, false)
				return f.stat(8, t0.Add(time.Second))
			},
			"",
		},
		{"O27 a deallocate past the end keeps the size", func(m *soakModel, f *fileModel) error {
			write(f, 0, 'a', 8, true)
			f.add(
				&soakOp{
					kind:  opDeallocate,
					off:   6,
					n:     10,
					proc:  1,
					done:  true,
					mtime: t0.Add(time.Second),
				},
				false,
			)
			return f.stat(8, t0.Add(time.Second))
		}, ""},
		{
			"O34 an acknowledged, faulted extending write served at the old size",
			func(m *soakModel, f *fileModel) error {
				write(f, 0, 'a', 4, true)
				write(f, 4, 'b', 4, true).faulted = true
				return f.stat(4, t0.Add(2*time.Second))
			},
			"served size 4",
		},
		{
			"O35 a faulted write no Commit acknowledged reads Lost",
			func(m *soakModel, f *fileModel) error {
				write(f, 0, 'a', 4, true)
				write(f, 0, 'b', 4, false).faulted = true
				return read(m, f, 0, bytesOf('b', 4), true)
			},
			"",
		},
		{
			"O36 a write no fault touched and no Commit acknowledged reads Lost",
			func(m *soakModel, f *fileModel) error {
				write(f, 0, 'a', 4, true)
				write(f, 0, 'b', 4, false)
				return read(m, f, 0, bytesOf('b', 4), true)
			},
			"Lost with no fault",
		},
		{
			"O37 a growing truncate no Commit acknowledged may be dropped",
			func(m *soakModel, f *fileModel) error {
				write(f, 0, 'a', 8, true)
				grow := f.add(
					&soakOp{kind: opTruncate, off: 16, proc: 1, done: true, grow: true},
					false,
				)
				grow.mtime = t0.Add(time.Minute)
				return f.stat(8, t0.Add(time.Second))
			},
			"",
		},
		{
			"O38 a growing truncate a Commit acknowledged is kept",
			func(m *soakModel, f *fileModel) error {
				write(f, 0, 'a', 8, true)
				grow := f.add(
					&soakOp{kind: opTruncate, off: 16, proc: 1, done: true, grow: true},
					false,
				)
				grow.mtime = t0.Add(time.Minute)
				f.commit(1, 0, 0, vA, nil)
				return f.stat(8, grow.mtime)
			},
			"served size 8",
		},
		{
			"O28 a loss found after an SMB open, flushed without ErrLost",
			func(m *soakModel, f *fileModel) error {
				write(f, 0, 'a', 4, true)
				o := m.open(f, 1)
				m.lose(f, 1)
				if err := m.flush(o, vA, errors.New("a store fault")); err != nil {
					return fmt.Errorf("a flush failing for another reason: %w", err)
				}
				return m.flush(o, vA, nil)
			},
			"not ErrLost",
		},
		{
			"O29 a loss found before an SMB open, flushed without ErrLost",
			func(m *soakModel, f *fileModel) error {
				write(f, 0, 'a', 4, true)
				m.lose(f, 1)
				o := m.open(f, 1)
				if err := m.flush(o, vA, nil); err != nil {
					return err
				}
				o2 := m.open(f, 1)
				m.lose(f, 1)
				if err := m.flush(o2, vA, vfs.ErrLost); err != nil {
					return fmt.Errorf("the flush that reported it: %w", err)
				}
				return m.flush(o2, vA, nil)
			},
			"",
		},
		{
			"O30 a loss found in another process than the open's",
			func(m *soakModel, f *fileModel) error {
				write(f, 0, 'a', 4, true)
				o := m.open(f, 1)
				m.lose(f, 2)
				return m.flush(o, vA, nil)
			},
			"",
		},
		{
			"O31 a failed window owes one failure of a file with an unacknowledged write",
			func(m *soakModel, f *fileModel) error {
				write(f, 0, 'a', 4, false)
				m.syncFailed()
				if !f.owed() {
					return errors.New("owes nothing after a failed window")
				}
				if f.owed() {
					return errors.New("owes a second failure")
				}
				return nil
			},
			"",
		},
		{
			"O32 a failed window owes nothing of a file whose writes are acknowledged",
			func(m *soakModel, f *fileModel) error {
				write(f, 0, 'a', 4, true)
				m.syncFailed()
				if f.owed() {
					return errors.New("owes a failure with every write acknowledged")
				}
				return nil
			},
			"",
		},
		{
			"O33 only a write answered with success is an acknowledged one",
			func(m *soakModel, f *fileModel) error {
				f.add(&soakOp{kind: opWrite, off: 0, data: bytesOf('a', 4), proc: 1}, false)
				if f.answered(2) {
					return errors.New("a write that failed counts as answered")
				}
				write(f, 8, 'b', 4, false)
				if !f.answered(10) {
					return errors.New("a write answered with success does not count")
				}
				return nil
			},
			"",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newSoakModel()
			err := c.run(m, m.file("f", t0))
			switch {
			case c.want == "" && err != nil:
				t.Errorf("rejected a correct answer: %v", err)
			case c.want != "" && err == nil:
				t.Errorf("accepted it, want a rejection naming %q", c.want)
			case c.want != "" && !strings.Contains(err.Error(), c.want):
				t.Errorf("rejected with %q, want it to name %q", err, c.want)
			}
		})
	}
}
