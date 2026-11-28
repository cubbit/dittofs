//go:build integration

package s3

import (
	"bytes"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/cubbit/dittofs/pkg/remote"
	"github.com/cubbit/dittofs/pkg/remote/s3/s3fault"
)

// deleting is crc on a service with a batch delete.
var deleting = choice{proved: crc.proved, sum: crc.sum, batch: true}

// putAll stores a small body under each name's key through the admin client,
// sixteen at a time.
func (r *lsRig) putAll(t *testing.T, s *store, ns []remote.Name) {
	t.Helper()

	var wg sync.WaitGroup
	work := make(chan remote.Name)
	for range 16 {
		wg.Go(func() {
			for n := range work {
				k := s.blockKey(n)
				if _, err := r.admin.PutObject(t.Context(), &s3.PutObjectInput{Bucket: &r.bucket, Key: &k, Body: bytes.NewReader(n[:4])}); err != nil {
					t.Errorf("PutObject %s: %v", k, err)
				}
			}
		})
	}
	for _, n := range ns {
		work <- n
	}
	close(work)
	wg.Wait()
}

// keysUnder lists every key the service holds under prefix.
func (r *lsRig) keysUnder(t *testing.T, prefix string) []string {
	t.Helper()

	var keys []string
	p := s3.NewListObjectsV2Paginator(r.admin, &s3.ListObjectsV2Input{Bucket: &r.bucket, Prefix: &prefix})
	for p.HasMorePages() {
		page, err := p.NextPage(t.Context())
		if err != nil {
			t.Fatalf("ListObjectsV2 %s: %v", prefix, err)
		}
		for _, o := range page.Contents {
			keys = append(keys, *o.Key)
		}
	}

	return keys
}

// requestsOf are the requests of op the transport saw.
func requestsOf(tr *s3fault.Transport, op string) []s3fault.Request {
	return slices.DeleteFunc(tr.Requests(), func(q s3fault.Request) bool { return q.Op != op })
}

// A batch with a never-written name deletes, every result nil, and so does
// the same batch again; each DeleteObjects LocalStack accepted carried
// Content-MD5 and no other checksum header.
func TestDeleteTwiceWithANeverWrittenName(t *testing.T) {
	r := newLSRig(t)
	s, tr, _, _ := r.open(t, "", deleting)
	ns := names(3)
	r.putAll(t, s, ns[:2])

	for round := range 2 {
		for i, err := range s.Delete(t.Context(), ns) {
			if err != nil {
				t.Errorf("Delete round %d name %d: %v, want nil", round, i, err)
			}
		}
	}

	if keys := r.keysUnder(t, s.prefix); len(keys) != 0 {
		t.Errorf("the service still holds %v, want nothing", keys)
	}

	got := requestsOf(tr, "DeleteObjects")
	if len(got) != 2 {
		t.Fatalf("the transport saw %d DeleteObjects, want 2", len(got))
	}
	for i, q := range got {
		if h := integrityHeaders(q.Header); !slices.Equal(h, []string{"Content-Md5"}) {
			t.Errorf("DeleteObjects %d carried %v, want Content-Md5 alone", i, h)
		}
	}
}

// A batch of 2,500 goes in three requests and every block is gone.
func TestDeleteABatchOf2500(t *testing.T) {
	r := newLSRig(t)
	s, tr, _, _ := r.open(t, "", deleting)
	ns := names(2500)
	r.putAll(t, s, ns)

	for i, err := range s.Delete(t.Context(), ns) {
		if err != nil {
			t.Errorf("Delete name %d: %v, want nil", i, err)
		}
	}

	if n := len(requestsOf(tr, "DeleteObjects")); n != 3 {
		t.Errorf("the transport saw %d DeleteObjects, want 3", n)
	}
	if keys := r.keysUnder(t, s.prefix); len(keys) != 0 {
		t.Errorf("the service still holds %d keys, want none", len(keys))
	}
}

