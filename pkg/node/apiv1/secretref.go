package apiv1

// SecretRef is a secret-bearing field as every read answers it: the
// secret's reference and its current version, never its value.
type SecretRef struct {
	Ref     string `json:"ref"`
	Version uint64 `json:"version"`
}

// Credential is the body that sets a secret: its value, which is sealed
// before anything is written and never read back.
type Credential struct {
	Spec CredentialSpec `json:"spec"`
}

// CredentialSpec is a secret's value, base64 in JSON.
type CredentialSpec struct {
	Value []byte `json:"value"`
}
