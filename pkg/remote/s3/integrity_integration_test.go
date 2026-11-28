//go:build integration

package s3

import (
	"encoding/base64"
	"net/http"
	"runtime"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/remote"
	"github.com/cubbit/dittofs/pkg/remote/s3/s3fault"
)

// largestBlock is the largest encoded block the integrity tests prove at.
const largestBlock = 1 << 20

// checkRig is a client on a fresh LocalStack bucket, every request through a
// recording fault transport, and a check namespace under a fresh prefix.
type checkRig struct {
	*lsRig
	c  *Client
	tr *s3fault.Transport
	ns string
}

func newCheckRig(t *testing.T) *checkRig {
	t.Helper()

	r := newGetRig(t)
	s, tr := r.openFaulted(t, choice{}, nil)

	return &checkRig{lsRig: r, c: s.c, tr: tr, ns: s.prefix + "check/0011223344556677/"}
}

// Compositions of the services RFC 4 Appendix B measured, over LocalStack.
var (
	// Cubbit DS3 ignores every put checksum and If-None-Match.
	cubbitDS3 = []string{"Content-Md5", "If-None-Match", "X-Amz-Sdk-Checksum-Algorithm", "X-Amz-Checksum-Crc32",
		"X-Amz-Checksum-Crc32c", "X-Amz-Checksum-Crc64nvme", "X-Amz-Checksum-Sha1", "X-Amz-Checksum-Sha256"}
)

func cubbit(tr *s3fault.Transport) {
	tr.Inject(s3fault.Rule{Op: "PutObject"}, s3fault.StripHeaders(cubbitDS3...))
}

// scaleway enforces only CRC32C, refusing a wrong one 400 InvalidRequest.
func scaleway(tr *s3fault.Transport) {
	tr.Inject(s3fault.Rule{Op: "PutObject"}, s3fault.StripHeaders("Content-Md5"))
	tr.Inject(s3fault.Rule{Op: "PutObject"}, s3fault.RecodeError("BadDigest", http.StatusBadRequest, "InvalidRequest"))
}

// md5Only enforces only Content-MD5: the CRC32C header never arrives.
func md5Only(tr *s3fault.Transport) {
	tr.Inject(s3fault.Rule{Op: "PutObject"}, s3fault.StripHeaders("X-Amz-Checksum-Crc32c", "X-Amz-Sdk-Checksum-Algorithm"))
}

// The choice each profile's check proves, read at proveIntegrity's result,
// what the store's puts consume, and each wrong put read back as the service
// holds it.
func TestProveIntegrityChoosesEachProfilesMechanism(t *testing.T) {
	for _, tc := range []struct {
		name    string
		compose func(*s3fault.Transport)
		// planted before the check, so a refused put finds an object there
		plant string
		want  choice
		// the wrong puts the service must hold nothing at, or must hold
		absent, stored []string
	}{
		{"localstack", func(*s3fault.Transport) {}, "",
			choice{proved: true, sum: remote.ChecksumCRC32C, rejected: recorded{400, "BadDigest"}},
			[]string{"integrity/crc32c-wrong", "integrity/large-wrong"}, nil},
		{"scaleway", scaleway, "",
			choice{proved: true, sum: remote.ChecksumCRC32C, rejected: recorded{400, "InvalidRequest"}},
			[]string{"integrity/crc32c-wrong", "integrity/large-wrong"}, nil},
		{"md5_only", md5Only, "",
			choice{proved: true, sum: remote.ChecksumMD5, rejected: recorded{400, "BadDigest"}},
			[]string{"integrity/md5-wrong", "integrity/large-wrong"}, []string{"integrity/crc32c-wrong"}},
		{"cubbit_ds3", cubbit, "",
			choice{proved: true, sum: remote.ChecksumNone},
			nil, []string{"integrity/crc32c-wrong", "integrity/md5-wrong", "integrity/etag"}},
		// A refusal counts only once nothing is found stored at the key.
		{"refused_but_held", func(*s3fault.Transport) {}, "integrity/crc32c-wrong",
			choice{proved: true, sum: remote.ChecksumMD5, rejected: recorded{400, "BadDigest"}},
			[]string{"integrity/md5-wrong", "integrity/large-wrong"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCheckRig(t)
			tc.compose(r.tr)
			if tc.plant != "" {
				r.rawPut(t, r.ns+tc.plant, []byte("planted"))
			}

			ch, v, err := proveIntegrity(t.Context(), r.c, r.ns, largestBlock)
			if err != nil {
				t.Fatalf("proveIntegrity: %v", err)
			}

			if ch != tc.want {
				t.Errorf("choice %+v, want %+v", ch, tc.want)
			}

			if want := (verdict{featPutIntegrity: statePresent}); v != want {
				t.Errorf("verdict %v, want put_integrity present and nothing else verified", v)
			}

			for _, k := range tc.absent {
				if _, ok := r.stored(t, r.ns+k); ok {
					t.Errorf("the service holds %s, whose put the check counted refused", k)
				}
			}

			for _, k := range tc.stored {
				if _, ok := r.stored(t, r.ns+k); !ok {
					t.Errorf("the service holds nothing at %s, which the composition stores", k)
				}
			}

			key := r.ns + "integrity/large"
			out, err := r.admin.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: &r.bucket, Key: &key})
			if err != nil {
				t.Fatalf("HeadObject of the largest-block proof: %v", err)
			}

			if *out.ContentLength != largestBlock {
				t.Errorf("the largest-block proof stored %d bytes, want %d", *out.ContentLength, largestBlock)
			}
		})
	}
}

