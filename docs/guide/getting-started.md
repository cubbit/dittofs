# Getting Started

This guide takes you from zero to a mounted DittoFS share in a few minutes. You'll
install the two binaries, start the server, create a share, and mount it over NFS and
SMB.

- **`dfs`** — the server daemon (protocol adapters + control-plane REST API).
- **`dfsctl`** — the command-line client that manages users, shares, stores, and adapters
  on a running server.

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

`dfs init` creates the installation, its metadata store and its `admin` user, in the
places the server's config file names: at least `node.id`, `keys.storage`,
`metadata.badger.path` and the `journals` devices (see [Configuration](configuration.md)).
`dfs start` then serves it. **Choose the admin password and hand it to `dfs init`** with
`--admin-password-file` or `--admin-password-env`. This is the recommended path for every
deployment and the only reliable one for Docker/Kubernetes/CI and systemd:

```bash
# Replace the example with your own password before running this command.
# A supplied password also skips the forced first-login password change.
DFS_ADMIN_PASSWORD=my-secure-password dfs init --admin-password-env DFS_ADMIN_PASSWORD
dfs start
```

With neither flag, `dfs init` generates a password and prints it once to standard output,
never to the log, and it must be changed at the first sign-in. To recover an
administrator's password, stop the node and run `dfs reset-admin` on the host that holds
the metadata store: it prints a new generated password once, which must be changed at the
first sign-in.

By default the server listens on these ports:

| Port    | Service |
|---------|---------|
| `12049` | NFS |
| `12445` | SMB |
| `8080`  | Control-plane REST API (login, management, health checks) |
| `9090`  | Prometheus metrics (on by default, loopback only; `metrics.listen`) |

## 3. Log in and set the admin password

> **Not available on this branch.** This build's `dfsctl` refuses each command in
> steps 3 to 6 with `not available until the CLI is rebuilt on the v2 API`; they
> show the released server's CLI.

```bash
dfsctl login --server http://localhost:8080 --username admin
```

If you handed `dfs init` the password above, log in with it and continue to step 4.
If `dfs init` generated the password instead, you **must** change it on first login
before any other command will work — until you do, the rest are rejected with HTTP 403:

```bash
dfsctl user change-password
```

## 4. Create a user

NFS write access maps to a host UID, so create a user bound to your current one:

```bash
dfsctl user create --username $(whoami) --host-uid
```

## 5. Stores: nothing to create

A node keeps file and directory metadata, and every control-plane record, in its one
**metadata store**, and file content in the **journal** on its `journals` devices. Both
come from the config file and are created by `dfs init`; there is no store to create per
share and no separate control-plane database. See [Choosing stores](choosing-stores.md).

## 6. Create a share and grant access

```bash
dfsctl share create --name /export --metadata default --block-store s3-remote
dfsctl share permission grant /export --user $(whoami) --level read-write
```

`--metadata` and `--block-store` name the released server's per-share stores; this
build has none to name (step 5).

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
dfsctl user create --username alice          # password prompted
dfsctl share permission grant /export --user alice --level read-write

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
