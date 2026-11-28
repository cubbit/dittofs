//go:build integration

package s3

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"golang.org/x/sync/errgroup"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/remote"
	"github.com/cubbit/dittofs/pkg/remote/s3/s3fault"
)

// smallBodyCap is RFC 3 §2.10's cap on a small call's reply.
const smallBodyCap = 256 << 10

// listRig is a fresh bucket on LocalStack and what opens stores over it.
type listRig struct {
	endpoint string
	roots    *x509.CertPool
	b        *s3fault.Bucket
	admin    *s3.Client // the bucket's own client, on connections closed before the watch's check
}

// newListRig makes a fresh bucket, then watches the test for leaked
// goroutines and descriptors.
func newListRig(t *testing.T) *listRig {
	t.Helper()

	// Before the watch, for the reason newLSRig gives.
	b := s3fault.FreshBucket(t)
	endpoint, roots := s3fault.LocalStack(t)
	prof.Watch(t, prof.Options{})

	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}
	t.Cleanup(tr.CloseIdleConnections)

	admin := s3.New(b.Admin.Options(), func(o *s3.Options) { o.HTTPClient = &http.Client{Transport: tr} })

	return &listRig{endpoint: endpoint, roots: roots, b: b, admin: admin}
}

// replies is a pass-through wrapper that keeps the size of each reply body
// and, when onReply is set, calls it once the reply has arrived.
type replies struct {
	next    http.RoundTripper
	onReply func()

	mu    sync.Mutex
	sizes []int
}

func (r *replies) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(b))

	r.mu.Lock()
	r.sizes = append(r.sizes, len(b))
	r.mu.Unlock()

	if r.onReply != nil {
		r.onReply()
	}

	return resp, err
}

func (r *replies) all() []int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.sizes)
}

