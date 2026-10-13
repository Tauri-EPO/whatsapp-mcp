"""Exercise the exact receiver verification snippet shipped in the docs."""

import hashlib
import hmac
import json
import re
import time
from pathlib import Path


def test_runtime_webhook_documented_verifier():
    document = (Path(__file__).resolve().parents[2] / "docs" / "CONFIGURATION.md").read_text(encoding="utf-8")
    snippet = next(
        code for code in re.findall(r"```python\n(.*?)```", document, re.DOTALL) if "def verify_webhook(" in code
    )
    namespace = {}
    exec(compile(snippet, "documented_webhook_verifier", "exec"), namespace)
    verify = namespace["verify_webhook"]
    secret = "fake-receiver-verification-secret"
    body = json.dumps({"event_id": "fake-event"}).encode()

    def signed_headers(timestamp):
        signature = hmac.new(secret.encode(), timestamp.encode() + b"." + body, hashlib.sha256).hexdigest()
        return {
            "X-Webhook-Id": "fake-event",
            "X-Webhook-Timestamp": timestamp,
            "X-Webhook-Signature": "sha256=" + signature,
        }

    headers = signed_headers(str(int(time.time())))
    assert verify(headers, body, secret)
    assert not verify(headers, body + b" ", secret)
    assert not verify(headers, body, "different-fake-secret")
    assert not verify(signed_headers(str(int(time.time()) - 301)), body, secret)
    assert not verify(signed_headers(str(int(time.time()) + 301)), body, secret)
    assert not verify({}, body, secret)
    headers["X-Webhook-Id"] = "other-fake-event"
    assert not verify(headers, body, secret)
