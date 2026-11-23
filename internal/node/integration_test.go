package node_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// failSets fails every set of a key pattern matches with the store
// unreachable.
func failSets(pattern string) sim.Rule {
	return sim.Rule{Op: kvtest.OpSet, Key: pattern, Action: sim.FaultAction{Err: vfserr.ErrUnavailable}}
}

// §2.1, API writes only records, its fault half, and §6 audit's one commit:
// a share create answers 503 when the share's own key cannot be written,
// leaving no share and no applied audit record, the refusal audited in its
// own write; and answers 503 when the audit record's key cannot be written,
// leaving no share, as the change and its record commit together. Neither
// leaves a binding once the store answers again and the node has reconciled
// through every counter. Catches a handler that binds a share itself, and a
// change committed apart from its audit record.
func TestWiredShareCreateWithItsCommitFaulted(t *testing.T) {
	onBackends(t, func(t *testing.T, memory bool) {
		w := newWired(t, memory, nil)
		configure(t, w.h, storeA)
		ns := namespaceOn(t, w.h, storeA)
		caughtUp(t, w.h)
		shares, bound := w.shareIDs(t), boundShares(w.h)
		create := func(what, pattern string) {
			t.Helper()
			body := apiv1.NewShare{Spec: apiv1.ShareSpec{Name: "faulted", Path: "/faulted", Namespace: uuid.UUID(ns).String()}, ExportPolicy: exportPolicy}
			restore := w.faulting(t, failSets(pattern))
			r := w.send(t, http.MethodPost, "/api/v1/shares", w.operator.token, body, idemKey()...)
			restore()
			wantStatus(t, "POST /api/v1/shares with "+what+" faulted", r, http.StatusServiceUnavailable, apiv1.CodeUnavailable)
			if got := w.shareIDs(t); !slices.Equal(got, shares) {
				t.Errorf("shares listed after the create with %s faulted: %v, want %v, none created", what, got, shares)
			}
		}
		create("the share's key", "S‖*")
		create("the audit record's key", "AU‖*")

		outcomes := map[string]int{}
		for _, rec := range w.auditsBy(t, w.admin.token, w.operator.p) {
			if rq := requestOf(t, rec); rq.Method == http.MethodPost && rq.Path == "/api/v1/shares" {
				outcomes[rec.Outcome]++
			}
		}
		if want := map[string]int{"refused: unavailable": 1}; !mapsEqual(outcomes, want) {
			t.Errorf("audit records of the two faulted creates, by outcome: %v, want %v: the first refusal alone, the second's record unwritable", outcomes, want)
		}

		w.h.Node().Wake()
		caughtUp(t, w.h)
		if got := w.shareIDs(t); !slices.Equal(got, shares) {
			t.Errorf("shares listed after a full reconcile: %v, want %v", got, shares)
		}
		if got := boundShares(w.h); !slices.Equal(got, bound) {
			t.Errorf("shares bound after a full reconcile: %v, want %v, no binding for either faulted create", got, bound)
		}
	})
}

func mapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// §2.2 order, its API clause: an operation readable before Stop reads
// through the API once reconcile stopped, once the adapters stopped, and
// while the runner's stop, step 3's, is waited on. Catches an API that stops
// before the drain it reports.
func TestWiredOperationReadableUntilStopStep3(t *testing.T) {
	onBackends(t, func(t *testing.T, memory bool) {
		var (
			mu    sync.Mutex
			read  = map[string]int{}
			probe func() int
		)
		stepped := func(_ *node.Node, step string) {
			mu.Lock()
			p := probe
			mu.Unlock()
			switch step {
			case "stop reconcile", "stop adapters", "wait runner":
				if p != nil {
					status := p()
					mu.Lock()
					read[step] = status
					mu.Unlock()
				}
			}
		}
		w := newWired(t, memory, node.NewStepping(stepped))
		op := entity.Operation{ID: entity.OperationID(uuid.New()), Kind: string(apiv1.OpUserDelete),
			Target: entity.ResourceRef{Kind: entity.ResourceUser, ID: make([]byte, len(entity.PrincipalID{}))}, State: entity.OpPending}
		if _, err := w.h.Store().ControlPlane().PutOperation(t.Context(), op, 0); err != nil {
			t.Fatalf("create the operation: %v", err)
		}
		path := "/api/v1/operations/" + uuid.UUID(op.ID).String()
		if r := w.send(t, http.MethodGet, path, w.viewer.token, nil); r.status != http.StatusOK {
			t.Fatalf("GET the operation before Stop: answered %d %s, want 200", r.status, r.body)
		}
		mu.Lock()
		probe = func() int {
			r, err := w.try(t.Context(), http.MethodGet, path, w.viewer.token, nil)
			if err != nil {
				return 0
			}
			return r.status
		}
		mu.Unlock()
		if err := w.h.Node().Stop(t.Context()); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		for _, step := range []string{"stop reconcile", "stop adapters", "wait runner"} {
			if got := read[step]; got != http.StatusOK {
				t.Errorf("GET the operation at %s: answered %d, want 200 until step 3 ends", step, got)
			}
		}
	})
}

