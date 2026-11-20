package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/keyprovider"
)

// passphrase is the passphrase of this file's key files.
const passphrase = "s3cr3t-pass"

// passphraseEnviron names passphrase the way the process environment does.
var passphraseEnviron = []string{"DITTOFS_ENCRYPTION_PASSPHRASE=" + passphrase}

// fileBase is the content every Load-based row starts from: a journal, and
// a kmip key for each role so a row refuses for the one fact it changes.
const fileBase = "journals:\n  - path: /j/1\n" +
	"keys:\n  protocol:\n    kind: kmip\n  storage:\n    kind: kmip\n"

// keyFiles returns two key files, each with its own key ID. A file unwraps
// with an argon2 pass of 64 MiB, so the tests that need real files make
// them once each.
func keyFiles(t *testing.T) [2]string {
	t.Helper()

	var files [2]string

	dir := t.TempDir()

	for i := range files {
		raw, err := keyprovider.GenerateKeyFile(passphrase)
		if err != nil {
			t.Fatalf("GenerateKeyFile: %v", err)
		}

		files[i] = filepath.Join(dir, "k"+strconv.Itoa(i)+".key")

		if err := os.WriteFile(files[i], raw, 0o600); err != nil {
			t.Fatalf("write key file: %v", err)
		}
	}

	return files
}

// handBuilt returns a bootstrap whose every fact passes: both roles, a
// journal, kmip wrapping keys, loopback listeners.
func handBuilt() *Bootstrap {
	var b Bootstrap

	b.Node.Roles = []string{"protocol", "storage"}
	b.Journals = []Journal{{Path: "/j/1"}}
	b.Keys.Protocol.Kind = keyprovider.KindKMIP
	b.Keys.Storage.Kind = keyprovider.KindKMIP
	b.API.Listen = []string{"127.0.0.1:8080"}
	b.Metrics.Listen = "127.0.0.1:9091"
	b.Metadata.Backend = "badger"

	return &b
}

// localKeys sets both roles to the local key files named.
func localKeys(b *Bootstrap, protocol, storage string) *Bootstrap {
	b.Keys.Protocol.Kind = keyprovider.KindLocal
	b.Keys.Protocol.Local.File = protocol
	b.Keys.Storage.Kind = keyprovider.KindLocal
	b.Keys.Storage.Local.File = storage

	return b
}

// stubKey is a provider that holds only an ID and has no key to close.
type stubKey struct {
	keyprovider.KeyProvider

	id string
}

func (s stubKey) CurrentMasterKeyID() string {
	return s.id
}

func (s stubKey) Close() error {
	return nil
}

// distinctKeys is a loader that hands each load a key ID of its own and
// unwraps nothing, for the rows that are about no key file.
func distinctKeys() providerLoader {
	n := 0

	return func(context.Context, keyprovider.Config, string) (keyprovider.KeyProvider, error) {
		n++

		return stubKey{id: "id-" + strconv.Itoa(n)}, nil
	}
}

// withStateDir names a state directory of its own after environ, so a
// row never checks the directory of the host that runs it. The first
// entry of a name wins, so a row naming its own keeps it.
func withStateDir(t *testing.T, environ []string) []string {
	t.Helper()

	return append(append([]string(nil), environ...), "STATE_DIRECTORY="+t.TempDir())
}

// show validates b with the loader given and returns the report as text,
// empty when it passed. An error with no text is a failure of its own.
func show(t *testing.T, b *Bootstrap, environ []string, load providerLoader) string {
	t.Helper()

	err := b.validate(context.Background(), withStateDir(t, environ), load, processUser())
	if err == nil {
		return ""
	}

	if err.Error() == "" {
		t.Fatal("Validate returned an error with no text")
	}

	return err.Error()
}

// showReal validates b with the providers of the package.
func showReal(t *testing.T, b *Bootstrap, environ []string) string {
	t.Helper()

	err := b.Validate(context.Background(), withStateDir(t, environ))
	if err == nil {
		return ""
	}

	return err.Error()
}

// loadsFile loads content from a file of its own and returns the bootstrap
// and the file's path.
func loadsFile(t *testing.T, content string, environ []string) (*Bootstrap, string) {
	t.Helper()

	path := writeFile(t, content)

	loaded, err := LoadWithSources(path, environ)
	if err != nil {
		t.Fatalf("load %q: %v", content, err)
	}

	return &loaded.Bootstrap, path
}

