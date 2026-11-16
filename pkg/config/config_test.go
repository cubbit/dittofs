package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

func TestLoad_DefaultConfig(t *testing.T) {
	// Create a temporary config file
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	// Write minimal config with new structure
	configContent := `
logging:
  level: "INFO"

blockstore:
  journal:
    path: /var/lib/dittofs/blocks
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	// Load config
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// Verify defaults were applied
	if cfg.Logging.Format != "text" {
		t.Errorf("Expected default format 'text', got %q", cfg.Logging.Format)
	}
	if cfg.Logging.Output != GetDefaultLogPath() {
		t.Errorf("Expected default output %q, got %q", GetDefaultLogPath(), cfg.Logging.Output)
	}
	if cfg.Blockstore.Journal.Path != "/var/lib/dittofs/blocks" {
		t.Errorf("Expected journal path /var/lib/dittofs/blocks, got %q", cfg.Blockstore.Journal.Path)
	}
}

func TestLoad_NoConfigFile(t *testing.T) {
	// Loading with no config file returns a valid default config.
	// This allows users to run the server without a config file for quick testing.
	tmpDir := t.TempDir()
	nonExistentPath := filepath.Join(tmpDir, "nonexistent.yaml")

	cfg, err := Load(nonExistentPath)
	if err != nil {
		t.Fatalf("Expected no error when loading default config, got: %v", err)
	}

	// Verify default config is returned
	if cfg == nil {
		t.Fatal("Expected default config to be returned")
	}

	if cfg.Logging.Level != "INFO" {
		t.Errorf("Expected default log level 'INFO', got %q", cfg.Logging.Level)
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "invalid.yaml")

	// Write invalid YAML
	configContent := `
logging:
  level: INFO
  invalid yaml here [[[
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	// Should return error
	_, err := Load(configPath)
	if err == nil {
		t.Fatal("Expected error with invalid YAML, got nil")
	}
}

func TestLoad_TOML(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")

	configContent := `
[logging]
level = "WARN"
format = "json"
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Failed to load TOML config: %v", err)
	}

	if cfg.Logging.Level != "WARN" {
		t.Errorf("Expected level 'WARN', got %q", cfg.Logging.Level)
	}
	if cfg.Logging.Format != "json" {
		t.Errorf("Expected format 'json', got %q", cfg.Logging.Format)
	}
}

func TestLoad_IgnoresUnknownKey(t *testing.T) {
	// An unknown top-level key (a typo or a stale section from a removed
	// config tree like the deleted `lock:`/`syncer:` blocks or a `cache:`
	// block) must NOT hard-fail boot — it is logged as a warning and
	// ignored, so an otherwise-valid config from an older version still
	// loads (upgrade safety).
	cases := map[string]string{
		"cache":  "cache:\n  size: 100\n",
		"lock":   "lock:\n  lease_break_timeout: 5s\n",
		"syncer": "syncer:\n  upload_concurrency: 4\n",
		"typo":   "loging:\n  level: DEBUG\n",
		"v1":     "database:\n  type: sqlite\ncontrolplane:\n  port: 8080\n",
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			tmpDir := t.TempDir()
			configPath := filepath.Join(tmpDir, "config.yaml")
			content := "logging:\n  level: INFO\n  format: text\n  output: stdout\n" + extra
			if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
				t.Fatalf("write config: %v", err)
			}
			if _, err := Load(configPath); err != nil {
				t.Fatalf("Load must tolerate unknown key %q (warn, not fail), got %v", name, err)
			}
		})
	}
}

func TestGetDefaultConfig(t *testing.T) {
	cfg := GetDefaultConfig()

	// Verify all defaults are set
	if cfg.Logging.Level != "INFO" {
		t.Errorf("Expected default log level 'INFO', got %q", cfg.Logging.Level)
	}
	if cfg.Logging.Format != "text" {
		t.Errorf("Expected default log format 'text', got %q", cfg.Logging.Format)
	}
	if cfg.Logging.Output != GetDefaultLogPath() {
		t.Errorf("Expected default log output %q, got %q", GetDefaultLogPath(), cfg.Logging.Output)
	}
	if !filepath.IsAbs(cfg.Blockstore.Journal.Path) {
		t.Errorf("Expected an absolute default journal path, got %q", cfg.Blockstore.Journal.Path)
	}
}

func TestGetDefaultConfigPath(t *testing.T) {
	path := GetDefaultConfigPath()

	// Should contain dittofs and config.yaml
	if !filepath.IsAbs(path) {
		t.Errorf("Expected absolute path, got %q", path)
	}
	if filepath.Base(path) != "config.yaml" {
		t.Errorf("Expected filename 'config.yaml', got %q", filepath.Base(path))
	}
}

func TestGetConfigDir(t *testing.T) {
	dir := getConfigDir()

	// Should contain dittofs
	if filepath.Base(dir) != "dittofs" {
		t.Errorf("Expected directory name 'dittofs', got %q", filepath.Base(dir))
	}
}

func TestGetConfigDir_PlatformEnvVars(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Run("UsesAPPDATA", func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("APPDATA", tmpDir)

			dir := getConfigDir()
			expected := filepath.Join(tmpDir, "dittofs")
			if dir != expected {
				t.Errorf("getConfigDir() = %q, expected %q", dir, expected)
			}
		})

		t.Run("FallbackWithoutAPPDATA", func(t *testing.T) {
			t.Setenv("APPDATA", "")

			dir := getConfigDir()
			// Should contain "AppData/Roaming/dittofs" or "dittofs" at minimum
			if filepath.Base(dir) != "dittofs" {
				t.Errorf("getConfigDir() = %q, expected directory name 'dittofs'", dir)
			}
		})
	} else {
		t.Run("UsesXDGConfigHome", func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", tmpDir)

			dir := getConfigDir()
			expected := filepath.Join(tmpDir, "dittofs")
			if dir != expected {
				t.Errorf("getConfigDir() = %q, expected %q", dir, expected)
			}
		})

		t.Run("FallbackWithoutXDG", func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", "")

			dir := getConfigDir()
			// Should end with .config/dittofs
			if filepath.Base(dir) != "dittofs" {
				t.Errorf("getConfigDir() = %q, expected directory name 'dittofs'", dir)
			}
			parent := filepath.Base(filepath.Dir(dir))
			if parent != ".config" {
				t.Errorf("parent dir = %q, expected '.config'", parent)
			}
		})
	}
}

func TestLoad_EnvironmentVariables(t *testing.T) {
	// Set environment variables
	t.Setenv("DITTOFS_LOGGING_LEVEL", "ERROR")
	t.Setenv("DITTOFS_BLOCKSTORE_JOURNAL_PATH", "/srv/blocks")

	// Create minimal config file
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	configContent := `
logging:
  level: "INFO"

blockstore:
  journal:
    path: /var/lib/dittofs/blocks
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// Verify environment variables override config file
	if cfg.Logging.Level != "ERROR" {
		t.Errorf("Expected level 'ERROR' from env var, got %q", cfg.Logging.Level)
	}
	if cfg.Blockstore.Journal.Path != "/srv/blocks" {
		t.Errorf("Expected journal path /srv/blocks from env var, got %q", cfg.Blockstore.Journal.Path)
	}
}

// Load never reads the node's identity or its devices, which the host sets:
// a file naming them leaves both zero.
func TestLoad_LeavesNodeAndDevicesToTheHost(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	content := "logging:\n  level: INFO\n  format: text\n  output: stdout\nnode: [1, 2, 3]\ndevices:\n  - /dev/one\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Node != (entity.NodeID{}) {
		t.Errorf("Node after Load: got %x, want zero", cfg.Node)
	}
	if cfg.Devices != nil {
		t.Errorf("Devices after Load: got %q, want nil", cfg.Devices)
	}
}
