package vfs

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest/suite"
)

func TestConformance(t *testing.T) {
	suite.Run(t, func(t *testing.T) vfs.Service { return newFilesystem(t) })
}

// The fault wrapper with no fault armed is the filesystem it wraps, as far as
// the suite can tell.
func TestConformanceThroughFaults(t *testing.T) {
	suite.Run(t, func(t *testing.T) vfs.Service { return vfstest.Faults(newFilesystem(t)) })
}
