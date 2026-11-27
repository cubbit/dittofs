// Package corpus is the carver's pinned corpus of real files: a source tree,
// a VM image and a film, fetched once into one directory and pinned by their
// hashes, so a run over them reads the bytes the last run read. Only tests and
// its fetch command import it; no per-change check needs it, and one that does
// skips when the directory is not given.
//
// The layout is the contract other readers rely on: each file sits at
// $DITTOFS_CORPUS_DIR/<kind>/<name>, the kinds source, vm and media.
package corpus

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// envDir names the environment variable holding the corpus directory.
const envDir = "DITTOFS_CORPUS_DIR"

// Dir returns DITTOFS_CORPUS_DIR, skipping tb, naming the variable, when it is
// unset or empty. A file missing from a set directory fails the test that
// opens it.
func Dir(tb testing.TB) string {
	tb.Helper()

	dir := os.Getenv(envDir)
	if dir == "" {
		tb.Skip(envDir + " is not set: fill a directory with " +
			"go run ./pkg/carver/internal/carvetest/corpus/fetch <dir> and set " + envDir + " to it")
	}

	return dir
}

// Entry is one pinned corpus file.
type Entry struct {
	Kind, Name, URL  string
	Download, Stored [32]byte // SHA-256 of what is downloaded and of the file stored
	Repetitive       bool     // has repetitive regions, such as zero runs
}

// Entries is the pinned corpus. An entry whose URL ends in .gz is stored
// decompressed.
var Entries = []Entry{
	{
		// The Linux 6.6 source tarball, about 1.4 GB once decompressed, which a
		// 1 GiB file of source needs.
		Kind: "source", Name: "linux-6.6.tar",
		URL:      "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.6.tar.gz",
		Download: sum("3c88ff4648122e81832b7067be6456ef28e119b64649a29e84f7f04691e489f9"),
		Stored:   sum("f7e99bfe7afbfca53b1beab2bcde4614d1d98b2a6bcc46ef7eec8ff482eb7b4e"),
	},
	{
		// A dated Debian 12 generic cloud raw image: zero runs, filesystem
		// metadata, binaries and text.
		Kind: "vm", Name: "debian-12-genericcloud-amd64.raw",
		URL:        "https://cloud.debian.org/images/cloud/bookworm/20261117-2665/debian-12-genericcloud-amd64-20261117-2665.raw",
		Download:   sum("3a46979f06ce16670ed7156e3ee1ee7507b145b487d97c57ca74227da68f64aa"),
		Stored:     sum("3a46979f06ce16670ed7156e3ee1ee7507b145b487d97c57ca74227da68f64aa"),
		Repetitive: true,
	},
	{
		// The Blender Foundation's open film Big Buck Bunny, 1080p at 30 fps.
		Kind: "media", Name: "big-buck-bunny-1080p.mp4",
		URL:      "https://archive.org/download/bbb_sunflower_1080p_30fps_normal/bbb_sunflower_1080p_30fps_normal.mp4",
		Download: sum("ae51005850b0ff757fe60c3dd7a12d754d3cd2397d87d939b55235e457f97658"),
		Stored:   sum("ae51005850b0ff757fe60c3dd7a12d754d3cd2397d87d939b55235e457f97658"),
	},
}

// sum decodes a pinned SHA-256 written in hex.
func sum(h string) [32]byte {
	var s [32]byte
	if n, err := hex.Decode(s[:], []byte(h)); err != nil || n != len(s) {
		panic(fmt.Sprintf("corpus: pinned hash %q is not a SHA-256", h))
	}

	return s
}

// Fetch stores each entry at <dir>/<kind>/<name>, downloading it unless a file
// there already has its stored hash, and refuses one whose hashes do not
// match. A stored file that differs is removed before it is fetched again, so
// a refused entry leaves no file under its name; the other entries are still
// fetched, and every refusal is returned.
func Fetch(ctx context.Context, client *http.Client, dir string, entries []Entry) error {
	var errs []error

	for _, e := range entries {
		if err := fetch(ctx, client, dir, e); err != nil {
			errs = append(errs, fmt.Errorf("corpus %s/%s: %w", e.Kind, e.Name, err))
		}
	}

	return errors.Join(errs...)
}

func fetch(ctx context.Context, client *http.Client, dir string, e Entry) error {
	if !plainName(e.Kind) || !plainName(e.Name) {
		return errors.New("kind and name must each be one path element")
	}

	kindDir := filepath.Join(dir, e.Kind)
	path := filepath.Join(kindDir, e.Name)

	switch got, err := hashFile(path); {
	case err == nil && got == e.Stored:
		return nil
	case err == nil:
		if err := os.Remove(path); err != nil {
			return err
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}

	if err := os.MkdirAll(kindDir, 0o755); err != nil {
		return err
	}

	// One temporary name per entry, so a fetch killed part-way leaves at most
	// one partial file, which the next fetch truncates.
	tmp, err := os.OpenFile(filepath.Join(kindDir, "."+e.Name+".part"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}

	defer func() { _ = os.Remove(tmp.Name()) }() // a no-op once renamed

	if err := download(ctx, client, e, tmp); err != nil {
		_ = tmp.Close()
		return err
	}

	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmp.Name(), path)
}

// download writes e's file to f and checks both of its hashes.
//
// ponytail: does not resume a partial download; a fetch that fails part-way
// starts the file over. Resume with a range request if a fetch on the
// reference box fails part-way more than once.
func download(ctx context.Context, client *http.Client, e Entry, f *os.File) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.URL, nil)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", e.URL, resp.Status)
	}

	downloaded, stored := sha256.New(), sha256.New()
	body := io.TeeReader(resp.Body, downloaded)

	src := body
	if strings.HasSuffix(e.URL, ".gz") {
		zr, err := gzip.NewReader(body)
		if err != nil {
			return err
		}

		src = zr
	}

	if _, err := io.Copy(io.MultiWriter(f, stored), src); err != nil {
		return err
	}

	if got := [32]byte(downloaded.Sum(nil)); got != e.Download {
		return fmt.Errorf("downloaded SHA-256 %x, want %x", got, e.Download)
	}

	if got := [32]byte(stored.Sum(nil)); got != e.Stored {
		return fmt.Errorf("stored SHA-256 %x, want %x", got, e.Stored)
	}

	return f.Sync()
}

// hashFile returns the SHA-256 of the file at path.
func hashFile(path string) ([32]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return [32]byte{}, err
	}

	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return [32]byte{}, err
	}

	return [32]byte(h.Sum(nil)), nil
}

// plainName reports whether s is one path element inside its directory.
func plainName(s string) bool {
	return filepath.IsLocal(s) && filepath.Base(s) == s
}
