'use strict';

// Run after npm ci in crw-renderer/tests/pipeline_dom. Reuse its locked jsdom
// dependency, but evaluate only this component's actual Rust-owned extractor.
const fs = require('node:fs');
const path = require('node:path');
const assert = require('node:assert/strict');
const {createRequire} = require('node:module');
const requireDOM = createRequire(path.resolve(__dirname, '../../../crw-renderer/tests/pipeline_dom/package.json'));
const {JSDOM} = requireDOM('jsdom');

const source = fs.readFileSync(path.resolve(__dirname, '../../src/camofox_search.rs'), 'utf8');
const constant = source.match(/\bconst\s+GOOGLE_SCRAPE_JS\s*:\s*&str\s*=\s*r(#{0,})"([\s\S]*?)"\1\s*;/);
assert.ok(constant, 'extract the actual GOOGLE_SCRAPE_JS raw string from Rust source');
const expression = constant[2];
const cases = JSON.parse(fs.readFileSync(path.join(__dirname, 'cases.json'), 'utf8'));
let failures = 0;

for (const test of cases) {
    const dom = new JSDOM(fs.readFileSync(path.join(__dirname, test.fixture), 'utf8'), {
        url: 'https://www.google.com/search?q=fixture-contract',
        runScripts: 'outside-only',
        pretendToBeVisual: true,
    });
    // jsdom has no layout-backed innerText. Fixture text is deliberately plain
    // and visible, so textContent supplies the same expected text for these DOMs.
    Object.defineProperty(dom.window.HTMLElement.prototype, 'innerText', {
        configurable: true,
        get() { return this.textContent; },
    });
    try {
        const raw = dom.window.eval(expression);
        assert.equal(typeof raw, 'string', 'preserve the Camofox JSON-string result contract');
        const rows = JSON.parse(raw);
        assert.ok(Array.isArray(rows), 'result is an array of scraped rows');
        assert.deepEqual(rows, test.expected);
        console.log(`PASS ${test.name}`);
    } catch (error) {
        failures += 1;
        console.error(`FAIL ${test.name}: ${error.message}`);
    } finally {
        dom.window.close();
    }
}

console.log(`${cases.length - failures}/${cases.length} Google DOM fixtures passed`);
process.exitCode = failures ? 1 : 0;
