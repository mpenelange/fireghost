#!/usr/bin/env python3
"""One-process probe of the installed Hermes Firecrawl provider."""

import asyncio
import json
import os
import platform
import time


SEARCH_QUERY = os.environ.get(
    "HERMES_REGRESSION_SEARCH_QUERY",
    "Python programming language official documentation",
)
SEARCH_LIMIT = 5
EXTRACT_URL = "https://example.com/"


def run_probe(provider, clock=time.monotonic):
    available = provider.is_available()
    search_started = clock()
    search_response = provider.search(query=SEARCH_QUERY, limit=SEARCH_LIMIT)
    search_seconds = clock() - search_started
    web = search_response.get("data", {}).get("web", [])

    extract_started = clock()
    extracted = asyncio.run(provider.extract([EXTRACT_URL], format="markdown"))
    extract_seconds = clock() - extract_started
    item = extracted[0] if extracted else {}
    return {
        "identity": {
            "python": platform.python_version(),
            "provider": f"{provider.__class__.__module__}.{provider.__class__.__qualname__}",
        },
        "available": bool(available),
        "search": {
            "success": search_response.get("success") is True,
            "results": [
                {key: value for key, value in result.items() if key in ("title", "url", "description")}
                for result in web
            ],
        },
        "extract": {
            "success": bool(extracted) and not item.get("error"),
            "items": [{
                "url": item.get("url", ""),
                "title": item.get("title", ""),
                "content_chars": len(item.get("content", "")),
            }],
        },
        "latency_seconds": {"search": search_seconds, "extract": extract_seconds,
                            "total": search_seconds + extract_seconds},
    }


def main():
    from plugins.web.firecrawl.provider import FirecrawlWebSearchProvider

    print(json.dumps(run_probe(FirecrawlWebSearchProvider()), separators=(",", ":")))


if __name__ == "__main__":
    main()
