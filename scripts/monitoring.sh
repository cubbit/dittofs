#!/usr/bin/env bash
# monitoring.sh - Start and stop a local monitoring stack (LGTM: Loki,
# Grafana, Tempo, Mimir/Prometheus) plus a node exporter, for dev and bench
# machines. One command, idempotent, no manual import step.
#
# Usage:
#   scripts/monitoring.sh start [--pull]   bring the stack up (no-op if up)
#   scripts/monitoring.sh stop [--purge]   tear it down (--purge wipes data)
#   scripts/monitoring.sh status          show containers and Grafana URL
#
#   Ports: MONITORING_GRAFANA_PORT (default 3000),
#          MONITORING_NODE_EXPORTER_PORT (default 9100),
#   Data:  MONITORING_DATA_DIR (default: the dittofs-monitoring-data volume,
#          wherever the Docker daemon puts it — set it to keep the stack's
#          time series off the disk under benchmark).
#
#   Grafana listens on 127.0.0.1 only; on a remote host reach it with
#   ssh -L 3000:localhost:3000 <host>. A random admin password is printed
#   once on first start and stored, mode 600, in the state directory.
#
# Docs: docs/internals/contributing.md, "Local monitoring stack".

set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MONITORING_DIR="$REPO/scripts/monitoring"
STATE="${XDG_STATE_HOME:-$HOME/.local/state}/dittofs/monitoring"
PASSWORD_FILE="$STATE/grafana-admin-password"

RUNTIME="${MONITORING_RUNTIME:-docker}"
GRAFANA_PORT="${MONITORING_GRAFANA_PORT:-3000}"
NODE_EXPORTER_PORT="${MONITORING_NODE_EXPORTER_PORT:-9100}"
# Set to keep the stack's time series off the disk under test: the default
# named volume lives wherever the Docker daemon puts it (often the bench disk).
DATA_DIR="${MONITORING_DATA_DIR:-}"

LABEL="io.dittofs.monitoring=1"
NETWORK="dittofs-monitoring"
BRIDGE_NAME="dfsmon0"
LGTM_CONTAINER="dittofs-monitoring-lgtm"
NODE_EXPORTER_CONTAINER="dittofs-monitoring-node-exporter"
VOLUME="dittofs-monitoring-data"

# Pinned by digest too: node-exporter runs with the host's namespaces and a
# read-only /, so a retagged image must not slip in.
LGTM_IMAGE="grafana/otel-lgtm:0.41.1@sha256:1b852b77d7bcadb7699bfe7c78a51d059dff85168de674728cf7abf4474645e5"
NODE_EXPORTER_IMAGE="prom/node-exporter:v1.13.1@sha256:7296504d4ada44b7d7ab4431e27e9f9900d8488f8d55842f4b12ae2704e16dfe"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
info()  { echo -e "\033[1;34m[INFO]\033[0m  $*"; }
warn()  { echo -e "\033[1;33m[WARN]\033[0m  $*"; }
error() { echo -e "\033[1;31m[ERROR]\033[0m $*"; }
die()   { error "$@"; exit 1; }

usage() { awk 'NR>1 { if (!/^#/) exit; if (/^# /) {sub(/^# ?/,""); print} else print "" }' "$0"; }

runt() { "$RUNTIME" "$@"; }

# ---------------------------------------------------------------------------
# Preconditions
# ---------------------------------------------------------------------------
prechecks() {
    if [ "$(id -u)" -eq 0 ]; then
        die "run as your own user, not root: the state directory must stay in your home. Re-run without sudo (members of the docker group need no sudo)."
    fi
    [ "$(uname -s)" = "Linux" ] || die "The monitoring stack supports Linux only."
    # Only rootful Docker: the bridge options and the IPAM gateway the stack
    # builds on are Docker-specific, and rootless daemons report the VM or
    # namespace as the host, not the bench machine.
    [ "$RUNTIME" = "docker" ] || die "only Docker is supported; MONITORING_RUNTIME='$RUNTIME' was never tested (podman and rootless daemons do not give node-exporter the host's view)."
    command -v "$RUNTIME" >/dev/null 2>&1 \
        || die "$RUNTIME is not installed."
    # The podman-docker shim answers to "docker" and only fails later, at the
    # bridge gateway, with a cryptic template error — refuse it for what it is.
    case "$(readlink -f "$(command -v "$RUNTIME")" 2>/dev/null || echo x)" in
        *podman*) die "\`$RUNTIME\` is the podman-docker shim; only real Docker is supported (rootful — a rootless daemon would show the VM, not the host)." ;;
    esac
    runt info >/dev/null 2>&1 || die "the $RUNTIME daemon is not reachable."
    # Numeric ports up front: a bad value must die before anything starts.
    case "$GRAFANA_PORT:$NODE_EXPORTER_PORT" in
        *[!0-9:]*) die "GRAFANA_PORT and NODE_EXPORTER_PORT must be numeric ports" ;;
    esac
    # Checked up front, so a missing tool is a clear error, not a half-start.
    local tool
    for tool in openssl curl flock sha256sum hostname; do
        command -v "$tool" >/dev/null 2>&1 || die "$tool is required but not installed."
    done
    install -d -m 700 "$STATE"   # GNU install applies -m to the leaf only; the leaf is all that matters
    exec 9>"$STATE/lock"
    flock -n 9 || die "another monitoring.sh is already running; wait for it."
}

