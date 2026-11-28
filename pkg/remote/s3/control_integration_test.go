//go:build integration

package s3

import (
	"bytes"
	"errors"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/cubbit/dittofs/pkg/remote"
	"github.com/cubbit/dittofs/pkg/remote/s3/s3fault"
)

var roles = []remote.Role{remote.RoleClaim, remote.RoleHealth, remote.RoleKeys}

// rawPut stores b at key beneath the store, as the service would hold it.
func (r *lsRig) rawPut(t *testing.T, key string, b []byte) {
	t.Helper()

	if _, err := r.admin.PutObject(t.Context(), &s3.PutObjectInput{Bucket: &r.bucket, Key: &key, Body: bytes.NewReader(b)}); err != nil {
		t.Fatalf("PutObject %s: %v", key, err)
	}
}

// rawKeys lists every key LocalStack holds under prefix.
func (r *lsRig) rawKeys(t *testing.T, prefix string) []string {
	t.Helper()

	out, err := r.admin.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: &r.bucket, Prefix: &prefix})
	if err != nil {
		t.Fatalf("ListObjectsV2 %s: %v", prefix, err)
	}

	var keys []string
	for _, o := range out.Contents {
		keys = append(keys, *o.Key)
	}

	return keys
}

func TestControlRoundTripsEachRole(t *testing.T) {
	r := newGetRig(t)

	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			s, tr, rec, _ := r.open(t, "", m.ch)

			for _, role := range roles {
				if _, _, err := s.GetControl(t.Context(), role); !errors.Is(err, remote.ErrNotFound) {
					t.Errorf("GetControl of an absent %v = %v, want ErrNotFound", role, err)
				}

				first, second := randomBytes(1000), randomBytes(700)
				for _, b := range [][]byte{first, second} {
					if err := s.PutControl(t.Context(), role, b); err != nil {
						t.Fatalf("PutControl %v of %d bytes: %v", role, len(b), err)
					}
				}

				got, v, err := s.GetControl(t.Context(), role)
				if err != nil || !bytes.Equal(got, second) || v != "" {
					t.Errorf("GetControl %v = %d bytes, version %q, %v; want the second put's %d bytes, no version", role, len(got), v, err, len(second))
				}

				if raw, ok := r.stored(t, s.prefix+"control/"+role.String()); !ok || !bytes.Equal(raw, second) {
					t.Errorf("LocalStack holds %d bytes at %scontrol/%v, want the second put's", len(raw), s.prefix, role)
				}
			}

			each := []recordedCall{
				{"get_control", "not_found", "", 0},
				{"put_control", "ok", "", 1000},
				{"put_control", "ok", "", 700},
				{"get_control", "ok", "", 700},
			}
			want := slices.Concat(each, each, each)
			if got := rec.all(); !slices.Equal(got, want) {
				t.Errorf("recorded %+v, want %+v for each role", got, each)
			}

			for _, req := range tr.Requests() {
				if c := req.Header.Get("X-Amz-Storage-Class"); req.Op == "PutObject" && c != "STANDARD" {
					t.Errorf("a control put of %s sent the storage class %q, want STANDARD", req.Key, c)
				}
			}
		})
	}
}

