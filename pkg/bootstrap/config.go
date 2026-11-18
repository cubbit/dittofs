package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/cubbit/dittofs/internal/logger"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
)

// Bootstrap is what the server reads before it opens any store: where it
// logs, and where each share's local state lives. Everything else it serves
// is managed through its API and kept in its metadata store.
//
// Configuration sources (in order of precedence):
//  1. Environment variables (DITTOFS_*)
//  2. Configuration file (YAML or TOML)
//  3. Default values (lowest priority)
type Bootstrap struct {
	// Logging controls log output behavior
	Logging LoggingConfig `mapstructure:"logging" yaml:"logging"`

	// Blockstore holds the per-share local state root.
	Blockstore BlockstoreConfig `mapstructure:"blockstore" yaml:"blockstore"`

	// Node is this node's identity, Devices its journal directories, one
	// per device, which the engine opens its journals in, and APIAddr and
	// MetricsAddr the host:port its API and metrics listeners bind, a port
	// of 0 picking a free one.
	//
	// ponytail: nothing in the file sets any of them today, and Load never
	// reads them: the host sets them before it builds the node. RFC 13's
	// bootstrap loader reads them once the node's identity, its devices and
	// its listen addresses are bootstrap facts.
	Node        entity.NodeID `mapstructure:"-" yaml:"-" json:"-"`
	Devices     []string      `mapstructure:"-" yaml:"-" json:"-"`
	APIAddr     string        `mapstructure:"-" yaml:"-" json:"-"`
	MetricsAddr string        `mapstructure:"-" yaml:"-" json:"-"`
}

// LoggingConfig controls logging behavior.
type LoggingConfig struct {
	// Level is the minimum log level to output
	// Valid values: DEBUG, INFO, WARN, ERROR (case-insensitive, normalized to uppercase)
	Level string `mapstructure:"level" validate:"required,oneof=DEBUG INFO WARN ERROR debug info warn error" yaml:"level"`

	// Format specifies the log output format
	// Valid values: text, json
	Format string `mapstructure:"format" validate:"required,oneof=text json" yaml:"format"`

	// Output specifies where logs are written
	// Valid values: stdout, stderr, or a file path
	Output string `mapstructure:"output" validate:"required" yaml:"output"`

	// Rotation configures log file rotation (only active when Output is a file path)
	Rotation LogRotationConfig `mapstructure:"rotation" yaml:"rotation"`
}

// LogRotationConfig controls log file rotation via lumberjack.
// Rotation is only active when logging output is a file path (not stdout/stderr).
type LogRotationConfig struct {
	// MaxSize is the maximum size in megabytes of the log file before it gets rotated.
	// If MaxSize is 0, size-based rotation is disabled; if greater than 0, rotation
	// occurs when the file exceeds this size. The defaults layer sets this to 100 MB.
	MaxSize int `mapstructure:"max_size" yaml:"max_size"`

	// MaxBackups is the maximum number of old log files to retain.
	// 0 means keep all old log files.
	// The generated config template sets this to 5.
	MaxBackups int `mapstructure:"max_backups" yaml:"max_backups"`

	// MaxAge is the maximum number of days to retain old log files.
	// 0 means no age limit (keep forever).
	// The generated config template sets this to 30.
	MaxAge int `mapstructure:"max_age" yaml:"max_age"`

	// Compress determines whether rotated log files are gzip compressed.
	// Default: false
	Compress bool `mapstructure:"compress" yaml:"compress"`
}

// Load loads configuration from file, environment, and defaults.
//
// Configuration precedence (highest to lowest):
//  1. Environment variables (DITTOFS_*)
//  2. Configuration file
//  3. Default values
//
// Parameters:
//   - configPath: Path to config file (empty string uses default location)
//   - environ: the process environment, as os.Environ returns it
//
// Returns:
//   - *Bootstrap: Loaded and validated configuration
//   - error: Configuration loading or validation error
//
// environ is not read yet: the environment still reaches the keys through
// viper's AutomaticEnv. The argument is there so that the loader replacing
// this one reads no process state, only what its caller passes.
func Load(configPath string, environ []string) (*Bootstrap, error) {
	v := viper.New()

	// Configure viper
	setupViper(v, configPath)

	// Read configuration file if it exists
	configFileFound, err := readConfigFile(v)
	if err != nil {
		return nil, err
	}

	// Unmarshal into config struct with custom decode hooks.
	//
	// This runs even when no config file was found. setupViper already bound
	// every DITTOFS_* env key, and v.Unmarshal is the only path that applies
	// those bindings; short-circuiting to GetDefaultConfig() on the no-file
	// path would silently drop every env override (e.g.
	// DITTOFS_LOGGING_LEVEL) for container/CI deployments that ship no config file, violating the
	// documented "env > file > defaults" precedence. Unmarshalling into a zero
	// struct then ApplyDefaults yields defaults for every field the env doesn't
	// set, identical to the file path.
	//
	// Capture unknown keys via decoder Metadata and log them as a warning
	// rather than silently dropping them — this surfaces typos and stale keys
	// from removed config trees (e.g. the deleted `lock:`/`syncer:` sections or
	// a `cache:` block) without hard-failing boot on an otherwise-valid config
	// that still carries a legacy key (upgrade safety).
	var md mapstructure.Metadata
	var cfg Bootstrap
	if err := v.Unmarshal(&cfg,
		func(dc *mapstructure.DecoderConfig) { dc.Metadata = &md },
	); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}
	// Unknown-key warnings only make sense when a file was actually parsed;
	// the env-only path has no file keys to be reported as unused.
	if configFileFound && len(md.Unused) > 0 {
		// A key that was renamed rather than removed is refused instead of
		// warned about — see checkRenamedKeys.
		if err := checkRenamedKeys(md.Unused); err != nil {
			return nil, err
		}
		sort.Strings(md.Unused)
		fmt.Fprintf(os.Stderr, "WARNING: config contains unknown keys (ignored): %s\n", strings.Join(md.Unused, ", "))
	}

	// Apply defaults for any missing values
	ApplyDefaults(&cfg)

	// Validate configuration
	if err := Validate(&cfg); err != nil {
		return nil, fmt.Errorf("configuration validation failed: %w", err)
	}

	return &cfg, nil
}

