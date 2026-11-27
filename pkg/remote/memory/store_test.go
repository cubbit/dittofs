package memory_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"slices"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/cubbit/dittofs/pkg/remote"
	"github.com/cubbit/dittofs/pkg/remote/memory"
	"github.com/cubbit/dittofs/pkg/sim"
)

// sentinels is RFC 4 §4.8's set, ErrThrottled counted as one with
// ErrTransient and ErrMalformed with ErrCorrupt, which each wraps.
var sentinels = []error{remote.ErrNotFound, remote.ErrInvalid, remote.ErrDenied, remote.ErrTransient, remote.ErrCorrupt}

// matched returns the sentinels err matches.
func matched(err error) []error {
	var out []error

	for _, s := range sentinels {
		if errors.Is(err, s) {
			out = append(out, s)
		}
	}

	return out
}

// checkErr fails t unless err matches want and no other sentinel: want is a
// sentinel, or a local error that must match no sentinel at all.
func checkErr(t *testing.T, what string, err, want error) {
	t.Helper()

	if !errors.Is(err, want) {
		t.Errorf("%s: error %v, want %v", what, err, want)
		return
	}

	got := matched(err)

	wantSentinel := slices.Contains(sentinels, want)
	if (wantSentinel && len(got) != 1) || (!wantSentinel && len(got) != 0) {
		t.Errorf("%s: error %v matches sentinels %v, want only %v", what, err, got, want)
	}
}

// sumOf returns body's checksum of kind, the raw digest.
func sumOf(kind remote.Checksum, body []byte) []byte {
	switch kind {
	case remote.ChecksumCRC32C:
		h := crc32.New(crc32.MakeTable(crc32.Castagnoli))
		h.Write(body)

		return h.Sum(nil)
	case remote.ChecksumMD5:
		d := md5.Sum(body)
		return d[:]
	}

	return nil
}

var (
	blockA = remote.Name{0xa}
	first  = bytes.Repeat([]byte("first "), 100)
	second = bytes.Repeat([]byte("second"), 100)
)

func newStore(sum remote.Checksum) *memory.Store {
	return memory.New(sim.NewTestClock(epoch).Now, sum)
}

// mustPut puts body under n with its right checksum.
func mustPut(t *testing.T, s *memory.Store, n remote.Name, body []byte) {
	t.Helper()

	if err := s.Put(t.Context(), n, bytes.NewReader(body), int64(len(body)), sumOf(s.Checksum(), body)); err != nil {
		t.Fatalf("Put of %d bytes: %v", len(body), err)
	}
}

// read gets the whole of n.
func read(t *testing.T, s *memory.Store, n remote.Name) ([]byte, error) {
	t.Helper()

	r, _, err := s.Get(t.Context(), n, remote.Range{})
	if err != nil {
		return nil, err
	}
	got, err := io.ReadAll(r)
	if cerr := r.Close(); err == nil {
		err = cerr
	}

	return got, err
}

var errReader = errors.New("the body's reader failed")

// Each put that fails wraps exactly its sentinel, or returns the local error
// as itself, and leaves the store as it was: a new name stays absent, and a
// name already stored keeps its first bytes.
func TestAFailedPutWrapsOneSentinelAndStoresNothing(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	// A zero field takes the put that succeeds: no checksum mode, the test's
	// context, second's bytes and length, and their right checksum.
	for _, tc := range []struct {
		name string
		mode remote.Checksum
		ctx  context.Context
		body func() io.Reader // a new reader for each put
		size int64
		sum  []byte
		want error
	}{
		{name: "negative size", size: -1, want: remote.ErrInvalid},
		{name: "body one byte short", size: int64(len(second)) + 1, want: remote.ErrInvalid},
		{name: "a size far past the body", size: 1 << 50, want: remote.ErrInvalid},
		{name: "body one byte long", size: int64(len(second)) - 1, want: remote.ErrInvalid},
		{name: "body one byte short, CRC32C", mode: remote.ChecksumCRC32C, size: int64(len(second)) + 1, want: remote.ErrInvalid},
		{name: "CRC32C of other bytes", mode: remote.ChecksumCRC32C, sum: sumOf(remote.ChecksumCRC32C, first), want: remote.ErrCorrupt},
		{name: "MD5 of other bytes", mode: remote.ChecksumMD5, sum: sumOf(remote.ChecksumMD5, first), want: remote.ErrCorrupt},
		{name: "no checksum where one is needed", mode: remote.ChecksumCRC32C, sum: []byte{}, want: remote.ErrInvalid},
		{name: "a checksum of the other kind", mode: remote.ChecksumCRC32C, sum: sumOf(remote.ChecksumMD5, second), want: remote.ErrInvalid},
		{name: "a body reader failing", body: func() io.Reader { return io.MultiReader(bytes.NewReader(second[:10]), iotest.ErrReader(errReader)) }, want: errReader},
		{name: "a context already ended", ctx: cancelled, want: context.Canceled},
	} {
		for _, existing := range []bool{false, true} {
			s := newStore(tc.mode)
			if existing {
				mustPut(t, s, blockA, first)
			}

			ctx := t.Context()
			if tc.ctx != nil {
				ctx = tc.ctx
			}

			body := io.Reader(bytes.NewReader(second))
			if tc.body != nil {
				body = tc.body()
			}

			size := tc.size
			if size == 0 {
				size = int64(len(second))
			}

			sum := sumOf(tc.mode, second)
			if tc.sum != nil {
				sum = tc.sum
			}

			what := tc.name
			if existing {
				what += " over a stored block"
			}

			perr := s.Put(ctx, blockA, body, size, sum)
			checkErr(t, "Put, "+what, perr, tc.want)

			if tc.want == errReader && perr != errReader {
				t.Errorf("Put, %s: error %v, want the reader's error itself, unwrapped", what, perr)
			}

			got, err := read(t, s, blockA)
			switch {
			case existing && (err != nil || !bytes.Equal(got, first)):
				t.Errorf("Get after a failed Put, %s: %d bytes and error %v, want the %d bytes stored first", what, len(got), err, len(first))
			case !existing && !errors.Is(err, remote.ErrNotFound):
				t.Errorf("Get after a failed Put, %s: %d bytes and error %v, want ErrNotFound", what, len(got), err)
			}
		}
	}
}

