# Fireghost continuous integration

The monorepo keeps component checks independent and adds one whole-appliance contract gate:

- `router.yaml` runs only when the Go router or its root command changes.
- `crw.yaml` runs only when the Rust component or its root command changes.
- `appliance.yaml` runs for every push and pull request. It validates packaging and Compose, builds the router, and exercises it against a deterministic local upstream without contacting Firecrawl Cloud.
- `release.yaml` accepts only `fireghost-vMAJOR.MINOR.PATCH` tags. Historical `appliance-v*` tags and releases remain intact at their original commits, but cannot publish into the Fireghost package namespace. The workflow serializes releases, runs the full gate, builds run-unique router and CRW candidates, validates them together in an isolated local-only appliance, and only then creates immutable full-version image tags. Minor and `latest` aliases advance only when the new version is not older than the versions recorded on the existing registry aliases; failed Git tags therefore do not block a later successful promotion. Because the registry has no cross-package transaction, paired alias updates are retried and prior aliases are restored when possible; exact matching version tags remain the reproducible deployment authority.
- `validation.yaml` runs only on manual dispatch. It builds a CRW validation image, then on a separate runner starts a control stack and a candidate stack that differ only in one supplied browser image digest, runs the browser regression matrix cold and warm, uploads the evidence as an artifact, and removes both stacks. It never changes an image pin or a deployment. Dispatch it from the Actions UI or with `gh workflow run validation.yaml --ref <branch> -f camofox_image=<digest>`.

Releases publish `ghcr.io/mpenelange/fireghost-router` and
`ghcr.io/mpenelange/fireghost-crw`. Images published before the GitHub move
live at `git.firewire.cc/michael/fireghost-*`, and the old
`hermes-web-retrieval-*` packages are historical; release workflows do not
overwrite or delete them.

The root `make check` command is the local equivalent of the component and packaging gates. Its Rust invocation disables incremental artifacts and debugger symbols to keep combined checks within bounded disk usage; component developers can still use `crw/Makefile` directly when debugger artifacts are useful.

## GitHub status

CI runs on GitHub Actions from `.github/workflows/` using GitHub-hosted
`ubuntu-latest` runners. Workflows log in to the registry with the automatic
`GITHUB_TOKEN` (`packages: write`); repository secrets `REGISTRY_USERNAME` and
`REGISTRY_TOKEN` override it when publishing elsewhere, and repository
variables `REGISTRY` and `IMAGE_NAMESPACE` override the `ghcr.io` defaults.
Confirm an ordinary push completes before pushing an appliance release tag.

## Portability

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

The move from Forgejo Actions on `git.firewire.cc` replaced the workflow
directory, URL-form action references (`uses: https://…`), the Forgejo
artifact-upload fork, the self-hosted `validation` runner label, and the
hard-coded `git.firewire.cc` registry. The release-page request is unchanged:
GitHub and Forgejo accept the same `POST …/releases` body.
