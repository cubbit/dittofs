package s3

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/cubbit/dittofs/pkg/remote"
)

// opKind is the call a reply answers, which decides the rows that apply to it.
// Its String is the call's op label (RFC 4 §4.12).
type opKind uint8

const (
	opPut        opKind = iota + 1 // a block put
	opPutControl                   // a control object's put
	opGet                          // a block's get
	opGetControl                   // a control object's get
	opDelete                       // a one-name DeleteObject
	opList                         // a ListObjectsV2 page
	opDeleteKey                    // one key's error inside a DeleteObjects reply
	opSetting                      // a bucket-setting read of the capability check
	opHealth                       // a probe, recorded only: it is judged as the control put or get it rides on
)

func (o opKind) String() string {
	switch o {
	case opPut:
		return "put"
	case opPutControl:
		return "put_control"
	case opGet:
		return "get"
	case opGetControl:
		return "get_control"
	case opDelete:
		return "delete"
	case opList:
		return "list"
	case opDeleteKey:
		return "delete_key"
	case opSetting:
		return "setting"
	case opHealth:
		return "health"
	}

	return fmt.Sprintf("opKind(%d)", uint8(o))
}

// recorded is the status and code the capability check saw its chosen checksum
// rejected with; the zero value is none. A put passes it only when it carried
// that checksum.
type recorded struct {
	status int
	code   string
}

// quotaCodes are the error codes the profile's services answer a reached quota
// or a full store with, whatever the status (RFC 4 Appendix B).
var quotaCodes = []string{"XMinioAdminBucketQuotaExceeded"}

// codeStatus is the status the S3 error reference documents for each code. A
// key's error inside a DeleteObjects reply carries only its code, so it is read
// as this status and then classified like any other reply.
var codeStatus = map[string]int{
	"AccessControlListNotSupported":           400,
	"AccessDenied":                            403,
	"AccessPointAlreadyOwnedByYou":            409,
	"AccountProblem":                          403,
	"AllAccessDisabled":                       403,
	"AmbiguousGrantByEmailAddress":            400,
	"AuthorizationHeaderMalformed":            400,
	"BadDigest":                               400,
	"BucketAlreadyExists":                     409,
	"BucketAlreadyOwnedByYou":                 409,
	"BucketNotEmpty":                          409,
	"ConditionalRequestConflict":              409,
	"CredentialsNotSupported":                 400,
	"CrossLocationLoggingProhibited":          403,
	"EncryptionTypeMismatch":                  400,
	"EntityTooLarge":                          400,
	"EntityTooSmall":                          400,
	"ExpiredToken":                            400,
	"IllegalVersioningConfigurationException": 400,
	"IncompleteBody":                          400,
	"IncorrectNumberOfFilesInPostRequest":     400,
	"InlineDataTooLarge":                      400,
	"InternalError":                           500,
	"InvalidAccessKeyId":                      403,
	"InvalidArgument":                         400,
	"InvalidBucketName":                       400,
	"InvalidBucketState":                      409,
	"InvalidDigest":                           400,
	"InvalidEncryptionAlgorithmError":         400,
	"InvalidLocationConstraint":               400,
	"InvalidObjectState":                      403,
	"InvalidPart":                             400,
	"InvalidPartOrder":                        400,
	"InvalidPayer":                            403,
	"InvalidPolicyDocument":                   400,
	"InvalidRange":                            416,
	"InvalidRequest":                          400,
	"InvalidSecurity":                         403,
	"InvalidSOAPRequest":                      400,
	"InvalidStorageClass":                     400,
	"InvalidTag":                              400,
	"InvalidTargetBucketForLogging":           400,
	"InvalidToken":                            400,
	"InvalidURI":                              400,
	"InvalidWriteOffset":                      400,
	"KeyTooLongError":                         400,
	"MalformedACLError":                       400,
	"MalformedPOSTRequest":                    400,
	"MalformedXML":                            400,
	"MaxMessageLengthExceeded":                400,
	"MaxPostPreDataLengthExceededError":       400,
	"MetadataTooLarge":                        400,
	"MethodNotAllowed":                        405,
	"MissingContentLength":                    411,
	"MissingRequestBodyError":                 400,
	"MissingSecurityElement":                  400,
	"MissingSecurityHeader":                   400,
	"NoLoggingStatusForKey":                   400,
	"NoSuchBucket":                            404,
	"NoSuchBucketPolicy":                      404,
	"NoSuchKey":                               404,
	"NoSuchLifecycleConfiguration":            404,
	"NoSuchObjectLockConfiguration":           404,
	"NoSuchTagSet":                            404,
	"NoSuchUpload":                            404,
	"NoSuchVersion":                           404,
	"NotImplemented":                          501,
	"NotSignedUp":                             403,
	"ObjectNotInActiveTierError":              403,
	"OperationAborted":                        409,
	"PermanentRedirect":                       301,
	"PreconditionFailed":                      412,
	"Redirect":                                307,
	"RequestIsNotMultiPartContent":            400,
	"RequestTimeout":                          400,
	"RequestTimeTooSkewed":                    403,
	"RequestTorrentOfBucketError":             400,
	"RestoreAlreadyInProgress":                409,
	"ServiceUnavailable":                      503,
	"SignatureDoesNotMatch":                   403,
	"SlowDown":                                503,
	"TemporaryRedirect":                       307,
	"TokenRefreshRequired":                    400,
	"TooManyBuckets":                          400,
	"UnexpectedContent":                       400,
	"UnresolvableGrantByEmailAddress":         400,
	"UserKeyMustBeSpecified":                  400,
	"XAmzContentSHA256Mismatch":               400,
}

