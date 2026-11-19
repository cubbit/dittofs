package directory_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

const base = "DC=corp,DC=example"

// dialLog passes each dial through to next and keeps the address it was
// sent to.
type dialLog struct {
	mu    sync.Mutex
	addrs []string
}

func (l *dialLog) wrap(next directory.Dialer) directory.Dialer {
	return func(ctx context.Context, rec directory.Record) (directory.Conn, error) {
		l.mu.Lock()
		l.addrs = append(l.addrs, rec.Address)
		l.mu.Unlock()
		return next(ctx, rec)
	}
}

func (l *dialLog) dials() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.addrs)
}

// env is a client over an in-memory directory, its dials logged.
type env struct {
	d     *identitytest.Directory
	clock *sim.TestClock
	dials *dialLog
	rec   directory.Record
	pw    []byte
	logs  *bytes.Buffer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d := identitytest.NewDirectory(base)
	name, pw := d.Account()
	return &env{
		d:     d,
		clock: sim.NewTestClock(time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)),
		dials: &dialLog{},
		rec:   directory.Record{Address: "ldaps://dc.corp.example", Base: base, BindName: name, Realm: "CORP.EXAMPLE"},
		pw:    pw,
		logs:  &bytes.Buffer{},
	}
}

// client is a client of e.rec through dial, logged into e.logs.
func (e *env) client(dial directory.Dialer) *directory.Client {
	pw := e.pw
	log := slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return directory.NewClient(e.rec, e.dials.wrap(dial), func(context.Context) ([]byte, error) { return pw, nil }, e.clock, log)
}

// faulty wraps e's directory in a fault wrapper running rules.
func (e *env) faulty(t *testing.T, rules ...sim.Rule) *identitytest.FaultyDirectory {
	t.Helper()
	sched, err := sim.NewSchedule(1, e.clock, rules...)
	if err != nil {
		t.Fatal(err)
	}
	return identitytest.NewFaultyDirectory(e.d.Dial, sched)
}

// Every value placed in a filter is escaped: a name holding a filter's
// special characters finds its own entry only, beside the entries it would
// match unescaped, and a SID or GUID holding them finds its own.
func TestLookupIsExact(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	guid := []byte("*()\\\x00guid-bytes-x")
	dns := map[string]string{}
	for _, name := range []string{"*", "ab*", "abc", `p(q)`, "pq", `x)(y`, `back\slash`, `back\5cslash`} {
		var attrs map[string][][]byte
		if name == "ab*" {
			attrs = map[string][][]byte{"objectGUID": {guid}}
		}
		dns[name] = e.d.AddUser(t, name, attrs)
	}
	c := e.client(e.d.Dial)
	for name, dn := range dns {
		a, ok, err := c.Lookup(ctx, directory.ByUserName, []byte(name))
		if err != nil || !ok || a.DN != dn || a.Name != name {
			t.Errorf("Lookup(%q) = %q %q, %v, %v; want only %q", name, a.DN, a.Name, ok, err, dn)
		}
	}
	a, ok, err := c.Lookup(ctx, directory.ByEntryID, guid)
	if err != nil || !ok || a.DN != dns["ab*"] {
		t.Errorf("Lookup by a GUID holding *()\\ and NUL = %q, %v, %v; want %q", a.DN, ok, err, dns["ab*"])
	}
	sidOf, _, err := c.Lookup(ctx, directory.ByUserName, []byte("abc"))
	if err != nil {
		t.Fatal(err)
	}
	a, ok, err = c.Lookup(ctx, directory.BySID, sidOf.SID)
	if err != nil || !ok || a.DN != dns["abc"] {
		t.Errorf("Lookup by abc's SID = %q, %v, %v; want %q", a.DN, ok, err, dns["abc"])
	}
	if a, ok, err := c.Lookup(ctx, directory.ByUserName, []byte("nobody")); err != nil || ok {
		t.Errorf("Lookup(nobody) = %q, %v, %v; want no entry and no error", a.DN, ok, err)
	}
}

