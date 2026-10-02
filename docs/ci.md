# Fireghost continuous integration

The monorepo keeps component checks independent and adds one whole-appliance contract gate:

- `router.yaml` runs only when the Go router or its root command changes.
- `crw.yaml` runs only when the Rust component or its root command changes.
- `appliance.yaml` runs for every push and pull request. It validates packaging and Compose, builds the router, and exercises it against a deterministic local upstream without contacting Firecrawl Cloud.
- `release.yaml` accepts only `fireghost-vMAJOR.MINOR.PATCH` tags. Historical `appliance-v*` tags and releases remain intact at their original commits, but cannot publish into the Fireghost package namespace. The workflow serializes releases, runs the full gate, builds run-unique router and CRW candidates, validates them together in an isolated local-only appliance, and only then creates immutable full-version image tags. Minor and `latest` aliases advance only when the new version is not older than the versions recorded on the existing registry aliases; failed Git tags therefore do not block a later successful promotion. Because the registry has no cross-package transaction, paired alias updates are retried and prior aliases are restored when possible; exact matching version tags remain the reproducible deployment authority.

Forgejo releases publish `git.firewire.cc/michael/fireghost-router` and
`git.firewire.cc/michael/fireghost-crw`. The old `hermes-web-retrieval-*`
packages are historical; release workflows do not overwrite or delete them.

The root `make check` command is the local equivalent of the component and packaging gates. Its Rust invocation disables incremental artifacts and debugger symbols to keep combined checks within bounded disk usage; component developers can still use `crw/Makefile` directly when debugger artifacts are useful.

## Firewire status

Forgejo Actions is enabled on `git.firewire.cc`; pushes and pull requests run on dedicated Ubuntu host-runner VMs using the `ubuntu-latest` label. Each runner has 4 vCPUs, 16 GB RAM, and a separate 14 GB filesystem shared by its workspace, caches, Rust toolchains, and Docker/containerd data. `release.yaml` additionally requires user-level Actions secrets `REGISTRY_USERNAME` and `REGISTRY_TOKEN`, with package-write access. Confirm an ordinary push completes before pushing an appliance release tag. Forgejo Actions is the sole build, test, and image-publication system for this repository; no GitHub build or release workflows are defined.

## Portability to GitHub Actions

Workflows use GitHub Actions syntax so the project can move to GitHub-hosted or
other self-hosted runners without redesigning CI. Keep new workflow code portable:
use `owner/repo@ref` action references, `$GITHUB_*` variables, `actions/cache` for
caches, and repository variables instead of hard-coded hosts. Runner VMs match
GitHub's `ubuntu-latest` (Ubuntu LTS, 4 vCPUs, 16 GB RAM, 14 GB job disk); a
runner that differs must carry a different label.

Forgejo-specific touchpoints, each a small edit when moving:

| Location | Forgejo today | On GitHub |
|---|---|---|
| Workflow directory | `.forgejo/workflows/` | `git mv` to `.github/workflows/` (Forgejo also reads it) |
| `crw.yaml`, `release.yaml` | `uses: https://github.com/dtolnay/rust-toolchain@stable` | `uses: dtolnay/rust-toolchain@stable`; set Forgejo `[actions] DEFAULT_ACTIONS_URL = https://github.com` first so both work |
| `validation.yaml` artifact upload | `https://code.forgejo.org/forgejo/upload-artifact@v4` (upstream v4 rejects non-GitHub servers) | `actions/upload-artifact@v4` |
| `validation.yaml` registry | variables `REGISTRY`, `IMAGE_NAMESPACE` (default `git.firewire.cc`, `git.firewire.cc/michael`) | set to `ghcr.io`, `ghcr.io/<owner>` |
| `validation.yaml` runner | `runs-on: validation` | `ubuntu-latest` or a self-hosted label |
| `release.yaml` | hard-coded `git.firewire.cc/michael/*` images and source URL | parameterize like `validation.yaml`; only runs on release tags, so change it with a release |
| Secrets | `REGISTRY_USERNAME`, `REGISTRY_TOKEN` | same names; `GITHUB_TOKEN` can push to ghcr.io |
