#!/usr/bin/env node
// build-faq.mjs — inject FAQ blocks into docs/{slug}/index.html
//
// Reads docs/data/endpoint-faq-data.json (single source of truth) and replaces
// <!-- FAQ-INJECT:{slug} --> with a visible <dl> block plus a JSON-LD FAQPage
// script. Visible text and schema text are emitted from one shared object so
// they stay byte-identical (geo-metadata rule 4).
//
// Usage:
//   node docs/scripts/build-faq.mjs            # write changes
//   node docs/scripts/build-faq.mjs --dry-run  # report-only, no writes
//
// Idempotent: re-running with the placeholder already replaced and the
// <!-- FAQ-INJECTED:{slug} --> sentinel present logs "already injected" and
// skips. No external dependencies — Node stdlib only.

import { readFile, writeFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";

const __filename = fileURLToPath(import.meta.url);
const __dirname = dirname(__filename);
const DOCS_ROOT = resolve(__dirname, "..");
const DATA_PATH = resolve(DOCS_ROOT, "data/endpoint-faq-data.json");

const args = process.argv.slice(2);
const DRY_RUN = args.includes("--dry-run");

const HTML_ESCAPE = {
  "&": "&amp;",
  "<": "&lt;",
  ">": "&gt;",
  '"': "&quot;",
  "'": "&#39;",
};

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => HTML_ESCAPE[c]);
}

function buildVisibleBlock(slug, faqs) {
  const items = faqs
    .map(
      (f) =>
        `    <dt>${escapeHtml(f.q)}</dt>\n    <dd>${escapeHtml(f.a)}</dd>`,
    )
    .join("\n");
  return [
    `  <section class="faq" id="faq-${escapeHtml(slug)}" aria-labelledby="faq-${escapeHtml(slug)}-heading">`,
    `    <h2 id="faq-${escapeHtml(slug)}-heading">Frequently asked</h2>`,
    `    <dl>`,
    items,
    `    </dl>`,
    `  </section>`,
  ].join("\n");
}

function buildSchemaBlock(faqs) {
  // Note: visible Q/A is the same source object as schema name / acceptedAnswer.text.
  // JSON.stringify escapes JSON correctly; the <script> body never goes through
  // the HTML escaper, so the schema strings remain byte-identical to the source.
  const payload = {
    "@context": "https://schema.org",
    "@type": "FAQPage",
    mainEntity: faqs.map((f) => ({
      "@type": "Question",
      name: f.q,
      acceptedAnswer: {
        "@type": "Answer",
        text: f.a,
      },
    })),
  };
  // Defensive: escape </script in case any answer ever contains it.
  const json = JSON.stringify(payload).replace(/<\/script/gi, "<\\/script");
  return `  <script type="application/ld+json">${json}</script>`;
}

function buildReplacement(slug, faqs) {
  const visible = buildVisibleBlock(slug, faqs);
  const schema = buildSchemaBlock(faqs);
  const sentinel = `<!-- FAQ-INJECTED:${slug} -->`;
  return `${sentinel}\n${visible}\n${schema}`;
}

async function processEntry(entry) {
  const { slug, faqs } = entry;
  if (!slug || !Array.isArray(faqs) || faqs.length === 0) {
    return { slug, status: "skip", reason: "empty entry" };
  }
  const htmlPath = resolve(DOCS_ROOT, slug, "index.html");
  let html;
  try {
    html = await readFile(htmlPath, "utf8");
  } catch (err) {
    return { slug, status: "skip", reason: `cannot read ${htmlPath}: ${err.code || err.message}` };
  }

  const placeholder = `<!-- FAQ-INJECT:${slug} -->`;
  const injectedMarker = `<!-- FAQ-INJECTED:${slug} -->`;
  const hasPlaceholder = html.includes(placeholder);
  const hasInjected = html.includes(injectedMarker);

  if (!hasPlaceholder && hasInjected) {
    return { slug, status: "already-injected", path: htmlPath };
  }
  if (!hasPlaceholder && !hasInjected) {
    return { slug, status: "warn", reason: `placeholder ${placeholder} missing`, path: htmlPath };
  }

  const replacement = buildReplacement(slug, faqs);
  const next = html.replace(placeholder, replacement);

  if (next === html) {
    return { slug, status: "skip", reason: "no-op replace" };
  }

  if (DRY_RUN) {
    return { slug, status: "would-inject", path: htmlPath, faqCount: faqs.length };
  }

  await writeFile(htmlPath, next, "utf8");
  return { slug, status: "injected", path: htmlPath, faqCount: faqs.length };
}

async function main() {
  let raw;
  try {
    raw = await readFile(DATA_PATH, "utf8");
  } catch (err) {
    console.error(`error: cannot read ${DATA_PATH}: ${err.message}`);
    process.exit(1);
  }

  let parsed;
  try {
    parsed = JSON.parse(raw);
  } catch (err) {
    console.error(`error: ${DATA_PATH} is not valid JSON: ${err.message}`);
    process.exit(1);
  }

  const entries = Array.isArray(parsed.entries) ? parsed.entries : [];
  if (entries.length === 0) {
    console.error("error: no entries found in endpoint-faq-data.json");
    process.exit(1);
  }

  const mode = DRY_RUN ? "dry-run" : "write";
  console.log(`build-faq: ${mode} — ${entries.length} entries`);

  const results = [];
  for (const entry of entries) {
    const result = await processEntry(entry);
    results.push(result);
    const tag = result.status.padEnd(16);
    const extra = result.reason ? ` (${result.reason})` : "";
    const count = result.faqCount ? ` [${result.faqCount} FAQs]` : "";
    console.log(`  ${tag} ${result.slug}${count}${extra}`);
  }

  const warned = results.filter((r) => r.status === "warn");
  if (warned.length > 0) {
    console.warn(`build-faq: ${warned.length} page(s) had no placeholder — see warnings above`);
  }

  const failedHard = results.some((r) => r.status === "warn" && /cannot read/.test(r.reason || ""));
  process.exit(failedHard ? 1 : 0);
}

main().catch((err) => {
  console.error(`build-faq: fatal: ${err.stack || err.message}`);
  process.exit(1);
});
