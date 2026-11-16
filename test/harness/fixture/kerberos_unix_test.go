//go:build !windows

package fixture

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeKDCDocker puts a docker first on the PATH whose run, of a KDC, writes
// the keytab and krb5.conf into the directory -v mounts, and its port and
// USER_PASSWORD into state, then runs until rm kills it, or with
// FAKE_KDC_FAIL exits at once. Each call's arguments go to state/calls.
func fakeKDCDocker(t *testing.T) (state string) {
	t.Helper()
	dir, state := t.TempDir(), t.TempDir()
	script := `#!/bin/sh
echo "$*" >> ` + state + `/calls
case $1 in
run) ;;
rm) kill "$(cat ` + state + `/pid)"; exit 0 ;;
*) exit 0 ;;
esac
while [ $# -gt 1 ]; do
	case $1 in
	-v) keytabs=${2%:/keytabs}; shift ;;
	-e) case $2 in KDC_PORT=*) port=${2#KDC_PORT=} ;; esac; shift ;;
	esac
	shift
done
[ -z "$FAKE_KDC_FAIL" ] || { echo "kdb5_util: broke"; exit 1; }
: > "$keytabs/krb5.conf"
: > "$keytabs/dittofs.keytab"
echo "$USER_PASSWORD" > ` + state + `/password
echo $$ > ` + state + `/pid
echo "$port" > ` + state + `/port
exec sleep 60
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_KDC_FAIL", "")
	return state
}

// waitFile waits up to a minute for path and returns what it holds, or "".
func waitFile(path string) string {
	for end := time.Now().Add(time.Minute); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			return strings.TrimSpace(string(data))
		}
	}
	return ""
}

// The KDC starts on the server's network, with the server's principal named
// after the host it is given and {user}'s, whose password reaches it through
// docker's environment, never on the command line, and is ready once it
// answers on its port; stopping it removes its container. A KDC that exits
// before it answers fails at once, and one that never answers is removed
// when ctx ends.
func TestStartKDC(t *testing.T) {
	for _, tc := range []struct {
		name          string
		answers, fail bool
	}{
		{name: "answers", answers: true},
		{name: "exits", fail: true},
		{name: "never answers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := fakeKDCDocker(t)
			if tc.fail {
				t.Setenv("FAKE_KDC_FAIL", "1")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			// Port 0 is one no dial reaches, so a KDC on it never answers.
			// A KDC that answers is a listener the test holds open from the
			// start, so no other process can take its port meanwhile.
			port := 0
			if tc.answers {
				l, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = l.Close() })
				port = l.Addr().(*net.TCPAddr).Port
			} else if !tc.fail {
				// The bounded wait is the assertion: a KDC that never
				// answers is removed when ctx ends.
				go func() {
					if waitFile(filepath.Join(state, "port")) != "" {
						cancel()
					}
				}()
			}
			out := t.TempDir()
			dir := filepath.Join(out, "kdc")
			password := secret()
			k, err := startKDC(ctx, dir, "kdc-host.test", password, port, out)
			if tc.fail {
				log, _ := os.ReadFile(filepath.Join(out, "kdc.log"))
				if err == nil || !strings.Contains(err.Error(), "the KDC exited before it answered (exit status 1)") || !strings.Contains(string(log), "kdb5_util: broke") {
					t.Errorf("startKDC: %v; kdc.log %q", err, log)
				}
				return
			}
			if tc.answers {
				if err != nil {
					t.Fatalf("startKDC: %v", err)
				}
				if got := k.principal(); got != "cifs/kdc-host.test" {
					t.Errorf("principal %q, want cifs/kdc-host.test", got)
				}
				// The listener answers before the fake runs; stop it once it
				// has, as a real KDC answers only then.
				waitFile(filepath.Join(state, "pid"))
				k.stop()
			} else if !errors.Is(err, context.Canceled) {
				t.Fatalf("startKDC: %v, want %v", err, context.Canceled)
			}
			data, _ := os.ReadFile(filepath.Join(state, "calls"))
			calls := strings.Split(strings.TrimSpace(string(data)), "\n")
			run := regexp.MustCompile(`^run --rm --name (dt-kdc-[0-9a-f]{8}) --network ` + regexp.QuoteMeta(Network()) +
				` -v ` + regexp.QuoteMeta(dir) + `:/keytabs -e KRB5_REALM=DITTOFS.TEST -e KDC_HOST=127.0.0.1 -e KDC_PORT=` + strconv.Itoa(port) +
				` -e DITTOFS_SPN=cifs/kdc-host.test -e DITTOFS_UID=` + strconv.Itoa(os.Getuid()) + ` -e DITTOFS_GID=` + strconv.Itoa(os.Getgid()) +
				` -e USER_PRINCIPAL=dt-user -e USER_PASSWORD dittofs-dt-kdc$`).FindStringSubmatch(calls[0])
			if run == nil || len(calls) != 2 || calls[1] != "rm -f "+run[1] {
				t.Fatalf("docker calls:\n%s", data)
			}
			if !ended(waitFile(filepath.Join(state, "pid"))) {
				t.Error("the KDC still runs")
			}
			if got := waitFile(filepath.Join(state, "password")); got != password || strings.Contains(string(data), password) {
				t.Errorf("USER_PASSWORD %q, want %q, off the command line:\n%s", got, password, data)
			}
		})
	}
}

// alive reports whether the process pid runs; one that ended and waits to be
// reaped doesn't.
func alive(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

// ended waits up to 10 seconds for the process pid to end.
func ended(pid string) bool {
	n, err := strconv.Atoi(pid)
	for end := time.Now().Add(10 * time.Second); err == nil && time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if !alive(n) {
			return true
		}
	}
	return false
}
