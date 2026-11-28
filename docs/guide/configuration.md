# Configuration

DittoFS is configured in two places:

- **The config file** holds what the node must know about its host before it can
  open any store: the node's ID, where its metadata and journals live, its key
  files, its listen addresses and its logging. You edit it on the host.
- **The management API** holds everything else: shares, users, groups, grants,
  adapters, remote stores, namespaces and settings. You change these with `dfsctl`
  while the node runs. They are kept in the metadata store, never in the file.

Read this page when you write or change the config file, or when `dfs` refuses it.
For the first setup, follow [Getting started](getting-started.md) instead.

> Unfamiliar with a term? See the [Glossary](glossary.md).

## Table of Contents

- [A first config file](#a-first-config-file)
- [Where the file is](#the-file)
- [Not working as documented yet](#not-working-as-documented-yet)
- [Every key](#the-keys)
- [Adding the protocol key](#adding-the-protocol-key)
- [The state and runtime directories](#the-state-and-runtime-directories)
- [FoundationDB metadata](#foundationdb-metadata)
- [Environment variables](#the-environment)
- [Reading a refusal](#refusals)
- [Checking a config file](#the-commands)
- [The management API and settings](#the-management-api)
- [Remote stores](#remote-stores)

## A first config file

This is the smallest file `dfs init` and `dfs start` accept:

```yaml
node:
  id: 6f1c2a4e-8d3b-4f7a-9c5e-2b8d4a6f0e13   # generate once with uuidgen
metadata:
  badger:
    path: /var/lib/dittofs/metadata
journals:
  - path: /var/lib/dittofs/journal           # must exist before dfs init
keys:
  storage:
    kind: local
    local:
      file: /var/lib/dittofs/storage.key     # dfs init creates it
```

A fuller file adds the protocol key, logging, and the API and metrics addresses:

```yaml
node:
  id: 6f1c2a4e-8d3b-4f7a-9c5e-2b8d4a6f0e13
metadata:
  badger:
    path: /var/lib/dittofs/metadata
journals:
  - path: /data/j1
  - path: /data/j2
keys:
  storage:
    kind: local
    local:
      file: /etc/dittofs/keys/storage.key
  protocol:
    kind: local
    local:
      file: /etc/dittofs/keys/protocol.key
api:
  listen:
    - 127.0.0.1:8080
metrics:
  listen: 127.0.0.1:9090
logging:
  level: info
  format: json
  rotation:
    max_size: "100MiB"
    max_age: "720h"
```

Both key files are unlocked with the passphrase in the `DITTOFS_ENCRYPTION_PASSPHRASE`
environment variable. Every `dfs` command that opens the installation needs it.

## The file

`dfs` reads one YAML file. It uses the first of these that applies:

| Order | Path |
|-------|------|
| 1 | the path `--config` names |
| 2 | the path the `DITTOFS_CONFIG` variable names |
| 3 | `$XDG_CONFIG_HOME/dittofs/config.yaml`, when `XDG_CONFIG_HOME` is set |
| 4 | `$HOME/.config/dittofs/config.yaml` |

If `--config` or `DITTOFS_CONFIG` names a file that does not exist, `dfs` refuses to
start. If the default file does not exist, that is not an error: `dfs` runs on the
defaults and the environment.

An environment variable overrides the same key in the file, and only that key. For
example, `DITTOFS_LOGGING_LEVEL=debug` changes `logging.level` and leaves the rest of
`logging` as the file has it.

Rules that catch people out:

- **Keys are exact.** `Logging.Level` is not `logging.level`. A misspelled or
  unknown key stops the start, and the message names the closest real key.
- **Settings are not keys.** A key such as `adapters.nfs.port` is a setting. The
  file refuses it and tells you to use the management API.
- **Paths are absolute and clean.** A relative path, a `~` or a trailing slash is
  refused, not rewritten. This matters most for journals: a journal's path is its
  identity, so `/data/j1/` and `/data/j1` would be two different journals.
- **One document.** The file holds one YAML document.

## Not working as documented yet

> **Not working as documented yet.** Four keys behave differently from
> [the table below](#the-keys) on this version:
>
> - `dfs config validate` refuses a file without `keys.protocol`, while `dfs init`
>   and `dfs start` run without it.
> - `dfs init`, `dfs start` and `dfs reset-admin` refuse a file without `node.id`,
>   with `node.id is not set`, because nothing else names the node yet.
> - `dfs start` fails on `metrics.listen: off`, because it tries to bind `off` as an
>   address.
> - `api.listen` takes one address: the node refuses to start with a second.
>
> `dfs config validate` passes the last three of these, and also passes overlapping
> `adapters.listen` entries: only `dfs start` refuses them. So a clean `validate`
> does not yet promise a clean start.

## The keys

Every key the file accepts. `<role>` is `protocol` or `storage`, so
`keys.<role>.local.file` means both `keys.protocol.local.file` and
`keys.storage.local.file`.

| Key | Type | Default | What it sets |
| --- | --- | --- | --- |
| `node.id` | UUID | none; the journals' `node-id` files remember it | The node's identity. Generate it once and never change it. Needed today: see [above](#not-working-as-documented-yet). |
| `node.roles` | list of `protocol`, `storage` | `[protocol, storage]` | The node's roles. Both is the only accepted value today; a repeated role is refused. |
| `metadata.backend` | `badger` or `foundationdb` | `badger` | Which metadata store to use. Only the block it names is read; a key of the other block is refused. |
| `metadata.badger.path` | path | `metadata` in the state directory | Badger's directory. |
| `metadata.foundationdb.cluster_file` | path | required with `foundationdb` | FoundationDB's cluster file. |
| `metadata.foundationdb.tls.cert_file` | path | none | The client certificate for FoundationDB's TLS. Name it with the key and CA files, or none of them. |
| `metadata.foundationdb.tls.key_file` | path | none | The client certificate's key. Name it with the certificate and CA files, or none of them. |
| `metadata.foundationdb.tls.ca_file` | path | none | The CA file for FoundationDB's TLS. Name it with the certificate and key files, or none of them. |
| `journals` | list of entries, each with a `path` | required | The journal devices, one directory each. A path listed twice, or two paths for one directory, is refused. `DITTOFS_JOURNALS` replaces the list with a comma-separated list of paths; an empty entry, such as a trailing comma, is refused. |
| `adapters.listen` | list of IP addresses | every interface | The addresses the NFS and SMB adapters bind. Unset, they bind `0.0.0.0`, plus `::` when the host has IPv6. A written list binds as written. Overlapping entries, such as `0.0.0.0` beside `192.0.2.10`, or one address twice, stop the start. Each adapter's port is a setting, not a key. |
| `api.listen` | list of `ip:port` | `127.0.0.1:8080` | The management API's address. Write a full address: `:8080` is refused; use `0.0.0.0:8080` or `[::]:8080` for every interface. One address only today. |
| `api.tls.cert_file` | path | required off loopback | The API's TLS certificate. Required when `api.listen` or `metrics.listen` is not a loopback IP (`127.0.0.0/8` or `::1`). A host name, `localhost` included, and `0.0.0.0` or `::` are not loopback. |
| `api.tls.key_file` | path | required off loopback | The key of the API's certificate. Required in the same cases as `api.tls.cert_file`. |
| `api.socket` | path | `dfs.sock` in the runtime directory | The API's local socket, created with mode `0600`. `dfsctl login --server` takes this path. A path longer than the system's socket limit (about 104 to 108 bytes) is refused. |
| `metrics.listen` | `ip:port`, or `off` | `127.0.0.1:9090` | The Prometheus metrics address. Off loopback it is served over the API's TLS. `off` is the only way to turn it off, in the file and in the environment, since a YAML null and an empty variable both mean not set; but see [above](#not-working-as-documented-yet). |
| `logging.level` | `debug`, `info`, `warn`, `error` | `info` | The log level, in lower case. |
| `logging.format` | `text` or `json` | `text` | The log format. |
| `logging.output` | `stdout`, `stderr` or a path | `dittofs.log` in the state directory | Where logs go. |
| `logging.rotation.max_size` | size in whole `MiB`, at least `1MiB` | `"100MiB"` | A log file's size before it rotates. A fraction is refused, since it would turn rotation off. |
| `logging.rotation.max_backups` | integer | `5` | Rotated files kept; `0` keeps all. |
| `logging.rotation.max_age` | duration in whole days, or `0s` | `"720h"` | How long rotated files are kept; `0s` keeps all. Less than a day is refused, since it would mean never. |
| `logging.rotation.compress` | boolean | `false` | Whether rotated files are compressed. |
| `keys.<role>.kind` | `local` or `kmip` | required for `storage`; for `protocol` see [above](#not-working-as-documented-yet) | Where the role's key is held. Only the block `kind` names is read. The two roles must use two different keys. |
| `keys.<role>.local.file` | path | required with `local` | The key file that seals new secrets. |
| `keys.<role>.local.retired_files` | list of paths | none | Older key files that still open what they sealed, during a rotation. |
| `keys.<role>.kmip.endpoint` | string | none | The KMIP server that holds the role's key. |
| `keys.<role>.kmip.server_ca` | path | none | The CA file that checks the KMIP server. |
| `keys.<role>.kmip.client_cert` | path | none | The client certificate the node shows the KMIP server. |
| `keys.<role>.kmip.client_key` | path | none | That certificate's key. |
| `keys.<role>.kmip.key_uid` | string | none | The KMIP key that seals. |
| `keys.<role>.kmip.timeout` | duration | none | How long a call to the KMIP server may take. |
| `keys.<role>.kmip.retired_key_uids` | list of strings | none | Older KMIP keys that still open what they sealed. |

What the two keys protect:

| Key | Protects | Needed by |
|-----|----------|-----------|
| `keys.storage` | the administrator's password hash and the API's signing key | `dfs init` and `dfs start` |
| `keys.protocol` | users' NT hashes for SMB passwords, and directory bind credentials | `dfsctl user set-password`, binding to a directory, `dfs reset-admin` for an account with a uid |

`dfs init` creates the storage key file. Nothing creates the protocol key file for
you; the next section shows how.

## Adding the protocol key

You need `keys.protocol` before you set any user's password with
`dfsctl user set-password` or bind to a directory. Without it, `set-password`
answers `503 unavailable`.

> **Not available yet on this version:** a `dfs` command that writes a key file.
> Use the short Go program below, run from your DittoFS clone (see
> [Getting started](getting-started.md#1-build-from-source)).

1. Write the key file, with the same passphrase as the storage key:

   ```bash
   cd ~/dittofs                                   # your clone
   mkdir -p /tmp/dfs-keygen
   cat > /tmp/dfs-keygen/main.go <<'EOF'
   package main

   import (
   	"os"

   	"github.com/cubbit/dittofs/pkg/keyprovider"
   )

   func main() {
   	b, err := keyprovider.GenerateKeyFile(os.Getenv("DITTOFS_ENCRYPTION_PASSPHRASE"))
   	if err != nil {
   		panic(err)
   	}
   	if err := os.WriteFile(os.Args[1], b, 0o600); err != nil {
   		panic(err)
   	}
   }
   EOF
   go run /tmp/dfs-keygen/main.go ~/dittofs-data/protocol.key
   ```

2. Name it in the config file, under `keys`:

   ```yaml
   keys:
     protocol:
       kind: local
       local:
         file: /home/alice/dittofs-data/protocol.key
   ```

3. Check the file, then restart the node:

   ```console
   $ dfs config validate
   Configuration file: /home/alice/.config/dittofs/config.yaml
   Validation: OK
   ```

Copy the key file off the host, as you did the storage key. More on key files,
KMIP and rotation is in [Encryption](encryption.md#local-key-files).

## The state and runtime directories

The node keeps its log, and by default its metadata store, in a **state
directory**. It keeps its local socket in a **runtime directory**. The environment
chooses both; they are not keys.

| Directory | First of |
|-----------|----------|
| State | `$STATE_DIRECTORY` (set by systemd's `StateDirectory=`), `$XDG_STATE_HOME/dittofs`, `$HOME/.local/state/dittofs` |
| Runtime | `$RUNTIME_DIRECTORY` (set by systemd's `RuntimeDirectory=`), `$XDG_RUNTIME_DIR/dittofs`, a `run` directory inside the state directory, mode `0700` |

A systemd unit may set these variables as a colon-separated list; the first entry
is used. Two nodes on one host each get their own directories from their own units.

So a node you run as yourself, with nothing set, writes its metadata, its log and
`dfs.sock` under `~/.local/state/dittofs`.

An existing directory is checked before it is used, because whoever can write the
runtime directory could replace `dfs.sock` with their own socket:

| Directory | Refused when it is |
|-----------|--------------------|
| State | a symlink, owned by neither the node's user nor root, or writable by others |
| Runtime | a symlink, not owned by the node's user, or writable by its group or others |

Today only `dfs config validate` runs all of these checks. `dfs start` checks only
a runtime directory it creates itself.

## FoundationDB metadata

`metadata.backend: foundationdb` keeps the metadata, users, shares and settings in a
FoundationDB 7.3 cluster instead of a Badger directory. Only a `dfs` built with the
`foundationdb` tag can open it: the `-foundationdb` image and tarballs (see
[Installation](install.md#foundationdb-metadata)). Any other build refuses it and
names the tag.

```yaml
metadata:
  backend: foundationdb
  foundationdb:
    cluster_file: /etc/foundationdb/fdb.cluster
```

- `cluster_file` is required. An empty one is refused rather than falling back to
  the client's default cluster.
- The installation owns the whole cluster. Keys are written with no prefix, so the
  cluster must hold nothing else. Opening the store writes the node's clock-ceiling key
  before the start check reads the cluster, so a `cluster_file` that points at someone
  else's cluster by mistake is refused, but leaves that one key behind in it.
- A `metadata.badger` key beside `foundationdb` is refused, and the other way round.
- `tls.cert_file`, `tls.key_file` and `tls.ca_file` are named together or not at
  all. With them, every command that opens the store (`dfs init`, `dfs start` and
  `dfs reset-admin`) connects over TLS, and refuse a missing
  file by name before connecting. Without them, the client connects without TLS.
- Every command that opens the store waits up to 30 seconds to connect.

## The environment

Every key has an environment variable: `DITTOFS_`, then the key in upper case with
dots as underscores.

| Key | Variable |
|-----|----------|
| `logging.level` | `DITTOFS_LOGGING_LEVEL` |
| `logging.rotation.max_size` | `DITTOFS_LOGGING_ROTATION_MAX_SIZE` |
| `keys.protocol.local.file` | `DITTOFS_KEYS_PROTOCOL_LOCAL_FILE` |
| `journals` | `DITTOFS_JOURNALS` (comma-separated paths) |

```bash
DITTOFS_LOGGING_LEVEL=debug dfs start
```

Names are exact: a lower-case name is ignored. A `DITTOFS_` variable that is not a
key stops the start, unless it is one of these:

| Variable | Read by |
| --- | --- |
| `DITTOFS_CONFIG` | `dfs`, the config file's path when `--config` is not given |
| `DITTOFS_ENCRYPTION_PASSPHRASE` | `dfs`, the passphrase of every local key file |
| `DITTOFS_PASSWORD` | `dfsctl`, which a shell running both may export |
| `DITTOFS_OTLP_ENDPOINT` | `dfs start`, where to send traces; temporary, until tracing has a key of its own |
| `DITTOFS_<service>_PORT*`, `DITTOFS_<service>_SERVICE_*` | Kubernetes, which sets them for every Service in the pod's namespace whose name starts with `dittofs`. Ignored while the value looks like what Kubernetes sets (a `tcp://`, `udp://` or `sctp://` address, an IP, a port or a protocol name); refused otherwise |
| `DITTOFS_TEST_*`, `DITTOFS_E2E_*`, `DITTOFS_PREPUSH_*` and the other names the repository's development tools export | those tools; a key's own name wins over them |

A variable that was a key in an older version is refused, and the message names its
replacement. A refusal never prints a variable's value.

## Refusals

When `dfs` refuses the file or the environment, it prints every problem at once, so
you can fix them in one pass. Each line reads:

```text
<where>: <key>: <rule>[; <what to do>]
```

*Where* is the file's path or `environment`. For example, this file has four
mistakes:

```yaml
loging:
  level: info
adapters:
  smb:
    port: 1445
journals:
  - path: data/j1
api:
  listen:
    - localhost:8080
```

```console
$ dfs config validate --config /etc/dittofs/config.yaml
Error: /etc/dittofs/config.yaml: adapters.smb.port: a setting, not a host fact; set it through the management API
/etc/dittofs/config.yaml: api.listen: localhost:8080: the host is not an IP literal
/etc/dittofs/config.yaml: journals: data/j1 is not an absolute path
/etc/dittofs/config.yaml: loging.level: unknown key; did you mean logging.level?
```

A file from an older version gets one line per old key, naming what replaced it:

```text
/etc/dittofs/config.yaml: controlplane.port: no longer read; write api.listen
/etc/dittofs/config.yaml: database.type: no longer read; the metadata store is metadata.backend
/etc/dittofs/config.yaml: shutdown_timeout: no longer read; set adapters.<protocol>.drain_timeout through the management API
```

A refusal never prints the value of a variable, of a key that holds a credential, or
of an unknown key.

## The commands

Four `dfs config` commands read the same file `dfs start` would.

### `dfs config validate`

Checks the file and the host without starting anything, and creates no directory.
Beyond what a start checks, it opens both key files, and checks the roles, the
journals, the API certificate off loopback, the socket path and the two
directories. So it can refuse a host that `dfs start` would still run on. It also
misses a few things a start refuses; see
[Not working as documented yet](#not-working-as-documented-yet).

```console
$ dfs config validate
Configuration file: /home/alice/.config/dittofs/config.yaml
Validation: OK
```

### `dfs config show`

Prints the file it read, the two directories and what chose them, the socket
path, and every key with its value and where the value came from: `file`, the
variable that set it, or `default`. It never prints a variable's value or a key
file's content. `--output json` prints the same as JSON. The output is a report,
not a config file you can load.

```console
$ dfs config show
file: /home/alice/.config/dittofs/config.yaml
directories:
  state:
    path: /home/alice/.local/state/dittofs
    source: HOME
  runtime:
    path: /home/alice/.local/state/dittofs/run
    source: HOME
socket:
  path: /home/alice/.local/state/dittofs/run/dfs.sock
  source: default
...
sources:
  journals: file
  logging.level: default
  metadata.badger.path: file
  node.id: file
...
```

### `dfs config schema`

Prints [the table above](#the-keys) as a JSON Schema, for editors that check YAML.
`--output <file>` writes it to a file, for example
`dfs config schema --output dittofs.schema.json`. The schema checks the file's
shape only: a file whose journals or keys come from variables fails its required
keys. `dfs config validate` is what decides whether a file is valid.

### `dfs config edit`

Opens the file in `$EDITOR`, else `$VISUAL`, else `vi`. It checks nothing; run
`dfs config validate` afterwards.

## The management API

Everything except the config file lives in the metadata store and is changed
through the management API: shares and their export policies, users, groups,
grants, netgroups, quotas, adapters, remote stores, namespaces and settings.
`dfs init` and `dfs reset-admin` are the only commands that write it on the host.
Most changes apply without a restart.

The API listens on `api.listen` and on the socket `api.socket` names. Off loopback
it serves TLS only, with `api.tls.cert_file` and `api.tls.key_file`. Two health
checks need no sign-in:

```console
$ curl http://127.0.0.1:8080/health/live
live
$ curl http://127.0.0.1:8080/health/ready
ready
```

Every other route under `/api/v1/`, apart from sign-in and token refresh, needs a
signed-in caller. The caller's role, `viewer`, `operator` or `admin`, decides what
it may do. `dfsctl` is the API's client; see the [CLI reference](cli.md).

### Settings

A setting is read and written by its scope and key:

```console
$ dfsctl setting set installation api.session_lifetime 8h
KEY                   VALUE  APPLIED
api.session_lifetime  "8h"

$ dfsctl setting get installation api.session_lifetime
KEY                   VALUE  APPLIED
api.session_lifetime  "8h"   0/1

$ dfsctl setting unset installation api.session_lifetime   # back to the default
```

`APPLIED` counts the nodes that have applied the value so far. A scope is
`installation`, `node:<id>`, `namespace:<id>`, `store:<id>` or `share:<name>`, but
every setting today exists at `installation` only, and a write at another scope is
refused. A `get` of a key that has no value at that scope answers `404 not_found`; it does not
print the default.

These settings exist today, all at the `installation` scope only:

| Section | Keys | Who may write |
| --- | --- | --- |
| `api` | `session_lifetime` (default `12h`), `token_max_lifetime` (default a year) | `admin` |
| `audit` | `retention` (default 400 days, at least 90) | `admin` |
| `identity` | `id_range`, `ntlm`, `netbios_name`, `keytab`, `directory_stale_grace`, `nfs4_domain` | `operator`; `ntlm` and `keytab` `admin` only |
| `identity.directory` | the directory record; see [Identity](identity.md) | `admin` |

> **Not available yet on this version:** settings for the NFS and SMB adapters,
> remote stores and namespaces. Any other key is refused with `unknown_key`, so
> `dfsctl adapter set`, `dfsctl store create` and `dfsctl namespace create` fail:
>
> ```console
> $ dfsctl adapter set nfs --enabled
> Error: dfsctl adapter set: writing adapter nfs: 400 unknown_key: Bad Request: refused by the settings table (field adapters.nfs.enabled)
> ```

### Remote stores

A remote store is an S3 bucket, on Amazon S3 or a service compatible with it, that
DittoFS keeps file data in. A store's settings say where the bucket is and how to
reach it; each namespace on the store adds the prefix its objects live under.

> **Not available yet on this version:** the node's settings table does not carry
> these keys, so `dfsctl store create` and `dfsctl namespace create` are refused
> with `unknown_key`, as the note above shows. This section describes the keys and
> the credential as the node takes them once it does.

#### The keys

| Key | Scope | Class | Default | What it is |
| --- | --- | --- | --- | --- |
| `remote.endpoint` | remote store | live | required | the service's URL: `https://` and a host, with an optional port and path |
| `remote.credential` | remote store | live | required | the access key and secret, set as a secret, never as a setting ([below](#the-credential)) |
| `remote.region` | remote store | live | `us-east-1` | the region requests are signed for |
| `remote.trust_roots` | remote store | live | `""`, the system's roots | CA certificates the endpoint's certificate is verified against ([below](#trust-roots)) |
| `remote.bucket` | remote store | bound | required | the bucket every object of the store lives in |
| `remote.storage_class` | remote store | next write | `STANDARD` | the class every object is written in |
| `remote.block_target` | remote store | next write | `4MiB` | the data carried per block, from `1MiB` to `64MiB` |
| `remote.prefix` | namespace | bound | required | the key prefix of the namespace's objects; ends in `/` |
| `remote.block_format_version` | namespace | next write | `1` | the block format version written |

The class says when a change takes effect:

- **live** applies to the running store. A new `remote.endpoint` or a new credential
  must reach the same content, which the store's capability check proves; until
  the API runs that check, only the first value is accepted.
- **bound** cannot change once the store or namespace holds content. On this
  version only the value written at creation is accepted.
- **next write** applies to blocks written after the change; blocks already in the
  bucket keep theirs.

Every value is checked when it is written and again when the node starts, and a
value that breaks a rule is refused, naming the key and the rule. Nothing is
clamped or replaced by its default:

- `remote.endpoint` must be `https://` with a host. User info
  (`https://key:secret@host`), a query and a fragment are refused, so no credential
  rides in the URL. Plain `http://` is refused: the client speaks only HTTPS.
- `remote.region` and `remote.storage_class` may be left unset for their default,
  but an explicit empty value is refused.
- `remote.storage_class` takes only a class whose objects can be read back without
  a restore: `STANDARD`, `REDUCED_REDUNDANCY`, `STANDARD_IA`, `ONEZONE_IA`,
  `INTELLIGENT_TIERING`, `GLACIER_IR` or `EXPRESS_ONEZONE`, spelt as S3 spells them.
  `GLACIER`, `DEEP_ARCHIVE` and any class not on the list are refused. When a store
  opens, the node logs `remote store opened` with the class and its
  `failure_domain`: `one zone` for `ONEZONE_IA` and `EXPRESS_ONEZONE`, whose objects
  live in a single availability zone and are lost with it, and
  `as the service documents` for the others.
- `remote.block_target` outside `1MiB` to `64MiB` is refused.
- `remote.prefix` must end in `/`, so one namespace's keys never run into
  another's (`ns/` beside `ns2/`), and must be valid UTF-8 with no control
  characters.
- `remote.block_format_version` takes only a version this binary's codec writes,
  which is `1` today.

No key sets how many connections the client opens to the service, and no key turns
off certificate verification.

For example, a store on a service at `s3.example.com`, then a namespace on it:

```console
$ dfsctl store create \
    --setting remote.endpoint=https://s3.example.com \
    --setting remote.bucket=dittofs-data \
    --setting remote.region=eu-west-1 \
    --credential-file s3-credential.json
$ dfsctl namespace create --store <store-id> --setting remote.prefix=builds/
```

#### The credential

The credential is one JSON object holding the access key ID and the secret access
key, under exactly these two names:

```json
{"access_key_id": "EXAMPLEACCESSKEYID", "secret_access_key": "example-secret-access-key"}
```

Put it in a file only you can read and pass the file with `--credential-file`, to
`dfsctl store create` or to `dfsctl store set-credential <id>`. The server seals it
as a secret; the store's `remote.credential` setting holds only a reference to it,
and no answer, audit record or log line holds the key or the secret. On this
version `set-credential` is refused `409 unproven`, since a credential set there
must first pass the store's capability check, which the API does not run yet.

A value that is not this object is refused when it is written, with `400 invalid`
on the credential, and again when the node builds the store's client. The names are
matched exactly, in lower case; each must appear once with a non-empty string of
valid UTF-8; and
nothing but whitespace may follow the object. So `KEY:SECRET`, a third field such
as a session token, `"Access_Key_Id"` or an empty secret are all refused. The
refusal names the field that is missing, empty or repeated, and never prints the
value.

#### Trust roots

The client connects to the endpoint over HTTPS only and always verifies the
service's certificate; no key skips that check. `remote.trust_roots` is how you
reach an endpoint whose certificate comes from a CA your system does not trust.
Set it for:

- a private or company CA;
- an on-premises S3 service, such as MinIO or Ceph, behind such a CA;
- a test endpoint with a self-signed certificate, such as LocalStack when it
  serves its own.

Leave it unset, or set it to `""`, to use the system's roots, which is what
Amazon S3 and most hosted services need.

The value is a PEM bundle of `CERTIFICATE` blocks, and it **replaces** the
system's roots for that store; it does not add to them. So it must hold every CA
the endpoint's certificate chain needs: a store on a private CA, set this way,
cannot reach a service whose certificate a public CA signed.

For example, for a service whose certificate your company's CA signed, with the CA
in `company-ca.pem`:

```console
$ cat company-ca.pem
-----BEGIN CERTIFICATE-----
MIIBszCCAVmgAwIBAgIUJ3m0o6c1i0f9V0aJ2pbq9dF8Q0kwCgYIKoZIzj0EAwIw
...
-----END CERTIFICATE-----
$ dfsctl store create \
    --setting remote.endpoint=https://minio.internal.example:9000 \
    --setting remote.bucket=dittofs-data \
    --setting "remote.trust_roots=$(cat company-ca.pem)" \
    --credential-file s3-credential.json
```

In a request body the bundle is a JSON string, its line breaks written `\n`:

```json
{"spec": {"settings": {
  "remote.endpoint": "https://minio.internal.example:9000",
  "remote.bucket": "dittofs-data",
  "remote.trust_roots": "-----BEGIN CERTIFICATE-----\nMIIBszCCAVmgAwIBAgIU...\n-----END CERTIFICATE-----\n"
}}}
```

LocalStack on your own machine serves a public certificate for
`localhost.localstack.cloud` when it can download one, and a self-signed one when it
cannot. In the second case, save the certificate it serves and trust that one
certificate:

```console
$ openssl s_client -connect localhost.localstack.cloud:4566 \
    -servername localhost.localstack.cloud </dev/null 2>/dev/null \
    | openssl x509 > localstack.pem
$ dfsctl store create \
    --setting remote.endpoint=https://localhost.localstack.cloud:4566 \
    --setting remote.bucket=test-bucket \
    --setting "remote.trust_roots=$(cat localstack.pem)" \
    --credential-file localstack-credential.json
```

LocalStack accepts any credential, so `localstack-credential.json` can hold
`{"access_key_id": "test", "secret_access_key": "test"}`. Its certificate can change
when the container is recreated; save it again then.

Each block of the bundle must be a `CERTIFICATE` that parses, and only whitespace
may sit between and around the blocks. A value with no certificate, a block of
another type (a private key, say), a corrupted block or stray text is refused when
it is written and again when the node starts, naming the block's position, for
example `trust_roots: block 2 is not a well-formed PEM block`. A malformed CA is
caught there, not later as connections that fail.
