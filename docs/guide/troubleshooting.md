# Troubleshooting DittoFS

Use this page when `dfs` refuses to start, `dfsctl` refuses a command, or a client
cannot mount or write. Find the message you see in the table, then follow its entry.
Each entry shows the error, what it means, and what to do.

> **Two parts.** [The server and its API](#the-server-and-its-api) covers this
> version. Everything from [Connection issues](#connection-issues) down covers NFS
> and SMB clients, which this version does not serve yet: those entries describe the
> released server.

## Find your problem

| You see | Go to |
|---------|-------|
| `DITTOFS_ENCRYPTION_PASSPHRASE is unset` | [The passphrase is not set](#the-passphrase-is-not-set) |
| `node.id is not set` | [The config file is refused](#the-config-file-is-refused) |
| `unknown key`, `not an absolute path`, `no longer read` | [The config file is refused](#the-config-file-is-refused) |
| `refusing to start: ... is from v1` | [Data from the released server](#data-from-the-released-server) |
| `initialise the journal in ...: no such file or directory` | [`dfs init` stopped half way](#dfs-init-stopped-half-way) |
| `the installation is already initialised` | [The admin password is lost](#the-admin-password-is-lost) |
| `Cannot acquire directory lock` | [Another node holds the metadata store](#another-node-holds-the-metadata-store) |
| `503 starting: ... the node is starting` | [The node is still starting](#the-node-is-still-starting) |
| `is neither an https URL, an http URL on a loopback address, nor a socket's absolute path` | [`dfsctl login` refuses the server](#dfsctl-login-refuses-the-server) |
| `401 unauthenticated` | [`dfsctl login` refuses the server](#dfsctl-login-refuses-the-server) |
| `403 password_change_required` | [Every command answers 403](#every-command-answers-403-after-sign-in) |
| `not logged in - run 'dfsctl login' first` | [`dfsctl login` refuses the server](#dfsctl-login-refuses-the-server) |
| `400 unknown_key: ... refused by the settings table` | [A store, namespace or adapter command is refused](#a-store-namespace-or-adapter-command-is-refused) |
| `dfsctl user set-password`: `503 unavailable` | [Setting a password fails](#setting-a-password-fails) |
| `mount.nfs: Connection refused` | [Cannot mount: Connection refused](#cannot-mount-connection-refused) |
| `Permission denied`, `Read-only file system` | [Permission issues](#permission-issues) |
| `Stale file handle` | [File handle issues](#file-handle-issues) |
| `NFS4ERR_SHARE_DENIED`, `STATUS_SHARING_VIOLATION` | [Cross-protocol issues](#cross-protocol-issues) |

## The server and its API

### Where the log is

`dfs start` prints nothing to the terminal. It logs to `dittofs.log` in its state
directory, `~/.local/state/dittofs/` when you run it as yourself:

```bash
tail -f ~/.local/state/dittofs/dittofs.log
```

```text
[2026-11-25 10:32:30] [INFO] node: listening api=127.0.0.1:8080 metrics=127.0.0.1:9090
```

For more detail, start it with `DITTOFS_LOGGING_LEVEL=debug dfs start`. To log to
the terminal instead, set `logging.output: stderr` in the config file. Where the
state directory is under systemd or Docker is explained in
[Configuration](configuration.md#the-state-and-runtime-directories).

### The passphrase is not set

```console
$ dfs start
Error: building the node: node: the storage key: keyprovider: DITTOFS_ENCRYPTION_PASSPHRASE is unset
```

Every `dfs` command that opens the installation needs the passphrase you gave
`dfs init`. Export it in the same shell, then retry:

```bash
export DITTOFS_ENCRYPTION_PASSPHRASE='correct horse battery staple'
dfs start
```

A wrong passphrase cannot open the key file. There is no way to recover a lost
passphrase.

### The config file is refused

`dfs init`, `dfs start` and `dfs config validate` print every problem in the file at
once, one per line, each naming the key:

```console
$ dfs config validate
Error: /etc/dittofs/config.yaml: journals: data/j1 is not an absolute path
/etc/dittofs/config.yaml: loging.level: unknown key; did you mean logging.level?
```

Fix each line and run `dfs config validate` again. A clean `validate` does not yet
promise a clean start: a few problems only `dfs start` reports (see
[Not working as documented yet](configuration.md#not-working-as-documented-yet)). To see which value each key got
and where it came from (the file, a variable or the default):

```bash
dfs config show
```

Common causes:

- **`node.id is not set`**: add `node.id` with a UUID from `uuidgen`.
- **A setting in the file**, such as `adapters.nfs.port`: settings are changed
  through the management API, not the file.
- **`no longer read`**: the file is from the released server. Each line names the
  replacement. See [Upgrading from v1](upgrading-from-v1.md).
- **`keys.protocol.kind: required`** from `dfs config validate` only: `validate` asks
  for the protocol key, which `dfs init` and `dfs start` do not need yet. See
  [Adding the protocol key](configuration.md#adding-the-protocol-key).

How to read a refusal is explained in
[Configuration: Refusals](configuration.md#refusals).

### Data from the released server

```text
refusing to start: <found> at <path> is from v1; upgrading from v1 is not supported: start a new installation with dfs init on empty storage
```

`dfs init` or `dfs start` found data the released server (v1) wrote: its
control-plane database, its block layout, or its metadata store. Upgrading from v1 is
not supported. Point the config at new, empty directories and run `dfs init`. See [Upgrading from v1](upgrading-from-v1.md).

### `dfs init` stopped half way

```console
$ dfs init
created the storage key file /home/alice/dittofs-data/storage.key
Copy it off this host now and keep its passphrase: losing either loses the data, and nothing recovers it.
Error: install: initialise the journal in /home/alice/dittofs-data/journal: open /home/alice/dittofs-data/journal: no such file or directory
```

Every journal directory must exist before `dfs init`. Create it and run `dfs init`
again: it finishes what the first run started, and reuses the key file it already
wrote.

```bash
mkdir -p ~/dittofs-data/journal
dfs init
```

### Another node holds the metadata store

```console
$ dfs start
Error: opening the metadata store: Cannot acquire directory lock on "/home/alice/dittofs-data/metadata".  Another process is using this Badger database. err: resource temporarily unavailable
```

A node is already running over this metadata store, perhaps in another terminal or
under systemd. Use that node, or stop it first. `dfs reset-admin` refuses the same
way while a node runs.

### The node is still starting

```console
$ dfsctl login --server http://127.0.0.1:8080 --username admin
Error: dfsctl login: 503 starting: Service Unavailable: the node is starting
```

The node takes a few seconds to open its stores. Wait until the readiness check
answers `ready`, then retry.

### Is the server up?

Two health checks answer without signing in, each with one word:

```console
$ curl http://127.0.0.1:8080/health/live     # live, or stalled
live
$ curl http://127.0.0.1:8080/health/ready    # ready, or starting, draining, not_ready
ready
```

Signed in, `dfsctl status` shows the installation's health and anything wrong with
it, and `dfsctl node list` shows each node:

```console
$ dfsctl status
READY  CONDITION  SCOPE  SUBJECT  CAUSE  DETAIL
ready
```

### `dfsctl login` refuses the server

```console
$ dfsctl login --server http://localhost:8080 --username admin
Error: client: "http://localhost:8080" is neither an https URL, an http URL on a loopback address, nor a socket's absolute path
```

`dfsctl` sends a password over plain `http` only to a loopback IP address, and a
host name is never treated as one. Write `http://127.0.0.1:8080`. On the server's own
host you can use the local socket instead, which needs no address and no TLS:

```bash
dfsctl login --server ~/.local/state/dittofs/run/dfs.sock --username admin
```

That is the socket's path when `XDG_STATE_HOME` and `XDG_RUNTIME_DIR` are unset;
`dfs config show` prints the real one. For a node whose API is not on loopback,
use its `https` URL and pass the CA that signed its certificate with `--cacert`.

`401 unauthenticated` means the user name or password is wrong. `not logged in`
means the current context has no session: run `dfsctl login`.

### Every command answers 403 after sign-in

```console
$ dfsctl user list
Error: dfsctl user list: 403 password_change_required: Forbidden: change the account's password first
```

The account's password must be changed first, as with the `admin` password
`dfs init` generates. Until then the account can only change its password and sign
out:

```bash
dfsctl password change
```

### The admin password is lost

Stop the node, then run `dfs reset-admin` on the host that holds the metadata store,
with the same config file and passphrase. It prints a new password once, which must
be changed at the first sign-in:

```console
$ dfs reset-admin
The administrator "admin"'s generated password, shown once; it must be changed at the first sign-in:
HTHOnU2J24V2InufG7y7XmkM11H_vzyv
```

`dfs init` cannot do this: on an existing installation it refuses with
`the installation is already initialised`.

### A store, namespace or adapter command is refused

```console
$ dfsctl store create --setting remote.endpoint=https://s3.example.com
Error: dfsctl store create: 400 unknown_key: Bad Request: refused by the settings table (field remote.endpoint)
```

Not available yet on this version: the node does not accept settings for remote
stores, namespaces or the NFS and SMB adapters. So `dfsctl store create`,
`dfsctl namespace create` and `dfsctl adapter set` all fail this way, and no share can
be created on a new installation. There is no workaround. See
[Configuration: The management API](configuration.md#the-management-api).

### Setting a password fails

```console
$ dfsctl user set-password alice
new password:
new password again:
Error: dfsctl user set-password: user c763989b-76b9-4204-ac7e-a7e2a066d128: 503 unavailable: Service Unavailable
```

The node stores a user's NT hash, for SMB, under the protocol key, and the config file
has no `keys.protocol`. Add it as
[Adding the protocol key](configuration.md#adding-the-protocol-key) shows, restart the
node, and retry.

### Getting help

If you are still stuck:

1. Search the [GitHub issues](https://github.com/cubbit/dittofs/issues).
2. Start the node with `DITTOFS_LOGGING_LEVEL=debug` and reproduce the problem.
3. Open a new issue with the DittoFS version (`dfs version`), your operating system,
   the output of `dfs config show`, the full error, the relevant log lines and the
   steps to reproduce. `dfs config show` prints no secret values, but check the log
   lines before you paste them.

---

> **The sections below describe the released server.** This version serves no NFS or
> SMB yet.

## Connection Issues

### Cannot mount: Connection refused

```text
mount.nfs: Connection refused
```

1. Check that `dfs` is running: `pgrep -l dfs`.
2. Check that something listens on the NFS port:

   ```bash
   lsof -i :12049          # or: netstat -an | grep 12049
   ```

3. Check the firewall:

   ```bash
   sudo iptables -L | grep 12049       # Linux
   sudo pfctl -s rules | grep 12049    # macOS
   ```

4. Start the node with debug logging and watch the log:
   `DITTOFS_LOGGING_LEVEL=debug dfs start`.

### Connection timeout

```text
mount.nfs: Connection timed out
```

1. Check the network path: `nc -vz 192.0.2.10 12049`.
2. Check the adapter's timeouts: `read_timeout`, `write_timeout`, `idle_timeout`
   and `handshake_timeout` are adapter settings. On this version they cannot be read
   yet, so `dfsctl adapter get nfs` shows an empty `SETTINGS` column.

## Mount Issues

### Invalid file system

```text
mount: /mnt/nfs: invalid file system.
```

The mount point does not exist. Create it first:

```bash
sudo mkdir -p /mnt/nfs                  # Linux
mkdir -p /tmp/nfs                       # macOS: /mnt does not exist, use /tmp
```

### Permission denied when mounting

```text
mount.nfs: access denied by server while mounting
```

1. **macOS:** add the `resvport` option:

   ```bash
   sudo mount -t nfs -o tcp,port=12049,mountport=12049,resvport 127.0.0.1:/archive /tmp/archive
   ```

2. **Linux:** allow non-privileged source ports:

   ```bash
   sudo sysctl -w net.ipv4.ip_unprivileged_port_start=0
   ```

3. **Check the share's client rules.** Only matching client addresses may mount:

   ```bash
   dfsctl share export-policy get archive
   dfsctl share export-policy update archive --client 0.0.0.0/0=read-write --client ::/0=read-write
   ```

4. **Check the share's security flavours.** A share that requires Kerberos refuses an
   `AUTH_SYS` mount. For development over `AUTH_SYS`:

   ```bash
   dfsctl share export-policy update archive --flavor sys --min-proof none --min-protection none
   ```

   The same cause gives the message `authentication failed`.

### No such file or directory

```text
mount.nfs: mounting 127.0.0.1:/archive failed, reason given by server: No such file or directory
```

The path does not match a share. List the shares and mount by the exact `--path` the
share was created with:

```bash
dfsctl share list
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049 127.0.0.1:/archive /mnt/archive
```

The same cause gives the message `export not found`.

### SMB mount permission denied (macOS)

```text
zsh: permission denied: /tmp/smb-test/file.txt
```

macOS lets only the user who made an SMB mount reach its files, whatever the file
permissions, and the request never reaches the server. Apple considers this
intended. Mount as yourself, not with `sudo`. `dfsctl share mount` does this for you:

```bash
mkdir -p ~/mnt/archive
dfsctl share mount archive ~/mnt/archive --protocol smb
```

With `mount_smbfs` directly, run it as your user, also without `sudo`:

```bash
mount_smbfs //alice@127.0.0.1:12445/archive ~/mnt/archive
```

On Linux this does not happen: `dfsctl share mount` runs `mount.cifs` through `sudo`
with your uid and gid. See [the FAQ](faq.md#macos-mount-owner-only-access).

### A share cannot open: corrupt sealed journal segment

On the released server, an error beginning `journal: sealed segment ... is corrupt`
means the journal could not read all records from a segment that was already
committed. The share refuses to open rather than treat the missing records as
empty file ranges. The error names the segment and the offset where reading
stopped, and leaves the file untouched.

1. Stop the server, and copy the journal and the metadata aside before anything
   else.
2. Prefer restoring a consistent backup.
3. Only if you accept losing or seeing stale file data: rename the named segment to
   the `.seg.quarantine` path the error shows, then open the share again. The
   suffix keeps the segment out of the journal while keeping its bytes for offline
   recovery. Do not delete it.

Quarantine is not recovery. The segment may hold the only copy of data not yet
uploaded, or deletion and truncation records whose loss can expose older contents.
Do not let clients back in until the affected data is recovered or checked. A torn
write at the end of the active, unsealed segment is different: the journal discards
it by itself.

## Permission Issues

### Permission denied on file operations

```text
touch: cannot touch 'file.txt': Permission denied
```

1. **Squash mode.** A squashed client runs every operation as the anonymous user:

   ```bash
   dfsctl share export-policy get archive
   dfsctl share export-policy update archive --squash none   # development only
   ```

2. **Your user ID.** NFS sends your numeric user ID. Check it with `id -u`, and
   check that the DittoFS user you were granted access as has the same `--uid`
   (`dfsctl user list`).

3. **The grant.** A share admits nobody until a grant names them:
   `dfsctl grant list archive`.

4. **Debug logging.** `DITTOFS_LOGGING_LEVEL=debug dfs start` logs the user and group
   IDs of each request.

### Read-only filesystem

```text
touch: cannot touch 'file.txt': Read-only file system
```

1. Check whether the share is read-only, and clear it:

   ```bash
   dfsctl share get archive
   dfsctl share export-policy update archive --read-only=false
   ```

2. Check that you did not mount with `ro`.

## File Handle Issues

### Stale file handle

```text
ls: cannot access 'file.txt': Stale file handle
```

The file was deleted while the client still held it, or the node now runs over a
different metadata store, for example a new installation in place of a released
server's ([Upgrading from v1](upgrading-from-v1.md)). Unmount and mount again:

```bash
sudo umount /mnt/archive
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049 127.0.0.1:/archive /mnt/archive
```

File handles survive a restart as long as `metadata.badger.path` stays the same.

## Performance Issues

Watch the log for slow operations and timeouts:

```bash
tail -f ~/.local/state/dittofs/dittofs.log | grep -i "slow\|timeout"
```

For memory and other resource use, read the metrics, on by default at
`127.0.0.1:9090`:

```bash
curl http://127.0.0.1:9090/metrics
```

To capture NFS traffic for Wireshark:

```bash
sudo tcpdump -i lo -w nfs.pcap port 12049     # Linux
sudo tcpdump -i lo0 -w nfs.pcap port 12049    # macOS
```

## Cross-Protocol Issues

When a file is used over NFS and SMB at once, the server coordinates locks, SMB
leases and NFS delegations between the two protocols.

### File locked by another protocol

```text
NFS: NFS4ERR_SHARE_DENIED or NFS4ERR_LOCKED
SMB: STATUS_SHARING_VIOLATION or STATUS_LOCK_NOT_GRANTED
```

A client on one protocol holds an exclusive lease or a byte-range lock on a file
that a client on the other protocol wants.

NFS can cause this alone too: an NFSv4 OPEN with a deny mode excludes later OPENs of
the same file, the same client's included, and READ and WRITE sent without an open.
If no SMB client is involved, look for that NFSv4 OPEN.

What to do:

1. Wait. The server asks the holder to give the lease or delegation back.
2. If the holder does not answer in time, 35 seconds for an SMB lease and 90 seconds
   for an NFS delegation, the server revokes it and the operation goes ahead.
3. Check that the client holding the lock is still connected.

### Delegation recall timeouts

An NFS client did not answer a recall within 90 seconds, because it is unresponsive,
its callback connection is broken, or the recall was lost. The server then revokes
the delegation and the conflicting operation goes ahead. Check the client's network
and its callback connection.

### Lease break storms

Two clients, one on each protocol, keep opening the same file with conflicting
access, so the server keeps breaking and granting caching rights. After a break the
server stops granting again for 30 seconds. If it persists, have both clients use
the same protocol for that file. Read-only use from both sides does not cause this.

### SMB write waits on an NFS-delegated file

An SMB write or create hangs for a moment, or fails with `STATUS_SHARING_VIOLATION`.
An NFS client holds a write delegation, and the server must recall it first. This is
expected: the wait is the recall's round trip, usually under a second, at most 90
seconds.

### NFS client sees stale data after an SMB write

The NFS client cached the file under a read delegation that has not been recalled
yet. The server recalls it when SMB writes, and the client then re-reads. If the
client does not answer, the delegation is revoked after 90 seconds and the client's
next request fails with `EXPIRED` or `ADMIN_REVOKED`, which forces a refresh. To turn
off attribute caching on the client:

```bash
sudo mount -t nfs4 -o noac 192.0.2.10:/archive /mnt/archive
```

### A new file is missing from a directory listing

A file created over one protocol does not yet show in the other's listing, because
the other side's directory lease or delegation has not been broken yet. The server
breaks it when a file is created, removed or renamed. A directory's change queue
holds 1024 events. List the directory again to refresh it.

## NFSv4 Issues

### Client cannot establish an NFSv4 session

```text
mount.nfs4: Protocol not supported
```

Check that the client supports NFSv4; older clients default to NFSv3. Name the
version explicitly: `sudo mount -t nfs4 127.0.0.1:/archive /mnt/archive`.

### Session expired or lost

```text
NFS4ERR_EXPIRED or NFS4ERR_STALE_CLIENTID
```

The server restarted and the session was lost. Mount again. If the server is in its
grace period after a restart, wait for it to end first.

### Kerberos authentication fails

Not available yet on this version: Kerberos for NFS. On the released server:

1. Check the keytab: `klist -k /etc/krb5.keytab`.
2. Check that the service principal matches the server's host name.
3. Check that client and KDC clocks are within 5 minutes of each other.
4. Start the server with `DITTOFS_LOGGING_LEVEL=debug` to log the negotiation.

### ACL operations fail

```text
setfacl: Operation not supported
```

ACLs need NFSv4; NFSv3 has only mode bits. Mount with
`sudo mount -t nfs4 127.0.0.1:/archive /mnt/archive`.
