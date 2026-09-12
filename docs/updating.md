# Updating upstream components

Upstream discovery and deployment selection are deliberately separate:

- `upstreams.json` records the exact Git commits and mutable image-tag digests that were last reviewed. It is a review ledger, not a deployment input.
- `dev/stack.lock.json` remains the tested appliance lock. Compose and stack-lock tests continue to require immutable deployment digests.
- `./scripts/check-updates.sh check` reads the review ledger and reports every component as `current`, `changed`, or `unavailable`. It does not edit either file, fetch source into the worktree, pull an image, or deploy anything. A nonzero exit means operator review is required.

The monorepo's `crw/` is our maintained fork, with local changes after the shared lineage commit `aac7999b9379fd8b6ef818ce37f78634416f79c1`. The source entries are intentionally distinct: `crwVendor` tracks the immediate vendor upstream's renderer work at `adambenhassen/crw-camofox` branch `feat/camofox-renderer`; its reviewed head is `84f12bb3ef4c4111142e4da894444f2052fea493`, whose patch is already represented in the maintained fork. `crwFoundation` independently exposes movement in foundational `us/crw`. A foundation change is research input and must not be imported directly into `crw/`. The retired `michael/crw-camofox` repository is not an update source.

The image entries probe mutable tags only to discover new published artifacts. Their `reviewedDigest` values never authorize a deployment. Resolve and test a platform-appropriate immutable digest before proposing any later change to the appliance lock.

## Check for movement

Run:

```sh
make check-updates
```

The checker uses `git ls-remote` for branch heads and `docker buildx imagetools inspect` for tag digests. A missing executable, authentication failure, network failure, malformed response, or missing ref is reported as `unavailable`, not as “no update.” `--git-command` and `--image-command` replace those command boundaries for offline tests.

When a result changes, review upstream commits, releases, licensing, security notices, and browser compatibility before updating the reviewed ledger. Update one component per review; do not advance unrelated entries merely because they were discovered together.

## Prepare a CRW-fork source review

Work from a separate local clone so fetching cannot alter this repository's refs:

```sh
git clone https://github.com/adambenhassen/crw-camofox /tmp/crw-camofox-review
git -C /tmp/crw-camofox-review fetch --prune origin feat/camofox-renderer
./scripts/check-updates.sh prepare-crw \
  --source-repo /tmp/crw-camofox-review \
  --candidate <full-candidate-commit> \
  --output /tmp/crw-update-review
```

The candidate must resolve to a commit descended from `crwVendor.reviewedCommit`. The command diffs that reviewed vendor base to the candidate, prefixes the patch for `crw/`, and checks whether it applies over our maintained fork without changing the worktree. It creates:

- `crw-fork.patch`, an exact binary-capable diff prefixed for `crw/`;
- `review.json`, recording reviewed and candidate commits, applicability, whether action is required, and the explicit apply command retained for review evidence.

Preparation treats the target monorepo as read-only: it never changes the target worktree, index, refs, or object database, and never fetches source objects into it. It also never applies, merges, commits, pushes, or edits deployment state. The separate source clone is the only repository that must contain the reviewed and candidate objects. If the forward `git apply --check` fails, a successful `git apply --reverse --check` means the complete upstream patch is already present. When later fork work has changed that content, preserved Git history may instead prove integration: the candidate is already an ancestor of the target, or stable patch IDs computed independently in the source and target show that every non-merge source patch in `reviewed..candidate` occurs in target history. This proof requires at least one non-merge, non-empty source patch, so an empty or merge-only range is never accepted accidentally. Either proof makes preparation exit successfully, record `applicability: already-integrated` and `actionRequired: false`, and retain both review artifacts without applying anything. Do not run the recorded apply command for that state. Without one of those proofs, fork-local changes are treated as divergent: preparation exits nonzero, records `applicability: conflict`, retains the patch, and prints Git's conflict details. Resolve such updates deliberately; never replace `crw/` with an upstream snapshot.

After inspecting both artifacts, apply the recorded command from the repository root. Review every resulting change and confirm that only `crw/` moved. Preserve fork patches explicitly, then run CRW component tests and the complete appliance suite. Do not change `dev/stack.lock.json` during source review.

## Candidate and staging sequence

For each source or browser update:

1. Advance only that component's reviewed entry after completing review.
2. Run `make check-crw` for CRW changes and `make test-appliance` for all changes; run the broader regression gates required by `docs/migration-validation.md`.
3. Build and identify a candidate without changing production. Runtime images and release manifests must identify the monorepo revision and component version.
4. Resolve the candidate image to an immutable digest and update `dev/stack.lock.json` only in the later appliance-candidate change. Run `make check-stack-lock` and the full validation suite.
5. Exercise the isolated staging appliance and its smoke/live-contract gates. Obtain equivalence and regression approval before any production cutover.

`CRW_IMAGE` remains supplied through ignored `dev/.env`; never copy credentials into the ledger or commit them. `dev/.env.example` and the lock describe tested candidates using immutable digests. Back up volumes and retain the previous image/configuration for rollback before any separately authorized deployment.
