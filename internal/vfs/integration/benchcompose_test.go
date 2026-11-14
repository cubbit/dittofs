//go:build engine_conformance && engine_bench

package integration

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/prof"
	filesystem "github.com/cubbit/dittofs/internal/vfs"
	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// The benchmark composition's sizes. The share limit holds every burst of a
// run, since nothing frees journal space while it runs; the existence age is
// short enough for the group commit to come due between two bursts and long
// enough that a burst on a fast device ends before it.
const (
	benchWriters      = 64
	benchShareLimit   = 8 << 30
	benchExistenceAge = time.Second
	benchMaxFiles     = 64 // the group commit's batch bound, G
)

// benchCreators is how many goroutines create a composition's files, so a
// large pool is made in seconds rather than one transaction after another.
const benchCreators = 32

// benchDirEnv names a directory on the device under test. Without it the
// benchmarks run in the test's temporary directory, which measures whatever
// device holds it.
const benchDirEnv = "DITTOFS_BENCH_DIR"

// benchInstall is the installation ID the benchmark's journal is created under.
var benchInstall = journal.InstallationID{'e', 'n', 'g', 'b', 'e', 'n', 'c', 'h'}

// benchNode is the node the benchmark's engine runs as.
var benchNode = entity.NodeID{'b', 'e', 'n', 'c', 'h'}

// bench is the filesystem service over the engine as production composes it:
// the journal on a real directory, opened through the operating system's
// files, the metadata store on badger in a directory beside it, the open-state
// table over the store, and the wall clock read by all of them. Counts is the
// store's kv.Counting: every Update the store committed or failed.
type bench struct {
	fs     *filesystem.Filesystem
	e      *engine.Engine
	store  *kvstoretest.FS
	share  engine.Share
	admin  vfs.Call
	files  []entity.Handle
	ids    []entity.FileID // each file's ID, in the order of files
	dir    string
	counts *kv.Counting
	txns   *txnRecord // the committed transactions, classified by the keys they wrote
}

// composeBench builds the benchmark composition, with existence age age, and
// creates n files in the share's root, all before the benchmark's timer
// starts. heapSlack is the live heap growth the profiler allows; zero is its
// default.
func composeBench(b *testing.B, n int, age time.Duration, heapSlack uint64) *bench {
	b.Helper()

	// Registered first, so its checks run after every other cleanup.
	prof.Watch(b, prof.Options{HeapSlack: heapSlack})

	base := benchBase(b)

	jdir := filepath.Join(base, "journal")
	if err := os.Mkdir(jdir, 0o700); err != nil {
		b.Fatalf("bench: journal directory: %v", err)
	}

	clock := newWallClock()
	ctx := b.Context()

	id, err := journal.Init(ctx, jdir, benchInstall, journal.WithClock(clock),
		journal.WithSettings(benchJournalSettings))
	if err != nil {
		b.Fatalf("bench: journal init: %v", err)
	}

	rec := &txnRecord{}

	store := kvstoretest.NewFS(b, func(tb testing.TB) kv.KV {
		db, err := badgerkv.Open(badgerkv.Options{Dir: filepath.Join(base, "store")})
		if err != nil {
			tb.Fatalf("bench: store: %v", err)
		}

		tb.Cleanup(func() {
			_ = db.Close()
		})

		rec.KV = db

		return rec
	})
	if err := store.Store.RecordJournal(ctx, id, benchNode, entity.JournalDir(jdir)); err != nil {
		b.Fatalf("bench: record the journal: %v", err)
	}

	c := &bench{
		store:  store,
		share:  enginetest.ShareOf(b, store.Store, store.Share.ID, benchShareLimit),
		dir:    jdir,
		counts: store.Counts,
		txns:   rec,
	}
	c.start(b, clock, age)
	c.signIn(b)

	c.files, c.ids = make([]entity.Handle, n), make([]entity.FileID, n)

	if err := together(ctx, benchCreators, func(ctx context.Context, w int) error {
		for i := w; i < n; i += benchCreators {
			name := []byte(fmt.Sprintf("f%05d", i))
			ns := store.Store.Namespace()

			h, f, err := ns.Create(ctx, store.Admin, store.Root, name, kvstore.Regular(0o644))
			if err != nil {
				return fmt.Errorf("create file %d: %w", i, err)
			}

			c.files[i], c.ids[i] = h, f.ID
		}

		return nil
	}); err != nil {
		b.Fatalf("bench: files: %v", err)
	}

	printBox(b, base)

	return c
}

