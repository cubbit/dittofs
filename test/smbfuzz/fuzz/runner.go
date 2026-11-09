//go:build linux

package fuzz

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cubbit/dittofs/test/smbfuzz/sd"
)

// Sides, in the order every step runs on them.
const (
	SideRef = "ref"
	SideDut = "dut"
)

var sides = []string{SideRef, SideDut}

// UserCred is one user's credentials and local uid.
type UserCred struct {
	Account  string `json:"account"`
	Password string `json:"password"`
	UID      int    `json:"uid"`
	GID      int    `json:"gid"`
	Keytab   string `json:"keytab,omitempty"`
}

// Env is everything a run needs from the fixture.
type Env struct {
	RefRoot   string              `json:"ref_root"` // mountpoint of the reference share
	DutRoot   string              `json:"dut_root"`
	Domain    string              `json:"domain"`
	Realm     string              `json:"realm"`
	Auth      string              `json:"auth"`    // ntlm | krb5
	Profile   string              `json:"profile"` // sd | posix
	Users     map[string]UserCred `json:"users"`   // by plan name: alice, bob, carol, admin
	DomainSID string              `json:"domain_sid"`
	Accounts  map[string]string   `json:"accounts"` // trustee name -> SID
	Timeout   time.Duration       `json:"timeout"`
}

// Hooks are the fixture's out-of-band actions (DESIGN.md §10).
type Hooks interface {
	// CleanRef removes a round directory on the reference server's disk.
	CleanRef(dir string) error
	// ResetDut removes and recreates the DittoFS share.
	ResetDut() error
	// DutAlive reports whether the DittoFS process is running, and a log
	// tail when it is not.
	DutAlive() (bool, string)
	// Ticket writes a Kerberos credential cache for account at path.
	Ticket(account, path string) error
}

// Runner executes plans.
type Runner struct {
	env      Env
	hooks    Hooks
	resolver *sd.Resolver
	self     string
	log      io.Writer

	mu      sync.Mutex
	workers map[string]*workerProc
	attempt int
	rounds  int // rounds since the last DittoFS share reset
	known   *Known
}

// SetKnown makes the runner record a known divergence and carry on with the
// round instead of ending it there (DESIGN.md §12.2): a known divergence says
// nothing about the steps after it, which may still find a new one.
func (r *Runner) SetKnown(k *Known) { r.known = k }

// NewRunner builds a runner; workers start on first use.
func NewRunner(env Env, hooks Hooks, log io.Writer) (*Runner, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if env.Timeout == 0 {
		env.Timeout = 30 * time.Second
	}
	res := &sd.Resolver{Accounts: map[string]sd.SID{}}
	if env.DomainSID != "" {
		res.Domain, err = sd.ParseSID(env.DomainSID)
		if err != nil {
			return nil, err
		}
	}
	for n, s := range env.Accounts {
		sid, err := sd.ParseSID(s)
		if err != nil {
			return nil, err
		}
		res.Accounts[n] = sid
	}
	return &Runner{env: env, hooks: hooks, resolver: res, self: self, log: log, workers: map[string]*workerProc{}}, nil
}

// Resolver returns the trustee resolver.
func (r *Runner) Resolver() *sd.Resolver { return r.resolver }

// Close stops every worker.
func (r *Runner) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, w := range r.workers {
		w.kill()
	}
	r.workers = map[string]*workerProc{}
}

type workerProc struct {
	cmd   *exec.Cmd
	in    io.WriteCloser
	out   *bufio.Scanner
	reply chan Reply
	dead  chan struct{}
}

func (w *workerProc) kill() {
	_ = w.in.Close()
	if w.cmd.Process != nil {
		_ = w.cmd.Process.Kill()
	}
	_ = w.cmd.Wait()
}

// WorkerEnv is the environment variable carrying a worker's configuration.
const WorkerEnv = "SMBFUZZ_WORKER"

