package commands

// Exempt as dfs reset-admin's, so its import is not the rule's.
import "github.com/cubbit/dittofs/pkg/metadata/kvstore"

var _ = kvstore.Open