// start builds the open-state table, the engine and the filesystem, starts
// them in that order, and stops them in reverse when the benchmark ends.
func (c *bench) start(b *testing.B, clock *wallClock, age time.Duration) {
	b.Helper()
	ctx := b.Context()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	meta := c.store.Store

	opens, err := openstate.New(
		openstate.Deps{
			Records: meta,
			Clock:   clock,
			Rand:    rand.Reader,
			Logger:  logger,
		},
	)
	if err != nil {
		b.Fatalf("bench: open state: %v", err)
	}

	e, err := engine.New(engine.Deps{
		Node:      benchNode,
		Devices:   []string{c.dir},
		Open:      openJournal(clock),
		Existence: meta.Existence(),
		Content:   meta.Content(),
		Lifecycle: meta.Lifecycle(),
		Holds:     enginetest.Holds(opens, meta.Opens()),
		Clock:     clock,
		Logger:    logger,
		Tracer:    noTrace,
	})
	if err != nil {
		b.Fatalf("bench: engine: %v", err)
	}

	c.e = e

	c.fs, err = filesystem.New(filesystem.Deps{
		Meta:   meta,
		Open:   opens,
		Engine: e,
		Logger: logger,
	})
	if err != nil {
		b.Fatalf("bench: filesystem: %v", err)
	}

	if err := opens.Start(ctx, c.fs, c.fs.Admission); err != nil {
		b.Fatalf("bench: open state start: %v", err)
	}

	b.Cleanup(opens.Stop)

	settings := engine.Settings{
		ExistenceAge:        age,
		GroupCommitMaxFiles: benchMaxFiles,
		CloneMaxLen:         1 << 30,
	}
	if err := e.Apply(settings); err != nil {
		b.Fatalf("bench: engine settings: %v", err)
	}

	if err := e.Start(ctx, []engine.Share{c.share}); err != nil {
		b.Fatalf("bench: engine start: %v", err)
	}

	b.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		if err := e.Close(ctx); err != nil {
			b.Errorf("bench: engine close: %v", err)
		}
	})

	if err := c.fs.Start(ctx); err != nil {
		b.Fatalf("bench: filesystem start: %v", err)
	}

	b.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), benchBurstDeadline)
		defer cancel()

		if err := c.fs.Drain(ctx); err != nil {
			b.Errorf("bench: filesystem drain: %v", err)
		}

		if err := c.fs.Stop(); err != nil {
			b.Errorf("bench: filesystem stop: %v", err)
		}
	})
}

// signIn records the store's administrator as a user holding a uid, so it
// signs in as itself rather than as the share's anonymous principal.
func (c *bench) signIn(b *testing.B) {
	b.Helper()

	cp := c.store.Store.ControlPlane()
	p := c.store.Admin.Principal

	u := entity.User{
		Principal: p,
		Name:      "admin",
		IDs:       entity.ProtocolIDs{{Scheme: entity.SchemeUID, Value: "100"}},
	}
	if err := cp.PutUser(b.Context(), u); err != nil {
		b.Fatalf("bench: admin user: %v", err)
	}

	grant := entity.ShareGrant{
		Share:     c.share.ID,
		Principal: p,
		Access:    entity.AccessAdmin,
	}
	if err := cp.SetGrant(b.Context(), grant); err != nil {
		b.Fatalf("bench: admin grant: %v", err)
	}

	c.admin = signedIn(p)
}

// fill writes size bytes to each writer's file and commits their existence
// through the engine at once, so the content is committed without waiting out
// the existence age.
func (c *bench) fill(b *testing.B, size int64) {
	b.Helper()

	ctx := b.Context()
	data := make([]byte, size)

	if err := together(ctx, benchWriters, func(ctx context.Context, w int) error {
		ref := vfs.Anonymous(c.files[w])
		if _, err := c.fs.Write(ctx, c.admin, ref, 0, data, vfs.Unstable); err != nil {
			return err
		}

		return c.e.CommitExistence(ctx, c.share.ID, c.ids[w])
	}); err != nil {
		b.Fatalf("bench: the content to overwrite: %v", err)
	}

	c.awaitSizes(b, size, benchBurstDeadline)
}

// committedSize is file i's size as the store has committed it.
func (c *bench) committedSize(b *testing.B, i int) int64 {
	b.Helper()

	f, err := c.store.Store.Files().Get(b.Context(), c.store.Admin, c.files[i])
	if err != nil {
		b.Fatalf("bench: read file %d's record: %v", i, err)
	}

	return f.Size
}

