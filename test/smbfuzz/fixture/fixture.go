//go:build linux

// Package fixture brings up RFC 31 §3's topology on this host: the AD-DC
// fixture's domain controller, a Samba member server as the reference, a
// DittoFS built from this checkout and joined to the domain, and the two
// kernel cifs mounts the fuzzer drives.
package fixture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cubbit/dittofs/pkg/apiclient"
	"github.com/cubbit/dittofs/test/smbfuzz/fuzz"
)

// Config chooses what the fixture builds.
type Config struct {
	State   string // state directory
	Repo    string // repository root, to build dfs and the DC image
	Auth    string // ntlm | krb5
	Profile string // sd | posix
	Log     io.Writer
}

const (
	network     = "smbfuzz"
	subnet      = "10.89.77.0/24"
	gatewayIP   = "10.89.77.1"
	dcIP        = "10.89.77.10"
	refIP       = "10.89.77.11"
	dcName      = "smbfuzz-dc"
	refName     = "smbfuzz-ref"
	refImage    = "smbfuzz-ref"
	dcImage     = "dittofs-ad-dc-test"
	realm       = "DITTOFS.AD"
	domain      = "DITTOFS"
	adminPass   = "Passw0rd!2024"
	userPass    = "TestPassword01!"
	machinePass = "MachinePass01!"
	dfsAdmin    = "adminpassword"
	smbPort     = 12445
	apiPort     = 18080
	shareName   = "profiles"
)

// Paths of the fixture's state.
func (c Config) envFile() string  { return filepath.Join(c.State, "env.json") }
func (c Config) refMount() string { return filepath.Join(c.State, "mnt", "ref") }
func (c Config) dutMount() string { return filepath.Join(c.State, "mnt", "dut") }
func (c Config) dfsDir() string   { return filepath.Join(c.State, "dfs") }
func (c Config) keytabs() string  { return filepath.Join(c.State, "keytabs") }

func (c Config) logf(format string, a ...any) { fmt.Fprintf(c.Log, "[fixture] "+format+"\n", a...) }

func run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func dexec(ctx context.Context, container string, args ...string) (string, error) {
	return run(ctx, "docker", append([]string{"exec", container}, args...)...)
}

// Users the fixture creates, by plan name, with their local uids
// (RFC 31 §4.2).
var users = []struct {
	plan, account string
	uid           int
}{
	{"admin", "Administrator", 20000},
	{"alice", "alice", 20001},
	{"bob", "bob", 20002},
	{"carol", "carol", 20003},
}