// §2.5 health, its API clause: GET /api/v1/health without a token answers
// 401, while a viewer reads it. Catches grace state or names leaked to an
// anonymous caller.
func TestWiredHealthNeedsAToken(t *testing.T) {
	onBackends(t, func(t *testing.T, memory bool) {
		w := newWired(t, memory, nil)
		wantStatus(t, "GET /api/v1/health without a token", w.send(t, http.MethodGet, "/api/v1/health", "", nil), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
		if r := w.send(t, http.MethodGet, "/api/v1/health", w.viewer.token, nil); r.status != http.StatusOK {
			t.Errorf("GET /api/v1/health as a viewer: answered %d %s, want 200", r.status, r.body)
		}
	})
}

// §8.1, a record the node refuses to build, through the API: a share whose
// namespace names a remote store no setting configures is committed, and its
// status read through the API counts the node refusing it and applying it
// nowhere. Catches a 201 with the failure only in a log.
func TestWiredRefusedShareReadsRefused(t *testing.T) {
	onBackends(t, func(t *testing.T, memory bool) {
		w := newWired(t, memory, nil)
		sh := createShare(t, w.h, "nowhere", storeB)
		path := "/api/v1/shares/" + sh.ID.String()
		// The status writer writes a turn a second; ten turns are well past
		// the one that reports the refusal.
		var got apiv1.Share
		for i := 0; ; i++ {
			r := w.send(t, http.MethodGet, path, w.viewer.token, nil)
			if r.status != http.StatusOK || json.Unmarshal(r.body, &got) != nil {
				t.Fatalf("GET the refused share: answered %d %s, want 200", r.status, r.body)
			}
			if got.Status.Refused > 0 {
				break
			}
			if i == 10 {
				t.Fatalf("the refused share's status after ten turns: %+v, want it refused", got.Status)
			}
			turn(w.h)
		}
		if got.Status.Refused != 1 || got.Status.Applied == nil || got.Status.Applied.Nodes != 0 || got.Status.Applied.Of != 1 {
			t.Errorf("the refused share's status: refused %d, applied %+v; want refused by 1, applied on 0 of 1", got.Status.Refused, got.Status.Applied)
		}
	})
}

// healthBehind reports whether GET /api/v1/health, read as c, names
// config_generation_behind of the settings counter.
func (w *wired) healthBehind(t *testing.T, c account) bool {
	t.Helper()
	r := w.send(t, http.MethodGet, "/api/v1/health", c.token, nil)
	var h apiv1.Health
	if r.status != http.StatusOK || json.Unmarshal(r.body, &h) != nil {
		t.Fatalf("GET /api/v1/health: answered %d %s, want 200", r.status, r.body)
	}
	return slices.ContainsFunc(h.Conditions, func(c apiv1.Condition) bool {
		return c.Name == "config_generation_behind" && c.Subject == "settings"
	})
}

// The condition a node behind raises reaches an admin through
// /api/v1/health: config_generation_behind of the settings counter is named
// while the frozen node lags, and cleared once it catches up. When it is
// raised is TestStatusRaisesBehindOnAFrozenApply's. Catches a condition never
// raised or never cleared through the API.
func TestWiredBehindReadThroughHealth(t *testing.T) {
	onBackends(t, func(t *testing.T, memory bool) {
		var frozen atomic.Bool
		reached, release := make(chan kvstore.Counters, 1), make(chan struct{})
		w := newWired(t, memory, node.NewPausing(func(c kvstore.Counters) {
			if frozen.Load() {
				reached <- c
				<-release
			}
		}))
		defer func() {
			if frozen.Swap(false) {
				close(release)
			}
		}()
		caughtUp(t, w.h)
		if w.healthBehind(t, w.admin) {
			t.Fatal("config_generation_behind named before any change")
		}

		frozen.Store(true)
		configure(t, w.h, storeA)
		want := counters(t, w.h).Settings
		w.h.Node().Wake()
		if c := awaitRead(t, reached); c.Settings != want {
			t.Fatalf("frozen pass read settings %d, want the change at %d", c.Settings, want)
		}
		w.h.Clock().Advance(time.Minute)
		if !w.healthBehind(t, w.admin) {
			t.Error("config_generation_behind not named while the node lags by a minute")
		}

		frozen.Store(false)
		close(release)
		caughtUp(t, w.h)
		if w.healthBehind(t, w.admin) {
			t.Error("config_generation_behind still named once the node caught up")
		}
	})
}

// §5.2 roles, its secret clause: an operator cannot reach the admin's
// password or the signing key through any route that writes a secret or the
// admin's record. Its PATCH, PUT, DELETE and :set-password of the admin,
// under the admin's own ETag, answer 404, as the admin is hidden from it; its
// token creation for the admin and its keytab write are refused; its own
// store credential write and its password change carrying the admin's
// password reach only what it holds. After them the admin's record, its
// password's version and the signing key's version are unchanged, and the
// admin signs in with its password. Catches a privileged credential replaced
// through a secret write, and an account with a role an operator may edit.
func TestWiredOperatorReachesNoAdminCredential(t *testing.T) {
	onBackends(t, func(t *testing.T, memory bool) {
		w := newWired(t, memory, nil)
		configure(t, w.h, storeA)
		in, err := w.h.Store().Installation(t.Context())
		if err != nil {
			t.Fatalf("read the installation: %v", err)
		}
		before := w.stored(t, w.admin.p)
		password, signing := versionOf(t, w.authNode, before.Password), versionOf(t, w.authNode, in.SigningKey)
		admin := userPath(w.admin.p)
		get := w.send(t, http.MethodGet, admin, w.admin.token, nil)
		var spec apiv1.User
		if get.status != http.StatusOK || json.Unmarshal(get.body, &spec) != nil {
			t.Fatalf("GET the admin as the admin: answered %d %s, want 200", get.status, get.body)
		}
		tag := get.header.Get("ETag")
		hidden := []struct {
			method, path string
			body         any
			hdr          []string
		}{
			{http.MethodPatch, admin, json.RawMessage(`{"spec":{"disabled":true,"email":"taken@example.com"}}`), []string{"If-Match", tag}},
			{http.MethodPut, admin, map[string]any{"spec": apiv1.UserSpec{Name: spec.Spec.Name, Role: spec.Spec.Role, Disabled: true}}, []string{"If-Match", tag}},
			{http.MethodDelete, admin, nil, append([]string{"If-Match", tag}, idemKey()...)},
			{http.MethodPost, admin + ":set-password", apiv1.SetPassword{Password: "taken over"}, nil},
		}
		for _, c := range hidden {
			wantStatus(t, "the operator's "+c.method+" "+c.path, w.send(t, c.method, c.path, w.operator.token, c.body, c.hdr...), http.StatusNotFound, codeNotFound)
		}
		// Each body is one the route accepts from an admin, so only the
		// operator's role refuses it: the admin's tokens are hidden from it, as
		// the admin's record is, and the keytab's PUT is an admin's. The
		// settings table's per-key role refuses the keytab to an operator too,
		// naming the key; the route refuses it first, naming none, and this row
		// holds the route's check.
		for _, c := range []struct {
			method, path string
			body         any
			hdr          []string
			status       int
			code         apiv1.Code
		}{
			{http.MethodPost, admin + "/api-tokens", apiv1.APITokenSpec{Expires: w.h.Clock().Now().Add(time.Hour)}, nil, http.StatusNotFound, codeNotFound},
			{http.MethodPut, "/api/v1/installation/keytab", map[string]any{"spec": map[string]any{"value": keytabOf(t, "cifs/nas.ad.example")}}, []string{"If-None-Match", "*"}, http.StatusForbidden, apiv1.CodeForbidden},
		} {
			r := w.send(t, c.method, c.path, w.operator.token, c.body, c.hdr...)
			wantStatus(t, "the operator's "+c.method+" "+c.path, r, c.status, c.code)
			if p := r.problem(t); p.Field != "" {
				t.Errorf("the operator's %s %s: refused naming %s, want the route's refusal, naming no field", c.method, c.path, p.Field)
			}
		}
		// An operator holds these routes: what each reaches is checked by the
		// versions below, not by its answer.
		w.send(t, http.MethodPut, "/api/v1/stores/"+uuid.UUID(storeA).String()+"/credential", w.operator.token,
			map[string]any{"spec": apiv1.CredentialSpec{Value: []byte("taken over")}}, "If-None-Match", "*")
		w.send(t, http.MethodPost, "/api/v1/auth/password", w.operator.token, apiv1.PasswordChange{Current: adminPassword, New: "taken over"})

		after := w.stored(t, w.admin.p)
		if after.Disabled != before.Disabled || after.Email != before.Email || after.Generation != before.Generation {
			t.Errorf("the admin after the operator's writes: disabled %v, email %q, generation %d; want %v, %q, %d unchanged",
				after.Disabled, after.Email, after.Generation, before.Disabled, before.Email, before.Generation)
		}
		if got := versionOf(t, w.authNode, after.Password); got != password {
			t.Errorf("the admin's password version after the operator's writes: %d, want %d", got, password)
		}
		if got := versionOf(t, w.authNode, in.SigningKey); got != signing {
			t.Errorf("the signing key's version after the operator's writes: %d, want %d", got, signing)
		}
		w.tokens(t, "admin", adminPassword)
	})
}

// throttled is a node whose API listens where set leaves its bootstrap's
// listen and socket, holding five viewer accounts, user0 to user4, each with
// its own password.
func throttled(t *testing.T, memory bool, set func(cfg *node.Config)) (*nodetest.Harness, []entity.Principal) {
	t.Helper()
	opts := []nodetest.Option{nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		set(&cfg)
		return node.New(cfg)
	})}
	if memory {
		opts = append(opts, nodetest.InMemory())
	}
	h := nodetest.New(t, opts...)
	var ps []entity.Principal
	for i := range 5 {
		ps = append(ps, h.Account(t, accountName(i), accountName(i)+"'s password", entity.RoleViewer))
	}
	started(t, h)
	return h, ps
}

