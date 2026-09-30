//! Compact browser-side extraction. Scripts inspect the rendered DOM without
//! exporting the full document or modifying it during article extraction.

const MAX_SNAPSHOT_BYTES: usize = 96 * 1024;
const READABILITY_JS: &str = include_str!("../vendor/readability/Readability.min.js");

fn build_script(
    adapter: &str,
    expected_url: Option<&str>,
    max_items: usize,
    max_bytes: usize,
    seen_ids: &[String],
) -> String {
    let limits = serde_json::json!({
        "expectedUrl": expected_url,
        "maxItems": max_items.min(500),
        "maxBytes": max_bytes.min(MAX_SNAPSHOT_BYTES),
        "seenIds": seen_ids.iter().filter(|id| id.len() <= 32).take(1000).collect::<Vec<_>>(),
    });
    let mut script = String::from("(()=>{\n");
    script.push_str(COMMON_JS);
    script.push_str("\nconst LIMITS=");
    script.push_str(&limits.to_string());
    script.push_str(";\n");
    if adapter == ARTICLE_JS {
        script.push_str(READABILITY_JS);
        script.push('\n');
    }
    script.push_str(adapter);
    script.push_str("\n})()");
    script
}

pub fn article_snapshot(max_bytes: usize) -> String {
    build_script(ARTICLE_JS, None, 0, max_bytes, &[])
}

pub fn reddit_snapshot(
    expected_url: &str,
    max_items: usize,
    max_bytes: usize,
    seen_ids: &[String],
) -> String {
    build_script(
        REDDIT_JS,
        Some(expected_url),
        max_items,
        max_bytes,
        seen_ids,
    )
}

pub fn reddit_expand(expected_url: &str) -> String {
    build_script(
        REDDIT_EXPAND_JS,
        Some(expected_url),
        0,
        MAX_SNAPSHOT_BYTES,
        &[],
    )
}

const COMMON_JS: &str = r###"
const byteLength = value => new TextEncoder().encode(value).length;
const boundedText = (value, limit) => String(value || '').slice(0, limit);
const fullname = (value, kind) => {
    const match = String(value || '').toLowerCase().match(/^(?:thing_)?(t[13]_[a-z0-9]+)$/);
    return match && (!kind || match[1].startsWith(kind + '_')) ? match[1] : null;
};
const safeURL = value => {
    if (!value || String(value).length > 2048) return null;
    try {
        const url = new URL(value, location.href);
        return ['http:', 'https:'].includes(url.protocol) && !url.username && !url.password ? url.href : null;
    } catch (_) { return null; }
};
const own = (root, selector, owner) => Array.from(root.querySelectorAll(selector))
    .find(node => node.closest(owner) === root) || null;

