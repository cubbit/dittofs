package node_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// traced builds a harness with a real SDK tracer whose spans a real in-memory
// recorder collects, over journals of maxBytes bytes in segments of 1 MiB —
// the same shape the journal series tests fill, so a write loop reaches the
// journal's own refusal.
func traced(t *testing.T, maxBytes int64) (*nodetest.Harness, *tracetest.SpanRecorder) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tracer := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)).Tracer("dittofs")
	h := nodetest.New(t, nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		cfg.Journal = append(cfg.Journal, journal.WithSettings(fillSettings(maxBytes)))
		cfg.Tracer = tracer
		return node.New(cfg)
	}))
	return h, sr
}

// fillSettings is the shape of journal that fills: maxBytes bytes over
// segments of 1 MiB, with the spare target the conformance fixtures use.
func fillSettings(maxBytes int64) journal.Settings {
	settings := journal.Settings{
		MaxBytes: maxBytes, Streams: 2, SpareTarget: 2,
		HeadroomRecords: 1024,
		IdleSealBytes:   1 << 20 / 16,
	}
	journaltest.SetSegmentCap(&settings, 1<<20)
	return settings
}

// journalSpans returns every span the recorder ended whose name carries the
// journal's prefix.
func journalSpans(t *testing.T, sr *tracetest.SpanRecorder) []sdktrace.ReadOnlySpan {
	t.Helper()
	var out []sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if strings.HasPrefix(s.Name(), "journal.") {
			out = append(out, s)
		}
	}
	return out
}

// journalName reads the journal's hex identity out of one scrape: the journal
// label its series come with, which TestJournalSeriesLabelAllowList holds to
// the identity itself.
func journalName(t *testing.T, h *nodetest.Harness) string {
	t.Helper()
	scr := jscrape(t, h)
	for _, series := range []string{"dittofs_journal_syncs_total", "dittofs_journal_written_bytes_total"} {
		for labels := range scr[series] {
			if v, ok := cutOne(labels, "journal="); ok {
				return v
			}
		}
	}
	t.Fatal("no journal series of the scrape carried the journal label, so the journal's identity is unreadable")
	return ""
}

// refused names what a write into a full journal answers: the engine answers
// a capacity refusal two ways — retry-later for a spill flat of spares or a
// pass still running, no-space-at-once when nothing is left to free — and
// either way the journal refused the write at its own limit first. The
// error's shape is not the check a span test needs, so this matches both.
func refused(err error) bool {
	var delay *vfserr.Delay
	return errors.As(err, &delay) && delay.Cause == vfserr.DelayCapacity || errors.Is(err, vfserr.ErrNoSpace)
}

// fillUntilRefused writes chunks of 256 KiB at file until the journal itself
// refuses a reservation at its limit, never longer than 128 chunks into a
// journal of 4 MiB. A capacity answer is not that refusal by its shape: a
// pool still making its next spare answers the same retry-later, so the
// journal's own refused-reservation counter, read from the node's listener,
// says when the limit was reached, and a chunk refused before it is written
// again. The bound on the wait is a real-time backstop, never the assertion.
func fillUntilRefused(t *testing.T, h *nodetest.Harness, shID entity.ShareID, fileID entity.FileID) {
	t.Helper()
	const chunk = 256 << 10
	deadline := time.Now().Add(60 * time.Second)
	for i := 0; i < 128; {
		err := jwrite(t, h, shID, fileID, int64(i)*chunk, pattern(fmt.Sprint("fill", i), chunk))
		if err == nil {
			i++
			continue
		}
		if !refused(err) {
			t.Fatalf("write %d into the journal: %v", i, err)
		}
		if v, _ := at(t, jscrape(t, h), "dittofs_journal_reservation_refused_total"); v >= 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("write %d was refused for want of a spare for 60s, want the pool to make one: %v", i, err)
		}
	}
	t.Fatal("128 writes of 256 KiB into a journal of 4 MiB were never refused, want the fill to reach the journal's limit")
}

// writeSpared is the first write into a started node: the journal's pool
// makes its first spare segment in the background, so a write that reaches
// the journal before it is answered retry-later for want of a spare. The
// write is made again on that answer, until the pool has made one; any other
// outcome is the write's own. The bound is a real-time backstop, never the
// assertion.
func writeSpared(t *testing.T, h *nodetest.Harness, s entity.ShareID, f entity.FileID, p []byte) error {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		err := jwrite(t, h, s, f, 0, p)
		var delay *vfserr.Delay
		if !errors.As(err, &delay) || delay.Cause != vfserr.DelayCapacity {
			return err
		}
		if time.Now().After(deadline) {
			t.Fatalf("the first write was refused for want of a spare for 60s, want the pool to make one: %v", err)
		}
		runtime.Gosched()
		time.Sleep(time.Millisecond) // a pause between polls of the real condition
	}
}