// openAt opens a store over prefix on a new client whose requests pass
// through a recording fault transport and then rp, which keeps the replies.
func (r *listRig) openAt(t *testing.T, prefix string, rp *replies) (*store, *s3fault.Transport, *calls, *logs) {
	t.Helper()

	var tr *s3fault.Transport
	rec, l := &calls{}, &logs{}

	c, err := NewClient(ClientConfig{
		Endpoint:        r.endpoint,
		Bucket:          r.b.Name,
		AccessKeyID:     "test",
		SecretAccessKey: "test",
		TrustRoots:      r.roots,
		Limit:           4,
	}, Deps{
		Logger:   slog.New(slog.NewJSONHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Recorder: rec,
		Transport: func(rt http.RoundTripper) http.RoundTripper {
			if rp != nil {
				rp.next, rt = rt, rp
			}
			tr = s3fault.New(rt, nil)
			return tr
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	s, err := open(c, prefix, remote.Mutable, crc)
	if err != nil {
		t.Fatalf("open %q: %v", prefix, err)
	}

	return s, tr, rec, l
}

func freshPrefix() string {
	var p [4]byte
	_, _ = rand.Read(p[:])

	return "ns-" + hex.EncodeToString(p[:]) + "/"
}

// plant puts body under each key straight through the bucket's own client,
// beneath the store, as many at once as the pool allows.
func (r *listRig) plant(t *testing.T, keys []string, body func(i int) []byte) {
	t.Helper()

	g, ctx := errgroup.WithContext(t.Context())
	g.SetLimit(16)

	for i, k := range keys {
		g.Go(func() error {
			_, err := r.admin.PutObject(ctx, &s3.PutObjectInput{Bucket: &r.b.Name, Key: &k, Body: bytes.NewReader(body(i))})
			if err != nil {
				return fmt.Errorf("PutObject %s: %w", k, err)
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
}

// listsSent is the ListObjectsV2 requests among reqs.
func listsSent(reqs []s3fault.Request) int {
	n := 0
	for _, q := range reqs {
		if q.Op == "ListObjectsV2" {
			n++
		}
	}

	return n
}

// TestListWalksResumably lists 2,500 blocks in pages of 1,000, stops after
// the second page and resumes, on a store opened again over the same prefix,
// from the cursor it returned: each block once, in order, with its size and
// the time the service stored it, one request per call and at most n each.
// Walked again in pages of 500, no reply exceeds the small-body cap.
func TestListWalksResumably(t *testing.T) {
	r := newListRig(t)
	prefix := freshPrefix()
	s, tr, rec, _ := r.openAt(t, prefix, nil)

	const total = 2500
	names := make([]remote.Name, total)
	keys := make([]string, total)
	for i := range names {
		_, _ = rand.Read(names[i][:])
		keys[i] = s.blockKey(names[i])
	}
	size := func(i int) int64 { return int64(i%7 + 1) }

	before := time.Now().Add(-time.Minute) // the service's clock may trail ours
	r.plant(t, keys, func(i int) []byte { return make([]byte, size(i)) })
	afterPut := time.Now().Add(time.Minute)

	wantSize := make(map[remote.Name]int64, total)
	for i, n := range names {
		wantSize[n] = size(i)
	}
	slices.SortFunc(names, func(a, b remote.Name) int { return bytes.Compare(a[:], b[:]) })

	var got []remote.Info

	page := func(s *store, tr *s3fault.Transport, after remote.Cursor, n int) (remote.Cursor, bool) {
		t.Helper()

		sent := listsSent(tr.Requests())
		p, next, done, err := s.List(t.Context(), after, n)
		if err != nil {
			t.Fatalf("List after %q: %v", after, err)
		}
		if d := listsSent(tr.Requests()) - sent; d != 1 {
			t.Errorf("List after %q sent %d ListObjectsV2, want 1", after, d)
		}
		if len(p) > n {
			t.Errorf("List of %d returned %d blocks", n, len(p))
		}
		got = append(got, p...)

		return next, done
	}

	next, done := page(s, tr, "", 1000)
	if done {
		t.Fatal("the first page of 1,000 of 2,500 blocks is done")
	}
	if next, done = page(s, tr, next, 1000); done {
		t.Fatal("the second page of 1,000 of 2,500 blocks is done")
	}

	// Stopped here: a second store over the same prefix resumes from the
	// cursor the first returned.
	s2, tr2, _, _ := r.openAt(t, prefix, nil)
	if next, done = page(s2, tr2, next, 1000); !done {
		t.Errorf("the third page after 2,000 of 2,500 blocks is not done, next %q", next)
	}

	if len(got) != total {
		t.Fatalf("the walk yielded %d blocks, want %d", len(got), total)
	}
	for i, info := range got {
		if info.Name != names[i] {
			t.Fatalf("block %d of the walk is %x, want %x: not each once in ascending order", i, info.Name, names[i])
		}
		if info.Size != wantSize[info.Name] {
			t.Errorf("block %x: size %d, want %d", info.Name, info.Size, wantSize[info.Name])
		}
		if info.ModTime.Before(before) || info.ModTime.After(afterPut) {
			t.Errorf("block %x: stored at %v, want between %v and %v", info.Name, info.ModTime, before, afterPut)
		}
	}

	if c := rec.all(); len(c) != 2 || c[0] != (recordedCall{"list", "ok", "", 0}) {
		t.Errorf("the first store recorded %+v, want two lists, ok", c)
	}

	// The walk at 500 a page, each reply within the small-body cap.
	rp := &replies{}
	s500, tr500, _, _ := r.openAt(t, prefix, rp)
	got = nil
	for after, done := remote.Cursor(""), false; !done; {
		after, done = page(s500, tr500, after, 500)
	}
	if len(got) != total {
		t.Errorf("the walk at 500 a page yielded %d blocks, want %d", len(got), total)
	}
	for i, n := range rp.all() {
		if n > smallBodyCap {
			t.Errorf("reply %d to a page of 500 is %d bytes, over the small-body cap of %d", i, n, smallBodyCap)
		}
	}
}

// TestListSkipsForeignKeys lists 1,001 keys under blocks/ that are not block
// names, then one block, with n 1,000: the first page holds only foreign keys
// and still advances, and each foreign key is counted and logged once.
func TestListSkipsForeignKeys(t *testing.T) {
	r := newListRig(t)
	prefix := freshPrefix()
	s, tr, rec, l := r.openAt(t, prefix, nil)

	block := remote.Name{0xff, 1}
	keys := []string{s.blockKey(block)}
	for i := range 1001 {
		keys = append(keys, fmt.Sprintf("%sblocks/%05d", prefix, i))
	}
	r.plant(t, keys, func(int) []byte { return []byte("x") })

	p, next, done, err := s.List(t.Context(), "", 1000)
	switch {
	case err != nil:
		t.Fatalf("first List: %v", err)
	case len(p) != 0 || done:
		t.Fatalf("first page holds %d blocks, done %v; want none, not done", len(p), done)
	case next != remote.Cursor(keys[1000]):
		t.Fatalf("first page's next %q, want its last key %q", next, keys[1000])
	}

	p, next, done, err = s.List(t.Context(), next, 1000)
	switch {
	case err != nil:
		t.Fatalf("second List: %v", err)
	case len(p) != 1 || p[0].Name != block || !done:
		t.Fatalf("second page %+v, done %v; want the one block, done", p, done)
	case next != remote.Cursor(keys[0]):
		t.Errorf("second page's next %q, want the block's key %q", next, keys[0])
	}

	if got := listsSent(tr.Requests()); got != 2 {
		t.Errorf("two Lists sent %d ListObjectsV2, want 2", got)
	}
	if got := rec.foreign.Load(); got != 1001 {
		t.Errorf("remote_list_foreign_total counted %d, want 1001", got)
	}

	warned := map[any]int{}
	for _, m := range l.records(t) {
		if m["level"] == "WARN" {
			warned[m["key"]]++
		}
	}
	for _, k := range keys[1:] {
		if warned[k] != 1 {
			t.Fatalf("the foreign key %q logged at Warn %d times, want once", k, warned[k])
		}
	}
	if len(warned) != 1001 {
		t.Errorf("%d keys logged at Warn, want the 1001 foreign ones", len(warned))
	}
}

// TestListOfAnEncodedPrefix lists under a prefix holding non-ASCII and
// percent-encoded bytes, whose keys the store must match byte for byte.
func TestListOfAnEncodedPrefix(t *testing.T) {
	r := newListRig(t)
	prefix := "ns-ü%2F" + freshPrefix()
	s, _, rec, _ := r.openAt(t, prefix, nil)

	want := []remote.Name{{1}, {2}, {3}}
	keys := make([]string, len(want))
	for i, n := range want {
		keys[i] = s.blockKey(n)
	}
	// A key the prefix would hold were it decoded, which this store must not
	// list.
	keys = append(keys, "ns-ü/"+prefix[len("ns-ü%2F"):]+"blocks/"+hex.EncodeToString(want[0][:]))
	r.plant(t, keys, func(int) []byte { return []byte("x") })

	p, _, done, err := s.List(t.Context(), "", 10)
	if err != nil || !done {
		t.Fatalf("List = %v, done %v; want the page, done", err, done)
	}

	var got []remote.Name
	for _, i := range p {
		got = append(got, i.Name)
	}
	if !slices.Equal(got, want) {
		t.Errorf("List under %q yielded %x, want %x", prefix, got, want)
	}
	if n := rec.foreign.Load(); n != 0 {
		t.Errorf("%d foreign keys counted, want none", n)
	}
}

// TestListOfACursorOutsideItsBlocksSendsNothing: a cursor from another
// prefix's walk is refused before any request reaches the transport.
func TestListOfACursorOutsideItsBlocksSendsNothing(t *testing.T) {
	r := newListRig(t)
	a, _, _, _ := r.openAt(t, freshPrefix(), nil)
	b, tr, _, _ := r.openAt(t, freshPrefix(), nil)

	r.plant(t, []string{a.blockKey(remote.Name{1})}, func(int) []byte { return []byte("x") })

	_, next, _, err := a.List(t.Context(), "", 10)
	if err != nil || next == "" {
		t.Fatalf("List of the first store = %v, next %q", err, next)
	}

	if _, _, _, err := b.List(t.Context(), next, 10); !errors.Is(err, remote.ErrInvalid) {
		t.Errorf("List of the second store after the first's cursor = %v, want ErrInvalid", err)
	}
	if got := tr.Requests(); len(got) != 0 {
		t.Errorf("%d requests reached the transport, want none", len(got))
	}
}

// TestListCancelledMidPage cancels the call once the reply arrived: it ends
// with the context's error, recorded as local.
func TestListCancelledMidPage(t *testing.T) {
	r := newListRig(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	s, _, rec, _ := r.openAt(t, freshPrefix(), &replies{onReply: cancel})
	r.plant(t, []string{s.blockKey(remote.Name{1}), s.blockKey(remote.Name{2})}, func(int) []byte { return []byte("x") })

	p, next, done, err := s.List(ctx, "", 10)
	if !errors.Is(err, context.Canceled) || p != nil || next != "" || done {
		t.Errorf("List cancelled mid-page = %v, %q, %v, %v; want context.Canceled at the cursor it was given", p, next, done, err)
	}
	if got := rec.all(); len(got) != 1 || got[0].result != "local" {
		t.Errorf("recorded %+v, want one list, local", got)
	}
}

// TestListAllocatesFlat bounds the bytes one page of 1,000 keys allocates.
func TestListAllocatesFlat(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the race detector's allocations make the count meaningless")
	}

	r := newListRig(t)
	s, _, _, _ := r.openAt(t, freshPrefix(), nil)

	keys := make([]string, 1000)
	for i := range keys {
		var n remote.Name
		_, _ = rand.Read(n[:])
		keys[i] = s.blockKey(n)
	}
	r.plant(t, keys, func(int) []byte { return []byte("x") })

	list := func() {
		if p, _, _, err := s.List(t.Context(), "", 1000); err != nil || len(p) != 1000 {
			t.Fatalf("List = %d blocks, %v; want 1000", len(p), err)
		}
	}
	list() // the connection and the pool in place first

	const pages = 4
	var before, after runtime.MemStats

	runtime.GC()
	runtime.ReadMemStats(&before)
	for range pages {
		list()
	}
	runtime.ReadMemStats(&after)

	perPage := (after.TotalAlloc - before.TotalAlloc) / pages

	const bound = 6 << 20
	if perPage > bound {
		t.Errorf("a page of 1,000 keys allocates %d bytes, more than %d", perPage, bound)
	}

	t.Logf("bytes allocated per page of 1,000: %d", perPage)
}
