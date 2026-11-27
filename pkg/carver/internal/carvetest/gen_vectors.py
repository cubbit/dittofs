# /// script
# requires-python = ">=3.10"
# dependencies = ["blake3==1.0.8"]
# ///
"""Writes testdata/vectors.json: the carver's golden vectors.

Run by hand, from the repository root, whenever RFC 2 §3.2 or §4.2 changes:

    uv run pkg/carver/internal/carvetest/gen_vectors.py

uv reads the pinned dependency above; without uv, `pip install blake3==1.0.8`
and run the file with python3. blake3 is the BLAKE3 team's binding of their
Rust implementation, so the values here come from code the Go carver does not
share. CI never runs this script: vectors_test.go recomputes every value in Go
and fails on any disagreement.
"""

import json
import pathlib
import struct

from blake3 import blake3

GEAR_CONTEXT = "dittofs gear table v1"
CHUNKING_INPUT = b"dittofs chunking key v1"
NAME_CONTEXT = "dittofs content-defined block name v2"
CHAIN_CONTEXT = "dittofs transform chain id v1"
GEAR_BYTES = 2048


def pattern(n, start=0):
    """n bytes counting up from start, wrapping at 256."""
    return bytes((start + i) % 256 for i in range(n))


def derive_key(context, material, n):
    return blake3(material, derive_key_context=context).digest(length=n)


def keyed(key, data, n):
    return blake3(data, key=key).digest(length=n)


def gear(table):
    """The table's bytes with its first and last words, read little-endian."""
    words = struct.unpack("<256Q", table)
    return {"bytes": table.hex(), "first_word": str(words[0]), "last_word": str(words[-1])}


def encode_chain(transforms):
    """RFC 5 §2.8's encoded chain.

    transforms is a list, in stage order, of dicts with "id" (int),
    "version" (int), "length_key" (bytes) and "materials", a list of dicts
    with "kind" (ASCII str), "id" (bytes) and "fingerprint" (32 bytes).
    """
    if len(transforms) > 3:
        raise ValueError("a chain holds at most three transforms")
    out = bytearray([len(transforms)])
    for t in transforms:
        out += struct.pack(">HB", t["id"], t["version"])
        out += struct.pack(">I", len(t["length_key"])) + t["length_key"]
        materials = sorted(t["materials"], key=lambda m: (m["kind"].encode("ascii"), m["id"]))
        out.append(len(materials))
        for m in materials:
            kind = m["kind"].encode("ascii")
            if len(m["fingerprint"]) != 32:
                raise ValueError("a fingerprint is 32 bytes")
            out += struct.pack(">I", len(kind)) + kind
            out += struct.pack(">I", len(m["id"])) + m["id"]
            out += m["fingerprint"]
    return bytes(out)


def chain_id(transforms):
    return derive_key(CHAIN_CONTEXT, encode_chain(transforms), 32)


def block_name(scope, nonce, chain, ids):
    """RFC 2 §4.2: len(scope) ‖ scope ‖ nonce ‖ len(chain) ‖ chain ‖ h₁ … hₙ."""
    if len(scope) != 16 or len(nonce) != 16 or len(chain) != 32:
        raise ValueError("scope and nonce are 16 bytes, the chain ID 32")
    material = bytes([len(scope)]) + scope + nonce + bytes([len(chain)]) + chain + b"".join(ids)
    return derive_key(NAME_CONTEXT, material, 32)


def vectors():
    chunking_key = pattern(32, 0x40)
    chunk_id_key = pattern(32, 0x80)
    # Longer than one BLAKE3 chunk (1,024 bytes), so the tree is exercised.
    chunk_input = bytes(i % 251 for i in range(4096))

    # One transform: ID 0x0001, format version 1, a one-byte length key, no material.
    transforms = [{"id": 0x0001, "version": 1, "length_key": b"\x03", "materials": []}]
    chain = chain_id(transforms)
    scope = pattern(16, 0x10)
    nonce = pattern(16, 0xA0)
    ids = [pattern(32, start) for start in (0xC0, 0xE0, 0x00)]

    return {
        "gear_unkeyed": {"context": GEAR_CONTEXT, **gear(derive_key(GEAR_CONTEXT, b"", GEAR_BYTES))},
        "gear_keyed": {
            "key": chunking_key.hex(),
            "input": CHUNKING_INPUT.decode("ascii"),
            **gear(keyed(chunking_key, CHUNKING_INPUT, GEAR_BYTES)),
        },
        "chunk_id": {
            "key": chunk_id_key.hex(),
            "input": chunk_input.hex(),
            "id": keyed(chunk_id_key, chunk_input, 32).hex(),
        },
        "name": {
            "context": NAME_CONTEXT,
            "scope": scope.hex(),
            "nonce": nonce.hex(),
            "chain": {
                "context": CHAIN_CONTEXT,
                "transforms": [
                    {"id": t["id"], "version": t["version"], "length_key": t["length_key"].hex()}
                    for t in transforms
                ],
                "encoded": encode_chain(transforms).hex(),
                "id": chain.hex(),
            },
            "ids": [h.hex() for h in ids],
            "name": block_name(scope, nonce, chain, ids).hex(),
        },
    }


if __name__ == "__main__":
    out = pathlib.Path(__file__).parent / "testdata" / "vectors.json"
    out.parent.mkdir(exist_ok=True)
    out.write_text(json.dumps(vectors(), indent=2) + "\n")