// Each get that fails wraps exactly its sentinel, or returns the context's
// error as itself.
func TestAFailedGetWrapsOneSentinel(t *testing.T) {
	s := newStore(remote.ChecksumNone)
	mustPut(t, s, blockA, first)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	size := int64(len(first))

	for _, tc := range []struct {
		name string
		ctx  context.Context
		n    remote.Name
		r    remote.Range
		want error
	}{
		{"an absent block", nil, remote.Name{0xb}, remote.Range{}, remote.ErrNotFound},
		{"a range straddling the end", nil, blockA, remote.Range{Off: size - 1, Len: 2}, remote.ErrInvalid},
		{"a range starting at the end", nil, blockA, remote.Range{Off: size, Len: 1}, remote.ErrInvalid},
		{"a range starting past the end", nil, blockA, remote.Range{Off: size + 1, Len: 1}, remote.ErrInvalid},
		{"a negative offset", nil, blockA, remote.Range{Off: -1, Len: 2}, remote.ErrInvalid},
		{"an empty range off the start", nil, blockA, remote.Range{Off: 1}, remote.ErrInvalid},
		{"a negative length", nil, blockA, remote.Range{Off: 1, Len: -1}, remote.ErrInvalid},
		{"a length that overflows the offset", nil, blockA, remote.Range{Off: 1, Len: 1<<63 - 1}, remote.ErrInvalid},
		{"a context already ended", cancelled, blockA, remote.Range{}, context.Canceled},
	} {
		ctx := t.Context()
		if tc.ctx != nil {
			ctx = tc.ctx
		}

		r, _, err := s.Get(ctx, tc.n, tc.r)
		if r != nil {
			_ = r.Close()
			t.Errorf("Get, %s: returned a reader with error %v", tc.name, err)
		}

		checkErr(t, "Get, "+tc.name, err, tc.want)
	}
}

// A put refused for its checksum stores nothing, and the retry with the
// right checksum stores the block.
func TestAPutRefusedForItsChecksumIsStoredByItsRetry(t *testing.T) {
	for _, mode := range []remote.Checksum{remote.ChecksumCRC32C, remote.ChecksumMD5} {
		s := newStore(mode)

		err := s.Put(t.Context(), blockA, bytes.NewReader(second), int64(len(second)), sumOf(mode, first))
		checkErr(t, "Put with the checksum of other bytes", err, remote.ErrCorrupt)

		mustPut(t, s, blockA, second)

		if got, err := read(t, s, blockA); err != nil || !bytes.Equal(got, second) {
			t.Errorf("mode %d: Get after the retried Put: %d bytes and error %v, want the %d bytes put", mode, len(got), err, len(second))
		}
	}
}

// blockingReader signals entered on its first Read and waits there for ctx to
// end; that Read and every later one yields one byte.
type blockingReader struct {
	ctx     context.Context
	entered chan struct{}
	once    sync.Once
}

func (b *blockingReader) Read(p []byte) (int, error) {
	b.once.Do(func() {
		close(b.entered)
		<-b.ctx.Done()
	})

	if len(p) == 0 {
		return 0, nil
	}

	p[0] = 'x'

	return 1, nil
}

// A context cancelled while the body's reader is blocked ends the put with
// the context's error at its next check, and stores nothing: Put cannot
// interrupt a Read the caller's reader is blocked in, so the reader ending is
// what lets the check run.
func TestAPutCancelledDuringItsBodyReturnsTheContextsError(t *testing.T) {
	s := newStore(remote.ChecksumNone)

	ctx, cancel := context.WithCancel(t.Context())
	body := &blockingReader{ctx: ctx, entered: make(chan struct{})}

	done := make(chan error, 1)
	go func() { done <- s.Put(ctx, blockA, body, 10, nil) }()

	// backstop bounds each wait, so a Put that never reads or never returns
	// fails here rather than at the suite's timeout.
	const backstop = 30 * time.Second

	select {
	case <-body.entered:
	case <-time.After(backstop):
		t.Fatal("Put never read its body")
	}

	cancel()

	select {
	case err := <-done:
		checkErr(t, "Put cancelled while its body blocks", err, context.Canceled)
	case <-time.After(backstop):
		t.Fatal("Put did not return once its context was cancelled and its reader returned")
	}

	if _, err := read(t, s, blockA); !errors.Is(err, remote.ErrNotFound) {
		t.Errorf("Get after a cancelled Put: error %v, want ErrNotFound", err)
	}
}

