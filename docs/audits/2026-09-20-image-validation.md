# Fireghost image validation — 2026-09-20

This audit records vulnerability scans of isolated upgrade candidates on `michael@docker0.i.firewire.cc`. The validation host used local candidate tags and the exact browser digests below; no production services were changed or stopped.

## Scanner verification

The scanner was the official Aquasecurity Trivy `v0.74.0` Linux 64-bit release downloaded from its GitHub release and checked against the published checksum before execution.

- Archive SHA-256: `2ae6fe3ee734b7fdf11335663e18c75ea12dccc76062f09f164a3b0f8be4371a`
- Binary SHA-256: `d89bcc6510a267f11b773398cbf1be5520ce39f9e8b6633178c4487f05b7d791`
- Mode: `trivy image --scanners vuln`
- Vulnerability database: `mirror.gcr.io/aquasec/trivy-db:2`, updated before the first scan and reused for the remaining scans
- All four scans exited with code 0

## Candidate identities and results

| Candidate | Image identity | Detected OS | Scanner records | Severity counts | Advisory status counts |
| --- | --- | --- | ---: | --- | --- |
| Router | `fireghost-validation-router:20260920-upgrade-01`, local image ID `sha256:10c32ef9cabea4432af579485a73715a0d98a97b38e701436bce3d1434517a67` | Alpine 3.22.6 | 0 | none | none |
| CRW | `fireghost-validation-crw:20260920-upgrade-01`, local image ID `sha256:38e1148065ed927986ff660fa353cb9e19726307d821fd9077f6845ed1f556b8` | Debian 12.15 | 309 | 4 critical, 63 high, 121 medium, 121 low | 229 affected, 66 fix deferred, 14 will not fix |
| Camofox | `ghcr.io/redf0x1/camofox-browser@sha256:afaaf9795af8793f3e6353e9e5dd5b03713b6ffed6e80c1b0a179575322bcff0` | Debian 12.15 | 3,913 | 18 critical, 410 high, 2,041 medium, 1,085 low, 359 unknown | 3,033 affected, 166 fix deferred, 55 will not fix, 659 advisory-fixed |
| Lightpanda | `lightpanda/browser@sha256:8af7584500145dda4beb1a2e1d92a0e4d36c39c690b1bd8b1e2b174c09b363e0` | Debian 13.7 | 149 | 43 high, 49 medium, 56 low, 1 unknown | 147 affected, 2 fix deferred |

“Advisory-fixed” means Trivy’s upstream advisory status is fixed and a fixed package version is recorded; it does not mean the installed package in this image is already at that version. Camofox examples include `tar 7.5.11 → 7.5.19`, `pacote 19.0.2/20.0.1 → 21.5.1`, and some `linux-libc-dev` findings with fixed version `6.1.187-1`.

The CRW critical records include CVE-2023-45853 in `zlib1g` (will not fix), CVE-2026-13221 in `perl-base` (affected), CVE-2026-42496 in `perl-base` (fix deferred), and CVE-2026-8376 in `perl-base` (affected). Camofox has the same base-image families plus Perl, SQLite, GLib, XML, and kernel-header records. Lightpanda’s highest records include CVE-2025-69720 in ncurses packages and CVE-2026-16742 in systemd/udev packages.

## Interpretation and limits

The report counts are Trivy records and can repeat one CVE across several packages or files. “Affected,” “fix deferred,” and “will not fix” are advisory/database states requiring triage; they are not reachability or exploitability determinations. The scans did not establish whether a vulnerable code path is reachable in the deployed appliance.

The Debian scans emitted Trivy warnings that vendor severities were used for some advisories. Empty fixed-version fields mean the selected advisory feed did not provide a fixed version. The scan covered vulnerability advisories only; it did not run a configuration, secret, license, or runtime behavior audit.

## Evidence

The validation host retains the scanner summary, raw reports, scanner logs, restricted CRW identity record, and status file under:

`/home/michael/fireghost-validation/20260920-upgrade-01/artifacts/`

Relevant files are `trivy-summary.json`, `trivy-router.json`, `trivy-crw.json`, `trivy-camofox.json`, `trivy-lightpanda.json`, the matching `.stderr` logs, `image-identity-crw.txt`, and `trivy-status.txt`. The sanitized summary is copied locally beside this audit as `2026-09-20-image-validation.json`.
