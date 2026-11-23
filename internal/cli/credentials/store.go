// Package credentials keeps dfsctl's contexts: for each named context, the
// server it reaches, the account signed in and the tokens it holds.
package credentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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

// Store is the credentials file at one path, as it was read.
type Store struct {
	configPath string
	config     *Config
}

// NewStoreAt reads the credentials file at path; a missing file is an empty
// store. A file a context of which still says tls_skip_verify is refused:
// ignoring the field would start verifying a server its user chose not to.
func NewStoreAt(path string) (*Store, error) {
	s := &Store{configPath: path, config: &Config{Contexts: map[string]*Context{}}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, s.config); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
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
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for name, c := range old.Contexts {
		if c.TLSSkipVerify {
			return nil, fmt.Errorf("%s: context %q holds tls_skip_verify, which dfsctl no longer honours; remove the field and log in again with --cacert naming the server's CA", path, name)
		}
	}
	return s, nil
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
// owner only, and renames that into place, so a reader never sees half a
// file. The directory is created 0700 when missing.
func (s *Store) save() error {
	dir := filepath.Dir(s.configPath)
	if err := os.MkdirAll(dir, DirPermissions); err != nil {
		return fmt.Errorf("cannot create config directory: %w", err)
	}
	data, err := json.MarshalIndent(s.config, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(s.configPath)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(FilePermissions)
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
	return os.Rename(tmp, s.configPath)
}

// GetCurrentContext returns the current context.
func (s *Store) GetCurrentContext() (*Context, error) {
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
func (s *Store) GetCurrentContextName() string {
	return s.config.CurrentContext
}

// GetContext returns a specific context by name.
func (s *Store) GetContext(name string) (*Context, error) {
	ctx, ok := s.config.Contexts[name]
	if !ok {
		return nil, ErrContextNotFound
	}
	return ctx, nil
}

// ListContexts returns all context names.
func (s *Store) ListContexts() []string {
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
