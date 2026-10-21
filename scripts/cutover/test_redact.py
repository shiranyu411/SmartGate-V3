import json
import unittest

from redact import redact


class RedactTest(unittest.TestCase):
    def test_nested_json_and_escaped_credentials(self):
        credential = "fixture-secret"
        value = json.dumps({"nested": [{"private_key": credential + '"suffix', "url": "https://example.test/private"}]})
        result = redact(value)
        self.assertNotIn(credential, result)
        self.assertNotIn("suffix", result)
        self.assertNotIn("example.test", result)
        self.assertEqual(json.loads(result)["nested"][0]["private_key"], "[REDACTED]")

    def test_logs_redact_full_urls_and_headers(self):
        value = "fetch https://example.test/private/path Authorization: Bearer fixture-token key='other-secret'"
        result = redact(value)
        self.assertNotIn("example.test", result)
        self.assertNotIn("fixture-token", result)

    def test_embedded_json_remains_private(self):
        value = "state=" + json.dumps({"token": "fixture-token", "private_key": "fixture-secret" + '"suffix'}) + "\n"
        result = redact(value)
        self.assertNotIn("fixture-token", result)
        self.assertNotIn("suffix", result)

    def test_operational_fields_survive(self):
        result = json.loads(redact('{"ok":true,"data":{"dirty":false,"gateway_mode":"on"}}'))
        self.assertEqual(result["data"], {"dirty": False, "gateway_mode": "on"})
