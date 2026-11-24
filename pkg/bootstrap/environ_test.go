package bootstrap

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

// writeRule is the rule of the first password's v1 name.
const writeRule = "no longer read; pass dfs init --admin-password-env <name> " +
	"or --admin-password-file"

// testAllowedNames, testAllowedPrefixes and testProvisionalNames are the
// lists written apart from the ones the scan reads, so a name dropped from
// any of them must reach this file too.
var (
	testAllowedNames = []string{
		"DITTOFS_CONFIG",
		"DITTOFS_ENCRYPTION_PASSPHRASE",
		"DITTOFS_PASSWORD",
		"DITTOFS_REPO",
		"DITTOFS_FDB_CLUSTER_FILE",
		"DITTOFS_FDB_DIR",
		"DITTOFS_MOUNT",
		"DITTOFS_RESULTS_DIR",
		"DITTOFS_KNOWN_FAILURES",
		"DITTOFS_CLIENT_NS",
		"DITTOFS_SPN",
		"DITTOFS_UID",
		"DITTOFS_GID",
		"DITTOFS_BENCH_DIR",
		"DITTOFS_SKIP_BIGFILE",
		"DITTOFS_SKIP_BUILD",
		"DITTOFS_SKIP_COMMITMSG",
		"DITTOFS_SKIP_CONFLICT",
		"DITTOFS_SKIP_GOFMT",
		"DITTOFS_SKIP_LINT",
		"DITTOFS_SKIP_PREPUSH",
		"DITTOFS_SKIP_SHELLCHECK",
		"DITTOFS_SKIP_TIDY",
		"DITTOFS_SKIP_VET",
	}

	testAllowedPrefixes = []string{"DITTOFS_TEST_", "DITTOFS_E2E_", "DITTOFS_PREPUSH_"}

	testProvisionalNames = []string{"DITTOFS_OTLP_ENDPOINT"}
)

