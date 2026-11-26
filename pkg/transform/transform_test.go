package transform_test

import (
	"errors"
	"testing"

	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// A chain encodes in stage order and a reader refuses an envelope out of it,
// so the order is the stages' numbering, fixed from 1.
func TestStageOrder(t *testing.T) {
	for i, s := range []transform.Stage{transform.StageCompress, transform.StageEncrypt, transform.StageRedundancy} {
		if want := transform.Stage(i + 1); s != want {
			t.Errorf("stage %d = %d, want %d", i, s, want)
		}
	}
}

// A decode ends in one of five errors, each reported differently by the codec,
// so none may match another under errors.Is.
func TestDecodeErrorsAreDistinct(t *testing.T) {
	errs := map[string]error{
		"ErrMalformed":                transform.ErrMalformed,
		"ErrTooLarge":                 transform.ErrTooLarge,
		"keys.ErrUnknownMaterial":     keys.ErrUnknownMaterial,
		"keys.ErrMaterialUnavailable": keys.ErrMaterialUnavailable,
		"keys.ErrMaterialDestroyed":   keys.ErrMaterialDestroyed,
	}
	for an, a := range errs {
		for bn, b := range errs {
			if an != bn && errors.Is(a, b) {
				t.Errorf("errors.Is(%s, %s) = true, want false", an, bn)
			}
		}
	}
}
