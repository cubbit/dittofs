package protocol

import (
	"slices"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
)

// The programs NFS serves, MOUNT, NLM and NSM being NFS version 3's auxiliary
// programs (RFC 5531 Appendix A, RFC 21 sections 2.1 and 2.2).
const (
	progNFS   uint32 = 100003
	progMount uint32 = 100005
	progNLM   uint32 = 100021
	progNSM   uint32 = 100024

	versNFS3   uint32 = 3
	versNFS4   uint32 = 4
	versMount3 uint32 = 3
	versNLM4   uint32 = 4
	versNSM1   uint32 = 1
)

// nfsVersions are the versions a settings range names, in order: version 3,
// then version 4's minor versions (RFC 13's settings).
var nfsVersions = []string{"3", "4.0", "4.1", "4.2"}

// pair is one served program and version.
type pair struct {
	prog, vers uint32
}

// served is the set of program and version pairs one adapter serves, from its
// Versions range, each program's lowest and highest served version, which a
// PROG_MISMATCH names (RFC 5531 section 9's mismatch_info), and NFS version 4's
// served minor versions.
type served struct {
	pairs            []pair
	low, high        map[uint32]core.Version
	minorLo, minorHi core.Version // closed; served only where four is
	four             bool         // any minor version served
}

// newServed reads a Versions range into the served set: version 3 brings MOUNT,
// NLM and NSM with it, and version 4's versions are its minor versions.
func newServed(v settings.VersionRange) served {
	from, to := rank(v.Min), rank(v.Max)
	s := served{low: map[uint32]core.Version{}, high: map[uint32]core.Version{}}
	if from <= 0 && 0 <= to {
		s.add(progNFS, versNFS3)
		s.add(progMount, versMount3)
		s.add(progNLM, versNLM4)
		s.add(progNSM, versNSM1)
	}
	if to >= 1 && from <= to {
		s.four = true
		s.minorLo, s.minorHi = core.Version(max(from, 1)-1), core.Version(to-1)
		s.add(progNFS, versNFS4)
	}
	return s
}

// add records one served pair and its program's lowest and highest.
func (s *served) add(prog, vers uint32) {
	s.pairs = append(s.pairs, pair{prog, vers})
	v := core.Version(vers)
	if lo, ok := s.low[prog]; !ok || v < lo {
		s.low[prog] = v
	}
	if hi, ok := s.high[prog]; !ok || v > hi {
		s.high[prog] = v
	}
}

// rank is a version's place in nfsVersions, -1 for a name not there.
func rank(vers string) int { return slices.Index(nfsVersions, vers) }

// has reports whether prog has any version served.
func (s served) has(prog uint32) bool {
	_, ok := s.low[prog]
	return ok
}

// serves reports whether prog's version is served.
func (s served) serves(prog, vers uint32) bool {
	return slices.Contains(s.pairs, pair{prog, vers})
}

// minor reports whether NFS version 4's minor version is served.
func (s served) minor(v core.Version) bool {
	return s.four && s.minorLo <= v && v <= s.minorHi
}
