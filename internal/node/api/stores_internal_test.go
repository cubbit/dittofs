package api

import (
	"bytes"
	"context"
	"testing"

	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// A refusal of the create's step clears the credential's value out of the
// request's memory: the bytes the body carried do not outlive the refusal
// for its reference or its version.
func TestSealStoreClearsARefusedValue(t *testing.T) {
	rq := request[storeDoc]{}
	rq.Body.Spec.Credential = &apiv1.StoreCredential{Ref: "se_1", Value: []byte("s3-secret")}

	if _, err := sealStore(context.Background(), rq); err == nil {
		t.Fatalf("the create naming its credential's reference: sealed, want the refusal")
	}

	value := rq.Body.Spec.Credential.Value

	if bytes.Equal(value, []byte("s3-secret")) {
		t.Errorf("the refused credential's value stays in memory: %q", value)
	}
}