func accountName(i int) string { return "user" + strconv.Itoa(i) }

// signInVia signs name in with password through c at base, and returns the
// status it answered.
func signInVia(t *testing.T, c *http.Client, base, name, password string) int {
	t.Helper()
	b, _ := json.Marshal(apiv1.SignIn{Name: name, Password: password})
	resp, err := c.Post(base+"/api/v1/auth/sign-in", "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatalf("sign %s in at %s: %v", name, base, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// fromAddr is a client whose connections leave from local.
func fromAddr(local net.IP) *http.Client {
	d := &net.Dialer{LocalAddr: &net.TCPAddr{IP: local}}
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableKeepAlives: true, DialContext: d.DialContext}}
}

// sprayed fails a sign-in for each of the five accounts through c at base,
// each answered 401, and then signs user0 in with its own password, returning
// what that sixth attempt answered.
func sprayed(t *testing.T, c *http.Client, base string) int {
	t.Helper()
	for i := range 5 {
		if got := signInVia(t, c, base, accountName(i), "wrong"); got != http.StatusUnauthorized {
			t.Fatalf("failed sign-in %d, for %s: answered %d, want 401", i+1, accountName(i), got)
		}
	}
	return signInVia(t, c, base, accountName(0), accountName(0)+"'s password")
}

