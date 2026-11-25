# Troubleshooting DittoFS

This guide covers common issues and their solutions when working with DittoFS.

> **Not served on this branch.** This build serves no file protocol. The entries
> that mount a share or read an NFS or SMB error (connection, mount, permission,
> file handle, cross-protocol, NFSv4 session, Kerberos and ACL issues, and the
> `export not found` and `authentication failed` messages) describe the released
> server. The [server and API entries](#the-server-and-its-api) describe this one.

## Table of Contents

- [The server and its API](#the-server-and-its-api)
- [Connection Issues](#connection-issues)
- [Mount Issues](#mount-issues)
  - [A share cannot open: corrupt sealed journal segment](#a-share-cannot-open-corrupt-sealed-journal-segment)
  - [SMB mount permission denied (macOS)](#smb-mount-permission-denied-macos)
- [Permission Issues](#permission-issues)
- [File Handle Issues](#file-handle-issues)
- [Performance Issues](#performance-issues)
- [Cross-Protocol Issues](#cross-protocol-issues)
- [Logging and Debugging](#logging-and-debugging)

## The server and its API

### The server refuses to start

`dfs start` and `dfs init` print every refusal of the config file and the
environment together, each naming the key and the rule
([Configuration: Refusals](configuration.md#refusals)). Check a file without
starting anything:

```bash
dfs config validate --config /etc/dittofs/config.yaml
dfs config show --config /etc/dittofs/config.yaml   # each key and where its value came from
```

A key that is a setting, such as `adapters.nfs.port`, is refused in the file: set it
through the management API instead. A start over data a v1 release wrote refuses
too; see [Upgrading from v1](upgrading-from-v1.md).

### Is the server up?

`dfs start` runs in the foreground until it receives SIGINT or SIGTERM. The API
listener answers two probes without a token, each with one word:

```bash
curl http://127.0.0.1:8080/health/live    # live, or stalled
curl http://127.0.0.1:8080/health/ready   # ready, or starting, draining, not_ready
```

Signed in, `dfsctl status` reads the installation's health and its conditions
(`GET /api/v1/health`), and `dfsctl node list` and `dfsctl node get <id>` read each
node's status (`GET /api/v1/nodes`).

### `dfsctl login` refuses the server

`dfsctl` accepts an https URL, an http URL only on a loopback IP address, or the
absolute path of the node's local socket. `http://localhost:8080` is refused, as a
name is not checked as loopback: write `http://127.0.0.1:8080`. On the server's own
host the socket needs no TLS and no address:

```bash
dfsctl login --server /path/to/dfs.sock --username admin
```

The socket is `dfs.sock` in the node's runtime directory unless `api.socket` names
another path; `dfs config show` prints it. An API address that is not loopback is
served over TLS only: pass the CA that signed its certificate with `--cacert`.

### Every command answers 403 after sign-in

An account whose password must change, such as `admin` with the password
`dfs init` generated, can only change it and sign out. Run `dfsctl password change`,
then retry.

### The admin password is lost

Stop the node and run `dfs reset-admin` on the host that holds the metadata store,
with the same config file. It prints a new generated password once, which must be
changed at the first sign-in.

## Connection Issues

### Cannot mount: Connection refused

**Symptoms:**
```
mount.nfs: Connection refused
```

**Solutions:**

1. **Check if DittoFS is running:**
   ```bash
   ps aux | grep dfs
   ```

2. **Verify the port is correct:**
   ```bash
   netstat -an | grep 12049
   # or
   lsof -i :12049
   ```

3. **Check firewall rules:**
   ```bash
   # Linux
   sudo iptables -L | grep 12049

   # macOS
   sudo pfctl -s rules | grep 12049
   ```

4. **Verify configuration:**
   ```bash
   # Check the config file
   cat ~/.config/dittofs/config.yaml

   # Start with debug logging
   DITTOFS_LOGGING_LEVEL=debug ./dfs start
   ```

### Connection timeout

**Symptoms:**
```
mount.nfs: Connection timed out
```

**Solutions:**

1. **Check network connectivity:**
   ```bash
   ping localhost
   telnet localhost 12049
   ```

2. **Review the adapter's timeouts.** They are adapter settings, read with
   `dfsctl adapter get nfs`: `read_timeout`, `write_timeout`, `idle_timeout` and
   `handshake_timeout`.

## Mount Issues

### A share cannot open: corrupt sealed journal segment

An error beginning `journal: sealed segment ... is corrupt` means the journal
could not read all records from a segment that had already been committed. The
share refuses to open rather than silently treating missing records as empty
file ranges. The error identifies the segment and the offset where scanning
stopped; the damaged file is left untouched.

Stop the server and preserve the journal and metadata before attempting
recovery. Prefer restoring a consistent backup. If you explicitly accept the
risk of missing or stale file data, quarantine the named segment by renaming it
to the `.seg.quarantine` path shown in the error, then retry opening the share.
That suffix keeps it out of the journal scan while retaining its bytes for
offline recovery. Do not delete the damaged file.

Quarantine is not data recovery: the segment may contain the only copy of
unuploaded data, or deletion and truncation records whose absence can expose
older contents. Do not resume client access until the affected data has been
recovered or verified. An incomplete append in an active, unsealed segment is
different; the journal still discards its torn tail automatically.

### Invalid file system

**Symptoms:**
```
mount: /mnt/nfs: invalid file system.
```

**Cause:** The mount point directory does not exist.

**Solution:** Create the mount point before mounting:
```bash
# Linux
sudo mkdir -p /mnt/nfs
sudo mount -t nfs -o tcp,port=12049,mountport=12049 localhost:/export /mnt/nfs

# macOS (use /tmp since /mnt doesn't exist, sudo not required)
mkdir -p /tmp/nfs
mount -t nfs -o tcp,port=12049,mountport=12049 localhost:/export /tmp/nfs
```

### Permission denied when mounting

**Symptoms:**
```
mount.nfs: access denied by server while mounting
```

**Solutions:**

1. **On Linux, allow non-privileged ports:**
   ```bash
   sudo sysctl -w net.ipv4.ip_unprivileged_port_start=0
   ```

2. **On macOS, use resvport option:**
   ```bash
   sudo mount -t nfs -o tcp,port=12049,mountport=12049,resvport localhost:/export /mnt/test
   ```

3. **Check the export's client allowlist.** If the share has a netgroup attached,
   only matching client addresses may mount it:
   ```bash
   dfsctl share export-policy get export
   ```
   Clear the association to allow all clients:
   ```bash
   dfsctl share export-policy update export --client 0.0.0.0/0=read-write --client ::/0=read-write
   ```

4. **Verify the export's auth-flavor policy.** A share that requires Kerberos
   refuses an AUTH_SYS mount:
   ```bash
   dfsctl share export-policy get export
   ```
   For development over AUTH_SYS:
   ```bash
   dfsctl share export-policy update export --flavor sys --min-proof none --min-protection none
   ```

### No such file or directory

**Symptoms:**
```
mount.nfs: mounting localhost:/export failed, reason given by server: No such file or directory
```

**Solutions:**

1. **Verify the share exists:**
   ```bash
   dfsctl share list
   ```

2. **Check share names are correct:**
   ```bash
   # Mount using the exact share name from config
   sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049 localhost:/export /mnt/test
   ```

### SMB mount permission denied (macOS)

**Symptoms:**
```
zsh: permission denied: /tmp/smb-test/file.txt
```

This happens after mounting an SMB share, even with 0777 permissions.

**Cause:** macOS has a security restriction where **only the mount owner can access files**,
regardless of Unix permissions. This is enforced at a level below file permissions - no SMB
traffic even reaches the server. Apple confirmed this is "works as intended".

**Solution - use dfsctl (handles this automatically):**

`dfsctl share mount` runs `mount_smbfs` as you, never through `sudo`, so the mount
is yours. Run dfsctl as yourself and mount to a directory you own:
```bash
mkdir -p ~/mnt/share
dfsctl share mount --protocol smb export ~/mnt/share
```

**If using manual mount_smbfs:**
```bash
# Mount as your user, not root
sudo -u $USER mount_smbfs //user:pass@localhost:12445/export /mnt/share
```

**Note:** This is a macOS-specific issue. On Linux, `dfsctl share mount` runs `mount.cifs`
through `sudo` with your uid and gid, which work correctly.

See [Known Limitations](faq.md#macos-mount-owner-only-access) for details.

## Permission Issues

### Permission denied on file operations

**Symptoms:**
```
touch: cannot touch 'file.txt': Permission denied
```

**Solutions:**

1. **Check the share's squash mode.** If clients are being squashed to guest,
   every operation runs as the anonymous identity:
   ```bash
   dfsctl share export-policy get export
   ```
   For development, stop squashing root (or all clients):
   ```bash
   dfsctl share export-policy update export --squash none
   ```

2. **Verify the root directory's permissions on the backing store.** The export
   root is a real directory; check it directly:
   ```bash
   ls -ld /path/to/backing/store/export
   ```

3. **Check your client UID/GID:**
   ```bash
   id  # Check your UID and GID
   ```

4. **Enable debug logging to see auth context:**
   ```bash
   DITTOFS_LOGGING_LEVEL=debug ./dfs start
   # Look for lines showing UID/GID in requests
   ```

### Read-only filesystem

**Symptoms:**
```
touch: cannot touch 'file.txt': Read-only file system
```

**Solutions:**

1. **Check whether the share is read-only:**
   ```bash
   dfsctl share get export
   ```
   Clear it to allow writes:
   ```bash
   dfsctl share export-policy update export --read-only=false
   ```

2. **Verify mount options:**
   ```bash
   # Make sure you're not mounting with 'ro'
   sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049,rw localhost:/export /mnt/test
   ```

## File Handle Issues

### Stale file handle

**Symptoms:**
```
ls: cannot access 'file.txt': Stale file handle
```

**Causes:**
- File was deleted while client held a handle
- The node was pointed at a different metadata store, for example a new
  installation started in place of a v1 one ([Upgrading from v1](upgrading-from-v1.md))

**Solutions:**

1. **Unmount and remount the filesystem:**
   ```bash
   sudo umount /mnt/test
   sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049 localhost:/export /mnt/test
   ```

2. **For persistent handles, keep `metadata.backend: badger` with a `metadata.badger.path`
   on disk.** v2 has no per-share stores to choose; see
   [Choosing stores](choosing-stores.md).

3. **Clear client NFS cache (Linux):**
   ```bash
   # This varies by distribution
   sudo service nfs-common restart
   ```

## Performance Issues

### Slow read/write operations

**Diagnostics:**
```bash
# Check server logs for slow operations (the default log file)
tail -f ~/.local/state/dittofs/dittofs.log | grep -i "slow\|timeout"
```

### High memory usage

**Diagnostics:** read the metrics. The listener is on by default at
`metrics.listen`, `127.0.0.1:9090`:

```bash
curl http://127.0.0.1:9090/metrics
```

## Logging and Debugging

### Enable Debug Logging

**Via environment variable:**
```bash
DITTOFS_LOGGING_LEVEL=debug ./dfs start
```

**Via configuration:**
```yaml
logging:
  level: debug
  format: text  # or json
  output: stdout  # or file path
```

### Understanding Log Output

**Key log patterns:**

- `[INFO] NFS: Accepted connection from 127.0.0.1:54321` - Client connected
- `[DEBUG] NFS: LOOKUP(handle=..., name=file.txt)` - Operation details
- `[ERROR] NFS: Failed to read file: no such file` - Error conditions
- `[DEBUG] Auth: UID=1000, GID=1000, GIDs=[1000,4,20]` - Authentication context

### Capture Traffic

For deep debugging, capture NFS traffic:

```bash
# Linux
sudo tcpdump -i lo -w nfs.pcap port 12049

# macOS
sudo tcpdump -i lo0 -w nfs.pcap port 12049

# Analyze with Wireshark
wireshark nfs.pcap
```

### Check Server Health

```bash
# Check if the server is running
pgrep -a dfs

# Check its probes and metrics
curl http://127.0.0.1:8080/health/ready
curl http://127.0.0.1:9090/metrics

# Check configuration
dfs config show
```

## Cross-Protocol Issues

When running both NFS and SMB adapters simultaneously, the shared LockManager coordinates caching state between protocols. This section covers common cross-protocol issues and their resolution.

### File Locked by Another Protocol

**Symptoms:**
```
NFS: NFS4ERR_SHARE_DENIED or NFS4ERR_LOCKED
SMB: STATUS_SHARING_VIOLATION or STATUS_LOCK_NOT_GRANTED
```

**Cause:** An SMB client holds an exclusive lease (RWH) or byte-range lock on a file that an NFS client is trying to access, or vice versa.

`NFS4ERR_SHARE_DENIED` and `NFS4ERR_LOCKED` also arise without SMB in the picture: an NFSv4 OPEN carrying a deny mode excludes later OPENs of the same file, including the same open-owner's, and excludes READ and WRITE issued under the anonymous stateid. If no SMB adapter is running, look for the NFSv4 OPEN that established the deny mode rather than for a cross-protocol break.

**Diagnosis:**
```bash
# Check active locks and leases via debug logging
DITTOFS_LOGGING_LEVEL=debug ./dfs start
# Look for log entries containing:
#   "cross_protocol_break" - Break initiated across protocols
#   "lease_break" - SMB lease being broken
#   "delegation_recall" - NFS delegation being recalled
```

**Resolution:**
1. Wait for the lease/delegation break to complete (the LockManager automatically initiates breaks)
2. If the break times out (35s for SMB leases, 90s for NFS delegations), the server force-revokes and the operation proceeds
3. Check if the client holding the lock is still connected

### Delegation Recall Timeouts

**Symptoms:**
```
[WARN] delegation recall timeout: delegID=abc123 client=nfs-client elapsed=90s
```

**Cause:** An NFS client did not respond to a CB_RECALL (callback recall) within the configured timeout (default 90 seconds). This happens when:
- The NFS client is unresponsive or has network issues
- The NFS client's backchannel is broken
- The CB_RECALL message was lost

**Resolution:**
1. After timeout, the delegation is **force-revoked** and the conflicting operation proceeds
2. Check NFS client connectivity and backchannel health
3. Enable debug logging to see the recall flow:
   ```bash
   DITTOFS_LOGGING_LEVEL=debug ./dfs start 2>&1 | grep "delegation"
   ```

### Lease Break Storms

**Symptoms:**
```
[INFO] cross_protocol_break: file=/export/data.bin protocol=smb->nfs type=lease_break count=47 period=60s
```
Rapid grant-break-grant-break cycles visible in logs.

**Cause:** Two clients (one NFS, one SMB) are alternately opening the same file with conflicting access modes, causing the LockManager to repeatedly break and re-grant caching state.

**Resolution:**
1. DittoFS has a built-in **anti-storm cache** (default 30-second TTL) that prevents re-grants after a break
2. Consider configuring the conflicting clients to use the same protocol for the shared file
3. Read-only access from both protocols does not cause storms (NFS read delegation + SMB Read lease coexist)

### SMB Client Cannot Write to NFS-Delegated File

**Symptoms:**
- SMB WRITE or CREATE (write access) hangs for several seconds before succeeding
- Or fails with STATUS_SHARING_VIOLATION

**Cause:** An NFS client holds a write delegation on the file. The LockManager must recall the delegation via CB_RECALL and wait for the NFS client to return it before the SMB write can proceed.

**Expected behavior:** This is correct cross-protocol coordination. The delay is the delegation recall round-trip time (typically under 1 second for responsive NFS clients, up to 90 seconds for the timeout).

**Resolution:**
1. This is normal behavior -- the SMB write will succeed once the NFS delegation is returned
2. If the delay is unacceptable, disable delegations for the share (reduces NFS performance)
3. Monitor recall latency in debug logs

### NFS Client Sees Stale Data After SMB Write

**Symptoms:**
- NFS client reads file and gets old content after an SMB client has written new content

**Cause:** The NFS client cached the file data under a read delegation, and the delegation recall + cache invalidation has not completed yet.

**Resolution:**
1. DittoFS automatically sends CB_RECALL to NFS clients when an SMB client modifies a file
2. After the recall, the NFS client invalidates its cache and re-reads from the server
3. If the NFS client is not responding to recalls:
   - Check NFS client backchannel connectivity
   - After delegation recall timeout (90s), the delegation is force-revoked
   - NFS client's next request will fail with EXPIRED or ADMIN_REVOKED, forcing a refresh
4. Force a cache refresh on the NFS client:
   ```bash
   # Remount with noac to disable attribute caching
   sudo mount -t nfs4 -o noac server:/export /mnt/nfs
   ```

### Directory Listing Inconsistency Across Protocols

**Symptoms:**
- Creating a file via SMB, but NFS `ls` does not show it (or vice versa)

**Cause:** Directory change notifications have not yet broken the other protocol's directory lease/delegation.

**Resolution:**
1. DittoFS automatically breaks directory leases and delegations when `CreateFile`, `RemoveFile`, or `Rename` modifies a directory
2. The break is processed through the LockManager and dispatched to both protocols
3. If inconsistency persists:
   - Check for notification queue overflow (1024 events/directory capacity)
   - Force a directory re-read: `ls -la /mnt/nfs/dir/` or refresh the SMB directory listing

### Diagnostic Commands

```bash
# Enable cross-protocol debug logging
DITTOFS_LOGGING_LEVEL=debug ./dfs start

# Key log patterns to search for:
# "cross_protocol_break" - Break initiated between protocols
# "delegation_recall" - NFS delegation being recalled
# "lease_break" - SMB lease being broken
# "anti_storm" - Grant suppressed by anti-storm cache
# "dir_change_notify" - Directory change notification dispatched
# "break_timeout" - Break acknowledgment timed out
```

### Log Messages Reference

| Log Message | Level | Meaning |
| ------------- | ------- | --------- |
| `cross_protocol_break` | INFO | A caching break was initiated across protocols |
| `delegation_recall` | DEBUG | CB_RECALL sent to NFS client |
| `delegation_returned` | DEBUG | NFS client returned delegation voluntarily |
| `delegation_force_revoked` | WARN | Delegation revoked after timeout |
| `lease_break` | DEBUG | Lease break notification sent to SMB client |
| `lease_break_ack` | DEBUG | SMB client acknowledged lease break |
| `anti_storm_suppressed` | DEBUG | Grant suppressed by anti-storm cache |
| `dir_change_notify` | DEBUG | Directory change event queued |
| `notification_overflow` | WARN | Directory notification queue overflowed |

## Common Error Messages

### "export not found"

**Cause:** The path in the mount command doesn't match a share's NFS path.

**Solution:** List the shares and mount by the exact `--path` one was created with:
```bash
dfsctl share list
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049 localhost:/export /mnt/test
```

### "authentication failed"

**Cause:** Server requires authentication but client isn't providing it.

**Solution:** Allow the AUTH_SYS flavor on the export, or set the client up for
Kerberos:
```bash
dfsctl share export-policy update export --flavor sys --min-proof none --min-protection none
```

### NFSv4 Session Issues

#### Client cannot establish NFSv4 session

**Symptoms:**
```
mount.nfs4: Protocol not supported
```

**Solutions:**
1. Check that the client supports NFSv4. Some older clients default to NFSv3.
2. Try mounting with explicit version:
   ```bash
   sudo mount -t nfs4 localhost:/export /mnt/test
   ```

#### Session expired or lost

**Symptoms:**
```
NFS4ERR_EXPIRED or NFS4ERR_STALE_CLIENTID
```

**Solutions:**
1. The server may have restarted, causing session loss. Remount the share.
2. If the server is in its grace period, wait for it to complete before retrying.

### Kerberos Authentication Issues

#### Kerberos authentication fails

**Symptoms:**
```
mount.nfs4: access denied by server
```
with RPCSEC_GSS configured.

**Solutions:**
1. Verify the keytab file is accessible:
   ```bash
   klist -k /etc/krb5.keytab
   ```
2. Check that the service principal matches the server hostname
3. Verify clock synchronization between client and KDC (Kerberos requires clocks within 5 minutes)
4. Enable debug logging to see the RPCSEC_GSS negotiation:
   ```bash
   DITTOFS_LOGGING_LEVEL=debug ./dfs start
   ```

### ACL Issues

#### ACL operations fail with NFS3ERR_NOTSUPP

**Symptoms:**
```
setfacl: Operation not supported
```

**Solutions:**
1. ACLs require NFSv4. Ensure you are mounting with NFSv4:
   ```bash
   sudo mount -t nfs4 localhost:/export /mnt/test
   ```
2. NFSv3 does not support ACLs - only standard POSIX mode bits.

## Getting More Help

If you're still experiencing issues:

1. **Check existing issues:** [GitHub Issues](https://github.com/cubbit/dittofs/issues)
2. **Enable debug logging** and capture relevant output
3. **Open a new issue** with:
   - DittoFS version
   - Operating system and version
   - Configuration file (redact sensitive info), or `dfs config show`
   - Full error messages
   - Debug logs showing the problem
   - Steps to reproduce
