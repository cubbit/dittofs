package apiv1

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// A body holding a secret prints neither through slog's handlers nor through
// fmt, and keeps what is safe to print.
func TestSecretsStayOutOfLogs(t *testing.T) {
	created := time.Date(2026, 11, 17, 9, 30, 0, 0, time.UTC)
	cases := []struct {
		name   string
		body   any
		secret []string
		keep   string
	}{
		{"SignIn", SignIn{Name: "alice", Password: "hunter2"}, []string{"hunter2"}, "alice"},
		{"Tokens", Tokens{Access: "acc-secret", Refresh: "ref-secret"}, []string{"acc-secret", "ref-secret"}, "[redacted]"},
		{"Refresh", Refresh{Token: "ref-secret"}, []string{"ref-secret"}, "[redacted]"},
		{"PasswordChange", PasswordChange{Current: "old-secret", New: "new-secret"}, []string{"old-secret", "new-secret"}, "[redacted]"},
		{"SetPassword", SetPassword{Password: "hunter2"}, []string{"hunter2"}, "[redacted]"},
		{"APITokenStatus", APITokenStatus{Created: created, Token: "tok-secret"}, []string{"tok-secret"}, "2026-11-17"},
	}
	for _, c := range cases {
		out := map[string]string{}
		for kind, h := range map[string]func(*bytes.Buffer) slog.Handler{
			"slog text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
			"slog json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
		} {
			var b bytes.Buffer
			slog.New(h(&b)).Info("x", "body", c.body)
			out[kind] = b.String()
		}
		for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
			out["fmt "+verb] = fmt.Sprintf(verb, c.body)
		}
		for kind, s := range out {
			for _, secret := range c.secret {
				if strings.Contains(s, secret) {
					t.Errorf("%s through %s = %q, holds the secret %q", c.name, kind, s, secret)
				}
			}
			if !strings.Contains(s, c.keep) {
				t.Errorf("%s through %s = %q, want it to hold %q", c.name, kind, s, c.keep)
			}
		}
	}
}
