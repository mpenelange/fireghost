# Fireghost continuous integration

The monorepo keeps component checks independent and adds one whole-appliance contract gate:

- `router.yaml` runs only when the Go router or its root command changes.
- `crw.yaml` runs only when the Rust component or its root command changes. After the default-feature gate it installs `cmake clang libclang-dev` and runs clippy and tests for the shipped `cdp,camofox,impersonated` renderer features.
- `appliance.yaml` runs for every push and pull request. It validates packaging and Compose, builds the router, and exercises it against a deterministic local upstream without contacting Firecrawl Cloud.
- `release.yaml` accepts only `fireghost-vMAJOR.MINOR.PATCH` tags. Historical `appliance-v*` tags and releases remain intact at their original commits, but cannot publish into the Fireghost package namespace. The workflow serializes releases, runs the full gate, builds run-unique router and CRW candidates, validates them together in an isolated local-only appliance, and only then creates immutable full-version image tags. Minor and `latest` aliases advance only when the new version is not older than the versions recorded on the existing registry aliases; failed Git tags therefore do not block a later successful promotion. Because the registry has no cross-package transaction, paired alias updates are retried and prior aliases are restored when possible; exact matching version tags remain the reproducible deployment authority.
- `validation.yaml` runs only on manual dispatch. It builds a CRW validation image on a build runner, then on the `validation` runner starts a control stack and a candidate stack that differ only in one supplied browser image digest, runs the browser regression matrix cold and warm, uploads the evidence as an artifact, and removes both stacks. It never changes an image pin or a deployment. Dispatch it from the Actions UI or with `tea actions workflows dispatch validation.yaml --ref <branch> -i camofox_image=<digest>`.

Forgejo releases publish `git.firewire.cc/michael/fireghost-router` and
`git.firewire.cc/michael/fireghost-crw`. The old `hermes-web-retrieval-*`
packages are historical; release workflows do not overwrite or delete them.

The root `make check` command is the local equivalent of the component and packaging gates. Its Rust invocation disables incremental artifacts and debugger symbols to keep combined checks within bounded disk usage; component developers can still use `crw/Makefile` directly when debugger artifacts are useful.

## Firewire status

Forgejo Actions is enabled on `git.firewire.cc`. Jobs run on ephemeral VMs on docker0, managed by the private `michael/ci-runners` repository: every job gets a fresh Ubuntu VM (4 vCPUs, 16 GB RAM, a new 14 GB job disk for its workspace and Docker data) that is deleted when the job ends. Two slots carry the `ubuntu-latest` label; one carries only the `validation` label, so browser and appliance validation never queues behind Rust builds and build jobs never land on it. `actions/cache` uses a shared 100 GB cache server, so Cargo inputs and `crw/target` survive between VMs. `release.yaml` additionally requires user-level Actions secrets `REGISTRY_USERNAME` and `REGISTRY_TOKEN`, with package-write access. Confirm an ordinary push completes before pushing an appliance release tag. Forgejo Actions is the sole build, test, and image-publication system for this repository; no GitHub build or release workflows are defined.

## Portability to GitHub Actions

Workflows use GitHub Actions syntax so the project can move to GitHub-hosted or
other self-hosted runners without redesigning CI. Keep new workflow code portable:
use `owner/repo@ref` action references, `$GITHUB_*` variables, `actions/cache` for
caches, and repository variables instead of hard-coded hosts. Runner VMs match
GitHub's `ubuntu-latest` (Ubuntu LTS, 4 vCPUs, 16 GB RAM, 14 GB job disk); a
runner that differs must carry a different label.

The CRW image build caches compiled dependencies portably. `crw/Dockerfile`
uses cargo-chef so the dependency graph compiles in its own layer, and the
`buildx` calls in `release.yaml` and `validation.yaml` export layers to a
registry tag (`fireghost-crw:buildcache`) with `type=registry,…,image-manifest=true`.
That works with any OCI registry (Forgejo, ghcr.io) and needs no runner-local
state, unlike BuildKit cache mounts or the GitHub-only `type=gha` backend.
Per-build identity (`CRW_REVISION`, build date) is declared after the dependency
layer so it does not invalidate it. Dependencies recompile only when the recipe
(manifests and `Cargo.lock`) changes; a missing or failed cache only slows the
build (`ignore-error=true`).

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
| `release.yaml` release page | `POST $GITHUB_API_URL/repos/$GITHUB_REPOSITORY/releases` with the automatic `GITHUB_TOKEN` (Forgejo API base `…/api/v1`) | unchanged; GitHub accepts the same request and needs the job's `contents: write` |
