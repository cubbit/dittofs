package config

import (
	"testing"
)

func TestApplyDefaults_Logging(t *testing.T) {
	cfg := &Bootstrap{}
	ApplyDefaults(cfg)

	if cfg.Logging.Level != "INFO" {
		t.Errorf("Expected default log level 'INFO', got %q", cfg.Logging.Level)
	}
	if cfg.Logging.Format != "text" {
		t.Errorf("Expected default log format 'text', got %q", cfg.Logging.Format)
	}
	if cfg.Logging.Output != GetDefaultLogPath() {
		t.Errorf("Expected default log output %q, got %q", GetDefaultLogPath(), cfg.Logging.Output)
	}
}

func TestApplyDefaults_RotationDefaults(t *testing.T) {
	cfg := &Bootstrap{}
	ApplyDefaults(cfg)

	if cfg.Logging.Rotation.MaxSize != 100 {
		t.Errorf("Expected default MaxSize 100, got %d", cfg.Logging.Rotation.MaxSize)
	}
	// MaxBackups and MaxAge should remain 0 (unlimited) when not set,
	// because 0 has meaning in lumberjack (keep all / no age limit).
	if cfg.Logging.Rotation.MaxBackups != 0 {
		t.Errorf("Expected MaxBackups 0 (keep all) when not set, got %d", cfg.Logging.Rotation.MaxBackups)
	}
	if cfg.Logging.Rotation.MaxAge != 0 {
		t.Errorf("Expected MaxAge 0 (no limit) when not set, got %d", cfg.Logging.Rotation.MaxAge)
	}
}

func TestApplyDefaults_PreservesZeroRotationValues(t *testing.T) {
	cfg := &Bootstrap{
		Logging: LoggingConfig{
			Rotation: LogRotationConfig{
				MaxSize:    50,
				MaxBackups: 0, // explicitly: keep all
				MaxAge:     0, // explicitly: no age limit
			},
		},
	}
	ApplyDefaults(cfg)

	if cfg.Logging.Rotation.MaxSize != 50 {
		t.Errorf("Expected explicit MaxSize 50 to be preserved, got %d", cfg.Logging.Rotation.MaxSize)
	}
	if cfg.Logging.Rotation.MaxBackups != 0 {
		t.Errorf("Expected explicit MaxBackups 0 to be preserved, got %d", cfg.Logging.Rotation.MaxBackups)
	}
	if cfg.Logging.Rotation.MaxAge != 0 {
		t.Errorf("Expected explicit MaxAge 0 to be preserved, got %d", cfg.Logging.Rotation.MaxAge)
	}
}

func TestApplyDefaults_PreservesExplicitValues(t *testing.T) {
	cfg := &Bootstrap{
		Logging: LoggingConfig{
			Level:  "DEBUG",
			Format: "json",
			Output: "/var/log/dittofs.log",
		},
	}

	ApplyDefaults(cfg)

	// Verify explicit values were preserved
	if cfg.Logging.Level != "DEBUG" {
		t.Errorf("Expected explicit level 'DEBUG' to be preserved, got %q", cfg.Logging.Level)
	}
	if cfg.Logging.Format != "json" {
		t.Errorf("Expected explicit format 'json' to be preserved, got %q", cfg.Logging.Format)
	}
	if cfg.Logging.Output != "/var/log/dittofs.log" {
		t.Errorf("Expected explicit output to be preserved, got %q", cfg.Logging.Output)
	}
}

func TestGetDefaultConfig_IsValid(t *testing.T) {
	cfg := GetDefaultConfig()

	// The default config should pass validation
	err := Validate(cfg)
	if err != nil {
		t.Errorf("Default config should be valid, got error: %v", err)
	}
}

func TestGetDefaultConfig_HasRequiredFields(t *testing.T) {
	cfg := GetDefaultConfig()

	// Check all required sections are present
	if cfg.Logging.Level == "" {
		t.Error("Default config missing logging level")
	}
	if cfg.Blockstore.Journal.Path == "" {
		t.Error("Default config missing the journal path")
	}
}
