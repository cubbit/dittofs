# Block Store Migration Guide

DittoFS v1 releases wrote file content in three block layouts:

| Layout | Servers | On disk | In the block store |
|--------|---------|---------|--------------------|
| Path-indexed (`.blk`) | ≤ v0.15 | `{payloadID}/block-{idx}.blk` | per-block objects |
| Standalone CAS | v0.16 – v0.21 | per-chunk files `blocks/{hh}/{hh}/{hex}` | per-chunk objects `cas/{hh}/{hh}/{hex}` |
| Packed blocks | v0.22 and later v1 releases | `blobs/` + `logs/`, then journal segments | packed containers `blocks/<id>` |

## This build reads none of them

The v1 block tier is deleted: its remote stores, block codec, carver and
syncer, and every conversion between the layouts above. The engine keeps a
share's content in the journal, and no remote store is wired to it yet.

There is no in-place upgrade. To carry a share's content over, copy it out
through a protocol with the v1 release that wrote it, and copy it into a share
on this build. Objects a v1 release left in a bucket (`cas/`, `blocks/`) are
read by nothing here and stay billed until removed by hand.

A start that finds a v1 local layout, at a journal device or under
`blockstore.journal.path`, refuses rather than open it as empty.