// Two entries answering one name refuse it ErrUnavailable, logged, rather
// than answer either.
func TestLookupAmbiguousIsUnavailable(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	for _, cn := range []string{"dup1", "dup2"} {
		e.d.AddUser(t, cn, map[string][][]byte{"sAMAccountName": {[]byte("dup")}})
	}
	a, ok, err := e.client(e.d.Dial).Lookup(ctx, directory.ByUserName, []byte("dup"))
	if !errors.Is(err, vfserr.ErrUnavailable) || ok {
		t.Errorf("Lookup(dup) held by two entries = %q, %v, %v; want ErrUnavailable", a.DN, ok, err)
	}
	if !strings.Contains(e.logs.String(), "more than one entry") {
		t.Errorf("logs %q, want the ambiguous answer logged", e.logs.String())
	}
}

// A question based outside the naming context, which the directory refuses
// with no such object, is ErrUnavailable, never "no such entry".
func TestLookupOutsideNamingContextIsUnavailable(t *testing.T) {
	e := newEnv(t)
	e.d.AddUser(t, "alice", nil)
	e.rec.Base = "DC=other,DC=example"
	a, ok, err := e.client(e.d.Dial).Lookup(context.Background(), directory.ByUserName, []byte("alice"))
	if !errors.Is(err, vfserr.ErrUnavailable) || ok {
		t.Errorf("Lookup based outside the naming context = %q, %v, %v; want ErrUnavailable", a.DN, ok, err)
	}
}

// A referral, the answer turned into an LDAPResult of code 10 naming
// another host, is ErrUnavailable and dials nothing but the record's address.
func TestLookupReferralIsUnavailableAndNotFollowed(t *testing.T) {
	e := newEnv(t)
	e.d.AddUser(t, "alice", nil)
	referral := fmt.Errorf("%w: ldap://elsewhere.example/DC=corp,DC=example", directory.ErrReferral)
	f := e.faulty(t, sim.Rule{Op: "search", Key: "(&(objectClass=user)(sAMAccountName=alice))", Action: sim.FaultAction{Err: referral}})
	a, ok, err := e.client(f.Dial).Lookup(context.Background(), directory.ByUserName, []byte("alice"))
	if !errors.Is(err, vfserr.ErrUnavailable) || ok {
		t.Errorf("Lookup answered by a referral = %q, %v, %v; want ErrUnavailable", a.DN, ok, err)
	}
	if got := e.dials.dials(); !slices.Equal(got, []string{e.rec.Address}) {
		t.Errorf("dials %v, want only the record's %s", got, e.rec.Address)
	}
}

// A subtree question over the naming context, which the directory answers
// with the entry and the partitions' continuation references, answers the
// entry: the references are ignored, not refused.
func TestLookupIgnoresContinuationReferences(t *testing.T) {
	e := newEnv(t)
	dn := e.d.AddUser(t, "alice", nil)
	a, ok, err := e.client(e.d.Dial).Lookup(context.Background(), directory.ByUserName, []byte("alice"))
	if err != nil || !ok || a.DN != dn {
		t.Errorf("Lookup(alice) over the naming context = %q, %v, %v; want %q", a.DN, ok, err, dn)
	}
}

