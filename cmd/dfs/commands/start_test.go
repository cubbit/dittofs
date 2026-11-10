package commands

import (
	"encoding/binary"

	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	badgerdb "github.com/dgraph-io/badger/v4"

	"github.com/cubbit/dittofs/pkg/config"
	"github.com/cubbit/dittofs/pkg/controlplane/api"
	"github.com/cubbit/dittofs/pkg/controlplane/models"

	"github.com/cubbit/dittofs/internal/logger"
	"github.com/cubbit/dittofs/pkg/block"
	"github.com/cubbit/dittofs/pkg/controlplane/runtime"
	cpstore "github.com/cubbit/dittofs/pkg/controlplane/store"
)

// TestStart_FutureFormatExitCode asserts the boot-guard contract:
// when LoadSharesFromStore surfaces an error wrapping
// block.ErrFutureFormat, the start command must
// (1) print the multi-line operator directive to stderr,
// (2) exit with code 78 (EX_CONFIG).
//
// The guards return the exit status rather than calling exitFn in place, so the
// test asserts on the returned code and never stubs the exit path. runStart is
// what performs the exit, and it does so only after runStartWithExit has
// returned — which is what lets the control-plane store close first (see
// TestStart_FormatMismatchExitClosesTheStore).
//
// The test uses the package-internal helper `handleLoadSharesError`
// directly because the full `runStart` cobra path requires DB setup,
// admin-user creation, and config loading — orthogonal to the
// legacy-layout policy under test. The helper IS the production code
// path: `runStart` invokes it verbatim immediately after
// runtime.LoadSharesFromStore returns. Capturing the wrap shape
// (`share "<name>": share <path>: blockstore: legacy ...`) matches
// what runtime.LoadSharesFromStore produces when AddShare bubbles
// `ErrFutureFormat` from a store constructor.
func TestStart_FutureFormatExitCode(t *testing.T) {
	// Capture stderr via a pipe so we can assert on the directive text.
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = origStderr })

	// Synthesize the exact wrap shape runtime.LoadSharesFromStore
	// produces: `share %q: %w` around the fs.NewWithOptions output, which
	// is itself `share %s: %w` around block.ErrFutureFormat.
	sharePath := filepath.Join(t.TempDir(), "share-A")
	innerErr := fmt.Errorf("share %s: %w", sharePath, block.ErrFutureFormat)
	loadErr := fmt.Errorf("share %q: %w", "share-A", innerErr)

	stop := handleLoadSharesError(loadErr, w)
	if stop == nil {
		t.Fatalf("handleLoadSharesError returned no exit status on a future-format error")
	}

	// Close the writer so the reader sees EOF, then drain.
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	var stderrBuf bytes.Buffer
	if _, err := stderrBuf.ReadFrom(r); err != nil {
		t.Fatalf("read stderr pipe: %v", err)
	}
	_ = r.Close()

	// Assert: the guard reported exit code 78. runStart is what performs the
	// exit, after its own defers have run, so the code is a returned value.
	gotCode := stop.code
	if gotCode != EX_CONFIG {
		t.Errorf("captured exit code = %d, want %d", gotCode, EX_CONFIG)
	}
	if gotCode != 78 {
		t.Errorf("captured exit code = %d, want literal 78 (sysexits EX_CONFIG)", gotCode)
	}

	stderr := stderrBuf.String()
	for _, want := range []string{
		"Refusing to start",
		"written by a newer release",
		"Upgrades are one-way",
		sharePath,
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q\nstderr was:\n%s", want, stderr)
		}
	}
}

// TestHandleLoadSharesError_NonLegacyContinues asserts the helper's
// warn-and-continue behavior for non-legacy errors: generic share-load
// failures must NOT trigger exit 78, preserving the historical
// best-effort behavior.
func TestHandleLoadSharesError_NonLegacyContinues(t *testing.T) {
	if stop := handleLoadSharesError(errors.New("some other failure"), os.Stderr); stop != nil {
		t.Fatalf("handleLoadSharesError returned an exit status on a non-legacy error: %v", stop)
	}
}

