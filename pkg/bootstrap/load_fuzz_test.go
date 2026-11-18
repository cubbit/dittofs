package bootstrap

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// fuzzSeeds are the files of the load tests' rows.
var fuzzSeeds = []string{
	"",
	"- a\n",
	"logging: debug\n",
	"logging: [s3cr3t\n",
	"logging:\n  level: debug\nlogging:\n  level: warn\n",
	"loging:\n  level: debug\nmetrics:\n  listen_address: x\n" +
		"adapters:\n  smb:\n    port: 445\njournals:\n  - path: data/j1\n",
	"LOGGING:\n  LEVEL: debug\n",
	"events:\n  path: /x\n",
	"adapters:\n  listen: [127.0.0.1]\n  nfs:\n    port: 2049\nidentity:\n  oidc:\n    issuer: x\n",
	"journals: \"/data/j1\"\n",
	"journals:\n  - path: /data/j1/\n",
	"journals:\n  - path: \"\"\n",
	"journals:\n  - path: /d/j1\n  - path: /d/j1\n",
	"logging:\n  rotation:\n    compress: \"t\"\n",
	"logging:\n  rotation:\n    compress: true\n    max_backups: 0\n",
	"logging:\n  format: 1\n",
	"logging:\n  rotation:\n    max_age: 7\n",
	"logging:\n  rotation:\n    max_age: \"-24h\"\n",
	"logging:\n  rotation:\n    max_age: \"48h0m\"\n",
	"logging:\n  rotation:\n    max_size: 1.5\n",
	"logging:\n  rotation:\n    max_size: \"1536KiB\"\n",
	"logging:\n  rotation:\n    max_size: \"1GiB\"\n",
	"logging:\n  rotation:\n    max_backups: s3cr3t\n",
	"logging:\n  rotation:\n    max_backups: -1\n",
	"logging:\n  level: INFO\n  output: logs/d.log\n",
	"logging:\n  level:\n",
	"node:\n  id: not-a-uuid\n  roles: [compute]\n",
	"keys:\n  protocol:\n    kind: aws\n",
	"keys:\n  protocol:\n    kind: kmip\n    local:\n      file: /k\n",
	"keys:\n  storage:\n    kind: local\n    kmip:\n      endpoint: x\n",
	"keys:\n  protocol:\n    kind: kmip\n    kmip:\n      timeout: \"1h30m\"\n      key_uid: u\n",
	"metadata:\n  badger:\n    path: ~/m\n",
	"metadata:\n  backend: sqlite\n  badger:\n    path: /m\n",
	"metadata:\n  foundationdb:\n    cluster_file: /c\n",
	"metadata:\n  backend: foundationdb\n  foundationdb:\n    cluster_file: /c\n" +
		"    tls:\n      cert_file: /t/c\n",
	"api:\n  tls:\n    key_file: \"-----BEGIN PRIVATE KEY-----\\nMIIBcanary\"\n",
	"api:\n  listen: [\":8080\", \"host.example:8080\", \"127.0.0.1:70000\"]\n",
	"api:\n  socket: /run/d/dfs.sock\n",
	"adapters:\n  listen: [\"127.0.0.1:80\"]\n",
	"metrics:\n  listen: \"off\"\n",
	"metrics:\n  listen:\n",
	"pasword: s3cr3t\n",
	"controlplane:\n  jwt:\n    secret: s3cr3t\n",
	"blockstore:\n  local:\n    path: /srv/blocks\n",
	"blockstore:\n  journal:\n    path: /b\n",
	"a: &x\n  b: *x\n",
	"logging:\n  level: debug\n---\nlogging:\n  level: s3cr3t\n",
	"\"a\\nenvironment: x\": 1\n",
	"api:\n  tls:\n    cert_file: MIIBcanary\n",
	"logging:\n  output: \"a\\nb\"\n",
	"logging: &x\n  rotation: *x\n",
}

// fuzzCanary is a variable's value no report may print.
const fuzzCanary = "CANARYenv7"

// fileCanary is the value of a credential key in a seed; no report may print
// it while it appears nowhere else in the file.
const fileCanary = "MIIBcanary"

// credentialKey reports whether key holds a path to key material, by RFC 13
// Appendix C's rows, written here apart from the loader's own table so a
// key the table forgets to mark is caught.
func credentialKey(key string) bool {
	switch key {
	case "metadata.foundationdb.tls.cert_file", "metadata.foundationdb.tls.key_file",
		"metadata.foundationdb.tls.ca_file", "api.tls.cert_file", "api.tls.key_file":
		return true
	}

	for _, role := range []string{"keys.protocol.", "keys.storage."} {
		switch strings.TrimPrefix(key, role) {
		case "local.file", "local.retired_files", "kmip.server_ca", "kmip.client_cert",
			"kmip.client_key":
			return strings.HasPrefix(key, role)
		}
	}

	return false
}