// dialsFrom reports whether a connection can leave from local for a listener
// on 127.0.0.1.
func dialsFrom(t *testing.T, local net.IP) bool {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	c, err := (&net.Dialer{LocalAddr: &net.TCPAddr{IP: local}, Timeout: 5 * time.Second}).Dial("tcp", l.Addr().String())
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// §5.3 throttling, its loopback and local-socket clauses, through the
// node's own listeners: five failed sign-ins for five accounts from
// 127.0.0.6, from ::1, and from 127.0.0.1 each make the sixth from that
// source answer 429, a loopback address being a source like any other; five
// for one account through the local socket leave the sixth checked, and
// write no source or pair count. Catches a listener that loses the caller's
// address.
//
// decision: ::ffff:127.0.0.1 is not sprayed apart from 127.0.0.1. On a
// listener of both families the platform hands the node an IPv4 peer as
// 127.0.0.1 whatever spelling the client dialled, so no client here can make
// the node see the mapped spelling; entity.SourceKey's own test holds the
// two spellings to one source. Withdraw this if the node ever reads a peer
// address the platform has not already unmapped.
func TestWiredLoopbackSignInsThrottled(t *testing.T) {
	onBackends(t, func(t *testing.T, memory bool) {
		t.Run("127.0.0.6", func(t *testing.T) {
			if !dialsFrom(t, net.ParseIP("127.0.0.6")) {
				t.Skip("this host does not route 127.0.0.6 to loopback; on macOS, sudo ifconfig lo0 alias 127.0.0.6")
			}
			h, _ := throttled(t, memory, func(*node.Config) {})
			addr, _ := h.Node().Addrs()
			if got := sprayed(t, fromAddr(net.ParseIP("127.0.0.6")), "http://"+addr); got != http.StatusTooManyRequests {
				t.Errorf("the sixth sign-in from 127.0.0.6: answered %d, want 429", got)
			}
		})
		t.Run("::1", func(t *testing.T) {
			l, err := net.Listen("tcp", "[::1]:0")
			if err != nil {
				t.Skipf("this host has no IPv6 loopback to listen on: %v", err)
			}
			_ = l.Close()
			h, _ := throttled(t, memory, func(cfg *node.Config) { cfg.Bootstrap.API.Listen = []string{"[::1]:0"} })
			addr, _ := h.Node().Addrs()
			if got := sprayed(t, fromAddr(nil), "http://"+addr); got != http.StatusTooManyRequests {
				t.Errorf("the sixth sign-in from ::1: answered %d, want 429", got)
			}
		})
		t.Run("127.0.0.1", func(t *testing.T) {
			h, _ := throttled(t, memory, func(*node.Config) {})
			addr, _ := h.Node().Addrs()
			if got := sprayed(t, fromAddr(nil), "http://"+addr); got != http.StatusTooManyRequests {
				t.Errorf("the sixth sign-in from 127.0.0.1: answered %d, want 429", got)
			}
		})
		t.Run("local socket", func(t *testing.T) {
			// A socket's path is capped near 104 bytes on some hosts, which a
			// subtest's t.TempDir() can pass.
			dir, err := os.MkdirTemp("", "dfs")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			path := filepath.Join(dir, "api.sock")
			h, ps := throttled(t, memory, func(cfg *node.Config) { cfg.Bootstrap.API.Socket = path })
			c := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableKeepAlives: true,
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", path)
				}}}
			for i := range 5 {
				if got := signInVia(t, c, "http://node", accountName(0), "wrong"); got != http.StatusUnauthorized {
					t.Fatalf("failed sign-in %d on the socket: answered %d, want 401", i+1, got)
				}
			}
			if got := signInVia(t, c, "http://node", accountName(0), accountName(0)+"'s password"); got != http.StatusOK {
				t.Errorf("the sixth sign-in on the socket, with the password: answered %d, want 200, checked", got)
			}
			for _, k := range []entity.SignInKey{
				{Source: entity.SourceKey(netip.IPv4Unspecified())},
				{Source: entity.SourceKey(netip.MustParseAddr("127.0.0.1"))},
				{Account: ps[0], Source: entity.SourceKey(netip.IPv4Unspecified())},
				{Account: ps[0], Source: entity.SourceKey(netip.MustParseAddr("127.0.0.1"))},
			} {
				if f, found, err := h.Store().ControlPlane().SignInFailures(t.Context(), k); err != nil || found {
					t.Errorf("sign-in count %+v after failures on the socket: %+v, found %v (%v), want none", k, f, found, err)
				}
			}
		})
	})
}