func (r *Runner) worker(user string) (*workerProc, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if w, ok := r.workers[user]; ok {
		select {
		case <-w.dead:
			w.kill()
			delete(r.workers, user)
		default:
			return w, nil
		}
	}
	cred, ok := r.env.Users[user]
	if !ok {
		return nil, fmt.Errorf("no credentials for user %q", user)
	}
	if r.env.Auth == "krb5" {
		if err := r.kinit(cred); err != nil {
			return nil, err
		}
	}
	cfg := WorkerConfig{User: user, Account: cred.Account, Domain: r.env.Domain, Password: cred.Password,
		Auth: r.env.Auth, Keytab: cred.Keytab, Realm: r.env.Realm}
	b, _ := json.Marshal(cfg)
	cmd := exec.Command(r.self, "worker")
	cmd.Env = append(os.Environ(), WorkerEnv+"="+string(b),
		fmt.Sprintf("KRB5CCNAME=FILE:/tmp/krb5cc_%d", cred.UID))
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(cred.UID), Gid: uint32(cred.GID), Groups: []uint32{}},
		Setsid:     true,
	}
	if user == "admin" {
		// The passthrough ioctl that sends an explicit SET_SPARSE needs
		// CAP_SYS_ADMIN (DESIGN.md §7.2); only the admin draws it.
		cmd.SysProcAttr.AmbientCaps = []uintptr{unix.CAP_SYS_ADMIN}
	}
	cmd.Stderr = r.log
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start worker %s: %w", user, err)
	}
	sc := bufio.NewScanner(outPipe)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	w := &workerProc{cmd: cmd, in: in, out: sc, reply: make(chan Reply), dead: make(chan struct{})}
	go func() {
		defer close(w.dead)
		for sc.Scan() {
			var rep Reply
			if err := json.Unmarshal(sc.Bytes(), &rep); err != nil {
				rep = Reply{Fatal: "bad reply: " + err.Error()}
			}
			w.reply <- rep
		}
	}()
	r.workers[user] = w
	return w, nil
}

// kinit fills the user's credential cache, where cifs.upcall looks for the
// calling uid's tickets (DESIGN.md §4.2).
func (r *Runner) kinit(c UserCred) error {
	if r.hooks == nil {
		return fmt.Errorf("krb5 needs the fixture's hooks")
	}
	cc := fmt.Sprintf("/tmp/krb5cc_%d", c.UID)
	if err := r.hooks.Ticket(c.Account, cc); err != nil {
		return err
	}
	return os.Chown(cc, c.UID, c.GID)
}

// Errno values the runner uses for outcomes that are not system call errors.
const (
	errTimeout = -1
	errCrashed = -2
)

func (r *Runner) call(user string, req *Request) Reply {
	w, err := r.worker(user)
	if err != nil {
		return Reply{Fatal: err.Error()}
	}
	b, _ := json.Marshal(req)
	if _, err := w.in.Write(append(b, '\n')); err != nil {
		r.dropWorker(user, w)
		return Reply{Errno: errCrashed}
	}
	select {
	case rep := <-w.reply:
		return rep
	case <-w.dead:
		r.dropWorker(user, w)
		return Reply{Errno: errCrashed}
	case <-time.After(r.env.Timeout):
		r.dropWorker(user, w)
		return Reply{Errno: errTimeout}
	}
}

func (r *Runner) dropWorker(user string, w *workerProc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w.kill()
	if r.workers[user] == w {
		delete(r.workers, user)
	}
}

func (r *Runner) root(side string) string {
	if side == SideRef {
		return r.env.RefRoot
	}
	return r.env.DutRoot
}

