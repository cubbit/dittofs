package bootstrap

import (
	"reflect"
	"testing"
)

// The defaults are RFC 13 Appendix C's, and the state directory's for the
// keys that follow it.
func TestGetDefaultConfig(t *testing.T) {
	t.Parallel()

	var want Bootstrap

	want.Node.Roles = []string{"protocol", "storage"}
	want.Metadata.Backend = "badger"
	want.API.Listen = []string{"127.0.0.1:8080"}
	want.Metrics.Listen = "127.0.0.1:9090"
	want.Logging = LoggingConfig{
		Level:  "info",
		Format: "text",
		Output: getDefaultLogPath(),
		Rotation: LogRotationConfig{
			MaxSize:    100,
			MaxBackups: 5,
			MaxAge:     30,
		},
	}
	want.Blockstore.Journal.Path = GetDefaultConfig().Blockstore.Journal.Path

	if got := GetDefaultConfig(); !reflect.DeepEqual(*got, want) {
		t.Errorf("GetDefaultConfig:\ngot  %+v\nwant %+v", *got, want)
	}
}
