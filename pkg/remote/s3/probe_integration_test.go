//go:build integration

package s3

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/remote"
	"github.com/cubbit/dittofs/pkg/remote/s3/s3fault"
	"github.com/cubbit/dittofs/pkg/sim"
)

// The choices step 1 proves on LocalStack as it is and under Cubbit DS3's
// composition (TestProveIntegrityChoosesEachProfilesMechanism).
var (
	lsChoice     = choice{proved: true, sum: remote.ChecksumCRC32C, rejected: recorded{400, "BadDigest"}}
	cubbitChoice = choice{proved: true, sum: remote.ChecksumNone}
)

// probeFeatures are the features steps 2 to 4 judge.
var probeFeatures = []feature{featReadAfterWrite, featStorageClass, featRangeGet, featListBucket, featBatchDelete, featList, featConditionalPut}

// probeVerdict is every feature of steps 2 to 4 present, those in missing
// missing and those in unverified unverified.
func probeVerdict(missing, unverified []feature) verdict {
	var v verdict
	for _, f := range probeFeatures {
		v[f] = statePresent
	}
	for _, f := range missing {
		v[f] = stateMissing
	}
	for _, f := range unverified {
		v[f] = stateUnverified
	}

	return v
}

// checkVerdict fails t naming each feature whose state differs from want.
func checkVerdict(t *testing.T, got, want verdict) {
	t.Helper()

	states := [...]string{stateUnverified: "unverified", statePresent: "present", stateMissing: "missing"}
	for f := range featureCount {
		if got[f] != want[f] {
			t.Errorf("%s is %s, want %s", f, states[got[f]], states[want[f]])
		}
	}
}

// conditional is the requests of the record that carry a conditional header.
func conditional(reqs []s3fault.Request) []s3fault.Request {
	var out []s3fault.Request
	for _, q := range reqs {
		for _, h := range []string{"If-None-Match", "If-Match", "If-Modified-Since", "If-Unmodified-Since"} {
			if q.Header.Get(h) != "" {
				out = append(out, q)
				break
			}
		}
	}

	return out
}

// firstListKey is the key of step 4's first block, the one put again with
// If-None-Match.
func firstListKey(ns string) string {
	n := remote.Name{1}
	return ns + "blocks/" + hex.EncodeToString(n[:])
}

// On LocalStack as it is every step passes, the batch is chosen and sent with
// Content-MD5 and no SDK checksum, and the one conditional request goes to a
// key of the check namespace.
func TestProbePassesOnLocalStack(t *testing.T) {
	r := newCheckRig(t)

	ch, v, err := probe(t.Context(), r.c, r.ns, lsChoice)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}

	checkVerdict(t, v, probeVerdict(nil, nil))

	if want := (choice{proved: true, sum: remote.ChecksumCRC32C, rejected: recorded{400, "BadDigest"}, batch: true}); ch != want {
		t.Errorf("choice %+v, want %+v", ch, want)
	}

	batches := 0
	for _, q := range r.tr.Requests() {
		if q.Op != "DeleteObjects" {
			continue
		}
		batches++

		if q.Header.Get("Content-Md5") == "" {
			t.Errorf("DeleteObjects sent no Content-MD5")
		}
		for h := range q.Header {
			if strings.HasPrefix(h, "X-Amz-Checksum-") || h == "X-Amz-Sdk-Checksum-Algorithm" {
				t.Errorf("DeleteObjects sent %s: %q, want no checksum header but Content-MD5", h, q.Header.Get(h))
			}
		}
	}
	if batches != 1 {
		t.Errorf("%d DeleteObjects requests, want 1", batches)
	}

	if c := conditional(r.tr.Requests()); len(c) != 1 || c[0].Op != "PutObject" || c[0].Key != firstListKey(r.ns) {
		t.Errorf("conditional requests %+v, want one PutObject to %s", c, firstListKey(r.ns))
	}

	for _, k := range []string{"delete/a", "delete/b"} {
		if _, ok := r.stored(t, r.ns+k); ok {
			t.Errorf("the service still holds %s, which the batch reported deleted", k)
		}
	}
}

// Each service answer the check provokes and finds wrong makes its own
// feature missing, and only that one.
func TestProbeNamesEachWrongAnswer(t *testing.T) {
	for _, tc := range []struct {
		name       string
		inject     func(tr *s3fault.Transport, ns string)
		missing    []feature
		unverified []feature
		batch      bool
		// kept is a key the service must still hold after the probe
		kept string
	}{
		// A get at once after the put that misses the object; its class is
		// then unread.
		{"read_lags", func(tr *s3fault.Transport, ns string) {
			tr.Inject(s3fault.Rule{Op: "GetObject", KeyPrefix: ns + "read/object", Times: 1}, s3fault.Answer(http.StatusNotFound, "NoSuchKey", nil))
		}, []feature{featReadAfterWrite}, []feature{featStorageClass}, true, ""},
		// A credential without the list right answers 403 for an absent key.
		{"absent_denied", func(tr *s3fault.Transport, ns string) {
			tr.Inject(s3fault.Rule{Op: "GetObject", KeyPrefix: ns + "read/absent"}, s3fault.Answer(http.StatusForbidden, "AccessDenied", nil))
		}, []feature{featListBucket}, nil, true, ""},
		// A service without the batch operation is recorded, not failed.
		{"batch_rejected", func(tr *s3fault.Transport, _ string) {
			tr.Inject(s3fault.Rule{Op: "DeleteObjects"}, s3fault.Answer(http.StatusNotImplemented, "NotImplemented", nil))
		}, []feature{featBatchDelete}, nil, false, ""},
		// One key refused inside the batch, for real.
		{"batch_key_refused", func(tr *s3fault.Transport, ns string) {
			tr.Inject(s3fault.Rule{Op: "DeleteObjects"}, s3fault.FailBatchKey(ns+"delete/a", "AccessDenied"))
		}, []feature{featBatchDelete}, nil, false, "delete/a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCheckRig(t)
			tc.inject(r.tr, r.ns)

			ch, v, err := probe(t.Context(), r.c, r.ns, lsChoice)
			if err != nil {
				t.Fatalf("probe: %v", err)
			}

			checkVerdict(t, v, probeVerdict(tc.missing, tc.unverified))

			if ch.batch != tc.batch {
				t.Errorf("choice.batch %v, want %v", ch.batch, tc.batch)
			}

			if tc.kept != "" {
				if _, ok := r.stored(t, r.ns+tc.kept); !ok {
					t.Errorf("the service holds nothing at %s, whose delete it refused", tc.kept)
				}
			}
		})
	}
}

