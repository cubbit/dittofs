package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// answerOf is how one error is answered: its status, its code, and whether
// the same request may succeed later unchanged.
type answerOf struct {
	status    int
	code      apiv1.Code
	retryable bool
}

// own answers each of the API's own codes.
var own = map[apiv1.Code]answerOf{
	apiv1.CodeUnauthenticated:        {http.StatusUnauthorized, apiv1.CodeUnauthenticated, false},
	apiv1.CodeForbidden:              {http.StatusForbidden, apiv1.CodeForbidden, false},
	apiv1.CodeGenerationMismatch:     {http.StatusPreconditionFailed, apiv1.CodeGenerationMismatch, false},
	apiv1.CodePreconditionRequired:   {http.StatusPreconditionRequired, apiv1.CodePreconditionRequired, false},
	apiv1.CodeIdempotencyMismatch:    {http.StatusUnprocessableEntity, apiv1.CodeIdempotencyMismatch, false},
	apiv1.CodeManaged:                {http.StatusConflict, apiv1.CodeManaged, false},
	apiv1.CodeOperationRunning:       {http.StatusConflict, apiv1.CodeOperationRunning, true},
	apiv1.CodeNotCancellable:         {http.StatusConflict, apiv1.CodeNotCancellable, false},
	apiv1.CodeLastAdmin:              {http.StatusConflict, apiv1.CodeLastAdmin, false},
	apiv1.CodeNotACopy:               {http.StatusConflict, apiv1.CodeNotACopy, false},
	apiv1.CodePasswordChangeRequired: {http.StatusForbidden, apiv1.CodePasswordChangeRequired, false},
	apiv1.CodeThrottled:              {http.StatusTooManyRequests, apiv1.CodeThrottled, true},
	apiv1.CodeStarting:               {http.StatusServiceUnavailable, apiv1.CodeStarting, true},
	apiv1.CodeUnavailable:            {http.StatusServiceUnavailable, apiv1.CodeUnavailable, true},
}

// storeCode is a metadata store error's code, as an error value a row of
// components can name; a *merrs.StoreError matches the row of its code.
type storeCode merrs.ErrorCode

func (c storeCode) Error() string { return merrs.ErrorCode(c).String() }

