package protocol

import (
	"strconv"
	"strings"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
)

// pair is one served program and version.
type pair struct {
	prog, vers uint32
}

// served is the set of program and version pairs one connection serves, each
// with its table, from its Versions range and the tables it was given; each
// program's lowest and highest served version, which a PROG_MISMATCH names
// (RFC 5531 section 9's mismatch_info); and NFS version 4's served minor
// versions.
type served struct {
	tables           map[pair]*core.OpTable
	low, high        map[uint32]core.Version
	minorLo, minorHi core.Version // closed; served only where four is
	four             bool         // NFS version 4 served
}

// newServed reads a Versions range into the served set, each pair only where
// t holds its table: version 3 brings MOUNT, NLM and NSM with it, and version
// 4's versions are its minor versions. A range naming a version rank does not
// know serves nothing.
func newServed(v settings.VersionRange, t Tables) served {
	from, to := rank(v.Min), rank(v.Max)
	s := served{tables: map[pair]*core.OpTable{}, low: map[uint32]core.Version{}, high: map[uint32]core.Version{}}
	if from < 0 || to < 0 {
		return s
	}
	if from <= 0 && 0 <= to {
		s.add(t.V3, wire.ProgNFS, wire.VersNFS3)
		s.add(t.Mount, wire.ProgMount, wire.VersMount3)
		s.add(t.NLM, wire.ProgNLM, wire.VersNLM4)
		s.add(t.NSM, wire.ProgNSM, wire.VersNSM1)
	}
	if to >= 1 && from <= to && t.V4 != nil {
		s.four = true
		s.minorLo, s.minorHi = core.Version(max(from, 1)-1), core.Version(to-1)
		s.add(t.V4, wire.ProgNFS, wire.VersNFS4)
	}
	return s
}

// add records one served pair, its table and its program's lowest and
// highest, unless its table is nil.
func (s *served) add(t *core.OpTable, prog, vers uint32) {
	if t == nil {
		return
	}
	s.tables[pair{prog, vers}] = t
	v := core.Version(vers)
	if lo, ok := s.low[prog]; !ok || v < lo {
		s.low[prog] = v
	}
	if hi, ok := s.high[prog]; !ok || v > hi {
		s.high[prog] = v
	}
}

// rank is a version's place in the order a settings range names them: version
// 3 at 0, then version 4's minor versions, "4.N" at N+1; -1 for any other
// name. Reading the name keeps no copy of settings' list to drift from it.
func rank(vers string) int {
	if vers == "3" {
		return 0
	}
	minor, ok := strings.CutPrefix(vers, "4.")
	n, err := strconv.Atoi(minor)
	if !ok || err != nil || n < 0 || strconv.Itoa(n) != minor {
		return -1
	}
	return n + 1
}

// has reports whether prog has any version served.
func (s served) has(prog uint32) bool {
	_, ok := s.low[prog]
	return ok
}

// table is prog's version's table, nil where that version is not served.
func (s served) table(prog, vers uint32) *core.OpTable {
	return s.tables[pair{prog, vers}]
}

// minor reports whether NFS version 4's minor version is served.
func (s served) minor(v core.Version) bool {
	return s.four && s.minorLo <= v && v <= s.minorHi
}
