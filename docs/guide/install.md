# Deployment

This guide covers running DittoFS beyond a local source build: package managers, Docker,
and the Kubernetes operator. For the quick local path (Nix / Homebrew / source build), see
the [README](../../README.md). For configuration details, see [CONFIGURATION.md](configuration.md).

DittoFS ships two binaries: `dfs`, the server, and `dfsctl`, the client of its management
API. Most package managers install both.

> **Not served on this branch.** This build serves no file protocol. The steps that
> mount a share, and the NFS and SMB ports below, describe the released server.

## Default ports

| Port | Service |
|------|---------|
| `12049` | NFS |
| `12445` | SMB |
| `8080`  | Management API and health probes (`api.listen`, `127.0.0.1:8080`) |
| `9090`  | Prometheus metrics (`metrics.listen`, `127.0.0.1:9090`) |

DittoFS defaults to these **non-privileged** ports (>1024) so it starts without
root. The API and metrics addresses are bootstrap keys of the config file (see
[Configuration](configuration.md#the-keys)). The NFS and SMB ports are not: each is its
adapter's `port` setting, `adapters.nfs.port` and `adapters.smb.port`, kept in the
metadata store and changed through the management API.

## Running on standard ports (production)

In production we recommend running on the **standard, well-known ports** so
clients connect with no special options:

| Service | Default (unprivileged) | Standard (production) |
|---------|------------------------|-----------------------|
| NFS | `12049` | `2049` |
| SMB | `12445` | `445` |

On standard ports, mount commands drop the `port=` / `mountport=` options:

```bash
# Non-standard (default)
sudo mount -t nfs -o vers=4.1,tcp,port=12049 server:/export /mnt/point
# Standard ports — no port option needed
sudo mount -t nfs -o vers=4.1 server:/export /mnt/point
```

Two things are required: **(1)** free the standard ports on the host, and
**(2)** let DittoFS bind a port below 1024.

### 1. Free the standard ports on the host

Ports 2049 and 445 are usually claimed by the OS's own NFS, SMB, and
RPC-bind services. Stop and disable them, or DittoFS cannot bind:

```bash
# Linux (systemd) — disable the kernel NFS server, Samba, and rpcbind
sudo systemctl disable --now nfs-server rpcbind rpcbind.socket smbd nmbd

# Confirm nothing else holds the ports
sudo ss -tulpn | grep -E ':(2049|445)\b'
```

```bash
# macOS — turn off built-in File Sharing (SMB) and nfsd
sudo nfsd stop && sudo nfsd disable
# Disable SMB file sharing in System Settings → General → Sharing, or:
sudo launchctl disable system/com.apple.smbd
```

### 2. Bind the privileged ports — by deployment mode

#### Single binary

Set each adapter's port through the management API, then grant the binary
permission to bind low ports:

```bash
dfsctl adapter set nfs --setting port=2049
dfsctl adapter set smb --setting port=445
```

A port is a setting, not a host fact, so a config file that names
`adapters.nfs.port` is refused at start. On this branch the API also refuses both
commands with `unknown_key`, until the node declares the adapters' settings.

Binding a port below 1024 needs privilege. Either run as root, or — preferred —
grant just the bind capability so the process stays unprivileged:

```bash
# Grant the capability to the binary (no root at runtime)
sudo setcap 'cap_net_bind_service=+ep' /usr/local/bin/dfs

# Or, under systemd, add to the [Service] section:
#   AmbientCapabilities=CAP_NET_BIND_SERVICE
```

#### Docker

The process is root **inside** the container, so it can bind low ports there
directly. Set the adapter ports to the standard values, as above, and publish them
1:1, with the volumes and the config of [Single container](#single-container):

```bash
docker run -d -p 2049:2049 -p 445:445 -p 8080:8080 marmos91c/dittofs:latest
```

The Docker **host** must still not run its own `nfsd` / `smbd` / `rpcbind` on
those ports — see [step 1](#1-free-the-standard-ports-on-the-host).

#### Kubernetes (operator)

In-cluster there is no host service to disable, and the pod never needs to bind
a privileged port: set the adapter ports in the `DittoServer` CR and let the
operator's **Service** publish them. With a `LoadBalancer` (or `NodePort`)
service, external clients reach the standard ports through the load balancer.

```yaml
apiVersion: dittofs.dittofs.com/v1alpha1
kind: DittoServer
metadata:
  name: dittofs
spec:
  nfs:
    port: 2049
  smb:
    enabled: true
    port: 445
  service:
    type: LoadBalancer   # publishes the adapter ports externally
```

The operator wires each adapter port into the Service. For NFSv3 portmapper
exposure (port 111) and the complete CR schema, see the
[Kubernetes operator](#kubernetes-operator) section and the chart under
`k8s/dittofs-operator/`.

## Package managers

### Debian / Ubuntu (APT)

```bash
curl -fsSL https://s3.cubbit.eu/dittofs-binaries/apt/dittofs.gpg.key \
  | gpg --dearmor --yes | sudo tee /usr/share/keyrings/dittofs.gpg > /dev/null
echo "deb [signed-by=/usr/share/keyrings/dittofs.gpg] https://s3.cubbit.eu/dittofs-binaries/apt stable main" \
  | sudo tee /etc/apt/sources.list.d/dfs.list
sudo apt update && sudo apt install dfs
sudo systemctl enable --now dfs
```

### RHEL / Fedora (YUM)

```bash
sudo curl -fsSLo /etc/yum.repos.d/dfs.repo https://s3.cubbit.eu/dittofs-binaries/rpm/dfs.repo
sudo yum install dfs
sudo systemctl enable --now dfs
```

### Arch Linux

```bash
# Download the latest .pkg.tar.zst from GitHub Releases, then:
sudo pacman -U dfs_<version>_amd64.pkg.tar.zst
sudo systemctl enable --now dfs
```

### Windows (Scoop)

```powershell
scoop bucket add cubbit https://github.com/cubbit/scoop-bucket
scoop install dfs       # server
scoop install dfsctl    # client CLI
```

When installed via the system package managers, the server runs under systemd as the `dfs`
service, which runs `dfs start` in the foreground; systemd stops it with a
signal. The unit names no config file, so `dfs start` reads the default path; add
`--config` or `DITTOFS_CONFIG` in a drop-in to name one. Run `dfs init` once before the first start, with the same config file, handing
it the admin password with `--admin-password-file` or `--admin-password-env` (see
[Getting started](getting-started.md#2-initialize-and-start-the-server)). With neither, `dfs init` prints a
generated password once to its standard output and never to the log, so run it where
you can read that output; `dfs reset-admin`, with the node stopped, prints a new one.

## Docker

Pre-built multi-architecture images (`linux/amd64`, `linux/arm64`) are published on Docker Hub.

### Single container

The image runs `dfs` as user `65532`, and its default command is
`start --config /config/config.yaml`. A container needs a config file that binds the
API to every interface, which needs a certificate, and names the node, the journal and
the two wrapping keys:

```yaml
node:
  id: 6f1c2a4e-8d3b-4f7a-9c5e-2b8d4a6f0e13   # any UUID, generated once: uuidgen
api:
  listen:
    - 0.0.0.0:8080
  tls:
    cert_file: /config/tls/api.crt
    key_file: /config/tls/api.key
metadata:
  badger:
    path: /data/metadata/store
journals:
  - path: /data/blocks/journal
keys:
  protocol:
    kind: local
    local:
      file: /config/keys/protocol.key
  storage:
    kind: local
    local:
      file: /config/keys/storage.key
```

An API address that is not loopback is served over TLS only, so the certificate is
required; the image ships none. For these examples, make one that names `localhost`. Write the protocol key file before `init`, with the
helper [Encryption: local key files](encryption.md#local-key-files) shows. `dfs init`
creates the storage key file and prints once where it is; copy it off the host. Both
key files are unlocked with `DITTOFS_ENCRYPTION_PASSPHRASE`. The `keys` and `tls`
directories must be readable by user `65532`, and `keys` writable too.

Initialize the installation once, then run the server:

```bash
docker pull marmos91c/dittofs:latest

docker run --rm \
  -e DFS_ADMIN_PASSWORD=my-secure-password \
  -e DITTOFS_ENCRYPTION_PASSPHRASE \
  -v ~/.config/dittofs/config.yaml:/config/config.yaml:ro \
  -v ./keys:/config/keys \
  -v ./tls:/config/tls:ro \
  -v dittofs-metadata:/data/metadata \
  -v dittofs-blocks:/data/blocks \
  -v dittofs-state:/data/state \
  marmos91c/dittofs:latest init --config /config/config.yaml \
  --admin-password-env DFS_ADMIN_PASSWORD

docker run -d \
  --name dittofs \
  -e DITTOFS_ENCRYPTION_PASSPHRASE \
  -p 8080:8080 \
  -v ~/.config/dittofs/config.yaml:/config/config.yaml:ro \
  -v ./keys:/config/keys \
  -v ./tls:/config/tls:ro \
  -v dittofs-metadata:/data/metadata \
  -v dittofs-blocks:/data/blocks \
  -v dittofs-state:/data/state \
  marmos91c/dittofs:latest

curl --cacert tls/api.crt https://localhost:8080/health/ready   # "ready" once it serves
```

Without `--admin-password-env`, `init` prints a generated password once, to that
terminal only: it never reaches the log or `docker logs`. The image's health check
polls `/health/live`. Metrics stay on the container's loopback (`127.0.0.1:9090`).
`docker stop` sends the signal that stops the node.

`dittofs-blocks` carries the journal, which holds the only copy of every byte written,
so never leave it in the container's writable layer. These volumes only receive data if
the config points at directories on them, never at their mount points: hence
`metadata.badger.path` set to `/data/metadata/store` and `/data/blocks/journal` under
`journals` (the image creates that directory; `dfs init` needs it to exist).

Then sign in from the host, naming an https URL the certificate names:

```bash
dfsctl login --server https://localhost:8080 --cacert tls/api.crt --username admin
```

**Image tags:**

- `marmos91c/dittofs:latest` — latest stable release
- `marmos91c/dittofs:vX.Y.Z` — a specific version
- `marmos91c/dittofs:vX.Y` — latest patch of a minor version
- `marmos91c/dittofs:vX` — latest minor of a major version

### Docker Compose

The repository's `docker-compose.yml` does not run on this branch. Its
`docker/config.yaml` names neither the wrapping keys nor the API's certificate, it runs
no `dfs init`, and its provisioning sidecar (`docker/bootstrap.sh`) calls the released
server's routes, which this API does not serve. Use the single container above.

## FoundationDB metadata

The default build keeps its metadata in Badger, on one node's disk. To keep it in a
FoundationDB cluster (`metadata.backend: foundationdb`) you need a `dfs` built with the
`foundationdb` tag, which links FoundationDB's client library. Releases publish two such
builds next to the default ones: an image and two tarballs. The package managers, the
quick install script and the default image do not carry it: a `dfs` without the tag
refuses `metadata.backend: foundationdb` at `dfs init` and `dfs start`, naming the tag.

**Requirements:**

- A FoundationDB **7.3** cluster, and its cluster file (usually `/etc/foundationdb/fdb.cluster`).
  The client is 7.3 too, and a 7.3 client reaches only a 7.3 cluster.
- **An installation owns its whole cluster.** `dfs` writes with no key prefix, so the cluster
  must hold no other data, and no second installation may share it.
- Linux. The tarballs need glibc 2.34 or later (Debian 12, Ubuntu 22.04, RHEL 9 or newer).

### The image

```bash
docker pull marmos91c/dittofs:vX.Y.Z-foundationdb
```

The image is published for `linux/amd64` and `linux/arm64` under the tag `<release tag>-foundationdb`
(`vX.Y.Z-foundationdb`). It carries the 7.3 client, runs as the non-root user `65532`, and has
the same ports, volumes and health check as the default image, plus one volume:

- `/etc/foundationdb`: put the cluster file `fdb.cluster` here, and name it as
  `metadata.foundationdb.cluster_file`. The client rewrites the cluster file when the cluster's
  coordinators change, so mount a directory there that user `65532` can write, not the file
  alone and not read-only.

### The tarball

`dfs-foundationdb_<version>_Linux_x86_64.tar.gz` and `dfs-foundationdb_<version>_Linux_arm64.tar.gz`
are on the release page, listed in the release's `checksums.txt` like every other asset. Each holds
the `dfs` binary, the README, the LICENSE and a `FOUNDATIONDB.txt` note. It does **not**
carry `libfdb_c.so`: install the `foundationdb-clients` 7.3 package from
[FoundationDB's releases](https://github.com/apple/foundationdb/releases). The deb puts it in
`/usr/lib` and the rpm in `/usr/lib64`, both on the default library path.

```bash
sha256sum -c --ignore-missing checksums.txt
tar -xzf dfs-foundationdb_<version>_Linux_x86_64.tar.gz
```

### Configure, initialize, start

Select the backend and name the cluster file. Only the `metadata.foundationdb` block is read
with `foundationdb`, and a `metadata.badger` key beside it is refused:

```yaml
node:
  id: 7c0f3a52-9d4e-4b8e-8a31-5b6f2f0e9c11   # a UUID of your own
metadata:
  backend: foundationdb
  foundationdb:
    cluster_file: /etc/foundationdb/fdb.cluster
journals:
  - path: /data/blocks/journal
keys:
  protocol:
    kind: local
    local:
      file: /config/keys/protocol.key
  storage:
    kind: local
    local:
      file: /config/keys/storage.key
```

The journals stay on local disk: FoundationDB holds the metadata, not the journal. See
[Configuration](configuration.md#foundationdb-metadata) for every key.

Both key files are unlocked with `DITTOFS_ENCRYPTION_PASSPHRASE`, which `dfs init` and
`dfs start` both need. `dfs init` creates the storage key file over a new cluster, and prints
once where it is; copy it off the host, as
[Encryption: local key files](encryption.md#local-key-files) says. Its directory has to exist and
be writable. The protocol key file is not created by `init`: write it first, with the helper that
section shows.

Then, with the tarball's binary:

```bash
export DITTOFS_ENCRYPTION_PASSPHRASE=...   # the key files' passphrase
./dfs init --config /etc/dittofs/config.yaml --admin-password-env DFS_ADMIN_PASSWORD
./dfs start --config /etc/dittofs/config.yaml
```

or with the image, as for the [default image](#single-container) with no metadata volume, the
cluster file's directory mounted, and the key files' directory mounted where the configuration
names them. Both host directories must be writable by user `65532`:

```bash
docker run --rm -e DFS_ADMIN_PASSWORD=my-secure-password \
  -e DITTOFS_ENCRYPTION_PASSPHRASE \
  -v ~/.config/dittofs/config.yaml:/config/config.yaml:ro \
  -v ./foundationdb:/etc/foundationdb \
  -v ./keys:/config/keys \
  -v dittofs-blocks:/data/blocks \
  marmos91c/dittofs:vX.Y.Z-foundationdb init --config /config/config.yaml \
  --admin-password-env DFS_ADMIN_PASSWORD
```

Run the server the same way with `start`, the ports and the other volumes of
[Single container](#single-container). `init` and `start` wait up to 30 seconds for the cluster, and
a wrong cluster file ends in a refusal naming it rather than a hang.

For a cluster that requires TLS, name the client's certificate, its key and the CA file under
`metadata.foundationdb.tls` (see [Configuration](configuration.md#foundationdb-metadata)). In the
image, mount them read-only and name their paths inside the container. A file that is missing is
refused, named, before `dfs` connects.

## Kubernetes operator

DittoFS has an official operator that manages the deployment lifecycle, configuration via
Custom Resources, persistent volume claims for metadata and block stores, and service
exposure for the NFS/SMB protocols.

```bash
# From the operator directory
cd k8s/dittofs-operator
make deploy

# Create a DittoFS instance
kubectl apply -f config/samples/dittofs_v1alpha1_dittofs.yaml

# Check status
kubectl get dittofs
```

`make deploy` uses the published operator image (`marmos91c/dittofs-operator:latest`) by
default. To build and deploy from source instead, point `IMG` at your own registry:
`make docker-build docker-push IMG=<your-registry>/dittofs-operator:tag` then
`make deploy IMG=<your-registry>/dittofs-operator:tag`.

See the [`k8s/dittofs-operator/`](../../k8s/dittofs-operator/) directory for the CRD reference,
RBAC, and Helm chart configuration.

## Uninstall

Removing DittoFS takes three steps:

1. Stop the server.
2. Remove the binaries.
3. Optionally, delete the data.

Removing the binaries never deletes your data.

Before you start, if you plan to delete the data, note the directories
`metadata.badger.path` and `journals` name in the server's config file;
`dfs config show` prints them.

### 1. Stop the server and unmount shares

On every client, unmount NFS and SMB shares first. A mount left behind hangs
once the server is gone:

```bash
sudo umount /mnt/nfs /mnt/smb
```

Then stop the server:

```bash
# Package install (.deb, .rpm, Arch)
sudo systemctl disable --now dfs

# Quick install script, Homebrew, or source build: the server runs in the
# foreground, so press Ctrl+C in its terminal, or
pkill -TERM -x dfs
```

### 2. Remove the binaries

| Install method | Command |
|---|---|
| Quick install script | `sudo rm /usr/local/bin/dfs /usr/local/bin/dfsctl` |
| Homebrew | `brew uninstall --cask dfs dfsctl` |
| Debian / Ubuntu | `sudo apt purge dfs dfsctl` |
| RHEL / Fedora | `sudo yum remove dfs dfsctl` |
| Arch Linux | `sudo pacman -R dfs` (add `dfsctl` if you installed it) |
| Scoop | `scoop uninstall dfs dfsctl` |
| Docker | `docker rm -f dittofs && docker rmi marmos91c/dittofs:latest` |
| Docker Compose | `docker compose down && docker rmi dittofs:compose` |
| Source build | delete the `dfs` and `dfsctl` binaries you built, and the clone |
| Kubernetes operator | see the [operator uninstall guide](../../k8s/dittofs-operator/docs/INSTALL.md#uninstallation) |

If you installed the Homebrew formula instead of the cask (the README
command, without `--cask`), use
`brew uninstall cubbit/tap/dfs cubbit/tap/dfsctl`.

> **Note:** removing the `.deb` also removes `/usr/local/bin` if it is left
> empty. Recreate it with `sudo mkdir -p /usr/local/bin` if other tools need it.

Optionally remove the package sources:

```bash
# APT
sudo rm /etc/apt/sources.list.d/dfs.list /usr/share/keyrings/dittofs.gpg
sudo apt update

# YUM
sudo rm /etc/yum.repos.d/dfs.repo

# Scoop
scoop bucket rm cubbit
```

### 3. Remove data (optional)

> **Warning:** This cannot be undone. It deletes your configuration, users,
> shares, file metadata and file content: the journal holds the only copy of
> every byte written, since no remote store is wired yet.

**Server run by your user** (quick install script, Homebrew, source build):

| Data | Default path |
|---|---|
| Config file | `~/.config/dittofs/` |
| State directory: the metadata store, the log, `dfs.sock` | `~/.local/state/dittofs/` |
| Journals | the paths `journals` lists; there is no default |
| Key files | the paths `keys.<role>.local.file` names |

```bash
rm -rf ~/.config/dittofs ~/.local/state/dittofs
```

If you set `XDG_CONFIG_HOME`, `XDG_STATE_HOME`, `metadata.badger.path` or
`logging.output`, remove those locations instead. The state directory's rules are in
[Configuration](configuration.md#the-state-and-runtime-directories).

**Server run by the packaged systemd service:** the unit names no config file and no
directories, so the paths follow its environment, by the same rules. Remove the
directories your config file and your drop-ins name, the journals and the key files,
then the drop-ins:

```bash
sudo rm -rf /etc/systemd/system/dfs.service.d
sudo systemctl daemon-reload
```

**Everywhere:**

- The metadata store and the journals: the directories `metadata.badger.path` and
  `journals` name, which you noted before stopping the server.
- `dfsctl` credentials: `~/.config/dfsctl/config.json` (Windows:
  `%APPDATA%\dfsctl\config.json`), for each user who ran `dfsctl login`.
- Client leftovers: `~/.smbcredentials`, mount point directories, and any
  DittoFS entries in `/etc/fstab`.

**Docker:**

```bash
docker volume rm dittofs-metadata dittofs-blocks dittofs-state
# an upgraded v1 host also has this one
docker volume rm dittofs-cache
```

The single-container setup also bind-mounts `~/.config/dittofs/config.yaml`, the
`keys` and the `tls` directories from the host; remove them too.

**Remote storage:** data a v1 release left in remote block stores such as S3
buckets, or in an externally managed PostgreSQL database, is not touched by any
step above. Delete it on the storage side if you no longer need it. See
[Upgrading from v1](upgrading-from-v1.md).
