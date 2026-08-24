#!/usr/bin/env python3
"""Unit tests for the live scrape compatibility gate."""

from __future__ import annotations

import sys
import unittest
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

from scrape_compatibility import Case, Client  # noqa: E402


class FakeClient(Client):
    def __init__(self, status: int, response: Any):
        super().__init__("http://unused.invalid", None, 1)
        self.status = status
        self.response = response

    def request(self, path: str, body: dict[str, Any]) -> tuple[int, Any]:
        return self.status, self.response


class ScrapeGateTests(unittest.TestCase):
    def test_static_success_passes(self) -> None:
        client = FakeClient(
            200,
            {
                "success": True,
                "data": {
                    "markdown": "Example Domain " + "x" * 100,
                    "metadata": {"statusCode": 200, "renderedWith": "http"},
                },
            },
        )
        result = client.scrape(
            Case("static", "https://example.com", True, 200, "Example Domain", 100)
        )
        self.assertTrue(result.ok)
        self.assertEqual(result.origin_status, 200)

    def test_synthetic_200_fails_origin_404_contract(self) -> None:
        client = FakeClient(
            200,
            {
                "success": True,
                "data": {
                    "markdown": "Example Domain",
                    "metadata": {"statusCode": 200, "renderedWith": "camofox"},
                },
            },
        )
        result = client.scrape(
            Case("origin_404", "https://example.com/missing", False, 404)
        )
        self.assertFalse(result.ok)
        self.assertIn("expected success=False", result.error or "")

    def test_preserved_origin_404_passes(self) -> None:
        client = FakeClient(
            200,
            {
                "success": False,
                "error": "Target returned HTTP 404",
                "data": {
                    "markdown": "not found",
                    "metadata": {"statusCode": 404, "renderedWith": "camofox"},
                },
            },
        )
        result = client.scrape(
            Case("origin_404", "https://example.com/missing", False, 404)
        )
        self.assertTrue(result.ok)
        self.assertEqual(result.origin_status, 404)

    def test_protected_page_requires_antibot_error(self) -> None:
        client = FakeClient(200, {"success": False, "error": "generic failure"})
        result = client.scrape(
            Case("protected", "https://protected.example", False, error_contains="anti-bot")
        )
        self.assertFalse(result.ok)
        self.assertIn("did not contain", result.error or "")

    def test_redirect_requires_https_canonical(self) -> None:
        client = FakeClient(
            200,
            {
                "success": True,
                "data": {
                    "markdown": "GitHub " + "x" * 100,
                    "metadata": {"statusCode": 200, "canonicalUrl": "http://github.com"},
                },
            },
        )
        result = client.scrape(
            Case(
                "redirect",
                "http://github.com",
                True,
                200,
                "GitHub",
                100,
                canonical_https=True,
            )
        )
        self.assertFalse(result.ok)
        self.assertIn("HTTPS canonical", result.error or "")


if __name__ == "__main__":
    unittest.main()
