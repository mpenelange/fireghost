"""Capture golden fixtures from the REAL Firecrawl v2 API.

    FIRECRAWL_API_KEY=fc-... uv run python -m conformance.capture

Writes one normalized `{status, body}` JSON per case under fixtures/firecrawl_v2/.
The key is read from the env and never written to disk (see .gitignore).
"""

from __future__ import annotations

import json
import os
import pathlib

from . import corpus
from ._http import run_case

FIRECRAWL_BASE = os.environ.get("FIRECRAWL_BASE", "https://api.firecrawl.dev")
KEY = os.environ.get("FIRECRAWL_API_KEY")
FIXDIR = pathlib.Path(__file__).resolve().parent.parent / "fixtures" / "firecrawl_v2"


def main() -> None:
    if not KEY:
        raise SystemExit("set FIRECRAWL_API_KEY (it is .gitignore'd, never committed)")
    FIXDIR.mkdir(parents=True, exist_ok=True)
    for case in corpus.ALL_CASES:
        status, body = run_case(FIRECRAWL_BASE, KEY, case)
        out = FIXDIR / f"{case.name}.json"
        out.write_text(json.dumps({"status": status, "body": body}, indent=2))
        print(f"captured {case.name}: HTTP {status} -> fixtures/firecrawl_v2/{out.name}")


if __name__ == "__main__":
    main()