// committedSizes is the committed size of each writer's file, the first
// benchWriters files.
func (c *bench) committedSizes(b *testing.B) []int64 {
	b.Helper()

	sizes := make([]int64, min(len(c.files), benchWriters))
	for i := range sizes {
		sizes[i] = c.committedSize(b, i)
	}

	return sizes
}

// awaitSizes waits until every writer's file has committed size want, polling the
// store, and fails at the deadline.
func (c *bench) awaitSizes(b *testing.B, want int64, deadline time.Duration) {
	b.Helper()

	ctx, cancel := context.WithTimeout(b.Context(), deadline)
	defer cancel()

	for {
		differ := func(s int64) bool {
			return s != want
		}
		if !slices.ContainsFunc(c.committedSizes(b), differ) {
			return
		}

		select {
		case <-ctx.Done():
			b.Fatalf(
				"bench: committed sizes %v after %v, want every one %d",
				c.committedSizes(b),
				deadline,
				want,
			)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// openJournal opens the journal on the operating system's files, waits for
// its first spares and hands it to the engine.
func openJournal(
	clock journal.Clock,
) func(context.Context, string, journal.Version, journal.JournalID) (engine.Journal, error) {
	return func(
		ctx context.Context,
		dir string,
		floor journal.Version,
		expect journal.JournalID,
	) (engine.Journal, error) {
		j, err := journal.Open(ctx, dir, floor, expect, nil, journal.WithClock(clock),
			journal.WithSettings(benchJournalSettings))
		if err != nil {
			return nil, err
		}

		if err := awaitJournalSpares(ctx, j); err != nil {
			_ = j.Close()
			return nil, err
		}

		return benchJournal{j}, nil
	}
}

// benchSpares is the journal's spare target: every write holds a spare for as
// long as it runs, so a burst of benchWriters concurrent writes needs one each,
// plus one per stream of the default 16 for its first roll. The default target,
// one per stream, refuses the rest of a 64-writer burst as retry-later.
const benchSpares = benchWriters + 16

// benchJournalSettings are the journal's defaults but for the spare target.
var benchJournalSettings = journal.Settings{SpareTarget: benchSpares}

// awaitJournalSpares returns once j's pool holds its spares: Open returns
// while the pool is still making them, and a write that needs one first is
// refused. It polls the real condition, since the pool works in real time,
// and fails at a backstop well above the pace measured here: about 25 spares
// a minute on an idle box, and 76 of 80 in 10 minutes on a loaded one.
func awaitJournalSpares(ctx context.Context, j *journal.Journal) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	want := benchSpares
	for j.Stats().Spares < want {
		select {
		case <-ctx.Done():
			return fmt.Errorf(
				"bench: the journal's pool made %d of %d spares: %w",
				j.Stats().Spares,
				want,
				ctx.Err(),
			)
		case <-time.After(time.Millisecond):
		}
	}

	return nil
}

// The journal's Share and Reserve return its concrete handle and reservation,
// and Go does not accept a concrete result for a method whose interface
// declares an interface result, so these wrap the two calls to pass the
// journal as the engine's Journal. Every other method is the journal's own.
type benchJournal struct {
	*journal.Journal
}

func (j benchJournal) Share(
	tag journal.ShareTag,
	limit int64,
	floor journal.Version,
) (engine.Handle, error) {
	h, err := j.Journal.Share(tag, limit, floor)
	if err != nil {
		return nil, err
	}

	return benchHandle{h}, nil
}

// decision: the journal's Sync returns its per-stream marks, but its
// Stats.SyncMarks and MarksRecorded, which the group commit reads and hands
// the recorded marks to, do not exist yet, so over the real journal the engine
// records none and no stability point waits on one. These two and
// benchHandle.Sync's drop of the marks stand in for all of it and withdraw
// together once the journal reports its marks: real marks reaching the
// engine while nothing records them would block every stability point.
func (benchJournal) SyncMarks() []entity.SyncMark {
	return nil
}

func (benchJournal) MarksRecorded([]entity.SyncMark) {}

type benchHandle struct {
	*journal.Handle
}

func (h benchHandle) Sync(ctx context.Context, ids ...journal.FileID) ([]entity.SyncMark, error) {
	_, err := h.Handle.Sync(ctx, ids...)

	return nil, err
}

func (h benchHandle) Reserve(n int64, writes int) (engine.Reservation, error) {
	r, err := h.Handle.Reserve(n, writes)
	if err != nil {
		return nil, err
	}

	return r, nil
}

// noTrace is a Tracer that records no span.
func noTrace(ctx context.Context, _ string, _ ...slog.Attr) (context.Context, func(error)) {
	return ctx, func(error) {}
}

// wallClock is the node's clock read from the operating system: Now is the
// wall time, Elapsed the monotonic time since the clock was made, and After a
// real timer.
type wallClock struct {
	origin time.Time
}

func newWallClock() *wallClock {
	return &wallClock{origin: time.Now()}
}

func (c *wallClock) Now() time.Time {
	return time.Now()
}

func (c *wallClock) Elapsed() time.Duration {
	return time.Since(c.origin)
}

func (c *wallClock) After(d time.Duration) <-chan struct{} {
	ch := make(chan struct{})

	time.AfterFunc(d, func() {
		close(ch)
	})

	return ch
}

// benchBase is a fresh directory under benchDirEnv, removed when the benchmark
// ends, or the benchmark's temporary directory.
func benchBase(b *testing.B) string {
	b.Helper()

	root := os.Getenv(benchDirEnv)
	if root == "" {
		return b.TempDir()
	}

	dir, err := os.MkdirTemp(root, "engine-bench-")
	if err != nil {
		b.Fatalf("bench: directory under %s: %v", benchDirEnv, err)
	}

	b.Cleanup(func() {
		_ = os.RemoveAll(dir)
	})

	return dir
}

// boxOnce prints the box once per run.
var boxOnce sync.Once

// printBox prints, before the first result line, what each result is
// recorded with: the commit, the memory, the device holding dir and its
// filesystem with the mount options, the kernel, and the device's own fsync
// latency. They are key: value lines, which benchstat reads as the
// configuration of the results below them; go test prints the CPU, the
// operating system and the architecture the same way.
func printBox(b *testing.B, dir string) {
	b.Helper()
	boxOnce.Do(func() {
		source, fstype, options := mountOf(dir)

		fmt.Printf("commit: %s\n", gitCommit())
		fmt.Printf("memory: %s\n", memTotal())
		fmt.Printf("device: %s\n", source)
		fmt.Printf("filesystem: %s\n", fstype)
		fmt.Printf("mount-options: %s\n", options)
		fmt.Printf("kernel: %s %s\n", runtime.GOOS, readTrim("/proc/sys/kernel/osrelease"))
		fmt.Printf("device-raw: %s\n", fsyncProbe(b, dir))
	})
}

// fsyncProbe is the device's own figure for the run: the p50 and p99 of 64
// writes of 4 KiB each followed by an fsync, on a file in dir.
func fsyncProbe(b *testing.B, dir string) string {
	b.Helper()

	f, err := os.CreateTemp(dir, "fsync-probe-")
	if err != nil {
		b.Fatalf("bench: fsync probe: %v", err)
	}

	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()

	buf := make([]byte, 4096)

	var took []time.Duration

	for range 64 {
		start := time.Now()

		if _, err := f.Write(buf); err != nil {
			b.Fatalf("bench: fsync probe write: %v", err)
		}

		if err := f.Sync(); err != nil {
			b.Fatalf("bench: fsync probe sync: %v", err)
		}

		took = append(took, time.Since(start))
	}

	return fmt.Sprintf("write-4KiB-fsync p50=%v p99=%v", percentile(took, 50), percentile(took, 99))
}

// mountOf is the source, filesystem type and mount options of the mount
// holding dir, from /proc/self/mountinfo: the longest mount point that
// prefixes it.
func mountOf(dir string) (source, fstype, options string) {
	abs, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "unknown", "unknown", "unknown"
	}

	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return "unknown", "unknown", "unknown"
	}

	best := ""
	source, fstype, options = "unknown", "unknown", "unknown"

	for line := range strings.Lines(string(data)) {
		pre, post, ok := strings.Cut(line, " - ")

		fields, tail := strings.Fields(pre), strings.Fields(post)
		if !ok || len(fields) < 6 || len(tail) < 2 {
			continue
		}

		point := fields[4]

		inside := abs == point || strings.HasPrefix(abs, strings.TrimSuffix(point, "/")+"/")
		if inside && len(point) > len(best) {
			best, source, fstype, options = point, tail[1], tail[0], fields[5]
		}
	}

	return source, fstype, options
}

