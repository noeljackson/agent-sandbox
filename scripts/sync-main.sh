#!/usr/bin/env bash
set -euo pipefail

# Rebuild main from upstream + fork patches + PR branches.
# Run this whenever upstream moves, a PR is rebased, or a new PR opens.
#
# Usage: scripts/sync-main.sh [--push]

UPSTREAM_REMOTE="upstream"
ORIGIN_REMOTE="origin"
FORK_BRANCH="desired-fork"

# PR branches to merge into main. Update this list as PRs open/close.
PR_BRANCHES=(
  "refactor/warmpool-sandbox-crs"  # PR #395 — warm pool Sandbox CRs
)

PUSH=false
if [[ "${1:-}" == "--push" ]]; then
  PUSH=true
fi

echo "==> Fetching remotes..."
git fetch "$UPSTREAM_REMOTE"
git fetch "$ORIGIN_REMOTE"

# Save current branch to restore later
ORIGINAL_BRANCH="$(git symbolic-ref --short HEAD 2>/dev/null || git rev-parse HEAD)"

echo "==> Resetting main to ${UPSTREAM_REMOTE}/main..."
git checkout -B main "${UPSTREAM_REMOTE}/main"

echo "==> Merging ${FORK_BRANCH} (fork-only patches)..."
git merge --no-ff "$FORK_BRANCH" -m "Merge ${FORK_BRANCH} patches"

for branch in "${PR_BRANCHES[@]}"; do
  # Strip inline comments
  branch="${branch%%#*}"
  branch="$(echo "$branch" | xargs)"
  [[ -z "$branch" ]] && continue

  echo "==> Merging ${branch}..."
  git merge --no-ff "$branch" -m "Merge branch '${branch}'"
done

echo ""
echo "main is now:"
git log --oneline "${UPSTREAM_REMOTE}/main..main"

if $PUSH; then
  echo ""
  echo "==> Pushing main to ${ORIGIN_REMOTE}..."
  git push "$ORIGIN_REMOTE" main --force-with-lease
  echo "Done. Image build should trigger if paths changed."
else
  echo ""
  echo "Run 'git push origin main --force-with-lease' to publish, or re-run with --push."
fi

# Restore original branch if it wasn't main
if [[ "$ORIGINAL_BRANCH" != "main" ]]; then
  git checkout "$ORIGINAL_BRANCH" 2>/dev/null || true
fi
