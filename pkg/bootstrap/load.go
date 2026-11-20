package bootstrap

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/cubbit/dittofs/internal/logger"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// Bootstrap is what the server reads before it opens any store: the facts of
// its host, RFC 13 Appendix C's keys. Everything else it serves is managed
// through its API and kept in its metadata store.
//
// Sources, in order of precedence: the environment (DITTOFS_*), the file,
// the defaults.
type Bootstrap struct {
	// Logging controls log output behavior
	Logging LoggingConfig `mapstructure:"logging" yaml:"logging"`

	// Blockstore holds the per-share local state root.
	Blockstore BlockstoreConfig `mapstructure:"blockstore" yaml:"blockstore"`

	// Node is this node's identity and its roles. The file names the
	// identity only when every journal directory's node-id copy is lost.
	Node struct {
		ID    entity.NodeID `mapstructure:"id"    yaml:"id"`
		Roles []string      `mapstructure:"roles" yaml:"roles"`
	} `mapstructure:"node" yaml:"node"`

	// Metadata is where the metadata store is and how to reach it. The
	// store's credential is files, so only their paths are held here.
	Metadata struct {
		Backend string `mapstructure:"backend" yaml:"backend"`
		Badger  struct {
			Path string `mapstructure:"path" yaml:"path"`
		} `mapstructure:"badger" yaml:"badger"`
		FoundationDB struct {
			ClusterFile string `mapstructure:"cluster_file" yaml:"cluster_file"`
			TLS         struct {
				CertFile string `mapstructure:"cert_file" yaml:"cert_file"`
				KeyFile  string `mapstructure:"key_file"  yaml:"key_file"`
				CAFile   string `mapstructure:"ca_file"   yaml:"ca_file"`
			} `mapstructure:"tls" yaml:"tls"`
		} `mapstructure:"foundationdb" yaml:"foundationdb"`
	} `mapstructure:"metadata" yaml:"metadata"`

	// Journals are the node's journal devices, one directory each.
	Journals []Journal `mapstructure:"journals" yaml:"journals"`

	// Adapters holds the addresses the protocol adapters bind; each
	// adapter's port is its own setting.
	Adapters struct {
		Listen []string `mapstructure:"listen" yaml:"listen"`
	} `mapstructure:"adapters" yaml:"adapters"`

	// API is where the management API listens: its ip:port addresses, its
	// TLS certificate and key, and its local socket.
	API struct {
		Listen []string `mapstructure:"listen" yaml:"listen"`
		TLS    struct {
			CertFile string `mapstructure:"cert_file" yaml:"cert_file"`
			KeyFile  string `mapstructure:"key_file"  yaml:"key_file"`
		} `mapstructure:"tls" yaml:"tls"`
		Socket string `mapstructure:"socket" yaml:"socket"`
	} `mapstructure:"api" yaml:"api"`

	// Metrics is the metrics listener's ip:port, or off.
	Metrics struct {
		Listen string `mapstructure:"listen" yaml:"listen"`
	} `mapstructure:"metrics" yaml:"metrics"`

	// Keys names each role's wrapping key for Secret records.
	Keys struct {
		Protocol WrappingKey `mapstructure:"protocol" yaml:"protocol"`
		Storage  WrappingKey `mapstructure:"storage"  yaml:"storage"`
	} `mapstructure:"keys" yaml:"keys"`
}

// LoggingConfig controls logging behavior.
type LoggingConfig struct {
	// Level is the minimum log level: debug, info, warn or error.
	Level string `mapstructure:"level" yaml:"level"`

	// Format is the log format: text or json.
	Format string `mapstructure:"format" yaml:"format"`

	// Output is where logs go: stdout, stderr, or an absolute file path.
	Output string `mapstructure:"output" yaml:"output"`

	// Rotation configures log file rotation (only active when Output is a file path)
	Rotation LogRotationConfig `mapstructure:"rotation" yaml:"rotation"`
}

// LogRotationConfig controls log file rotation via lumberjack.
// Rotation is only active when logging output is a file path (not stdout/stderr).
type LogRotationConfig struct {
	// MaxSize is the size in MiB a log file reaches before it rotates. The
	// file writes it as a size, a whole number of MiB of at least 1MiB.
	MaxSize int `mapstructure:"max_size" yaml:"max_size"`

	// MaxBackups is the number of rotated files kept; 0 keeps all.
	MaxBackups int `mapstructure:"max_backups" yaml:"max_backups"`

	// MaxAge is the number of days a rotated file is kept; 0 keeps all. The
	// file writes it as a duration, a whole number of days.
	MaxAge int `mapstructure:"max_age" yaml:"max_age"`

	// Compress determines whether rotated log files are gzip compressed.
	Compress bool `mapstructure:"compress" yaml:"compress"`
}

