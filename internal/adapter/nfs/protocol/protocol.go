package protocol

import (
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/resources"
)

// framer is RFC 5531 section 11's record marking: a record comes in fragments,
// each after a mark giving its length and whether it ends the record, at most
// RFC 21 section 2.3's 64 fragments and 1 MiB and 4 KiB together.
type framer struct{}

func (framer) HeaderLen() int                         { return wire.RecordMarkLen }
func (framer) BodyLen(mark []byte) (int, bool, error) { return wire.Fragment(mark) }
func (framer) MaxRecord() (bytes, fragments int)      { return wire.MaxRecord, wire.MaxFragments }
func (framer) Frame(dst []byte, body int) []byte      { return wire.AppendRecordMark(dst, body) }

// Protocol is ONC RPC over TCP for NFS and its auxiliary programs: the framer,
// each connection's header stage and router, and the tables they route to
// (RFC 20 sections 4.7 and 4.8).
type Protocol struct {
	tables Tables
	errs   *core.ErrorTable // the stages' error table, which every connection shares
	cache  *replayCache     // the one duplicate-request cache every connection shares
}

// New is the protocol over t, which holds every table its router may return;
// a connection serves each pair its settings' range names whose table t holds.
// Its duplicate-request cache answers from the reply budget b hands, pooled
// for the fresh copies a retransmission reads; with none the cache is not
// built and no reply is ever answered from another call's bytes.
func New(t Tables, b *resources.Budgets) *Protocol {
	p := &Protocol{tables: t, errs: newErrors()}
	if b != nil && b.Reply != nil {
		p.cache = newReplayCache(resources.NewBufPool(b.Reply))
	}

	return p
}

// Framer is the record marking every connection reads and writes.
func (p *Protocol) Framer() core.Framer { return framer{} }

// NewConn builds one connection's stages: its served set from the protocol's
// tables and the range of the settings it was accepted with (RFC 20 section
// 4.8), which are a settings.NFS, since nfs.New hands core one and Reconfigure
// refuses any other type. With a nil c, NewServer's check at configure, it
// reads no setting and serves nothing. The stages it does not build take
// core's defaults; the replay stage is built over the connection's own
// address and the shared cache, so one cache answers every connection.
func (p *Protocol) NewConn(c *core.Conn) core.Stages {
	s := &stage{errs: p.errs, conn: c}
	if c != nil {
		s.served = newServed(c.Settings().(settings.NFS).Versions, p.tables)
	}

	st := core.Stages{Header: s, Router: s, Errors: p.errs, Protocol: "nfs3"}
	if p.cache != nil {
		st.Replay = &replayStage{stage: s, cache: p.cache}
	}

	return st
}

// Tables is every non-nil table New was given.
func (p *Protocol) Tables() []*core.OpTable {
	var ts []*core.OpTable
	for _, t := range []*core.OpTable{p.tables.V3, p.tables.V4, p.tables.Mount, p.tables.NLM, p.tables.NSM} {
		if t != nil {
			ts = append(ts, t)
		}
	}
	return ts
}
