# Installation

This page shows every way to install and run DittoFS: from source, from a package
manager, in Docker, with FoundationDB, and on Kubernetes. It also covers standard
ports and uninstalling. For a first run on your own machine, follow
[Getting started](getting-started.md) instead; for the config file, see
[Configuration](configuration.md).

DittoFS ships two programs: `dfs`, the server, and `dfsctl`, the client of its
management API.

> **No release ships this version yet.** The packages, the install script, Homebrew,
> Scoop and the published Docker images install the released server, which uses a
> different config file and does not have `dfs init` as described here. To run this
> version, [build it from source](#build-from-source), or
> [build the Docker image yourself](#build-the-image).

> **Not served on this version:** NFS and SMB. Steps that mount a share, and the NFS
> and SMB ports below, describe the released server.

## Build from source

You need [Go](https://go.dev/dl/) 1.26 or later and `git`:

```bash
git clone --depth 1 --branch feat/storage-v2 https://github.com/cubbit/dittofs.git
cd dittofs
go build -o dfs ./cmd/dfs
go build -o dfsctl ./cmd/dfsctl
sudo install -m 755 dfs dfsctl /usr/local/bin/   # optional: put them on your PATH
```

Then continue with [Getting started](getting-started.md#2-write-the-config-file).

## Package managers

These install the released server today (see the note above).

| System | Install |
|--------|---------|
| macOS, Linux (Homebrew) | `brew tap cubbit/tap && brew install --cask cubbit/tap/dfs cubbit/tap/dfsctl` |
| macOS, Linux (script) | `curl -fsSL https://github.com/cubbit/dittofs/releases/latest/download/install.sh \| sh` |
| Debian, Ubuntu | see [APT](#debian--ubuntu-apt) |
| RHEL, Fedora | see [YUM](#rhel--fedora-yum) |
| Arch Linux | `sudo pacman -U dfs_<version>_amd64.pkg.tar.zst`, downloaded from GitHub Releases |
| Windows (Scoop) | `scoop bucket add cubbit https://github.com/cubbit/scoop-bucket` then `scoop install dfs dfsctl` |

The install script puts both programs in `/usr/local/bin`, asking for `sudo` if it
needs to.

### Debian / Ubuntu (APT)

```bash
curl -fsSL https://s3.cubbit.eu/dittofs-binaries/apt/dittofs.gpg.key \
  | gpg --dearmor --yes | sudo tee /usr/share/keyrings/dittofs.gpg > /dev/null
echo "deb [signed-by=/usr/share/keyrings/dittofs.gpg] https://s3.cubbit.eu/dittofs-binaries/apt stable main" \
  | sudo tee /etc/apt/sources.list.d/dfs.list
sudo apt update && sudo apt install dfs dfsctl
```

### RHEL / Fedora (YUM)

```bash
sudo curl -fsSLo /etc/yum.repos.d/dfs.repo https://s3.cubbit.eu/dittofs-binaries/rpm/dfs.repo
sudo yum install dfs dfsctl
```

### Run it under systemd

The `.deb`, `.rpm` and Arch packages install a systemd service, `dfs`, that runs
`dfs start` in the foreground as root. The unit names no config file and no
directories, so set them in a drop-in before the first start:

1. Make the config directory, readable by root only, and put the passphrase in a file
   there. Quote the value, since the file is read both by systemd and by a shell:

   ```bash
   sudo install -d -m 700 /etc/dittofs
   echo "DITTOFS_ENCRYPTION_PASSPHRASE='correct horse battery staple'" | sudo tee /etc/dittofs/passphrase.env > /dev/null
   sudo chmod 600 /etc/dittofs/passphrase.env
   ```

2. Write the config file to `/etc/dittofs/config.yaml`, with absolute paths under
   `/var/lib/dittofs` (see [Configuration](configuration.md#a-first-config-file)), and
   create each journal directory, for example `sudo mkdir -p /var/lib/dittofs/journal`.

3. Add a drop-in that names the config, the passphrase file and the directories:

   ```bash
   sudo systemctl edit dfs
   ```

   ```ini
   [Service]
   Environment=DITTOFS_CONFIG=/etc/dittofs/config.yaml
   EnvironmentFile=/etc/dittofs/passphrase.env
   StateDirectory=dittofs
   RuntimeDirectory=dittofs
   ```

4. Create the installation once, with the same passphrase and state directory, then
   start the service:

   ```bash
   sudo sh -c 'set -a; . /etc/dittofs/passphrase.env; STATE_DIRECTORY=/var/lib/dittofs exec dfs init --config /etc/dittofs/config.yaml'
   sudo systemctl enable --now dfs
   ```

`dfs init` prints the `admin` account's generated password once, to its terminal and
never to the log, so run it where you can read the output. To choose the password
instead, pass `--admin-password-file` or `--admin-password-env`. If the password is
lost, stop the service and run `dfs reset-admin` the same way as `init`; it prints a
new one:

```bash
sudo systemctl stop dfs
sudo sh -c 'set -a; . /etc/dittofs/passphrase.env; STATE_DIRECTORY=/var/lib/dittofs exec dfs reset-admin --config /etc/dittofs/config.yaml'
sudo systemctl start dfs
```

With `RuntimeDirectory=dittofs`, the local socket is `/run/dittofs/dfs.sock`, so on
the host you can sign in without a network address:

```bash
sudo dfsctl login --server /run/dittofs/dfs.sock --username admin
```

## Docker

Multi-architecture images (`linux/amd64`, `linux/arm64`) are published on Docker Hub
as `marmos91c/dittofs`. They hold the released server today.

### Build the image

To run this version, build the image from your clone:

```bash
cd dittofs
docker build -t dittofs:dev .
```

Use `dittofs:dev` in place of `marmos91c/dittofs:latest` below.

### Single container

The image runs `dfs` as user `65532`, and its default command is
`start --config /config/config.yaml`. In a container the API must listen on every
interface, and an API that is not on loopback is served over TLS only. So the
container needs a certificate, and a config file that names the node, the journal and
both key files.

1. Make a working directory, and a certificate that names `localhost` (the image
   ships none):

   ```bash
   mkdir -p ~/dittofs-docker/keys ~/dittofs-docker/tls && cd ~/dittofs-docker
   openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 365 \
     -subj "/CN=localhost" -addext "subjectAltName=DNS:localhost" \
     -keyout tls/api.key -out tls/api.crt
   export DITTOFS_ENCRYPTION_PASSPHRASE='correct horse battery staple'
   ```

2. Write `config.yaml`:

   ```yaml
   node:
     id: 6f1c2a4e-8d3b-4f7a-9c5e-2b8d4a6f0e13   # generate your own with uuidgen
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

3. Write the protocol key file with the program in
   [Adding the protocol key](configuration.md#adding-the-protocol-key), run from your
   clone: `(cd ~/dittofs && go run /tmp/dfs-keygen/main.go ~/dittofs-docker/keys/protocol.key)`. Then
   hand `keys` and `tls` to the container's user, `65532`; `dfs init` writes
   `keys/storage.key` itself:

   ```bash
   cd ~/dittofs-docker
   sudo chown -R 65532 keys tls
   ```

4. Create the installation once, then run the server:

   ```bash
   docker run --rm \
     -e DITTOFS_ENCRYPTION_PASSPHRASE \
     -e STATE_DIRECTORY=/data/state \
     -v ./config.yaml:/config/config.yaml:ro \
     -v ./keys:/config/keys \
     -v ./tls:/config/tls:ro \
     -v dittofs-metadata:/data/metadata \
     -v dittofs-blocks:/data/blocks \
     -v dittofs-state:/data/state \
     marmos91c/dittofs:latest init --config /config/config.yaml

   docker run -d --name dittofs \
     -e DITTOFS_ENCRYPTION_PASSPHRASE \
     -e STATE_DIRECTORY=/data/state \
     -p 8080:8080 \
     -v ./config.yaml:/config/config.yaml:ro \
     -v ./keys:/config/keys \
     -v ./tls:/config/tls:ro \
     -v dittofs-metadata:/data/metadata \
     -v dittofs-blocks:/data/blocks \
     -v dittofs-state:/data/state \
     marmos91c/dittofs:latest
   ```

5. Check it and sign in from the host:

   ```console
   $ curl --cacert tls/api.crt https://localhost:8080/health/ready
   ready
   $ dfsctl login --server https://localhost:8080 --cacert tls/api.crt --username admin
   password:
   Logged in to https://localhost:8080 as admin (context default)
   ```

Things to know:

- `init` prints the generated `admin` password once, to that terminal only. It never
  reaches the log or `docker logs`. Pass `-e DFS_ADMIN_PASSWORD=...` and
  `--admin-password-env DFS_ADMIN_PASSWORD` to choose it yourself.
- `init` prints where it wrote `keys/storage.key`. Copy that file off the host and
  keep the passphrase: losing either loses the data.
- **Never keep the journal in the container's writable layer.** `dittofs-blocks`
  holds the journal, the only copy of every byte written. A volume only receives
  data if the config points inside it, not at its mount point: hence
  `/data/metadata/store` and `/data/blocks/journal`. The image creates
  `/data/blocks/journal`, which `dfs init` needs to exist.
- The health check polls `/health/live`. Metrics stay on the container's loopback,
  `127.0.0.1:9090`. `docker stop` stops the node cleanly.

**Image tags:**

| Tag | Meaning |
|-----|---------|
| `marmos91c/dittofs:latest` | latest stable release |
| `marmos91c/dittofs:vX.Y.Z` | a specific version |
| `marmos91c/dittofs:vX.Y` | latest patch of a minor version |
| `marmos91c/dittofs:vX` | latest minor of a major version |
| `marmos91c/dittofs:vX.Y.Z-foundationdb` | a version built with FoundationDB support |

### Docker Compose

> **Does not run on this version.** The repository's `docker-compose.yml` uses
> `docker/config.yaml`, which names neither the key files nor the API certificate,
> runs no `dfs init`, and its setup script (`docker/bootstrap.sh`) calls the released
> server's API. Use the single container above.

## FoundationDB metadata

By default the metadata lives in Badger, on one node's disk. To keep it in a
FoundationDB cluster instead (`metadata.backend: foundationdb`), you need a `dfs`
built with FoundationDB support. Releases publish one image and two tarballs for
this. The packages, the install script and the default image do not include it: a
`dfs` without it refuses `metadata.backend: foundationdb` and says so.

**You need:**

- A FoundationDB **7.3** cluster and its cluster file (usually
  `/etc/foundationdb/fdb.cluster`). The client is 7.3, and reaches only a 7.3 cluster.
- **A cluster for this installation alone.** `dfs` writes keys with no prefix, so the
  cluster must hold no other data, and no second installation may share it.
- Linux. The tarballs need glibc 2.34 or later (Debian 12, Ubuntu 22.04, RHEL 9 or newer).

### The image

```bash
docker pull marmos91c/dittofs:vX.Y.Z-foundationdb
```

It is published for `linux/amd64` and `linux/arm64`. It carries the 7.3 client, runs
as user `65532`, and has the same ports, volumes and health check as the default
image, plus one volume:

- `/etc/foundationdb`: put `fdb.cluster` here and name it in
  `metadata.foundationdb.cluster_file`. The client rewrites the file when the
  cluster's coordinators change, so mount a directory that user `65532` can write,
  not the file alone and not read-only.

### The tarball

`dfs-foundationdb_<version>_Linux_x86_64.tar.gz` and
`dfs-foundationdb_<version>_Linux_arm64.tar.gz` are on the release page, listed in
`checksums.txt`. Each holds `dfs`, the README, the LICENSE and a `FOUNDATIONDB.txt`
note. It does **not** include `libfdb_c.so`: install the `foundationdb-clients` 7.3
package from [FoundationDB's releases](https://github.com/apple/foundationdb/releases)
(the deb puts it in `/usr/lib`, the rpm in `/usr/lib64`).

```bash
sha256sum -c --ignore-missing checksums.txt
tar -xzf dfs-foundationdb_*_Linux_x86_64.tar.gz
```

### Configure, initialize, start

Select the backend and name the cluster file. A `metadata.badger` key beside it is
refused:

```yaml
node:
  id: 7c0f3a52-9d4e-4b8e-8a31-5b6f2f0e9c11   # generate your own with uuidgen
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

The journals stay on local disk: FoundationDB holds only the metadata. Write the
protocol key file first ([how](configuration.md#adding-the-protocol-key)); `dfs init`
creates the storage key file in a directory that must exist and be writable. Every
key is in [Configuration](configuration.md#foundationdb-metadata).

With the tarball's binary:

```bash
export DITTOFS_ENCRYPTION_PASSPHRASE='correct horse battery staple'
./dfs init --config /etc/dittofs/config.yaml
./dfs start --config /etc/dittofs/config.yaml
```

Or with the image, as for the [default image](#single-container), with no metadata
volume, the cluster file's directory mounted, and the key directory mounted where the
config names it. Both host directories must be writable by user `65532`:

```bash
docker run --rm \
  -e DITTOFS_ENCRYPTION_PASSPHRASE \
  -v ./config.yaml:/config/config.yaml:ro \
  -v ./foundationdb:/etc/foundationdb \
  -v ./keys:/config/keys \
  -v dittofs-blocks:/data/blocks \
  marmos91c/dittofs:vX.Y.Z-foundationdb init --config /config/config.yaml
```

Run the server the same way with `start`, plus the ports and other volumes of
[Single container](#single-container). `init` and `start` wait up to 30 seconds for
the cluster; a wrong cluster file ends in an error naming it, not a hang.

For a cluster that requires TLS, name the client certificate, its key and the CA file
under `metadata.foundationdb.tls` (see
[Configuration](configuration.md#foundationdb-metadata)). In the image, mount them
read-only and use their paths inside the container. A missing file is refused, by
name, before `dfs` connects.

## Kubernetes operator

> **Does not run on this version.** The operator still writes the released server's
> config file (`database:`, `controlplane:`, `shutdown_timeout`), with no `node.id`,
> `journals` or `keys`, so this version refuses it. Use the
> [single container](#single-container) instead.

For the released server, the operator manages the deployment, its configuration through
a `DittoServer` custom resource, volume claims for its stores, and a Service for NFS
and SMB:

```bash
cd k8s/dittofs-operator
make deploy                                                       # uses marmos91c/dittofs-operator:latest
kubectl apply -f config/samples/dittofs_v1alpha1_dittofs.yaml     # create an instance
kubectl get dittofs                                               # check it
```

To deploy your own build of the operator:
`make docker-build docker-push IMG=registry.example.com/dittofs-operator:dev`, then
`make deploy IMG=registry.example.com/dittofs-operator:dev`. The CRD reference, RBAC
and Helm chart are in [`k8s/dittofs-operator/`](../../k8s/dittofs-operator/).

## Default ports

| Port | Service |
|------|---------|
| `12049` | NFS, once served |
| `12445` | SMB, once served |
| `8080`  | Management API and health checks (`api.listen`, default `127.0.0.1:8080`) |
| `9090`  | Prometheus metrics (`metrics.listen`, default `127.0.0.1:9090`) |

All four are above 1024, so `dfs` starts without root. The API and metrics addresses
are keys in the config file. The NFS and SMB ports are not: each is a setting of its
adapter, `adapters.nfs.port` and `adapters.smb.port`, kept in the metadata store and
changed through the management API.

## Running on standard ports (production)

In production, serve NFS on `2049` and SMB on `445`, so clients connect with no port
options:

```bash
# Default ports: the port must be named
sudo mount -t nfs -o vers=4.1,tcp,port=12049 server:/export /mnt/point
# Standard ports: no port option
sudo mount -t nfs -o vers=4.1 server:/export /mnt/point
```

> **Not available yet on this version:** setting the adapter ports. The commands in
> step 2 are refused with `unknown_key` until the adapters' settings exist
> ([why](configuration.md#the-management-api)).

### 1. Free the standard ports on the host

The operating system's own NFS, SMB and rpcbind services usually hold `2049` and
`445`. Stop them first:

```bash
# Linux (systemd)
sudo systemctl disable --now nfs-server rpcbind rpcbind.socket smbd nmbd
sudo ss -tulpn | grep -E ':(2049|445)\b'      # should print nothing
```

```bash
# macOS
sudo nfsd stop && sudo nfsd disable
sudo launchctl disable system/com.apple.smbd   # or turn off File Sharing in System Settings → General → Sharing
```

### 2. Set the ports and allow `dfs` to bind them

**Single binary.** Set each adapter's port, then let `dfs` bind ports below 1024
without running as root:

```bash
dfsctl adapter set nfs --setting port=2049
dfsctl adapter set smb --setting port=445
sudo setcap 'cap_net_bind_service=+ep' /usr/local/bin/dfs
```

Under systemd, add `AmbientCapabilities=CAP_NET_BIND_SERVICE` to the `[Service]`
section instead of `setcap`. A config file that names `adapters.nfs.port` is refused:
the port is a setting, not a host fact.

**Docker.** The process may bind low ports inside the container. Set the adapter ports
as above and publish them one to one, with the volumes and config of
[Single container](#single-container):

```bash
docker run -d -p 2049:2049 -p 445:445 -p 8080:8080 marmos91c/dittofs:latest
```

The Docker host must still not run its own `nfsd`, `smbd` or `rpcbind` on those
ports; see [step 1](#1-free-the-standard-ports-on-the-host).

**Kubernetes.** The operator does not run this version yet (see
[above](#kubernetes-operator)). For the released server, set the ports in the
`DittoServer` resource and let the operator's Service publish them; the pod never
binds a privileged port itself:

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
    type: LoadBalancer   # publishes the adapter ports outside the cluster
```

## Uninstall

Removing DittoFS takes three steps: stop the server, remove the programs, and,
if you want, delete the data. Removing the programs never deletes data.

If you plan to delete the data, first note the directories your config names for
`metadata.badger.path` and `journals`. `dfs config show` prints them.

### 1. Unmount shares and stop the server

On every client, unmount the shares first; a mount left behind hangs once the server
is gone:

```bash
sudo umount /mnt/nfs /mnt/smb
```

Then stop the server:

```bash
# Package install (.deb, .rpm, Arch)
sudo systemctl disable --now dfs

# Source build, install script or Homebrew: press Ctrl+C in its terminal, or
pkill -TERM -x dfs
```

### 2. Remove the programs

| Install method | Command |
|---|---|
| Source build | delete the `dfs` and `dfsctl` binaries and the clone; `sudo rm /usr/local/bin/dfs /usr/local/bin/dfsctl` if you installed them |
| Install script | `sudo rm /usr/local/bin/dfs /usr/local/bin/dfsctl` |
| Homebrew | `brew uninstall --cask dfs dfsctl` |
| Debian / Ubuntu | `sudo apt purge dfs dfsctl` |
| RHEL / Fedora | `sudo yum remove dfs dfsctl` |
| Arch Linux | `sudo pacman -R dfs` (add `dfsctl` if you installed it) |
| Scoop | `scoop uninstall dfs dfsctl` |
| Docker | `docker rm -f dittofs && docker rmi marmos91c/dittofs:latest` |
| Docker Compose | `docker compose down && docker rmi dittofs:compose` |
| Kubernetes operator | see the [operator uninstall guide](../../k8s/dittofs-operator/docs/INSTALL.md#uninstallation) |

If you installed the older Homebrew formula rather than the cask, use
`brew uninstall cubbit/tap/dfs cubbit/tap/dfsctl`.

Removing the `.deb` also removes `/usr/local/bin` if it is left empty. Recreate it with
`sudo mkdir -p /usr/local/bin` if other tools need it.

To remove the package sources too:

```bash
sudo rm /etc/apt/sources.list.d/dfs.list /usr/share/keyrings/dittofs.gpg && sudo apt update   # APT
sudo rm /etc/yum.repos.d/dfs.repo                                                            # YUM
scoop bucket rm cubbit                                                                       # Scoop
```

### 3. Remove the data (optional)

> **Warning:** this cannot be undone. It deletes your configuration, users, shares,
> file metadata and file content. The journal holds the only copy of every byte
> written, since no remote store is wired yet.

**A server you ran as yourself** (source build, install script, Homebrew):

| Data | Default path |
|---|---|
| Config file | `~/.config/dittofs/` |
| State directory: the metadata store, the log, `dfs.sock` | `~/.local/state/dittofs/` |
| Journals | the paths `journals` lists; there is no default |
| Key files | the paths `keys.<role>.local.file` names |

```bash
rm -rf ~/.config/dittofs ~/.local/state/dittofs
```

Also delete the journal directories and the key files your config names. If you set
`XDG_CONFIG_HOME`, `XDG_STATE_HOME`, `metadata.badger.path` or `logging.output`, remove
those locations instead; the rules are in
[Configuration](configuration.md#the-state-and-runtime-directories).

**A server run by the packaged service:** remove the directories your config file and
drop-ins name (with the drop-in above, `/etc/dittofs` and `/var/lib/dittofs`), the
journals and the key files, then the drop-ins:

```bash
sudo rm -rf /etc/systemd/system/dfs.service.d
sudo systemctl daemon-reload
```

**Everywhere:**

- `dfsctl` sessions: `~/.config/dfsctl/config.json` (Windows:
  `%APPDATA%\dfsctl\config.json`), for each user who ran `dfsctl login`.
- Client leftovers: `~/.smbcredentials`, mount point directories, and DittoFS entries
  in `/etc/fstab`.

**Docker:**

```bash
docker volume rm dittofs-metadata dittofs-blocks dittofs-state
docker volume rm dittofs-cache       # only on a host upgraded from the released server
```

Also remove the `config.yaml`, `keys` and `tls` you mounted from the host.

**Remote storage:** data the released server left in remote stores such as S3
buckets, or in an externally managed PostgreSQL database, is not touched by any step
above. Delete it on the storage side if you no longer need it. See
[Upgrading from v1](upgrading-from-v1.md).
