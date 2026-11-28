#!/usr/bin/env python3
"""Computes the salvage-key wrap vector salvage_test.go checks, from RFC 5
Appendix B.1's key wrap and stored wrap with an empty namespace ID and kind
salvage-key, with an AES-GCM implementation other than Go's.

Run: python3 salvage-wrap-v1.py (needs the `cryptography` package).
"""
import struct

from cryptography.hazmat.primitives.ciphers.aead import AESGCM

master_key = bytes(range(0x00, 0x20))
master_key_id = bytes(range(0x10, 0x20))
salvage_key = bytes(range(0x50, 0x70))
nonce = bytes(range(0xA0, 0xAC))
export = b"journal-7/tag-3"


def prefixed(field):
    return struct.pack(">I", len(field)) + field


aad = prefixed(b"") + prefixed(b"salvage-key") + prefixed(export)
wrap = nonce + AESGCM(master_key).encrypt(nonce, salvage_key, aad)
stored = bytes([0x01]) + struct.pack(">H", len(master_key_id)) + master_key_id + struct.pack(">I", len(wrap)) + wrap

print("aad   ", aad.hex())
print("stored", stored.hex())
