# Generated Readability distribution

`Readability.min.js` is generated from the unmodified, pinned `Readability.js`
identified in `UPSTREAM.json`. The original source and Mozilla license remain
alongside this generated distribution. Minification is required to keep the
JSON-escaped article evaluation request below Camofox's 64 KiB request limit.

Generation runs in the isolated test VM using `node:24-bookworm-slim` and
Terser **5.39.0**, pinned by `tests/pipeline_dom/package-lock.json`:

```sh
node tests/pipeline_dom/node_modules/terser/bin/terser \
  vendor/readability/Readability.js --compress --mangle --comments false \
  --output vendor/readability/Readability.min.js
```

SHA-256:

- Original: `e9330028c8a5a4aa7d75147be2605d520f7f213c7b28474947dc0e9c984e9bed`
- Generated: `dcc66b7ac1da023e300c9c8423771b1af2174216392715de7f1e3bf6b5496b5c`

The generated file is 32,858 bytes. DOM fixtures execute the generated library
and the actual adapter scripts, verify article extraction leaves the live DOM
unchanged, and assert the complete JSON evaluation request is below 64 KiB.
