#!/usr/bin/env bash
# A local FoundationDB for the fdbkv backend's tests: the client library, a
# cluster in containers, and go test pointed at both.
#
#   test/fdb/cluster.sh client      download libfdb_c and its headers
#   test/fdb/cluster.sh up [1|3]    start N fdbserver processes (default 1); three
#                                   run double replication with three coordinators
#   test/fdb/cluster.sh down        stop and remove them, and their data
#   test/fdb/cluster.sh kill N      kill process N (1-based) without a clean stop
#   test/fdb/cluster.sh start N     start a killed process N again
#   test/fdb/cluster.sh test ...    go test from the repository root, with the
#                                   client library on the cgo paths and
#                                   DITTOFS_FDB_CLUSTER_FILE set
#   test/fdb/cluster.sh smoke IMAGE run a Dockerfile.foundationdb image's dfs
#                                   init, then dfs start until it is ready, then
#                                   stop it; the cluster must be up and own
#                                   nothing yet, since the installation owns it.
#                                   The journal gets a loop-mounted ext4 made
#                                   with sudo, or DITTOFS_SMOKE_BLOCKS if set
#
# Host networking, so the cluster file's 127.0.0.1 addresses are the ones the
# test client dials. That works on Linux only; on macOS run it inside a Linux
# VM. The client library is x86_64 only here. Docker or podman.
set -euo pipefail

VERSION="${FDB_VERSION:-7.3.77}"
RUNTIME="${CONTAINER_RUNTIME:-$(command -v docker || command -v podman)}"
# On disk, not under /tmp: /tmp is a RAM disk on many Linux hosts, which hides
# the cost of every commit's sync.
BASE="${DITTOFS_FDB_DIR:-$HOME/.cache/dittofs-fdb}"
DATA="$BASE/data"
CLIENT="$BASE/client-$VERSION"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
IMAGE="docker.io/foundationdb/foundationdb:$VERSION"

client() {
    local base="https://github.com/apple/foundationdb/releases/download/$VERSION"
    local tmp
    tmp="$(mktemp -d)"
    trap 'rm -rf "$tmp"' RETURN
    curl -fsSL -o "$tmp/libfdb_c.so" "$base/libfdb_c.x86_64.so"
    curl -fsSL -o "$tmp/headers.tar.gz" "$base/fdb-headers-$VERSION.tar.gz"
    # fdb_c.h includes fdb_c_types.h, which the release's header tarball leaves out.
    curl -fsSL -o "$tmp/fdb_c_types.h" \
        "https://raw.githubusercontent.com/apple/foundationdb/$VERSION/bindings/c/foundationdb/fdb_c_types.h"
    # The release's own digests (and the tagged header's), so a tampered
    # download never reaches LD_LIBRARY_PATH. Another FDB_VERSION needs its own.
    (cd "$tmp" && sha256sum -c - <<EOF) || exit 1
617572467897d0360f76ffc33f24b58090ee691e3273750d2ff334cc71aae3fa  libfdb_c.so
73b1efca62bf1779e0d4d62131514c96f689db82865695618086e73a057ea385  headers.tar.gz
82ea67e61592873c637e228764f5921179a710826be558ee0d1703023913e14d  fdb_c_types.h
EOF
    mkdir -p "$CLIENT/lib" "$CLIENT/include/foundationdb"
    mv "$tmp/libfdb_c.so" "$CLIENT/lib/"
    tar -xzf "$tmp/headers.tar.gz" -C "$CLIENT/include/foundationdb"
    mv "$tmp/fdb_c_types.h" "$CLIENT/include/foundationdb/"
    echo "client $VERSION in $CLIENT"
}

