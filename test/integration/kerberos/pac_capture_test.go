//go:build kerberos

// Package kerberos_test captures a ticket from a real MIT KDC, built from
// Debian bookworm's krb5-kdc (krb5 1.20), as a vector identity's unit tests
// replay.
//
// Run with: go test -tags=kerberos -v -timeout 5m ./test/integration/kerberos/
// Requires: Docker
package kerberos_test

import (
	"bytes"
	"cmp"
	"crypto/hmac"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jcmturner/gokrb5/v8/client"
	krb5config "github.com/jcmturner/gokrb5/v8/config"
	"github.com/jcmturner/gokrb5/v8/crypto"
	"github.com/jcmturner/gokrb5/v8/iana/adtype"
	"github.com/jcmturner/gokrb5/v8/iana/keyusage"
	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/jcmturner/gokrb5/v8/messages"
	"github.com/jcmturner/gokrb5/v8/types"
)

const (
	realm     = "DITTOFS.TEST"
	alice     = "alice"
	alicePass = "alicepass"
	service   = "cifs/nas.dittofs.test"
)

// PAC buffer types (MS-PAC 2.4) the capture is checked for.
const (
	pacLogonInfo       = 1
	pacServerSignature = 6
	pacKDCSignature    = 7
)

// namePrefix starts the name of the image, container and network the test
// makes. Setting DITTOFS_KDC_NAME_PREFIX replaces it on a host whose Docker
// names are shared between users.
var namePrefix = strings.ToLower(cmp.Or(os.Getenv("DITTOFS_KDC_NAME_PREFIX"), "dittofs-kdc-test"))

