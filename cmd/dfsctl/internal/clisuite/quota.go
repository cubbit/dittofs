package clisuite

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
)

// quotaRows are the rows of the quota commands, harness-only for the share
// each needs.
func quotaRows() []row {
	return []row{
		{name: "quota set, list and remove on a user and on the share", harnessOnly: true, run: func(t *testing.T, run Runner, s Server, c *client.Client) {
			sh := s.share(t, c)
			uname, uid := user(t, c)
			for _, tc := range []struct {
				on    []string
				quota func() (client.Tagged[apiv1.QuotaSpec, struct{}], error)
			}{
				{[]string{"--user", uname}, func() (client.Tagged[apiv1.QuotaSpec, struct{}], error) { return c.Quotas(sh.ID).Get(t.Context(), uid) }},
				{nil, func() (client.Tagged[apiv1.QuotaSpec, struct{}], error) {
					return c.ShareQuotas().Get(t.Context(), sh.ID)
				}},
			} {
				on := strings.Join(tc.on, " ")
				mustRun(t, run, append([]string{"quota", "set", sh.Spec.Name, "--hard-bytes", "1000", "--grace", "1h"}, tc.on...)...)
				// A second set changes the limit it names and keeps the others.
				mustRun(t, run, append([]string{"quota", "set", sh.Spec.Name, "--soft-files", "5"}, tc.on...)...)
				want := apiv1.QuotaSpec{Hard: apiv1.Limit{Bytes: 1000}, Soft: apiv1.Limit{Files: 5}, GraceSeconds: 3600}
				if q, err := tc.quota(); err != nil || q.Spec != want {
					t.Errorf("quota set %q: the server holds %+v (%v), want %+v", on, q.Spec, err, want)
				}
				mustRun(t, run, append([]string{"quota", "remove", sh.Spec.Name}, tc.on...)...)
				if _, err := tc.quota(); !cmdutil.HasStatus(err, http.StatusNotFound) {
					t.Errorf("quota remove %q: reading the quota back answered %v, want 404", on, err)
				}
			}
			mustRun(t, run, "quota", "set", sh.Spec.Name, "--user", uname, "--hard-files", "7")
			mustRun(t, run, "quota", "set", sh.Spec.Name, "--advisory-bytes", "9")
			qs, err := decodeFormat[[]apiv1.Quota]("json", mustRun(t, run, "quota", "list", sh.Spec.Name, "-o", "json"))
			if err != nil || len(qs) != 2 || qs[0].ID != sh.ID || qs[0].Spec.Advisory.Bytes != 9 || qs[1].ID != uid || qs[1].Spec.Hard.Files != 7 {
				t.Errorf("quota list -o json: got %+v (%v), want the share's own quota, then %s's", qs, err, uname)
			}
		}},
	}
}
