# Browser candidate validation

Validation date: 2026-09-19 (host clock emitted 2026-09-20 UTC). The checks below used only the isolated candidate names `fireghost-browser-upgrade-camofox` and `fireghost-browser-upgrade-lightpanda`; no baseline or production container was changed, and no credentials were supplied.

## Compose merge

Docker Compose v2.39.4 rendered the final candidate overlay with:

```sh
CRW_IMAGE=git.firewire.cc/michael/crw-camofox@sha256:3898cae0970787b095d4348578c0219c9b2ff2461bd8b419e67ef2b47d701115 \
MONOREPO_REVISION=b6bcd7d29bbe59769f07a844da86eb389a75151b \
/private/tmp/fireghost-compose -p fireghost-upgrades \
  -f dev/compose.yaml -f dev/compose.upgrades.yaml config
```

The merge passed. It produced project `fireghost-upgrades`, router image `fireghost-router:upgrade-candidate`, router build revision `b6bcd7d29bbe59769f07a844da86eb389a75151b`, version `upgrade-20260919`, loopback-only `127.0.0.1:33020:8080`, and isolated names `fireghost-upgrades_appliance`, `fireghost-upgrades_camofox-profiles`, and `fireghost-upgrades_router-data`. The inherited `127.0.0.1:33000` publication was replaced by `!override`; neither browser service published a host port. The merged router environment had an empty cloud key and zero daily, monthly, burst, and refill credit values.

The baseline CRW digest above was an interpolation input for configuration parsing only; that appliance was not launched. An actual upgrade appliance run must use the newly built CRW candidate and its source revision, as described in the implementation notes.

The merged browser images were:

| Service | Image | Runtime platform | Limits used in smoke |
| --- | --- | --- | --- |
| Camofox | `ghcr.io/redf0x1/camofox-browser@sha256:afaaf9795af8793f3e6353e9e5dd5b03713b6ffed6e80c1b0a179575322bcff0` | Linux amd64 | 3 CPU, 4 GiB, 1 GiB shm/tmpfs, all capabilities dropped |
| Lightpanda | `lightpanda/browser@sha256:8af7584500145dda4beb1a2e1d92a0e4d36c39c690b1bd8b1e2b174c09b363e0` | Linux arm64 on this ARM host | 1 CPU, 512 MiB, read-only root, inherited tmpfs, all capabilities dropped |

## Registry checks

Anonymous public registry requests returned HTTP 200 and the exact requested immutable digest for both candidates. The Camofox config reports `os=linux`, `architecture=amd64`; its single-platform manifest matches the explicit `platform: linux/amd64` override. The Lightpanda index reports Linux amd64 (`sha256:49e7794b489f95c718224b8d767faf280cd58f84d43af7f4fa15364269995065`) and Linux arm64 (`sha256:0dec6fe0cb0ccc99f0c644c926180374005596134e5f2dfb5e31e24ff3f50e22`) manifests. Requests for Lightpanda tags `0.2.6` and `v0.2.6` both returned HTTP 404, so the digest-only latest index remains intentionally unassociated with that release tag.

## Isolated browser smoke

Apple `container` 1.4.1 ran the candidates without host port publication and with unique names. The Camofox container used Linux amd64 emulation and the compose environment (`CAMOFOX_HOST=0.0.0.0`, port 9377, auth disabled, isolated profile path). Its internal health request passed:

```json
{"ok":true,"running":true,"engine":"camoufox","version":"2.4.7","browserConnected":false,"poolSize":0,"activeUserIds":[],"profileDirsTotal":0}
```

A disposable HTML server was started inside the Camofox container. Camofox correctly rejected `http://127.0.0.1:8787/` with `Blocked private network target: 127.0.0.1`, so the local-fixture navigation could not be used. A bounded public fixture (`https://example.com/`) then passed tab creation, wait, JavaScript evaluation, and tab deletion. Evaluation returned title and heading `Example Domain` and the expected final URL.

Lightpanda started on native Linux arm64 with the inherited read-only and tmpfs settings. The exact inherited healthcheck command (`bash -c 'exec 3<>/dev/tcp/127.0.0.1/9222'`) passed. An internal `/json/version` request returned HTTP 200 and `Lightpanda/1.0`, protocol `1.3`, and `Lightpanda-Version: 1.0.0-nightly.9608+b1ffc164`.

Both isolated containers were stopped and deleted. A final `container list --all` query found no `fireghost-browser-upgrade-*` container.

## Scope and remaining gate

These are candidate image, Compose merge, health, and bounded browser smoke results. The full production-versus-candidate browser regression matrix was not run: the complete Compose appliance requires the candidate CRW image and router build under a Docker Compose daemon, while this host provides Apple Container only. The Camofox check used amd64 emulation; it does not establish native ARM64 compatibility. Default pins and production remain unchanged pending the full matrix and equivalence/regression approval.
