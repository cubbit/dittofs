package netgroup_test

import (
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/cmd/dfsctl/internal/clisuite"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// The shared suite's netgroup rows, each through a root built in this process,
// over a node the harness starts.
func TestNetgroupRows(t *testing.T) {
	clisuite.Run(t, func(t *testing.T) clisuite.Runner {
		return clisuite.InProcess(cmdutil.Env{ConfigPath: filepath.Join(t.TempDir(), "dfsctl", "config.json")})
	}, clisuite.Start(t), "netgroup")
}

// changeOnRead runs change on its first Read, as a person answering the
// prompt after another client changed the record, then reads r.
type changeOnRead struct {
	once   sync.Once
	change func()
	r      io.Reader
}

func (c *changeOnRead) Read(p []byte) (int, error) {
	c.once.Do(c.change)
	return c.r.Read(p)
}

// A netgroup changed while delete's prompt waits is not removed, and the
// refusal says it changed since it was read rather than echo the 412.
func TestDeleteOfANetgroupChangedAtThePrompt(t *testing.T) {
	s := clisuite.Start(t)
	cfg := filepath.Join(t.TempDir(), "dfsctl", "config.json")
	s.Login(t, clisuite.InProcess(cmdutil.Env{ConfigPath: cfg}))
	c := s.Client(t)
	const name = "changed-at-the-prompt"
	if _, err := c.Netgroups().Put(t.Context(), name, apiv1.NetgroupSpec{Members: []string{"10.0.0.7"}}, "*"); err != nil {
		t.Fatalf("create netgroup %s: %v", name, err)
	}
	in := &changeOnRead{r: strings.NewReader("y\n"), change: func() {
		n, err := c.Netgroups().Get(t.Context(), name)
		if err == nil {
			_, err = c.Netgroups().Put(t.Context(), name, apiv1.NetgroupSpec{Members: []string{"10.0.0.8"}}, n.ETag)
		}
		if err != nil {
			t.Errorf("change netgroup %s at the prompt: %v", name, err)
		}
	}}
	del := clisuite.InProcess(cmdutil.Env{ConfigPath: cfg, In: in, Interactive: true})
	want := "netgroup '" + name + "' changed or was removed since it was read"
	if _, errOut, status := del(t.Context(), "netgroup", "delete", name); status == 0 || !strings.Contains(errOut, want) {
		t.Errorf("netgroup delete %s changed at the prompt: status %d, stderr %q, want %q", name, status, errOut, want)
	}
	if _, err := c.Netgroups().Get(t.Context(), name); err != nil {
		t.Errorf("netgroup %s after the refused delete: %v, want it kept", name, err)
	}
}
