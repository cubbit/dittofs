package cmdutil

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cubbit/dittofs/internal/cli/credentials"
	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// Dial returns a client of server, a URL or a local socket's path, trusting
// the CAs in the PEM bundle at cacert when it is set and the system's roots
// otherwise.
func Dial(server, cacert string, opts ...client.Option) (*client.Client, error) {
	if cacert != "" {
		pem, err := os.ReadFile(cacert)
		if err != nil {
			return nil, fmt.Errorf("the CA bundle: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("the CA bundle %s holds no PEM certificate", cacert)
		}
		opts = append(opts, client.WithTLS(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}))
	}
	return client.New(server, opts...)
}

// refreshBound is the longest a refresh may take while it holds the
// credentials file's lock, which every other dfsctl waits on.
const refreshBound = 30 * time.Second

// NewClient returns a client of the current context's server and that
// context's name. Every call carries the context's token; one the server
// refuses 401 is refreshed through the credentials file under Locked.
func NewClient(env Env) (*client.Client, string, error) {
	s, err := credentials.Read(env.ConfigPath)
	if err != nil {
		return nil, "", err
	}
	name := s.GetCurrentContextName()
	cur, err := s.GetCurrentContext()
	if err != nil || cur.AccessToken == "" {
		return nil, "", credentials.ErrNotLoggedIn
	}
	src := &fileTokens{env: env, name: name, dialled: *cur, access: cur.AccessToken}
	c, err := Dial(cur.ServerURL, cur.CACert, client.WithTokenSource(src))
	if err != nil {
		return nil, "", err
	}
	src.c = c
	return c, name, nil
}

// fileTokens is the token source of one context in the credentials file.
type fileTokens struct {
	env  Env
	name string
	c    *client.Client
	// dialled is the context as the client was built from it; only its
	// server, CA bundle and account are compared.
	dialled credentials.Context

	mu     sync.Mutex
	access string
}

func (s *fileTokens) Token(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.access, nil
}

// Refresh takes the credentials file's lock and reads the context again. A
// context that no longer names the server, CA bundle and account the client
// was built for is refused, so no token reaches a server it was not issued
// by. A token there other than failed is another command's refresh, used as
// it is, which only saves this command a refresh of its own; otherwise the
// context's refresh token is exchanged, once and within refreshBound, and
// the new pair saved before the lock is released.
func (s *fileTokens) Refresh(ctx context.Context, failed string) (string, error) {
	err := Locked(ctx, s.env, func(st *credentials.Store) error {
		cur, err := st.GetContext(s.name)
		if err != nil || cur.AccessToken == "" {
			return credentials.ErrNotLoggedIn
		}
		if cur.ServerURL != s.dialled.ServerURL || cur.CACert != s.dialled.CACert || cur.Username != s.dialled.Username {
			return fmt.Errorf("context %q changed since this command started; run it again", s.name)
		}
		if cur.AccessToken != failed {
			s.set(cur.AccessToken)
			return nil
		}
		if cur.RefreshToken == "" {
			return fmt.Errorf("context %q: the server refused its API token; run 'dfsctl login' again", s.name)
		}
		rctx, cancel := context.WithTimeout(ctx, refreshBound)
		defer cancel()
		tk, err := s.c.Refresh(rctx, cur.RefreshToken)
		if err != nil {
			return fmt.Errorf("context %q: refreshing the session: %w; run 'dfsctl login' again", s.name, err)
		}
		s.set(tk.Access)
		return st.SetTokens(s.name, tk.Access, tk.Refresh)
	})
	if err != nil {
		return "", err
	}
	return s.Token(ctx)
}

func (s *fileTokens) set(access string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.access = access
}

// Secrets reads the secrets one invocation needs, each from the file a flag
// names or, without one, from a prompt on env.In, all prompts through one
// reader so a second prompt loses nothing the first read ahead.
type Secrets struct {
	env Env
	in  *bufio.Reader
}

// NewSecrets returns the secret reader of one invocation.
func NewSecrets(env Env) *Secrets { return &Secrets{env: env, in: bufio.NewReader(env.In)} }

// Read returns the secret the file named by cmd's flag holds, its trailing
// line break dropped, or, with the flag unset, the line typed at a prompt
// labelled label, unechoed on a terminal. With no person at env.In it
// refuses, naming the flag.
func (s *Secrets) Read(cmd *cobra.Command, flag, label string) ([]byte, error) {
	if path, _ := cmd.Flags().GetString(flag); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return []byte(strings.TrimRight(string(b), "\r\n")), nil
	}
	return s.Prompt(cmd, flag, label)
}

// Prompt is Read with no file: the line typed at a prompt labelled label.
// On a terminal the input is not echoed, and an interrupt (cmd's context
// ending) restores the terminal's echo before the command returns.
func (s *Secrets) Prompt(cmd *cobra.Command, flag, label string) ([]byte, error) {
	if !s.env.Interactive {
		return nil, fmt.Errorf("%s: no terminal to prompt for the %s; pass --%s", cmd.CommandPath(), label, flag)
	}
	_, _ = fmt.Fprintf(s.env.Err, "%s: ", label)
	if f, ok := s.env.In.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		return readPassword(cmd.Context(), int(f.Fd()), s.env.Err)
	}
	line, err := s.in.ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || line == "") {
		return nil, fmt.Errorf("%s: reading the %s: %w", cmd.CommandPath(), label, err)
	}
	return []byte(strings.TrimRight(line, "\r\n")), nil
}

// readPassword reads a line from the terminal fd with its echo off. It
// returns when ctx ends too, the terminal's state restored, leaving the
// read to end with the process.
func readPassword(ctx context.Context, fd int, out io.Writer) ([]byte, error) {
	state, err := term.GetState(fd)
	if err != nil {
		return nil, err
	}
	type read struct {
		b   []byte
		err error
	}
	done := make(chan read, 1)
	go func() {
		b, err := term.ReadPassword(fd)
		done <- read{b, err}
	}()
	defer func() { _, _ = fmt.Fprintln(out) }()
	select {
	case r := <-done:
		return r.b, r.err
	case <-ctx.Done():
		_ = term.Restore(fd, state)
		return nil, ctx.Err()
	}
}
