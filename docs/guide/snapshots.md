# Share Snapshots

There are no share snapshots to take, list or restore today.

v1's snapshots, a metadata dump plus a manifest of the content-addressed blocks
a share referenced, went with the v1 block and metadata stores, and so did
their restore, their safety snapshot, their scheduled policies and the CLI and
REST commands that drove them. No command or API route creates a snapshot.

## Configuration

A config file that still carries v1's `snapshot` block
(`snapshot.scheduler_poll_interval`, `snapshot.scheduler_disabled`,
`snapshot.restore_http_timeout`) is refused at load, naming each key as
unknown. The `snapshots` keys of the new design, such as
`snapshots.cut_deadline`, are refused in the file too, as settings to set
through the management API; nothing reads them yet.

## Related

- [RFC 12](../internals/rfcs/rfc-12-snapshots.md): the design of the snapshots to come.
- [Upgrading from v1](upgrading-from-v1.md).
