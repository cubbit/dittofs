# Issue priority triage

How an open item gets its `Priority` in the DittoFS project board. The rules are written to be
applied by a person or an agent from the item's title, body, labels and board status alone, and
their purpose is to let an agent set priorities automatically: wherever a rule would need a
judgement the text cannot support, it should be tightened rather than left to taste.

The rules cover both kinds of item on the board: repository issues and draft items. Drafts hold
what must not be public yet, security findings above all, so triage reads them like any issue but
never copies their content anywhere public: not into an issue, a comment, a commit or a PR.

Triage covers **leaf issues** only: an open issue that is not labelled `epic` or `roadmap`, and
whose parent, if it has one, is labelled `roadmap`. Two kinds of issue are out of scope, and
triage never sets or proposes their priority:

- issues labelled `epic` or `roadmap`: they are containers, and how roadmap items are defined;
- sub-issues of an `epic`: they are subtasks of planned work, ordered by the epic's own plan.

The rules are tuned to the project's current stage: one first use case, described below, and
a lot of weight on the test suites. They will change as the use cases do. Re-tiering the board
after a change is a proposal for a person to approve, item by item, never a bulk rewrite.

## How to apply

What triage may do depends on whether the item has a priority, and whether it is pinned:

| The item | Triage |
| --- | --- |
| Has no `Priority` | Sets one with the rules below. |
| Has a `Priority`, not pinned | Leaves the field unchanged. If the rules place it elsewhere, it proposes the change to a person, who decides. |
| Is pinned | Leaves it alone and does not question it: the priority was set by hand on purpose. A pinned item with no `Priority` stays without one. |

A pin is the human override. Use it when a priority was decided for a reason the rules do not
capture, or must hold even if the rules change later: a pinned item keeps its priority through any
future revision of this file. An item is pinned when it carries the `priority-pinned` label, or
when a line of its body starts with `priority-pinned:` followed by the reason. A draft cannot carry
a label, so the body line is how a draft is pinned, and it keeps working on an issue converted
from that draft.

**Planned feature work** is new functionality that someone is building or has committed to build,
such as the implementation of a design RFC. Triage may place it with the rules below like any other
item, but its actual priority is agreed among the people doing the work, who are free to disregard
the one triage set.

**Pick the section first.** Each section below matches one roadmap epic of the release: Bugfixing,
Testing, Architecture, Tech debt, CI, Infra and packaging, User documentation, Features,
enhancements and performance, Metrics and tracing, UI. Read the body, not the labels: a label says
where a problem shows, not what it is. Ask what the issue wants changed, and take the **first**
match:

1. The web interface → **UI**.
2. The product does something wrong that a client, an operator or a background process can see:
   a wrong result, lost or returning data, a hang, a stall, a wrong error, a health signal that
   exists and lies → **Bugs**. This holds even when the issue is labelled `observability`,
   `documentation` or `tech-debt`, and even when the cause is a missing lock or a slow path. An
   audit tracker is a list of findings, so it is Bugs too. Two exceptions: when the code does what
   a design RFC says and the RFC is what is wrong, it is **Architecture**; when the defect is in
   the package or the install, it is **CI, Infra and packaging**.
3. A test, a suite, a fixture or the harness → **Testing**.
4. A shared pipeline, the release, packaging, or running DittoFS as a service → **CI, Infra and
   packaging**.
5. A design RFC that is silent, wrong or contradicts another, or a spike to settle one →
   **Architecture**.
6. Operator documentation → **User documentation**.
7. A signal that does not exist yet: a metric, a trace, a log, a health condition → **Metrics and
   tracing**.
8. A capability that does not exist, or a performance limit with no wrong result → **Features,
   enhancements and performance**.
9. Code that works as specified but costs more than it should → **Tech debt**.

An item under one of those roadmap epics normally takes that epic's section. When the list above
places it elsewhere, the list wins: triage places the item there and proposes moving it.

