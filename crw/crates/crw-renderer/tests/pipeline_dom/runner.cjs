'use strict';
const fs = require('node:fs');
const path = require('node:path');
const assert = require('node:assert/strict');
const {JSDOM} = require('jsdom');

const crate = path.resolve(__dirname, '../..');
const rust = fs.readFileSync(path.join(crate, 'src/pipeline_scripts.rs'), 'utf8');
const readability = fs.readFileSync(path.join(crate, 'vendor/readability/Readability.min.js'), 'utf8');
function scriptConstant(name) {
    const marker = `const ${name}: &str = r###"`;
    const start = rust.indexOf(marker);
    assert.notEqual(start, -1, `actual ${name} script is present`);
    const end = rust.indexOf('"###;', start + marker.length);
    assert.notEqual(end, -1);
    return rust.slice(start + marker.length, end);
}
const common = scriptConstant('COMMON_JS');
const adapters = Object.fromEntries(['ARTICLE_JS', 'REDDIT_JS', 'REDDIT_EXPAND_JS'].map(name => [name, scriptConstant(name)]));
const threadURL = 'https://www.reddit.com/r/test/comments/abc123/scoped_thread/';

function page(fixture, url = threadURL) {
    const html = fixture.includes('<') ? fixture : fs.readFileSync(path.join(__dirname, fixture), 'utf8');
    const dom = new JSDOM(html, {url, runScripts: 'outside-only', pretendToBeVisual: true});
    dom.window.TextEncoder = TextEncoder;
    dom.scrolls = [];
    dom.window.scrollBy = (x, y) => dom.scrolls.push([x, y]);
    return dom;
}
function evaluate(dom, adapter, options = {}) {
    const limits = {expectedUrl: adapter === 'ARTICLE_JS' ? null : threadURL,
        maxItems: 100, maxBytes: 96 * 1024, seenIds: [], ...options};
    const expression = `(()=>{\n${common}\nconst LIMITS=${JSON.stringify(limits)};\n${adapter === 'ARTICLE_JS' ? readability : ''}\n${adapters[adapter]}\n})()`;
    const requestBytes = Buffer.byteLength(JSON.stringify({userId: 'fixture-worker', expression, timeout: 2000}), 'utf8');
    assert.ok(requestBytes < 64 * 1024, `actual evaluation request fits Camofox's 64KiB limit (${requestBytes} bytes)`);
    const raw = dom.window.eval(expression);
    assert.equal(typeof raw, 'string', 'Camofox receives the JSON-string contract');
    const snapshot = JSON.parse(raw);
    assert.ok(Buffer.byteLength(raw, 'utf8') <= limits.maxBytes, 'serialized result respects the snapshot budget');
    return snapshot;
}
function htmlText(html) {
    const fragment = JSDOM.fragment(html);
    return fragment.textContent;
}

