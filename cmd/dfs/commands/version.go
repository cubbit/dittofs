package commands

import (
	"runtime"

	"github.com/spf13/cobra"
)

func newVersionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Show version information",
		Long: `Display the DittoFS server build version and system information.

Shows the full semantic version, git commit, build date, Go toolchain version,
and OS/architecture. Use --short to emit only the version string for scripting.

Examples:
  # Show full version information
  dfs version

  # Print only the version number (useful in scripts)
  dfs version --short`,
		Run: func(cmd *cobra.Command, args []string) {
			if short, _ := cmd.Flags().GetBool("short"); short {
				cmd.Println(Version)
				return
			}

			cmd.Printf("%s %s\n", cmd.Root().Name(), Version)
			cmd.Printf("  Commit:     %s\n", Commit)
			cmd.Printf("  Built:      %s\n", Date)
			cmd.Printf("  Go version: %s\n", runtime.Version())
			cmd.Printf("  OS/Arch:    %s/%s\n", runtime.GOOS, runtime.GOARCH)
		},
	}
	cmd.Flags().Bool("short", false, "Show only version number")
	return cmd
}