A **shared gate** is a pipeline whose verdict others rely on: CI on `develop` and on an integration
branch such as `feat/storage-v2`, the release build, and the nightly suite. A problem in one
developer's local setup is not a shared gate.

Then walk that section's tiers top-down and stop at the **first** one that matches. Write one line
naming the rule that matched, section included (`Bugs P1.3`), so the next reader can disagree with
the reasoning and not just with the label. If no rule matches, leave the priority unset and, when
possible, ask a person.

- **What the issue describes decides, not how it is phrased.** A `documentation` label on an issue
  whose real content is "a new share inherits a deleted share's files" doesn't make it a docs issue.
- **Not reproduced doesn't lower a tier.** A finding from code reading with a concrete mechanism
  ranks as if it were reproduced. Only a body that shows the mechanism cannot happen lowers it.
- **A section with no P0 has none on purpose.** An item that looks critical there is in the wrong
  section: a missing metric that hides data loss is a bug.
- **When unsure between two tiers, take the lower one.** P0 has to stay short, or it stops
  meaning "drop everything".

Each section ends with examples: anchors to check a borderline call against, each linked to the
rule that places it. Replace an anchor once its issue is closed. A section with no open issue to
anchor it has no examples yet.

## The first use case

Several rules refer to it, so it is spelled out here. FSLogix profile and Office containers on an
SMB share whose data stays in the journal on the node's local disk. Offload to S3 (the syncer, the
carver, cache eviction) is not part of it in v1.0:

- **Clients:** Windows over SMB. Any other SMB client counts too, a Linux CIFS mount included.
  NFS is not involved.
- **Identity:** domain users from an Active Directory domain controller, authenticated over
  Kerberos or NTLM, with domain SIDs in the files' ACLs.
- **Files:** one VHDX per user, 10–30 GB or more, held open through one handle for a whole session
  of several hours.
- **Write pattern:** random, 4 KiB-aligned overwrites of data already written. The file
  grows by writing past its end and shrinks by a truncate at sign-out.
- **Scale:** about 10,000 users, so many users, many large files, and access control between users
  matters.
- **Paths it exercises:** the journal under a steady stream of writes to one file, its reclamation
  of overwritten data, durable handles, and installing and running the service.

## Bugs

Defects in the product: what a client, an operator or a background process sees go wrong.

### P0 — Critical

Any one of these:

1. **Data loss, corruption or unreadable data** triggered by what a client does, or by a normal
   background process (sync, GC, eviction, compaction, restart, recovery) in a supported
   configuration. What a client does includes unusual but legitimate patterns, such as a program
   that keeps writing to a file after making it read-only, or names that differ only by case across
   SMB and NFS. Corruption includes a deleted file coming back and a read that returns wrong bytes
   or zeros. Unreadable data is still stored but can no longer be read back, for example after a
   restart or an upgrade. The trigger has to be the operation, not the bytes: when the loss needs
   file content chosen to collide with an internal format, it is Bugs P1.2.
2. **Stability:** a crash, panic, deadlock or hang, an operation that fails permanently from then on,
   or resource growth that exhausts disk or memory within days of normal use.
   **Loss of availability that needs manual intervention** belongs here too: the server or a share
   stays down, or keeps failing, until an operator acts. An outage that recovers on its own is not
   P0 through this rule.
3. **Security reachable from the network without credentials**: access without authentication,
   an authentication bypass, or a privilege escalation, over any protocol: SMB, NFS or the control
   plane API.
4. **Denial of service without credentials**: requests from a client that has not authenticated
   crash the server, exhaust its memory, disk or cache, or stop it serving other clients. A
   handshake that is never finished but leaves state behind counts.
5. **Denial of service through a legitimate workload**: an ordinary user, with no special rights
   and no crafted requests, stops the service for other users through the way they use it. The
   usual shape is write amplification: a mass rewrite fills the local cache or journal faster than
   backpressure slows that user down, and writes start failing or stalling for all users. Slowing
   down or stalling only the user or file that causes the load is not this rule: backpressure
   working is fine, and a defect confined to that file is placed by the other rules.

