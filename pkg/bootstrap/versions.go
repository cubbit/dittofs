package bootstrap

import (
	"context"
	"fmt"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/settings"
)

// The series of a start's version gate, each labelled by format (RFC 13 §8).
var (
	activeVersion = metrics.Spec{
		Name:   "format_active_version",
		Help:   "The version the installation writes of each format nodes share.",
		Labels: []string{"format"},
	}
	supportedMin = metrics.Spec{
		Name:   "format_supported_min",
		Help:   "The lowest version of each shared format this node reads and writes.",
		Labels: []string{"format"},
	}
	supportedMax = metrics.Spec{
		Name:   "format_supported_max",
		Help:   "The highest version of each shared format this node reads and writes.",
		Labels: []string{"format"},
	}
)

// Register is a start's version gate (RFC 13 §5.5): it registers node, its
// ranges being this binary's for every shared format, in its node record,
// over the store the node opened. In the same transaction it refuses an
// active version of the installation outside them, or of a format they hold
// no range for, naming the format and both versions, and the record is then
// left as it was. It never raises a version. After a registration that
// passed, reg's gauges hold each format's active version and range.
func Register(
	ctx context.Context,
	store *kvstore.Store,
	node entity.Node,
	reg *metrics.Registry,
) error {
	return register(ctx, store, node, reg, settings.Versions())
}

// register is Register over table in place of this binary's ranges.
func register(
	ctx context.Context,
	store *kvstore.Store,
	node entity.Node,
	reg *metrics.Registry,
	table map[entity.Format]entity.VersionRange,
) error {
	node.Formats = table
	if _, err := store.Nodes().RegisterNode(ctx, node); err != nil {
		return fmt.Errorf("version gate: %w", err)
	}

	// The active versions are read again after the commit. Only a
	// raise can change them in between, and a raise is refused unless this
	// node's range, just registered, holds the new version.
	in, err := store.Installation(ctx)
	if err != nil {
		return err
	}

	lowest, highest := reg.Gauge(supportedMin), reg.Gauge(supportedMax)
	for f, r := range table {
		lowest.WithLabelValues(f.String()).Set(float64(r.Min))
		highest.WithLabelValues(f.String()).Set(float64(r.Max))
	}

	active := reg.Gauge(activeVersion)
	for f, v := range in.Active {
		active.WithLabelValues(f.String()).Set(float64(v))
	}

	active.WithLabelValues(entity.FormatStore.String()).Set(float64(store.Format()))

	return nil
}