// MustLoad loads configuration with helpful error messages. A missing file at
// the default location falls back to the defaults; a missing file named
// explicitly is an error.
//
// Parameters:
//   - configPath: Path to config file (empty string uses default location)
//   - environ: the process environment, passed on to Load
//
// Returns:
//   - *Bootstrap: Loaded and validated configuration
//   - error: User-friendly error if the named config file is missing
func MustLoad(configPath string, environ []string) (*Bootstrap, error) {
	// Determine config path
	if configPath == "" {
		if !DefaultConfigExists() {
			// No config file at the default location. Rather than hard-fail
			// (which makes a systemd unit crash-loop), fall back to built-in
			// defaults + DITTOFS_* env overrides and warn clearly. Load("")
			// already resolves env > defaults and validates, so a defaults-
			// only boot is a fully supported configuration.
			fmt.Fprintf(os.Stderr,
				"WARNING: no configuration file found at default location: %s\n"+
					"         starting with built-in defaults (override via DITTOFS_* env vars).\n",
				GetDefaultConfigPath())

			cfg, err := Load("", environ)
			if err != nil {
				return nil, fmt.Errorf("failed to load default configuration: %w", err)
			}
			return cfg, nil
		}
		configPath = GetDefaultConfigPath()
	} else {
		if _, err := os.Stat(configPath); os.IsNotExist(err) {
			return nil, fmt.Errorf("configuration file not found: %s", configPath)
		}
	}

	// Load configuration
	cfg, err := Load(configPath, environ)
	if err != nil {
		return nil, fmt.Errorf("failed to load configuration: %w", err)
	}

	return cfg, nil
}

// setupViper configures viper with environment variables and config file settings.
func setupViper(v *viper.Viper, configPath string) {
	// Set up environment variable support
	// Environment variables use DITTOFS_ prefix and underscores
	// Example: DITTOFS_LOGGING_LEVEL=DEBUG
	v.SetEnvPrefix("DITTOFS")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// Bind every config key for AutomaticEnv resolution.
	//
	// Viper's AutomaticEnv + Unmarshal only honours an env var for a key
	// that is already known to viper (present in the config file, a
	// SetDefault, or an explicit BindEnv). Any key absent from the file
	// is otherwise silently dropped from the env — so e.g. a container
	// setting DITTOFS_LOGGING_LEVEL=DEBUG with `level` omitted from the
	// file would be ignored.
	//
	// To make env precedence reliable for the whole config surface we
	// reflect over the default Config struct and BindEnv every nested
	// mapstructure key path (dot-separated; the key replacer above maps
	// "." -> "_" so blockstore.journal.path binds
	// DITTOFS_BLOCKSTORE_JOURNAL_PATH).
	for _, key := range configEnvKeys() {
		_ = v.BindEnv(key)
	}

	// Configure config file search
	if configPath != "" {
		// Use explicitly specified config file
		v.SetConfigFile(configPath)
	} else {
		// Use default location: $XDG_CONFIG_HOME/dittofs/config.{yaml,toml}
		configDir := getConfigDir()
		v.AddConfigPath(configDir)
		v.SetConfigName("config")
		v.SetConfigType("yaml") // Primary format
	}
}

// configEnvKeys returns every dot-separated mapstructure key path in the
// Bootstrap struct, so each can be bound for AutomaticEnv resolution. Nested
// structs are walked recursively; leaf fields (including maps and slices)
// contribute their own key. Maps with dynamic keys bind at the container
// level only: env cannot address arbitrary map entries.
func configEnvKeys() []string {
	var keys []string
	collectMapstructureKeys(reflect.TypeOf(Bootstrap{}), "", &keys)
	return keys
}

