//go:build linux

package fuzz

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// A worker serves a sequence of requests against a local directory, and a
// read returns the hash of exactly the bytes the pattern wrote.
func TestWorkerLocal(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "f0")
	reqs := []Request{
		{Step: 1, Op: "open", Path: f, Flags: unix.O_CREAT | unix.O_RDWR, Mode: 0o644, Slot2: "ref:alice.h0"},
		{Step: 2, Op: "pwrite", Slot: "ref:alice.h0", Off: 4097, Len: 8192, Data: "seq"},
		{Step: 3, Op: "pread", Slot: "ref:alice.h0", Off: 4097, Len: 1 << 20},
		{Step: 4, Op: "fstat", Slot: "ref:alice.h0"},
		{Step: 5, Op: "lock", Slot: "ref:alice.h0", LockType: unix.F_WRLCK, Off: 0, Len: 10},
		{Step: 6, Op: "close", Slot: "ref:alice.h0"},
		{Step: 7, Op: "close", Slot: "ref:alice.h0"},
		{Step: 8, Op: "readdir", Path: dir},
		{Step: 9, Op: "rmdir", Path: f},
	}
	var in bytes.Buffer
	enc := json.NewEncoder(&in)
	for i := range reqs {
		_ = enc.Encode(&reqs[i])
	}
	var out bytes.Buffer
	if err := RunWorker(WorkerConfig{User: "alice", Auth: "local"}, &in, &out); err != nil {
		t.Fatal(err)
	}
	var reps []Reply
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var r Reply
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		reps = append(reps, r)
	}
	if len(reps) != len(reqs) {
		t.Fatalf("%d replies for %d requests", len(reps), len(reqs))
	}
	want := make([]byte, 8192)
	Pattern("seq", 0, 2, want)
	if got := hashOut(want).Hash; reps[2].Out == nil || reps[2].Out.Hash != got || reps[2].Out.N != 8192 {
		t.Fatalf("pread: %+v, want hash %s", reps[2].Out, got)
	}
	if st := reps[3].Out.Stat; st.Type != "file" || st.Size != 4097+8192 {
		t.Fatalf("fstat: %+v", st)
	}
	for i, want := range []int{0, 0, 0, 0, 0, 0, int(unix.EBADF), 0, int(unix.ENOTDIR)} {
		if reps[i].Errno != want {
			t.Errorf("step %d: errno %s, want %s", i+1, errName(reps[i].Errno), errName(want))
		}
	}
	if d := reps[7].Out.Dir; len(d) != 1 || d[0].Name != "f0" || d[0].Type != "file" {
		t.Fatalf("readdir: %+v", d)
	}
}

