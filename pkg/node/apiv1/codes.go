package apiv1

// Code names an error in a Problem. A component's own error values reach the
// client by name as Codes too; the constants here are the API's own.
type Code string

// The API's own codes.
const (
	CodeUnauthenticated        Code = "unauthenticated"
	CodeForbidden              Code = "forbidden"
	CodeGenerationMismatch     Code = "generation_mismatch"
	CodePreconditionRequired   Code = "precondition_required"
	CodeIdempotencyMismatch    Code = "idempotency_mismatch"
	CodeManaged                Code = "managed"
	CodeOperationRunning       Code = "operation_running"
	CodeNotCancellable         Code = "not_cancellable"
	CodeLastAdmin              Code = "last_admin"
	CodeNotACopy               Code = "not_a_copy"
	CodeJoined                 Code = "joined"
	CodePasswordChangeRequired Code = "password_change_required"
	CodeThrottled              Code = "throttled"
	CodeStarting               Code = "starting"
	CodeUnavailable            Code = "unavailable"
)

// apiCodes is the closed set of the API's own codes, each once.
var apiCodes = []Code{
	CodeUnauthenticated,
	CodeForbidden,
	CodeGenerationMismatch,
	CodePreconditionRequired,
	CodeIdempotencyMismatch,
	CodeManaged,
	CodeOperationRunning,
	CodeNotCancellable,
	CodeLastAdmin,
	CodeNotACopy,
	CodeJoined,
	CodePasswordChangeRequired,
	CodeThrottled,
	CodeStarting,
	CodeUnavailable,
}
