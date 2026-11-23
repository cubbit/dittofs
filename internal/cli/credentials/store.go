// Package credentials keeps dfsctl's contexts: for each named context, the
// server it reaches, the account signed in and the tokens it holds.
package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const (
	// DefaultConfigDir is the default directory for dfsctl configuration.
	DefaultConfigDir = "dfsctl"
	// ConfigFileName is the name of the configuration file.
	ConfigFileName = "config.json"
	// FilePermissions for config files (read/write for owner only).
	FilePermissions = 0600
	// DirPermissions for config directories.
	DirPermissions = 0700
)

var (
	// ErrNoCurrentContext indicates no context is currently set.
	ErrNoCurrentContext = errors.New("no current context set")
	// ErrContextNotFound indicates the requested context doesn't exist.
	ErrContextNotFound = errors.New("context not found")
	// ErrNotLoggedIn indicates no valid credentials exist.
	ErrNotLoggedIn = errors.New("not logged in - run 'dfsctl login' first")
)

// Context is one server and the account signed in to it.
type Context struct {
	// ServerURL is an https URL, an http URL on a loopback address, or the
	// path of a node's local socket.
	ServerURL string `json:"server_url"`
	Username  string `json:"username,omitempty"`
	// AccessToken is the token every call carries: a session's access token,
	// or an API token, which has no RefreshToken.
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	// CACert is the path of a PEM bundle of the CAs the server's certificate
	// is checked against, re-read on every run so a rotated bundle is picked
	// up; empty for the system's roots.
	CACert string `json:"ca_cert,omitempty"`
}

// Config represents the complete dfsctl configuration.
type Config struct {
	CurrentContext string              `json:"current_context"`
	Contexts       map[string]*Context `json:"contexts"`
}

// Snapshot is the credentials file as one read found it. It has no method
// that writes: a change goes through Update, under the file's lock.
type Snapshot struct {
	config *Config
}

// Store is the credentials file read under its lock, which Update holds
// while its function runs; every change is saved before it returns.
type Store struct {
	Snapshot
	configPath string
}

// Read reads the credentials file at path without taking its lock; a
// missing file is an empty one. A file a context of which still says
// tls_skip_verify is refused: ignoring the field would start verifying a
// server its user chose not to.
func Read(path string) (Snapshot, error) {
	s := Snapshot{config: &Config{Contexts: map[string]*Context{}}}
	data, err := readFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return Snapshot{}, err
	}
	if err := json.Unmarshal(data, s.config); err != nil {
		return Snapshot{}, fmt.Errorf("%s: %w", path, err)
	}
	if s.config.Contexts == nil {
		s.config.Contexts = map[string]*Context{}
	}
	var old struct {
		Contexts map[string]struct {
			TLSSkipVerify bool `json:"tls_skip_verify"`
		} `json:"contexts"`
	}
	if err := json.Unmarshal(data, &old); err != nil {
		return Snapshot{}, fmt.Errorf("%s: %w", path, err)
	}
	// decision: the whole file is refused, not only the context holding
	// tls_skip_verify, so dfsctl cannot repair it either: context remove and
	// a login replacing that context are refused too, and the error names
	// the field to delete by hand. Letting them through would mean keeping
	// that context's raw record across every other save; do that if editing
	// the file by hand proves to be a burden in practice.
	for name, c := range old.Contexts {
		if c.TLSSkipVerify {
			return Snapshot{}, fmt.Errorf("%s: context %q holds tls_skip_verify, which dfsctl no longer honours; remove the field and log in again with --cacert naming the server's CA", path, name)
		}
	}
	return s, nil
}

// lockPoll is how long Update pauses before it tries a held lock again.
const lockPoll = 50 * time.Millisecond

// Update takes the lock on <path>.lock, reads the file at path and runs fn
// on it, holding the lock until fn returns. Every change to the file goes
// through it, so two commands never read the same refresh token and both
// present it. While another process holds the lock it calls waiting once,
// naming the lock file, and tries again until the lock is free or ctx ends.
// The lock is the operating system's, released when its holder exits,
// however it exits; the lock file itself stays.
func Update(ctx context.Context, path string, waiting func(lock string), fn func(*Store) error) error {
	lock := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(lock), DirPermissions); err != nil {
		return err
	}
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, FilePermissions)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	for told := false; ; told = true {
		held, err := tryLock(f)
		if err != nil {
			return fmt.Errorf("lock %s: %w", lock, err)
		}
		if !held {
			break
		}
		if !told && waiting != nil {
			waiting(lock)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s: %w", lock, ctx.Err())
		case <-time.After(lockPoll):
		}
	}
	snap, err := Read(path)
	if err != nil {
		return err
	}
	return fn(&Store{Snapshot: snap, configPath: path})
}