// A divergence's signature names the op and the two errnos; outputs name the
// part that differs.
func TestCompareSignatures(t *testing.T) {
	c := newComparer("sd", testResolver())
	s := Step{User: "bob", Op: "open", Args: map[string]string{"path": "f0"}, Out: "bob.h0"}
	d, _ := c.compare(s, Reply{Errno: int(unix.EACCES)}, Reply{})
	if d == nil || d.Sig != "open:errno:EACCES/OK" {
		t.Fatalf("got %+v", d)
	}
	r := testResolver()
	a, _ := r.Parse("O:aliceG:DUD:(A;;FA;;;WD)")
	b, _ := r.Parse("O:bobG:DUD:(A;;FA;;;WD)")
	g := Step{User: "bob", Op: "getacl", Args: map[string]string{"path": "f0", "part": "ntsd"}}
	d, _ = c.compare(g, Reply{Out: &Output{Bytes: a.Marshal()}}, Reply{Out: &Output{Bytes: b.Marshal()}})
	if d == nil || d.Sig != "getacl:owner:alice/bob" {
		t.Fatalf("got %+v", d)
	}
	// An ACL difference is signed by the part of the first entry that
	// differs, a stat field by the object's type, DOS attributes by the bits
	// that differ: a known entry names one situation, not a whole field.
	for _, x := range []struct{ ref, dut, sig string }{
		{"D:(A;;FA;;;WD)(A;;FR;;;alice)", "D:(A;;FA;;;WD)(A;;FW;;;alice)", "getacl:dacl:mask"},
		{"D:(A;;FA;;;WD)(A;;FR;;;alice)", "D:(A;;FA;;;WD)(A;;FR;;;bob)", "getacl:dacl:trustee"},
		{"D:(A;;FA;;;WD)(A;OI;FR;;;alice)", "D:(A;;FA;;;WD)(A;;FR;;;alice)", "getacl:dacl:flags"},
		{"D:(A;;FA;;;WD)(A;;FR;;;alice)", "D:(A;;FA;;;WD)", "getacl:dacl:count"},
		{"D:P(A;;FA;;;WD)", "D:(A;;FA;;;WD)", "getacl:dacl:control"},
	} {
		a, _ := r.Parse("O:aliceG:DU" + x.ref)
		b, _ := r.Parse("O:aliceG:DU" + x.dut)
		if d, _ := c.compare(g, Reply{Out: &Output{Bytes: a.Marshal()}}, Reply{Out: &Output{Bytes: b.Marshal()}}); d == nil || d.Sig != x.sig {
			t.Errorf("%s vs %s: got %+v, want %s", x.ref, x.dut, d, x.sig)
		}
	}
	sz := Step{User: "bob", Op: "stat", Args: map[string]string{"path": "d0"}}
	if d, _ := c.compare(sz, Reply{Out: &Output{Stat: &StatOut{Type: "dir", Size: 4096}}},
		Reply{Out: &Output{Stat: &StatOut{Type: "dir"}}}); d == nil || d.Sig != "stat:size:dir" {
		t.Errorf("a directory's size: got %+v", d)
	}
	da := Step{User: "bob", Op: "getdosattr", Args: map[string]string{"path": "f0"}}
	if d, _ := c.compare(da, Reply{Out: &Output{Bytes: []byte{0x22, 0, 0, 0}}},
		Reply{Out: &Output{Bytes: []byte{0x20, 0, 0, 0}}}); d == nil || d.Sig != "getdosattr:attrs:HIDDEN" {
		t.Errorf("DOS attributes: got %+v", d)
	}
	st := Step{User: "bob", Op: "stat", Args: map[string]string{"path": "f0"}}
	at := func(sec float64) *StatOut { return &StatOut{Type: "file", Mtime: int64(sec * 1e9)} }
	if d, _ := c.compare(st, Reply{Out: &Output{Stat: at(1000)}}, Reply{Out: &Output{Stat: at(1000.5)}}); d != nil {
		t.Fatalf("a first look within the slack differs, got %+v", d)
	}
	if d, _ := c.compare(st, Reply{Out: &Output{Stat: at(1003)}}, Reply{Out: &Output{Stat: at(1000.5)}}); d == nil || d.Sig != "stat:mtime:advanced/unchanged" {
		t.Fatalf("got %+v", d)
	}
	st2 := Step{User: "bob", Op: "stat", Args: map[string]string{"path": "f1"}}
	if d, _ := c.compare(st2, Reply{Out: &Output{Stat: at(1000)}}, Reply{Out: &Output{Stat: at(1002)}}); d == nil || d.Sig != "stat:mtime:apart" {
		t.Fatalf("a first look two seconds apart: got %+v", d)
	}
}

// The minimiser keeps a removal only when the signature survives it.
func TestMinimizeKeepsSignature(t *testing.T) {
	p, _ := Generate(1, 7)
	// The fake fails with "x" whenever step 5's line is in the plan, and
	// with "y" when only step 9's is.
	key5, key9 := FormatStep(0, p.Steps[4]), FormatStep(0, p.Steps[8])
	x := fakeExec(func(q *Plan) string {
		has5, has9 := false, false
		for _, s := range q.Steps {
			has5 = has5 || FormatStep(0, s) == key5
			has9 = has9 || FormatStep(0, s) == key9
		}
		switch {
		case has5:
			return "x"
		case has9:
			return "y"
		}
		return ""
	})
	first := x.Run(p)
	m := Minimize(x, first, &bytes.Buffer{})
	if len(m.Plan.Steps) != 1 || FormatStep(0, m.Plan.Steps[0]) != key5 || m.Result.Signature() != "x" {
		t.Fatalf("minimised to %d steps:\n%s", len(m.Plan.Steps), m.Plan.Format())
	}
}

type fakeExec func(*Plan) string

func (f fakeExec) Run(p *Plan) *Result {
	sig := f(p)
	r := &Result{Plan: p}
	if sig != "" {
		r.Diverge = &Divergence{Sig: sig}
	}
	return r
}

