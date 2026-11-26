#!/usr/bin/env bash
# Sets up DittoFS in a throwaway rootless podman container, then opens a shell in it or runs
# scenarios, each in its own container. Silent unless a scenario fails.
#   test/scenarios/setup.sh                       a bash shell in the set-up container
#   test/scenarios/setup.sh a.sh                  one scenario
#   test/scenarios/setup.sh a.sh b.sh | all       several; one PASS, FAIL or SLOW line each
# CACHE (/var/tmp/dittofs-scenarios) keeps the Go caches and the kept logs. How a run is reported,
# with its timing and Slack messages, is in lib/report.sh. SCENARIO_TIMEOUT caps a run in seconds
# (default 900). CACHE is the one volume, at /cache; CACHE/inputs is at /inputs, for large inputs
# worth keeping, and the run's logs directory at /logs.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
CACHE="${CACHE:-/var/tmp/dittofs-scenarios}"
# shellcheck source-path=SCRIPTDIR source=lib/report.sh
. "$HERE/lib/report.sh"
SCENARIOS=("$@")
if test "${SCENARIOS[0]:-}" = all; then
    SCENARIOS=()
    for F in "$HERE"/*.sh; do test "${F##*/}" = setup.sh || SCENARIOS+=("${F##*/}"); done
fi
for SCENARIO in "${SCENARIOS[@]}"; do test -f "$HERE/$SCENARIO"; done

mkdir -p "$CACHE/go/mod" "$CACHE/go/build" "$CACHE/failed" "$CACHE/slow" "$CACHE/inputs"
P=(podman --cgroup-manager cgroupfs --events-backend none)
"${P[@]}" pull -q --policy missing docker.io/library/ubuntu:26.04 docker.io/library/golang:1.26 docker.io/chrislusf/seaweedfs:4.48 >/dev/null
IMAGES=(--mount "type=image,source=docker.io/library/golang:1.26,destination=/opt/go"
    --mount "type=image,source=docker.io/chrislusf/seaweedfs:4.48,destination=/opt/seaweedfs"
    -v "$HERE/../..:/src:ro" -v "$CACHE:/cache")

# The setup that runs inside every container: tools, DittoFS, SeaweedFS, share /test.
SETUP="$(cat <<'SETUP'
set -euxo pipefail
export DEBIAN_FRONTEND=noninteractive
ln -s /cache/inputs /inputs
ln -s "/cache/$LOGS" /logs

# Downloads are kept in CACHE/downloads and reused: apt's packages and lists, so a run updates the
# lists only when they are a day old or the packages fail to download, and the libnfs source,
# fetched once and checked every run.
mkdir -p /cache/downloads/apt/partial /cache/downloads/apt-lists/partial
echo 'Dir::Cache::Archives "/cache/downloads/apt"; Dir::State::Lists "/cache/downloads/apt-lists";' >/etc/apt/apt.conf.d/99cache
rm -f /etc/apt/apt.conf.d/docker-clean

# Tools: SMB and NFS clients, rclone for S3, libnfs's nfs-io example (for NFS deletes) built from its
# sha256-checked source, and the scenario tools in lib/tools, the C ones built here. The packages
# are downloaded (-d) before anything installs, so a failed try leaves nothing half installed.
# apt-updated dates the last update, since apt dates each list as the mirror does. autoclean then
# drops the packages the lists no longer offer, so superseded versions don't pile up in the cache.
TOOLS=(ca-certificates curl jq rclone smbclient libnfs-utils libnfs-dev libsmb2-dev libc6-dev tcc)
test -n "$(find /cache/downloads -maxdepth 1 -name apt-updated -mmin -1440)" && apt-get install -d -y -qq --no-install-recommends "${TOOLS[@]}" >/dev/null || { apt-get update -qq; touch /cache/downloads/apt-updated; }
apt-get install -y -qq --no-install-recommends "${TOOLS[@]}" >/dev/null
apt-get autoclean -qq
test -f /cache/downloads/libnfs-5.0.2.tar.gz || { curl -sfL -o /cache/downloads/libnfs-5.0.2.tar.gz.part https://github.com/sahlberg/libnfs/archive/refs/tags/libnfs-5.0.2.tar.gz && mv /cache/downloads/libnfs-5.0.2.tar.gz.part /cache/downloads/libnfs-5.0.2.tar.gz; }
echo '637e56643b19da9fba98f06847788c4dad308b723156a64748041035dcdf9bd3  /cache/downloads/libnfs-5.0.2.tar.gz' | sha256sum -c --quiet
tar xzf /cache/downloads/libnfs-5.0.2.tar.gz -C /tmp
tcc -o /usr/local/bin/nfs-io /tmp/libnfs-libnfs-5.0.2/examples/nfs-io.c -lnfs
for T in /src/test/scenarios/lib/tools/*; do
    case "$T" in *.c) tcc -o "/usr/local/bin/$(basename "$T" .c)" "$T" -lnfs -lsmb2 ;; *) install "$T" /usr/local/bin/ ;; esac
done

# DittoFS, built from this checkout, and scenario-keygen, which makes a key file: dfs init makes the
# storage key but not the protocol key, and no command makes one. It joins the read-only checkout
# through an overlay, so it builds against the checkout's go.mod.
mkdir /tmp/keygen
cat >/tmp/keygen/main.go <<'KEYGEN'
package main

import (
	"os"

	"github.com/cubbit/dittofs/pkg/keyprovider"
)

func main() {
	for _, path := range os.Args[2:] {
		key, err := keyprovider.GenerateKeyFile(os.Args[1])
		if err != nil {
			panic(err)
		}
		if err := os.WriteFile(path, key, 0o600); err != nil {
			panic(err)
		}
	}
}
KEYGEN
echo '{"Replace": {"/src/cmd/scenario-keygen/main.go": "/tmp/keygen/main.go"}}' >/tmp/keygen/overlay.json
cd /src
CGO_ENABLED=0 GOFLAGS=-buildvcs=false GOMODCACHE=/cache/go/mod GOCACHE=/cache/go/build /opt/go/usr/local/go/bin/go build -overlay /tmp/keygen/overlay.json -o /usr/local/bin/ ./cmd/dfs ./cmd/dfsctl ./cmd/scenario-keygen
cd /

# A local S3 (SeaweedFS, run by s3 start) with bucket dittofs. rclone reaches it as s3:, and DittoFS
# stores through dfs-store, with the credential in /etc/dittofs-s3.credential.
export RCLONE_CONFIG_S3_TYPE=s3 RCLONE_CONFIG_S3_PROVIDER=SeaweedFS RCLONE_CONFIG_S3_ENDPOINT=http://127.0.0.1:9000 RCLONE_CONFIG_S3_ACCESS_KEY_ID=test RCLONE_CONFIG_S3_SECRET_ACCESS_KEY=test-secret RCLONE_LOG_LEVEL=ERROR
echo '{"identities":[{"name":"test","credentials":[{"accessKey":"test","secretKey":"test-secret"}],"actions":["Admin","Read","Write","List","Tagging"]}]}' >/tmp/seaweedfs-identities.json
mkdir /tmp/seaweedfs
s3 start
rclone mkdir s3:dittofs
echo '{"access_key_id":"test","secret_access_key":"test-secret"}' >/etc/dittofs-s3.credential

# DittoFS: its configuration, an API and a metrics listener each on a port the kernel picks (dfs-server
# reads the API's from the log), and its two wrapping keys under one passphrase, which stays out of the
# trace. dfs init prints the administrator's password, which dfs-server's first start signs in with
# and changes.
mkdir -p /etc/dittofs /var/lib/dittofs/journal ~/.config/dittofs
(umask 077 && head -c 24 /dev/urandom | base64 >/etc/dittofs/passphrase)
{ set +x; } 2>/dev/null
scenario-keygen "$(cat /etc/dittofs/passphrase)" /etc/dittofs/storage.key /etc/dittofs/protocol.key
set -x
cat >~/.config/dittofs/config.yaml <<CONFIG
node: { id: $(cat /proc/sys/kernel/random/uuid) }
journals: [ { path: /var/lib/dittofs/journal } ]
keys:
  storage: { kind: local, local: { file: /etc/dittofs/storage.key } }
  protocol: { kind: local, local: { file: /etc/dittofs/protocol.key } }
api: { listen: ['127.0.0.1:0'] }
metrics: { listen: '127.0.0.1:0' }
CONFIG
{ set +x; } 2>/dev/null
(umask 077 && DITTOFS_ENCRYPTION_PASSPHRASE="$(cat /etc/dittofs/passphrase)" dfs init >/etc/dittofs/init.out)
DITTOFS_ENCRYPTION_PASSPHRASE="$(cat /etc/dittofs/passphrase)" dfs config validate
set -x
dfs-server start

# Share test (path /test, namespace prefix test/ on bucket dittofs) for user tester (uid and group
# 1000), SMB on 445 and NFS on 2049. smbclient defaults to SMB3 and, through USER, to tester.
S="$(dfs-store dittofs)"
N="$(dfsctl namespace create --store "$S" --setting namespace.prefix=test/ -o json | jq -r .id)"
dfsctl share create test --path /test --namespace "$N" --flavor sys --client 0.0.0.0/0=read-write
dfsctl user create tester --uid 1000
dfsctl group create tester --gid 1000
dfsctl group add-member tester tester
echo tester-secret >/tmp/tester.password
dfsctl user set-password tester --password-file /tmp/tester.password
dfsctl grant set test --user tester --access read-write
dfsctl adapter set smb --enabled --setting port=445
dfsctl adapter set nfs --enabled --setting port=2049
mkdir -p /etc/samba
printf '[global]\nclient min protocol = SMB3\n' >/etc/samba/smb.conf
SETUP
)"

# No scenario: a shell in a set-up container, removed when the shell exits. It takes no lock and
# keeps its logs apart, so an open shell never blocks a scheduled run.
if test "${#SCENARIOS[@]}" -eq 0; then
    mkdir -p "$CACHE/shell/logs"
    exec "${P[@]}" run --rm -it "${IMAGES[@]}" -e LOGS=shell/logs \
        docker.io/library/ubuntu:26.04 bash -c "$SETUP
set +x
echo 'Set up: share test (/test), user tester, SMB on 445, NFS on 2049, logs in /logs.'
echo 'Run a scenario: scenario NAME (see ls /src/test/scenarios). Leave: exit.'
export USER=tester%tester-secret
exec bash"
fi

# One run at a time per CACHE. A second run fails rather than exiting 0: a scheduled run that tested
# nothing must not read as a pass. 75 (EX_TEMPFAIL) tells it apart from a scenario failure (1).
exec 9<"$CACHE"
flock -n 9 || { echo "not run: another run holds $CACHE" >&2; exit 75; }

# A scenario runs after the same setup, which exits 3 when it fails, so a run that stopped in setup
# is told apart from a scenario's own failure. Each of the scenario's commands is traced as
# "+ <epoch seconds> <line> <command>".
NODE="set -E; trap 'exit 3' ERR
$SETUP
trap - ERR; set +E
$(cat <<'RUN'
USER=tester%tester-secret bash -c 'PS4="+ \$EPOCHREALTIME \$LINENO "; set -euxo pipefail; . /scenario.sh' || exit 1
RUN
)"

FAILED=0
SETUP_FAILED=0
for SCENARIO in "${SCENARIOS[@]}"; do
    rm -rf "$CACHE/run"
    mkdir -p "$CACHE/run/logs"
    timeout "${SCENARIO_TIMEOUT:-900}" "${P[@]}" run --rm -i --log-driver k8s-file "${IMAGES[@]}" \
        -v "$HERE/$SCENARIO:/scenario.sh:ro" -e LOGS=run/logs \
        --name "dittofs-${SCENARIO%.sh}" --replace \
        docker.io/library/ubuntu:26.04 bash -s <<<"$NODE" >"$CACHE/run/run.log" 2>&1 && CODE=0 || CODE=$?
    if test "$CODE" -eq 0; then RESULT=pass; else RESULT=fail; fi
    report "$SCENARIO" "$RESULT"
    test "$RESULT" = pass || FAILED=$((FAILED + 1))
    test "$CODE" -ne 3 || SETUP_FAILED=$((SETUP_FAILED + 1))
done
# 0 when every scenario passed, 3 when every failure stopped in setup, 1 otherwise.
test "$FAILED" -eq 0 || { test "$SETUP_FAILED" -eq "$FAILED" && exit 3; exit 1; }
