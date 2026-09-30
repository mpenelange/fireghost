These fixtures execute the JavaScript embedded in `pipeline_scripts.rs`, including
the vendored Mozilla Readability implementation, inside jsdom. The runner reads
the actual Rust raw-string script constants; it does not reimplement selection,
identity, sanitization, or budgeting logic. Rust request-builder/protocol tests
remain in the component's ordinary test suite.

Run only in the isolated Docker testing VM:

```sh
npm ci --ignore-scripts
npm test
```

The fixtures cover supported DOM shapes and failure semantics. They do not prove
that all current Reddit variants match these shapes, that a visible snapshot is
complete, or that public Reddit will consistently serve the same HTML. An isolated
live public-thread check is required before any deployment decision. Fixtures are
authored examples and contain no captured page HTML, cookies, or private content.
