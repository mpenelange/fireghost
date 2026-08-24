# Monorepo engineering rules

## Migration safety

- Preserve the deployed baseline before changing behavior.
- Keep source imports and mechanical path moves separate from behavioral changes.
- Preserve the full Git history of both source repositories.
- Do not mutate the current Hermes production deployment from this repository until equivalence and regression approval.
- Never commit secrets. Runtime credentials remain in ignored `.env` files.

## Component boundaries

- CRW owns Rust retrieval and rendering behavior and publishes a versioned container image.
- The Go router depends only on CRW's HTTP contract, never its private implementation.
- Deployment configuration owns the assembled appliance and immutable image pins.
- Unit tests remain component-local; appliance contract and regression tests live at the repository root.
- Runtime images and release manifests must identify both the monorepo revision and component version.

More specific `AGENTS.md` files inside imported components continue to apply within their directories.
