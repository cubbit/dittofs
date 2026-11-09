package runtime

import (
	"testing"
)

// TestExportNoAuthFlavorCause_RemedyIsShellSafe pins the remedy against the
// share names that are actually allowed. Share-name validation is about the
// file-handle format — it refuses ':', an empty name and '.'/'..' — so a name
// may contain spaces, quotes and command substitutions. The remedy is printed
// for an operator to copy into a shell, so an unquoted name either silently
// becomes several arguments or runs whatever it substitutes.
func TestExportNoAuthFlavorCause_RemedyIsShellSafe(t *testing.T) {
	tests := []struct {
		name  string
		share string
		want  string
	}{
		{
			name:  "space splits the argument",
			share: "/team files",
			want:  "`dfsctl share nfs-config set '/team files' --allow-auth-sys true`",
		},
		{
			name:  "command substitution would run",
			share: "/$(id -u)",
			want:  "`dfsctl share nfs-config set '/$(id -u)' --allow-auth-sys true`",
		},
		{
			name:  "a quote closes, escapes and reopens",
			share: "/it's",
			want:  "`dfsctl share nfs-config set '/it'\\''s' --allow-auth-sys true`",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, remedy := ExportNoAuthFlavorCause(tt.share, false, false)
			if remedy != tt.want {
				t.Errorf("remedy = %s, want %s", remedy, tt.want)
			}
		})
	}
}