// Construct valid, bounded HTML and preserve author-written links. No scripts,
// forms, event attributes, or browser UI are exported. Budget costs include JSON
// escaping; a partial prefix is marked explicitly rather than cutting markup.
function bodyHTML(root, budget) {
    const holder = document.createElement('div');
    const state = {used: 0, nodes: 0, overflow: false, stopped: false};
    const allowed = new Set('p a strong em b i s del blockquote pre code ul ol li h1 h2 h3 h4 h5 h6 br hr img figure figcaption table thead tbody tr td th sup sub div span'.split(' '));
    const excluded = new Set('script style template noscript form button iframe svg canvas input textarea select'.split(' '));
    const escaped = text => text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
    const charge = value => byteLength(JSON.stringify(value));
    function visit(node, parent, depth) {
        if (state.stopped) return;
        if (++state.nodes > 8000 || depth > 64) {
            state.overflow = state.stopped = true; return;
        }
        if (node.nodeType === 3) {
            const text = node.nodeValue || '';
            let take = text.length;
            if (charge(escaped(text)) > budget - state.used) {
                let low = 0, high = Math.min(text.length, Math.max(0, budget - state.used));
                while (low < high) {
                    const middle = Math.ceil((low + high) / 2);
                    if (charge(escaped(text.slice(0, middle))) <= budget - state.used) low = middle;
                    else high = middle - 1;
                }
                take = low;
                // Do not split a UTF-16 surrogate pair.
                if (take && /[\uD800-\uDBFF]/.test(text[take - 1])) --take;
                state.overflow = state.stopped = true;
            }
            if (take) {
                const part = text.slice(0, take);
                parent.appendChild(document.createTextNode(part));
                state.used += charge(escaped(part));
            }
            return;
        }
        if (node.nodeType !== 1) return;
        if (node.matches('shreddit-comment, .thing.comment')) return;
        const tag = node.tagName.toLowerCase();
        if (excluded.has(tag)) return;
        let target = parent;
        if (allowed.has(tag)) {
            target = document.createElement(tag);
            if (tag === 'a') {
                const href = safeURL(node.getAttribute('href'));
                if (href) target.setAttribute('href', href);
            }
            if (tag === 'img') {
                const src = safeURL(node.getAttribute('src'));
                if (src) target.setAttribute('src', src);
                if (node.hasAttribute('alt')) target.setAttribute('alt', boundedText(node.getAttribute('alt'), 512));
            }
            const shellCost = charge(target.outerHTML);
            if (shellCost > budget - state.used) {
                state.overflow = state.stopped = true; return;
            }
            state.used += shellCost;
            parent.appendChild(target);
        }
        for (const child of Array.from(node.childNodes)) visit(child, target, depth + 1);
    }
    if (root) for (const child of Array.from(root.childNodes)) visit(child, holder, 0);
    return {html: holder.innerHTML, overflow: state.overflow};
}
function baseSnapshot() {
    return {url: safeURL(location.href) || '', title: '', contentHtml: '', items: [],
        complete: false, overflow: false, outstandingControls: 0, advertisedCount: null,
        error: null, postId: null, commentsLoaded: false};
}
function serializeSnapshot(snapshot) {
    let encoded = JSON.stringify(snapshot);
    while (byteLength(encoded) > LIMITS.maxBytes && snapshot.items.length) {
        snapshot.items.pop(); snapshot.overflow = true; snapshot.complete = false;
        encoded = JSON.stringify(snapshot);
    }
    if (byteLength(encoded) > LIMITS.maxBytes) {
        snapshot.contentHtml = ''; snapshot.title = ''; snapshot.url = '';
        snapshot.overflow = true; snapshot.complete = false;
        snapshot.error = 'snapshot_budget_exceeded';
        encoded = JSON.stringify(snapshot);
    }
    if (byteLength(encoded) > LIMITS.maxBytes) return JSON.stringify({error: 'snapshot_budget_too_small'});
    return encoded;
}
const REDDIT_HOSTS = new Set(['reddit.com', 'www.reddit.com', 'old.reddit.com', 'new.reddit.com']);
function threadIdentity(value) {
    try {
        const url = new URL(value);
        const match = url.pathname.match(/\/comments\/([a-z0-9]+)(?:\/|$)/i);
        if (!REDDIT_HOSTS.has(url.hostname) || !['https:', 'http:'].includes(url.protocol)
            || url.username || url.password || (url.port && !['80', '443'].includes(url.port)) || !match) return null;
        return 't3_' + match[1].toLowerCase();
    } catch (_) { return null; }
}
function redditContext() {
    const expected = threadIdentity(LIMITS.expectedUrl);
    if (!expected) return {error: 'invalid_reddit_thread_url'};
    if (threadIdentity(location.href) !== expected) return {error: 'reddit_thread_identity_changed', postId: expected};
    function postIdentity(post) {
        const values = [post.getAttribute('id'), post.getAttribute('data-fullname'), post.getAttribute('post-id'), post.getAttribute('thingid')]
            .map(value => fullname(value, 't3')).filter(Boolean);
        const permalink = safeURL(post.getAttribute('permalink') || post.getAttribute('data-permalink'));
        if (permalink) values.push(threadIdentity(permalink));
        return values.length && values.every(value => value === expected) ? expected : null;
    }
    const modern = Array.from(document.querySelectorAll('shreddit-post')).find(post => postIdentity(post) === expected);
    const old = modern ? null : Array.from(document.querySelectorAll('.thing.link')).find(post => postIdentity(post) === expected);
    const post = modern || old;
    if (!post) {
        // Inspect only the gate shell when the requested post is absent. A post
        // quoting these phrases must never become a false block classification.
        const text = boundedText(document.body && document.body.textContent, 20000).toLowerCase();
        const blocked = /blocked by network security|whoa there, pardner|verify you are human|log in to continue|private community|banned community/.test(text);
        const missing = /page not found|there doesn't seem to be anything here|this post is no longer available/.test(text);
        return {error: blocked ? 'reddit_thread_blocked' : missing ? 'reddit_thread_missing' : 'reddit_thread_not_loaded', postId: expected};
    }
    const scope = post.closest('main') || document;
    const tree = modern ? scope.querySelector('shreddit-comment-tree') : scope.querySelector('.commentarea');
    if (tree) {
        const treeId = fullname(tree.getAttribute('post-id') || tree.getAttribute('link-id'), 't3');
        if (treeId && treeId !== expected) return {error: 'reddit_comment_tree_identity_mismatch', postId: expected};
    }
    return {post, tree, modern: !!modern, postId: expected, error: null};
}
function redditControls(context) {
    if (!context.tree) return [];
    const selector = context.modern
        ? 'button, a, shreddit-comment-more-comments, shreddit-comment-more-replies'
        : '.morechildren a, a.morechildren, .morecomments a, a.showreplies';
    const candidates = Array.from(context.tree.querySelectorAll(selector));
    const controls = [];
    for (let node of candidates) {
        if (node.tagName.toLowerCase().startsWith('shreddit-comment-more-')) {
            node = node.querySelector('button, a') || (node.shadowRoot && node.shadowRoot.querySelector('button, a'));
            if (!node) continue;
        }
        if (controls.includes(node) || node.disabled || node.getAttribute('aria-disabled') === 'true'
            || node.closest('[hidden], [aria-hidden="true"]')) continue;
        const style = getComputedStyle(node);
        if (style.display === 'none' || style.visibility === 'hidden') continue;
        const label = (node.getAttribute('aria-label') || node.textContent || '').replace(/\s+/g, ' ').trim();
        const safeLabel = /^(?:(?:load|view|show|see)\s+(?:\d+\s+)?(?:more\s+)?(?:comments?|replies)|(?:\d+\s+)?more\s+(?:comments?|replies)|continue\s+this\s+thread)(?:\s*\(\d+(?:\s+(?:comments?|replies))?\))?$/i.test(label);
        if (!safeLabel) continue;
        if (node.tagName === 'A') {
            const raw = node.getAttribute('href') || '';
            if (raw && !raw.startsWith('#') && !/^javascript:\s*(?:void\(0\)|;?)\s*;?$/i.test(raw)) {
                const destination = safeURL(raw);
                if (!destination || threadIdentity(destination) !== context.postId) continue;
            }
        }
        controls.push(node);
    }
    return controls;
}
"###;