// Up builds everything and writes env.json.
func Up(ctx context.Context, c Config) (*fuzz.Env, error) {
	if err := os.MkdirAll(c.State, 0o755); err != nil {
		return nil, err
	}
	// A keytab or ready marker left by an earlier domain would be taken for
	// this one's: the DC entrypoint exports a keytab only when none exists.
	_ = os.RemoveAll(c.keytabs())
	if err := os.MkdirAll(c.keytabs(), 0o777); err != nil {
		return nil, err
	}
	c.logf("building the AD-DC image")
	if _, err := run(ctx, "docker", "build", "-q", "-t", dcImage, filepath.Join(c.Repo, "test/integration/ad-dc")); err != nil {
		return nil, err
	}
	_, _ = run(ctx, "docker", "network", "create", "--subnet", subnet, "--gateway", gatewayIP, network)

	c.logf("starting the domain controller")
	_, _ = run(ctx, "docker", "rm", "-f", dcName)
	if _, err := run(ctx, "docker", "run", "-d", "--privileged", "--name", dcName, "--hostname", "dc",
		"--network", network, "--ip", dcIP, "-v", c.keytabs()+":/keytabs", dcImage); err != nil {
		return nil, err
	}
	if err := waitFor(ctx, 5*time.Minute, func() bool {
		_, err := os.Stat(filepath.Join(c.keytabs(), ".ready"))
		return err == nil
	}); err != nil {
		return nil, fmt.Errorf("domain controller never became ready: %w", err)
	}
	accounts, domSID, err := c.populate(ctx)
	if err != nil {
		return nil, err
	}

	c.logf("building and starting the Samba reference")
	if _, err := run(ctx, "docker", "build", "-q", "-t", refImage, "-f",
		filepath.Join(c.Repo, "test/smbfuzz/fixture/Dockerfile.ref"), filepath.Join(c.Repo, "test/smbfuzz/fixture")); err != nil {
		return nil, err
	}
	_, _ = run(ctx, "docker", "rm", "-f", refName)
	if _, err := run(ctx, "docker", "run", "-d", "--privileged", "--name", refName, "--hostname", "ref",
		"--network", network, "--ip", refIP, "-e", "DC_IP="+dcIP, refImage); err != nil {
		return nil, err
	}
	if err := waitFor(ctx, 3*time.Minute, func() bool {
		_, err := dexec(ctx, refName, "test", "-f", "/srv/.ready")
		return err == nil
	}); err != nil {
		logs, _ := run(ctx, "docker", "logs", "--tail", "40", refName)
		return nil, fmt.Errorf("Samba reference never became ready: %w\n%s", err, logs)
	}

	c.logf("starting DittoFS")
	if err := c.startDFS(ctx); err != nil {
		return nil, err
	}
	if err := c.createShare(ctx); err != nil {
		return nil, err
	}

	c.logf("mounting both shares")
	if err := c.mount(ctx); err != nil {
		return nil, err
	}

	env := &fuzz.Env{
		RefRoot: c.refMount(), DutRoot: c.dutMount(),
		Domain: domain, Realm: realm, Auth: c.Auth, Profile: c.Profile,
		Users:     map[string]fuzz.UserCred{},
		DomainSID: domSID, Accounts: accounts,
	}
	for _, u := range users {
		pass := userPass
		if u.account == "Administrator" {
			pass = adminPass
		}
		env.Users[u.plan] = fuzz.UserCred{Account: u.account, Password: pass, UID: u.uid, GID: u.uid,
			Keytab: filepath.Join(c.keytabs(), u.account+".keytab")}
	}
	b, _ := json.MarshalIndent(env, "", "  ")
	if err := os.WriteFile(c.envFile(), b, 0o600); err != nil {
		return nil, err
	}
	return env, nil
}

// populate creates what the suite adds to the AD-DC fixture (RFC 31 §3.1)
// and returns every account's SID.
func (c Config) populate(ctx context.Context) (map[string]string, string, error) {
	tool := func(args ...string) (string, error) {
		return dexec(ctx, dcName, append([]string{"samba-tool"}, args...)...)
	}
	if _, err := tool("user", "show", "carol"); err != nil {
		if _, err := tool("user", "create", "carol", userPass); err != nil {
			return nil, "", err
		}
	}
	if _, err := tool("group", "show", "fuzzers"); err != nil {
		if _, err := tool("group", "add", "fuzzers"); err != nil {
			return nil, "", err
		}
	}
	_, _ = tool("group", "addmembers", "fuzzers", "alice,carol")
	_, _ = tool("dns", "add", "127.0.0.1", "dittofs.ad", "ref", "A", refIP, "-U", "Administrator%"+adminPass)
	_, _ = tool("dns", "add", "127.0.0.1", "dittofs.ad", "dittofs", "A", gatewayIP, "-U", "Administrator%"+adminPass)
	accounts := map[string]string{}
	var domSID string
	for _, name := range []string{"alice", "bob", "carol", "Administrator", "devs", "engineering", "fuzzers"} {
		kind := "user"
		if name == "devs" || name == "engineering" || name == "fuzzers" {
			kind = "group"
		}
		out, err := tool(kind, "show", name, "--attributes=objectSid")
		if err != nil {
			return nil, "", err
		}
		for _, line := range strings.Split(out, "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "objectSid: "); ok {
				accounts[name] = v
				if domSID == "" {
					domSID = v[:strings.LastIndex(v, "-")]
				}
			}
		}
		if accounts[name] == "" {
			return nil, "", fmt.Errorf("no objectSid for %s", name)
		}
	}
	for _, u := range users {
		kt := "/keytabs/" + u.account + ".keytab"
		_, _ = dexec(ctx, dcName, "rm", "-f", kt)
		if _, err := tool("domain", "exportkeytab", kt, "--principal="+u.account+"@"+realm); err != nil {
			return nil, "", err
		}
		_, _ = dexec(ctx, dcName, "chmod", "0644", kt)
	}
	return accounts, domSID, nil
}

