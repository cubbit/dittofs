// Package chain holds the transforms' registry and builds the chain that
// implements transform.Chain and transform.Attempt (RFC 5 §3.1). Each shipped
// transform registers here at init; the node's content wiring builds a chain
// per store configuration and injects it into the codec and the engine.
package chain

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// Register makes a transform available for configuration, by name, and for
// decoding, by ID (RFC 5 §2.5, §3.1). It is called at init; a duplicate ID
// panics, since an ID is assigned once and never reused (RFC 5 §4), and so does
// a duplicate name, since a configuration names its transform by name. Until
// the registry is built it records nothing.
func Register(id transform.ID, name string, f transform.Factory) {}

// NewChain builds the configured transforms, each through its registered
// Factory with m, and the decoders of every registered transform (RFC 5
// §2.5). It refuses, rather than building a chain without it (RFC 5 §2.7): an
// unregistered name, in cfg or in deps.Require; a factory's failure; a
// duplicate, two transforms of one stage, or a list out of stage order
// (RFC 5 §2.3); a transform, configured or registered, whose Stage is not one
// of StageCompress, StageEncrypt and StageRedundancy; and an encrypt stage
// beside a compress stage whose settings do not carry with_encryption: accept
// (RFC 5 §3.2). Until the chain is built it refuses every configuration with
// errors.ErrUnsupported.
func NewChain(ctx context.Context, cfg []transform.Config, m keys.Provider, deps Deps) (transform.Chain, error) {
	return nil, errors.ErrUnsupported
}

// Deps is what a chain takes from its caller, the node's content wiring,
// rather than from a global or a context (RFC 25 §8).
type Deps struct {
	// Logger receives a chain's build failures, at Error with the transform
	// and the reason (RFC 5 §6). A transform logs nothing per chunk.
	Logger *slog.Logger

	// Recorder resolves RFC 5 §6's per-transform series, which the chain
	// records around each transform's call.
	Recorder Recorder

	// Require is the store configuration's require (RFC 5 §3.2): registered
	// transforms' names that every body must carry. A body decoded without
	// one of them is transform.ErrMalformed (RFC 5 §2.5).
	Require []string

	// Encrypts reports whether namespace ns encrypts, the setting bound when
	// ns was created and never changed (RFC 5 §3.2). In a namespace that
	// encrypts, the encrypt stage is implicitly required, so a body without it
	// is transform.ErrMalformed even after the chain has lost its encrypt
	// stage. NewChain refuses a nil Encrypts, so the check never fails open.
	Encrypts func(ns entity.NamespaceID) bool
}

// Recorder resolves RFC 5 §6's series for one transform, labelled by its
// registered name. NewChain calls For once for every transform it builds, so a
// chunk's recording is an add on a series resolved at build, with no label
// lookup and no allocation (RFC 25 §2.9). The content wiring implements it over
// the node's metrics.
type Recorder interface {
	For(name string) TransformRecorder
}

// TransformRecorder records one transform's series, one call per moment the
// chain observes.
type TransformRecorder interface {
	// Encoded records one encode of one chunk: transform_chunks_total by
	// applied, and transform_bytes_total and transform_seconds with direction
	// encode.
	Encoded(applied bool, in, out int, took time.Duration)

	// Decoded records one successful decode of one body:
	// transform_bytes_total and transform_seconds with direction decode.
	Decoded(in, out int, took time.Duration)

	// DecodeFailed records one transform_decode_failures_total under reason's
	// error label.
	DecodeFailed(reason FailureReason)
}

// FailureReason is a decode failure's error label (RFC 5 §6). It is a small
// integer rather than the label itself, so a TransformRecorder resolves one
// series per reason in For and indexes it per call.
type FailureReason uint8

const (
	ReasonMalformed FailureReason = iota
	ReasonMaterialUnavailable
	ReasonMaterialUnknown
	ReasonMaterialDestroyed
	ReasonTooLarge
)

var reasonLabels = [...]string{
	ReasonMalformed:           "malformed",
	ReasonMaterialUnavailable: "material_unavailable",
	ReasonMaterialUnknown:     "material_unknown",
	ReasonMaterialDestroyed:   "material_destroyed",
	ReasonTooLarge:            "too_large",
}

// String is the reason's error label, as RFC 5 §6 spells it.
func (r FailureReason) String() string {
	if int(r) < len(reasonLabels) {
		return reasonLabels[r]
	}
	return "FailureReason(" + strconv.Itoa(int(r)) + ")"
}
