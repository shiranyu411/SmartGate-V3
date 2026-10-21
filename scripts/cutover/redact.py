#!/usr/bin/env python3
"""Redact device evidence before it is written to the workspace."""

import json
import re
import sys


SENSITIVE = r"(?:url|subscription_url|password|passwd|psk|key|private_key|token|token_hash|uuid|secret|auth|auth_str|authorization|cookie)"
QUOTED = r'"(?:[^"\\]|\\.)*"'
FIELD = re.compile(r'("' + SENSITIVE + r'"\s*:\s*)' + QUOTED, re.IGNORECASE)
URL = re.compile(r"[a-zA-Z][a-zA-Z0-9+.-]*://[^\s\"<>]+")
BEARER = re.compile(r"(Bearer\s+)[A-Za-z0-9._~+/-]+", re.IGNORECASE)
INLINE = re.compile(
    r"((?:password|passwd|psk|token|secret|\.key)\s*=\s*)(?:'[^']*'|\"[^\"]*\"|[^\s&]+)",
    re.IGNORECASE,
)


def text(value):
    value = FIELD.sub(r'\1"[REDACTED]"', value)
    value = URL.sub("[REDACTED-URL]", value)
    value = BEARER.sub(r"\1[REDACTED]", value)
    return INLINE.sub(r"\1[REDACTED]", value)


def sanitize(value):
    if isinstance(value, dict):
        return {
            key: "[REDACTED]" if re.fullmatch(SENSITIVE, key, re.IGNORECASE) else sanitize(item)
            for key, item in value.items()
        }
    if isinstance(value, list):
        return [sanitize(item) for item in value]
    if isinstance(value, str):
        return text(value)
    return value


def redact(value):
    try:
        parsed = json.loads(value)
    except (ValueError, TypeError):
        return text(value)
    return json.dumps(sanitize(parsed), ensure_ascii=True, separators=(",", ":")) + "\n"


if __name__ == "__main__":
    sys.stdout.write(redact(sys.stdin.read()))