// gitCommit is the checkout's HEAD, or "unknown" outside a checkout.
func gitCommit() string {
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return "unknown"
	}

	return strings.TrimSpace(string(out))
}

// memTotal is the box's memory, as /proc/meminfo's MemTotal line gives it.
func memTotal() string {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return "unknown"
	}

	for line := range strings.Lines(string(data)) {
		if v, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			return strings.Join(strings.Fields(v), " ")
		}
	}

	return "unknown"
}

// readTrim is the file's content without surrounding space, or "unknown".
func readTrim(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "unknown"
	}

	return strings.TrimSpace(string(data))
}

// percentile is the p-th percentile of took, nearest rank; zero when empty.
func percentile(took []time.Duration, p int) time.Duration {
	if len(took) == 0 {
		return 0
	}

	s := slices.Clone(took)
	slices.Sort(s)
	rank := (p*len(s) + 99) / 100

	return s[max(rank, 1)-1]
}

// microseconds is d in microseconds, as a metric value.
func microseconds(d time.Duration) float64 {
	return float64(d) / float64(time.Microsecond)
}

// benchBurstDeadline bounds one burst, and every other wait of a benchmark
// on the code under test: a call stuck past it fails the benchmark with the
// writers still running, rather than hanging it.
const benchBurstDeadline = time.Minute

