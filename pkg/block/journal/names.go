package journal

import "github.com/cubbit/dittofs/pkg/block/journal/internal/format"

// The names of the node's identity copy and its temporary, which the node
// writes into a journal's directory (RFC 28 §2.1) and the journal's open leaves
// alone without reporting them.
const (
	NodeIDFileName = format.NodeIDFileName
	NodeIDTmpName  = format.NodeIDTmpName
)