// Every entry of the v1 table refuses under its variable as it refuses in
// the file, the rule spelled in full here, and the alias the v1 loader bound
// by another name reads under its own rule too. No value reaches the
// report, and a name one letter off a family prefix is unknown. The bare
// family name takes the family's rule, as the bare key does in the file.
func TestEnvironmentRefusesV1Names(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		variable string
		rule     string
	}{
		{"DITTOFS_DATABASE_X", databaseKeyRule},
		{"DITTOFS_METADATA_BADGER_BLOCK_CACHE_MB", removedKeyRule},
		{"DITTOFS_METADATA_BADGER_INDEX_CACHE_MB", removedKeyRule},
		{"DITTOFS_CONTROLPLANE_TLS_CLIENT_CA", removedKeyRule},
		{"DITTOFS_CONTROLPLANE_TLS_MIN_VERSION", removedKeyRule},
		{"DITTOFS_CONTROLPLANE_REQUIRE_INITIAL_PASSWORD_CHANGE", removedKeyRule},
		{"DITTOFS_CONTROLPLANE_DRAIN_STALL_TIMEOUT", removedKeyRule},
		{"DITTOFS_CONTROLPLANE_READ_TIMEOUT", removedKeyRule},
		{"DITTOFS_CONTROLPLANE_WRITE_TIMEOUT", removedKeyRule},
		{"DITTOFS_CONTROLPLANE_IDLE_TIMEOUT", removedKeyRule},
		{"DITTOFS_CONTROLPLANE_HOST", "no longer read; write api.listen"},
		{"DITTOFS_CONTROLPLANE_PORT", "no longer read; write api.listen"},
		{"DITTOFS_CONTROLPLANE_TLS_CERT_FILE", "no longer read; write api.tls.cert_file"},
		{"DITTOFS_CONTROLPLANE_TLS_KEY_FILE", "no longer read; write api.tls.key_file"},
		{"DITTOFS_CONTROLPLANE_PPROF", removedKeyRule},
		{"DITTOFS_CONTROLPLANE_PPROF_MUTEX_RATE", removedKeyRule},
		{"DITTOFS_CONTROLPLANE_PPROF_BLOCK_RATE_NS", removedKeyRule},
		{"DITTOFS_CONTROLPLANE_JWT_X", removedKeyRule},
		{"DITTOFS_METRICS_AUTH", removedKeyRule},
		{"DITTOFS_METRICS_TOKEN_FILE", removedKeyRule},
		{"DITTOFS_METRICS_PATH", removedKeyRule},
		{"DITTOFS_METRICS_ENABLED", "no longer read; write metrics.listen"},
		{"DITTOFS_METRICS_HOST", "no longer read; write metrics.listen"},
		{"DITTOFS_METRICS_PORT", "no longer read; write metrics.listen"},
		{"DITTOFS_METRICS_TLS_X", removedKeyRule},
		{"DITTOFS_BLOCKSTORE_JOURNAL_PATH", "no longer read; write journals"},
		{"DITTOFS_BLOCKSTORE_LOCAL_PATH", "no longer read; write journals"},
		{"DITTOFS_BLOCKSTORE_JOURNAL_CHUNK_SIZE", removedKeyRule},
		{"DITTOFS_BLOCKSTORE_JOURNAL_CHUNK_MAX", removedKeyRule},
		{"DITTOFS_BLOCKSTORE_JOURNAL_DIRTY_EXPIRE", removedKeyRule},
		{"DITTOFS_BLOCKSTORE_JOURNAL_MAX_LOG_BYTES", removedKeyRule},
		{"DITTOFS_BLOCKSTORE_JOURNAL_BACKPRESSURE_MAX_WAIT", removedKeyRule},
		{
			"DITTOFS_SHUTDOWN_TIMEOUT",
			"no longer read; set adapters.<protocol>.drain_timeout through the management API",
		},
		{"DITTOFS_DATABASE", databaseKeyRule},
		{"DITTOFS_CONTROLPLANE_JWT", removedKeyRule},
		{"DITTOFS_METRICS_TLS", removedKeyRule},
		{"DITTOFS_ADMIN", familyKeyRule},
		{"DITTOFS_KERBEROS", familyKeyRule},
		{"DITTOFS_IDENTITY", familyKeyRule},
		{"DITTOFS_LDAP", familyKeyRule},
		{"DITTOFS_GC", familyKeyRule},
		{"DITTOFS_SNAPSHOT", familyKeyRule},
		{"DITTOFS_INTEGRITY", familyKeyRule},
		{"DITTOFS_ADMIN_X", familyKeyRule},
		{"DITTOFS_KERBEROS_X", familyKeyRule},
		{"DITTOFS_IDENTITY_X", familyKeyRule},
		{"DITTOFS_LDAP_X", familyKeyRule},
		{"DITTOFS_GC_X", familyKeyRule},
		{"DITTOFS_SNAPSHOT_X", familyKeyRule},
		{"DITTOFS_INTEGRITY_X", familyKeyRule},
		{"DITTOFS_CONTROLPLANE_SECRET", removedKeyRule},
		{"DITTOFS_GC_INTERVAL_X", familyKeyRule},
		{"DITTOFS_DATABASES_X", "unknown name; did you mean DITTOFS_JOURNALS?"},
		{"DITTOFS_ADMINX", "unknown name; did you mean DITTOFS_NODE_ID?"},
	} {
		t.Run(tc.variable, func(t *testing.T) {
			t.Parallel()

			path, got := loadRefused(t, "", []string{tc.variable + "=s3cr3t"})
			want := wantReport(path, "environment: "+tc.variable+": "+tc.rule)

			if got != want {
				t.Errorf("report:\ngot:\n%s\nwant:\n%s", got, want)
			}

			if strings.Contains(got, "s3cr3t") {
				t.Errorf("report prints the value:\n%s", got)
			}
		})
	}
}

// The names no key ever had are the two the RFC's table names for their own
// rule.
func TestRetiredEnvNamesAreTheTwoWithNoKey(t *testing.T) {
	t.Parallel()

	got := slices.Sorted(maps.Keys(retiredEnvNames))
	want := []string{"DITTOFS_ADMIN_INITIAL_PASSWORD", "DITTOFS_CONTROLPLANE_SECRET"}

	if !slices.Equal(got, want) {
		t.Errorf("the names with no key:\ngot  %q\nwant %q", got, want)
	}
}

// The v1 name of the first password refuses under its own rule, set or
// empty, and its value, which may hold the first password, never reaches
// the report.
func TestEnvironmentFirstPassword(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"hunter2", ""} {
		path, got := loadRefused(t, "", []string{"DITTOFS_ADMIN_INITIAL_PASSWORD=" + value})
		want := wantReport(path, "environment: DITTOFS_ADMIN_INITIAL_PASSWORD: "+writeRule)

		if got != want {
			t.Errorf("report of the set name:\ngot:\n%s\nwant:\n%s", got, want)
		}

		if strings.Contains(got, "hunter2") {
			t.Errorf("report prints the value:\n%s", got)
		}
	}
}

