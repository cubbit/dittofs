package corpus

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// content is 64 KiB of seeded bytes, the same on every run.
func content() []byte {
	b := make([]byte, 64<<10)
	_, _ = rand.NewChaCha8([32]byte{1}).Read(b)

	return b
}

func gzipped(t *testing.T, b []byte) []byte {
	t.Helper()

	var buf bytes.Buffer

	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}

	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

// serve serves each file at its path on 127.0.0.1:0. The same path under
// /truncated announces the file's full length and sends half of it.
func serve(t *testing.T, files map[string][]byte) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, truncated := strings.CutPrefix(r.URL.Path, "/truncated")

		b, ok := files[path]
		if !ok {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Length", strconv.Itoa(len(b)))

		if truncated {
			b = b[:len(b)/2]
		}

		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)

	return srv
}

// corpusFixture is a server holding a plain file and a gzipped one, and the
// entries pinning them.
type corpusFixture struct {
	srv       *httptest.Server
	raw       []byte
	film, src Entry
}

func newFixture(t *testing.T) corpusFixture {
	t.Helper()

	raw := content()
	gz := gzipped(t, raw)
	srv := serve(t, map[string][]byte{
		"/film.mp4":   raw,
		"/src.tar.gz": gz,
	})

	return corpusFixture{
		srv: srv, raw: raw,
		film: Entry{Kind: "media", Name: "film.mp4", URL: srv.URL + "/film.mp4", Download: sha256.Sum256(raw), Stored: sha256.Sum256(raw)},
		src:  Entry{Kind: "source", Name: "src.tar", URL: srv.URL + "/src.tar.gz", Download: sha256.Sum256(gz), Stored: sha256.Sum256(raw)},
	}
}

func readStored(t *testing.T, dir string, e Entry) []byte {
	t.Helper()

	b, err := os.ReadFile(filepath.Join(dir, e.Kind, e.Name))
	if err != nil {
		t.Fatalf("%s/%s: %v", e.Kind, e.Name, err)
	}

	return b
}