### P1 — High

Any one of these:

1. **Affects the first use case** described above. This covers SMB behaviour, large files,
   overwrites, a single file under a steady stream of writes, the journal's write and reclamation paths, multi-user
   access control on SMB.
2. **Data loss or unreadable data with an unusual trigger**: either an admin action the system
   should have refused, such as moving a store that is in use, two stores on one bucket, or
   deleting a share together with its snapshots; or file content chosen to collide with an internal
   format, such as a file that starts with the compression frame marker. Bugs P0.1 is triggered by
   the operation a client performs or by a background process; here the trigger is the admin
   action or the content itself.
3. **Silent durability failure**: the system stops uploading data, or stops being able to, and
   does not report it, while nothing is lost yet. Examples are a missing bucket, a journal that
   disappeared, and a health check that stays green while uploads fail. Once data is actually lost,
   Bugs P0.1 or P1.2 places it.
4. **Storage leaks on the remote**: objects that are never reclaimed, so the bucket grows without
   limit under the first use case's overwrite pattern.
5. **First start**: the server does not come up, or its share cannot be mounted, on its first
   start. A documented install path that fails is CI, Infra and packaging P1.
6. **Breaking configuration changes**: a config key, environment variable, CLI flag or default
   that is renamed, removed or changes meaning between releases, so an existing setup stops working
   or silently runs with different settings.
7. **Regressions**: something that worked in the previous release no longer works, unless it is P0.
8. **Security that needs an authenticated client**: a client with valid credentials can reach data
   or state it should not, such as another user's files or another client's stateids, over any
   protocol.
9. **Denial of service that needs an authenticated client**: a client with valid credentials
   sends crafted or hostile requests, such as an oversized count or a malformed structure, that
   crash the server, exhaust a resource or stop it serving others, over any protocol: SMB, NFS or
   the control plane API. A heavy but legitimate workload is Bugs P0.5, not this rule.
10. **Non-conformance with POSIX**, whichever protocol it was found through.

### P2 — Medium

1. **Non-conformance with a protocol spec or a design RFC**, when no P0 or P1 rule applies. That
   covers the NFS RFCs (RFC 1813, 7530, 8881), the SMB specifications (MS-SMB2, MS-FSA, MS-DTYP)
   and the DittoFS design documents under `docs/`: the code does something other than what they
   say. An RFC that is itself wrong, silent or contradictory is Architecture. SMB behaviour
   usually hits Bugs P1.1 first.
2. **NFS only**: NFSv3 and v4.x bugs, conformance gaps and audit findings whose effect cannot reach
   anything but NFS clients, such as protocol state, delegations or grace. A wrong POSIX result
   found through NFS is Bugs P1.10, because what it breaks is shared with SMB.
3. **Security that needs more than a client**: an attacker who can already read the bucket or log
   in to the server host, such as metadata exposed in an encrypted bucket, and hardening with no
   known way to exploit it.
4. **Audit and umbrella issues**: a list of findings or a tracker with sub-issues. Each finding
   split out of it is placed on its own.

### P3 — Low

1. **Low impact**: a wrong error code or message with no further consequence, a race on a field
   with no observable effect, goroutines that outlive shutdown, cosmetic CLI output.

### Examples

| Issue | Tier | Rule |
| --- | --- | --- |
| #2910: sync stops short on a 160 GB file, client saw the write complete | P0 | Bugs P0.1, data loss through normal I/O |
| #3097: NFSv3 WRITE refuses the owner after `chmod 0`, the data is lost at close | P0 | Bugs P0.1, an unusual but legitimate client pattern |
| #2895: GC deletes the object before its metadata, dedup adopts missing bytes | P0 | Bugs P0.1, a background process loses data |
| #3056: editing a store that is in use makes the share unreadable after restart | P1 | Bugs P1.2, an admin action that should be refused |
| #2897: crafted content colliding with the compression magic is unreadable | P1 | Bugs P1.2, content built to collide with an internal format |
| #3051: uploads to a missing bucket fail silently | P1 | Bugs P1.3, silent durability failure |
| #3099: a directory's nlink reads one short right after an NFSv3 MKDIR | P1 | Bugs P1.10, POSIX, found through NFS |
| #3078: REMOVE does not recall another client's delegation | P2 | Bugs P2.1, protocol spec (RFC 8881) |
| #3080: grace status reports 0 expected clients | P2 | Bugs P2.2, NFS only; a signal that exists and lies is a bug |
| #2511: SMB adapter audit, ~55 live defects | P2 | Bugs P2.4, an audit tracker |
| #2625: unlocked access to the SMB position field | P3 | Bugs P3.1, no observable effect |