// The wrong value a refusal is recorded for was sent, as a header that is not
// the body's digest.
func TestProveIntegritySendsTheWrongValue(t *testing.T) {
	r := newCheckRig(t)

	if _, _, err := proveIntegrity(t.Context(), r.c, r.ns, largestBlock); err != nil {
		t.Fatalf("proveIntegrity: %v", err)
	}

	right := base64.StdEncoding.EncodeToString(crc32cOf(checkBody))
	sent := map[string]string{}
	for _, q := range r.tr.Requests() {
		if q.Op == "PutObject" {
			sent[q.Key] = q.Header.Get("X-Amz-Checksum-Crc32c")
		}
	}

	if got := sent[r.ns+"integrity/crc32c"]; got != right {
		t.Errorf("the correct put sent CRC32C %q, want %q", got, right)
	}

	if got := sent[r.ns+"integrity/crc32c-wrong"]; got == "" || got == right {
		t.Errorf("the wrong put sent CRC32C %q, want a value other than the body's %q", got, right)
	}
}

// The chosen mechanism broken only above half the largest block, which no
// small object reaches, leaves put_integrity missing and nothing proved.
func TestProveIntegrityAtTheLargestBlock(t *testing.T) {
	above := s3fault.Rule{Op: "PutObject", MinBody: largestBlock / 2}

	for _, tc := range []struct {
		name    string
		compose func(*s3fault.Transport)
	}{
		{"crc32c_stripped", func(tr *s3fault.Transport) {
			tr.Inject(above, s3fault.StripHeaders("X-Amz-Checksum-Crc32c", "X-Amz-Sdk-Checksum-Algorithm"))
		}},
		{"md5_stripped", func(tr *s3fault.Transport) {
			md5Only(tr)
			tr.Inject(above, s3fault.StripHeaders("Content-Md5"))
		}},
		// The small refusal recorded as BadDigest, the large one answered
		// under another code, which the store would read as ErrInvalid.
		{"large_refusal_recoded", func(tr *s3fault.Transport) {
			tr.Inject(above, s3fault.RecodeError("BadDigest", http.StatusBadRequest, "InvalidRequest"))
		}},
		{"etag_body_flipped", func(tr *s3fault.Transport) {
			cubbit(tr)
			tr.Inject(above, s3fault.FlipByte(largestBlock-1))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCheckRig(t)
			tc.compose(r.tr)

			ch, v, err := proveIntegrity(t.Context(), r.c, r.ns, largestBlock)
			if err != nil {
				t.Fatalf("proveIntegrity: %v", err)
			}

			if want := (verdict{featPutIntegrity: stateMissing}); v != want {
				t.Errorf("verdict %v, want put_integrity missing", v)
			}

			if ch != (choice{}) {
				t.Errorf("choice %+v, want nothing proved", ch)
			}
		})
	}
}

// TestProveIntegrityAllocatesFlat bounds the bytes the check allocates: at
// 8 MiB no more than a fixed slack over 1 MiB, so the largest block is never
// held.
func TestProveIntegrityAllocatesFlat(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the race detector's allocations make the count meaningless")
	}

	r := newCheckRig(t)

	perCheck := func(size int64) uint64 {
		check := func() {
			if _, v, err := proveIntegrity(t.Context(), r.c, r.ns, size); err != nil || v[featPutIntegrity] != statePresent {
				t.Fatalf("proveIntegrity at %d bytes: verdict %v, %v", size, v, err)
			}
		}
		check() // the connections and the pool in place first

		const checks = 3
		var before, after runtime.MemStats

		runtime.GC()
		runtime.ReadMemStats(&before)
		for range checks {
			check()
		}
		runtime.ReadMemStats(&after)

		return (after.TotalAlloc - before.TotalAlloc) / checks
	}

	small, large := perCheck(1<<20), perCheck(8<<20)

	const slack = 256 << 10
	if large > small+slack {
		t.Errorf("the check allocates %d bytes at 8 MiB and %d at 1 MiB, more than %d apart: the block is held", large, small, slack)
	}

	t.Logf("bytes allocated per check: %d at 1 MiB, %d at 8 MiB", small, large)
}
