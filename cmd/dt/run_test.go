package main

import (
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/cubbit/dittofs/test/harness/registry"
)

// The registry refuses an option named like any of dt run's own flags: the
// option is a flag of its own, and with a flag of that name already there it
// would get none.
func TestOptionsCannotTakeRunFlagNames(t *testing.T) {
	const head = "version: 1\ndefaults: { format: exit, tier: merge, timeout: { test: 30m, cell: 60m } }\n"
	newRun().Flags().VisitAll(func(f *pflag.Flag) {
		if strings.HasPrefix(f.Usage, "run the suite's ") {
			return // a suite option's own flag
		}
		yaml := head + "suites:\n  a: { cmd: true, options: { " + f.Name + ": { cmd: false } } }\n"
		if _, err := registry.Parse(t.TempDir(), []byte(yaml)); err == nil || !strings.Contains(err.Error(), "dt run has a flag of that name") {
			t.Errorf("an option named %s: %v", f.Name, err)
		}
	})
}