// Source is where a key's value came from: the file ("file"), the variable
// that set it (its name), or the default ("default").
type Source string

// The two sources that are not a variable's name.
const (
	sourceFile    Source = "file"
	sourceDefault Source = "default"
)

// Loaded is a loaded bootstrap with the source of each of its keys, by the
// key's dotted name.
type Loaded struct {
	Bootstrap

	Source map[string]Source
}

// whereEnvironment is the <where> of a refusal of a variable.
const whereEnvironment = "environment"

// Load reads the host's file and environ, the process environment as
// os.Environ returns it, and returns the bootstrap, or one error whose lines
// are every refusal of the file and the environment, in RFC 13 §6's shape.
func Load(configPath string, environ []string) (*Bootstrap, error) {
	loaded, err := LoadWithSources(configPath, environ)
	if err != nil {
		return nil, err
	}

	return &loaded.Bootstrap, nil
}

// MustLoad is Load. The name stays for its callers.
func MustLoad(configPath string, environ []string) (*Bootstrap, error) {
	return Load(configPath, environ)
}

// LoadWithSources is Load, with the source of each key's value, recorded
// where the value is chosen.
//
// The file is configPath, else DITTOFS_CONFIG's, else the default path
// (defaultConfigPath). A named file that is missing refuses; a default file
// that is missing loads the defaults; a file that cannot be read refuses.
func LoadWithSources(configPath string, environ []string) (*Loaded, error) {
	env := environMap(environ)

	var rep report

	path, named := configPath, configPath != ""
	if !named {
		if v := env["DITTOFS_CONFIG"]; v != "" {
			path, named = v, true
		} else {
			path = defaultConfigPath(env)
		}
	}

	var leaves map[string]*yaml.Node

	if path != "" {
		leaves = readFile(path, named, &rep)
	}

	values := chooseValues(path, leaves, env, &rep)
	relate(values, &rep)

	if len(rep) > 0 {
		return nil, rep.err()
	}

	return assemble(values)
}

// GetDefaultConfigPath returns the default configuration file path, from the
// process environment: $XDG_CONFIG_HOME/dittofs/config.yaml, else
// $HOME/.config/dittofs/config.yaml, else "" when neither is set.
func GetDefaultConfigPath() string {
	return defaultConfigPath(environMap(os.Environ()))
}

// defaultConfigPath is RFC 13 §2.2's path of the host's file, read from env:
// XDG_CONFIG_HOME's when set, else HOME's .config, else none.
func defaultConfigPath(env map[string]string) string {
	if xdg := env["XDG_CONFIG_HOME"]; xdg != "" {
		return filepath.Join(xdg, "dittofs", "config.yaml")
	}

	if home := env["HOME"]; home != "" {
		return filepath.Join(home, ".config", "dittofs", "config.yaml")
	}

	return ""
}

// environMap reads environ's NAME=value entries; the first entry of a name
// wins, as os.Getenv reads the process environment.
func environMap(environ []string) map[string]string {
	env := make(map[string]string, len(environ))

	for _, kv := range environ {
		name, value, ok := strings.Cut(kv, "=")
		if _, seen := env[name]; ok && !seen {
			env[name] = value
		}
	}

	return env
}

// readFile reads and parses the file at path into its declared keys' nodes,
// adding a refusal for the file itself or for each key it cannot place.
func readFile(path string, named bool, rep *report) map[string]*yaml.Node {
	data, err := os.ReadFile(path)

	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist) && !named:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		rep.add(path, "-", "the file does not exist")

		return nil
	default:
		rep.add(path, "-", "cannot read the file")

		return nil
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		rep.add(path, "-", yamlRule(err))

		return nil
	}

	// decision: the file holds one YAML document. yaml.Unmarshal reads the
	// first alone, so keys written after a "---" would be dropped without a
	// word; a second document is refused, without its content, which may be
	// anything.
	if secondDocument(data) {
		rep.add(path, "-", "a second YAML document; the file holds one")

		return nil
	}

	leaves := map[string]*yaml.Node{}

	if len(doc.Content) == 0 {
		return leaves
	}

	root := deref(doc.Content[0])

	switch {
	case root.Kind == yaml.MappingNode:
		walk(root, "", path, leaves, rep)
	case root.Kind == yaml.ScalarNode && root.ShortTag() == "!!null":
	default:
		rep.add(path, "-", "the root is not a mapping")
	}

	return leaves
}

