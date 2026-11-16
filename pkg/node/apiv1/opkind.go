package apiv1

// OpKind is the kind of an operation, read as <resource>.<verb>.
type OpKind string

// The operation kinds: one per action that is an operation, one per delete
// that is one, and the deletion of the stored NT hashes that turning NTLM off
// starts.
const (
	OpRotateSigningKey      OpKind = "installation.rotate-signing-key"
	OpResolvePrincipal      OpKind = "installation.resolve-principal"
	OpTestDirectory         OpKind = "installation.test-directory"
	OpJoinDomain            OpKind = "installation.join-domain"
	OpRotateMachinePassword OpKind = "installation.rotate-machine-password"
	OpShareDelete           OpKind = "share.delete"
	OpUserDelete            OpKind = "user.delete"
	OpGroupDelete           OpKind = "group.delete"
	OpNTHashDelete          OpKind = "identity.nt-hash-delete"
)

// opKinds is the closed set of operation kinds, each once.
var opKinds = []OpKind{
	OpRotateSigningKey,
	OpResolvePrincipal,
	OpTestDirectory,
	OpJoinDomain,
	OpRotateMachinePassword,
	OpShareDelete,
	OpUserDelete,
	OpGroupDelete,
	OpNTHashDelete,
}