const ARTICLE_JS: &str = r###"
const snapshot = baseSnapshot();
try {
    const article = new Readability(document.cloneNode(true), {charThreshold: 100}).parse();
    if (!article || !article.content) {
        snapshot.error = 'article_not_readable'; return serializeSnapshot(snapshot);
    }
    snapshot.title = boundedText(article.title || document.title, 512);
    const template = document.createElement('template');
    template.innerHTML = article.content;
    const body = bodyHTML(template.content, Math.max(0, LIMITS.maxBytes - 4096));
    snapshot.contentHtml = body.html;
    snapshot.overflow = body.overflow;
    snapshot.complete = !body.overflow;
    return serializeSnapshot(snapshot);
} catch (_) {
    snapshot.error = 'article_extraction_failed'; return serializeSnapshot(snapshot);
}
"###;

const REDDIT_JS: &str = r###"
const snapshot = baseSnapshot();
const context = redditContext();
snapshot.postId = context.postId || null;
if (context.error) { snapshot.error = context.error; return serializeSnapshot(snapshot); }
const postOwner = context.modern ? 'shreddit-post' : '.thing.link';
const title = own(context.post, '[slot="title"], h1, a.title', postOwner);
snapshot.title = boundedText(context.post.getAttribute('post-title') || (title && title.textContent) || document.title, 512);
const heading = document.createElement('h1');
heading.textContent = snapshot.title;
const postBody = own(context.post, context.modern ? '[slot="text-body"]' : '.usertext-body .md', postOwner);
const postContent = bodyHTML(postBody, Math.max(0, Math.min(32768, Math.floor((LIMITS.maxBytes - 4096) / 2))));
snapshot.contentHtml = heading.outerHTML + postContent.html;
snapshot.overflow = postContent.overflow;
const declared = context.post.getAttribute('comment-count') || context.post.getAttribute('data-comments-count');
if (declared !== null && /^\d+$/.test(declared)) snapshot.advertisedCount = Number(declared);
snapshot.outstandingControls = redditControls(context).length;
const owner = context.modern ? 'shreddit-comment' : '.thing.comment';
const nodes = context.tree ? Array.from(context.tree.querySelectorAll(owner)) : [];
const seenIds = new Set((LIMITS.seenIds || []).map(id => fullname(id, 't1')).filter(Boolean));
let loadedComments = false;
let remaining = Math.max(0, LIMITS.maxBytes - byteLength(JSON.stringify(snapshot)) - 1024);
for (const comment of nodes) {
    const bodyNode = own(comment, context.modern ? '[slot="comment"]' : '.entry .usertext-body .md', owner);
    if (!bodyNode) continue; // A skeleton is not a loaded comment.
    loadedComments = true;
    const permalinkNode = own(comment, 'a.bylink, a[data-testid="comment-permalink"]', owner);
    const permalink = safeURL(comment.getAttribute('permalink') || comment.getAttribute('data-permalink') || (permalinkNode && permalinkNode.getAttribute('href')));
    if (permalink && threadIdentity(permalink) !== context.postId) {
        snapshot.error = 'reddit_comment_identity_mismatch'; return serializeSnapshot(snapshot);
    }
    let id = fullname(comment.getAttribute('thingid') || comment.getAttribute('thing-id') || comment.getAttribute('data-fullname') || comment.id, 't1');
    if (!id && permalink && threadIdentity(permalink) === context.postId) {
        const match = new URL(permalink).pathname.match(/\/comments\/[a-z0-9]+\/[^/]+\/([a-z0-9]+)(?:\/|$)/i);
        if (match) id = 't1_' + match[1].toLowerCase();
    }
    // Previously emitted identities do not consume this snapshot's body budget.
    // Missing IDs remain observable and are never replaced with synthetic IDs.
    if (id && seenIds.has(id)) continue;
    if (snapshot.items.length >= LIMITS.maxItems) { snapshot.overflow = true; break; }
    const ancestor = comment.parentElement && comment.parentElement.closest(owner);
    let parentId = fullname(comment.getAttribute('parentid') || comment.getAttribute('parent-id'));
    if (parentId && parentId.startsWith('t3_') && parentId !== context.postId) {
        snapshot.error = 'reddit_comment_identity_mismatch'; return serializeSnapshot(snapshot);
    }
    if (!parentId && ancestor) parentId = fullname(ancestor.getAttribute('thingid') || ancestor.getAttribute('data-fullname') || ancestor.id, 't1');
    const declaredDepth = comment.getAttribute('depth');
    let depth = 0;
    for (let node = ancestor; node && depth < 256; node = node.parentElement && node.parentElement.closest(owner)) ++depth;
    if (declaredDepth !== null && /^\d+$/.test(declaredDepth)) depth = Math.min(256, Number(declaredDepth));
    if (!parentId && context.modern && declaredDepth === '0') parentId = context.postId;
    const authorNode = own(comment, '.tagline .author', owner);
    const author = boundedText(comment.getAttribute('author') || comment.getAttribute('data-author') || (authorNode && authorNode.textContent), 128) || null;
    const content = bodyHTML(bodyNode, Math.max(0, Math.min(16384, remaining - 512)));
    const item = {id, parentId, permalink: permalink && threadIdentity(permalink) === context.postId ? permalink : null,
        author, depth, bodyHtml: content.html};
    const itemBytes = byteLength(JSON.stringify(item)) + 1;
    if (itemBytes > remaining || !item.bodyHtml) { snapshot.overflow = true; break; }
    snapshot.items.push(item); remaining -= itemBytes;
    if (content.overflow) { snapshot.overflow = true; break; }
}
snapshot.commentsLoaded = loadedComments || (snapshot.advertisedCount === 0 && !!context.tree);
// A DOM snapshot cannot establish that every public comment has been retrieved.
snapshot.complete = false;
return serializeSnapshot(snapshot);
"###;

const REDDIT_EXPAND_JS: &str = r###"
const context = redditContext();
const result = {clicked: 0, scrolled: false, error: context.error || null, postId: context.postId || null};
if (context.error) return JSON.stringify(result);
const controls = redditControls(context);
result.outstandingControls = controls.length;
if (controls.length) {
    controls[0].click(); result.clicked = 1;
    if (threadIdentity(location.href) !== context.postId) result.error = 'reddit_thread_identity_changed';
} else if (context.tree) {
    window.scrollBy(0, Math.max(1, Math.min(600, Math.floor(window.innerHeight * 0.75) || 600)));
    result.scrolled = true;
}
return JSON.stringify(result);
"###;
