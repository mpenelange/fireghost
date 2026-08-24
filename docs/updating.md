# Updating

Run `./scripts/check-updates.sh` to verify current immutable references remain retrievable. It does not mutate configuration or select versions.

For each update, read upstream release notes and licensing/security notices, record the new digest, update one component at a time, run the complete validation suite, then exercise a staging appliance. Never replace a digest with `latest` or an unpinned tag. Back up volumes before deployment and retain the previous images and configuration for rollback.

`CRW_IMAGE` is deliberately supplied through `deploy/.env` because the Firewire registry may require authentication. The `crw/` component produces the image; `deploy/` consumes and validates it as part of the appliance. `deploy/.env.example` records the latest appliance-tested image by registry digest. When publishing a new build, record the digest returned by the registry and replace `CRW_IMAGE` with the new `git.firewire.cc/michael/crw-camofox@sha256:...` value; never deploy the mutable image tag itself. See the [component and release boundary](architecture.md#component-and-release-boundary) for the complete handoff.

Update `deploy/stack.lock.json` in the same candidate commit and run `make check-stack-lock`. The validator rejects drift between the manifest, `CRW_IMAGE`, and the digest-pinned Camofox and Lightpanda Compose inputs. Historical locks belong under `docs/baselines/` and are never reused as current configuration.

`make build-router` and `make build-crw-image` label local candidates with the current monorepo revision, build date, version, and canonical source URL. Override `ROUTER_IMAGE`, `ROUTER_VERSION`, `CRW_BUILD_IMAGE`, or `CRW_CANDIDATE_VERSION` when preparing a registry release. Verify CRW identity with `docker run --rm --entrypoint crw-server "$CRW_BUILD_IMAGE" version` before testing or publishing it.