// A byte flipped in a control put's body is caught in transit in each mode;
// one flipped in a control get's reply fails the read.
func TestControlOfAFlippedByte(t *testing.T) {
	r := newGetRig(t)

	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			s, tr, _, l := r.open(t, "", m.ch)

			for _, role := range []remote.Role{remote.RoleClaim, remote.RoleKeys} {
				b := randomBytes(4000)

				tr.Inject(s3fault.Rule{Op: "PutObject", Times: 1}, s3fault.FlipByte(5))
				if err := s.PutControl(t.Context(), role, b); !errors.Is(err, remote.ErrCorrupt) {
					t.Fatalf("the flipped PutControl %v = %v, want ErrCorrupt", role, err)
				}

				var warns []map[string]any
				for _, rec := range l.records(t) {
					if rec["level"] == "WARN" && rec["role"] == role.String() {
						warns = append(warns, rec)
					}
				}
				if len(warns) != 1 {
					t.Errorf("logged %d Warns naming the role %v, want 1", len(warns), role)
				}

				if err := s.PutControl(t.Context(), role, b); err != nil {
					t.Fatalf("PutControl %v without the fault: %v", role, err)
				}
				if raw, _ := r.stored(t, s.prefix+"control/"+role.String()); !bytes.Equal(raw, b) {
					t.Errorf("LocalStack holds %d bytes for %v after the put without the fault, want its %d", len(raw), role, len(b))
				}

				if m.ch.sum == remote.ChecksumMD5 {
					// The service returns no checksum to an object put with
					// Content-MD5, and this mode compares no ETag on a get, so
					// nothing checks the reply in transit.
					continue
				}

				tr.Inject(s3fault.Rule{Op: "GetObject", Times: 1}, s3fault.FlipByte(5))
				if m.ch.sum == remote.ChecksumNone {
					tr.Inject(s3fault.Rule{Op: "GetObject", Times: 1}, s3fault.StripHeaders("X-Amz-Checksum-Mode"))
				}

				if got, _, err := s.GetControl(t.Context(), role); !errors.Is(err, remote.ErrTransient) || got != nil {
					t.Errorf("GetControl %v through a flipped byte = %d bytes, %v; want nothing, ErrTransient", role, len(got), err)
				}

				if got, _, err := s.GetControl(t.Context(), role); err != nil || !bytes.Equal(got, b) {
					t.Errorf("GetControl %v without the fault = %d bytes, %v; want the stored bytes", role, len(got), err)
				}
			}
		})
	}
}

// The cap from both sides: a body over it is never sent; an object stored
// beneath the store longer than it is refused, one of exactly it read.
func TestControlCap(t *testing.T) {
	r := newGetRig(t)
	s, tr, rec, _ := r.open(t, "", crc)

	if err := s.PutControl(t.Context(), remote.RoleKeys, make([]byte, maxControl+1)); !errors.Is(err, remote.ErrInvalid) {
		t.Errorf("PutControl of the cap + 1 = %v, want ErrInvalid", err)
	}
	if reqs := tr.Requests(); len(reqs) != 0 {
		t.Errorf("PutControl of the cap + 1 sent %+v, want nothing", reqs)
	}

	at := randomBytes(maxControl)
	if err := s.PutControl(t.Context(), remote.RoleKeys, at); err != nil {
		t.Fatalf("PutControl of the cap: %v", err)
	}
	if got, _, err := s.GetControl(t.Context(), remote.RoleKeys); err != nil || !bytes.Equal(got, at) {
		t.Errorf("GetControl of the cap = %d bytes, %v; want every byte", len(got), err)
	}

	r.rawPut(t, s.prefix+"control/keys", append(at, 0))
	if got, _, err := s.GetControl(t.Context(), remote.RoleKeys); !errors.Is(err, remote.ErrInvalid) || got != nil {
		t.Errorf("GetControl of the cap + 1 = %d bytes, %v; want nothing, ErrInvalid", len(got), err)
	}
	if got := rec.all(); len(got) == 0 || got[len(got)-1] != (recordedCall{"get_control", "invalid", "", 0}) {
		t.Errorf("GetControl of the cap + 1 recorded %+v last, want a get_control, invalid, of 0 bytes", got)
	}
}

// Control objects beside blocks sit outside blocks/, so no block listing
// holds one, and the store writes nothing else under its prefix.
func TestControlKeysAreNoBlocks(t *testing.T) {
	r := newGetRig(t)
	s, _, _, _ := r.open(t, "", crc)

	var blocks []string
	for i := range 10 {
		n := remote.Name{byte(i + 1)}
		put(t, s, n, []byte("a block"), crc32cOf)
		blocks = append(blocks, s.blockKey(n))
	}

	for _, role := range roles {
		if err := s.PutControl(t.Context(), role, []byte(role.String())); err != nil {
			t.Fatalf("PutControl %v: %v", role, err)
		}
	}

	slices.Sort(blocks)
	if got := r.rawKeys(t, s.prefix+"blocks/"); !slices.Equal(got, blocks) {
		t.Errorf("the block namespace holds %v, want only the 10 blocks %v", got, blocks)
	}

	want := slices.Clone(blocks)
	for _, role := range roles {
		want = append(want, s.prefix+"control/"+role.String())
	}
	slices.Sort(want)

	if got := r.rawKeys(t, s.prefix); !slices.Equal(got, want) {
		t.Errorf("the prefix holds %v, want the blocks and the control objects %v", got, want)
	}
}
