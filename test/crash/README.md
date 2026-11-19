# Crash testing (dm-flakey and cold reads)

Two rigs check that data lost underneath DittoFS comes back as an error, never as plausible zeros.
Neither is a registered suite. Both need root, Linux, `cifs-utils` and `python3`: a VM, not a
laptop.

## Device loss (dm-flakey)

`test/crash/device-loss.sh` answers one question: can an acknowledged write come back as the right
size with zero content? Losing an uncommitted write in a crash is expected; returning
plausible-looking zeros for it is not, because no reader can tell the difference.

**`kill -9` does not test this.** The page cache outlives the process, so the data is still there
when the server restarts. Only device-level loss exposes the gap between metadata that reached
stable storage and data that did not.

The rig puts the metadata store and the share's journal on an ext4 filesystem over `dm-flakey`,
writes self-identifying 4096-byte records over SMB (`cache=none`, so the server answers every
`write()`), then swaps the device table to `drop_writes` with `dmsetup suspend --noflush
--nolockfs`. From that instant only bytes that really reached the device survive. The writer is
stopped before the swap, so every record in its acknowledgement log was answered while the device
was healthy, and that log is fsynced to a different disk. After the swap the server is killed, the
device is restored to what survived, and the file is read back.

```bash
sudo ./test/crash/device-loss.sh ./dfs ./dfsctl [write-seconds] [commit-every]
```

`write-seconds` defaults to 15. `commit-every` is how often the writer calls `fsync`, and the two
modes test opposite directions:

- **`0`:** nothing is ever committed, so nothing may be published. The file must come back short
  or empty, never full-size and zeroed.
- **`64`** (the default): a durable prefix exists. Every fsynced record must read back byte for
  byte; only records written since the last fsync may be missing.

The verdict fails on any record that reads as zeros or garbage inside the file's own size, and on
any fsynced record that is missing. The machine needs the `dm-flakey` kernel module.

## Cold-read loss

`test/crash/invalidate-cold-loss.sh` checks the read path once a range lives only in the block
store. It writes records over SMB, waits until the syncer has mirrored them, reads one back, evicts
the journal's copy, then restarts a server whose block store is in memory, so the only copy is
gone. The second read must fail. Zeros, or any other wrong bytes, at full length are the bug; the
right bytes mean the copy was never the only one, and the run reports INCONCLUSIVE.

```bash
sudo ./test/crash/invalidate-cold-loss.sh ./dfs ./dfsctl
```