const tests = [
    ['article uses real Readability, keeps links, and preserves live DOM', () => {
        const dom = page('article.html', 'https://article.example/story');
        const before = dom.window.document.documentElement.outerHTML;
        const result = evaluate(dom, 'ARTICLE_JS');
        assert.equal(result.error, null);
        assert.equal(result.complete, true);
        assert.equal(result.overflow, false);
        assert.match(result.contentHtml, /bounded browser extraction/);
        assert.match(result.contentHtml, /href="https:\/\/article\.example\/guide\?chapter=2"/);
        assert.match(result.contentHtml, /href="https:\/\/reference\.example\/paper"/);
        assert.doesNotMatch(result.contentHtml, /Account navigation|private script|<script|<form/);
        assert.equal(dom.window.document.documentElement.outerHTML, before, 'Readability runs on a clone');
        dom.window.close();
    }],
    ['article never returns a complete page when Readability finds no content', () => {
        const dom = page('<!doctype html><html><head><title>Empty shell</title></head><body></body></html>', 'https://article.example/empty');
        const result = evaluate(dom, 'ARTICLE_JS');
        assert.equal(result.error, 'article_not_readable');
        assert.equal(result.contentHtml, '');
        assert.equal(result.complete, false);
        dom.window.close();
    }],
    ['modern Reddit keeps exact thread, own bodies, parent links, and deleted markers', () => {
        const dom = page('modern.html');
        const result = evaluate(dom, 'REDDIT_JS');
        assert.equal(result.error, null);
        assert.equal(result.postId, 't3_abc123');
        assert.equal(result.title, 'Scoped thread');
        assert.equal(result.advertisedCount, 5);
        assert.equal(result.commentsLoaded, true);
        assert.equal(result.complete, false, 'loaded snapshot never claims global completeness');
        assert.equal(result.items.length, 4, 'skeleton is not treated as a loaded comment');
        assert.equal(result.items[0].id, 't1_parent');
        assert.equal(result.items[0].parentId, 't3_abc123');
        assert.doesNotMatch(result.items[0].bodyHtml, /Nested-only/);
        assert.equal(result.items[1].id, 't1_child');
        assert.equal(result.items[1].parentId, 't1_parent');
        assert.equal(result.items[1].depth, 1);
        assert.match(result.items[1].bodyHtml, /href="https:\/\/reference\.example\/reply"/);
        assert.equal(htmlText(result.items[2].bodyHtml), '[removed]');
        assert.equal(result.items[3].id, null, 'missing identity is never fabricated');
        assert.equal(htmlText(result.items[3].bodyHtml), '[deleted]');
        assert.doesNotMatch(result.contentHtml, /Unrelated recommendation/);
        dom.window.close();
    }],
    ['old Reddit preserves nesting without duplicating child bodies or inventing deleted IDs', () => {
        const dom = page('old.html', 'https://old.reddit.com/r/test/comments/abc123/scoped_thread/');
        const result = evaluate(dom, 'REDDIT_JS');
        assert.equal(result.error, null);
        assert.equal(result.title, 'Old scoped thread');
        assert.equal(result.items.length, 3);
        assert.equal(result.items[0].author, 'alice');
        assert.doesNotMatch(result.items[0].bodyHtml, /Old nested-only/);
        assert.equal(result.items[1].parentId, 't1_parent');
        assert.equal(result.items[1].depth, 1);
        assert.equal(result.items[2].id, null);
        assert.equal(htmlText(result.items[2].bodyHtml), '[deleted]');
        assert.equal(result.outstandingControls, 1);
        dom.window.close();
    }],
    ['link-only and deleted-text posts retain a readable requested title', () => {
        for (const fixture of ['modern.html', 'old.html']) {
            const dom = page(fixture);
            dom.window.document.querySelectorAll('[slot="text-body"], .thing.link .usertext-body').forEach(node => node.remove());
            const result = evaluate(dom, 'REDDIT_JS');
            assert.equal(result.error, null);
            assert.match(result.contentHtml, /<h1>(?:Old )?Scoped thread<\/h1>/i);
            assert.ok(result.items.length > 0);
            dom.window.close();
        }
    }],
    ['one safe modern expansion loads a reply, leaves vote/report/reply actions untouched', () => {
        const dom = page('modern.html');
        let unsafeClicks = 0;
        for (const id of ['vote', 'compose', 'report']) dom.window.document.getElementById(id).addEventListener('click', () => unsafeClicks++);
        const tree = dom.window.document.querySelector('shreddit-comment-tree');
        dom.window.document.getElementById('load-more').addEventListener('click', event => {
            tree.insertAdjacentHTML('beforeend', '<shreddit-comment thingid="t1_lazy" parentid="t1_parent" depth="1" author="carol"><div slot="comment"><p>Lazy loaded reply.</p></div></shreddit-comment>');
            event.target.remove();
        });
        const before = evaluate(dom, 'REDDIT_JS');
        const step = evaluate(dom, 'REDDIT_EXPAND_JS');
        assert.equal(step.clicked, 1);
        assert.equal(step.scrolled, false);
        assert.equal(step.error, null);
        assert.equal(unsafeClicks, 0);
        const after = evaluate(dom, 'REDDIT_JS');
        assert.equal(after.items.length, before.items.length + 1);
        assert.equal(after.items.at(-1).id, 't1_lazy');
        assert.equal(after.items.at(-1).parentId, 't1_parent');
        const scroll = evaluate(dom, 'REDDIT_EXPAND_JS');
        assert.equal(scroll.clicked, 0);
        assert.equal(scroll.scrolled, true);
        assert.deepEqual(dom.scrolls, [[0, 576]], 'scroll remains bounded');
        assert.equal(after.complete, false, 'stable scroll height is not completeness evidence');
        dom.window.close();
    }],
    ['old inline load-more controls expand only one safe action', () => {
        const dom = page('old.html', 'https://old.reddit.com/r/test/comments/abc123/scoped_thread/');
        let clicks = 0;
        dom.window.document.getElementById('old-load-more').addEventListener('click', event => {event.preventDefault(); clicks++;});
        const result = evaluate(dom, 'REDDIT_EXPAND_JS');
        assert.equal(result.clicked, 1);
        assert.equal(clicks, 1);
        dom.window.close();
    }],
    ['post identity mismatch, offsite hosts, and conflicting comment parent IDs fail closed', () => {
        const wrongLocation = page('modern.html', 'https://www.reddit.com/r/test/comments/wrong123/other/');
        assert.equal(evaluate(wrongLocation, 'REDDIT_JS').error, 'reddit_thread_identity_changed');
        assert.equal(evaluate(wrongLocation, 'REDDIT_EXPAND_JS').clicked, 0);
        wrongLocation.window.close();
        const wrongDOM = page('modern.html');
        wrongDOM.window.document.querySelector('shreddit-post#t3_abc123').setAttribute('id', 't3_wrong123');
        assert.notEqual(evaluate(wrongDOM, 'REDDIT_JS').error, null);
        wrongDOM.window.close();
        const offsite = page('modern.html');
        assert.equal(evaluate(offsite, 'REDDIT_JS', {expectedUrl: 'https://reddit.com.attacker.invalid/r/test/comments/abc123/title/'}).error, 'invalid_reddit_thread_url');
        offsite.window.document.querySelector('shreddit-comment').setAttribute('parentid', 't3_wrong123');
        assert.equal(evaluate(offsite, 'REDDIT_JS').error, 'reddit_comment_identity_mismatch');
        offsite.window.close();
    }],
    ['block screen differs from valid thread quotations and missing comment hydration', () => {
        const gate = page('<html><body><h1>You have been blocked by network security</h1></body></html>');
        assert.equal(evaluate(gate, 'REDDIT_JS').error, 'reddit_thread_blocked');
        assert.equal(evaluate(gate, 'REDDIT_EXPAND_JS').clicked, 0);
        gate.window.close();
        const loading = page('modern.html');
        loading.window.document.querySelectorAll('shreddit-comment').forEach(node => node.remove());
        const result = evaluate(loading, 'REDDIT_JS');
        assert.equal(result.error, null);
        assert.equal(result.advertisedCount, 5);
        assert.equal(result.commentsLoaded, false);
        assert.equal(result.complete, false);
        loading.window.document.querySelector('shreddit-post').setAttribute('comment-count', '0');
        assert.equal(evaluate(loading, 'REDDIT_JS').commentsLoaded, true, 'explicit zero count plus tree distinguishes a loaded empty state');
        loading.window.close();
    }],
    ['UTF-8 and escaped oversized content is bounded, structurally valid, explicitly partial', () => {
        const dom = page('modern.html');
        const body = dom.window.document.querySelector('shreddit-comment [slot="comment"]');
        body.innerHTML = '<p><a href="/r/test/wiki/index">Kept link</a></p><p></p>';
        body.lastElementChild.textContent = '😀 < > & " escaped prose '.repeat(5000);
        const result = evaluate(dom, 'REDDIT_JS', {maxBytes: 8192});
        assert.equal(result.error, null);
        assert.equal(result.overflow, true);
        assert.equal(result.complete, false);
        assert.ok(result.items.length >= 1);
        assert.match(result.items[0].bodyHtml, /href="https:\/\/www\.reddit\.com\/r\/test\/wiki\/index"/);
        assert.doesNotMatch(result.items[0].bodyHtml, /\uFFFD/);
        assert.ok(htmlText(result.items[0].bodyHtml).includes('😀'));
        const capped = evaluate(dom, 'REDDIT_JS', {maxItems: 1});
        assert.equal(capped.items.length, 1);
        assert.equal(capped.overflow, true);
        dom.window.close();
    }],
    ['seen-ID cursor reaches later already-loaded comments across bounded snapshots', () => {
        const dom = page('modern.html');
        const tree = dom.window.document.querySelector('shreddit-comment-tree');
        tree.innerHTML = '';
        for (let index = 0; index < 8; index++) {
            const comment = dom.window.document.createElement('shreddit-comment');
            comment.setAttribute('thingid', 't1_batch' + index);
            comment.setAttribute('parentid', 't3_abc123');
            comment.innerHTML = '<div slot="comment"><p>' + ('Body ' + index + ' ').repeat(400) + '</p></div>';
            tree.appendChild(comment);
        }
        const seenIds = [];
        for (let round = 0; round < 10; round++) {
            const result = evaluate(dom, 'REDDIT_JS', {maxBytes: 8192, seenIds});
            assert.equal(result.error, null);
            assert.equal(result.commentsLoaded, true);
            assert.ok(result.items.every(item => !seenIds.includes(item.id)));
            seenIds.push(...result.items.map(item => item.id));
            if (!result.overflow) break;
            assert.ok(result.items.length, 'each overflow snapshot advances the cursor');
        }
        assert.deepEqual(seenIds, Array.from({length: 8}, (_, index) => 't1_batch' + index));
        const exhausted = evaluate(dom, 'REDDIT_JS', {seenIds});
        assert.equal(exhausted.items.length, 0);
        assert.equal(exhausted.commentsLoaded, true);
        assert.equal(exhausted.overflow, false);
        evaluate(dom, 'REDDIT_JS', {seenIds: Array.from({length: 1000}, (_, index) => 't1_' + index.toString(36).padStart(29, 'a'))});
        dom.window.close();
    }],
    ['unsafe HTML and external load-more navigation are never exported or clicked', () => {
        const dom = page('modern.html');
        const body = dom.window.document.querySelector('shreddit-comment [slot="comment"]');
        body.innerHTML = '<p onclick="bad()">Safe prose <a href="javascript:alert(1)">unsafe link</a></p><script>bad()</script><form>secret controls</form>';
        const load = dom.window.document.getElementById('load-more');
        const external = dom.window.document.createElement('a');
        external.href = 'https://attacker.invalid/'; external.textContent = 'View more replies';
        load.replaceWith(external);
        let clicks = 0; external.addEventListener('click', () => clicks++);
        const snapshot = evaluate(dom, 'REDDIT_JS');
        assert.doesNotMatch(snapshot.items[0].bodyHtml, /onclick|javascript:|<script|<form|secret controls/);
        assert.equal(snapshot.outstandingControls, 0);
        const step = evaluate(dom, 'REDDIT_EXPAND_JS');
        assert.equal(step.clicked, 0);
        assert.equal(clicks, 0);
        dom.window.close();
    }],
];

let failures = 0;
for (const [name, run] of tests) {
    try { run(); console.log(`PASS ${name}`); }
    catch (error) { failures++; console.error(`FAIL ${name}\n${error.stack}`); }
}
console.log(JSON.stringify({tests: tests.length, passed: tests.length - failures, failed: failures}));
process.exitCode = failures ? 1 : 0;
