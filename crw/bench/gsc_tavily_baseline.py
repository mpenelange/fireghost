"""GSC baseline pull for the Tavily SEO cluster.

Fires three searchanalytics.query calls:
  1. Top queries containing tavily-related tokens (last 28 days)
  2. Per-page metrics for the 4 priority Tavily cluster URLs (last 28 days)
  3. Top queries site-wide (last 28 days) — context for relative volume

Output: Markdown report to seo-baselines/gsc-tavily-2026-05-11.md
"""

from __future__ import annotations

import datetime as dt
import json
import sys
from pathlib import Path

from google.auth import default
from google.auth.transport.requests import AuthorizedSession

SITE = "sc-domain:fastcrw.com"
DAYS = 28
PRIORITY_PAGES = [
    "https://fastcrw.com/alternatives/tavily",
    "https://fastcrw.com/alternatives/open-source-tavily",
    "https://fastcrw.com/alternatives/tavily-vs-serper",
    "https://fastcrw.com/alternatives/self-hosted-search-api",
]
TAVILY_TOKENS = ["tavily", "serper", "searxng", "self-hosted search", "open source search"]
ENDPOINT = (
    f"https://searchconsole.googleapis.com/webmasters/v3/sites/"
    f"{SITE.replace(':', '%3A').replace('/', '%2F')}/searchAnalytics/query"
)


def query(sess: AuthorizedSession, body: dict) -> dict:
    r = sess.post(ENDPOINT, json=body, timeout=30)
    if r.status_code != 200:
        sys.exit(f"GSC error {r.status_code}: {r.text}")
    return r.json()


def fmt_pct(x: float) -> str:
    return f"{x * 100:.1f}%"