func (c Config) dfsConfig() string {
	return fmt.Sprintf(`logging:
  level: INFO
  format: text
  output: stdout
controlplane:
  port: %d
  jwt:
    secret: "smbfuzz-jwt-secret-key-that-is-long-enough-32"
database:
  type: sqlite
  sqlite:
    path: "%s/dittofs.db"
blockstore:
  journal:
    path: "%s/blocks"
ldap:
  enabled: true
  url: "ldaps://%s:636"
  base_dn: "DC=dittofs,DC=ad"
  bind_dn: "CN=Administrator,CN=Users,DC=dittofs,DC=ad"
  bind_password: %q
  user_attr: sAMAccountName
  realm: %q
  idmap: rfc2307
  nested_groups: true
  timeout: 10s
  tls:
    insecure_skip_verify: true
kerberos:
  enabled: %t
  keytab_path: "%s/dittofs.keytab"
  service_principal: "cifs/dittofs.dittofs.ad@%s"
  realm: %q
  netbios_domain: %q
  krb5_conf: "%s/krb5.conf"
  machine_account:
    enabled: true
    account_name: "dittofs$"
    secret: %q
    dc_address:
      - %q
`, apiPort, c.dfsDir(), c.dfsDir(), dcIP, adminPass, realm, c.Auth == "krb5", c.keytabs(), realm, realm, domain, c.keytabs(), machinePass, dcIP)
}

