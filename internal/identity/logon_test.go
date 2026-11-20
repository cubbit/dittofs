package identity_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/cubbit/dittofs/internal/identity"
	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// logonRecorder is a LogonSink that keeps the events it was given.
type logonRecorder struct {
	mu     sync.Mutex
	events []identity.LogonEvent
}

func (l *logonRecorder) Logon(_ context.Context, e *identity.LogonEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, *e)
}

// since is the events recorded after the first n.
func (l *logonRecorder) since(n int) []identity.LogonEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]identity.LogonEvent(nil), l.events[n:]...)
}

func (l *logonRecorder) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.events)
}

// signIns is identity_authentications_total for NTLM over SMB with outcome,
// or summed over every series when outcome is empty.
func (r *resolving) signIns(t *testing.T, outcome string) float64 {
	t.Helper()
	if outcome == "" {
		return r.total(t, "identity_authentications_total")
	}
	return r.series(t, "identity_authentications_total", map[string]string{"protocol": "smb", "mechanism": "ntlm", "outcome": outcome})
}

// series is the named counter's value, or histogram's count, in the series
// whose labels are exactly want.
func (r *resolving) series(t *testing.T, name string, want map[string]string) float64 {
	t.Helper()
	mfs, err := r.reg.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != metrics.Namespace+"_"+name {
			continue
		}
		for _, m := range mf.GetMetric() {
			got := map[string]string{}
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				continue
			}
			if h := m.GetHistogram(); h != nil {
				return float64(h.GetSampleCount())
			}
			return m.GetCounter().GetValue()
		}
	}
	return 0
}

// authenticateSpans is the identity.authenticate spans ended after the first
// n spans of any name.
func (r *resolving) authenticateSpans(n int) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range r.spans.Ended()[n:] {
		if s.Name() == "identity.authenticate" {
			out = append(out, s)
		}
	}
	return out
}