// Every search the client sends asks for the domain-scope control.
func TestEverySearchSendsDomainScope(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.d.AddUser(t, "alice", nil)
	f := e.faulty(t)
	c := e.client(f.Dial)
	if _, _, err := c.Lookup(ctx, directory.ByUserName, []byte("alice")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Test(ctx); err != nil {
		t.Fatal(err)
	}
	qs := f.Queries()
	if len(qs) < 4 {
		t.Fatalf("%d queries recorded, want the domain SID's two, the lookup and the test call's", len(qs))
	}
	for _, q := range qs {
		if !q.DomainScope {
			t.Errorf("query %q on %q sent without the domain-scope control", q.Filter, q.Base)
		}
	}
}

// filetime is t as an accountExpires value.
func filetime(t time.Time) []byte {
	return []byte(strconv.FormatInt(t.Unix()*1e7+116444736000000000, 10))
}

// A user's answer carries its enabled state from userAccountControl and
// accountExpires, its mail, SID and objectGUID; a group's its gid.
func TestLookupAnswer(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	now := e.clock.Now()
	guid := []byte("0123456789abcdef")
	for name, attrs := range map[string]map[string][][]byte{
		"off":     {"userAccountControl": {[]byte("514")}},
		"expired": {"accountExpires": {filetime(now.Add(-time.Hour))}},
		"later": {"accountExpires": {filetime(now.Add(time.Hour))}, "mail": {[]byte("later@corp.example")},
			"objectGUID": {guid}, "uidNumber": {[]byte("10001")}, "userAccountControl": {[]byte("512")}},
		"never": {"accountExpires": {[]byte("9223372036854775807")}},
		"zero":  {"accountExpires": {[]byte("0")}},
		"staff": {"objectClass": {[]byte("top"), []byte("group")}, "gidNumber": {[]byte("20000")}},
	} {
		e.d.AddUser(t, name, attrs)
	}
	c := e.client(e.d.Dial)
	for name, want := range map[string]bool{"off": false, "expired": false, "later": true, "never": true, "zero": true} {
		a, ok, err := c.Lookup(ctx, directory.ByUserName, []byte(name))
		if err != nil || !ok || a.Enabled != want || a.Group {
			t.Errorf("Lookup(%s): enabled %v, group %v, %v, %v; want enabled %v, a user", name, a.Enabled, a.Group, ok, err, want)
		}
	}
	a, _, err := c.Lookup(ctx, directory.ByUserName, []byte("later"))
	if err != nil || a.Mail != "later@corp.example" || !bytes.Equal(a.EntryID, guid) || a.UID == nil || *a.UID != 10001 || len(a.SID) != 28 {
		t.Errorf("Lookup(later) = mail %q, entry ID %x, uid %v, SID %x, %v; want later@corp.example, %x, 10001, a domain SID",
			a.Mail, a.EntryID, a.UID, a.SID, err, guid)
	}
	g, ok, err := c.Lookup(ctx, directory.ByGroupName, []byte("staff"))
	if err != nil || !ok || !g.Group || g.GID == nil || *g.GID != 20000 || len(g.SID) == 0 {
		t.Errorf("Lookup(group staff) = group %v, gid %v, SID %x, %v, %v; want a group, gid 20000, a SID", g.Group, g.GID, g.SID, ok, err)
	}
}

// An rfc2307 directory's user is found by uid, and its entry ID is its
// entryUUID.
func TestLookupRFC2307(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.rec.Schema = directory.SchemaRFC2307
	uuid := "4f6e3a2c-9b1d-4c1e-8f00-1234567890ab"
	dn := e.d.AddUser(t, "bob", map[string][][]byte{
		"objectClass": {[]byte("top"), []byte("posixAccount")}, "uid": {[]byte("bob")}, "entryUUID": {[]byte(uuid)}})
	c := e.client(e.d.Dial)
	a, ok, err := c.Lookup(ctx, directory.ByUserName, []byte("bob"))
	if err != nil || !ok || a.DN != dn || string(a.EntryID) != uuid {
		t.Fatalf("Lookup(bob) = %q, entry ID %q, %v, %v; want %q, %q", a.DN, a.EntryID, ok, err, dn, uuid)
	}
	if a, ok, err := c.Lookup(ctx, directory.ByEntryID, []byte(uuid)); err != nil || !ok || a.DN != dn {
		t.Errorf("Lookup by entryUUID = %q, %v, %v; want %q", a.DN, ok, err, dn)
	}
}

// An ad client reads its domain SID before its first answer, and answers
// ErrUnavailable while it cannot; an rfc2307 one reads none and answers.
func TestDomainSID(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.d.AddUser(t, "alice", nil)
	e.d.AddUser(t, "bob", map[string][][]byte{"objectClass": {[]byte("posixAccount")}, "uid": {[]byte("bob")}})

	conn, _ := e.d.Dial(ctx, e.rec)
	if err := conn.Bind(ctx, e.rec.BindName, e.pw); err != nil {
		t.Fatal(err)
	}
	res, err := conn.Search(ctx, directory.Request{Base: base, Scope: directory.ScopeBase, Filter: "(objectClass=*)", Attrs: []string{"objectSid"}})
	if err != nil || len(res.Entries) != 1 {
		t.Fatalf("reading the domain object: %v, %v", res, err)
	}
	want := res.Entries[0].Attrs["objectSid"][0]
	if got, err := e.client(e.d.Dial).DomainSID(ctx); err != nil || !bytes.Equal(got, want) {
		t.Errorf("DomainSID = %x, %v; want the domain object's %x", got, err, want)
	}

	// The domain object's read refused, or answered with its objectSid
	// torn away, as a bind without the right to read it is.
	refused := e.faulty(t, sim.Rule{Op: "search", Key: "(objectSid=*)", Action: sim.FaultAction{Err: errors.New("insufficient access")}})
	torn := e.faulty(t, sim.Rule{Op: "search", Key: "(objectSid=*)", Action: sim.FaultAction{TearAfter: 1}})
	torn.TearAttr("objectSid", "objectSid-unreadable")
	for name, f := range map[string]*identitytest.FaultyDirectory{"refused": refused, "torn": torn} {
		e.rec.Schema = directory.SchemaAD
		c := e.client(f.Dial)
		for range 2 {
			if a, ok, err := c.Lookup(ctx, directory.ByUserName, []byte("alice")); !errors.Is(err, vfserr.ErrUnavailable) {
				t.Errorf("%s: ad Lookup with no readable domain object = %q, %v, %v; want ErrUnavailable", name, a.DN, ok, err)
			}
		}
		e.rec.Schema = directory.SchemaRFC2307
		if a, ok, err := e.client(f.Dial).Lookup(ctx, directory.ByUserName, []byte("bob")); err != nil || !ok {
			t.Errorf("%s: rfc2307 Lookup with no readable domain object = %q, %v, %v; want bob", name, a.DN, ok, err)
		}
	}
}

// A bind credential that resolves empty, or not at all, is refused before
// any dial.
func TestEmptyCredentialRefusedBeforeDial(t *testing.T) {
	for name, resolve := range map[string]directory.Password{
		"empty":      func(context.Context) ([]byte, error) { return []byte{}, nil },
		"unresolved": func(context.Context) ([]byte, error) { return nil, errors.New("sealed under a key not loaded") },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.d.AddUser(t, "alice", nil)
			c := directory.NewClient(e.rec, e.dials.wrap(e.d.Dial), resolve, e.clock, nil)
			if _, _, err := c.Lookup(context.Background(), directory.ByUserName, []byte("alice")); !errors.Is(err, vfserr.ErrUnavailable) {
				t.Errorf("Lookup = %v, want ErrUnavailable", err)
			}
			if _, err := c.Test(context.Background()); !errors.Is(err, vfserr.ErrUnavailable) {
				t.Errorf("Test = %v, want ErrUnavailable", err)
			}
			if got := e.dials.dials(); len(got) != 0 {
				t.Errorf("dials %v, want none", got)
			}
		})
	}
}

