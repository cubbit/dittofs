# Prints RFC 5 §2.8's three chain-ID vectors and the wrong variants §8.2's
# §2.8 row names: an independent implementation of the chain ID, working from
# §2.8's byte table alone, with BLAKE3's key-derivation mode from the blake3
# package. Run: python3 -I testdata/chain-id-v1.py (needs pip's blake3).
import hashlib, hmac, struct

import blake3

CONTEXT = "dittofs transform chain id v1"


def fingerprint(material):
    # HMAC-SHA256(material, "dittofs key check v1"), RFC 5 §2.5.
    return hmac.new(material, b"dittofs key check v1", hashlib.sha256).digest()


def prefixed(field, width=4, order=">"):
    return struct.pack(order + ("I" if width == 4 else "H"), len(field)) + field


def encode(chain, width=4, order=">", sort=True, flip=False):
    # chain: [(transform ID, format version, length key, [(kind, ID, material)])]
    out = bytes([len(chain)])
    for tid, version, lkey, materials in chain:
        out += struct.pack(">H", tid) + bytes([version]) + prefixed(lkey, width, order)
        if sort:
            materials = sorted(materials, key=lambda m: (m[0], m[1]))
        out += bytes([len(materials)])
        for kind, mid, material in materials:
            fp = bytearray(fingerprint(material))
            if flip:
                fp[0] ^= 1
            out += prefixed(kind, width, order) + prefixed(mid, width, order) + bytes(fp)
    return out


def chain_id(encoded):
    return blake3.blake3(encoded, derive_key_context=CONTEXT).hexdigest()


# The encrypt transform is ID 0x0010, format version 1, with an empty length
# key (RFC 5 Appendix B.1); zstd is ID 0x0001, format version 1, its length key
# the level as one signed byte (RFC 5 Appendix A). The compress-then-encrypt
# chain's encrypt entry declares its two kinds in the order that kind-then-ID
# order reverses.
d1 = (b"data-key", b"d1", bytes(range(0x00, 0x20)))
d2 = (b"data-key", b"d2", bytes(range(0x20, 0x40)))
c1 = (b"chunk-id-key", b"c1", bytes(range(0x40, 0x60)))
vectors = {
    "empty": [],
    "encrypt": [(0x0010, 1, b"", [d1])],
    "compress-then-encrypt": [(0x0001, 1, bytes([3]), []), (0x0010, 1, b"", [d2, c1])],
}
for name, chain in vectors.items():
    print(name, chain_id(encode(chain)))
two = vectors["compress-then-encrypt"]
print("unsorted", chain_id(encode(two, sort=False)))
print("fingerprint-flipped", chain_id(encode(two, flip=True)))
print("prefix-2-byte", chain_id(encode(two, width=2)))
print("prefix-little-endian", chain_id(encode(two, order="<")))