// onlyInCredentials reports whether every copy of canary in data is inside
// the value of a credential key, so a report printing it leaks that value.
// A copy elsewhere, in a key's name or another key's value, may be printed.
func onlyInCredentials(data []byte, canary string) bool {
	total := strings.Count(string(data), canary)
	if total == 0 {
		return false
	}

	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) != nil || len(doc.Content) == 0 {
		return false
	}

	inside := 0

	var visit func(n *yaml.Node, key string, depth int)

	visit = func(n *yaml.Node, key string, depth int) {
		if depth > 8 {
			return
		}

		switch n.Kind {
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				visit(n.Content[i+1], joinKey(key, n.Content[i].Value), depth+1)
			}
		case yaml.SequenceNode:
			for _, e := range n.Content {
				visit(e, key, depth+1)
			}
		case yaml.ScalarNode:
			if credentialKey(key) {
				inside += strings.Count(n.Value, canary)
			}
		}
	}
	visit(doc.Content[0], "", 0)

	return inside == total
}

// Any file loads without a panic into either a bootstrap or a report whose
// every line has RFC 13 §6's shape and prints no variable's value; a file
// that loads, written again in Appendix C's form, loads to the same
// bootstrap.
func FuzzLoad(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}

		_, err := Load(path, []string{"DITTOFS_LOGGING_LEVEL=" + fuzzCanary})
		if err == nil {
			t.Fatalf("Load with DITTOFS_LOGGING_LEVEL=%s: got no error", fuzzCanary)
		}

		for _, line := range strings.Split(err.Error(), "\n") {
			if !strings.HasPrefix(line, path+": ") &&
				!strings.HasPrefix(line, "environment: DITTOFS_") &&
				!strings.HasPrefix(line, "environment: XDG_STATE_HOME: ") &&
				!strings.HasPrefix(line, "environment: HOME: ") {
				t.Errorf("report line %q: neither the file's nor a variable's", line)
			}
		}

		if strings.Contains(err.Error(), fuzzCanary) {
			t.Errorf("report prints the variable's value:\n%s", err)
		}

		if onlyInCredentials(data, fileCanary) && strings.Contains(err.Error(), fileCanary) {
			t.Errorf("report prints a credential key's value:\n%s", err)
		}

		first, err := Load(path, nil)
		if err != nil {
			return
		}

		again := filepath.Join(t.TempDir(), "again.yaml")
		if err := os.WriteFile(again, encodeBootstrap(t, first), 0o600); err != nil {
			t.Fatal(err)
		}

		second, err := Load(again, nil)
		if err != nil {
			t.Fatalf("Load of the re-encoded file: %v\nfile:\n%s", err, encodeBootstrap(t, first))
		}

		if !reflect.DeepEqual(first, second) {
			t.Errorf("re-encoded load:\ngot  %+v\nwant %+v", second, first)
		}
	})
}

// encodeBootstrap writes b in RFC 13 Appendix C's form, independently of the
// loader's key table: a size as MiB, an age as hours, a duration as
// milliseconds, every other value as itself, leaving out an empty string, a
// zero identity, a zero duration and a nil list, as a file leaves out a key
// it does not set.
func encodeBootstrap(t *testing.T, b *Bootstrap) []byte {
	t.Helper()

	tree := map[string]any{}
	encodeStruct(reflect.ValueOf(*b), "", tree)

	out, err := yaml.Marshal(tree)
	if err != nil {
		t.Fatalf("encode the bootstrap: %v", err)
	}

	return out
}

func encodeStruct(v reflect.Value, prefix string, tree map[string]any) {
	for i := range v.NumField() {
		f := v.Type().Field(i)
		key := joinKey(prefix, f.Tag.Get("mapstructure"))
		fv := v.Field(i)

		switch val := fv.Interface().(type) {
		case entity.NodeID:
			if val != (entity.NodeID{}) {
				put(tree, key, uuid.UUID(val).String())
			}
		case time.Duration:
			if val != 0 {
				put(tree, key, strconv.FormatInt(val.Milliseconds(), 10)+"ms")
			}
		case []Journal:
			if val != nil {
				entries := make([]map[string]string, len(val))
				for j, e := range val {
					entries[j] = map[string]string{"path": e.Path}
				}

				put(tree, key, entries)
			}
		case []string:
			if val != nil {
				put(tree, key, val)
			}
		case string:
			if val != "" {
				put(tree, key, val)
			}
		case int:
			switch key {
			case "logging.rotation.max_size":
				put(tree, key, strconv.Itoa(val)+"MiB")
			case "logging.rotation.max_age":
				put(tree, key, strconv.Itoa(val*24)+"h")
			default:
				put(tree, key, val)
			}
		case bool:
			put(tree, key, val)
		default:
			switch {
			case fv.Kind() == reflect.Struct:
				encodeStruct(fv, key, tree)
			case fv.Kind() == reflect.String && fv.String() != "":
				put(tree, key, fv.String())
			}
		}
	}
}

// put sets the dotted key in tree, making the mappings on its way.
func put(tree map[string]any, key string, value any) {
	parts := strings.Split(key, ".")
	for _, p := range parts[:len(parts)-1] {
		next, ok := tree[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			tree[p] = next
		}

		tree = next
	}

	tree[parts[len(parts)-1]] = value
}