// Fetch stores each entry at <dir>/<kind>/<name>, a .gz decompressed; fetches
// a stored file changed by a byte again; and downloads nothing once every
// file is stored, which a closed server would refuse.
func TestFetchStoresEachEntry(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	entries := []Entry{f.film, f.src}

	if err := Fetch(t.Context(), http.DefaultClient, dir, entries); err != nil {
		t.Fatalf("first fetch: %v", err)
	}

	for _, e := range entries {
		if got := readStored(t, dir, e); !bytes.Equal(got, f.raw) {
			t.Errorf("%s/%s after the first fetch: %d bytes, want the %d served, decompressed", e.Kind, e.Name, len(got), len(f.raw))
		}
	}

	changed := append([]byte(nil), f.raw...)
	changed[len(changed)/2] ^= 1

	if err := os.WriteFile(filepath.Join(dir, f.src.Kind, f.src.Name), changed, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Fetch(t.Context(), http.DefaultClient, dir, entries); err != nil {
		t.Fatalf("fetch over a changed file: %v", err)
	}

	if got := readStored(t, dir, f.src); !bytes.Equal(got, f.raw) {
		t.Errorf("%s/%s changed by a byte was kept, want it fetched again", f.src.Kind, f.src.Name)
	}

	f.srv.Close()

	if err := Fetch(t.Context(), http.DefaultClient, dir, entries); err != nil {
		t.Errorf("fetch with every file stored: %v, want nothing downloaded", err)
	}
}

// An entry whose bytes do not match is refused and leaves no file under its
// name, nor a temporary one beside it, and the entry beside it is fetched.
func TestFetchRefuses(t *testing.T) {
	f := newFixture(t)

	for _, tc := range []struct {
		name  string
		entry func(Entry, Entry) Entry // from film and src
		old   bool                     // a changed file is stored under the name first
		want  string
	}{
		{"a download that does not match", func(film, _ Entry) Entry { film.Download[0] ^= 1; return film }, false, "downloaded SHA-256"},
		{"a stored file that does not match", func(_, src Entry) Entry { src.Stored[0] ^= 1; return src }, false, "stored SHA-256"},
		{"a changed file whose refetch does not match", func(film, _ Entry) Entry { film.Download[0] ^= 1; return film }, true, "downloaded SHA-256"},
		{"a truncated download", func(film, _ Entry) Entry { film.URL = f.srv.URL + "/truncated/film.mp4"; return film }, false, "unexpected EOF"},
		{"a truncated gzip download", func(_, src Entry) Entry { src.URL = f.srv.URL + "/truncated/src.tar.gz"; return src }, false, "unexpected EOF"},
		{"a missing file", func(film, _ Entry) Entry { film.URL = f.srv.URL + "/gone.mp4"; return film }, false, "404"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bad := tc.entry(f.film, f.src)
			good := f.film
			good.Kind, good.Name = "vm", "image.raw"

			if tc.old {
				if err := os.MkdirAll(filepath.Join(dir, bad.Kind), 0o755); err != nil {
					t.Fatal(err)
				}

				if err := os.WriteFile(filepath.Join(dir, bad.Kind, bad.Name), []byte("changed"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			err := Fetch(t.Context(), http.DefaultClient, dir, []Entry{bad, good})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("fetch error %v, want one containing %q", err, tc.want)
			}

			left, err := os.ReadDir(filepath.Join(dir, bad.Kind))
			if err != nil {
				t.Fatal(err)
			}

			if len(left) != 0 {
				t.Errorf("files left under %s after the refusal: %v, want none", bad.Kind, left)
			}

			if got := readStored(t, dir, good); !bytes.Equal(got, f.raw) {
				t.Errorf("the entry beside the refused one: %d bytes, want the %d served", len(got), len(f.raw))
			}
		})
	}
}

// Fetch stops at a cancelled context and stores nothing.
func TestFetchCancelled(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := Fetch(ctx, http.DefaultClient, dir, []Entry{f.film}); !errors.Is(err, context.Canceled) {
		t.Fatalf("fetch under a cancelled context: %v, want context.Canceled", err)
	}

	if _, err := os.Stat(filepath.Join(dir, f.film.Kind, f.film.Name)); !os.IsNotExist(err) {
		t.Errorf("stat of the file after a cancelled fetch: %v, want it absent", err)
	}
}

// skipRecorder keeps what Skip was given and passes the skip on to the real
// test.
type skipRecorder struct {
	testing.TB
	skipped string
}

func (r *skipRecorder) Skip(args ...any) {
	r.Helper()
	r.skipped = fmt.Sprint(args...)
	r.TB.Skip(args...)
}

// Dir skips, naming the variable, when it is unset, and returns it when set.
// The name is spelled out: other readers of the layout find the corpus by it.
func TestDir(t *testing.T) {
	t.Setenv("DITTOFS_CORPUS_DIR", "") // restores the variable at cleanup

	if err := os.Unsetenv("DITTOFS_CORPUS_DIR"); err != nil {
		t.Fatal(err)
	}

	rec := &skipRecorder{}
	reached := false

	t.Run("unset", func(t *testing.T) {
		rec.TB = t
		Dir(rec)
		reached = true
	})

	if reached || !strings.Contains(rec.skipped, "DITTOFS_CORPUS_DIR") {
		t.Errorf("Dir with DITTOFS_CORPUS_DIR unset: went on %v, skipped with %q; want a skip naming the variable", reached, rec.skipped)
	}

	want := t.TempDir()
	t.Setenv("DITTOFS_CORPUS_DIR", want)

	if got := Dir(t); got != want {
		t.Errorf("Dir with DITTOFS_CORPUS_DIR set: %q, want %q", got, want)
	}
}

// The pinned corpus is one entry of each kind, source, vm and media; only the
// VM image, with its zero runs, is repetitive; and a .gz is stored
// decompressed, so its two hashes differ.
func TestEntries(t *testing.T) {
	kinds := map[string]bool{}

	for _, e := range Entries {
		kinds[e.Kind] = true

		if e.Repetitive != (e.Kind == "vm") {
			t.Errorf("entry %s/%s: repetitive %v, want %v", e.Kind, e.Name, e.Repetitive, e.Kind == "vm")
		}

		if gz := strings.HasSuffix(e.URL, ".gz"); gz == (e.Download == e.Stored) {
			t.Errorf("entry %s/%s from %s: download and stored hashes equal %v, want %v", e.Kind, e.Name, e.URL, e.Download == e.Stored, !gz)
		}
	}

	if len(Entries) != 3 || !kinds["source"] || !kinds["vm"] || !kinds["media"] {
		t.Errorf("pinned kinds %v over %d entries, want one each of source, vm and media", kinds, len(Entries))
	}
}

// An entry whose kind or name is not one path element is refused before
// anything under dir is touched.
func TestFetchRefusesAPathName(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")

	if err := os.WriteFile(victim, []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, e := range []Entry{{Kind: "media", Name: "../victim"}, {Kind: "..", Name: "victim"}, {Kind: "media/x", Name: "film"}, {Kind: "media", Name: ""}} {
		if err := Fetch(t.Context(), http.DefaultClient, dir, []Entry{e}); err == nil || !strings.Contains(err.Error(), "one path element") {
			t.Errorf("fetch of kind %q name %q: %v, want the path refusal", e.Kind, e.Name, err)
		}
	}

	if b, err := os.ReadFile(victim); err != nil || string(b) != "kept" {
		t.Errorf("the file outside the kind directory after the refusals: %q, %v; want it kept", b, err)
	}
}
