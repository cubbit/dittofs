package config

import (
	"strings"
	"testing"
)

func TestValidate_ValidConfig(t *testing.T) {
	cfg := GetDefaultConfig()

	err := Validate(cfg)
	if err != nil {
		t.Errorf("Expected valid config to pass validation, got error: %v", err)
	}
}

func TestValidate_InvalidLogLevel(t *testing.T) {
	cfg := GetDefaultConfig()
	cfg.Logging.Level = "INVALID"

	err := Validate(cfg)
	if err == nil {
		t.Fatal("Expected validation error for invalid log level")
	}
	if !strings.Contains(err.Error(), "oneof") {
		t.Errorf("Expected 'oneof' validation error, got: %v", err)
	}
}

func TestValidate_InvalidLogFormat(t *testing.T) {
	cfg := GetDefaultConfig()
	cfg.Logging.Format = "xml"

	err := Validate(cfg)
	if err == nil {
		t.Fatal("Expected validation error for invalid log format")
	}
}

func TestValidate_LogLevelNormalization(t *testing.T) {
	// Test that validation accepts both uppercase and lowercase log levels
	testCases := []string{"info", "INFO", "debug", "DEBUG", "warn", "WARN", "error", "ERROR"}

	for _, level := range testCases {
		cfg := GetDefaultConfig()
		cfg.Logging.Level = level

		err := Validate(cfg)
		if err != nil {
			t.Errorf("Validation failed for level %q: %v", level, err)
		}

		// Validation should NOT normalize - level should remain as-is
		if cfg.Logging.Level != level {
			t.Errorf("Expected level to remain %q after validation, got %q", level, cfg.Logging.Level)
		}
	}

	// Test that normalization happens in ApplyDefaults
	cfg := &Bootstrap{Logging: LoggingConfig{Level: "info"}}
	ApplyDefaults(cfg)
	if cfg.Logging.Level != "INFO" {
		t.Errorf("Expected ApplyDefaults to normalize 'info' to 'INFO', got %q", cfg.Logging.Level)
	}
}

// Validate reaches the journal path's own check: a relative path is refused.
func TestValidate_JournalPathInFanOut(t *testing.T) {
	cfg := GetDefaultConfig()
	cfg.Blockstore.Journal.Path = "relative/blocks"
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "blockstore.journal.path") {
		t.Fatalf("Validate = %v, want the blockstore.journal.path error", err)
	}
}
