package kmip

import "github.com/gemalto/kmip-go/kmip14"

// Class is what a read of a key's state means for the material under it.
// The zero value is Unavailable, so a Class nobody set fails closed.
type Class int

const (
	// Unavailable: the key cannot be used now, and nothing says its
	// material is gone.
	Unavailable Class = iota
	// Active: the key may wrap and unwrap.
	Active
	// Destroyed: the server reports the key's material destroyed.
	Destroyed
)

// Classify maps a state read, the state it returned and its error, to a
// Class.
//
// decision: only a read that answers Destroyed or Destroyed Compromised
// classes a key Destroyed. A read that failed, Item Not Found included, a
// state value KMIP 1.4 does not name, and every other state (Pre-Active,
// Deactivated, Compromised) class it Unavailable, so a misconfigured
// endpoint or a revoked key never reports material destroyed. Compromised
// still decrypts. Withdraw this if a KMIP profile ever reports destruction
// unambiguously on a failed read.
func Classify(state kmip14.State, err error) Class {
	if err != nil {
		return Unavailable
	}
	switch state {
	case kmip14.StateActive:
		return Active
	case kmip14.StateDestroyed, kmip14.StateDestroyedCompromised:
		return Destroyed
	default:
		return Unavailable
	}
}