// Both roles are needed, compared as a set: one alone is refused, a role
// listed twice is refused, and both in any order is the only value accepted.
func TestValidateRoles(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		roles []string
		want  string
	}{
		{
			"storage alone",
			[]string{"storage"},
			"environment: node.roles: both roles are needed, \"protocol\" is missing",
		},
		{
			"protocol alone",
			[]string{"protocol"},
			"environment: node.roles: both roles are needed, \"storage\" is missing",
		},
		{
			"no role", nil,
			"environment: node.roles: both roles are needed, " +
				"\"protocol\" and \"storage\" are missing",
		},
		{
			"protocol twice",
			[]string{"protocol", "protocol"},
			"environment: node.roles: \"protocol\" is listed twice\n" +
				"environment: node.roles: both roles are needed, \"storage\" is missing",
		},
		{
			"storage twice beside protocol",
			[]string{"protocol", "storage", "storage"},
			"environment: node.roles: \"storage\" is listed twice",
		},
		{"both", []string{"protocol", "storage"}, ""},
		{"both, storage first", []string{"storage", "protocol"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b := handBuilt()
			b.Node.Roles = tc.roles

			if got := show(t, b, nil, distinctKeys()); got != tc.want {
				t.Errorf("roles %v:\ngot:\n%s\nwant:\n%s", tc.roles, got, tc.want)
			}
		})
	}
}

