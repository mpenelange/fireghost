#!/usr/bin/env python3
"""Compare frozen and candidate routers through the installed Hermes provider."""

import argparse
import json
import os
import pathlib
import re
import subprocess
import tempfile
import urllib.request
import urllib.parse
from datetime import datetime, timezone


ROOT = pathlib.Path(__file__).resolve().parents[1]
DEFAULT_HERMES_PYTHON = "/usr/local/lib/hermes-agent/venv/bin/python"
PROBE = ROOT / "scripts" / "hermes-provider-probe.py"
SEARCH_QUERY = "Python programming language official documentation"
SEARCH_LIMIT = 5
EXTRACT_URL = "https://example.com/"
PRODUCTION_URL = "http://127.0.0.1:33000"
CANDIDATE_URL = "http://127.0.0.1:33010"
PROBE_TIMEOUT_SECONDS = 180
THRESHOLDS = {
    "search_min_results": 1,
    "search_max_results": 5,
    "search_required_fields": ["title", "url", "description"],
    "extract_title": "Example Domain",
    "extract_min_content_chars": 100,
    "candidate_total_timeout_seconds": 120.0,
    "search_min_title_overlap_ratio": 0.5,
    "extract_min_production_size_ratio": 0.9,
}
METRIC_NAMES = {
    "cache_hits": "web_retrieval_cache_hits_total",
    "local_attempts": "web_retrieval_local_attempts_total",
    "cloud_attempts": "web_retrieval_cloud_attempts_total",
}
class MetricsError(ValueError):
    """A required, non-secret Prometheus sample was absent."""


def _run_command(command, env, timeout):
    completed = subprocess.run(
        command, env=env, timeout=timeout, check=True, capture_output=True, text=True
    )
    return completed.stdout


def _fetch_metrics(endpoint):
    with urllib.request.urlopen(endpoint.rstrip("/") + "/metrics", timeout=10) as response:
        return response.read().decode("utf-8")


def _parse_metrics(text):
    samples = {}
    sample_pattern = re.compile(r"^([^#][^ ]*)\s+([0-9.eE+-]+)$")
    for line in text.splitlines():
        match = sample_pattern.match(line.strip())
        if match:
            samples[match.group(1)] = float(match.group(2))

    parsed = {"requests": {}, **{key: {} for key in METRIC_NAMES}}
    missing = []
    for endpoint in ("search", "scrape"):
        request_key = (
            'web_retrieval_requests_total{endpoint="%s",status_class="2xx"}' % endpoint
        )
        if request_key not in samples:
            missing.append(request_key)
        else:
            parsed["requests"][endpoint] = samples[request_key]
        for family, metric_name in METRIC_NAMES.items():
            key = f'{metric_name}{{endpoint="{endpoint}"}}'
            if key not in samples:
                missing.append(key)
            else:
                parsed[family][endpoint] = samples[key]
    if missing:
        raise MetricsError("missing required metric family or label")
    return parsed


def _safe_text(value, limit=256):
    text = str(value or "")[:limit]
    text = re.sub(
        r"(?i)\b(api[_-]?key|token|secret|password|passwd)\s*[:=]\s*[^\s]+",
        r"\1=[REDACTED]",
        text,
    )
    return re.sub(r"\bsk-[A-Za-z0-9._-]{8,}\b", "[REDACTED]", text)


def _safe_url(value):
    try:
        parsed = urllib.parse.urlsplit(str(value or ""))
        if parsed.scheme not in ("http", "https") or not parsed.hostname:
            return ""
        host = parsed.hostname
        if parsed.port:
            host += f":{parsed.port}"
        return urllib.parse.urlunsplit((parsed.scheme, host, "", "", ""))
    except (TypeError, ValueError):
        return ""


def _sanitize_probe(probe):
    results = probe.get("search", {}).get("results", [])
    items = probe.get("extract", {}).get("items", [])
    identity = probe.get("identity", {})
    return {
        "available": probe.get("available") is True,
        "identity": {key: _safe_text(identity.get(key, ""), 160)
                     for key in ("python", "provider")},
        "search": {
            "success": probe.get("search", {}).get("success") is True,
            "results": [
                {"title": _safe_text(item.get("title")),
                 "url": _safe_url(item.get("url")),
                 "description": _safe_text(item.get("description"), 512)}
                for item in results
            ],
        },
        "extract": {
            "success": probe.get("extract", {}).get("success") is True,
            "items": [
                {"url": _safe_url(item.get("url")),
                 "title": _safe_text(item.get("title")),
                 "content_chars": item.get("content_chars", 0)}
                for item in items
            ],
        },
        "latency_seconds": {
            key: value for key, value in probe.get("latency_seconds", {}).items()
            if key in ("search", "extract", "total") and isinstance(value, (int, float))
        },
    }