// The tree's setup steps, resolved as the runner resolves them and run by a
// worker on a local directory, build the tree: every object, its content, the
// wide directory's fillers, and the fixed times.
func TestTreeSetupLocal(t *testing.T) {
	dir := t.TempDir()
	r := &Runner{env: Env{RefRoot: dir, DutRoot: dir}, resolver: testResolver()}
	tree := Tree{
		{Path: "d0", Dir: true},
		{Path: "d0/d1", Dir: true},
		{Path: "d0/d1/f0", Len: "4096+1", Data: "seq"},
		{Path: "f1", Len: "0", Data: "zero"},
		{Path: WideDir, Dir: true, Fill: 250},
	}
	setup := tree.SetupSteps()
	var in bytes.Buffer
	enc := json.NewEncoder(&in)
	for _, ts := range setup {
		req, err := r.resolve(9, "", SideRef, ts.N, ts.Step)
		if err != nil {
			t.Fatal(err)
		}
		_ = enc.Encode(req)
	}
	var out bytes.Buffer
	if err := RunWorker(WorkerConfig{User: "admin", Auth: "local"}, &in, &out); err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var rep Reply
		if err := json.Unmarshal([]byte(line), &rep); err != nil {
			t.Fatal(err)
		}
		// The local file system may refuse the client's creation-time
		// attribute; everything else must succeed.
		if rep.Errno != 0 && setup[i].Step.Op != "setbtime" {
			t.Fatalf("%s: %s", FormatStep(setup[i].N, setup[i].Step), errName(rep.Errno))
		}
	}
	var st unix.Stat_t
	if err := unix.Stat(filepath.Join(dir, "d0/d1/f0"), &st); err != nil || st.Size != 4097 {
		t.Fatalf("d0/d1/f0: %v, size %d", err, st.Size)
	}
	if st.Mtim.Sec != 1500000000+1000*2 || st.Mtim.Nsec != 123456700 {
		t.Fatalf("d0/d1/f0 mtime %d.%d", st.Mtim.Sec, st.Mtim.Nsec)
	}
	if err := unix.Stat(filepath.Join(dir, "d0"), &st); err != nil || st.Mtim.Sec != 1500000000 {
		t.Fatalf("d0 mtime moved after its times were set: %d", st.Mtim.Sec)
	}
	ents, err := os.ReadDir(filepath.Join(dir, WideDir))
	if err != nil || len(ents) != 250 || !IsFiller(WideDir+"/"+ents[0].Name()) {
		t.Fatalf("wide directory: %v, %d entries", err, len(ents))
	}
}

// An ACE edit, resolved from a plan step as the runner resolves it, inserts,
// removes or changes exactly the entry it names and keeps every other entry,
// inherited ones included, in its place.
func TestApplyACEEdit(t *testing.T) {
	res := testResolver()
	r := &Runner{env: Env{RefRoot: "/r", DutRoot: "/d"}, resolver: res}
	const start = "D:(A;;FA;;;alice)(D;;FR;;;bob)(A;ID;FR;;;WD)"
	for _, c := range []struct {
		step Step
		want string
		err  error
	}{
		{Step{Op: "aceadd", Args: map[string]string{"part": "dacl", "index": "0", "ace": "(D;;0x1;;;carol)"}},
			"D:(D;;0x1;;;carol)(A;;FA;;;alice)(D;;FR;;;bob)(A;ID;FR;;;WD)", nil},
		{Step{Op: "aceadd", Args: map[string]string{"part": "dacl", "index": "1", "ace": "(A;OICI;0x2;;;devs)"}},
			"D:(A;;FA;;;alice)(A;OICI;0x2;;;devs)(D;;FR;;;bob)(A;ID;FR;;;WD)", nil},
		{Step{Op: "aceadd", Args: map[string]string{"part": "dacl", "index": "end", "ace": "(A;;0x1;;;carol)"}},
			"D:(A;;FA;;;alice)(D;;FR;;;bob)(A;ID;FR;;;WD)(A;;0x1;;;carol)", nil},
		{Step{Op: "aceadd", Args: map[string]string{"part": "ntsd_full", "index": "0", "ace": "(AU;SAFA;0x1;;;WD)"}},
			"D:(A;;FA;;;alice)(D;;FR;;;bob)(A;ID;FR;;;WD)S:(AU;SAFA;0x1;;;WD)", nil},
		{Step{Op: "acedel", Args: map[string]string{"part": "dacl", "index": "last"}},
			"D:(A;;FA;;;alice)(D;;FR;;;bob)", nil},
		{Step{Op: "acedel", Args: map[string]string{"part": "dacl", "index": "1"}},
			"D:(A;;FA;;;alice)(A;ID;FR;;;WD)", nil},
		{Step{Op: "aceedit", Args: map[string]string{"part": "dacl", "index": "1", "mask": "0x2", "aceflags": "OICI"}},
			"D:(A;;FA;;;alice)(D;OICI;0x2;;;bob)(A;ID;FR;;;WD)", nil},
		{Step{Op: "aceedit", Args: map[string]string{"part": "dacl", "index": "0", "mask": "0x1", "aceflags": "-"}},
			"D:(A;;0x1;;;alice)(D;;FR;;;bob)(A;ID;FR;;;WD)", nil},
	} {
		d, err := res.Parse(start)
		if err != nil {
			t.Fatal(err)
		}
		c.step.Args["path"] = "f0"
		req, err := r.resolve(1, "", SideRef, 1, c.step)
		if err != nil {
			t.Fatalf("%s: resolve: %v", FormatStep(1, c.step), err)
		}
		if err := applyACEEdit(d, req); err != c.err {
			t.Fatalf("%s: %v, want %v", FormatStep(1, c.step), err, c.err)
		}
		want, _ := res.Parse(c.want)
		if got, w := res.Format(d), res.Format(want); got != w {
			t.Errorf("%s:\n got %s\nwant %s", FormatStep(1, c.step), got, w)
		}
	}
	// A position past the list, or a list that is not there, is refused.
	empty, _ := res.Parse("D:")
	if err := applyACEEdit(empty, &Request{Op: "acedel", Index: "0"}); err != unix.ENOENT {
		t.Fatalf("acedel on an empty DACL: %v", err)
	}
	null, _ := res.Parse("D:NO_ACCESS_CONTROL")
	if err := applyACEEdit(null, &Request{Op: "aceedit", Index: "0"}); err != unix.ENOENT {
		t.Fatalf("aceedit on a null DACL: %v", err)
	}
}

