# Fireghost continuous integration

The monorepo keeps component checks independent and adds one whole-appliance contract gate:

- `router.yaml` runs only when the Go router or its root command changes.
- `crw.yaml` runs only when the Rust component or its root command changes.
- `appliance.yaml` runs for every push and pull request. It validates packaging and Compose, builds the router, and exercises it against a deterministic local upstream without contacting Firecrawl Cloud.
- `release.yaml` accepts only `fireghost-vMAJOR.MINOR.PATCH` tags. Historical `appliance-v*` tags and releases remain intact at their original commits, but cannot publish into the Fireghost package namespace. The workflow serializes releases, runs the full gate, builds run-unique router and CRW candidates, validates them together in an isolated local-only appliance, and only then creates immutable full-version image tags. Minor and `latest` aliases advance only when the new version is not older than the versions recorded on the existing registry aliases; failed Git tags therefore do not block a later successful promotion. Because the registry has no cross-package transaction, paired alias updates are retried and prior aliases are restored when possible; exact matching version tags remain the reproducible deployment authority.
- `validation.yaml` runs only on manual dispatch. It builds a CRW validation image on a build runner, then on the `validation` runner starts a control stack and a candidate stack that differ only in one supplied browser image digest, runs the browser regression matrix cold and warm, uploads the evidence as an artifact, and removes both stacks. It never changes an image pin or a deployment. Dispatch it from the Actions UI or with `tea actions workflows dispatch validation.yaml --ref <branch> -i camofox_image=<digest>`.

Forgejo releases publish `git.firewire.cc/michael/fireghost-router` and
`git.firewire.cc/michael/fireghost-crw`. The old `hermes-web-retrieval-*`
packages are historical; release workflows do not overwrite or delete them.

The root `make check` command is the local equivalent of the component and packaging gates. Its Rust invocation disables incremental artifacts and debugger symbols to keep combined checks within bounded disk usage; component developers can still use `crw/Makefile` directly when debugger artifacts are useful.

## Firewire status

Forgejo Actions is enabled on `git.firewire.cc`; pushes and pull requests run on dedicated Ubuntu host-runner VMs using the `ubuntu-latest` label. Each runner has 4 vCPUs, 16 GB RAM, and a separate 14 GB filesystem shared by its workspace, caches, Rust toolchains, and Docker/containerd data. `release.yaml` additionally requires user-level Actions secrets `REGISTRY_USERNAME` and `REGISTRY_TOKEN`, with package-write access. A third host-runner VM with the same resources carries only the `validation` label, so browser and appliance validation never queues behind Rust builds and build jobs never land on it. Confirm an ordinary push completes before pushing an appliance release tag. Forgejo Actions is the sole build, test, and image-publication system for this repository; no GitHub build or release workflows are defined.
