# Getting Started

This guide takes you from nothing to a running DittoFS node that you manage with
`dfsctl`. It takes about ten minutes on macOS or Linux, needs no root, and keeps
everything under your home directory. Follow it top to bottom.

> **Where this guide stops today.** This version of DittoFS cannot create a share
> yet, and it serves no NFS or SMB. You will end with the node running, signed in
> as the administrator, with a user created. [Step 7](#7-where-you-stop-today)
> shows why, and [after that](#once-shares-can-be-created) shows the commands
> that will create and mount a share.

> ⚠️ **Experimental, pre-1.0.** Not production ready, no security audit. Don't use it
> for data you can't afford to lose. See the [FAQ](faq.md) for known limitations.

DittoFS has two programs:

| Program | What it does |
|---------|--------------|
| `dfs` | The server. `dfs init` creates an installation once; `dfs start` runs it. |
| `dfsctl` | The client. It talks to a running node's management API to manage users, shares and settings. |

## 1. Build from source

This guide describes the new node (v2), which no release ships yet. Homebrew, the
install script, the packages and the Docker image all install the released server,
which works differently. So build both programs from the `feat/storage-v2` branch.
You need [Go](https://go.dev/dl/) 1.26 or later and `git`.

```bash
cd ~
git clone --depth 1 --branch feat/storage-v2 https://github.com/cubbit/dittofs.git
cd dittofs
go build -o dfs ./cmd/dfs
go build -o dfsctl ./cmd/dfsctl
export PATH="$PWD:$PATH"     # so the steps below can run dfs and dfsctl
```

Check that both run. Each prints its version, `dev` for a build from source:

```bash
dfs version
dfsctl version
```

## 2. Write the config file

`dfs` reads its config from `~/.config/dittofs/config.yaml`. This one keeps the
node's data under `~/dittofs-data`, apart from the clone in `~/dittofs`. Copy the
whole block: the shell fills in your home directory and a new node ID.

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
```

What each part is for:

| Key | Meaning |
|-----|---------|
| `node.id` | A UUID that names this node. Generate it once and never change it. |
| `metadata.badger.path` | Where the node keeps file metadata, users, shares and settings. |
| `journals` | Where the node keeps file content. The directory must exist before `dfs init`. |
| `keys.storage` | A key file that protects the secrets in the metadata store. `dfs init` creates it. |

Paths must be absolute: `~` is not expanded inside the file, which is why the
block above lets the shell write `$HOME` out. If your shell sets `XDG_CONFIG_HOME`,
`dfs` looks in `$XDG_CONFIG_HOME/dittofs/config.yaml` instead, so write the file
there. Likewise, with `XDG_STATE_HOME` set, the log below is under
`$XDG_STATE_HOME/dittofs`, and `dfsctl` keeps its session under
`$XDG_CONFIG_HOME/dfsctl`. No `uuidgen`? On Linux use
`cat /proc/sys/kernel/random/uuid`. Every key is in
[Configuration](configuration.md).

## 3. Create the installation

`dfs init` creates the metadata store, prepares the journal, writes the storage
key file and creates the `admin` account. It needs a passphrase, which protects the
key file. `dfs start` needs the same passphrase every time it runs.

```bash
export DITTOFS_ENCRYPTION_PASSPHRASE='correct horse battery staple'   # pick your own
dfs init
```

```console
created the storage key file /home/alice/dittofs-data/storage.key
Copy it off this host now and keep its passphrase: losing either loses the data, and nothing recovers it.
The administrator "admin"'s generated password, shown once; it must be changed at the first sign-in:
ueDzOVgh8ObDp0kAxMJc-UsgMX3K_cfL
```

Copy the password now: it is printed only here, never to the log. Take the
warning seriously too. The key file and the passphrase together unlock the
node's secrets. Lose either one and the installation cannot be opened again.

You can choose the first password yourself instead, with
`--admin-password-file` or `--admin-password-env`. See
[`dfs init`](cli.md#dfs-init).

## 4. Start the node

Run `dfs start` in the same terminal, so it sees the passphrase:

```bash
dfs start
```

It runs in the foreground and prints nothing to the terminal. It writes its log to
`~/.local/state/dittofs/dittofs.log`. Leave it running and open a second terminal.
The node answers `starting` for the first few seconds, then `ready`:

```console
$ curl http://127.0.0.1:8080/health/ready
ready
```

The node listens on these addresses:

| Address | What it serves | Config key |
|---------|----------------|------------|
| `127.0.0.1:8080` | Management API and health checks | `api.listen` |
| `dfs.sock` in the runtime directory (`dfs config show` prints the path) | Management API, local socket | `api.socket` |
| `127.0.0.1:9090` | Prometheus metrics | `metrics.listen` |

## 5. Sign in and change the password

In the second terminal, make `dfsctl` available again and sign in as `admin` with
the password `dfs init` printed:

```bash
cd ~/dittofs && export PATH="$PWD:$PATH"     # the clone from step 1
dfsctl login --server http://127.0.0.1:8080 --username admin
```

```console
password:
Logged in to http://127.0.0.1:8080 as admin (context default)
The password must be changed before any other call: run 'dfsctl password change'
```

Write `127.0.0.1`, not `localhost`: `dfsctl` sends a password over plain `http`
only to a loopback IP address.

A generated password must be changed before anything else works:

```console
$ dfsctl password change
current password:
new password:
new password again:
Password changed
```

`dfsctl` keeps the session in `~/.config/dfsctl/config.json`, so later commands
need no password.

## 6. Check the node and create a user

```console
$ dfsctl status
READY  CONDITION  SCOPE  SUBJECT  CAUSE  DETAIL
ready

$ dfsctl node list
ID                                    ADDRESS         BINARY  DOMAIN  LAPSED
0ab994b4-617e-40d4-a181-f46f66728314  127.0.0.1:8080                  no
```

Create a user for yourself. NFS identifies a client user by its numeric user ID,
so give the user the same ID you have on the machine you will mount from
(`id -u` prints it):

```console
$ dfsctl user create alice --uid "$(id -u)"
NAME   ID                                    UID   ROLE  EMAIL  DISABLED
alice  c763989b-76b9-4204-ac7e-a7e2a066d128  1000               no
```

<a id="5-stores-and-namespaces"></a>

## 7. Where you stop today

A share keeps its files in a **namespace**, and a namespace lives on a **remote
store**. On this version, the node does not accept settings for remote stores,
namespaces or the NFS and SMB adapters yet. So you cannot create a store, a
namespace or a share, and you cannot turn an adapter on:

```console
$ dfsctl store create --setting remote.endpoint=https://s3.example.com
Error: dfsctl store create: 400 unknown_key: Bad Request: refused by the settings table (field remote.endpoint)

$ dfsctl adapter list
PROTOCOL  ENABLED  SETTINGS  NODE                                  STATE
nfs       no                 0ab994b4-617e-40d4-a181-f46f66728314  stopped
smb       no                 0ab994b4-617e-40d4-a181-f46f66728314  stopped
```

Everything up to here works and is kept: users, groups, grants and settings you
create now stay in the metadata store. To read about stores and namespaces, see
[Choosing stores](choosing-stores.md).

## 8. Stop and clean up

In the second terminal, sign out while the node still runs:

```bash
dfsctl logout
```

Then press **Ctrl+C** in the first terminal. The node stops cleanly and logs
`dfs: stopped`. To start it again later, export the same passphrase and run
`dfs start`.

To remove everything this guide created:

```bash
rm -rf ~/dittofs-data ~/.config/dittofs ~/.local/state/dittofs ~/.config/dfsctl
rm -rf ~/dittofs     # the clone and the binaries, if you are done with them
```

## Once shares can be created

> **Not available yet on this version.** The commands below show how a share will
> be created and mounted once remote stores, namespaces and adapters can be set.
> They describe the released server's behaviour; see the [NFS guide](nfs.md) and
> the [SMB guide](smb.md).

Create a share named `archive` in a namespace, open it to every client address,
and give `alice` read-write access. A new share admits nobody until a grant names
them:

```bash
dfsctl namespace list        # prints each namespace's ID
dfsctl share create archive --path /archive \
  --namespace 3f6d2b1a-7c4e-4e2a-9b8d-1a2b3c4d5e6f \
  --flavor sys --client 0.0.0.0/0=read-write --client ::/0=read-write
dfsctl grant set archive --user alice --access read-write
```

Mount it over NFS. The NFS port is `12049` by default, so the mount names it:

```bash
# Linux (install nfs-common or nfs-utils first)
sudo mkdir -p /mnt/archive
sudo mount -t nfs -o tcp,port=12049,mountport=12049 127.0.0.1:/archive /mnt/archive

# macOS
mkdir -p /tmp/archive
sudo mount -t nfs -o tcp,port=12049,mountport=12049,resvport,nolock 127.0.0.1:/archive /tmp/archive
```

Or let `dfsctl` build the mount command for you:

```bash
dfsctl share mount archive /mnt/archive --protocol nfs
```

SMB always needs a password, and the SMB port is `12445` by default. Setting a
password needs `keys.protocol` in the config file, because the node stores the
password's NT hash under that key (see [Encryption](encryption.md#local-key-files)):

```bash
dfsctl user set-password alice              # prompts for the password
dfsctl share mount archive /mnt/archive --protocol smb --username alice
```

Connecting from Windows? See [Windows clients](windows.md).

## Next steps

- [Configuration](configuration.md): every config key, with defaults.
- [CLI reference](cli.md): every `dfs` and `dfsctl` command.
- [Installation](install.md): packages, Docker, systemd, uninstalling.
- [Troubleshooting](troubleshooting.md): when something refuses to work.
- [Security](security.md): before you expose the node beyond your own machine.
