# Continuous integration

The monorepo keeps component checks independent and adds one whole-appliance contract gate:

- `router.yaml` runs only when the Go router or its root command changes.
- `crw.yaml` runs only when the Rust component or its root command changes.
- `appliance.yaml` runs for every push and pull request. It validates packaging and Compose, builds the router, and exercises it against a deterministic local upstream without contacting Firecrawl Cloud.
- `release.yaml` accepts only `appliance-vMAJOR.MINOR.PATCH` tags. It serializes releases, runs the full gate, builds run-unique router and CRW candidates, validates them together in an isolated local-only appliance, and only then creates immutable full-version tags. Minor and `latest` aliases move only forward. Because the registry has no cross-package transaction, paired alias updates are retried and prior aliases are restored when possible; exact matching version tags remain the reproducible deployment authority.

The root `make check` command is the local equivalent of the component and packaging gates. Its Rust invocation disables incremental artifacts and debugger symbols to keep combined checks within bounded disk usage; component developers can still use `crw/Makefile` directly when debugger artifacts are useful.

## Firewire status

Forgejo Actions is enabled on `git.firewire.cc`; pushes and pull requests run on dedicated Ubuntu host-runner VMs using the `ubuntu-latest` label. Each runner has 4 vCPUs, 16 GB RAM, and a separate 14 GB filesystem shared by its workspace, caches, Rust toolchains, and Docker/containerd data. `release.yaml` additionally requires repository secrets `REGISTRY_USERNAME` and `REGISTRY_TOKEN`, with package-write access. Confirm an ordinary push completes on a runner and configure those secrets before pushing the first appliance release tag.

The GitHub workflows provide the same three CI gates on GitHub-hosted `ubuntu-latest` runners. GitHub release tags use a separate five-job workflow that verifies the source, builds both candidate images in parallel, validates them on a clean runner, and then promotes them in GHCR. It authenticates with the repository's built-in `GITHUB_TOKEN`; the workflow requires `packages: write` permission but no manually configured registry secret. Normal CI is restricted to branch pushes and pull requests so a release tag does not duplicate the full checks outside the release graph.