// secondDocument reports whether data holds a YAML document after its
// first.
func secondDocument(data []byte) bool {
	d := yaml.NewDecoder(bytes.NewReader(data))

	var first, second yaml.Node
	if err := d.Decode(&first); err != nil {
		return false
	}

	return !errors.Is(d.Decode(&second), io.EOF)
}

// yamlLine finds the line a YAML error names.
var yamlLine = regexp.MustCompile(`line (\d+)`)

// yamlRule rewords a YAML parser's error: the parser quotes the text it could
// not read, which may be a secret, so only the line is kept.
func yamlRule(err error) string {
	if m := yamlLine.FindStringSubmatch(err.Error()); m != nil {
		return "not valid YAML at line " + m[1]
	}

	return "not valid YAML"
}

// deref follows a YAML alias to the node it names.
func deref(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}

	return n
}

// walk records each declared key of mapping m, as the file spells it, and
// refuses every other key: a key written twice, a block that is no mapping,
// a name holding a dot, and a key no block declares.
func walk(m *yaml.Node, prefix, path string, leaves map[string]*yaml.Node, rep *report) {
	seen := map[string]bool{}

	for i := 0; i+1 < len(m.Content); i += 2 {
		name := m.Content[i].Value
		value := deref(m.Content[i+1])
		key := joinKey(prefix, name)

		// A key is matched exactly as declared (RFC 13 Appendix C), one name
		// per level, so "logging.level:" written at the root is no
		// logging.level: read as one, it would be a second spelling of the
		// key, and one of the two would silently win.
		if strings.Contains(name, ".") {
			rep.add(path, key, "unknown key; write each part of a key at its own level")

			continue
		}

		if seen[name] {
			rep.add(path, key, "listed twice")

			continue
		}

		seen[name] = true

		switch {
		case specByKey[key] != nil:
			leaves[key] = value
		case blocks[key]:
			switch {
			case value.Kind == yaml.MappingNode:
				walk(value, key, path, leaves, rep)
			case value.Kind == yaml.ScalarNode && value.ShortTag() == "!!null":
			default:
				rep.add(path, key, "must be a mapping, got "+typeName(value))
			}
		default:
			refuseUndeclared(value, key, path, rep)
		}
	}
}

// refuseUndeclared refuses each leaf under an undeclared key, naming why:
// a key that moved names its replacement, a setting names the management
// API, and any other key names the closest declared one. No value is
// printed: an undeclared key's value may be a secret (RFC 13 §6).
//
// An alias inside an undeclared mapping is a leaf, not followed: an alias
// may name a mapping that holds it, and following it would never end.
func refuseUndeclared(n *yaml.Node, key, path string, rep *report) {
	if n.Kind == yaml.MappingNode && len(n.Content) > 0 {
		for i := 0; i+1 < len(n.Content); i += 2 {
			refuseUndeclared(n.Content[i+1], joinKey(key, n.Content[i].Value), path, rep)
		}

		return
	}

	switch replacement, moved := renamedKey(key); {
	case moved:
		rep.add(path, key, "no longer read; write "+replacement)
	case isSetting(key):
		rep.add(path, key, "a setting, not a host fact; set it through the management API")
	default:
		rep.add(path, key, "unknown key; did you mean "+closestKey(key)+"?")
	}
}

func joinKey(prefix, name string) string {
	if prefix == "" {
		return name
	}

	return prefix + "." + name
}

// closestKey is the declared key a mistyped key most likely meant: the one
// equal to it in another case, else the one at the smallest edit distance,
// however far, a tie going to the first in RFC 13 Appendix C's order.
func closestKey(key string) string {
	lower := strings.ToLower(key)

	for _, s := range keySpecs {
		if s.key == lower {
			return s.key
		}
	}

	best, bestDist := "", -1

	for _, s := range keySpecs {
		if d := editDistance(key, s.key); bestDist < 0 || d < bestDist {
			best, bestDist = s.key, d
		}
	}

	return best
}

// editDistance is the Levenshtein distance between a and b, by bytes.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}

	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i

		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}

			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}

		prev = cur
	}

	return prev[len(b)]
}

