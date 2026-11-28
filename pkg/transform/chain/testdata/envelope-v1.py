# Prints RFC 5 §2.4's envelope vectors: bodies of 0, 1 and 3 transform IDs
# over the payload "dittofs", an independent implementation of envelope
# version 1 working from §2.4's layout alone. Run: python3 -I testdata/envelope-v1.py
import struct

PAYLOAD = b"dittofs"


def body(ids):
    # version (1 byte), count (1 byte), each ID 2 bytes big-endian, payload.
    return bytes([1, len(ids)]) + b"".join(struct.pack(">H", i) for i in ids) + PAYLOAD


for name, ids in [("none", []), ("one", [0x8002]), ("three", [0x8001, 0x8002, 0x8003])]:
    print(name, body(ids).hex())
