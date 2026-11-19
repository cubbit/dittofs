package commands

import (
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
)

// checkKV reads the bootstrap's store settings — backend, store path and
// node identity — and refuses a configuration this build cannot serve:
// these are the start's own misconfigurations, answered in front of the
// start check, which would otherwise name a verdict over whatever storage
// a wrong configuration happened to point at.
func checkKV(cfg *bootstrap.Bootstrap) error {
	if b := cfg.Metadata.Backend; b != "" && b != "badger" {
		return fmt.Errorf("metadata.backend %q is not one this build opens; it opens badger", b)
	}
	// ponytail: RFC 13 Appendix C defaults metadata.badger.path to the state
	// directory's metadata, which the bootstrap loader does not fill yet, so
	// an unset path is refused until it does.
	if cfg.Metadata.Badger.Path == "" {
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

// openKV opens the metadata store's KV the bootstrap names, its directory
// already accepted by the start check: every refusal that read only the
// configuration ran before the check, in checkKV, and every refusal the
// directory itself could take the check has already answered.
func openKV(cfg *bootstrap.Bootstrap, clock systemClock) (*badgerkv.Store, error) {
	return badgerkv.Open(badgerkv.Options{Dir: cfg.Metadata.Badger.Path, NodeID: cfg.Node.ID, Clock: clock.Now})
}
