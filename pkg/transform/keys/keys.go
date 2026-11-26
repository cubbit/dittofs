// Package keys is the material contract of the transforms (RFC 5 §3.1): the
// name of one piece of material, the provider that holds it per namespace, the
// kinds read outside the provider, and the errors a read of material can end
// in. It imports nothing of package transform, so a provider implements it in
// a package of its own with no import cycle, and the codec and the engine
// import it without reaching any implementation.
package keys

import (
	"context"
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// MaterialID names one piece of material of one kind, of one namespace
// (RFC 5 §2.5). Everything outside the provider names material by this value,
// never by its content: the census records it as (kind, ID, fingerprint)
// (RFC 6 §2.3), and the chain ID and a namespace export carry its ID and
// fingerprint.
type MaterialID struct {
	Namespace   entity.NamespaceID
	Kind        string
	ID          string
	Fingerprint [32]byte // HMAC-SHA256(material, "dittofs key check v1"), RFC 5 §2.5
}

// Provider holds a store configuration's material, per namespace (RFC 5 §2.5,
// §3.1). Every call names the namespace, so a lookup never returns another
// namespace's key (RFC 5 §7 T14).
//
// decision: Provider declares only the reads. Remove, which destroys material
// once no census entry and no open pin holds it (RFC 5 §5.3), lands with its
// first caller, garbage collection retiring material or an operator destroying
// it, together with the count of open pins it waits on and ErrMaterialInUse,
// and is added then to every implementation. ErrNotEscrowed lands with the
// provider that first returns it. Declaring them now would oblige every
// implementation to answer a call no caller makes.
type Provider interface {
	// Current is the material new bodies of kind use in namespace ns. A kind
	// the provider holds no material of for ns is ErrUnknownMaterial; current
	// material it cannot reach now, or whose content no longer matches its
	// fingerprint, is ErrMaterialUnavailable; current material it has lost or
	// removed for good is ErrMaterialDestroyed. While the last state the
	// provider read of the master key that wraps ns's current data key is any
	// state but active, Current of KindDataKey for ns fails although the
	// provider still holds the data key: with ErrMaterialDestroyed when the key
	// service reports the master key destroyed, and with
	// ErrMaterialUnavailable for every other state, a state the provider does
	// not know included. A failed state read leaves the last state in force.
	// Until a state read of that master key succeeds there is no last state,
	// and Current of KindDataKey for ns fails with ErrMaterialUnavailable.
	// This stops new writes only: Get, and so a decode, is unchanged by it
	// (RFC 5 Appendix B.1). A chain's pin returns these unchanged.
	Current(ctx context.Context, ns entity.NamespaceID, kind string) (MaterialID, error)

	// Get returns the content of material id, looked up in id.Namespace:
	// an *UnknownMaterialError naming id for an ID the provider was never given,
	// ErrMaterialUnavailable for one it cannot reach now or whose content no
	// longer matches its fingerprint, ErrMaterialDestroyed for one it once had
	// and has lost or removed for good (RFC 5 §2.7).
	Get(ctx context.Context, id MaterialID) ([]byte, error)
}

// The kinds consumed outside the provider, by RFC 5 §2.5's strings. A kind is
// specific to what consumes it, so a key of one kind is never handed to a
// consumer of another.
const (
	// KindDataKey is the key the encryption transform encrypts bodies under.
	KindDataKey = "data-key"
	// KindChunkIDKey is the key every chunk ID of a namespace is computed under.
	KindChunkIDKey = "chunk-id-key"
	// KindChunkingKey is the key an encrypting namespace's carver draws its
	// keyed chunk boundaries from.
	KindChunkingKey = "chunking-key"
	// KindExportKey is the key that seals and authenticates a namespace's
	// exports and backup state objects.
	KindExportKey = "export-key"
)

// SalvageWrapper draws a fresh key for one salvage export of a stranded
// journal tag and wraps it under the installation's current master key
// (RFC 5 Appendix B.2).
type SalvageWrapper interface {
	// WrapSalvageKey returns the key and its wrap. export names the export;
	// the provider frames it into the wrap's associated data. wrap has RFC 5
	// Appendix B.1's stored-wrap layout, under associated data of its own,
	// since a salvage key belongs to no namespace.
	WrapSalvageKey(ctx context.Context, export []byte) (key, wrap []byte, err error)
}

// DestroyedNotice reports material the provider has found destroyed, so the
// engine can mark what was stored under it lost (RFC 5 §2.7).
type DestroyedNotice interface {
	// DestroyedSince returns the material found destroyed after cursor, and
	// the cursor to pass next. The cursor is derived from the provider's
	// durable records, so it means the same after a restart. A call may return
	// a bounded batch; the caller polls again until a call returns none.
	DestroyedSince(ctx context.Context, cursor uint64) (ids []MaterialID, next uint64, err error)
}

// The material errors a decode can end in (RFC 5 §2.7). Each is its own
// value, since the codec reports each differently.
var (
	// ErrUnknownMaterial is material the provider was never given. Get returns it
	// inside an *UnknownMaterialError, so errors.Is matches it.
	ErrUnknownMaterial = errors.New("keys: unknown material")
	// ErrMaterialUnavailable is material the provider holds but cannot reach
	// now, or whose content no longer matches its fingerprint.
	ErrMaterialUnavailable = errors.New("keys: material unavailable")
	// ErrMaterialDestroyed is material the provider once held and has lost or
	// removed for good.
	ErrMaterialDestroyed = errors.New("keys: material destroyed")
)

// UnknownMaterialError is ErrUnknownMaterial naming the material, so the codec
// can raise the configuration health condition naming the ID (RFC 5 §2.7)
// with nothing but the error in hand.
type UnknownMaterialError struct {
	ID MaterialID
}

func (e *UnknownMaterialError) Error() string {
	return fmt.Sprintf("%v: %s %q of namespace %v", ErrUnknownMaterial, e.ID.Kind, e.ID.ID, e.ID.Namespace)
}

// Unwrap returns ErrUnknownMaterial.
func (e *UnknownMaterialError) Unwrap() error { return ErrUnknownMaterial }
