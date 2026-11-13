package main

import (
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/spf13/cobra"
)

// cGet is cPost's read-only sibling for the viewer routes.
func cGet(cmd *cobra.Command, route string, resp any) error {
	return Dial(socketFrom(cmd)).Do(cmd.Context(), "GET", route, nil, resp)
}

func newIndexCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "index",
		Short: "Show the placement index, one row per entry: kind, range, version, record, tag.",
	}
	var file string
	cmd.Flags().StringVar(&file, "file", "", "one file's id, 32 hex; empty: every file's")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if file != "" {
			if _, err := parseFileID(file); err != nil {
				return err
			}
		}
		var resp struct {
			Entries []indexEntryJSON `json:"entries"`
		}
		route := "/index"
		if file != "" {
			route += "?file=" + file
		}
		if err := cGet(cmd, route, &resp); err != nil {
			return err
		}
		renderIndexEntries(cmd.OutOrStdout(), indexEntriesFromWire(resp.Entries))
		return nil
	}
	return cmd
}

func indexEntriesFromWire(wire []indexEntryJSON) []journal.IndexEntry {
	out := make([]journal.IndexEntry, 0, len(wire))
	for _, e := range wire {
		out = append(out, journal.IndexEntry{
			Kind:      kindFromEntryName(e.Kind),
			Extent:    journal.Extent{Off: e.Off, Len: e.Len},
			Version:   versionFromJSON(e.Version),
			Tag:       journal.ShareTag(e.Tag),
			Location:  journal.RecordRef{Segment: journal.SegmentID(e.Segment), Offset: e.SegOff},
			Offloaded: e.Offloaded,
		})
	}
	return out
}

func kindFromEntryName(name string) journal.EntryKind {
	switch name {
	case "held":
		return journal.EntryHeld
	case "retained":
		return journal.EntryRetained
	case "marker":
		return journal.EntryMarker
	}
	return 0
}

func newSegmentsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "segments",
		Short: "Show every segment's accounting: id, stream, allocation, held, retained, state.",
		RunE: func(cmd *cobra.Command, args []string) error {
			var resp struct {
				Segments []segmentInfoJSON `json:"segments"`
			}
			if err := cGet(cmd, "/segments", &resp); err != nil {
				return err
			}
			out := make([]journal.SegmentInfo, 0, len(resp.Segments))
			for _, s := range resp.Segments {
				out = append(out, journal.SegmentInfo{
					ID:            journal.SegmentID(s.ID),
					Stream:        s.Stream,
					Sealed:        s.Sealed,
					Failed:        s.Failed,
					Damaged:       s.Damaged,
					Allocated:     s.Allocated,
					HeldBytes:     s.HeldBytes,
					RetainedBytes: s.Retained,
				})
			}
			renderSegments(cmd.OutOrStdout(), out)
			return nil
		},
	}
}

func newStatsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stats",
		Short: "Show the journal's totals and per-share breakdown, from Journal.Stats.",
		RunE: func(cmd *cobra.Command, args []string) error {
			var resp struct {
				Stats journal.Stats `json:"stats"`
			}
			if err := cGet(cmd, "/stats", &resp); err != nil {
				return err
			}
			renderStats(cmd.OutOrStdout(), resp.Stats)
			return nil
		},
	}
}

func newVerifyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "verify",
		Short: "Check the index against the segments and report; never repairs.",
		RunE: func(cmd *cobra.Command, args []string) error {
			var resp struct {
				Report verifyJSON `json:"report"`
			}
			if err := cGet(cmd, "/verify", &resp); err != nil {
				return err
			}
			renderVerify(cmd.OutOrStdout(), resp.Report)
			return nil
		},
	}
}
