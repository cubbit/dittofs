# Writes keys-object-v1.bin and prints its keys key and keys MAC: an independent
# implementation of the keys object, working from RFC 5 Appendix B.1's keys key
# and keys MAC and the object's version 1 layout alone, with HKDF by hand from
# RFC 5869. Run: python3 -I testdata/keys-object-v1.py testdata/keys-object-v1.bin
import hashlib, hmac, struct, sys

ns = bytes(range(16))
seq = 7
export_key_id = b"export-1"
export_key = bytes(range(0x20, 0x40))
rows = [  # (kind, ID, state, record), in the byte-wise order of kind, then ID
    (b"chunk-id-key", b"c1", b"current", b"\x01" * 40),
    (b"data-key", b"d1", b"retired", b"\x02" * 60),
    (b"data-key", b"d2", b"current", b"\x03" * 60),
    (b"export-key", b"export-1", b"current", b"\x04" * 60),
]


def prefixed(field):
    return struct.pack(">I", len(field)) + field


def hkdf_sha256(ikm, salt, info, n=32):
    prk = hmac.new(salt, ikm, hashlib.sha256).digest()
    t, out, i = b"", b"", 1
    while len(out) < n:
        t = hmac.new(prk, t + info + bytes([i]), hashlib.sha256).digest()
        out += t
        i += 1
    return out[:n]


records = struct.pack(">Q", len(rows)) + b"".join(
    prefixed(k) + prefixed(i) + prefixed(s) + prefixed(r) for k, i, s, r in rows
)
keys_key = hkdf_sha256(export_key, ns, b"dittofs keys object v1")
mac = hmac.new(
    keys_key,
    prefixed(ns) + struct.pack(">Q", seq) + prefixed(export_key_id) + prefixed(hashlib.sha256(records).digest()),
    hashlib.sha256,
).digest()
obj = bytes([1]) + prefixed(ns) + struct.pack(">Q", seq) + prefixed(export_key_id) + records + mac
with open(sys.argv[1], "wb") as f:
    f.write(obj)
print("keys key", keys_key.hex())
print("keys MAC", mac.hex())
