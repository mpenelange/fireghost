# Continuous integration

The monorepo keeps component checks independent and adds one whole-appliance contract gate:

- `router.yaml` runs only when the Go router or its root command changes.
- `crw.yaml` runs only when the Rust component or its root command changes.
- `appliance.yaml` runs for every push and pull request. It validates packaging and Compose, builds the router, and exercises it against a deterministic local upstream without contacting Firecrawl Cloud.
- `release.yaml` accepts only `appliance-vMAJOR.MINOR.PATCH` tags. It serializes releases, runs the full gate, builds run-unique router and CRW candidates, validates them together in an isolated local-only appliance, and only then creates immutable full-version tags. Minor and `latest` aliases move only forward. Because the registry has no cross-package transaction, paired alias updates are retried and prior aliases are restored when possible; exact matching version tags remain the reproducible deployment authority.

The root `make check` command is the local equivalent of the component and packaging gates. Its Rust invocation disables incremental artifacts and debugger symbols to keep combined checks within bounded disk usage; component developers can still use `crw/Makefile` directly when debugger artifacts are useful.

## Firewire status

Forgejo Actions is not currently enabled on `git.firewire.cc`. These workflow files define and review the intended automation, but their presence must not be interpreted as an executed gate. `release.yaml` also requires repository secrets `REGISTRY_USERNAME` and `REGISTRY_TOKEN`, with package-write access. Do not push an appliance release tag until Actions and those secrets are configured. Until then, release evidence and registry publication remain manual operations backed by `make check`, isolated appliance validation, and recorded regression artifacts.
