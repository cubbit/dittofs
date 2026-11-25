# Glossary

Plain-language definitions of the protocol, security, and storage terms used
throughout the DittoFS documentation. Each entry links to the authoritative
specification for readers who want the full detail. Protocol-specific glossaries
also live in the [NFS guide](nfs.md#glossary) and the [SMB guide](smb.md#glossary).

## Filesystem protocols

| Term | Definition |
|------|------------|
| **NFS** (Network File System) | A protocol that lets a client read and write files on a remote server as if they were local. The released DittoFS server speaks NFSv3, v4.0, v4.1 and v4.2. [RFC 1813](https://www.rfc-editor.org/rfc/rfc1813) (v3), [RFC 7530](https://www.rfc-editor.org/rfc/rfc7530) (v4.0), [RFC 8881](https://www.rfc-editor.org/rfc/rfc8881) (v4.1) |
| **SMB** (Server Message Block) | Microsoft's network file-sharing protocol, used natively by Windows and macOS. The released DittoFS server speaks SMB 2.0.2 through 3.1.1. [MS-SMB2](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/) |
| **CIFS** (Common Internet File System) | An older name for SMB; the two terms are used interchangeably. [MS-SMB2](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/) |
| **Export** | An NFS term for a directory the server makes available to clients. The SMB equivalent is a "share". |
| **RPC** (Remote Procedure Call) | The request/response framing NFS is built on: a client invokes a numbered procedure on the server and gets a reply. DittoFS uses ONC RPC. [RFC 5531](https://www.rfc-editor.org/rfc/rfc5531) |
| **XDR** (External Data Representation) | The canonical binary encoding used to serialize RPC/NFS data structures on the wire. [RFC 4506](https://www.rfc-editor.org/rfc/rfc4506) |
| **NLM** (Network Lock Manager) | The sideband protocol NFSv3 uses for file locking (NFSv3 itself is stateless). [Open Group NLM](https://pubs.opengroup.org/onlinepubs/9629799/chap10.htm) |
| **NSM / statd** (Network Status Monitor) | The companion to NLM that tracks client/server reboots so locks can be reclaimed or released after a crash. [Open Group NSM](https://pubs.opengroup.org/onlinepubs/9629799/chap11.htm) |
| **Mount protocol** | The NFSv3 helper protocol that turns an export path into the initial file handle a client uses to start accessing files. |
| **File handle** | An opaque, server-issued identifier for a file or directory. Clients pass it back on every operation; they never interpret its bytes. |
| **stateid** | An NFSv4 identifier that names a piece of open/lock state on the server, so the server can tie an operation to a specific open or lock. |
| **WCC** (Weak Cache Consistency) | NFSv3 data returning a file's attributes both before and after an operation, so a client can cheaply tell whether its cached copy is still valid. |
| **Delegation** | An NFSv4 grant that lets a client cache a file (or directory) and operate on it locally until the server recalls the grant. The SMB analog is a lease/oplock. |

## SMB caching and resilience

| Term | Definition |
|------|------------|
| **Oplock** (opportunistic lock) | An SMB caching hint that lets a client buffer reads/writes locally until another client needs the file. |
| **Lease** | The SMB 2.1+ successor to oplocks: finer-grained (read/write/handle) caching rights. DittoFS implements Lease V2 with directory leasing. [MS-SMB2 §2.2.13.2.8](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/) |
| **Durable / persistent handle** | An open file handle a client can reclaim after a brief network drop (durable) or a server restart (persistent), avoiding data loss on transient failures. |

## Identity, permissions, and ACLs

| Term | Definition |
|------|------------|
| **ACL** (Access Control List) | The ordered list of rules that decides who may do what to a file. DittoFS uses one cross-protocol ACL model for both NFS and SMB. See [Access control](access-control.md). |
| **ACE** (Access Control Entry) | A single rule inside an ACL — e.g. "allow user X to read and write". [MS-DTYP §2.4.4](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-dtyp/) |
| **DACL** (Discretionary ACL) | The part of a security descriptor that grants or denies access. "Discretionary" because the object's owner controls it. [MS-DTYP §2.4.5](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-dtyp/) |
| **SACL** (System ACL) | The part of a security descriptor that defines auditing (which accesses get logged), set by an administrator rather than the owner. [MS-DTYP §2.4.5](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-dtyp/) |
| **Security descriptor** | The Windows/SMB structure that bundles a file's owner, group, DACL, and SACL together. [MS-DTYP §2.4.6](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-dtyp/) |
| **SID** (Security Identifier) | The Windows-style unique identifier for a user or group (e.g. `S-1-5-21-...`). DittoFS maps SIDs to and from Unix UIDs/GIDs and NFSv4 `user@domain` names. [MS-DTYP §2.4.2](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-dtyp/) |
| **Root-squash** | An export option that maps a client's `root` (UID 0) to an unprivileged user, so a remote root user can't gain root power on the server's files. |
| **All-squash** | An export option that maps *every* client user to a single unprivileged user, regardless of who they claim to be. |

## Authentication

| Term | Definition |
|------|------------|
| **Kerberos** | A ticket-based network authentication protocol. The released DittoFS server supports it for SMB (via SPNEGO); Kerberos for NFS (via RPCSEC_GSS) is not served yet. [RFC 4120](https://www.rfc-editor.org/rfc/rfc4120) |
| **Active Directory (AD)** | Microsoft's directory service. DittoFS authenticates AD domain users directly — Kerberos service tickets from a keytab, or NTLM passed through to a Domain Controller by a machine account. |
| **idmap** | The mapping from an authenticated principal or SID to a Unix UID/GID. DittoFS supports `idmap_ad` (RFC2307 `uidNumber`/`gidNumber` attributes) and `idmap_rid` (derive from the RID). See [identity.md](identity.md). |
| **GSS-API** | A standard programming interface that lets applications use security mechanisms like Kerberos without hard-coding them. [RFC 2743](https://www.rfc-editor.org/rfc/rfc2743) |
| **SPNEGO** (Simple and Protected GSS-API Negotiation) | The wrapper that lets an SMB client and server agree on whether to use Kerberos or NTLM. [RFC 4178](https://www.rfc-editor.org/rfc/rfc4178) |
| **NTLM / NTLMSSP** | Microsoft's challenge/response authentication scheme, used by SMB when Kerberos isn't available. NTLMSSP is the GSS-API wrapping of NTLM. [MS-NLMP](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-nlmp/) |
| **RPCSEC_GSS** | The NFS/RPC security flavor that carries GSS-API (Kerberos) authentication and optional integrity/privacy. [RFC 2203](https://www.rfc-editor.org/rfc/rfc2203) |
| **AUTH_UNIX** | The basic NFS authentication flavor that simply asserts a UID/GID — trusted only on a trusted network. |
| **TLS** (Transport Layer Security) | Encrypts and authenticates a network connection. DittoFS serves the management API over TLS, with the certificate and key files `api.tls.cert_file` and `api.tls.key_file` name; renewing the certificate is left to you. [RFC 8446](https://www.rfc-editor.org/rfc/rfc8446) |
| **mTLS** (mutual TLS) | TLS where both the server *and* the client present certificates, so the server authenticates the caller by its certificate. NFS-over-TLS can require one once it is served; the management API does not support it. |

## DittoFS terms

The words this documentation uses for DittoFS's own parts.

| Term | Definition |
|------|------------|
| **Installation** | Everything one `dfs init` creates: the metadata store, the journals, the key files and the `admin` account. One installation is served by one node today. |
| **Node** | A running `dfs start` process. It serves the management API and, once they are served, the NFS and SMB adapters. |
| **Config file** | The YAML file a node reads at start (`~/.config/dittofs/config.yaml` by default): its ID, paths, keys and listen addresses. See [Configuration](configuration.md). |
| **Management API** | The HTTP API through which shares, users, grants and settings are managed. `dfsctl` is its client. |
| **Context** | A named `dfsctl` sign-in: a server, an account and its session, kept in `~/.config/dfsctl/config.json`. |
| **Metadata store** | Where a node keeps the directory tree, file attributes, ACLs, users, shares and settings: [BadgerDB](https://github.com/dgraph-io/badger) on disk, or a FoundationDB cluster. One per installation. |
| **Journal** | The on-disk, append-only log where a node keeps file content, on the directories the `journals` key lists. On this version it holds the only copy of the data. |
| **Remote store** | An object store, such as S3, that will hold file content away from the node. Not wired yet on this version. |
| **Namespace** | The unit a share's content is counted in, on a remote store. A share is created in a namespace. |
| **Share** | A named directory tree that clients mount, over NFS by its path and over SMB by its name. |
| **Export policy** | A share's access rules: which client addresses may connect, which security flavours are accepted, squashing, read-only. |
| **Grant** | A user's or group's access to one share: `none`, `read`, `read-write` or `admin`. A new share admits nobody until a grant names them. |
| **Setting** | A value managed through the API rather than the config file, at a scope such as `installation` or `share:<name>`; for example `api.session_lifetime`. |
| **Wrapping key** | A key that seals the secrets the node keeps in its metadata store. `keys.storage` seals password hashes and the API's signing key; `keys.protocol` seals SMB password hashes and directory credentials. |
| **Passphrase** | The secret in `DITTOFS_ENCRYPTION_PASSPHRASE` that unlocks the local key files. Losing it, or the key files, loses the installation. |
| **State directory** | Where a node keeps its log and, by default, its metadata: `~/.local/state/dittofs` when run as yourself. |

## Implementation and tooling

External components and standards DittoFS builds on. Links go to each project's
own documentation for readers who want to go deeper.

| Term | What it is |
|------|------------|
| **S3 API** | The HTTP object-storage API a remote store will speak, against Amazon S3 and S3-compatible stores (MinIO, Ceph, …); not wired yet on this version. [S3 API reference](https://docs.aws.amazon.com/AmazonS3/latest/API/Welcome.html) |
| **BadgerDB** | An embedded key/value store (pure Go) — the metadata-store backend. [Badger](https://github.com/dgraph-io/badger) |
| **Cobra** | The Go library that builds the `dfs`/`dfsctl` command trees (subcommands, flags, help). [Cobra](https://github.com/spf13/cobra) |
| **Viper** | The configuration library that layers the config file, environment variables, and flags. [Viper](https://github.com/spf13/viper) |
| **XDG Base Directory Specification** | The Unix convention that places config under `~/.config` and state under `~/.local/state`; DittoFS follows it for its config and state directories. [XDG spec](https://specifications.freedesktop.org/basedir-spec/latest/) |
| **JWT** (JSON Web Token) | The signed token format of the management API's short-lived access tokens, which a sign-in returns with a refresh token. [RFC 7519](https://www.rfc-editor.org/rfc/rfc7519) |
| **Prometheus** | The metrics format/scraper DittoFS exposes operational metrics in. [Prometheus](https://prometheus.io/docs/) |

## See also

- [NFS guide](nfs.md#glossary) — NFS-specific terms (XID, verifier, COMPOUND, FSID, …)
- [SMB guide](smb.md#glossary) — SMB-specific terms (dialect, credit, transform header, KDF, …)
- [Access control](access-control.md) — the cross-protocol ACL model in depth
- [Encryption](encryption.md) — the key files and what they protect