// classify maps one reply to the error of RFC 4 §4.8's closed set that
// Appendix C.3's table gives it: the call op, the reply's status (0 when only a
// code is known, as for a key inside a DeleteObjects reply, which is read by
// its code) and code ("" when it carried
// none), and rec, the check's recorded checksum rejection. It is total: every
// input returns an error wrapping exactly one sentinel, but an absent key to a
// delete, which is the delete done and returns nil.
func classify(op opKind, status int, code string, rec recorded) error {
	switch {
	case status == http.StatusInsufficientStorage || slices.Contains(quotaCodes, code):
		return denied(op, status, code, remote.CauseQuota)
	case status == 0:
		s, ok := codeStatus[code]
		if !ok {
			// A code the reference does not list: a delete is idempotent, so a
			// retry is safe, where ErrInvalid would leave the object and the
			// caller's record of it both in place with no retry.
			return wrap(op, status, code, remote.ErrTransient)
		}

		return classify(op, s, code, rec)
	case status == http.StatusRequestEntityTooLarge || code == "EntityTooLarge":
		switch op {
		case opPut:
			// decision: a block put's size refusal is cause size, neither access
			// nor quota, because nothing is full and no credential is wrong, so
			// the operator is told which setting to change. The engine answers
			// it as it answers the other two, out of space, since only an
			// operator clears any of them. The rule leans on the block-size cap;
			// withdraw it, and treat a size refusal as ErrInvalid for that put
			// alone, if a block put can ever exceed a service's documented
			// single-put limit.
			return denied(op, status, code, remote.CauseSize)
		case opPutControl:
			return wrap(op, status, code, remote.ErrInvalid)
		}
	}

	// status is never 0 here, so a zero rec matches nothing. A recorded 413 or
	// EntityTooLarge is taken by the size row above: quota and size come first.
	if (op == opPut || op == opPutControl) && rec == (recorded{status, code}) {
		return wrap(op, status, code, remote.ErrCorrupt)
	}

	switch {
	case status == http.StatusNotFound && code == "NoSuchKey" && (op == opDelete || op == opDeleteKey):
		return nil
	case status == http.StatusNotFound && (code == "NoSuchKey" || code == "NoSuchVersion" && (op == opGet || op == opGetControl)):
		return wrap(op, status, code, remote.ErrNotFound)
	case status >= 300 && status < 400,
		status == http.StatusUnauthorized,
		status == http.StatusForbidden,
		status == http.StatusNotFound && code == "NoSuchBucket",
		status == http.StatusBadRequest && code == "InvalidToken":
		return denied(op, status, code, remote.CauseAccess)
	case status == http.StatusTooManyRequests,
		status == http.StatusServiceUnavailable && code == "SlowDown":
		return wrap(op, status, code, remote.ErrThrottled)
	case status == http.StatusBadRequest && (code == "BadDigest" || code == "InvalidDigest" || code == "XAmzContentSHA256Mismatch"):
		return wrap(op, status, code, remote.ErrCorrupt)
	case status == http.StatusBadRequest && (code == "ExpiredToken" || code == "RequestTimeout"),
		status == http.StatusRequestTimeout,
		status == http.StatusConflict && code == "OperationAborted":
		return wrap(op, status, code, remote.ErrTransient)
	case status == http.StatusNotImplemented,
		status >= 400 && status < 500:
		return wrap(op, status, code, remote.ErrInvalid)
	}

	// Any other 5xx, a 1xx or 2xx error, and a status outside 100 to 599.
	return wrap(op, status, code, remote.ErrTransient)
}

// fromSDK maps what an SDK call returned to the store's error. local is what a
// put's stream guard recorded, the body reader's own error or the guard's
// refusal; it comes back as itself, as does ctx's error once ctx has ended. A
// reply goes through classify; anything else, a network failure, a timeout, a
// certificate the client cannot verify, a failure reading a reply body (the
// SDK's checksum validation among them), is ErrTransient. The SDK's error is
// never wrapped.
func fromSDK(ctx context.Context, op opKind, rec recorded, local, err error) error {
	switch {
	case local != nil:
		return local
	case err == nil:
		return nil
	case ctx.Err() != nil:
		// The caller's context, not the error chain: a dial or header timeout
		// also matches context.DeadlineExceeded, and is the service's failure.
		return ctx.Err()
	}

	status, code := reply(err)
	if status == 0 {
		return wrap(op, 0, "", remote.ErrTransient)
	}

	return classify(op, status, code, rec)
}

// reply is the status and code of the reply behind an SDK error, status 0 when
// none came back (the SDK wraps a failure to send in a ResponseError of status
// 0). A code the SDK made up, from the status text or its UnknownError for a
// body with no code, is no code, and a body that does not parse is its status
// alone, so a proxy's HTML 413 stays a size refusal. The capability check
// records its rejection through it, so the recorded code compares with the
// code classify is given.
func reply(err error) (status int, code string) {
	var re *smithyhttp.ResponseError
	if !errors.As(err, &re) {
		return 0, ""
	}

	status = re.HTTPStatusCode()
	if api, ok := errors.AsType[smithy.APIError](err); ok {
		code = api.ErrorCode()

		text := http.StatusText(status)
		if code == "UnknownError" || code == strings.ReplaceAll(text, " ", "") && api.ErrorMessage() == text {
			code = ""
		}
	}

	return status, code
}

// denied is an ErrDenied of cause, carrying the reply's code.
func denied(op opKind, status int, code string, cause remote.Cause) error {
	return wrap(op, status, code, &remote.DeniedError{Cause: cause, Code: code})
}

// wrap names the call, the status and the code, and wraps err, the one sentinel
// (or DeniedError); never the reply's message or a header.
func wrap(op opKind, status int, code string, err error) error {
	if code == "" {
		code = "no code"
	}

	return fmt.Errorf("remote/s3: %s: status %d, %s: %w", op, status, code, err)
}
