package bootstrap

import (
	"path/filepath"
	"reflect"
	"testing"
)

// The defaults are RFC 13 Appendix C's, and the directories the empty
// environment falls back to for the keys that follow them, since
// GetDefaultConfig carries no environment of its own.
func TestGetDefaultConfig(t *testing.T) {
	t.Parallel()

	dirs := lookupDirs(nil)

	var want Bootstrap

	want.Node.Roles = []string{"protocol", "storage"}
	want.Metadata.Backend = "badger"
	want.Metadata.Badger.Path = filepath.Join(dirs.state, "metadata")
	want.API.Listen = []string{"127.0.0.1:8080"}
	want.API.Socket = filepath.Join(dirs.runtime, "dfs.sock")
	want.Metrics.Listen = "127.0.0.1:9090"
	want.Logging = LoggingConfig{
		Level:  "info",
		Format: "text",
		Output: filepath.Join(dirs.state, "dittofs.log"),
		Rotation: LogRotationConfig{
			MaxSize:    100,
			MaxBackups: 5,
			MaxAge:     30,
		},
	}

	if got := GetDefaultConfig(); !reflect.DeepEqual(*got, want) {
		t.Errorf("GetDefaultConfig:\ngot  %+v\nwant %+v", *got, want)
	}
}
