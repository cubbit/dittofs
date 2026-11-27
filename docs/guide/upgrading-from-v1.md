# Upgrading from v1

Upgrading from v1 is not supported. This release is a new installation: it reads no
configuration, control-plane database, metadata store or block layout a v1 release
wrote, and no tool converts them. Every release up to v0.36.0 is v1.

## What a start says

A start that finds v1 data refuses rather than serve beside it. A v1 config file is
refused at load, `dfs config validate` included, each key this release does not read
named with what replaces it where something does:

```
config.yaml: database.sqlite.path: unknown key; did you mean metadata.badger.path?
config.yaml: blockstore.local.path: no longer read; write blockstore.journal.path
```

Every other v1 find refuses with one sentence:

```
refusing to start: <found> at <path> is from v1; upgrading from v1 is not supported: start a new installation with dfs init on empty storage
```

| `<found>` | Where it is looked for |
|-----------|------------------------|
| a control-plane database | `controlplane.db` beside the config file, `/data/controlplane/controlplane.db`, `/data/state/controlplane.db` |
| a pre-journal block layout | a `shares/<name>/` holding a populated `journal`, `blobs` or `logs` directory, under a journal device or under `blockstore.journal.path` |
| a metadata store | `metadata.badger.path`, at `dfs start` and at `dfs init` |

`dfs start` may prefix the sentence with the step that found it (`the start check: `
or `building the node: `). Nothing v1 wrote is changed, but opening a v1 metadata
store's directory writes one record of this release's own into it, so do not point
this release at a v1 store you mean to keep using with v1.

## Starting on this release

Leave the v1 data where it is, or move it out of the paths this release reads. Write a
new config file in this release's keys, pointing `metadata.badger.path` and each
`journals` device at empty directories, then run `dfs init` and `dfs start`.

This build serves no file protocol yet (see
[Data path](../internals/architecture.md#data-path)), so there is nothing yet to copy a
v1 share's files into. Block layouts are covered in the
[block store migration guide](block-store-migration.md).

## v1 releases stay available

v0.36.0 and the earlier releases stay on the
[GitHub releases page](https://github.com/cubbit/dittofs/releases). Keep v1 data on the
v1 release that wrote it. That release's own guides, the metadata store migration guide
among them, are in its tree:
[docs/guide at v0.36.0](https://github.com/cubbit/dittofs/tree/v0.36.0/docs/guide).
