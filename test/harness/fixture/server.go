package fixture

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
	"github.com/cubbit/dittofs/test/harness/registry"
)

// apiListening matches the record dfs start logs naming the API's bound
// address.
var apiListening = regexp.MustCompile(`node: listening api=(\S+)`)

// passwordEnv is the variable dfs init reads the administrator's first
// password from.
const passwordEnv = "DT_ADMIN_PASSWORD"

// installedText is what dfs init answers over a finished installation.
const installedText = "the installation is already initialised"

// Start initialises an installation under out/dfs with dfs init from bin,
// starts it there with dfs start, its output in out/dfs.log, and configures
// it through the API as f asks: the users User and User2, the share Share
// with the grants RFC 29 gives on it, applied on the node. It refuses a
// fixture Unsupported names, before anything starts. It makes one attempt:
// when a step fails, or ctx ends first, it stops what it started and returns
// why. w gets dt's lines on the setup, and never a credential.
func Start(ctx context.Context, f registry.Fixture, bin, out string, w io.Writer) (*Server, error) {
	if why := Unsupported(&f, runtime.GOOS); why != "" {
		return nil, errors.New(why)
	}
	// A server only the API reaches listens on loopback, so a host with no
	// other address runs it too.
	server := "127.0.0.1"
	if f.NFS.Serves() || f.SMB.Enabled {
		var err error
		if server, err = address(); err != nil {
			return nil, err
		}
	}
	s := &Server{f: f, data: filepath.Join(out, "dfs"), server: server, shares: []string{strings.TrimPrefix(Share, "/")},
		out: w, bin: bin, password: secret(), users: map[string]string{}, shareIDs: map[string]string{}}
	var err error
	if s.log, err = os.Create(filepath.Join(out, "dfs.log")); err != nil {
		return nil, err
	}
	if err = s.install(ctx); err == nil {
		err = s.run(ctx)
	}
	if err == nil {
		err = s.configure(ctx)
	}
	if err != nil {
		_ = s.Stop()
		return nil, err
	}
	return s, nil
}

// install writes the installation's bootstrap and storage key under s.data,
// runs dfs init over them, and seeds the namespace the share is in.
func (s *Server) install(ctx context.Context) error {
	s.home = filepath.Join(s.data, "home")
	journal := filepath.Join(s.data, "journal")
	for _, dir := range []string{s.home, journal} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	s.passphrase = secret()
	key, err := keyprovider.GenerateKeyFile(s.passphrase)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(s.data, "storage.key"), key, 0o600); err != nil {
		return err
	}
	s.node = uuid.New()
	s.config = filepath.Join(s.data, "config.yaml")
	// decision: dfs logs to standard error, which dfs.log keeps, since the
	// API's bound address is read from that output.
	config := fmt.Sprintf(`logging: { output: stderr }
node: { id: %s }
metadata: { badger: { path: %q } }
journals: [ { path: %q } ]
keys: { storage: { kind: local, local: { file: %q } } }
api: { listen: ['127.0.0.1:0'] }
metrics: { listen: '127.0.0.1:0' }
adapters: { listen: [%q] }
`, uuid.UUID(s.node), filepath.Join(s.data, "store"), journal, filepath.Join(s.data, "storage.key"), s.server)
	if err := os.WriteFile(s.config, []byte(config), 0o600); err != nil {
		return err
	}
	if err := s.initialise(ctx); err != nil {
		return err
	}
	return s.seed(ctx)
}

// initialise runs dfs init over the installation, with the administrator's
// password in passwordEnv, its output in dfs.log.
func (s *Server) initialise(ctx context.Context) error {
	argv := []string{filepath.Join(s.bin, "dfs"), "init", "--config", s.config, "--admin-password-env", passwordEnv}
	_, _ = fmt.Fprintf(s.out, "[dt] fixture: $ %s\n", strings.Join(argv, " "))
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(s.env(), passwordEnv+"="+s.password)
	cmd.Stdout, cmd.Stderr = s.log, s.log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("dfs init: %w; its output is %s", err, s.log.Name())
	}
	return nil
}