// A service that ignores If-None-Match is measured, conditional_put 0, and
// refused for nothing; the one conditional request is still sent.
func TestProbeMeasuresConditionalPut(t *testing.T) {
	r := newCheckRig(t)
	cubbit(r.tr)

	_, v, err := probe(t.Context(), r.c, r.ns, cubbitChoice)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}

	checkVerdict(t, v, probeVerdict([]feature{featConditionalPut}, nil))

	if v.refusals() != "" {
		t.Errorf("refusals %q, want none: conditional put refuses nothing", v.refusals())
	}

	if c := conditional(r.tr.Requests()); len(c) != 1 || c[0].Key != firstListKey(r.ns) {
		t.Errorf("conditional requests %+v, want one to %s", c, firstListKey(r.ns))
	}
}

// A listing that holds new keys out passes once they appear within five
// re-lists, 6 s apart on the clock, and fails when they appear only after.
// The clock is advanced only once the probe waits on it, so no real time
// passes in the wait.
func TestProbeRelistsWithinTheBound(t *testing.T) {
	for _, tc := range []struct {
		held int
		ok   bool
	}{{5, true}, {6, false}} {
		t.Run("held_"+string(rune('0'+tc.held)), func(t *testing.T) {
			r := newCheckRig(t)
			// Under the zero rule: the effect learns its keys from the puts.
			r.tr.Inject(s3fault.Rule{}, s3fault.HoldNewKeys(tc.held))

			clk := sim.NewTestClock(time.Unix(0, 0))
			r.c.clock = clk

			type result struct {
				v   verdict
				err error
			}
			done := make(chan result, 1)
			armed := clk.Armed(relistWait)

			go func() {
				_, v, err := probe(context.Background(), r.c, r.ns, lsChoice)
				done <- result{v, err}
			}()

			waits := 0
			var res result
		wait:
			for {
				select {
				case <-armed:
					waits++
					armed = clk.Armed(relistWait)
					clk.Advance(relistWait)
				case res = <-done:
					break wait
				case <-time.After(2 * time.Minute): // a backstop only; every wait is released on the clock
					t.Fatalf("the probe neither waited nor returned after %d waits", waits)
				}
			}

			if res.err != nil {
				t.Fatalf("probe: %v", res.err)
			}

			var missing []feature
			if !tc.ok {
				missing = []feature{featList}
			}
			checkVerdict(t, res.v, probeVerdict(missing, nil))

			if waits != relists || clk.Elapsed() != 30*time.Second {
				t.Errorf("%d waits over %v, want %d over 30s", waits, clk.Elapsed(), relists)
			}
		})
	}
}

// A context ended during a re-list wait ends the probe at once, with its
// error, and no clock advance.
func TestProbeRelistWaitEndsWithTheContext(t *testing.T) {
	r := newCheckRig(t)
	r.tr.Inject(s3fault.Rule{}, s3fault.HoldNewKeys(6))

	clk := sim.NewTestClock(time.Unix(0, 0))
	r.c.clock = clk
	armed := clk.Armed(relistWait)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() {
		_, _, err := probe(ctx, r.c, r.ns, lsChoice)
		done <- err
	}()

	select {
	case <-armed:
	case err := <-done:
		t.Fatalf("probe returned %v before waiting to re-list", err)
	}
	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("probe = %v, want context.Canceled", err)
	}
}

// A call that fails rather than answers fails the probe with its sentinel and
// nothing judged, a 403 included everywhere but the never-written key.
func TestProbeFailsOnAFailedCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		rule func(ns string) s3fault.Rule
		eff  s3fault.Effect
		want error
	}{
		{"get_denied", func(ns string) s3fault.Rule { return s3fault.Rule{Op: "GetObject", KeyPrefix: ns + "read/object"} },
			s3fault.Answer(http.StatusForbidden, "AccessDenied", nil), remote.ErrDenied},
		{"batch_unavailable", func(string) s3fault.Rule { return s3fault.Rule{Op: "DeleteObjects"} },
			s3fault.Answer(http.StatusServiceUnavailable, "ServiceUnavailable", nil), remote.ErrTransient},
		{"list_throttled", func(string) s3fault.Rule { return s3fault.Rule{Op: "ListObjectsV2"} },
			s3fault.Answer(http.StatusServiceUnavailable, "SlowDown", nil), remote.ErrThrottled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCheckRig(t)
			r.tr.Inject(tc.rule(r.ns), tc.eff)

			ch, v, err := probe(t.Context(), r.c, r.ns, lsChoice)
			if !errors.Is(err, tc.want) {
				t.Errorf("probe = %v, want %v", err, tc.want)
			}

			if ch != (choice{}) || v != (verdict{}) {
				t.Errorf("choice %+v and verdict %v, want both zero", ch, v)
			}
		})
	}
}