// resolve turns a plan step into a request for one side.
func (r *Runner) resolve(round uint64, dir, side string, n int, s Step) (*Request, error) {
	req := &Request{Step: n, Side: side, Op: s.Op, Seed: round}
	a := s.Args
	abs := func(p string) string { return filepath.Join(r.root(side), dir, ExpandPath(p)) }
	if p, ok := a["path"]; ok {
		req.Path = abs(p)
	}
	if p, ok := a["path2"]; ok {
		req.Path2 = abs(p)
	}
	if v, ok := a["slot"]; ok {
		req.Slot = side + ":" + v
	}
	if v, ok := a["slot2"]; ok {
		req.Slot2 = side + ":" + v
	}
	if s.Out != "" {
		req.Slot2 = side + ":" + s.Out
	}
	var err error
	size := func(k string) int64 {
		if err != nil {
			return 0
		}
		v, ok := a[k]
		if !ok {
			return 0
		}
		var x int64
		x, err = EvalSize(v)
		return x
	}
	req.Off, req.Off2, req.Len = size("off"), size("off2"), size("len")
	if err != nil {
		return nil, err
	}
	req.Data = a["data"]
	if v, ok := a["mode"]; ok && s.Op != "fallocate" {
		m, e := strconv.ParseUint(v, 8, 32)
		if e != nil {
			return nil, fmt.Errorf("mode %q", v)
		}
		req.Mode = uint32(m)
	}
	flagsOf := func(v string, t []flagName) (int, error) {
		x, e := parseFlags(v, t)
		return int(x), e
	}
	switch s.Op {
	case "open":
		req.Flags, err = flagsOf(a["flags"], openFlagNames)
	case "fallocate":
		var m int
		m, err = flagsOf(a["mode"], fallocNames)
		req.Mode = uint32(m)
	case "lseek":
		req.Whence, err = flagsOf(a["whence"], whenceNames)
	case "rename":
		req.Flags, err = flagsOf(a["flags"], renameNames)
	case "setxattr":
		req.Flags, err = flagsOf(a["flags"], xattrFlagNames)
	case "flock":
		req.Flags, err = flagsOf(a["how"], flockNames)
	case "setsparse":
		req.On = a["on"] == "true"
	case "mkfiles":
		var from, count int
		from, err = strconv.Atoi(a["from"])
		if err == nil {
			count, err = strconv.Atoi(a["count"])
		}
		req.Off, req.Len = int64(from), int64(count)
	case "utimensat":
		req.ATime, err = parseTime(a["atime"])
		if err == nil {
			req.MTime, err = parseTime(a["mtime"])
		}
	case "setbtime":
		var t TimeArg
		t, err = parseTime(a["t"])
		req.BTime = fileTime(t)
	case "setdosattr":
		var v uint64
		v, err = parseFlags(a["attrs"], dosAttrNames)
		req.Attrs = uint32(v)
	case "lock", "testlock":
		if a["kind"] == "shared" {
			req.LockType = unix.F_RDLCK
		} else {
			req.LockType = unix.F_WRLCK
		}
	case "getacl":
		req.Part = a["part"]
	case "setacl":
		req.Part = a["part"]
		var d *sd.SD
		d, err = r.resolver.Parse(a["sd"])
		if err == nil {
			req.SD = d.Marshal()
		}
	case "setowner":
		req.Part = "owner"
		var d *sd.SD
		d, err = r.resolver.Parse("O:" + a["owner"] + "G:" + a["group"])
		if err == nil {
			req.SD = d.Marshal()
		}
	case "chown":
		req.UID, req.GID = r.localID(a["owner"]), r.localID(a["group"])
	}
	if v, ok := a["name"]; ok {
		req.XName = ExpandXattrName(v)
	}
	if v, ok := a["value"]; ok {
		pat, l, _ := strings.Cut(v, ":")
		n, e := strconv.Atoi(l)
		if e != nil {
			return nil, fmt.Errorf("xattr value %q", v)
		}
		req.XValue = make([]byte, n)
		Pattern(pat, round, n, req.XValue)
	}
	return req, err
}

// localID maps a trustee to the local id chown sends. In the sd profile the
// client only passes it on as a number, so any fixed mapping works as long as
// both sides get the same number.
func (r *Runner) localID(t string) int {
	if t == "-1" {
		return -1
	}
	for name, c := range r.env.Users {
		if c.Account == t || name == t {
			return c.UID
		}
	}
	for i, tr := range Trustees {
		if tr.V == t {
			return 30000 + i
		}
	}
	return 39999
}

