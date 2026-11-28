package s3

import (
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/cubbit/dittofs/pkg/remote"
)

// TestReadPage reads ListObjectsV2 replies, as the service could send them,
// into the page, the cursor and done.
func TestReadPage(t *testing.T) {
	// Non-ASCII and percent-encoded bytes in the prefix: keys must match it
	// byte for byte, never after a decoding the request never asked for.
	s := &store{prefix: "ns-ü%2F/"}
	blocks := s.prefix + "blocks/"

	a, b := remote.Name{0x0a}, remote.Name{0x0b}
	ka, kb := s.blockKey(a), s.blockKey(b)
	zz := blocks + "zz"
	stamp := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)

	obj := func(key string, size int64) types.Object {
		return types.Object{Key: new(key), Size: new(size), LastModified: new(stamp)}
	}

	tests := []struct {
		name      string
		after     remote.Cursor
		n         int
		contents  []types.Object
		truncated bool

		want    []remote.Name
		next    remote.Cursor
		done    bool
		foreign []string
		err     error
	}{
		{name: "empty and truncated cannot advance", after: remote.Cursor(ka), n: 10, truncated: true, next: remote.Cursor(ka), err: remote.ErrTransient},
		{name: "empty and not truncated is done at after", after: remote.Cursor(ka), n: 10, next: remote.Cursor(ka), done: true},
		{name: "empty store from the start", n: 10, done: true},
		{name: "ending on blocks/zz, not truncated", n: 10, contents: []types.Object{obj(ka, 1), obj(kb, 2), obj(zz, 3)}, want: []remote.Name{a, b}, next: remote.Cursor(zz), done: true, foreign: []string{zz}},
		{name: "ending on blocks/zz, truncated", n: 10, contents: []types.Object{obj(ka, 1), obj(zz, 3)}, truncated: true, want: []remote.Name{a}, next: remote.Cursor(zz), foreign: []string{zz}},
		{name: "only foreign keys still advance", n: 10, contents: []types.Object{obj(blocks+"0", 1), obj(blocks+"1", 1)}, truncated: true, next: remote.Cursor(blocks + "1"), foreign: []string{blocks + "0", blocks + "1"}},
		{name: "an upper-case name is foreign", n: 10, contents: []types.Object{obj(blocks+strings.ToUpper(hex.EncodeToString(a[:])), 1)}, next: remote.Cursor(blocks + strings.ToUpper(hex.EncodeToString(a[:]))), done: true, foreign: []string{blocks + strings.ToUpper(hex.EncodeToString(a[:]))}},
		{name: "more keys than n are read as the first n, truncated", n: 1, contents: []types.Object{obj(ka, 1), obj(kb, 2)}, want: []remote.Name{a}, next: remote.Cursor(ka)},
		{name: "a key not past after", after: remote.Cursor(kb), n: 10, contents: []types.Object{obj(ka, 1)}, next: remote.Cursor(kb), err: remote.ErrTransient},
		{name: "keys out of order", n: 10, contents: []types.Object{obj(kb, 1), obj(ka, 1)}, err: remote.ErrTransient},
		{name: "a key decoded out of the prefix", n: 10, contents: []types.Object{obj("ns-ü//blocks/"+hex.EncodeToString(a[:]), 1)}, err: remote.ErrTransient},
		{name: "a key under another prefix", n: 10, contents: []types.Object{obj("other/blocks/"+hex.EncodeToString(a[:]), 1)}, err: remote.ErrTransient},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page, next, done, foreign, err := s.readPage(t.Context(), tt.after, tt.n, tt.contents, tt.truncated)

			if !errors.Is(err, tt.err) || (tt.err == nil) != (err == nil) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
			if next != tt.next {
				t.Errorf("next = %q, want %q", next, tt.next)
			}
			if done != tt.done {
				t.Errorf("done = %v, want %v", done, tt.done)
			}
			if !slices.Equal(foreign, tt.foreign) {
				t.Errorf("foreign = %q, want %q", foreign, tt.foreign)
			}

			var names []remote.Name
			for _, i := range page {
				names = append(names, i.Name)
				if i.Size == 0 || !i.ModTime.Equal(stamp) {
					t.Errorf("block %x: size %d, time %v, want the reply's size and %v", i.Name[:1], i.Size, i.ModTime, stamp)
				}
			}
			if !slices.Equal(names, tt.want) {
				t.Errorf("page names = %x, want %x", names, tt.want)
			}
		})
	}
}

func TestReadPageChecksTheContextBeforeEachKey(t *testing.T) {
	s := &store{prefix: "ns/"}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, next, _, _, err := s.readPage(ctx, "", 10, []types.Object{{Key: new(s.blockKey(remote.Name{1}))}}, false)
	if !errors.Is(err, context.Canceled) || next != "" {
		t.Errorf("readPage with a cancelled context = %v, next %q; want context.Canceled and next the cursor it was given", err, next)
	}
}

// TestListRefusesBeforeAnyRequest: a cursor outside the prefix's blocks/, an
// n below 1 and a closed store send nothing.
func TestListRefusesBeforeAnyRequest(t *testing.T) {
	srv, n := counting(t, md5ETag)
	s, rec := storeOn(t, srv, choice{}, nil)

	for _, tt := range []struct {
		name  string
		after remote.Cursor
		n     int
	}{
		{"cursor of another prefix", "other/blocks/00", 10},
		{"cursor of the prefix's control/", "ns/control/claim", 10},
		{"cursor of the prefix alone", "ns/", 10},
		{"n of zero", "", 0},
		{"negative n", "", -1},
	} {
		page, next, done, err := s.List(t.Context(), tt.after, tt.n)
		if !errors.Is(err, remote.ErrInvalid) || page != nil || next != tt.after || done {
			t.Errorf("%s: List = %v, %q, %v, %v; want ErrInvalid at the cursor it was given", tt.name, page, next, done, err)
		}
	}

	_ = s.Close()
	if _, _, _, err := s.List(t.Context(), "", 10); !errors.Is(err, remote.ErrInvalid) {
		t.Errorf("List after Close = %v, want ErrInvalid", err)
	}

	if got := n.Load(); got != 0 {
		t.Errorf("%d requests reached the service, want none", got)
	}
	if got := rec.all(); len(got) != 6 || got[0] != (recordedCall{"list", "invalid", "", 0}) {
		t.Errorf("recorded %+v, want six lists, invalid", got)
	}
}