// The bind password is in no log line and no error, whether it binds or
// not.
func TestBindPasswordNeverLogged(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	for _, cn := range []string{"dup1", "dup2"} {
		e.d.AddUser(t, cn, map[string][][]byte{"sAMAccountName": {[]byte("dup")}})
	}
	right := string(e.pw)
	_, _, err1 := e.client(e.d.Dial).Lookup(ctx, directory.ByUserName, []byte("dup"))
	e.pw = []byte("a-wrong-password-7f3a")
	_, _, err2 := e.client(e.d.Dial).Lookup(ctx, directory.ByUserName, []byte("dup"))
	if err1 == nil || err2 == nil {
		t.Fatalf("Lookups = %v, %v; want both refused", err1, err2)
	}
	for _, pw := range []string{right, string(e.pw)} {
		if strings.Contains(e.logs.String(), pw) || strings.Contains(err1.Error()+err2.Error(), pw) {
			t.Errorf("the password %q is in a log line or an error: %q, %v, %v", pw, e.logs.String(), err1, err2)
		}
	}
	if e.logs.Len() == 0 {
		t.Error("nothing logged, want the ambiguous answer and the failed bind")
	}
}

// Each call is capped at 5 s on the client's clock, over a caller's context
// that has no deadline.
func TestQueryTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.d.AddUser(t, "alice", nil)
		f := e.faulty(t, sim.Rule{Op: "search", Key: "(&(objectClass=user)(sAMAccountName=alice))", Action: sim.FaultAction{Err: identitytest.ErrHold}})
		c := e.client(f.Dial)
		done := make(chan error, 1)
		go func() {
			_, _, err := c.Lookup(context.Background(), directory.ByUserName, []byte("alice"))
			done <- err
		}()
		synctest.Wait()
		e.clock.Advance(5*time.Second - time.Nanosecond)
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("Lookup answered %v before 5 s, want it held", err)
		default:
		}
		e.clock.Advance(time.Nanosecond)
		synctest.Wait()
		select {
		case err := <-done:
			if !errors.Is(err, vfserr.ErrUnavailable) || !strings.Contains(err.Error(), "query timeout") {
				t.Errorf("Lookup at 5 s = %v, want ErrUnavailable for the query timeout", err)
			}
		default:
			t.Fatal("Lookup still held at 5 s, want it ended by the query timeout")
		}
	})
}