// Every sign-in path of the local check and the throttle records exactly one
// logon event, one count under its outcome, one latency and one
// identity.authenticate span under the request's span, whose attributes are
// the proof kind, the outcome and, on success alone, the principal's ID.
// Across them all, no event, logon line, Debug log line, error text or span
// attribute carries the response, the MIC, a session key or an NT hash, and
// no log line carries the name as presented. Catches an attempt not
// recorded or recorded twice, a name or SID among the span's attributes, and
// a credential or name leaked (RFC 18 §3.1, §9.2; RFC 25 §5.4).
func TestSignInRecordsEachAttempt(t *testing.T) {
	r, p := ntlmServer(t, "KIOSK")
	alice := r.ntlmUser(t, p, "alice", "1500", alicePassword)
	carol := entity.User{Principal: entity.NewUser(), Name: "carol", IDs: fileIDs("1502"), Disabled: true}
	r.withNTHash(t, p, &carol, alicePassword)
	r.user(t, carol, 0)
	h, err := pid.NewHashes(entity.User{IDs: fileIDs("1")}, []byte(alicePassword), pid.NTLMSetting{On: true})
	if err != nil {
		t.Fatal(err)
	}
	dummy := r.srv.DummyNTHash()
	secretsSeen := [][]byte{h.NTHash, dummy[:], clientKey}

	right := func(user, domain string) func() pid.SignInStep {
		return func() pid.SignInStep { return ntlmStep(alicePassword, user, domain, false) }
	}
	spoil := func(mic bool, f func(a *pid.NTLMAuthenticate)) func() pid.SignInStep {
		return func() pid.SignInStep {
			s := ntlmStep(alicePassword, "ALICE", "", mic)
			f(s.NTLM)
			return s
		}
	}
	rows := []struct {
		name    string
		prep    func(t *testing.T, client netip.Addr)
		step    func() pid.SignInStep
		outcome string
	}{
		{"the right password, with a MIC", nil, func() pid.SignInStep { return ntlmStep(alicePassword, "ALICE", "", true) }, "ok"},
		{"a wrong password", nil, func() pid.SignInStep { return ntlmStep("wrong", "ALICE", "", false) }, "logon_failure"},
		{"an unknown user", nil, right("NoBody", ""), "logon_failure"},
		{"a disabled user", nil, right("Carol", ""), "disabled"},
		{"another domain", nil, right("ALICE", "OTHER"), "unknown_domain"},
		{"the Kelvin sign for K", nil, right("ALICE", "KIOSK"), "unknown_domain"},
		{"an NTLMv1 response", nil, spoil(false, func(a *pid.NTLMAuthenticate) { a.NTResponse = a.NTResponse[:24] }), "logon_failure"},
		{"a corrupted MIC", nil, spoil(true, func(a *pid.NTLMAuthenticate) { a.Raw[80] ^= 1 }), "logon_failure"},
		{"a malformed response", nil, spoil(false, func(a *pid.NTLMAuthenticate) { a.NTResponse[16] = 2 }), "invalid"},
		{"NTLM off", func(t *testing.T, _ netip.Addr) { r.applyNTLM(t, "KIOSK", false) }, right("ALICE", ""), "logon_failure"},
		{"the user's read failing", func(t *testing.T, _ netip.Addr) { r.fail(t, kvtest.OpGet, "U‖*") }, right("ALICE", ""), "unavailable"},
		{"the eleventh attempt after 10 wrong passwords", func(t *testing.T, client netip.Addr) {
			r.fails(t, 10, func() pid.SignInStep { return from(ntlmStep("wrong", "ALICE", "", false), client) }, vfserr.ErrLogonFailure)
		}, right("ALICE", ""), "throttled"},
	}

	var errTexts, attrs []string
	var names []string
	for i, c := range rows {
		client := netip.AddrFrom4([4]byte{192, 0, 2, byte(20 + i)})
		if c.prep != nil {
			c.prep(t, client)
		}
		step := from(c.step(), client)
		secretsSeen = append(secretsSeen, step.NTLM.NTResponse, step.NTLM.EncryptedSessionKey)
		if len(step.NTLM.Raw) >= 88 {
			secretsSeen = append(secretsSeen, step.NTLM.Raw[72:88])
		}
		names = append(names, step.NTLM.User)

		events, count, every, seconds, spans := r.logons.len(), r.signIns(t, c.outcome), r.signIns(t, ""),
			r.series(t, "identity_authentication_seconds", map[string]string{"mechanism": "ntlm"}), len(r.spans.Ended())
		ctx, parent := r.tp.Tracer("test").Start(context.Background(), "request")
		res, err := r.srv.SignIn(ctx, step)
		parent.End()
		r.fault.SetFault(nil)
		r.applyNTLM(t, "KIOSK", true)
		if err != nil {
			errTexts = append(errTexts, err.Error())
		}
		if res.Keys.Session != nil {
			secretsSeen = append(secretsSeen, res.Keys.Session)
		}

		var wantPrincipal entity.PrincipalID
		if c.outcome == "ok" {
			wantPrincipal = alice.Principal.ID
		}
		if got := r.logons.since(events); len(got) != 1 {
			t.Errorf("%s: logon events = %d, want 1", c.name, len(got))
		} else if e := got[0]; e.Outcome != c.outcome || e.Mechanism != "ntlm" || e.Protocol != "smb" || e.Name != step.NTLM.User ||
			e.Principal != wantPrincipal || e.Client != client || !e.Time.Equal(r.clock.Now()) {
			t.Errorf("%s: logon event %+v, want outcome %q, ntlm over smb, name %q, principal %v, client %v at %v",
				c.name, e, c.outcome, step.NTLM.User, wantPrincipal, client, r.clock.Now())
		}
		if d := r.signIns(t, c.outcome) - count; d != 1 {
			t.Errorf("%s: identity_authentications_total{smb, ntlm, %s} rose by %v, want 1", c.name, c.outcome, d)
		}
		if d := r.signIns(t, "") - every; d != 1 {
			t.Errorf("%s: identity_authentications_total rose by %v over every series, want 1", c.name, d)
		}
		if d := r.series(t, "identity_authentication_seconds", map[string]string{"mechanism": "ntlm"}) - seconds; d != 1 {
			t.Errorf("%s: identity_authentication_seconds{ntlm} count rose by %v, want 1", c.name, d)
		}
		got := r.authenticateSpans(spans)
		if len(got) != 1 {
			t.Errorf("%s: identity.authenticate spans = %d, want 1", c.name, len(got))
			continue
		}
		if p := got[0].Parent().SpanID(); p != parent.SpanContext().SpanID() {
			t.Errorf("%s: identity.authenticate's parent %v, want the request's span %v", c.name, p, parent.SpanContext().SpanID())
		}
		want := map[string]string{"identity.proof": fmt.Sprint(int(pid.ProofSecret)), "identity.outcome": c.outcome}
		if c.outcome == "ok" {
			want["identity.principal"] = alice.Principal.ID.String()
		}
		have := map[string]string{}
		for _, kv := range got[0].Attributes() {
			have[string(kv.Key)] = kv.Value.Emit()
			attrs = append(attrs, kv.Value.Emit())
		}
		if fmt.Sprint(have) != fmt.Sprint(want) {
			t.Errorf("%s: identity.authenticate's attributes %v, want exactly %v", c.name, have, want)
		}
	}

	// Every event the table recorded, as LogLines writes them: one line each.
	var lines bytes.Buffer
	sink := identity.LogLines(slog.New(slog.NewJSONHandler(&lines, nil)))
	all := r.logons.since(0)
	for i := range all {
		sink.Logon(context.Background(), &all[i])
	}
	if n := strings.Count(lines.String(), "\n"); n != len(all) {
		t.Errorf("LogLines wrote %d lines for %d events, want one each", n, len(all))
	}
	logs := lines.String() + r.logs.String()
	haystack := strings.Join([]string{logs, strings.Join(errTexts, "\n"), strings.Join(attrs, "\n"), fmt.Sprintf("%+v", all)}, "\n")
	for _, b := range secretsSeen {
		// A 4-byte prefix in hex too, the form a log line holding part of a
		// hash takes.
		forms := []string{hex.EncodeToString(b[:4]), strings.ToUpper(hex.EncodeToString(b[:4])), base64.StdEncoding.EncodeToString(b), string(b)}
		for _, form := range forms {
			if strings.Contains(haystack, form) {
				t.Errorf("a credential's bytes %x appear in an event, a log line, an error or a span attribute", b)
			}
		}
	}
	for _, n := range names {
		if strings.Contains(logs, n) {
			t.Errorf("the presented name %q appears in a log line", n)
		}
	}
}