func parseTime(v string) (TimeArg, error) {
	switch v {
	case "OMIT":
		return TimeArg{Omit: true}, nil
	case "NOW":
		return TimeArg{Now: true}, nil
	}
	sec, nsec, _ := strings.Cut(v, ".")
	s, err := strconv.ParseInt(sec, 10, 64)
	if err != nil {
		return TimeArg{}, fmt.Errorf("time %q", v)
	}
	n, err := strconv.ParseInt(nsec, 10, 64)
	if err != nil {
		return TimeArg{}, fmt.Errorf("time %q", v)
	}
	return TimeArg{Sec: s, Nsec: n}, nil
}

// fileTime converts to a Windows FILETIME; times before 1601 clamp to 0.
func fileTime(t TimeArg) uint64 {
	v := (t.Sec+11644473600)*10_000_000 + t.Nsec/100
	if v < 0 {
		return 0
	}
	return uint64(v)
}

// StepResult is one step on both sides.
type StepResult struct {
	N       string // "01", or "S" for the sweep
	Line    string
	Ref     Reply
	Dut     Reply
	Diverge *Divergence
	Notes   []string
}

// Result is one run of one plan.
type Result struct {
	Plan    *Plan
	Dir     string
	Steps   []StepResult
	Diverge *Divergence
	Known   []string // known divergences met and passed over
	Err     error    // the run itself failed: fixture, setup, plan error
	DutLog  string
}

// Signature returns the divergence signature, or "".
func (res *Result) Signature() string {
	if res.Diverge == nil {
		return ""
	}
	return res.Diverge.Sig
}

// Run executes a plan once, in a fresh round directory.
func (r *Runner) Run(p *Plan) *Result {
	r.mu.Lock()
	r.attempt++
	dir := fmt.Sprintf("r%d-a%d", p.Round, r.attempt)
	r.mu.Unlock()
	res := &Result{Plan: p, Dir: dir}
	defer r.cleanup(dir)
	if err := r.setupRound(dir, p.Root); err != nil {
		res.Err = err
		return res
	}
	cmp := newComparer(r.env.Profile, r.resolver)
	// The tree is built as admin steps, compared like plan steps: a server
	// that cannot build it the same way diverges at a step numbered T.
	for _, ts := range p.Tree.SetupSteps() {
		sr := r.runStep(p.Round, dir, ts.N, "T", ts.Step, cmp)
		sr.Line = "T" + strings.TrimLeft(sr.Line, "0123456789")
		res.Steps = append(res.Steps, sr)
		if sr.Ref.Fatal != "" || sr.Dut.Fatal != "" {
			res.Err = fmt.Errorf("tree setup: %s%s", sr.Ref.Fatal, sr.Dut.Fatal)
			return res
		}
		if sr.Diverge != nil && !r.passKnown(res, sr.Diverge) {
			res.Diverge = sr.Diverge
			r.checkDut(res)
			return res
		}
	}
	for i, s := range p.Steps {
		sr := r.runStep(p.Round, dir, i+1, fmt.Sprintf("%02d", i+1), s, cmp)
		res.Steps = append(res.Steps, sr)
		if sr.Ref.Fatal != "" || sr.Dut.Fatal != "" {
			res.Err = fmt.Errorf("step %d: %s%s", i+1, sr.Ref.Fatal, sr.Dut.Fatal)
			return res
		}
		if sr.Diverge != nil {
			if r.known != nil && r.known.Has(sr.Diverge.Sig) {
				res.Known = append(res.Known, sr.Diverge.Sig)
				continue
			}
			res.Diverge = sr.Diverge
			r.checkDut(res)
			return res
		}
	}
	r.sweep(p.Round, dir, cmp, res)
	if res.Diverge != nil {
		r.checkDut(res)
	}
	return res
}

func (r *Runner) checkDut(res *Result) {
	if r.hooks == nil {
		return
	}
	if alive, tail := r.hooks.DutAlive(); !alive {
		res.DutLog = tail
	}
}

