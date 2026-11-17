package apiv1

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"
)

// Each body encodes under RFC 23's JSON names and decodes back to the value
// it was written from, so a renamed or dropped tag fails here.
func TestWireBodies(t *testing.T) {
	at := time.Date(2026, 11, 16, 9, 30, 0, 0, time.UTC)
	full := Problem{
		Status: 409, Code: CodeOperationRunning, Title: "an operation is running",
		Detail: "share.delete runs on sh_1", Field: "spec.name", Rule: "unique",
		Names: []string{"op_1"}, Retryable: true, RequestID: "req_1",
	}
	op := Operation{
		ID: "op_1", Kind: OpShareDelete, Target: Ref{Kind: "share", ID: "sh_1"},
		Account: Principal{Kind: "user", ID: "us_1"}, State: OpFailed,
		Progress: Progress{Phase: "files", Done: 3, Total: -1, Unit: "files"},
		Started:  at, Updated: at.Add(time.Minute), Error: &full,
		Result: json.RawMessage(`{"share":"sh_2"}`),
	}
	for _, tc := range []struct {
		name string
		in   any
		out  any // a pointer to a zero value of in's type
		keys []string
	}{
		{"resource", Resource[map[string]string, map[string]string]{
			ID: "sh_1", Generation: 15, Spec: map[string]string{"name": "builds"},
			Status: map[string]string{"state": "enabled"},
		}, new(Resource[map[string]string, map[string]string]),
			[]string{"generation", "id", "spec", "status"}},
		{"page with more", Page[string]{Items: []string{"a"}, Next: "c1"}, new(Page[string]),
			[]string{"items", "next"}},
		{"last page", Page[string]{Items: []string{"a"}}, new(Page[string]),
			[]string{"items"}},
		{"problem", full, new(Problem),
			[]string{"code", "detail", "field", "names", "request_id", "retryable", "rule", "status", "title"}},
		{"problem without a refused value", Problem{Status: 401, Code: CodeUnauthenticated, Title: "no valid token", RequestID: "req_2"}, new(Problem),
			[]string{"code", "request_id", "retryable", "status", "title"}},
		{"operation", op, new(Operation),
			[]string{"account", "error", "id", "kind", "progress", "result", "started", "state", "target", "updated"}},
		{"progress", op.Progress, new(Progress), []string{"done", "phase", "total", "unit"}},
		{"target", op.Target, new(Ref), []string{"id", "kind"}},
		{"account", op.Account, new(Principal), []string{"id", "kind"}},
		{"version", Version{Majors: []string{"v1"}}, new(Version), []string{"majors"}},
		{"sign-in", SignIn{Name: "alice", Password: "pw"}, new(SignIn), []string{"name", "password"}},
		{"tokens", Tokens{Access: "a.b.c", Refresh: "r1"}, new(Tokens), []string{"access_token", "refresh_token"}},
		{"refresh", Refresh{Token: "r1"}, new(Refresh), []string{"refresh_token"}},
		{"password change", PasswordChange{Current: "old", New: "new"}, new(PasswordChange),
			[]string{"current_password", "new_password"}},
		{"set password", SetPassword{Password: "pw"}, new(SetPassword), []string{"password"}},
		{"API token spec", APITokenSpec{Expires: at.AddDate(1, 0, 0)}, new(APITokenSpec), []string{"expires"}},
		{"created API token", APITokenStatus{Created: at, Token: "t1"}, new(APITokenStatus),
			[]string{"created", "token"}},
		{"listed API token", APITokenStatus{Created: at}, new(APITokenStatus), []string{"created"}},
		{"applied", Applied{Nodes: 2, Of: 3}, new(Applied), []string{"nodes", "of"}},
		{"node status", NodeStatus{Address: "10.0.0.1:12049", Binary: "v0.20.0", Domain: "rack-1", Lapsed: true,
			Reconciled: &Reconciled{Settings: 7, ShareList: 3, Principals: 2}}, new(NodeStatus),
			[]string{"address", "binary", "domain", "lapsed", "reconciled"}},
		{"reconciled", Reconciled{Settings: 7, ShareList: 3, Principals: 2}, new(Reconciled),
			[]string{"principals", "settings", "share_list"}},
		{"audit record", AuditRecord{
			ID: "au_1", Time: at, Actor: Principal{Kind: "user", ID: "us_1"}, ActorName: "eli", Credential: "session-1",
			Node: "n_1", Source: "10.0.0.9:51234", Request: json.RawMessage(`{"method":"PUT"}`),
			Target: Ref{Kind: "share", ID: "sh_1"}, Before: 1, After: 2, Change: json.RawMessage(`{"spec.name":["a","b"]}`),
			Outcome: "applied",
		}, new(AuditRecord), []string{"actor", "actor_name", "after", "before", "change", "credential", "id", "node", "outcome", "request", "source", "target", "time"}},
		{"health", Health{Ready: "ready", Conditions: []Condition{{Name: "store_unreachable", Scope: "store", ID: "rs_1", Subject: "s3", Cause: "dial tcp: refused", Detail: "3 failures"}}}, new(Health),
			[]string{"conditions", "ready"}},
		{"condition", Condition{Name: "store_unreachable", Scope: "store", ID: "rs_1", Subject: "s3", Cause: "dial tcp: refused", Detail: "3 failures"}, new(Condition),
			[]string{"cause", "detail", "id", "name", "scope", "subject"}},
	} {
		b, err := json.Marshal(tc.in)
		if err != nil {
			t.Fatalf("%s: encode: %v", tc.name, err)
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(b, &obj); err != nil {
			t.Fatalf("%s: %s is not an object: %v", tc.name, b, err)
		}
		if got := slices.Sorted(maps.Keys(obj)); !slices.Equal(got, tc.keys) {
			t.Errorf("%s: keys %q, want %q", tc.name, got, tc.keys)
		}
		if err := json.Unmarshal(b, tc.out); err != nil {
			t.Fatalf("%s: decode: %v", tc.name, err)
		}
		if got := reflect.ValueOf(tc.out).Elem().Interface(); !reflect.DeepEqual(got, tc.in) {
			t.Errorf("%s: decoded %+v, want %+v", tc.name, got, tc.in)
		}
	}
}

// A Problem is an error whose text names its status, code and title.
func TestProblemIsAnError(t *testing.T) {
	var err error = &Problem{Status: 409, Code: CodeLastAdmin, Title: "the last admin", Detail: "admin is the only enabled admin"}
	if got, want := err.Error(), "409 last_admin: the last admin: admin is the only enabled admin"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
