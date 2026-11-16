package kvstore

import (
	"context"

	"github.com/cubbit/dittofs/pkg/metadata/kv"
)

// Nodes is the node's own view: the counters it reconciles through.
type Nodes struct{ s *Store }

func (s *Store) Nodes() Nodes { return Nodes{s} }

// Counters is each reconciled kind's counter: a record of the kind is
// applied on a node once the node has reconciled the kind through the
// record's position.
type Counters struct {
	Settings   uint64 // the settings generation
	ShareList  uint64 // the share list version
	Principals uint64 // the principals generation
	Claims     uint64 // the claim generation
}

// Counters reads every counter in one snapshot.
//
// ponytail: Claims is always zero, as no claim generation is kept until
// namespace claims are written. Read it here beside the others once one is.
func (n Nodes) Counters(ctx context.Context) (c Counters, err error) {
	err = n.s.kv.View(ctx, func(r kv.Reader) (err error) {
		if c.Settings, err = counterIn(r, cfgGenKey); err != nil {
			return err
		}
		if c.ShareList, err = counterIn(r, shareListKey); err != nil {
			return err
		}
		c.Principals, err = counterIn(r, pgenKey)
		return err
	})
	return c, err
}
