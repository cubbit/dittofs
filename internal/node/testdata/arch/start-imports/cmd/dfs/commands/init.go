package commands

// Exempt as dfs init's, so its import is not the rule's, but a call to its
// function from a held file is.
import "github.com/cubbit/dittofs/pkg/metadata/kvstore"

func initRecords() { _ = kvstore.Open }