// NewClient dials nothing: the first question does.
func TestNewClientDialsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		f := e.faulty(t, sim.Rule{Op: "dial", Key: "*", Action: sim.FaultAction{Err: identitytest.ErrHold}})
		c := e.client(f.Dial)
		synctest.Wait()
		if got := e.dials.dials(); len(got) != 0 {
			t.Errorf("dials %v after NewClient, want none", got)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, err := c.Lookup(ctx, directory.ByUserName, []byte("alice")); !errors.Is(err, vfserr.ErrUnavailable) {
			t.Errorf("Lookup with its context ended = %v, want ErrUnavailable", err)
		}
	})
}

// A closed client lets the question running on it finish, then closes its
// connection: the next question dials again.
func TestCloseWaitsForQuestionsInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		e := newEnv(t)
		dn := e.d.AddUser(t, "alice", nil)
		// The root DSE's read, after the bind and before the domain
		// object's, is held a second.
		f := e.faulty(t, sim.Rule{Op: "search", Key: "(objectClass=*)", Count: 1, Action: sim.FaultAction{Delay: time.Second}})
		c := e.client(f.Dial)
		type result struct {
			a   directory.Answer
			err error
		}
		done := make(chan result, 1)
		go func() { a, _, err := c.Lookup(ctx, directory.ByUserName, []byte("alice")); done <- result{a, err} }()
		synctest.Wait()
		c.Close()
		e.clock.Advance(time.Second)
		if r := <-done; r.err != nil || r.a.DN != dn {
			t.Errorf("Lookup in flight across Close = %q, %v; want %q", r.a.DN, r.err, dn)
		}
		if _, _, err := c.Lookup(ctx, directory.ByUserName, []byte("alice")); err != nil {
			t.Errorf("Lookup after Close = %v, want it answered", err)
		}
		if got := e.dials.dials(); len(got) != 2 {
			t.Errorf("%d dials, want 2: the connection closed once the first question returned", len(got))
		}
	})
}

// The test call binds and reads the record's base entry, and refuses a
// bind that fails.
func TestTestCall(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if got, err := e.client(e.d.Dial).Test(ctx); err != nil || !strings.EqualFold(got.DN, base) {
		t.Errorf("Test = %q, %v; want the base %q", got.DN, err, base)
	}
	e.pw = []byte("wrong")
	if got, err := e.client(e.d.Dial).Test(ctx); !errors.Is(err, vfserr.ErrUnavailable) {
		t.Errorf("Test with a wrong password = %q, %v; want ErrUnavailable", got.DN, err)
	}
}

// No question asks for an attribute holding an NT or LM hash.
func TestAttrsReadNoHash(t *testing.T) {
	for _, schema := range []string{directory.SchemaAD, directory.SchemaRFC2307} {
		for _, a := range directory.Attrs(directory.Record{Schema: schema}) {
			for _, hash := range []string{"unicodePwd", "ntPwdHistory", "sambaNTPassword", "dBCSPwd", "lmPwdHistory", "supplementalCredentials", "sambaLMPassword"} {
				if strings.EqualFold(a, hash) {
					t.Errorf("schema %s: the attribute list names %s", schema, a)
				}
			}
		}
	}
}

