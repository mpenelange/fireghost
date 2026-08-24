#!/usr/bin/env python3
"""Search the web via CRW API.

Hosted (default):  set CRW_API_KEY=crw_live_...
Self-hosted:       set CRW_API_URL=http://localhost:3000
"""
import os
from crw import CrwClient

api_url = os.environ.get("CRW_API_URL", "https://api.fastcrw.com")
api_key = os.environ.get("CRW_API_KEY")

client = CrwClient(api_url=api_url, api_key=api_key)
results = client.search("best web scraper 2026", limit=3)

print("Search results for 'best web scraper 2026':\n")
for r in results["data"]:
    print(f"  • {r['title']}")
    print(f"    {r['url']}\n")
