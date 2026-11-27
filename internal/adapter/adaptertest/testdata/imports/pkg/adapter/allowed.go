// A seed of the adapters' import check: its imports are read, nothing here is built.
package adapter

import (
	_ "context"

	_ "github.com/cubbit/dittofs/pkg/adapter/settings"
	_ "github.com/cubbit/dittofs/pkg/metrics"
	_ "github.com/cubbit/dittofs/pkg/resources"
	_ "github.com/cubbit/dittofs/pkg/vfs"
	_ "go.opentelemetry.io/otel/trace"
)