// A timeout on one side is signed by that side, whatever the other answered;
// a timeout or crash on both sides is the fuzzer's failure, not agreement.
func TestTimeoutsAndCrashes(t *testing.T) {
	c := newComparer("sd", testResolver())
	s := Step{User: "bob", Op: "pread", Args: map[string]string{"slot": "bob.h0"}}
	for _, x := range []struct {
		ref, dut int
		sig      string
	}{
		{0, errTimeout, "pread:timeout:dut"},
		{int(unix.EACCES), errTimeout, "pread:timeout:dut"},
		{errTimeout, 0, "pread:timeout:ref"},
		{errCrashed, 0, "pread:errno:CRASHED/OK"},
	} {
		d, _ := c.compare(s, Reply{Errno: x.ref}, Reply{Errno: x.dut})
		if d == nil || d.Sig != x.sig {
			t.Errorf("%s/%s: got %+v, want %s", errName(x.ref), errName(x.dut), d, x.sig)
		}
	}
	for _, x := range [][2]int{{errTimeout, errTimeout}, {errCrashed, errCrashed}, {errTimeout, errCrashed}} {
		if fuzzerFailure(Reply{Errno: x[0]}, Reply{Errno: x[1]}) == "" {
			t.Errorf("%s/%s: not a fuzzer failure", errName(x[0]), errName(x[1]))
		}
	}
	for _, x := range [][2]int{{0, 0}, {errTimeout, 0}, {int(unix.EIO), int(unix.EIO)}} {
		if f := fuzzerFailure(Reply{Errno: x[0]}, Reply{Errno: x[1]}); f != "" {
			t.Errorf("%s/%s: a fuzzer failure: %s", errName(x[0]), errName(x[1]), f)
		}
	}
}

// SEEK_DATA answered ENXIO on one side and an offset on the other is an
// allocation difference, noted like two different offsets; SEEK_HOLE's ENXIO,
// and SEEK_DATA's other errnos, still diverge.
func TestSeekDataENXIOIsANote(t *testing.T) {
	c := newComparer("sd", testResolver())
	seek := func(whence string) Step {
		return Step{User: "bob", Op: "lseek", Args: map[string]string{"slot": "bob.h0", "off": "0", "whence": whence}}
	}
	enxio, at := Reply{Errno: int(unix.ENXIO)}, Reply{Out: &Output{N: 8192}}
	for _, x := range []struct{ ref, dut Reply }{{enxio, at}, {at, enxio}} {
		d, notes := c.compare(seek("SEEK_DATA"), x.ref, x.dut)
		if d != nil || len(notes) != 1 || !strings.Contains(notes[0], "ENXIO") {
			t.Errorf("SEEK_DATA %s/%s: diverge %+v, notes %q", errName(x.ref.Errno), errName(x.dut.Errno), d, notes)
		}
	}
	if d, _ := c.compare(seek("SEEK_HOLE"), enxio, at); d == nil || d.Sig != "lseek:errno:ENXIO/OK" {
		t.Errorf("SEEK_HOLE ENXIO/OK: got %+v", d)
	}
	if d, _ := c.compare(seek("SEEK_DATA"), Reply{Errno: int(unix.EINVAL)}, at); d == nil || d.Sig != "lseek:errno:EINVAL/OK" {
		t.Errorf("SEEK_DATA EINVAL/OK: got %+v", d)
	}
}
