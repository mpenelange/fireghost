# Google DOM extractor fixtures

From the repository root, inside the isolated validation VM or CI:

```sh
make test-browser-dom
```

The target runs both renderer and Google fixtures in a pinned Node container with a read-only source mount. It copies only the two crate directories and installs dependencies inside the disposable container, leaving no source-tree `node_modules`. Both root `make check-crw` and `make test-crw` include it.

The runner reuses the renderer fixture package's locked jsdom 26.1.0 dependency. It reads `GOOGLE_SCRAPE_JS` directly from `src/camofox_search.rs`; there is no separate test copy of the extractor and no duplicate dependency lockfile.

`cases.json` defines exact expected rows for synthetic HTML fixtures. They cover linked headings, unrelated preceding links, nested wrappers, Google tracking links, invalid destinations, and provider widgets. Positive Google-owned documentation and Maps results guard against rejecting publisher domains or map pages. Headings containing a single child link are supported; multiple child links remain ambiguous. Snippet lookup may ascend single-heading wrappers, but must not borrow another result's text.

Each result card contributes its first heading, with duplicate headings removed across nested wrappers. Sitelink headings remain part of their primary card instead of becoming independent results; distinct nested result cards remain eligible. Snippet ascent counts those primary headings, so a card with sitelinks keeps its own snippet.

Failures produce a nonzero exit status. These fixtures test extraction only. They do not establish that a live Google page loaded successfully or distinguish a challenge page from a valid SERP; live browser and REST validation remain required.
