# Debugging Protocol Interop (SMB / NFS)

When a conformance test fails against DittoFS but passes against Samba, Windows, or a Linux
NFS server, **go straight to a byte-level pcap diff.** Reading source alone misses the things
that actually break interop: SPNEGO token layout, NTLMSSP flag ordering, SMB2 error body
format (MS-SMB2 §2.2.2), `mechListMIC` presence, nonce derivation, and similar wire-level
details.

The reliable method is: run the same test against a reference server and against DittoFS,
capture both with `tcpdump`, and diff the decoded packets with `tshark`.

## Reference servers

| Image | Use |
|-------|-----|
| `dperson/samba:latest` | quick SMB reference |
| `quay.io/samba/samba:latest` | full SMB reference (DC-capable) |
| `erichough/nfs-server` | NFSv3 / NFSv4 reference |

On Apple Silicon, force `--platform linux/amd64` on every container.

## Workflow

```bash
# 1. Reference SMB server on port 11445 (DittoFS owns 12445 — keep them distinct)
docker run --rm -d --name ref-server --platform linux/amd64 -p 11445:445 \
  -v /tmp/ref-share:/share \
  dperson/samba:latest \
  -u "testuser;TestPassword01!" \
  -s "share;/share;yes;no;no;testuser"

# 2. Capture on the reference server (capture DittoFS traffic similarly on its port)
docker exec -u root -d ref-server tcpdump -i any -w /tmp/ref.pcap -s 0 port 445

# 3. Run the same test against each server

# 4. Diff the decoded packets with tshark.
#    DittoFS runs SMB on a non-standard port, so tshark needs the NBSS dissector hint.
docker run --rm -v /tmp:/tmp --platform linux/amd64 nicolaka/netshoot \
  tshark -r /tmp/ref.pcap -V \
  -Y "smb2.cmd==1 and smb2.nt_status==0 and smb2.flags.response==1" \
  -c 1 2>/dev/null | grep -iE "spnego|negtoken|mechListMIC|supportedMech"

docker run --rm -v /tmp:/tmp --platform linux/amd64 nicolaka/netshoot \
  tshark -r /tmp/dittofs.pcap -d tcp.port==12445,nbss -V \
  -Y "smb2.cmd==1 and smb2.nt_status==0 and smb2.flags.response==1" \
  -c 1 2>/dev/null | grep -iE "spnego|negtoken|mechListMIC|supportedMech"
```

## Pitfalls

- Force `--platform linux/amd64` on Apple Silicon.
- `tshark` needs `-d tcp.port==N,nbss` for SMB on any port other than 445.
- Always `docker compose down -v` between runs so stale state doesn't leak.
- Keep both pcaps for post-mortem comparison.

## Reference implementations

When in doubt about protocol correctness, compare against the canonical implementations — our
behavior sometimes drifts in subtle ways:

- NFS: <https://github.com/torvalds/linux/tree/master/fs/nfs>
- SMB: <https://github.com/samba-team/samba>

## Viewing metrics and traces

A dev node gives a laptop two kinds of telemetry to watch. Its series, on the
metrics listener the bootstrap's `metrics.listen` names — a start refuses an
empty one. And, behind the `DITTOFS_OTLP_ENDPOINT` environment gate, its
journals' spans, over OTLP/HTTP: no key in the configuration file turns tracing
on, the gate is the environment alone, and unset it leaves the node untraced.

Run the Grafana LGTM stack — Loki, Grafana, Tempo and Prometheus in one image,
Grafana on 3000 and OTLP/HTTP on 4318. Prometheus in the image needs the node
as a scrape target: the image's own config at `/otel-lgtm/prometheus.yaml`
carries no scrape (its metrics arrive over OTLP), and a mounted file replaces
it wholesale, so start from the image's own (`docker/prometheus.yaml` in the
`grafana/docker-otel-lgtm` repository, whose `otlp` and `storage` sections
the image's dashboards need) and append, for a node configured with
`metrics.listen: 127.0.0.1:9100`, the job below
(`host.docker.internal` reaches the host from Docker Desktop; on Linux pass
`--add-host=host.docker.internal:host-gateway`):

```yaml
scrape_configs:
  - job_name: dittofs
    static_configs:
      - targets: ["host.docker.internal:9100"]
```

Mount it where the image reads it:

```sh
docker run --name otel-lgtm -p 3000:3000 -p 4318:4318 \
  -v "$(pwd)/prometheus.yaml:/otel-lgtm/prometheus.yaml:ro" \
  grafana/otel-lgtm:latest
```

Then start the node with the gate:

```sh
DITTOFS_OTLP_ENDPOINT=http://localhost:4318 dfs start
```

The export is unauthenticated OTLP/HTTP to whatever the variable names, in
clear for an `http` URL, and every span carries the journal's identity: point it only at a collector you
trust, on a network you control.

What to look for, in Grafana's Explore:

- **Series**, from Prometheus: every journal series is prefixed
  `dittofs_journal_` — `dittofs_journal_syncs_total` over a committed write,
  `dittofs_journal_reservation_refused_total` as a fill reaches the journal's
  limit — each labelled with the `journal` identity it belongs to.
- **Spans**, from Tempo, under the service `dittofs`: each journal opens one
  per sync (`journal.sync`), per refused reservation
  (`journal.reserve_refused`), per repack pass (`journal.repack_pass`), the
  open (`journal.open`, its phases `journal.open.*`) over a written segment
  on a restart. Every span carries the `journal` attribute — the same 32 hex
  characters of identity the series label holds — so a trace filters to one
  journal the same way its series do, by the same value.

When the node stops and the start unwinds, the provider flushes what it
recorded within a five-second bound and shuts down — enough for a collector
on the same laptop, but a collector that will not answer loses that run's
spans rather than holding the exit, and a second termination signal ends the
process before the flush runs at all.
