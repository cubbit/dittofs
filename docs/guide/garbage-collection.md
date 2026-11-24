# Garbage Collection

There is no garbage collector to run or configure today.

v1's collector, a mark-sweep over the remote block store's content-addressed
objects, went with the v1 block and metadata stores, and so did its one-time
reconcile, the per-share recycle bin and the `dittofs_gc_*` metrics. No remote block store is wired to the engine
yet, so there are no objects to sweep.

What frees space now is the engine itself: removing a file records its removal
in the metadata store, and the engine finishes it in the background, picking up
any removal left unfinished at the next start. Nothing about it is configurable.

## Configuration

A config file that still carries v1's `gc` block (`gc.auto_enabled`,
`gc.auto_interval`, `gc.grace_period`, `gc.dry_run_sample_size`,
`gc.compaction_live_ratio`) or its `integrity` block is refused at load, naming
each key as unknown. The `gc` keys of the new design, such as `gc.interval`, are
refused in the file too, as settings to set through the management API; nothing
reads them yet.

## Related

- [RFC 9](../internals/rfcs/rfc-9-gc.md): the design of the collector to come.
- [Upgrading from v1](upgrading-from-v1.md).