// An unknown name of the start names the closest declared variable, and a
// name that does not belong to the start, by its prefix or its case, is no
// business of the scan.
func TestEnvironmentUnknownName(t *testing.T) {
	t.Parallel()

	runRefusals(t, []refusalRow{
		{
			name: "a mistyped level", environ: []string{"DITTOFS_LOGGING_LEVL=x"},
			want: []string{
				"environment: DITTOFS_LOGGING_LEVL: unknown name; " +
					"did you mean DITTOFS_LOGGING_LEVEL?",
			},
		},
		{
			name: "a name nothing declares", environ: []string{"DITTOFS_NOPE=x"},
			want: []string{
				"environment: DITTOFS_NOPE: unknown name; did you mean DITTOFS_NODE_ID?",
			},
		},
		{
			name: "a v1 name in another case", environ: []string{"DITTOFS_Database_Type=x"},
			want: []string{
				"environment: DITTOFS_Database_Type: " +
					"unknown name; did you mean DITTOFS_NODE_ID?",
			},
		},
		{
			name: "a declared name in another case", environ: []string{"DITTOFS_Logging_Level=x"},
			want: []string{
				"environment: DITTOFS_Logging_Level: " +
					"unknown name; did you mean DITTOFS_LOGGING_LEVEL?",
			},
		},
		{
			name:    "two words where a protocol goes",
			environ: []string{"DITTOFS_ADAPTERS_NFS_PORTMAPPER_PORT=x"},
			want: []string{
				"environment: DITTOFS_ADAPTERS_NFS_PORTMAPPER_PORT: " +
					"unknown name; did you mean DITTOFS_ADAPTERS_LISTEN?",
			},
		},
		{
			name: "no word where a protocol goes", environ: []string{"DITTOFS_ADAPTERS__PORT=x"},
			want: []string{
				"environment: DITTOFS_ADAPTERS__PORT: " +
					"unknown name; did you mean DITTOFS_ADAPTERS_LISTEN?",
			},
		},
		{
			name: "a protocol in lower case", environ: []string{"DITTOFS_ADAPTERS_smb_PORT=x"},
			want: []string{
				"environment: DITTOFS_ADAPTERS_smb_PORT: " +
					"unknown name; did you mean DITTOFS_ADAPTERS_LISTEN?",
			},
		},
		{
			name:    "a name that only starts like an exact entry",
			environ: []string{"DITTOFS_METRICS_PORTX=x"},
			want: []string{
				"environment: DITTOFS_METRICS_PORTX: " +
					"unknown name; did you mean DITTOFS_METRICS_LISTEN?",
			},
		},
		{
			name: "a tool prefix inside a name", environ: []string{"DITTOFS_DITTOFS_TEST_X=x"},
			want: []string{
				"environment: DITTOFS_DITTOFS_TEST_X: " +
					"unknown name; did you mean DITTOFS_ADAPTERS_LISTEN?",
			},
		},
		{
			name: "a declared name in lower case", environ: []string{"DITTOFS_logging_level=x"},
			want: []string{
				"environment: DITTOFS_logging_level: " +
					"unknown name; did you mean DITTOFS_LOGGING_LEVEL?",
			},
		},
		{
			name:    "a family alias without its underscore",
			environ: []string{"DITTOFS_BLOCKSTORE_LOCALPATH=x"},
			want: []string{
				"environment: DITTOFS_BLOCKSTORE_LOCALPATH: " +
					"unknown name; did you mean DITTOFS_KEYS_STORAGE_LOCAL_FILE?",
			},
		},
	})

	b := loads(t, "", []string{"dittofs_nope=x", "NOTDITTOFS_X=x"})
	if got, source := b.Metrics.Listen, b.Source["metrics.listen"]; got != "127.0.0.1:9090" ||
		source != sourceDefault {
		t.Errorf(
			"names the start does not read: got metrics.listen %q from %q, want the default",
			got,
			source,
		)
	}
}

