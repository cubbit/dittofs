package bootstrap

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// resolvedDirs is the state and runtime directories the given environment
// names, with the variable that chose each.
type resolvedDirs struct {
	// state and runtime are the two directories' paths, the runtime one
	// falling back to a directory under the state one.
	state, runtime string

	// stateVar and runtimeVar are the environment variables the directories
	// came from, so a refusal of a directory names the one that chose it.
	// The state variable stands in for the runtime directory when the runtime
	// directory is the fallback under the state one.
	stateVar, runtimeVar string

	// stateRefused is set when the variable that chose the state directory
	// gave a path that is not absolute. The runtime directory stays empty,
	// since it hangs under the state one.
	stateRefused bool

	// runtimeRefused is set when the variable that chose the runtime
	// directory gave a path that is not absolute: a relative path depends on
	// the directory the process was started from.
	runtimeRefused bool

	// stateTemp is set when no variable named the state directory and the
	// process home is not available either, so the state directory is the
	// temp directory's, a directory no variable names.
	stateTemp bool

	// runtimeManaged is set when the runtime directory is the one the
	// service manager owns, which no start may create.
	runtimeManaged bool
}

// lookupDirs resolves the state and runtime directories the given
// environment names, the process environment as os.Environ returns it. A
// variable a unit sets as a colon-separated list is read for its first
// entry.
//
// The state directory is $STATE_DIRECTORY's first entry, else
// $XDG_STATE_HOME/dittofs, else $HOME/.local/state/dittofs ($LOCALAPPDATA/
// dittofs, else $USERPROFILE/AppData/Local/dittofs, on the platforms whose
// home that is). With none of those variables set it is the process home
// directory's, else the temp directory's dittofs.
//
// decision: with no variable to name it, the state directory falls back to
// the process home directory os.UserHomeDir finds, the one place the loader
// reaches for a host variable it was not given: a load with no environment
// (the defaults a fresh node reads, and every caller with no environment of
// its own) still has to name a directory, and the process home belongs to
// whoever runs the process. The caller that checks the facts, Validate,
// refuses what this fallback can produce when nothing can be trusted; the
// loader never does. Withdraw it when every caller can be given the
// variables it needs.
func lookupDirs(environ []string) resolvedDirs {
	env := environMap(environ)

	var d resolvedDirs

	switch {
	case runtime.GOOS == "windows":
		switch {
		case env["LOCALAPPDATA"] != "":
			d.state, d.stateVar = filepath.Join(env["LOCALAPPDATA"], "dittofs"), "LOCALAPPDATA"
		case env["USERPROFILE"] != "":
			d.state = filepath.Join(env["USERPROFILE"], "AppData", "Local", "dittofs")
			d.stateVar = "USERPROFILE"
		default:
			home, err := os.UserHomeDir()
			if err != nil {
				d.state = filepath.Join(os.TempDir(), "dittofs")
				d.stateVar = "USERPROFILE"
				d.stateTemp = true

				break
			}

			d.state = filepath.Join(home, "AppData", "Local", "dittofs")
			d.stateVar = "USERPROFILE"
		}
	case firstEntry(env["STATE_DIRECTORY"]) != "":
		d.state, d.stateVar = firstEntry(env["STATE_DIRECTORY"]), "STATE_DIRECTORY"
	case env["XDG_STATE_HOME"] != "":
		d.state, d.stateVar = filepath.Join(env["XDG_STATE_HOME"], "dittofs"), "XDG_STATE_HOME"
	case env["HOME"] != "":
		d.state = filepath.Join(env["HOME"], ".local", "state", "dittofs")
		d.stateVar = "HOME"
	default:
		home, err := os.UserHomeDir()
		if err != nil {
			d.state = filepath.Join(os.TempDir(), "dittofs")
			d.stateVar = "HOME"
			d.stateTemp = true

			break
		}

		d.state = filepath.Join(home, ".local", "state", "dittofs")
		d.stateVar = "HOME"
	}

	if d.stateRefused = !filepath.IsAbs(d.state); d.stateRefused {
		return d
	}

	switch v := firstEntry(env["RUNTIME_DIRECTORY"]); {
	case v != "":
		d.runtime, d.runtimeVar, d.runtimeManaged = v, "RUNTIME_DIRECTORY", true
	case env["XDG_RUNTIME_DIR"] != "":
		d.runtime = filepath.Join(env["XDG_RUNTIME_DIR"], "dittofs")
		d.runtimeVar = "XDG_RUNTIME_DIR"
	default:
		d.runtime, d.runtimeVar = filepath.Join(d.state, "run"), d.stateVar
	}

	d.runtimeRefused = !filepath.IsAbs(d.runtime)

	return d
}

// firstEntry reads the first entry of a value a unit may set as a
// colon-separated list, whose first entry is the one read here.
func firstEntry(v string) string {
	entry, _, _ := strings.Cut(v, ":")

	return entry
}

// MakeRuntimeDir makes the runtime directory the given environment names
// and returns it. $RUNTIME_DIRECTORY is the service manager's directory,
// returned as the manager set it and never created here;
// $XDG_RUNTIME_DIR/dittofs and, with neither variable set, the run/
// directory under the state directory are made at 0700 when they are
// missing. One that exists is checked as Validate checks a runtime directory:
// a symlink, a path that is not a directory, an owner other than the node's
// user and a write bit of group or others refuse, since another user could
// replace the directory the management socket binds in. So does a relative path, which
// depends on the directory the process started from. Every refusal is a line
// of the load's report, <where>: <key>: <rule>, naming the variable.
//
// It is the start that calls this, the one place that may create the
// runtime directory; Validate refuses the directories it finds and creates
// nothing.
func MakeRuntimeDir(environ []string) (string, error) {
	d := lookupDirs(environ)

	if d.stateRefused {
		return "", refuseOne(d.stateVar, "must be an absolute path")
	}

	if d.runtimeRefused {
		return "", refuseOne(d.runtimeVar, "must be an absolute path")
	}

	// A state directory no variable names is the temp directory's, where
	// another user could have made dittofs first: refused, as Validate does.
	if d.stateTemp {
		return "", refuseOne(d.stateVar, "is not set")
	}

	if d.runtimeManaged {
		return d.runtime, nil
	}

	if err := makeDir(d.runtime, d.runtimeVar, processUser()); err != nil {
		return "", err
	}

	return d.runtime, nil
}

// refuseOne is one refusal of the environment as an error, in the load's
// format, <where>: <key>: <rule>.
func refuseOne(key, rule string) error {
	var rep report

	rep.add(whereEnvironment, key, rule)

	return rep.err()
}

// makeDir makes dir at 0700 when it is missing, the missing parents at the
// same mode, the state directory included when the fallback hangs under a
// state directory no start made yet. A directory already on the path is
// checked as Validate checks a runtime directory (refuseRuntime) rather than
// used. A refusal names variable, the one that chose dir, and never the
// path: the host's own error text holds it.
func makeDir(dir, variable string, node uint32) error {
	f := dirFacts(dir)

	if rules := refuseRuntime(f, node); len(rules) > 0 {
		var rep report

		for _, rule := range rules {
			rep.add(whereEnvironment, variable, rule)
		}

		return rep.err()
	}

	if f.exists {
		return nil
	}

	if err := os.MkdirAll(dir, 0o0700); err != nil {
		return refuseOne(variable, makeRule(err))
	}

	return nil
}

// makeRule names why a directory could not be made with a fixed phrase.
func makeRule(err error) string {
	if errors.Is(err, fs.ErrPermission) {
		return "is not writable"
	}

	return "cannot be made"
}
