// Package errors provides error types and error codes for the metadata store.
// This is a leaf package with no internal dependencies, so the store, the
// filesystem service and their callers can all share it without circular imports.
package errors

import (
	goerrors "errors"
	"fmt"
	"slices"
)

// ErrorCode represents the type of error that occurred.
type ErrorCode int

const (
	// ErrNotFound indicates the requested resource does not exist.
	ErrNotFound ErrorCode = iota + 1

	// ErrAccessDenied indicates permission bit violations (POSIX EACCES).
	// Used when the caller lacks the required read/write/execute permission bits.
	ErrAccessDenied

	// ErrAuthRequired indicates authentication is required but not provided.
	ErrAuthRequired

	// ErrPermissionDenied indicates operation not permitted (POSIX EPERM).
	// Used when the operation requires ownership or root privileges.
	ErrPermissionDenied

	// ErrAlreadyExists indicates the resource already exists.
	ErrAlreadyExists

	// ErrNotEmpty indicates directory is not empty.
	ErrNotEmpty

	// ErrIsDirectory indicates operation not valid on directory.
	ErrIsDirectory

	// ErrNotDirectory indicates operation requires a directory.
	ErrNotDirectory

	// ErrInvalidArgument indicates an invalid argument was provided.
	ErrInvalidArgument

	// ErrIOError indicates an I/O error occurred.
	ErrIOError

	// ErrNoSpace indicates no space is available.
	ErrNoSpace

	// ErrQuotaExceeded indicates quota has been exceeded.
	ErrQuotaExceeded

	// ErrReadOnly indicates operation failed because filesystem is read-only.
	ErrReadOnly

	// ErrNotSupported indicates operation is not supported by implementation.
	ErrNotSupported

	// ErrInvalidHandle indicates the file handle is invalid.
	ErrInvalidHandle

	// ErrStaleHandle indicates the file handle is valid but stale.
	ErrStaleHandle

	// ErrLocked indicates the resource is locked.
	ErrLocked

	// ErrPrivilegeRequired indicates elevated privileges are required.
	ErrPrivilegeRequired

	// ErrNameTooLong indicates the name exceeds maximum length.
	ErrNameTooLong

	// ErrDeadlock indicates a deadlock would occur.
	ErrDeadlock

	// ErrGracePeriod indicates operation blocked by grace period.
	ErrGracePeriod

	// ErrLockConflict indicates a lock conflict (enhanced lock types).
	ErrLockConflict

	// ErrConflict indicates a rule refused a write in the state it found: a
	// namespace a share still names, a share holding snapshots or one a clone
	// onto another share still reads (DeleteNamespace, DeleteShare), or a share
	// not in the state its transition starts from (SetShareState). The refusal
	// lasts only as long as that state, so a caller may retry once it changes.
	ErrConflict

	// ErrCrossShare indicates a two-handle operation named handles from two
	// different shares. One condition, one code: NFS surfaces EXDEV from its
	// handler-level LINK/RENAME checks, so the store-layer guard
	// (requireSameShare) answers with this code and the protocol mappers lift
	// it to NFS3ErrXDev / NFS4ERR_XDEV / StatusNotSameDevice — not the
	// generic invalid-parameter error that masked the condition over SMB.
	ErrCrossShare

	// errCodeSentinel bounds the "every code is covered" iota-walks. Anchor
	// new codes BEFORE this line: a code appended after it silently escapes
	// every switch-exhaustiveness test.
	errCodeSentinel
)

// String returns a human-readable name for the error code.
func (e ErrorCode) String() string {
	switch e {
	case ErrNotFound:
		return "NotFound"
	case ErrAccessDenied:
		return "AccessDenied"
	case ErrAuthRequired:
		return "AuthRequired"
	case ErrPermissionDenied:
		return "PermissionDenied"
	case ErrAlreadyExists:
		return "AlreadyExists"
	case ErrNotEmpty:
		return "NotEmpty"
	case ErrIsDirectory:
		return "IsDirectory"
	case ErrNotDirectory:
		return "NotDirectory"
	case ErrInvalidArgument:
		return "InvalidArgument"
	case ErrIOError:
		return "IOError"
	case ErrNoSpace:
		return "NoSpace"
	case ErrQuotaExceeded:
		return "QuotaExceeded"
	case ErrReadOnly:
		return "ReadOnly"
	case ErrNotSupported:
		return "NotSupported"
	case ErrInvalidHandle:
		return "InvalidHandle"
	case ErrStaleHandle:
		return "StaleHandle"
	case ErrLocked:
		return "Locked"
	case ErrPrivilegeRequired:
		return "PrivilegeRequired"
	case ErrNameTooLong:
		return "NameTooLong"
	case ErrDeadlock:
		return "Deadlock"
	case ErrGracePeriod:
		return "GracePeriod"
	case ErrLockConflict:
		return "LockConflict"
	case ErrConflict:
		return "Conflict"
	case ErrCrossShare:
		return "CrossShare"
	default:
		return fmt.Sprintf("Unknown(%d)", e)
	}
}

