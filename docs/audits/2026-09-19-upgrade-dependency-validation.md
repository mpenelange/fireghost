# Rust dependency upgrade validation — 2026-09-19

OSV queried all 513 registry package/version entries in the updated [`crw/Cargo.lock`](../../crw/Cargo.lock). The response contains **zero vulnerability advisories**. The only remaining records are maintenance notices:

| Package/version | Advisory | Classification | Fixed version or constraint |
| --- | --- | --- | --- |
| `number_prefix 0.4.0` | [RUSTSEC-2025-0119](https://rustsec.org/advisories/RUSTSEC-2025-0119.html) | Unmaintained | No fixed version; applies from `0.0.0-0`. Consider `unit-prefix`. |
| `ttf-parser 0.25.1` | [RUSTSEC-2026-0192](https://rustsec.org/advisories/RUSTSEC-2026-0192.html) | Unmaintained | No fixed version; applies from `0.0.0-0`. Consider `skrifa`. |

This is a version-level OSV result, not a reachability or production-inventory claim. The lockfile SHA-256 used for this validation is `b1af2c2adace333324e3821ea04f56163c253ce216b09636d4a361e7a04b90d1`.
