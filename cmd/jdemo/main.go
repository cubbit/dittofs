// Command jdemo drives the journal for a live demo: one daemon owns a
// journal directory, every other subcommand talks to it over a unix socket.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

func main() {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "jdemo",
		Short: "Drive the journal from its public API and view its state, for demos.",
	}
	root.PersistentFlags().String("socket", "jdemo.sock", "path of the daemon's unix socket")

	start := &cobra.Command{
		Use:   "start",
		Short: "Own the journal in --dir and serve the socket.",
		RunE:  runStart,
	}
	start.Flags().String("dir", "", "journal directory (initialised only when empty)")
	start.Flags().String("offload-dir", "", "directory the offload command writes payloads to (default: <dir>-offload)")
	_ = start.MarkFlagRequired("dir")
	root.AddCommand(start)
	for _, c := range []*cobra.Command{
		newShareCmd(), newForgetCmd(), newStrandCmd(),
		newDrawCmd(), newSetVersionLimitCmd(),
		newWriteAtCmd(), newReadAtCmd(), newSyncCmd(),
		newTruncateCmd(), newDeallocateCmd(), newDeleteCmd(), newCloneTargetCmd(),
		newReserveCmd(), newReserveWriteCmd(), newReserveCloseCmd(), newFillCmd(),
		newOffloadCmd(), newMarkOffloadedCmd(), newUnmarkCmd(), newReleaseCmd(),
		newRepackCmd(),
		newHoldCmd(), newUnholdCmd(), newHeldCmd(), newStampCmd(),
		newSinceCmd(), newSettleCmd(),
		newExtentsCmd(), newFilesCmd(), newDirtyFilesCmd(),
		newIndexCmd(), newSegmentsCmd(), newStatsCmd(), newVerifyCmd(),
	} {
		root.AddCommand(c)
	}
	return root
}

func runStart(cmd *cobra.Command, args []string) error {
	dir, _ := cmd.Flags().GetString("dir")
	socket, _ := cmd.Flags().GetString("socket")
	offload, _ := cmd.Flags().GetString("offload-dir")
	if offload == "" {
		offload = filepath.Join(filepath.Dir(dir), filepath.Base(filepath.Clean(dir))+"-offload")
	}

	srv := &Server{Dir: dir, SocketPath: socket, OffloadDir: offload}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := srv.Start(ctx); err != nil {
		return err
	}
	fmt.Printf("jdemo: serving %s for journal %s (offload: %s); ctrl-c stops\n", socket, dir, offload)

	<-ctx.Done()
	stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
