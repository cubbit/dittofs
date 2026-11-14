package main

import (
	"strings"

	"github.com/spf13/cobra"
)

func newHoldCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hold",
		Short: "Take a snapshot hold on a share through Handle.Hold.",
	}
	tag := tagFlag(cmd)
	var share, cut string
	var marks []string
	cmd.Flags().StringVar(&share, "share", "", "the share to hold, 32 hex")
	cmd.Flags().StringVar(&cut, "cut", "", "the snapshot's cut number")
	cmd.Flags().StringArrayVar(&marks, "mark", nil, "one file's version to protect: FILE:VERSION, repeatable")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if share == "" {
			return errRequired("--share")
		}
		if cut == "" {
			return errRequired("--cut")
		}
		wireMarks := map[string]string{}
		for _, m := range marks {
			f, v, ok := strings.Cut(m, ":")
			if !ok {
				return errBadMark(m)
			}
			if _, err := parseFileID(f); err != nil {
				return err
			}
			if _, err := parseVersion(v); err != nil {
				return errBadMark(m)
			}
			wireMarks[f] = v
		}
		cutNum, err := parseInt64(cut)
		if err != nil || cutNum < 0 {
			return errBadMark("cut " + cut)
		}
		if err := cPost(cmd, "/hold", map[string]any{
			"tag": *tag, "share": share, "cut": cutNum, "marks": wireMarks,
		}, nil); err != nil {
			return err
		}
		cmd.Printf("cut %s held\n", cut)
		return nil
	}
	return cmd
}

func errBadMark(m string) error {
	return parseErr("mark " + m + ": want FILE:VERSION")
}

func newUnholdCmd() *cobra.Command {
	return newShareCutCmd("unhold", "release a cut's hold",
		"/unhold", "released cut")
}

func newHeldCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "held",
		Short: "Print the bytes a share's holds keep, through Handle.Held.",
	}
	tag := tagFlag(cmd)
	var share string
	cmd.Flags().StringVar(&share, "share", "", "the share's id, 32 hex")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if share == "" {
			return errRequired("--share")
		}
		var resp struct {
			Held int64 `json:"held"`
		}
		if err := cPost(cmd, "/held", map[string]any{"tag": *tag, "share": share}, &resp); err != nil {
			return err
		}
		cmd.Printf("%d bytes held\n", resp.Held)
		return nil
	}
	return cmd
}

// newShareCutCmd builds the two share+cut commands that differ only in
// route and wording.
func newShareCutCmd(use, short, route, ack string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
	}
	tag := tagFlag(cmd)
	var share, cut string
	cmd.Flags().StringVar(&share, "share", "", "the share's id, 32 hex")
	cmd.Flags().StringVar(&cut, "cut", "", "the snapshot's cut number")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if share == "" {
			return errRequired("--share")
		}
		if cut == "" {
			return errRequired("--cut")
		}
		if _, err := parseShareID(share); err != nil {
			return err
		}
		cutNum, err := parseInt64(cut)
		if err != nil || cutNum < 0 {
			return parseErr("cut " + cut)
		}
		if err := cPost(cmd, route, map[string]any{
			"tag": *tag, "share": share, "cut": cutNum,
		}, nil); err != nil {
			return err
		}
		cmd.Printf("%s %s\n", ack, cut)
		return nil
	}
	return cmd
}

func newStampCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stamp FILE THROUGH",
		Short: "Record the cut an existence commit read, through Handle.Stamp.",
		Args:  cobra.ExactArgs(2),
	}
	tag := tagFlag(cmd)
	var cut string
	cmd.Flags().StringVar(&cut, "cut", "", "the cut the commit read")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if cut == "" {
			return errRequired("--cut")
		}
		through, err := parseVersion(args[1])
		if err != nil {
			return err
		}
		cutNum, err := parseInt64(cut)
		if err != nil || cutNum < 0 {
			return parseErr("cut " + cut)
		}
		if err := cPost(cmd, "/stamp", map[string]any{
			"tag": *tag, "file": args[0],
			"through": versionToJSON(through), "cut": cutNum,
		}, nil); err != nil {
			return err
		}
		cmd.Printf("stamped %s through v%s\n", args[0], fmtVersion(through))
		return nil
	}
	return cmd
}

func newSinceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "since FILE FROM",
		Short: "Report a file's changes strictly above a version, through Handle.Since.",
		Args:  cobra.ExactArgs(2),
	}
	tag := tagFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		from, err := parseVersion(args[1])
		if err != nil {
			return err
		}
		var resp struct {
			Changes []wireChange `json:"changes"`
		}
		if err := cPost(cmd, "/since", map[string]any{
			"tag": *tag, "file": args[0], "from": versionToJSON(from),
		}, &resp); err != nil {
			return err
		}
		if len(resp.Changes) == 0 {
			cmd.Println("no changes")
			return nil
		}
		for _, ch := range resp.Changes {
			cmd.Println(changeLine(ch.toChange()))
		}
		return nil
	}
	return cmd
}

func newSettleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "settle FILE VERSION",
		Short: "Record metadata's progress through Handle.Settle.",
		Args:  cobra.ExactArgs(2),
	}
	tag := tagFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		v, err := parseVersion(args[1])
		if err != nil {
			return err
		}
		if err := cPost(cmd, "/settle", map[string]any{
			"tag": *tag, "file": args[0], "version": versionToJSON(v),
		}, nil); err != nil {
			return err
		}
		cmd.Printf("settled through v%s\n", fmtVersion(v))
		return nil
	}
	return cmd
}

func newExtentsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "extents FILE",
		Short: "List the extents a file holds, through Handle.Extents.",
		Args:  cobra.ExactArgs(1),
	}
	tag := tagFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		var resp struct {
			Extents []extentJSON `json:"extents"`
		}
		if err := cPost(cmd, "/extents", map[string]any{
			"tag": *tag, "file": args[0],
		}, &resp); err != nil {
			return err
		}
		if len(resp.Extents) == 0 {
			cmd.Println("no extents")
			return nil
		}
		for _, e := range resp.Extents {
			cmd.Printf("%d–%d\n", e.Off, e.Off+e.Len)
		}
		return nil
	}
	return cmd
}

func newFilesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "files",
		Short: "List the share's files, through Handle.Files.",
	}
	tag := tagFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		var resp struct {
			Files []string `json:"files"`
		}
		if err := cPost(cmd, "/files", map[string]any{"tag": *tag}, &resp); err != nil {
			return err
		}
		if len(resp.Files) == 0 {
			cmd.Println("no files")
			return nil
		}
		for _, f := range resp.Files {
			cmd.Println(f)
		}
		return nil
	}
	return cmd
}

func newDirtyFilesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dirty-files",
		Short: "List files holding un-offloaded extents, with their oldest dirty byte's write time.",
	}
	tag := tagFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		var resp struct {
			Files []struct {
				File   string `json:"file"`
				Oldest int64  `json:"oldest"`
			} `json:"files"`
		}
		if err := cPost(cmd, "/dirty-files", map[string]any{"tag": *tag}, &resp); err != nil {
			return err
		}
		if len(resp.Files) == 0 {
			cmd.Println("no dirty files")
			return nil
		}
		for _, f := range resp.Files {
			cmd.Printf("%s oldest=%d\n", f.File, f.Oldest)
		}
		return nil
	}
	return cmd
}
