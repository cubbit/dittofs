package main

import (
	"github.com/spf13/cobra"
)

// socketFrom reads the root's persistent --socket flag.
func socketFrom(cmd *cobra.Command) string {
	socket, _ := cmd.Flags().GetString("socket")
	return socket
}

// tagFlag adds the required --tag every handle-level command carries: a
// one-op command has no current-share state, by design.
func tagFlag(cmd *cobra.Command) *uint32 {
	var tag uint32
	cmd.Flags().Uint32Var(&tag, "tag", 0, "the share's tag")
	_ = cmd.MarkFlagRequired("tag")
	return &tag
}

func newShareCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "share",
		Short: "Attach the share --tag through the journal's Share.",
	}
	tag := tagFlag(cmd)
	var limit int64
	var floor string
	cmd.Flags().Int64Var(&limit, "limit", 0, "record bytes the share may hold on disk; 0: none below the journal's max")
	cmd.Flags().StringVar(&floor, "floor", "0", "the share's version floor")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		v, err := parseVersion(floor)
		if err != nil {
			return err
		}
		c := Dial(socketFrom(cmd))
		if err := c.Do(cmd.Context(), "POST", "/share", map[string]any{
			"tag": *tag, "limit": limit, "floor": versionToJSON(v),
		}, nil); err != nil {
			return err
		}
		cmd.Printf("share %d attached\n", *tag)
		return nil
	}
	return cmd
}

func newForgetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "forget",
		Short: "Drop --tag whole through the journal's Forget.",
	}
	tag := tagFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c := Dial(socketFrom(cmd))
		if err := c.Do(cmd.Context(), "POST", "/forget", map[string]any{"tag": *tag}, nil); err != nil {
			return err
		}
		cmd.Printf("forgot tag %d\n", *tag)
		return nil
	}
	return cmd
}

func newStrandCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "strand",
		Short: "Refuse new work on --tag through the journal's Strand.",
	}
	tag := tagFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c := Dial(socketFrom(cmd))
		if err := c.Do(cmd.Context(), "POST", "/strand", map[string]any{"tag": *tag}, nil); err != nil {
			return err
		}
		cmd.Printf("stranded tag %d\n", *tag)
		return nil
	}
	return cmd
}

func newDrawCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "draw",
		Short: "Draw and print the next content version.",
		RunE: func(cmd *cobra.Command, args []string) error {
			c := Dial(socketFrom(cmd))
			var resp struct {
				Version versionJSON `json:"version"`
			}
			if err := c.Do(cmd.Context(), "POST", "/draw", nil, &resp); err != nil {
				return err
			}
			cmd.Printf("v%s\n", fmtVersion(versionFromJSON(resp.Version)))
			return nil
		},
	}
}

func newSetVersionLimitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set-version-limit VERSION",
		Short: "Raise the journal's version limit to VERSION.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := parseVersion(args[0])
			if err != nil {
				return err
			}
			c := Dial(socketFrom(cmd))
			if err := c.Do(cmd.Context(), "POST", "/set-version-limit",
				map[string]any{"version": versionToJSON(v)}, nil); err != nil {
				return err
			}
			cmd.Println("limit set")
			return nil
		},
	}
}