## Testing

Tests, suites and the harness: what tells us whether the product works.

- **P0:** a test in a shared gate gives the wrong verdict: it passes while the behaviour it checks is broken, fails while it works every time,
  or is flaky at any rate.
- **P1:** any one of these:
  - a flaky test outside the shared gates;
  - a defect in the harness, a runner or a fixture that can hide or distort the results of other
    tests, such as one that kills processes it did not start, leaves state behind or misreports a
    run;
  - a missing, skipped or broken test of anything the first use case exercises, such as SMB through
    a Linux CIFS mount, several users on one file at once, or domain users against a domain
    controller.
- **P2:** a test or suite gap outside the first use case that gates nothing: a suite that skips, a
  missing test, a runner that fails locally only.
- **P3:** test hygiene and refactors with no coverage at stake.

| Issue | Tier | Rule |
| --- | --- | --- |
| #4011: a journal stats test fails 1 run in 10 on `feat/storage-v2` | P0 | Testing P0, flaky in a shared gate |
| #3060: the e2e wrapper reports a pass when `-run` matches no test | P0 | Testing P0, a wrong verdict in a shared gate |
| #3074: `setup-posix.sh` kills any running dfs, not only its own | P1 | Testing P1, a harness defect that can distort other runs |
| #3093: cthon04 never runs over a CIFS mount | P1 | Testing P1, a missing test of the first use case |
| #3094: no test has two SMB users, or a domain user, on one file at once | P1 | Testing P1, a missing test of the first use case |
| #2996: pynfs runs only the `all` flag, so delegation and reboot tests never run | P2 | Testing P2, a suite gap outside the first use case |
| #2775: migrate the SMB handlers' white-box tests across package boundaries | P3 | Testing P3, a test refactor |

## Architecture

Design decisions: a design RFC that is silent, wrong or contradicts another, and the
investigations and spikes that feed a decision.

- **P0:** the missing decision blocks a subtask of an open epic that someone is working on now.
- **P1:** an open epic of the release will need the decision before it closes, or the decision
  changes what the code must do about something costly to change once shipped: an on-disk or wire
  format, persistent state, or a data-safety promise.
- **P2:** the gap is in a part no epic of the release implements (clustering, replication,
  snapshots), or the RFC text drifts from the code without changing what it does.
- **P3:** wording, cross-references, and restating or consolidating rules the RFCs already make,
  when no behaviour changes.

| Issue | Tier | Rule |
| --- | --- | --- |
| #3784: RFC 8 and engine epic 6 disagree on a start with nothing recorded for a device | P1 | Architecture P1, an open epic needs the decision; not P0, the epic goes on with an interim behaviour |
| #3498: RFC 28 drops a rotten truncate record at open, and the removed bytes come back | P1 | Architecture P1, an on-disk format and a data-safety promise; the code follows the RFC |
| #3574: no rule for a replicated shard whose journal turns read-only | P2 | Architecture P2, replication is in no epic of the release |
| #3646: state RFC 0's data-safety promises once, as §10.4 | P3 | Architecture P3, consolidates rules the RFCs already make |

## Tech debt

Code that works but costs more than it should: shortcuts and missing structure.

