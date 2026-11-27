<div align="center">

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/logo-light.svg">
  <source media="(prefers-color-scheme: light)" srcset="assets/logo-dark.svg">
  <img alt="DittoFS" src="assets/logo-dark.svg" width="320">
</picture>

<br>

[![Go Version](https://img.shields.io/badge/Go-1.26+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![Nix Flake](https://img.shields.io/badge/Nix-flake-5277C3?style=flat&logo=nixos)](https://nixos.org/)
[![Go Report Card](https://goreportcard.com/badge/github.com/cubbit/dittofs)](https://goreportcard.com/report/github.com/cubbit/dittofs)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg?style=flat)](LICENSE)
[![Status](https://img.shields.io/badge/status-experimental-orange?style=flat)](https://github.com/cubbit/dittofs)

[![Tests](https://github.com/cubbit/dittofs/actions/workflows/tests.yml/badge.svg?branch=develop)](https://github.com/cubbit/dittofs/actions/workflows/tests.yml)

**A modular virtual filesystem written entirely in Go**

NFSv3/v4.0/v4.1/v4.2 and SMB2/3 servers in userspace — no FUSE, no kernel modules — with
one metadata store and an on-disk journal per node, managed through an API.

[Website](https://dittofs.io) • [Quick Start](#quick-start) • [Documentation](#documentation) • [Features](#features) • [Contributing](docs/internals/contributing.md)

</div>

---

> ⚠️ **Experimental software, pre-1.0.** Not production ready. No security audit has
> been performed. APIs and on-disk formats may change without notice. Do not use for
> data you cannot afford to lose. See [Security](#security) and [FAQ](docs/guide/faq.md).

> [!NOTE]
> **`develop` is the development branch.** It holds v2, the next storage design: it changes
> every day, it is not production ready, and some features of the released versions do not
> work here yet. The released v1 code is at the latest `v0.x` tag: use the
> [latest release](https://github.com/cubbit/dittofs/releases/latest), or check out its tag on
> the [`main`](https://github.com/cubbit/dittofs/tree/main) branch.

## What is DittoFS?

Traditional file servers are welded to one storage layer and one access protocol.
DittoFS separates the two. A single server process:

- Speaks **NFSv3, NFSv4.0, NFSv4.1, NFSv4.2 and SMB2/3** at the same time, over the same data.
- Keeps metadata in **[BadgerDB](https://github.com/dgraph-io/badger)** on disk, one store for
  the whole node, or in a FoundationDB cluster.
- Keeps file content in an on-disk **journal** on the node's own devices.
- Runs **entirely in userspace**: no FUSE, no kernel modules, no special privileges.

> **This version is a rebuild (v2), in progress.** It serves no NFS or SMB yet, cannot
> create a share yet, and has no remote (S3) store wired yet. The protocol rows in
> [Features](#features) describe the released server. The [Quick Start](#quick-start)
> shows what runs today.

Two programs drive it:

| Program | Role |
|---------|------|
| **`dfs`** | The server. `dfs init` creates an installation; `dfs start` runs its node in the foreground: the management API and, once they are served, the NFS and SMB adapters. |
| **`dfsctl`** | The client of the management API. It manages users, groups, shares, stores and adapters on a running node. |

## Features

| Area | Status |
|------|--------|
| **NFSv3** | All 28 core procedures; embedded portmapper + mount protocol; NLM/NSM byte-range locking over TCP+UDP (opt-in) |
| **NFSv4.0** | Compound ops, ACLs, delegations, built-in byte-range locking |
| **NFSv4.1** | Sessions, sequence slots, backchannel |
| **NFSv4.2** | Extended attributes (RFC 8276); sparse files — ALLOCATE/DEALLOCATE/SEEK/READ_PLUS; CLONE/reflink (RFC 7862) |
| **SMB 2.0.2 / 2.1 / 3.0 / 3.0.2 / 3.1.1** | Multi-dialect negotiation, preauth integrity, compound requests |
| **SMB3 encryption** | AES-128/256-GCM and AES-128/256-CCM |
| **SMB3 signing** | AES-128-CMAC / AES-128-GMAC (HMAC-SHA256 for 2.x) |
| **SMB leases & oplocks** | Leases V2 with directory leasing; oplocks; byte-range locks |
| **SMB durable handles** | V1 and V2 for session resilience |
| **SMB security descriptors** | Windows ACL mapping via a shared cross-protocol ACL model |
| **Authentication** | AUTH_UNIX for NFS (Kerberos over RPCSEC_GSS not served yet); NTLM + Kerberos (SPNEGO) for SMB |
| **Active Directory** | Kerberos service keytab for AD-issued tickets; optional machine account (offline or online domain join) for NTLM pass-through; LDAP idmap (`idmap_ad` / `idmap_rid`) maps AD users and nested groups to the same Unix UID/GID over SMB and NFS |
| **Cross-protocol coordination** | Bidirectional lease/delegation breaks between SMB and NFS |
| **Metadata stores** | BadgerDB, on disk — one per node. FoundationDB, in the `-foundationdb` image and tarballs; still one node per installation today |
| **File content** | One engine per node over its on-disk journal devices; every share binds to it. No remote store is wired yet |
| **Share snapshots** | Not yet: v1's snapshots are gone, and [the snapshots design](docs/internals/rfcs/rfc-12-snapshots.md) describes the new ones |
| **Control plane** | A management API over the metadata store: users, groups, grants, shares, settings, long operations and an audit log; roles `admin`, `operator`, `viewer`; `dfsctl` is its client |
| **Observability** | Prometheus metrics endpoint, structured JSON logging |
| **Deployment** | Single static binary; Docker image. The Kubernetes operator does not run this version yet |

DittoFS passes the pjdfstest POSIX suite at 99.99% (8,788/8,789) on the BadgerDB
metadata store; the single expected failure is the NFSv3 32-bit timestamp limit
(year 2106). See the [FAQ](docs/guide/faq.md) for known limitations.

On the SMB side, DittoFS passes the Samba **smbtorture** and Microsoft **Windows
Protocol Test Suite (WPTS)** conformance batteries on the implementable surface —
every test that a single-node userspace VFS can satisfy. The remaining known-failures
are genuinely out-of-scope features (RSVD shared-VHD, Service Witness clustering,
Storage QoS, DFS namespaces, kernel oplocks, NTFS-internal pseudo-files) plus a
handful of upstream-Samba known-fails; none are fixable protocol gaps. See
[docs/guide/smb.md](docs/guide/smb.md) and [docs/guide/windows.md](docs/guide/windows.md).

## DittoFS PRO

The community edition is fully open source. **DittoFS PRO** adds a web
management UI and commercial support on top of the same engine, for teams
running DittoFS in production.

<div align="center">
  <img src="assets/pro-dashboard.png" alt="DittoFS PRO management console" width="820">
</div>

Manage users, shares, stores, and protocol adapters from one console. Learn
more at [dittofs.io/pro](https://dittofs.io/pro).

## Quick Start

This gets a node running and `dfsctl` signed in, on macOS or Linux, without root.
[Getting Started](docs/guide/getting-started.md) is the same walk-through with every
output and explanation.

### 1. Build

No release ships this version yet: Homebrew, the install script, the packages and the
Docker image install the released server, which works differently. Build from source
with [Go](https://go.dev/dl/) 1.26 or later:

```bash
cd ~
git clone --depth 1 --branch develop https://github.com/cubbit/dittofs.git
cd dittofs
go build -o dfs ./cmd/dfs
go build -o dfsctl ./cmd/dfsctl
export PATH="$PWD:$PATH"
```

### 2. Configure and create the installation

```bash
mkdir -p ~/.config/dittofs ~/dittofs-data/journal
cat > ~/.config/dittofs/config.yaml <<EOF
node:
  id: $(uuidgen | tr 'A-Z' 'a-z')
metadata:
  badger:
    path: $HOME/dittofs-data/metadata
journals:
  - path: $HOME/dittofs-data/journal
keys:
  storage:
    kind: local
    local:
      file: $HOME/dittofs-data/storage.key
EOF

export DITTOFS_ENCRYPTION_PASSPHRASE='correct horse battery staple'   # pick your own
dfs init
```

`dfs init` prints the `admin` account's generated password once. Copy it. It also
creates `~/dittofs-data/storage.key`: keep a copy of that file and the passphrase, since
losing either loses the installation.

### 3. Start the node

```bash
dfs start
```

It runs in the foreground, prints nothing, and logs to
`~/.local/state/dittofs/dittofs.log`. Stop it with Ctrl+C.

### 4. Sign in

In a second terminal, run `cd ~/dittofs && export PATH="$PWD:$PATH"` again, then:

```bash
dfsctl login --server http://127.0.0.1:8080 --username admin   # the password dfs init printed
dfsctl password change                                          # required for a generated password
dfsctl status
dfsctl user create alice --uid "$(id -u)"
```

Write `127.0.0.1`, not `localhost`: over plain `http`, `dfsctl` talks only to a
loopback IP address.

### Where it stops today

You cannot create a share yet. A share lives in a namespace on a remote store, and the
node does not accept settings for remote stores, namespaces or the NFS and SMB
adapters yet:

```console
$ dfsctl store create --setting remote.endpoint=https://s3.example.com
Error: dfsctl store create: 400 unknown_key: Bad Request: refused by the settings table (field remote.endpoint)
```

[Getting Started](docs/guide/getting-started.md#once-shares-can-be-created) shows how a
share will be created and mounted once they are.

### Default ports

| Port | Service |
|------|---------|
| `8080`  | Management API and health checks, on `127.0.0.1` (`api.listen`) |
| `9090`  | Prometheus metrics, on `127.0.0.1` (`metrics.listen`) |
| `12049` | NFS, once the NFS adapter is served |
| `12445` | SMB, once the SMB adapter is served |

The API also listens on a local socket, `dfs.sock` in the runtime directory
(`api.socket`). The NFS and SMB ports are adapter settings, changed through the
management API rather than the config file.

## Configuration model

DittoFS is configured in **two places**:

1. **The config file** (`~/.config/dittofs/config.yaml`) holds what the node needs before
   it opens any store: its ID, where its metadata and journals live, its key files, its
   listen addresses, logging and metrics. A `DITTOFS_` environment variable overrides any
   key, for example `DITTOFS_LOGGING_LEVEL=debug`.
2. **The management API** holds everything else: shares, users, groups, grants, adapters,
   stores, namespaces and settings. They live in the metadata store. A config file that
   names a setting is refused, and the message points you to the API.

See [docs/guide/configuration.md](docs/guide/configuration.md) for every key.

## Documentation

Full docs live in [`docs/`](docs/), split by audience.

### For users & operators — [`docs/guide/`](docs/guide/)

**Start here**

- [Getting Started](docs/guide/getting-started.md) — build, start a node and sign in
- [Installation](docs/guide/install.md) — build from source, packages, Docker, systemd, FoundationDB, uninstalling
- [Configuration](docs/guide/configuration.md) — every config key and environment variable, with defaults
- [CLI Reference](docs/guide/cli.md) — every `dfs` and `dfsctl` command (generated)
- [Choosing Stores](docs/guide/choosing-stores.md) — the metadata store and where file content lives
- [Sizing & Hardware](docs/guide/sizing.md) — how to size the machine that runs `dfs`

**Connect clients**

- [NFS](docs/guide/nfs.md) — mounting on Linux/macOS, options, Kerberos, NFS-over-TLS
- [SMB](docs/guide/smb.md) — mounting, dialects, encryption, signing
- [Windows clients](docs/guide/windows.md) — connecting from Windows
- [Identity: AD / LDAP / Kerberos](docs/guide/identity.md) — join a directory service
- [Access Control](docs/guide/access-control.md) — the cross-protocol ACL model
- [SMB ACL Fidelity](docs/guide/smb-acl-fidelity.md) — what the SMB Security Descriptor path round-trips (Works/Partial/Unsupported)

**Features & operations**

- [Snapshots](docs/guide/snapshots.md) · [Quotas](docs/guide/quotas.md) · [Encryption](docs/guide/encryption.md)
- [Security](docs/guide/security.md) — hardening checklist and secure configuration
- [Block Store Migration](docs/guide/block-store-migration.md) — what happened to v1's block layouts
- [Upgrading from v1](docs/guide/upgrading-from-v1.md) — not supported: what a start over v1 data says
- [Troubleshooting](docs/guide/troubleshooting.md) · [FAQ](docs/guide/faq.md) · [Glossary](docs/guide/glossary.md)

### For contributors — [`docs/internals/`](docs/internals/)

- [Architecture](docs/internals/architecture.md) — design, components, directory map
- [NFS protocol](docs/internals/nfs-protocol.md) · [SMB protocol](docs/internals/smb-protocol.md) — wire format, dispatch, internals
- [ACL design](docs/internals/acl-design.md) · [Security model](docs/internals/security-model.md) · [Encryption design](docs/internals/encryption-design.md)
- [Implementing Stores](docs/internals/implementing-stores.md) — metadata/block store contracts
- [Testing](docs/internals/testing.md): running, reading and adding tests with `dt`, and the conformance suites; the Windows checklists are in [testing-windows.md](docs/internals/testing-windows.md)
- [Debugging](docs/internals/debugging.md) — protocol interop and pcap-diff playbook
- [Issue priority triage](docs/internals/priorities.md) — how board items get their priority
- [Contributing](docs/internals/contributing.md) · [Releasing](docs/internals/releasing.md)

### Product

- [DittoFS Pro](docs/product/pro.md) — the managed dashboard built on open-source DittoFS

## Testing

```bash
test/harness/bin/dt run unit                     # the Go tests
test/harness/bin/dt services up kmip localstack   # the services integration and e2e need
test/harness/bin/dt run integration
test/harness/bin/dt run e2e                      # Linux and root; dt doctor says what else is missing
```

[docs/internals/testing.md](docs/internals/testing.md) is the guide: what each suite needs, how to
run and read a run, and the conformance suites. The Windows checklists are in
[testing-windows.md](docs/internals/testing-windows.md). See also [test/e2e/](test/e2e/) and, for
the benchmarks, [bench/README.md](bench/README.md).

## Security

DittoFS is experimental and has not been professionally audited.

- **Authentication:** AUTH_UNIX for NFS (Kerberos over RPCSEC_GSS is not served yet); NTLM and Kerberos
  (SPNEGO) for SMB. AD domain users can authenticate directly against a directory —
  see [docs/guide/identity.md](docs/guide/identity.md).
- **Transport encryption:** SMB3 encrypts its transport (AES-GCM/CCM). NFS-over-TLS
  (RFC 9289) and Kerberos privacy (`krb5p`) are not served yet, so run NFS over a VPN or a
  trusted network.
- **At rest:** file content and metadata are not encrypted on this version. Put the journal
  devices and the metadata directory on an encrypted filesystem (LUKS, FileVault). The node's
  own secrets, such as password hashes, are sealed under its key files. See
  [docs/guide/encryption.md](docs/guide/encryption.md).

See [docs/guide/security.md](docs/guide/security.md) for the full picture and recommendations.

## Contributing

Contributions are welcome — see [docs/internals/contributing.md](docs/internals/contributing.md) for development
setup, code structure, and testing guidelines.

## References

New to these protocols? Start with the [Glossary](docs/guide/glossary.md) for plain-language
definitions, then dive into the authoritative specs below.

- [RFC 1813](https://www.rfc-editor.org/rfc/rfc1813) — NFS Version 3
- [RFC 7530](https://www.rfc-editor.org/rfc/rfc7530) — NFS Version 4.0
- [RFC 8881](https://www.rfc-editor.org/rfc/rfc8881) — NFS Version 4.1
- [RFC 5531](https://www.rfc-editor.org/rfc/rfc5531) — ONC RPC Protocol
- [RFC 4506](https://www.rfc-editor.org/rfc/rfc4506) — XDR Standard
- [MS-SMB2](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/) — SMB2/3 protocol
- [MS-DTYP](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-dtyp/) — SID, ACL, ACE, and security descriptor formats
- [MS-NLMP](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-nlmp/) — NTLM authentication
- [RFC 4120](https://www.rfc-editor.org/rfc/rfc4120) — Kerberos V5 · [RFC 4178](https://www.rfc-editor.org/rfc/rfc4178) — SPNEGO · [RFC 2743](https://www.rfc-editor.org/rfc/rfc2743) — GSS-API

## DittoFS Pro

[**DittoFS Pro**](docs/product/pro.md) is the premium edition: a modern web dashboard
that brings the same capabilities as the `dfsctl` CLI — managing stores, shares,
adapters, and access control — to a point-and-click web UI. It builds on this
open-source server and ships as a single binary with the UI embedded. Learn more
at [dittofs.io](https://dittofs.io).

[![DittoFS Pro dashboard](docs/assets/pro/dashboard.png)](docs/product/pro.md)

## License

MIT — see [LICENSE](LICENSE).