# ---------------------------------------------------------------------------
# start
# ---------------------------------------------------------------------------
bridge_gateway() {
    # The IPv4 gateway of the monitoring bridge. node-exporter binds to it: a
    # host address on dfsmon0, not bound on any LAN interface. Note this is a
    # routing property, not a firewall: a LAN peer that routes to it, and any
    # local container, can reach it — firewall dfsmon0 if that matters.
    runt network inspect "$NETWORK" \
        -f '{{range .IPAM.Config}}{{.Gateway}} {{end}}' | tr ' ' '\n' \
        | grep -oE '^[0-9.]+$' | head -1
}

render_prometheus_config() {
    local target="$1" host escaped
    host="$(hostname)"
    escaped="${host//\\/\\\\}"   # \ is a sed replacement metachar
    escaped="${escaped//&/\\&}"     # so is &
    escaped="${escaped//|/\\|}"     # and | is the delimiter
    sed -e "s|@NODE_EXPORTER_TARGET@|$target|" \
        -e "s|@HOSTNAME@|$escaped|" \
        "$MONITORING_DIR/prometheus.yaml.tmpl" > "$STATE/prometheus.yaml"
    chmod 644 "$STATE/prometheus.yaml"
}

config_hash() {
    # Hash of everything a rebuild depends on — the repo files (never the
    # rendered state file, which can be missing while the stack runs) and the
    # ports. Stored as a container label, so a second `start` can warn about a
    # stale config without restarting (a second start must be a no-op).
    sha256sum "$MONITORING_DIR/prometheus.yaml.tmpl" \
        "$MONITORING_DIR/grafana/provisioning/dashboards/dittofs.yaml" \
        "$MONITORING_DIR/../monitoring.sh" \
        | cut -d' ' -f1
    echo "$LGTM_IMAGE $NODE_EXPORTER_IMAGE"
    echo "ports $GRAFANA_PORT $NODE_EXPORTER_PORT"
    echo "host $(hostname)"
}

lgtm_running()   { [ "$(runt inspect -f '{{.State.Running}}' "$LGTM_CONTAINER" 2>/dev/null)" = "true" ]; }
exporter_running() { [ "$(runt inspect -f '{{.State.Running}}' "$NODE_EXPORTER_CONTAINER" 2>/dev/null)" = "true" ]; }

grafana_admin_password_args() {
    # shellcheck disable=SC2016  # the literal $__file{} is Grafana's expander; it must not expand here
    # The password reaches Grafana through $__file{}: never argv, never env.
    # Grafana creates the admin user only when its user table is empty, so
    # re-passing the generated password never overwrites one the user changed.
    if [ -f "$PASSWORD_FILE" ]; then
        printf '%s\n' \
            --mount "type=bind,src=$PASSWORD_FILE,target=/run/secrets/grafana-admin-password,readonly" \
            -e 'GF_SECURITY_ADMIN_PASSWORD=$__file{/run/secrets/grafana-admin-password}'
    fi
}

both_running() { lgtm_running && exporter_running; }

