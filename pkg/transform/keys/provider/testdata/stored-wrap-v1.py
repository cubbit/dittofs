#!/usr/bin/env python3
"""Computes the key-file stored-wrap vector wrap_test.go checks, from RFC 5
Appendix B.1's text alone, with an AES-GCM implementation other than Go's.

Run: python3 stored-wrap-v1.py (needs the `cryptography` package).
"""
import struct

from cryptography.hazmat.primitives.ciphers.aead import AESGCM

master_key = bytes(range(0x00, 0x20))
master_key_id = bytes(range(0x10, 0x20))
nonce = bytes(range(0xA0, 0xAC))
namespace = bytes(range(0x30, 0x40))
kind = b"data-key"
key_id = bytes(range(0x40, 0x50))
key = bytes(range(0x50, 0x70))


def prefixed(field):
    return struct.pack(">I", len(field)) + field


aad = prefixed(namespace) + prefixed(kind) + prefixed(key_id)
wrap = nonce + AESGCM(master_key).encrypt(nonce, key, aad)
stored = bytes([0x01]) + struct.pack(">H", len(master_key_id)) + master_key_id + struct.pack(">I", len(wrap)) + wrap

print("aad   ", aad.hex())
print("stored", stored.hex())
