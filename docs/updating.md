# Updating

Run `./scripts/check-updates.sh` to verify current immutable references remain retrievable. It does not mutate configuration or select versions.

For each update, read upstream release notes and licensing/security notices, record the new digest, update one component at a time, run the complete validation suite, then exercise a staging appliance. Never replace a digest with `latest` or an unpinned tag. Back up volumes before deployment and retain the previous images and configuration for rollback.

`CRW_IMAGE` is deliberately supplied through `.env` because the Firewire registry may require authentication. `.env.example` uses the immutable upstream `1.2.0` digest as a reproducible baseline. After publishing `git.firewire.cc/michael/crw-camofox:1.2.0-fw.1`, record the digest returned by the registry and replace `CRW_IMAGE` with `git.firewire.cc/michael/crw-camofox@sha256:...`; never deploy the mutable tag itself.

