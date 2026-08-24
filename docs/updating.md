# Updating

Run `./scripts/check-updates.sh` to verify current immutable references remain retrievable. It does not mutate configuration or select versions.

For each update, read upstream release notes and licensing/security notices, record the new digest, update one component at a time, run the complete validation suite, then exercise a staging appliance. Never replace a digest with `latest` or an unpinned tag. Back up volumes before deployment and retain the previous images and configuration for rollback.

`CRW_IMAGE` is deliberately supplied through `.env` because the Firewire registry may require authentication. The `michael/crw-camofox` source repository produces the image; this repository consumes and validates it as part of the appliance. `.env.example` records the latest appliance-tested image by registry digest. When publishing a new fork build, record the digest returned by the registry and replace `CRW_IMAGE` with the new `git.firewire.cc/michael/crw-camofox@sha256:...` value; never deploy the mutable image tag itself. See the [repository and release boundary](architecture.md#repository-and-release-boundary) for the complete handoff.
