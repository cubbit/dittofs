package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// A remote that times out on its own call deadline must not stop the reclaim of
// the remotes after it. The caller's own deadline, and any other error, still do.
func TestRemoteStalled(t *testing.T) {
	timedOut := fmt.Errorf("reclaim: delete: %w", context.DeadlineExceeded)
	other := errors.New("boom")

	expired, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()

	cases := []struct {
		name    string
		ctx     context.Context
		err     error
		wantNil bool
	}{
		{"remote deadline goes on to the next remote", context.Background(), timedOut, true},
		{"caller deadline ends the run", expired, timedOut, false},
		{"another error ends the run", context.Background(), other, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := remoteStalled(tc.ctx, "cfg", tc.err)
			if (got == nil) != tc.wantNil {
				t.Fatalf("remoteStalled = %v, want nil=%v", got, tc.wantNil)
			}
		})
	}
}