// components answers each component error, by its name: the first row whose
// error the answered one is, under errors.Is, and only when none is, the row
// of its *merrs.StoreError's code, so a cause the store error carries answers
// before the code that wraps it. A typed error matches the row of the
// sentinel it is, so vfserr's *AccessError answers as ErrAccess.
//
// Identity's logon failure and unknown principal are to be declared in
// vfserr, whose every error this table must name, and its unavailable is
// already vfserr.ErrUnavailable.
var components = []struct {
	err error
	answerOf
}{
	{entity.ErrGenerationMismatch, answerOf{http.StatusPreconditionFailed, "generation_mismatch", false}},
	{entity.ErrIdempotencyMismatch, answerOf{http.StatusUnprocessableEntity, "idempotency_mismatch", false}},
	{entity.ErrOperationRunning, answerOf{http.StatusConflict, apiv1.CodeOperationRunning, true}},
	{entity.ErrInvalidName, answerOf{http.StatusBadRequest, "invalid_name", false}},
	{entity.ErrNameTooLong, answerOf{http.StatusBadRequest, "name_too_long", false}},
	{entity.ErrSyntax, answerOf{http.StatusBadRequest, "syntax", false}},
	{entity.ErrInvalidFile, answerOf{http.StatusBadRequest, "invalid_file", false}},
	{entity.ErrDelay, answerOf{http.StatusConflict, "delay", true}},
	{entity.ErrStale, answerOf{http.StatusConflict, "stale", false}},
	{entity.ErrWrongShare, answerOf{http.StatusBadRequest, "wrong_share", false}},
	{entity.ErrCrossProject, answerOf{http.StatusConflict, "cross_project", false}},
	{entity.ErrInconsistent, answerOf{http.StatusInternalServerError, "inconsistent", false}},
	{entity.ErrMoving, answerOf{http.StatusConflict, "moving", true}},
	{entity.ErrRehoming, answerOf{http.StatusConflict, "rehoming", true}},
	// The installation's state refuses a new share, for good.
	{entity.ErrTagsExhausted, answerOf{http.StatusConflict, "tags_exhausted", false}},

	{vfserr.ErrNotFound, answerOf{http.StatusNotFound, "not_found", false}},
	{vfserr.ErrExist, answerOf{http.StatusConflict, "exist", false}},
	{vfserr.ErrNotEmpty, answerOf{http.StatusConflict, "not_empty", false}},
	{vfserr.ErrNotDir, answerOf{http.StatusBadRequest, "not_dir", false}},
	{vfserr.ErrIsDir, answerOf{http.StatusBadRequest, "is_dir", false}},
	{vfserr.ErrAccess, answerOf{http.StatusForbidden, "access", false}},
	{vfserr.ErrPerm, answerOf{http.StatusForbidden, "perm", false}},
	{vfserr.ErrStale, answerOf{http.StatusConflict, "stale", false}},
	{vfserr.ErrReadOnly, answerOf{http.StatusConflict, "read_only", false}},
	{vfserr.ErrNameTooLong, answerOf{http.StatusBadRequest, "name_too_long", false}},
	{vfserr.ErrInvalid, answerOf{http.StatusBadRequest, "invalid", false}},
	{vfserr.ErrNoSpace, answerOf{http.StatusConflict, "no_space", false}},
	{vfserr.ErrQuota, answerOf{http.StatusConflict, "quota", false}},
	{vfserr.ErrLocked, answerOf{http.StatusConflict, "locked", true}},
	{vfserr.ErrRangeNotLocked, answerOf{http.StatusConflict, "range_not_locked", false}},
	{vfserr.ErrShareViolation, answerOf{http.StatusConflict, "share_violation", true}},
	{vfserr.ErrDeletePending, answerOf{http.StatusConflict, "delete_pending", false}},
	{vfserr.ErrGrace, answerOf{http.StatusServiceUnavailable, "grace", true}},
	{vfserr.ErrDelay, answerOf{http.StatusConflict, "delay", true}},
	{vfserr.ErrBlocked, answerOf{http.StatusConflict, "blocked", true}},
	{vfserr.ErrWrongSecurity, answerOf{http.StatusForbidden, "wrong_security", false}},
	{vfserr.ErrState, answerOf{http.StatusConflict, "state", false}},
	{vfserr.ErrCross, answerOf{http.StatusConflict, "cross", false}},
	{vfserr.ErrIncomplete, answerOf{http.StatusConflict, "incomplete", false}},
	{vfserr.ErrInconsistent, answerOf{http.StatusInternalServerError, "inconsistent", false}},
	{vfserr.ErrLost, answerOf{http.StatusInternalServerError, "lost", false}},
	{vfserr.ErrUnavailable, answerOf{http.StatusServiceUnavailable, "unavailable", true}},
	{vfserr.ErrCorrupt, answerOf{http.StatusInternalServerError, "corrupt", false}},
	{vfserr.ErrIO, answerOf{http.StatusInternalServerError, "io", false}},
	{vfserr.ErrPastEnd, answerOf{http.StatusBadRequest, "past_end", false}},
	{vfserr.ErrFileOpen, answerOf{http.StatusInternalServerError, "file_open", false}},

	// ponytail: the metadata store's codes, and kv's errors below, are
	// answered here because kvstore passes them up from Write untranslated.
	// Upgrade by translating them to vfserr or entity errors at Write, after
	// which merrs and kv leave this table and its covered packages.
	{storeCode(merrs.ErrNotFound), answerOf{http.StatusNotFound, "not_found", false}},
	{storeCode(merrs.ErrAccessDenied), answerOf{http.StatusForbidden, "access_denied", false}},
	{storeCode(merrs.ErrAuthRequired), answerOf{http.StatusForbidden, "auth_required", false}},
	{storeCode(merrs.ErrPermissionDenied), answerOf{http.StatusForbidden, "permission_denied", false}},
	{storeCode(merrs.ErrAlreadyExists), answerOf{http.StatusConflict, "already_exists", false}},
	{storeCode(merrs.ErrNotEmpty), answerOf{http.StatusConflict, "not_empty", false}},
	{storeCode(merrs.ErrIsDirectory), answerOf{http.StatusBadRequest, "is_directory", false}},
	{storeCode(merrs.ErrNotDirectory), answerOf{http.StatusBadRequest, "not_directory", false}},
	{storeCode(merrs.ErrInvalidArgument), answerOf{http.StatusBadRequest, "invalid_argument", false}},
	{storeCode(merrs.ErrIOError), answerOf{http.StatusInternalServerError, "io_error", false}},
	{storeCode(merrs.ErrNoSpace), answerOf{http.StatusConflict, "no_space", false}},
	{storeCode(merrs.ErrQuotaExceeded), answerOf{http.StatusConflict, "quota_exceeded", false}},
	{storeCode(merrs.ErrReadOnly), answerOf{http.StatusConflict, "read_only", false}},
	{storeCode(merrs.ErrNotSupported), answerOf{http.StatusBadRequest, "not_supported", false}},
	{storeCode(merrs.ErrInvalidHandle), answerOf{http.StatusBadRequest, "invalid_handle", false}},
	{storeCode(merrs.ErrStaleHandle), answerOf{http.StatusConflict, "stale_handle", false}},
	{storeCode(merrs.ErrLocked), answerOf{http.StatusConflict, "locked", true}},
	{storeCode(merrs.ErrLockNotFound), answerOf{http.StatusConflict, "lock_not_found", false}},
	{storeCode(merrs.ErrPrivilegeRequired), answerOf{http.StatusForbidden, "privilege_required", false}},
	{storeCode(merrs.ErrNameTooLong), answerOf{http.StatusBadRequest, "name_too_long", false}},
	{storeCode(merrs.ErrDeadlock), answerOf{http.StatusConflict, "deadlock", true}},
	{storeCode(merrs.ErrGracePeriod), answerOf{http.StatusServiceUnavailable, "grace_period", true}},
	{storeCode(merrs.ErrLockLimitExceeded), answerOf{http.StatusConflict, "lock_limit_exceeded", true}},
	{storeCode(merrs.ErrLockConflict), answerOf{http.StatusConflict, "lock_conflict", true}},
	{storeCode(merrs.ErrConnectionLimitReached), answerOf{http.StatusServiceUnavailable, "connection_limit_reached", true}},
	{storeCode(merrs.ErrConflict), answerOf{http.StatusConflict, "conflict", true}},
	{storeCode(merrs.ErrCrossShare), answerOf{http.StatusBadRequest, "cross_share", false}},

	{kv.ErrNotFound, answerOf{http.StatusNotFound, "not_found", false}},
	// A transaction that kept conflicting until its request's context ended
	// is the store's conflict, answered as its code is above: the same
	// request, once the contention passes, may succeed.
	{kv.ErrConflict, answerOf{http.StatusConflict, "conflict", true}},
	{kv.ErrTooLarge, answerOf{http.StatusBadRequest, "too_large", false}},
	{kv.ErrUncommittedVersion, answerOf{http.StatusInternalServerError, "uncommitted_version", false}},
	// A closed store is a node stopping, which a restarted one answers.
	{kv.ErrClosed, answerOf{http.StatusServiceUnavailable, "closed", true}},

	// A request whose client went away, or whose deadline passed, is
	// refused as the node being unable to answer it now.
	{context.Canceled, answerOf{http.StatusServiceUnavailable, apiv1.CodeUnavailable, true}},
	{context.DeadlineExceeded, answerOf{http.StatusServiceUnavailable, apiv1.CodeUnavailable, true}},
}

