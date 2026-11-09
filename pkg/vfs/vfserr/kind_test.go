package vfserr_test

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// An adapter's error table names a kind by a typed value, so under errors.Is
// every typed value vfs.Errors lists matches a fresh value of its own kind,
// no other kind, and its sentinel, wrapped or not.
func TestTypedErrorsMatchByKind(t *testing.T) {
	var typed []error
	for _, err := range vfs.Errors() {
		if sentinel(err) != nil {
			typed = append(typed, err)
		}
	}
	if len(typed) == 0 {
		t.Fatal("vfs.Errors lists no typed value")
	}
	for i, err := range typed {
		fresh := reflect.New(reflect.TypeOf(err).Elem())
		fresh.Elem().Set(reflect.ValueOf(err).Elem())
		for _, got := range []error{err, fmt.Errorf("op: %w", err)} {
			if !errors.Is(got, fresh.Interface().(error)) {
				t.Errorf("errors.Is(%v, %#v) is false, want its own kind matched", got, fresh.Interface())
			}
			if !errors.Is(got, sentinel(err)) {
				t.Errorf("errors.Is(%v, %v) is false, want its sentinel matched", got, sentinel(err))
			}
			for j, other := range typed {
				if j != i && errors.Is(got, other) {
					t.Errorf("errors.Is(%v, %#v) is true, want another kind refused", got, other)
				}
			}
		}
	}
}

// A zero cause is no cause, so it matches no declared one; a freeze's bound
// is not its kind.
func TestDelayMatchesNeitherAZeroCauseNorByBound(t *testing.T) {
	if errors.Is(&vfserr.Delay{Cause: vfserr.DelayRecall}, &vfserr.Delay{}) {
		t.Error("a zero Cause matched DelayRecall")
	}
	frozen := &vfserr.Delay{Cause: vfserr.DelayFrozen, Until: time.Now()}
	if !errors.Is(frozen, &vfserr.Delay{Cause: vfserr.DelayFrozen}) {
		t.Error("a bounded freeze did not match DelayFrozen")
	}
}

func sentinel(err error) error {
	switch err.(type) {
	case *vfserr.AccessError:
		return vfserr.ErrAccess
	case *vfserr.QuotaError:
		return vfserr.ErrQuota
	case *vfserr.Delay:
		return vfserr.ErrDelay
	case *vfserr.StateError:
		return vfserr.ErrState
	case *vfserr.CrossError:
		return vfserr.ErrCross
	}
	return nil
}
