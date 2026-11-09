package fixture

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cubbit/dittofs/pkg/apiclient"
	"github.com/cubbit/dittofs/test/harness/registry"
)

// fakeAPI answers the calls the fixture makes as the server would, and
// records each, with the stores it creates.
type fakeAPI struct {
	mu     sync.Mutex
	calls  []string
	stores map[string]map[string]any // the config of each store, by its path
	shares []map[string]any          // each share created, as sent
	sick   string                    // when set, the block store's health check fails with it
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := r.Method + " " + r.URL.Path
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch {
	case r.URL.Path == "/api/v1/shares" && r.Method == http.MethodPost:
		f.shares = append(f.shares, body)
		call += fmt.Sprintf(" %v owner=%v", body["name"], body["owner"])
		_, _ = w.Write([]byte("{}"))
	case strings.HasPrefix(r.URL.Path, "/api/v1/store/") && r.Method == http.MethodPost:
		var config map[string]any
		_ = json.Unmarshal([]byte(fmt.Sprint(body["config"])), &config)
		f.stores[r.URL.Path] = config
		call += fmt.Sprintf(" %v", body["type"])
		_, _ = w.Write([]byte("{}"))
	case r.URL.Path == "/api/v1/store/block/default/health":
		_ = json.NewEncoder(w).Encode(apiclient.BlockStoreHealthResult{Healthy: f.sick == "", Details: f.sick})
	case r.URL.Path == "/api/v1/auth/login":
		_, _ = w.Write([]byte(`{"access_token": "t"}`))
	case strings.HasPrefix(r.URL.Path, "/api/v1/users"):
		call += fmt.Sprintf(" %v uid=%v gid=%v", body["username"], body["uid"], body["gid"])
		_, _ = w.Write([]byte("{}"))
	case strings.Contains(r.URL.Path, "/permissions/users/"):
		call += fmt.Sprintf(" %v", body["level"])
	case r.URL.Path == "/api/v1/identity-mappings":
		call += fmt.Sprintf(" %v %v to %v", body["provider_name"], body["principal"], body["username"])
		_, _ = w.Write([]byte("{}"))
	default:
		_, _ = w.Write([]byte("{}"))
	}
	f.calls = append(f.calls, call)
}

func newFakeAPI(t *testing.T) (*fakeAPI, *apiclient.Client, string) {
	t.Helper()
	f := &fakeAPI{stores: map[string]map[string]any{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := apiclient.New(srv.URL)
	c.SetToken("t")
	return f, c, srv.URL
}

// testServer is a fixture f with its API at url and its data in /data, serving
// NFS on nfsPort and SMB on smbPort when they aren't 0, with the default
// share.
func testServer(f registry.Fixture, url string, nfsPort, smbPort int) *Server {
	return &Server{f: f, api: url, admin: "secret", data: "/data", shares: []apiclient.CreateShareRequest{{Name: Share}}, nfsPort: nfsPort, smbPort: smbPort, out: io.Discard}
}

// The stores, the users, then the share, its squash and the users' grants go
// in. The check then makes an authenticated call.
func TestConfigureOrder(t *testing.T) {
	f, c, url := newFakeAPI(t)
	s := testServer(registry.Fixture{Profile: "badger", NFS: registry.NFS{Version: "export"}, Squash: "none", Users: []string{"65532", "1"}}, url, 40001, 0)
	if err := s.configure(c, nil, "pw"); err != nil {
		t.Fatal(err)
	}
	if err := s.check(c, nil); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"POST /api/v1/store/metadata badger",
		"POST /api/v1/store/block memory",
		"POST /api/v1/users dt-65532 uid=65532 gid=65532",
		"POST /api/v1/users dt-1 uid=1 gid=1",
		"POST /api/v1/users dt-user uid=65535 gid=65535",
		"POST /api/v1/users dt-user2 uid=65536 gid=65536",
		"POST /api/v1/shares /export owner=<nil>",
		"PATCH /api/v1/shares/export/adapters/nfs/config",
		"PUT /api/v1/shares/export/permissions/users/dt-65532 read-write",
		"PUT /api/v1/shares/export/permissions/users/dt-1 read-write",
		"GET /api/v1/auth/me",
	}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
}

// A shares file's shares are each created with their own settings, owned by
// {user}, who gets the admin grant on each, and the check then makes an
// authenticated call.
func TestConfigureSMB(t *testing.T) {
	f, c, url := newFakeAPI(t)
	s := testServer(registry.Fixture{Profile: "memory", SMB: registry.SMB{Enabled: true}, Shares: "shares.yaml"}, url, 0, 40002)
	on := true
	s.shares = []apiclient.CreateShareRequest{{Name: "/a"}, {Name: "/b", AccessBasedEnumeration: &on}}
	if err := s.configure(c, nil, "pw"); err != nil {
		t.Fatal(err)
	}
	if err := s.check(c, nil); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"POST /api/v1/store/metadata badger",
		"POST /api/v1/store/block memory",
		"POST /api/v1/users dt-user uid=65535 gid=65535",
		"POST /api/v1/users dt-user2 uid=65536 gid=65536",
		"POST /api/v1/shares /a owner=dt-user",
		"PUT /api/v1/shares/a/permissions/users/dt-user admin",
		"POST /api/v1/shares /b owner=dt-user",
		"PUT /api/v1/shares/b/permissions/users/dt-user admin",
		"GET /api/v1/auth/me",
	}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	if f.shares[1]["access_based_enumeration"] != true {
		t.Errorf("shares %v", f.shares)
	}
}

