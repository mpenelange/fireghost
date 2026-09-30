These fixtures execute the JavaScript embedded in `pipeline_scripts.rs`, including
the vendored Mozilla Readability implementation, inside jsdom. The runner reads
the actual Rust raw-string script constants; it does not reimplement selection,
identity, sanitization, or budgeting logic. Rust request-builder/protocol tests
remain in the component's ordinary test suite.

From the repository root, use the disposable Docker fixture target in the
isolated testing VM or CI:

```sh
make test-browser-dom
```

The target runs both renderer and Google DOM fixtures with a pinned Node image.
It mounts CRW source read-only, copies only the two fixture-owning crate
directories, and installs the locked dependencies inside the container. Container
removal leaves no `node_modules` in the source tree. Both root `make check-crw`
and `make test-crw` include this target.

The fixtures cover supported DOM shapes and failure semantics. They do not prove
that all current Reddit variants match these shapes, that a visible snapshot is
complete, or that public Reddit will consistently serve the same HTML. An isolated
live public-thread check is required before any deployment decision. Fixtures are
authored examples and contain no captured page HTML, cookies, or private content.
