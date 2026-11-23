# DittoFS Configuration Guide

DittoFS uses a flexible configuration system with support for YAML/TOML files and environment variable overrides.

> Unfamiliar with terms like CAS, AUTH_UNIX, NTLM, or root-squash? See the
> [Glossary](glossary.md) for plain-language definitions.

## Table of Contents

- [Configuration Files](#configuration-files)
- [Configuration Structure](#configuration-structure)
  - [Logging](#1-logging)
  - [Observability](#2-observability)
  - [Server Settings](#3-server-settings)
  - [Database (Control Plane)](#4-database-control-plane)
  - [API Server](#5-api-server)
  - [Block Store Configuration](#6-block-store-configuration)
  - [Metadata Configuration](#7-metadata-configuration)
  - [Shares (Exports)](#8-shares-exports)
  - [User Management](#9-user-management)
  - [Protocol Adapters](#10-protocol-adapters)
  - [Snapshot Scheduler](#14-snapshot-scheduler)
- [Metrics (Prometheus)](#metrics-prometheus)
- [Environment Variables](#environment-variables)
- [Configuration Precedence](#configuration-precedence)
- [Configuration Examples](#configuration-examples)
- [IDE Support with JSON Schema](#ide-support-with-json-schema)

## Configuration Files

### Default Location

The config file is resolved per platform:

| Platform | Default config file |
| --- | --- |
| Linux / macOS | `$XDG_CONFIG_HOME/dittofs/config.yaml` (typically `~/.config/dittofs/config.yaml`) |
| Windows | `%APPDATA%\dittofs\config.yaml` (typically `...\AppData\Roaming\dittofs\config.yaml`) |

Pass `--config <path>` to any `dfs` command to override the location.

### State Directory

Runtime state — the log file (`dittofs.log`) and PID file — lives in a separate state directory, also resolved per platform:

| Platform | Default state directory |
| --- | --- |
| Linux / macOS | `$XDG_STATE_HOME/dittofs` (typically `~/.local/state/dittofs`); falls back to the system temp directory if no home is resolvable |
| Windows | `%LOCALAPPDATA%\dittofs` |

### Initialization

Without a configuration file the server starts on the defaults, overridden by
`DITTOFS_*` variables. A file is written by hand, at the default path or the one
`--config` names; `dfs config validate` checks it.

### Supported Formats

YAML (`.yaml`, `.yml`) and TOML (`.toml`)

## Configuration Structure

DittoFS uses a flexible configuration approach with named, reusable stores. This allows different shares to use completely different backends, or multiple shares can efficiently share the same store instances.

### 1. Logging

Controls log output behavior:

```yaml
logging:
  level: "info"           # debug, info, warn, error (lowercase only)
  format: "text"          # text, json
  output: "stdout"        # stdout, stderr, or file path
```

**Log Formats:**

- **text**: Human-readable format with colored output (when terminal supports it)
  ```
  2024-01-15T10:30:45.123Z INFO  Starting DittoFS server component=server version=1.0.0
  ```

- **json**: Structured JSON format for log aggregation (Elasticsearch, Loki, etc.)
  ```json
  {"time":"2024-01-15T10:30:45.123Z","level":"INFO","msg":"Starting DittoFS server","component":"server","version":"1.0.0"}
  ```

### 2. Observability

DittoFS has **no in-file tracing configuration**. The one tracing switch is the
`DITTOFS_OTLP_ENDPOINT` environment variable, a development gate: while it is
set, `dfs start` exports the journals' spans over OTLP/HTTP to the endpoint it
names, as unauthenticated OTLP/HTTP (in clear for an `http` URL), so point it only at a collector you
trust; unset, the node runs untraced. See
[Viewing metrics and traces](../internals/debugging.md#viewing-metrics-and-traces)
for the development runbook, and [Metrics (Prometheus)](#metrics-prometheus)
below for the metrics listener's configuration.

### 3. Server Settings

There is no `shutdown_timeout`: a config file that carries it is refused at load,
naming it as an unknown key. How long an adapter waits for its requests to finish
when it stops is the `adapters.<protocol>.drain_timeout` setting, set through the
management API rather than in the file.

> The Prometheus `metrics:` block is a top-level key, documented in its own
> section below ([Metrics (Prometheus)](#metrics-prometheus)).

### 4. Database (Control Plane)

There is no separate control-plane database. Users, groups, shares and the
installation's other records live in the metadata store, which the `metadata`
block configures ([Metadata Configuration](#7-metadata-configuration)). A config
file that still carries a `database` block is refused at load, naming each of its
keys as unknown.

### 5. API Server

The REST API serves sign-in, the management resources and the health checks. It
is always on; the `api` block says where it listens:

```yaml
api:
  listen:                      # default 127.0.0.1:8080
    - 127.0.0.1:8080
  tls:                         # needed for any address but loopback
    cert_file: /etc/dittofs/tls/tls.crt
    key_file: /etc/dittofs/tls/tls.key
  socket: /run/dittofs/api.sock   # optional local socket, no TLS, mode 0600
```

| Key | Default | Description |
|-----|---------|-------------|
| `api.listen` | `127.0.0.1:8080` | The addresses the API listens on, each `ip:port` |
| `api.tls.cert_file` | (unset) | PEM certificate the API serves under. An address that is not loopback is served only over TLS, so without `cert_file` and `key_file` a start on such an address is refused |
| `api.tls.key_file` | (unset) | PEM private key for `cert_file` |
| `api.socket` | (unset) | A Unix socket where the API is also served, without TLS, to the server's own user only |

The metrics listener serves under the same certificate on an address that is not
loopback.

v1's `controlplane` block is gone, and a config file that still carries it is
refused at load, naming each of its keys as unknown. What replaced each part:

- `controlplane.host` and `controlplane.port` are `api.listen`.
- `controlplane.tls.*` is `api.tls.cert_file` and `api.tls.key_file`. There is no
  client CA (mutual TLS) and no minimum-version key.
- `controlplane.jwt.*` and `DITTOFS_CONTROLPLANE_SECRET` are gone: `dfs init`
  creates the signing key and seals it under `keys.storage`. Session and API token
  lifetimes are the `api.session_lifetime` (default 12h) and
  `api.token_max_lifetime` (default 365 days) settings, set through the
  management API; the file refuses them.
- `controlplane.require_initial_password_change` is gone: the administrator must
  change its password at the first sign-in only when `dfs init` generated it (see
  [Installation](install.md)).
- The timeouts, `drain_stall_timeout` and the `pprof` keys have no replacement.

**Health and API endpoints.** `/health/live` and `/health/ready` answer without a
token. Every resource is under `/api/v1/` (sign-in is `POST /api/v1/auth/sign-in`),
and `GET /api/version` names the release.

Related glossary terms: [TLS / mTLS](glossary.md#authentication).

### 6. Block Store Configuration

Every share references exactly **one block store** — the durable home for its
content. The CLI's share commands are
unavailable until they are rebuilt on the v2 management API, so no command
attaches a block store to a share today. Today's CLI has no command to create a block store: remote stores
return as resource commands of the v2 management API.

> **No protocol serves files yet.** The NFS and SMB adapters have no file
> operations yet; see [Data path](../internals/architecture.md#data-path).

#### State root (`blockstore.journal.path`)

The key is still parsed and must be an absolute path, but shares keep no state
directory under it. The node reads it only at start, to detect a v1 state layout
(a `shares/<name>/` directory holding a populated `journal`, `blobs` or `logs`
directory) and refuse to start over one. The journal devices come from `journals`:

```yaml
blockstore:
  journal:
    path: /var/lib/dittofs/blocks   # default <state dir>/blocks
```

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `path` | string | `<state dir>/blocks` | Root probed at start for a v1 state layout. Must be absolute (a leading `~` is expanded). |

Env-var mapping follows the dot-path convention:
`DITTOFS_BLOCKSTORE_JOURNAL_PATH`.

Point this root — and `metadata.badger.path`, and every `journals` device — at a
**directory on the volume, never the volume's mount point**: a mount point holds
its filesystem's own files (`lost+found` on a fresh ext4 volume among them), and
a store directory holding anything but a store **refuses the start** rather than
initialise over files the node does not own.

A config file still carrying the old `blockstore.local.*` keys is **refused at
startup**, naming both spellings — see [Upgrading](#upgrading-from-the-localremote-block-store-split).
The former journal tuning keys (`chunk_size`, `chunk_max`, `dirty_expire`,
`max_log_bytes`, `backpressure_max_wait`) are no longer read; a config file
carrying them is refused at load, naming each one as an unknown key.
There is no `syncer:` config block: a config file that still has one is refused
at load, naming each `syncer.*` key.

#### Garbage collection and the integrity scan

Neither runs, and a config file carrying a `gc` or `integrity` key is refused at
load: see [Garbage Collection](garbage-collection.md).

#### Recycle bin (trash)

There is none. v1's per-share recycle bin (`#recycle` and the share's `trash_*`
fields) went with the v1 metadata store, and nothing replaces it yet.

#### Remote stores

The v1 remote block tier is deleted: its `s3` and `memory` stores, and the
`compression` and `encryption` settings in a remote store's `config` JSON, are
read by nothing.

#### Upgrading from the local/remote block store split

A config file that still names a key under `blockstore.local` is refused at
load, naming the key under `blockstore.journal` that replaced it:

```
config.yaml: blockstore.local.path: no longer read; write blockstore.journal.path
```

`path` is the only key left under `blockstore.journal`. Any other key the
refusal names, such as `blockstore.journal.max_log_bytes`, is refused in turn as
unknown, so delete it rather than rename it.

The migrations earlier releases ran for the split are gone. A v1 installation is not upgraded at all: see
[Upgrading from v1](upgrading-from-v1.md).

### 7. Metadata Configuration

The metadata store holds the namespace (files, directories and their
attributes) and every control-plane record: users, groups, shares and the
installation itself. One store serves the whole node, so a share does not name
one. Four keys, those of RFC 13's Appendix C, say which store and where:

```yaml
metadata:
  backend: badger                        # or foundationdb
  badger:
    path: /var/lib/dittofs/metadata
  # foundationdb:                        # read only with backend: foundationdb
  #   cluster_file: /etc/foundationdb/fdb.cluster
  #   tls:                               # all three, or none
  #     cert_file: /etc/dittofs/fdb/client.pem
  #     key_file: /etc/dittofs/fdb/client.key
  #     ca_file: /etc/dittofs/fdb/ca.pem
```

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `metadata.backend` | `badger` or `foundationdb` | `badger` | Which store. Only the block it names is read: a key of the other block is refused at load. |
| `metadata.badger.path` | absolute path | none (see below) | Badger's directory. |
| `metadata.foundationdb.cluster_file` | absolute path | required with `backend: foundationdb` | FoundationDB's cluster file. |
| `metadata.foundationdb.tls.cert_file`, `.key_file`, `.ca_file` | absolute paths | none | The client certificate, its key and the CA file. Name the three together or none. |

Each key's environment name is `DITTOFS_` and the key in upper case, its dots
as underscores: `DITTOFS_METADATA_BACKEND`, `DITTOFS_METADATA_BADGER_PATH`,
`DITTOFS_METADATA_FOUNDATIONDB_CLUSTER_FILE`, and so on.

**Only a build with the `foundationdb` tag opens FoundationDB.** The loader and
`dfs config validate` accept `backend: foundationdb` and its keys in every build, but in
one without the tag `dfs init` and `dfs start` refuse it before opening anything:

```
metadata.backend "foundationdb" is not one this build opens; it opens badger
```

**Set `metadata.badger.path`.** RFC 13 defaults it to `metadata` under the
state directory, but the loader does not fill that default yet, so `dfs init`
and `dfs start` refuse a path that is not set (`metadata.badger.path is not
set`). Like the journal devices, it belongs in a directory on a volume, never at
the volume's mount point (see [State root](#state-root-blockstorejournalpath)).

**`dfs init` creates the store; `dfs start` only opens one.** A start over a
directory holding no store refuses and names `dfs init`. A start over a
directory holding something that is not a store of this installation, a v1
metadata store among them, refuses too: see [Upgrading from v1](upgrading-from-v1.md).

**v1's metadata settings are gone.** No key sizes Badger's caches: a file still
setting `metadata.badger.block_cache_mb` or `metadata.badger.index_cache_mb` is
refused at load as an unknown key. The per-store settings v1 kept with each
metadata store (`in_memory`, `relaxed_durability` and the per-store cache sizes)
have no equivalent: there is one on-disk store per node and nothing to create
from the CLI.

### 8. Shares (Exports)

Shares are persisted in the metadata store. Like the block-store commands in
[§6](#6-block-store-configuration), the CLI's share commands (`dfsctl share …`
and `dfsctl share permission …`) are unavailable until they are rebuilt on the
v2 management API; each refuses with `not available until the CLI is rebuilt on
the v2 API`.

#### Per-share and per-identity quotas

DittoFS supports two complementary quota layers, both enforced by NFS *and* SMB:

1. **Per-share quota** (`dfsctl share create/edit --quota-bytes`) — a single byte
   ceiling for the whole share. Exceeding it returns `NFS3ERR_NOSPC` /
   `STATUS_DISK_FULL`.

2. **Per-identity quotas** (`dfsctl quota …`) — per-**user** (uid) and per-**group**
   (gid) limits, plus an optional **default-user** fallback applied to any user
   without an explicit quota. Each quota bounds both **bytes** and **inodes**
   (file count) and supports a **soft** threshold with a **grace period** before
   the soft threshold is enforced as a hard limit. Usage is charged to the file
   *owner* (standard quota semantics). Exceeding a hard limit (or an expired
   soft+grace) returns `NFS3ERR_DQUOT` / `NFS4ERR_DQUOT` /
   `STATUS_QUOTA_EXCEEDED`. `df` / FSSTAT and SMB `FS_FULL_SIZE_INFORMATION`
   report the smallest applicable quota for the calling identity.

```bash
# Per-user quota: uid 1000 limited to 10 GiB / 100k files, soft at 8 GiB,
# 7-day grace (604800s) before the soft byte threshold becomes hard.
./dfsctl quota set /cloud --scope user --id 1000 \
    --limit-bytes 10GiB --soft-bytes 8GiB \
    --limit-files 100000 --soft-files 90000 --grace-seconds 604800

# Per-group quota: gid 2000 limited to 50 GiB.
./dfsctl quota set /cloud --scope group --id 2000 --limit-bytes 50GiB

# Default-user fallback (applies to any user without an explicit quota).
./dfsctl quota set /cloud --scope default-user --limit-bytes 5GiB

# Inspect and remove.
./dfsctl quota list /cloud
./dfsctl quota remove /cloud --scope user --id 1000
```

Per-identity quota usage is tracked incrementally by every metadata backend
(badger, on disk or in memory), keyed by owner uid and gid, and is reconstructed
from the file rows on startup. A `chown` that changes a file's owner moves its
bytes and inode count between identities. Limits live in the control-plane DB
and are also manageable via the REST API
(`/api/v1/shares/{name}/quotas[/{scope}/{id}]`).

The **soft → grace → hard** state machine records when an identity first crosses
its soft threshold and enforces the soft limit as hard once the grace window
elapses. For an explicit user/group quota the grace timer lives on the quota
row. For the **default-user** fallback the timer is inherently per-user (each
user trips soft at a different time, and the single shared template row cannot
hold per-user state), so it is recorded in a small side table keyed by
`(share, uid)`, written the first time a default-user breaches soft and reaped
when usage drops back under soft. This makes default-user grace **durable across
a server restart** — a restart no longer hands every over-soft default user a
fresh grace window.

> **Note**: enforcement is best-effort (matching the per-share soft quota):
> under high write concurrency a few operations may briefly exceed a limit until
> usage catches up. This is standard for userspace NFS/SMB servers.
>
> **Kubernetes operator**: the operator manages infrastructure only — there is
> no Share/Quota CRD. Quotas are managed via the REST API / `dfsctl` as above.

### 9. User Management

DittoFS supports a unified user management system for both NFS and SMB protocols. Users, groups, and their permissions are stored in the metadata store (see [Metadata Configuration](#7-metadata-configuration)) and can be managed via:

1. **CLI commands** (`dfsctl user`, `dfsctl group`) - Recommended for initial setup
2. **REST API** - For programmatic management and integrations

Permission resolution follows a priority order: user explicit permissions > group permissions (highest wins) > share default.

The server config bootstraps only the administrator through `admin` settings;
it does not import top-level user or group lists. Create other accounts and
memberships through the CLI or REST API.

#### Users

Create named users through the CLI; the password is prompted interactively:

```bash
dfsctl group create --name editors --gid 101
dfsctl user create --username editor --uid 1001 --gid 101 --groups editors
```

**NFS Authentication**: NFS clients authenticate via AUTH_UNIX. The client's UID is matched against DittoFS user UIDs. If a match is found, the user's permissions are applied.

**SMB Authentication**: SMB clients authenticate via NTLM. The username is matched against DittoFS users, and permissions are applied from the user's configuration.

#### Groups

Create a group, add members, and grant access to an existing share:

```bash
dfsctl group create --name viewers --gid 102
dfsctl group add-user viewers editor
dfsctl share permission grant /archive --group viewers --level read
```

#### Guest Configuration

There is no top-level guest bootstrap section. Configure anonymous NFS identity
mapping per export, for example with
`dfsctl share nfs-config set /public --squash root_to_guest`. SMB guest sessions
are governed by adapter authentication policy and share permissions. See
[NFS access controls](nfs.md) and [SMB authentication](smb.md).

#### Permission Levels

| Permission | Description |
|------------|-------------|
| `none` | No access (cannot connect to share) |
| `read` | Read-only access |
| `read-write` | Read and write access |
| `admin` | Full access including delete and ownership |

#### Permission Resolution Order

1. **User explicit permission**: If the user has a direct `share_permissions` entry for the share, use it
2. **Group permissions**: Check all groups the user belongs to, use the highest permission level
3. **Share default**: Fall back to the share's `default_permission` setting

**Example** (with the `viewers` group and `/archive` share already created):

```bash
dfsctl user create --username special-viewer --groups viewers
dfsctl share permission grant /archive --group viewers --level read
dfsctl share permission grant /archive --user special-viewer --level read-write
```

In this example, `special-viewer` gets `read-write` on `/archive` (user explicit), even though the `viewers` group only has `read`.

#### CLI Management Commands

Users and groups live in the metadata store, not the config file. Manage them with
`dfsctl` against a running server (run `dfsctl login` first). See [CLI.md](cli.md) for the
complete, generated reference.

**User Commands:**

```bash
# Create a user (password prompted interactively)
dfsctl user create --username alice
dfsctl user create --username alice --host-uid                 # map to your current host UID
dfsctl user create --username bob --email bob@example.com --groups editors,viewers

# Inspect and edit
dfsctl user list
dfsctl user get alice
dfsctl user edit alice --email alice@example.com
dfsctl user remove alice

# Passwords
dfsctl user change-password           # change your own
dfsctl user password alice            # admin: reset another user's password
```

**Group Commands:**

```bash
dfsctl group create --name editors
dfsctl group list
dfsctl group get editors
dfsctl group add-user editors alice
dfsctl group remove-user editors alice
dfsctl group remove editors
```

**Share Permissions:**

Permissions are granted per share to a user or group via `dfsctl share permission`:

```bash
dfsctl share permission list  /export
dfsctl share permission grant /export --user alice  --level read-write
dfsctl share permission grant /export --group editors --level read
dfsctl share permission revoke /export --user alice
```

### 10. Protocol Adapters

This build serves no file protocol: `dfs start` runs only the control-plane
REST API and metrics. The adapter, grace, netgroup and client commands and
their REST routes are removed. These settings never belonged in the server YAML/TOML file and are not
read from `DITTOFS_ADAPTERS_*` environment variables.

### SMB3 Encryption Configuration

Require encrypted SMB traffic for a particular share with `--encrypt-data`:

```bash
dfsctl share create --name /secure --encrypt-data
```

See the [SMB security model](security.md#smb-encryption) for protocol-level
enforcement and the distinction between session and per-share encryption.

### 11. NFSv4 Configuration

NFSv4 delegation, grace-period and lease settings were adapter settings; see
[Protocol Adapters](#10-protocol-adapters).

### 12. Kerberos Configuration

`kerberos` is a top-level config block (it backs both NFS RPCSEC_GSS and SMB
SPNEGO — not nested under `adapters`):

```yaml
# Kerberos (RPCSEC_GSS + SMB SPNEGO) settings — top-level block
kerberos:
  enabled: true
  keytab_path: /etc/dittofs/dittofs.keytab
  service_principal: nfs/server.example.com@EXAMPLE.COM
  krb5_conf: /etc/krb5.conf

  # AD domain identity (optional; for an Active-Directory-joined server).
  # When all three are unset the server is standalone and advertises the
  # NetBIOS workgroup "WORKGROUP" exactly as a non-domain server does —
  # leaving these empty is fully backward-compatible.
  realm: EXAMPLE.COM            # Kerberos realm; defaults to the @REALM of service_principal
  netbios_domain: EXAMPLE       # NetBIOS short name; NOT derivable, must be set to enable domain-aware SMB
  dns_domain: example.com       # defaults to the lowercased realm
```

| Key | Env var | Default |
|---|---|---|
| `kerberos.realm` | `DITTOFS_KERBEROS_REALM` | `@REALM` of `service_principal` |
| `kerberos.netbios_domain` | `DITTOFS_KERBEROS_NETBIOS_DOMAIN` | (empty → standalone `WORKGROUP`) |
| `kerberos.dns_domain` | `DITTOFS_KERBEROS_DNS_DOMAIN` | lowercased `realm` |

#### Domain-aware SMB

When `netbios_domain` is set, the SMB server advertises the AD domain in the
NTLM challenge (`MsvAvNbDomainName` / `MsvAvDnsDomainName`) and stamps it on
authenticated sessions, so domain users authenticate against the correct domain.
Unset → the server advertises `WORKGROUP` / `local` (pre-AD-4 standalone
behavior).

#### Offline keytab import (one keytab, both protocols)

A keytab can hold multiple service principals, so a single file serves SMB and
NFS. Pre-create the computer/service account in AD and export a keytab
containing **both** `cifs/<host>@REALM` (SMB) and `nfs/<host>@REALM` (NFS):

```bash
# On a domain-joined admin host (samba-tool / adcli / Windows ktpass):
samba-tool domain exportkeytab /etc/dittofs/dittofs.keytab \
    --principal=cifs/server.example.com@EXAMPLE.COM
samba-tool domain exportkeytab /etc/dittofs/dittofs.keytab \
    --principal=nfs/server.example.com@EXAMPLE.COM
```

Import the combined keytab as `identity.keytab` with `PUT /api/v1/installation/keytab`. The SMB handler selects
the `cifs/` principal (deriving it from the NFS `service_principal`, or via an
explicit override); NFS RPCSEC_GSS uses the `nfs/` principal.

#### NTLM pass-through for AD domain users (NETLOGON machine account)

When a client connects by a name with no Kerberos SPN (an IP address, or the
LAN-discovery name from Explorer), Windows falls back to **NTLM**. DittoFS
validates a domain user's NTLM response against a Domain Controller over a
NETLOGON secure channel, which needs a **computer account** in the domain.

The computer account is not configured in this file: the `kerberos.machine_account`
block and its `DITTOFS_KERBEROS_MACHINE_ACCOUNT_*` variables are not read. Joining is a
management-API operation, online or offline, and rotation is automatic; see
[identity.md, Part C2](identity.md#part-c2-join-the-domain). What the join reads and
writes, as settings of the installation:

| Setting | Meaning |
|---|---|
| `identity.directory.realm`, `identity.directory.base` | the realm and base DN the join uses; an online join's OU must lie strictly under the base |
| `identity.netbios_name` | written by the join from the account's name (default `DITTOFS` until then); while joined, a change through the settings API is refused `409` `joined`, and a new join with `"takeover": true` changes it (see [identity.md](identity.md#after-a-join)) |
| `identity.keytab` | the sealed keytab of AES keys for service names the computer account does not hold, set with `PUT /api/v1/installation/keytab` |

For the step-by-step, including pre-creating the account for an offline join and
testing the Explorer double-click, see
[docs/guide/windows-ad-setup.md](windows-ad-setup.md).

### 13. Identity Mapping Configuration

```yaml
identity:
  # Pin this node's machine SID (Windows S-1-5-21-{a}-{b}-{c}).
  #
  # When unset, the machine SID is generated once on first boot and
  # persisted, staying stable across restarts. Local/algorithmic SIDs are
  # derived purely from the machine SID + the Samba RID formula
  # (user RID = uid*2+1000, group RID = gid*2+1001), so pinning the SAME
  # value on every node in a cluster makes them compute IDENTICAL SIDs for
  # the same Unix UID/GID — required for cross-node identity parity.
  # Foreign (Active Directory / LDAP) domain SIDs are NOT derived this way;
  # they are bound to stable UID/GIDs durably in the control-plane store.
  #
  # Env override: DITTOFS_IDENTITY_MACHINE_SID
  machine_sid: "S-1-5-21-1111111111-2222222222-3333333333"
```

The CLI no longer maps a Kerberos principal to a user.

### 14. Snapshot Scheduler

There are no snapshots to schedule yet (see [Snapshots](snapshots.md)). A config
file that still carries v1's `snapshot` block (`snapshot.scheduler_poll_interval`,
`snapshot.scheduler_disabled`, `snapshot.restore_http_timeout`) is refused at
load, naming each key as unknown. The `snapshots` keys of the new design, such
as `snapshots.cut_deadline`, are refused in the file too, as settings to set
through the management API.

### 15. LDAP / Active Directory Identity Provider

Resolves directory principals (a `user@REALM` form or an AD SID) to a Unix
identity by querying LDAP/AD. It reads the **RFC2307** `uidNumber`/`gidNumber`
POSIX attributes (the `idmap_ad` model) or falls back to **RID**-based
derivation (`idmap_rid`), and resolves the user's group memberships — including
nested AD groups via the `LDAP_MATCHING_RULE_IN_CHAIN` matching rule. The
provider is registered in the identity-resolution chain after Kerberos, so an AD
principal/SID with no local user mapping is resolved against the directory.

**Security:** the connection is encrypted by default. A plaintext `ldap://`
connection is **refused** unless it is upgraded with `start_tls: true` or the
operator explicitly opts in with `allow_plaintext: true`. Prefer `ldaps://`.

```yaml
ldap:
  enabled: true
  url: ldaps://dc.example.com:636        # ldaps:// (preferred) or ldap:// + start_tls
  start_tls: false                       # upgrade an ldap:// connection to TLS
  allow_plaintext: false                 # explicit opt-in for an unencrypted bind (off)
  base_dn: "DC=example,DC=com"
  bind_dn: "CN=svc-dittofs,CN=Users,DC=example,DC=com"
  bind_password: "********"              # service-account password (redacted on show)
  user_attr: sAMAccountName              # attribute matched against the bare username
  realm: EXAMPLE.COM                     # matches "user@REALM" credentials
  idmap: rfc2307                          # "rfc2307" (uidNumber/gidNumber) or "rid"
  nested_groups: true                     # resolve transitive AD group membership
  max_group_results: 200                  # cap on (nested) groups resolved per user
  timeout: 10s
  tls:
    ca_cert_file: /etc/dittofs/ad-ca.pem  # CA bundle to verify the directory cert
    client_cert_file: ""                  # optional mutual-TLS client cert
    client_key_file: ""
    insecure_skip_verify: false           # lab-only escape hatch (off)
    min_version: "1.2"                    # "1.2" or "1.3"
```

| Key | Env var | Default |
|---|---|---|
| `ldap.enabled` | `DITTOFS_LDAP_ENABLED` | `false` |
| `ldap.url` | `DITTOFS_LDAP_URL` | (required when enabled) |
| `ldap.start_tls` | `DITTOFS_LDAP_START_TLS` | `false` |
| `ldap.allow_plaintext` | `DITTOFS_LDAP_ALLOW_PLAINTEXT` | `false` |
| `ldap.base_dn` | `DITTOFS_LDAP_BASE_DN` | (required when enabled) |
| `ldap.bind_dn` | `DITTOFS_LDAP_BIND_DN` | (required when enabled) |
| `ldap.bind_password` | `DITTOFS_LDAP_BIND_PASSWORD` | (required when enabled; empty triggers an anonymous bind and is rejected) |
| `ldap.user_attr` | `DITTOFS_LDAP_USER_ATTR` | `sAMAccountName` |
| `ldap.realm` | `DITTOFS_LDAP_REALM` | (empty) |
| `ldap.idmap` | `DITTOFS_LDAP_IDMAP` | `rfc2307` |
| `ldap.nested_groups` | `DITTOFS_LDAP_NESTED_GROUPS` | `false` |
| `ldap.max_group_results` | `DITTOFS_LDAP_MAX_GROUP_RESULTS` | `200` |
| `ldap.timeout` | `DITTOFS_LDAP_TIMEOUT` | `10s` |
| `ldap.tls.ca_cert_file` | `DITTOFS_LDAP_TLS_CA_CERT_FILE` | (system roots) |
| `ldap.tls.client_cert_file` | `DITTOFS_LDAP_TLS_CLIENT_CERT_FILE` | (empty) |
| `ldap.tls.client_key_file` | `DITTOFS_LDAP_TLS_CLIENT_KEY_FILE` | (empty) |
| `ldap.tls.insecure_skip_verify` | `DITTOFS_LDAP_TLS_INSECURE_SKIP_VERIFY` | `false` |
| `ldap.tls.min_version` | `DITTOFS_LDAP_TLS_MIN_VERSION` | `1.2` |

Admin-configured identity links take precedence over the
directory query: a link for an `ldap` external ID resolves to the mapped local
user before any LDAP search is issued.

**Samba AD-DC self-signed certificates.** A default Samba AD-DC serves an
auto-generated TLS certificate that commonly has a *negative serial number*.
Go's `crypto/x509` rejects such certificates at parse time — before TLS
verification runs — so `ldap.tls.insecure_skip_verify` does **not** bypass it.
Use a properly-issued DC certificate.

**Managing identity providers over the API (no restart).** The `ldap.*` and
`kerberos.*` keys above seed the configuration on first boot. After that, both
providers can be read, updated, and tested over the control-plane API without
editing files:

| Method & path | Purpose |
|---|---|
| `GET /api/v1/identity-providers` | List providers + enabled state (no secrets). |
| `GET /api/v1/identity-providers/{type}/config` | Read config (bind password redacted to `********`). |
| `PUT /api/v1/identity-providers/{type}/config` | Create/replace config (validated). |
| `POST /api/v1/identity-providers/{type}/test` | Dry-run dial+bind (LDAP) / keytab check (Kerberos); never persists. |

`{type}` is `ldap` or `kerberos`; all routes are admin-only. A persisted config
**takes precedence over the file/env config** on subsequent boots. **LDAP
changes hot-reload the live resolver**; **Kerberos changes take effect on the
next server restart** (the NFS/SMB adapters bind it at startup). The bind
password is write-only — submit `********` (or omit it) on `PUT` to keep the
stored secret. The CLI has no equivalent.

## Migration

### Standalone CAS (v0.16 - v0.21) → packed blocks: removed

Current servers store remote data as packed `blocks/<id>` containers. The
automatic startup conversion for shares still holding standalone-CAS state
(per-chunk `cas/` objects and locators from v0.16-v0.21 servers) has been
removed: such a store cannot be upgraded in place by this build, and a
locator still pointing at a standalone object fails closed on read. See
[the migration guide](block-store-migration.md).

### Pre-v0.16 `.blk` layouts: migrate with dittofs ≤ v0.21 first

The offline `migrate-to-cas` command was removed after v0.21. On startup,
`dfs start` still probes each share for the legacy `.blk` layout (a
`.cas-migrated-v1` sentinel from an old migration short-circuits the probe)
and refuses to start un-migrated shares:

- Exits with code **78** (`EX_CONFIG` per sysexits(3)).
- Prints a directive to stderr naming the offending share and pointing at
  dittofs ≤ v0.21 for the `.blk` migration.
- Halts on the FIRST share that surfaces the legacy layout.

Run `migrate-to-cas` with a v0.21 binary, verify the per-share `.cas-migrated-v1`
sentinel exists, then upgrade — the automatic conversion above finishes the
job.

## Metrics (Prometheus)

DittoFS exposes a Prometheus `/metrics` endpoint on a **dedicated listener**,
separate from the management API and the protocol adapters. It is **on by
default, on loopback only**. It serves the standard Go/process collectors plus
the DittoFS instruments (request RED metrics per adapter, connection counts,
sync/remote/local-store/quota gauges).

```yaml
# Top-level metrics block (NOT nested under `server:`).
metrics:
  listen: 127.0.0.1:9090   # ip:port, or "off" to turn the listener off
```

`metrics.listen` is the only key of the block. It is an `ip:port` with an IP
literal, and `off` disables the listener; the default is `127.0.0.1:9090`. v1's
`enabled`, `host`, `port`, `path`, `auth`, `token_file` and `tls.*` keys are
gone, and a file that carries them is refused as unknown. The endpoint has no
Bearer-token auth of its own: it serves under the `api.tls` certificate on an
address that is not loopback (see [§5](#5-api-server)).

> **Security**: the endpoint binds `127.0.0.1` by default so it is not reachable
> off-host without an explicit `metrics.listen` change. In production prefer to
> bind loopback and scrape via a sidecar, or restrict the address with a
> firewall/NetworkPolicy. Never expose it on `0.0.0.0` on an untrusted network.

The environment override is `DITTOFS_METRICS_LISTEN`:

```bash
export DITTOFS_METRICS_LISTEN=0.0.0.0:9090
```

### Ownership model

**DittoFS is only a scrape target.** It never runs, bundles, manages, or depends
on a Prometheus/Thanos/Mimir instance. Point your cluster's existing Prometheus
at the endpoint. For production, the standard path is the
[kube-prometheus-stack](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack)
(prometheus-operator) with long-term storage via Thanos or Mimir. DittoFS ships
**no** dashboards or compose artifacts — the guidance below is enough to wire it
into any standard stack.

> **DittoFS Pro** bundles a turnkey monitoring stack on top of this endpoint: a
> `docker compose --profile monitoring` profile that stands up Prometheus +
> Grafana with a pre-provisioned DittoFS dashboard, plus an in-dashboard Metrics
> section. The endpoint and config documented here are the contract it builds on.

### Scraping (standalone Prometheus)

Add a static or service-discovery scrape job:

```yaml
scrape_configs:
  - job_name: dittofs
    metrics_path: /metrics
    static_configs:
      - targets: ["dittofs-host:9090"]
```

### Scraping (Kubernetes via the operator)

The DittoFS Kubernetes operator wires this up for you when you opt in on the
`DittoServer` spec. It renders the metrics container port, a dedicated metrics
`Service` carrying `prometheus.io/scrape` annotations (for annotation-based
discovery), and — **only if the `monitoring.coreos.com` CRDs are installed and
you enable it** — a `ServiceMonitor` for the prometheus-operator. If those CRDs
are absent the operator skips the `ServiceMonitor` (logged) and never fails the
reconcile.

```yaml
apiVersion: dittofs.dittofs.com/v1alpha1
kind: DittoServer
metadata:
  name: example
spec:
  storage:
    metadataSize: 10Gi
  metrics:
    enabled: true            # renders the metrics Service + scrape annotations
    port: 9090
    path: /metrics
    # bearerTokenSecret:     # optional authed scrape
    #   name: dittofs-metrics-token
    #   key: token
    serviceMonitor:
      enabled: true          # requires the prometheus-operator CRDs
      interval: 30s
      labels:                # match your Prometheus serviceMonitorSelector
        release: kube-prometheus-stack
```

### Core series (⭐ = key signals)

| Metric | Meaning |
|--------|---------|
| ⭐ `dittofs_adapter_requests_in_flight{protocol}` | Requests admitted and not yet answered. |
| ⭐ `dittofs_adapter_status_mapped_total{protocol,error}` | Service errors that reached the client, by error (`not_found`, `exist`, `io` and the rest of the closed set). |
| `dittofs_adapter_refusals_total{protocol,stage}` | Requests the adapter refused itself, by the pipeline stage that refused them. |
| ⭐ `dittofs_adapter_unmapped_errors_total{protocol}` | Errors no protocol row maps, answered the fault status: each one is a bug. |
| `dittofs_adapter_wire_seconds{protocol,stage}` | Time a request spends in each pipeline stage around the service call; its `_count` for `stage="decode_header"` counts every record received, one whose header does not decode included, so a ratio over it reads slightly low while a client sends records that do not decode. |
| `dittofs_adapter_connections{protocol,state}` / `dittofs_adapter_connection_closes_total{protocol,reason}` | Connections open, by state (`handshake`, `established`), and connections refused or closed, by reason. |
| ⭐ `dittofs_remote_up{share}` | `1` if the share's remote backend is healthy, else `0`. |
| ⭐ `dittofs_sync_pending_bytes{share}` | On-disk bytes present locally but not yet mirrored to the remote (data at risk). |
| `dittofs_localstore_disk_used_bytes{share}` | Local block-store disk bytes in use. |
| `dittofs_quota_used_bytes{scope,principal,share}` | Bytes used by a quota principal (`scope` user/group, `principal` is the uid/gid). |
| ⭐ `dittofs_snapshot_last_success_timestamp_seconds{share}` | Unix time of the last successful snapshot create (backup-freshness signal). |
| ⭐ `dittofs_integrity_findings{share,kind}` | Findings from the last structural manifest scan, by `kind`: `payloads_with_findings`, `damaged_payloads`, `claimed_uncovered_ranges`, `unplaceable_rows`, `unknown_hash_rows`. |
| ⭐ `dittofs_integrity_last_scan_timestamp_seconds{share}` | Unix time the structural manifest scan last completed. 0 means it has never run since process start **or** its last attempt failed. |
| ⭐ `dittofs_integrity_last_scan_failed{share}` | `1` when the most recent structural manifest scan failed, `0` when it completed or has never run. Pairs with the timestamp above to tell "never scanned" from "scans are erroring". |
| `dittofs_integrity_last_scan_duration_seconds{share}` / `dittofs_integrity_files_scanned{share}` | Cost and reach of the last structural manifest scan. |
| ⭐ `dittofs_offline_safe{share}` | `1` when every byte the share holds can be served with the remote unreachable, else `0`. A share whose residency cannot be determined reports `0` and publishes no byte counts. |
| `dittofs_offline_remote_only_bytes{share}` / `dittofs_offline_remote_only_ranges{share}` | Data the local tier no longer holds and would have to fetch from the remote to serve, and how many ranges it spans. |

### Example alert expressions

```yaml
groups:
  - name: dittofs
    rules:
      # Scheduled snapshots stale: a share with no successful create in 24h.
      # Uses the last-success gauge (its value is the snapshot time); timestamp()
      # on a counter would return the scrape time, not the last snapshot time.
      # Evaluated PER SHARE — an aggregate max() would let a freshly-snapshotted
      # share mask one that has never been snapshotted.
      - alert: DittoFSSnapshotStale
        expr: |
          (time() - dittofs_snapshot_last_success_timestamp_seconds) > 86400
        for: 1h
        labels: { severity: warning }
        annotations:
          summary: "DittoFS has had no successful snapshot create in >24h"

      # A share holding manifest damage. The read path cannot report this
      # class: an uncovered range a file still claims reads back as a sparse
      # hole, so reads return zeros and succeed. Only the scan sees it.
      - alert: DittoFSManifestDamage
        expr: dittofs_integrity_findings{kind="damaged_payloads"} > 0
        for: 15m
        labels: { severity: critical }
        annotations:
          summary: "DittoFS share {{ $labels.share }} has damaged payloads"

      # The integrity scan has not completed in 48h. A last-scan value of 0
      # means never scanned since process start, which this expression catches
      # because time() - 0 is far past the threshold.
      - alert: DittoFSIntegrityScanStale
        expr: (time() - dittofs_integrity_last_scan_timestamp_seconds) > 172800
        for: 1h
        labels: { severity: warning }
        annotations:
          summary: "DittoFS share {{ $labels.share }} has not been integrity-scanned in >48h"

      # The scan is running but failing. Without this the share looks
      # identical to one whose scanner was never enabled: both report a
      # last-scan timestamp of 0, forever.
      - alert: DittoFSIntegrityScanFailing
        expr: dittofs_integrity_last_scan_failed == 1
        for: 1h
        labels: { severity: warning }
        annotations:
          summary: "DittoFS integrity scan for {{ $labels.share }} is failing; the share is not being verified"

      # Remote block store unreachable.
      - alert: DittoFSRemoteDown
        expr: dittofs_remote_up == 0
        for: 5m
        labels: { severity: critical }
        annotations:
          summary: "DittoFS remote store for {{ $labels.share }} is down"

      # Sync backlog growing (unsynced data not draining).
      - alert: DittoFSSyncBacklogGrowing
        expr: delta(dittofs_sync_pending_bytes[30m]) > 0 and dittofs_sync_pending_bytes > 1e9
        for: 30m
        labels: { severity: warning }
        annotations:
          summary: "DittoFS sync backlog for {{ $labels.share }} is growing"

      # Local cache disk usage near a target ceiling (adjust threshold to your PVC size).
      - alert: DittoFSLocalStoreNearLimit
        expr: dittofs_localstore_disk_used_bytes > 0.9 * 100e9
        for: 10m
        labels: { severity: warning }
        annotations:
          summary: "DittoFS local store for {{ $labels.share }} is near its disk limit"

      # Elevated adapter error rate (>5% of requests over 5m): service errors
      # that reached the client, not_found left out as a lookup's usual answer.
      - alert: DittoFSAdapterErrorRate
        expr: |
          sum(rate(dittofs_adapter_status_mapped_total{error!="not_found"}[5m])) by (protocol)
            / sum(rate(dittofs_adapter_wire_seconds_count{stage="decode_header"}[5m])) by (protocol) > 0.05
        for: 5m
        labels: { severity: warning }
        annotations:
          summary: "DittoFS {{ $labels.protocol }} error rate above 5%"
```

### Dashboard guidance

Build (or import) a Grafana dashboard around the ⭐ core series: an adapter row
(request rate from `dittofs_adapter_wire_seconds_count{stage="decode_header"}`,
requests in flight, service errors by `error` from
`dittofs_adapter_status_mapped_total`, refusals by `stage`, unmapped errors, and
per-stage latency percentiles from `dittofs_adapter_wire_seconds`), a durability row
(`dittofs_remote_up`, `dittofs_sync_pending_bytes`), and a capacity row
(`dittofs_localstore_disk_used_bytes`, `dittofs_quota_used_bytes`). DittoFS does
not ship a dashboard JSON; these series are stable and named for direct use.

## Environment Variables

Override configuration using environment variables with the `DITTOFS_` prefix:

**Format**: `DITTOFS_<SECTION>_<SUBSECTION>_<KEY>`

- Use uppercase
- Replace dots with underscores
- Nested paths use underscores

#### Bootstrap admin password

`dfs init` creates the administrator account `admin`. Its first password comes
from `--admin-password-file` (a file of at most 4 KiB; one trailing newline is
dropped) or `--admin-password-env` (the name of an environment variable holding
it). With neither, `dfs init` generates one and prints it once to standard
output, never to the log, and it must be changed at the first sign-in.

To recover an administrator's password, stop the node and run `dfs reset-admin`
on the host that holds the metadata store. It prints a new generated password
once to standard output, which must be changed at the first sign-in, and refuses
every credential issued before it.

**Examples**:

```bash
# Logging
export DITTOFS_LOGGING_LEVEL=debug
export DITTOFS_LOGGING_FORMAT=json

# Metadata store
export DITTOFS_METADATA_BADGER_PATH=/var/lib/dittofs/metadata/store

# Start server with overrides
DITTOFS_LOGGING_LEVEL=debug ./dfs start
```

## Configuration Precedence

Server settings are applied in the following order (highest to lowest priority).
This precedence does not apply to stores managed through `dfsctl`:

1. **Environment Variables** (`DITTOFS_*`) - Highest priority
2. **Configuration File** (YAML/TOML)
3. **Default Values** - Lowest priority

Example:

```bash
# config.yaml has logging.level: info
# Override the log level for this process
DITTOFS_LOGGING_LEVEL=debug ./dfs start
```

## Configuration Examples

### Minimal Configuration

Server config file with minimal settings:

```yaml
logging:
  level: info
```

Then create the share with `dfsctl share create --name /export` (not available until the
CLI is rebuilt on the v2 API).

### Development Setup

Fast iteration with in-memory stores:

```yaml
logging:
  level: debug
  format: text
```

Then create the share with `dfsctl share create --name /export`.

### Production Setup

Persistent storage with access control, structured logging, and metrics:

```yaml
logging:
  level: warn
  format: json
  output: /var/log/dittofs/server.log

metrics:
  listen: 127.0.0.1:9090
```

Then create the share and grant permissions (not available until the CLI is rebuilt on
the v2 API):

```bash
./dfsctl share create --name /export
./dfsctl share permission grant /export --user alice --level read-write
```

### Multi-Share with Different Backends

In v2 every share lives in the node's one metadata store and journal, so shares do not
pick backends; they differ in permissions (not available until the CLI is rebuilt on the
v2 API):

```bash
./dfsctl share create --name /temp
./dfsctl share create --name /cloud
./dfsctl share create --name /public

# Grant permissions
./dfsctl share permission grant /temp --user alice --level read-write
./dfsctl share permission grant /cloud --user alice --level read-write
```

## IDE Support with JSON Schema

DittoFS provides a JSON schema for configuration validation and autocomplete in VS Code and other editors.

### Setup for VS Code

1. The `.vscode/settings.json` file is already configured
2. Install the [YAML extension](https://marketplace.visualstudio.com/items?itemName=redhat.vscode-yaml)
3. Open any `dittofs.yaml` or `config.yaml` file
4. Get autocomplete, validation, and inline documentation

### Generate Schema

If modified:

```bash
go run ./cmd/dfs config schema --output config.schema.json
```

### Features

- ✅ Field autocomplete
- ✅ Type validation
- ✅ Inline documentation on hover
- ✅ Error highlighting for invalid values

## Viewing Active Configuration

Check the generated config file:

```bash
# Default location
cat ~/.config/dittofs/config.yaml

# Custom location
cat /path/to/config.yaml
```

Start server with debug logging to see loaded configuration:

```bash
DITTOFS_LOGGING_LEVEL=debug ./dfs start
```
