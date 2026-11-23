package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestBuildInfoPresent(t *testing.T) {
	m := New("v1.2.3", "deadbeef")
	expected := `
# HELP dittofs_build_info Build information for the running DittoFS server (always 1).
# TYPE dittofs_build_info gauge
dittofs_build_info{commit="deadbeef",version="v1.2.3"} 1
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(expected), "dittofs_build_info"); err != nil {
		t.Fatalf("build_info mismatch: %v", err)
	}
}

func TestHandlerServesText(t *testing.T) {
	m := New("v-test", "abc123")
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}