// A malformed response is an attempt: each records one event and one count,
// outcome invalid, and none counts toward the throttle, so after ten from one
// address the right password still signs in. Catches a malformed response
// left unrecorded, or counted as a wrong password (RFC 18 §9.2).
func TestSignInRecordsMalformedUncounted(t *testing.T) {
	r, p := ntlmServer(t, "NAS01")
	r.ntlmUser(t, p, "alice", "1500", alicePassword)
	events, count := r.logons.len(), r.signIns(t, "invalid")
	for range 10 {
		s := from(ntlmStep(alicePassword, "alice", "", false), addrA)
		s.NTLM.NTResponse[16] = 2
		if _, err := r.srv.SignIn(context.Background(), s); !errors.Is(err, vfserr.ErrInvalid) {
			t.Fatalf("a malformed response: SignIn = %v, want ErrInvalid", err)
		}
	}
	got := r.logons.since(events)
	n := 0
	for _, e := range got {
		if e.Outcome == "invalid" {
			n++
		}
	}
	if len(got) != 10 || n != 10 {
		t.Errorf("logon events = %d, %d of them invalid; want 10, all invalid", len(got), n)
	}
	if d := r.signIns(t, "invalid") - count; d != 10 {
		t.Errorf("identity_authentications_total{invalid} rose by %v, want 10", d)
	}
	r.signsIn(t, "the right password after 10 malformed responses", from(ntlmStep(alicePassword, "alice", "", false), addrA), false)
}

// A step that is no attempt, carrying neither or both mechanisms or naming no
// user, records nothing: no event, no count and no span (RFC 18 §3.1's
// unclassified step).
func TestSignInRecordsNoUnclassifiedStep(t *testing.T) {
	r, p := ntlmServer(t, "NAS01")
	r.ntlmUser(t, p, "alice", "1500", alicePassword)
	for _, c := range []struct {
		name string
		step func() pid.SignInStep
	}{
		{"neither mechanism", func() pid.SignInStep { return pid.SignInStep{Client: ntlmClient} }},
		{"both mechanisms", func() pid.SignInStep {
			s := ntlmStep(alicePassword, "alice", "", false)
			s.Kerberos = []byte("an AP-REQ")
			return s
		}},
		{"no user name", func() pid.SignInStep {
			s := ntlmStep(alicePassword, "alice", "", false)
			s.NTLM.User = ""
			return s
		}},
	} {
		events, count, spans := r.logons.len(), r.signIns(t, ""), len(r.spans.Ended())
		ctx, parent := r.tp.Tracer("test").Start(context.Background(), "request")
		if _, err := r.srv.SignIn(ctx, c.step()); !errors.Is(err, vfserr.ErrInvalid) {
			t.Errorf("%s: SignIn = %v, want ErrInvalid", c.name, err)
		}
		parent.End()
		if n := len(r.logons.since(events)); n != 0 {
			t.Errorf("%s: logon events = %d, want 0", c.name, n)
		}
		if d := r.signIns(t, "") - count; d != 0 {
			t.Errorf("%s: identity_authentications_total rose by %v, want 0", c.name, d)
		}
		if n := len(r.authenticateSpans(spans)); n != 0 {
			t.Errorf("%s: identity.authenticate spans = %d, want 0", c.name, n)
		}
	}
}

// New accepts a Deps without Logons, and every sign-in then answers
// ErrUnavailable with the zero result, the right password included. Catches
// a sign-in that goes unrecorded when no sink is wired.
func TestSignInWithoutLogonSink(t *testing.T) {
	r, p := ntlmServer(t, "NAS01")
	r.ntlmUser(t, p, "alice", "1500", alicePassword)
	s, err := identity.New(identity.Deps{Store: r.st, Clock: sim.NewTestClock(time.Unix(0, 0)), Secrets: p})
	if err != nil {
		t.Fatalf("New without Logons: %v", err)
	}
	st := state(1)
	st.Settings.NetBIOSName, st.Settings.NTLM = "NAS01", true
	if err := s.Apply(st); err != nil {
		t.Fatal(err)
	}
	res, oc, err := s.SignInOutcome(context.Background(), ntlmStep(alicePassword, "alice", "", false))
	refused(t, "the right password with no logon sink", res, oc, err, vfserr.ErrUnavailable, "unavailable")
}
