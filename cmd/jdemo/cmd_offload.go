package main

import (
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

func newOffloadCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "offload FILE THROUGH",
		Short: "Offload dirty extents through Handle.Offload; payloads land in the daemon's offload dir.",
		Args:  cobra.ExactArgs(2),
	}
	tag := tagFlag(cmd)
	var limit, widen int64
	cmd.Flags().Int64Var(&limit, "limit", 0, "the most bytes one offer may carry")
	cmd.Flags().Int64Var(&widen, "widen", 0, "bytes to widen each offer by, offloading neighbours")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		through, err := parseVersion(args[1])
		if err != nil {
			return err
		}
		var resp struct {
			Offloaded int    `json:"offloaded"`
			Dir       string `json:"dir"`
		}
		c := Dial(socketFrom(cmd))
		if err := c.Do(cmd.Context(), "POST", "/offload", map[string]any{
			"tag": *tag, "file": args[0], "through": versionToJSON(through),
			"limit": limit, "widen": widen,
		}, &resp); err != nil {
			return err
		}
		cmd.Printf("offloaded %d extents into %s\n", resp.Offloaded, resp.Dir)
		return nil
	}
	return cmd
}

// addExtentsFlag adds --extents OFF:LEN,... and returns a parser bound to
// the flag's value.
func addExtentsFlag(cmd *cobra.Command, name string) func() (string, error) {
	var v string
	cmd.Flags().StringVar(&v, name, "", "target extents, OFF:LEN,OFF:LEN,...")
	return func() (string, error) {
		if v == "" {
			return "", errRequired("--" + name)
		}
		return v, nil
	}
}

func newMarkOffloadedCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mark-offloaded FILE",
		Short: "Record extents offloaded elsewhere through Handle.MarkOffloaded.",
		Args:  cobra.ExactArgs(1),
	}
	tag := tagFlag(cmd)
	extentsFlag := addExtentsFlag(cmd, "extents")
	var oldest, newest string
	cmd.Flags().StringVar(&oldest, "oldest", "", "the oldest content version among the extents")
	cmd.Flags().StringVar(&newest, "newest", "", "the newest content version among the extents")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		for _, miss := range []struct{ flag, val string }{
			{"--oldest", oldest}, {"--newest", newest},
		} {
			if miss.val == "" {
				return errRequired(miss.flag)
			}
		}
		raw, err := extentsFlag()
		if err != nil {
			return err
		}
		exts, err := parseExtents(raw)
		if err != nil {
			return err
		}
		ov, err := parseVersion(oldest)
		if err != nil {
			return err
		}
		nv, err := parseVersion(newest)
		if err != nil {
			return err
		}
		wire := make([]extentJSON, len(exts))
		for i, e := range exts {
			wire[i] = extentJSON{Off: e.Off, Len: e.Len}
		}
		var resp struct {
			Marked int `json:"marked"`
		}
		c := Dial(socketFrom(cmd))
		if err := c.Do(cmd.Context(), "POST", "/mark-offloaded", map[string]any{
			"tag": *tag, "file": args[0], "extents": wire,
			"oldest": versionToJSON(ov), "newest": versionToJSON(nv),
		}, &resp); err != nil {
			return err
		}
		cmd.Printf("marked %d extents offloaded\n", resp.Marked)
		return nil
	}
	return cmd
}

func newUnmarkCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unmark FILE",
		Short: "Unmark extents through Handle.Unmark.",
		Args:  cobra.ExactArgs(1),
	}
	tag := tagFlag(cmd)
	extentsFlag := addExtentsFlag(cmd, "extents")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		raw, err := extentsFlag()
		if err != nil {
			return err
		}
		exts, err := parseExtents(raw)
		if err != nil {
			return err
		}
		wire := make([]extentJSON, len(exts))
		for i, e := range exts {
			wire[i] = extentJSON{Off: e.Off, Len: e.Len}
		}
		var resp struct {
			Unmarked int `json:"unmarked"`
		}
		c := Dial(socketFrom(cmd))
		if err := c.Do(cmd.Context(), "POST", "/unmark", map[string]any{
			"tag": *tag, "file": args[0], "extents": wire,
		}, &resp); err != nil {
			return err
		}
		cmd.Printf("unmarked %d extents\n", resp.Unmarked)
		return nil
	}
	return cmd
}

func newReleaseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "release FILE",
		Short: "Give up holding extents through Handle.Release.",
		Args:  cobra.ExactArgs(1),
	}
	tag := tagFlag(cmd)
	extentsFlag := addExtentsFlag(cmd, "extents")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		raw, err := extentsFlag()
		if err != nil {
			return err
		}
		exts, err := parseExtents(raw)
		if err != nil {
			return err
		}
		wire := make([]extentJSON, len(exts))
		for i, e := range exts {
			wire[i] = extentJSON{Off: e.Off, Len: e.Len}
		}
		var resp struct {
			Released int `json:"released"`
		}
		c := Dial(socketFrom(cmd))
		if err := c.Do(cmd.Context(), "POST", "/release", map[string]any{
			"tag": *tag, "file": args[0], "extents": wire,
		}, &resp); err != nil {
			return err
		}
		cmd.Printf("released %d extents\n", resp.Released)
		return nil
	}
	return cmd
}

func newRepackCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repack BUDGET",
		Short: "Repack segments within budget through Journal.Repack.",
		Args:  cobra.ExactArgs(1),
	}
	var tag uint32
	var segments string
	cmd.Flags().Uint32Var(&tag, "tag", 0, "the share to repack; 0: any")
	cmd.Flags().StringVar(&segments, "segments", "", "segment ids, comma-separated; empty: any")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		budget, err := parseInt64(args[0])
		if err != nil {
			return err
		}
		if budget <= 0 {
			return errRequired("a positive BUDGET")
		}
		var segs []uint64
		if segments != "" {
			for p := range strings.SplitSeq(segments, ",") {
				n, err := strconv.ParseUint(strings.TrimSpace(p), 10, 64)
				if err != nil {
					return err
				}
				segs = append(segs, n)
			}
		}
		var resp struct {
			Freed int64 `json:"freed"`
		}
		c := Dial(socketFrom(cmd))
		if err := c.Do(cmd.Context(), "POST", "/repack", map[string]any{
			"budget": budget, "tag": tag, "segments": segs,
		}, &resp); err != nil {
			return err
		}
		cmd.Printf("freed %d bytes\n", resp.Freed)
		return nil
	}
	return cmd
}