cmd_start() {
    local pull=()
    case "${1:-}" in
        "") ;;
        --pull) pull=(--pull always) ;;
        *) die "unknown flag for start: $1 (only --pull is supported)" ;;
    esac
    [ "$#" -le 1 ] || die "start takes at most one flag (--pull)"

    if both_running; then
        local running_port
        running_port="$(grafana_url_port)"
        running_port="${running_port:-$GRAFANA_PORT}"
        info "already running: Grafana at http://127.0.0.1:$running_port"
        info "grafana URL hint: on a remote host, ssh -L $running_port:localhost:$running_port -- for the node's own traces/metrics there is the recipe in docs/internals/debugging.md"
        local rendered label_running
        rendered="$(config_hash | sha256sum | cut -d' ' -f1)"
        label_running="$(runt inspect -f '{{index .Config.Labels "io.dittofs.monitoring.config"}}' "$LGTM_CONTAINER" 2>/dev/null || true)"
        if [ "$rendered" != "$label_running" ]; then
            warn "running with an older config; run 'stop' then 'start' to apply changes"
        fi
        password_status
        exit 0
    fi

    # The interrupt trap is armed only on the start path: on the already-running
    # no-op above, a Ctrl-C must not tear down a healthy stack.
    trap 'teardown; die "interrupted — the stack was cleaned up."' INT TERM

    # Half-started or half-torn-down leftovers are cleaned up, never adopted.
    # stop before rm -f: a leftover LGTM may hold a TSDB that SKIPS recovery
    # when SIGKILLed (teardown does the same for a full stop).
    runt stop -t 10 "$LGTM_CONTAINER" "$NODE_EXPORTER_CONTAINER" >/dev/null 2>&1 || true
    runt rm -f "$LGTM_CONTAINER" "$NODE_EXPORTER_CONTAINER" >/dev/null 2>&1 || true

    runt network inspect "$NETWORK" >/dev/null 2>&1 \
        || runt network create --driver bridge \
               -o com.docker.network.bridge.name="$BRIDGE_NAME" "$NETWORK" >/dev/null
    local gateway
    gateway="$(bridge_gateway || true)"
    [ -n "$gateway" ] || { teardown; die "could not read the bridge gateway address for $NETWORK"; }
    render_prometheus_config "$gateway:$NODE_EXPORTER_PORT"
    local cfg_hash
    cfg_hash="$(config_hash | sha256sum | cut -d' ' -f1)"

    local data_mount=(-v "$VOLUME:/data")
    if [ -n "$DATA_DIR" ]; then
        install -d "$DATA_DIR"
        data_mount=(--mount "type=bind,src=$DATA_DIR,target=/data")
    fi

    info "starting the stack (first run pulls images, can take a few minutes)..."
    # The password is generated before the containers start, so a fresh
    # volume and its password are always created together — an aborted first
    # start can never leave a Grafana with the default 'admin' credentials.
    generate_password
    local password_args=()
    readarray -t password_args < <(grafana_admin_password_args)

    runt run -d --name "$LGTM_CONTAINER" \
        --network "$NETWORK" \
        --label "$LABEL" \
        --label "io.dittofs.monitoring.config=$cfg_hash" \
        -p "127.0.0.1:$GRAFANA_PORT:3000" \
        "${data_mount[@]}" \
        -v "$STATE/prometheus.yaml":/otel-lgtm/prometheus.yaml:ro \
        -v "$MONITORING_DIR/grafana/provisioning/dashboards/dittofs.yaml":/otel-lgtm/grafana/conf/provisioning/dashboards/dittofs.yaml:ro \
        -v "$MONITORING_DIR/grafana/dashboards":/otel-lgtm/dittofs-dashboards:ro \
        "${password_args[@]}" \
        -e GF_AUTH_ANONYMOUS_ENABLED=false \
        -e GF_AUTH_BASIC_PASSWORD_POLICY=true \
        -e GF_USERS_ALLOW_SIGN_UP=false \
        -e GF_ANALYTICS_REPORTING_ENABLED=false \
        -e GF_ANALYTICS_CHECK_FOR_UPDATES=false \
        -e GF_METRICS_ENABLED=false \
        -e GF_DASHBOARDS_DEFAULT_HOME_DASHBOARD_PATH=/otel-lgtm/dittofs-dashboards/node-overview.json \
        "${pull[@]}" \
        "$LGTM_IMAGE" >/dev/null \
        || { teardown; die "failed to start $LGTM_IMAGE — port $GRAFANA_PORT busy, or docker failed; everything that was started has been cleaned up."; }

    # decision: host network + PID namespace and read-only host mounts give
    # node-exporter the host's per-NIC and per-disk view, not the container's.
    # rslave on / so mounts made after the exporter starts (bench volumes)
    # are visible.
    # decision: nfs and smb file systems are excluded from the filesystem
    # collector so a scrape never sends FSSTAT to a DittoFS mount under
    # benchmark and never hangs on a stuck mount.
    runt run -d --name "$NODE_EXPORTER_CONTAINER" \
        --network host --pid host \
        --user 65534:65534 --cap-drop ALL --security-opt no-new-privileges \
        --label "$LABEL" \
        -v /proc:/host/proc:ro -v /sys:/host/sys:ro -v /:/rootfs:ro,rslave \
        "${pull[@]}" \
        "$NODE_EXPORTER_IMAGE" \
        --path.procfs=/host/proc --path.sysfs=/host/sys --path.rootfs=/rootfs \
        --path.udev.data=/rootfs/run/udev/data \
        --web.listen-address="$gateway:$NODE_EXPORTER_PORT" \
        --collector.filesystem.fs-types-exclude='^(autofs|binfmt_misc|bpf|cgroup2?|configfs|debugfs|devpts|devtmpfs|fusectl|hugetlbfs|iso9660|mqueue|nsfs|overlay|proc|procfs|pstore|rpc_pipefs|securityfs|selinuxfs|squashfs|erofs|sysfs|tracefs|tmpfs|nfs4?|cifs|smb3|fuse\..*)$' \
        --collector.netdev.device-exclude='^(veth.*|lo)$' \
        >/dev/null \
        || { teardown; die "failed to start $NODE_EXPORTER_IMAGE; everything that was started has been cleaned up."; }

    wait_for_grafana || { warn "diagnostics from $LGTM_CONTAINER:"; runt logs --tail 50 "$LGTM_CONTAINER" || true; teardown; die "the stack did not come up; everything was cleaned up."; }
    wait_for_node_target || warn "the node exporter target is not up: a host firewall may block $BRIDGE_NAME — on ufw default-deny hosts: sudo ufw allow in on $BRIDGE_NAME"

    if [ "${VOLUME_WITHOUT_PASSWORD:-}" = 1 ]; then
        local code
        code="$(curl -s -o /dev/null -w '%{http_code}' -K - \
            "http://127.0.0.1:$GRAFANA_PORT/api/user" 2>/dev/null <<EOF2 || true