process_up() {
    local n=$1 port=$((4499 + $1))
    mkdir -p "$DATA/p$n"
    "$RUNTIME" run -d --name "dittofs-fdb$n" --network host \
        -v "$DATA:/cluster" --entrypoint /usr/bin/fdbserver "$IMAGE" \
        --cluster-file /cluster/fdb.cluster --public-address "127.0.0.1:$port" \
        --listen-address "127.0.0.1:$port" --datadir "/cluster/p$n" --logdir "/cluster/p$n" \
        --locality-zoneid "z$n" --locality-machineid "m$n" >/dev/null
}

fdbcli() {
    "$RUNTIME" exec dittofs-fdb1 /usr/bin/fdbcli -C /cluster/fdb.cluster --exec "$1"
}

up() {
    local n=$1 coords="" mode=single
    for i in $(seq 1 "$n"); do coords+="${coords:+,}127.0.0.1:$((4499 + i))"; done
    # double, not triple: triple places logs in three zones, so three processes
    # cannot lose one. double keeps two durable copies and survives one loss.
    [[ "$n" -ge 3 ]] && mode=double
    echo "dittofs:dittofs@$coords" >"$DATA/fdb.cluster"
    for i in $(seq 1 "$n"); do process_up "$i"; done
    sleep 2
    fdbcli "configure new $mode ssd" >/dev/null
    for _ in $(seq 1 60); do
        if fdbcli "status minimal" 2>/dev/null | grep -q "available"; then
            echo "FoundationDB $VERSION up: $n process(es), $mode replication; cluster file $DATA/fdb.cluster"
            return 0
        fi
        sleep 1
    done
    fdbcli "status" >&2 || true
    return 1
}

