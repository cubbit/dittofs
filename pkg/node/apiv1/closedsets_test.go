package apiv1

import (
	"slices"
	"testing"
)

// Each closed set is RFC 23's literal list, each value once: a value missing,
// extra, repeated or misspelt fails.
func TestClosedSetsAreRFC23s(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  []string
		want []string
	}{
		{"API codes", strs(apiCodes), []string{
			"unauthenticated", "forbidden", "generation_mismatch", "precondition_required",
			"idempotency_mismatch", "managed", "operation_running", "not_cancellable",
			"last_admin", "not_a_copy", "password_change_required", "throttled", "starting",
			"unavailable",
		}},
		{"operation kinds", strs(opKinds), []string{
			"installation.rotate-signing-key", "installation.resolve-principal",
			"installation.test-directory", "installation.join-domain",
			"installation.rotate-machine-password", "share.delete", "user.delete",
			"group.delete", "identity.nt-hash-delete",
		}},
		{"operation states", strs(opStates), []string{
			"pending", "running", "succeeded", "failed", "cancelling", "cancelled",
		}},
	} {
		got := slices.Sorted(slices.Values(tc.got))
		want := slices.Sorted(slices.Values(tc.want))
		if !slices.Equal(got, want) {
			t.Errorf("%s: got %q, want %q", tc.name, got, want)
		}
	}
}

func strs[S ~string](s []S) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[i] = string(v)
	}
	return out
}
