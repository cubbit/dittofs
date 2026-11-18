package bootstrap

import (
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// NodeID reads every journal directory's node-id copy and compares them, with b's
// identity if it names one. Before the store opens; it writes nothing.
func NodeID(b *Bootstrap) (entity.NodeID, error) {
	return entity.NodeID{}, unfilled("NodeID")
}

// InitNodeID adopts or generates the identity and writes it into one journal
// directory, between that journal's Init and RecordJournal, for the initialisation
// step and a recovery import. Called once per directory.
func InitNodeID(b *Bootstrap, installation [16]byte, dir string) (entity.NodeID, error) {
	return entity.NodeID{}, unfilled("InitNodeID")
}

// WriteMissingNodeIDs writes a missing copy only beside a journal the store records
// for this node and device, logging each write.
func WriteMissingNodeIDs(
	b *Bootstrap,
	id entity.NodeID,
	recorded func(dir string) (bool, error),
) error {
	return unfilled("WriteMissingNodeIDs")
}

// unfilled is the error of an entry point whose body is not written yet.
func unfilled(name string) error {
	return fmt.Errorf(
		"bootstrap: %s is filled by configuration epic 2: %w",
		name,
		errors.ErrUnsupported,
	)
}