// chosen is one key's value as the load chose it: from the environment over
// the file over the default.
type chosen struct {
	spec   *keySpec
	value  any
	source Source
	// set says a source named the key; passed, that its value passed its
	// own checks, so the rules relating it to another key may read it.
	set    bool
	passed bool
	where  string
	name   string
}

// chooseValues takes each declared key's value from its variable, else the
// file, else its default, checking every value a source gives, the file's
// included when a variable replaces it.
func chooseValues(
	path string,
	leaves map[string]*yaml.Node,
	env map[string]string,
	rep *report,
) map[string]*chosen {
	values := make(map[string]*chosen, len(keySpecs))

	for i := range keySpecs {
		s := &keySpecs[i]
		c := &chosen{spec: s, value: s.defaultValue(), source: sourceDefault, passed: true}

		if n, ok := leaves[s.key]; ok && (!s.nullUnset || !isNull(n)) {
			v, ok := s.fromNode(n, path, rep)
			c.value, c.source, c.set, c.passed = v, sourceFile, true, ok
			c.where, c.name = path, s.key
		}

		if raw, ok := env[s.envName()]; ok && (!s.nullUnset || raw != "") {
			v, ok := s.fromEnv(raw, rep)
			c.value, c.source, c.set, c.passed = v, Source(s.envName()), true, ok
			c.where, c.name = whereEnvironment, s.envName()
		}

		values[s.key] = c
	}

	return values
}

// relate runs the rules that relate two keys, over the values as the file
// and the environment leave them, and only over keys that passed their own
// checks, so a refused key does not also refuse the keys it relates to
// (RFC 13 §6).
func relate(values map[string]*chosen, rep *report) {
	refuse := func(c *chosen, rule string) {
		rep.add(c.where, c.name, rule)
		c.passed = false
	}

	if b := values["metadata.backend"]; b.passed {
		backend := b.value.(string)

		for key, c := range values {
			block, ok := strings.CutPrefix(key, "metadata.")
			if !ok || !c.set || !c.passed || key == "metadata.backend" {
				continue
			}

			if name, _, _ := strings.Cut(block, "."); name != backend {
				refuse(c, "read only with metadata.backend: "+name+"; remove it")
			}
		}
	}

	for _, role := range []string{"protocol", "storage"} {
		kind := values["keys."+role+".kind"]
		if !kind.set || !kind.passed {
			continue
		}

		for key, c := range values {
			block, ok := strings.CutPrefix(key, "keys."+role+".")
			if !ok || !c.set || !c.passed || block == "kind" {
				continue
			}

			if name, _, _ := strings.Cut(block, "."); name != kind.value.(string) {
				refuse(c, "read only with keys."+role+".kind: "+name+"; remove it or change kind")
			}
		}
	}

	relateTLS(values, rep)
	relateJournals(values["journals"], rep)
}

// relateTLS refuses FoundationDB's TLS files unless the three are named
// together or none is, once, on the first named. It runs only when every
// named one passed its own checks, so a refused file is not also reported
// as missing.
func relateTLS(values map[string]*chosen, rep *report) {
	var named, missing []string

	for _, f := range []string{"cert_file", "key_file", "ca_file"} {
		c := values["metadata.foundationdb.tls."+f]

		switch {
		case c.set && !c.passed:
			return
		case c.set:
			named = append(named, f)
		default:
			missing = append(missing, f)
		}
	}

	if len(named) == 0 || len(missing) == 0 {
		return
	}

	c := values["metadata.foundationdb.tls."+named[0]]
	rep.add(
		c.where,
		c.name,
		"named without "+strings.Join(missing, " and ")+"; name the three together or none",
	)
}

// relateJournals refuses a journals list naming one directory twice: one
// path listed twice, or two paths to one directory, which would be two
// journals over one directory's files.
func relateJournals(c *chosen, rep *report) {
	if !c.set || !c.passed {
		return
	}

	journals := c.value.([]Journal)
	env := c.where == whereEnvironment

	for i := range journals {
		for j := range i {
			a, b := journals[j].Path, journals[i].Path

			switch {
			case a == b && env:
				rep.add(c.where, c.name, "a path listed twice")
			case a == b:
				rep.add(c.where, c.name, a+" listed twice")
			case sameDir(a, b) && env:
				rep.add(c.where, c.name, "two paths name one directory")
			case sameDir(a, b):
				rep.add(c.where, c.name, a+" and "+b+" name one directory")
			}
		}
	}
}

