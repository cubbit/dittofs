package main

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
)

// errRequired is a missing flag the operation cannot live without; the name
// is the flag as the presenter types it.
func errRequired(what string) error {
	return fmt.Errorf("%s is required", what)
}

// parseInt64 is the plain decimal parse behind every numeric positional.
func parseInt64(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q: %w", s, err)
	}
	return n, nil
}

// printVersion is the one-line answer of every op that draws a version.
func printVersion(cmd *cobra.Command, out string, v versionJSON) {
	cmd.Printf("%s v%s\n", out, fmtVersion(versionFromJSON(v)))
}

func newTruncateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "truncate FILE SIZE",
		Short: "Stop holding FILE at or beyond SIZE through Handle.Truncate.",
		Args:  cobra.ExactArgs(2),
	}
	tag := tagFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		size, err := parseInt64(args[1])
		if err != nil {
			return err
		}
		if size < 0 {
			return fmt.Errorf("truncate: size %d is negative", size)
		}
		var resp struct {
			Version versionJSON `json:"version"`
		}
		c := Dial(socketFrom(cmd))
		if err := c.Do(cmd.Context(), "POST", "/truncate", map[string]any{
			"tag": *tag, "file": args[0], "size": size,
		}, &resp); err != nil {
			return err
		}
		printVersion(cmd, "truncated to "+args[1]+":", resp.Version)
		return nil
	}
	return cmd
}

func newDeallocateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deallocate FILE OFFSET LENGTH",
		Short: "Stop holding FILE's extent through Handle.Deallocate.",
		Args:  cobra.ExactArgs(3),
	}
	tag := tagFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		off, err := parseOffset(args[1])
		if err != nil {
			return err
		}
		length, err := parseLen(args[2])
		if err != nil {
			return err
		}
		var resp struct {
			Version versionJSON `json:"version"`
		}
		c := Dial(socketFrom(cmd))
		if err := c.Do(cmd.Context(), "POST", "/deallocate", map[string]any{
			"tag": *tag, "file": args[0], "off": off, "len": length,
		}, &resp); err != nil {
			return err
		}
		printVersion(cmd, "deallocated "+args[1]+"–"+args[2]+":", resp.Version)
		return nil
	}
	return cmd
}

func newDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete FILE",
		Short: "Stop holding every extent of FILE through Handle.Delete.",
		Args:  cobra.ExactArgs(1),
	}
	tag := tagFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		var resp struct {
			Version versionJSON `json:"version"`
		}
		c := Dial(socketFrom(cmd))
		if err := c.Do(cmd.Context(), "POST", "/delete", map[string]any{
			"tag": *tag, "file": args[0],
		}, &resp); err != nil {
			return err
		}
		printVersion(cmd, "deleted:", resp.Version)
		return nil
	}
	return cmd
}

func newCloneTargetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clone-target FILE",
		Short: "Remove FILE's destination range and record the clone through Handle.CloneTarget.",
		Args:  cobra.ExactArgs(1),
	}
	tag := tagFlag(cmd)
	var srcShare, src, asof string
	var srcOff, dstOff, length int64
	cmd.Flags().StringVar(&srcShare, "src-share", "", "the source share's id, 32 hex")
	cmd.Flags().StringVar(&src, "src", "", "the source file's id, 32 hex")
	cmd.Flags().Int64Var(&srcOff, "src-off", 0, "the source range's offset")
	cmd.Flags().Int64Var(&dstOff, "dst-off", 0, "the destination range's offset")
	cmd.Flags().Int64Var(&length, "len", 0, "the cloned range's length")
	cmd.Flags().StringVar(&asof, "asof", "", "the existence commit's cut the clone reads")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		for _, miss := range []struct{ flag, val string }{
			{"--src-share", srcShare}, {"--src", src}, {"--asof", asof},
		} {
			if miss.val == "" {
				return errRequired(miss.flag)
			}
		}
		if _, err := parseShareID(srcShare); err != nil {
			return err
		}
		if _, err := parseFileID(src); err != nil {
			return err
		}
		av, err := parseVersion(asof)
		if err != nil {
			return fmt.Errorf("clone-target: --asof: %w", err)
		}
		if length <= 0 || srcOff < 0 || dstOff < 0 {
			return fmt.Errorf("clone-target: len %d, src-off %d, dst-off %d: want len > 0 and offsets ≥ 0",
				length, srcOff, dstOff)
		}
		var resp struct {
			Version versionJSON `json:"version"`
		}
		c := Dial(socketFrom(cmd))
		if err := c.Do(cmd.Context(), "POST", "/clone-target", map[string]any{
			"tag": *tag, "file": args[0], "srcShare": srcShare, "src": src,
			"srcOff": srcOff, "dstOff": dstOff, "len": length, "asOf": versionToJSON(av),
		}, &resp); err != nil {
			return err
		}
		printVersion(cmd, "clone target recorded:", resp.Version)
		return nil
	}
	return cmd
}

func newReserveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reserve BYTES WRITES",
		Short: "Take bytes for a known number of writes through Handle.Reserve.",
		Args:  cobra.ExactArgs(2),
	}
	tag := tagFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		bytes_, err := parseInt64(args[0])
		if err != nil {
			return err
		}
		if bytes_ < 0 {
			return fmt.Errorf("reserve: bytes %d is negative", bytes_)
		}
		writes, err := parseInt64(args[1])
		if err != nil {
			return err
		}
		if writes <= 0 {
			return fmt.Errorf("reserve: writes %d, want a positive count", writes)
		}
		var resp struct {
			Rid uint32 `json:"rid"`
		}
		c := Dial(socketFrom(cmd))
		if err := c.Do(cmd.Context(), "POST", "/reserve", map[string]any{
			"tag": *tag, "bytes": bytes_, "writes": writes,
		}, &resp); err != nil {
			return err
		}
		cmd.Printf("rid %d\n", resp.Rid)
		return nil
	}
	return cmd
}

func newReserveWriteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reserve-write FILE OFFSET",
		Short: "Write through an open reservation, exempt from the limits.",
		Args:  cobra.ExactArgs(2),
	}
	tag := tagFlag(cmd)
	var rid uint32
	var str, hexp string
	var mtime int64
	cmd.Flags().Uint32Var(&rid, "rid", 0, "the reservation to spend")
	cmd.Flags().StringVar(&str, "str", "", "write this ascii string")
	cmd.Flags().StringVar(&hexp, "hex", "", "write these hex bytes")
	cmd.Flags().Int64Var(&mtime, "mtime", 0, "the write's modification time, nanoseconds since the Unix epoch")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if rid == 0 {
			return errRequired("rid")
		}
		off, err := parseOffset(args[1])
		if err != nil {
			return err
		}
		req := map[string]any{
			"tag": *tag, "rid": rid, "file": args[0], "off": off, "mtime": mtime,
		}
		switch {
		case cmd.Flags().Changed("str"):
			req["str"] = str
		case cmd.Flags().Changed("hex"):
			req["hex"] = hexp
		default:
			return errRequired("--str or --hex")
		}
		var resp struct {
			Version versionJSON `json:"version"`
		}
		c := Dial(socketFrom(cmd))
		if err := c.Do(cmd.Context(), "POST", "/reserve-write", req, &resp); err != nil {
			return err
		}
		printVersion(cmd, "wrote from reservation:", resp.Version)
		return nil
	}
	return cmd
}

func newReserveCloseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reserve-close",
		Short: "Return a reservation's remainder to the journal.",
	}
	var rid uint32
	cmd.Flags().Uint32Var(&rid, "rid", 0, "the reservation to close")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if rid == 0 {
			return errRequired("rid")
		}
		c := Dial(socketFrom(cmd))
		if err := c.Do(cmd.Context(), "POST", "/reserve-close",
			map[string]any{"rid": rid}, nil); err != nil {
			return err
		}
		cmd.Printf("closed rid %d\n", rid)
		return nil
	}
	return cmd
}

func newFillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fill FILE OFFSET",
		Short: "Place remote-fetched bytes into missing extents through Handle.Fill.",
		Args:  cobra.ExactArgs(2),
	}
	tag := tagFlag(cmd)
	var asof, ver string
	var str, hexp string
	cmd.Flags().StringVar(&asof, "asof", "", "the read's change version the fill must not race")
	cmd.Flags().StringVar(&ver, "ver", "", "the content version of the ref the bytes were fetched from")
	cmd.Flags().StringVar(&str, "str", "", "the fetched bytes, ascii")
	cmd.Flags().StringVar(&hexp, "hex", "", "the fetched bytes, hex")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		for _, miss := range []struct{ flag, val string }{
			{"--asof", asof}, {"--ver", ver},
		} {
			if miss.val == "" {
				return errRequired(miss.flag)
			}
		}
		off, err := parseOffset(args[1])
		if err != nil {
			return err
		}
		av, err := parseVersion(asof)
		if err != nil {
			return fmt.Errorf("fill: --asof: %w", err)
		}
		vv, err := parseVersion(ver)
		if err != nil {
			return fmt.Errorf("fill: --ver: %w", err)
		}
		req := map[string]any{
			"tag": *tag, "file": args[0], "off": off,
			"asOf": versionToJSON(av),
			"ver":  versionToJSON(vv),
		}
		switch {
		case cmd.Flags().Changed("str"):
			req["str"] = str
		case cmd.Flags().Changed("hex"):
			req["hex"] = hexp
		default:
			return errRequired("--str or --hex")
		}
		var resp struct {
			Filled int `json:"filled"`
		}
		c := Dial(socketFrom(cmd))
		if err := c.Do(cmd.Context(), "POST", "/fill", req, &resp); err != nil {
			return err
		}
		cmd.Printf("filled %d bytes\n", resp.Filled)
		return nil
	}
	return cmd
}