func (c Config) startDFS(ctx context.Context) error {
	_ = c.stopDFS()
	_ = os.RemoveAll(c.dfsDir())
	if err := os.MkdirAll(c.dfsDir(), 0o755); err != nil {
		return err
	}
	bin := filepath.Join(c.State, "dfs-bin")
	build := exec.CommandContext(ctx, "go", "build", "-o", bin, "./cmd/dfs/")
	build.Dir = c.Repo
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("build dfs: %v\n%s", err, out)
	}
	cfg := filepath.Join(c.dfsDir(), "config.yaml")
	if err := os.WriteFile(cfg, []byte(c.dfsConfig()), 0o600); err != nil {
		return err
	}
	logPath := filepath.Join(c.State, "dfs.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, "start", "--foreground", "--config", cfg, "--pid-file", filepath.Join(c.State, "dfs.pid"))
	cmd.Env = append(os.Environ(), "DITTOFS_ADMIN_INITIAL_PASSWORD="+dfsAdmin)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = os.WriteFile(filepath.Join(c.State, "dfs.pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
	go func() { _ = cmd.Wait(); _ = logFile.Close() }()
	return waitFor(ctx, 30*time.Second, func() bool {
		_, err := c.api()
		return err == nil
	})
}

func (c Config) stopDFS() error {
	b, err := os.ReadFile(filepath.Join(c.State, "dfs.pid"))
	if err != nil {
		return err
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		for i := 0; i < 50; i++ {
			if syscall.Kill(pid, 0) != nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	return os.Remove(filepath.Join(c.State, "dfs.pid"))
}

func (c Config) api() (*apiclient.Client, error) {
	cli := apiclient.New(fmt.Sprintf("http://127.0.0.1:%d", apiPort))
	tok, err := cli.Login("admin", dfsAdmin)
	if err != nil {
		return nil, err
	}
	return cli.WithToken(tok.AccessToken), nil
}

// createShare creates the profiles share and turns on the SMB adapter.
func (c Config) createShare(ctx context.Context) error {
	cli, err := c.api()
	if err != nil {
		return err
	}
	if _, err := cli.GetMetadataStore("fuzz-meta"); err != nil {
		if _, err := cli.CreateMetadataStore(&apiclient.CreateStoreRequest{Name: "fuzz-meta", Type: "badger",
			Config: map[string]any{"db_path": filepath.Join(c.dfsDir(), "meta")}}); err != nil {
			return fmt.Errorf("create metadata store: %w", err)
		}
	}
	if _, err := cli.GetBlockStore("fuzz-block"); err != nil {
		if _, err := cli.CreateBlockStore(&apiclient.CreateStoreRequest{Name: "fuzz-block", Type: "memory"}); err != nil {
			return fmt.Errorf("create block store: %w", err)
		}
	}
	meta, err := cli.GetMetadataStore("fuzz-meta")
	if err != nil {
		return err
	}
	if _, err := cli.CreateShare(&apiclient.CreateShareRequest{Name: "/" + shareName, MetadataStoreID: meta.ID,
		BlockStore: "fuzz-block", DefaultPermission: "read-write"}); err != nil {
		return fmt.Errorf("create share: %w", err)
	}
	port, enabled := smbPort, true
	if _, err := cli.UpdateAdapter("smb", &apiclient.UpdateAdapterRequest{Port: &port, Enabled: &enabled}); err != nil {
		if _, cErr := cli.CreateAdapter(&apiclient.CreateAdapterRequest{Type: "smb", Port: port, Enabled: &enabled}); cErr != nil {
			return fmt.Errorf("SMB adapter: %v / %v", err, cErr)
		}
	}
	return waitFor(ctx, 20*time.Second, func() bool {
		_, err := run(ctx, "bash", "-c", fmt.Sprintf("exec 3<>/dev/tcp/127.0.0.1/%d", smbPort))
		return err == nil
	})
}

func (c Config) mountOpts(port int) string {
	o := []string{"vers=3.1.1", "multiuser", "sec=ntlmssp", "username=Administrator", "password=" + adminPass,
		"domain=" + domain, "noperm", "cache=none", "actimeo=0", "nolease", "nohandlecache",
		"rsize=1048576", "wsize=1048576", "serverino", "soft", "echo_interval=10", "nosharesock"}
	if c.Auth == "krb5" {
		o = append([]string{"vers=3.1.1", "multiuser", "sec=krb5", "cruid=0", "domain=" + domain}, o[6:]...)
	}
	if c.Profile == "posix" {
		o = append(o, "cifsacl")
	}
	if port != 445 {
		o = append(o, "port="+strconv.Itoa(port))
	}
	return strings.Join(o, ",")
}

// uncs returns both shares' UNC paths: by address under NTLM, by the name
// each server's principal carries under Kerberos.
func (c Config) uncs() (ref, dut string) {
	if c.Auth == "krb5" {
		return "//ref.dittofs.ad/" + shareName, "//dittofs.dittofs.ad/" + shareName
	}
	return "//" + refIP + "/" + shareName, "//127.0.0.1/" + shareName
}

func (c Config) mount(ctx context.Context) error {
	if c.Auth == "krb5" {
		if err := c.hostKerberos(); err != nil {
			return err
		}
		if err := c.localUsers(ctx); err != nil {
			return err
		}
		// The mount's own session is Administrator's, from root's cache.
		if err := (Hooks{C: c}).Ticket("Administrator", "/tmp/krb5cc_0"); err != nil {
			return err
		}
	}
	refUNC, dutUNC := c.uncs()
	for _, m := range []struct {
		dir, unc string
		port     int
	}{
		{c.refMount(), refUNC, 445},
		{c.dutMount(), dutUNC, smbPort},
	} {
		_, _ = run(ctx, "umount", "-l", m.dir)
		if err := os.MkdirAll(m.dir, 0o755); err != nil {
			return err
		}
		var err error
		for i := 0; i < 10; i++ {
			if _, err = run(ctx, "mount", "-t", "cifs", m.unc, m.dir, "-o", c.mountOpts(m.port)); err == nil {
				break
			}
			time.Sleep(2 * time.Second)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

const (
	hostsBegin = "# smbfuzz begin"
	hostsEnd   = "# smbfuzz end"
	krb5Conf   = "/etc/krb5.conf"
)

// hostKerberos makes the host's own Kerberos client able to reach the
// fixture's realm, which cifs.upcall needs: it reads only /etc/krb5.conf and
// resolves the servers' names, which must match their principals. It writes
// /etc/krb5.conf only when the host has none, and the names in a marked
// block of /etc/hosts; Down undoes both.
func (c Config) hostKerberos() error {
	if _, err := os.Stat(krb5Conf); errors.Is(err, os.ErrNotExist) {
		conf := fmt.Sprintf(`[libdefaults]
    default_realm = %[1]s
    dns_lookup_realm = false
    dns_lookup_kdc = false
    rdns = false
[realms]
    %[1]s = {
        kdc = %[2]s
    }
[domain_realm]
    .dittofs.ad = %[1]s
    dittofs.ad = %[1]s
`, realm, dcIP)
		if err := os.WriteFile(krb5Conf, []byte(conf), 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(c.State, "created-krb5.conf"), nil, 0o644); err != nil {
			return err
		}
	}
	removeHostsBlock()
	f, err := os.OpenFile("/etc/hosts", os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s\n%s ref.dittofs.ad\n127.0.0.1 dittofs.dittofs.ad\n%s dc.dittofs.ad\n%s\n",
		hostsBegin, refIP, dcIP, hostsEnd)
	return err
}

// localUsers creates a system user per worker uid: cifs.upcall looks the
// calling uid up before it reads its credential cache. Down removes them.
func (c Config) localUsers(ctx context.Context) error {
	for _, u := range users {
		name := "smbfuzz-" + u.plan
		if _, err := run(ctx, "getent", "passwd", name); err == nil {
			continue
		}
		if _, err := run(ctx, "groupadd", "-g", strconv.Itoa(u.uid), name); err != nil {
			return err
		}
		if _, err := run(ctx, "useradd", "-r", "-M", "-u", strconv.Itoa(u.uid), "-g", strconv.Itoa(u.uid),
			"-s", "/usr/sbin/nologin", name); err != nil {
			return err
		}
	}
	return nil
}

func removeLocalUsers(ctx context.Context) {
	for _, u := range users {
		name := "smbfuzz-" + u.plan
		_, _ = run(ctx, "userdel", name)
		_, _ = run(ctx, "groupdel", name)
	}
}

func removeHostsBlock() {
	b, err := os.ReadFile("/etc/hosts")
	if err != nil {
		return
	}
	var out []string
	skip := false
	for _, line := range strings.Split(string(b), "\n") {
		switch {
		case line == hostsBegin:
			skip = true
		case line == hostsEnd:
			skip = false
		case !skip:
			out = append(out, line)
		}
	}
	_ = os.WriteFile("/etc/hosts", []byte(strings.Join(out, "\n")), 0o644)
}

// Ticket gets a ticket for account with its keytab, using the domain
// controller's own kinit, and copies the credential cache to path.
func (h Hooks) Ticket(account, path string) error {
	ctx := context.Background()
	cc := "/keytabs/cc-" + account
	conf := fmt.Sprintf("[libdefaults]\n    default_realm = %[1]s\n    dns_lookup_kdc = false\n[realms]\n    %[1]s = {\n        kdc = %[2]s\n    }\n", realm, dcIP)
	if err := os.WriteFile(filepath.Join(h.C.keytabs(), "client-krb5.conf"), []byte(conf), 0o644); err != nil {
		return err
	}
	if _, err := run(ctx, "docker", "exec", "-e", "KRB5_CONFIG=/keytabs/client-krb5.conf", dcName, "kinit", "-k", "-t", "/keytabs/"+account+".keytab", "-c", "FILE:"+cc, account+"@"+realm); err != nil {
		return err
	}
	_, _ = dexec(ctx, dcName, "chmod", "0644", cc)
	b, err := os.ReadFile(filepath.Join(h.C.keytabs(), "cc-"+account))
	if err != nil {
		return err
	}
	// Removed first: in a sticky /tmp, protected_regular refuses even root an
	// O_CREAT open of a file another user owns.
	_ = os.Remove(path)
	return os.WriteFile(path, b, 0o600)
}

// Down removes everything Up made.
func Down(ctx context.Context, c Config) {
	removeHostsBlock()
	removeLocalUsers(ctx)
	if _, err := os.Stat(filepath.Join(c.State, "created-krb5.conf")); err == nil {
		_ = os.Remove(krb5Conf)
		_ = os.Remove(filepath.Join(c.State, "created-krb5.conf"))
	}
	for _, u := range users {
		_ = os.Remove(fmt.Sprintf("/tmp/krb5cc_%d", u.uid))
	}
	for _, d := range []string{c.refMount(), c.dutMount()} {
		_, _ = run(ctx, "umount", "-l", d)
	}
	_ = c.stopDFS()
	_, _ = run(ctx, "docker", "rm", "-f", refName, dcName)
	_, _ = run(ctx, "docker", "network", "rm", network)
}

// Load reads env.json written by Up.
func Load(c Config) (*fuzz.Env, error) {
	b, err := os.ReadFile(c.envFile())
	if err != nil {
		return nil, fmt.Errorf("no fixture (run `smbfuzz fixture up` first): %w", err)
	}
	var env fuzz.Env
	return &env, json.Unmarshal(b, &env)
}

// Hooks implements fuzz.Hooks for this fixture.
type Hooks struct{ C Config }

// CleanRef removes a round directory on the reference's own disk.
func (h Hooks) CleanRef(dir string) error {
	if strings.Contains(dir, "/") || strings.HasPrefix(dir, ".") || dir == "" {
		return errors.New("refusing to remove " + dir)
	}
	_, err := dexec(context.Background(), refName, "rm", "-rf", "/srv/fuzz/"+dir)
	return err
}

// ResetDut removes the DittoFS share and creates it again, then remounts.
func (h Hooks) ResetDut() error {
	ctx := context.Background()
	cli, err := h.C.api()
	if err != nil {
		return err
	}
	_, _ = run(ctx, "umount", "-l", h.C.dutMount())
	_ = cli.RemoveShare("/" + shareName)
	if err := h.C.createShare(ctx); err != nil {
		return err
	}
	_, dutUNC := h.C.uncs()
	if _, err := run(ctx, "mount", "-t", "cifs", dutUNC, h.C.dutMount(), "-o", h.C.mountOpts(smbPort)); err != nil {
		return err
	}
	return nil
}

// DutAlive reports whether dfs runs, with its log tail when it does not.
func (h Hooks) DutAlive() (bool, string) {
	b, err := os.ReadFile(filepath.Join(h.C.State, "dfs.pid"))
	if err == nil {
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		if pid > 0 && syscall.Kill(pid, 0) == nil {
			return true, ""
		}
	}
	tail, _ := run(context.Background(), "tail", "-n", "60", filepath.Join(h.C.State, "dfs.log"))
	return false, tail
}

func waitFor(ctx context.Context, d time.Duration, ok func() bool) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ok() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return errors.New("timed out")
}
