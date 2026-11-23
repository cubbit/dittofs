package errors

import (
	goerrors "errors"
	"fmt"
	"testing"
)

func TestErrorCodeString(t *testing.T) {
	cases := []struct {
		code ErrorCode
		want string
	}{
		{ErrNotFound, "NotFound"},
		{ErrAccessDenied, "AccessDenied"},
		{ErrAuthRequired, "AuthRequired"},
		{ErrPermissionDenied, "PermissionDenied"},
		{ErrAlreadyExists, "AlreadyExists"},
		{ErrNotEmpty, "NotEmpty"},
		{ErrIsDirectory, "IsDirectory"},
		{ErrNotDirectory, "NotDirectory"},
		{ErrInvalidArgument, "InvalidArgument"},
		{ErrIOError, "IOError"},
		{ErrNoSpace, "NoSpace"},
		{ErrQuotaExceeded, "QuotaExceeded"},
		{ErrReadOnly, "ReadOnly"},
		{ErrNotSupported, "NotSupported"},
		{ErrInvalidHandle, "InvalidHandle"},
		{ErrStaleHandle, "StaleHandle"},
		{ErrLocked, "Locked"},
		{ErrPrivilegeRequired, "PrivilegeRequired"},
		{ErrNameTooLong, "NameTooLong"},
		{ErrDeadlock, "Deadlock"},
		{ErrGracePeriod, "GracePeriod"},
		{ErrLockConflict, "LockConflict"},
		{ErrConflict, "Conflict"},
		{ErrCrossShare, "CrossShare"},
	}
	for _, c := range cases {
		if got := c.code.String(); got != c.want {
			t.Errorf("ErrorCode(%d).String() = %q, want %q", c.code, got, c.want)
		}
	}
}

func TestErrorCodeStringUnknown(t *testing.T) {
	// Zero value and an out-of-range code should both render as Unknown(N).
	if got := ErrorCode(0).String(); got != "Unknown(0)" {
		t.Errorf("ErrorCode(0).String() = %q, want %q", got, "Unknown(0)")
	}
	if got := ErrorCode(999).String(); got != "Unknown(999)" {
		t.Errorf("ErrorCode(999).String() = %q, want %q", got, "Unknown(999)")
	}
}

// Every named code must have a non-Unknown String mapping. This guards against
// adding a new code constant without a corresponding switch arm. The
// errCodeSentinel tail is excluded: it bounds the walks and has no arm.
func TestEveryCodeHasName(t *testing.T) {
	for c := ErrNotFound; c < errCodeSentinel; c++ {
		if s := c.String(); len(s) >= 7 && s[:7] == "Unknown" {
			t.Errorf("code %d has no String() name (got %q)", c, s)
		}
	}
}

func TestStoreErrorError(t *testing.T) {
	t.Run("with path", func(t *testing.T) {
		e := &StoreError{Code: ErrNotFound, Message: "file not found", Path: "/a/b"}
		want := "NotFound: file not found (path: /a/b)"
		if got := e.Error(); got != want {
			t.Errorf("Error() = %q, want %q", got, want)
		}
	})
	t.Run("without path", func(t *testing.T) {
		e := &StoreError{Code: ErrInvalidArgument, Message: "bad arg"}
		want := "InvalidArgument: bad arg"
		if got := e.Error(); got != want {
			t.Errorf("Error() = %q, want %q", got, want)
		}
	})
}

func TestFactoryFunctions(t *testing.T) {
	cases := []struct {
		name     string
		err      *StoreError
		wantCode ErrorCode
		wantPath string
		wantMsg  string
	}{
		{"NotFound", NewNotFoundError("/x", "file"), ErrNotFound, "/x", "file not found"},
		{"PermissionDenied", NewPermissionDeniedError("/x"), ErrPermissionDenied, "/x", "permission denied"},
		{"IsDirectory", NewIsDirectoryError("/x"), ErrIsDirectory, "/x", "is a directory"},
		{"NotDirectory", NewNotDirectoryError("/x"), ErrNotDirectory, "/x", "not a directory"},
		{"NotEmpty", NewNotEmptyError("/x"), ErrNotEmpty, "/x", "directory not empty"},
		{"AlreadyExists", NewAlreadyExistsError("/x"), ErrAlreadyExists, "/x", "already exists"},
		{"Conflict", NewConflictError("memory UpdateAttrs", "dup"), ErrConflict, "", "memory UpdateAttrs: dup"},
		{"InvalidArgument", NewInvalidArgumentError("nope"), ErrInvalidArgument, "", "nope"},
		{"AccessDenied", NewAccessDeniedError("no read bit"), ErrAccessDenied, "", "no read bit"},
		{"QuotaExceeded", NewQuotaExceededError("/x"), ErrQuotaExceeded, "/x", "disk quota exceeded"},
		{"PrivilegeRequired", NewPrivilegeRequiredError("chown"), ErrPrivilegeRequired, "", "operation requires root privileges: chown"},
		{"NameTooLong", NewNameTooLongError("/x"), ErrNameTooLong, "/x", "name too long"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.err.Code != c.wantCode {
				t.Errorf("Code = %v, want %v", c.err.Code, c.wantCode)
			}
			if c.err.Path != c.wantPath {
				t.Errorf("Path = %q, want %q", c.err.Path, c.wantPath)
			}
			if c.err.Message != c.wantMsg {
				t.Errorf("Message = %q, want %q", c.err.Message, c.wantMsg)
			}
		})
	}
}