// A write the caller commits, a repack pass and a refused reservation each
// produce a span of their own, and every span a journal opens names its
// journal: each carries the identity the metrics export labels the same
// journal with. The drives are bounded by their own outcomes — the commit
// returns, the fill loop stops at its first refusal — never by a sleep.
func TestTheJournalTracesItsOperations(t *testing.T) {
	h, sr := traced(t, 4<<20)
	started(t, h)
	sh := addShare(t, h)
	file := createFiles(t, h, sh, 1)[0]
	caughtUp(t, h)

	if err := writeSpared(t, h, sh.ID, file.ID, pattern("trace", 4096)); err != nil {
		t.Fatal(err)
	}
	if err := jcommit(t, h, sh.ID, file.ID); err != nil {
		t.Fatal(err)
	}
	var syncs int
	for _, s := range journalSpans(t, sr) {
		if s.Name() == "journal.sync" {
			syncs++
		}
	}
	if syncs != 1 {
		t.Errorf("one committed write produced %d journal.sync spans so far, want exactly 1", syncs)
	}

	fillUntilRefused(t, h, sh.ID, file.ID)

	const hexLen = 32 // hex of the journal's 16-byte identity
	seen := map[string]int{}
	var missing, wrongLen int
	for _, s := range journalSpans(t, sr) {
		seen[s.Name()]++
		ident := ""
		for _, a := range s.Attributes() {
			if string(a.Key) == "journal" {
				ident = a.Value.AsString()
			}
		}
		switch {
		case ident == "":
			missing++
		case len(ident) != hexLen:
			wrongLen++
			t.Errorf("span %s carries the journal as %q, want the hex of its 16-byte identity", s.Name(), ident)
		}
	}
	if missing > 0 {
		t.Errorf("%d of the %d journal spans carry no journal attribute, want every span of the journal's prefix to name its journal", missing, len(journalSpans(t, sr)))
	}
	var refusals int
	for _, s := range journalSpans(t, sr) {
		if s.Name() != "journal.reserve_refused" {
			continue
		}
		refusals++
		if s.Status().Code != codes.Error {
			t.Errorf("a journal.reserve_refused span ended with status %v, want Error: the span ends with the refusal", s.Status().Code)
		}
		var excepted bool
		for _, e := range s.Events() {
			excepted = excepted || e.Name == "exception"
		}
		if !excepted {
			t.Errorf("a journal.reserve_refused span recorded no exception event, want the refusal recorded on it")
		}
	}
	if refusals == 0 {
		t.Error("no journal.reserve_refused span to hold to the refusal, want the fill's")
	}
	for _, name := range []string{"journal.sync", "journal.reserve_refused", "journal.repack_pass"} {
		if seen[name] < 1 {
			t.Errorf("no span of op %s after the drives, want at least the one each of them ran", name)
		}
	}
	if want := journalName(t, h); want != "" {
		for _, s := range journalSpans(t, sr) {
			for _, a := range s.Attributes() {
				if string(a.Key) == "journal" && a.Value.AsString() != want {
					t.Errorf("span %s names the journal %s, want the identity the metrics export labels it %s", s.Name(), a.Value.AsString(), want)
				}
			}
		}
	}
}

// With Config.Tracer nil, the same drives run to their own outcomes: the
// commit returns and the fill reaches its first refusal.
func TestTheJournalRunsUntracedWithoutATracer(t *testing.T) {
	h := nodetest.New(t, nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		cfg.Journal = append(cfg.Journal, journal.WithSettings(fillSettings(4<<20)))
		return node.New(cfg) // Config.Tracer left nil: the journals open untraced
	}))
	started(t, h)
	sh := addShare(t, h)
	file := createFiles(t, h, sh, 1)[0]
	caughtUp(t, h)
	if err := writeSpared(t, h, sh.ID, file.ID, pattern("untraced", 4096)); err != nil {
		t.Fatal(err)
	}
	if err := jcommit(t, h, sh.ID, file.ID); err != nil {
		t.Fatalf("a committed write over an untraced journal: %v, want it done, never blocked", err)
	}
	fillUntilRefused(t, h, sh.ID, file.ID)
}

// The logger the node hands its journals names the journal in every line the
// journal writes: after a restart over a written segment, the attach pass
// meets a file this journal does not recognise and logs it under the
// journal's hex identity. The segment must already be there — an open over a
// directory holding none runs no recovery, and no attach pass to log — so
// the drive writes one first, as the node's own writes do.
func TestTheJournalLoggerNamesTheJournal(t *testing.T) {
	log := &logBuffer{}
	var dev string
	h := nodetest.New(t, nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		dev = cfg.Bootstrap.Journals[0].Path
		cfg.Logger = slog.New(slog.NewJSONHandler(log, &slog.HandlerOptions{Level: slog.LevelDebug}))
		return node.New(cfg)
	}))
	started(t, h)
	sh := addShare(t, h)
	file := createFiles(t, h, sh, 1)[0]
	caughtUp(t, h)
	if err := writeSpared(t, h, sh.ID, file.ID, pattern("logged", 4096)); err != nil {
		t.Fatal(err)
	}
	if err := jcommit(t, h, sh.ID, file.ID); err != nil {
		t.Fatal(err)
	}

	// A file the journal could not have written, planted beside its
	// segment before the restart: the attach pass of the open the restart
	// runs leaves it alone and says so.
	if err := os.WriteFile(filepath.Join(dev, "stray.txt"), []byte("neither a segment nor a mark"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.Restart(t)
	started(t, h)

	sc := bufio.NewScanner(strings.NewReader(log.String()))
	identities := map[string]bool{}
	attach := false
	for sc.Scan() {
		var r map[string]any
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("parse the journal's line %q: %v", sc.Text(), err)
		}
		v, ok := r["journal"]
		if !ok {
			continue // the node's own line, not a journal's
		}
		s, ok := v.(string)
		if !ok {
			t.Fatalf("a journal line names the journal as %v, want a string", v)
		}
		if len(s) != 32 || strings.TrimLeft(s, "0123456789abcdef") != "" {
			t.Errorf("a journal line names the journal %q, want the hex of its 16-byte identity", s)
		}
		identities[s] = true
		if m, _ := r["msg"].(string); strings.HasPrefix(m, "journal attach: a file this journal does not recognise") {
			attach = true
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(identities) != 1 {
		t.Errorf("the journal's lines name %d journals, want the one identity the node's one device holds", len(identities))
	}
	if !attach {
		t.Error("no journal line named the file the attach pass left alone, want the attach's own Debug line through the node's logger")
	}
}

// logBuffer is a log the node writes while it runs and the test reads once
// the drive is done, its own lock keeping the two apart.
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