// sameDir reports whether a and b both exist and are one directory.
func sameDir(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}

	bi, err := os.Stat(b)
	if err != nil {
		return false
	}

	return os.SameFile(ai, bi)
}

// assemble builds the bootstrap from the chosen values.
func assemble(values map[string]*chosen) (*Loaded, error) {
	tree := map[string]any{}
	sources := make(map[string]Source, len(values))

	for key, c := range values {
		sources[key] = c.source
		node := tree
		parts := strings.Split(key, ".")

		for _, p := range parts[:len(parts)-1] {
			next, ok := node[p].(map[string]any)
			if !ok {
				next = map[string]any{}
				node[p] = next
			}

			node = next
		}

		node[parts[len(parts)-1]] = c.value
	}

	v := viper.New()
	if err := v.MergeConfigMap(tree); err != nil {
		return nil, fmt.Errorf("merge the bootstrap: %w", err)
	}

	loaded := &Loaded{Source: sources}

	// decision: weak typing is off and viper's default hooks are dropped.
	// Every value is already of its field's type, checked against RFC 13
	// Appendix B's forms, so a decode that converts anything (a scalar into a
	// list, "t" into true, a number into a duration) would be running a
	// configuration other than the one written (RFC 13 §6, "refused, never
	// replaced"). Turn either back on only for a field whose value the
	// checks above do not type.
	if err := v.Unmarshal(&loaded.Bootstrap, func(dc *mapstructure.DecoderConfig) {
		dc.WeaklyTypedInput = false
		dc.DecodeHook = nil
		dc.ErrorUnused = true
	}); err != nil {
		return nil, fmt.Errorf("decode the bootstrap: %w", err)
	}

	loaded.Blockstore.applyDefaults()

	if err := loaded.Blockstore.validate(); err != nil {
		var rep report

		// A default path is the state directory's, so the variable that
		// made it relative is named.
		if c := values["blockstore.journal.path"]; c.set {
			rep.add(c.where, c.name, "must be an absolute directory")
		} else {
			rep.add(whereEnvironment, stateDirVariable(), "must be an absolute path")
		}

		return nil, rep.err()
	}

	return loaded, nil
}

// refusal is one line of a load's report: <where>: <key>: <rule>.
type refusal struct {
	where, key, rule string
}

// report collects every refusal of one load (RFC 13 §6).
type report []refusal

func (r *report) add(where, key, rule string) {
	*r = append(*r, refusal{where: where, key: key, rule: rule})
}

// err returns the refusals as one error, sorted by where then key, one line
// each.
func (r report) err() error {
	sort.SliceStable(r, func(i, j int) bool {
		if r[i].where != r[j].where {
			return r[i].where < r[j].where
		}

		return r[i].key < r[j].key
	})

	lines := make([]string, len(r))
	for i, f := range r {
		lines[i] = oneLine(f.where) + ": " + oneLine(f.key) + ": " + oneLine(f.rule)
	}

	return errors.New(strings.Join(lines, "\n"))
}

// oneLine returns s as it is when it is one line of printable characters,
// else quoted, with its control characters escaped: a key the file spells
// with a line break would otherwise print a second line that reads as
// another refusal, and a control character would reach the terminal.
func oneLine(s string) string {
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return strconv.Quote(s)
		}
	}

	return s
}

// getStateDir returns the state directory path for runtime data (logs, PID files).
//
// On Windows, uses %LOCALAPPDATA%\dittofs.
// On Unix, uses XDG_STATE_HOME/dittofs or ~/.local/state/dittofs.
func getStateDir() string {
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

// stateDirVariable names the variable getStateDir read the state directory
// from.
func stateDirVariable() string {
	if runtime.GOOS == "windows" {
		if os.Getenv("LOCALAPPDATA") != "" {
			return "LOCALAPPDATA"
		}

		return "USERPROFILE"
	}

	if os.Getenv("XDG_STATE_HOME") != "" {
		return "XDG_STATE_HOME"
	}

	return "HOME"
}

// getDefaultLogPath returns the default log file path.
func getDefaultLogPath() string {
	return filepath.Join(getStateDir(), "dittofs.log")
}

// InitLogger initializes the structured logger from a LoggingConfig,
// including rotation settings. This is the canonical way to initialize
// the logger from configuration: prefer this over constructing
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