// A key with no kind is refused for its role, and a local key with no file
// is refused naming the file; the other role is checked as well.
func TestValidateKeysNamed(t *testing.T) {
	t.Parallel()

	const storage = "  storage:\n    kind: local\n    local:\n      file: /k/storage.key\n"

	for _, tc := range []struct {
		name    string
		content string
		want    []string
	}{
		{
			"protocol's kind unset",
			"journals:\n  - path: /j/1\nkeys:\n" + storage,
			[]string{"P: keys.protocol.kind: required"},
		},
		{
			"storage's kind unset",
			"journals:\n  - path: /j/1\nkeys:\n  protocol:\n    kind: kmip\n",
			[]string{"P: keys.storage.kind: required"},
		},
		{
			"both kinds unset",
			"journals:\n  - path: /j/1\n",
			[]string{"P: keys.protocol.kind: required", "P: keys.storage.kind: required"},
		},
		{
			"a local key with no file",
			"journals:\n  - path: /j/1\nkeys:\n  protocol:\n    kind: local\n" + storage,
			[]string{"P: keys.protocol.local.file: required with local"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b, path := loadsFile(t, tc.content, nil)

			got := show(t, b, nil, distinctKeys())

			if want := wantReport(path, tc.want...); got != want {
				t.Errorf("keys named:\ngot:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

// Two roles naming one key are refused whatever makes the two files one
// key: one file named twice, a byte copy, a link to it.
func TestValidateSameKey(t *testing.T) {
	t.Parallel()

	files := keyFiles(t)
	dir := t.TempDir()

	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read key file: %v", err)
	}

	copied := filepath.Join(dir, "copy.key")
	if err := os.WriteFile(copied, raw, 0o600); err != nil {
		t.Fatalf("write the copy: %v", err)
	}

	link := filepath.Join(dir, "link.key")
	if err := os.Symlink(files[0], link); err != nil {
		t.Fatalf("link the key file: %v", err)
	}

	for _, tc := range []struct {
		name     string
		protocol string
		storage  string
		want     string
	}{
		{
			"one file named twice", files[0], files[0],
			"environment: keys.storage.kind: names the same key as keys.protocol",
		},
		{
			"a byte copy", files[0], copied,
			"environment: keys.storage.kind: names the same key as keys.protocol",
		},
		{
			"a link to it", files[0], link,
			"environment: keys.storage.kind: names the same key as keys.protocol",
		},
		{"two different keys", files[0], files[1], ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b := localKeys(handBuilt(), tc.protocol, tc.storage)

			if got := showReal(t, b, passphraseEnviron); got != tc.want {
				t.Errorf("same key:\ngot:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

// The passphrase in the environ given unwraps the keys, and the process's
// own variable, holding another, is never read. Not parallel: it sets the
// process's variable.
func TestValidatePassphraseFromEnviron(t *testing.T) {
	t.Setenv("DITTOFS_ENCRYPTION_PASSPHRASE", "the-host-passphrase")

	files := keyFiles(t)
	b := localKeys(handBuilt(), files[0], files[1])

	if got := showReal(t, b, passphraseEnviron); got != "" {
		t.Errorf("the passphrase from environ: got a refusal:\n%s", got)
	}
}

// The process's passphrase variable is not read: set, with none in the
// environ given, both keys refuse as missing their passphrase, and no line
// prints it. Not parallel: it sets the process's variable.
func TestValidateIgnoresHostPassphrase(t *testing.T) {
	t.Setenv("DITTOFS_ENCRYPTION_PASSPHRASE", passphrase)

	files := keyFiles(t)
	b := localKeys(handBuilt(), files[0], files[1])

	got := showReal(t, b, nil)

	const want = "environment: keys.protocol.local.file: the passphrase is unset\n" +
		"environment: keys.storage.local.file: the passphrase is unset"

	if got != want {
		t.Errorf("the host passphrase:\ngot:\n%s\nwant:\n%s", got, want)
	}

	if strings.Contains(got, passphrase) {
		t.Errorf("the report prints the passphrase:\n%s", got)
	}
}

// A key file that does not unwrap refuses with a fixed phrase that prints
// neither passphrase, nor the file's bytes.
func TestValidateWrongPassphrase(t *testing.T) {
	t.Parallel()

	files := keyFiles(t)
	b := localKeys(handBuilt(), files[0], files[1])

	got := showReal(t, b, []string{"DITTOFS_ENCRYPTION_PASSPHRASE=a-wrong-one"})

	const want = "environment: keys.protocol.local.file: the key file does not unwrap\n" +
		"environment: keys.storage.local.file: the key file does not unwrap"

	if got != want {
		t.Errorf("a wrong passphrase:\ngot:\n%s\nwant:\n%s", got, want)
	}

	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read key file: %v", err)
	}

	for _, secret := range []string{passphrase, "a-wrong-one", string(raw)} {
		if strings.Contains(got, secret) {
			t.Errorf("the report prints %q:\n%s", secret, got)
		}
	}
}

// The listeners off loopback need the certificate and its key, loopback
// read as RFC 23 §5.3 reads it.
func TestValidateCertificate(t *testing.T) {
	t.Parallel()

	both := []string{
		"P: api.tls.cert_file: required, api.listen has an address off loopback",
		"P: api.tls.key_file: required, api.listen has an address off loopback",
	}
	metrics := []string{
		"P: api.tls.cert_file: required, metrics.listen has an address off loopback",
		"P: api.tls.key_file: required, metrics.listen has an address off loopback",
	}

	for _, tc := range []struct {
		name string
		yaml string
		want []string
	}{
		{"every interface on IPv4", "api:\n  listen: [\"0.0.0.0:8080\"]\n", both},
		{"every interface on IPv6", "api:\n  listen: [\"[::]:8080\"]\n", both},
		{"a private address", "api:\n  listen: [\"10.0.0.5:8080\"]\n", both},
		{
			"an IPv4-mapped address off loopback",
			"api:\n  listen: [\"[::ffff:10.0.0.5]:8080\"]\n", both,
		},
		{
			"loopback beside another address",
			"api:\n  listen: [\"127.0.0.1:8080\", \"0.0.0.0:8081\"]\n", both,
		},
		{"a metrics address off loopback", "metrics:\n  listen: \"0.0.0.0:9090\"\n", metrics},
		{
			"the certificate only",
			"api:\n  listen: [\"0.0.0.0:8080\"]\n  tls:\n    cert_file: /a/c\n",
			both[1:],
		},
		{
			"the key only",
			"api:\n  listen: [\"0.0.0.0:8080\"]\n  tls:\n    key_file: /a/k\n",
			both[:1],
		},
		{
			"both files",
			"api:\n  listen: [\"0.0.0.0:8080\"]\n  tls:\n    cert_file: /a/c\n    key_file: /a/k\n",
			nil,
		},
		{"loopback of IPv4", "api:\n  listen: [\"127.0.0.1:8080\"]\n", nil},
		{"another address of 127.0.0.0/8", "api:\n  listen: [\"127.0.0.2:8080\"]\n", nil},
		{"loopback of IPv6", "api:\n  listen: [\"[::1]:8080\"]\n", nil},
		{"an IPv4-mapped loopback", "api:\n  listen: [\"[::ffff:127.0.0.1]:8080\"]\n", nil},
		{"metrics off", "metrics:\n  listen: off\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b, path := loadsFile(t, tc.yaml+fileBase, nil)

			got := show(t, b, nil, distinctKeys())

			if want := wantReport(path, tc.want...); got != want {
				t.Errorf("certificate:\ngot:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

// Load refuses a host name in api.listen with its own line, so it never
// reaches Validate through a load.
func TestValidateCertificateLoadRefusesHostName(t *testing.T) {
	t.Parallel()

	_, err := Load(writeFile(t, "api:\n  listen: [\"localhost:8080\"]\n"+fileBase), nil)

	if err == nil ||
		!strings.Contains(
			err.Error(),
			"api.listen: localhost:8080: the host is not an IP literal",
		) {
		t.Errorf("Load of a host name: got %v, want the host refusal", err)
	}
}

// A bootstrap built by hand can hold a host name, and localhost is not
// loopback to Validate: the certificate is required.
func TestValidateCertificateHostName(t *testing.T) {
	t.Parallel()

	b := handBuilt()
	b.API.Listen = []string{"localhost:8080"}

	const off = "required, api.listen has an address off loopback"

	const want = "environment: api.tls.cert_file: " + off + "\n" +
		"environment: api.tls.key_file: " + off

	if got := show(t, b, nil, distinctKeys()); got != want {
		t.Errorf("a host name:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// The journals list and the FoundationDB cluster file are required in
// Validate, not in Load.
func TestValidateRequired(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		set  func(*Bootstrap)
		want string
	}{
		{
			"no journals",
			func(b *Bootstrap) {
				b.Journals = nil
			},
			"environment: journals: required",
		},
		{"foundationdb with no cluster file", func(b *Bootstrap) {
			b.Metadata.Backend = "foundationdb"
		}, "environment: metadata.foundationdb.cluster_file: " +
			"required with metadata.backend: foundationdb"},
		{"foundationdb with one", func(b *Bootstrap) {
			b.Metadata.Backend = "foundationdb"
			b.Metadata.FoundationDB.ClusterFile = "/f.cluster"
		}, ""},
		{"badger with none", func(*Bootstrap) {
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b := handBuilt()
			tc.set(b)

			if got := show(t, b, nil, distinctKeys()); got != tc.want {
				t.Errorf("%s:\ngot:\n%s\nwant:\n%s", tc.name, got, tc.want)
			}
		})
	}
}

// A file with no journals loads: the rule is Validate's.
func TestLoadAllowsNoJournals(t *testing.T) {
	t.Parallel()

	_, err := Load(
		writeFile(t, "keys:\n  protocol:\n    kind: kmip\n  storage:\n    kind: kmip\n"),
		nil,
	)
	if err != nil {
		t.Errorf("Load of a file with no journals: %v", err)
	}
}

// Every refusal is a line of one error, sorted by where then key.
func TestValidateOneReport(t *testing.T) {
	t.Parallel()

	b := handBuilt()
	b.Node.Roles = []string{"storage"}
	b.Journals = nil
	b.API.Listen = []string{"0.0.0.0:8080"}

	const off = "required, api.listen has an address off loopback"

	const want = "environment: api.tls.cert_file: " + off + "\n" +
		"environment: api.tls.key_file: " + off + "\n" +
		"environment: journals: required\n" +
		"environment: node.roles: both roles are needed, \"protocol\" is missing"

	if got := show(t, b, nil, distinctKeys()); got != want {
		t.Errorf("one report:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// A default api.socket past the socket-address limit is refused naming the
// key: the socket could never bind.
func TestValidateSocketPastLimit(t *testing.T) {
	t.Parallel()

	environ := []string{"RUNTIME_DIRECTORY=/" + strings.Repeat("r", 120)}

	loaded, err := LoadWithSources("", append(
		environ,
		"DITTOFS_JOURNALS=/j/1",
		"DITTOFS_KEYS_PROTOCOL_KIND=kmip",
		"DITTOFS_KEYS_STORAGE_KIND=kmip",
	))
	if err != nil {
		t.Fatalf("load with no file: %v", err)
	}

	want := "environment: api.socket: longer than " + strconv.Itoa(socketLimit) +
		" bytes, the socket address limit"

	if got := show(t, &loaded.Bootstrap, environ, distinctKeys()); got != want {
		t.Errorf("a default socket past the limit:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// A state directory no variable names and no home gives is the temp
// directory's, which others may have written first: it is refused unless a
// variable names a directory. Not parallel: it clears the process's HOME.
func TestValidateRefusesUnnamedStateDir(t *testing.T) {
	t.Setenv("HOME", "")

	b := handBuilt()

	err := b.validate(context.Background(), nil, distinctKeys(), processUser())
	if err == nil || err.Error() != "environment: HOME: is not set" {
		t.Errorf("no variable names the state directory: got %v, want HOME is not set", err)
	}

	err = b.validate(context.Background(), []string{"HOME=/h"}, distinctKeys(), processUser())
	if err != nil {
		t.Errorf("HOME names the state directory: got %v, want no refusal", err)
	}
}

// Validate closes each provider it opens: both on success, both when the
// two files hold the same key, and the first when the second fails to load.
// The loader passes every load through to the real one and keeps what it
// returned; a closed local provider has zeroed its key, so it wraps nothing.
func TestValidateClosesProviders(t *testing.T) {
	t.Parallel()

	files := keyFiles(t)
	dir := t.TempDir()

	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read key file: %v", err)
	}

	copied := filepath.Join(dir, "copy.key")

	if err := os.WriteFile(copied, raw, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}

	missing := filepath.Join(dir, "missing.key")

	for _, tc := range []struct {
		name       string
		storage    string
		wantLoaded int
		wantErr    bool
	}{
		{"two keys", files[1], 2, false},
		{"the same key", copied, 2, true},
		{"the second fails to load", missing, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var loaded []keyprovider.KeyProvider

			load := func(
				ctx context.Context,
				cfg keyprovider.Config,
				passphrase string,
			) (keyprovider.KeyProvider, error) {
				p, err := keyprovider.NewProviderWithPassphrase(ctx, cfg, passphrase)
				if err == nil {
					loaded = append(loaded, p)
				}

				return p, err
			}

			b := localKeys(handBuilt(), files[0], tc.storage)

			if got := show(t, b, passphraseEnviron, load); (got != "") != tc.wantErr {
				t.Errorf("report %q, want an error: %t", got, tc.wantErr)
			}

			if len(loaded) != tc.wantLoaded {
				t.Fatalf("%d providers loaded, want %d", len(loaded), tc.wantLoaded)
			}

			for i, p := range loaded {
				if _, _, err := p.Wrap(context.Background(), []byte("block key")); err == nil {
					t.Errorf("provider %d still wraps after Validate, want it closed", i)
				}
			}
		})
	}
}

// The refusal of a key nobody set names the file the load read, and the
// environment when no file was read: a default file that is not there is
// not a place to look.
func TestValidateWhere(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	environ := []string{"HOME=" + home, "STATE_DIRECTORY=" + t.TempDir()}

	loaded, err := LoadWithSources("", environ)
	if err != nil {
		t.Fatalf("load with no default file: %v", err)
	}

	err = loaded.validate(context.Background(), environ, distinctKeys(), processUser())
	if err == nil || !strings.HasPrefix(err.Error(), "environment: journals: required") {
		t.Errorf("no default file: got %v, want lines of the environment", err)
	}

	dir := filepath.Join(home, ".config", "dittofs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("make the config directory: %v", err)
	}

	file := filepath.Join(dir, "config.yaml")
	content := []byte("node:\n  roles: [protocol, storage]\n")

	if err := os.WriteFile(file, content, 0o600); err != nil {
		t.Fatalf("write the default file: %v", err)
	}

	loaded, err = LoadWithSources("", environ)
	if err != nil {
		t.Fatalf("load with the default file: %v", err)
	}

	err = loaded.validate(context.Background(), environ, distinctKeys(), processUser())
	if err == nil || !strings.HasPrefix(err.Error(), file+": journals: required") {
		t.Errorf("an existing default file: got %v, want lines of %s", err, file)
	}

	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatalf("empty the default file: %v", err)
	}

	loaded, err = LoadWithSources("", environ)
	if err != nil {
		t.Fatalf("load with an empty default file: %v", err)
	}

	err = loaded.validate(context.Background(), environ, distinctKeys(), processUser())
	if err == nil || !strings.HasPrefix(err.Error(), file+": journals: required") {
		t.Errorf("an empty default file: got %v, want lines of %s", err, file)
	}
}

// A default socket of exactly the address limit binds and one byte more
// does not.
func TestValidateSocketLimitEdge(t *testing.T) {
	t.Parallel()

	const socketName = "/dfs.sock"

	for _, tc := range []struct {
		name string
		size int
		want string
	}{
		{"at the limit", socketLimit, ""},
		{"one past", socketLimit + 1, "environment: api.socket: longer than " +
			strconv.Itoa(socketLimit) + " bytes, the socket address limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			environ := []string{
				"RUNTIME_DIRECTORY=/" + strings.Repeat("r", tc.size-len(socketName)-1),
			}

			loaded, err := LoadWithSources("", append(environ,
				"DITTOFS_JOURNALS=/j/1",
				"DITTOFS_KEYS_PROTOCOL_KIND=kmip",
				"DITTOFS_KEYS_STORAGE_KIND=kmip",
			))
			if err != nil {
				t.Fatalf("load: %v", err)
			}

			if got := len(loaded.API.Socket); got != tc.size {
				t.Fatalf("default socket: %d bytes, want %d", got, tc.size)
			}

			if got := show(t, &loaded.Bootstrap, environ, distinctKeys()); got != tc.want {
				t.Errorf("socket of %d bytes:\ngot:\n%s\nwant:\n%s", tc.size, got, tc.want)
			}
		})
	}
}

// A relative state directory reaches Validate through a bootstrap built by
// hand, and is refused naming the variable, not read as a missing one.
func TestValidateRelativeStateDir(t *testing.T) {
	t.Parallel()

	err := handBuilt().validate(
		context.Background(), []string{"STATE_DIRECTORY=rel"}, distinctKeys(), processUser(),
	)

	const want = "environment: STATE_DIRECTORY: must be an absolute path"
	if err == nil || err.Error() != want {
		t.Errorf("a relative state directory: got %v, want %q", err, want)
	}
}

// A directory the host cannot report on is refused, not read as missing:
// the name holds a NUL, which no file system takes.
func TestValidateUnreadableStateDir(t *testing.T) {
	t.Parallel()

	environ := []string{"STATE_DIRECTORY=/s\x00x"}

	err := handBuilt().validate(context.Background(), environ, distinctKeys(), processUser())

	const want = "environment: STATE_DIRECTORY: cannot be read"
	if err == nil || err.Error() != want {
		t.Errorf("an unreadable state directory: got %v, want %q", err, want)
	}
}

// A runtime directory the host cannot report on is refused naming its
// variable, with the state directory valid.
func TestValidateUnreadableRuntimeDir(t *testing.T) {
	t.Parallel()

	environ := []string{"RUNTIME_DIRECTORY=/r\x00x"}

	err := handBuilt().validate(
		context.Background(),
		withStateDir(t, environ),
		distinctKeys(),
		processUser(),
	)

	const want = "environment: RUNTIME_DIRECTORY: cannot be read"
	if err == nil || err.Error() != want {
		t.Errorf("an unreadable runtime directory: got %v, want %q", err, want)
	}
}

// An address with no port is not loopback to Validate either: it cannot be
// told to be, and the certificate is required.
func TestValidateCertificateNoPort(t *testing.T) {
	t.Parallel()

	b := handBuilt()
	b.API.Listen = []string{"127.0.0.1"}

	const off = "required, api.listen has an address off loopback"

	const want = "environment: api.tls.cert_file: " + off + "\n" +
		"environment: api.tls.key_file: " + off

	if got := show(t, b, nil, distinctKeys()); got != want {
		t.Errorf("an address with no port:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// A key that does not load is refused at the key that names where it came
// from, with the phrase of its error and no text of the provider's own.
func TestValidateKeyLoadFailure(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		set  func(*Bootstrap)
		err  error
		want string
	}{
		{
			"a kmip key", func(*Bootstrap) {
			}, errors.New("keyprovider: dial 10.9.9.9: refused"),
			"environment: keys.protocol.kmip.endpoint: the key does not load\n" +
				"environment: keys.storage.kmip.endpoint: the key does not load",
		},
		{
			"a corrupt local file", func(b *Bootstrap) {
				localKeys(b, "/k/p.key", "/k/s.key")
			}, keyprovider.ErrKeyFileCorrupt,
			"environment: keys.protocol.local.file: the key file is corrupt\n" +
				"environment: keys.storage.local.file: the key file is corrupt",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b := handBuilt()
			tc.set(b)

			load := func(
				context.Context, keyprovider.Config, string,
			) (keyprovider.KeyProvider, error) {
				return nil, tc.err
			}

			if got := show(t, b, nil, load); got != tc.want {
				t.Errorf("key load failure:\ngot:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}
