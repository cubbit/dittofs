package commands

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

func newVersionCmd() *cobra.Command {
	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Show version information",
		Long: `Display the dfsctl build version and system information.

Shows the full semantic version, git commit, build date, Go toolchain version,
and OS/architecture. Use --short to emit only the version string for scripting.

Examples:
  # Show full version information
  dfsctl version

  # Print only the version number (useful in scripts)
  dfsctl version --short`,
		Run: func(cmd *cobra.Command, args []string) {
			w := cmd.OutOrStdout()
			if short, _ := cmd.Flags().GetBool("short"); short {
				_, _ = fmt.Fprintln(w, Version)
				return
			}

			_, _ = fmt.Fprintf(w, "dfsctl %s\n", Version)
			_, _ = fmt.Fprintf(w, "  Commit:     %s\n", Commit)
			_, _ = fmt.Fprintf(w, "  Built:      %s\n", Date)
			_, _ = fmt.Fprintf(w, "  Go version: %s\n", runtime.Version())
			_, _ = fmt.Fprintf(w, "  OS/Arch:    %s/%s\n", runtime.GOOS, runtime.GOARCH)
		},
	}
	versionCmd.Flags().Bool("short", false, "Show only version number")
	return versionCmd
}
