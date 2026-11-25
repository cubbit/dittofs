# Getting Started

This guide takes you from zero to a mounted DittoFS share in a few minutes. You'll
install the two binaries, start the server, create a share, and mount it over NFS and
SMB.

- **`dfs`** — the server. `dfs init` creates an installation and `dfs start` runs its
  node: the protocol adapters and the management API.
- **`dfsctl`** — the command-line client of the management API. It manages users,
  shares, stores and adapters on a running node, and nothing else.

> ⚠️ **Experimental, pre-1.0.** Not production ready, no security audit. Don't use it for
> data you can't afford to lose. See the [FAQ](faq.md) for known limitations.

## 1. Install

Pick one:

```bash
# Nix (temporary shell with dfs and dfsctl; no permanent install)
nix shell github:cubbit/dittofs

# Homebrew (macOS / Linux)
brew tap cubbit/tap
brew install --cask cubbit/tap/dfs cubbit/tap/dfsctl

# Quick install script (macOS / Linux)
curl -fsSL https://github.com/cubbit/dittofs/releases/latest/download/install.sh | sh
```

Docker, the Kubernetes operator, APT/YUM/Arch packages, and Scoop (Windows) are in the
[Installation guide](install.md).

**Build from source** (needs Go 1.26+):

```bash
git clone https://github.com/cubbit/dittofs.git
cd dittofs
go build -o dfs    cmd/dfs/main.go
go build -o dfsctl cmd/dfsctl/main.go
```

If you built from source, use `./dfs` and `./dfsctl` in the commands below.

## 2. Initialize and start the server

`dfs init` creates the installation: its metadata store, its journals, its
storage key file and its `admin` account. `dfs start` then serves it. Both read
the same config file (see [Configuration](configuration.md)). A small one:

```yaml
# ~/.config/dittofs/config.yaml
node:
  id: 6f1c2a4e-8d3b-4f7a-9c5e-2b8d4a6f0e13   # any UUID, generated once: uuidgen
metadata:
  badger:
    path: /var/lib/dittofs/metadata
journals:
  - path: /var/lib/dittofs/journal
keys:
  storage:
    kind: local
    local:
      file: /var/lib/dittofs/storage.key
```

`dfs init` refuses a config without `node.id` or `metadata.badger.path`. Each
journal's directory must exist before `dfs init` runs. The storage key file is
created by `dfs init`, wrapped under the passphrase in
`DITTOFS_ENCRYPTION_PASSPHRASE`, which `dfs start` needs too. Copy the key file
off the host: losing it or the passphrase loses the data.

Choose the admin password and hand it to `dfs init` with `--admin-password-file`
or `--admin-password-env`:

```bash
mkdir -p /var/lib/dittofs/journal
export DITTOFS_ENCRYPTION_PASSPHRASE='choose-a-passphrase'
# Replace the example with your own password before running this command.
DFS_ADMIN_PASSWORD=my-secure-password dfs init --admin-password-env DFS_ADMIN_PASSWORD
dfs start
```

`dfs start` runs in the foreground until it receives SIGINT or SIGTERM. Run it
under systemd or a container runtime to keep it in the background.

With neither password flag, `dfs init` generates a password and prints it once to
standard output, never to the log, and it must be changed at the first sign-in.
To recover an administrator's password, stop the node and run `dfs reset-admin`
on the host that holds the metadata store: it prints a new generated password
once, which must be changed at the first sign-in.

The node listens on these addresses by default:

| Address | Service |
|---------|---------|
| `127.0.0.1:8080` | Management API (sign-in, management, health checks); `api.listen` |
| `dfs.sock` in the runtime directory | Management API over a local socket; `api.socket` |
| `127.0.0.1:9090` | Prometheus metrics; `metrics.listen` |
| port `12049` | NFS, once the NFS adapter is enabled and served |
| port `12445` | SMB, once the SMB adapter is enabled and served |

`GET /health/ready` answers `ready` once the node serves.

## 3. Log in and set the admin password

`dfsctl` reaches the node at an `https` URL, at an `http` URL on a loopback IP
address, or through the local socket's path. A host name is never treated as
loopback, so write `127.0.0.1`, not `localhost`:

