// A seed of the adapters' import check: its imports are read, nothing here is built.
package nfs

import (
	_ "context"

	_ "github.com/cubbit/dittofs/internal/adapter/core"
	_ "github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	_ "github.com/cubbit/dittofs/internal/tlsconfig"
	_ "github.com/cubbit/dittofs/pkg/adapter"
	_ "github.com/cubbit/dittofs/pkg/adapter/settings"
	_ "github.com/cubbit/dittofs/pkg/metrics"
	_ "github.com/cubbit/dittofs/pkg/resources"
	_ "github.com/cubbit/dittofs/pkg/vfs"
	_ "github.com/cubbit/dittofs/pkg/vfs/vfserr"
	_ "go.opentelemetry.io/otel/trace"
)
