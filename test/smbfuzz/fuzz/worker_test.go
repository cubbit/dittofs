//go:build linux

package fuzz

import (
	"bytes"
	"encoding/json"
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
	if d == nil || d.Sig != "getacl:owner" {
		t.Fatalf("got %+v", d)
	}
	st := Step{User: "bob", Op: "stat", Args: map[string]string{"path": "f0"}}
	one := &StatOut{Type: "file", Mtime: 10}
	two := &StatOut{Type: "file", Mtime: 20}
	if d, _ := c.compare(st, Reply{Out: &Output{Stat: one}}, Reply{Out: &Output{Stat: two}}); d != nil {
		t.Fatalf("first observation differs only in value, got %+v", d)
	}
	if d, _ := c.compare(st, Reply{Out: &Output{Stat: &StatOut{Type: "file", Mtime: 11}}}, Reply{Out: &Output{Stat: two}}); d == nil || d.Sig != "stat:mtime:advanced/unchanged" {
		t.Fatalf("got %+v", d)
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