func (r *Runner) runStep(round uint64, dir string, n int, label string, s Step, cmp *comparer) StepResult {
	sr := StepResult{N: label, Line: FormatStep(n, s)}
	var reqs [2]*Request
	for i, side := range sides {
		req, err := r.resolve(round, dir, side, n, s)
		if err != nil {
			sr.Ref.Fatal = "plan: " + err.Error()
			return sr
		}
		reqs[i] = req
	}
	sr.Ref = r.call(s.User, reqs[0])
	sr.Dut = r.call(s.User, reqs[1])
	if sr.Ref.Fatal != "" || sr.Dut.Fatal != "" {
		return sr
	}
	sr.Diverge, sr.Notes = cmp.compare(s, sr.Ref, sr.Dut)
	if sr.Diverge != nil {
		sr.Diverge.Step = label
	}
	return sr
}

// setupRound creates the round directory as admin on both sides and sets the
// round root's DACL.
func (r *Runner) setupRound(dir, root string) error {
	text, ok := RoundRoots[root]
	if !ok {
		return fmt.Errorf("unknown round root %q", root)
	}
	d, err := r.resolver.Parse(text)
	if err != nil {
		return err
	}
	for _, side := range sides {
		path := filepath.Join(r.root(side), dir)
		if rep := r.call("admin", &Request{Side: side, Op: "mkdir", Path: path, Mode: 0o777}); rep.Errno != 0 || rep.Fatal != "" {
			return fmt.Errorf("%s: mkdir %s: %s%s", side, dir, errName(rep.Errno), rep.Fatal)
		}
		if rep := r.call("admin", &Request{Side: side, Op: "setacl", Path: path, Part: "dacl", SD: d.Marshal()}); rep.Errno != 0 || rep.Fatal != "" {
			return fmt.Errorf("%s: set round root: %s%s", side, errName(rep.Errno), rep.Fatal)
		}
	}
	return nil
}

// cleanup removes a round directory (DESIGN.md §10): out of band on the
// reference; as admin over SMB on DittoFS, taking ownership where refused,
// and resetting the share when anything is left or every 100 rounds.
func (r *Runner) cleanup(dir string) {
	// Close every slot first: an open handle keeps a file from going.
	r.Close()
	if r.hooks != nil {
		if err := r.hooks.CleanRef(dir); err != nil {
			fmt.Fprintf(r.log, "cleanup ref %s: %v\n", dir, err)
		}
	}
	left := r.removeTree(SideDut, filepath.Join(r.env.DutRoot, dir))
	r.mu.Lock()
	r.rounds++
	reset := left || r.rounds >= 100
	if reset {
		r.rounds = 0
	}
	r.mu.Unlock()
	if reset && r.hooks != nil {
		if err := r.hooks.ResetDut(); err != nil {
			fmt.Fprintf(r.log, "reset DittoFS share: %v\n", err)
		}
	}
}

func (r *Runner) adminTakeOver(side, path string) {
	own, _ := r.resolver.Parse("O:AdministratorG:DU")
	full, _ := r.resolver.Parse("D:(A;OICI;FA;;;Administrator)(A;OICI;FA;;;WD)")
	r.call("admin", &Request{Side: side, Op: "setowner", Path: path, Part: "owner", SD: own.Marshal()})
	r.call("admin", &Request{Side: side, Op: "setacl", Path: path, Part: "dacl", SD: full.Marshal()})
	r.call("admin", &Request{Side: side, Op: "setdosattr", Path: path, Attrs: 0x80})
}

