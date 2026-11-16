#!/bin/sh
# Regenerates the PACs captured from the Samba fixture, with the keys that
# verify them: tgt.pac with krbtgt.keytab, and service.pac, from alice's
# cifs/ service ticket, with service.keytab. It starts a fixture DC, so it
# needs what the ad_dc tests need: Docker and a DC IP the host can reach.
#
#   test/integration/ad-dc/capture-pacs.sh <output dir>
#
# Every run provisions a new domain, so its keys, SIDs and PACs all change.
set -eu
out="${1:?usage: $0 <output dir>}"
mkdir -p "$out"
out="$(cd "$out" && pwd)"
cd "$(dirname "$0")"
DITTOFS_AD_DC_PAC_OUT="$out" exec go test -tags=ad_dc -timeout=20m -count=1 -v -run '^TestADPACCapture$' .
