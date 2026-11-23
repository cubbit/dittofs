package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/sim"
)

func TestStoreFaultReportsAnIncident(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	clock := sim.NewTestClock(time.Unix(0, 0))
	n := &Node{cfg: Config{Clock: clock, Logger: slog.New(slog.NewJSONHandler(&output, nil))}}
	ctx := t.Context()
	var fault storeFault
	initial := errors.New("the first read failed")
	latest := errors.New("the next read failed")

	fault.observed(ctx, n, stepReconcile, initial, slog.Attr{})
	clock.Advance(time.Minute - time.Second)
	fault.observed(ctx, n, stepReconcile, latest, slog.Attr{})
	fault.summarize(ctx, n, stepReconcile)
	clock.Advance(time.Second)
	fault.summarize(ctx, n, stepReconcile)
	// The store can hang through several reporting intervals. There is
	// still one incident, with no new failed calls while the loop waits.
	clock.Advance(2*time.Minute + 10*time.Second)
	fault.summarize(ctx, n, stepReconcile)
	fault.summarize(ctx, n, stepReconcile)
	clock.Advance(50 * time.Second)
	fault.observed(ctx, n, stepReconcile, latest, slog.Attr{})

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	fault.observed(cancelled, n, stepReconcile, nil, slog.Attr{})
	fault.observed(cancelled, n, stepReconcile, initial, slog.Attr{})
	fault.observed(ctx, n, stepReconcile, nil, slog.Attr{})
	fault.observed(ctx, n, stepReconcile, nil, slog.Attr{})
	fault.observed(ctx, n, stepReconcile, initial, slog.Attr{})

	type line struct {
		Level    string        `json:"level"`
		Message  string        `json:"msg"`
		Loop     string        `json:"loop"`
		Error    string        `json:"error"`
		Duration time.Duration `json:"duration"`
		Failures uint64        `json:"failures"`
		Total    uint64        `json:"failures_total"`
	}
	var got []line
	for _, raw := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n")) {
		var record line
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatalf("decode a log line: %v", err)
		}
		got = append(got, record)
	}
	want := []line{
		{Level: "ERROR", Message: "node store fault started", Loop: stepReconcile, Error: initial.Error()},
		{Level: "WARN", Message: "node store fault ongoing", Loop: stepReconcile, Error: latest.Error(), Duration: time.Minute, Failures: 1},
		{Level: "WARN", Message: "node store fault ongoing", Loop: stepReconcile, Error: latest.Error(), Duration: 190 * time.Second},
		{Level: "WARN", Message: "node store fault ongoing", Loop: stepReconcile, Error: latest.Error(), Duration: 4 * time.Minute, Failures: 1},
		{Level: "INFO", Message: "node store fault cleared", Loop: stepReconcile, Duration: 4 * time.Minute, Total: 3},
		{Level: "ERROR", Message: "node store fault started", Loop: stepReconcile, Error: initial.Error()},
	}
	if !slices.Equal(got, want) {
		t.Errorf("store fault logs: got\n%+v\nwant\n%+v", got, want)
	}
}