// refusal is a refusal in the API's own terms: one of its codes, with text
// for a person, what it names, and how long the caller should wait before
// trying again, zero for no wait of its own.
type refusal struct {
	code   apiv1.Code
	detail string
	names  []string
	wait   time.Duration
}

func (r *refusal) Error() string { return string(r.code) + ": " + r.detail }

// RetryAfter is how long the caller should wait before trying again.
func (r *refusal) RetryAfter() time.Duration { return r.wait }

// problemOf is err's problem body, without its request ID, and whether the
// table names err: an error it does not name is a 500 with no code, since no
// code of the closed set names it, which the caller counts and logs. Only a
// refusal's or a refused field's text reaches the detail; any other error's
// text stays in the log, since it may carry what no caller should read.
func problemOf(err error) (apiv1.Problem, bool) {
	a, ok := lookup(err)
	if !ok {
		a = answerOf{status: http.StatusInternalServerError}
	}
	p := apiv1.Problem{Status: a.status, Code: a.code, Title: http.StatusText(a.status), Retryable: a.retryable}
	var r *refusal
	var f *fieldError
	var running *entity.OperationRunningError
	switch {
	case !ok:
		p.Detail = "internal error"
	case errors.As(err, &r):
		p.Detail, p.Names = r.detail, r.names
		// A refusal that carries a negative wait is final: no wait makes a
		// retry answer otherwise.
		p.Retryable = p.Retryable && r.wait >= 0
	case errors.As(err, &f):
		p.Field, p.Detail = f.field, f.detail
	case errors.As(err, &running):
		p.Detail, p.Names = "an operation of this kind runs on this target", []string{idString(running.ID)}
	}
	return p, ok
}

func lookup(err error) (answerOf, bool) {
	var r *refusal
	if errors.As(err, &r) {
		a, ok := own[r.code]
		return a, ok
	}
	if a, ok := row(err); ok {
		return a, true
	}
	var se *merrs.StoreError
	if errors.As(err, &se) {
		return row(storeCode(se.Code))
	}
	return answerOf{}, false
}

// row is the first component row err is.
func row(err error) (answerOf, bool) {
	for _, c := range components {
		if errors.Is(err, c.err) {
			return c.answerOf, true
		}
	}
	return answerOf{}, false
}