def _semantic_reasons(identity, probe):
    reasons = []
    if not probe["available"]:
        reasons.append(f"{identity} provider is unavailable")
    results = probe["search"]["results"]
    if not probe["search"]["success"]:
        reasons.append(f"{identity} search failed")
    if not THRESHOLDS["search_min_results"] <= len(results) <= THRESHOLDS["search_max_results"]:
        reasons.append(f"{identity} search result count {len(results)} is outside bounds")
    if any(not all(result.get(field) for field in THRESHOLDS["search_required_fields"])
           for result in results):
        reasons.append(f"{identity} search result missing required fields")
    items = probe["extract"]["items"]
    if not probe["extract"]["success"] or not items:
        reasons.append(f"{identity} extraction failed")
    elif items[0].get("title") != THRESHOLDS["extract_title"]:
        reasons.append(f"{identity} extraction title does not match representative page")
    if not items or items[0].get("content_chars", 0) < THRESHOLDS["extract_min_content_chars"]:
        reasons.append(f"{identity} extraction content is too small")
    return reasons


def _comparison_reasons(production, candidate):
    reasons = []
    production_titles = {
        re.sub(r"\s+", " ", item.get("title", "").strip().casefold())
        for item in production["search"]["results"] if item.get("title")
    }
    candidate_titles = {
        re.sub(r"\s+", " ", item.get("title", "").strip().casefold())
        for item in candidate["search"]["results"] if item.get("title")
    }
    if production_titles:
        overlap = len(production_titles & candidate_titles) / len(production_titles)
        if overlap < THRESHOLDS["search_min_title_overlap_ratio"]:
            reasons.append(f"candidate search title overlap {overlap:.3f} is below production threshold")
    production_items = production["extract"]["items"]
    candidate_items = candidate["extract"]["items"]
    if production_items and candidate_items:
        production_size = production_items[0].get("content_chars", 0)
        candidate_size = candidate_items[0].get("content_chars", 0)
        if production_size and candidate_size < production_size * THRESHOLDS["extract_min_production_size_ratio"]:
            reasons.append("candidate extraction content is smaller than production threshold")
    return reasons


def _validate_cli_endpoints(production_url, candidate_url):
    if production_url != PRODUCTION_URL or candidate_url != CANDIDATE_URL:
        raise ValueError(
            f"migration gate endpoints are fixed at {PRODUCTION_URL} and {CANDIDATE_URL}"
        )


def _probe_env(endpoint, api_key, hermes_home, search_query):
    # Build a minimal environment rather than filtering the parent environment:
    # deny-by-default prevents unrelated credentials from reaching the probe.
    env = {
        "PATH": os.environ.get("PATH", os.defpath),
        "LANG": os.environ.get("LANG", "C.UTF-8"),
        "HOME": str(hermes_home),
        "HERMES_HOME": str(hermes_home),
        "FIRECRAWL_API_URL": endpoint,
        "HERMES_REGRESSION_SEARCH_QUERY": search_query,
    }
    if api_key:
        env["FIRECRAWL_API_KEY"] = api_key
    return env


def _initialize_probe_home(hermes_home):
    """Pin the isolated profile to direct Firecrawl without touching user state."""
    pathlib.Path(hermes_home, "config.yaml").write_text(
        "web:\n  backend: firecrawl\n  search_backend: firecrawl\n"
        "  extract_backend: firecrawl\n  use_gateway: false\n",
        encoding="utf-8",
    )


def _delta(after, before):
    return {
        family: {endpoint: after[family][endpoint] - before[family][endpoint]
                 for endpoint in ("search", "scrape")}
        for family in ("requests", "cache_hits", "local_attempts", "cloud_attempts")
    }


def _failure_reason(stage, exc):
    if isinstance(exc, subprocess.TimeoutExpired):
        return f"{stage} probe timed out"
    if isinstance(exc, subprocess.CalledProcessError):
        return f"{stage} probe subprocess failed"
    if isinstance(exc, json.JSONDecodeError):
        return f"{stage} probe returned invalid JSON"
    if isinstance(exc, MetricsError):
        return f"{stage} failed: missing required metric family or label"
    if isinstance(exc, (OSError, ValueError)):
        return f"{stage} failed: {type(exc).__name__}"
    return f"{stage} failed"


def _repo_head():
    try:
        return subprocess.check_output(
            ["git", "rev-parse", "HEAD"], cwd=ROOT, text=True, stderr=subprocess.DEVNULL
        ).strip()
    except (OSError, subprocess.SubprocessError):
        return "unknown"