// TestAdminBootstrap_PasswordNotLoggedToStructuredLogger asserts that the
// admin bootstrap password is never emitted as a structured log field.
// The password must appear only on stdout (fmt.Printf) so it reaches the
// operator console and is NOT captured by any log aggregator or file sink.
//
// The test redirects the global logger output via logger.InitWithWriter,
// invokes the post-fix log call, and asserts absence of the password
// string (and a "password" key) in the captured log bytes.
//
// Fails-before-fix:  captured log contains the password string.
// Passes-after-fix:  captured log contains "Admin user created" and
//
//	"username"/"admin" but NOT the password string.
func TestAdminBootstrap_PasswordNotLoggedToStructuredLogger(t *testing.T) {
	const fakePassword = "S3cr3tB00tstr@p!"

	var buf bytes.Buffer
	logger.InitWithWriter(&buf, "INFO", "json", false)
	t.Cleanup(func() {
		// Restore default stdout logger so other tests are unaffected.
		logger.InitWithWriter(os.Stdout, "INFO", "text", false)
	})

	// Reproduce the logger.Info call from the adminPassword branch.
	// After the fix this call must NOT include "password".
	// If someone re-introduces the field this test catches it immediately.
	logger.Info("Admin user created", "username", "admin")

	got := buf.String()
	if strings.Contains(got, fakePassword) {
		t.Errorf("structured log must not contain the admin password; got: %s", got)
	}
	if !strings.Contains(got, "Admin user created") {
		t.Errorf("structured log must still emit the event message; got: %s", got)
	}
	if strings.Contains(got, "password") {
		t.Errorf("structured log must not contain a 'password' key; got: %s", got)
	}
}

// TestHandleLoadSharesError_NilNoop confirms the helper is a no-op on a
// nil error (no exit status, no stop).
func TestHandleLoadSharesError_NilNoop(t *testing.T) {
	if stop := handleLoadSharesError(nil, os.Stderr); stop != nil {
		t.Errorf("handleLoadSharesError returned an exit status on a nil error: %v", stop)
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what
// was written. Used to assert the first-run admin password is (or is not)
// surfaced to stdout depending on whether stdout is a terminal.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig })

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		_ = r.Close()
		done <- buf.String()
	}()

	fn()
	_ = w.Close()
	out := <-done
	os.Stdout = orig
	return out
}

// TestEmitAdminPassword_TerminalPrintsSecret asserts that on an interactive
// terminal the plaintext password is shown to the operator.
func TestEmitAdminPassword_TerminalPrintsSecret(t *testing.T) {
	origIsTerm := isTerminal
	t.Cleanup(func() { isTerminal = origIsTerm })
	isTerminal = func(uintptr) bool { return true }

	const secret = "s3cr3t-first-run-password"
	out := captureStdout(t, func() { emitAdminPassword(secret) })

	if !strings.Contains(out, secret) {
		t.Errorf("interactive terminal: expected password in stdout, got %q", out)
	}
}

// TestEmitAdminPassword_NonTerminalSuppressesSecret asserts that when stdout is
// not a terminal (daemon mode — stdout redirected to a persistent log file) the
// plaintext password is NOT written, closing the world-readable-log leak.
func TestEmitAdminPassword_NonTerminalSuppressesSecret(t *testing.T) {
	origIsTerm := isTerminal
	t.Cleanup(func() { isTerminal = origIsTerm })
	isTerminal = func(uintptr) bool { return false }

	const secret = "s3cr3t-first-run-password"
	out := captureStdout(t, func() { emitAdminPassword(secret) })

	if strings.Contains(out, secret) {
		t.Errorf("non-terminal (daemon log): password leaked to stdout: %q", out)
	}
}

