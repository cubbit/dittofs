package commands

import (
	"context"
	"errors"

	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
)

// noFoundationDB refuses metadata.backend foundationdb in a build that leaves
// the FoundationDB backend out, naming the build that has it.
const noFoundationDB = `metadata.backend "foundationdb" is opened only by a build with the foundationdb tag, which links FoundationDB's client library; this build opens badger`

// checkKV reads the bootstrap's store settings — backend, store path and
// node identity — and refuses a configuration this build cannot serve:
// these are the start's own misconfigurations, answered in front of the
// start check, which would otherwise name a verdict over whatever storage
// a wrong configuration happened to point at.
func checkKV(cfg *bootstrap.Bootstrap) error {
	if cfg.Metadata.Backend == "foundationdb" {
		if !foundationDB {
			return errors.New(noFoundationDB)
		}
	} else if cfg.Metadata.Badger.Path == "" {
		// ponytail: RFC 13 Appendix C defaults metadata.badger.path to the
		// state directory's metadata, which the bootstrap loader does not
		// fill yet, so an unset path is refused until it does.
		return errors.New("metadata.badger.path is not set")
	}
	// ponytail: node.id must be written in the bootstrap, as nothing else
	// names the node yet. The node's identity read at start, from the
	// journal directories' node-id copies and node.id, replaces this check.
	if cfg.Node.ID == [16]byte{} {
		return errors.New("node.id is not set")
	}
	return nil
}

// openKV opens the metadata store's KV the bootstrap names, on the backend
// metadata.backend selects, the configuration already accepted by checkKV: a
// Badger directory after the start check, which answered every refusal the
// directory itself could take, and a FoundationDB cluster, which has no
// directory, before it, for the check to read. ctx bounds a FoundationDB
// open, which otherwise waits for an unreachable cluster forever.
func openKV(ctx context.Context, cfg *bootstrap.Bootstrap, clock systemClock) (kv.KV, error) {
	if cfg.Metadata.Backend == "foundationdb" {
		return openFoundationDB(ctx, cfg, clock)
	}
	db, err := badgerkv.Open(badgerkv.Options{Dir: cfg.Metadata.Badger.Path, NodeID: cfg.Node.ID, Clock: clock.Now})
	if err != nil {
		return nil, err
	}
	return db, nil
}