// One key refused inside a batch: exactly that name fails, with the
// refusal's class, and stays stored; the others are gone.
func TestDeleteOneKeyFailedInsideABatch(t *testing.T) {
	r := newLSRig(t)
	s, tr, _, _ := r.open(t, "", deleting)
	ns := names(3)
	r.putAll(t, s, ns)
	refused := s.blockKey(ns[1])
	tr.Inject(s3fault.Rule{Op: "DeleteObjects"}, s3fault.FailBatchKey(refused, "AccessDenied"))

	for i, err := range s.Delete(t.Context(), ns) {
		switch {
		case i == 1 && !errors.Is(err, remote.ErrDenied):
			t.Errorf("Delete of the refused name: %v, want ErrDenied", err)
		case i != 1 && err != nil:
			t.Errorf("Delete name %d: %v, want nil", i, err)
		}
	}

	if keys := r.keysUnder(t, s.prefix); !slices.Equal(keys, []string{refused}) {
		t.Errorf("the service holds %v, want the refused key alone", keys)
	}
}

// A batch whose reply is lost: the service applied its one request, and
// every result is ErrTransient.
func TestDeleteABatchWhoseReplyIsLost(t *testing.T) {
	r := newLSRig(t)
	s, tr, _, _ := r.open(t, "", deleting)
	ns := names(3)
	r.putAll(t, s, ns)
	tr.Inject(s3fault.Rule{Op: "DeleteObjects"}, s3fault.LoseReply())

	for i, err := range s.Delete(t.Context(), ns) {
		if !errors.Is(err, remote.ErrTransient) {
			t.Errorf("Delete name %d, reply lost: %v, want ErrTransient", i, err)
		}
	}

	if n := len(requestsOf(tr, "DeleteObjects")); n != 1 {
		t.Errorf("the transport saw %d DeleteObjects, want 1", n)
	}
	if keys := r.keysUnder(t, s.prefix); len(keys) != 0 {
		t.Errorf("the service still holds %v, want nothing: the request was not applied", keys)
	}
}

// A refusal of the whole request fails every name of it with that refusal;
// a 404 NoSuchKey to the request states nothing of any key.
func TestDeleteAppliesABatchRefusalToEveryName(t *testing.T) {
	r := newLSRig(t)

	for _, tc := range []struct {
		status int
		code   string
		want   error
	}{
		{403, "AccessDenied", remote.ErrDenied},
		{503, "SlowDown", remote.ErrThrottled},
		{404, "NoSuchKey", remote.ErrTransient},
	} {
		s, tr, _, _ := r.open(t, "", deleting)
		tr.Inject(s3fault.Rule{Op: "DeleteObjects"}, s3fault.Answer(tc.status, tc.code, nil))

		for i, err := range s.Delete(t.Context(), names(3)) {
			if !errors.Is(err, tc.want) {
				t.Errorf("Delete name %d, batch answered %d %s: %v, want %v", i, tc.status, tc.code, err, tc.want)
			}
		}
	}
}

// On a service without batch delete, one DeleteObject per name, in order,
// a never-written name among them answered nil, as is one the service
// answers 404 NoSuchKey.
func TestDeleteOneKeyPerRequest(t *testing.T) {
	r := newLSRig(t)
	s, tr, _, _ := r.open(t, "", crc)
	ns := names(4)
	r.putAll(t, s, []remote.Name{ns[0], ns[2]})
	tr.Inject(s3fault.Rule{Op: "DeleteObject", KeyPrefix: s.blockKey(ns[3])}, s3fault.Answer(404, "NoSuchKey", nil))

	for i, err := range s.Delete(t.Context(), ns) {
		if err != nil {
			t.Errorf("Delete name %d: %v, want nil", i, err)
		}
	}

	var keys []string
	for _, q := range tr.Requests() {
		if q.Op != "DeleteObject" {
			t.Errorf("the transport saw a %s, want DeleteObject alone", q.Op)
		}
		keys = append(keys, q.Key)
	}
	if want := []string{s.blockKey(ns[0]), s.blockKey(ns[1]), s.blockKey(ns[2]), s.blockKey(ns[3])}; !slices.Equal(keys, want) {
		t.Errorf("the requests named %v, want %v in order", keys, want)
	}
	if left := r.keysUnder(t, s.prefix); len(left) != 0 {
		t.Errorf("the service still holds %v, want nothing", left)
	}
}