// DefaultPath is the credentials file's path: config.json under dfsctl in
// the user's configuration directory, %APPDATA% on Windows and
// $XDG_CONFIG_HOME or ~/.config elsewhere.
func DefaultPath() (string, error) {
	var configHome string

	if runtime.GOOS == "windows" {
		configHome = os.Getenv("APPDATA")
		if configHome == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("cannot determine home directory: %w", err)
			}
			configHome = filepath.Join(home, "AppData", "Roaming")
		}
	} else {
		configHome = os.Getenv("XDG_CONFIG_HOME")
		if configHome == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("cannot determine home directory: %w", err)
			}
			configHome = filepath.Join(home, ".config")
		}
	}

	return filepath.Join(configHome, DefaultConfigDir, ConfigFileName), nil
}

// save writes the config to a temporary file beside it, readable by its
// owner only, renames that into place, so a reader never sees half a file,
// and syncs the directory, so the rename survives a crash. The directory is
// kept 0700, a wider existing one narrowed, and on Windows both carry a DACL
// granting only the current user.
func (s *Store) save() error {
	dir := filepath.Dir(s.configPath)
	if err := os.MkdirAll(dir, DirPermissions); err != nil {
		return fmt.Errorf("cannot create config directory: %w", err)
	}
	data, err := json.MarshalIndent(s.config, "", "  ")
	if err != nil {
		return err
	}
	if err := restrict(dir, true); err != nil {
		return fmt.Errorf("restrict %s to its owner: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(s.configPath)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	err = f.Chmod(FilePermissions)
	if err == nil {
		err = restrict(tmp, false)
	}
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, s.configPath); err != nil {
		return err
	}
	return syncDir(dir)
}

// GetCurrentContext returns the current context.
func (s Snapshot) GetCurrentContext() (*Context, error) {
	if s.config.CurrentContext == "" {
		return nil, ErrNoCurrentContext
	}

	ctx, ok := s.config.Contexts[s.config.CurrentContext]
	if !ok {
		return nil, ErrContextNotFound
	}

	return ctx, nil
}

// GetCurrentContextName returns the name of the current context.
func (s Snapshot) GetCurrentContextName() string {
	return s.config.CurrentContext
}

// GetContext returns a specific context by name.
func (s Snapshot) GetContext(name string) (*Context, error) {
	ctx, ok := s.config.Contexts[name]
	if !ok {
		return nil, ErrContextNotFound
	}
	return ctx, nil
}

// ListContexts returns all context names.
func (s Snapshot) ListContexts() []string {
	names := make([]string, 0, len(s.config.Contexts))
	for name := range s.config.Contexts {
		names = append(names, name)
	}
	return names
}

// SetContext creates or replaces the context name, makes it the current
// one, and saves.
func (s *Store) SetContext(name string, ctx *Context) error {
	s.config.Contexts[name] = ctx
	s.config.CurrentContext = name
	return s.save()
}

// UseContext switches to a different context.
func (s *Store) UseContext(name string) error {
	if _, ok := s.config.Contexts[name]; !ok {
		return ErrContextNotFound
	}
	s.config.CurrentContext = name
	return s.save()
}

// RenameContext renames a context.
func (s *Store) RenameContext(oldName, newName string) error {
	ctx, ok := s.config.Contexts[oldName]
	if !ok {
		return ErrContextNotFound
	}

	delete(s.config.Contexts, oldName)
	s.config.Contexts[newName] = ctx

	if s.config.CurrentContext == oldName {
		s.config.CurrentContext = newName
	}

	return s.save()
}

// DeleteContext removes a context.
func (s *Store) DeleteContext(name string) error {
	if _, ok := s.config.Contexts[name]; !ok {
		return ErrContextNotFound
	}

	delete(s.config.Contexts, name)

	if s.config.CurrentContext == name {
		s.config.CurrentContext = ""
	}

	return s.save()
}

// SetTokens replaces the tokens of the context name and saves.
func (s *Store) SetTokens(name, access, refresh string) error {
	ctx, ok := s.config.Contexts[name]
	if !ok {
		return ErrContextNotFound
	}
	ctx.AccessToken, ctx.RefreshToken = access, refresh
	return s.save()
}
