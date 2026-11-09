package vfs_test

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/vfs"
)

// An adapter's error table names a kind by a typed value, so under errors.Is
// every typed value vfs.Errors lists matches a fresh value of its own kind,
// no other kind, and its sentinel, wrapped or not. A zero value of its type
// is no kind, so it matches none.
func TestTypedErrorsMatchByKind(t *testing.T) {
	var typed []error
	for _, err := range vfs.Errors() {
		if kindSentinel(err) != nil {
			typed = append(typed, err)
		}
	}
	if len(typed) == 0 {
		t.Fatal("vfs.Errors lists no typed value")
	}
	for i, err := range typed {
		fresh := reflect.New(reflect.TypeOf(err).Elem())
		fresh.Elem().Set(reflect.ValueOf(err).Elem())
		zero := reflect.New(reflect.TypeOf(err).Elem()).Interface().(error)
		for _, got := range []error{err, fmt.Errorf("op: %w", err)} {
			if !errors.Is(got, fresh.Interface().(error)) {
				t.Errorf("errors.Is(%v, %#v) is false, want its own kind matched", got, fresh.Interface())
			}
			if !errors.Is(got, kindSentinel(err)) {
				t.Errorf("errors.Is(%v, %v) is false, want its sentinel matched", got, kindSentinel(err))
			}
			if errors.Is(got, zero) {
				t.Errorf("errors.Is(%v, %#v) is true, want a zero kind refused", got, zero)
			}
			for j, other := range typed {
				if j != i && errors.Is(got, other) {
					t.Errorf("errors.Is(%v, %#v) is true, want another kind refused", got, other)
				}
			}
		}
	}
}

// A freeze's bound is not its kind.
func TestDelayMatchesWhateverItsBound(t *testing.T) {
	frozen := &vfs.Delay{Cause: vfs.DelayFrozen, Until: time.Now()}
	if !errors.Is(frozen, &vfs.Delay{Cause: vfs.DelayFrozen}) {
		t.Error("a bounded freeze did not match DelayFrozen")
	}
}

// kindSentinel is the sentinel a typed error matches, nil for an error that
// is not typed.
func kindSentinel(err error) error {
	switch err.(type) {
	case *vfs.AccessError:
		return vfs.ErrAccess
	case *vfs.QuotaError:
		return vfs.ErrQuota
	case *vfs.Delay:
		return vfs.ErrDelay
	case *vfs.StateError:
		return vfs.ErrState
	case *vfs.CrossError:
		return vfs.ErrCross
	}
	return nil
}