// TestMITPACCapture builds alice's AP-REQ for cifs/ with gokrb5's client from
// the service ticket an MIT KDC issues her, and checks the ticket, decrypted
// with the exported service keytab, carries a PAC whose server signature
// verifies under the key that decrypted it and which holds no
// logon-information buffer: what krb5 1.20 puts in every ticket, and the path
// the vector exists to hold.
//
// With DITTOFS_MIT_PAC_OUT naming a directory, it also writes there
// mit-alice.apreq, mit-alice.json (the authenticator's time), mit-service.pac
// (the ticket's PAC) and mit-service.keytab.
//
// decision: the KDC runs only to capture. The adapter path a ticket takes is
// the same for every KDC and is held elsewhere; what only an MIT KDC shows is
// its PAC, and a committed vector holds that in every unit run, where a live
// KDC would hold it only in this cell. Overturned if identity ever treats an
// MIT ticket differently from what its AP-REQ alone shows.
func TestMITPACCapture(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping KDC integration test in short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found, skipping KDC integration test")
	}

	port := startKDC(t)
	dir := t.TempDir()
	ktPath := filepath.Join(dir, "service.keytab")
	dockerExec(t, "kadmin.local", "-q", fmt.Sprintf("addprinc -pw %s %s@%s", alicePass, alice, realm))
	dockerExec(t, "kadmin.local", "-q", fmt.Sprintf("addprinc -randkey %s@%s", service, realm))
	dockerExec(t, "kadmin.local", "-q", fmt.Sprintf("ktadd -k /tmp/service.keytab -e aes256-cts-hmac-sha1-96:normal,aes128-cts-hmac-sha1-96:normal %s@%s", service, realm))
	if out, err := docker("cp", container+":/tmp/service.keytab", ktPath); err != nil {
		t.Fatalf("copy the service keytab out of the KDC: %v\n%s", err, out)
	}
	kt, err := keytab.Load(ktPath)
	if err != nil {
		t.Fatalf("load the service keytab: %v", err)
	}

	confPath := filepath.Join(dir, "krb5.conf")
	conf := fmt.Sprintf("[libdefaults]\n default_realm = %s\n dns_lookup_realm = false\n dns_lookup_kdc = false\n udp_preference_limit = 1\n\n[realms]\n %s = {\n  kdc = 127.0.0.1:%d\n }\n", realm, realm, port)
	if err := os.WriteFile(confPath, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := krb5config.Load(confPath)
	if err != nil {
		t.Fatal(err)
	}
	cl := client.NewWithPassword(alice, realm, alicePass, cfg, client.DisablePAFXFAST(true))
	if err := cl.Login(); err != nil {
		t.Fatalf("kinit as alice: %v", err)
	}
	defer cl.Destroy()
	tkt, key, err := cl.GetServiceTicket(service)
	if err != nil {
		t.Fatalf("alice's service ticket for %s: %v", service, err)
	}
	if err := tkt.DecryptEncPart(kt, nil); err != nil {
		t.Fatalf("decrypt alice's %s ticket under the service keytab: %v", service, err)
	}
	svcKey, _, err := kt.GetEncryptionKey(tkt.SName, tkt.Realm, tkt.EncPart.KVNO, tkt.EncPart.EType)
	if err != nil {
		t.Fatalf("the service key of alice's ticket: %v", err)
	}
	raw := rawPAC(t, tkt)
	checkPAC(t, raw, svcKey)

	auth, err := types.NewAuthenticator(cl.Credentials.Domain(), cl.Credentials.CName())
	if err != nil {
		t.Fatal(err)
	}
	req, err := messages.NewAPReq(tkt, key, auth)
	if err != nil {
		t.Fatalf("alice's AP-REQ: %v", err)
	}
	apreq, err := req.Marshal()
	if err != nil {
		t.Fatalf("marshal alice's AP-REQ: %v", err)
	}

	out := os.Getenv("DITTOFS_MIT_PAC_OUT")
	if out == "" {
		return
	}
	meta, err := json.MarshalIndent(struct {
		Time time.Time `json:"time"`
	}{auth.CTime.Truncate(time.Microsecond)}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	ktBytes, err := os.ReadFile(ktPath)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"mit-alice.apreq": apreq, "mit-alice.json": append(meta, '\n'),
		"mit-service.pac": raw, "mit-service.keytab": ktBytes,
	} {
		if err := os.WriteFile(filepath.Join(out, name), data, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	t.Logf("wrote mit-alice.apreq, mit-alice.json, mit-service.pac (%d bytes) and mit-service.keytab to %s", len(raw), out)
}

// checkPAC checks raw's server signature verifies under key, the key that
// decrypted its ticket, with the server and KDC signatures zeroed (MS-PAC
// 2.8), and that raw holds no logon-information buffer.
func checkPAC(t *testing.T, raw []byte, key types.EncryptionKey) {
	t.Helper()
	if len(raw) < 8 {
		t.Fatalf("PAC of %d bytes, shorter than its header", len(raw))
	}
	n := binary.LittleEndian.Uint32(raw)
	if uint64(len(raw)) < 8+16*uint64(n) {
		t.Fatalf("PAC of %d bytes claims %d buffers", len(raw), n)
	}
	type buffer struct{ off, size uint64 }
	bufs := map[uint32]buffer{}
	var seen []uint32
	for i := range n {
		e := raw[8+16*i:]
		b := buffer{binary.LittleEndian.Uint64(e[8:]), uint64(binary.LittleEndian.Uint32(e[4:]))}
		typ := binary.LittleEndian.Uint32(e)
		if b.off+b.size > uint64(len(raw)) {
			t.Fatalf("PAC buffer %d lies past the PAC's end", typ)
		}
		seen = append(seen, typ)
		if _, dup := bufs[typ]; !dup {
			bufs[typ] = b
		}
	}
	if _, ok := bufs[pacLogonInfo]; ok {
		t.Errorf("the PAC holds buffer types %v, want no logon information (%d)", seen, pacLogonInfo)
	}
	srv, kdc := bufs[pacServerSignature], bufs[pacKDCSignature]
	if srv.size < 4 || kdc.size < 4 {
		t.Fatalf("the PAC holds buffer types %v, want a server (%d) and a KDC (%d) signature", seen, pacServerSignature, pacKDCSignature)
	}
	et, err := crypto.GetEtype(key.KeyType)
	if err != nil {
		t.Fatal(err)
	}
	if got := int32(binary.LittleEndian.Uint32(raw[srv.off:])); got != et.GetHashID() {
		t.Fatalf("server signature of type %d, want %d, the ticket key's", got, et.GetHashID())
	}
	zeroed := bytes.Clone(raw)
	for _, sig := range []buffer{srv, kdc} {
		clear(zeroed[sig.off+4 : sig.off+sig.size])
	}
	sum, err := et.GetChecksumHash(key.KeyValue, zeroed, keyusage.KERB_NON_KERB_CKSUM_SALT)
	if err != nil {
		t.Fatal(err)
	}
	if !hmac.Equal(sum, raw[srv.off+4:srv.off+srv.size]) {
		t.Errorf("the PAC's server signature does not verify under the key that decrypted its ticket")
	}
}

// rawPAC returns the PAC's bytes as the ticket carries them.
func rawPAC(t *testing.T, tkt messages.Ticket) []byte {
	t.Helper()
	for _, ad := range tkt.DecryptedEncPart.AuthorizationData {
		if ad.ADType != adtype.ADIfRelevant {
			continue
		}
		var inner types.AuthorizationData
		if err := inner.Unmarshal(ad.ADData); err != nil {
			t.Fatalf("unmarshal the ticket's AD-IF-RELEVANT: %v", err)
		}
		for _, e := range inner {
			if e.ADType == adtype.ADWin2KPAC {
				return e.ADData
			}
		}
	}
	t.Fatalf("the ticket carries no PAC")
	return nil
}

// container is the KDC container startKDC runs, private to this run.
var container string

// startKDC builds the image, runs the KDC on a network of its own with its
// port 88 published on loopback, and returns that port once a TCP connection
// to it is accepted and the realm's database answers kadmin.local.
func startKDC(t *testing.T) int {
	t.Helper()
	id := strings.ToLower(rand.Text()[:8])
	image, network := namePrefix+":"+id, namePrefix+"-"+id+"-net"
	container = namePrefix + "-" + id
	t.Cleanup(func() {
		for _, args := range [][]string{{"rm", "-f", container}, {"network", "rm", network}, {"rmi", image}} {
			if out, err := docker(args...); err != nil {
				t.Logf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
			}
		}
	})
	build := exec.Command("docker", "build", "-t", image, ".")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("docker build: %v", err)
	}
	if out, err := docker("network", "create", network); err != nil {
		t.Fatalf("docker network create: %v\n%s", err, out)
	}
	if out, err := docker("run", "-d", "--name", container, "--network", network,
		"-e", "KRB5_REALM="+realm, "-p", "127.0.0.1:0:88/tcp", image); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	out, err := docker("port", container, "88/tcp")
	if err != nil {
		t.Fatalf("docker port: %v\n%s", err, out)
	}
	_, p, err := net.SplitHostPort(strings.TrimSpace(strings.Split(string(out), "\n")[0]))
	if err != nil {
		t.Fatalf("parse the KDC's port from %q: %v", out, err)
	}
	var port int
	if _, err := fmt.Sscan(p, &port); err != nil {
		t.Fatalf("parse the KDC's port %q: %v", p, err)
	}
	// Polls the real condition: the entrypoint creates the database, then
	// execs krb5kdc.
	deadline := time.Now().Add(60 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", p), time.Second)
		if err == nil {
			c.Close()
			if _, err := docker("exec", container, "kadmin.local", "-q", "listprincs"); err == nil {
				return port
			}
		}
		if time.Now().After(deadline) {
			logs, _ := docker("logs", container)
			t.Fatalf("the KDC did not come up within 60s: %v\n%s", err, logs)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func docker(args ...string) ([]byte, error) {
	return exec.Command("docker", args...).CombinedOutput()
}

func dockerExec(t *testing.T, args ...string) {
	t.Helper()
	if out, err := docker(append([]string{"exec", container}, args...)...); err != nil {
		t.Fatalf("%s in the KDC: %v\n%s", strings.Join(args, " "), err, out)
	}
}
