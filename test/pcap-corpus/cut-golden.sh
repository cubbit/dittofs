#!/usr/bin/env bash
#
# cut-golden.sh — cut one message out of a capture into a golden .hex file.
#
# tshark gives the frame's message: the TCP payload reassembled across
# segments when the frame completes a reassembly, the frame's own TCP payload
# otherwise, or a UDP datagram whole. The framing is then stripped:
#
#   rpc   RFC 5531 §11's record mark; a record of more than one fragment is
#         refused, naming the frame
#   smb2  MS-SMB2 2.1's direct-TCP header: a zero byte, a 3-byte length
#   none  nothing, for a datagram
#
# The framing's length must cover the rest of the payload exactly, so a frame
# holding part of a message, or more than one, is refused too.
#
# The output is the format the wire packages' golden readers read: hex digits,
# whitespace ignored, '#' to the end of a line a comment. Its header names the
# capture, the frame and the tshark command that cut it.
#
# Run by hand where captures are recorded; only that machine needs tshark.
#
# Exit status: 0 = written, 1 = the frame refused, 2 = usage/tooling error.
#
# Usage:
#   cut-golden.sh <capture> <frame> <rpc|smb2|none> <out.hex>

set -euo pipefail

usage() { grep '^#' "$0" | sed -n '3,$s/^# \{0,1\}//p'; exit 2; }
[[ $# -eq 4 ]] || usage
cap="$1" frame="$2" framing="$3" out="$4"
[[ "$frame" =~ ^[1-9][0-9]*$ ]] || { echo "frame must be a frame number: $frame" >&2; exit 2; }
case "$framing" in rpc|smb2|none) ;; *) echo "unknown framing: $framing" >&2; exit 2 ;; esac
[[ -r "$cap" ]] || { echo "cannot read capture: $cap" >&2; exit 2; }
command -v tshark >/dev/null || { echo "tshark not found: install wireshark-cli (nix shell nixpkgs#wireshark-cli)" >&2; exit 2; }

cmd=(tshark -r "$cap" -Y "frame.number==$frame" -T fields
  -e tcp.reassembled.data -e tcp.payload -e udp.payload)
fields="$("${cmd[@]}")"

# The first non-empty field, in the order -e names them. Older tshark prints
# bytes colon-separated; a comma means the field occurs more than once.
hex=""
IFS=$'\t' read -r -a values <<<"$fields" || true
for v in "${values[@]}"; do
  [[ -n "$v" ]] && { hex="${v//:/}"; break; }
done
[[ -n "$hex" ]] || { echo "frame $frame: no TCP or UDP payload" >&2; exit 1; }
[[ "$hex" != *,* ]] && [[ "$hex" =~ ^[0-9a-f]+$ ]] \
  || { echo "frame $frame: more than one payload in the frame" >&2; exit 1; }

total=$(( ${#hex} / 2 ))
case "$framing" in
  rpc)
    (( total >= 4 )) || { echo "frame $frame: shorter than a record mark" >&2; exit 1; }
    mark=$(( 16#${hex:0:8} ))
    (( mark & 0x80000000 )) \
      || { echo "frame $frame: a record of more than one fragment" >&2; exit 1; }
    len=$(( mark & 0x7fffffff )) hdr=4 ;;
  smb2)
    (( total >= 4 )) && [[ "${hex:0:2}" == 00 ]] \
      || { echo "frame $frame: no direct-TCP header" >&2; exit 1; }
    len=$(( 16#${hex:2:6} )) hdr=4 ;;
  none)
    len=$total hdr=0 ;;
esac
(( len == total - hdr )) \
  || { echo "frame $frame: framing says $len bytes, the payload holds $(( total - hdr ))" >&2; exit 1; }

{
  echo "# $cap, frame $frame, cut by test/pcap-corpus/cut-golden.sh with framing $framing:"
  printf '# %s\n' "${cmd[*]}"
  # 16 bytes a line, in 4-byte groups.
  printf '%s\n' "${hex:$(( hdr * 2 ))}" | fold -w 32 | sed -E 's/(.{8})/\1 /g; s/ $//'
} >"$out"
