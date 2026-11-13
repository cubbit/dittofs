package main

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// hexdumpLimit is how much of a read the terminal shows without --full; a
// pane stays readable while the presenter can still prove the whole read.
const hexdumpLimit = 512

func newWriteAtCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "write-at FILE OFFSET",
		Short: "Append a write through Handle.WriteAt and print the version it issued.",
		Args:  cobra.ExactArgs(2),
	}
	tag := tagFlag(cmd)
	var str, hexp string
	var size int64
	var mtime int64
	cmd.Flags().StringVar(&str, "str", "", "write this ascii string")
	cmd.Flags().StringVar(&hexp, "hex", "", "write these hex bytes")
	cmd.Flags().Int64Var(&size, "size", 0, "write N bytes of a cycling pattern (big writes generated daemon-side)")
	cmd.Flags().Int64Var(&mtime, "mtime", 0, "the write's modification time, unix seconds")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := parseFileID(args[0])
		if err != nil {
			return err
		}
		off, err := parseOffset(args[1])
		if err != nil {
			return err
		}
		req := map[string]any{
			"tag": *tag, "file": args[0], "off": off, "mtime": mtime,
		}
		// Exactly one source of payload; Changed() is what says the
		// presenter set a flag, not its value.
		chosen := []string{}
		if cmd.Flags().Changed("str") {
			chosen = append(chosen, "--str")
			req["str"] = str
		}
		if cmd.Flags().Changed("hex") {
			chosen = append(chosen, "--hex")
			req["hex"] = hexp
		}
		if cmd.Flags().Changed("size") {
			chosen = append(chosen, "--size")
			req["size"] = size
		}
		if len(chosen) != 1 {
			return fmt.Errorf("write-at: exactly one of --str, --hex, --size names the payload, got {%s}",
				strings.Join(chosen, ", "))
		}
		var resp struct {
			Version versionJSON `json:"version"`
			Bytes   int64       `json:"bytes"`
		}
		c := Dial(socketFrom(cmd))
		if err := c.Do(cmd.Context(), "POST", "/write-at", req, &resp); err != nil {
			return err
		}
		cmd.Printf("wrote %d bytes at %d → v%s (id %s)\n",
			resp.Bytes, off, fmtVersion(versionFromJSON(resp.Version)), shortHex(id.Bytes()))
		return nil
	}
	return cmd
}

func newReadAtCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "read-at FILE OFFSET LENGTH",
		Short: "Read through Handle.ReadAt and hexdump what came back.",
		Args:  cobra.ExactArgs(3),
	}
	tag := tagFlag(cmd)
	var full bool
	cmd.Flags().BoolVar(&full, "full", false, "dump the whole buffer, not the first 512 bytes")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if _, err := parseFileID(args[0]); err != nil {
			return err
		}
		off, err := parseOffset(args[1])
		if err != nil {
			return err
		}
		length, err := strconv.ParseInt(args[2], 10, 64)
		if err != nil || length <= 0 {
			return fmt.Errorf("length %q, want a positive byte count", args[2])
		}
		var resp struct {
			N       int64        `json:"n"`
			Data    string       `json:"data"`
			Missing []extentJSON `json:"missing"`
			Held    []heldJSON   `json:"held"`
			AsOf    versionJSON  `json:"asOf"`
		}
		c := Dial(socketFrom(cmd))
		if err := c.Do(cmd.Context(), "POST", "/read-at", map[string]any{
			"tag": *tag, "file": args[0], "off": off, "len": length, "full": full,
		}, &resp); err != nil {
			return err
		}
		raw, _ := hex.DecodeString(resp.Data)
		limit := len(raw)
		if !full && limit > hexdumpLimit {
			limit = hexdumpLimit
		}
		cmd.Printf("n=%d asOf=v%s", resp.N, fmtVersion(versionFromJSON(resp.AsOf)))
		if resp.N != length {
			cmd.Printf(" (wanted %d)", length)
		}
		cmd.Println()
		dumper := hex.Dumper(cmd.OutOrStdout())
		if _, err := dumper.Write(raw[:limit]); err != nil {
			return err
		}
		dumper.Close()
		if !full && len(raw) > limit {
			cmd.Printf("(+%d more bytes, --full shows them)\n", len(raw)-limit)
		}
		for _, m := range resp.Missing {
			cmd.Printf("missing %d–%d\n", m.Off, m.Off+m.Len)
		}
		for _, h := range resp.Held {
			cmd.Printf("held %d–%d at v%s\n", h.Off, h.Off+h.Len, fmtVersion(versionFromJSON(h.Version)))
		}
		return nil
	}
	return cmd
}

func newSyncCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync [FILE...]",
		Short: "Make the named files' records durable through Handle.Sync; none names everything.",
	}
	tag := tagFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		files := make([]string, len(args))
		for i, a := range args {
			if _, err := parseFileID(a); err != nil {
				return err
			}
			files[i] = a
		}
		var resp struct {
			Synced int64 `json:"synced"`
		}
		c := Dial(socketFrom(cmd))
		if err := c.Do(cmd.Context(), "POST", "/sync", map[string]any{
			"tag": *tag, "files": files,
		}, &resp); err != nil {
			return err
		}
		if resp.Synced == 0 {
			cmd.Println("synced all")
		} else {
			cmd.Printf("synced %d files\n", resp.Synced)
		}
		return nil
	}
	return cmd
}

// shortHex renders the first 4 bytes of an id, for mentioning a file without
// filling a pane.
func shortHex(b [16]byte) string {
	return hex.EncodeToString(b[:4])
}