// A default share the fixture mounts over SMB, as {user}, gives {user} the
// read-write grant the mount needs, and no more: its root stays unowned.
func TestConfigureSMBMount(t *testing.T) {
	f, c, url := newFakeAPI(t)
	s := testServer(registry.Fixture{Profile: "memory", SMB: registry.SMB{Enabled: true, Mount: true}}, url, 0, 40002)
	if err := s.configure(c, nil, "pw"); err != nil {
		t.Fatal(err)
	}
	want := []string{"POST /api/v1/shares /export owner=<nil>", "PUT /api/v1/shares/export/permissions/users/dt-user read-write"}
	if i := slices.Index(f.calls, want[0]); i < 0 || i+len(want) > len(f.calls) || !slices.Equal(f.calls[i:i+len(want)], want) {
		t.Errorf("calls:\n%s\nwant, in a row:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
}

// With Kerberos, {user}'s principal maps to {user} once the test users are
// in, as the scripts' bootstrap maps it.
func TestConfigureKerberos(t *testing.T) {
	f, c, url := newFakeAPI(t)
	s := testServer(registry.Fixture{Profile: "memory", SMB: registry.SMB{Enabled: true}, Kerberos: true}, url, 0, 40002)
	if err := s.configure(c, nil, "pw"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"POST /api/v1/users dt-user uid=65535 gid=65535",
		"POST /api/v1/users dt-user2 uid=65536 gid=65536",
		"POST /api/v1/identity-mappings kerberos dt-user@DITTOFS.TEST to dt-user",
		"POST /api/v1/shares /export owner=<nil>",
	}
	if i := slices.Index(f.calls, want[0]); i < 0 || i+len(want) > len(f.calls) || !slices.Equal(f.calls[i:i+len(want)], want) {
		t.Errorf("calls:\n%s\nwant, in a row:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
}

// Reset removes the share it's given, a test's {share}, and creates it again
// as configure did, with its settings, owner and grant, and touches no other
// share. A share the fixture doesn't have isn't reset.
func TestReset(t *testing.T) {
	f, _, url := newFakeAPI(t)
	s := testServer(registry.Fixture{Profile: "memory", SMB: registry.SMB{Enabled: true}, Shares: "shares.yaml"}, url, 0, 40002)
	on := true
	s.shares = []apiclient.CreateShareRequest{{Name: "/a"}, {Name: "/b", EncryptData: true, StreamsDisabled: &on}}
	if err := s.Reset("b"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"POST /api/v1/auth/login",
		"DELETE /api/v1/shares/b",
		"POST /api/v1/shares /b owner=dt-user",
		"PUT /api/v1/shares/b/permissions/users/dt-user admin",
	}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	if sent := f.shares[0]; sent["encrypt_data"] != true || sent["streams_disabled"] != true {
		t.Errorf("the share recreated as %v", sent)
	}
	if err := s.Reset("c"); err == nil || len(f.calls) != len(want) {
		t.Errorf("a share the fixture lacks: %v, calls %q", err, f.calls)
	}
}

// A fixture that sets NFS or SMB adapter settings is refused before any call
// reaches the server, which serves no file protocol and could apply none.
func TestConfigureRefusesAdapterSettings(t *testing.T) {
	no := false
	for name, fx := range map[string]registry.Fixture{
		"lease":          {Profile: "memory", Lease: "30s"},
		"delegations":    {Profile: "memory", Delegations: &no},
		"rpcbind":        {Profile: "memory", RPCBind: "host"},
		"smb encryption": {Profile: "memory", SMB: registry.SMB{Enabled: true, Encryption: "preferred"}},
	} {
		t.Run(name, func(t *testing.T) {
			f, c, url := newFakeAPI(t)
			err := testServer(fx, url, 0, 0).configure(c, nil, "pw")
			if err == nil || !strings.Contains(err.Error(), "serves no file protocol") {
				t.Errorf("configure = %v, want the refusal", err)
			}
			if len(f.calls) != 0 {
				t.Errorf("calls made before the refusal: %q", f.calls)
			}
		})
	}
}

// self is the user dt runs as, in its own group, with a read-write grant like
// any user's.
func TestConfigureSelf(t *testing.T) {
	// A Windows process has no uid (os.Getuid is -1), and users: self runs on Linux only.
	if runtime.GOOS == "windows" {
		t.Skip("users: self runs on Linux only")
	}
	f, c, url := newFakeAPI(t)
	s := testServer(registry.Fixture{Profile: "memory", NFS: registry.NFS{Version: "export"}, Users: []string{"self"}}, url, 40001, 0)
	if err := s.configure(c, nil, "pw"); err != nil {
		t.Fatal(err)
	}
	self := fmt.Sprintf("dt-%d", os.Getuid())
	for _, call := range []string{
		fmt.Sprintf("POST /api/v1/users %s uid=%d gid=%d", self, os.Getuid(), os.Getgid()),
		"PUT /api/v1/shares/export/permissions/users/" + self + " read-write",
	} {
		if !slices.Contains(f.calls, call) {
			t.Errorf("no %q among the calls:\n%s", call, strings.Join(f.calls, "\n"))
		}
	}
}

// The badger-s3 profile keeps its metadata on disk and its blocks in the
// fixture's bucket, under its own prefix; the check then fails when the
// server's own check of that block store doesn't find it healthy.
func TestConfigureS3(t *testing.T) {
	f, c, url := newFakeAPI(t)
	b := &bucket{endpoint: "http://10.0.0.5:4566", prefix: "dt-0123456789abcdef/"}
	s := testServer(registry.Fixture{Profile: "badger-s3"}, url, 0, 0)
	if err := s.configure(c, b, "pw"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.calls, "POST /api/v1/store/block s3") {
		t.Errorf("calls %q", f.calls)
	}
	blocks, meta := f.stores["/api/v1/store/block"], f.stores["/api/v1/store/metadata"]
	if blocks["bucket"] != bucketName || blocks["prefix"] != b.prefix || blocks["endpoint"] != b.endpoint || meta["path"] != filepath.Join("/data", "metadata") {
		t.Errorf("block store %v, metadata store %v", blocks, meta)
	}
	if err := s.check(c, b); err != nil {
		t.Errorf("a healthy block store: %v", err)
	}
	f.sick = "S3 health check failed: NotFound"
	if err := s.check(c, b); err == nil || !strings.Contains(err.Error(), "can't reach the bucket dittofs-dt at http://10.0.0.5:4566: S3 health check failed: NotFound") {
		t.Errorf("an unhealthy block store: %v", err)
	}
}