# smoke proves the image's libfdb_c loads and serves an installation on this
# cluster: dfs init, dfs start until /health/ready answers ready, and a SIGTERM
# that stops it with exit 0. Any step failing fails it. start takes init's
# volumes and its bind mount of the journal's directory, so it finds what init
# wrote; host networking reaches the cluster file's 127.0.0.1 addresses.
smoke() {
    local image=$1 port=18080 conf="$DATA/smoke" ready="" code
    # decision: the journal gets a loop-mounted ext4 of its own at
    # /data/blocks rather than the image's anonymous volume, because the
    # journal refuses every disk mount of the hosted runners, amd64 and arm64
    # alike: one root ext4 mounted nobarrier behind /var/lib/docker,
    # RUNNER_TEMP, /tmp, the workspace and /mnt. The loop mount reports no
    # nobarrier, so the platform check accepts it, though its syncs still land
    # on that disk: the smoke proves the image's libfdb_c loads and an
    # installation serves, not durability. It needs sudo, which the hosted
    # runners give. DITTOFS_SMOKE_BLOCKS names a directory to use instead, on
    # a mount the journal accepts, holding an empty journal directory the
    # image's user 65532 can write; the smoke leaves it as it is.
    # Globals, not locals: the EXIT trap reads them after smoke has returned.
    blocks="${DITTOFS_SMOKE_BLOCKS:-}" img=""
    if [[ -z "$blocks" ]]; then
        blocks="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/dittofs-smoke-blocks"
        img="$blocks.img"
    fi
    # As the image's user: a fixed name, removed before and after, so a run
    # that failed half-way leaves nothing for the next.
    local run=("$RUNTIME" run --network host -e DITTOFS_ENCRYPTION_PASSPHRASE=smoke-passphrase)
    clean() {
        "$RUNTIME" rm -f -v dittofs-smoke-start dittofs-smoke-init >/dev/null 2>&1 || true
        if [[ -n "$img" ]]; then
            if mountpoint -q "$blocks"; then sudo umount "$blocks" || true; fi
            rm -f "$img"
            rmdir "$blocks" 2>/dev/null || true
        fi
    }
    clean
    trap clean EXIT
    if [[ -n "$img" ]]; then
        mkdir -p "$blocks"
        truncate -s 1G "$img"
        mkfs.ext4 -q "$img"
        sudo mount -o loop "$img" "$blocks"
        # The bind mount hides the journal directory the image creates, and
        # init does not create it.
        sudo mkdir "$blocks/journal"
        sudo chown -R 65532:65532 "$blocks"
    fi
    echo "smoke: journal directory $blocks is on $(findmnt -no FSTYPE,OPTIONS -T "$blocks")"
    mkdir -p "$conf"
    cp "$DATA/fdb.cluster" "$conf/fdb.cluster"
    cat >"$conf/config.yaml" <<EOF
logging:
  output: stderr
node:
  id: 00112233-4455-6677-8899-aabbccddeeff
metadata:
  backend: foundationdb
  foundationdb:
    cluster_file: /config/fdb.cluster
journals:
  - path: /data/blocks/journal
keys:
  storage:
    kind: local
    local:
      file: /data/state/storage.key
api:
  listen: ['127.0.0.1:$port']
metrics:
  listen: '127.0.0.1:0'
adapters:
  listen: ['127.0.0.1']
EOF
    chmod 0755 "$conf"
    chmod 0644 "$conf"/*
    "${run[@]}" --name dittofs-smoke-init -v "$conf:/config:ro" -v "$blocks:/data/blocks" \
        "$image" init --config /config/config.yaml
    "${run[@]}" -d --name dittofs-smoke-start --volumes-from dittofs-smoke-init "$image" >/dev/null
    for _ in $(seq 1 120); do
        ready="$(curl -fsS "http://127.0.0.1:$port/health/ready" 2>/dev/null || true)"
        [[ "$ready" == ready ]] && break
        [[ "$("$RUNTIME" inspect -f '{{.State.Running}}' dittofs-smoke-start)" == true ]] || break
        sleep 1
    done
    if [[ "$ready" != ready ]]; then
        "$RUNTIME" logs dittofs-smoke-start >&2
        echo "smoke: dfs start never answered ready on /health/ready (last answer: '$ready')" >&2
        return 1
    fi
    "$RUNTIME" stop -t 60 dittofs-smoke-start >/dev/null
    code="$("$RUNTIME" inspect -f '{{.State.ExitCode}}' dittofs-smoke-start)"
    if [[ "$code" != 0 ]] || ! "$RUNTIME" logs dittofs-smoke-start 2>&1 | grep -q "dfs: stopped"; then
        "$RUNTIME" logs dittofs-smoke-start >&2
        echo "smoke: dfs start on SIGTERM exited $code; want 0 after logging 'dfs: stopped'" >&2
        return 1
    fi
    echo "smoke: $image initialised, served ready and stopped cleanly on FoundationDB $VERSION"
}

# Created here, as the user: a bind mount of a missing directory would have the
# container runtime create it as root, and nothing after could write to it.
mkdir -p "$DATA"

case "${1:-}" in
client) client ;;
up) up "${2:-1}" ;;
down)
    for i in 1 2 3; do "$RUNTIME" rm -f "dittofs-fdb$i" >/dev/null 2>&1 || true; done
    # The processes may write as root; remove the data from inside a container.
    "$RUNTIME" run --rm -v "$DATA:/d" docker.io/library/busybox:1.37 sh -c 'rm -rf /d/*' >/dev/null 2>&1 || true
    ;;
kill) "$RUNTIME" kill -s KILL "dittofs-fdb${2:?process number}" >/dev/null ;;
start) "$RUNTIME" start "dittofs-fdb${2:?process number}" >/dev/null ;;
test)
    shift
    [[ -f "$CLIENT/lib/libfdb_c.so" ]] || client
    export CGO_CFLAGS="-I$CLIENT/include" CGO_LDFLAGS="-L$CLIENT/lib"
    export LD_LIBRARY_PATH="$CLIENT/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
    export DITTOFS_FDB_CLUSTER_FILE="${DITTOFS_FDB_CLUSTER_FILE:-$DATA/fdb.cluster}"
    cd "$ROOT" && exec go test "$@"
    ;;
smoke) smoke "${2:?image}" ;;
*)
    sed -n '2,23p' "$0"
    exit 2
    ;;
esac