// A record field set from the host is a second source, refused as the
// setting it is: by its variable's protocol form, by the v1 family that
// used to hold it, and set but empty.
func TestNoHostOverride(t *testing.T) {
	t.Parallel()

	runRefusals(t, []refusalRow{
		{
			name:    "a port by its protocol form",
			environ: []string{"DITTOFS_ADAPTERS_SMB_PORT=s3cr3t"},
			want:    []string{"environment: DITTOFS_ADAPTERS_SMB_PORT: " + setting},
		},
		{
			name: "the gc interval v1 also read", environ: []string{"DITTOFS_GC_INTERVAL=s3cr3t"},
			want: []string{"environment: DITTOFS_GC_INTERVAL: " + setting},
		},
		{
			name: "a variable set but empty", environ: []string{"DITTOFS_ADAPTERS_SMB_PORT="},
			want: []string{"environment: DITTOFS_ADAPTERS_SMB_PORT: " + setting},
		},
	})
}

// Every allowed name passes, its value unread, and none of them sets a key.
func TestEnvironmentAllowed(t *testing.T) {
	t.Parallel()

	if !slices.Equal(testAllowedNames, allowedEnvNames) {
		t.Errorf("the allowed names:\ngot  %q\nwant %q", allowedEnvNames, testAllowedNames)
	}

	if !slices.Equal(testAllowedPrefixes, allowedEnvPrefixes) {
		t.Errorf(
			"the allowed prefixes:\ngot  %q\nwant %q",
			allowedEnvPrefixes, testAllowedPrefixes,
		)
	}

	for _, name := range slices.Concat(
		testAllowedNames,
		[]string{"DITTOFS_TEST_X", "DITTOFS_E2E_X", "DITTOFS_PREPUSH_X"},
	) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			b := loads(t, "", []string{name + "=s3cr3t"})

			for key, source := range b.Source {
				if source != sourceDefault {
					t.Errorf(
						"source of %s: got %q, want %q; an allowed name set a key",
						key, source, sourceDefault,
					)
				}
			}
		})
	}
}

// A provisional name passes, its value unread, and sets no key. It passes
// only because it is on the provisional list: it is no allowed name, sits
// under no allowed prefix, is no key's variable and has no service link's
// form. It is matched whole, so a name one letter or one word past it is
// unknown. A report refusing another name of the same environment names
// neither it nor its value.
func TestEnvironmentProvisional(t *testing.T) {
	t.Parallel()

	if !slices.Equal(testProvisionalNames, provisionalEnvNames) {
		t.Errorf(
			"the provisional names:\ngot  %q\nwant %q",
			provisionalEnvNames, testProvisionalNames,
		)
	}

	for _, name := range testProvisionalNames {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			endpoint := "http://s3cr3t:4318"

			if slices.Contains(allowedEnvNames, name) || allowedPrefix(name) || declaredEnv[name] ||
				serviceLink(name, endpoint) {
				t.Errorf("%s passes by a rule other than the provisional list", name)
			}

			class, rule := classifyEnv(name, endpoint, declaredEnv)
			if class != envPass || rule != "" {
				t.Errorf(
					"classifyEnv(%s): got %d and %q, want %d and no rule",
					name,
					class,
					rule,
					envPass,
				)
			}

			for _, longer := range []string{name + "X", name + "_X"} {
				if class, _ := classifyEnv(longer, endpoint, declaredEnv); class != envUnknown {
					t.Errorf(
						"classifyEnv(%s): got %d, want %d; a provisional name matched as a prefix",
						longer,
						class,
						envUnknown,
					)
				}
			}

			value := name + "=" + endpoint
			b := loads(t, "", []string{value})

			for key, source := range b.Source {
				if source != sourceDefault {
					t.Errorf(
						"source of %s: got %q, want %q; a provisional name set a key",
						key, source, sourceDefault,
					)
				}
			}

			path, got := loadRefused(t, "", []string{value, "DITTOFS_CONFIGX=x"})

			want := wantReport(
				path,
				"environment: DITTOFS_CONFIGX: unknown name; did you mean DITTOFS_NODE_ID?",
			)
			if got != want {
				t.Errorf("report:\ngot:\n%s\nwant:\n%s", got, want)
			}

			if strings.Contains(got, "s3cr3t") || strings.Contains(got, name) {
				t.Errorf("report names the provisional name or its value:\n%s", got)
			}
		})
	}
}