// storeCodes answers each of the metadata store's error codes, as §4.7's
// table answers them: its status and its code.
var storeCodes = map[merrs.ErrorCode]struct {
	status int
	code   string
}{
	merrs.ErrNotFound: {404, "not_found"}, merrs.ErrAccessDenied: {403, "access_denied"}, merrs.ErrAuthRequired: {403, "auth_required"},
	merrs.ErrPermissionDenied: {403, "permission_denied"}, merrs.ErrAlreadyExists: {409, "already_exists"}, merrs.ErrNotEmpty: {409, "not_empty"},
	merrs.ErrIsDirectory: {400, "is_directory"}, merrs.ErrNotDirectory: {400, "not_directory"}, merrs.ErrInvalidArgument: {400, "invalid_argument"},
	merrs.ErrIOError: {500, "io_error"}, merrs.ErrNoSpace: {409, "no_space"}, merrs.ErrQuotaExceeded: {409, "quota_exceeded"},
	merrs.ErrReadOnly: {409, "read_only"}, merrs.ErrNotSupported: {400, "not_supported"}, merrs.ErrInvalidHandle: {400, "invalid_handle"},
	merrs.ErrStaleHandle: {409, "stale_handle"}, merrs.ErrLocked: {409, "locked"},
	merrs.ErrPrivilegeRequired: {403, "privilege_required"}, merrs.ErrNameTooLong: {400, "name_too_long"}, merrs.ErrDeadlock: {409, "deadlock"},
	merrs.ErrGracePeriod: {503, "grace_period"}, merrs.ErrLockConflict: {409, "lock_conflict"},
	merrs.ErrConflict: {409, "conflict"}, merrs.ErrCrossShare: {400, "cross_share"},
}

