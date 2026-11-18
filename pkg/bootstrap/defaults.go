package bootstrap

import "strings"

// applyDefaults sets default values for any unspecified configuration fields.
//
// Called after loading configuration from file and environment
// variables to fill in any missing values with sensible defaults.
//
// Default Strategy:
//   - Zero values (0, "", false, nil) are replaced with defaults
//   - Explicit values are preserved
func applyDefaults(cfg *Bootstrap) {
	applyLoggingDefaults(&cfg.Logging)
	cfg.Blockstore.applyDefaults()
}

// applyLoggingDefaults sets logging defaults and normalizes values.
func applyLoggingDefaults(cfg *LoggingConfig) {
	if cfg.Level == "" {
		cfg.Level = "INFO"
	}
	// Normalize log level to uppercase for consistent internal representation
	cfg.Level = strings.ToUpper(cfg.Level)

	if cfg.Format == "" {
		cfg.Format = "text"
	}
	if cfg.Output == "" {
		cfg.Output = getDefaultLogPath()
	}

	// Log rotation defaults (only meaningful when output is a file path).
	// Only MaxSize is defaulted here; MaxBackups and MaxAge are left at 0
	// (meaning "keep all" and "no age limit" respectively in lumberjack)
	// so that users can explicitly set them to 0 without being overridden.
	// GetDefaultConfig provides sensible starting values.
	if cfg.Rotation.MaxSize == 0 {
		cfg.Rotation.MaxSize = 100 // 100 MB
	}
}

// GetDefaultConfig returns a Bootstrap with all default values applied.
//
// This is useful for:
//   - Generating sample configuration files
//   - Testing
//   - Documentation
func GetDefaultConfig() *Bootstrap {
	cfg := &Bootstrap{
		Logging: LoggingConfig{
			Rotation: LogRotationConfig{
				MaxBackups: 5,
				MaxAge:     30,
			},
		},
	}

	applyDefaults(cfg)
	return cfg
}