// The allowed names pass by name, never by prefix, and no production name
// hides under a tool's prefix: a name one letter past a hook's name, a
// prefix without a name after it, and a name only the plans mention refuse,
// each naming the closest declared variable.
func TestEnvironmentAllowedByNameNotPrefix(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		variable string
		want     string
	}{
		{
			"DITTOFS_SKIP_TLS_VERIFY",
			"environment: DITTOFS_SKIP_TLS_VERIFY: unknown name; " +
				"did you mean DITTOFS_API_TLS_CERT_FILE?",
		},
		{
			"DITTOFS_SKIP_",
			"environment: DITTOFS_SKIP_: unknown name; did you mean DITTOFS_NODE_ID?",
		},
		{
			"DITTOFS_SKIP_LINTX",
			"environment: DITTOFS_SKIP_LINTX: unknown name; did you mean DITTOFS_API_LISTEN?",
		},
		{
			"DITTOFS_TESTX",
			"environment: DITTOFS_TESTX: unknown name; did you mean DITTOFS_NODE_ID?",
		},
		{
			"DITTOFS_CONFIGX",
			"environment: DITTOFS_CONFIGX: unknown name; did you mean DITTOFS_NODE_ID?",
		},
		{
			"DITTOFS_",
			"environment: DITTOFS_: unknown name; did you mean DITTOFS_NODE_ID?",
		},
	} {
		t.Run(tc.variable, func(t *testing.T) {
			t.Parallel()

			path, got := loadRefused(t, "", []string{tc.variable + "=s3cr3t"})
			if want := wantReport(path, tc.want); got != want {
				t.Errorf("report:\ngot:\n%s\nwant:\n%s", got, want)
			}

			if strings.Contains(got, "s3cr3t") {
				t.Errorf("report prints the value:\n%s", got)
			}
		})
	}
}

// The classifier, on a declared set the test picks: a key under an allowed
// prefix is the key, an arbitrary name under one is allowed, and with no
// key declared the same name is only allowed.
func TestEnvironmentDeclaredWinsOverPrefix(t *testing.T) {
	t.Parallel()

	declared := map[string]bool{"DITTOFS_TEST_FLAG": true}

	if got, _ := classifyEnv("DITTOFS_TEST_FLAG", "", declared); got != envKey {
		t.Errorf("classifyEnv of a declared name: got %v, want %v", got, envKey)
	}

	if got, _ := classifyEnv("DITTOFS_TEST_FLAG", "", nil); got != envPass {
		t.Errorf("classifyEnv with nothing declared: got %v, want %v", got, envPass)
	}

	for _, declares := range []map[string]bool{declared, nil} {
		if got, _ := classifyEnv("DITTOFS_E2E_X", "", declares); got != envPass {
			t.Errorf(
				"classifyEnv of a name under a prefix: got %v, want %v; the declared set was %v",
				got, envPass, declares,
			)
		}
	}
}

// No entry of the table covers a live key's variable or an allowed name: a
// family entry that swallowed a variable would refuse a key the file and
// the environment both still read.
func TestRetiredNamesDoNotShadowKeys(t *testing.T) {
	t.Parallel()

	for _, s := range keySpecs {
		if _, retired := retiredEnvRule(s.envName()); retired {
			t.Errorf(
				"the table retires %s, the variable of the declared key %s",
				s.envName(),
				s.key,
			)
		}
	}

	for _, key := range typedRefusals {
		if _, retired := retiredEnvRule(variableOf(key)); retired {
			t.Errorf("the table retires %s, the variable of the typed key %s", variableOf(key), key)
		}
	}

	for _, name := range slices.Concat(
		testAllowedNames,
		[]string{"DITTOFS_TEST_X", "DITTOFS_E2E_X", "DITTOFS_PREPUSH_X"},
	) {
		if _, retired := retiredEnvRule(name); retired {
			t.Errorf("the table retires %s, an allowed name", name)
		}
	}
}

// The file's refusals and the environment's are one report, the file's
// lines under the file's path, sorted before the environment's, and none
// of the refused values reaches it.
func TestFileAndEnvironmentOneReport(t *testing.T) {
	t.Parallel()

	path, got := loadRefused(
		t,
		"database:\n  type: sqlite\n",
		[]string{"DITTOFS_METRICS_PORT=9091"},
	)

	want := wantReport(path,
		"P: database.type: "+databaseKeyRule,
		"environment: DITTOFS_METRICS_PORT: no longer read; write metrics.listen",
	)

	if got != want {
		t.Errorf("report:\ngot:\n%s\nwant:\n%s", got, want)
	}

	for _, value := range []string{"sqlite", "9091"} {
		if strings.Contains(got, value) {
			t.Errorf("report prints %q:\n%s", value, got)
		}
	}
}
