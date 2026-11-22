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
	"slices"
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

// Start initialises an installation under out/dfs with dfs init from bin,
// starts it there with dfs start, its output in out/dfs.log, and configures
// it through the API as f asks: the users User and User2, the share Share,
// and both users' grants on it. It makes one attempt: when a step fails, or
// ctx ends first, it stops what it started and returns why. w gets dt's
// lines on the setup, and never a credential.
func Start(ctx context.Context, f registry.Fixture, bin, out string, w io.Writer) (*Server, error) {
	if why := unserved(f); why != "" {
		return nil, errors.New(why)
	}
	server, err := address()
	if err != nil {
		return nil, err
	}
	s := &Server{f: f, data: filepath.Join(out, "dfs"), shares: []string{Share}, out: w, bin: bin,
		password: secret(), vals: map[string]string{"server": server, "share": strings.TrimPrefix(Share, "/"), "user": User, "user2": User2}}
	if s.log, err = os.Create(filepath.Join(out, "dfs.log")); err != nil {
		return nil, err
	}
	if err = s.install(ctx, server); err == nil {
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

// unserved says why fixture f can't be set up yet, or returns "".
//
// decision: a fixture that serves NFS or SMB, with its clients or KDC, or holds its blocks in S3, or
// names a shares file, is refused, since the node serves no adapter yet, its
// adapter status names no port a cell could reach, a share's settings are
// written only through the settings API, which refuses them until the
// settings table is wired, and no route sets a user's password. Every such
// fixture's cell would otherwise run against a server answering none of its
// tests. Withdraw each refusal once its part serves.
func unserved(f registry.Fixture) string {
	switch {
	case f.NFS.Serves() || f.SMB.Enabled || f.Client != "" || f.Kerberos:
		return "dt serves no NFS or SMB through the node yet: the node serves no adapter, and its adapter status names no port"
	case f.Profile == "badger-s3":
		return "dt seeds no remote store yet, so it sets up no S3 profile"
	case f.Shares != "":
		return "dt sets no share settings yet, so it reads no shares file"
	}
	return ""
}

// install writes the installation's bootstrap and storage key under s.data,
// runs dfs init over them, and seeds the namespace the share is in.
func (s *Server) install(ctx context.Context, server string) error {
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
	// decision: the server logs at INFO to standard error, which dfs.log
	// keeps: errors, warnings and a panic's trace, and the record naming
	// the API's address, without a line per operation.
	config := fmt.Sprintf(`logging: { level: INFO, output: stderr }
node: { id: %s }
metadata: { badger: { path: %q } }
journals: [ { path: %q } ]
keys: { storage: { kind: local, local: { file: %q } } }
api: { listen: ['127.0.0.1:0'] }
metrics: { listen: '127.0.0.1:0' }
adapters: { listen: [%q] }
blockstore: { journal: { path: %q } }
`, uuid.UUID(s.node), s.store(), journal, filepath.Join(s.data, "storage.key"), server, filepath.Join(s.data, "blocks"))
	if err := os.WriteFile(s.config, []byte(config), 0o600); err != nil {
		return err
	}
	if err := s.initialise(ctx); err != nil {
		return err
	}
	return s.seed(ctx)
}

// store is the metadata store's directory.
func (s *Server) store() string { return filepath.Join(s.data, "store") }

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
// with no remote store, before the node opens it.
//
// decision: the namespace is written through the store, not the API, which
// refuses a namespace's write until the settings table is wired. Move it
// onto the API once that write is accepted.
func (s *Server) seed(ctx context.Context) (err error) {
	db, err := badgerkv.Open(badgerkv.Options{Dir: s.store(), NodeID: entity.NodeID(s.node)})
	if err != nil {
		return fmt.Errorf("opening the metadata store to seed it: %w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	st, err := kvstore.Open(ctx, db)
	if err != nil {
		return fmt.Errorf("opening the metadata store to seed it: %w", err)
	}
	id := entity.NewNamespaceID()
	if _, err := st.ControlPlane().PutNamespace(ctx, entity.Namespace{ID: id}, 0); err != nil {
		return fmt.Errorf("seeding the namespace: %w", err)
	}
	s.namespace = uuid.UUID(id).String()
	return nil
}

// env is a child's environment: dt's, without the server's own variables,
// with its home and XDG directories under s.home, and the storage key's
// passphrase.
func (s *Server) env() []string {
	// decision: the fixture's server is configured by dt's file alone, so a
	// DITTOFS_ variable set for a developer's own server can't move its
	// ports or its stores, and no child reads or writes the user's own
	// configuration.
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "DITTOFS_") || strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "XDG_")
	})
	return append(env, "HOME="+s.home, "XDG_CONFIG_HOME="+filepath.Join(s.home, "config"),
		"XDG_STATE_HOME="+filepath.Join(s.home, "state"), "XDG_DATA_HOME="+filepath.Join(s.home, "data"),
		"XDG_CACHE_HOME="+filepath.Join(s.home, "cache"), "DITTOFS_ENCRYPTION_PASSPHRASE="+s.passphrase)
}