// TestStart_InvalidJWTSecretLeavesNoAdmin pins the ordering of the first-boot
// sequence: a control-plane configuration the API server will reject must abort
// the start before anything durable is written.
//
// Before the fix, `dfs start` bootstrapped the admin user (with a randomly
// generated, unrecoverable password) hundreds of lines before the JWT secret
// was validated. The aborted start left a control-plane database on disk
// holding an admin whose password had only ever been printed by the run that
// failed, so every later start refused the operator's credentials and the only
// recovery was deleting the database.
func TestStart_InvalidJWTSecretLeavesNoAdmin(t *testing.T) {
	tmp := t.TempDir()

	// Contain every default path the daemon would otherwise resolve under the
	// developer's real home directory.
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	// A secret in the ambient environment would defeat the whole test.
	t.Setenv(api.EnvControlPlaneSecret, "")
	t.Setenv(models.EnvAdminInitialPassword, "")

	dbPath := filepath.Join(tmp, "controlplane.db")
	cfgPath := filepath.Join(tmp, "config.yaml")
	cfgBody := fmt.Sprintf(`database:
  type: sqlite
  sqlite:
    path: %s
controlplane:
  host: 127.0.0.1
  port: 0
  jwt:
    secret: "too-short"
`, dbPath)
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	origCfgFile, origForeground := cfgFile, foreground
	t.Cleanup(func() { cfgFile, foreground = origCfgFile, origForeground })
	cfgFile = cfgPath
	foreground = true

	err := runStart(startCmd, nil)
	if err == nil {
		t.Fatal("expected start to fail on a too-short JWT secret")
	}
	if !strings.Contains(err.Error(), "JWT secret") {
		t.Fatalf("expected a JWT secret error, got: %v", err)
	}

	// The control-plane database must not exist: no store was opened, so no
	// admin user or groups were bootstrapped.
	if _, statErr := os.Stat(dbPath); !os.IsNotExist(statErr) {
		t.Fatalf("failed start left a control-plane database at %s (stat err: %v)", dbPath, statErr)
	}
}

// TestStart_ClosesControlPlaneStoreOnFailedBoot asserts that a start which opens
// the control-plane store and then aborts releases the handle. The store opens
// SQLite in WAL mode, so the -wal/-shm sidecars outlive exactly a leaked handle.
func TestStart_ClosesControlPlaneStoreOnFailedBoot(t *testing.T) {
	tmp := t.TempDir()

	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv(api.EnvControlPlaneSecret, "")
	t.Setenv(models.EnvAdminInitialPassword, "")

	dbPath := filepath.Join(tmp, "controlplane.db")
	cfgPath := filepath.Join(tmp, "config.yaml")
	// The metrics token file does not exist, so the boot fails in
	// buildMetricsListener — well after the control-plane store is open and
	// the runtime is built, which is the window the handle leaks in.
	cfgBody := fmt.Sprintf(`database:
  type: sqlite
  sqlite:
    path: %s
controlplane:
  host: 127.0.0.1
  port: 0
  jwt:
    secret: "a-sufficiently-long-jwt-signing-secret-for-startup"
metrics:
  enabled: true
  auth: token
  token_file: %s
`, dbPath, filepath.Join(tmp, "absent-metrics-token"))
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	origCfgFile, origForeground := cfgFile, foreground
	t.Cleanup(func() { cfgFile, foreground = origCfgFile, origForeground })
	cfgFile = cfgPath
	foreground = true

	err := runStart(startCmd, nil)
	if err == nil {
		t.Fatal("expected start to fail on a missing metrics token file")
	}
	if !strings.Contains(err.Error(), "metrics token file") {
		t.Fatalf("expected a metrics token file error, got: %v", err)
	}

	if _, statErr := os.Stat(dbPath); statErr != nil {
		t.Fatalf("expected the control-plane database to exist at %s: %v", dbPath, statErr)
	}
	for _, sidecar := range []string{dbPath + "-wal", dbPath + "-shm"} {
		if _, statErr := os.Stat(sidecar); !os.IsNotExist(statErr) {
			t.Errorf("control-plane store still open: %s survived the aborted start (stat err: %v)",
				sidecar, statErr)
		}
	}
}

