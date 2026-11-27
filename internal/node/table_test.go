package node_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// writeOf sends method on addr's path with body and the headers hdr names in
// pairs, and returns its status and body.
func writeOf(t *testing.T, addr, method, path, body string, hdr ...string) (int, string) {
	t.Helper()

	req, err := http.NewRequest(method, "http://"+addr+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Add(hdr[i], hdr[i+1])
	}

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}

	defer func() { _ = resp.Body.Close() }()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read the answer: %v", method, path, err)
	}

	return resp.StatusCode, string(b)
}

// problemOf is the answer body decoded as a problem body.
func problemOf(t *testing.T, body string) apiv1.Problem {
	t.Helper()

	var p apiv1.Problem
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("the answer %s: %v", body, err)
	}

	return p
}

// storeOf is the state a refused write must leave: the installation's
// settings and the counters, as one comparable string.
func storeOf(t testing.TB, h *nodetest.Harness) string {
	t.Helper()

	rows := map[string]string{}

	for s, err := range h.Store().ControlPlane().Settings(t.Context(),
		entity.SettingScope{Kind: entity.ScopeInstallation}) {
		if err != nil {
			t.Fatalf("read the installation settings: %v", err)
		}

		rows[s.Key] = string(s.Value)
	}

	c, err := h.Store().Nodes().Counters(t.Context())
	if err != nil {
		t.Fatalf("read the counters: %v", err)
	}

	b, err := json.Marshal(struct {
		Settings map[string]string `json:"settings"`
		Counters kvstore.Counters  `json:"counters"`
	}{rows, c})
	if err != nil {
		t.Fatal(err)
	}

	return string(b)
}

// The node's own settings table judges a write that reaches the API: an
// operator's write of an admin-only key is refused 403 naming the key and
// changes nothing, an admin's write of an unknown key is refused 400 naming
// it, and an admin's write of the key is stored as its row.
func TestTheNodeTableJudgesWrites(t *testing.T) {
	t.Run("as the operator", func(t *testing.T) {
		h, addr := judgedNode(t, entity.RoleOperator)
		before := storeOf(t, h)

		code, body := writeOf(t, addr, http.MethodPut, "/api/v1/settings/audit.retention",
			`{"spec":{"value":"2880h"}}`, "If-None-Match", "*")
		if code != http.StatusForbidden {
			t.Fatalf("an operator's PUT of audit.retention: answered %d %s, want 403", code, body)
		}

		if p := problemOf(t, body); p.Code != "forbidden" || p.Field != "audit.retention" {
			t.Errorf(
				"an operator's PUT of audit.retention: %s, want forbidden naming audit.retention",
				body,
			)
		}

		if after := storeOf(t, h); after != before {
			t.Errorf("the refused write changed the store:\n got %s\nwant %s", after, before)
		}
	})

	t.Run("as the admin", func(t *testing.T) {
		h, addr := judgedNode(t, entity.RoleAdmin)

		before := storeOf(t, h)

		code, body := writeOf(t, addr, http.MethodPut, "/api/v1/settings/nosuch.key",
			`{"spec":{"value":1}}`, "If-None-Match", "*")
		if code != http.StatusBadRequest {
			t.Fatalf("an admin's PUT of an unknown key: answered %d %s, want 400", code, body)
		}

		if p := problemOf(t, body); p.Code != "unknown_key" || p.Field != "nosuch.key" {
			t.Errorf(
				"an admin's PUT of an unknown key: %s, want unknown_key naming nosuch.key",
				body,
			)
		}

		if after := storeOf(t, h); after != before {
			t.Errorf("the refused write changed the store:\n got %s\nwant %s", after, before)
		}

		code, body = writeOf(t, addr, http.MethodPut, "/api/v1/settings/audit.retention",
			`{"spec":{"value":"2880h"}}`, "If-None-Match", "*")
		if code != http.StatusOK {
			t.Fatalf("an admin's PUT of audit.retention: answered %d %s, want 200", code, body)
		}

		rows := map[string]string{}

		for s, err := range h.Store().ControlPlane().Settings(t.Context(),
			entity.SettingScope{Kind: entity.ScopeInstallation}) {
			if err != nil {
				t.Fatalf("read the installation settings: %v", err)
			}

			rows[s.Key] = string(s.Value)
		}

		if v, ok := rows["audit.retention"]; !ok || v != `"2880h"` {
			t.Errorf("audit.retention %q after the admin's PUT, want the row \"2880h\"", v)
		}
	})
}

// judgedNode is a node serving its API signed in as role, started, with
// its settings table handed in, and its address.
func judgedNode(t *testing.T, role entity.APIRole) (*nodetest.Harness, string) {
	t.Helper()

	authenticate := func(*http.Request) (auth.Caller, error) {
		return auth.Caller{
			Principal: entity.Principal{Kind: entity.PrincipalUser, ID: entity.NewPrincipalID()},
			Role:      role,
		}, nil
	}

	h := nodetest.New(t, nodetest.Watch(),
		nodetest.WithNew(node.NewSigning(authenticate, func(context.Context) ([]byte, error) {
			return []byte("the table judges writes"), nil
		})))
	started(t, h)
	addr, _ := h.Node().Addrs()

	return h, addr
}