// together runs fn(ctx, i) for every i below n at once and waits for all of
// them: the calls between two of its barriers are one burst, and the return
// of one call of together is the barrier the next one's writers start from. The first error
// cancels the others' ctx; it returns that error, naming the writer, or, once
// benchBurstDeadline passes, the writers still running.
func together(ctx context.Context, n int, fn func(ctx context.Context, i int) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errs := make([]error, n)
	running := make([]bool, n)

	var mu sync.Mutex

	done := make(chan struct{})

	var wg sync.WaitGroup

	for i := range n {
		running[i] = true

		wg.Go(func() {
			err := fn(ctx, i)

			mu.Lock()
			errs[i], running[i] = err, false
			mu.Unlock()

			if err != nil {
				cancel()
			}
		})
	}

	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(benchBurstDeadline):
		mu.Lock()
		defer mu.Unlock()

		var stuck []int

		for i, r := range running {
			if r {
				stuck = append(stuck, i)
			}
		}

		for i, err := range errs {
			if err != nil {
				return fmt.Errorf(
					"writer %d: %w; writers %v still running after %v",
					i,
					err,
					stuck,
					benchBurstDeadline,
				)
			}
		}

		return fmt.Errorf("writers %v still running after %v", stuck, benchBurstDeadline)
	}

	for i, err := range errs {
		if err != nil {
			return fmt.Errorf("writer %d: %w", i, err)
		}
	}

	return nil
}

// txnRecord passes every call through to the store's kv.KV and records, for
// each Update that committed, whether its final attempt wrote an existence key
// (an overwrite record, F‖share‖file‖ov), only other keys, or both. Earlier
// attempts the backend retried count for nothing. It answers nothing in the
// store's place: it is the pass-through record a test may assert on, beside
// kv.Counting.
//
// decision: a transaction is classified by its keys' prefixes, from the key
// layout of pkg/metadata/kv/keys; a change of that layout must change
// existenceKey, or every transaction reads as the storm's. Only the overwrite
// record marks an existence commit: a chmod writes the share's version floor
// (S‖share‖vf) too, so that key would count every chmod as the engine's.
type txnRecord struct {
	kv.KV

	mu            sync.Mutex
	engine, mixed int
}

// recordTxn notes the keys one attempt of a transaction writes.
type recordTxn struct {
	kv.Txn

	existence, other *bool
}

func (t recordTxn) Set(key, value []byte) error {
	t.note(key)

	return t.Txn.Set(key, value)
}

func (t recordTxn) Delete(key []byte) error {
	t.note(key)

	return t.Txn.Delete(key)
}

func (t recordTxn) note(key []byte) {
	if existenceKey(key) {
		*t.existence = true
	} else {
		*t.other = true
	}
}

func (r *txnRecord) Update(ctx context.Context, fn func(kv.Txn) error) error {
	var existence, other bool

	err := r.KV.Update(ctx, func(t kv.Txn) error {
		existence, other = false, false

		return fn(recordTxn{
			Txn:       t,
			existence: &existence,
			other:     &other,
		})
	})
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	switch {
	case existence && other:
		r.engine, r.mixed = r.engine+1, r.mixed+1
	case existence:
		r.engine++
	}

	return nil
}

// take returns the committed transactions since the last take, those that
// wrote an existence key (mixed ones included) and those among them that also
// wrote other keys, and resets the counts.
func (r *txnRecord) take() (engine, mixed int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	engine, mixed = r.engine, r.mixed
	r.engine, r.mixed = 0, 0

	return engine, mixed
}

// existenceKey reports whether key is an overwrite record, which only an
// existence commit of an overwrite writes.
func existenceKey(key []byte) bool {
	const sub = 1 + 2*keys.IDLen

	return len(key) > sub && key[0] == keys.KindF && key[sub] == keys.FOv
}