// removeTree removes path recursively as admin; it reports whether anything
// is left.
func (r *Runner) removeTree(side, path string) bool {
	rep := r.call("admin", &Request{Side: side, Op: "lstat", Path: path})
	if rep.Errno == int(unix.ENOENT) {
		return false
	}
	if rep.Errno != 0 || rep.Out == nil || rep.Out.Stat == nil {
		r.adminTakeOver(side, path)
		rep = r.call("admin", &Request{Side: side, Op: "lstat", Path: path})
		if rep.Errno != 0 || rep.Out == nil || rep.Out.Stat == nil {
			return rep.Errno != int(unix.ENOENT)
		}
	}
	if rep.Out.Stat.Type == "dir" {
		ls := r.call("admin", &Request{Side: side, Op: "readdir", Path: path})
		if ls.Errno != 0 {
			r.adminTakeOver(side, path)
			ls = r.call("admin", &Request{Side: side, Op: "readdir", Path: path})
		}
		if ls.Out != nil {
			for _, e := range ls.Out.Dir {
				r.removeTree(side, filepath.Join(path, e.Name))
			}
		}
		if r.call("admin", &Request{Side: side, Op: "rmdir", Path: path}).Errno == 0 {
			return false
		}
		r.adminTakeOver(side, path)
		return r.call("admin", &Request{Side: side, Op: "rmdir", Path: path}).Errno != 0
	}
	if r.call("admin", &Request{Side: side, Op: "unlink", Path: path}).Errno == 0 {
		return false
	}
	r.adminTakeOver(side, path)
	return r.call("admin", &Request{Side: side, Op: "unlink", Path: path}).Errno != 0
}

// sweep walks the round directory as admin on both sides and compares what
// it finds (DESIGN.md §9.1). It stops at the first divergence.
func (r *Runner) sweep(round uint64, dir string, cmp *comparer, res *Result) {
	queue := []string{"."}
	for len(queue) > 0 && res.Diverge == nil {
		rel := queue[0]
		queue = queue[1:]
		ls := Step{User: "admin", Op: "readdir", Args: map[string]string{"path": rel}}
		sr := r.runStep(round, dir, 0, "S", ls, cmp)
		res.Steps = append(res.Steps, sr)
		if sr.Diverge != nil && !r.passKnown(res, sr.Diverge) {
			res.Diverge = sr.Diverge
			return
		}
		if sr.Ref.Out == nil {
			continue
		}
		for _, e := range sr.Ref.Out.Dir {
			p := e.Name
			if rel != "." {
				p = rel + "/" + e.Name
			}
			// The wide directory's fillers are empty files the listing
			// above already compared; looking at each would cost thousands
			// of requests per round.
			if IsFiller(p) {
				continue
			}
			steps := []Step{
				{User: "admin", Op: "lstat", Args: map[string]string{"path": p}},
				{User: "admin", Op: "getdosattr", Args: map[string]string{"path": p}},
				{User: "admin", Op: "getacl", Args: map[string]string{"path": p, "part": "ntsd"}},
			}
			if e.Type == "file" {
				steps = append(steps,
					Step{User: "admin", Op: "open", Args: map[string]string{"path": p, "flags": "O_RDONLY", "mode": "0000"}, Out: "admin.sweep"},
					Step{User: "admin", Op: "pread", Args: map[string]string{"slot": "admin.sweep", "off": "0", "len": "16777216+16777216"}},
					Step{User: "admin", Op: "close", Args: map[string]string{"slot": "admin.sweep"}},
				)
			}
			for _, s := range steps {
				sr := r.runStep(round, dir, 0, "S", s, cmp)
				sr.Line = strings.Replace(sr.Line, "00", " S", 1)
				res.Steps = append(res.Steps, sr)
				if sr.Diverge != nil && !r.passKnown(res, sr.Diverge) {
					res.Diverge = sr.Diverge
					return
				}
			}
			if e.Type == "dir" {
				queue = append(queue, p)
			}
		}
	}
}

func (r *Runner) passKnown(res *Result, d *Divergence) bool {
	if r.known != nil && r.known.Has(d.Sig) {
		res.Known = append(res.Known, d.Sig)
		return true
	}
	return false
}

func errName(e int) string {
	switch e {
	case 0:
		return "OK"
	case errTimeout:
		return "TIMEOUT"
	case errCrashed:
		return "CRASHED"
	}
	if n := unix.ErrnoName(unix.Errno(e)); n != "" {
		return n
	}
	return "E" + strconv.Itoa(e)
}