// collectMapstructureKeys recursively appends the mapstructure key paths of t
// (rooted at prefix) to out.
func collectMapstructureKeys(t reflect.Type, prefix string, out *[]string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}

		tag := field.Tag.Get("mapstructure")
		// Strip mapstructure options like ",squash"/",omitempty".
		name := tag
		if comma := strings.IndexByte(name, ','); comma >= 0 {
			name = name[:comma]
		}
		if name == "" {
			// Untagged exported field: skip — it has no stable key.
			continue
		}

		key := name
		if prefix != "" {
			key = prefix + "." + name
		}

		ft := field.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}

		// Recurse into nested structs (but not time.Duration, which is a
		// named int64, nor stdlib structs we treat as leaves). A struct
		// with mapstructure-tagged fields is a nested namespace.
		if ft.Kind() == reflect.Struct && ft != reflect.TypeOf(time.Duration(0)) && hasMapstructureFields(ft) {
			collectMapstructureKeys(ft, key, out)
			continue
		}

		*out = append(*out, key)
	}
}

// hasMapstructureFields reports whether t has at least one exported field with
// a mapstructure tag, i.e. it is a config namespace worth recursing into.
func hasMapstructureFields(t reflect.Type) bool {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.IsExported() && f.Tag.Get("mapstructure") != "" {
			return true
		}
	}
	return false
}

// readConfigFile reads the configuration file if it exists.
// Returns (fileFound, error) where fileFound indicates if a config file was found.
func readConfigFile(v *viper.Viper) (bool, error) {
	if err := v.ReadInConfig(); err != nil {
		// Check if error is "config file not found"
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			// Config file not found is acceptable - use defaults
			return false, nil
		}
		// Also check for os.PathError when explicit config file doesn't exist
		if os.IsNotExist(err) {
			// Config file not found is acceptable - use defaults
			return false, nil
		}
		// Other errors are problems
		return false, fmt.Errorf("failed to read config file: %w", err)
	}

	return true, nil
}

// getConfigDir returns the configuration directory path.
//
// On Windows, uses %APPDATA%\dittofs (matching internal/cli/credentials/store.go pattern).
// On Unix, uses XDG_CONFIG_HOME/dittofs or ~/.config/dittofs.
// Falls back to current directory (.) if home directory cannot be determined.
func getConfigDir() string {
	if runtime.GOOS == "windows" {
		// On Windows, use %APPDATA%\dittofs
		appData := os.Getenv("APPDATA")
		if appData != "" {
			return filepath.Join(appData, "dittofs")
		}
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, "AppData", "Roaming", "dittofs")
		}
		return "."
	}

	// Unix: XDG_CONFIG_HOME or ~/.config
	if xdgConfig := os.Getenv("XDG_CONFIG_HOME"); xdgConfig != "" {
		return filepath.Join(xdgConfig, "dittofs")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, ".config", "dittofs")
}

// GetDefaultConfigPath returns the default configuration file path.
func GetDefaultConfigPath() string {
	return filepath.Join(getConfigDir(), "config.yaml")
}

// DefaultConfigExists checks if a config file exists at the default location.
func DefaultConfigExists() bool {
	path := GetDefaultConfigPath()
	_, err := os.Stat(path)
	return err == nil
}

// GetStateDir returns the state directory path for runtime data (logs, PID files).
//
// On Windows, uses %LOCALAPPDATA%\dittofs.
// On Unix, uses XDG_STATE_HOME/dittofs or ~/.local/state/dittofs.
func GetStateDir() string {
	if runtime.GOOS == "windows" {
		localAppData := os.Getenv("LOCALAPPDATA")
		if localAppData != "" {
			return filepath.Join(localAppData, "dittofs")
		}
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, "AppData", "Local", "dittofs")
		}
		return filepath.Join(os.TempDir(), "dittofs")
	}

	stateDir := os.Getenv("XDG_STATE_HOME")
	if stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(os.TempDir(), "dittofs")
		}
		stateDir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateDir, "dittofs")
}

// GetDefaultLogPath returns the default log file path.
func GetDefaultLogPath() string {
	return filepath.Join(GetStateDir(), "dittofs.log")
}

// InitLogger initializes the structured logger from a LoggingConfig,
// including rotation settings. This is the canonical way to initialize
// the logger from configuration — prefer this over constructing
// logger.Config manually to ensure rotation settings are plumbed through.
func InitLogger(cfg *Bootstrap) error {
	loggerCfg := logger.Config{
		Level:      cfg.Logging.Level,
		Format:     cfg.Logging.Format,
		Output:     cfg.Logging.Output,
		MaxSize:    cfg.Logging.Rotation.MaxSize,
		MaxBackups: cfg.Logging.Rotation.MaxBackups,
		MaxAge:     cfg.Logging.Rotation.MaxAge,
		Compress:   cfg.Logging.Rotation.Compress,
	}
	if err := logger.Init(loggerCfg); err != nil {
		return fmt.Errorf("failed to initialize logger: %w", err)
	}
	return nil
}
