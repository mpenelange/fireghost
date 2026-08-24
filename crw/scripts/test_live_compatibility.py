#!/usr/bin/env python3
"""Unit tests for the live compatibility gate's response semantics."""

from __future__ import annotations

import sys
import unittest
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

from live_compatibility import CASES, Case, Client  # noqa: E402


class FakeClient(Client):
    def __init__(self, response: Any):
        super().__init__("http://unused.invalid", None, 1)
        self.response = response

    def request(self, method: str, path: str, body: dict[str, Any] | None = None) -> Any:
        return self.response


class SearchGateTests(unittest.TestCase):
    def test_nonempty_results_pass(self) -> None:
        client = FakeClient(
            {"success": True, "data": {"results": [{"url": "https://example.com"}]}}
        )
        result = client.search(Case("wikipedia", "Alan Turing"), "cold")
        self.assertTrue(result.ok)
        self.assertEqual(result.result_count, 1)

    def test_required_empty_results_fail(self) -> None:
        client = FakeClient({"success": True, "data": {"results": []}})
        result = client.search(Case("amazon", "USB C cable"), "warm")
        self.assertFalse(result.ok)
        self.assertIn("unexpected empty", result.error or "")

    def test_explicit_baseline_empty_results_pass(self) -> None:
        client = FakeClient(
            {
                "success": True,
                "data": {"results": [], "warnings": ["challenge detected"]},
            }
        )
        result = client.search(
            Case(
                "google",
                "Camoufox",
                require_results=False,
                require_warning_if_empty=True,
            ),
            "cold",
        )
        self.assertTrue(result.ok)
        self.assertEqual(result.result_count, 0)
        self.assertEqual(result.warning, "challenge detected")

    def test_google_empty_without_warning_fails(self) -> None:
        client = FakeClient({"success": True, "data": {"results": []}})
        result = client.search(
            Case(
                "google",
                "Camoufox",
                require_results=False,
                require_warning_if_empty=True,
            ),
            "warm",
        )
        self.assertFalse(result.ok)
        self.assertIn("challenge warning", result.error or "")

    def test_unsuccessful_envelope_fails(self) -> None:
        client = FakeClient({"success": False, "error": "navigation timeout"})
        result = client.search(Case("youtube", "Rust tutorial"), "concurrent")
        self.assertFalse(result.ok)
        self.assertIn("unsuccessful API response", result.error or "")

    def test_matrix_covers_every_supported_engine_once(self) -> None:
        expected = {
            "google",
            "bing",
            "duckduckgo",
            "wikipedia",
            "youtube",
            "reddit",
            "amazon",
            "github",
        }
        self.assertEqual({case.engine for case in CASES}, expected)
        self.assertEqual(len(CASES), len(expected))


if __name__ == "__main__":
    unittest.main()
