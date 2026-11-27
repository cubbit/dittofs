package node_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// BenchmarkSignInCommitCost measures one metadata commit, a netgroup's put,
// with checked sign-ins from many sources running beside it and without
// them, so benchstat -col /signins shows what the sign-ins' two commits
// each cost the commits beside them. Each sign-in comes from its own
// address, so none waits on a throttle; half of them fail.
func BenchmarkSignInCommitCost(b *testing.B) {
	for _, signins := range []int{0, 8} {
		b.Run(fmt.Sprintf("signins=%d", signins), func(b *testing.B) {
			h := nodetest.New(b, nodetest.AdminPassword(adminPassword))
			started(b, h)
			svc := h.Node().SignIns()
			ctx, stop := context.WithCancel(b.Context())
			var wg sync.WaitGroup
			for w := range signins {
				wg.Go(func() {
					for i := 0; ctx.Err() == nil; i++ {
						password := adminPassword
						if i%2 == 1 {
							password = "wrong"
						}
						rec := entity.AuditRecord{Source: fmt.Sprintf("10.%d.%d.%d:1", w, (i/250)%250, i%250+1)}
						_, _, _ = svc.SignIn(ctx, rec, apiv1.SignIn{Name: "admin", Password: password})
					}
				})
			}
			cp := h.Store().ControlPlane()
			b.ReportAllocs()
			for i := 0; b.Loop(); i++ {
				n := entity.Netgroup{Name: fmt.Sprintf("bench%d", i)}
				if _, err := cp.PutNetgroup(b.Context(), n, 0); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			stop()
			wg.Wait()
		})
	}
}