// StoreError represents a metadata store error with an error code.
type StoreError struct {
	Code    ErrorCode
	Message string
	Path    string

	// ConflictOwnerID is the effective owner identifier of the lock that
	// caused the conflict (populated only for ErrLocked). The SMB handler
	// uses this to distinguish self-conflicts (same open re-locking its
	// own range) from cross-handle conflicts for the LOCK_NOT_GRANTED vs
	// FILE_LOCK_CONFLICT status mapping (MS-SMB2 3.3.5.14).
	ConflictOwnerID string

	// Cause is the underlying error that produced this StoreError. It is set
	// for errors mapped from the storage engine (e.g. a Badger SSI conflict)
	// so that errors.Is/errors.As can find the raw engine error through the
	// unwrap chain and the transaction layer can decide to retry.
	Cause error
}

// Error implements the error interface.
func (e *StoreError) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("%s: %s (path: %s)", e.Code, e.Message, e.Path)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap returns the underlying cause so errors.As/errors.Is can traverse the
// chain to the original storage-engine error. Returns nil when
// no cause was set, which is the common case.
func (e *StoreError) Unwrap() error { return e.Cause }

// ============================================================================
// Generic Factory Functions (no lock type dependencies)
// ============================================================================

// NewNotFoundError creates a NotFound error.
func NewNotFoundError(path, resourceType string) *StoreError {
	return &StoreError{
		Code:    ErrNotFound,
		Message: fmt.Sprintf("%s not found", resourceType),
		Path:    path,
	}
}

// NewPermissionDeniedError creates a PermissionDenied error.
func NewPermissionDeniedError(path string) *StoreError {
	return &StoreError{
		Code:    ErrPermissionDenied,
		Message: "permission denied",
		Path:    path,
	}
}

// NewIsDirectoryError creates an IsDirectory error.
func NewIsDirectoryError(path string) *StoreError {
	return &StoreError{
		Code:    ErrIsDirectory,
		Message: "is a directory",
		Path:    path,
	}
}

// NewNotDirectoryError creates a NotDirectory error.
func NewNotDirectoryError(path string) *StoreError {
	return &StoreError{
		Code:    ErrNotDirectory,
		Message: "not a directory",
		Path:    path,
	}
}

// NewNotEmptyError creates a NotEmpty error.
func NewNotEmptyError(path string) *StoreError {
	return &StoreError{
		Code:    ErrNotEmpty,
		Message: "directory not empty",
		Path:    path,
	}
}

// NewAlreadyExistsError creates an AlreadyExists error.
func NewAlreadyExistsError(path string) *StoreError {
	return &StoreError{
		Code:    ErrAlreadyExists,
		Message: "already exists",
		Path:    path,
	}
}

// NewConflictError creates a Conflict error. op is the calling operation
// name (e.g., "badger UpdateAttrs"); message is the specific reason (e.g.,
// "transaction conflict, retries exhausted").
func NewConflictError(op, message string) *StoreError {
	return &StoreError{
		Code:    ErrConflict,
		Message: fmt.Sprintf("%s: %s", op, message),
	}
}

// NewInvalidArgumentError creates an InvalidArgument error.
func NewInvalidArgumentError(message string) *StoreError {
	return &StoreError{
		Code:    ErrInvalidArgument,
		Message: message,
	}
}

// NewAccessDeniedError creates an AccessDenied error.
func NewAccessDeniedError(reason string) *StoreError {
	return &StoreError{
		Code:    ErrAccessDenied,
		Message: reason,
	}
}

// NewQuotaExceededError creates a QuotaExceeded error.
func NewQuotaExceededError(path string) *StoreError {
	return &StoreError{
		Code:    ErrQuotaExceeded,
		Message: "disk quota exceeded",
		Path:    path,
	}
}

// NewPrivilegeRequiredError creates a PrivilegeRequired error.
func NewPrivilegeRequiredError(operation string) *StoreError {
	return &StoreError{
		Code:    ErrPrivilegeRequired,
		Message: fmt.Sprintf("operation requires root privileges: %s", operation),
	}
}

// NewNameTooLongError creates a NameTooLong error.
func NewNameTooLongError(path string) *StoreError {
	return &StoreError{
		Code:    ErrNameTooLong,
		Message: "name too long",
		Path:    path,
	}
}

// ============================================================================
// Error Type Checking Helpers
// ============================================================================

// hasCode reports whether err is, or wraps, a StoreError carrying one of the
// given codes.
func hasCode(err error, codes ...ErrorCode) bool {
	var storeErr *StoreError
	if !goerrors.As(err, &storeErr) {
		return false
	}
	return slices.Contains(codes, storeErr.Code)
}

// IsNotFoundError returns true if the error is a NotFound error.
// Unwraps via errors.As so wrapped StoreErrors (fmt.Errorf("...: %w", storeErr))
// still classify correctly.
func IsNotFoundError(err error) bool {
	return hasCode(err, ErrNotFound)
}

// IsAlreadyExistsError returns true if the error is, or wraps, an
// AlreadyExists error.
func IsAlreadyExistsError(err error) bool {
	return hasCode(err, ErrAlreadyExists)
}

// IsAccessDeniedError returns true if the error is a permission denial
// (POSIX EACCES), whether from file mode bits or an ACL.
//
// ErrReadOnly is deliberately not included: it is a distinct outcome (EROFS),
// and every caller so far wants to tell the two apart.
func IsAccessDeniedError(err error) bool {
	return hasCode(err, ErrAccessDenied)
}

// IsConflictError returns true if the error is a Conflict error
// Unwraps via errors.As so wrapped StoreErrors classify
// correctly.
func IsConflictError(err error) bool {
	return hasCode(err, ErrConflict)
}