```bash
dfsctl login --server http://127.0.0.1:8080 --username admin
```

If you handed `dfs init` the password, log in with it and continue to step 4. If
`dfs init` generated it, change it first. Until you do, the account can only
change its password and sign out:

```bash
dfsctl password change
```

## 4. Create a user

NFS write access maps to a host UID, so create a user bound to your current one:

```bash
dfsctl user create $(whoami) --uid $(id -u)
```

## 5. Stores and namespaces

A node keeps file and directory metadata, and every control-plane record, in its
one **metadata store**, and file content in the **journal** on its `journals`
devices. Both come from the config file and are created by `dfs init`. See
[Choosing stores](choosing-stores.md).

A share's content is counted in a **namespace**, which lives on a **remote store**.
On this branch the node's settings table does not declare the remote store's or
the namespace's settings yet, so `dfsctl store create` and
`dfsctl namespace create` are refused with `unknown_key`. Until they are, no
namespace can be created through the API, and step 6 cannot run on a new
installation.

## 6. Create a share and grant access

With a namespace in place, a share is created with its export policy, and then
granted. A new share admits nobody until a grant names them:

```bash
dfsctl share create export --path /export --namespace <namespace-id> --flavor sys --client 0.0.0.0/0=read-write --client ::/0=read-write
dfsctl grant set export --user $(whoami) --access read-write
```

`dfsctl namespace list` prints each namespace's ID.

## 7. Mount over NFS

> **Not served on this branch.** This build serves no file protocol; this step
> describes the released server (see the [NFS guide](nfs.md)).

<details>
<summary><strong>Client prerequisites (Linux)</strong></summary>

macOS includes the NFS and SMB clients. On Linux, install them before mounting:

```bash
sudo apt install -y nfs-common cifs-utils   # Debian / Ubuntu
sudo dnf install -y nfs-utils cifs-utils    # RHEL / Fedora
```

</details>

```bash
# Linux
sudo mkdir -p /mnt/nfs
sudo mount -t nfs -o tcp,port=12049,mountport=12049 localhost:/export /mnt/nfs
echo "Hello DittoFS!" > /mnt/nfs/hello.txt

# macOS
mkdir -p /tmp/nfs
sudo mount -t nfs -o tcp,port=12049,mountport=12049,resvport,nolock localhost:/export /tmp/nfs
echo "Hello DittoFS!" > /tmp/nfs/hello.txt
```

Writes land in the share's journal first and sync to S3 in the background. More mount
options, Kerberos, and NFS-over-TLS are in the [NFS guide](nfs.md).

## 8. Mount over SMB

> **Not served on this branch.** This build serves no file protocol; this step
> describes the released server (see the [SMB guide](smb.md)).

SMB always requires user authentication:

```bash
dfsctl user create alice
dfsctl user set-password alice                # password prompted
dfsctl grant set export --user alice --access read-write

# Linux: keep alice's password in a private credentials file, never on the command line
printf 'username=alice\npassword=ALICE_PASSWORD\n' > ~/.smbcredentials
chmod 600 ~/.smbcredentials
sudo mkdir -p /mnt/smb

# uid/gid make the mounted files yours locally, so you can write as a regular user
sudo mount -t cifs //localhost/export /mnt/smb \
  -o port=12445,credentials=$HOME/.smbcredentials,vers=3.1.1,uid=$(id -u),gid=$(id -g)

# macOS (prompts for the password)
mkdir -p /tmp/smb
mount -t smbfs //alice@localhost:12445/export /tmp/smb
```

Connecting from **Windows**? See [Windows clients](windows.md). Dialects, encryption, and
signing are in the [SMB guide](smb.md).

## Next steps

- [Configuration](configuration.md): every config key and flag, with defaults.
- [CLI reference](cli.md): every `dfs` and `dfsctl` command.
- [Choosing stores](choosing-stores.md): metadata and block store trade-offs.
- [Identity: AD / LDAP / Kerberos](identity.md): connect to a directory service.
- [Snapshots](snapshots.md), [Quotas](quotas.md), [Encryption](encryption.md).
- [Mount at boot](persistent-mounts.md): make NFS and SMB mounts survive a reboot.
- [Troubleshooting](troubleshooting.md): when a mount or permission won't cooperate.
