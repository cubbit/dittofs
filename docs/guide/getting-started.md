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

`dfs init` creates the installation, and its `admin` user, before the first start. **Choose
the password before you initialize**, from a file or from an environment variable you name.
This is the recommended path for every deployment and the only reliable one for
Docker/Kubernetes/CI and systemd:

```bash
# Replace the example with your own password before running this command.
DFS_ADMIN_PASSWORD=my-secure-password dfs init --admin-password-env DFS_ADMIN_PASSWORD
dfs start
```

If you set neither, a password is generated and printed once to standard output, never to
the log, and must be changed at the first sign-in. Over a finished installation `dfs init`
refuses and changes nothing; if no administrator can sign in, stop the node and run
`dfs reset-admin`.

By default the server listens on these ports:

| Port    | Service |
|---------|---------|
| `12049` | NFS |
| `12445` | SMB |
| `8080`  | Control-plane REST API (login, management, health checks) |
| `9090`  | Prometheus metrics (opt-in) |

## 3. Log in and set the admin password

```bash
dfsctl login --server http://localhost:8080 --username admin
```

If you set the password with `dfs init` above, log in with that password and
continue to step 4. If you let DittoFS generate the password instead, you **must**
change it on first login before any other command will work — until you do, the rest
are rejected with HTTP 403:

```bash
dfsctl user change-password
```

You can disable the forced change entirely with
`controlplane.require_initial_password_change: false` — see
[Configuration](configuration.md).

## 4. Create a user

NFS write access maps to a host UID, so create a user bound to your current one:

```bash
dfsctl user create --username $(whoami) --host-uid
```

## 5. Create stores

A share is built from a **metadata store** (where file metadata lives) and a **block
store** (the durable home for file content). Every share also keeps an on-disk **journal**
that absorbs writes before they reach the block store; it is provisioned automatically
under `blockstore.journal.path` and is not a store you create. Not sure which to pick?
See [Choosing stores](choosing-stores.md).

The CLI has no command to create either store today. The CLI creates no metadata store,
and block stores return as resources of the v2 management API.

## 6. Create a share and grant access

```bash
dfsctl share create --name /export --metadata default --block-store s3-remote
dfsctl share permission grant /export --user $(whoami) --level read-write
```

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