user = "admin:admin"
EOF2
)"
        if [ "$code" = "200" ]; then
            teardown
            die "Grafana accepted the DEFAULT admin:admin credentials (its volume was initialised without a password). Re-run with 'stop --purge' to start over. The containers were stopped; the volume was left alone."
        fi
    fi

    print_urls
    password_status
}

generate_password() {
    if [ -f "$PASSWORD_FILE" ]; then
        info "Grafana admin password: kept (first run printed it); file: $PASSWORD_FILE"
        return
    fi
    if runt volume inspect "$VOLUME" >/dev/null 2>&1; then
        warn "data volume $VOLUME already exists without a password file: the admin password is whatever that Grafana already has (admin:admin if the Grafana inside was never initialised with one). To reset: docker exec -i $LGTM_CONTAINER /otel-lgtm/grafana/bin/grafana cli --homepath /otel-lgtm/grafana admin reset-admin-password --password-from-stdin"
        VOLUME_WITHOUT_PASSWORD=1
        return
    fi
    local password
    # hex, not a tr|head pipeline: with pipefail, head closing the pipe
    # after 32 chars kills tr with SIGPIPE and fails the whole command.
    password="$(openssl rand -hex 16)"
    (umask 077; printf '%s' "$password" > "$PASSWORD_FILE")
    chmod 600 "$PASSWORD_FILE"
    info "Grafana 'admin' password (printed once, kept at $PASSWORD_FILE): $password"
}

# One unauthenticated probe per start, always re-done: a 401 can also be
# Grafana's brute-force lockout, so its result is never persisted as a marker.
# The probe reads the port the container really publishes (docker port), so
# the credential is never handed to whatever else sits on the env-var port.
password_status() {
    [ -f "$PASSWORD_FILE" ] || return 0
    local real_port
    real_port="$(runt port "$LGTM_CONTAINER" 3000/tcp 2>/dev/null || true)"
    real_port="$(printf '%s' "$real_port" | sed -n '1p' | cut -d: -f2)"
    real_port="${real_port%%[[:space:]]*}"
    real_port="${real_port:-$GRAFANA_PORT}"
    local code
    code="$(curl -s -o /dev/null -w '%{http_code}' -K - \
        "http://127.0.0.1:$real_port/api/user" 2>/dev/null <<EOF || true
user = "admin:$(cat "$PASSWORD_FILE")"
EOF
)"
    if [ "$code" = "200" ]; then
        warn "the generated admin password is still active — change it in Grafana (Profile → Change password). The password policy is enforced there."
    elif [ "$code" = "401" ]; then
        info "the generated admin password no longer logs in (changed, or lockout after failed attempts elsewhere)."
    fi
    return 0   # an unexpected probe result must never fail the start
}