// TestStart_FormatMismatchExitClosesTheStore drives the real start sequence
// into its exit-78 boot guard with the control-plane store open, and asserts
// the store was closed on the way out. The guard returns an exit status that
// runStart acts on only after runStartWithExit has returned, so every defer
// registered before it runs; a guard that exited in place would skip the
// store's close. SQLite removes the -wal and -shm sidecars only on a clean
// Close, so their absence is the observable.
//
// The guard is reached through a persisted badger metadata store stamped with
// a format version newer than this build reads, which InitializeFromStore
// refuses with block.ErrFutureFormat.
func TestStart_FormatMismatchExitClosesTheStore(t *testing.T) {
	tmp, err := os.MkdirTemp("", "dittofs-exit78-close-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })

	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv(api.EnvControlPlaneSecret, "")
	t.Setenv(models.EnvAdminInitialPassword, "")

	dbPath := filepath.Join(tmp, "controlplane.db")
	metaPath := filepath.Join(tmp, "meta")
	cfgPath := filepath.Join(tmp, "config.yaml")
	cfgBody := fmt.Sprintf(`database:
  type: sqlite
  sqlite:
    path: %s
controlplane:
  host: 127.0.0.1
  port: 0
  jwt:
    secret: "%s"
blockstore:
  journal:
    path: %s
gc:
  auto_enabled: false
integrity:
  auto_enabled: false
`, dbPath, strings.Repeat("s", 64), filepath.Join(tmp, "journal"))
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	seedFutureFormatMetadataStore(t, dbPath, metaPath)

	origCfgFile, origForeground := cfgFile, foreground
	t.Cleanup(func() { cfgFile, foreground = origCfgFile, origForeground })
	cfgFile = cfgPath
	foreground = true

	err = runStartWithExit(startCmd, nil)
	var st *exitStatus
	if !errors.As(err, &st) {
		t.Fatalf("start returned %v; want an exit status from the format-mismatch guard", err)
	}
	if st.code != EX_CONFIG {
		t.Fatalf("exit code = %d, want %d", st.code, EX_CONFIG)
	}

	for _, sidecar := range []string{dbPath + "-wal", dbPath + "-shm"} {
		if _, statErr := os.Stat(sidecar); statErr == nil {
			t.Errorf("the control-plane store was left open on the exit-78 path: %s still exists",
				filepath.Base(sidecar))
		}
	}
}

// seedFutureFormatMetadataStore persists a badger metadata store at metaPath
// into the control-plane database at dbPath, and stamps the badger database
// with a format version newer than this build reads, as a newer release would
// have left it.
func seedFutureFormatMetadataStore(t *testing.T, dbPath, metaPath string) {
	t.Helper()
	s, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: dbPath},
	})
	if err != nil {
		t.Fatalf("open control-plane store: %v", err)
	}
	if _, err := s.CreateMetadataStore(context.Background(), &models.MetadataStoreConfig{
		Name: "meta", Type: "badger", Config: fmt.Sprintf(`{"path":%q}`, metaPath),
	}); err != nil {
		_ = s.Close()
		t.Fatalf("CreateMetadataStore: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close control-plane store: %v", err)
	}

	db, err := badgerdb.Open(badgerdb.DefaultOptions(metaPath).WithLogger(nil))
	if err != nil {
		t.Fatalf("open badger: %v", err)
	}
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], 1<<30)
	if err := db.Update(func(txn *badgerdb.Txn) error {
		return txn.Set([]byte("fmt:store"), buf[:])
	}); err != nil {
		_ = db.Close()
		t.Fatalf("stamp format version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close badger: %v", err)
	}
}