// classifier captures one Is*Error predicate together with its name for
// table-driven assertions.
type classifier struct {
	name string
	fn   func(error) bool
}

var classifiers = []classifier{
	{"IsNotFoundError", IsNotFoundError},
	{"IsAlreadyExistsError", IsAlreadyExistsError},
	{"IsConflictError", IsConflictError},
}

// For each code, exactly the expected set of classifiers must return true.
// This pins the code->predicate mapping.
func TestClassifiers(t *testing.T) {
	// code -> set of classifier names expected to return true
	expect := map[ErrorCode]map[string]bool{
		ErrNotFound:      {"IsNotFoundError": true},
		ErrAlreadyExists: {"IsAlreadyExistsError": true},
		ErrConflict:      {"IsConflictError": true},
		// Every other code maps to no classifier, so its expected set is
		// all-false.
		// errCodeSentinel is excluded: it is a bound, not an arm, and
		// want[nil] would nil-map to all-false anyway.
	}

	for c := ErrNotFound; c < errCodeSentinel; c++ {
		err := &StoreError{Code: c, Message: "x"}
		want := expect[c]
		for _, cl := range classifiers {
			got := cl.fn(err)
			wantTrue := want[cl.name]
			if got != wantTrue {
				t.Errorf("code %s: %s = %v, want %v", c, cl.name, got, wantTrue)
			}
		}
	}
}

// Classifiers must unwrap wrapped StoreErrors via errors.As, so a
// fmt.Errorf("...: %w", storeErr) chain still classifies correctly.
func TestClassifiersUnwrapWrapped(t *testing.T) {
	base := NewNotFoundError("/p", "file")
	wrapped := fmt.Errorf("read failed: %w", base)
	if !IsNotFoundError(wrapped) {
		t.Error("IsNotFoundError should unwrap a wrapped StoreError")
	}

	deep := fmt.Errorf("layer2: %w", fmt.Errorf("layer1: %w", NewConflictError("op", "msg")))
	if !IsConflictError(deep) {
		t.Error("IsConflictError should unwrap a doubly-wrapped StoreError")
	}
}

// Non-StoreError values (nil, plain errors) must classify as false everywhere.
func TestClassifiersNonStoreError(t *testing.T) {
	for _, in := range []error{nil, goerrors.New("plain"), fmt.Errorf("wrapped: %w", goerrors.New("plain"))} {
		for _, cl := range classifiers {
			if cl.fn(in) {
				t.Errorf("%s(%v) = true, want false", cl.name, in)
			}
		}
	}
}

// errors.As must be able to extract a *StoreError from the concrete type.
func TestErrorsAsExtraction(t *testing.T) {
	orig := NewConflictError("badger UpdateAttrs", "transaction conflict")
	var got *StoreError
	if !goerrors.As(orig, &got) {
		t.Fatal("errors.As failed to extract *StoreError")
	}
	if got.Code != ErrConflict {
		t.Errorf("extracted Code = %v, want ErrConflict", got.Code)
	}
}

// ConflictOwnerID is a free-form field on StoreError used by the SMB lock
// path; it must round-trip through errors.As untouched.
func TestConflictOwnerIDRoundTrips(t *testing.T) {
	e := &StoreError{Code: ErrLocked, Message: "locked", ConflictOwnerID: "owner-42"}
	wrapped := fmt.Errorf("lock op: %w", e)
	var got *StoreError
	if !goerrors.As(wrapped, &got) {
		t.Fatal("errors.As failed")
	}
	if got.ConflictOwnerID != "owner-42" {
		t.Errorf("ConflictOwnerID = %q, want %q", got.ConflictOwnerID, "owner-42")
	}
}
