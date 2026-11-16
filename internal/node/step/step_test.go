package step_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/cubbit/dittofs/internal/node/step"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// A problem, wrapped or not, fails the operation as it is; an error wrapping
// ErrStepFailed fails it as an internal problem carrying the error's text;
// any other error, a context's included, is retried.
func TestFailed(t *testing.T) {
	gone := &apiv1.Problem{Status: 404, Code: "not_found", Title: "no such user"}
	for _, c := range []struct {
		name   string
		err    error
		failed bool
		want   apiv1.Problem
	}{
		{"a problem", gone, true, *gone},
		{"a wrapped problem", fmt.Errorf("batch: %w", gone), true, *gone},
		{"a step failure", fmt.Errorf("batch: %w", step.ErrStepFailed), true,
			apiv1.Problem{Status: 500, Title: "the operation failed", Detail: "batch: step failed"}},
		{"a plain error", errors.New("store unreachable"), false, apiv1.Problem{}},
		{"a cancelled context", context.Canceled, false, apiv1.Problem{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			body, failed := step.Failed(c.err)
			if failed != c.failed {
				t.Fatalf("Failed(%v): got failed %v, want %v", c.err, failed, c.failed)
			}
			if !failed {
				if body != nil {
					t.Errorf("Failed(%v) of a retried error: got body %s, want none", c.err, body)
				}
				return
			}
			var got apiv1.Problem
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("Failed(%v) body %s: %v", c.err, body, err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("Failed(%v) problem: got %+v, want %+v", c.err, got, c.want)
			}
		})
	}
}