// run starts dfs start in a process group of its own, reads the API's
// address from its log, waits until the API answers ready, and signs in as
// the administrator.
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
	s.cmd, s.exited, s.stopped = cmd, make(chan struct{}), false
	addr := make(chan string, 1)
	go func() {
		lines := bufio.NewScanner(pipe)
		for lines.Scan() {
			_, _ = fmt.Fprintln(s.log, lines.Text())
			if m := apiListening.FindStringSubmatch(lines.Text()); m != nil {
				select {
				case addr <- m[1]:
				default:
				}
			}
		}
		s.err = cmd.Wait()
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
	return s.signIn(ctx)
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
	src := &session{c: bare, password: s.password}
	if src.access, err = src.Refresh(ctx, ""); err != nil {
		return err
	}
	s.client, err = client.New(s.api, client.WithTokenSource(src))
	return err
}

// session is the administrator's token, signed in again when the server
// refuses it.
type session struct {
	c        *client.Client
	password string
	mu       sync.Mutex
	access   string
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
	return tk.Access, nil
}

// configure creates the users User and User2, with their UIDs, and each of
// the fixture's shares with both users' grants on it.
func (s *Server) configure(ctx context.Context) error {
	for i, u := range []apiv1.UserSpec{{Name: User, UID: ptr(UserUID)}, {Name: User2, UID: ptr(User2UID)}} {
		res, err := s.client.Users().Create(ctx, u)
		if err != nil {
			return fmt.Errorf("creating the user %s: %w", u.Name, err)
		}
		if res.Resource == nil {
			return fmt.Errorf("creating the user %s started an operation", u.Name)
		}
		s.users[i] = res.Resource.ID
	}
	for _, name := range s.shares {
		if err := s.addShare(ctx, name); err != nil {
			return err
		}
	}
	return nil
}

func ptr[T any](v T) *T { return &v }

// addShare creates the share name, admitting every client with the
// fixture's squash, and grants both users read-write on it.
func (s *Server) addShare(ctx context.Context, name string) error {
	sh, err := s.client.CreateShare(ctx, apiv1.NewShare{
		Spec:         apiv1.ShareSpec{Name: strings.TrimPrefix(name, "/"), Path: name, Namespace: s.namespace},
		ExportPolicy: apiv1.ExportPolicySpec{Flavors: []string{"sys"}, Squash: s.f.Squash, Clients: []apiv1.ClientRule{}},
	})
	if err != nil {
		return fmt.Errorf("creating the share %s: %w", name, err)
	}
	for _, u := range s.users {
		if _, err := s.client.Grants(sh.ID).Put(ctx, u, apiv1.GrantSpec{Access: "read-write"}, "*"); err != nil {
			return fmt.Errorf("granting %s on the share %s: %w", u, name, err)
		}
	}
	return nil
}

// Reset deletes the share a test runs on, its {share}, and creates it again
// with its grants.
func (s *Server) Reset(name string) error {
	ctx := context.Background()
	var found *apiv1.Resource[apiv1.ShareSpec, apiv1.ShareStatus]
	for sh, err := range s.client.Shares().List(ctx, nil) {
		if err != nil {
			return fmt.Errorf("listing the shares: %w", err)
		}
		if sh.Spec.Name == name {
			found = &sh
			break
		}
	}
	if found == nil {
		return fmt.Errorf("the fixture has no share %s", name)
	}
	op, err := s.client.Shares().Delete(ctx, found.ID, "")
	if err == nil && op != nil {
		var done apiv1.Operation
		if done, err = s.client.Wait(ctx, *op); err == nil && done.State != apiv1.OpSucceeded {
			err = fmt.Errorf("its deletion ended %s", done.State)
		}
	}
	if err != nil {
		return fmt.Errorf("deleting the share %s: %w", name, err)
	}
	if err := s.addShare(ctx, "/"+name); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(s.out, "[dt] fixture: share %s recreated\n", name)
	return nil
}

// Placeholders returns what the server gives a cell's commands, by name.
func (s *Server) Placeholders() map[string]string { return s.vals }

// Kill ends dfs's group with SIGKILL, leaving its installation for Restart.
func (s *Server) Kill() {
	stopGroup(s.cmd, true)
	<-s.exited
	s.stopped = true
}

// Restart starts dfs again over the installation a Kill left, and signs in
// again.
func (s *Server) Restart(ctx context.Context) error { return s.run(ctx) }

// Stop stops the process group dt started dfs in, and nothing outside it,
// and removes the server's data; its output stays. dfs gets SIGTERM and
// stopGrace to exit before SIGKILL. It returns an error when dfs had exited
// before Stop, the server ending before its cell did, or did not exit zero.
func (s *Server) Stop() error {
	s.stop.Do(func() {
		defer func() {
			_ = s.log.Close()
			_ = os.RemoveAll(s.data)
		}()
		if s.cmd == nil {
			return
		}
		select {
		case <-s.exited:
			if !s.stopped {
				s.stopErr = fmt.Errorf("dfs exited before its cell ended (%v); its output is %s", s.err, s.log.Name())
			}
			return
		default:
		}
		stopGroup(s.cmd, false)
		select {
		case <-s.exited:
		case <-time.After(stopGrace):
			stopGroup(s.cmd, true)
			<-s.exited
		}
		if s.err != nil {
			s.stopErr = fmt.Errorf("dfs did not stop cleanly on SIGTERM (%v); its output is %s", s.err, s.log.Name())
		}
	})
	return s.stopErr
}
