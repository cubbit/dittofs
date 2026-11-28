#!/usr/bin/env python3
"""Computes the key-service stored-wrap vector kmip_test.go checks, from RFC 5
Appendix B.1's stored-wrap layout and the KMIP profile's wrap bytes (version
1 || IV length, 2 bytes big-endian || IV || tag, 16 bytes || data), with an
AES-GCM implementation other than Go's standing in for the server's Encrypt.

Run: python3 kmip-stored-wrap-v1.py (needs the `cryptography` package).
"""
import struct

from cryptography.hazmat.primitives.ciphers.aead import AESGCM

master_key = bytes(range(0x00, 0x20))
endpoint = b"127.0.0.1:5696"
uid = b"3"
iv = bytes(range(0xB0, 0xC0))  # 16 bytes, as PyKMIP draws
namespace = bytes(range(0x30, 0x40))
kind = b"data-key"
key_id = bytes(range(0x40, 0x50))
key = bytes(range(0x50, 0x70))


def prefixed(field):
    return struct.pack(">I", len(field)) + field


aad = prefixed(namespace) + prefixed(kind) + prefixed(key_id)
sealed = AESGCM(master_key).encrypt(iv, key, aad)
data, tag = sealed[:-16], sealed[-16:]
wrap = bytes([1]) + struct.pack(">H", len(iv)) + iv + tag + data
master_key_id = endpoint + b"/" + uid
stored = bytes([0x02]) + struct.pack(">H", len(master_key_id)) + master_key_id + struct.pack(">I", len(wrap)) + wrap

print("tag   ", tag.hex())
print("data  ", data.hex())
print("stored", stored.hex())