// A caller that gives up stops waiting at once, with its context's error,
// and leaves the shared connection to the other questions: one cancelled
// while its search is held, or while its dial holds the connection being
// opened, costs no other question an answer or a dial.
func TestCancelledCallerLeavesConnection(t *testing.T) {
	// returned reports whether done holds an answer yet, without waiting.
	returned := func(done <-chan error) (error, bool) {
		select {
		case err := <-done:
			return err, true
		default:
			return nil, false
		}
	}
	for name, rule := range map[string]sim.Rule{
		"search": {Op: "search", Key: "(&(objectClass=user)(sAMAccountName=alice))", Action: sim.FaultAction{Err: identitytest.ErrHold}},
		"dial":   {Op: "dial", Key: "*", Count: 1, Action: sim.FaultAction{Delay: time.Second}},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t)
				e.d.AddUser(t, "alice", nil)
				bob := e.d.AddUser(t, "bob", nil)
				c := e.client(e.faulty(t, rule).Dial)
				if name == "search" { // the connection is open before alice's search
					if _, _, err := c.Lookup(context.Background(), directory.ByUserName, []byte("bob")); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithCancel(context.Background())
				gaveUp := make(chan error, 1)
				go func() { _, _, err := c.Lookup(ctx, directory.ByUserName, []byte("alice")); gaveUp <- err }()
				synctest.Wait()
				cancel()
				synctest.Wait()
				if err, ok := returned(gaveUp); !ok {
					t.Error("the cancelled Lookup still waits, want it returned at once")
				} else if !errors.Is(err, context.Canceled) || !errors.Is(err, vfserr.ErrUnavailable) {
					t.Errorf("the cancelled Lookup = %v, want ErrUnavailable wrapping context.Canceled", err)
				}
				type result struct {
					dn  string
					err error
				}
				other := make(chan result, 1)
				go func() {
					a, _, err := c.Lookup(context.Background(), directory.ByUserName, []byte("bob"))
					other <- result{a.DN, err}
				}()
				synctest.Wait()
				e.clock.Advance(time.Second) // the held dial, if any, answers
				r := <-other
				if r.err != nil || r.dn != bob {
					t.Errorf("the other Lookup = %q, %v; want %q", r.dn, r.err, bob)
				}
				if got := e.dials.dials(); len(got) != 1 {
					t.Errorf("%d dials, want 1: the cancelled caller dropped the shared connection", len(got))
				}
				e.clock.Advance(5 * time.Second) // the held search ends
				synctest.Wait()
			})
		})
	}
}

// Resolving the bind password is capped at 5 s too: a secret provider that
// never answers fails the question then, and the one queued behind it, which
// waits for the connection being opened, goes on to answer.
func TestPasswordUnderQueryTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		dn := e.d.AddUser(t, "alice", nil)
		var calls sync.Mutex
		n := 0
		password := func(ctx context.Context) ([]byte, error) {
			calls.Lock()
			n++
			first := n == 1
			calls.Unlock()
			if first {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return e.pw, nil
		}
		c := directory.NewClient(e.rec, e.dials.wrap(e.d.Dial), password, e.clock, nil)
		held := make(chan error, 1)
		go func() {
			_, _, err := c.Lookup(context.Background(), directory.ByUserName, []byte("alice"))
			held <- err
		}()
		synctest.Wait()
		type result struct {
			dn  string
			err error
		}
		queued := make(chan result, 1)
		go func() {
			a, _, err := c.Lookup(context.Background(), directory.ByUserName, []byte("alice"))
			queued <- result{a.DN, err}
		}()
		synctest.Wait()
		e.clock.Advance(5*time.Second - time.Nanosecond)
		synctest.Wait()
		select {
		case err := <-held:
			t.Fatalf("Lookup answered %v before 5 s, want it held on the password", err)
		default:
		}
		e.clock.Advance(time.Nanosecond)
		synctest.Wait()
		select {
		case err := <-held:
			if !errors.Is(err, vfserr.ErrUnavailable) || !strings.Contains(err.Error(), "query timeout") {
				t.Errorf("Lookup at 5 s = %v, want ErrUnavailable for the query timeout", err)
			}
		default:
			t.Fatal("Lookup still held on the password at 5 s, want it ended by the query timeout")
		}
		if r := <-queued; r.err != nil || r.dn != dn {
			t.Errorf("the queued Lookup = %q, %v; want %q", r.dn, r.err, dn)
		}
	})
}
