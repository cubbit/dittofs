package commands

import (
	"testing"

	"github.com/spf13/cobra"
)

// Every v1 command dfsctl still lists refuses, naming itself, until the CLI is
// rebuilt on the v2 API; the commands that work locally (context, completion,
// version, logout) are left out of the walk.
func TestV1CommandsRefuse(t *testing.T) {
	kept := map[string]bool{"context": true, "completion": true, "version": true, "logout": true, "help": true}
	refused := 0
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if c == GetRootCmd() && kept[sub.Name()] {
				continue
			}
			if sub.Runnable() {
				refused++
				if sub.RunE == nil {
					t.Errorf("%s: no RunE, want the refusal", sub.CommandPath())
				} else if err := sub.RunE(sub, nil); err == nil || err.Error() != sub.CommandPath()+": not available until the CLI is rebuilt on the v2 API" {
					t.Errorf("%s: got %v, want the refusal naming the command", sub.CommandPath(), err)
				}
			}
			walk(sub)
		}
	}
	walk(GetRootCmd())
	if refused == 0 {
		t.Error("no command refused, want every v1 command")
	}
}
