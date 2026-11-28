#!/usr/bin/env python3
"""Computes the fingerprint vector record_test.go checks, from RFC 5 §2.5's
text alone, with Python's HMAC rather than Go's.

Run: python3 fingerprint-v1.py
"""
import hashlib
import hmac

material = bytes(range(0x50, 0x70))
print(hmac.new(material, b"dittofs key check v1", hashlib.sha256).hexdigest())
