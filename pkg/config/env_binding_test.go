package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// writeConfigFile writes content to a temp config.yaml and returns its path.
func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// setConfigDirEnv points the environment variable getConfigDir() reads at
// tmpDir: APPDATA on Windows, XDG_CONFIG_HOME elsewhere.
func setConfigDirEnv(t *testing.T, tmpDir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Setenv("APPDATA", tmpDir)
	} else {
		t.Setenv("XDG_CONFIG_HOME", tmpDir)
	}
}

// TestLoad_EnvOverride_KeyAbsentFromFile verifies that an env var overrides a
// key that is NOT present in the config file. The documented precedence is
// env > file > defaults, which only holds if every key is bound for
// AutomaticEnv.
func TestLoad_EnvOverride_KeyAbsentFromFile(t *testing.T) {
	path := writeConfigFile(t, "logging:\n  format: text\n")
	journal := filepath.Join(t.TempDir(), "blocks")
	t.Setenv("DITTOFS_LOGGING_LEVEL", "ERROR")
	t.Setenv("DITTOFS_BLOCKSTORE_JOURNAL_PATH", journal)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Logging.Level != "ERROR" {
		t.Errorf("logging.level: env override dropped, got %q want ERROR", cfg.Logging.Level)
	}
	if cfg.Blockstore.Journal.Path != journal {
		t.Errorf("blockstore.journal.path: env override dropped, got %q want %q", cfg.Blockstore.Journal.Path, journal)
	}
}

// TestConfigEnvKeys_CoversKnownKeys asserts the reflective key walk produces
// the nested key paths, so a refactor that drops a namespace from the BindEnv
// walk (which would silently drop its env overrides) fails here.
func TestConfigEnvKeys_CoversKnownKeys(t *testing.T) {
	keys := configEnvKeys()
	set := make(map[string]bool, len(keys))
	for _, k := range keys {
		set[k] = true
	}
	for _, k := range []string{"logging.level", "logging.rotation.max_size", "blockstore.journal.path"} {
		if !set[k] {
			t.Errorf("configEnvKeys() missing %q (got %v)", k, keys)
		}
	}
}

// TestLoad_EnvOnly_NoConfigFile verifies that env-var overrides are honoured
// even when NO config file exists on disk, the container/CI deployment case.
func TestLoad_EnvOnly_NoConfigFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	t.Setenv("DITTOFS_LOGGING_LEVEL", "ERROR")

	cfg, err := Load(missing)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Logging.Level != "ERROR" {
		t.Errorf("logging.level: env override dropped on no-file path, got %q want ERROR", cfg.Logging.Level)
	}
}

// TestLoad_NoFileNoEnv_StillDefaults guards the plain no-file/no-env path:
// defaults must still be applied for every field the env doesn't set.
func TestLoad_NoFileNoEnv_StillDefaults(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")

	cfg, err := Load(missing)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Logging.Level != "INFO" {
		t.Errorf("default logging.level: got %q want INFO", cfg.Logging.Level)
	}
	if !filepath.IsAbs(cfg.Blockstore.Journal.Path) {
		t.Errorf("default blockstore.journal.path: got %q, want an absolute path", cfg.Blockstore.Journal.Path)
	}
}