// seed writes the namespace the share is in through the metadata store,
// before the node opens it, with one setting at its remote store's scope.
//
// decision: the namespace is written through the store, not the API, which
// refuses a namespace's write until the settings table is wired. A namespace
// must name a remote store, and the node's reconcile refuses a share whose
// namespace's store holds no setting at its scope, so the store gets an
// endpoint that nothing dials, as a memory profile reads no block from it.
// Move both onto the API, with a real store for an S3 profile, once those
// writes are accepted.
func (s *Server) seed(ctx context.Context) (err error) {
	db, err := badgerkv.Open(badgerkv.Options{Dir: filepath.Join(s.data, "store"), NodeID: entity.NodeID(s.node)})
	if err != nil {
		return fmt.Errorf("opening the metadata store to seed it: %w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	st, err := kvstore.Open(ctx, db)
	if err != nil {
		return fmt.Errorf("opening the metadata store to seed it: %w", err)
	}
	cp := st.ControlPlane()
	store := uuid.New()
	endpoint := entity.Setting{Scope: entity.SettingScope{Kind: entity.ScopeRemote, ID: store}, Key: "endpoint", Value: []byte("https://store.invalid")}
	if _, err := cp.PutSetting(ctx, endpoint, 0); err != nil {
		return fmt.Errorf("seeding the remote store: %w", err)
	}
	id := entity.NewNamespaceID()
	if _, err := cp.PutNamespace(ctx, entity.Namespace{ID: id, Store: store}, 0); err != nil {
		return fmt.Errorf("seeding the namespace: %w", err)
	}
	s.namespace = uuid.UUID(id).String()
	return nil
}

// env is a child's environment: dt's, without the server's own variables,
// with its home, XDG and Windows profile directories under s.home, and the
// storage key's passphrase.
func (s *Server) env() []string {
	// decision: the fixture's server is configured by dt's file alone, so a
	// DITTOFS_ variable set for a developer's own server can't move its
	// ports or its stores, and no child reads or writes the user's own
	// configuration.
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "DITTOFS_") || strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "XDG_") ||
			strings.HasPrefix(kv, "LOCALAPPDATA=") || strings.HasPrefix(kv, "APPDATA=") || strings.HasPrefix(kv, "USERPROFILE=")
	})
	return append(env, "HOME="+s.home, "USERPROFILE="+s.home, "LOCALAPPDATA="+filepath.Join(s.home, "local"),
		"APPDATA="+filepath.Join(s.home, "roaming"), "XDG_CONFIG_HOME="+filepath.Join(s.home, "config"),
		"XDG_STATE_HOME="+filepath.Join(s.home, "state"), "XDG_DATA_HOME="+filepath.Join(s.home, "data"),
		"XDG_CACHE_HOME="+filepath.Join(s.home, "cache"), "DITTOFS_ENCRYPTION_PASSPHRASE="+s.passphrase)
}

