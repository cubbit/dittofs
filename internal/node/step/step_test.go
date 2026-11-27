package step_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/node/step"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// TestFailureSortsAStepsError pins the contract a step's error speaks: a
// problem or an error wrapping ErrStepFailed fails the operation with a
// recorded problem, anything else is retried.
func TestFailureSortsAStepsError(t *testing.T) {
	coded := &apiv1.Problem{Status: http.StatusConflict, Title: "busy", Code: "busy"}
	cases := []struct {
		name  string
		err   error
		fails bool
		want  apiv1.Problem
	}{
		{name: "no error", err: nil},
		{name: "plain error is retried", err: errors.New("transient")},
		{name: "ErrStepFailed fails as internal", err: fmt.Errorf("%w: bad target", step.ErrStepFailed), fails: true,
			want: apiv1.Problem{Status: http.StatusInternalServerError, Title: "the operation failed", Detail: "step failed: bad target"}},
		{name: "wrapped problem fails as itself", err: fmt.Errorf("wrap: %w", coded), fails: true, want: *coded},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body, fails := step.Failure(c.err)
			if fails != c.fails {
				t.Fatalf("Failure(%v) fails = %v, want %v", c.err, fails, c.fails)
			}
			if !fails {
				if body != nil {
					t.Fatalf("Failure(%v) body = %s, want none for a retried error", c.err, body)
				}
				return
			}
			var got apiv1.Problem
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("Failure(%v) body %s does not decode as a problem: %v", c.err, body, err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Failure(%v) problem = %+v, want %+v", c.err, got, c.want)
			}
		})
	}
}

// TestRunFailsAPanickingStep shows a step's panic comes back as an error that
// fails the operation instead of unwinding the runner.
func TestRunFailsAPanickingStep(t *testing.T) {
	_, err := step.Run(context.Background(), func(context.Context, entity.Operation) (step.StepResult, error) {
		panic("boom")
	}, entity.Operation{})
	if !errors.Is(err, step.ErrStepFailed) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Run of a panicking step: err = %v, want one wrapping ErrStepFailed naming the panic", err)
	}
	if _, fails := step.Failure(err); !fails {
		t.Fatalf("Failure(%v) = retried, want the panic to fail the operation", err)
	}
}
