package api

// The API's allowed imports, and Badger, a store's implementation, which it may not import.
import (
	_ "github.com/cubbit/dittofs/pkg/metadata/errors"
	_ "github.com/cubbit/dittofs/pkg/vfs/vfserr"
	_ "github.com/dgraph-io/badger/v4"
	_ "github.com/prometheus/client_golang/prometheus"
	_ "go.opentelemetry.io/otel/trace"
)