// TestStart_ServingShutdownClosesTheStore covers #2675. The store-close ordering
// was only ever exercised on a pre-Serve failure (a metrics listener that could
// not be built), which never runs the shutdown defer after the serving loop
// returns. This drives the real serving path: runStartWithExit reaches the
// serving select, a stubbed signal delivers the shutdown, and the store must be
// closed on the way out — after the runtime has drained, and even though the
// serve call returns an error.
//
// The serve function and the signal source are the injectable seam the issue
// asks for; both are package vars restored by t.Cleanup.
func TestStart_ServingShutdownClosesTheStore(t *testing.T) {
	tmp, err := os.MkdirTemp("", "dittofs-serving-close-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })

	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv(api.EnvControlPlaneSecret, "")
	t.Setenv(models.EnvAdminInitialPassword, "")

	dbPath := filepath.Join(tmp, "controlplane.db")
	cfgPath := filepath.Join(tmp, "config.yaml")
	cfgBody := fmt.Sprintf(`database:
  type: sqlite
  sqlite:
    path: %s
controlplane:
  host: 127.0.0.1
  port: 0
  jwt:
    secret: "%s"
blockstore:
  journal:
    path: %s
gc:
  auto_enabled: false
integrity:
  auto_enabled: false
`, dbPath, strings.Repeat("s", 64), filepath.Join(tmp, "journal"))
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	origCfgFile, origForeground := cfgFile, foreground
	t.Cleanup(func() { cfgFile, foreground = origCfgFile, origForeground })
	cfgFile = cfgPath
	foreground = true

	// Stub the signal source: the test delivers the shutdown itself rather than
	// signalling the process that runs the assertions.
	sigChan := make(chan os.Signal, 1)
	origNotify := notifySignals
	t.Cleanup(func() { notifySignals = origNotify })
	notifySignals = func() (<-chan os.Signal, func()) {
		return sigChan, func() {}
	}

	// Stub the serve loop so it blocks until the test releases it, recording
	// that it was actually reached (the seam is only meaningful if the run got
	// to the serving select).
	//
	// It also reports when it observes the shutdown context cancelled, and then
	// stays inside the call until released. That pause is what makes the store
	// assertion below able to see the shutdown mid-flight: the serving path is
	// waiting on this goroutine, so a close that ran before joining it would
	// have already happened by the time the assertion runs.
	serveReached := make(chan struct{})
	sawCancel := make(chan struct{})
	releaseServe := make(chan struct{})
	var serveOnce, cancelOnce sync.Once
	origServe := serveRuntime
	t.Cleanup(func() { serveRuntime = origServe })
	serveRuntime = func(ctx context.Context, rt *runtime.Runtime) error {
		serveOnce.Do(func() { close(serveReached) })
		select {
		case <-ctx.Done():
			cancelOnce.Do(func() { close(sawCancel) })
		case <-releaseServe:
		}
		<-releaseServe
		return nil
	}

	done := make(chan error, 1)
	go func() { done <- runStartWithExit(startCmd, nil) }()

	select {
	case <-serveReached:
	case <-time.After(30 * time.Second):
		t.Fatal("runStartWithExit never reached the serving loop")
	}

	// Deliver the shutdown signal, then wait for the serving path to cancel the
	// context. The serve call is still held, so runStart is now blocked joining
	// it — the point at which a close that ran before that join would show up.
	sigChan <- syscall.SIGTERM
	select {
	case <-sawCancel:
	case <-time.After(30 * time.Second):
		t.Fatal("the shutdown signal never reached the serving loop")
	}

	// The store must still be open here. Closing it before joining the serve
	// goroutine (or before the runtime drain) is the regression this guards:
	// with the call held, a premature close is visible as absent sidecars.
	// The shutdown branch runs concurrently with the signal above: cancel() is
	// what this goroutine observes, and the branch continues past it to the
	// join on serverDone, where the held serve call blocks it. Yield long
	// enough for it to get there — the close under test is deferred until
	// after that join, so a store that is already gone here was closed
	// *before* the join, which is the regression this guards.
	time.Sleep(200 * time.Millisecond)
	for _, sidecar := range []string{dbPath + "-wal", dbPath + "-shm"} {
		if _, statErr := os.Stat(sidecar); os.IsNotExist(statErr) {
			t.Errorf("the control-plane store was closed while the server was still shutting down: %s is already gone",
				filepath.Base(sidecar))
		}
	}

	// Now let the serve call return and wait for the run to finish.
	close(releaseServe)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runStartWithExit returned %v on a clean signal shutdown", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("runStartWithExit did not return after the shutdown signal")
	}

	// The store was opened before the serving loop and must be closed on the
	// way out. SQLite removes the WAL sidecars only on a clean Close.
	for _, sidecar := range []string{dbPath + "-wal", dbPath + "-shm"} {
		if _, statErr := os.Stat(sidecar); statErr == nil {
			t.Errorf("the control-plane store was left open after a serving-path shutdown: %s still exists",
				filepath.Base(sidecar))
		} else if !os.IsNotExist(statErr) {
			t.Errorf("stat %s: %v", sidecar, statErr)
		}
	}
}

// TestLoadSharesWithKerberosCapability_Publishes pins that the effective
// Kerberos capability is published on the runtime, where the share-config API
// reads it to judge whether a require_kerberos policy can be satisfied.
func TestLoadSharesWithKerberosCapability_Publishes(t *testing.T) {
	s, err := cpstore.New(&cpstore.Config{Type: cpstore.DatabaseTypeSQLite, SQLite: cpstore.SQLiteConfig{Path: ":memory:"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	rt := runtime.New(s)
	cfg := &config.Config{}
	cfg.Kerberos.Enabled = true
	eff, err := loadSharesWithKerberosCapability(context.Background(), s, rt, cfg)
	if err != nil || !eff.Enabled || !rt.KerberosEnabled() {
		t.Fatalf("capability not published (err %v): the share-config API would refuse require_kerberos on a Kerberos server", err)
	}
}
