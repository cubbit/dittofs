package bootstrap

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// envLabel is how a report starts the lines of a name, quoted as the report
// quotes it.
func envLabel(name string) string {
	return whereEnvironment + ": " + oneLine(name) + ": "
}

// wantEnvLines are the lines the scan owes one name that no key declares. It
// re-derives only the order of the classes and the allowed lists: an allowed
// name gets none, a setting's variable the setting line, a retired name its
// rule, and any other the closest declared variable, the rules themselves
// coming from the loader's own lookups. A name that does not start with
// DITTOFS_ is not the start's and gets none.
func wantEnvLines(name string) []string {
	if !strings.HasPrefix(name, "DITTOFS_") {
		return nil
	}

	allowed := slices.Contains(testAllowedNames, name) ||
		slices.ContainsFunc(testAllowedPrefixes, func(prefix string) bool {
			return strings.HasPrefix(name, prefix)
		})

	switch rule, retired := retiredEnvRule(name); {
	case allowed:
		return nil
	case settingEnvName(name):
		return []string{envLabel(name) + settingLine}
	case retired:
		return []string{envLabel(name) + rule}
	default:
		return []string{envLabel(name) + "unknown name; did you mean " + closestEnvName(name) + "?"}
	}
}

// Two names of any shape, each with a value that holds a canary, scan into
// a report with no panic and the same lines on a second run, whose every
// line starts with one of the two names, never prints a value, and whose
// lines for a name no key declares are exactly the ones its class owes.
func FuzzEnvironment(f *testing.F) {
	seeds := []string{
		"DITTOFS_SKIP_TLS_VERIFY", "DITTOFS_SKIP_", "DITTOFS_SKIP_LINTX",
		"DITTOFS_TESTX", "DITTOFS_CONFIGX", "DITTOFS_", "dittofs_nope",
		"DITTOFS_LOGGING_LEVEL", "DITTOFS_ADAPTERS_SMB_PORT", "DITTOFS_GC_INTERVAL",
		"DITTOFS_DATABASES_X", "DITTOFS_ADMINX", "",
	}

	for _, e := range retiredKeys {
		if e.family {
			seeds = append(seeds, variableOf(e.key)+"_X")
		} else {
			seeds = append(seeds, variableOf(e.key))
		}
	}

	for name := range retiredEnvNames {
		seeds = append(seeds, name)
	}

	seeds = append(seeds, allowedEnvNames...)

	for _, prefix := range allowedEnvPrefixes {
		seeds = append(seeds, prefix+"X")
	}

	for _, name := range seeds {
		f.Add(name, "s3cr3t", "DITTOFS_NODE_ID", "x")
	}

	f.Fuzz(func(t *testing.T, name1, value1, name2, value2 string) {
		// A name holding "=" would be read as a shorter name with a longer
		// value, which is another input.
		name1 = strings.ReplaceAll(name1, "=", "_")
		name2 = strings.ReplaceAll(name2, "=", "_")

		environ := []string{name1 + "=" + value1 + fuzzCanary, name2 + "=" + value2 + fuzzCanary}

		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}

		first := reportOf(LoadWithSources(path, environ))
		if again := reportOf(LoadWithSources(path, environ)); again != first {
			t.Errorf("the same environ:\nfirst %q\nagain %q", first, again)
		}

		if !strings.Contains(name1+name2, fuzzCanary) && strings.Contains(first, fuzzCanary) {
			t.Errorf("report prints a value:\n%s", first)
		}

		labels := []string{envLabel(name1), envLabel(name2)}

		for _, line := range strings.Split(first, "\n") {
			if line == "" {
				continue
			}

			if !slices.ContainsFunc(labels, func(label string) bool {
				return strings.HasPrefix(line, label)
			}) {
				t.Errorf("report line %q starts with neither name", line)
			}
		}

		for _, name := range slices.Compact(slices.Sorted(slices.Values([]string{name1, name2}))) {
			if declaredEnv[name] {
				continue
			}

			var got []string

			for _, line := range strings.Split(first, "\n") {
				if strings.HasPrefix(line, envLabel(name)) {
					got = append(got, line)
				}
			}

			if want := wantEnvLines(name); !slices.Equal(got, want) {
				t.Errorf("the lines of %q:\ngot  %q\nwant %q", name, got, want)
			}
		}
	})
}

// reportOf renders a load's error, an empty report as "".
func reportOf(_ *Loaded, err error) string {
	if err == nil {
		return ""
	}

	return err.Error()
}