// Two puts of one name with the same bytes overlap with gets of it, and every
// get that finds the block reads it whole. Run under -race, this catches
// unsynchronised access to the store's objects.
func TestConcurrentPutsAndGetsOfOneName(t *testing.T) {
	s := newStore(remote.ChecksumCRC32C)

	var wg sync.WaitGroup

	for range 2 {
		wg.Go(func() {
			for range 50 {
				if err := s.Put(t.Context(), blockA, bytes.NewReader(first), int64(len(first)), sumOf(remote.ChecksumCRC32C, first)); err != nil {
					t.Errorf("concurrent Put: %v", err)
				}
			}
		})
	}

	for range 4 {
		wg.Go(func() {
			for range 50 {
				got, err := read(t, s, blockA)
				if err == nil && !bytes.Equal(got, first) {
					t.Errorf("concurrent Get: %d bytes, want the %d bytes put", len(got), len(first))
				}

				if err != nil && !errors.Is(err, remote.ErrNotFound) {
					t.Errorf("concurrent Get: %v, want the block or ErrNotFound", err)
				}
			}
		})
	}

	wg.Wait()

	if got, err := read(t, s, blockA); err != nil || !bytes.Equal(got, first) {
		t.Errorf("Get after the puts: %d bytes and error %v, want the %d bytes put", len(got), err, len(first))
	}
}

// A put under a stored name replaces the block whole.
func TestAPutReplacesTheBlockUnderItsName(t *testing.T) {
	s := newStore(remote.ChecksumMD5)
	mustPut(t, s, blockA, first)
	mustPut(t, s, blockA, second[:len(second)-1])

	if got, err := read(t, s, blockA); err != nil || !bytes.Equal(got, second[:len(second)-1]) {
		t.Errorf("Get after a second Put of the name: %d bytes and error %v, want the %d bytes put second", len(got), err, len(second)-1)
	}
}

// A store built with a checksum kind it cannot check refuses to be built,
// rather than accept every put unverified.
func TestNewPanicsOnAnUnknownChecksumKind(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("New with checksum kind 7 returned a store, want a panic")
		}
	}()

	memory.New(sim.NewTestClock(epoch).Now, remote.Checksum(7))
}

// A listing reports the time the store's clock read at the put, never the
// host's: the clock sits at a fixed past instant when the block is put.
func TestAListingReportsTheClocksTimeAtThePut(t *testing.T) {
	clock := sim.NewTestClock(epoch)
	clock.Advance(time.Hour)

	s := memory.New(clock.Now, remote.ChecksumNone)
	mustPut(t, s, blockA, first)

	clock.Advance(time.Hour)

	page, _, _, err := s.List(t.Context(), "", 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if want := epoch.Add(time.Hour); len(page) != 1 || !page[0].ModTime.Equal(want) {
		t.Fatalf("List after one put at %v: %+v, want one block with that ModTime", want, page)
	}
}

// A batch naming one block three times and one never written returns one nil
// per name, in order, not one per distinct name.
func TestADeleteReturnsOneResultPerNameItWasGiven(t *testing.T) {
	s := newStore(remote.ChecksumNone)
	mustPut(t, s, blockA, first)

	batch := []remote.Name{blockA, {0xb}, blockA, blockA}

	errs := s.Delete(t.Context(), batch)
	if len(errs) != len(batch) {
		t.Fatalf("Delete of %d names: %d results, want %d", len(batch), len(errs), len(batch))
	}

	for i, err := range errs {
		if err != nil {
			t.Errorf("Delete: result %d is %v, want nil", i, err)
		}
	}

	if _, err := read(t, s, blockA); !errors.Is(err, remote.ErrNotFound) {
		t.Errorf("Get after Delete: %v, want ErrNotFound", err)
	}
}

// A page of no keys asked for is ErrInvalid; an empty store's page is done at
// the cursor it was given.
func TestAListOfNoKeysIsRefusedAndAnEmptyStoreIsDone(t *testing.T) {
	s := newStore(remote.ChecksumNone)

	for _, n := range []int{0, -1} {
		_, _, _, err := s.List(t.Context(), "", n)
		checkErr(t, fmt.Sprintf("List of %d keys", n), err, remote.ErrInvalid)
	}

	page, next, done, err := s.List(t.Context(), "", 10)
	if err != nil || len(page) != 0 || next != "" || !done {
		t.Errorf("List of an empty store: %d blocks, next %q, done %v, error %v; want none, the zero Cursor, done, nil", len(page), next, done, err)
	}
}