def main() -> None:
    creds, _ = default(scopes=["https://www.googleapis.com/auth/webmasters.readonly"])
    sess = AuthorizedSession(creds)

    end = dt.date.today()
    start = end - dt.timedelta(days=DAYS)
    iso = lambda d: d.isoformat()

    # 1. Top 1000 queries site-wide, then filter client-side for Tavily tokens
    all_q = query(sess, {
        "startDate": iso(start),
        "endDate": iso(end),
        "dimensions": ["query"],
        "rowLimit": 1000,
    })
    all_rows = all_q.get("rows", [])
    tavily_rows = [
        r for r in all_rows
        if any(tok in r["keys"][0].lower() for tok in TAVILY_TOKENS)
    ]
    tavily_q = {"rows": tavily_rows}

    # 2. Priority page metrics — one call per page, GSC AND-only filter logic
    page_rows_collected = []
    for p in PRIORITY_PAGES:
        r = query(sess, {
            "startDate": iso(start),
            "endDate": iso(end),
            "dimensions": ["page"],
            "dimensionFilterGroups": [{
                "filters": [{"dimension": "page", "operator": "equals", "expression": p}],
            }],
            "rowLimit": 1,
        })
        page_rows_collected.extend(r.get("rows", []))
    page_rows = {"rows": page_rows_collected}

    # 3. Per-(page,query) for priority pages — one call per page
    pq_collected = []
    for p in PRIORITY_PAGES:
        r = query(sess, {
            "startDate": iso(start),
            "endDate": iso(end),
            "dimensions": ["page", "query"],
            "dimensionFilterGroups": [{
                "filters": [{"dimension": "page", "operator": "equals", "expression": p}],
            }],
            "rowLimit": 50,
        })
        pq_collected.extend(r.get("rows", []))
    page_query_rows = {"rows": pq_collected}

    # 4. Site-wide top 30 — context
    site_top = query(sess, {
        "startDate": iso(start),
        "endDate": iso(end),
        "dimensions": ["query"],
        "rowLimit": 30,
    })

    out_dir = Path(__file__).parent.parent.parent / "crw-saas" / "seo-baselines"
    out_dir.mkdir(exist_ok=True)
    md_path = out_dir / f"gsc-tavily-{end.isoformat()}.md"
    json_path = out_dir / f"gsc-tavily-{end.isoformat()}.json"

    json_path.write_text(json.dumps({
        "fetched": end.isoformat(),
        "window_days": DAYS,
        "site": SITE,
        "tavily_queries": tavily_q,
        "priority_pages": page_rows,
        "priority_page_queries": page_query_rows,
        "site_top_queries": site_top,
    }, indent=2))

    lines = [
        f"# GSC Tavily Cluster Baseline — {end.isoformat()}",
        "",
        f"**Window:** {start.isoformat()} → {end.isoformat()} ({DAYS} days)",
        f"**Property:** `{SITE}`",
        "",
        "## 1. Queries containing Tavily-cluster tokens",
        "",
        "Filter: query contains any of " + ", ".join(f"`{t}`" for t in TAVILY_TOKENS),
        "",
    ]

    rows = tavily_q.get("rows", [])
    if not rows:
        lines.append("_No impressions yet for any Tavily-token query in this window._")
        lines.append("")
        lines.append("> **Read:** the new pages haven't been served impressions on these queries yet — expected for a 2026-05-09 publish; Google indexing + serving lag is typically 3–14 days.")
    else:
        lines.append("| Query | Impressions | Clicks | CTR | Avg position |")
        lines.append("|---|---:|---:|---:|---:|")
        for row in rows[:50]:
            q = row["keys"][0]
            lines.append(f"| {q} | {row['impressions']:.0f} | {row['clicks']:.0f} | {fmt_pct(row['ctr'])} | {row['position']:.1f} |")
    lines.append("")

    lines.append("## 2. Priority Tavily-cluster pages — page-level metrics")
    lines.append("")
    page_rows_data = page_rows.get("rows", [])
    if not page_rows_data:
        lines.append("_No impressions yet for any of the 4 priority pages in this window._")
        lines.append("")
        lines.append("Pages tracked:")
        for p in PRIORITY_PAGES:
            lines.append(f"- `{p}`")
    else:
        lines.append("| Page | Impressions | Clicks | CTR | Avg position |")
        lines.append("|---|---:|---:|---:|---:|")
        for row in page_rows_data:
            p = row["keys"][0]
            lines.append(f"| {p} | {row['impressions']:.0f} | {row['clicks']:.0f} | {fmt_pct(row['ctr'])} | {row['position']:.1f} |")
    lines.append("")

    lines.append("## 3. Per-page query mix for priority pages")
    lines.append("")
    pq_data = page_query_rows.get("rows", [])
    if not pq_data:
        lines.append("_No impressions yet at the page+query level._")
    else:
        lines.append("| Page | Query | Impr | Clicks | CTR | Pos |")
        lines.append("|---|---|---:|---:|---:|---:|")
        for row in pq_data[:60]:
            p, q = row["keys"]
            lines.append(f"| {p.replace('https://fastcrw.com', '')} | {q} | {row['impressions']:.0f} | {row['clicks']:.0f} | {fmt_pct(row['ctr'])} | {row['position']:.1f} |")
    lines.append("")

    lines.append("## 4. Site-wide top 30 queries (context)")
    lines.append("")
    site_data = site_top.get("rows", [])
    if not site_data:
        lines.append("_No site-wide impressions in this window._")
    else:
        lines.append("| Query | Impressions | Clicks | CTR | Avg position |")
        lines.append("|---|---:|---:|---:|---:|")
        for row in site_data:
            q = row["keys"][0]
            lines.append(f"| {q} | {row['impressions']:.0f} | {row['clicks']:.0f} | {fmt_pct(row['ctr'])} | {row['position']:.1f} |")
    lines.append("")

    md_path.write_text("\n".join(lines))
    print(f"wrote {md_path}")
    print(f"wrote {json_path}")
    print(f"tavily queries rows: {len(rows)}")
    print(f"priority pages rows: {len(page_rows_data)}")
    print(f"page+query rows: {len(pq_data)}")
    print(f"site-wide rows: {len(site_data)}")


if __name__ == "__main__":
    main()