def run_gate(*, production_url, candidate_url, api_key, output, command_runner=_run_command,
             metrics_fetcher=None, hermes_python=DEFAULT_HERMES_PYTHON, repo_revision=None,
             search_query=SEARCH_QUERY):
    metrics_fetcher = metrics_fetcher or _fetch_metrics
    output = pathlib.Path(output)
    reasons, probes, metrics = [], {}, {}
    artifact = {
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "endpoints": {"production": production_url, "candidate": candidate_url},
        "probes": probes,
        "metrics": metrics,
        "thresholds": THRESHOLDS,
        "reproducibility": {
            "repo_head": repo_revision or _repo_head(),
            "hermes_python": str(hermes_python),
            "probe_inputs": {"search_query": _safe_text(search_query, 512), "search_limit": SEARCH_LIMIT,
                             "extract_url": EXTRACT_URL, "extract_format": "markdown"},
            "check_definitions": {
                "semantic": "availability, result bounds/fields, production title overlap, representative extraction parity",
                "endpoint_provenance": "each bracketed router probe must gain exactly one 2xx search and scrape request",
                "latency": "candidate total must not exceed the absolute ceiling",
                "cloud": "candidate search and scrape attempt deltas must remain zero; production is the recorded baseline",
            },
        },
    }
    try:
        for identity, endpoint in (("production", production_url), ("candidate", candidate_url)):
            before = _parse_metrics(metrics_fetcher(endpoint))
            with tempfile.TemporaryDirectory(prefix=f"hermes-regression-{identity}-") as home:
                _initialize_probe_home(home)
                raw = command_runner(
                    [str(hermes_python), str(PROBE)],
                    _probe_env(endpoint, api_key, home, search_query),
                    PROBE_TIMEOUT_SECONDS,
                )
            probes[identity] = _sanitize_probe(json.loads(raw))
            after = _parse_metrics(metrics_fetcher(endpoint))
            delta = _delta(after, before)
            # Compatibility aliases keep cloud counters especially easy to inspect.
            metrics[identity] = {"before": before, "after": after,
                                 "delta": {**delta, **delta["cloud_attempts"]}}
            for endpoint in ("search", "scrape"):
                if delta["requests"][endpoint] != 1:
                    reasons.append(
                        f"{identity} {endpoint} request delta is {delta['requests'][endpoint]:g}; expected exactly 1"
                    )
                if identity == "candidate" and delta["cloud_attempts"][endpoint] != 0:
                    reasons.append(
                        f"{identity} cloud attempts increased for {endpoint}: "
                        f"{delta['cloud_attempts'][endpoint]:g}"
                    )
            reasons.extend(_semantic_reasons(identity, probes[identity]))
        reasons.extend(_comparison_reasons(probes["production"], probes["candidate"]))
        candidate_latency = probes["candidate"]["latency_seconds"].get("total")
        if candidate_latency is None:
            reasons.append("candidate latency is missing")
        elif candidate_latency > THRESHOLDS["candidate_total_timeout_seconds"]:
            reasons.append(
                f"candidate latency {candidate_latency:.3f}s exceeds absolute "
                f"{THRESHOLDS['candidate_total_timeout_seconds']:.3f}s bound"
            )
    except Exception as exc:  # Every operational failure must leave safe evidence.
        reasons.append(_failure_reason("regression gate", exc))
    artifact["passed"] = not reasons
    artifact["reasons"] = reasons
    output.parent.mkdir(parents=True, exist_ok=True)
    with output.open("x", encoding="utf-8") as artifact_file:
        artifact_file.write(json.dumps(artifact, indent=2) + "\n")
    return artifact


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--production-url", default=PRODUCTION_URL)
    parser.add_argument("--candidate-url", default=CANDIDATE_URL)
    parser.add_argument("--hermes-python", default=DEFAULT_HERMES_PYTHON)
    parser.add_argument("--search-query", default=SEARCH_QUERY)
    parser.add_argument("--output", type=pathlib.Path, required=True)
    args = parser.parse_args()
    try:
        _validate_cli_endpoints(args.production_url, args.candidate_url)
    except ValueError as exc:
        parser.error(str(exc))
    artifact = run_gate(
        production_url=args.production_url, candidate_url=args.candidate_url,
        api_key=os.environ.get("FIRECRAWL_API_KEY", ""), output=args.output,
        hermes_python=args.hermes_python, search_query=args.search_query,
    )
    print(f"Hermes regression gate: {'PASS' if artifact['passed'] else 'FAIL'}; artifact={args.output}")
    for reason in artifact["reasons"]:
        print(f"- {reason}")
    raise SystemExit(0 if artifact["passed"] else 1)


if __name__ == "__main__":
    main()