wait_for_grafana() {
    info "waiting for Grafana to come up..."
    local _i # loop counter
    for _i in $(seq 1 90); do
        [ "$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$GRAFANA_PORT/api/health" 2>/dev/null || true)" = "200" ] && return 0
        sleep 2
    done
    return 1
}

wait_for_node_target() {
    info "waiting for the node exporter target..."
    local _i # loop counter
    for _i in $(seq 1 15); do
        if runt exec "$LGTM_CONTAINER" curl -s 'localhost:9090/api/v1/targets' 2>/dev/null \
            | grep '"job":"node"' >/dev/null \
            && runt exec "$LGTM_CONTAINER" curl -s 'localhost:9090/api/v1/targets' 2>/dev/null \
            | grep '"health":"up"' >/dev/null; then
            return 0
        fi
        sleep 2
    done
    return 1
}

grafana_url_port() {
    runt port "$LGTM_CONTAINER" 3000/tcp 2>/dev/null | sed -n '1p' | cut -d: -f2
}

print_urls() {
    local port
    port="$(grafana_url_port)"
    port="${port:-$GRAFANA_PORT}"
    info "Grafana:       http://127.0.0.1:$port  (localhost only; ssh -L $port:localhost:$port <host> on remote machines)"
    info "Dashboards:    'DittoFS' folder — Node Overview, Performance"
    info "Stop with:     scripts/monitoring.sh stop   (stop --purge also wipes the data)"
}

# ---------------------------------------------------------------------------
# stop / status
# ---------------------------------------------------------------------------
teardown() {
    # stop (SIGTERM + grace) before rm -f: the lgtm entrypoint has a graceful
    # shutdown handler, and a hard kill can leave Prometheus's TSDB dirty.
    runt stop -t 10 "$LGTM_CONTAINER" "$NODE_EXPORTER_CONTAINER" >/dev/null 2>&1 || true
    runt rm -f "$LGTM_CONTAINER" "$NODE_EXPORTER_CONTAINER" >/dev/null 2>&1 || true
    runt network rm "$NETWORK" >/dev/null 2>&1 || true
}

cmd_stop() {
    case "${1:-}" in
        ""|--purge) ;;
        *) die "unknown flag for stop: $1 (only --purge is supported)" ;;
    esac
    [ "$#" -le 1 ] || die "stop takes at most one flag (--purge)"
    local had_any
    had_any="$(runt ps -a --filter "label=$LABEL" --format '{{.Names}}' 2>/dev/null | head -1 || true)"
    teardown
    if [ -n "$had_any" ]; then
        info "stopped. Metric history is kept in the $VOLUME volume."
    else
        info "not running."
    fi
    if [ "${1:-}" = "--purge" ]; then
        runt volume rm "$VOLUME" >/dev/null 2>&1 || true
        if runt volume inspect "$VOLUME" >/dev/null 2>&1; then
            warn "the $VOLUME volume could not be removed (in use?); the password file was kept, since that Grafana still uses it."
        else
            rm -f "$PASSWORD_FILE" "$STATE/prometheus.yaml"
            info "purged: password removed; the next start generates a fresh one."
            if [ -n "$DATA_DIR" ]; then
                info "monitoring data was bind-mounted at $DATA_DIR — that directory was NOT deleted; remove it yourself if you want the history gone."
            fi
        fi
    fi
    exit 0
}

cmd_status() {
    local out
    if both_running; then
        local running_port
        running_port="$(grafana_url_port)"
        running_port="${running_port:-$GRAFANA_PORT}"
        out="running: Grafana at http://127.0.0.1:$running_port"
    else
        out="not running"
    fi
    runt ps -a --filter "label=$LABEL" --format 'table {{.Names}}\t{{.Status}}' || true
    info "$out"
}

# ---------------------------------------------------------------------------
main() {
    local cmd="${1:-}"; shift || true
    case "$cmd" in
        start)   prechecks 1; cmd_start "$@" ;;
        stop)    prechecks 1; cmd_stop "$@" ;;
        status)  [ "$(uname -s)" = "Linux" ] || die "The monitoring stack supports Linux only."
                 command -v "$RUNTIME" >/dev/null 2>&1 || die "$RUNTIME is not installed."
                 runt info >/dev/null 2>&1 || die "the $RUNTIME daemon is not reachable."
                 cmd_status ;;
        -h|--help|help|'') usage ;;
        *)       error "unknown command: $cmd"; usage; exit 1 ;;
    esac
}
main "$@"
