package bootstrap

import (
	"slices"
	"strings"
)

// envPrefix starts every name the start passes to the bootstrap: a name is
// matched exactly as written (RFC 13 Appendix C), a lower-case one is not
// one of the start's names.
const envPrefix = "DITTOFS_"

// allowedEnvNames are the names a start reads outside the file and its keys
// (RFC 13 Appendix C, "Names read outside the file"), each by the tool that
// reads it. A name is its own, never a prefix.
var allowedEnvNames = []string{
	// decision: the start reads the config path, the local key provider a
	// key file's passphrase, and dfsctl a shell's password, so a start
	// cannot refuse these three.
	"DITTOFS_CONFIG",
	"DITTOFS_ENCRYPTION_PASSPHRASE",
	"DITTOFS_PASSWORD",

	// decision: the repository's launchers and benchmark scripts export
	// these, and a server the tools start inherits them, so refusing one
	// would break every scripted run. None of them is a secret the loader
	// reads, and each is its own name, so a production name cannot hide
	// under one.
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

	// decision: the git hooks read their own skip switch, by name and
	// never as a prefix, so a production name cannot hide under
	// "DITTOFS_SKIP_". A name the tools no longer read, such as
	// DITTOFS_SKIP_TLS_VERIFY, which only the plans name, stays refused.
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

// provisionalEnvNames are names the start reads outside the file only until
// a key or a setting takes their place. The scan passes them as it passes an allowed name;
// they are kept apart so that each entry can be dropped with its last reader.
//
// decision: dfs start reads DITTOFS_OTLP_ENDPOINT in
// cmd/dfs/commands/tracing.go as the temporary switch for tracing, until the
// start reads the tracing.endpoint setting (RFC 25 §6.3). Drop the entry when
// that reader goes. A name is matched whole, never as a prefix.
var provisionalEnvNames = []string{
	"DITTOFS_OTLP_ENDPOINT",
}

// allowedEnvPrefixes are the prefixes under which the repository's test
// suites export arbitrary names.
//
// decision: a test that starts a server exports flags under these, so a
// refusal would break the suites; a declared key's variable is read as its
// key even when it sits under one of them, checked before the prefix, so no
// production fact hides here.
var allowedEnvPrefixes = []string{
	"DITTOFS_TEST_",
	"DITTOFS_E2E_",
	"DITTOFS_PREPUSH_",
}

// declaredEnv is the set of the declared keys' variables.
var declaredEnv = func() map[string]bool {
	names := make(map[string]bool, len(keySpecs))

	for i := range keySpecs {
		names[keySpecs[i].envName()] = true
	}

	return names
}()

// coversVariable reports whether name is the variable of the entry's key, or
// for a family the family's own variable or one of a key under it, which
// takes an underscore after the family's variable.
func (e retiredKey) coversVariable(name string) bool {
	variable := variableOf(e.key)

	if e.family {
		return name == variable || strings.HasPrefix(name, variable+"_")
	}

	return name == variable
}

// retiredEnvRule is the rule of a retired name: an environment-only name's
// own, or the entry whose key the name spells as its variable, the longest
// such entry winning. No value is printed or kept.
func retiredEnvRule(name string) (string, bool) {
	if rule, ok := retiredEnvNames[name]; ok {
		return rule, true
	}

	for old, replacement := range retiredAliases {
		if rest, ok := strings.CutPrefix(name, variableOf(old)+"_"); ok {
			name = variableOf(replacement) + "_" + rest

			break
		}
	}

	rule, found, length := "", false, -1

	for _, e := range retiredKeys {
		if e.coversVariable(name) && len(e.key) > length {
			rule, found, length = e.rule, true, len(e.key)
		}
	}

	return rule, found
}

// envClass is what the scan concludes about one of the start's names.
type envClass int

const (
	// envKey is a declared key's variable, read as the key itself.
	envKey envClass = iota

	// envPass is an allowed or a provisional name, or a service link: it
	// passes, no value kept.
	envPass

	// envSetting is an Appendix B setting's variable, refused as a
	// setting.
	envSetting

	// envRetired is a retired key's or name's variable, refused under the
	// table's rule.
	envRetired

	// envUnknown is none of the above, refused naming the closest
	// declared variable.
	envUnknown
)

// classifyEnv concludes what the scan reads in one name holding value, and
// the rule that refuses it when it refuses. The declared variables come
// first, so a key wins over an allowed prefix; then the allowed names, so a
// tool's export passes; then the provisional names; then a service link, so
// the kubelet's variables pass; then the settings, so a setting overridden
// from the host refuses as one, before the v1 table, so a setting whose
// family is a v1 family still names the setting; then the table; then the
// unknown, with the closest declared variable named.
func classifyEnv(name, value string, declared map[string]bool) (envClass, string) {
	if declared[name] {
		return envKey, ""
	}

	if slices.Contains(allowedEnvNames, name) || allowedPrefix(name) {
		return envPass, ""
	}

	if slices.Contains(provisionalEnvNames, name) {
		return envPass, ""
	}

	// decision: a pair whose name is one of a service link's forms and whose
	// value has that form's shape is ignored, because the kubelet writes it
	// into every pod and no operator can remove it. The value decides, never
	// the name alone: a retired variable such as DITTOFS_METRICS_PORT is also
	// a link's name, and holding a plain number it still refuses, so the
	// start never reads a second source of configuration under a link's name.
	// It follows the declared and allowed names, so a key's variable is still
	// read as the key. Withdraw it if the kubelet's variables stop sharing
	// this prefix, or if anything but the declared-key lookup reads a
	// DITTOFS_ name (load.go, the env[s.envName()] read), since a link
	// passed here would then be read as configuration.
	if serviceLink(name, value) {
		return envPass, ""
	}

	if settingEnvName(name) {
		return envSetting, settingLine
	}

	if rule, ok := retiredEnvRule(name); ok {
		return envRetired, rule
	}

	return envUnknown, "unknown name; did you mean " + closestEnvName(name) + "?"
}

// allowedPrefix reports whether name sits under one of the allowed
// prefixes, underscore included.
func allowedPrefix(name string) bool {
	return slices.ContainsFunc(allowedEnvPrefixes, func(prefix string) bool {
		return strings.HasPrefix(name, prefix)
	})
}

// protocolWord stands for any one adapter's name in a setting's key.
const protocolWord = "<protocol>"

// settingEnvName reports whether name is the variable of an Appendix B
// setting: the key's upper-case form, "<protocol>" standing for one word.
func settingEnvName(name string) bool {
	probe, ok := strings.CutPrefix(name, envPrefix)
	if !ok {
		return false
	}

	for _, setting := range settingKeys {
		variable := strings.ReplaceAll(strings.ToUpper(setting), ".", "_")

		before, after, split := strings.Cut(variable, strings.ToUpper(protocolWord))
		if !split {
			if probe == variable {
				return true
			}

			continue
		}

		word, ok := strings.CutPrefix(probe, before)
		if !ok {
			continue
		}

		word, ok = strings.CutSuffix(word, after)
		if ok && word != "" && !strings.Contains(word, "_") && word == strings.ToUpper(word) {
			return true
		}
	}

	return false
}

// closestEnvName is the declared variable a name most likely meant: the one
// equal to it in another case, else the one at the smallest edit distance,
// however far, a tie going to the first in RFC 13 Appendix C's order.
func closestEnvName(name string) string {
	for i := range keySpecs {
		if strings.EqualFold(name, keySpecs[i].envName()) {
			return keySpecs[i].envName()
		}
	}

	best, bestDist := "", -1

	for i := range keySpecs {
		variable := keySpecs[i].envName()

		if d := editDistance(name, variable); bestDist < 0 || d < bestDist {
			best, bestDist = variable, d
		}
	}

	return best
}

// scanEnv refuses every one of the start's names that reads as no key and
// no allowed tool: a setting's line, a retired key's rule, or the unknown's
// closest declared variable (RFC 13 Appendix C, "Names read outside the
// file"). No value is printed or kept; one is read only to tell a service link by
// its shape. The lines join the file's in one report.
func scanEnv(env map[string]string, rep *report) {
	names := make([]string, 0, len(env))

	for name := range env {
		if strings.HasPrefix(name, envPrefix) {
			names = append(names, name)
		}
	}

	slices.Sort(names)

	for _, name := range names {
		class, rule := classifyEnv(name, env[name], declaredEnv)

		if class >= envSetting {
			rep.add(whereEnvironment, name, rule)
		}
	}
}