// run starts dfs start in a process group of its own, reads the API's
// address from its log, waits until the API answers ready, signs in as the
// administrator, and reads what this start decided.
func (s *Server) run(ctx context.Context) error {
	argv := []string{filepath.Join(s.bin, "dfs"), "start", "--config", s.config}
	_, _ = fmt.Fprintf(s.out, "[dt] fixture: $ %s\n", strings.Join(argv, " "))
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = s.env()
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	ownGroup(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	s.cmd, s.exited, s.killed, s.api = cmd, make(chan struct{}), false, ""
	addr := make(chan string, 1)
	go func() {
		s.err = s.copyLog(pipe, addr)
		close(s.exited)
	}()
	select {
	case a := <-addr:
		s.api = "http://" + a
	case <-s.exited:
		return fmt.Errorf("dfs exited before it logged its API's address (%v); its output is %s", s.err, s.log.Name())
	case <-ctx.Done():
		return ctx.Err()
	}
	_, _ = fmt.Fprintf(s.out, "[dt] fixture: dfs started, pid %d, API at %s, output in %s\n", cmd.Process.Pid, s.api, s.log.Name())
	if err := s.waitReady(ctx); err != nil {
		return err
	}
	if err := s.signIn(ctx); err != nil {
		return err
	}
	return s.discover(ctx)
}

// copyLog copies dfs's output into dfs.log until it ends, sending the API's
// address on addr once it is logged, and returns what cmd's Wait returns. A
// line of any length is read whole, and the output is drained to its end, so
// dfs never blocks writing its log.
func (s *Server) copyLog(pipe io.Reader, addr chan<- string) error {
	r := bufio.NewReader(pipe)
	for {
		line, err := r.ReadString('\n')
		_, _ = io.WriteString(s.log, line)
		if m := apiListening.FindStringSubmatch(line); m != nil {
			addr <- m[1]
			break
		}
		if err != nil {
			break
		}
	}
	_, _ = io.Copy(s.log, r)
	return s.cmd.Wait()
}

// waitReady polls /health/ready until it answers ready, or dfs exits, or
// ctx ends.
func (s *Server) waitReady(ctx context.Context) error {
	for {
		got, err := ready(ctx, s.api)
		if got == "ready" {
			return nil
		}
		select {
		case <-s.exited:
			return fmt.Errorf("dfs exited before its API answered ready (%v, last %q, %v); its output is %s", s.err, got, err, s.log.Name())
		case <-ctx.Done():
			return fmt.Errorf("the API never answered ready (last %q, %v): %w", got, err, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// ready asks the API's readiness probe once, and returns its word.
func ready(ctx context.Context, api string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api+"/health/ready", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(b)), err
}

// signIn signs the administrator in and keeps a client carrying its token.
func (s *Server) signIn(ctx context.Context) error {
	bare, err := client.New(s.api)
	if err != nil {
		return err
	}
	s.session = &session{c: bare, password: s.password}
	if _, err := s.session.Refresh(ctx, ""); err != nil {
		return err
	}
	s.client, err = client.New(s.api, client.WithTokenSource(s.session))
	return err
}

// discover reads what a start decides that the placeholders carry: each
// adapter's bound port, which a restart can move.
//
// ponytail: the node's adapter status names no port and no fixture that
// serves a protocol starts (Unserved), so nothing is read yet and the ports
// stay unset. Read each adapter's port from its status here once it names
// one.
func (s *Server) discover(context.Context) error {
	s.nfsPort, s.smbPort = 0, 0
	return nil
}

// session is the administrator's token, signed in again when the server
// refuses it.
type session struct {
	c        *client.Client
	password string
	mu       sync.Mutex
	access   string
	signIns  int
}

func (t *session) Token(context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.access, nil
}

func (t *session) Refresh(ctx context.Context, _ string) (string, error) {
	tk, err := t.c.SignIn(ctx, "admin", []byte(t.password))
	if err != nil {
		return "", fmt.Errorf("signing in as admin: %w", err)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.access = tk.Access
	t.signIns++
	return tk.Access, nil
}

// configure creates the users User and User2, with their UIDs, and each of
// the fixture's shares with its grants.
func (s *Server) configure(ctx context.Context) error {
	for _, u := range []apiv1.UserSpec{{Name: User, UID: new(UserUID)}, {Name: User2, UID: new(User2UID)}} {
		res, err := s.client.Users().Create(ctx, u)
		if err != nil {
			return fmt.Errorf("creating the user %s: %w", u.Name, err)
		}
		if res.Resource == nil {
			return fmt.Errorf("creating the user %s started an operation", u.Name)
		}
		s.users[u.Name] = res.Resource.ID
	}
	for _, name := range s.shares {
		if err := s.addShare(ctx, name); err != nil {
			return err
		}
	}
	return nil
}

// grants is the access each fixture user holds on every share of fixture f,
// by the user's name, as RFC 29 §4.2 gives it: on a shares file's shares,
// {user} holds admin; on the default share, {user} holds read-write when the
// fixture mounts it over SMB, since the mount is {user}'s; otherwise no
// user, {user2} included, holds a grant.
func grants(f registry.Fixture) map[string]string {
	switch {
	case f.Shares != "":
		return map[string]string{User: "admin"}
	case f.SMB.Mount:
		return map[string]string{User: "read-write"}
	}
	return map[string]string{}
}

// addShare creates the share name, admitting every client with the
// fixture's squash, puts its grants, and waits until the node applied it.
func (s *Server) addShare(ctx context.Context, name string) error {
	sh, err := s.client.CreateShare(ctx, apiv1.NewShare{
		Spec:         apiv1.ShareSpec{Name: name, Path: "/" + name, Namespace: s.namespace},
		ExportPolicy: apiv1.ExportPolicySpec{Flavors: []string{"sys"}, Squash: s.f.Squash, Clients: []apiv1.ClientRule{}},
	})
	if err != nil {
		return fmt.Errorf("creating the share %s: %w", name, err)
	}
	s.shareIDs[name] = sh.ID
	for user, access := range grants(s.f) {
		if _, err := s.client.Grants(sh.ID).Put(ctx, s.users[user], apiv1.GrantSpec{Access: access}, "*"); err != nil {
			return fmt.Errorf("granting %s %s on the share %s: %w", user, access, name, err)
		}
	}
	return s.waitApplied(ctx, name)
}

// waitApplied polls the share name until every node reporting it applied it
// and none refused it, or ctx ends.
func (s *Server) waitApplied(ctx context.Context, name string) error {
	for {
		sh, err := s.client.Shares().Get(ctx, s.shareIDs[name])
		if err == nil && applied(sh.Status) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the share %s was not applied (status %+v, %v): %w", name, sh.Status, err, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// applied reports whether a share's status has it applied on every node
// reporting it, at least one, and refused by none.
func applied(st apiv1.ShareStatus) bool {
	return st.Applied != nil && st.Applied.Of > 0 && st.Applied.Nodes == st.Applied.Of && st.Refused == 0
}

// Reset deletes the share a test runs on, its {share}, and creates it again
// with its grants.
//
// decision: Reset waits for the delete's answer, not for the deletion's
// operation to end, since a share deletion's last step waits for the journal
// to forget the share's tag, which it cannot yet, so the operation never
// ends. The share leaves the list and frees its name once the delete is
// answered, but the old share stays removing, with its grants, and the new
// one is created on the same namespace, which races that deletion once the
// share holds files. A fixture must therefore serve no files through a reset
// share until a share's deletion ends, and each reset then gets a namespace
// of its own, through the API once the settings table is wired.
func (s *Server) Reset(ctx context.Context, name string) error {
	id, ok := s.shareIDs[name]
	if !ok {
		return fmt.Errorf("the fixture has no share %s", name)
	}
	// A share already removing, by an earlier Reset whose recreate failed or
	// by the test itself, is not deleted again: the store refuses that as
	// stale, which would fail every later reset of the cell.
	if id != "" {
		tagged, err := s.client.Shares().Get(ctx, id)
		if err == nil && tagged.Status.State != "removing" {
			_, err = s.client.Shares().Delete(ctx, id, tagged.ETag)
		}
		if err != nil {
			return fmt.Errorf("deleting the share %s: %w", name, err)
		}
		s.shareIDs[name] = ""
	}
	if err := s.addShare(ctx, name); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(s.out, "[dt] fixture: share %s recreated\n", name)
	return nil
}

// Placeholders returns what the server gives a cell's commands, by name, as
// its current start decided them.
func (s *Server) Placeholders() map[string]string {
	vals := map[string]string{"server": s.server, "share": s.shares[0], "user": User, "user2": User2, "password": ""}
	if s.nfsPort > 0 {
		vals["nfs-port"] = strconv.Itoa(s.nfsPort)
	}
	if s.smbPort > 0 {
		vals["smb-port"] = strconv.Itoa(s.smbPort)
	}
	if s.mount != "" {
		vals["mount"] = s.mount
	}
	return vals
}

// Kill ends dfs's group with SIGKILL, leaving its installation for Restart.
// It returns an error, and leaves dfs not restartable, when dfs had already
// exited on its own: a crash it would otherwise hide.
func (s *Server) Kill() error {
	select {
	case <-s.exited:
		return fmt.Errorf("dfs exited on its own before Kill (%v); its output is %s", s.err, s.log.Name())
	default:
	}
	stopGroup(s.cmd, true)
	<-s.exited
	s.killed = true
	return nil
}

// Restart starts dfs again over the installation a Kill left, and signs in
// again. It refuses a dfs that Kill didn't end, or a server Stop ended, so it
// never leaves a dfs that nothing stops.
func (s *Server) Restart(ctx context.Context) error {
	if !s.killed || s.stopDone {
		return errors.New("Restart follows a Kill, before Stop")
	}
	return s.run(ctx)
}

// Stop stops the process group dt started dfs in, and nothing outside it,
// and removes the server's data; its output stays. dfs gets SIGTERM and
// stopGrace to exit before SIGKILL. Its error is stopResult's.
func (s *Server) Stop() error {
	s.stop.Do(func() {
		s.stopDone = true
		defer func() {
			_ = s.log.Close()
			_ = os.RemoveAll(s.data)
		}()
		if s.cmd == nil {
			return
		}
		early := false
		select {
		case <-s.exited:
			early = !s.killed
		default:
			stopGroup(s.cmd, false)
			select {
			case <-s.exited:
			case <-time.After(stopGrace):
				stopGroup(s.cmd, true)
				<-s.exited
			}
		}
		if s.killed {
			return
		}
		s.stopErr = stopResult(early, s.err, s.log.Name())
	})
	return s.stopErr
}

// stopResult is Stop's answer for a dfs that exited before Stop, early, or
// on Stop's SIGTERM with Wait's error waitErr: an error when dfs ended before
// its cell did, the server crashing under it, or didn't exit zero on the
// signal; nil otherwise. log names dfs's output.
func stopResult(early bool, waitErr error, log string) error {
	switch {
	case early:
		return fmt.Errorf("dfs exited before its cell ended (%v); its output is %s", waitErr, log)
	case waitErr != nil:
		return fmt.Errorf("dfs did not stop cleanly on SIGTERM (%v); its output is %s", waitErr, log)
	}
	return nil
}
