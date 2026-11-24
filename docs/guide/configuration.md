# DittoFS Configuration Guide

A DittoFS node reads a few facts of its host before it opens any store: where
its metadata store is, its journals, its listeners, its logs and its wrapping
keys. These facts are its bootstrap, read from one YAML file and from the
environment. Everything else the node serves, shares, users, adapters'
settings and secrets among them, is managed through its management API and
kept in its metadata store, never in this file.

> Unfamiliar with a term? See the [Glossary](glossary.md) for plain-language
> definitions.

## Table of Contents

- [The file](#the-file)
- [The state and runtime directories](#the-state-and-runtime-directories)
- [The keys](#the-keys)
- [FoundationDB metadata](#foundationdb-metadata)
- [The environment](#the-environment)
- [Refusals](#refusals)
- [The commands](#the-commands)
- [Settings](#settings)
- [Secrets](#secrets)
- [Integration](#integration)

## The file

The node reads one YAML file, the first of:

1. the path `--config` names;
2. the path the `DITTOFS_CONFIG` variable names;
3. `$XDG_CONFIG_HOME/dittofs/config.yaml` when `XDG_CONFIG_HOME` is set;
4. `$HOME/.config/dittofs/config.yaml`.

A file named by `--config` or `DITTOFS_CONFIG` that does not exist refuses the
start. A default file that does not exist is no error: the node runs on the
defaults and the environment.

Each value is taken from the environment, else the file, else the default. A
value from the environment replaces the same key's value from the file and
merges with nothing else: a leaf is replaced alone, and the rest of its block
stays as the file has it.

The file holds one YAML document. A key is matched exactly as declared, one
name per level, so `Logging.Level` is an unknown key and a key that differs
only in case is refused with the declared one named. A key that is not in
[the table](#the-keys) refuses the start, with the closest declared key named;
a key that is a setting names the management API instead. A path is absolute
and written in its clean form: a journal's path is its identity in the
metadata store, so `/data/j1/` beside `/data/j1` would be a second journal,
and a relative path, a `~` or a trailing slash is refused rather than
rewritten.

A small file:

```yaml
logging:
  level: info
  format: json
  rotation:
    max_size: "100MiB"
    max_age: "720h"
journals:
  - path: /data/j1
  - path: /data/j2
api:
  listen:
    - 127.0.0.1:8080
metrics:
  listen: 127.0.0.1:9090
```

Each role also needs its wrapping key, `keys.protocol` and `keys.storage`,
each with its `kind` and the block that kind names; see
[the table](#the-keys).

## The state and runtime directories

The two directories are not keys: the environment names them.

The state directory is `$STATE_DIRECTORY` when the service manager sets it,
else `$XDG_STATE_HOME/dittofs`, else `$HOME/.local/state/dittofs`. The runtime
directory is `$RUNTIME_DIRECTORY`, else `$XDG_RUNTIME_DIR/dittofs`, else a
directory under the state directory that only the process's user owns (mode
`0700`). A unit may set its variable as a colon-separated list, whose first
entry is read.

A state directory that already exists is checked, not merely used: a start
refuses one that is a symlink, is owned by neither the node's user nor root,
or is writable by others. A runtime directory that already exists is checked
harder, since the socket in it has to be trusted: the start refuses one that
is a symlink, that the node's user does not own, or that group or others can
write, because whoever can write the directory can unlink `dfs.sock` and bind
its own. Today only `dfs config validate` runs every one of these checks: see
[The commands](#the-commands).

A start without root, `dfs init` then `dfs start`, writes its metadata, log and
`dfs.sock` under the user's home. A systemd unit's `StateDirectory=` and
`RuntimeDirectory=` set the first of each, so two nodes on one host each get
their own from their own unit.

## The keys

Every key of the file, with its type and default. `<role>` is `protocol` or
`storage`, so a key is `keys.protocol.local.file`.

| Key | Type | Default | Fact |
| --- | --- | --- | --- |
| `node.id` | UUID | none: the `node-id` copies give it | the node's identity, needed only when every copy is lost |
| `node.roles` | list of `protocol`, `storage` | `[protocol, storage]` | the node's roles, compared as a set, a repeated role refused; both is the only accepted value |
| `metadata.backend` | `badger` or `foundationdb` | `badger` | which metadata store; only the block it names is read, and a key of the other block is refused |
| `metadata.badger.path` | path | the state directory's `metadata` | Badger's directory |
| `metadata.foundationdb.cluster_file` | path | required with `foundationdb` | FoundationDB's cluster file |
| `metadata.foundationdb.tls.cert_file` | path | none: named with the key and CA files, or none | the client certificate FoundationDB's TLS options take; the file is the credential, never a value in this file |
| `metadata.foundationdb.tls.key_file` | path | none: named with the certificate and CA files, or none | the client certificate's key; the file is the credential, never a value in this file |
| `metadata.foundationdb.tls.ca_file` | path | none: named with the certificate and key files, or none | the CA file FoundationDB's TLS options take |
| `journals` | list of entries, each with a `path` | required | the journal devices, one directory each; a path listed twice, or two that name one directory, is refused. The variable `DITTOFS_JOURNALS` sets the paths only, as a comma-separated list, and replaces the file's list; a fact of one journal needs the file. An empty entry, so a trailing or doubled comma as well as an empty value, is refused |
| `adapters.listen` | list of IP literals, each an address to bind | every interface the host serves | the addresses the protocol adapters bind; each adapter's port is its own setting; an unset key binds `0.0.0.0`, plus `::` when the host has IPv6; a written list binds as written; a list in which one address overlaps another (a family's wildcard beside any address of that family, or the same address twice) refuses the node's start, naming the key and the index, never the value |
| `api.listen` | list of `ip:port` | `127.0.0.1:8080` | the management API's addresses. `:8080`, an empty host, is refused, not read as every interface: write `0.0.0.0:8080` or `[::]:8080` |
| `api.tls.cert_file` | path | required when an `api.listen` or `metrics.listen` address is not loopback: an IP in `127.0.0.0/8` or `::1`, so a host name, `localhost` included, and an unspecified address (`0.0.0.0`, `::`) are not | the API's certificate |
| `api.tls.key_file` | path | required when an `api.listen` or `metrics.listen` address is not loopback, as for `api.tls.cert_file` | the key of the API's certificate |
| `api.socket` | path | `dfs.sock` in the runtime directory | the management API's local socket, created mode `0600`. A `dfsctl` context names the socket by its path, so a shell without the unit's `$RUNTIME_DIRECTORY` passes the path, and two nodes on one host each name their own in their own file. A path past the operating system's socket-address limit (about 104 to 108 bytes) is refused, naming this key |
| `metrics.listen` | `ip:port`, or `off` | `127.0.0.1:9090` | the metrics listener, served over the API's TLS off loopback. `off` is the only way to turn it off, in the file and in the environment, since a YAML null and an empty variable both read as not set |
| `logging.level` | `debug`, `info`, `warn`, `error` | `info` | the log level, in lower case |
| `logging.format` | `text` or `json` | `text` | the log format |
| `logging.output` | `stdout`, `stderr` or a path | the state directory's `dittofs.log` | where logs go |
| `logging.rotation.max_size` | size, a whole number of `MiB`, at least `1MiB` | `"100MiB"` | a log file's size before it rotates. The rotation library takes whole MiB and reads a smaller value as none, so a fraction is refused rather than silently turning rotation off |
| `logging.rotation.max_backups` | integer | `5` | rotated files kept; `0` keeps all |
| `logging.rotation.max_age` | duration, a whole number of days (`0s` is zero) | `"720h"` | how long rotated files are kept; `0s` keeps all. The age is rounded to whole days by the rotation library, so a value below a day would read as never, and is refused rather than truncated |
| `logging.rotation.compress` | boolean | `false` | whether rotated files are compressed |
| `keys.<role>.kind` | `local` or `kmip` | required, for `protocol` and for `storage`, and the two roles name two different keys | each role's wrapping key; only the block `kind` names is read, and a key of the other block is refused, naming `kind` |
| `keys.<role>.local.file` | path | required with `local` | the key file that seals new `Secret` records |
| `keys.<role>.local.retired_files` | list of paths | none | the key files that still open what they sealed, and the next key before it seals, in a rotation's first step |
| `keys.<role>.kmip.endpoint` | string | none | the KMIP server that holds the role's key |
| `keys.<role>.kmip.server_ca` | path | none | the CA file that checks the KMIP server |
| `keys.<role>.kmip.client_cert` | path | none | the client certificate the node presents to the KMIP server |
| `keys.<role>.kmip.client_key` | path | none | the client certificate's key |
| `keys.<role>.kmip.key_uid` | string | none | the key that seals |
| `keys.<role>.kmip.timeout` | duration | none | how long a call to the KMIP server may take |
| `keys.<role>.kmip.retired_key_uids` | list of strings | none | the keys that still open what they sealed |

A key file's passphrase is read from `DITTOFS_ENCRYPTION_PASSPHRASE`, one for
every key file of the node.

## FoundationDB metadata

`metadata.backend: foundationdb` keeps the metadata, and the control plane's records, in a
FoundationDB 7.3 cluster instead of a Badger directory. It is opened only by a `dfs` built with the
`foundationdb` tag (the `-foundationdb` image and tarballs; see
[Deployment](install.md#foundationdb-metadata)); any other build refuses it, naming the tag.

```yaml
metadata:
  backend: foundationdb
  foundationdb:
    cluster_file: /etc/foundationdb/fdb.cluster
```

- `metadata.foundationdb.cluster_file` is required: an empty one is refused rather than reading
  the client's default cluster.
- An installation owns its whole cluster. The keys are written with no prefix, so the cluster must
  hold nothing else.
- `metadata.badger.*` is refused beside it, and `metadata.foundationdb.*` beside `badger`.
- `metadata.foundationdb.tls.cert_file`, `key_file` and `ca_file` are named together or not at all,
  and each file is the credential, never a value in this file. Named, they are the client's TLS
  files: `dfs init` and `dfs start` connect over TLS, and refuse a file that is missing, naming
  it, before they connect. Unnamed, the client connects without TLS.
- `dfs init` and `dfs start` wait up to 30 seconds to connect.

## The environment

Each key has one environment name: `DITTOFS_`, then the key in upper case with
its dots as underscores, so `logging.rotation.max_size` is
`DITTOFS_LOGGING_ROTATION_MAX_SIZE` and `keys.protocol.local.file` is
`DITTOFS_KEYS_PROTOCOL_LOCAL_FILE`. A name is matched exactly as written: a
lower-case name is not one of the node's.

A `DITTOFS_` name that is not a key's refuses the start, unless it is one of
these names, read outside the file:

| Name | Read by |
| --- | --- |
| `DITTOFS_CONFIG` | the start, the path of the host's file when `--config` is not given |
| `DITTOFS_ENCRYPTION_PASSPHRASE` | the local key provider, a key file's passphrase |
| `DITTOFS_PASSWORD` | `dfsctl`, which a shell that runs both may export |
| `DITTOFS_<service>_PORT*` and `DITTOFS_<service>_SERVICE_*`, for each Service of the pod's namespace | Kubernetes, which sets them for every Service with an address: ignored while the value has the shape the kubelet gives it (a `tcp://`, `udp://` or `sctp://` link, an IP, a port number or a protocol name, by the name's form), and refused otherwise |
| the names and prefixes the repository's development tools export (`DITTOFS_TEST_*`, `DITTOFS_E2E_*`, `DITTOFS_PREPUSH_*`, and the launchers' and git hooks' own names) | the tools, which a server they start inherits; a key's name wins over any of these |
| `DITTOFS_OTLP_ENDPOINT` | `dfs start`, the collector it sends traces to; provisional, read only until tracing has a key of its own |

A name that was a key of an older file is refused naming its replacement, and
a refusal never prints the variable's value.

## Refusals

Every refusal of the file or the environment reads:

```text
<where>: <key>: <rule>[; <what to do>]
```

*Where* is the file's path or `environment`. *Key* is the key as declared, or
the environment name. *Rule* is the rule broken. *What to do* names the
replacement of a key that moved, the closest declared key to an unknown one,
or the way to change a value that is not the host's. All the refusals of one
file and one environment are reported together, so one pass fixes them; a
rule that relates two keys runs only over keys that passed their own checks.
A refusal never prints the value of an environment variable, of a key that
holds a credential, or of a key the node does not declare or no longer reads,
such as a misspelled key or a key of an older file.

```text
/etc/dittofs/config.yaml: adapters.smb.port: a setting, not a host fact; set it through the management API
/etc/dittofs/config.yaml: journals: data/j1 is not an absolute path
/etc/dittofs/config.yaml: loging.level: unknown key; did you mean logging.level?
environment: DITTOFS_METADATA_BADGER_PATH: not an absolute path
```

## The commands

Each command reads the file a start reads: `--config`, else `DITTOFS_CONFIG`,
else the default path.

- `dfs config show` prints the file it read (empty when none was), the state
  and runtime directories with the variable that chose each, or `default` when
  none did, the socket path, and every key with where its value came from:
  `file`, the variable that set it, or `default`. A value that came from a
  variable prints as the variable's name, never its value, and a socket set by
  `DITTOFS_API_SOCKET` prints no path. A credential prints as its file's path,
  never its content. `--output json` prints the same as JSON. The output is a
  report, not a file a start can read.
- `dfs config validate` refuses what a start would refuse, in the shape above,
  and starts nothing: it creates no directory. It also opens both wrapping
  keys and checks the roles, the journals, the API's certificate off
  loopback, the socket path and the two directories. A start does not run
  these checks yet, except that it checks a runtime directory it makes, so
  `validate` can refuse a host that a start still runs on. On success it prints the file it read, or `none`, and
  `Validation: OK`.
- `dfs config schema` prints the table above as a JSON Schema: the types, the
  enumerations and the keys required whatever another key holds. It checks
  the file alone: a file whose journals or keys come from variables fails its
  required keys, and the rules that relate two keys are checked by
  `dfs config validate`. The schema describes the file's structure, and
  `dfs config validate` decides what is valid. `--output <file>` writes it
  to a file.
- `dfs config edit` opens the file in `$EDITOR`, else `$VISUAL`, else `vi`,
  and checks nothing.

## Settings

## Secrets

## Integration