// §4.7 error table, and §8.1's store-unreachable row, through the API of a
// wired node: each value of the API's own set and of the components' sets
// that a call can provoke here answers its status and code; each error of
// the metadata store's own seam, the KV's errors and the store's codes,
// injected on the store's reads of one share, answers its own as that
// share's GET, a 503 among them with Retry-After, the store unreachable
// (vfserr.ErrUnavailable) answering 503 unavailable; and after them all
// api_unmapped_errors_total is 0. Catches an error with no row, and a store
// failure answered as anything but itself.
//
// decision: some values are neither provoked nor injected here. The
// filesystem's and the engine's errors (vfserr's file-operation values, the
// entity errors of the file tree) are raised by components this node's API
// reaches through no route; of the settings table's refusals, only
// unknown_key is reached, as the node's table holds no section that reads a
// key the API writes, so wrong_scope, required, bound_setting, secret_ref,
// newer_schema and format_version wait for one; of the secret provider's,
// only the store's unproven is reached; joined needs
// a joined installation; managed and not_a_copy are raised by no route yet;
// a KV conflict reaches no caller bare, and only contention makes one. Each
// is answered by the problem table, which the API's own test holds total; a
// fault seam per component on node.Config is what would reach them here.
// Withdraw an entry once a route or a seam on this node raises it.
func TestWiredErrorTable(t *testing.T) {
	onBackends(t, func(t *testing.T, memory bool) {
		w := newWired(t, memory, nil)
		sh := addShare(t, w.h)
		share := "/api/v1/shares/" + sh.ID.String()
		op, admin, viewer := w.operator.token, w.admin.token, w.viewer.token

		// The provoked rows run in order: each may set up the next.
		r := w.send(t, http.MethodPost, "/api/v1/users", admin, map[string]any{"spec": apiv1.UserSpec{Name: "gina"}}, "Idempotency-Key", "gina")
		if r.status != http.StatusCreated {
			t.Fatalf("create gina: answered %d %s, want 201", r.status, r.body)
		}
		gina, _ := entity.ParsePrincipalID(strings.TrimPrefix(r.header.Get("Location"), "/api/v1/users/"))
		pending := entity.Operation{ID: entity.OperationID(uuid.New()), Kind: string(apiv1.OpUserDelete),
			Target: entity.ResourceRef{Kind: entity.ResourceUser, ID: gina[:]}, State: entity.OpRunning}
		if _, err := w.h.Store().ControlPlane().PutOperation(t.Context(), pending, 0); err != nil {
			t.Fatalf("put a running deletion of gina: %v", err)
		}
		machine := entity.Principal{Kind: entity.PrincipalMachine, ID: entity.NewPrincipalID()}
		if _, err := w.h.Store().ControlPlane().PutUser(t.Context(), entity.User{Principal: machine, IDs: entity.ProtocolIDs{{Scheme: entity.SchemeKerberos, Value: "host/m.example.com@EXAMPLE.COM"}}}, 0, entity.Alloc{}); err != nil {
			t.Fatalf("put the machine: %v", err)
		}
		changer := w.h.Account(t, "hank", "hank's password", entity.RoleViewer)
		w.change(t, changer, func(tx entity.ControlPlaneTx, u *entity.User) error {
			u.MustChange = true
			_, err := tx.PutUser(*u, u.Generation, entity.Alloc{})
			return err
		})
		hank := w.tokens(t, "hank", "hank's password").Access
		etag := func(path string) string { return w.send(t, http.MethodGet, path, admin, nil).header.Get("ETag") }
		adminPath := userPath(w.admin.p)
		opPath := "/api/v1/operations/" + uuid.UUID(pending.ID).String()
		grant := share + "/grants/" + uuid.UUID(machine.ID).String()

		for _, c := range []struct {
			what   string
			send   func() authReply
			status int
			code   apiv1.Code
		}{
			{"no token", func() authReply { return w.send(t, http.MethodGet, "/api/v1/shares", "", nil) }, 401, apiv1.CodeUnauthenticated},
			{"a viewer's share create", func() authReply {
				return w.send(t, http.MethodPost, "/api/v1/shares", viewer, nil, idemKey()...)
			}, 403, apiv1.CodeForbidden},
			{"a share that does not exist", func() authReply { return w.send(t, http.MethodGet, "/api/v1/shares/"+uuid.NewString(), viewer, nil) }, 404, "not_found"},
			{"an unknown field", func() authReply {
				return w.send(t, http.MethodPost, "/api/v1/groups", op, json.RawMessage(`{"bogus":1}`), idemKey()...)
			}, 400, "invalid"},
			{"a PATCH with no If-Match", func() authReply {
				return w.send(t, http.MethodPatch, share, op, json.RawMessage(`{"spec":{}}`))
			}, 428, apiv1.CodePreconditionRequired},
			{"a PATCH under a stale If-Match", func() authReply {
				return w.send(t, http.MethodPatch, share, op, json.RawMessage(`{"spec":{}}`), "If-Match", `"999999"`)
			}, 412, apiv1.CodeGenerationMismatch},
			{"a key reused with another body", func() authReply {
				return w.send(t, http.MethodPost, "/api/v1/users", admin, map[string]any{"spec": apiv1.UserSpec{Name: "ivan"}}, "Idempotency-Key", "gina")
			}, 422, apiv1.CodeIdempotencyMismatch},
			{"a cancel", func() authReply {
				return w.send(t, http.MethodPost, opPath+":cancel", admin, nil, idemKey()...)
			}, 409, apiv1.CodeNotCancellable},
			{"a delete of a user whose deletion runs", func() authReply {
				p := "/api/v1/users/" + uuid.UUID(gina).String()
				return w.send(t, http.MethodDelete, p, op, nil, append([]string{"If-Match", etag(p)}, idemKey()...)...)
			}, 409, apiv1.CodeOperationRunning},
			{"the last admin deleted", func() authReply {
				return w.send(t, http.MethodDelete, adminPath, admin, nil, append([]string{"If-Match", etag(adminPath)}, idemKey()...)...)
			}, 409, apiv1.CodeLastAdmin},
			{"a machine granted", func() authReply {
				return w.send(t, http.MethodPut, grant, op, json.RawMessage(`{"spec":{"access":"read"}}`), "If-None-Match", "*")
			}, 400, "invalid"},
			{"a call before the password changes", func() authReply { return w.send(t, http.MethodGet, "/api/v1/shares", hank, nil) }, 403, apiv1.CodePasswordChangeRequired},
			{"a setting no section reads", func() authReply {
				return w.send(t, http.MethodPut, "/api/v1/settings/gc.schedule", admin, json.RawMessage(`{"spec":{"value":"weekly"}}`), "If-None-Match", "*")
			}, 400, "unknown_key"},
			{"a store's first credential, its capability unchecked", func() authReply {
				configureJSON(t, w.h, storeD)
				return w.send(t, http.MethodPut, "/api/v1/stores/"+uuid.UUID(storeD).String()+"/credential", admin,
					map[string]any{"spec": apiv1.CredentialSpec{Value: []byte("a credential")}}, "If-None-Match", "*")
			}, 409, "unproven"},
		} {
			wantStatus(t, c.what, c.send(), c.status, c.code)
		}
		if _, found, err := w.h.Store().ControlPlane().ShareGrant(t.Context(), sh.ID, machine); err != nil || found {
			t.Errorf("the machine's grant after its refused PUT: found %v (%v), want none written", found, err)
		}

		// Every call on a key naming the share fails with err, as keys.Describe
		// names an ID by its first four bytes.
		id := "*" + hex.EncodeToString(sh.ID[:4]) + "*"
		inject := func(what string, err error, status int, code apiv1.Code) {
			t.Helper()
			restore := w.faulting(t, sim.Rule{Op: "*", Key: id, Action: sim.FaultAction{Err: err}})
			r := w.send(t, http.MethodGet, share, viewer, nil)
			restore()
			what = "GET a share with its read failing " + what
			wantStatus(t, what, r, status, code)
			if status == http.StatusServiceUnavailable && r.header.Get("Retry-After") == "" {
				t.Errorf("%s: answered 503 with no Retry-After", what)
			}
		}
		for c := merrs.ErrorCode(1); !strings.HasPrefix(c.String(), "Unknown("); c++ {
			want, ok := storeCodes[c]
			if !ok {
				t.Errorf("store code %v: no row in the test's table", c)
				continue
			}
			inject(c.String(), &merrs.StoreError{Code: c, Message: "injected"}, want.status, apiv1.Code(want.code))
		}
		for _, c := range []struct {
			err    error
			status int
			code   apiv1.Code
		}{
			{kv.ErrNotFound, 404, "not_found"}, {kv.ErrTooLarge, 400, "too_large"}, {kv.ErrUncommittedVersion, 500, "uncommitted_version"},
			{kv.ErrClosed, 503, "closed"}, {vfserr.ErrUnavailable, 503, apiv1.CodeUnavailable},
			{context.DeadlineExceeded, 503, apiv1.CodeUnavailable}, {context.Canceled, 503, apiv1.CodeUnavailable},
		} {
			inject(c.err.Error(), c.err, c.status, c.code)
		}
		if r := w.send(t, http.MethodGet, share, viewer, nil); r.status != http.StatusOK {
			t.Errorf("GET the share once the store answers: %d %s, want 200", r.status, r.body)
		}

		// Six failed sign-ins from this address for one account throttle the
		// next, so this row runs last.
		w.h.Account(t, "jo", "jo's password", entity.RoleViewer)
		var last authReply
		for range 6 {
			last = w.signIn(t, "jo", "wrong")
		}
		wantStatus(t, "a sign-in past its wait", last, http.StatusTooManyRequests, apiv1.CodeThrottled)

		if n := series(t, w.h, "dittofs_api_unmapped_errors_total", ""); n != 0 {
			t.Errorf("dittofs_api_unmapped_errors_total after the error table: %v, want 0", n)
		}
	})
}
