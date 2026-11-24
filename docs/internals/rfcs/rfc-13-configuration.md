---
rfc: 13
title: "RFC 13 — configuration"
component: configuration
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-1-journal]]"
  - "[[rfc-2-carver]]"
  - "[[rfc-3-syncer]]"
  - "[[rfc-4-remote-tier]]"
  - "[[rfc-5-transforms]]"
  - "[[rfc-7-namespace-metadata]]"
  - "[[rfc-8-engine]]"
  - "[[rfc-9-gc]]"
  - "[[rfc-10-journal-replication]]"
  - "[[rfc-11-ownership]]"
  - "[[rfc-12-snapshots]]"
  - "[[rfc-15-topology]]"
  - "[[rfc-16-metadata-store]]"
  - "[[rfc-26-catalog-backups]]"
  - "[[rfc-27-namespace-migration]]"
  - "[[rfc-28-journal-format]]"
aliases:
  - RFC 13
tags:
  - rfc
---
# RFC 13 — configuration

**Status:** draft. [§11](#11.%20Open%20questions) lists what is undecided; [§10](#10.%20Edits%20this%20document%20asks%20of%20other%20RFCs)
records the edits it asked of other RFCs.
**Audience:** anyone adding a setting, implementing the control plane's records,
or deciding what an operator may change and when. Conventions and test tiers are
in [the RFC index](rfc-index.md).

This document specifies behaviour, not the current code.
[Appendix A](#Appendix%20A%20%E2%80%94%20where%20the%20current%20code%20differs) lists where the code differs.

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

### In plain words

Every DittoFS setting lives in one place: records in the metadata store, called the control plane.
A server's own file holds only what it needs to find that place: who it is, where the store is, its local disks.
Each setting also says what changing it does: apply now, apply at the next restart, apply to new data only, or never once data exists.
Think of a building whose plans are kept at the town hall: you may repaint a room any day, but not move a load-bearing wall.
Where the picture breaks: here the town hall refuses the wall change outright, naming the setting and the rule.
A wrong value is refused when it is written, and again when it is used; it is never quietly swapped for a default.
Passwords and keys never sit in a setting; a setting holds only the name of a sealed secret.

**What this is.** Every DittoFS component takes a few settings. This RFC says
where all of them live — in one place, the control plane, kept in the metadata
store — which quantities are settings at all, how far each one reaches, and
above all what happens when one is changed while data is already stored.

**The problem, with one example.** The `profiles` share keeps its content in a
**namespace**, a prefix in the S3 bucket `dfs-data`. Each block's location in
the bucket follows from its name and that prefix, and each chunk is named by a
keyed hash of its bytes under the namespace's chunk-ID key.

An operator tidying the bucket edits the prefix to `dittofs/profiles/` in a
configuration file on the node and restarts it. Nothing fails at once, and:

1. every block already stored is unreachable, because its location follows from
   its name and the old prefix: alice's next sign-in reads her evicted container
   blocks and fails;
2. new blocks land under the new prefix, so the namespace is split across two
   prefixes, and GC listing the old one finds blocks it thinks nothing records;
3. **(cluster)** in a cluster, nodes still reading the old prefix from their own
   files disagree with this one about where the namespace is.

Under this RFC:

1. The prefix is one field of the namespace's record in the metadata store,
   read by every node. No host file can override it.
2. It is **bound**: it decides where stored content is. While the store holds
   any, the control plane refuses the change and names the field and the rule.
   alice's data stays readable.
3. What the operator wants is a migration — a re-home into a new namespace
   ([RFC 27](rfc-27-namespace-migration.md)) — not an edit. `Target`, the chunk-ID key and whether the
   namespace encrypts are bound the same way: each decides the identity of
   stored content.

Other settings of the same share change differently:

- turning on compression, part of the remote store's transform chain, governs
  the **next write**: new blocks are compressed, old ones stay as they were
  written and remain readable, because each block's name records how it was
  written. Turning on encryption is not: whether a namespace encrypts is fixed
  when it is created;
- the `atime` policy is **live**: every node applies a change within 5 s;
- the syncer's worker pool sizes need a **restart**: a pool is sized once, when
  the process builds it;
- the S3 credential is never in configuration at all. The record holds a
  reference to a sealed secret, and rotating the credential changes no record.

![Each host holds only its bootstrap (node identity and roles, how to reach the metadata store, local devices and scratch paths, listen addresses, logging, the memory limit and where its own roles' wrapping keys are) and uses it to reach the control plane in the metadata store, which holds every setting as a record at one of five scopes (installation, node, namespace, remote store, share) with one of four binding classes (live, restart, next write, bound), and secrets only as sealed records referenced by name; an operator writes records through the API or a provisioning file, validated before commit, and each component reads the settings generation every second, runs a live change within 5 s and validates again when built, refusing rather than defaulting an invalid value](img/rfc13-overview.svg)

**The words you need.**

- **control plane** — the records in the metadata store that hold every
  setting; the one source ([§2.1](#2.1%20The%20control%20plane%20is%20the%20source)).
- **bootstrap** — the few facts a host must hold to reach the control plane, and
  nothing else ([§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap)).
- **fixed** — a quantity that is not a setting, because no operator can name a
  workload its value is wrong for; a wrong one is changed in a release
  ([§4.1](#4.1%20Fixed%20by%20default)).
- **scope** — what a setting must be the same across: the
  [installation](rfc-0-data-lifecycle.md#Glossary), a node, a namespace, a remote
  store — the store configuration its namespaces share — or a share
  ([§3](#3.%20Scopes)).
- **binding class** — what a change does: live, restart, next write, or bound
  (refused while content exists) ([§5](#5.%20Binding%20classes)).
- **secret reference** — the name of a secret sealed under the wrapping key of
  the role that uses it; configuration never holds the value ([§7](#7.%20Secrets)).
- **provisioning file** — an optional file of records, applied through the API;
  the API then refuses to edit what it declares ([§2.4](#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file)).

**What this RFC promises.**

- Each setting has one source, the control plane; a host cannot override it.
- An invalid or unknown value is refused, naming the field, when it is written
  and again when the component is built. It is never clamped or replaced by a
  default, and it stops only what it configures.
- A bound setting never changes while content depends on it, and that content
  stays readable.
- A live change reaches every node within 5 s; a node that lags is reported as
  unhealthy.
- A node joins only an installation whose format and protocol versions it
  supports, and no version is raised until every node supports it.
- No secret value appears in any record, API answer, export, log or metric.

**How the rest is organised.** §2 says where configuration lives, including the
backup location's record, §3 the scopes, §4 what is fixed and what is a setting.
§5, the binding classes and the version gate, is the core. §6 is validation and
§7 secrets. §8–§9 are metrics and tests, §10 the edits this RFC asks of others,
§11 what is still open. Appendix A lists where today's code differs, and
Appendix B every setting the RFCs name, with its scope, class and default —
the table to look a setting up in.

### Scenarios

Each runs on N1, serving the `profiles` share from the bucket `dfs-data`.

**An operator changes the `atime` policy of `profiles`.**

1. The change goes through the API, which runs the component's own validator before writing it ([§6](#6.%20Validation)).
2. The same transaction raises one installation-wide counter, the settings generation ([§5](#5.%20Binding%20classes)).
3. Every node reads that counter once a second and re-reads only the records whose generation moved ([§5](#5.%20Binding%20classes)).
4. The setting is live, so every node runs the new policy within 5 s of the commit ([§5](#5.%20Binding%20classes)).

Outcome: no restart, and each node reports which generation of the record it runs ([§2.3](#2.3%20A%20record%20is%20versioned)).

**A provisioning file with a misspelled field is reloaded.**

1. The reload validates the whole file with the API's validator before writing anything ([§2.4](#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file)).
2. The misspelled field is unknown to the schema, so it is refused rather than ignored ([§6](#6.%20Validation)).
3. The whole file is refused; nothing is half-applied ([§2.4](#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file)).
4. The running records stay as they were, and N1 reports a health condition naming the record and the reason ([§2.4](#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file)).
5. Had N1 been starting rather than reloading, it would have refused to start ([§2.4](#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file)).

Outcome: a typo never silently runs the default it was meant to replace.

**The bucket's credential is rotated.**

1. The remote store's record holds a reference to a sealed secret, never the credential itself ([§7](#7.%20Secrets)).
2. Rotating writes the new value under the same reference as a new version; the old version stays usable ([§7](#7.%20Secrets)).
3. The change raises the settings generation, so every node using it picks it up within the live bound ([§7](#7.%20Secrets)).
4. The new credential must prove it reaches the same bucket: the store's check must find the namespace's claim there ([§5.2](#5.2%20Reaching%20the%20same%20content%20another%20way%20is%20not%20a%20change)).
5. Only once every node reports the new version is the old one deleted ([§7](#7.%20Secrets)).

Outcome: no setting changes, and no node is ever left holding a credential that was already deleted.

**N1 is upgraded to a new binary, then rolled back.**

1. At start the new binary registers the format versions it can read and write ([§5.5](#5.5%20A%20node%20joins%20only%20where%20its%20versions%20overlap)).
2. It opens the store the old binary wrote and keeps writing the old active versions, the ones the installation records as current ([§5.5](#5.5%20A%20node%20joins%20only%20where%20its%20versions%20overlap)).
3. Until an operator raises a version through the control plane, reinstalling the old binary still works ([§5.5](#5.5%20A%20node%20joins%20only%20where%20its%20versions%20overlap)).
4. In a cluster, the raise is refused while any registered node lacks the new version, naming the nodes that lag ([§5.5](#5.5%20A%20node%20joins%20only%20where%20its%20versions%20overlap)).
5. Once a version is raised, the old binary refuses to start rather than misread what it finds ([§5.5](#5.5%20A%20node%20joins%20only%20where%20its%20versions%20overlap)).

Outcome: a rollback is safe up to the moment the operator raises a version, and refused cleanly after it.

## In short

- Configuration lives in the **control plane**. A host holds only what it needs
  to reach it: its identity, where the metadata store is, and its local
  devices.
- Most quantities are **fixed**, not settings. A value becomes a setting only
  when an operator can name a workload the fixed value is wrong for.
- Every setting has one **scope** — installation, node, namespace, remote store
  or share — and one **binding class**, which says what changing it does: apply
  now, apply at restart, govern the next write, or nothing, because it is bound
  to content already stored.
- A setting bound to stored content **MUST NOT** change while content exists.
  Changing it is a migration ([RFC 27](rfc-27-namespace-migration.md)), never an edit.
- An invalid setting is refused, twice: by the control plane when it is written,
  and by the component when it is built. It is never replaced by a default.
- Configuration never holds a secret. It holds a reference to one.

## 1. Purpose

The storage RFCs each name the few settings their component takes, and each
states some rule about them: refused rather than replaced ([RFC 8 §2.4](rfc-8-engine.md#2.4%20Settings%20are%20validated%20once%2C%20and%20refused%20rather%20than%20replaced)), two
settings and everything else fixed ([RFC 3 §2.10](rfc-3-syncer.md#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed)), configuration governs the
next write ([RFC 5 §5.1](rfc-5-transforms.md#5.1%20Configuration%20governs%20the%20next%20write)). This document collects those rules into one model,
gives every setting a scope and a binding class, and states what the control
plane owes the components that read it.

### 1.1 Non-goals

This document **MUST NOT** be read as specifying:

- the management API, its authentication or its resources: [RFC 23](rfc-23-control-plane.md);
- what each setting means inside its component: that stays in the component's
  RFC, which this document cites;
- how the metadata store replicates: it is a store that runs consensus
  ([RFC 16](rfc-16-metadata-store.md)); this document states only what it must hold and answer.

## 2. Where configuration lives

### 2.1 The control plane is the source

Every setting is a field of a record in the **metadata store**, held by the
control plane. Components receive records; they do not read files, and no
component has a configuration source of its own. Two sources for one setting are
two answers to one question, and the one that wins is whichever the code
happened to read last.

Settings live in the metadata store's KV ([RFC 16](rfc-16-metadata-store.md)): each setting is a
`Setting` record at its scope, beside users, shares and the file metadata, so
one database is run, backed up and replicated for all of it. The store's format
record, read before anything else at open, covers these records as it covers
every other ([RFC 16 §4.6](rfc-16-metadata-store.md#4.6%20Store%20format)).

### 2.2 A host holds only its bootstrap

A process needs a few facts before it can reach the control plane. These, and
only these, come from the host — a file or the environment:

| Bootstrap fact                                                                                                        | Why it cannot come from the control plane                                                                                                 |
| --------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------- |
| the node's identity                                                                                                   | the control plane addresses the node by it; changing it makes the node's journals new to the metadata store, so each serves only once `RecordJournal` records it again ([RFC 16 §3.2](rfc-16-metadata-store.md#3.2%20How%20it%20is%20assembled)). The initialisation step generates it once and writes it as the file `node-id` in every journal directory, after that journal's `Init`, which refuses a directory holding any entry ([RFC 1 §3](rfc-1-journal.md#3.%20Interface)), and before the store records the journal, through a temporary file that is synced and published by a hard link, which never replaces a copy. An interrupted initialisation, run again, adopts a copy it already wrote, and only from a directory whose `format` carries the installation's ID, generating the identity only when no such copy exists; it refuses, as a start does, a copy that does not parse or two that differ, and takes nothing from a directory of another installation. To reach that copy, a rerun reads the installation ID from a store that already holds its format record, skips the store's `Init` ([RFC 16 §3.2](rfc-16-metadata-store.md#3.2%20How%20it%20is%20assembled)) and each journal's `Init` where the `format` carries that ID ([RFC 1 §3](rfc-1-journal.md#3.%20Interface)), and refuses only a finished installation, one whose signing key is set, which the step's last write commits together with the bootstrap administrator ([RFC 23 §5.4](rfc-23-control-plane.md#5.4%20The%20bootstrap%20administrator%20and%20the%20first%20password)). A start reads every copy and compares them as parsed identities, in [RFC 28 §2.1](rfc-28-journal-format.md#2.1%20Layout)'s layout; a copy that does not parse — empty, truncated, or failing its checksum — refuses the start, naming its directory, and is never taken as missing or written over. It refuses, naming the directories, if two copies differ, or if no copy exists and the host's file names no identity, since a start generates nothing. The host's file may name the identity too: a start then refuses, naming the directories, if any copy differs from it. Nothing rewrites a copy, so a mistyped or cloned host file refuses the start instead of remapping its journals. A start writes a copy, logging the write, only into a directory that lacks one and whose `format` names the journal the store records at `JD‖node‖device` for this node and device. A directory whose journal is not yet recorded — a replaced device's, left empty for its re-initialisation ([RFC 28 §3.4](rfc-28-journal-format.md#3.4%20Unattachable%20files)) — gets its copy only from whoever records it, the initialisation step or a recovery import, after `Init` and beside `RecordJournal`'s keys, so `Init` still finds it empty. A `node-id.tmp` left by a crash is ignored, and the next write removes it and writes its own. A lost device loses only its copy, and a node that lost every copy starts once its host's file names its identity |
| how to reach the metadata store, and the credential for it                                                            | needed to read anything else                                                                                                              |
| the local devices the node may use, and their paths                                                                   | a path means something only on its host ([§3](#3.%20Scopes)); a journal named by another path is new to the metadata store, as for a changed identity ([RFC 16 §3.2](rfc-16-metadata-store.md#3.2%20How%20it%20is%20assembled)) |
| the scratch paths beside them: `backups.spill_path`, where copy staging spills ([RFC 3 §2.2](rfc-3-syncer.md#2.2%20The%20pool%20size%20is%20a%20memory%20bound)), and `salvage.path`, where stranded journal tags are exported ([RFC 1 §3](rfc-1-journal.md#3.%20Interface)) | a path means something only on its host, and whether it shares space with a journal only the host can tell |
| the process memory limit — the soft limit the language runtime enforces (on Go, `GOMEMLIMIT`) or the container's, else the host's memory | it sizes every node memory pool, the hold pool among them (RFC 24 §2.3, [§4.3](rfc-24-resources.md#4.3%20Held%20requests%3A%20where%20their%20bytes%20go)), read once at start in RFC 24 §2.1's order, and what memory a process has only its host knows |
| listen addresses: the adapters', the management API's and the metrics listener's | a port conflict is a host's problem. An adapter's port is not bootstrap: it is the adapter's `adapters.<protocol>.port` setting, at installation scope with a node override (Appendix B), bound on `adapters.listen` |
| the management API's TLS certificate and key (RFC 23 §5.3) | the API is served over TLS before anything is read from the control plane, and a certificate names its host |
| the management API's local socket, `api.socket`, `dfs.sock` in the runtime directory by default (RFC 23 §5.3) | a caller on it is a process of that host, which the sign-in throttle exempts from a source count |
| `events.path`, where the node spools its event streams (RFC 25 §4.6) | a path means something only on its host, and whether it shares space with a journal only the host can tell |
| log destination, format, rotation and level                                                                           | needed before the control plane answers, to say why it did not                                                                            |
| the node's **roles** — `protocol` and `storage`, and both is the only accepted value ([RFC 15 §2.1](rfc-15-topology.md#2.1%20One%20node%20type)) | kept as a flag so a later split of the roles needs no new setting; a node configured with one refuses to start ([RFC 15 §2.2](rfc-15-topology.md#2.2%20Startup%20refuses%20what%20cannot%20work)); fixed for the life of the process |
| the location of each wrapping key for `Secret` records ([§7](#7.%20Secrets)) | a key kept in the store it protects protects nothing |
| the provisioning file's path ([§2.4](#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file)) | the node applies that file at start, before the control plane could answer, and its path is on the host |

**A scratch path is checked against the journals at every start, in both
directions.** The node refuses `backups.spill_path`, `salvage.path` or
`events.path` if it
shares a device, a filesystem or free space with any of its journals — the path
checked against every journal, and every journal, one added since included,
against the path — since a scratch write that fills a journal's space answers
its writers no space ([RFC 8 §10.2](rfc-8-engine.md#10.2%20A%20capacity%20refusal%20comes%20back%20here)). Free space is shared when
the two lie in one filesystem, one pool or one thinly provisioned volume group,
not only on one device. A refused spill or salvage path does not stop the node: a refused spill
path spills nothing, and a refused salvage path fails each export as
[RFC 1 §3](rfc-1-journal.md#3.%20Interface) has it; a refused events path refuses the start, as
RFC 25 §4.6 requires. A value set for the whole
installation could not be checked this way, since one node's scratch disk can
be another's journal filesystem.

**One file, one name per fact.** The host's facts are read from one YAML file,
`$XDG_CONFIG_HOME/dittofs/config.yaml` when that is set, else
`~/.config/dittofs/config.yaml`, unless `--config`, or else `DITTOFS_CONFIG`,
names another, and from the environment, which wins over the file; a fact
neither sets takes its default. Each fact [Appendix C](#Appendix%20C%3A%20the%20bootstrap%20file)
lists has one key in the file and one environment name, and it lists them with
their types and defaults. A key is matched exactly as declared,
in the file as in a record, so `Logging.Level` is an unknown key, not `logging.level`.
A key that is not in Appendix C, and a `DITTOFS_` name that is neither a key's
environment name nor one of the names Appendix C lists as read elsewhere, refuses
the start ([§6](#6.%20Validation)).

A bootstrap fact **MUST NOT** also be a record field, and a record field
**MUST NOT** be overridable from the host. An override from the host is a
second source ([§2.1](#2.1%20The%20control%20plane%20is%20the%20source)): nodes of one installation then disagree, silently.
A provisioning file ([§2.4](#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file)) is not an override: it writes records through the
control plane, and the record then says it came from the file.

**An installation is created by an explicit initialisation step, never by a
start.** The step mints the installation ID once and passes the same ID to the
store's `Init` ([RFC 16 §3.2](rfc-16-metadata-store.md#3.2%20How%20it%20is%20assembled)), which writes it into the Installation record
beside the store format record ([RFC 16 §4.6](rfc-16-metadata-store.md#4.6%20Store%20format)), and to each journal's `Init`, which writes it into the journal's `format`
file ([RFC 1 §3](rfc-1-journal.md#3.%20Interface)); the step then records each journal's identity, node and
device in the store with `RecordJournal` ([RFC 16 §3.2](rfc-16-metadata-store.md#3.2%20How%20it%20is%20assembled)). A start opens what exists and creates neither: one that
finds journals naming an installation and no metadata store reports **metadata
store lost**, and one that finds a store whose journals are missing, or name
another installation, reports the journal missing or foreign; both wait for an
operator. One that finds a journal naming this installation that the store does
not record for this node and device, since the node's identity or the directory
changed, refuses its shares as not recorded, never as the store lost, until
`RecordJournal` records it ([RFC 8 §2.5](rfc-8-engine.md#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing), step 1). All evidence that an installation exists is otherwise inside the
store, so a store volume that mounts late would look like a first start. A
start-time option to **initialise if empty**, given on the command line and
never a key or a `DITTOFS_` name, exists for development and tests
only, and **MUST NOT** be on by default.

### 2.3 A record is versioned

Each record carries a **generation**, raised by every change, and a **schema
version**. Which nodes have applied it is [RFC 23 §2.4](rfc-23-control-plane.md#2.4%20What%20a%20node%20reports)'s, read from each
node's reconciled position and its refused and restart-pending sets
([§8](#8.%20Observability)). A reader that meets a schema version it does not know refuses
the record rather than reading the fields it recognises: a field it skipped may
be the one that mattered.

### 2.4 Records can be declared in a provisioning file

An operator **MAY** declare records in a **provisioning file**, so a server or a
test starts in a known state without a script of API calls, and the file can be
kept under version control. Its path is a bootstrap fact, which joins
[Appendix C](#Appendix%20C%3A%20the%20bootstrap%20file) with this file's first reader. The file declares
records by their API names — settings at any scope, remote stores, namespaces,
shares, users, groups, grants, quotas, snapshot policies — in the same shape
the management API accepts.

**Applying is a merge through the API.** At start, and on every reload, the node
validates the whole file with the API's validator ([§6](#6.%20Validation)) and then writes
each declared record as the API would: created if absent, replaced if it
differs. Records the file does not declare are left alone. A file that fails
validation, or whose change the API would refuse — a bound setting with content
behind it ([§5.1](#5.1%20A%20bound%20setting%20refuses%20change)), for instance — is refused whole: at start the node refuses to
start, and on reload the running records stay as they were and the node reports
a health condition naming the record and the reason. Nothing is half-applied.
Three cases are reported and the rest applied (table below): a refused prune,
because a deletion's refusal can last as long as a snapshot's lock; a declared
record the control plane retired, such as a namespace and its shares a move
released; and a create-only field whose value differs, such as a re-homed
share's namespace. Each is a state the API legitimately reached, so refusing
the whole file over it would stop a start or a reload that has nothing wrong.

**A declared record has one source, the file.** A file declares a **source
name**, stable across edits, and a **revision**, a number its author raises with
every edit. Each record the file writes is marked **managed**, with the source
name, the revision and the content hash that wrote it. Ownership follows the
source name, never the hash: a hash changes with every edit, and a record owned
by a hash would refuse the next version of its own file. The API **MUST** refuse
to change or delete a managed record, answering that it is declared by that
source and must be changed there and reloaded. This keeps §2.1's rule: every
record has exactly one source, and an operator can see which.

**The refusal covers what the file declares, not what the system records.** A
managed record's fields are of two kinds. **Declared** fields are the ones the
file states; the refusal above guards them. **System** fields are written by the
control plane as a consequence of an operation, never by the file: a backup
location's put-integrity outcome ([§2.5](#2.5%20A%20backup%20location%20is%20its%20own%20record)), a share's namespace generations
and state during a re-home or a move ([RFC 27 §2](rfc-27-namespace-migration.md#2.%20Moving%20a%20namespace%20between%20installations)), a record's generation.
The control plane **MUST** write a system field of a managed record as it would
of any other, and a file that declares one is refused at validation. A
declared field that an operation the API allows must change — a re-home rebinds
a provisioned share to a new namespace — is **create-only**: the file's value
applies when the record is created, and afterwards a file whose value differs
is not applied but reported as a health condition naming the field and both
values, until its author updates the file. So a provisioned share can be
re-homed, and a managed location's check outcome can be recorded, without
giving the API a way to edit what the file owns.

| Event | Effect |
| --- | --- |
| a declared record differs from the store | the file's value is written |
| the control plane deleted a managed record by an operation the API allows — a move's release of a namespace and its shares to another installation, a completed share removal | the deletion records a **retired** marker with the source name, and a reload does not re-create the record: it reports a health condition naming the record and the operation until the file stops declaring it. Re-created, a namespace a move released would be claimed again on a prefix the new owner holds |
| a record is removed from the file | it stays, and stops being managed; with `prune: true` in the file it is deleted instead, subject to the same refusals as an API delete (`DeleteShare` for a share, with the snapshot disposition the file's `prune_snapshots` names — `refuse`, the default, `delete` or `detach` — [RFC 16 §2.3.2](rfc-16-metadata-store.md#2.3.2%20Removing%20a%20share)). A share already `being removed` or `retired` counts as deleted, so a restart during its removal applies nothing. A refused prune never refuses a start or a reload: the record stays managed and the node reports a health condition naming it and the refusal, since the file is the only source that may delete a managed record |
| an API call edits a declared field of a managed record | refused, naming the file |
| an operation writes a system field of a managed record, or changes a create-only field through an allowed operation | written; for a create-only field the file's differing value is reported, not applied |
| a file declares a record managed by another source name | refused: the record changes source only after the first source stops declaring it; health condition on the refusing node |
| a node applies its source at a revision below the one recorded | refused as stale, so a node restarted with an older copy never reverts a newer edit; health condition naming both revisions |
| a node applies its source at the recorded revision with a different hash | refused: one revision is one content; health condition |
| reload | an explicit reload request through the control-plane API, a signal, or at start; the node reports the applied source, revision and hash ([§8](#8.%20Observability)) |

**Secrets stay references.** A provisioning file **MUST NOT** hold a secret value.
It names one by reference — an environment variable or a file on the host — and
applying seals the value into a `Secret` record ([§7](#7.%20Secrets)) as the API would.

**Node-scoped records** a file declares apply only to the node that reads it.
Every other scope is installation-wide, so in a cluster one node, or an
identical file on every node, provisions it.

> ponytail: a node applies its file only at start and on an explicit reload; it
> does not watch the file. Add a watch when operators edit files in place often
> enough that a forgotten reload shows up as drift.

### 2.5 A backup location is its own record

A backup location ([RFC 26 §2.4](rfc-26-catalog-backups.md#2.4%20Copying%20backups)) is an installation-scoped record,
not a field of a snapshot policy, so its mode, credential and lifecycle are
validated once and shared by every policy that names it. It holds:

| Field | Meaning | Class |
| --- | --- | --- |
| store | endpoint, bucket and prefix, as a remote store's | bound while any backup is held there |
| credential reference | a `Secret` ([§7](#7.%20Secrets)), sealed under the `storage` key | live, under [§5.2](#5.2%20Reaching%20the%20same%20content%20another%20way%20is%20not%20a%20change) |
| inspection credential reference | for `immutable`, optional: a `Secret` of the bucket owner's that may only read the bucket's settings, used where the location's credential cannot read them. The open **MUST** find a bucket-policy deny naming its principal for every action but the reads of the bucket's settings — in its own form: the named principal, a resource covering what its actions act on, and no condition ([RFC 4 §4.14](rfc-4-remote-tier.md#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)) — since an explicit deny binds even the owner's own principals and the credential can read it; a credential its policy would let extend retention, or change the policy, is refused ([RFC 4 §4.14](rfc-4-remote-tier.md#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)) | live, under [§5.2](#5.2%20Reaching%20the%20same%20content%20another%20way%20is%20not%20a%20change) |
| **mode** | `mutable` or `immutable`; required, no default | bound while any backup is held there |
| lifecycle age | for `immutable` only: the age at which the service expires an object, as its expiry rule is configured; the current-version expiry rule's age is at least this ([RFC 4 §4.14](rfc-4-remote-tier.md#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)) | live, within the checks below; required for `immutable` |
| retention cap | for `immutable`, the longest retention a put may set, enforced by the bucket's policy and verified at every open: at least the longest retention of any policy writing there, plus one generation, `backups.max_copy_time` and one day, in whole days ([RFC 26 §2.4.4](rfc-26-catalog-backups.md#2.4.4%20Expiry%20and%20the%20sweep)) | live, within the checks below; required for `immutable` |
| generation `G` | for `immutable`, the period every retain-until is extended past its need, so a reused version is extended about once per `G` ([RFC 4 §4.14](rfc-4-remote-tier.md#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)) | live; proposed: 7 days ([Appendix B](#Appendix%20B%20%E2%80%94%20the%20settings)) |
| put-integrity outcome | for `immutable`, the result of the capability check's integrity step, run once when the record is created and whenever its store or credential changes, since check objects there cannot be deleted | system field ([§2.4](#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file)): written by the control plane, never configured |
| break-glass principal | for `immutable`, optional: the one principal the bucket policy may exempt from the cap and legal-hold denies. The open checks the exemption and both denies by effect, in the one form whose effect it can read off the policy, and the record's creation provokes an assumption of this principal with both credentials and a legal hold with the location's, each to be refused ([RFC 4 §4.14](rfc-4-remote-tier.md#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)); the control plane **MUST** refuse a record whose break-glass principal is the location's credential or the inspection credential, since a stolen put credential that may place legal holds can lock every version for good ([RFC 4 §4.14](rfc-4-remote-tier.md#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)). Absent, no principal is exempt, and the open requires both denies to bind every principal | live, within the checks below; default: absent |
| health object identity | the location-level health object at `<location>control/health`, put when the record is created, before any namespace copies there: its version for `immutable`, the nonce written in it for `mutable` ([RFC 4 §4.7](rfc-4-remote-tier.md#4.7%20Health%20is%20one%20probe%20call)). Creating the record reads the object first, through a get that returns its version, and adopts the version or nonce already there — one object serves every installation that configures the location, so a second installation, a move's target or a recovery import, never overwrites the first's nonce. Every folder's get probe reads it, and a changed endpoint or credential proves the same location by it ([§5.2](#5.2%20Reaching%20the%20same%20content%20another%20way%20is%20not%20a%20change)) | system field, written with the record; when the recorded version is missing at an open, a new one is put and recorded, but only under the endpoint and credential that last proved the identity, so a rotation is still proven and a credential proven since may re-put it |

**An immutable location is proven, not trusted.** The control plane **MUST**
refuse an `immutable` record, and the store **MUST** refuse to open it, unless
its open-time probe finds versioning on, compliance-mode object lock with **no**
default retention — every put sets its own retain-until, and a default long
enough for the longest policy would lock every hourly block as long — a bucket
policy capping the retention a put may set at the record's retention cap and
denying the credential every change to the bucket's policy, lock, lifecycle and
versioning, every principal but the record's break-glass principal, if it
names one, denied a retention beyond the cap and any legal hold, a noncurrent-version and a current-version expiry rule — the latter
no younger than the lifecycle age — over the location's whole root, no
transition to an archive class, and a delete of the health object, naming no
version, refused for its credential; it issues no put as a probe
([RFC 4 §4.14](rfc-4-remote-tier.md#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)). A
`mutable` location takes the ordinary capability check
([RFC 4 §4.11](rfc-4-remote-tier.md#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens)).

**Ages are checked against each other.** For an `immutable` location, a policy
that writes there is refused when its `retain` plus `backups.max_copy_time`
exceeds the lifecycle age — `retain` starts when a copy completes, so a block
put at the start of a long copy must outlive the copy and the retention — and
when its `retain` plus one generation, `backups.max_copy_time` and one day
exceeds the retention cap; a `mutable` location has neither field. At any
location a policy is refused with `ErrPolicyPeriod` when its period is shorter than its estimated incremental
copy at the configured copy rate ([RFC 26 §2.4.4](rfc-26-catalog-backups.md#2.4.4%20Expiry%20and%20the%20sweep)). A first copy is not held to
the period: it completes across attempts, each building on the last.

**A location in use is not deleted.** Deleting a location record **MUST** be
refused with `ErrLocation`, naming the backups, while any backup there is
neither `expired` nor `lost`: a master key's destroy check reads only the locations the
installation has a record for ([RFC 26 §2](rfc-26-catalog-backups.md#2.%20Catalog%20backups)), so a deleted record would
hide the backups that check protects. Nor while the location is pending in
an export key's rotation: deletion first makes the location's `keys` put and
reads it back, or, if it cannot, refuses, so a retiring key is never left
waiting on a location no re-seal can reach
([RFC 5 Appendix B.3](rfc-5-transforms.md#B.3%20Rotation)); a lost location
leaves every pending set with its lost mark.

**A location lost for good is declared, not deleted.** A bucket that is gone
can neither be read nor have its backups expired, so the refusal above would
hold its record, and every master key its exports name, forever. An operator
**MAY** state that a location is lost, through one audited control-plane call
naming the location. Its first transaction writes a durable **lost mark** on
the location record; from that commit a master key's destroy check skips the
location ([RFC 26 §2](rfc-26-catalog-backups.md#2.%20Catalog%20backups)). Then, in batches within the
transaction key bound, resumed from the mark after a crash, it marks every
backup there `lost`, deletes their use records, and empties the census and the
block count of every folder record at the location, so the namespaces that
copied there can drain and their material can retire
([RFC 9 §7.6](rfc-9-gc.md#7.6%20A%20namespace%20no%20share%20names%20is%20drained)): a lost bucket's folder records could
never be settled by a sweep, which must read the bucket. A location of 10⁴
shares' retained backups holds far more records than one transaction may
write, so the work is batched, not one transaction. Once the last batch has
committed, `ErrLocation` no longer refuses deleting the record, since every
backup there is `lost`. The statement is the operator's, not a
probe's: an outage, however long, never makes one, since a location that
answers again would hold backups the store had stopped protecting.

## 3. Scopes

A setting has exactly one scope, the smallest thing it must be the same across:

| Scope | Holds | Examples |
| --- | --- | --- |
| **installation** | what every node and namespace shares | the syncer's pool sizes' defaults, GC's schedule |
| **node** | what describes one host's resources | its journals, one per device, and each one's maximum footprint ([RFC 1 §6](rfc-1-journal.md#6.%20Capacity)) |
| **namespace** | what decides stored bytes and their identity, and where they sit | prefix, chunk-ID key, chunking key, data keys, `Target`, whether the chain encrypts, the counting domain |
| **remote store** | the store configuration every namespace created on it shares: how blocks reach and sit on a service ([RFC 4 §4.2](rfc-4-remote-tier.md#4.2%20Names%20in%2C%20locations%20kept%20inside)) | endpoint, bucket, credential reference, storage class, transform chain, block target, material provider, master keys |
| **share** | what a client sees, and what orders its names | case sensitivity, entry-digest key, name length, `atime`, per-share journal limit, snapshot policy, export policy |

![The five scopes nested inside one installation: node N1 holds its journals and their footprints; a remote store configuration holds endpoint, bucket dfs-data, credential reference, storage class, transform chain, block target, material provider and master keys, and is shared by several namespaces, each opening its own store under its own prefix with its own prefix, chunk-ID key, chunking key, data keys, Target, encryption choice and counting domain; shares such as profiles hang off a namespace and hold what a client sees; bootstrap facts, fixed and derived values have no scope; what decides a chunk's boundaries or identity is a namespace setting because deduplication compares chunks across a namespace](img/rfc13-scopes.svg)

**A setting that decides stored bytes belongs to the scope content is compared
across.** Deduplication compares chunks across a namespace ([RFC 2 §4.3](rfc-2-carver.md#4.3%20Key%20scope)), so
whatever decides a chunk's boundaries or its identity is a namespace setting. A
share-level `Target` would let two shares of one namespace cut the same file
differently and stop deduplicating each other without saying so.

**A namespace has one store configuration, which many namespaces may share.**
Each namespace opens its own per-namespace store under its own prefix from that
configuration ([RFC 4 §4.2](rfc-4-remote-tier.md#4.2%20Names%20in%2C%20locations%20kept%20inside)), so the prefix is a namespace setting and
everything that reaches the service is the configuration's. The transform chain
is a configuration setting ([RFC 5 §3.2](rfc-5-transforms.md#3.2%20Configuration)), and so the same for every share of
every namespace on it. Whether the
chain encrypts is the one part that is bound and recorded with the namespace at
its creation ([§5.1](#5.1%20A%20bound%20setting%20refuses%20change)); the rest of the chain is next write.

**How a scope is written.** Every setting has exactly one of the five scopes
above. [Appendix B](#Appendix%20B%20%E2%80%94%20the%20settings) writes it in one of three forms, and no other:

- *scope* — one value across that scope;
- *scope*, **per** *thing* — one value for each instance of a thing inside that
  scope, such as `node, per journal`; the thing is never itself a scope. Each
  instance's value is its own record. A name that already carries the thing is
  the key as written: `adapters.<protocol>.<field>` carries the adapter, and
  its `<protocol>` is the thing's ID. Any other name takes the thing's ID after
  its first part: `journal.spare_target` for the journal `<id>` is the key
  `journal.<id>.spare_target`. Either way a bad value refuses that one instance
  and leaves the others readable;
- *scope* **default**, *narrower scope* **override** — a value at the wider
  scope that one narrower record **MAY** replace, such as a replica count with
  an installation default and a per-shard value in the shard record. The two are
  one setting, validated by one rule. A shard is not a sixth scope: it is a
  part of one share ([RFC 11](rfc-11-ownership.md)), so a shard override is a refinement of the share
  scope, the narrowest record a value may be written in.

A quantity that is **fixed** or **derived** ([§4.1](#4.1%20Fixed%20by%20default)), or a **bootstrap** fact
([§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap)), has no scope, and Appendix B says so in its class column instead.

## 4. Settings and fixed values

### 4.1 Fixed by default

A quantity is **fixed** unless an operator can name a workload the fixed value
is wrong for, generalising [RFC 3 §2.10](rfc-3-syncer.md#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed). A setting is a promise that every
value in its range works, a range to test, and one more way to misconfigure a
deployment. A fixed value that turns out wrong is changed in a release.

Between the two sit **derived** values, computed from settings or from the
host (the carver's bounds from `Target`, the connection pool from its callers),
and **measured** values, set once by a tool ([RFC 3 §2.11](rfc-3-syncer.md#2.11%20Pool%20sizes%20are%20measured%20once%2C%20by%20a%20tool)). Neither is
configured.

### 4.2 Every setting has a default, or is required

A setting either has a default stated in its RFC or is **required**: a record
that omits it is refused. There is no third kind — a setting with no default
that the code fills in is a default nobody reviewed.

Required, because no default is safe:

- a remote store's endpoint, bucket and credential reference; a namespace's
  prefix;
- whether a namespace's chain encrypts;
- a backup location's mode, and for an `immutable` one its lifecycle age and
  retention cap ([§2.5](#2.5%20A%20backup%20location%20is%20its%20own%20record)); a `mutable` location takes neither, since no
  bucket rule there is checked against them;
- a node's journal devices;
- every store configuration's material provider and master key, whether or not
  its chain encrypts, since every namespace's export key is wrapped under a
  master key ([RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys)); and, for a master key held in a key file,
  its verified off-host escrow: until the provider's escrow check has passed,
  creating an encrypting namespace or a backup policy on that configuration is
  refused with `ErrNotEscrowed`, naming the key.

Everything else takes a default. [Appendix B](#Appendix%20B%20%E2%80%94%20the%20settings) lists the settings the RFCs
name, each with its default. A default marked **proposed** there is a starting
value awaiting a measurement, whether its owning RFC or this document supplies
it; every other default is settled.

## 5. Binding classes

What changing a setting does is a property of the setting, declared with it:

| Class | A change takes effect | Examples |
| --- | --- | --- |
| **live** | on the next operation that reads it, on every node | snapshot schedule, GC interval, `atime` policy |
| **restart** | when the process next starts | pool sizes ([RFC 3 §2.11](rfc-3-syncer.md#2.11%20Pool%20sizes%20are%20measured%20once%2C%20by%20a%20tool)), listen addresses; an adapter's port rebuilds that adapter instead ([RFC 23 §2.3](rfc-23-control-plane.md#2.3%20The%20node%20reconciles%3B%20it%20does%20not%20take%20orders)) |
| **next write** | for content written from then on; stored content keeps what it was written with and stays readable | the compression stage and its settings ([RFC 5 §5.1](rfc-5-transforms.md#5.1%20Configuration%20governs%20the%20next%20write)), storage class, block target |
| **bound** | never, once content exists | chunk-ID key, chunking key, `Target`, whether the chain encrypts, bucket, prefix, case sensitivity and entry-digest key, a backup location's mode |

![The four binding classes side by side: live changes reach every node within 5 s through the settings generation read every second; restart changes apply when the process starts, and values nodes must agree on at every instant are restart, held old and new during a cluster rolling restart; next write changes govern only new content, recorded with it through the chain ID in each block name and the census, so old blocks stay readable; bound settings such as bucket, prefix, chunk-ID key, Target and whether the chain encrypts are recorded at creation, compared at every open, and refused while content exists, a change being a migration; a new endpoint or rotated credential for the same bucket is not a change but must be proven by finding the namespace claim](img/rfc13-binding-classes.svg)

**A live change reaches every node within 5 s.** Every change to a setting or
`Secret` record raises one installation-wide **settings generation**, in the
same transaction. Each node reads that one record every second — one point read,
which any backend answers without a watch ([RFC 16 §4.1](rfc-16-metadata-store.md#4.1%20One%20small%20interface%20per%20backend)) — and re-reads
the records whose generation moved. A node **MUST** run a live change within 5 s
of its commit; the one-second read leaves the rest of the bound to a slow store. While a change spreads, nodes **MAY** run
different generations of one record; each reports the generation it runs
([§8](#8.%20Observability)). A setting whose nodes **MUST** agree at every instant — a node
lease, the drift bound — is therefore not live: it belongs to the **restart**
class, where the change is taken up by a planned restart of every node.

**(cluster)** **A restart-class value nodes must agree on changes in two
steps.** During a rolling restart some nodes run the old value and some the new,
so the record holds both until every node reports the new generation. Each node
restarted meanwhile runs under the safer of the two in each direction: it waits
out the longer of the two timeouts it waits on another node for, and promises
the shorter of the two intervals it renews within, so a lease one node grants
under the old value never outlasts what another fences under the new. Once every
node reports the new generation, the control plane drops the old value, and the
next restart of each node runs the new one alone.

### 5.1 A bound setting refuses change

A **bound** setting decides where stored content is or what its identity is, so
changing it in place does not change the content — it makes the content
unreachable, or makes new content fail to match old. Repointing a store's prefix
loses every block, because a block's location is derived from its name and the
store's configuration ([RFC 4 §4.2](rfc-4-remote-tier.md#4.2%20Names%20in%2C%20locations%20kept%20inside)); changing `Target` re-cuts every file and
deduplicates nothing against what is stored ([RFC 2 §3.6](rfc-2-carver.md#3.6%20Changing%20any%20of%20this%20is%20a%20migration)).

So the control plane **MUST** refuse a change to a bound setting while the
namespace, store or share it is scoped to holds content, and a share's binding to its namespace
**MUST NOT** change while the share holds content, except by a re-home
([RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)), the one sanctioned change of that binding: it copies the
share's content into a new namespace while the share serves, and rebinds it only
once no ref names the old one. What an operator wants from such a change is a
migration — a re-home into a new namespace, or a move between installations
([RFC 27 §2](rfc-27-namespace-migration.md#2.%20Moving%20a%20namespace%20between%20installations)) — which leaves the old content where it can still be read
until the move completes.

### 5.2 Reaching the same content another way is not a change

Some fields name how to reach content, not where it is: a store's endpoint, and
its credential. A new endpoint for the same bucket, or a rotated credential, is
allowed, and is proven rather than trusted: the store's capability check
([RFC 4 §4.11](rfc-4-remote-tier.md#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens)) **MUST** find, at the new location, what identifies the old one.
For a namespace's store that is the namespace claim the old one held
([RFC 27 §2.1](rfc-27-namespace-migration.md#2.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim)). A backup location holds no claim, so for it that is its health
object's identity in its record ([§2.5](#2.5%20A%20backup%20location%20is%20its%20own%20record)): the recorded version, read by
version, at an immutable location, or the recorded nonce in the object's body at
a mutable one. A location that answers but holds another claim or identity, or
none, is a different store, and the change is refused.

### 5.3 What content was written with is recorded with it

A **next write** setting is recorded with the content it governed, where a later
reader or a census can find it: the chain ID in every block name, the census in
every block record ([RFC 5 §5.3](rfc-5-transforms.md#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census)). A **bound** setting is recorded once, with
the namespace, when it is created, and compared with configuration at every
open: a namespace whose recorded `Target` differs from its record
does not open ([RFC 8 §2.4](rfc-8-engine.md#2.4%20Settings%20are%20validated%20once%2C%20and%20refused%20rather%20than%20replaced)); so does one whose chunk-ID key fingerprint, or
whether it encrypts, differs from what was recorded at its creation. Reading never consults configuration for how
content was written ([RFC 5 §2.5](rfc-5-transforms.md#2.5%20Reading%20needs%20no%20configuration%2C%20only%20material)).

### 5.4 A format version is written only once every reader reads it

The block format version a namespace writes ([RFC 4 §3.5](rfc-4-remote-tier.md#3.5%20Format%20changes%20are%20migrations)) is a **namespace
setting**, next write, defaulting to the newest version every node of the
installation reads. The control plane **MUST** refuse to advance it past the
oldest version any node that may read the namespace supports, and a node
**MUST** refuse to join while a namespace it would serve writes a version it
cannot read. A newer binary alone therefore never writes blocks an older one,
still serving, would refuse.

### 5.5 A node joins only where its versions overlap

Every format and protocol a node shares with others is versioned: the metadata
store format ([RFC 16 §4.6](rfc-16-metadata-store.md#4.6%20Store%20format)), the journal format ([RFC 28 §2](rfc-28-journal-format.md#2.%20On-disk%20format)), the block
format ([§5.4](#5.4%20A%20format%20version%20is%20written%20only%20once%20every%20reader%20reads%20it)), the settings record schema ([§2.3](#2.3%20A%20record%20is%20versioned)), and **(cluster)** the
messages nodes exchange. For each, the installation records one **active
version**, the one written, and each node registers the range it can read and
write, at every start, in its node record.

- **A node refuses to start** in an installation, and a binary refuses to open a
  store, when any active version lies outside the range it registers. It names
  the format and both versions.
- **An active version is raised only when every registered node reports the new
  version in its range**, and the control plane **MUST** refuse the raise
  otherwise, naming the nodes that lag. A node that is down still counts, since
  it may start again with its old binary; one that will not return is
  **decommissioned** by the operator, which deletes its node record, and no
  longer counts. The raise is one transaction, on the installation record or,
  for the store format, on the store format record `\x00format`, its only record
  ([RFC 16 §4.6](rfc-16-metadata-store.md#4.6%20Store%20format)); a node learns it through the settings generation ([§5](#5.%20Binding%20classes)).
- **A downgrade is possible only while every active version lies in the older
  binary's range**; once a version has been raised past it, the older binary
  refuses to start rather than misread what it finds.

So a rolling upgrade is: upgrade every node's binary, each still writing the old
active versions; then raise each active version, which the control plane allows
only once the last node runs the new binary. On a single node the same gate runs
at start: a new binary opens a store written by the old one and keeps writing
the old active versions; it raises one only through the same control-plane
call, never at open. Until that call the old binary can still be started on the
same store, so a single node rolls back by reinstalling it.

## 6. Validation

**Refused, never replaced** ([RFC 0 §1.2](rfc-0-data-lifecycle.md#1.2%20Component%20autonomy)). A value out of range, of the wrong
type, or naming something that does not exist **MUST** be refused with an error
naming the field and the rule. A component **MUST NOT** clamp it, round it, or
fall back to a default: the operator asked for something, and running something
else is a lie the operator finds out about from a failure.

**Checked twice.** The control plane validates a record when it is written,
so an operator sees the error at the moment of the mistake. The component
validates again when it is built ([RFC 8 §2.4](rfc-8-engine.md#2.4%20Settings%20are%20validated%20once%2C%20and%20refused%20rather%20than%20replaced)), because a record can predate the
rule, or come from a newer control plane. Both run the same rules, owned by the
component: the control plane calls the component's validator, and has none of
its own to drift.

**A rule across settings is checked from both sides.** Some rules relate two
settings, such as `adapters.smb.pending_timeout` exceeding
`migration.freeze_timeout` plus the break deadline
([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)). When the two belong to
different components, as there, the control plane runs the check: neither
component's validator sees the other's settings. A write of
either setting **MUST** be validated against the current value of every setting
a rule relates it to, in the same transaction as the write, reading those
values tracked so that a concurrent write of the other side conflicts. A rule
stated in an RFC names its settings in [Appendix B](#Appendix%20B%20%E2%80%94%20the%20settings),
so the validator knows which writes re-check it. Another such rule: on each
node, the effective `adapters.nfs.port`, `adapters.smb.port` and
`sidecars.portmap.port` — the node's override, else the installation's value —
differ unless 0, so a write of any of them at either scope is checked against the
others on every node it reaches; a clash with the API's or the metrics
listener's port, which are the host's bootstrap facts, refuses that adapter on
that node, as a port conflict does (Appendix B), or fails the port mapper alone
([RFC 20 §3.5](rfc-20-adapters.md#3.5%20Sidecars)).

**A write check may read store state beyond settings.** A section's check
**MAY** read a record that is not a setting, as a tracked read in the write's
transaction, so a concurrent change of that record conflicts with the write.
One rule does: a write of `identity.id_range` reads the uid and gid counters,
which only rise, and refuses a range no further ID could be drawn from
([RFC 18 §2.6](rfc-18-identity.md#2.6%20Components%2C%20wiring%20and%20lifecycle),
Appendix B). Such a rule is checked at the write only: the component does not
check it again when it applies the value, since what the rule read is no part
of the setting, and a second answer would leave two readers of the setting
holding different values.

**Unknown fields are refused.** A field the schema does not know is most often a
misspelling of one it does, and ignoring it silently runs the default.

**A record that does not validate stops only what it configures.** A share whose
record is invalid does not start; the rest of the installation does.

**A refusal says where, what and why, in one shape.** Every refusal of
configuration, of the host's file, of the environment or of a record, reads:

```text
<where>: <key>: <rule>[; <what to do>]
```

*Where* is the file's path, `environment`, or `setting` with the scope the
record is written at. *Key* is the key as declared, or the environment name.
*Rule* is the rule broken, in the component's words. *What to do* names the
replacement of a key that moved, the closest declared key to an unknown one, or
the way to change a value that is not the host's. All the refusals of one file,
one environment or one write are reported together, so one pass fixes them; a
rule that relates two keys runs only over keys that passed their own, so a bad
`metadata.backend` does not also refuse the keys of a block it names. A refusal
**MUST NOT** print the value of an environment variable or of a key that
holds a credential, since either may hold a secret ([§7](#7.%20Secrets)), nor the
value of a key it does not declare or no longer reads, since those are the keys
most likely to be refused and where today's files hold secrets
(`controlplane.jwt.secret`, `admin.password_hash`), as would a misspelled
`pasword:`; it prints any other value it refuses, which a setting may print
since it never holds a secret. A decoder's error is never printed as it comes
either: a start, a reload or `dfs config show` rewords it into this shape, since
a decoder puts the value it could not read into its own text.

```text
/etc/dittofs/config.yaml: database.type: no longer read; the metadata store is metadata.backend
/etc/dittofs/config.yaml: loging.level: unknown key; did you mean logging.level?
/etc/dittofs/config.yaml: adapters.smb.port: a setting, not a host fact; set it through the management API
environment: DITTOFS_CONTROLPLANE_SECRET: no longer read; remove it
setting (node 3f2a…): adapters.smb.port: must be between 1 and 65535, got 70000
setting (namespace ns-1): prefix: bound to the namespace's content, refused while it holds any; moving it is a migration (RFC 27)
```

## 7. Secrets

Configuration **MUST NOT** hold a secret value: not a credential, not a key, not
a passphrase. It holds a **reference** — a secret's name in a secret provider —
and the component resolves it when it is built.

The default secret provider is the metadata store itself: a `Secret`
record ([RFC 16](rfc-16-metadata-store.md)) in the same KV, its value **envelope-encrypted** under a
wrapping key held outside that KV — a host file or an external key service,
named by the bootstrap ([§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap)). A copy of the store alone reveals no
secret.

**Two wrapping keys, one per role.** Each secret kind is sealed under the
wrapping key of the role that uses it, and every node runs both roles and names
both keys in its bootstrap:

| Wrapping key | Seals | Needed by |
| --- | --- | --- |
| `protocol` | NT hashes, keytabs, identity-provider bind credentials, the machine-account password NTLM pass-through signs in with | authentication |
| `storage` | password hashes, which only the management API's sign-in verifies (RFC 23 §5.3); remote-tier credentials, references to master keys, the management API's token-signing key (RFC 23 §5.3), OpenID Connect client secrets (RFC 23 §5.5) | the remote tier, the material provider and the management API |

A deployment **MAY** split a key further by secret kind.

**A node credential is not a secret of the store.** Its private half is issued
to the node and kept on its own volume with its identity
([RFC 30 §4.2](rfc-30-kubernetes.md#4.2%20A%20pod's%20identity%20is%20its%20volume)),
and never enters the KV, which any node can read
([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)).
Sealed under a key every node holds, it would let any node unseal every other
node's credential and speak on the node channel in its name, which
[RFC 15 §4.4](rfc-15-topology.md#4.4%20Node%20channels)'s bound-to-the-sender rule
forbids. What a peer verifies a node credential with, and who may write it, is
open ([§11](#11.%20Open%20questions), question 4): no record of the store holds
it today, and none may be a row a node writes itself.

> decision: the two keys stay separate although every node holds both, so a
> later split of the roles ([RFC 15 §2.1](rfc-15-topology.md#2.1%20One%20node%20type))
> re-seals nothing and a disk-less protocol node is handed the `protocol` key
> alone. Overturned if that split is ruled out; the keys then merge.

**One interface, two providers.** Every secret is resolved through one
interface. The built-in provider above — sealed `Secret` records under the role
wrapping keys — ships now; an external key-management provider comes later,
behind the same interface and the same references, so adding it changes no
record. Signatures are indicative; besides the kind and owner checks, the
built-in provider carries the three things a rotating holder needs — `Meta`,
`Versions` and `Prune`:

```go
// SecretProvider resolves a secret reference to its value. It never lists
// values and never returns one through any other path.
type SecretProvider interface {
	// Resolve returns the current value of ref, unsealed with the wrapping key
	// of role. It fails if this node does not hold that role's key, which
	// never happens while every node holds both, and if the record's kind or
	// owner ([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)'s `Secret`) is not the caller's, so a
	// reference cannot be pointed at another holder's secret.
	Resolve(ctx context.Context, role Role, kind SecretKind, owner SecretOwner, ref SecretRef) ([]byte, error)
	// Seal wraps value with role's wrapping key as the next version of ref and
	// returns it unwritten; a new version of an existing ref keeps its kind
	// and owner, and one naming others is refused. The caller commits it with
	// ControlPlaneTx.PutSecret(s, s.Version-1) inside its own Write, beside the
	// record naming ref and its audit record (RFC 16 §3, RFC 23 §6), so a
	// sealed version commits with its change or not at all. Wrapping runs
	// before that Write, so a rerun of its function does not wrap again; a
	// concurrent seal since fails the commit's expect.
	Seal(ctx context.Context, role Role, kind SecretKind, owner SecretOwner, ref SecretRef, value []byte, m Meta) (Secret, error)
	// Versions opens every version still held, current first, checked
	// against the caller's kind and owner as Resolve checks the current one,
	// for a holder that needs them all at once.
	Versions(ctx context.Context, role Role, kind SecretKind, owner SecretOwner, ref SecretRef) ([]Version, error)
	// Prune drops the versions below the lowest one any node reports holding
	// (§8), never the current one; the rotation's owner calls it once its
	// retention ends. It commits through tx, inside the caller's own Write
	// beside the audit record naming the versions dropped (RFC 23 §6), so a
	// prune commits with its audit or not at all. Prune and Delete check the
	// caller's kind and owner as Resolve does, so a wrong reference neither
	// destroys another holder's secret nor cuts its retention short.
	Prune(ctx context.Context, tx ControlPlaneTx, kind SecretKind, owner SecretOwner, ref SecretRef) error
	// Delete destroys every version of the secret; a later Resolve of ref
	// fails.
	Delete(ctx context.Context, kind SecretKind, owner SecretOwner, ref SecretRef) error
}

// Meta is written with a version: a machine password's KeyVersion, set once
// and never changed, and HandedOver, which Seal binds into the sealed value's
// additional data with the role, kind, owner, reference and version (RFC 16
// §2.3), so a flipped mark fails to open.
type Meta struct {
	KeyVersion uint32
	HandedOver bool
}

// Version is one opened version of a secret.
type Version struct {
	Number uint64
	Value  []byte
	Meta   Meta
}
```

Whatever provider seals, it **MUST** length-prefix each field of the additional
data `Seal` binds (see `Meta`) and each part of the owner (the record's kind and ID, the scope's tag and
ID, the key), so two owners whose parts concatenate to the same bytes bind
differently and a value moved onto the other's reference fails to open.

- **Read never returns a secret.** No API, export, log line or metric carries
  one. An export that must travel with its secrets, a backup that must be
  restorable ([RFC 27 §2.4](rfc-27-namespace-migration.md#2.4%20Key%20scope%20and%20material)), carries references, and says which secrets the
  destination must hold.
- **A secret changes without a record change, in two phases.** Rotating a
  credential writes a new value under the same reference, as a new version of
  the `Secret` record; the previous version stays resolvable. The change raises
  the settings generation ([§5](#5.%20Binding%20classes)), so every node using the reference
  resolves it again within the live bound and records the version it now holds in its node-status record (RFC 23 §2.4)
  ([§8](#8.%20Observability)). Only once every such node reports the new version does the
  provider delete the old one — except a keytab's and a machine password's,
  kept, a restart included, for [RFC 18 §3.3](rfc-18-identity.md#3.3%20Kerberos)'s retention after the change at
  the KDC, so a ticket issued under the old key still verifies, and the management API's signing key's, kept
  until the last access token it signed has expired ([RFC 23 §5.3](rfc-23-control-plane.md#5.3%20Sign-in%2C%20tokens%20and%20sessions)) — and the control plane report that the old
  credential may be revoked at its service; revoking it earlier fails the nodes
  that still hold it. The new value is proven as [§5.2](#5.2%20Reaching%20the%20same%20content%20another%20way%20is%20not%20a%20change) proves a new
  credential before it is used.
- **A user's secrets go with the user.** `DeleteUser`'s last transaction, the
  one that deletes the user record, deletes every version of each `Secret`
  that record names — its password hash and NT hash among them
  ([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)).
  Nothing else names those secrets, so a later pass could not find them, and a
  password-equivalent left behind outlives the account in the store and every
  copy of it.
- **A wrapping key rotates the same way.** A node's bootstrap names, for one
  role, the key that seals, and the retired keys that still open what they
  sealed (Appendix C's `local.file` and `local.retired_files`). To rotate, the next key
  is first listed among every node's retired keys, so each opens what it will
  seal; only then does it become the one that seals, and the old one is retired,
  so a node restarted early seals nothing another node cannot open. Re-sealing every
  `Secret` of that role under the new key is one pass, run by a node of that
  role, since it opens each; each record names the key ID it is sealed under, and
  a retired key is removed from bootstraps only once no record names it.
- **Material is a secret with a lifecycle.** Encryption keys are resolved through
  the material provider, which also tracks which keys exist, are current or are
  destroyed. A secret provider need not.
- **Which keys exist is RFC 5's table, not this document's.**
  [RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys) is the one statement of the keys each kind of namespace
  has, what wraps each, which rotate and which must live off the host. This
  document adds only where configuration names them: a remote store's
  configuration references its **master keys**, which the provider holds and
  never releases; a namespace's record holds each of its keys as (material ID,
  fingerprint), never the key; the provider refuses an ID whose fingerprint
  changed. In the terms of that table:
  - a **master key** is held where [RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys) says,
    including when a key file's off-host escrow must be verified first, which
    [§4.2](#4.2%20Every%20setting%20has%20a%20default%2C%20or%20is%20required) enforces and this list does not restate, and rotates by
    re-wrapping, which changes no record. A master key that any retained export
    names, or that a move pins, is never destroyed, and a destroy that cannot
    read a backup location is refused ([RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys)) unless an
    operator has declared that location lost ([§2.5](#2.5%20A%20backup%20location%20is%20its%20own%20record));
  - every namespace key — data, chunking, export, and an encrypting
    namespace's chunk-ID key — is wrapped only under a master key of its store
    configuration. **No namespace key is wrapped under the `storage` wrapping
    key**: that key lives on the host, so a backup wrapped under it would be
    restorable only on the host that made it, the one place a backup is not
    needed;
  - a non-encrypting namespace's **chunk-ID key** is held in the clear, in its
    key record and beside its blocks, so losing the host loses none of its
    content; its keyed IDs hide nothing, since its bodies are plaintext. Its
    exports are still sealed under its export key, which is wrapped under a
    master key like every other: **restoring any namespace's metadata from a
    backup needs the master key**, and the bucket alone does not suffice;
  - every key record reaches the namespace's `keys` object in its bucket before
    it becomes current, and a backup location's before any state, export or
    progress version is put there, so a recovery finds keys rotated after the
    last backup ([RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys));
  - **data keys and export keys rotate**: a data key by
    new material plus relocation ([RFC 5 §5.3](rfc-5-transforms.md#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census)), an export key by issuing a new
    current one, of a higher generation, that later exports are sealed with,
    the old one retired only once every backup location has been re-sealed
    under the new one ([RFC 5 Appendix B.3](rfc-5-transforms.md#B.3%20Rotation)). Each is a **next write**
    setting, not bound. The chunk-ID and chunking keys never rotate: changing
    either renames or re-cuts every chunk, which is a re-home
    ([RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)).
- **An export names its keys and is sealed under them.** It carries the
  namespace's key records as that table holds them, the IDs of the master keys
  that wrap them and of its current and every older (retiring or retired)
  export key, with the digests of the retained exports sealed under an older
  key ([RFC 5 Appendix B.3](rfc-5-transforms.md#B.3%20Rotation)), so a forgery under a
  retired key is refused, and is encrypted and authenticated under the export key ([RFC 26 §3.1](rfc-26-catalog-backups.md#3.1%20Layout)).

- The only secrets on a host are the bootstrap credential for the configuration
  store, its wrapping keys and the passphrase of a key file, or references to
  them in an external key service ([§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap)).

## 8. Observability

Metric names are shown without the deployment's prefix, which the exporter adds.

| Answers | Metric | Type |
| --- | --- | --- |
| the position a node has reconciled through per kind — `settings`, `shares`, `principals` — read against the store's current counter per kind | RFC 23's `runtime_applied_generation{kind}` | gauge |
| how long a node's reconciled position has been behind the store's counter, per kind — `settings` (`CFGGEN`), `shares` (`SL`), `principals` (`PGEN`) | `config_generation_lag_seconds{kind}` | gauge |
| secrets a node holds at an older version than the current one | `config_secrets_behind` | gauge, a count |
| each format's active version, and the lowest and highest version each node registers | `format_active_version`, `format_supported_min`, `format_supported_max` | gauges, labelled by format |
| records refused, by component and reason | `config_refused_total` | counter |
| changes refused because a setting is bound | `config_bound_refusals_total` | counter |
| the provisioning file hash each node applied, and applies refused by reason | `config_provisioning_applied`, `config_provisioning_refused_total` | gauge, counter |

A node whose reconciled position stays behind a live record's own for longer
than the 5 s propagation bound ([§5](#5.%20Binding%20classes)) is a health condition of that node,
`config_generation_behind`, which names the records behind; no metric is
labelled by record or by secret.

## 9. Test plan

Conventions and tiers are the index's ([Test tiers](rfc-index.md#Test%20tiers)).

| Requirement | Check |
| --- | --- |
| [§5](#5.%20Binding%20classes) adapter port | Change `adapters.smb.port` on a serving node. Assert the node builds a new adapter on the new port and stops the old one, the other adapters serving throughout. Change it to a port another process holds: assert the record reported refused and the adapter serving again on the last port that served. |
| [§5.1](#5.1%20A%20bound%20setting%20refuses%20change) bound | For every bound setting, write content, then change the setting through the API. Assert refused, and content still readable. Repeat with no content; assert accepted. |
| [§5.1](#5.1%20A%20bound%20setting%20refuses%20change) share binding | Rebind a share with content to another namespace. Assert refused. |
| [§5.2](#5.2%20Reaching%20the%20same%20content%20another%20way%20is%20not%20a%20change) same content | Change a store's endpoint to a second address of the same bucket; assert accepted. Change it to another bucket; assert refused, and the store still serving the old one. |
| [§5.3](#5.3%20What%20content%20was%20written%20with%20is%20recorded%20with%20it) recorded | Edit a namespace's stored `Target` underneath the record. Assert the namespace does not open. |
| [§6](#6.%20Validation) refused | For every setting, feed one value out of range and one of the wrong type, through the API and directly to the component. Assert both refuse, with the field named; assert no component runs a default in its place. |
| [§6](#6.%20Validation) one validator | Assert the API and the component return the same error for every invalid record in the fixture set. |
| [§6](#6.%20Validation) unknown field | Misspell one field of each record. Assert refused. |
| [§7](#7.%20Secrets) role keys | Seal a remote-tier credential and an NT hash. Assert each unseals under its own wrapping key and fails under the other, and that every `SecretKind` maps to exactly one key. |
| [§7](#7.%20Secrets) no secret out | Configure every secret-bearing record; read every API, export and log produced by a full test run. Assert no secret value appears. |
| [§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap) explicit initialisation | Start a node against an empty metadata store and empty journal directories: assert it refuses, creating nothing but, on a store with no directory, the node's clock ceiling. Initialise: assert the store's Installation record and every journal's `format` file carry one installation ID. Assert every journal directory holds one `node-id`, and that a start with two that differ refuses, naming both. Delete every copy with no identity in the host's file: assert the start refuses, generating none. Start with the store volume unmounted: assert it reports the metadata store lost and creates nothing; with the journal directory emptied and the identity named in the host's file, the journal missing; with the node's identity changed in its bootstrap, the start refused, naming the directories, and every copy unchanged. Truncate one copy and flip a byte of another: assert each refuses the start, naming its directory, and neither is written over. Against a committed golden vector of [RFC 28 §2.1](rfc-28-journal-format.md#2.1%20Layout)'s `node-id` layout, assert a `node-id` written by the step is its 24 bytes, and that a 23- or 25-byte copy, another magic and a bad checksum each fail to parse. Replace a recorded device with an empty directory: assert a start writes nothing into it, and that its re-initialisation's `Init` then succeeds, after which the step writes its copy. Delete every copy and name a new identity in the host's file: assert the start writes no copy, and refuses the journals' shares as not recorded, never the store lost, until a recovery import records them under the new identity and writes their copies, after which a start serves them. Only with initialise-if-empty set does a start on empty storage create an installation. |
| [§2.5](#2.5%20A%20backup%20location%20is%20its%20own%20record) location deletion | Delete a location record holding one unexpired backup: refused `ErrLocation`, naming it. Expire it; delete again: accepted. Make a second location unreachable for good with one backup there: deletion refused. State it lost, crashing after the lost mark and after the first batch: assert the statement resumes from the mark, the backup reads `lost` in the store, its folder record's census and block count are empty, the statement is in the audit log, deletion is accepted, and a master key only that backup's export named can be destroyed. Leave a third location unreachable for a day without the statement: still refused. |
| [§2.5](#2.5%20A%20backup%20location%20is%20its%20own%20record) break-glass principal | Create an `immutable` record naming a break-glass principal whose policy exempts exactly it: accepted. Exempt a second principal as well, or name the location's credential or the inspection credential as the break-glass one: refused. Omit the field with a policy that exempts anyone: refused. Let the break-glass role's trust admit the installation's account, or scope the legal-hold deny to the bucket rather than its objects, or add a condition to it: each refused. |
| [§2.5](#2.5%20A%20backup%20location%20is%20its%20own%20record) health object re-put | Rotate an `immutable` location's credential and prove it; make the health object's recorded version missing — through a fault wrapper around a real store, since no principal can delete a compliance-locked version before its retain-until; open: assert a new version is put under the rotated credential and recorded. Open under a credential never proven: refused, nothing put. |
| [§5.2](#5.2%20Reaching%20the%20same%20content%20another%20way%20is%20not%20a%20change) backup location credential | Rotate an immutable and a mutable backup location's credential: assert both accepted, proven by the health object's recorded version and nonce. Point the record at another bucket: refused. A design that looks for a namespace claim at a backup location refuses every rotation. |
| [§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap) scratch paths | Each path runs this once its first reader adds its key to [Appendix C](#Appendix%20C%3A%20the%20bootstrap%20file), since a key not listed there is refused as unknown. Start a node with `backups.spill_path` on a journal's filesystem, then on a second subvolume of the journal's pool, then on a separate filesystem and add a journal on it before the next start; assert each is refused at start, the node serves, nothing spills, and a stranded tag's export with `salvage.path` refused the same way is a failed export reported as a health condition. Set either path as a record field: refused.  Start one with `events.path` on a journal's filesystem: assert the start is refused. |
| [§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap) no host override | Set a record field in the host's environment. Assert it is refused at start, not applied. |
| [§2.3](#2.3%20A%20record%20is%20versioned) schema | Present a record with a newer schema version. Assert refused. |
| [§2.4](#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file) provisioning | Start a node with a file declaring a namespace, a share and a setting. Assert all three exist and are marked managed. Edit the share through the API; assert refused, naming the file. Change the file and reload; assert applied. Remove the share from the file; assert it stays, unmanaged; add `prune: true`; assert deleted. |
| [§2.4](#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file) all or nothing | Reload a file with one valid change and one invalid record, then one changing a bound setting with content behind it. Assert nothing changed, the health condition names the record, and at start the node refuses to start. Move a declared namespace away and reload the unchanged file with one valid change: assert the change applied and the retired record reported, not re-created and not refusing the file. |
| [§2.4](#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file) secrets | Declare a remote store whose credential is an environment variable. Assert the file holds no value, the record references a sealed `Secret`, and no API or log shows the value. |
| [§5.4](#5.4%20A%20format%20version%20is%20written%20only%20once%20every%20reader%20reads%20it) format version | Run two nodes, one reading one format version fewer. Advance the namespace's version to write; assert refused. Upgrade the second node; assert accepted, and every block either node writes afterwards reads on both. |
| [§7](#7.%20Secrets) namespace keys | Replace a namespace's chunking key under the same ID. Assert the provider refuses it by fingerprint and the namespace does not open. |
| [§7](#7.%20Secrets) chunk-ID key | Create two namespaces without encryption and store one file's bytes in each. Assert the chunk IDs differ between them and differ from the unkeyed hash of the bytes. A design that keys chunk IDs only when encrypting fails. |
| [§5.1](#5.1%20A%20bound%20setting%20refuses%20change) encryption bound | Turn encryption on for a namespace that holds content. Assert refused. Turn compression on; assert accepted, and old blocks still read. |
| [§2.4](#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file) source, not hash | Apply a file, edit it and raise its revision, reload. Assert applied — a design that owns records by hash refuses its own edit. Restart a second node with the earlier revision; assert refused as stale and no record reverted. Apply the same revision with different content; assert refused. |
| [§2.5](#2.5%20A%20backup%20location%20is%20its%20own%20record) immutable location | Point an `immutable` location at a bucket with versioning but no object lock; assert the record is refused. Add compliance-mode lock, noncurrent- and current-version expiry over the whole root, and a bucket policy holding the retention cap and denying the five configuration actions; assert accepted, that the record holds the health object's version, and that the open issued one delete naming no version, saw it refused, and issued no put. Drop the current-version rule, or one configuration deny; assert refused. Add a bucket default retention; assert refused. Remove the cap policy, or raise it past the record's cap; assert refused at open and at `Recheck`. Give a policy a `retain` plus `max_copy_time` above the lifecycle age, or a `retain` plus one generation and `max_copy_time` above the cap; assert refused. Give a 40 TiB share a daily policy at 200 MiB/s whose increment fits a day: assert accepted, though its full copy does not; shrink the period below the increment: refused `ErrPolicyPeriod`. |
| [§2.4](#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file) system fields | Provision a share and an immutable location from a file. Re-home the share: assert it runs, the share's namespace changes, and the next reload reports the file's stale namespace as a health condition without applying it. Create the location: assert its put-integrity outcome is recorded. Declare the outcome in the file: refused at validation. A design that refuses every write to a managed record fails both. |
| [§5](#5.%20Binding%20classes) rolling restart class | **(cluster)** Raise the node lease from 10 s to 20 s and restart three nodes one at a time. Assert each restarted node waits out 20 s before taking over a lease and renews within the old interval until every node reports the new generation, and that no lease was served by two nodes at any instant. Apply the new value on each node as it restarts instead: a takeover by a restarted node falls inside a lease an old node still holds. |
| [§5.5](#5.5%20A%20node%20joins%20only%20where%20its%20versions%20overlap) dead node | Stop one of three nodes for good and raise an active version: assert refused, naming it. Decommission it: assert the raise is accepted. On a single node, upgrade the binary without raising; assert the old binary still starts on the store. |
| [§7](#7.%20Secrets) key table | Create a non-encrypting namespace: assert its chunk-ID key is in the clear in its key record and its key object, and that no namespace key of any namespace is wrapped under the `storage` key. Delete the host's wrapping keys and restore its catalog backup on a fresh host holding the master key: assert every file reads back. Create an encrypting namespace on a key-file master key whose escrow is not verified: refused `ErrNotEscrowed`. Rotate a data key and an export key of a namespace holding content: assert accepted, old blocks and exports still read. Destroy a master key a retained export names: refused. |
| [§5](#5.%20Binding%20classes) propagation | Change a live setting on a store backend that offers no watch. Assert every node runs the new generation within 5 s. Freeze one node's reads; assert the health condition names it after 5 s. |
| [§5.5](#5.5%20A%20node%20joins%20only%20where%20its%20versions%20overlap) version gate | Run two nodes; register a third whose range excludes an active version; assert it refuses to start, naming the format. Upgrade one node's binary; raise an active version; assert refused, naming the other node. Upgrade it; assert the raise is accepted. Start the old binary; assert it refuses. A design without a recorded active version accepts the first raise. |
| [§7](#7.%20Secrets) user secrets | Create a user with a password, rotate it once, delete the user. Read the store's raw keys: assert no version of either secret remains, and a restart mid-`DeleteUser` still leaves none once it resumes. |
| [§7](#7.%20Secrets) rotation | Rotate a credential while two nodes use it. Assert both report the new version within 5 s, the old version stays resolvable until both do, and only then is it deleted. Rotate a wrapping key in §7's order, the next key among every node's retired keys before it seals; assert a node restarted before the others seals nothing another node cannot open, and every `Secret` names the new key ID before the old key can be removed. |
| [§3](#3.%20Scopes) scope forms | Parse Appendix B's scope column. Assert every entry is one of the five scopes in one of §3's three forms, a shard override counting as a refinement of share (§3), or "—" with a class of fixed, derived or bootstrap. |
| [§6](#6.%20Validation) block target range | Set the block target to 512 KiB and to 128 MiB. Assert both refused, naming the field and the range; assert 1 MiB and 64 MiB accepted. |

**Suites.** §9's single-node rows run in `unit`, and the rows that provoke an immutable location's
lock through RFC 4's fault transport in `integration`. No suite runs yet the rows that need two or
more nodes.

## 10. Edits this document asks of other RFCs

Each owning RFC is the authority for what its settings mean; this document is
the authority only for their scope and class, and where the two disagree the
disagreement is a defect in one of them, fixed there, not settled by
precedence. Every edit this document asked of another RFC is applied, dropped
because the rule stands here alone, or marked not yet applied:

1. **RFC 2 §3.2:** `Target` is a namespace setting, bound. *Applied.*
2. **RFC 2 §6 and RFC 5 §3.2:** "a share that encrypts" reads "a namespace that
   encrypts"; the chunking key is the namespace's. *Applied.*
3. **RFC 4 §4.2 and Appendix C:** bucket and prefix bound; endpoint and
   credential change under [§5.2](#5.2%20Reaching%20the%20same%20content%20another%20way%20is%20not%20a%20change). *Dropped:* RFC 4 states no class for them,
   so nothing there contradicts [§5.1](#5.1%20A%20bound%20setting%20refuses%20change), [§5.2](#5.2%20Reaching%20the%20same%20content%20another%20way%20is%20not%20a%20change) and Appendix B, which carry
   the rule.
4. **RFC 12 §3.2:** its example is the control-plane records it describes, with
   the scopes of Appendix B: `snapshots.hold_bound` and `snapshots.directory`,
   which Appendix B scopes per share, sit under the share. *Applied.*
5. **RFC 7 §3.3:** case sensitivity is fixed at the share's creation. *Applied.*
6. **RFC 9 §8:** GC's `Config` holds the six namespace `gc.*` settings of
   Appendix B, `gc.hold_threshold` among them. *Applied.*
7. **RFC 2 §5:** the block target is a remote-store setting, next write, 4 MiB,
   settable from 1 MiB to 64 MiB. *Applied.*
8. **RFC 5 §3.2 and §5.1:** whether the chain encrypts is fixed when the
   namespace is created. *Applied.*
9. **RFC 26 §4.2 and RFC 12 §3.2:** a policy names a backup location by its record
   ([§2.5](#2.5%20A%20backup%20location%20is%20its%20own%20record)). *Applied.*
10. **RFC 16:** the node record holds its registered version ranges; the store
    format's active version is in the store's format record and every other
    active version in the installation record; a `Secret` holds its version and
    the wrapping key it is sealed under, and keytabs are `Secret` records.
    *Applied* ([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities), [§4.6](rfc-16-metadata-store.md#4.6%20Store%20format)).
11. **RFC 16:** the installation-wide settings generation of [§5](#5.%20Binding%20classes) is
    RFC 16's `CFGGEN` record, raised in the transaction of every `Setting` or
    `Secret` change and every write of `Installation.Join`. *Applied.*

## 11. Open questions

1. **Proposed defaults.** Every default Appendix B marks proposed is a
   starting value awaiting a measurement, and each row names the measurement
   that settles it, or carries a decision that none will. Each stands until that
   measurement, wherever it runs, replaces it with the value it selects, under
   the index's rule for setting defaults ([index, Test tiers](rfc-index.md#Test%20tiers)).
2. **Values open in their own RFCs.** The segment size, a fixed constant
   ([RFC 28 §5](rfc-28-journal-format.md#5.%20Open%20questions), question 2), the GC partition count
   ([RFC 9 §12](rfc-9-gc.md#12.%20Open%20questions), item 6); and
   **(cluster)** the gather interval, replica removal triggers, mark
   persistence, re-read period and repair pacing ([RFC 10 §16](rfc-10-journal-replication.md#16.%20Open%20questions)). Each takes a
   default here only once its RFC states one.
3. **The settings generation read.** One point read per node per second is
   nothing on one node; whether it stays cheap with many nodes on one replicated
   store, or needs the store's own change feed, is unmeasured **(cluster)**.
4. **The node credential's verifier.** What a peer verifies a node credential
   with (the issued certificate or its public key), where that is kept, who may
   write it and how a revocation reaches the peers are not written down: RFC 16's
   `Node` holds no such field, §4.2 has no key for one, and
   [RFC 30 §8](rfc-30-kubernetes.md#8.%20Secrets%20and%20the%20node%20channel)
   says the operator or a certificate manager issues a certificate bound to the
   node ID, renews it, and revokes it when the node is decommissioned, but not
   what a peer verifies it with or how that revocation reaches the peers.
   Whatever holds it **MUST NOT** be a row any node writes through
   `RegisterNode`, which would let a node choose the key its own messages verify
   with, against [RFC 15 §4.4](rfc-15-topology.md#4.4%20Node%20channels)'s
   bound-to-the-sender rule. It is settled with RFC 15's cluster work
   **(cluster)**.

---

## Appendix A — where the current code differs

Descriptive, for the refactor.

| # | This document says | The code today |
| --- | --- | --- |
| C1 | One source, the control plane ([§2.1](#2.1%20The%20control%20plane%20is%20the%20source)) | two layers, a host file with environment overrides and a control-plane database, merged key by key; the chunking settings come only from the host file and apply to every share on the node |
| C2 | Bound settings refuse change ([§5.1](#5.1%20A%20bound%20setting%20refuses%20change)) | a share's metadata store and block store can be rebound with no check for existing content; a store's type, bucket, endpoint and prefix can be edited, and a metadata store's type and settings are updated without validation |
| C3 | Refused, never replaced ([§6](#6.%20Validation)) | several settings fall back silently: an invalid chunk profile keeps the default, an out-of-range upload parallelism turns adaptive, an empty compression algorithm becomes zstd, a non-boolean durability flag is ignored, a zero backpressure wait becomes 60 s (its own range check can never fire), a zero Kerberos uid or gid becomes 65534 |
| C4 | Unknown fields refused ([§6](#6.%20Validation)) | unknown host-file keys are warnings; store settings are an untyped document |
| C5 | No secret in configuration ([§7](#7.%20Secrets)) | remote store credentials, the LDAP bind password and Kerberos settings are stored in the configuration database in plain text; reads redact them |
| C6 | Node-local facts only on the host ([§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap)); the management API's token-signing key is an installation-wide `Secret` sealed under the `storage` wrapping key (RFC 23 §5.3), not a host fact | the machine SID and the token-signing secret are per-host and must be copied between nodes by hand; key-file paths held in the shared database must exist on every node |
| C7 | Every setting read by something | a generic settings table and its CLI have no reader; the SMB encryption adapter setting is a stub |
| C8 | One default per setting ([§4.2](#4.2%20Every%20setting%20has%20a%20default%2C%20or%20is%20required)) | log retention defaults differ between the file path and the built-in defaults; a documented journal log limit is described as gating writes and does not |
| C9 | Host fields not overridable, flags or otherwise | the documented order names command-line flags as the highest source; none binds to a setting |

## Appendix B — the settings

Every setting the RFCs name, with its scope and class. A default marked
"proposed" is a starting value awaiting a measurement, which every such row names
after "settled by"; the first reference-box run replaces it under the index's rule
([Test tiers](rfc-index.md#Test%20tiers)), read for a setting default as the
value its row's settling measurement selects, wherever that measurement runs —
per service, per client OS, from traces — not as a measured value plus headroom,
which fits a benchmark target only. A default not marked proposed is settled,
and where no measurement could move it the row says why. A proposed value is not a
requirement, and RFC text cites the setting rather than restating its number as one.
A row still reading "open in RFC n" has no starting value yet; its owning RFC
supplies one before the setting is built.

**How a key and a value are written.** One way, in every section and in the
host's file, so a value valid in one place is valid in all:

- a key is lower case, its parts joined by dots and its words by underscores
  (`adapters.smb.drain_timeout`), matched exactly as declared;
- a duration is a number and a unit, `ms`, `s`, `m` or `h`, in lower case
  (`30s`, `1h30m`; quoted or not in the host's file); it is never negative, a
  number is whole, `0s` is zero, and a bare number is refused, since it names no
  unit;
- a size is a whole number and a unit written as it is listed, with no space
  between them: decimal `KB` to `TB` or binary `KiB` to `TiB` (`512MiB`), or
  `B`. `1.5GiB`, `4.1GB`, `1Gi`, `10m`, `1g`, `1kb`, `100mib` and `1 MiB` are
  refused. In a record an integer of bytes, written as a JSON number, is also
  accepted, exact (the string `"512"` is refused), but a bare integer in the
  host's file is refused, since an older file's `max_size: 100`
  meant megabytes; a value past `2^64 - 1` bytes is refused, in a record and in
  the file (in a record `18446744073709551616` is refused and
  `18446744073709551615` is the largest accepted, exact; in the file
  `18446744073709551616B` is refused and `18446744073709551615B` accepted);
- a boolean is `true` or `false`, and nothing else: `yes`, `on` and `1` are
  refused;
- a list and a map are JSON's in a record and YAML's in the host's file; in the
  environment a list of values is comma-separated, a map is one variable per leaf
  key, and a list of entries has no environment form but a named one (`journals`,
  Appendix C).

| Setting | Defined in | Scope | Class | Default |
| --- | --- | --- | --- | --- |
| journal devices and paths | [RFC 1](rfc-1-journal.md) | — | bootstrap ([§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap)) | required |
| journal maximum footprint | [RFC 1 §6](rfc-1-journal.md#6.%20Capacity) | node, per journal | live | proposed: 80% of the device; settled by: write refusals and repack bytes per byte freed under the profile workload with the footprint at 70%, 80% and 90% of the device |
| per-share journal limit | [RFC 1 §6](rfc-1-journal.md#6.%20Capacity) | share | live | the journal's maximum: no per-share limit until an operator sets one, a policy rather than a measurement |
| segment size | [RFC 28 §2.2](rfc-28-journal-format.md#2.2%20Segments) | — | fixed: a constant of the implementation, never a setting | value open in RFC 28 §5, question 2 |
| sync bound: longest a written record waits for a sync | [RFC 1 §5.2](rfc-1-journal.md#5.2%20Syncing) | — | fixed | proposed in RFC 1: 1 s; settled by: J1 without a `Sync` after each write, at 1 s and at a longer bound ([RFC 1 §5.2](rfc-1-journal.md#5.2%20Syncing)) |
| `ExtentLimit`: placement-index entries per journal | [RFC 1 §4.2](rfc-1-journal.md#4.2%20The%20index%20is%20bounded%20by%20extent%20count%2C%20not%20by%20bytes) | node, per journal | restart | derived: the journal's part of the node's journal-index pool, an eighth of the process memory limit given to the node's journals in proportion to each one's maximum footprint, divided by the entry cost and by nine eighths so the bound and its excess fit (RFC 24 §7.1): about 13.3 million entries for one journal on an 8 GiB node; a value set so that the node's journals together exceed the pool is refused at start, naming the journals and the pool |
| index excess: how far records without bytes may take the placement index above `ExtentLimit` before a release, deallocate or clone target that would split a held extent is refused, answered as [RFC 8 §10.2](rfc-8-engine.md#10.2%20A%20capacity%20refusal%20comes%20back%20here)'s table does | [RFC 1 §4.2](rfc-1-journal.md#4.2%20The%20index%20is%20bounded%20by%20extent%20count%2C%20not%20by%20bytes) | — | derived | an eighth of `ExtentLimit`; eviction releases whole extents, which split nothing, so the cap never wedges a full journal |
| drop ranges per (file, segment): past it the two closest ranges merge, keeping the lower pair | [RFC 1 §7.2](rfc-1-journal.md#7.2%20Repack) | — | fixed | 8; settled, since a merge only keeps a removal longer and never drops one, so the value trades memory against repack's cost, never safety; its ponytail names the evidence that would replace it |
| append streams per journal | [RFC 28 §2.2](rfc-28-journal-format.md#2.2%20Segments) | node, per journal | restart | proposed: 16; settled by: J1's throughput across stream counts with 16 concurrent writers |
| `DescriptorLimit`: segment descriptors held open per process | [RFC 1 §7.4](rfc-1-journal.md#7.4%20Open%20descriptors) | node | restart | proposed: a quarter of the process's open-file limit; floor: [RFC 1 §7.4](rfc-1-journal.md#7.4%20Open%20descriptors), over append streams per journal, `journal.spare_target` and the process's journals, refused at the engine's `Apply`; settled by: reopen rate under the mixed read load of [RFC 1](rfc-1-journal.md)'s soak |
| segments one offer may pin; segments all offers may pin together | [RFC 1 §3.3](rfc-1-journal.md#3.3%20Offload) | — | fixed | proposed in RFC 1: 64; 256; settled by: a journal held at its limit under offload ([RFC 1 §10](rfc-1-journal.md#10.%20Open%20questions), question 4), whether offers ever wait on the bound |
| `journal.headroom_records`: header-only records reserved for removal, loss, hold and stamp records, the last 4,096 (or half, if fewer) of them kept for the header-only records an open appends before it serves | [RFC 1 §6](rfc-1-journal.md#6.%20Capacity) | node, per journal | restart | proposed: 65,536 (14 MiB at 224 bytes each: the largest header-only record, a clone target, with its footer entry); settled by: a journal held at its limit under a removal-heavy load, `headroom_draws` against the headroom's size, and whether removals ever reach the refusal ([RFC 1 §10](rfc-1-journal.md#10.%20Open%20questions), question 4) |
| `journal.spare_target`: spare segments the journal keeps ready, made or recycled off the write path; a roll that finds none is refused naming `spare` | [RFC 28 §2.2](rfc-28-journal-format.md#2.2%20Segments) | node, per journal | restart | proposed: one per append stream (16 at the proposed 16 streams), so every stream can roll once without waiting for a spare; settled by: J8, whether recycling keeps `spare_refusals` at zero under sustained writes past the device's write cache ([RFC 28 §2.2](rfc-28-journal-format.md#2.2%20Segments)) |
| `journal.idle_seal_bytes`: idle-seal threshold | [RFC 28 §3.1](rfc-28-journal-format.md#3.1%20Rebuilding) | node, per journal | restart | proposed: 16 MiB; settled by: J6, the share of time to first read spent scanning active-segment tails, against footer writes from early seals under a trickle load |
| idle-seal time: an active segment with no append for this long is sealed once past `journal.idle_seal_bytes` | [RFC 28 §3.1](rfc-28-journal-format.md#3.1%20Rebuilding) | — | fixed | proposed in RFC 28: 30 s; settled by: J6, as `journal.idle_seal_bytes` |
| repack reserve: journal space outside every share's limit for repack's copies | [RFC 1 §7.2](rfc-1-journal.md#7.2%20Repack) | — | fixed | at least one segment's live payload ([RFC 0 §8.2](rfc-0-data-lifecycle.md#8.2%20Reclaim)) |
| `Target` | [RFC 2 §3.2](rfc-2-carver.md#3.2%20One%20setting%2C%20and%20the%20bounds%20derived%20from%20it) | namespace | bound | proposed: 256 KiB; settled by: [RFC 2 Appendix C](rfc-2-carver.md#Appendix%20C%20%E2%80%94%20choosing%20the%20target)'s plan through the real offload path, before the first namespace binds it |
| key scope | [RFC 2 §4.3](rfc-2-carver.md#4.3%20Key%20scope) | — | derived | the namespace ID's 16 raw bytes, never configured; it never changes, because the namespace ID does not |
| chunk-ID key (material kind `chunk-id-key`) | [RFC 0 §2.1](rfc-0-data-lifecycle.md#2.1%20Entities), [RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys) | namespace | bound | created with the namespace, never configured; wrapped under a master key when the namespace encrypts, in the clear beside its blocks when it does not ([RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys)) |
| export key (material kind `export-key`): seals and authenticates the namespace's exports and state objects | [RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys), [RFC 26 §3.1](rfc-26-catalog-backups.md#3.1%20Layout) | namespace | next write: a new current key seals later exports; a retired one stays until no retained export names it | created with the namespace, never configured; wrapped under a master key, by (ID, fingerprint) ([§7](#7.%20Secrets)) |
| `encrypts`: whether the namespace's chain has an encrypt stage, recorded at creation | [RFC 5 §3.2](rfc-5-transforms.md#3.2%20Configuration) | namespace | bound | required |
| chunking key | [RFC 2 §6](rfc-2-carver.md#6.%20Boundaries%20are%20public) | namespace | bound | drawn at random at creation when the chain encrypts; wrapped under a master key, by (ID, fingerprint) ([§7](#7.%20Secrets)) |
| data key, current | [RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys) | namespace | next write | created with the namespace when the chain encrypts; wrapped, by (ID, fingerprint) ([§7](#7.%20Secrets)) |
| master keys | [RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys) | remote store | live: rotates by re-wrapping; never destroyed while a retained export names it | required on every store configuration, held where [RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys) requires; a key file needs verified escrow where B.2 says (`ErrNotEscrowed`); references only ([§7](#7.%20Secrets)) |
| block format version to write | [§5.4](#5.4%20A%20format%20version%20is%20written%20only%20once%20every%20reader%20reads%20it) | namespace | next write | the newest version every node reads |
| `upload_workers`, `fetch_workers` | [RFC 3 §2.10](rfc-3-syncer.md#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed) | node | restart | derived: 128 each, or the sizing tool's figure, reduced in equal proportion until both halves' peak and the staging budget fit the node's transfers pool, an eighth of the process memory limit, and at least eight each (RFC 24 §7.2) — about 100 each on an 8 GiB node; counts set explicitly that do not fit are refused at start, naming the pool; the 128 is proposed, settled by: the sizing tool's put curve (upload) and ranged-get curve (fetch) per supported service ([RFC 4 §7.5](rfc-4-remote-tier.md#7.5%20Benchmarks%20and%20targets)), and B5 at that size |
| slow-tail retry share: most slow-tail retries per store and direction, as a share of its transfers over a fixed window | [RFC 3 §2.4](rfc-3-syncer.md#2.4%20Every%20transfer%20terminates%2C%20and%20reports) | — | fixed | proposed in RFC 3: 5% over 60 s; settled by: the slow-tail retry rate on each supported service under the sizing tool's load, and a store slowed fourfold, against first-attempt latency ([RFC 3 §2.11](rfc-3-syncer.md#2.11%20Pool%20sizes%20are%20measured%20once%2C%20by%20a%20tool)) |
| retry budget demand reserve; throttled-attempt bound | [RFC 3 §2.4](rfc-3-syncer.md#2.4%20Every%20transfer%20terminates%2C%20and%20reports) | — | fixed | proposed in RFC 3: a quarter of each bucket; eight throttled attempts per transfer; settled by: a copying backup and a reader's cold fetches sharing one failing path, and a throttling service's burst replies |
| `remote.endpoint`, `remote.credential`: the endpoint and the credential reference | [RFC 4 §4.1](rfc-4-remote-tier.md#4.1%20Interface) | remote store | live, under [§5.2](#5.2%20Reaching%20the%20same%20content%20another%20way%20is%20not%20a%20change) | required |
| bucket | [RFC 4 Appendix C](rfc-4-remote-tier.md#Appendix%20C%20%E2%80%94%20the%20S3-compatible%20block%20store) | remote store | bound | required |
| prefix | [RFC 4 §4.2](rfc-4-remote-tier.md#4.2%20Names%20in%2C%20locations%20kept%20inside) | namespace | bound | required |
| storage class | [RFC 4 §8](rfc-4-remote-tier.md#8.%20Decisions%20and%20open%20questions) | remote store | next write | the service's default |
| block target | [RFC 2 §5](rfc-2-carver.md#5.%20The%20block%20assembler) | remote store | next write | proposed: 4 MiB; settable from 1 MiB to 64 MiB, and a value outside is refused; settled by: the block-size benchmark against each service before release ([RFC 4 Appendix B](rfc-4-remote-tier.md#Appendix%20B%20%E2%80%94%20measurements)) |
| transform chain but its encrypt stage: compression and its settings, `require` | [RFC 5 §3.2](rfc-5-transforms.md#3.2%20Configuration) | remote store | next write | empty |
| material provider | [RFC 5 §2.5](rfc-5-transforms.md#2.5%20Reading%20needs%20no%20configuration%2C%20only%20material) | remote store | live | required on every store configuration |
| compress stage `with_encryption: accept`: compression in a chain that encrypts | [RFC 5 §3.2](rfc-5-transforms.md#3.2%20Configuration) | remote store | next write | absent: a chain with an encrypt stage and a compress stage without it is refused |
| compress stage `level` | [RFC 5 Appendix A](rfc-5-transforms.md#Appendix%20A%20%E2%80%94%20compression) | remote store | next write | proposed: 3 (zstd's own default); settled by: [RFC 5 §8.3](rfc-5-transforms.md#8.3%20Benchmarks%20and%20targets)'s compression ratio and MB/s per core on the three corpora, at levels 1 to 9 |
| case sensitivity: the share's fold rule, by ID from the store format record ([RFC 16 §4.6](rfc-16-metadata-store.md#4.6%20Store%20format)) | [RFC 7 §3.3](rfc-7-namespace-metadata.md#3.3%20Case) | share | bound; an export carries it ([RFC 26 §2.1](rfc-26-catalog-backups.md#2.1%20A%20backup%20is%20an%20export%20of%20one%20snapshot%27s%20metadata)) | sensitive (identity rule) |
| entry-digest hash key: orders a directory's entries and keys its listing cookies | [RFC 7 §3.4](rfc-7-namespace-metadata.md#3.4%20Enumeration) | share | bound; an export carries it ([RFC 26 §2.1](rfc-26-catalog-backups.md#2.1%20A%20backup%20is%20an%20export%20of%20one%20snapshot%27s%20metadata)) | drawn at the share's creation, never configured |
| xattr value bound, xattrs per file, named streams per file | [RFC 7 §2.7](rfc-7-namespace-metadata.md#2.7%20Extended%20attributes%20and%20named%20streams) | share | live: a change applies to later writes; existing ones stay readable | 64 KiB, Linux's own value bound; 1024; 1024. Compatibility bounds, not tuning, so no measurement settles them |
| `atime` policy | [RFC 7 §9.2](rfc-7-namespace-metadata.md#9.2%20Timestamps) | share | live | relative, Linux's default; a compatibility choice, not a measurement |
| `gc.interval`: between compaction and collection passes | [RFC 9 §8](rfc-9-gc.md#8.%20API%20surface) | namespace | live | proposed: 1 h; settled by: [RFC 9 §11.6](rfc-9-gc.md#11.6%20Benchmarks%2C%20scale%20and%20soak)'s backlog-drain and deletion-storm rows, the interval at which a pass's work stays within the deletion storm's p99 bound |
| `gc.trash_retention`: time a retired block with recoverable chunks waits before its delete | [RFC 9 §3.7](rfc-9-gc.md#3.7%20Trash) | namespace | live | proposed: 48 h; settled by: how long after its deletion a defect the audit finds is still recoverable, against the bytes held in trash under a deletion storm ([RFC 9 §11.6](rfc-9-gc.md#11.6%20Benchmarks%2C%20scale%20and%20soak)) |
| GC clock skew limit: largest offset of a deleter's clock from store time before it stops | [RFC 9 §3.7](rfc-9-gc.md#3.7%20Trash) | — | fixed | proposed in RFC 9: 1 min; settled by: measured skew on supported hosts under ordinary time sync |
| GC partitions per namespace | [RFC 9 §7.3](rfc-9-gc.md#7.3%20GC%20is%20one%20service%20per%20namespace%2C%20partitioned%20by%20prefix) | — | fixed | open in [RFC 9 §12](rfc-9-gc.md#12.%20Open%20questions), item 6 |
| `gc.space_amp_target`: stored over referenced bytes the compactor holds the namespace under; 0 turns compaction off | [RFC 9 §4.4](rfc-9-gc.md#4.4%20When%20to%20compact%20is%20policy) | namespace | live | proposed: 1.25; settled by: [RFC 9 §12](rfc-9-gc.md#12.%20Open%20questions) item 1, a churning workload's amplification under each target against the bytes each rewrites |
| GC `Recheck` period: between re-reads of a namespace store's settings and claim | [RFC 9 §7.5](rfc-9-gc.md#7.5%20Service%20settings%20are%20rechecked%20on%20their%20own%20period) | — | fixed | proposed in RFC 9: 5 min; settled by: the service calls one `Recheck` costs across every namespace of an installation, against how long a revoked credential may go unseen ([RFC 9 §7.5](rfc-9-gc.md#7.5%20Service%20settings%20are%20rechecked%20on%20their%20own%20period)'s decision) |
| lease durations: GC partition lease, a snapshot use record's deadline | [RFC 9 §7.3](rfc-9-gc.md#7.3%20GC%20is%20one%20service%20per%20namespace%2C%20partitioned%20by%20prefix), [RFC 26 §2.2](rfc-26-catalog-backups.md#2.2%20A%20backup%20holds%20its%20snapshot) | — | fixed: constants of the implementation | proposed: 60 s for a GC partition lease, 10 min for a use record's deadline, each renewed at a third of its length; settled by: the longest pause a holder shows in the soak runs ([RFC 9 §11.6](rfc-9-gc.md#11.6%20Benchmarks%2C%20scale%20and%20soak)), with the lease at least ten times it |
| backup folder record's lease *L* | [RFC 26 §2.4.1](rfc-26-catalog-backups.md#2.4.1%20Layout%20at%20the%20location), [RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile) | — | derived | max(10 min, 2σ), renewed at a third of its length: hosts on different installations judge it by their own clocks, and its ceiling holds whenever *L* exceeds about 1.62σ, so raising σ lengthens it with no other change; the 10 min floor is settled as the use record's deadline is |
| `gc.audit.period`: time within which the audit covers every chunk and block record; its rate is derived from it | [RFC 9 §6.1](rfc-9-gc.md#6.1%20Coverage) | namespace | live | proposed: 7 days; settled by: the projected time to cover 2 PB at measured audit throughput ([RFC 9 §11.6](rfc-9-gc.md#11.6%20Benchmarks%2C%20scale%20and%20soak)'s coverage row), which selects the shortest period that throughput holds, against how long a defect may go unseen |
| `gc.audit.forward_period`: time within which a full forward pass covers every ref; incremental passes cover the refs changed since the last pass, and only a full pass lets a count be lowered | [RFC 9 §6.1](rfc-9-gc.md#6.1%20Coverage) | namespace | live | proposed: 90 days; settled by: [RFC 9 §12](rfc-9-gc.md#12.%20Open%20questions) item 2, the full pass's point-read rate against how long a leaked count may hold its block |
| `gc.hold_threshold`: low-count mismatches within one audit period that set the namespace's hold | [RFC 9 §6.4](rfc-9-gc.md#6.4%20A%20store-wide%20trip%20holds%20the%20deleter) | namespace | live | proposed: 16 per `gc.audit.period`; settable from 1 to 1,024, a value outside refused; settled by: mismatch counts from the audit run on a real store at steady state, the threshold sitting above the background rate of isolated defects |
| replica count **(cluster)** | [RFC 10 §7.1](rfc-10-journal-replication.md#7.1%20Count%2C%20floor%20and%20placement) | installation default, shard override (in its shard record) | live | proposed: 3; settled by: the durability target against measured node-loss and repair-time distributions |
| replica floor **(cluster)** | [RFC 10 §7.1](rfc-10-journal-replication.md#7.1%20Count%2C%20floor%20and%20placement) | installation default, shard override (in its shard record) | live | proposed: 2; settled by: the exposed-window length from the repair-time distribution |
| failure domain **(cluster)**: the host, rack or zone a node lies in | [RFC 10 §7.1](rfc-10-journal-replication.md#7.1%20Count%2C%20floor%20and%20placement), [RFC 11 §2.2](rfc-11-ownership.md#2.2%20Automatic%20per-child%20shards) | node | live | the node itself |
| replication gate **(cluster)**: opened by an operator once every node runs a binary that knows the journal extension | [RFC 10 §2.3](rfc-10-journal-replication.md#2.3%20The%20journal%20extension) | installation | live, opened only under [§5.5](#5.5%20A%20node%20joins%20only%20where%20its%20versions%20overlap)'s rule; never closed again | closed |
| stall bound **(cluster)**: how long a sync or the serving loop may make no progress before a node stops renewing; a shard whose replication stalls is relinquished instead, and stops no renewal ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)) | [RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch) | installation | live | proposed: 5 s; settled by: the longest healthy journal-sync and serving-loop stall under load, against the takeover time it would trigger |
| node lease duration **(cluster)** | [RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch), [RFC 10 §3](rfc-10-journal-replication.md#3.%20What%20it%20assumes%20of%20shard%20placement) | installation | restart ([§5](#5.%20Binding%20classes)) | proposed: 10 s; settled by: the takeover-time benchmark ([RFC 10 §15](rfc-10-journal-replication.md#15.%20Test%20plan%20and%20benchmarks)) against the rate of self-fences on store hiccups shorter than the lease |
| node lease renewal interval **(cluster)** | [RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch), [RFC 10 §3](rfc-10-journal-replication.md#3.%20What%20it%20assumes%20of%20shard%20placement) | — | derived | a third of the node lease (3.3 s at its proposed default), not set apart from it, so no pair of values can leave an idle node self-fencing before each renewal |
| drift bound **(cluster)** | [RFC 10 §3](rfc-10-journal-replication.md#3.%20What%20it%20assumes%20of%20shard%20placement) | installation | restart | proposed: 500 ms; settled by: measured clock offset and rate between nodes and the store on the reference deployment |
| gather interval: **(cluster)** how long a claimant waits for replicas' committed points | [RFC 10 §9.2](rfc-10-journal-replication.md#9.2%20Takeover) | installation | live | open in [RFC 10 open question 4](rfc-10-journal-replication.md#16.%20Open%20questions) |
| replica removal triggers: **(cluster)** removal bound, lag in bytes, lag in time | [RFC 10 §7.2](rfc-10-journal-replication.md#7.2%20Removal) | installation | live | open in [RFC 10 open question 4](rfc-10-journal-replication.md#16.%20Open%20questions) |
| replica mark persistence period **(cluster)** | [RFC 10 §7.2](rfc-10-journal-replication.md#7.2%20Removal) | installation | live | open in [RFC 10 open question 4](rfc-10-journal-replication.md#16.%20Open%20questions) |
| removed-replica re-read period: **(cluster)** how often a replica re-reads the shard records it holds content for | [RFC 10 §6](rfc-10-journal-replication.md#6.%20Fencing) | installation | live | open in [RFC 10 open question 4](rfc-10-journal-replication.md#16.%20Open%20questions) |
| repair pacing: **(cluster)** joins in flight per node and per cluster | [RFC 10 §7.4](rfc-10-journal-replication.md#7.4%20Repair) | installation | live | open in [RFC 10 open question 4](rfc-10-journal-replication.md#16.%20Open%20questions) |
| repair scheduler enabled **(cluster)** | [RFC 10 §7.4](rfc-10-journal-replication.md#7.4%20Repair) | installation | live | on |
| open-state lease: NFSv4 lease period, SMB durable-handle timeout | [RFC 14 §4.1](rfc-14-open-state.md#4.1%20A%20client%20lease) | — | fixed | NFSv4 90 s; SMB per the protocol |
| `adapters.smb.pending_timeout`: longest an SMB request refused for a transient cause — capacity, a store stall, the version limit, quota slack, a read-only journal (`DelayJournal`), `ErrUnavailable`, or a metadata conflict (`DelayConflict`) — is held `STATUS_PENDING` before it is answered: `STATUS_DISK_FULL` for capacity, `STATUS_UNEXPECTED_IO_ERROR` for a store stall, the version limit, quota slack, a read-only journal or `ErrUnavailable`, `STATUS_FILE_NOT_AVAILABLE` for a metadata conflict still unresolved (`DelayConflict`); a request held for a share frozen for a move is bounded by `migration.freeze_timeout` plus the break deadline instead, and one held by a `DelayFrozen` with no bound (an administrator's quiesce, a share or open-state shard not yet served here, or a file the metadata reconcile has not visited) by this timeout, then answered `STATUS_FILE_NOT_AVAILABLE` ([RFC 22 §16.5](rfc-22-smb.md#16.5%20Errors), [RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)) | [RFC 20 §3.6](rfc-20-adapters.md#3.6%20Changing%20settings%20while%20serving), [RFC 22 §16.5](rfc-22-smb.md#16.5%20Errors), [RFC 8 §10.2](rfc-8-engine.md#10.2%20A%20capacity%20refusal%20comes%20back%20here) | installation, per adapter | live | proposed: 10 min, must exceed `migration.freeze_timeout` plus the SMB break deadline; settled by: the longest capacity refusal or freeze observed to clear under a fill-and-drain test, against the SMB clients' tolerance of a pending request |
| `adapters.smb.recall_timeout`: longest deadline an SMB request held `STATUS_PENDING` on a recall may carry; must exceed the SMB break deadline ([RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)) | [RFC 20 §3.6](rfc-20-adapters.md#3.6%20Changing%20settings%20while%20serving), [RFC 22 §16.5](rfc-22-smb.md#16.5%20Errors) | installation, per adapter | live | proposed: 60 s; settled by: the time each client OS (Windows, macOS, Samba) tolerates a pending request before cancelling or reconnecting, against the distribution of break-resolution times under a slow-client test |
| `adapters.<protocol>.port`: the port each of the adapter's listeners that would use its protocol's default port binds, on each of the host's listen addresses; a port conflict fails the new adapter's start on that node, which then builds one with the last settings that served and reports the record refused ([RFC 23 §2.3](rfc-23-control-plane.md#2.3%20The%20node%20reconciles%3B%20it%20does%20not%20take%20orders)); on each node it differs from the other adapter's and `sidecars.portmap.port` unless 0 ([§6](#6.%20Validation)) | [RFC 20 §3.6](rfc-20-adapters.md#3.6%20Changing%20settings%20while%20serving), [RFC 23 §3.4](rfc-23-control-plane.md#3.4%20Adapters) | installation default, node override | restart: the node builds a new adapter and swaps it in | NFS 12049, SMB 12445, above 1023 so an unprivileged `dfs` binds them; 0 lets the system pick; a client given no port tries the protocol's own, 2049 or 445 |
| `adapters.<protocol>.drain_timeout`: longest a draining adapter waits for requests already handed to the service | [RFC 20 §3.1](rfc-20-adapters.md#3.1%20The%20adapter%27s%20states) | installation, per adapter | live | proposed: 90 s, at least the SMB break deadline plus the request deadline; settled by: the longest held request a drain must let finish |
| `adapters.<protocol>.idle_timeout`: a connection with no request and no held state is closed | [RFC 20 §6.4](rfc-20-adapters.md#6.4%20Deadlines%20and%20limits) | installation, per adapter | live, for connections accepted afterwards | proposed: 15 min; settled by: clients' reconnect cost against idle connection state |
| `adapters.<protocol>.write_timeout`: longest one gathered write of the writer may take before its connection is closed | [RFC 20 §6.4](rfc-20-adapters.md#6.4%20Deadlines%20and%20limits) | installation, per adapter | live, as above | proposed: 60 s |
| `adapters.<protocol>.handshake_timeout`: negotiate and authenticate, from accept | [RFC 20 §6.4](rfc-20-adapters.md#6.4%20Deadlines%20and%20limits) | installation, per adapter | live, as above | proposed: 30 s |
| `adapters.<protocol>.read_timeout`: the rest of a request once its first byte has arrived | [RFC 20 §6.4](rfc-20-adapters.md#6.4%20Deadlines%20and%20limits) | installation, per adapter | live, as above | proposed: 30 s |
| `connection_limit`: most connections a node's adapters hold at once, a quarter of it per address | [RFC 20 §6.4](rfc-20-adapters.md#6.4%20Deadlines%20and%20limits), [RFC 24 §4.4](rfc-24-resources.md#4.4%20Connections) | node | live, for connections accepted afterwards | derived by RFC 24 §4.4: the least of half of what the connection-state pool can charge and half the process's open-file limit |
| `adapters.nfs.tls`: `mode` (`off`, `opportunistic`, `require`), `cert_file`, `key_file`, `client_ca`; any mode but `off` requires the certificate and key, and a file set while `off` is refused; until `StartTLS` exists, `Validate` refuses `opportunistic` and `require` ([RFC 20 §3.6](rfc-20-adapters.md#3.6%20Changing%20settings%20while%20serving)); TLS 1.3, fixed | [RFC 20 §5.5](rfc-20-adapters.md#5.5%20Message%20transforms) | installation, per adapter | live: certificate files re-read on change, each field for handshakes or connections begun afterwards | `mode` off |
| `adapters.nfs.versions`: `min` and `max`, the lowest and highest NFS version served, of `3`, `4.0`, `4.1`, `4.2`; `3` is NFSv3 with MOUNT, NLM and NSM | [RFC 20 §3.6](rfc-20-adapters.md#3.6%20Changing%20settings%20while%20serving), [RFC 21 §2.1](rfc-21-nfs.md#2.1%20Versions%20served) | installation, per adapter | live, for connections accepted afterwards | 3 to 4.2, RFC 21's whole range; an unknown value, or `min` above `max`, refused |
| `adapters.smb.dialects`: `min` and `max`, the lowest and highest SMB dialect served, the same range shape as `adapters.nfs.versions` | [RFC 20 §3.6](rfc-20-adapters.md#3.6%20Changing%20settings%20while%20serving), [RFC 22 §3.1](rfc-22-smb.md#3.1%20Dialects) | installation, per adapter | live, for connections negotiated afterwards | 2.1 to 3.1.1, RFC 22's whole range; a bound outside it refused |
| `adapters.<protocol>.disabled_operations`: operations switched off, by name | [RFC 20 §4.2](rfc-20-adapters.md#4.2%20The%20operation%20table) | installation, per adapter | live | none |
| sidecar enabled: `mdns`, `wsd`, `portmap`, `rpcbind` | [RFC 20 §3.5](rfc-20-adapters.md#3.5%20Sidecars) | installation default, node override | live | as the current release ships them |
| `sidecars.portmap.port`: the port the port mapper binds, on each of the host's listen addresses; on each node it differs from `adapters.<protocol>.port` unless 0 ([§6](#6.%20Validation)) | [RFC 20 §3.5](rfc-20-adapters.md#3.5%20Sidecars) | installation default, node override | restart: reported restart-pending until the process restarts ([RFC 23 §2.3](rfc-23-control-plane.md#2.3%20The%20node%20reconciles%3B%20it%20does%20not%20take%20orders)) | 10111; 0 lets the system pick |
| `clone_max_len`: longest `CLONE` or duplicate-extents request answered as one atomic clone; a longer one is `ErrInvalid` | [RFC 8 §9.1](rfc-8-engine.md#9.1%20Clone%20adopts%20carved%20refs%20and%20copies%20the%20rest%20locally), [RFC 17 §5.9](rfc-17-vfs.md#5.9%20Copy%20and%20clone) | installation | live | proposed: 1 GiB; settled by: the time one atomic clone of that length takes on the reference box, against the default request deadline and the store's transaction limits ([RFC 16 §4.1](rfc-16-metadata-store.md#4.1%20One%20small%20interface%20per%20backend)) |
| copy chunk: the length one atomic clone of a `COPY` or copychunk covers; the copy runs as a series of them and may answer a short count | [RFC 8 §9.1](rfc-8-engine.md#9.1%20Clone%20adopts%20carved%20refs%20and%20copies%20the%20rest%20locally), [RFC 17 §5.9](rfc-17-vfs.md#5.9%20Copy%20and%20clone) | — | fixed | proposed: 64 MiB; settled by: the longest a writer to the source waits behind one chunk on the reference box, against the copy's throughput ([RFC 8 §9.1](rfc-8-engine.md#9.1%20Clone%20adopts%20carved%20refs%20and%20copies%20the%20rest%20locally)'s decision) |
| version block: versions one durable raise of a share's version limit covers; the next raise runs on [RFC 8 §2.7](rfc-8-engine.md#2.7%20The%20version%20limit)'s trigger, evaluated at each draw: the larger of half a block and the peak rate × the margin, 30 s + `existence_age` + 1 s (the trigger's own sampling), the peak being the highest count of versions drawn in any one second of the last 60 s | [RFC 7 §9.4](rfc-7-namespace-metadata.md#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward) | — | fixed | proposed: 2²⁴ versions, so the half left when a raise falls due covers about 260,000 versions a second for the 32 s margin at the proposed `existence_age`; the 60 s peak window and the 1 s sampling are fixed with it ([RFC 8 §2.7](rfc-8-engine.md#2.7%20The%20version%20limit)); settled by: stall tolerance first — writes still acknowledged through a metadata stall of the self-fence's length at the reference box's peak version rate — then versions a crash skips, which the 64-bit counter absorbs |
| object bound: largest versioned object put at a backup location | [RFC 4 §4.15](rfc-4-remote-tier.md#4.15%20Versioned%20objects%20at%20a%20backup%20location), [RFC 26 §3.1](rfc-26-catalog-backups.md#3.1%20Layout) | — | derived | the largest encoded block, so every versioned object, an export part included, is scheduled as a block is. An export is cut into parts at it, and a head that would exceed it is cut into head parts the same way, listed by a small root ([RFC 26 §3.1](rfc-26-catalog-backups.md#3.1%20Layout)), so no export is too large to put |
| retry window **(cluster)**: how long a mutation's request ID and result are kept to answer a retry — the dedup table, a journal request entry, a request record | [RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope) | installation | live: a change applies to entries written after it | proposed: 15 min, above a Linux hard-mount client's retransmission back-off, which starts at 60 s and caps at 600 s; a value below 600 s plus the takeover bound — the node lease, plus the drift bound, plus one renewal period, plus the gather interval ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)) — is refused; settled by: retry traces of hard-mounted NFS clients across a takeover and a gateway node's loss, against the entries the window keeps at the peak mutation rate (about 1.8×10⁷ at 20,000 a second) |
| dedup entries per client **(cluster)**: most entries the dedup table keeps for one client within the retry window, the oldest dropped first | [RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope) | — | fixed | proposed: 65,536; settled by: the same retry traces as the retry window, the most mutations one client sends between a call and its last retransmission at the peak per-client rate. A retry whose entry the cap dropped is applied again ([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)'s decision) |
| hold budget per transport: requests and bytes an adapter may hold at once for one session or connection, counted by the adapter from a request's first hold until it is answered, whether it waits inside the service behind a clone under `vfs.WithHold` or between re-drives | [RFC 20 §4.6](rfc-20-adapters.md#4.6%20Held%20requests), [RFC 17 §5.9](rfc-17-vfs.md#5.9%20Copy%20and%20clone) | — | derived | NFSv4.1: max(1, a quarter of the session's slots) calls waiting behind a clone, a hard cap past which the adapter passes `WithHold(0)`, answered `NFS4ERR_DELAY`; SMB: 256 requests and 32 MiB per connection, constants, a floor past which a connection draws on the node's hold pool, a sixteenth of the process memory limit (a bootstrap fact, [§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap)), derived; a test sizes the pool by setting that limit, 128 MiB giving 8 MiB ([RFC 24 §4.3](rfc-24-resources.md#4.3%20Held%20requests%3A%20where%20their%20bytes%20go)); a request past a floor with the pool full is answered `STATUS_INSUFFICIENT_RESOURCES` at once. NFSv3, NFSv4.0 and NLM count nothing; the request deadline bounds every hold. The count is at the gateway node holding the request, so it is exact in a cluster too |
| default request deadline, for an operation that arrives with none | [RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values) | — | fixed | 30 s |
| single-node self-fence: time with a write transaction outstanding or failing and none committed before a node stops acknowledging writes | [RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile) | — | fixed | 30 s, the default request deadline |
| small-call bound: longest one Small attempt, its one request or one read-then-write | [RFC 3 §1.3](rfc-3-syncer.md#1.3%20Interface) | — | fixed | proposed in RFC 3: 30 s; settled by: the small-body cap at a working link's floor share, moved together with the cap ([RFC 3 §2.10](rfc-3-syncer.md#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed)'s decision) |
| small-body cap: largest body one Small call carries, either direction | [RFC 3 §1.3](rfc-3-syncer.md#1.3%20Interface) | — | fixed | proposed in RFC 3: 256 KiB; settled by: as the small-call bound, with which it moves |
| grant budget: delegations, leases and oplocks one client holds at one primary before recalls begin | [RFC 14 §5.5](rfc-14-open-state.md#5.5%20A%20client%27s%20grants%20are%20bounded) | — | fixed | proposed in RFC 14: 4096, recalled down to 2048; settled by: [RFC 14 §5.5](rfc-14-open-state.md#5.5%20A%20client%27s%20grants%20are%20bounded)'s decision, grants held by the profile workload's busiest client |
| clock-rate bound ρ: how far one clock's rate may differ from another's over a wait | [RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile) | — | fixed | proposed: 0.05; settled by: measured monotonic-clock rates of supported hosts against each other and the store's time |
| clock-offset bound σ: how far two hosts' clocks may read apart when a wait starts | [RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile) | installation | restart | 1 s; raised by a deployment whose time synchronisation cannot hold two hosts within it. It has no ceiling: every lease fenced by its holder's own clock is derived from it, the folder record's lease as max(10 min, 2σ), so raising σ lengthens those leases with it ([RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)). The cluster's lease waits do not use it ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)) |
| grant fence: how long a client that let a recall or break time out is offered no grant | [RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time) | — | fixed | 90 s, one lease period |
| SMB break deadline: an oplock or lease break not acknowledged by it times out | [RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time) | — | fixed | 35 s, Windows' value; MS-SMB2 leaves the timer to the implementation |
| settings generation read period | [§5](#5.%20Binding%20classes) | — | fixed | 1 s |
| live propagation bound: a live change runs on every node within it, and a node behind it is unhealthy | [§5](#5.%20Binding%20classes) | — | fixed | proposed: 5 s; settled by: commit-to-run time of a live change across nodes on a store with no watch ([§9](#9.%20Test%20plan)'s propagation row) |
| active version of each shared format and protocol | [§5.5](#5.5%20A%20node%20joins%20only%20where%20its%20versions%20overlap) | installation | live, raised only under §5.5's gate | the version the installation was created with |
| shard slot count **(cluster)** | [RFC 11 §2.2](rfc-11-ownership.md#2.2%20Automatic%20per-child%20shards) | installation | bound, from the installation's creation | proposed: 4096; settled by: slot-table size and rebalance skew at the largest expected node count |
| capacity weight **(cluster)** | [RFC 11 §2.2](rfc-11-ownership.md#2.2%20Automatic%20per-child%20shards) | node | live | 1, every node equal; an operator sets it for unequal hardware, so no measurement settles it |
| shard policy: per share, or **(cluster)** subtree or per child, and the directories that start a shard | [RFC 11 §2](rfc-11-ownership.md#2.%20Shards) | share | live: a change applies to files created from then on, and existing files change shard only by a batched move ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)) | per share: the whole share |
| `shard.follow_writer`, `.window`, `.share` **(cluster)** | [RFC 11 §3.3](rfc-11-ownership.md#3.3%20The%20primary%20follows%20the%20writer) | installation | live | on; proposed: 5 min; 0.9; settled by: forwarded-byte shares observed on reference workloads |
| `shard.dwell` **(cluster)** | [RFC 11 §3.3](rfc-11-ownership.md#3.3%20The%20primary%20follows%20the%20writer) | installation | live | proposed: 30 min; settled by: handover cost (tail shipped) against measured writer-alternation periods |
| `shard.replace_delay`: **(cluster)** the re-placement delay | [RFC 11 §2.2](rfc-11-ownership.md#2.2%20Automatic%20per-child%20shards) | installation | live | proposed: 10 min; settled by: the distribution of node return times after a lapse (restarts, reboots) |
| `shard.unreachable_delay`, `shard.unreachable_handover_bound`: **(cluster)** how long gateways may fail to reach a primary that reaches the store before its shards are handed away, and how long that handover may take before it becomes a failover | [RFC 11 §5.1](rfc-11-ownership.md#5.1%20Gateways%20forward%20to%20the%20primary) | installation | live | proposed: 30 s; 60 s; settled by: the client stall a deployment tolerates against brief gateway losses, and handover times of the share limit's tail |
| share's namespace | [RFC 12 §2.1](rfc-12-snapshots.md#2.1%20A%20namespace%20is%20the%20unit%20that%20moves) | share | bound | the share's own |
| oldest unoffloaded extent alert | [RFC 8 §11.4](rfc-8-engine.md#11.4%20How%20far%20behind%20offload%20is%2C%20is%20observable) | share | live | proposed: 1 h; settled by: the oldest-unoffloaded age's p99 under the profile workload with a healthy store, the alert set above it |
| snapshot policy: schedule, retention and the backup location it names | [RFC 12 §3.2](rfc-12-snapshots.md#3.2%20Configuration) | share | live | none |
| backup location: store, credential reference, inspection credential reference, mode, lifecycle age, retention cap, generation `G`, break-glass principal; system fields put-integrity outcome and health object identity | [§2.5](#2.5%20A%20backup%20location%20is%20its%20own%20record), [RFC 4 §4.14](rfc-4-remote-tier.md#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes), [RFC 26 §2.4.4](rfc-26-catalog-backups.md#2.4.4%20Expiry%20and%20the%20sweep) | installation | per field, as §2.5 states | mode required; for `immutable`, lifecycle age and retention cap required, since each is checked against the bucket's own rules and no default fits every policy, and the inspection credential and break-glass principal absent by default; `G` proposed: 7 days, settled by: extension calls a day against the service's request rate and bill (the immutable-location check of [RFC 26 §6.3](rfc-26-catalog-backups.md#6.3%20Group%20A%20%E2%80%94%20lost%20or%20wrong%20content)); the rest as the store's |
| policy `backup.kind`: `catalog` or `copy`, a copying backup | [RFC 26 §2.4](rfc-26-catalog-backups.md#2.4%20Copying%20backups) | share | live | `catalog` |
| policy `backup.verify_every`: period between verifications of a copying backup; 0 never verifies on a period | [RFC 26 §2.4.3](rfc-26-catalog-backups.md#2.4.3%20Writing%20one%2C%20step%20by%20step) | share | live | 0 |
| `backups.copy_rate`: copying backups' transfer rate | [RFC 26 §2.4.6](rfc-26-catalog-backups.md#2.4.6%20Cost%20and%20pacing) | installation | live | proposed: 200 MiB/s; settled by: [RFC 26 §6.5](rfc-26-catalog-backups.md#6.5%20Benchmarks%20and%20targets)'s daily copying backup, against the location's put throughput and live p99 latency |
| `rehome.rate`: a re-home's copy rate; 0 pauses it | [RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace) | installation default, share override while its re-home runs | live | proposed: 100 MiB/s; settled by: [RFC 27 §5.5](rfc-27-namespace-migration.md#5.5%20Benchmarks%20and%20targets)'s re-home under 64 writers, p99 write latency at that rate |
| `snapshots.hold_bound`: held journal bytes per share before a cut is refused | [RFC 12 §2.4](rfc-12-snapshots.md#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history) | share | live | proposed: 64 GiB; settled by: dirty bytes a share carries at a cut in [RFC 12 §5.5](rfc-12-snapshots.md#5.5%20Benchmarks%20and%20targets)'s cut-to-`complete` benchmark |
| `snapshots.hold_journal_fraction`: held share of one journal's capacity, summed over every share it carries, before a cut is refused | [RFC 12 §2.4](rfc-12-snapshots.md#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history) | installation | live | proposed: 0.25; settled by: the same benchmark, against the journal headroom [RFC 1 §6](rfc-1-journal.md#6.%20Capacity) leaves for live writes |
| `snapshots.reserve`: history bytes per share before a new cut is refused | [RFC 12 §2.9](rfc-12-snapshots.md#2.9%20Space%20is%20reported%2C%20not%20charged) | share | live | none |
| `snapshots.gate_max`: longest a cut gate stays closed, its drain included | [RFC 12 §2.3](rfc-12-snapshots.md#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate) | installation | live | proposed: 1 s; settled by: [RFC 12 §5.5](rfc-12-snapshots.md#5.5%20Benchmarks%20and%20targets)'s cut under a sustained writer, the gate's p99 at one shard and 64 shards under sustained offload |
| `snapshots.cut_deadline`: a cut not committed this long after its announce is aborted | [RFC 12 §2.3](rfc-12-snapshots.md#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate) | installation | live | proposed: 5 s; settled by: announce-to-commit time of a cut across many shards, from the same benchmark |
| `snapshots.lock_max`: longest lock a snapshot may carry past the time it is set | [RFC 12 §2.7](rfc-12-snapshots.md#2.7%20Scheduled%20snapshots%2C%20retention%20and%20locks) | installation | live | proposed: 8760 h (one year); a policy, not a measurement: the longest retention a compliance policy asks for, which no benchmark moves |
| `snapshots.reserve_fraction`: default reserve, history bytes per byte charged live | [RFC 12 §2.9](rfc-12-snapshots.md#2.9%20Space%20is%20reported%2C%20not%20charged) | installation | live | proposed: 1.0; settled by: history growth under hourly snapshots of a churny share (the reserve check of [RFC 12 §5.4](rfc-12-snapshots.md#5.4%20Group%20B%20%E2%80%94%20cost%20and%20wedging)) |
| `snapshots.directory`: the browse directory's name | [RFC 12 §2.5](rfc-12-snapshots.md#2.5%20Browsing%20a%20snapshot) | share | live | `.snapshot` |
| snapshot ordinal quarantine after a deletion | [RFC 12 §2.5](rfc-12-snapshots.md#2.5%20Browsing%20a%20snapshot) | — | fixed | 24 h |
| consecutive skipped policy ticks before a health condition | [RFC 12 §2.3](rfc-12-snapshots.md#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate) | — | fixed | 3 |
| incomplete files a cut may list per shard; a shard with more refuses the close | [RFC 12 §2.3](rfc-12-snapshots.md#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate) | — | fixed | 16 |
| `migration.freeze_timeout`: bound on a move's freeze, through B's ready | [RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step) | installation | live | proposed: 5 min; settled by: [RFC 27 §5.5](rfc-27-namespace-migration.md#5.5%20Benchmarks%20and%20targets)'s move of a 10⁷-file namespace, freeze to B serving, against key count |
| move pause deadline: how long past A's last renewal a move's pause record lapses, after which the scheduler resumes or aborts the move; a recovery import's pause has none and is never aborted by it | [RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step) | — | fixed | proposed: 10 min, renewed at a third of its length; settled by: the longest pause A shows during a pre-seed in [RFC 27 §5.5](rfc-27-namespace-migration.md#5.5%20Benchmarks%20and%20targets)'s move, with the deadline at least ten times it |
| `migration.reconnect_wait`: how long a moved persistent open is kept at the new owner, from when it begins serving, if its own client makes no tree connect to its share there; the open's own timeout starts at that client's (its `ClientGuid`'s) first tree connect to its share at the new owner, so one client reconnecting starts no other client's timer (decision: per client, not from the first tree connect by any client, which would start the timers of opens whose clients have not yet repointed and close their handles early); it also bounds the move grace a named address holds the moved shares in at B from publish, which ends at the earlier of this and the address's arrival plus one lease. That grace is NFS-only: it refuses non-reclaim NLM locks and NFSv4 opens and locks and accepts their reclaims, and SMB on the moved shares sees no grace, its persistent handles keeping this per-client timer ([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step), [RFC 14 §4.2](rfc-14-open-state.md#4.2%20Grace%20makes%20volatile%20state%20safe)) | [RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step) | installation | live | proposed: 1 h; settled by: the longest repoint-to-reconnect time the move test's clients show ([RFC 27 §5.5](rfc-27-namespace-migration.md#5.5%20Benchmarks%20and%20targets)) |
| `migration.hold_reply`: longest a call waits in a freeze before `ErrDelay` | [RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step) | installation | live | proposed: 1 s; settled by: the longest wait NFS clients in the support matrix tolerate before their own retry |
| staging budget: raw transfers held between fetch and upload by every copy and by GC's relocation, one pool per process, in memory and then spilled to disk | [RFC 3 §2.2](rfc-3-syncer.md#2.2%20The%20pool%20size%20is%20a%20memory%20bound), [RFC 26 §2.4.6](rfc-26-catalog-backups.md#2.4.6%20Cost%20and%20pacing) | — | derived | as [RFC 3 §2.2](rfc-3-syncer.md#2.2%20The%20pool%20size%20is%20a%20memory%20bound) derives it, from the copy flow's cap in the fetcher's pool; staging above it spills under `backups.spill_path`, and a reservation waits once memory plus spill reach `backups.spill_max`, or at the budget when no spill path is set; the spill is cleared at start |
| `backups.spill_path`: directory staging spills to above its memory budget | [RFC 3 §2.2](rfc-3-syncer.md#2.2%20The%20pool%20size%20is%20a%20memory%20bound) | — | bootstrap ([§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap)) | required with its first reader ([Appendix C](#Appendix%20C%3A%20the%20bootstrap%20file)); unset, nothing spills and staging waits at the memory budget. Checked against the journals at every start in both directions and refused if it shares space with one ([§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap)); a deployment choice, not a measurement |
| `salvage.path`: directory stranded journal tags are exported to | [RFC 1 §3](rfc-1-journal.md#3.%20Interface) | — | bootstrap ([§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap)) | required with its first reader ([Appendix C](#Appendix%20C%3A%20the%20bootstrap%20file)); unset, every export fails and is reported, and the tags stay stranded. Checked as `backups.spill_path` is; a deployment choice, not a measurement |
| `events.path`: directory the node spools its event streams to | RFC 25 §4.6 | — | bootstrap ([§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap)) | required with its first reader ([Appendix C](#Appendix%20C%3A%20the%20bootstrap%20file)); refused on a journal's filesystem, which refuses the start |
| `backups.spill_max`: memory plus spill at which a staging reservation waits for staging to drain, one total per process across every copy and GC's relocation ([RFC 3 §2.2](rfc-3-syncer.md#2.2%20The%20pool%20size%20is%20a%20memory%20bound)) | [RFC 26 §2.4.6](rfc-26-catalog-backups.md#2.4.6%20Cost%20and%20pacing) | installation | live | proposed: 64 GiB; settled by: the spill [RFC 26 §6.5](rfc-26-catalog-backups.md#6.5%20Benchmarks%20and%20targets)'s daily copying backup reaches from a source faster than its folder |
| `backups.max_copy_time`: bound on one copy attempt; a successor builds on a failed one | [RFC 26 §2.4.3](rfc-26-catalog-backups.md#2.4.3%20Writing%20one%2C%20step%20by%20step) | installation | live | proposed: 48 h; settled by: the longest incremental copy attempt in [RFC 26 §6.5](rfc-26-catalog-backups.md#6.5%20Benchmarks%20and%20targets)'s daily copying backup at `backups.copy_rate`, which selects the value, since an attempt cut short fails into a successor that builds on it rather than losing its work; a first copy spans as many attempts as it needs, so it does not size this |
| `existence_age`: longest a synced write's existence waits for the group commit | [RFC 8 §5.1](rfc-8-engine.md#5.1%20Commit%20is%20answered%20by%20the%20journal) | installation | live | proposed: 1 s; settled by: the sync bound it matches ([RFC 1 §5.2](rfc-1-journal.md#5.2%20Syncing)), and E5/E7's existence transactions per second at 1 s and shorter |
| `group_commit.max_files` (`G`): files per existence batch | [RFC 8 §5.2](rfc-8-engine.md#5.2%20Group%20commit%20is%20bounded%2C%20and%20retries%20only%20the%20files%20that%20conflict) | node, per journal | live | proposed: 256; settled by: E5/E7, transactions per stability point rising at the bound |
| `offload.retry_min`, `offload.retry_max`, `offload.retry_max_pressure`: offload and removal retry backoff, its floor and its caps normal and under capacity pressure | [RFC 8 §6.3](rfc-8-engine.md#6.3%20The%20offload%20pipeline), [§8.1](rfc-8-engine.md#8.1%20A%20removal%20is%20one%20transaction%2C%20then%20batches) | node | live | proposed: 1 s; 60 s; 5 s; settled by: failing-store and recovered-store runs, the cap neither hammering a failing store nor leaving a recovered one idle |
| `removal.rescan`: interval at which each served shard's unfinished-removal index is read again from its cursor, re-posting removals dropped or lost | [RFC 8 §6.1](rfc-8-engine.md#6.1%20The%20work%20queue) | node, per journal | live | proposed: 60 s; settled by: the deletion-storm benchmark ([RFC 9 §11.6](rfc-9-gc.md#11.6%20Benchmarks%2C%20scale%20and%20soak)), time from a dropped removal to its next batch against the index reads' metadata cost |
| age tick: interval at which the work queue re-derives due offload from the journal | [RFC 8 §6.1](rfc-8-engine.md#6.1%20The%20work%20queue) | — | fixed | a quarter of `offload.max_age` |
| `offload.max_age`: longest a file's dirty bytes wait below the dirty-byte threshold before it is offered | [RFC 8 §6.2](rfc-8-engine.md#6.2%20When%20a%20file%20is%20offered) | share | live | proposed: 5 s; settled by: a streaming workload (E1/E6), whether the age bound, not the byte bound, fragments blocks |
| consecutive failed offload passes before a per-file health condition | [RFC 8 §6.3](rfc-8-engine.md#6.3%20The%20offload%20pipeline) | — | fixed | 5 |
| speculation budget, bytes in flight per share | [RFC 8 §7.4](rfc-8-engine.md#7.4%20The%20speculator) | share | live | proposed: 256 MiB, four readers' read-ahead caps; settled by: E4 and E8 with 1, 4 and 16 concurrent readers, against write refusals while speculation runs |
| `readahead.max_blocks`, `readahead.max_bytes`: read-ahead cap, window ahead of one reader | [RFC 8 §7.4](rfc-8-engine.md#7.4%20The%20speculator) | share | live | proposed: 8 blocks or 64 MiB, whichever is smaller; settled by: E4 sequential bytes fetched per byte read and E8 first-byte latency across caps |
| cold-read threshold between fetching chunks and fetching the block | [RFC 8 §7.8](rfc-8-engine.md#7.8%20A%20cold%20read%20asks%20for%20chunks%2C%20or%20for%20the%20block) | — | fixed | a quarter of the block |
| `eviction.repack_threshold`: unreclaimed fraction of a journal's capacity that requests a repack | [RFC 8 §10.3](rfc-8-engine.md#10.3%20Repack%20is%20triggered%20here) | node, per journal | live | proposed: 10 %; settled by: repack bytes copied per byte freed, and write refusals, under the profile workload across thresholds |
| eviction occupancy target: allocated occupancy, under the journal's maximum, that eviction frees down to | [RFC 8 §10.1](rfc-8-engine.md#10.1%20Eviction%20is%20chosen%20here%2C%20and%20needs%20no%20new%20record) | node, per journal | live | proposed: 85% of the maximum footprint; settled by: repack bytes copied per byte freed under the profile workload ([RFC 8 §10.1](rfc-8-engine.md#10.1%20Eviction%20is%20chosen%20here%2C%20and%20needs%20no%20new%20record)), across targets |
| fill low-water mark: free journal capacity, reserved for writes, below which a demanded extent is not filled and pre-warm pauses | [RFC 8 §7.3](rfc-8-engine.md#7.3%20Filling%20is%20a%20decision) | node, per journal | live | proposed: 10% of the maximum footprint; settled by: [RFC 8 §16](rfc-8-engine.md#16.%20Open%20questions) item 1, hit rate and write refusals for fill-always, this rule and fill-never on a large-file workload |
| write pacing: soft threshold *S*, rate window, rate floor *r*₀ | [RFC 8 §10.2.1](rfc-8-engine.md#10.2.1%20Writes%20are%20paced%20before%20the%20limit%2C%20not%20stopped%20at%20it) | — | fixed | proposed: half of the limit *L*; 10 s; 1 MiB/s; settled by: E3's submission latency and offloaded throughput across thresholds and floors |
| **(cluster)** quota reservation slack `S`, per principal, project or share per primary, applied only where a quota's writers span primaries | [RFC 17 §5.6](rfc-17-vfs.md#5.6%20Quota) | installation | live | proposed: 1 GiB; settled by: one principal's write throughput over `existence_age` on the reference box, below which its writes wait for a commit ([RFC 17 §5.6](rfc-17-vfs.md#5.6%20Quota)) |
| **(cluster)** quota slack fraction `f`: a quota's slack is `min(S, f × Hard.Bytes)` | [RFC 17 §5.6](rfc-17-vfs.md#5.6%20Quota) | installation | live | proposed: 1 %; settled by: the overshoot of a 1 GiB quota written through two primaries against its write latency, across fractions |
| audit policy: which operations emit an access event — every decision, denials only, or writes and namespace changes — and whether the file's audit entries are honoured (honour file audit entries) | [RFC 17 §3.3](rfc-17-vfs.md#3.3%20Event%20hooks), RFC 19 §3.7 | share | live | none: no access events, and no audit entry selects anything |
| ACL list bounds: entries per access or audit list, encoded size per list; a set past either is refused whole | RFC 19 §2.8 | — | fixed | 128 entries; 64 KiB |
| `identity.id_range`: the range uids and gids are allocated from when a principal has none | RFC 18 §2.3 | installation | live: a change applies to later allocations; an allocated ID never changes. A write of `identity.id_range`, a put or a delete that leaves the default, whose `Hi` is at or below the uid or gid counter's high-water mark is refused (§6, RFC 18 §2.6); only writes of `identity.id_range` are checked, so a range used up refuses no other write | 1 000 000 to 1 999 999 999 |
| `identity.ntlm`: whether NTLMv2 is accepted; off deletes every stored NT hash | RFC 18 §3.5 | installation | live; written by `admin` only, taking the installation right, so an operator's write is refused ([RFC 23 §4.1](rfc-23-control-plane.md#4.1%20Shape)) | on: workgroup deployments have no KDC (RFC 18 §3.5's decision) |
| `identity.netbios_name`: the installation's own NetBIOS name, which an SMB CHALLENGE advertises as its computer name, and which a local NTLMv2 sign-in's domain is compared with, ASCII letters folded (RFC 18 §4.6); on a joined installation it is the computer account's name, which the join writes upper-cased, whatever case the account was created in, and refused change while joined. The DNS computer and domain names the CHALLENGE also carries are the host's name and the directory record's realm, or the host's own domain when not joined | [RFC 18 §3.5](rfc-18-identity.md#3.5%20NTLMv2), [RFC 18 §4.6](rfc-18-identity.md#4.6%20Names), [RFC 22 §4.1](rfc-22-smb.md#4.1%20Signing%20in) | installation | live | `DITTOFS`: one name for the installation, where a host name is per node; the CHALLENGE's DNS computer name tells installations apart. At most 15 bytes of `[A-Z0-9-]`, and not the directory record's NetBIOS domain compared with ASCII letters folded, refused otherwise; the upper-case rule binds the stored value, which a join writes upper-cased |
| `identity.keytab`: the reference to the installation's imported keytab, a `Secret` of kind keytab (§7), holding AES keys for the service names a node answers on that the computer account does not hold | [RFC 18 §3.3](rfc-18-identity.md#3.3%20Kerberos), [RFC 23 §3.5](rfc-23-control-plane.md#3.5%20Users%2C%20groups%2C%20grants%20and%20netgroups) | installation | live; written by `admin` only, through the keytab resource; a keytab with no key or a key that is not AES is refused whole, and so is one holding a key for a name the joined computer account holds, in `identity.directory.realm`'s realm or in every realm when none is recorded; a set over an existing one is refused until its rotation is built, so it is replaced by a delete, then a set | none: no imported keytab |
| `identity.nfs4_domain`: the domain of NFSv4 owner strings | RFC 18 §4.5 | installation | live | the keytab's first realm in lower case, or `localdomain` with none |
| `identity.directory`: the one directory — its schema, `ad` or `rfc2307`, address, base, bind name (`bind_name`, the distinguished name or UPN its simple bind sends), bind credential reference, trust roots, realm, NetBIOS domain, the attributes holding a user's and a group's name, whether a Kerberos ticket in its realm must carry a PAC (`require_pac`, RFC 18 §3.4), and optionally a domain controller address and a DNS server used instead of the domain's DNS service records | RFC 18 §5.1 | installation | live; a record asking for plain LDAP or for skipping certificate verification is refused, and so is an `ad` one whose realm holds a lower-case ASCII letter (RFC 18 §4.2), and one whose NetBIOS domain is `identity.netbios_name`, compared with ASCII letters folded (RFC 18 §3.5); written by `admin` only | none: no directory; schema `ad`, name attribute `sAMAccountName`, or `uid` and `cn` for an `rfc2307` one, `require_pac` true |
| `identity.directory_stale_grace`: how long past the TTL a directory entry may be served while the directory fails | RFC 18 §5.3 | installation | live | 15 min; a policy, not a measurement: the window an operator accepts a disabled directory account keeping access through an outage, 0 where it must stop within the TTL |
| `identity.oidc`: the OpenID Connect issuers the management API accepts — each one's issuer URL, client ID, audience, allowed signing algorithms (asymmetric only), role-claim name, and a confidential client's secret reference (a `Secret`, RFC 13 §7) | RFC 18 §8.2, RFC 23 §5.5 | installation | live; written by `admin` only | none: password sign-in only; `audience` the client ID |
| export policy defaults (fields of RFC 16's `ExportPolicy` record, written by `PutExportPolicy`, not `Setting` rows): `Squash`, `Anonymous`, `ReadOnly`, the SMB flag for access-based enumeration, and the SMB flag for guest access — who is squashed, the principal a squashed or anonymous caller acts as, whether every caller is narrowed to read-only, whether SMB listings omit what the caller cannot read, and whether a guest session may connect to a share that does not encrypt; its other fields default empty | RFC 18 §6.2; [RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees); [RFC 19 §3.7](rfc-19-authorization.md#3.7%20Listings%20hide%20what%20the%20caller%20cannot%20read); [RFC 22 §4.1](rfc-22-smb.md#4.1%20Signing%20in) | share | live: the policy's version is in every resolved caller's key | `root`; the well-known anonymous principal; off; off; off |
| process memory limit *L*: the language runtime's soft limit, else the container's, else the host's memory | RFC 24 §2.1 | — | bootstrap ([§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap)), read once at start | the first source set |
| node memory pools: share of *L*, and floor | RFC 24 §2.3, §2.4 | — | fixed | receive, reply, hold, connection state, open state 1/16 each; metadata store 3/16; journal index 1/8; transfers 1/8; a quarter unpooled. Floors: receive and reply 16 MiB, connection and open state 4 MiB, metadata store 64 MiB, transfers eight workers per half and the staging they imply. The metadata store's share is proposed, settled by: RFC 24 §8's M3; the unpooled quarter by M4 |
| buffer classes | RFC 24 §3.1 | — | fixed | 4 KiB, 64 KiB, 1 MiB, 8 MiB |
| record caps: an RPC record, an SMB message | [RFC 21 §2.3](rfc-21-nfs.md#2.3%20Transfer%20sizes), [RFC 22 §3.3](rfc-22-smb.md#3.3%20Sizes%20and%20credits) | — | derived | the capacity of the class holding each protocol's largest payload: 1 MiB and 4 KiB for an RPC record, 8 MiB and 4 KiB for an SMB message |
| fragments per RPC record | [RFC 21 §2.3](rfc-21-nfs.md#2.3%20Transfer%20sizes) | — | fixed | 64; settled, since common clients send one fragment, and its `ponytail:` says when to raise it |
| per-request overhead charged beside a request's buffer | RFC 24 §3.1 | — | fixed | proposed: 8 KiB; settled by: RFC 24 §8's M1 |
| connection cost charged at accept | RFC 24 §4.4 | — | fixed | proposed: 64 KiB; settled by: RFC 24 §8's M2 |
| NFSv4.1 session cost; slot cost, per slot of each channel | RFC 24 §4.5 | — | fixed | proposed: 4 KiB; 256 bytes; settled by: RFC 24 §8's M2 |
| open-state costs: per open; per lock range, waiter, grant or reservation | RFC 24 §4.6 | — | fixed | proposed: 1 KiB; 256 bytes; settled by: RFC 24 §8's M6 |
| open-state reclaim allowance: how far a reclaim during grace may take the open-state pool | RFC 24 §4.6 | — | fixed | the pool at twice its share, the excess taken from the unpooled quarter; a reclaim of durable state is never refused ([RFC 14 §4.2](rfc-14-open-state.md#4.2%20Grace%20makes%20volatile%20state%20safe)) |
| work-scheduler pending entries per instance | RFC 24 §5.1, [RFC 8 §6.1.1](rfc-8-engine.md#6.1.1%20The%20shared%20work%20scheduler) | — | fixed | proposed: 262,144; settled by: RFC 24 §8's M8 |
| client and share caps on the receive and reply pools | RFC 24 §4.1, §4.2 | — | fixed | three quarters of a pool per client key while another waits; three quarters per share while the pool is more than half full |
| `connection_limit`: client connections one node accepts | RFC 24 §4.4 | node | live, for connections accepted afterwards | derived: the least of the connection-state pool's ceiling over twice the connection cost and half the open-file limit; a value above either is refused at start and when the node applies a change |
| connections per address | RFC 24 §4.4 | — | fixed | a quarter of `connection_limit`, rounded down and at least one |
| requests in flight per connection, for protocols without flow control | RFC 24 §3.4 | — | fixed | proposed: 64, one NFSv4.1 session's fore channel; settled by: RFC 24 §8's M7 |
| background CPU: work holding a CPU token at once | RFC 24 §6.2 | — | fixed | proposed: three quarters of the language runtime's parallelism, at least one; settled by: RFC 24 §8's M5 |
| open-file limit split: journal segments, client connections, the rest | RFC 24 §6.3 | — | fixed | a quarter; a half; a quarter, of the soft limit raised to the hard limit |
| `api.session_lifetime`: how long a sign-in's refresh token lasts | RFC 23 §5.3 | installation | live; written by `admin` only (RFC 23 §5.2) | proposed: 12 h, a working day of one sign-in |
| `api.token_max_lifetime`: the latest expiry a service account's API token may have | RFC 23 §5.3 | installation | live; written by `admin` only (RFC 23 §5.2) | proposed: 365 days |
| `api.oidc_role_map`: role-claim values mapped to `viewer`, `operator` or `admin` | RFC 23 §5.5 | installation | live; written by `admin` only | none: a claim maps to no role |
| `api.password_sign_in`: the roles that may sign in by password | RFC 23 §5.5 | installation | live; written by `admin` only (RFC 23 §5.2) | every role; the last unlinked `admin` keeps it whatever the setting |
| `audit.retention`: how long a management audit record is kept before it is deleted in bounded batches | RFC 23 §6 | installation | live; written by `admin` only (RFC 23 §5.2); a value under 90 days is refused, so no write can make the sweep delete the record of that write or of the quarter before it | proposed: 400 days, a year and a quarter, so one annual review finds the whole year |
| management API limits: access token lifetime; idempotency records kept; an `Idempotency-Key`'s length; finished operations kept; an operation runner's claim | RFC 23 §4.5, §4.6, §5.3 | — | fixed | 15 min; 24 h; at most 255 bytes; 7 days; 30 s ahead, renewed every 10 s |
| management API paging and waits: page size; `?wait` cap | RFC 23 §4.2, §4.6 | — | fixed | 100 by default, at most 1 000; 60 s |
| API calls one node runs at once, the next answered `503` | RFC 23 §4.3 | — | fixed | 64 |
| sign-in throttle: attempts counted before waits begin, per account and source address together, per source address, and per account across all sources; the wait; how long a pair's count is kept after its last counted attempt; how long a source's count is kept after its last settled failure | RFC 23 §5.3 | — | fixed | 5; 5; 100; 1 s, doubling to 5 min; 24 h; 15 min |
| generated first password; a first-password file's size | RFC 23 §5.4 | — | fixed | 24 random bytes; at most 4 KiB, its trailing newline counted |
| `metrics.top_shares`: shares exported with a `share` label per per-share gauge, the worst by it | RFC 25 §2.5 | installation | live | 20 |
| `events.retention`, `events.max_bytes`: the event spool's age and size bounds, past which the oldest file is deleted | RFC 25 §4.6 | node | live | proposed: 7 days; 4 GiB |
| `tracing.endpoint`: the trace collector, an `https` or `http` URL (RFC 25 §6.3's decision); none turns tracing off, and any other value is refused at validation ([§6](#6.%20Validation)) | RFC 25 §6.3, §8.1 | node | restart: the root builds the provider and hands each component its tracer once, so a wrong collector is cut off only by a restart (RFC 25 §6.3's decision) | none: tracing off |
| `tracing.sample_ratio`: the fraction of requests sampled at the adapter, and of every other root span (RFC 25 §8.1) | RFC 25 §6.3, §8.1 | node | restart, as `tracing.endpoint` | proposed: 0.01; settled by: spans exported per second on the reference workload against what the collector ingests, with `health_spans_dropped_total` staying at 0. A value that is not a number, or lies outside 0 to 1, is refused at validation ([§6](#6.%20Validation)), since a ratio that is not a number samples nothing and says so nowhere |
| metric limits: values per entity label per metric per node; series per node and per metric at the reference profile | RFC 25 §2.4, §2.5 | — | fixed | 64; 50 000 and 10 000 |
| histogram bucket sets | RFC 25 §2.6 | — | fixed | `request`, `background`, `size`, `count`, as RFC 25 §2.6 lists them |
| event rings: entries per stream, size per entry | RFC 25 §4.3 | — | fixed | 16 384 for `access`, 4 096 each for `quota` and `logon`; 1 KiB |
| event spool: rotation; sync interval; high-water warning | RFC 25 §4.3, §4.6 | — | fixed | 64 MiB or one hour, whichever first; 1 s; 80% of `events.max_bytes` |
| quota hard-refusal coalescing window | RFC 25 §4.4 | — | fixed | 60 s |
| liveness watchdog: how long an internal loop may go without a turn before liveness fails | RFC 25 §3.5 | — | fixed | 60 s |

> ponytail: proposed, not yet agreed — the fixed rows copy values RFC 23, 24 and 25 tabulate
> themselves — about 25 rows, a count not yet checked against those tables — so
> that this appendix stays the one list of every number inside a MUST, as the
> index requires. A copy drifts: replace the trust in the copy with a test that
> diffs these rows against the owners' tables once a copied row is found
> drifted again.

## Appendix C: the bootstrap file

Every host fact of [§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap), listed
with its key, its type and its default. The environment name is `DITTOFS_` and the
key in upper case, its dots as underscores; no two keys share one. A key is
matched exactly as declared, so a key that differs only in case is refused as
unknown, with the declared one named. A path is absolute
and written in its clean form: a journal's path is its identity in the metadata
store ([RFC 16 §3](rfc-16-metadata-store.md#3.%20Interfaces%2C%20by%20consumer)'s
`JournalDir`), so `/data/j1/` beside `/data/j1` would be a second journal, and a
relative path, a `~` or a trailing slash is refused rather than rewritten.

Left out, since they have no reader yet or no key: the scratch paths
`backups.spill_path`, `salvage.path` and `events.path`, which join this table
with their first reader (a node that has one reads it from the file, and the
table then lists all three, so `dfs config schema` stays complete), the
provisioning file's path ([§2.4](#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file)), which joins with that file's first reader, and the
process memory limit, which is read from the runtime, the container and the host
(RFC 24 §2.1).

**The state and runtime directories** are not keys. The state directory is
`$STATE_DIRECTORY` when the service manager sets it, else `$XDG_STATE_HOME/dittofs`,
else `$HOME/.local/state/dittofs`; the runtime directory is `$RUNTIME_DIRECTORY`, else
`$XDG_RUNTIME_DIR/dittofs`, else a directory under the state directory that only
the process's user owns (mode `0700`). A unit may set its variable as a
colon-separated list, whose first entry is read here. A state directory that already
exists is checked, not merely used: a start refuses one that is a symlink, is
owned by neither the node's user nor root, or is writable by others. A runtime
directory that already exists is checked harder, since the socket in it has to
be trusted: the start refuses one that is a symlink, that the node's user does
not own, or that group or others can write, because whoever can write the
directory can unlink `dfs.sock` and bind its own. A volume a platform mounts for
a non-root user (a Kubernetes `fsGroup`, owned by root and writable by the group)
fails that check by itself, so there the node creates a subdirectory of it, mode
`0700`, owned by the node's user, and puts the socket in that; the start checks
the subdirectory as it checks any runtime directory. A start without root, `./dfs init` then
`./dfs start`, writes its metadata, log and `dfs.sock` under the user's home. A
systemd unit's `StateDirectory=`
and `RuntimeDirectory=` set the first of each, so two nodes on one host each get
their own from their own unit.

| Key | Type | Default | Fact |
| --- | --- | --- | --- |
| `node.id` | UUID | none: the `node-id` copies give it | the node's identity, needed only when every copy is lost |
| `node.roles` | list of `protocol`, `storage` | `[protocol, storage]` | the node's roles, compared as a set, a repeated role refused; both is the only accepted value |
| `metadata.backend` | `badger` or `foundationdb` | `badger` | which metadata store; only the block it names is read, and a key of the other block is refused |
| `metadata.badger.path` | path | the state directory's `metadata` | Badger's directory |
| `metadata.foundationdb.cluster_file` | path | required with `foundationdb` | FoundationDB's cluster file |
| `metadata.foundationdb.tls.cert_file`, `.key_file`, `.ca_file` | path | none: named together, or none | the client certificate, its key and the CA file FoundationDB's TLS options take; the files are the credential, never a value in this file |
| `journals` | list of entries, each with a `path` | required | the journal devices, one directory each; a path listed twice, or two that name one directory, is refused. The environment, `DITTOFS_JOURNALS=/data/j1,/data/j2`, sets the paths only, and replaces the file's list; a fact of one journal needs the file. An empty entry, so a trailing or doubled comma as well as an empty value, is refused |
| `adapters.listen` | list of IP literals, each an address to bind | every interface | the addresses the protocol adapters bind; each adapter's port is its own setting (Appendix B) |
| `api.listen` | list of `ip:port` | `127.0.0.1:8080` | the management API's addresses. `:8080`, an empty host, is refused, not read as every interface: write `0.0.0.0:8080` or `[::]:8080` |
| `api.tls.cert_file`, `api.tls.key_file` | path | required when an `api.listen` or `metrics.listen` address is not loopback: an IP in `127.0.0.0/8` or `::1`, as [RFC 23 §5.3](rfc-23-control-plane.md#5.3%20Sign-in%2C%20tokens%20and%20sessions) reads it, so a host name, `localhost` included, and an unspecified address (`0.0.0.0`, `::`) are not; a listen value's host is an IP literal, so a name or an empty host (`:8080`) is refused as that before this rule applies | the API's certificate and key (RFC 23 §5.3) |
| `api.socket` | path | `dfs.sock` in the runtime directory | the management API's local socket (RFC 23 §5.3), created mode `0600`. A `dfsctl` context names the socket by its path ([RFC 23 §7](rfc-23-control-plane.md#7.%20The%20CLI)), so a shell without the unit's `$RUNTIME_DIRECTORY` passes the path, and two nodes on one host each name their own in their own file. A path past the operating system's socket-address limit (about 104 to 108 bytes) is refused, naming this key |
| `metrics.listen` | `ip:port`, or `off` | `127.0.0.1:9090` | the metrics listener, served over the API's TLS off loopback (RFC 25 §2.8). `off` is the only way to turn it off, in the file and in the environment, since a YAML null and an empty variable both read as not set |
| `logging.level` | `debug`, `info`, `warn`, `error` | `info` | the log level, in lower case |
| `logging.format` | `text` or `json` | `text` | the log format |
| `logging.output` | `stdout`, `stderr` or a path | the state directory's `dittofs.log` | where logs go |
| `logging.rotation.max_size` | size, a whole number of `MiB`, at least `1MiB` | `"100MiB"` | a log file's size before it rotates. The rotation library takes whole MiB and reads a smaller value as none, so a fraction is refused rather than silently turning rotation off |
| `logging.rotation.max_backups`, `logging.rotation.max_age` | integer; duration, a whole number of days (`0s` is zero) | `5`, `"720h"` | rotated files kept; `0` (and `0s`) keeps all. The age is rounded to whole days by the rotation library, so a value below a day would read as never, and is refused rather than truncated |
| `logging.rotation.compress` | boolean | `false` | whether rotated files are compressed |
| `keys.<role>.kind` | `local` or `kmip` | required, for `protocol` and for `storage`, and the two roles name two different keys | each role's wrapping key ([§7](#7.%20Secrets)); only the block `kind` names is read, and a key of the other block is refused, naming `kind`, as `metadata.backend` refuses each block it does not name ([§6](#6.%20Validation) runs this rule over the values as the file and the environment leave them) |
| `keys.<role>.local.file` | path | required with `local` | the key file that seals new `Secret` records |
| `keys.<role>.local.retired_files` | list of paths | none | the key files that still open what they sealed, and the next key before it seals, in a rotation's first step ([§7](#7.%20Secrets)) |
| `keys.<role>.kmip.endpoint`, `.server_ca`, `.client_cert`, `.client_key`, `.key_uid`, `.timeout`, `.retired_key_uids` | strings and paths, `timeout` a duration | none | a role's key held by a KMIP server; `key_uid` is the key that seals, `retired_key_uids` the ones that still open |

`<role>` is `protocol` or `storage`, so a key is `keys.protocol.local.file`, its
environment name `DITTOFS_KEYS_PROTOCOL_LOCAL_FILE`. A value from the environment
replaces the same key's value from the file and merges with nothing else: a leaf
is replaced alone, and the rest of the role's block stays as the file has it.
A key file's passphrase is read from `DITTOFS_ENCRYPTION_PASSPHRASE`, one for
every key file of the node until a split of the roles ([RFC 15 §2.1](rfc-15-topology.md#2.1%20One%20node%20type))
makes a node hold one role's key alone.

**Names read outside the file.** A `DITTOFS_` name that is not a key is refused
unless it is one of these, which are not keys:

| Name | Read by |
| --- | --- |
| `DITTOFS_CONFIG` | the start, the path of the host's file when `--config` is not given |
| `DITTOFS_ENCRYPTION_PASSPHRASE` | the local key provider, a key file's passphrase |
| `DITTOFS_PASSWORD` | `dfsctl`, which a shell that runs both may export |
| `DITTOFS_<service>_PORT*` and `DITTOFS_<service>_SERVICE_*`, for each Service of the pod's namespace | Kubernetes, which sets them for every Service with an address, none for a headless one: ignored while the value has the shape the kubelet gives to any of the forms the name parses as, a `tcp://`, `udp://` or `sctp://` link then an IP and a port, an IPv6 one in brackets, for a name ending at `_PORT` or at `_PORT_<n>_<proto>`, an IP for `_ADDR` and `_SERVICE_HOST`, a port number for `_SERVICE_PORT*` and `_PORT_<n>_<proto>_PORT`, `tcp`, `udp` or `sctp` for `_PROTO`, and refused otherwise, as unknown when the name is no v1 key's environment name and naming its replacement when it is (the table below), so `DITTOFS_METRICS_PORT=tcp://10.0.0.5:9090` is ignored and `DITTOFS_METRICS_PORT=9091` is refused naming `metrics.listen` |
| the names and prefixes the repository's development tools export (`DITTOFS_TEST_*`, `DITTOFS_E2E_*`, `DITTOFS_PREPUSH_*`, and the launchers' and git hooks' own names) | the tools, which a server they start inherits. The RFC 13 plan's D7 is the list, its repository scan keeps it true, and a bootstrap key wins over any of these names |

**From the files of today.** The key a start refuses names its replacement
(§6's *what to do*):

| Today's key | Now |
| --- | --- |
| `database.*` | dropped: the control plane's database is now the metadata store, `metadata.backend`, `badger` by default; today's `database` block names no Badger path, so `metadata.badger.path` is new |
| `metadata.badger.block_cache_mb`, `.index_cache_mb` | dropped: no key sizes the Badger caches |
| `controlplane.host`, `controlplane.port` | `api.listen`, written `host:port` with an IP host: an empty `host` is read as `127.0.0.1` today, so it becomes `127.0.0.1:<port>`, never `:<port>`, which is refused; `controlplane.tls` is `api.tls`; its timeouts, `jwt.*` and `pprof*` are dropped |
| `controlplane.tls.client_ca`, `.min_version` | dropped: a start refuses a client CA outright (the RFC 23 plan's epic 4) |
| `controlplane.require_initial_password_change` | dropped: only a generated first password is must-change, and one the operator gives the initialisation step is not ([RFC 23 §5.4](rfc-23-control-plane.md#5.4%20The%20bootstrap%20administrator%20and%20the%20first%20password)) |
| `controlplane.drain_stall_timeout` | dropped: it bounded `system drain-uploads`, which v2 deletes (the RFC 23 plan's epic 6) |
| `metrics.enabled`, `.host`, `.port` | `metrics.listen`; a metrics listener was off by default and is now on, on loopback |
| `metrics.auth`, `.token_file`, `.path`, `.tls.*` | dropped: the listener carries no token and no TLS of its own, being served over the API's TLS (the row above) |
| `blockstore.journal.path`, a root directory | `journals`, one entry per device |
| `blockstore.journal.chunk_size`, `.chunk_max` | dropped: a namespace's chunking is its `Target`, set when the namespace is created, and `Min` and `Max` follow from it ([RFC 2 §3.2](rfc-2-carver.md#3.2%20One%20setting%2C%20and%20the%20bounds%20derived%20from%20it)), so neither is a host fact |
| `blockstore.journal.dirty_expire` | dropped: the sync bound, how long a written record waits for a sync, is fixed, proposed at 1 s ([RFC 1 §5.2](rfc-1-journal.md#5.2%20Syncing)) |
| `blockstore.journal.max_log_bytes` | dropped: a journal's maximum footprint and a share's journal limit bound what the append log did, both settings (Appendix B, [RFC 1 §6](rfc-1-journal.md#6.%20Capacity)) |
| `blockstore.journal.backpressure_max_wait` | dropped: a writer is slowed by a fixed curve before the limit and refused at it, with no wait to configure ([RFC 8 §10.2.1](rfc-8-engine.md#10.2.1%20Writes%20are%20paced%20before%20the%20limit%2C%20not%20stopped%20at%20it)) |
| `logging.level` in upper case | lower case only: `DEBUG` is refused, `use debug`; the operator's CRD enum and `dfs init`'s template move to lower case with it |
| `logging.rotation.max_size: 100`, megabytes | a size with its unit, `"100MiB"`: a bare integer is refused in the file |
| `logging.rotation.max_backups`, `.max_age` | the same keys, but their default moves from `0` (keep all) to `5` and `"720h"`; `max_age` was an integer number of days |
| `shutdown_timeout` | the setting `adapters.<protocol>.drain_timeout`; today's value bounds each stage of the process's shutdown and the operator derives a pod's grace period from it, while the setting bounds one adapter's drain alone |
| `admin.*`, `kerberos.*`, `identity.*` (`machine_sid` included), `ldap.*`, `gc.*`, `snapshot.*`, `integrity.*` | no longer host facts: records and settings, set through the management API |
| `DITTOFS_ADMIN_INITIAL_PASSWORD` | the initialisation step's password reference, the `--admin-password-env <name>` or `--admin-password-file` flag of `dfs init` ([RFC 23 §5.4](rfc-23-control-plane.md#5.4%20The%20bootstrap%20administrator%20and%20the%20first%20password), the RFC 23 plan's epic 5); a start refuses the old name, never printing its value, and the operator and the compose file that set it move with the RFC 23 plan's O4 and the RFC 13 plan's epic 6 |
| the `DITTOFS_*` names of the keys above, `DITTOFS_KERBEROS_KEYTAB`, `_PRINCIPAL`, `_KRB5CONF`, `DITTOFS_KERBEROS_MACHINE_ACCOUNT_SECRET`, `DITTOFS_LDAP_BIND_PASSWORD` and `DITTOFS_DATABASE_POSTGRES_*` among them | the same as their key, which the refusal names, never printing a value |
| a v1 key not above | refused as unknown, with the closest declared key named |

A start refuses all of these where today's loader only warns, so a file of
today fails until it is migrated, `docker/config.yaml` and the operator's
rendered file included. Off loopback the API needs a certificate and key: the
image ships none and takes the pair the operator mounts, and the repository's
launchers, the compose file among them, generate a self-signed one at run time
(the RFC 13 plan's epic 6). A listener kept on loopback behind a proxy on the
same host, which terminates TLS, needs none. [RFC 23 §5.3](rfc-23-control-plane.md#5.3%20Sign-in%2C%20tokens%20and%20sessions)
already serves nothing off loopback without TLS, and this table only states it.

**The file's commands.** `dfs config show` prints the file it read, the state and runtime directories and the socket path, and the effective bootstrap, each
value with where it came from (the file, a variable, or the default), a
credential's file by its path only, and a value that came from the environment
as the variable's name. `dfs config validate` refuses what a start would
refuse, in §6's shape, and starts nothing; it checks the rules that relate two
keys, such as "two different keys" or one journal path named twice, as `dfs start`
does, which refuses the same. `dfs config schema` prints this table as
a JSON Schema, which covers the types, the enumerations and the requirements
that do not depend on another key.