- **P0:** none. Debt that loses data or takes the server down is a bug.
- **P1:** the debt blocks or slows an open epic of the release, or keeps producing bugs.
- **P2:** debt in what guards correctness (locking, transactions, lifetimes) with no bug traced to
  it yet.
- **P3:** refactors and cleanups that change only how the code reads or is organised.

| Issue | Tier | Rule |
| --- | --- | --- |
| #3005: directory lifetime is guarded by process-local locks, not the store transaction | P2 | Tech debt P2, debt in what guards correctness |
| #3993: remove the control plane's Postgres backend and its CI service | P3 | Tech debt P3, a cleanup |

Place an item as if the code it names stays. Whether a rewrite has made it moot is checked when
someone picks the issue up, not at triage.

## CI, Infra and packaging

The shared pipelines, the release, and how DittoFS is installed and run as a service.

- **P0:** a shared gate cannot run, or reports a pass without testing anything (a false green).
- **P1:** a documented install path (package, Docker image, build from source) fails, or the
  installed service does not start or stay up.
- **P2:** a pipeline that gives the right verdict but is slow, wasteful or noisy, and packaging
  gaps on a platform outside the first use case.
- **P3:** tidiness: workflow refactors, version pins, cosmetic output.

| Issue | Tier | Rule |
| --- | --- | --- |
| #2977: pjdfstest cells hang until the 60 minute timeout | P0 | CI P0, a shared gate cannot run, even when the cause is a flaky mirror |
| #2946: the packaged service crash-loops on first start | P1 | CI P1, a documented install path fails |
| #3864: merge queue runs fill the Actions cache with entries nothing reads | P2 | CI P2, the verdict is right but the pipeline is wasteful |

## User documentation

What an operator reads under `docs/guide/`, the README and the CLI help. Design RFCs are
Architecture.

- **P0:** none. Docs that lead to data loss are placed by what they cause.
- **P1:** following the docs fails to reach a working server, or leads to data loss or an exposed
  share; install and first-start steps above all.
- **P2:** docs that are wrong, missing or outdated for a shipped feature.
- **P3:** wording, typos, layout.

| Issue | Tier | Rule |
| --- | --- | --- |
| #2951: the Docker single-container install fails at the `init` step | P1 | Docs P1, following the docs does not reach a working server |
| #2983: the production install with the .deb package is not documented | P2 | Docs P2, missing docs for a shipped feature |
| #3042: setting `encryption_mode` through the adapter config is not documented | P2 | Docs P2, missing docs for a shipped feature |

## Features, enhancements and performance

New capability, and how fast or how far an existing one goes.

- **P0:** none. A performance problem that stops the service for others is Bugs P0.5.
- **P1:** the first use case needs it: a capability, or a performance limit it hits at its scale.
- **P2:** planned for a release, or a performance gap measured on a realistic workload.
- **P3:** feature requests nobody has planned, field observations that ask for nothing yet, and
  performance ideas with no measurement.

| Issue | Tier | Rule |
| --- | --- | --- |
| #2817: journal group commit amortises only when writers share a shard | P2 | Features P2, a performance gap measured on a create-heavy workload |
| #1199: SMB over QUIC | P3 | Features P3, a feature nobody has planned for a release |
| #2353: dittofs under a local RAG pipeline | P3 | Features P3, a field observation that asks for nothing yet |

## Metrics and tracing

What an operator can see: health conditions, metrics, logs and traces.

- **P0:** none. A health check that stays green while data is lost or uploads stop is a bug (Bugs
  P1.3).
- **P1:** an operator cannot see a condition a P0 or P1 bug rule names, such as a journal turned
  read-only, stalled uploads or a full journal. A signal that exists and is wrong is a bug.
- **P2:** a metric missing for diagnosing capacity or performance.
- **P3:** naming, labels, dashboards.

## UI

The web interface.

- **P0:** none.
- **P1:** the UI shows state wrong enough to lead an operator into a destructive action, or a step
  the first use case's setup needs does not work.
- **P2:** a UI feature that does not work, when the CLI or API does it.
- **P3:** cosmetic.

